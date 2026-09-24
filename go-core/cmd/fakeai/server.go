package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/components"
)

const (
	modelLLM = "fake-llm"
	modelSTT = "fake-stt"
	modelTTS = "fake-tts"

	maxJobBody = 8 << 20 // транскрипт + легенда + эталон с запасом
	maxSyncTTS = 1 << 20
	// Тело multipart: аудио до 2 МБ + JSON-части + служебное.
	maxMultipartBody = maxAudioBytes + 2*maxJSONPart + 64<<10

	// Синхронные операции: «время движка» (×FAKEAI_SPEED).
	dialogLLMDelay = 800 * time.Millisecond
	sttDelay       = 300 * time.Millisecond
	ttsSyncDelay   = 200 * time.Millisecond
	busyRetryAfter = 2
)

// apiError — ответ ApiError (_components.yaml). Хендлер возвращает её как error.
type apiError struct {
	status     int
	code, msg  string
	retryAfter int
}

func (e *apiError) Error() string { return e.code + ": " + e.msg }

func badPayload(format string, args ...any) *apiError {
	return &apiError{status: http.StatusBadRequest, code: "bad_payload", msg: fmt.Sprintf(format, args...)}
}

type server struct {
	cfg   config
	log   *slog.Logger
	q     *queue
	tts   *ttsStore
	token []byte

	healthBody  []byte
	dialogCalls atomic.Int64
	sttCalls    atomic.Int64

	avgMu       sync.Mutex
	dialogAvgMs int
	noSpeechMu  sync.Mutex
	noSpeechDur int // файл «Алло? Вы меня слышите?» уже записан
}

func newServer(cfg config, log *slog.Logger, q *queue, tts *ttsStore) *server {
	profiles := map[string]string{
		string(components.EvalFast): modelLLM, string(components.EvalThorough): modelLLM,
		string(components.EvalDialogue): modelLLM, string(components.Generate): modelLLM,
		string(components.DialogFast): modelLLM, string(components.TtsDefault): modelTTS,
		string(components.SttDefault): modelSTT,
	}
	health, _ := json.Marshal(aiservice.Health{
		Status: aiservice.Ok, Ollama: ptr(true), Languagetool: ptr(true), Tts: ptr(true), Stt: ptr(true),
		ModelsAvailable: &[]string{modelLLM}, Profiles: &profiles,
	})
	return &server{cfg: cfg, log: log, q: q, tts: tts, token: []byte(cfg.token), healthBody: health}
}

type handlerFunc func(http.ResponseWriter, *http.Request) error

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /v1/health", s.wrap(s.health, false))
	mux.Handle("GET /v1/queue", s.wrap(s.queueStatus, true))
	mux.Handle("POST /v1/jobs/{kind}", s.wrap(s.submitJob, true))
	mux.Handle("POST /v1/dialog/turn", s.wrap(s.dialogTurn, true))
	mux.Handle("POST /v1/stt/transcribe", s.wrap(s.transcribe, true))
	mux.Handle("POST /v1/tts/sync", s.wrap(s.ttsSync, true))
	mux.Handle("/", s.wrap(func(http.ResponseWriter, *http.Request) error {
		return &apiError{status: http.StatusNotFound, code: "not_found", msg: "Нет такого эндпоинта"}
	}, false))
	return mux
}

// wrap — токен, recover, превращение ошибки в ApiError, access-лог (debug).
func (s *server) wrap(fn handlerFunc, auth bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if p := recover(); p != nil {
				s.log.Error("паника в хендлере", "path", r.URL.Path, "panic", p)
				writeError(rec, &apiError{status: http.StatusInternalServerError, code: "internal", msg: "Внутренняя ошибка имитатора"})
			}
			s.log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", rec.status, "ms", time.Since(start).Milliseconds())
		}()
		if auth && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Internal-Token")), s.token) != 1 {
			writeError(rec, &apiError{status: http.StatusUnauthorized, code: "unauthorized", msg: "Неверный X-Internal-Token"})
			return
		}
		if err := fn(rec, r); err != nil {
			var ae *apiError
			if !errors.As(err, &ae) {
				s.log.Error("ошибка обработки", "path", r.URL.Path, "err", err)
				ae = &apiError{status: http.StatusInternalServerError, code: "internal", msg: "Внутренняя ошибка имитатора"}
			}
			writeError(rec, ae)
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) { r.status = code; r.ResponseWriter.WriteHeader(code) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		body = []byte(`{"code":"internal","message":"Ошибка кодирования ответа"}`)
	}
	writeRaw(w, status, body)
}

