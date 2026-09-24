package evaluation

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
)

// Хендлеры без БД: всё, что отсекается до первого запроса к пулу (аутентификация, RBAC
// роутера, CSRF-замок, разбор пути и тела, валидация). Пул nil — обращение к нему упало бы.

func noDBRouter() http.Handler {
	s := New(Deps{Log: discardLog()})
	return newRouter(s, map[string]*core.Principal{
		"teacher": {UserID: uuid.New(), Role: core.RoleTeacher, LastName: "Учителев", FirstName: "Пётр"},
		"student": {UserID: uuid.New(), Role: core.RoleStudent, LastName: "Иванова", FirstName: "Анна"},
		"admin":   {UserID: uuid.New(), Role: core.RoleAdmin, LastName: "Админов", FirstName: "Иван"},
	})
}

func TestHandlersAuthAndRBAC(t *testing.T) {
	t.Parallel()
	h := noDBRouter()
	id := uuid.New().String()
	cases := []struct {
		name   string
		method string
		path   string
		user   string
		body   any
		csrf   bool
		status int
		code   string
	}{
		{"GET оценка без сессии", http.MethodGet, "/api/v1/attempts/" + id + "/evaluation", "", nil, false, 401, "unauthorized"},
		{"GET комментарии без сессии", http.MethodGet, "/api/v1/attempts/" + id + "/feedback", "", nil, false, 401, "unauthorized"},
		{"override без сессии", http.MethodPost, "/api/v1/attempts/" + id + "/evaluation/override", "", map[string]any{"score": 50, "reason": "Причина"}, true, 401, "unauthorized"},
		{"override студентом", http.MethodPost, "/api/v1/attempts/" + id + "/evaluation/override", "student", map[string]any{"score": 50, "reason": "Причина"}, true, 403, "forbidden"},
		{"комментарий студентом", http.MethodPost, "/api/v1/attempts/" + id + "/feedback", "student", map[string]any{"comment": "Хорошо"}, true, 403, "forbidden"},
		{"override без X-Requested-With", http.MethodPost, "/api/v1/attempts/" + id + "/evaluation/override", "teacher", map[string]any{"score": 50, "reason": "Причина"}, false, 403, "forbidden"},
		{"комментарий без X-Requested-With", http.MethodPost, "/api/v1/attempts/" + id + "/feedback", "teacher", map[string]any{"comment": "Хорошо"}, false, 403, "forbidden"},
		{"GET оценка: id не uuid", http.MethodGet, "/api/v1/attempts/not-a-uuid/evaluation", "admin", nil, false, 404, "not_found"},
		{"GET комментарии: id не uuid", http.MethodGet, "/api/v1/attempts/123/feedback", "admin", nil, false, 404, "not_found"},
		{"override: id не uuid", http.MethodPost, "/api/v1/attempts/xyz/evaluation/override", "teacher", map[string]any{"score": 50, "reason": "Причина"}, true, 404, "not_found"},
		{"комментарий: id не uuid", http.MethodPost, "/api/v1/attempts/xyz/feedback", "teacher", map[string]any{"comment": "Хорошо"}, true, 404, "not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := do(h, tc.method, tc.path, tc.user, tc.body, tc.csrf)
			if r.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", r.Code, tc.status, r.Body)
			}
			if code, msg, _ := apiErr(t, r); code != tc.code || msg == "" {
				t.Fatalf("ApiError = %q %q, want code %q and a message", code, msg, tc.code)
			}
		})
	}
}

func TestOverrideValidation(t *testing.T) {
	t.Parallel()
	h := noDBRouter()
	path := "/api/v1/attempts/" + uuid.New().String() + "/evaluation/override"
	cases := []struct {
		name   string
		body   any
		fields []string
	}{
		{"нет балла", map[string]any{"reason": "Причина"}, []string{"score"}},
		{"балл ниже 0", map[string]any{"score": -1, "reason": "Причина"}, []string{"score"}},
		{"балл выше 100", map[string]any{"score": 100.5, "reason": "Причина"}, []string{"score"}},
		{"нет причины", map[string]any{"score": 50}, []string{"reason"}},
		{"причина из пробелов", map[string]any{"score": 50, "reason": "   "}, []string{"reason"}},
		{"причина короче 3 после trim", map[string]any{"score": 50, "reason": "  аб  "}, []string{"reason"}},
		{"причина длиннее 1000", map[string]any{"score": 50, "reason": strings.Repeat("я", 1001)}, []string{"reason"}},
		{"и балл, и причина", map[string]any{"score": 101, "reason": "x"}, []string{"score", "reason"}},
		{"балл null", map[string]any{"score": nil, "reason": "Причина"}, []string{"score"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := do(h, http.MethodPost, path, "teacher", tc.body, true)
			if r.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", r.Code, r.Body)
			}
			code, _, fields := apiErr(t, r)
			if code != "validation" || len(fields) != len(tc.fields) {
				t.Fatalf("code=%q fields=%v, want %v", code, fields, tc.fields)
			}
			for _, f := range tc.fields {
				if msg, _ := fields[f].(string); msg == "" {
					t.Errorf("details.fields.%s missing: %v", f, fields)
				}
			}
		})
	}
}

func TestFeedbackValidation(t *testing.T) {
	t.Parallel()
	h := noDBRouter()
	path := "/api/v1/attempts/" + uuid.New().String() + "/feedback"
	cases := []struct {
		name   string
		body   any
		fields []string
	}{
		{"пустой комментарий", map[string]any{"comment": ""}, []string{"comment"}},
		{"нет комментария", map[string]any{"recommendation": "Повторить классификатор"}, []string{"comment"}},
		{"комментарий из пробелов", map[string]any{"comment": " \n\t "}, []string{"comment"}},
		{"комментарий длиннее 4000", map[string]any{"comment": strings.Repeat("ж", 4001)}, []string{"comment"}},
		{"путь поля длиннее 200", map[string]any{"comment": "Ок", "field": strings.Repeat("a", 201)}, []string{"field"}},
		{"рекомендация длиннее 2000", map[string]any{"comment": "Ок", "recommendation": strings.Repeat("ё", 2001)}, []string{"recommendation"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := do(h, http.MethodPost, path, "admin", tc.body, true)
			if r.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", r.Code, r.Body)
			}
			code, _, fields := apiErr(t, r)
			if code != "validation" || len(fields) != len(tc.fields) {
				t.Fatalf("code=%q fields=%v, want %v", code, fields, tc.fields)
			}
			for _, f := range tc.fields {
				if _, ok := fields[f]; !ok {
					t.Errorf("details.fields.%s missing: %v", f, fields)
				}
			}
		})
	}
}

func TestHandlersBadBody(t *testing.T) {
	t.Parallel()
	h := noDBRouter()
	id := uuid.New().String()
	for _, path := range []string{"/api/v1/attempts/" + id + "/evaluation/override", "/api/v1/attempts/" + id + "/feedback"} {
		if r := do(h, http.MethodPost, path, "teacher", `{"score": `, true); r.Code != http.StatusBadRequest {
			t.Errorf("%s broken JSON: %d %s", path, r.Code, r.Body)
		}
		if r := do(h, http.MethodPost, path, "teacher", "", true); r.Code != http.StatusBadRequest {
			t.Errorf("%s empty body: %d %s", path, r.Code, r.Body)
		}
	}
}
