package aijobs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/config"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/callbacks"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pgtest"
	"lct/gocore/internal/settings"
)

const (
	testToken    = "test-internal-token"
	testInstance = "test-instance"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newTestService — сервис без фоновых циклов. base — URL фейкового ai-service ("" — никуда).
func newTestService(t testing.TB, pool *pgxpool.Pool, base string) *Service {
	t.Helper()
	if base == "" {
		base = "http://127.0.0.1:1" // порт 1 закрыт: сетевые вызовы падают сразу
	}
	s := New(Deps{
		Pool: pool,
		Config: &config.Config{
			AIServiceURL:     base,
			InternalAPIToken: testToken,
			InstanceID:       testInstance,
			AIJobTimeout:     2 * time.Second,
			AISyncTimeout:    2 * time.Second,
		},
		Log:        discardLog(),
		HTTPClient: &http.Client{Transport: newTransport()},
	})
	t.Cleanup(func() { s.hc.CloseIdleConnections() })
	return s
}

// setAI подменяет снимок settings.ai (refreshSettings в тестах не работает — Settings nil).
func setAI(s *Service, mut func(ai *settings.AI)) {
	ai := *s.aiSettings()
	mut(&ai)
	s.ai.Store(&ai)
}

// newDBService — сервис над свежей БД с миграциями (тест пропускается без PostgreSQL).
func newDBService(t *testing.T, base string) (*Service, *pgxpool.Pool) {
	t.Helper()
	pool := pgtest.New(t)
	return newTestService(t, pool, base), pool
}

// ---------------------------------------------------------------- задачи

func rawPayload(id uuid.UUID) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"schema_version":"1","request_id":%q,"priority":3,"text":"Проверка связи"}`, id))
}

type jobOpt func(*core.NewJob)

func withDedup(k string) jobOpt { return func(j *core.NewJob) { j.DedupKey = k } }
func withMaxTries(n int) jobOpt { return func(j *core.NewJob) { j.MaxTries = n } }
func withRef(rt string, id uuid.UUID) jobOpt {
	return func(j *core.NewJob) { j.RefType, j.RefID = rt, id }
}
func withPriority(p int) jobOpt       { return func(j *core.NewJob) { j.Priority = p } }
func withRunAfter(t time.Time) jobOpt { return func(j *core.NewJob) { j.RunAfter = t } }

// enqueue ставит задачу вне транзакции и возвращает её id.
func enqueue(t *testing.T, s *Service, typ core.JobType, opts ...jobOpt) uuid.UUID {
	t.Helper()
	id := ids.New()
	j := core.NewJob{ID: id, Type: typ, Priority: 3, Payload: rawPayload(id)}
	for _, o := range opts {
		o(&j)
	}
	got, created, err := s.Enqueue(context.Background(), s.pool, j)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if !created || got != id {
		t.Fatalf("enqueue: created=%v id=%s, want new %s", created, got, id)
	}
	return id
}

// jobState — строка ai_jobs для проверок.
type jobState struct {
	ID         uuid.UUID
	Status     string
	TryCount   int
	MaxTries   int
	Priority   int
	Error      *string
	RequestID  string
	ResultCode *string
	Result     []byte
	LockedBy   *string
	LockedAt   *time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
	RunAfter   time.Time
	CreatedAt  time.Time
}

func readJob(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) jobState {
	t.Helper()
	var j jobState
	err := pool.QueryRow(context.Background(), `