func writeRaw(w http.ResponseWriter, status int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, e *apiError) {
	if e.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.retryAfter))
	}
	writeJSON(w, e.status, components.ApiError{Code: e.code, Message: e.msg})
}

// readBody — тело целиком с лимитом (JSON-эндпоинты).
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, badPayload("Тело запроса больше %d байт", limit)
		}
		return nil, badPayload("Не удалось прочитать тело запроса: %v", err)
	}
	return body, nil
}

// --------------------------------------------------------------- проверка payload

type rawObj = map[string]json.RawMessage

func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// decodeTop — объект верхнего уровня + schema_version (контракт: несовпадение — 400).
func decodeTop(body []byte, schemaRequired bool) (rawObj, error) {
	var top rawObj
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return nil, badPayload("Тело должно быть JSON-объектом")
	}
	if schemaRequired {
		sv, ok := top["schema_version"]
		if !ok || !bytes.Equal(bytes.TrimSpace(sv), []byte(`"1"`)) {
			return nil, badPayload("Неподдерживаемая schema_version: ожидается \"1\"")
		}
	}
	return top, nil
}

// requireKeys — обязательные поля контракта присутствуют и не null. Строгость здесь
// — польза: имитатор в CI ловит payload'ы go-core, не соответствующие контракту.
func requireKeys(obj rawObj, prefix string, keys ...string) error {
	for _, k := range keys {
		if isNull(obj[k]) {
			return badPayload("Нет обязательного поля %s%s", prefix, k)
		}
	}
	return nil
}

func subObj(obj rawObj, key, prefix string) (rawObj, error) {
	var sub rawObj
	if err := json.Unmarshal(obj[key], &sub); err != nil || sub == nil {
		return nil, badPayload("Поле %s%s должно быть объектом", prefix, key)
	}
	return sub, nil
}

func decodeInto(body []byte, v any) error {
	if err := json.Unmarshal(body, v); err != nil {
		return badPayload("Невалидный payload: %v", err)
	}
	return nil
}

func checkPriority(p *int) (int, error) {
	if p == nil {
		return 5, nil
	}
	if *p < 1 || *p > 9 {
		return 0, badPayload("priority вне диапазона 1..9")
	}
	return *p, nil
}

func checkProfile(p *components.Profile) error {
	if p != nil && !p.Valid() {
		return badPayload("Неизвестный profile %q", string(*p))
	}
	return nil
}

// checkCallScript — обязательные поля CallScript (caller, address, turns[≥1]).
func checkCallScript(top rawObj, key string) error {
	cs, err := subObj(top, key, "")
	if err != nil {
		return err
	}
	if err := requireKeys(cs, key+".", "caller", "address", "turns"); err != nil {
		return err
	}
	var turns []json.RawMessage
	if err := json.Unmarshal(cs["turns"], &turns); err != nil || len(turns) == 0 {
		return badPayload("%s.turns: нужна хотя бы одна реплика", key)
	}
	return nil
}

// --------------------------------------------------------------- асинхронные задачи

