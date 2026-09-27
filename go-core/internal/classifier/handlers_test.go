package classifier

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
)

// stubAuth — роль из заголовка X-Test-Role: пусто — нет сессии, blocked — заблокирован.
type stubAuth struct{}

func (stubAuth) Authenticate(r *http.Request) (*core.Principal, error) {
	switch role := r.Header.Get("X-Test-Role"); role {
	case "":
		return nil, core.ErrUnauthenticated
	case "blocked":
		return nil, core.ErrUserBlocked
	default:
		return &core.Principal{UserID: uuid.New(), Role: core.Role(role), Login: role, LastName: "Тестов", FirstName: "Тест"}, nil
	}
}

func newTestServer(t *testing.T, c *Catalog) (*httptest.Server, *httpx.Router) {
	t.Helper()
	mux := http.NewServeMux()
	r := httpx.NewRouter(mux, "/api/v1", stubAuth{}, discardLog(), nil)
	NewHandlers(c, nil).Register(r)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, r
}

type resp struct {
	status int
	header http.Header
	body   []byte
}

func do(t *testing.T, srv *httptest.Server, method, path, role string, body string, hdr map[string]string) resp {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+"/api/v1"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if role != "" {
		req.Header.Set("X-Test-Role", role)
	}
	if method != http.MethodGet {
		req.Header.Set("X-Requested-With", "fetch")
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
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

func get(t *testing.T, srv *httptest.Server, path string, q url.Values) resp {
	t.Helper()
	if q != nil {
		path += "?" + q.Encode()
	}
	return do(t, srv, http.MethodGet, path, "student", "", nil)
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

var getRoutes = []string{
	"/classifier/types",
	"/classifier/types/search?q=газ",
	"/classifier/featured",
	"/classifier/types/it-101/attributes",
	"/classifier/types/it-101/reference",
	"/classifier/labels",
	"/services",
	"/address/suggest?q=" + url.QueryEscape("Тверская 12"),
	"/reaction/transitions?current=" + url.QueryEscape("Получена службой") + "&serviceCode=101",
}

func TestRoutesRegistered(t *testing.T) {
	t.Parallel()
	_, r := newTestServer(t, fixtureCatalog(t))
	want := []string{
		"GET /classifier/types", "GET /classifier/types/search", "GET /classifier/featured",
		"GET /classifier/types/{typeId}/attributes", "GET /classifier/types/{typeId}/reference",
		"GET /classifier/labels", "POST /classifier/resolve-services", "GET /services",
		"GET /address/suggest", "GET /reaction/transitions",
	}
	got := r.Routes()
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("routes %v, want %v", got, want)
	}
}

func TestHandlersAuth(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, fixtureCatalog(t))
	for _, p := range getRoutes {
		expectError(t, do(t, srv, http.MethodGet, p, "", "", nil), http.StatusUnauthorized, httpx.CodeUnauthorized)
		expectError(t, do(t, srv, http.MethodGet, p, "blocked", "", nil), http.StatusLocked, httpx.CodeUserBlocked)
		// справочники нужны всем ролям
		for _, role := range []core.Role{core.RoleStudent, core.RoleTeacher, core.RoleAdmin} {
			if r := do(t, srv, http.MethodGet, p, string(role), "", nil); r.status != http.StatusOK {
				t.Errorf("%s as %s: %d %s", p, role, r.status, r.body)
			} else if ct := r.header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Errorf("%s: content-type %q", p, ct)
			}
		}
	}
	body := `{"typeIds":[],"attributes":{},"addressFilled":false,"current":[]}`
	expectError(t, do(t, srv, http.MethodPost, "/classifier/resolve-services", "", body, nil), http.StatusUnauthorized, httpx.CodeUnauthorized)
	// CSRF-замок: мутирующий запрос без X-Requested-With — 403 ещё до аутентификации
	expectError(t, do(t, srv, http.MethodPost, "/classifier/resolve-services", "student", body,
		map[string]string{"X-Requested-With": ""}), http.StatusForbidden, httpx.CodeForbidden)
	for _, role := range []string{"student", "teacher", "admin"} {
		if r := do(t, srv, http.MethodPost, "/classifier/resolve-services", role, body, nil); r.status != http.StatusOK {
			t.Errorf("resolve as %s: %d %s", role, r.status, r.body)
		}
	}
}

