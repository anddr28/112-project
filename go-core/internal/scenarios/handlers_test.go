package scenarios

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"lct/gocore/internal/platform/httpx"
)

// HTTP-тесты без БД: маршруты, RBAC, CSRF и проверки ввода, которые срабатывают до
// первого обращения к PostgreSQL (пул не задан — дошедший до БД запрос дал бы 500).

func unitMux(q *fakeQueue) http.Handler {
	d := Deps{Catalog: newStubCatalog(), Log: discardLog()}
	if q != nil {
		d.Queue = q
	}
	return newMux(New(d))
}

type route struct{ method, path string }

var allRoutes = []route{
	{http.MethodGet, "/scenarios"},
	{http.MethodPost, "/scenarios"},
	{http.MethodPost, "/scenarios/generate"},
	{http.MethodGet, "/scenarios/" + uuid.NewString()},
	{http.MethodPatch, "/scenarios/" + uuid.NewString()},
	{http.MethodPost, "/scenarios/" + uuid.NewString() + "/approve"},
	{http.MethodPost, "/scenarios/" + uuid.NewString() + "/reject"},
	{http.MethodPost, "/scenarios/" + uuid.NewString() + "/versions"},
	{http.MethodPost, "/scenarios/" + uuid.NewString() + "/tts-preview"},
	{http.MethodGet, "/scenarios/export"},
	{http.MethodPost, "/scenarios/import"},
	{http.MethodPost, "/attempts/" + uuid.NewString() + "/to-scenario"},
}

func TestRoutesRegistered(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	r := httpx.NewRouter(mux, "/api/v1", stubAuth{}, discardLog(), nil)
	New(Deps{}).Register(r)
	got := r.Routes()
	want := []string{
		"GET /scenarios", "POST /scenarios", "POST /scenarios/generate", "GET /scenarios/{scenarioId}",
		"PATCH /scenarios/{scenarioId}", "POST /scenarios/{scenarioId}/approve", "POST /scenarios/{scenarioId}/reject",
		"POST /scenarios/{scenarioId}/versions", "POST /scenarios/{scenarioId}/tts-preview",
		"GET /scenarios/export", "POST /scenarios/import", "POST /attempts/{attemptId}/to-scenario",
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("routes %v, want %v", got, want)
	}
}

func TestAccessControl(t *testing.T) {
	t.Parallel()
	mux := unitMux(nil)
	for _, rt := range allRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			t.Parallel()
			expectError(t, call(t, mux, rt.method, rt.path, "", uuid.Nil, "{}"), http.StatusUnauthorized, httpx.CodeUnauthorized)
			expectError(t, call(t, mux, rt.method, rt.path, "blocked", uuid.Nil, "{}"), http.StatusLocked, httpx.CodeUserBlocked)
			expectError(t, call(t, mux, rt.method, rt.path, "student", uuid.Nil, "{}"), http.StatusForbidden, httpx.CodeForbidden)
		})
	}
}

// Мутирующие запросы без X-Requested-With: fetch — 403 ещё до аутентификации.
func TestCSRFLock(t *testing.T) {
	t.Parallel()
	mux := unitMux(nil)
	for _, rt := range allRoutes {
		if rt.method == http.MethodGet {
			continue
		}
		req, _ := http.NewRequest(rt.method, "/api/v1"+rt.path, strings.NewReader("{}"))
		req.Header.Set("X-Test-Role", "teacher")
		req.Header.Set("Content-Type", "application/json")
		rec := newRecorder()
		mux.ServeHTTP(rec, req)
		expectError(t, testResp{status: rec.Code, body: rec.Body.Bytes()}, http.StatusForbidden, httpx.CodeForbidden)
	}
}

func TestBadScenarioIDIsNotFound(t *testing.T) {
	t.Parallel()
	mux := unitMux(nil)
	for _, rt := range []route{
		{http.MethodGet, "/scenarios/not-a-uuid"},
		{http.MethodPatch, "/scenarios/not-a-uuid"},
		{http.MethodPost, "/scenarios/123/approve"},
		{http.MethodPost, "/scenarios/xyz/reject"},
		{http.MethodPost, "/scenarios/xyz/versions"},
		{http.MethodPost, "/scenarios/xyz/tts-preview"},
	} {
		for _, role := range []string{"teacher", "admin"} {
			r := call(t, mux, rt.method, rt.path, role, uuid.Nil, `{"reason":"причина","text":"Алло"}`)
			expectError(t, r, http.StatusNotFound, httpx.CodeNotFound)
		}
	}
}

