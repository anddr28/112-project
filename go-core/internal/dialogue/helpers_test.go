package dialogue

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/config"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/pg"
)

// Общие заготовки тестов пакета: фейки портов (ai-service, очередь, WebSocket), заглушка
// аутентификации, HTTP-хелперы (запрос идёт через httpx.Router — CSRF, RBAC и ApiError
// как в бою) и сид попытки в свежей БД pgtest.

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func ptr[T any](v T) *T { return &v }

// ---------------------------------------------------------------- аутентификация

// stubAuth — роль из X-Test-Role (пусто — нет сессии), пользователь — из X-Test-User.
type stubAuth struct{}

func (stubAuth) Authenticate(r *http.Request) (*core.Principal, error) {
	role := r.Header.Get("X-Test-Role")
	if role == "" {
		return nil, core.ErrUnauthenticated
	}
	id := uuid.New()
	if v := r.Header.Get("X-Test-User"); v != "" {
		id = uuid.MustParse(v)
	}
	return &core.Principal{UserID: id, Role: core.Role(role), Login: role, LastName: "Тестов", FirstName: "Тест"}, nil
}

// ---------------------------------------------------------------- фейки портов

// aiCall — что go-core отправил в ai-service за ход.
type aiCall struct {
	req   *aiservice.DialogTurnRequest
	audio []byte
	ctype string
	fname string
}

// fakeAI — ai-service: DialogTurn отвечает fn (по умолчанию — эхо текста оператора).
// Аудио вычитывается целиком, как это делает multipart-прокси AIClient.
type fakeAI struct {
	mu      sync.Mutex
	fn      func(req *aiservice.DialogTurnRequest, audio []byte) (*aiservice.DialogTurnResult, error)
	calls   []aiCall
	block   chan struct{} // не nil — DialogTurn ждёт закрытия (тест «одного хода в полёте»)
	entered chan struct{}
}

func (f *fakeAI) DialogTurn(ctx context.Context, req *aiservice.DialogTurnRequest, audio *core.AudioInput) (*aiservice.DialogTurnResult, error) {
	call := aiCall{req: req}
	if audio != nil {
		b, err := io.ReadAll(audio.Reader)
		if err != nil {
			return nil, &core.AIError{Kind: core.AIUnavailable, Message: "read audio", Err: err}
		}
		call.audio, call.ctype, call.fname = b, audio.ContentType, audio.Filename
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	fn, block, entered := f.fn, f.block, f.entered
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, &core.AIError{Kind: core.AIUnavailable, Message: "timeout", Err: ctx.Err()}
		}
	}
	if fn != nil {
		return fn(req, call.audio)
	}
	return aiReply(req, "Слышу вас, улица Ленина, дом 12."), nil
}

func (f *fakeAI) TTSSync(context.Context, *aiservice.TtsSyncRequest) (*components.TtsResult, error) {
	return nil, &core.AIError{Kind: core.AIUnavailable, Message: "not used"}
}
func (f *fakeAI) Health(context.Context) (*aiservice.Health, error)     { return nil, nil }
func (f *fakeAI) Queue(context.Context) (*aiservice.QueueStatus, error) { return nil, nil }
func (f *fakeAI) BreakerState() string                                  { return "closed" }

// respond — задать ответ ai-service на следующие ходы.
func (f *fakeAI) respond(fn func(req *aiservice.DialogTurnRequest, audio []byte) (*aiservice.DialogTurnResult, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fn = fn
}

func (f *fakeAI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeAI) lastCall() aiCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

// aiReply — успешный ответ ai-service на ход (текст оператора — эхо запроса).
func aiReply(req *aiservice.DialogTurnRequest, callerText string) *aiservice.DialogTurnResult {
	op := ""
	if req.OperatorText != nil {
		op = *req.OperatorText
	}
	llm := "qwen2.5:3b"
	return &aiservice.DialogTurnResult{
		SchemaVersion: components.N1,
		RequestId:     req.RequestId,
		AttemptId:     req.AttemptId,
		TurnNo:        req.TurnNo,
		Operator:      components.SttResult{Text: op},
		Caller:        components.CallerReply{Text: callerText, RevealedFactIds: &[]string{}},
		Engine:        components.Engine{DurationMs: 900, LlmModel: &llm},
	}
}

// fakeQueue — очередь ai_jobs в памяти с дедупом по DedupKey (как aijobs.Queue).
type fakeQueue struct {
	mu    sync.Mutex
	jobs  []core.NewJob
	dedup map[string]uuid.UUID
}

func (q *fakeQueue) Enqueue(_ context.Context, _ pg.Querier, j core.NewJob) (uuid.UUID, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.dedup == nil {
		q.dedup = map[string]uuid.UUID{}
	}
	if id, ok := q.dedup[j.DedupKey]; ok {
		return id, false, nil
	}
	q.dedup[j.DedupKey] = j.ID
	q.jobs = append(q.jobs, j)
	return j.ID, true, nil
}

func (q *fakeQueue) Available() bool             { return true }
func (q *fakeQueue) EstWaitSec(core.JobType) int { return 1 }

func (q *fakeQueue) all() []core.NewJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]core.NewJob(nil), q.jobs...)
}

