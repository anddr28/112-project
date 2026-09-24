package aijobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/components"
)

// Синхронные вызовы ai-service (core.AIClient) — голосовой режим и служебные запросы.
// Студент ждёт на линии, поэтому: breaker open → мгновенный отказ (без 20 с таймаута);
// 503/429 — «полоса занята» (caller_busy), breaker не трогаем; refused/timeout/5xx — отказ.

const (
	ttsSyncTimeout   = 10 * time.Second
	healthTimeout    = 3 * time.Second
	infoCacheTTL     = 5 * time.Second // /v1/health и /v1/queue: админка и WS aiHealth опрашивают часто
	maxSyncResponse  = 1 << 20
	defaultAudioName = "audio.webm"
)

// DialogTurn — ход диалога: реплика оператора (аудио — multipart, иначе JSON с operator_text)
// → ответ заявителя. Аудио не буферизуется целиком: стримится через io.Pipe прямо в сокет.
func (s *Service) DialogTurn(ctx context.Context, req *aiservice.DialogTurnRequest, audio *core.AudioInput) (*aiservice.DialogTurnResult, error) {
	if req == nil {
		return nil, &core.AIError{Kind: core.AIBadRequest, Message: "пустой запрос хода диалога"}
	}
	reqJSON, err := json.Marshal(req)
	if err != nil {
		return nil, &core.AIError{Kind: core.AIBadRequest, Message: "не удалось сериализовать запрос хода", Err: err}
	}
	var out aiservice.DialogTurnResult
	if err := s.syncCall(ctx, "/v1/dialog/turn", s.dialogTimeout(), reqJSON, audio, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TTSSync — синхронная озвучка (предпрослушивание, служебные фразы). Файл пишет ai-service
// в общий volume, ответ — путь и длительность.
func (s *Service) TTSSync(ctx context.Context, req *aiservice.TtsSyncRequest) (*components.TtsResult, error) {
	if req == nil {
		return nil, &core.AIError{Kind: core.AIBadRequest, Message: "пустой запрос озвучки"}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, &core.AIError{Kind: core.AIBadRequest, Message: "не удалось сериализовать запрос озвучки", Err: err}
	}
	var out components.TtsResult
	if err := s.syncCall(ctx, "/v1/tts/sync", ttsSyncTimeout, body, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BreakerState — closed | open | half_open.
func (s *Service) BreakerState() string { return s.br.State().String() }

// dialogTimeout — settings.ai.dialog_timeout_sec (админ меняет без рестарта), иначе
// GOCORE_AI_SYNC_TIMEOUT; контракт — 20 с.
func (s *Service) dialogTimeout() time.Duration {
	if sec := s.aiSettings().DialogTimeoutSec; sec > 0 {
		return time.Duration(sec) * time.Second
	}
	if s.cfg.AISyncTimeout > 0 {
		return s.cfg.AISyncTimeout
	}
	return 20 * time.Second
}

// syncCall — POST с JSON-телом (audio == nil) или multipart {request, audio}; ответ 200 → out.
func (s *Service) syncCall(ctx context.Context, path string, timeout time.Duration, reqJSON []byte, audio *core.AudioInput, out any) error {
	s.st.syncCalls.Add(1)
	ok, probe := s.br.Allow()
	if !ok {
		s.st.syncFailures.Add(1)
		return &core.AIError{Kind: core.AIUnavailable, Message: "сервис ИИ недоступен (circuit breaker open)"}
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var (
		req    *http.Request
		err    error
		srcErr *atomic.Pointer[error]
	)
	if audio == nil {
		req, err = http.NewRequestWithContext(cctx, http.MethodPost, s.base+path, bytes.NewReader(reqJSON))
		if err == nil {
			s.setHeaders(req, "application/json")
		}
	} else {
		req, srcErr, err = s.multipartRequest(cctx, path, reqJSON, audio)
	}
	if err != nil {
		s.br.Release(probe)
		return &core.AIError{Kind: core.AIUnavailable, Message: "не удалось построить запрос к ai-service", Err: err}
	}

	resp, err := s.hc.Do(req)
	if err != nil {
		return s.transportError(ctx, cctx, probe, srcErr, err)
	}
	defer drainClose(resp.Body)

	switch code := resp.StatusCode; {
	case code == http.StatusOK:
		s.br.Success()
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxSyncResponse)).Decode(out); err != nil {
			// Ответ не по контракту: сервис жив, но разговаривать с ним нельзя.
			s.log.Error("ai-service sync: bad response body", "path", path, "err", err)
			s.st.syncFailures.Add(1)
			return &core.AIError{Kind: core.AIUnavailable, Message: "ответ ai-service не соответствует контракту", Err: err}
		}
		return nil
	case code == http.StatusServiceUnavailable || code == http.StatusTooManyRequests:
		s.br.Success() // честный backpressure: сервис жив
		s.st.syncBusy.Add(1)
		ra := 0
		if d, ok := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
			ra = int((d + time.Second - 1) / time.Second)
		}
		return &core.AIError{Kind: core.AIBusy, RetryAfter: ra, Message: "полоса ai-service занята"}
	case code == http.StatusRequestEntityTooLarge:
		s.br.Success()
		return &core.AIError{Kind: core.AIAudioTooLong, Message: "аудио слишком длинное"}
	case code == http.StatusUnsupportedMediaType:
		s.br.Success()
		return &core.AIError{Kind: core.AIAudioUnsupported, Message: "формат аудио не поддерживается"}
	case code == http.StatusBadRequest || code == http.StatusUnprocessableEntity:
		s.br.Success()
		snippet := readSnippet(resp.Body)
		s.log.Error("ai-service sync: payload rejected (go-core bug)", "path", path, "status", code, "body", logSnippet(snippet))
		return &core.AIError{Kind: core.AIBadRequest, Message: fmt.Sprintf("ai-service отклонил запрос (HTTP %d)", code), Err: errors.New(snippet)}
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		s.br.Success()
		s.st.syncFailures.Add(1)
		s.log.Error("ai-service sync: X-Internal-Token rejected — check INTERNAL_API_TOKEN on both sides", "path", path, "status", code)
		return &core.AIError{Kind: core.AIUnavailable, Message: "ai-service отклонил внутренний токен"}
	case code >= 500:
		s.br.Failure()
		s.st.syncFailures.Add(1)
		s.log.Warn("ai-service sync: server error", "path", path, "status", code, "body", logSnippet(readSnippet(resp.Body)))
		return &core.AIError{Kind: core.AIUnavailable, Message: fmt.Sprintf("ошибка ai-service (HTTP %d)", code)}
	default:
		s.br.Success()
		s.st.syncFailures.Add(1)
		s.log.Error("ai-service sync: unexpected status", "path", path, "status", code, "body", logSnippet(readSnippet(resp.Body)))
		return &core.AIError{Kind: core.AIUnavailable, Message: fmt.Sprintf("неожиданный ответ ai-service (HTTP %d)", code)}
	}
}

// transportError — запрос не получил ответа. Отказом сервиса считаются только refused/timeout
// нашего собственного дедлайна; уход клиента (отмена родительского ctx) и обрыв чтения аудио
// у студента — не вина ai-service.
func (s *Service) transportError(parent, cctx context.Context, probe bool, srcErr *atomic.Pointer[error], err error) error {
	if srcErr != nil {
		if e := srcErr.Load(); e != nil {
			s.br.Release(probe)
			var mbe *http.MaxBytesError
			if errors.As(*e, &mbe) { // лимит тела, выставленный хендлером приёма аудио
				return &core.AIError{Kind: core.AIAudioTooLong, Message: "аудио больше допустимого размера", Err: *e}
			}
			return &core.AIError{Kind: core.AIBadRequest, Message: "не удалось прочитать аудио реплики", Err: *e}
		}
	}
	if errors.Is(parent.Err(), context.Canceled) {
		// Вызывающий ушёл (студент закрыл вкладку) — о сервисе это ничего не говорит.
		// Истёкший дедлайн родителя, наоборот, — таймаут ai-service и считается отказом.
		s.br.Release(probe)
		return &core.AIError{Kind: core.AIUnavailable, Message: "запрос отменён", Err: parent.Err()}
	}
	s.br.Failure()
	s.st.syncFailures.Add(1)
	msg := "ai-service недоступен"
	if cctx.Err() != nil || parent.Err() != nil {
		msg = "ai-service не ответил вовремя"
	}
	s.log.Warn("ai-service sync: transport error", "err", err)
	return &core.AIError{Kind: core.AIUnavailable, Message: msg, Err: err}
}

var copyBufPool = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}

// multipartRequest — multipart/form-data {request: JSON, audio: файл}, тело пишет горутина в
// io.Pipe. Горутина ограничена: транспорт всегда закрывает тело запроса (и при ошибке), после
// чего запись в pipe возвращает ошибку и горутина завершается. srcErr — ошибка чтения аудио
// (обрыв загрузки у студента), чтобы не записать её в отказы ai-service.
func (s *Service) multipartRequest(ctx context.Context, path string, reqJSON []byte, audio *core.AudioInput) (*http.Request, *atomic.Pointer[error], error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+path, pr)
	if err != nil {
		_ = pr.Close()
		return nil, nil, err
	}
	s.setHeaders(req, mw.FormDataContentType())
	srcErr := &atomic.Pointer[error]{}
	go func() {
		pw.CloseWithError(writeDialogMultipart(mw, reqJSON, audio, srcErr))
	}()
	return req, srcErr, nil
}

