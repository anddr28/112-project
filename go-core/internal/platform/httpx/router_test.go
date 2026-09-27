package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"lct/gocore/internal/core"
)

// stubAuth — аутентификация по заголовкам теста: X-Test-Role (роль) или X-Test-Auth
// (none | blocked | boom).
type stubAuth struct{}

var testUserID = uuid.MustParse("018f0000-0000-7000-8000-000000000001")

func (stubAuth) Authenticate(r *http.Request) (*core.Principal, error) {
	switch r.Header.Get("X-Test-Auth") {
	case "blocked":
		return nil, fmt.Errorf("session: %w", core.ErrUserBlocked)
	case "boom":
		return nil, errors.New("db down")
	}
	role := core.Role(r.Header.Get("X-Test-Role"))
	if !role.Valid() {
		return nil, core.ErrUnauthenticated
	}
	return &core.Principal{UserID: testUserID, Role: role, Login: "u-" + string(role), LastName: "Иванов", FirstName: "Пётр"}, nil
}

type observation struct {
	route  string
	status int
}

type recorder struct {
	mu  sync.Mutex
	obs []observation
}

func (o *recorder) observe(route string, status int, _ time.Duration) {
	o.mu.Lock()
	o.obs = append(o.obs, observation{route, status})
	o.mu.Unlock()
}

func (o *recorder) statuses(route string) []int {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []int
	for _, x := range o.obs {
		if x.route == route {
			out = append(out, x.status)
		}
	}
	return out
}

func (o *recorder) has(route string) (int, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, x := range o.obs {
		if x.route == route {
			return x.status, true
		}
	}
	return 0, false
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type apiErr struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

func decodeErr(t *testing.T, resp *http.Response) apiErr {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type ошибки = %q", ct)
	}
	b, _ := io.ReadAll(resp.Body)
	var e apiErr
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("тело ошибки не ApiError: %s", b)
	}
	if e.Code == "" || e.Message == "" {
		t.Fatalf("ApiError без code/message: %s", b)
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(b, &raw)
	if d, ok := raw["details"]; ok && string(d) == "null" {
		t.Fatalf("details: null вместо отсутствия поля: %s", b)
	}
	return e
}

