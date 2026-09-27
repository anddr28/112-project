package evaluation

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
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/pg"
)

// ---------------------------------------------------------------- фейки портов

// stubAuth — core.Authenticator: субъект по заголовку X-Test-User (ключ карты); нет — 401.
type stubAuth struct{ users map[string]*core.Principal }

func (a *stubAuth) Authenticate(r *http.Request) (*core.Principal, error) {
	p, ok := a.users[r.Header.Get("X-Test-User")]
	if !ok {
		return nil, core.ErrUnauthenticated
	}
	return p, nil
}

// pubRec — core.Publisher: запоминает всё, что ушло в WS.
type pubRec struct {
	mu      sync.Mutex
	monitor []monitorMsg
	student []studentMsg
}

type monitorMsg struct {
	Lesson uuid.UUID
	Msg    public.MonitorMessage
}

type studentMsg struct {
	Attempt uuid.UUID
	Msg     public.StudentMessage
}

func (p *pubRec) Monitor(lessonID uuid.UUID, m public.MonitorMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.monitor = append(p.monitor, monitorMsg{lessonID, m})
}

func (p *pubRec) Student(attemptID uuid.UUID, m public.StudentMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.student = append(p.student, studentMsg{attemptID, m})
}

func (p *pubRec) monitorOf(typ public.MonitorMessageType) []monitorMsg {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []monitorMsg
	for _, m := range p.monitor {
		if m.Msg.Type == typ {
			out = append(out, m)
		}
	}
	return out
}

func (p *pubRec) studentFor(attemptID uuid.UUID) []studentMsg {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []studentMsg
	for _, m := range p.student {
		if m.Attempt == attemptID {
			out = append(out, m)
		}
	}
	return out
}

func (p *pubRec) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.monitor, p.student = nil, nil
}

// queueRec — core.JobQueue: задачи не пишутся в ai_jobs, а запоминаются вместе с payload.
type queueRec struct {
	mu   sync.Mutex
	jobs []core.NewJob
}

func (q *queueRec) Enqueue(_ context.Context, _ pg.Querier, j core.NewJob) (uuid.UUID, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.jobs = append(q.jobs, j)
	return j.ID, true, nil
}

func (q *queueRec) Available() bool             { return true }
func (q *queueRec) EstWaitSec(core.JobType) int { return 0 }

func (q *queueRec) byAttempt(attemptID uuid.UUID) map[core.JobType]core.NewJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := map[core.JobType]core.NewJob{}
	for _, j := range q.jobs {
		if j.RefID == attemptID {
			out[j.Type] = j
		}
	}
	return out
}

func (q *queueRec) count() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.jobs)
}

// auditRec — core.Auditor.
type auditRec struct {
	mu      sync.Mutex
	entries []core.AuditEntry
}

func (a *auditRec) Log(_ context.Context, e core.AuditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
}

func (a *auditRec) byAction(action string) []core.AuditEntry {
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

// ---------------------------------------------------------------- HTTP

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newRouter — маршруты пакета на httpx.Router со stub-аутентификацией.
func newRouter(s *Service, users map[string]*core.Principal) http.Handler {
	mux := http.NewServeMux()
	rt := httpx.NewRouter(mux, "/api/v1", &stubAuth{users: users}, discardLog(), nil)
	s.Register(rt)
	return mux
}

type resp struct {
	Code int
	Body []byte
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("response is not a JSON object: %v\n%s", err, r.Body)
	}
	return m
}

func (r resp) list(t *testing.T) []map[string]any {
	t.Helper()
	var m []map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("response is not a JSON array: %v\n%s", err, r.Body)
	}
	return m
}

// do — запрос к роутеру. user — ключ stubAuth ("" — без сессии). Мутирующие запросы
// получают X-Requested-With: fetch, если csrf=true.
func do(h http.Handler, method, path, user string, body any, csrf bool) resp {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	if csrf {
		req.Header.Set("X-Requested-With", "fetch")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return resp{Code: w.Code, Body: w.Body.Bytes()}
}

// apiErr — {code, message, details} из ответа-ошибки.
func apiErr(t *testing.T, r resp) (code, msg string, fields map[string]any) {
	t.Helper()
	m := r.json(t)
	code, _ = m["code"].(string)
	msg, _ = m["message"].(string)
	if d, ok := m["details"].(map[string]any); ok {
		fields, _ = d["fields"].(map[string]any)
	}
	return code, msg, fields
}

// requireKeys — обязательные поля контракта присутствуют в JSON-объекте.
func requireKeys(t *testing.T, m map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Errorf("required key %q missing in %v", k, m)
		}
	}
}

// requireArray — поле — JSON-массив (не null).
func requireArray(t *testing.T, m map[string]any, key string) []any {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("key %q missing", key)
	}
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("key %q = %#v, want JSON array (not null)", key, v)
	}
	return arr
}

// walkKeys — все ключи объектов на любой глубине.
func walkKeys(v any, fn func(key string)) {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			fn(k)
			walkKeys(x, fn)
		}
	case []any:
		for _, x := range t {
			walkKeys(x, fn)
		}
	}
}