func writeDialogMultipart(mw *multipart.Writer, reqJSON []byte, audio *core.AudioInput, srcErr *atomic.Pointer[error]) error {
	h := make(textproto.MIMEHeader, 2)
	h.Set("Content-Disposition", `form-data; name="request"`)
	h.Set("Content-Type", "application/json")
	part, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	if _, err := part.Write(reqJSON); err != nil {
		return err
	}

	name := strings.TrimSpace(audio.Filename)
	if name == "" {
		name = defaultAudioName
	}
	ct := strings.TrimSpace(audio.ContentType)
	if ct == "" {
		ct = "application/octet-stream"
	}
	h = make(textproto.MIMEHeader, 2)
	h.Set("Content-Disposition", `form-data; name="audio"; filename="`+quoteEscaper.Replace(name)+`"`)
	h.Set("Content-Type", ct)
	if part, err = mw.CreatePart(h); err != nil {
		return err
	}
	if audio.Reader != nil {
		bp := copyBufPool.Get().(*[]byte)
		_, err = io.CopyBuffer(part, &trackingReader{r: audio.Reader, err: srcErr}, *bp)
		copyBufPool.Put(bp)
		if err != nil {
			return err
		}
	}
	return mw.Close()
}

var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"", "\r", "", "\n", "")