func TestCreateValidation(t *testing.T) {
	t.Parallel()
	mux := unitMux(nil)
	cases := []struct {
		name   string
		body   string
		status int
		fields []string
	}{
		{"пустое тело", "", http.StatusBadRequest, nil},
		{"битый JSON", `{"title":`, http.StatusBadRequest, nil},
		{"сложность строкой", `{"title":"Пожар","categoryId":"101","difficulty":"2","mode":"cards"}`, http.StatusBadRequest, nil},
		{"всё неверно", `{"title":" аб ","categoryId":"nope","difficulty":7,"mode":"voice"}`, http.StatusBadRequest,
			[]string{"title", "categoryId", "difficulty", "mode"}},
		{"пустые поля", `{}`, http.StatusBadRequest, []string{"title", "categoryId", "difficulty", "mode"}},
		{"длинное название", `{"title":"` + strings.Repeat("ы", 301) + `","categoryId":"it-101","difficulty":1,"mode":"cards"}`,
			http.StatusBadRequest, []string{"title"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := call(t, mux, http.MethodPost, "/scenarios", "teacher", uuid.Nil, tc.body)
			expectError(t, r, tc.status, httpx.CodeValidation, tc.fields...)
		})
	}
	// Не JSON — 415.
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/scenarios", strings.NewReader("title=x"))
	req.Header.Set("X-Test-Role", "teacher")
	req.Header.Set("X-Requested-With", "fetch")
	req.Header.Set("Content-Type", "text/plain")
	rec := newRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain: status %d", rec.Code)
	}
}

func TestListValidation(t *testing.T) {
	t.Parallel()
	mux := unitMux(nil)
	r := call(t, mux, http.MethodGet, "/scenarios?status=deleted&mode=voice", "teacher", uuid.Nil, nil)
	expectError(t, r, http.StatusBadRequest, httpx.CodeValidation, "status", "mode")

	// Неизвестная категория — пустой список, а не ошибка (и без обращения к БД).
	r = call(t, mux, http.MethodGet, "/scenarios?categoryId=it-unknown", "admin", uuid.Nil, nil)
	if r.status != http.StatusOK || strings.TrimSpace(string(r.body)) != "[]" {
		t.Fatalf("unknown category: %d %s", r.status, r.body)
	}
}

func TestGenerateValidationAndAvailability(t *testing.T) {
	t.Parallel()
	q := &fakeQueue{unavailable: true}
	mux := unitMux(q)
	long := strings.Repeat("к", commentMax+1)

	r := call(t, mux, http.MethodPost, "/scenarios/generate", "teacher", uuid.Nil,
		map[string]any{"categoryId": "nope", "difficulty": 0, "mode": "x", "teacherComment": long})
	expectError(t, r, http.StatusBadRequest, httpx.CodeValidation, "categoryId", "difficulty", "mode", "teacherComment")

	// Валидный запрос при открытом breaker'е — 503 сразу, задача не ставится.
	ok := map[string]any{"categoryId": "it-101", "difficulty": 2, "mode": "both", "teacherComment": "  "}
	r = call(t, mux, http.MethodPost, "/scenarios/generate", "teacher", uuid.Nil, ok)
	expectError(t, r, http.StatusServiceUnavailable, httpx.CodeAIUnavailable)
	if len(q.jobs) != 0 {
		t.Fatalf("jobs enqueued while unavailable: %+v", q.jobs)
	}

	// Очереди нет вовсе — тоже 503.
	r = call(t, unitMux(nil), http.MethodPost, "/scenarios/generate", "admin", uuid.Nil, ok)
	expectError(t, r, http.StatusServiceUnavailable, httpx.CodeAIUnavailable)
}

func TestPatchValidation(t *testing.T) {
	t.Parallel()
	mux := unitMux(nil)
	path := "/scenarios/" + uuid.NewString()
	r := call(t, mux, http.MethodPatch, path, "teacher", uuid.Nil,
		map[string]any{"title": "  я ", "difficulty": 4, "mode": "voice", "teacherComment": strings.Repeat("ё", 2001),
			"callScript": map[string]any{"caller": map[string]any{}, "address": map[string]any{}, "keyFacts": []string{}, "turns": []any{},
				"dialogue": map[string]any{"persona": "p", "facts": []any{}, "maxTurns": 41}}})
	expectError(t, r, http.StatusBadRequest, httpx.CodeValidation,
		"title", "difficulty", "mode", "teacherComment", "callScript.dialogue.maxTurns")

	r = call(t, mux, http.MethodPatch, path, "teacher", uuid.Nil, `{"title":`)
	expectError(t, r, http.StatusBadRequest, httpx.CodeValidation)
}

func TestRejectValidation(t *testing.T) {
	t.Parallel()
	mux := unitMux(nil)
	path := "/scenarios/" + uuid.NewString() + "/reject"
	for _, body := range []string{`{}`, `{"reason":"  аб  "}`, `{"reason":"` + strings.Repeat("п", 2001) + `"}`} {
		r := call(t, mux, http.MethodPost, path, "teacher", uuid.Nil, body)
		expectError(t, r, http.StatusBadRequest, httpx.CodeValidation, "reason")
	}
	expectError(t, call(t, mux, http.MethodPost, path, "teacher", uuid.Nil, ""), http.StatusBadRequest, httpx.CodeValidation)
}

func TestTTSPreviewValidation(t *testing.T) {
	t.Parallel()
	mux := unitMux(nil)
	path := "/scenarios/" + uuid.NewString() + "/tts-preview"
	for _, body := range []string{`{}`, `{"text":"   "}`, `{"text":"` + strings.Repeat("а", previewTextMax+1) + `"}`} {
		r := call(t, mux, http.MethodPost, path, "teacher", uuid.Nil, body)
		expectError(t, r, http.StatusBadRequest, httpx.CodeValidation, "text")
	}
}