func TestCachedReferenceBodies(t *testing.T) {
	t.Parallel()
	c := fixtureCatalog(t)
	srv, _ := newTestServer(t, c)
	for _, p := range []string{"/classifier/types", "/classifier/featured", "/classifier/labels", "/services",
		"/classifier/types/it-101/attributes"} {
		r := get(t, srv, p, nil)
		etag := r.header.Get("ETag")
		if r.status != 200 || etag == "" || r.header.Get("Cache-Control") != "private, max-age=60" {
			t.Fatalf("%s: %d etag %q cache %q", p, r.status, etag, r.header.Get("Cache-Control"))
		}
		for _, inm := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
			r2 := do(t, srv, http.MethodGet, p, "teacher", "", map[string]string{"If-None-Match": inm})
			if r2.status != http.StatusNotModified || len(r2.body) != 0 || r2.header.Get("ETag") != etag {
				t.Errorf("%s If-None-Match %q: %d body %q", p, inm, r2.status, r2.body)
			}
		}
		if r3 := do(t, srv, http.MethodGet, p, "teacher", "", map[string]string{"If-None-Match": `"stale"`}); r3.status != 200 ||
			string(r3.body) != string(r.body) {
			t.Errorf("%s stale etag: %d", p, r3.status)
		}
	}
	types := decode[[]public.IncidentType](t, get(t, srv, "/classifier/types", nil).body)
	if len(types) != len(data().classifier.Types) {
		t.Errorf("types: %d", len(types))
	}
	svcs := decode[[]public.ServiceRef](t, get(t, srv, "/services", nil).body)
	if len(svcs) != len(data().services) || svcs[0].Code != "101" {
		t.Errorf("services: %+v", svcs)
	}
	feat := decode[map[string][]public.IncidentType](t, get(t, srv, "/classifier/featured", nil).body)
	if len(feat["frequent"]) == 0 || len(feat["significant"]) == 0 {
		t.Errorf("featured: %v", feat)
	}
	lb := decode[public.ClassifierLabels](t, get(t, srv, "/classifier/labels", nil).body)
	if lb.Fields == nil || lb.Attributes == nil || lb.Values == nil || lb.Types == nil {
		t.Errorf("labels: %+v", lb)
	}
}

func TestEmptyCatalogHandlers(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, NewCatalog(nil, nil))
	for p, want := range map[string]string{
		"/classifier/types":                  "[]",
		"/services":                          "[]",
		"/classifier/featured":               `{"frequent":[],"significant":[]}`,
		"/classifier/types/search?q=пожар":   "[]",
		"/classifier/types/it-101/reference": `{"exists":false}`,
	} {
		if r := get(t, srv, p, nil); r.status != 200 || string(r.body) != want {
			t.Errorf("%s: %d %s, want %s", p, r.status, r.body, want)
		}
	}
	expectError(t, get(t, srv, "/classifier/types/it-101/attributes", nil), http.StatusNotFound, httpx.CodeNotFound)
}

func TestSearchHandler(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, fixtureCatalog(t))
	expectError(t, get(t, srv, "/classifier/types/search", nil), http.StatusBadRequest, httpx.CodeValidation, "q")
	expectError(t, get(t, srv, "/classifier/types/search", url.Values{"q": {"   "}}), http.StatusBadRequest, httpx.CodeValidation, "q")

	r := get(t, srv, "/classifier/types/search", url.Values{"q": {"газ"}})
	found := decode[[]public.IncidentType](t, r.body)
	if r.status != 200 || len(found) == 0 || found[0].Code != "104" {
		t.Fatalf("search газ: %d %s", r.status, r.body)
	}
	if found := decode[[]public.IncidentType](t, get(t, srv, "/classifier/types/search", url.Values{"q": {"о"}, "limit": {"2"}}).body); len(found) != 2 {
		t.Errorf("limit 2: %d", len(found))
	}
	if found := decode[[]public.IncidentType](t, get(t, srv, "/classifier/types/search", url.Values{"q": {"о"}, "limit": {"мусор"}}).body); len(found) != min(SearchDefaultLimit, len(mockSearch("о"))) {
		t.Errorf("bad limit -> default: %d", len(found))
	}
	if r := get(t, srv, "/classifier/types/search", url.Values{"q": {"нетакого"}}); r.status != 200 || string(r.body) != "[]" {
		t.Errorf("no results: %d %s", r.status, r.body)
	}
}