// trackingReader запоминает ошибку источника (не EOF), чтобы отличить её от ошибки записи в сокет.
type trackingReader struct {
	r   io.Reader
	err *atomic.Pointer[error]
}

func (t *trackingReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && err != io.EOF {
		e := err
		t.err.Store(&e)
	}
	return n, err
}

// ---------------------------------------------------------------- health / queue (кэш 5 с)

type cached[T any] struct {
	mu  sync.Mutex
	at  time.Time
	val T
	err error
}

func (c *cached[T]) get(now time.Time) (v T, ok bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && now.Sub(c.at) < infoCacheTTL {
		return c.val, true, c.err
	}
	return v, false, nil
}

func (c *cached[T]) set(v T, err error, at time.Time) {
	c.mu.Lock()
	c.val, c.err, c.at = v, err, at
	c.mu.Unlock()
}

// Health — GET /v1/health (кэш 5 с, конкурентные вызовы схлопываются). Не блокируется
// breaker'ом: админка должна видеть реальное состояние сервиса.
func (s *Service) Health(ctx context.Context) (*aiservice.Health, error) {
	v, err := fetchCached(ctx, s, &s.health, "health", "/v1/health")
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// Queue — GET /v1/queue (кэш 5 с). Pending — всегда не-nil map.
func (s *Service) Queue(ctx context.Context) (*aiservice.QueueStatus, error) {
	v, err := fetchCached(ctx, s, &s.queue, "queue", "/v1/queue")
	if err != nil {
		return nil, err
	}
	if v.Pending == nil {
		v.Pending = map[string]int{}
	}
	return &v, nil
}

func fetchCached[T any](ctx context.Context, s *Service, c *cached[T], key, path string) (T, error) {
	if v, ok, err := c.get(time.Now()); ok {
		return v, err
	}
	ch := s.sf.DoChan(key, func() (any, error) {
		// Общий запрос для всех ждущих — на собственном контексте: уход одного вызывающего
		// не должен обрывать ответ остальным.
		fctx, cancel := context.WithTimeout(context.Background(), healthTimeout)
		defer cancel()
		var v T
		err := s.getJSON(fctx, path, &v)
		c.set(v, err, time.Now())
		return v, err
	})
	select {
	case <-ctx.Done():
		var zero T
		return zero, &core.AIError{Kind: core.AIUnavailable, Message: "запрос отменён", Err: ctx.Err()}
	case r := <-ch:
		v, _ := r.Val.(T)
		return v, r.Err
	}
}

// getJSON — GET с токеном; итог учитывается breaker'ом (кроме состояния open — там
// поздние ответы игнорируются самим breaker'ом).
func (s *Service) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+path, nil)
	if err != nil {
		return &core.AIError{Kind: core.AIUnavailable, Message: "не удалось построить запрос к ai-service", Err: err}
	}
	s.setHeaders(req, "")
	resp, err := s.hc.Do(req)
	if err != nil {
		s.br.Failure()
		return &core.AIError{Kind: core.AIUnavailable, Message: "ai-service недоступен", Err: err}
	}
	defer drainClose(resp.Body)
	switch code := resp.StatusCode; {
	case code == http.StatusOK:
		s.br.Success()
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxSyncResponse)).Decode(out); err != nil {
			return &core.AIError{Kind: core.AIUnavailable, Message: "ответ ai-service не соответствует контракту", Err: err}
		}
		return nil
	case code == http.StatusServiceUnavailable || code == http.StatusTooManyRequests:
		s.br.Success()
		return &core.AIError{Kind: core.AIBusy, Message: "ai-service перегружен"}
	case code >= 500:
		s.br.Failure()
		return &core.AIError{Kind: core.AIUnavailable, Message: fmt.Sprintf("ошибка ai-service (HTTP %d)", code)}
	default:
		s.br.Success()
		if code == http.StatusUnauthorized || code == http.StatusForbidden {
			s.log.Error("ai-service: X-Internal-Token rejected — check INTERNAL_API_TOKEN on both sides", "path", path, "status", code)
		}
		return &core.AIError{Kind: core.AIUnavailable, Message: fmt.Sprintf("неожиданный ответ ai-service (HTTP %d)", code)}
	}
}
