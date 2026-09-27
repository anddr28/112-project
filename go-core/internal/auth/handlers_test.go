package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/config"
	"lct/gocore/internal/core"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/platform/pgtest"
	"lct/gocore/internal/users"
)

// ---------------------------------------------------------------- фейки и окружение

type auditRec struct {
	mu      sync.Mutex
	entries []core.AuditEntry
}

func (a *auditRec) Log(_ context.Context, e core.AuditEntry) {
	a.mu.Lock()
	a.entries = append(a.entries, e)
	a.mu.Unlock()
}

// reasons — причины user.login_failed по порядку.
func (a *auditRec) reasons() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, e := range a.entries {
		if e.Action == "user.login_failed" {
			out = append(out, e.After.(loginAudit).Reason)
		}
	}
	return out
}

func (a *auditRec) count(action string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, e := range a.entries {
		if e.Action == action {
			n++
		}
	}
	return n
}

type env struct {
	svc   *Service
	mux   *http.ServeMux
	pool  *pgxpool.Pool
	audit *auditRec
}

func newEnv(t *testing.T, pool *pgxpool.Pool, cfg *config.Config) *env {
	t.Helper()
	if cfg == nil {
		cfg = &config.Config{CookieSecure: true}
	}
	a := &auditRec{}
	svc := New(Deps{Pool: pool, Config: cfg, Auditor: a, Log: slog.New(slog.DiscardHandler), LoadUser: users.Get})
	mux := http.NewServeMux()
	r := httpx.NewRouter(mux, "/api/v1", svc, slog.New(slog.DiscardHandler), nil)
	svc.Register(r)
	return &env{svc: svc, mux: mux, pool: pool, audit: a}
}

type reqOpt func(*http.Request)

func fromIP(ip string) reqOpt { return func(r *http.Request) { r.RemoteAddr = ip + ":40000" } }
func withCookie(tok string) reqOpt {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: CookieName, Value: tok}) }
}
func noCSRF() reqOpt { return func(r *http.Request) { r.Header.Del("X-Requested-With") } }
func rawBody(b string) reqOpt {
	return func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader(b)) }
}
func withUA(ua string) reqOpt { return func(r *http.Request) { r.Header.Set("User-Agent", ua) } }

func (e *env) do(t *testing.T, method, path string, body any, opts ...reqOpt) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, "/api/v1"+path, rd)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Requested-With", "fetch")
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, r)
	return w
}

func (e *env) login(t *testing.T, login, pw string, opts ...reqOpt) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(t, http.MethodPost, "/auth/login", map[string]string{"login": login, "password": pw}, opts...)
}

// mustLogin — вход, возвращает токен сессии из Set-Cookie.
func (e *env) mustLogin(t *testing.T, login, pw string, opts ...reqOpt) string {
	t.Helper()
	w := e.login(t, login, pw, opts...)
	if w.Code != http.StatusOK {
		t.Fatalf("login %s: %d %s", login, w.Code, w.Body)
	}
	c := sessionCookie(t, w)
	return c.Value
}

func sessionCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == CookieName {
			return c
		}
	}
	t.Fatalf("нет Set-Cookie %s: %v", CookieName, w.Header())
	return nil
}

type apiErr struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

func decodeErr(t *testing.T, w *httptest.ResponseRecorder) apiErr {
	t.Helper()
	var e apiErr
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("тело ошибки не ApiError: %v: %s", err, w.Body)
	}
	if e.Code == "" || e.Message == "" {
		t.Fatalf("ApiError без code/message: %s", w.Body)
	}
	return e
}

func expectErr(t *testing.T, w *httptest.ResponseRecorder, status int, code string) apiErr {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d, want %d: %s", w.Code, status, w.Body)
	}
	e := decodeErr(t, w)
	if e.Code != code {
		t.Fatalf("code=%q, want %q: %s", e.Code, code, w.Body)
	}
	return e
}