func TestAttributesAndReferenceHandlers(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, fixtureCatalog(t))
	for _, key := range []string{"it-101", "101", typeUUID("101").String()} {
		r := get(t, srv, "/classifier/types/"+url.PathEscape(key)+"/attributes", nil)
		groups := decode[[]public.AttributeGroup](t, r.body)
		if r.status != 200 || len(groups) != 9 || groups[0].Code != "where" {
			t.Errorf("attributes %s: %d %d", key, r.status, len(groups))
		}
		for i := 1; i < len(groups); i++ {
			if groups[i].Order < groups[i-1].Order {
				t.Errorf("attributes not sorted by order: %+v", groups)
			}
		}
	}
	if r := get(t, srv, "/classifier/types/it-cancel/attributes", nil); r.status != 200 || string(r.body) != "[]" {
		t.Errorf("type without attributes: %d %s", r.status, r.body)
	}
	expectError(t, get(t, srv, "/classifier/types/нет-такого/attributes", nil), http.StatusNotFound, httpx.CodeNotFound)
	expectError(t, get(t, srv, "/classifier/types/"+uuid.NewString()+"/attributes", nil), http.StatusNotFound, httpx.CodeNotFound)

	type ref struct {
		Exists bool    `json:"exists"`
		Text   *string `json:"text"`
	}
	r := decode[ref](t, get(t, srv, "/classifier/types/it-101/reference", nil).body)
	if !r.Exists || r.Text == nil || *r.Text == "" {
		t.Errorf("reference 101: %+v", r)
	}
	// у каждого типа с hasReference справка действительно есть (и наоборот)
	for _, ty := range data().classifier.Types {
		got := decode[ref](t, get(t, srv, "/classifier/types/"+ty.FixtureID+"/reference", nil).body)
		if got.Exists != (ty.Reference != "") || (got.Text != nil) != got.Exists {
			t.Errorf("reference %s: %+v", ty.Code, got)
		}
	}
	if r := get(t, srv, "/classifier/types/нет-такого/reference", nil); r.status != 200 || string(r.body) != `{"exists":false}` {
		t.Errorf("unknown type reference: %d %s", r.status, r.body)
	}
}

func TestResolveServicesHandler(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, fixtureCatalog(t))
	post := func(body string, hdr map[string]string) resp {
		return do(t, srv, http.MethodPost, "/classifier/resolve-services", "student", body, hdr)
	}
	expectError(t, post("", nil), http.StatusBadRequest, httpx.CodeValidation)
	expectError(t, post("{не json", nil), http.StatusBadRequest, httpx.CodeValidation)
	expectError(t, post(`{"typeIds":"it-101"}`, nil), http.StatusBadRequest, httpx.CodeValidation)
	if r := post(`{}`, map[string]string{"Content-Type": "text/plain"}); r.status != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain: %d", r.status)
	}
	svc := func(code, status, source string) string {
		return `{"serviceId":"x","code":"` + code + `","name":"n","shortName":"s","isPrimary":false,"source":"` + source +
			`","currentStatus":"` + status + `","currentStatusAt":"2026-09-24T10:00:00Z","history":[],"allowedNext":[],"editable":true}`
	}
	for _, tc := range []struct{ body, field string }{
		{`{"typeIds":["it-101",""],"attributes":{},"addressFilled":true,"current":[]}`, "typeIds"},
		{`{"typeIds":["  "],"attributes":{},"addressFilled":true,"current":[]}`, "typeIds"},
		{`{"typeIds":[],"attributes":{},"addressFilled":true,"addressSource":"google","current":[]}`, "addressSource"},
		{`{"typeIds":[],"attributes":{},"addressFilled":true,"current":[` + svc("101", "Уехала", "auto") + `]}`, "current"},
		{`{"typeIds":[],"attributes":{},"addressFilled":true,"current":[` + svc("101", "Принята", "robot") + `]}`, "current"},
		{`{"typeIds":[],"attributes":{},"addressFilled":true,"current":[` + svc(" ", "Принята", "auto") + `]}`, "current"},
	} {
		expectError(t, post(tc.body, nil), http.StatusBadRequest, httpx.CodeValidation, tc.field)
	}

	// пустой запрос -> пустой ответ с массивом, а не null
	r := post(`{}`, nil)
	if r.status != 200 || strings.TrimSpace(string(r.body)) != `{"services":[],"suppressedByAddressSource":false}` {
		t.Errorf("empty request: %d %s", r.status, r.body)
	}
	// ФИАС
	r = post(`{"typeIds":["it-101"],"attributes":{},"addressFilled":true,"addressSource":"fias","current":[]}`, nil)
	if r.status != 200 || strings.TrimSpace(string(r.body)) != `{"services":[],"suppressedByAddressSource":true}` {
		t.Errorf("fias: %d %s", r.status, r.body)
	}
	// обычный случай + ручная служба из current сохраняется
	r = post(`{"typeIds":["it-dtp"],"attributes":{"victims":"yes"},"addressFilled":true,"addressSource":"yandex_map","current":[`+
		svc("oati", "Принята", "manual")+`]}`, nil)
	res := decode[public.ResolvedServicesResult](t, r.body)
	var codes []string
	for _, s := range res.Services {
		codes = append(codes, s.Code)
	}
	if r.status != 200 || !slices.Equal(codes, []string{"102", "codd", "103", "oati"}) {
		t.Fatalf("dtp: %d %v %s", r.status, codes, r.body)
	}
	type rawResult struct {
		Services []map[string]any `json:"services"`
	}
	for _, s := range decode[rawResult](t, r.body).Services {
		for _, k := range []string{"serviceId", "code", "name", "shortName", "isPrimary", "source", "currentStatus",
			"currentStatusAt", "history", "allowedNext", "editable"} {
			if _, ok := s[k]; !ok {
				t.Errorf("AssignedService misses %q: %v", k, s)
			}
		}
		if _, ok := s["allowedNext"].([]any); !ok {
			t.Errorf("allowedNext is not an array: %v", s)
		}
	}
	if res.Services[3].ServiceId != serviceUUID("oati").String() || res.Services[3].Name == "n" {
		t.Errorf("manual service refreshed from the catalog: %+v", res.Services[3])
	}
}