func (s *server) submitJob(w http.ResponseWriter, r *http.Request) error {
	kind, ok := kindByName(r.PathValue("kind"))
	if !ok {
		return &apiError{status: http.StatusNotFound, code: "not_found", msg: "Неизвестный тип задачи"}
	}
	body, err := readBody(w, r, maxJobBody)
	if err != nil {
		return err
	}
	top, err := decodeTop(body, true)
	if err != nil {
		return err
	}
	p, err := parseJob(kind, body, top)
	if err != nil {
		return err
	}
	res, retryAfter, full := s.q.submit(kind, p.id, p.attempt, p.priority, p.payload)
	if full {
		return &apiError{status: http.StatusTooManyRequests, code: "queue_full",
			msg: "Очередь ai-service заполнена, повторите позже", retryAfter: retryAfter}
	}
	if res.resend != nil {
		s.q.cb.enqueue(delivery{id: p.id, body: res.resend})
	}
	s.log.Debug("задача принята", "request_id", p.id, "type", kindTypes[kind], "status", res.status, "position", res.position)
	writeJSON(w, http.StatusAccepted, aiservice.JobAccepted{
		RequestId: p.id, Status: res.status, QueuePosition: ptr(res.position), EstWaitSec: ptr(res.estSec),
	})
	return nil
}

func (s *server) queueStatus(w http.ResponseWriter, _ *http.Request) error {
	pending, running, est := s.q.snapshot()
	s.avgMu.Lock()
	avg := s.dialogAvgMs
	s.avgMu.Unlock()
	writeJSON(w, http.StatusOK, aiservice.QueueStatus{
		Pending: pending, Running: running, CurrentModel: ptr(modelLLM), LlmLoaded: ptr(true),
		EstWaitSec: ptr(est), DialogWaiting: ptr(0), DialogAvgMs: ptr(avg), SttRunning: ptr(0),
	})
	return nil
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) error {
	writeRaw(w, http.StatusOK, s.healthBody)
	return nil
}

// --------------------------------------------------------------- синхронные (голос)