func seedUser(t *testing.T, pool *pgxpool.Pool, login, role, pw, status string) uuid.UUID {
	t.Helper()
	id := ids.New()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO users (id, login, password_hash, role, last_name, first_name, middle_name, status, operator_no)
VALUES ($1, $2, $3, $4, 'Рожкова', 'Ольга', 'Ивановна', $5, 'оп. 227')`, id, login, fastHash(t, pw), role, status); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// ---------------------------------------------------------------- без БД

func TestLoginValidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil, nil) // до БД дело не доходит: nil pool упал бы паникой
	t.Cleanup(func() {       // после всех параллельных подтестов: валидация не тратит бюджет лимитера
		if len(e.svc.limiter.buckets) != 0 {
			t.Errorf("валидация съела жетоны лимитера: %v", e.svc.limiter.buckets)
		}
	})
	cases := []struct {
		name   string
		body   any
		opts   []reqOpt
		fields []string
	}{
		{"пусто", map[string]string{}, nil, []string{"login", "password"}},
		{"пробелы в логине", map[string]string{"login": "   ", "password": "x"}, nil, []string{"login"}},
		{"нет пароля", map[string]string{"login": "student"}, nil, []string{"password"}},
		{"NUL в логине", map[string]string{"login": "stu\x00dent", "password": "x"}, nil, []string{"login"}},
		{"длинный логин", map[string]string{"login": strings.Repeat("я", maxLoginLen+1), "password": "x"}, nil, []string{"login"}},
		{"длинный пароль", map[string]string{"login": "student", "password": strings.Repeat("x", MaxPasswordLen+1)}, nil, []string{"password"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := e.do(t, http.MethodPost, "/auth/login", c.body, c.opts...)
			er := expectErr(t, w, http.StatusBadRequest, httpx.CodeValidation)
			fields, _ := er.Details["fields"].(map[string]any)
			for _, f := range c.fields {
				if _, ok := fields[f]; !ok {
					t.Fatalf("нет ошибки поля %s: %s", f, w.Body)
				}
			}
			if len(fields) != len(c.fields) {
				t.Fatalf("лишние ошибки полей: %v", fields)
			}
		})
	}
	t.Run("кривой JSON", func(t *testing.T) {
		t.Parallel()
		w := e.do(t, http.MethodPost, "/auth/login", nil, rawBody(`{"login":`))
		expectErr(t, w, http.StatusBadRequest, httpx.CodeValidation)
	})
	t.Run("без X-Requested-With", func(t *testing.T) {
		t.Parallel()
		w := e.do(t, http.MethodPost, "/auth/login", map[string]string{"login": "a", "password": "b"}, noCSRF())
		expectErr(t, w, http.StatusForbidden, httpx.CodeForbidden)
	})
}

func TestDemoAccounts(t *testing.T) {
	t.Parallel()
	on := newEnv(t, nil, &config.Config{DemoMode: true})
	w := on.do(t, http.MethodGet, "/auth/demo-accounts", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var acc []struct{ Login, Password, Label string }
	if err := json.Unmarshal(w.Body.Bytes(), &acc); err != nil {
		t.Fatal(err)
	}
	want := []string{"teacher", "student", "student2", "admin"}
	if len(acc) != len(want) {
		t.Fatalf("учёток %d, want %d", len(acc), len(want))
	}
	for i, a := range acc {
		if a.Login != want[i] || a.Password != a.Login || a.Label == "" {
			t.Fatalf("учётка %d: %+v", i, a)
		}
	}
	if acc[0].Label != "Преподаватель · Ковалёва И. С." {
		t.Fatalf("подпись: %q", acc[0].Label)
	}

	off := newEnv(t, nil, &config.Config{DemoMode: false})
	w = off.do(t, http.MethodGet, "/auth/demo-accounts", nil)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("боевой контур: %d %q, want []", w.Code, w.Body)
	}
}

func TestProtectedRoutesWithoutSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil, nil)
	for _, c := range []struct {
		method, path string
		opts         []reqOpt
	}{
		{http.MethodGet, "/auth/me", nil},
		{http.MethodPost, "/auth/logout", nil},
		// мусорная cookie отсекается по длине, без хэширования и БД
		{http.MethodGet, "/auth/me", []reqOpt{withCookie("short")}},
		{http.MethodGet, "/auth/me", []reqOpt{withCookie(strings.Repeat("x", tokenLen+1))}},
	} {
		w := e.do(t, c.method, c.path, nil, c.opts...)
		expectErr(t, w, http.StatusUnauthorized, httpx.CodeUnauthorized)
	}
}

func TestTokenShape(t *testing.T) {
	t.Parallel()
	a, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newToken()
	if len(a) != tokenLen || a == b {
		t.Fatalf("токены: %q %q", a, b)
	}
	k1, h1 := hashToken(a)
	if len(h1) != 64 || k1 != key(a) {
		t.Fatalf("hashToken: %s", h1)
	}
}

func TestTruncateRunes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"", 3, ""},
		{"abc", 3, "abc"},
		{"abcd", 3, "abc"},
		{"Ковалёва", 4, "Кова"},
		{"ёж", 1, "ё"},
	} {
		if got := truncateRunes(c.in, c.n); got != c.want {
			t.Fatalf("truncateRunes(%q,%d)=%q, want %q", c.in, c.n, got, c.want)
		}
	}
	if nullIfEmpty("") != nil || *nullIfEmpty("x") != "x" {
		t.Fatal("nullIfEmpty")
	}
}

// ---------------------------------------------------------------- с БД

func TestLoginMeLogoutFlow(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	e := newEnv(t, pool, nil)
	id := seedUser(t, pool, "rozhkova", "student", "Секрет-123", "active")
	if _, err := pool.Exec(context.Background(), `UPDATE users SET failed_login_count = 3 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}

	// citext: логин без учёта регистра, пробелы по краям срезаются
	w := e.login(t, "  RozhKova ", "Секрет-123", fromIP("198.51.100.7"), withUA("Mozilla/5.0 test"))
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	var u map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"id", "login", "role", "lastName", "firstName", "status"} {
		if _, ok := u[f]; !ok {
			t.Fatalf("в User нет обязательного %s: %s", f, w.Body)
		}
	}
	if u["id"] != id.String() || u["login"] != "rozhkova" || u["role"] != "student" || u["status"] != "active" ||
		u["middleName"] != "Ивановна" || u["operatorNo"] != "оп. 227" {
		t.Fatalf("User: %s", w.Body)
	}
	if g, ok := u["groupIds"].([]any); !ok || len(g) != 0 {
		t.Fatalf("groupIds должен быть [], а не null: %s", w.Body)
	}
	c := sessionCookie(t, w)
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" ||
		c.MaxAge != defaultTTLHours*3600 || len(c.Value) != tokenLen {
		t.Fatalf("cookie: %+v", c)
	}

	var (
		ip, ua         string
		fails          int
		lastLogin, exp time.Time
	)
	if err := pool.QueryRow(context.Background(), `
SELECT host(s.ip), s.user_agent, u.failed_login_count, u.last_login_at, s.expires_at
  FROM auth_sessions s JOIN users u ON u.id = s.user_id WHERE s.user_id = $1`, id).Scan(&ip, &ua, &fails, &lastLogin, &exp); err != nil {
		t.Fatal(err)
	}
	if ip != "198.51.100.7" || ua != "Mozilla/5.0 test" || fails != 0 || time.Since(lastLogin) > time.Minute ||
		exp.Sub(time.Now()) < 11*time.Hour {
		t.Fatalf("сессия в БД: ip=%s ua=%s fails=%d last=%v exp=%v", ip, ua, fails, lastLogin, exp)
	}
	if e.audit.count("user.login") != 1 {
		t.Fatal("нет аудита user.login")
	}

	// /auth/me: тот же пользователь + cookie переставлена на остаток срока
	w = e.do(t, http.MethodGet, "/auth/me", nil, withCookie(c.Value))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), id.String()) {
		t.Fatalf("me: %d %s", w.Code, w.Body)
	}
	if mc := sessionCookie(t, w); mc.Value != c.Value || mc.MaxAge <= 0 {
		t.Fatalf("me cookie: %+v", mc)
	}

	// промах кэша: сессия читается из БД
	e.svc.PurgeUser(id)
	if w = e.do(t, http.MethodGet, "/auth/me", nil, withCookie(c.Value)); w.Code != http.StatusOK {
		t.Fatalf("me после сброса кэша: %d %s", w.Code, w.Body)
	}

	// logout: 204, cookie очищена, сессия отозвана, дальше 401
	w = e.do(t, http.MethodPost, "/auth/logout", nil, withCookie(c.Value))
	if w.Code != http.StatusNoContent {
		t.Fatalf("logout: %d %s", w.Code, w.Body)
	}
	if lc := sessionCookie(t, w); lc.MaxAge >= 0 || lc.Value != "" {
		t.Fatalf("logout cookie не удаляет сессию: %+v", lc)
	}
	var revoked bool
	if err := pool.QueryRow(context.Background(), `SELECT revoked_at IS NOT NULL FROM auth_sessions WHERE user_id = $1`, id).Scan(&revoked); err != nil || !revoked {
		t.Fatalf("сессия не отозвана: %v %v", revoked, err)
	}
	expectErr(t, e.do(t, http.MethodGet, "/auth/me", nil, withCookie(c.Value)), http.StatusUnauthorized, httpx.CodeUnauthorized)
	if e.audit.count("user.logout") != 1 {
		t.Fatal("нет аудита user.logout")
	}
	// и после истечения свежести кэша — тоже 401 (из БД)
	e.svc.cache.mu.Lock()
	clear(e.svc.cache.m)
	e.svc.cache.mu.Unlock()
	expectErr(t, e.do(t, http.MethodGet, "/auth/me", nil, withCookie(c.Value)), http.StatusUnauthorized, httpx.CodeUnauthorized)
}

