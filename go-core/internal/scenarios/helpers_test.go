package scenarios

import (
	"bytes"
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

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/pg"
)

// Общие заготовки тестов пакета: фейки портов, заглушка аутентификации, каталог в памяти
// и HTTP-хелперы (запрос идёт через httpx.Router — CSRF, RBAC и ApiError как в бою).

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func ptr[T any](v T) *T { return &v }

// ---------------------------------------------------------------- аутентификация

// stubAuth — роль из X-Test-Role (пусто — нет сессии, blocked — заблокирован),
// id пользователя — из X-Test-User (DB-тестам нужен существующий users.id для FK).
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

// ---------------------------------------------------------------- фейки портов

// fakeQueue — очередь ai_jobs в памяти: запоминает задачи, дедуп по DedupKey как в
// aijobs.Queue (повтор — id существующей задачи и created=false).
type fakeQueue struct {
	mu          sync.Mutex
	unavailable bool
	err         error
	jobs        []core.NewJob
	dedup       map[string]uuid.UUID
}

func (q *fakeQueue) Enqueue(_ context.Context, _ pg.Querier, j core.NewJob) (uuid.UUID, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return uuid.Nil, false, q.err
	}
	if q.dedup == nil {
		q.dedup = map[string]uuid.UUID{}
	}
	if id, ok := q.dedup[j.DedupKey]; ok && j.DedupKey != "" {
		return id, false, nil
	}
	q.dedup[j.DedupKey] = j.ID
	q.jobs = append(q.jobs, j)
	return j.ID, true, nil
}

func (q *fakeQueue) Available() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return !q.unavailable
}

func (q *fakeQueue) EstWaitSec(core.JobType) int { return 42 }

func (q *fakeQueue) byType(t core.JobType) []core.NewJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []core.NewJob
	for _, j := range q.jobs {
		if j.Type == t {
			out = append(out, j)
		}
	}
	return out
}

// fakeAuditor — записи аудита в памяти.
type fakeAuditor struct {
	mu      sync.Mutex
	entries []core.AuditEntry
}

func (a *fakeAuditor) Log(_ context.Context, e core.AuditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
}

func (a *fakeAuditor) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.entries))
	for i, e := range a.entries {
		out[i] = e.Action
	}
	return out
}

func (a *fakeAuditor) count(action string) int {
	n := 0
	for _, x := range a.actions() {
		if x == action {
			n++
		}
	}
	return n
}

// fakeAI — синхронный ai-service: TTSSync отвечает res/err и считает вызовы.
type fakeAI struct {
	mu    sync.Mutex
	res   *components.TtsResult
	err   error
	calls []*aiservice.TtsSyncRequest
}

func (f *fakeAI) DialogTurn(context.Context, *aiservice.DialogTurnRequest, *core.AudioInput) (*aiservice.DialogTurnResult, error) {
	panic("DialogTurn is not used by scenarios")
}

func (f *fakeAI) TTSSync(_ context.Context, req *aiservice.TtsSyncRequest) (*components.TtsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if f.err != nil {
		return nil, f.err
	}
	if f.res != nil {
		r := *f.res
		return &r, nil
	}
	return &components.TtsResult{FilePath: "ab/" + req.TextHash + ".wav", DurationMs: 1500, TextHash: req.TextHash}, nil
}

func (f *fakeAI) Health(context.Context) (*aiservice.Health, error)     { return nil, nil }
func (f *fakeAI) Queue(context.Context) (*aiservice.QueueStatus, error) { return nil, nil }
func (f *fakeAI) BreakerState() string                                  { return "closed" }