// dialogTurn — POST /v1/dialog/turn: JSON или multipart (request + audio).
func (s *server) dialogTurn(w http.ResponseWriter, r *http.Request) error {
	start := time.Now()
	var reqBody []byte
	var audio *audioInfo
	if isMultipart(r) {
		r.Body = http.MaxBytesReader(w, r.Body, maxMultipartBody)
		var err error
		audio, err = readMultipart(r, map[string]*[]byte{"request": &reqBody})
		if err != nil {
			return err
		}
		if reqBody == nil {
			return badPayload("Нет части request")
		}
	} else {
		var err error
		if reqBody, err = readBody(w, r, maxJobBody); err != nil {
			return err
		}
	}
	req, err := parseDialogTurn(reqBody)
	if err != nil {
		return err
	}

	// Имитация перегруженной полосы LLM: честный backpressure, не ошибка попытки.
	if n := s.cfg.busyEvery; n > 0 && s.dialogCalls.Add(1)%int64(n) == 0 {
		return &apiError{status: http.StatusServiceUnavailable, code: "service_busy",
			msg: "Полоса LLM перегружена, повторите ход", retryAfter: busyRetryAfter}
	}

	opText := trimmed(req.OperatorText)
	var op components.SttResult
	usedSTT := false
	switch {
	case opText != "":
		// Текст оператора важнее аудио (контракт): STT не вызывается.
		op = components.SttResult{Text: opText, Confidence: ptr(float32(1)), AudioDurationMs: 0,
			NoSpeech: ptr(false), Language: ptr("ru")}
	case audio != nil:
		usedSTT = true
		op = fakeSTT(req.TurnNo, *audio)
	default:
		return badPayload("Нужен operator_text или часть audio")
	}

	voice, rate, ttsOn := defaultVoice, float32(1), true
	if o := req.Options; o != nil && o.Tts != nil {
		if o.Tts.Enabled != nil {
			ttsOn = *o.Tts.Enabled
		}
		if v := trimmed(o.Tts.Voice); v != "" {
			voice = v
		}
		if o.Tts.Rate != nil && *o.Tts.Rate > 0 {
			rate = *o.Tts.Rate
		}
	}

	var reply components.CallerReply
	if deref(op.NoSpeech) {
		// Тишина: заявитель переспрашивает, транскрипт не пополняется (решает go-core).
		reply = components.CallerReply{Text: noSpeechText, RevealedFactIds: &[]string{}, ShouldEnd: ptr(false),
			IsUnknownAnswer: ptr(false), EmotionalState: req.CallScript.Caller.EmotionalState}
		if ttsOn {
			dur, err := s.noSpeechFile()
			if err != nil {
				return err
			}
			reply.Tts = &components.TtsResult{TextHash: ttsHash(noSpeechText, voice, rate), FilePath: noSpeechPath,
				DurationMs: dur, Voice: ptr(voice), Rate: ptr(rate)}
		}
	} else {
		reply = callerReply(&req, op.Text)
		if ttsOn {
			rel := "dialog/" + req.AttemptId.String() + "/" + strconv.Itoa(req.TurnNo) + ".wav"
			dur, err := s.tts.write(rel, reply.Text, false)
			if err != nil {
				s.log.Error("tts диалога: запись файла", "attempt", req.AttemptId, "err", err)
				return &apiError{status: http.StatusServiceUnavailable, code: "service_busy",
					msg: "TTS недоступен, повторите ход", retryAfter: busyRetryAfter}
			}
			reply.Tts = &components.TtsResult{TextHash: ttsHash(reply.Text, voice, rate), FilePath: rel,
				DurationMs: dur, Voice: ptr(voice), Rate: ptr(rate)}
		}
	}

	delay := dialogLLMDelay
	if usedSTT {
		delay += sttDelay
	}
	if !sleepCtx(r.Context(), s.q.scaled(delay)-time.Since(start)) {
		return nil // клиент ушёл — отвечать некому
	}
	dur := int(time.Since(start).Milliseconds())
	s.observeDialog(dur)

	eng := components.Engine{DurationMs: dur, LlmModel: ptr(modelLLM), PromptVersion: ptr("fake-caller-v1"), QueueWaitMs: ptr(0)}
	if usedSTT {
		eng.SttModel = ptr(modelSTT)
	}
	if reply.Tts != nil {
		eng.TtsVersion = ptr(modelTTS)
	}
	writeJSON(w, http.StatusOK, aiservice.DialogTurnResult{
		SchemaVersion: components.N1, RequestId: req.RequestId, AttemptId: req.AttemptId, TurnNo: req.TurnNo,
		Operator: op, Caller: reply, Fallback: ptr(false), Engine: eng,
	})
	return nil
}

func parseDialogTurn(body []byte) (aiservice.DialogTurnRequest, error) {
	var req aiservice.DialogTurnRequest
	top, err := decodeTop(body, true)
	if err != nil {
		return req, err
	}
	if err := requireKeys(top, "", "request_id", "attempt_id", "turn_no", "call_script", "history"); err != nil {
		return req, err
	}
	if err := checkCallScript(top, "call_script"); err != nil {
		return req, err
	}
	if err := decodeInto(body, &req); err != nil {
		return req, err
	}
	switch {
	case req.RequestId == [16]byte{} || req.AttemptId == [16]byte{}:
		return req, badPayload("request_id и attempt_id обязательны")
	case req.TurnNo < 1:
		return req, badPayload("turn_no должен быть ≥ 1")
	}
	if err := checkProfile(req.Profile); err != nil {
		return req, err
	}
	for i := range req.History {
		if !req.History[i].Speaker.Valid() {
			return req, badPayload("history[%d].speaker: ожидается operator|caller", i)
		}
	}
	if d := req.CallScript.Dialogue; d != nil {
		for i := range d.Facts {
			if !d.Facts[i].Reveal.Valid() {
				return req, badPayload("call_script.dialogue.facts[%d].reveal: неизвестное значение", i)
			}
		}
	}
	return req, nil
}

// observeDialog — скользящее среднее полного хода (QueueStatus.dialog_avg_ms).
func (s *server) observeDialog(ms int) {
	s.avgMu.Lock()
	if s.dialogAvgMs == 0 {
		s.dialogAvgMs = ms
	} else {
		s.dialogAvgMs = (s.dialogAvgMs*7 + ms) / 8
	}
	s.avgMu.Unlock()
}