// recPublisher — сообщения WebSocket в памяти.
type recPublisher struct {
	mu      sync.Mutex
	monitor []public.MonitorMessage
	student []public.StudentMessage
}

func (p *recPublisher) Monitor(_ uuid.UUID, m public.MonitorMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.monitor = append(p.monitor, m)
}

func (p *recPublisher) Student(_ uuid.UUID, m public.StudentMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.student = append(p.student, m)
}

func (p *recPublisher) monitorTypes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.monitor))
	for i, m := range p.monitor {
		out[i] = string(m.Type)
		if m.Event != nil {
			out[i] += ":" + string(m.Event.Type)
		}
	}
	return out
}

func (p *recPublisher) studentTypes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.student))
	for i, m := range p.student {
		out[i] = string(m.Type)
	}
	return out
}

// ---------------------------------------------------------------- сервис и HTTP

type harness struct {
	svc   *Service
	ai    *fakeAI
	queue *fakeQueue
	pub   *recPublisher
	h     http.Handler
	pool  *pgxpool.Pool
}

func newHarness(t testing.TB, pool *pgxpool.Pool) *harness {
	t.Helper()
	hs := &harness{ai: &fakeAI{}, queue: &fakeQueue{}, pub: &recPublisher{}, pool: pool}
	hs.svc = New(Deps{
		Pool:      pool,
		Config:    &config.Config{AISyncTimeout: 5 * time.Second, TTSDir: t.TempDir()},
		AI:        hs.ai,
		Queue:     hs.queue,
		Publisher: hs.pub,
		Log:       discardLog(),
	})
	mux := http.NewServeMux()
	r := httpx.NewRouter(mux, "/api/v1", stubAuth{}, discardLog(), nil)
	hs.svc.Register(r)
	hs.h = mux
	return hs
}

// as — кто делает запрос (роль + пользователь).
type as struct {
	role core.Role
	user uuid.UUID
}

func (hs *harness) do(t testing.TB, who as, method, path string, body io.Reader, ctype string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/v1"+path, body)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if method != http.MethodGet {
		req.Header.Set("X-Requested-With", "fetch")
	}
	if who.role != "" {
		req.Header.Set("X-Test-Role", string(who.role))
		req.Header.Set("X-Test-User", who.user.String())
	}
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	return rec
}

func (hs *harness) json(t testing.TB, who as, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	return hs.do(t, who, method, path, rd, "application/json")
}

// formPart — часть multipart-формы (ctype пусто — текстовое поле).
type formPart struct {
	name, filename, ctype string
	data                  []byte
}

func multipartBody(t testing.TB, parts ...formPart) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		if p.filename != "" {
			h.Set("Content-Disposition", `form-data; name="`+p.name+`"; filename="`+p.filename+`"`)
		} else {
			h.Set("Content-Disposition", `form-data; name="`+p.name+`"`)
		}
		if p.ctype != "" {
			h.Set("Content-Type", p.ctype)
		}
		w, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(p.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

// webm — минимальная «запись» с сигнатурой EBML.
func webm(n int) []byte {
	b := make([]byte, max(n, 8))
	copy(b, []byte{0x1A, 0x45, 0xDF, 0xA3})
	return b
}

func decode[T any](t testing.TB, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %T: %v; body=%s", v, err, rec.Body.String())
	}
	return v
}

// apiErr — тело ApiError.
type apiErr struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

func expectStatus(t testing.TB, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, want, rec.Body.String())
	}
}

func expectError(t testing.TB, rec *httptest.ResponseRecorder, status int, code string) apiErr {
	t.Helper()
	expectStatus(t, rec, status)
	e := decode[apiErr](t, rec)
	if e.Code != code {
		t.Fatalf("code = %q, want %q; body=%s", e.Code, code, rec.Body.String())
	}
	if e.Message == "" {
		t.Fatalf("empty message: %s", rec.Body.String())
	}
	return e
}
