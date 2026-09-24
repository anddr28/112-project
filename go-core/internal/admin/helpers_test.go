package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// stubAuth — роль из X-Test-Role (пусто — нет сессии, blocked — заблокирован), id — из
// X-Test-User (иначе случайный).
type stubAuth struct{}

func (stubAuth) Authenticate(r *http.Request) (*core.Principal, error) {
	switch role := r.Header.Get("X-Test-Role"); role {
	case "":
		return nil, core.ErrUnauthenticated
	case "blocked":
		return nil, core.ErrUserBlocked
	default:
		id, err := uuid.Parse(r.Header.Get("X-Test-User"))
		if err != nil {
			id = uuid.New()
		}
		return &core.Principal{UserID: id, Role: core.Role(role), Login: role, LastName: "Админов", FirstName: "Админ"}, nil
	}
}

// ---------------------------------------------------------------- фейки портов

type fakeAI struct {
	mu        sync.Mutex
	health    *aiservice.Health
	healthErr error
	queue     *aiservice.QueueStatus
	queueErr  error
	breaker   string
	calls     int
	delay     time.Duration
}

func (f *fakeAI) DialogTurn(context.Context, *aiservice.DialogTurnRequest, *core.AudioInput) (*aiservice.DialogTurnResult, error) {
	return nil, errors.New("not used")
}

func (f *fakeAI) TTSSync(context.Context, *aiservice.TtsSyncRequest) (*components.TtsResult, error) {
	return nil, errors.New("not used")
}

func (f *fakeAI) Health(ctx context.Context) (*aiservice.Health, error) {
	f.mu.Lock()
	f.calls++
	d := f.delay
	h, err := f.health, f.healthErr
	f.mu.Unlock()
	if d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return h, err
}

func (f *fakeAI) Queue(context.Context) (*aiservice.QueueStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queue, f.queueErr
}

func (f *fakeAI) BreakerState() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.breaker
}

func (f *fakeAI) healthCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func healthyAI() *fakeAI {
	lt, ol, tts, stt := true, true, true, false
	models := []string{"qwen2.5:3b"}
	profiles := map[string]string{"dialog_fast": "qwen2.5:3b"}
	wait, avg, est := 2, 1500, 7
	return &fakeAI{
		health: &aiservice.Health{Status: aiservice.Ok, Languagetool: &lt, Ollama: &ol, Tts: &tts, Stt: &stt,
			ModelsAvailable: &models, Profiles: &profiles},
		queue:   &aiservice.QueueStatus{Pending: map[string]int{"evaluate_semantic": 3}, Running: 1, DialogWaiting: &wait, DialogAvgMs: &avg, EstWaitSec: &est},
		breaker: "closed",
	}
}

type fakeOps struct {
	mu       sync.Mutex
	list     []public.Backup
	listErr  error
	last     *time.Time
	lastErr  error
	startErr error
	started  []*uuid.UUID
}

func (f *fakeOps) StartBackup(_ context.Context, by *uuid.UUID) (public.Backup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return public.Backup{}, f.startErr
	}
	f.started = append(f.started, by)
	return public.Backup{Id: uuid.New(), StartedAt: time.Now().UTC(), Status: public.BackupStatus("running"), TriggeredBy: by}, nil
}

func (f *fakeOps) ListBackups(context.Context) ([]public.Backup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.list, f.listErr
}

func (f *fakeOps) LastBackupAt(context.Context) (*time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last, f.lastErr
}

var _ OpsAPI = (*fakeOps)(nil)

type fakeWS struct {
	conns   int
	lessons []uuid.UUID
}

func (f *fakeWS) Connections() int            { return f.conns }
func (f *fakeWS) WatchedLessons() []uuid.UUID { return f.lessons }

type fakeJobs struct {
	m   map[string]int
	err error
}

func (f *fakeJobs) Counts(context.Context) (map[string]int, error) { return f.m, f.err }

type monitorMsg struct {
	lesson uuid.UUID
	msg    public.MonitorMessage
}

type recPublisher struct {
	mu   sync.Mutex
	msgs []monitorMsg
	ch   chan struct{}
}

func newRecPublisher() *recPublisher { return &recPublisher{ch: make(chan struct{}, 64)} }

func (p *recPublisher) Monitor(id uuid.UUID, m public.MonitorMessage) {
	p.mu.Lock()
	p.msgs = append(p.msgs, monitorMsg{id, m})
	p.mu.Unlock()
	select {
	case p.ch <- struct{}{}:
	default:
	}
}

func (p *recPublisher) Student(uuid.UUID, public.StudentMessage) {}

func (p *recPublisher) all() []monitorMsg {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]monitorMsg(nil), p.msgs...)
}

type recAuditor struct {
	mu      sync.Mutex
	entries []core.AuditEntry
}

func (a *recAuditor) Log(_ context.Context, e core.AuditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
}

func (a *recAuditor) all() []core.AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]core.AuditEntry(nil), a.entries...)
}

// ---------------------------------------------------------------- HTTP

func newServer(t *testing.T, h *Handlers) (*httptest.Server, *httpx.Router) {
	t.Helper()
	mux := http.NewServeMux()
	r := httpx.NewRouter(mux, "/api/v1", stubAuth{}, discardLog(), nil)
	h.Register(r)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, r
}

type resp struct {
	status int
	header http.Header
	body   []byte
}

type reqOpt struct {
	role   string
	user   uuid.UUID
	noCSRF bool
}

func do(t *testing.T, srv *httptest.Server, method, path, body string, o reqOpt) resp {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+"/api/v1"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if o.role != "" {
		req.Header.Set("X-Test-Role", o.role)
	}
	if o.user != uuid.Nil {
		req.Header.Set("X-Test-User", o.user.String())
	}
	if method != http.MethodGet {
		if !o.noCSRF {
			req.Header.Set("X-Requested-With", "fetch")
		}
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp{status: res.StatusCode, header: res.Header, body: b}
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details struct {
		Fields map[string]string `json:"fields"`
	} `json:"details"`
}

func expectError(t *testing.T, r resp, status int, code string, fields ...string) apiError {
	t.Helper()
	if r.status != status {
		t.Fatalf("status %d, want %d: %s", r.status, status, r.body)
	}
	var e apiError
	if err := json.Unmarshal(r.body, &e); err != nil {
		t.Fatalf("ApiError body %s: %v", r.body, err)
	}
	if e.Code != code || e.Message == "" {
		t.Errorf("ApiError %+v, want code %q and a message", e, code)
	}
	for _, f := range fields {
		if e.Details.Fields[f] == "" {
			t.Errorf("details.fields.%s missing: %s", f, r.body)
		}
	}
	return e
}

func decode[T any](t *testing.T, r resp) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("decode %T from %s: %v", v, r.body, err)
	}
	return v
}

// ---------------------------------------------------------------- БД

// seedUser — пользователь с нужной ролью (FK settings.updated_by, audit actor).
func seedUser(t *testing.T, pool *pgxpool.Pool, role, last string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO users (id, login, password_hash, role, last_name, first_name, middle_name)
		VALUES ($1, $2, 'x', $3, $4, 'Иван', 'Петрович')`, id, "u"+id.String()[:8], role, last); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}