// noSpeechFile — служебная фраза «Алло? Вы меня слышите?» пишется один раз.
func (s *server) noSpeechFile() (int, error) {
	s.noSpeechMu.Lock()
	defer s.noSpeechMu.Unlock()
	if s.noSpeechDur > 0 {
		return s.noSpeechDur, nil
	}
	dur, err := s.tts.write(noSpeechPath, noSpeechText, true)
	if err != nil {
		return 0, err
	}
	s.noSpeechDur = dur
	return dur, nil
}

// transcribe — POST /v1/stt/transcribe (multipart: audio + options).
func (s *server) transcribe(w http.ResponseWriter, r *http.Request) error {
	start := time.Now()
	if !isMultipart(r) {
		return &apiError{status: http.StatusUnsupportedMediaType, code: "unsupported_media", msg: "Ожидается multipart/form-data"}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMultipartBody)
	var optBody []byte
	audio, err := readMultipart(r, map[string]*[]byte{"options": &optBody})
	if err != nil {
		return err
	}
	if audio == nil {
		return badPayload("Нет части audio")
	}
	if optBody != nil {
		var opts aiservice.SttOptions
		if err := decodeInto(optBody, &opts); err != nil {
			return err
		}
		if err := checkProfile(opts.Profile); err != nil {
			return err
		}
	}
	res := fakeSTT(int(s.sttCalls.Add(1)), *audio)
	if !sleepCtx(r.Context(), s.q.scaled(sttDelay)-time.Since(start)) {
		return nil
	}
	writeJSON(w, http.StatusOK, aiservice.SttResponse{
		Text: res.Text, TextRaw: res.TextRaw, Confidence: res.Confidence, AudioDurationMs: res.AudioDurationMs,
		NoSpeech: res.NoSpeech, Language: res.Language, Segments: res.Segments,
		Engine: components.Engine{DurationMs: int(time.Since(start).Milliseconds()), SttModel: ptr(modelSTT),
			SttVersion: ptr("fake-stt-1"), QueueWaitMs: ptr(0)},
	})
	return nil
}

// ttsSync — POST /v1/tts/sync: файл пишется синхронно (предпрослушивание, служебные фразы).
func (s *server) ttsSync(w http.ResponseWriter, r *http.Request) error {
	start := time.Now()
	body, err := readBody(w, r, maxSyncTTS)
	if err != nil {
		return err
	}
	top, err := decodeTop(body, false)
	if err != nil {
		return err
	}
	if err := requireKeys(top, "", "text", "text_hash"); err != nil {
		return err
	}
	var req aiservice.TtsSyncRequest
	if err := decodeInto(body, &req); err != nil {
		return err
	}
	if trimmed(&req.Text) == "" {
		return badPayload("text пуст")
	}
	if !validHash(req.TextHash) {
		return badPayload("text_hash: ожидается 8..128 символов [0-9A-Za-z_-]")
	}
	voice := trimmed(req.Voice)
	if voice == "" {
		voice = defaultVoice
	}
	rate := float32(1)
	if req.Rate != nil && *req.Rate > 0 {
		rate = *req.Rate
	}
	rel := hashPath(req.TextHash)
	dur, err := s.tts.write(rel, req.Text, true)
	if err != nil {
		s.log.Error("tts sync: запись файла", "err", err)
		return &apiError{status: http.StatusServiceUnavailable, code: "service_busy", msg: "TTS недоступен", retryAfter: busyRetryAfter}
	}
	if !sleepCtx(r.Context(), s.q.scaled(ttsSyncDelay)-time.Since(start)) {
		return nil
	}
	writeJSON(w, http.StatusOK, components.TtsResult{TextHash: req.TextHash, FilePath: rel, DurationMs: dur,
		Voice: ptr(voice), Rate: ptr(rate)})
	return nil
}
