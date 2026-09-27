package aijobs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/core"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/ids"
)

// stubAuth — core.Authenticator: роль из заголовка X-Test-Role, без заголовка — 401.
type stubAuth struct{}

func (stubAuth) Authenticate(r *http.Request) (*core.Principal, error) {
	role := r.Header.Get("X-Test-Role")
	if role == "" {
		return nil, core.ErrUnauthenticated
	}
	return &core.Principal{UserID: ids.New(), Role: core.Role(role), Login: role, LastName: "Иванов", FirstName: "Пётр"}, nil
}

func newRouter(s *Service) http.Handler {
	mux := http.NewServeMux()
	rt := httpx.NewRouter(mux, "/api/v1", stubAuth{}, discardLog(), nil)
	s.RegisterRoutes(rt)
	return mux
}

func getJobHTTP(t *testing.T, h http.Handler, role, id string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/ai-jobs/"+id, nil)
	if role != "" {
		req.Header.Set("X-Test-Role", role)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec, body
}

func TestGetJobAccess(t *testing.T) {
	t.Parallel()
	s, _ := newDBService(t, "")
	h := newRouter(s)
	id := enqueue(t, s, core.JobGenerateScenario).String()
	cases := []struct {
		name, role, id string
		status         int
		code           string
	}{
		{"anonymous", "", id, http.StatusUnauthorized, "unauthorized"},
		{"student", string(core.RoleStudent), id, http.StatusForbidden, "forbidden"},
		{"teacher", string(core.RoleTeacher), id, http.StatusOK, ""},
		{"admin", string(core.RoleAdmin), id, http.StatusOK, ""},
		{"bad uuid", string(core.RoleTeacher), "not-a-uuid", http.StatusNotFound, "not_found"},
		{"unknown job", string(core.RoleTeacher), ids.New().String(), http.StatusNotFound, "not_found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, body := getJobHTTP(t, h, c.role, c.id)
			if rec.Code != c.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.status, rec.Body)
			}
			if c.code != "" && (body["code"] != c.code || body["message"] == "") {
				t.Errorf("error body %v", body)
			}
		})
	}
}

func TestGetJobShapes(t *testing.T) {
	t.Parallel()
	s, pool := newDBService(t, "")
	h := newRouter(s)
	ref := ids.New()

	// queued: позиция и ожидание; впереди — задача той же полосы с более высоким приоритетом.
	enqueue(t, s, core.JobEvaluateSemantic, withPriority(1))
	queued := enqueue(t, s, core.JobGenerateScenario, withPriority(core.PriorityGenerate), withRef(core.RefScenario, ref))
	rec, body := getJobHTTP(t, h, string(core.RoleTeacher), queued.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	for _, k := range []string{"id", "type", "status", "createdAt", "tryCount", "queuePosition", "estWaitSec", "refType", "refId"} {
		if _, ok := body[k]; !ok {
			t.Errorf("queued job: no %q in %v", k, body)
		}
	}
	if body["id"] != queued.String() || body["type"] != "generate_scenario" || body["status"] != "queued" ||
		body["queuePosition"] != float64(2) || body["refType"] != core.RefScenario || body["refId"] != ref.String() {
		t.Errorf("queued job body %v", body)
	}
	if _, ok := body["error"]; ok {
		t.Error("error present on a healthy job")
	}
	if _, ok := body["finishedAt"]; ok {
		t.Error("finishedAt present on a queued job")
	}

	// running.
	markRunning(t, pool, queued, time.Second, time.Second)
	_, body = getJobHTTP(t, h, string(core.RoleAdmin), queued.String())
	if body["status"] != "running" {
		t.Errorf("running body %v", body)
	}
	if _, ok := body["queuePosition"]; ok {
		t.Error("queuePosition on a running job")
	}
	if w, ok := body["estWaitSec"].(float64); !ok || w < 1 {
		t.Errorf("running estWaitSec %v", body["estWaitSec"])
	}

	// failed: человекочитаемая ошибка без технических подробностей, finishedAt.
	exec(t, pool, `UPDATE ai_jobs SET status = 'failed', finished_at = now(), try_count = 3,
	        error = 'llm_timeout: ollama read timeout after 120s (HTTP 500)' WHERE id = $1`, queued)
	_, body = getJobHTTP(t, h, string(core.RoleTeacher), queued.String())
	if body["status"] != "failed" || body["error"] != humanError("llm_timeout") || body["tryCount"] != float64(3) {
		t.Errorf("failed body %v", body)
	}
	if _, ok := body["finishedAt"]; !ok {
		t.Error("finishedAt missing on a failed job")
	}
	for _, k := range []string{"queuePosition", "estWaitSec"} {
		if _, ok := body[k]; ok {
			t.Errorf("%s on a finished job", k)
		}
	}
}

func TestJobViewNotFoundIsNoRows(t *testing.T) {
	t.Parallel()
	s, pool := newDBService(t, "")
	_, err := s.JobView(context.Background(), pool, ids.New())
	var he *httpx.Error
	if !errors.As(err, &he) || he.Status != http.StatusNotFound || !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("err = %v", err)
	}
}

// Задача, отложенная backoff'ом, ждёт дольше соседей по очереди.
func TestJobViewQueuedWithBackoff(t *testing.T) {
	t.Parallel()
	s, pool := newDBService(t, "")
	now := enqueue(t, s, core.JobTTS)
	later := enqueue(t, s, core.JobTTS, withRunAfter(time.Now().Add(2*time.Minute)))
	a, err := s.JobView(context.Background(), pool, now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.JobView(context.Background(), pool, later)
	if err != nil {
		t.Fatal(err)
	}
	if *a.QueuePosition != 1 || *b.QueuePosition != 2 {
		t.Errorf("positions %d, %d", *a.QueuePosition, *b.QueuePosition)
	}
	if *b.EstWaitSec < 110 || *b.EstWaitSec <= *a.EstWaitSec {
		t.Errorf("wait %d vs %d", *b.EstWaitSec, *a.EstWaitSec)
	}
}