type testServer struct {
	*httptest.Server
	rec         *recorder
	rt          *Router
	wsDone      chan struct{}
	wsDeadline  chan bool
	lastMetaReq chan core.RequestMeta
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	mux := http.NewServeMux()
	rec := &recorder{}
	rt := NewRouter(mux, "/api/v1/", stubAuth{}, quietLog(), rec.observe)
	ts := &testServer{rec: rec, rt: rt, wsDone: make(chan struct{}), wsDeadline: make(chan bool, 1),
		lastMetaReq: make(chan core.RequestMeta, 16)}

	rt.Handle("GET /public", Public, func(w http.ResponseWriter, r *http.Request) error {
		m, _ := core.RequestMetaFrom(r.Context())
		ts.lastMetaReq <- m
		WriteJSON(w, http.StatusOK, map[string]any{"principal": core.PrincipalFrom(r.Context()) != nil})
		return nil
	})
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		rt.Handle(m+" /public", Public, func(w http.ResponseWriter, r *http.Request) error {
			NoContent(w)
			return nil
		})
	}
	rt.Handle("GET /me", Authenticated, func(w http.ResponseWriter, r *http.Request) error {
		p := core.PrincipalFrom(r.Context())
		WriteJSON(w, http.StatusOK, map[string]string{"login": p.Login, "short": p.ShortName()})
		return nil
	})
	rt.Handle("GET /staff", Roles(core.RoleTeacher, core.RoleAdmin), func(w http.ResponseWriter, r *http.Request) error {
		NoContent(w)
		return nil
	})
	rt.Handle("GET /panic", Public, func(w http.ResponseWriter, r *http.Request) error { panic("boom") })
	rt.Handle("GET /err/{kind}", Public, func(w http.ResponseWriter, r *http.Request) error {
		switch r.PathValue("kind") {
		case "notfound":
			return NotFound("")
		case "validation":
			return Validation("Проверьте поля", map[string]string{"login": "обязательно"})
		case "conflict":
			return Conflict("Уже есть")
		case "plain":
			return errors.New("что-то сломалось внутри")
		case "wrapped":
			return fmt.Errorf("слой: %w", Forbidden("чужое"))
		case "busy":
			return CallerBusy(0)
		case "ratelimited":
			return TooManyRequests("", 7)
		case "foreign-deadline":
			// DeadlineExceeded чужого контекста (не потолок запроса) — это 500
			return fmt.Errorf("ai: %w", context.DeadlineExceeded)
		case "partial":
			WriteJSON(w, http.StatusOK, map[string]string{"ok": "yes"})
			return errors.New("после записи ответа")
		}
		return nil
	})
	rt.Handle("GET /deadline", Public, func(w http.ResponseWriter, r *http.Request) error {
		dl, ok := r.Context().Deadline()
		WriteJSON(w, http.StatusOK, map[string]any{"has": ok, "left": time.Until(dl).Seconds()})
		return nil
	})
	rt.HandleRaw("GET /ws", Roles(core.RoleStudent), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(ts.wsDone)
		_, has := r.Context().Deadline()
		ts.wsDeadline <- has
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := context.Background()
		typ, msg, err := c.Read(ctx)
		if err != nil {
			return
		}
		_ = c.Write(ctx, typ, append([]byte("echo:"), msg...))
		_, _, _ = c.Read(ctx) // до закрытия клиентом
	}))

	// отдельный потолок для «медленного» маршрута
	rt.SetRequestTimeout(50 * time.Millisecond)
	rt.Handle("GET /slow", Public, func(w http.ResponseWriter, r *http.Request) error {
		select {
		case <-r.Context().Done():
			return fmt.Errorf("pg: acquire: %w", r.Context().Err())
		case <-time.After(5 * time.Second):
			return errors.New("потолок не сработал")
		}
	})
	rt.Handle("GET /slow-typed", Public, func(w http.ResponseWriter, r *http.Request) error {
		<-r.Context().Done()
		return NotFound("типизированная ошибка остаётся как есть")
	})
	rt.SetRequestTimeout(DefaultRequestTimeout)

	ts.Server = httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func (ts *testServer) do(t *testing.T, method, path string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+"/api/v1"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestRouterAccess(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	cases := []struct {
		name   string
		path   string
		hdr    map[string]string
		status int
		code   string
	}{
		{"public без сессии", "/public", nil, 200, ""},
		{"нет сессии", "/me", nil, 401, CodeUnauthorized},
		{"заблокирован", "/me", map[string]string{"X-Test-Auth": "blocked"}, 423, CodeUserBlocked},
		{"ошибка аутентификации", "/me", map[string]string{"X-Test-Auth": "boom"}, 500, CodeInternal},
		{"любой вошедший", "/me", map[string]string{"X-Test-Role": "student"}, 200, ""},
		{"роль не подходит", "/staff", map[string]string{"X-Test-Role": "student"}, 403, CodeForbidden},
		{"преподаватель", "/staff", map[string]string{"X-Test-Role": "teacher"}, 204, ""},
		{"админ", "/staff", map[string]string{"X-Test-Role": "admin"}, 204, ""},
		{"роль без сессии — 401, не 403", "/staff", nil, 401, CodeUnauthorized},
	}
	for _, c := range cases {
		resp := ts.do(t, "GET", c.path, c.hdr)
		if resp.StatusCode != c.status {
			t.Errorf("%s: status %d, want %d", c.name, resp.StatusCode, c.status)
			continue
		}
		if _, err := uuid.Parse(resp.Header.Get("X-Request-Id")); err != nil {
			t.Errorf("%s: X-Request-Id = %q", c.name, resp.Header.Get("X-Request-Id"))
		}
		if c.code != "" {
			if e := decodeErr(t, resp); e.Code != c.code {
				t.Errorf("%s: code %q, want %q", c.name, e.Code, c.code)
			}
		}
	}
	resp := ts.do(t, "GET", "/me", map[string]string{"X-Test-Role": "teacher"})
	var me map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&me)
	if me["login"] != "u-teacher" || me["short"] != "Иванов П." {
		t.Fatalf("principal в контексте: %v", me)
	}
	// публичный маршрут: RequestMeta есть, Principal нет
	resp = ts.do(t, "GET", "/public", map[string]string{"X-Test-Role": "admin"})
	var pub map[string]bool
	_ = json.NewDecoder(resp.Body).Decode(&pub)
	if pub["principal"] {
		t.Fatal("публичный маршрут не должен аутентифицировать")
	}
	var meta core.RequestMeta
	for len(ts.lastMetaReq) > 0 {
		meta = <-ts.lastMetaReq
	}
	if meta.RequestID == uuid.Nil || meta.IP != "127.0.0.1" || meta.UserAgent == "" {
		t.Fatalf("RequestMeta: %+v", meta)
	}
}