// Регрессия (review: «ответ на lockout раскрывает существование логина»): серия неудач по
// реальному и выдуманному логину выглядит одинаково — 5×401, затем 429 с тем же текстом;
// во время блокировки верный пароль неотличим от неверного.
func TestLockoutIndistinguishableForUnknownLogin(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	e := newEnv(t, pool, nil)
	id := seedUser(t, pool, "real_user", "teacher", "right-pass", "active")

	ipN := 0
	nextIP := func() reqOpt { ipN++; return fromIP("203.0.113." + itoa(ipN)) } // IP-лимитер не мешает
	for i := 1; i <= 5; i++ {
		wr := e.login(t, "real_user", "wrong", nextIP())
		wg := e.login(t, "ghost_user", "wrong", nextIP())
		er := expectErr(t, wr, http.StatusUnauthorized, httpx.CodeUnauthorized)
		eg := expectErr(t, wg, http.StatusUnauthorized, httpx.CodeUnauthorized)
		if er.Message != eg.Message || wr.Body.String() != wg.Body.String() {
			t.Fatalf("неудача %d: ответы различаются:\n%s\n%s", i, wr.Body, wg.Body)
		}
	}
	for _, c := range []struct{ login, pw string }{
		{"real_user", "wrong"}, {"ghost_user", "wrong"},
		{"real_user", "right-pass"}, // верный пароль во время блокировки не раскрывается
		{"REAL_USER", "right-pass"}, {"Ghost_User", "anything"},
	} {
		w := e.login(t, c.login, c.pw, nextIP())
		er := expectErr(t, w, http.StatusTooManyRequests, httpx.CodeRateLimited)
		if er.Message != "Слишком много неудачных попыток входа. Повторите через 15 мин." {
			t.Fatalf("%s/%s: сообщение %q", c.login, c.pw, er.Message)
		}
		if ra, _ := strconv.Atoi(w.Header().Get("Retry-After")); ra < 890 || ra > 900 {
			t.Fatalf("%s: Retry-After=%d", c.login, ra)
		}
	}
	var fails int
	var until time.Time
	if err := pool.QueryRow(context.Background(), `SELECT failed_login_count, locked_until FROM users WHERE id = $1`, id).Scan(&fails, &until); err != nil {
		t.Fatal(err)
	}
	if fails != 0 || until.Sub(time.Now()) < 14*time.Minute {
		t.Fatalf("lockout в БД: fails=%d until=%v", fails, until)
	}
	reasons := map[string]int{}
	for _, r := range e.audit.reasons() {
		reasons[r]++
	}
	if want := map[string]int{"wrong_password": 5, "unknown_login": 5, "locked": 3, "unknown_login_locked": 2}; !maps.Equal(reasons, want) {
		t.Fatalf("аудит: %v, want %v", reasons, want)
	}

	// разблокировка администратором (PUT blocked=false делает то же) — снова можно войти
	if _, err := pool.Exec(context.Background(), `UPDATE users SET locked_until = NULL WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if w := e.login(t, "real_user", "right-pass", nextIP()); w.Code != http.StatusOK {
		t.Fatalf("после снятия блокировки: %d %s", w.Code, w.Body)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// Ответ «учётка заблокирована» тратит столько же времени, сколько проверка пароля: argon2
// считается и на этом пути (review: блокировка отвечала за 8 мс против 20 мс проверки).
// Детерминированно: пока слоты argon2 заняты, ответ не приходит. Не параллельный.
func TestLockedPathBurnsArgon2(t *testing.T) {
	pool := pgtest.New(t)
	e := newEnv(t, pool, nil)
	for _, l := range []string{"locked_real", "locked_ghost"} {
		if l == "locked_real" {
			id := seedUser(t, pool, l, "student", "pass-1234", "active")
			if _, err := pool.Exec(context.Background(), `UPDATE users SET locked_until = now() + interval '10 minutes' WHERE id = $1`, id); err != nil {
				t.Fatal(err)
			}
		} else {
			e.svc.ghosts.fail(l, time.Now(), 1, 10*time.Minute)
		}
		for range cap(hashSem) {
			hashSem <- struct{}{}
		}
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- e.login(t, l, "pass-1234", fromIP("192.0.2.90")) }()
		select {
		case w := <-done:
			for range cap(hashSem) {
				<-hashSem
			}
			t.Fatalf("%s: ответ без вычисления argon2: %d %s", l, w.Code, w.Body)
		case <-time.After(100 * time.Millisecond):
		}
		for range cap(hashSem) {
			<-hashSem
		}
		w := <-done
		expectErr(t, w, http.StatusTooManyRequests, httpx.CodeRateLimited)
	}
}

func TestLoginRateLimitPerIP(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	e := newEnv(t, pool, nil)
	seedUser(t, pool, "victim", "student", "pass-1234", "active")
	a := fromIP("192.0.2.50")
	for i := range 5 {
		// разные несуществующие логины: lockout логина не наступает, тратится только бюджет IP
		expectErr(t, e.login(t, "nobody"+itoa(i), "x", a), http.StatusUnauthorized, httpx.CodeUnauthorized)
	}
	w := e.login(t, "victim", "pass-1234", a)
	er := expectErr(t, w, http.StatusTooManyRequests, httpx.CodeRateLimited)
	// ~12 с на жетон (5/мин); пока шли 5 неудач (argon2 под -race), часть уже пополнилась
	if ra, _ := strconv.Atoi(w.Header().Get("Retry-After")); er.Message != "Слишком много попыток входа. Повторите позже." || ra < 1 || ra > 12 {
		t.Fatalf("429: %q Retry-After=%q", er.Message, w.Header().Get("Retry-After"))
	}
	// другой IP не затронут; верные входы бюджет не тратят
	b := fromIP("192.0.2.51")
	for range 8 {
		if w := e.login(t, "victim", "pass-1234", b); w.Code != http.StatusOK {
			t.Fatalf("верный вход с чистого IP: %d %s", w.Code, w.Body)
		}
	}
}

// Регрессия (review: «лимитер режет пачку верных входов с одного IP»): 20 одновременных
// верных входов из-за одного NAT. Детерминированно: все слоты argon2 заняты, пока все 20
// запросов не окажутся внутри лимитера (раньше 6-й получал 429 сразу), затем слоты
// освобождаются. Не параллельный: занимает глобальный семафор argon2.
func TestConcurrentCorrectLoginsFromOneIP(t *testing.T) {
	pool := pgtest.New(t)
	e := newEnv(t, pool, nil)
	const n = 20
	for i := range n {
		seedUser(t, pool, "class"+itoa(i), "student", "pw-"+itoa(i), "active")
	}

	for range cap(hashSem) {
		hashSem <- struct{}{}
	}
	released := false
	release := func() {
		if !released {
			released = true
			for range cap(hashSem) {
				<-hashSem
			}
		}
	}
	defer release()

	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			codes[i] = e.login(t, "class"+itoa(i), "pw-"+itoa(i), fromIP("10.10.10.10")).Code
		})
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		e.svc.limiter.mu.Lock()
		b := e.svc.limiter.buckets["10.10.10.10"]
		inflight := 0
		if b != nil {
			inflight = b.inflight
		}
		e.svc.limiter.mu.Unlock()
		if inflight == n {
			break
		}
		if time.Now().After(deadline) {
			release()
			wg.Wait()
			t.Fatalf("внутри лимитера %d из %d одновременных входов; коды: %v", inflight, n, codes)
		}
		time.Sleep(5 * time.Millisecond)
	}
	release()
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("вход %d: %d (все коды: %v)", i, c, codes)
		}
	}
	e.svc.limiter.mu.Lock()
	b := e.svc.limiter.buckets["10.10.10.10"]
	e.svc.limiter.mu.Unlock()
	if b.inflight != 0 || b.tokens < float64(loginPerMinute)-0.01 {
		t.Fatalf("после верных входов бюджет потрачен: %+v", *b)
	}
}

func TestBlockedUserLogin(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	e := newEnv(t, pool, nil)
	seedUser(t, pool, "blocked1", "student", "pass-1234", "blocked")
	expectErr(t, e.login(t, "blocked1", "pass-1234", fromIP("192.0.2.60")), http.StatusLocked, httpx.CodeUserBlocked)
	expectErr(t, e.login(t, "blocked1", "nope", fromIP("192.0.2.60")), http.StatusUnauthorized, httpx.CodeUnauthorized)
	if got := e.audit.reasons(); strings.Join(got, ",") != "blocked,wrong_password" {
		t.Fatalf("аудит: %v", got)
	}
	var n int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM auth_sessions`).Scan(&n)
	if n != 0 {
		t.Fatal("заблокированному создана сессия")
	}
}