func TestSuggestHandler(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, fixtureCatalog(t))
	expectError(t, get(t, srv, "/address/suggest", nil), http.StatusBadRequest, httpx.CodeValidation, "q")
	expectError(t, get(t, srv, "/address/suggest", url.Values{"q": {" Т "}}), http.StatusBadRequest, httpx.CodeValidation, "q")
	r := get(t, srv, "/address/suggest", url.Values{"q": {"Тверская 12"}})
	l := decode[[]public.AddressSuggestion](t, r.body)
	if r.status != 200 || len(l) == 0 || l[0].Label != "Москва, Тверская улица, 12" {
		t.Fatalf("suggest: %d %s", r.status, r.body)
	}
	if l := decode[[]public.AddressSuggestion](t, get(t, srv, "/address/suggest", url.Values{"q": {"ул"}, "limit": {"100"}}).body); len(l) != SuggestMaxLimit {
		t.Errorf("limit clamp: %d", len(l))
	}
	if l := decode[[]public.AddressSuggestion](t, get(t, srv, "/address/suggest", url.Values{"q": {"ул"}, "limit": {"0"}}).body); len(l) != 1 {
		t.Errorf("limit 0 -> 1 (minimum): %d", len(l))
	}
	if r := get(t, srv, "/address/suggest", url.Values{"q": {"Москва"}}); r.status != 200 || strings.TrimSpace(string(r.body)) != "[]" {
		t.Errorf("no street: %d %s", r.status, r.body)
	}
}

func TestTransitionsHandler(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, fixtureCatalog(t))
	expectError(t, get(t, srv, "/reaction/transitions", nil), http.StatusBadRequest, httpx.CodeValidation, "current", "serviceCode")
	expectError(t, get(t, srv, "/reaction/transitions", url.Values{"current": {"Уехала"}, "serviceCode": {"101"}}),
		http.StatusBadRequest, httpx.CodeValidation, "current")
	expectError(t, get(t, srv, "/reaction/transitions", url.Values{"current": {"Принята"}, "serviceCode": {"  "}}),
		http.StatusBadRequest, httpx.CodeValidation, "serviceCode")

	r := get(t, srv, "/reaction/transitions", url.Values{"current": {"Получена службой"}, "serviceCode": {"103"}})
	tr := decode[[]public.AllowedTransition](t, r.body)
	if r.status != 200 || len(tr) != 1 || tr[0].Status != "Принята" {
		t.Errorf("103: %d %s", r.status, r.body)
	}
	r = get(t, srv, "/reaction/transitions", url.Values{"current": {"Принята"}, "serviceCode": {"103"}})
	tr = decode[[]public.AllowedTransition](t, r.body)
	if len(tr) != 4 || tr[3].Label != "Работы завершены: Завершение работ без бригады" || !tr[3].CommentRequired {
		t.Errorf("103 after «Принята»: %s", r.body)
	}
	if r := get(t, srv, "/reaction/transitions", url.Values{"current": {"Работы завершены"}, "serviceCode": {"101"}}); r.status != 200 ||
		strings.TrimSpace(string(r.body)) != "[]" {
		t.Errorf("terminal: %d %s", r.status, r.body)
	}
}

func TestEtagMatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		header string
		want   bool
	}{
		{"", false},
		{`"abc"`, true},
		{`W/"abc"`, true},
		{` "x" , "abc" `, true},
		{`"abcd"`, false},
		{`abc`, false},
		{"*", true},
		{`"x", *`, true},
	} {
		if got := etagMatch(tc.header, `"abc"`); got != tc.want {
			t.Errorf("etagMatch(%q) = %v", tc.header, got)
		}
	}
}