func TestRouterCSRF(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		resp := ts.do(t, m, "/public", nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s без X-Requested-With: %d", m, resp.StatusCode)
		}
		if e := decodeErr(t, resp); e.Code != CodeForbidden || !strings.Contains(e.Message, "X-Requested-With") {
			t.Fatalf("%s: %+v", m, e)
		}
		if resp := ts.do(t, m, "/public", map[string]string{"X-Requested-With": "XMLHttpRequest"}); resp.StatusCode != 403 {
			t.Fatalf("%s с чужим значением заголовка: %d", m, resp.StatusCode)
		}
		if resp := ts.do(t, m, "/public", map[string]string{"X-Requested-With": "fetch"}); resp.StatusCode != 204 {
			t.Fatalf("%s с заголовком: %d", m, resp.StatusCode)
		}
	}
	// GET замком не закрыт
	if resp := ts.do(t, "GET", "/public", nil); resp.StatusCode != 200 {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
}

func TestRouterErrors(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	cases := []struct {
		kind       string
		status     int
		code       string
		retryAfter string
	}{
		{"notfound", 404, CodeNotFound, ""},
		{"validation", 400, CodeValidation, ""},
		{"conflict", 409, CodeConflict, ""},
		{"plain", 500, CodeInternal, ""},
		{"wrapped", 403, CodeForbidden, ""},
		{"busy", 503, CodeCallerBusy, "3"},
		{"ratelimited", 429, CodeRateLimited, "7"},
		{"foreign-deadline", 500, CodeInternal, ""},
	}
	for _, c := range cases {
		resp := ts.do(t, "GET", "/err/"+c.kind, nil)
		if resp.StatusCode != c.status {
			t.Errorf("%s: status %d, want %d", c.kind, resp.StatusCode, c.status)
			continue
		}
		if got := resp.Header.Get("Retry-After"); got != c.retryAfter {
			t.Errorf("%s: Retry-After %q, want %q", c.kind, got, c.retryAfter)
		}
		e := decodeErr(t, resp)
		if e.Code != c.code {
			t.Errorf("%s: code %q, want %q", c.kind, e.Code, c.code)
		}
		if c.kind == "plain" && strings.Contains(e.Message, "сломалось") {
			t.Errorf("внутренняя причина утекла клиенту: %q", e.Message)
		}
		if c.kind == "validation" {
			f, _ := e.Details["fields"].(map[string]any)
			if f["login"] != "обязательно" {
				t.Errorf("details.fields: %v", e.Details)
			}
		}
	}
	// ошибка после записанного ответа не пишет второй ответ
	resp := ts.do(t, "GET", "/err/partial", nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || strings.TrimSpace(string(b)) != `{"ok":"yes"}` {
		t.Fatalf("partial: %d %s", resp.StatusCode, b)
	}
	// метрики — по паттерну маршрута, не по сырому пути
	if st, ok := ts.rec.has("GET /err/{kind}"); !ok || st == 0 {
		t.Fatalf("observe: %v", ts.rec.obs)
	}
}

func TestRouterPanicRecovery(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	resp := ts.do(t, "GET", "/panic", nil)
	if resp.StatusCode != 500 {
		t.Fatalf("panic: %d", resp.StatusCode)
	}
	if e := decodeErr(t, resp); e.Code != CodeInternal {
		t.Fatalf("panic code: %+v", e)
	}
	if st, _ := ts.rec.has("GET /panic"); st != 500 {
		t.Fatalf("panic в метриках: %d", st)
	}
	// сервер жив
	if resp := ts.do(t, "GET", "/public", nil); resp.StatusCode != 200 {
		t.Fatalf("после паники: %d", resp.StatusCode)
	}
}

// Регрессия (ревью: missing-timeouts). Обычный запрос живёт не дольше потолка: зависший
// хендлер получает отмену контекста, клиент — 503 с Retry-After, а не вечное ожидание.
func TestRouterRequestTimeout(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	t0 := time.Now()
	resp := ts.do(t, "GET", "/slow", nil)
	if d := time.Since(t0); d > 3*time.Second {
		t.Fatalf("потолок не сработал: %v", d)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("slow: %d retry=%q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if e := decodeErr(t, resp); e.Code != CodeInternal {
		t.Fatalf("slow: %+v", e)
	}
	// типизированная ошибка хендлера не подменяется
	if resp := ts.do(t, "GET", "/slow-typed", nil); resp.StatusCode != 404 {
		t.Fatalf("slow-typed: %d", resp.StatusCode)
	}
	// маршруты, зарегистрированные с потолком по умолчанию
	resp = ts.do(t, "GET", "/deadline", nil)
	var d struct {
		Has  bool    `json:"has"`
		Left float64 `json:"left"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&d)
	if !d.Has || d.Left < DefaultRequestTimeout.Seconds()-5 || d.Left > DefaultRequestTimeout.Seconds() {
		t.Fatalf("потолок по умолчанию: %+v", d)
	}
}

// WebSocket через HandleRaw: Hijack проходит сквозь statusWriter, у апгрейда нет потолка
// времени, а его длительность (101) не попадает в метрики латентности.
func TestRouterWebSocket(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/ws"

	// без сессии — ApiError 401 ещё до апгрейда
	_, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err == nil || resp == nil || resp.StatusCode != 401 {
		t.Fatalf("ws без сессии: err=%v resp=%v", err, resp)
	}

	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"X-Test-Role": {"student"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, websocket.MessageText, []byte("привет")); err != nil {
		t.Fatal(err)
	}
	_, msg, err := c.Read(ctx)
	if err != nil || string(msg) != "echo:привет" {
		t.Fatalf("echo: %q %v", msg, err)
	}
	if has := <-ts.wsDeadline; has {
		t.Fatal("у WebSocket-апгрейда не должно быть потолка времени запроса")
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
	select {
	case <-ts.wsDone:
	case <-ctx.Done():
		t.Fatal("хендлер WS не завершился")
	}
	time.Sleep(50 * time.Millisecond) // defer роутера после выхода хендлера
	if st := ts.rec.statuses("GET /ws"); len(st) != 1 || st[0] != 401 {
		t.Fatalf("в метриках WS должен быть только отказ 401, а не сессия 101: %v", st)
	}
}

func TestRouterRoutesAndPatterns(t *testing.T) {
	t.Parallel()
	rt := NewRouter(http.NewServeMux(), "/api/v1", stubAuth{}, nil, nil)
	rt.Handle("GET /a/{id}", Public, func(http.ResponseWriter, *http.Request) error { return nil })
	rt.HandleRaw("POST /b", Authenticated, http.NotFoundHandler())
	got := rt.Routes()
	if len(got) != 2 || got[0] != "GET /a/{id}" || got[1] != "POST /b" {
		t.Fatalf("Routes = %v", got)
	}
	got[0] = "mutated"
	if rt.Routes()[0] != "GET /a/{id}" {
		t.Fatal("Routes отдаёт внутренний срез")
	}
	for _, bad := range []string{"GET", "GET a/b", ""} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("паттерн %q принят", bad)
				}
			}()
			rt.Handle(bad, Public, func(http.ResponseWriter, *http.Request) error { return nil })
		}()
	}
}

// Клиент ушёл (контекст запроса отменён) — хендлер вернул context.Canceled: ответа нет,
// в лог не пишется «ошибка 500».
func TestRouterClientGone(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	rt := NewRouter(mux, "/api/v1", stubAuth{}, quietLog(), nil)
	rt.Handle("GET /x", Public, func(w http.ResponseWriter, r *http.Request) error { return r.Context().Err() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/api/v1/x", nil).WithContext(ctx)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Body.Len() != 0 {
		t.Fatalf("ответ ушедшему клиенту: %d %s", rr.Code, rr.Body)
	}
}

func TestStatusWriter(t *testing.T) {
	t.Parallel()
	rr := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rr}
	sw.WriteHeader(201)
	sw.WriteHeader(500) // второй вызов не меняет запомненный статус
	if sw.status != 201 || !sw.wrote {
		t.Fatalf("status %d", sw.status)
	}
	sw2 := &statusWriter{ResponseWriter: httptest.NewRecorder()}
	_, _ = sw2.Write([]byte("x"))
	if sw2.status != 200 {
		t.Fatalf("implicit 200: %d", sw2.status)
	}
	if sw2.Unwrap() == nil {
		t.Fatal("Unwrap")
	}
	sw2.Flush() // recorder поддерживает Flush — не паникует
	// Hijack на писателе без поддержки — ошибка, а не паника
	if _, _, err := (&statusWriter{ResponseWriter: httptest.NewRecorder()}).Hijack(); err == nil {
		t.Fatal("Hijack без поддержки должен вернуть ошибку")
	}
}

func TestIsWebSocketUpgrade(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/", nil)
	if IsWebSocketUpgrade(r) {
		t.Fatal("обычный запрос")
	}
	r.Header.Set("Upgrade", "WebSocket")
	if !IsWebSocketUpgrade(r) {
		t.Fatal("регистр заголовка не важен")
	}
}