func (f *fakeAI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// ---------------------------------------------------------------- каталог в памяти

// stubCatalog — классификатор для юнит-тестов: типы по uuid/коду/id фикстуры и службы.
type stubCatalog struct {
	types    []core.IncidentTypeInfo
	aliases  map[string]string // id фикстуры -> uuid
	services []core.ServiceInfo
}

func newStubCatalog() *stubCatalog {
	return &stubCatalog{
		types: []core.IncidentTypeInfo{
			{ID: "3021058b-40ef-4aed-9e0c-78b1e8184831", Code: "101", Name: "Пожар", Path: []string{"Пожар"}, Services: []string{"101", "103"}, Attributes: []string{"where"}},
			{ID: "397fea9c-c875-4996-8d79-cd89309230dc", Code: "104", Name: "Запах газа", Path: []string{"Газ", "Запах газа"}, Services: []string{"104"}},
		},
		aliases: map[string]string{"it-101": "3021058b-40ef-4aed-9e0c-78b1e8184831", "it-104": "397fea9c-c875-4996-8d79-cd89309230dc"},
		services: []core.ServiceInfo{
			{ID: "8bfd7c9a-436b-497f-a30a-f9c3d543eb02", Code: "101", Name: "Пожарная охрана", ShortName: "Служба 101"},
			{ID: "0d18413b-8748-4898-9ebc-a18f5d87ca75", Code: "103", Name: "Скорая помощь", ShortName: "Служба 103"},
			{ID: "a9990bd6-00f3-4267-8126-7f21c321291e", Code: "104", Name: "Газовая служба", ShortName: "Служба 104"},
			{ID: "f3e7a105-1f1d-4551-9d73-f1032eb2ce4e", Code: "zhkh", Name: "Департамент ЖКХ", ShortName: "Деп. ЖКХ"},
		},
	}
}

func (c *stubCatalog) TypeByID(id string) (core.IncidentTypeInfo, bool) {
	if u, ok := c.aliases[id]; ok {
		id = u
	}
	for _, t := range c.types {
		if t.ID == id {
			return t, true
		}
	}
	return core.IncidentTypeInfo{}, false
}

func (c *stubCatalog) TypeByCode(code string) (core.IncidentTypeInfo, bool) {
	for _, t := range c.types {
		if t.Code == code {
			return t, true
		}
	}
	return core.IncidentTypeInfo{}, false
}

func (c *stubCatalog) ServiceByID(id string) (core.ServiceInfo, bool) {
	for _, s := range c.services {
		if s.ID == id {
			return s, true
		}
	}
	return core.ServiceInfo{}, false
}

func (c *stubCatalog) ServiceByCode(code string) (core.ServiceInfo, bool) {
	for _, s := range c.services {
		if s.Code == code {
			return s, true
		}
	}
	return core.ServiceInfo{}, false
}

func (c *stubCatalog) FieldLabel(string) string                          { return "Поле карточки" }
func (c *stubCatalog) AttributeLabel(string) (string, bool)              { return "", false }
func (c *stubCatalog) AttributeValueLabel(string, string) (string, bool) { return "", false }

// ---------------------------------------------------------------- HTTP

type testResp struct {
	status int
	header http.Header
	body   []byte
}

func (r testResp) json(t *testing.T, dst any) {
	t.Helper()
	if err := json.Unmarshal(r.body, dst); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

// obj — тело как map (проверка формы ответа: обязательные поля, [] вместо null).
func (r testResp) obj(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	r.json(t, &m)
	return m
}

func newMux(svc *Service) *http.ServeMux {
	mux := http.NewServeMux()
	r := httpx.NewRouter(mux, "/api/v1", stubAuth{}, discardLog(), nil)
	svc.Register(r)
	return mux
}

// call — запрос к роутеру. role "" — без сессии; user — id из X-Test-User (uuid.Nil — случайный).
// body — string (как есть) или значение для json.Marshal; nil — без тела.
func call(t *testing.T, mux http.Handler, method, path, role string, user uuid.UUID, body any) testResp {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, "/api/v1"+path, rd)
	if role != "" {
		req.Header.Set("X-Test-Role", role)
	}
	if user != uuid.Nil {
		req.Header.Set("X-Test-User", user.String())
	}
	if method != http.MethodGet {
		req.Header.Set("X-Requested-With", "fetch")
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return testResp{status: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
}

type apiError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// expectError — ApiError с нужными статусом и кодом; fields — ключи details.fields.
func expectError(t *testing.T, r testResp, status int, code string, fields ...string) apiError {
	t.Helper()
	if r.status != status {
		t.Fatalf("status %d, want %d: %s", r.status, status, r.body)
	}
	var e apiError
	if err := json.Unmarshal(r.body, &e); err != nil {
		t.Fatalf("ApiError body %s: %v", r.body, err)
	}
	if e.Code != code || e.Message == "" {
		t.Fatalf("ApiError %+v, want code %q and a message", e, code)
	}
	if len(fields) > 0 {
		m, _ := e.Details["fields"].(map[string]any)
		for _, f := range fields {
			if s, _ := m[f].(string); s == "" {
				t.Errorf("details.fields.%s missing: %s", f, r.body)
			}
		}
	}
	return e
}

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }
