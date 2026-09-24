package attempts

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
	"lct/gocore/internal/platform/httpx"
)

// Хендлер-тесты без БД: всё, что отвергается до первого запроса к пулу (аутентификация,
// роли, CSRF, путь, тело). Сервис собран без пула — дойди запрос до БД, тест упал бы с паникой.

func noDBMux() *http.ServeMux {
	return newRouter(New(Deps{Catalog: fakeCatalog{}, Log: quietLog()}))
}

type route struct{ method, path string }

func allRoutes(id string) []route {
	return []route{
		{http.MethodGet, "/attempts/" + id},
		{http.MethodGet, "/attempts/" + id + "/call-script"},
		{http.MethodGet, "/attempts/" + id + "/draft"},
		{http.MethodGet, "/attempts/" + id + "/events"},
		{http.MethodPost, "/attempts/" + id + "/accept-call"},
		{http.MethodPut, "/attempts/" + id + "/draft"},
		{http.MethodPost, "/attempts/" + id + "/events"},
		{http.MethodPost, "/attempts/" + id + "/services"},
		{http.MethodDelete, "/attempts/" + id + "/services/svc-101"},
		{http.MethodPost, "/attempts/" + id + "/services/svc-101/status"},
		{http.MethodPost, "/attempts/" + id + "/replay"},
		{http.MethodPost, "/attempts/" + id + "/submit"},
	}
}

func TestRoutes_Registered(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	r := httpx.NewRouter(mux, "/api/v1", stubAuth{}, quietLog(), nil)
	New(Deps{Log: quietLog()}).Register(r)
	want := map[string]bool{}
	for _, rt := range allRoutes("{attemptId}") {
		p := strings.Replace(rt.path, "svc-101", "{serviceId}", 1)
		want[rt.method+" "+p] = true
	}
	got := r.Routes()
	if len(got) != len(want) {
		t.Fatalf("маршрутов %d, ожидалось %d: %v", len(got), len(want), got)
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("неожиданный маршрут %q", p)
		}
	}
}

func TestRoutes_Unauthenticated(t *testing.T) {
	t.Parallel()
	mux := noDBMux()
	for _, rt := range allRoutes(uuid.NewString()) {
		r := serve(mux, nil, rt.method, rt.path, nil)
		if r.code != http.StatusUnauthorized {
			t.Errorf("%s %s без сессии: %d", rt.method, rt.path, r.code)
		}
	}
}

// Действовать от имени обучающегося может только студент: преподавателю и админу — 403.
func TestRoutes_StudentOnly(t *testing.T) {
	t.Parallel()
	mux := noDBMux()
	id := uuid.NewString()
	for _, role := range []core.Role{core.RoleTeacher, core.RoleAdmin} {
		p := who(role, uuid.New())
		for _, rt := range allRoutes(id) {
			if rt.method == http.MethodGet {
				continue
			}
			r := serve(mux, p, rt.method, rt.path, `{}`)
			if r.code != http.StatusForbidden {
				t.Errorf("%s: %s %s = %d, want 403", role, rt.method, rt.path, r.code)
				continue
			}
			if code, _, _ := r.apiError(t); code != httpx.CodeForbidden {
				t.Errorf("%s %s: code %q", rt.method, rt.path, code)
			}
		}
	}
}

func TestRoutes_CSRF(t *testing.T) {
	t.Parallel()
	mux := noDBMux()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/attempts/"+uuid.NewString()+"/draft", strings.NewReader(string(emptyDraftJSON)))
	req.Header.Set(principalHeader, "student:"+uuid.NewString())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("без X-Requested-With: %d", rec.Code)
	}
}

// Не-UUID в пути — 404 (как «не найдено»), до БД.
func TestRoutes_BadAttemptID(t *testing.T) {
	t.Parallel()
	mux := noDBMux()
	st := who(core.RoleStudent, uuid.New())
	for _, rt := range allRoutes("not-a-uuid") {
		r := serve(mux, st, rt.method, rt.path, nil)
		if r.code != http.StatusNotFound {
			t.Errorf("%s %s: %d", rt.method, rt.path, r.code)
		}
	}
}

// Тело проверяется до БД: 400 validation с полями.
func TestHandlers_BodyValidation(t *testing.T) {
	t.Parallel()
	mux := noDBMux()
	st := who(core.RoleStudent, uuid.New())
	id := uuid.NewString()
	cases := []struct {
		name, method, path string
		body               any
		field              string
	}{
		{"draft: не объект", http.MethodPut, "/draft", `[1,2]`, ""},
		{"draft: мусор", http.MethodPut, "/draft", `{"foo":`, ""},
		{"draft: чужая форма", http.MethodPut, "/draft", `{"foo":1}`, "description"},
		{"draft: адрес без raw", http.MethodPut, "/draft", strings.Replace(string(emptyDraftJSON), `"raw":""`, `"rawX":""`, 1), "address.raw"},
		{"submit: пустое тело", http.MethodPost, "/submit", ``, ""},
		{"submit: нет карточки", http.MethodPost, "/submit", `{"actionText":"x"}`, "card"},
		{"submit: длинное описание", http.MethodPost, "/submit",
			`{"card":` + strings.Replace(string(emptyDraftJSON), `"description":""`, `"description":"`+strings.Repeat("ы", 2000)+`"`, 1) + `}`,
			"card.description"},
		{"services: нет кода", http.MethodPost, "/services", `{"serviceCode":"  "}`, "serviceCode"},
		{"services: неизвестная служба", http.MethodPost, "/services", `{"serviceCode":"999"}`, "serviceCode"},
		{"status: неизвестный статус", http.MethodPost, "/services/svc-101/status", `{"status":"Горит"}`, "status"},
		{"events: пустая пачка", http.MethodPost, "/events", `{"events":[]}`, "events"},
		{"events: неизвестный тип", http.MethodPost, "/events", `{"events":[{"clientSeq":1,"type":"hack","at":"2026-09-24T10:00:00Z"}]}`, "events[0].type"},
	}
	for _, c := range cases {
		r := serve(mux, st, c.method, "/attempts/"+id+c.path, c.body)
		if r.code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", c.name, r.code, r.body)
			continue
		}
		code, msg, fields := r.apiError(t)
		if code != httpx.CodeValidation || msg == "" {
			t.Errorf("%s: code=%q msg=%q", c.name, code, msg)
		}
		if c.field != "" {
			if _, ok := fields[c.field]; !ok {
				t.Errorf("%s: нет поля %q: %s", c.name, c.field, r.body)
			}
		}
	}
}