func TestBadHashInDB(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	e := newEnv(t, pool, nil)
	id := seedUser(t, pool, "broken", "student", "x", "active")
	if _, err := pool.Exec(context.Background(), `UPDATE users SET password_hash = 'plain-text' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	expectErr(t, e.login(t, "broken", "plain-text", fromIP("192.0.2.70")), http.StatusUnauthorized, httpx.CodeUnauthorized)
}

func TestAuthenticateSessionStates(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	e := newEnv(t, pool, nil)
	ctx := context.Background()
	id := seedUser(t, pool, "states", "student", "pass-1234", "active")
	me := func(tok string) *httptest.ResponseRecorder {
		return e.do(t, http.MethodGet, "/auth/me", nil, withCookie(tok))
	}

	t.Run("отзыв вне транзакции — сразу", func(t *testing.T) {
		tok := e.mustLogin(t, "states", "pass-1234")
		if err := e.svc.RevokeUserSessions(ctx, pool, id); err != nil {
			t.Fatal(err)
		}
		expectErr(t, me(tok), http.StatusUnauthorized, httpx.CodeUnauthorized)
	})

	t.Run("отзыв в транзакции — кэш чистится после COMMIT", func(t *testing.T) {
		tok := e.mustLogin(t, "states", "pass-1234")
		err := pg.WithTx(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
			if err := e.svc.RevokeUserSessions(ctx, tx, id); err != nil {
				return err
			}
			if e.svc.cache.get(key(tok)) == nil {
				t.Error("кэш очищен до COMMIT")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		expectErr(t, me(tok), http.StatusUnauthorized, httpx.CodeUnauthorized)
	})

	t.Run("откат транзакции — сессия жива", func(t *testing.T) {
		tok := e.mustLogin(t, "states", "pass-1234")
		_ = pg.WithTx(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
			_ = e.svc.RevokeUserSessions(ctx, tx, id)
			return context.Canceled
		})
		e.svc.PurgeUser(id)
		if w := me(tok); w.Code != http.StatusOK {
			t.Fatalf("после отката: %d %s", w.Code, w.Body)
		}
	})

	t.Run("истёкшая", func(t *testing.T) {
		tok := e.mustLogin(t, "states", "pass-1234")
		_, h := hashToken(tok)
		if _, err := pool.Exec(ctx, `UPDATE auth_sessions SET expires_at = now() - interval '1 second' WHERE refresh_token_hash = $1`, h); err != nil {
			t.Fatal(err)
		}
		e.svc.PurgeUser(id)
		expectErr(t, me(tok), http.StatusUnauthorized, httpx.CodeUnauthorized)
	})

	t.Run("неизвестный токен не кэшируется", func(t *testing.T) {
		tok, _ := newToken()
		expectErr(t, me(tok), http.StatusUnauthorized, httpx.CodeUnauthorized)
		if e.svc.cache.get(key(tok)) != nil {
			t.Fatal("неизвестный токен попал в кэш")
		}
	})

	t.Run("заблокирован мимо go-core — 423 и сессия отозвана", func(t *testing.T) {
		tok := e.mustLogin(t, "states", "pass-1234")
		if _, err := pool.Exec(ctx, `UPDATE users SET status = 'blocked' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		e.svc.PurgeUser(id)
		defer func() {
			if _, err := pool.Exec(ctx, `UPDATE users SET status = 'active' WHERE id = $1`, id); err != nil {
				t.Fatal(err)
			}
		}()
		expectErr(t, me(tok), http.StatusLocked, httpx.CodeUserBlocked)
		_, h := hashToken(tok)
		var revoked bool
		_ = pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM auth_sessions WHERE refresh_token_hash = $1`, h).Scan(&revoked)
		if !revoked {
			t.Fatal("сессия заблокированного не отозвана")
		}
		// после разблокировки отозванная сессия не оживает
		if _, err := pool.Exec(ctx, `UPDATE users SET status = 'active' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		e.svc.PurgeUser(id)
		expectErr(t, me(tok), http.StatusUnauthorized, httpx.CodeUnauthorized)
	})

	t.Run("удалённый пользователь", func(t *testing.T) {
		tok := e.mustLogin(t, "states", "pass-1234")
		if _, err := pool.Exec(ctx, `UPDATE users SET deleted_at = now() WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		e.svc.PurgeUser(id)
		expectErr(t, me(tok), http.StatusUnauthorized, httpx.CodeUnauthorized)
	})
}

func TestSessionSlidingExpiry(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	e := newEnv(t, pool, nil)
	seedUser(t, pool, "slider", "student", "pass-1234", "active")
	tok := e.mustLogin(t, "slider", "pass-1234")
	_, h := hashToken(tok)

	// до порога touchEvery в БД не пишем
	ent := e.svc.cache.get(key(tok))
	var seen1 time.Time
	_ = pool.QueryRow(context.Background(), `SELECT last_seen_at FROM auth_sessions WHERE refresh_token_hash = $1`, h).Scan(&seen1)
	if w := e.do(t, http.MethodGet, "/auth/me", nil, withCookie(tok)); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}

	// прошло > touchEvery, до конца срока меньше половины TTL: продлеваем до now+TTL
	soon := time.Now().Add(time.Hour)
	if _, err := pool.Exec(context.Background(), `UPDATE auth_sessions SET expires_at = $2 WHERE refresh_token_hash = $1`, h, soon); err != nil {
		t.Fatal(err)
	}
	ent.expiresAt.Store(soon.UnixNano())
	ent.touchedAt.Store(time.Now().Add(-touchEvery - time.Second).UnixNano())
	w := e.do(t, http.MethodGet, "/auth/me", nil, withCookie(tok))
	if w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	var exp, seen2 time.Time
	_ = pool.QueryRow(context.Background(), `SELECT expires_at, last_seen_at FROM auth_sessions WHERE refresh_token_hash = $1`, h).Scan(&exp, &seen2)
	if exp.Sub(time.Now()) < 11*time.Hour || !seen2.After(seen1) {
		t.Fatalf("срок не продлён: exp=%v seen %v -> %v", exp, seen1, seen2)
	}
	if time.Until(time.Unix(0, ent.expiresAt.Load())) < 11*time.Hour {
		t.Fatal("кэш не знает о продлении")
	}
	if mc := sessionCookie(t, w); mc.MaxAge < 11*3600 {
		t.Fatalf("/auth/me не переставил cookie на новый срок: %+v", mc)
	}
	if e.svc.ActiveSessions() != 1 {
		t.Fatalf("ActiveSessions=%d", e.svc.ActiveSessions())
	}
}

func TestSeedDemoUsers(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	ctx := context.Background()
	if err := SeedDemoUsers(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var hashBefore string
	_ = pool.QueryRow(ctx, `SELECT password_hash FROM users WHERE login = 'teacher'`).Scan(&hashBefore)
	// демонстрация поменяла данные: блокировка и ФИО должны пережить/не пережить рестарт как задумано
	if _, err := pool.Exec(ctx, `UPDATE users SET status = 'blocked' WHERE login = 'student2';
UPDATE users SET last_name = 'Изменена' WHERE login = 'teacher'`); err != nil {
		t.Fatal(err)
	}
	if err := SeedDemoUsers(ctx, pool); err != nil {
		t.Fatalf("повторный сид: %v", err)
	}

	rows, err := pool.Query(ctx, `
SELECT u.login::text, u.role, u.last_name, u.status, COALESCE(s.code, ''), COALESCE(u.workstation, ''), u.must_change_password,
       ARRAY(SELECT g.name FROM group_members gm JOIN groups g ON g.id = gm.group_id WHERE gm.user_id = u.id)
  FROM users u LEFT JOIN services s ON s.id = u.service_id ORDER BY u.login`)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		login, role, last, status, svc, ws string
		mustChange                         bool
		groups                             []string
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.login, &r.role, &r.last, &r.status, &r.svc, &r.ws, &r.mustChange, &r.groups); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	rows.Close()
	if len(got) != 4 {
		t.Fatalf("пользователей %d, want 4: %+v", len(got), got)
	}
	byLogin := map[string]row{}
	for _, r := range got {
		byLogin[r.login] = r
		if r.mustChange {
			t.Fatalf("%s: демо-учётке не нужна смена пароля", r.login)
		}
	}
	if r := byLogin["teacher"]; r.role != "teacher" || r.last != "Ковалёва" {
		t.Fatalf("teacher: профиль не восстановлен сидом: %+v", r)
	}
	if r := byLogin["student"]; r.svc != "zhkh" || r.ws != "АРМ 4" || len(r.groups) != 1 || r.groups[0] != demoGroupName {
		t.Fatalf("student: %+v", r)
	}
	if r := byLogin["student2"]; r.status != "blocked" || len(r.groups) != 1 {
		t.Fatalf("student2: блокировку снял сид или нет группы: %+v", r)
	}
	var hashAfter string
	_ = pool.QueryRow(ctx, `SELECT password_hash FROM users WHERE login = 'teacher'`).Scan(&hashAfter)
	if hashAfter != hashBefore {
		t.Fatal("повторный сид перезаписал пароль")
	}
	var groups int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM groups`).Scan(&groups)
	if groups != 1 {
		t.Fatalf("групп %d после двух сидов, want 1", groups)
	}

	// пароль демо-учётки = логин
	e := newEnv(t, pool, nil)
	if w := e.login(t, "admin", "admin", fromIP("192.0.2.80")); w.Code != http.StatusOK {
		t.Fatalf("вход admin/admin: %d %s", w.Code, w.Body)
	}
}