SELECT id, status, try_count, max_tries, priority, error, payload ->> 'request_id', result #>> '{error,code}',
       result, locked_by, locked_at, started_at, finished_at, run_after, created_at
  FROM ai_jobs WHERE id = $1`, id).Scan(&j.ID, &j.Status, &j.TryCount, &j.MaxTries, &j.Priority, &j.Error,
		&j.RequestID, &j.ResultCode, &j.Result, &j.LockedBy, &j.LockedAt, &j.StartedAt, &j.FinishedAt, &j.RunAfter, &j.CreatedAt)
	if err != nil {
		t.Fatalf("read job %s: %v", id, err)
	}
	return j
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// markRunning — задача «отправлена» этим инстансом ago назад (started_at — startedAgo назад).
func markRunning(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, lockedAgo, startedAgo time.Duration) {
	t.Helper()
	exec(t, pool, `UPDATE ai_jobs SET status = 'running', locked_by = $2,
	        locked_at = now() - make_interval(secs => $3), started_at = now() - make_interval(secs => $4)
	  WHERE id = $1`, id, testInstance, lockedAgo.Seconds(), startedAgo.Seconds())
}

func strp(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// ---------------------------------------------------------------- обработчик результатов

type failCall struct {
	Job     core.JobRecord
	Code    string
	Message string
}

// recHandler — core.AIResultHandler, записывающий вызовы.
type recHandler struct {
	mu        sync.Mutex
	results   []core.JobRecord
	failures  []failCall
	resultErr error
}

func (h *recHandler) ApplyResult(_ context.Context, _ pgx.Tx, job core.JobRecord, _ *callbacks.AiResult) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.resultErr != nil {
		return h.resultErr
	}
	h.results = append(h.results, job)
	return nil
}

func (h *recHandler) ApplyFailure(_ context.Context, _ pgx.Tx, job core.JobRecord, code, message string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failures = append(h.failures, failCall{Job: job, Code: code, Message: message})
	return nil
}

func (h *recHandler) counts() (results, failures int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.results), len(h.failures)
}

func (h *recHandler) lastFailure(t *testing.T) failCall {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.failures) == 0 {
		t.Fatal("ApplyFailure was not called")
	}
	return h.failures[len(h.failures)-1]
}

// registerAll — один обработчик на все типы.
func registerAll(s *Service) *recHandler {
	h := &recHandler{}
	for _, typ := range jobTypes {
		s.Register(typ, h)
	}
	return h
}

// ---------------------------------------------------------------- callback'и

var resultField = map[core.JobType]string{
	core.JobEvaluateGrammar:  "grammar",
	core.JobEvaluateSemantic: "semantic",
	core.JobEvaluateDialogue: "dialogue",
	core.JobGenerateScenario: "scenario",
	core.JobTTS:              "tts",
}

func okCallback(reqID uuid.UUID, typ core.JobType) string {
	return fmt.Sprintf(`{"schema_version":"1","request_id":%q,"type":%q,"status":"ok","engine":{"duration_ms":1500},%q:{}}`,
		reqID, typ, resultField[typ])
}

func failedCallback(reqID uuid.UUID, typ core.JobType, code string, retryable bool) string {
	return fmt.Sprintf(`{"schema_version":"1","request_id":%q,"type":%q,"status":"failed","engine":{"duration_ms":10},`+
		`"error":{"code":%q,"message":"Модель не ответила вовремя","retryable":%v}}`, reqID, typ, code, retryable)
}

func postCallback(t *testing.T, s *Service, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/ai/v1/results", strings.NewReader(body))
	req.Header.Set("X-Internal-Token", testToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.CallbackHandler().ServeHTTP(rec, req)
	return rec
}

type callbackReply struct {
	Accepted  bool `json:"accepted"`
	Duplicate bool `json:"duplicate"`
}

func mustCallback(t *testing.T, s *Service, body string) callbackReply {
	t.Helper()
	rec := postCallback(t, s, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("callback: status %d, body %s", rec.Code, rec.Body)
	}
	var r callbackReply
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("callback reply: %v (%s)", err, rec.Body)
	}
	return r
}

// ---------------------------------------------------------------- фейковый ai-service

type seenRequest struct {
	Method         string
	Path           string
	Token          string
	IdempotencyKey string
	ContentType    string
	Body           []byte
}

// fakeAI — httptest-сервер с программируемым ответом; запоминает запросы.
type fakeAI struct {
	*httptest.Server
	mu      sync.Mutex
	seen    []seenRequest
	respond func(w http.ResponseWriter, r *http.Request, body []byte)
}

func newFakeAI(t testing.TB, respond func(w http.ResponseWriter, r *http.Request, body []byte)) *fakeAI {
	t.Helper()
	f := &fakeAI{respond: respond}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.seen = append(f.seen, seenRequest{Method: r.Method, Path: r.URL.Path, Token: r.Header.Get("X-Internal-Token"),
			IdempotencyKey: r.Header.Get("Idempotency-Key"), ContentType: r.Header.Get("Content-Type"), Body: body})
		fn := f.respond
		f.mu.Unlock()
		fn(w, r, body)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAI) setRespond(fn func(w http.ResponseWriter, r *http.Request, body []byte)) {
	f.mu.Lock()
	f.respond = fn
	f.mu.Unlock()
}

func (f *fakeAI) requests() []seenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seenRequest(nil), f.seen...)
}

func replyStatus(code int, headers map[string]string, body string) func(http.ResponseWriter, *http.Request, []byte) {
	return func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

// accepted202 — ответ ai-service на задачу: 202 JobAccepted с request_id из тела.
func accepted202(w http.ResponseWriter, _ *http.Request, body []byte) {
	var in struct {
		RequestID string `json:"request_id"`
	}
	_ = json.Unmarshal(body, &in)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = fmt.Fprintf(w, `{"request_id":%q,"status":"queued","queue_position":0,"est_wait_sec":3}`, in.RequestID)
}

// dispatchOnce — один раунд диспетчера и ожидание итогов отправок.
func dispatchOnce(s *Service) {
	s.dispatchRound(context.Background())
	s.sends.Wait()
}
