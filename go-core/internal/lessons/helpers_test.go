package lessons

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/settings"
)

// ---------------------------------------------------------------- фейки портов

// pubRecorder — core.Publisher, запоминающий сообщения (потокобезопасно: OnCommit-хуки и
// SetOnline зовутся из горутин хендлеров).
type pubRecorder struct {
	mu      sync.Mutex
	monitor []monitorMsg
	student []studentMsg
}

type monitorMsg struct {
	LessonID uuid.UUID
	Msg      public.MonitorMessage
}

type studentMsg struct {
	AttemptID uuid.UUID
	Msg       public.StudentMessage
}

func (p *pubRecorder) Monitor(lessonID uuid.UUID, m public.MonitorMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.monitor = append(p.monitor, monitorMsg{lessonID, m})
}

func (p *pubRecorder) Student(attemptID uuid.UUID, m public.StudentMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.student = append(p.student, studentMsg{attemptID, m})
}

func (p *pubRecorder) monitorMsgs() []monitorMsg {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]monitorMsg(nil), p.monitor...)
}

func (p *pubRecorder) studentMsgs() []studentMsg {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]studentMsg(nil), p.student...)
}

func (p *pubRecorder) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.monitor, p.student = nil, nil
}

// countMonitor — сколько сообщений типа typ ушло в монитор занятия.
func (p *pubRecorder) countMonitor(lessonID uuid.UUID, typ public.MonitorMessageType) int {
	n := 0
	for _, m := range p.monitorMsgs() {
		if m.LessonID == lessonID && m.Msg.Type == typ {
			n++
		}
	}
	return n
}

// auditRecorder — core.Auditor в память.
type auditRecorder struct {
	mu      sync.Mutex
	entries []core.AuditEntry
}

func (a *auditRecorder) Log(_ context.Context, e core.AuditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
}

func (a *auditRecorder) byAction(action string) []core.AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []core.AuditEntry
	for _, e := range a.entries {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// stubAuth — субъект из заголовков: X-Test-Role (пусто — нет сессии, blocked — заблокирован)
// и X-Test-User (UUID; пусто — случайный).
type stubAuth struct{}

func (stubAuth) Authenticate(r *http.Request) (*core.Principal, error) {
	role := r.Header.Get("X-Test-Role")
	switch role {
	case "":
		return nil, core.ErrUnauthenticated
	case "blocked":
		return nil, core.ErrUserBlocked
	}
	id := uuid.New()
	if v := r.Header.Get("X-Test-User"); v != "" {
		id = uuid.MustParse(v)
	}
	return &core.Principal{UserID: id, Role: core.Role(role), Login: role, LastName: "Тестов", FirstName: "Тест"}, nil
}

// who — субъект запроса в тестах (нулевой — без сессии).
type who struct {
	Role core.Role
	ID   uuid.UUID
}

func asRole(role core.Role) who { return who{Role: role} }

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testEnv — сервис занятий за роутером /api/v1 и его фейки.
type testEnv struct {
	svc   *Service
	pub   *pubRecorder
	audit *auditRecorder
	srv   *httptest.Server
	pool  *pgxpool.Pool
}

func newEnv(t *testing.T, pool *pgxpool.Pool, st *settings.Store) *testEnv {
	t.Helper()
	e := &testEnv{pub: &pubRecorder{}, audit: &auditRecorder{}, pool: pool}
	e.svc = New(Deps{Pool: pool, Settings: st, Publisher: e.pub, Auditor: e.audit, Log: discardLog()})
	mux := http.NewServeMux()
	r := httpx.NewRouter(mux, "/api/v1", stubAuth{}, discardLog(), nil)
	e.svc.Register(r)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

type resp struct {
	status int
	body   []byte
}

// do — запрос к API. body: nil — без тела, string — как есть, иначе JSON.
func (e *testEnv) do(t *testing.T, method, path string, as who, body any) resp {
	t.Helper()
	r, err := e.send(method, path, as, body)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// send — то же без *testing.T (для горутин: t.Fatal там нельзя).
func (e *testEnv) send(method, path string, as who, body any) (resp, error) {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return resp{}, err
		}
		rd = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, e.srv.URL+"/api/v1"+path, rd)
	if err != nil {
		return resp{}, err
	}
	if as.Role != "" {
		req.Header.Set("X-Test-Role", string(as.Role))
	}
	if as.ID != uuid.Nil {
		req.Header.Set("X-Test-User", as.ID.String())
	}
	if method != http.MethodGet {
		req.Header.Set("X-Requested-With", "fetch")
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := e.srv.Client().Do(req)
	if err != nil {
		return resp{}, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return resp{}, err
	}
	return resp{status: res.StatusCode, body: b}, nil
}

type apiError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

func (a apiError) field(name string) string {
	f, _ := a.Details["fields"].(map[string]any)
	s, _ := f[name].(string)
	return s
}

// expectError — ApiError с нужным статусом и кодом (сообщение по-русски не пустое).
func expectError(t *testing.T, r resp, status int, code string) apiError {
	t.Helper()
	if r.status != status {
		t.Fatalf("status %d, want %d: %s", r.status, status, r.body)
	}
	var e apiError
	if err := json.Unmarshal(r.body, &e); err != nil {
		t.Fatalf("ApiError %s: %v", r.body, err)
	}
	if e.Code != code || e.Message == "" {
		t.Fatalf("ApiError %+v, want code %q and a message", e, code)
	}
	return e
}

// decode — тело успешного ответа со статусом status.
func decode[T any](t *testing.T, r resp, status int) T {
	t.Helper()
	if r.status != status {
		t.Fatalf("status %d, want %d: %s", r.status, status, r.body)
	}
	var v T
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("decode %T from %s: %v", v, r.body, err)
	}
	return v
}

// rawKeys — ключи JSON-объекта ответа (проверка «обязательные поля есть, массивы не null»).
func rawKeys(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("object %s: %v", body, err)
	}
	return m
}

func requireArray(t *testing.T, obj map[string]json.RawMessage, key string) {
	t.Helper()
	v, ok := obj[key]
	if !ok {
		t.Fatalf("нет поля %q", key)
	}
	if s := strings.TrimSpace(string(v)); !strings.HasPrefix(s, "[") {
		t.Fatalf("поле %q = %s, ожидается массив (не null)", key, s)
	}
}

func ptr[T any](v T) *T { return &v }
