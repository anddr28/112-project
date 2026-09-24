package app

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/config"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/platform/pgtest"
)

const specPath = "../../../contracts/openapi/frontend.v1.yaml"

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// buildCore — полное ядро поверх свежей БД (без слушателей и фоновых воркеров).
func buildCore(t *testing.T, tune func(*config.Config)) *Core {
	t.Helper()
	_, url := pgtest.NewWithURL(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DatabaseURL = url
	cfg.DBMaxConns = 6
	cfg.StaticDir = ""
	cfg.TTSDir = t.TempDir()
	cfg.BackupDir = t.TempDir()
	cfg.TLSDir = t.TempDir()
	cfg.DemoMode = true
	cfg.HTTPServeAPI = false
	cfg.CookieSecure = true
	if tune != nil {
		tune(cfg)
	}
	c, err := Build(context.Background(), cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.Hub.Close()
		c.Pool.Close() // до DROP DATABASE из cleanup pgtest (LIFO)
	})
	return c
}

var (
	reSpecPath   = regexp.MustCompile(`^  (/[^\s:]*):\s*$`)
	reSpecMethod = regexp.MustCompile(`^    (get|put|post|patch|delete|head|options|trace):`)
	reParam      = regexp.MustCompile(`\{[^}]*\}`)
)

// normalize — "GET /media/tts/{path...}" и "GET /media/tts/{path}" -> "GET /media/tts/{}".
func normalize(op string) string { return reParam.ReplaceAllString(op, "{}") }

// specOperations — операции секции paths контракта (маленький построчный разбор YAML
// без зависимостей: пути — ключи с отступом 2, методы — с отступом 4).
func specOperations(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open(specPath)
	if err != nil {
		t.Fatalf("контракт: %v", err)
	}
	defer f.Close()
	ops := map[string]string{} // нормализованная -> как в контракте
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	inPaths, cur := false, ""
	for sc.Scan() {
		line := sc.Text()
		if line == "paths:" {
			inPaths = true
			continue
		}
		if !inPaths {
			continue
		}
		if line != "" && line[0] != ' ' && line[0] != '#' {
			break // следующая секция верхнего уровня
		}
		if m := reSpecPath.FindStringSubmatch(line); m != nil {
			cur = m[1]
			continue
		}
		if m := reSpecMethod.FindStringSubmatch(line); m != nil && cur != "" {
			op := strings.ToUpper(m[1]) + " " + cur
			ops[normalize(op)] = op
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ops) < 50 {
		t.Fatalf("разбор контракта нашёл подозрительно мало операций: %d", len(ops))
	}
	return ops
}

// Каждая операция frontend.v1.yaml зарегистрирована на роутере, и на роутере нет
// маршрутов вне контракта (WS в контракте описаны как GET).
func TestRoutesMatchContract(t *testing.T) {
	t.Parallel()
	spec := specOperations(t)
	c := buildCore(t, nil)
	registered := map[string]string{}
	for _, r := range c.Router.Routes() {
		n := normalize(r)
		if prev, dup := registered[n]; dup {
			t.Errorf("маршрут зарегистрирован дважды: %q и %q", prev, r)
		}
		registered[n] = r
	}
	var missing, extra []string
	for n, op := range spec {
		if _, ok := registered[n]; !ok {
			missing = append(missing, op)
		}
	}
	for n, r := range registered {
		if _, ok := spec[n]; !ok {
			extra = append(extra, r)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("операции контракта без маршрута:\n  %s", strings.Join(missing, "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("маршруты вне контракта:\n  %s", strings.Join(extra, "\n  "))
	}
}

// Каждая операция контракта реально доходит через публичный обработчик до своего маршрута:
// без сессии — 401 ApiError (публичные login/demo-accounts — не 401 и не 404).
func TestContractOperationsDispatch(t *testing.T) {
	t.Parallel()
	spec := specOperations(t)
	c := buildCore(t, nil)
	public := map[string]bool{"POST /auth/login": true, "GET /auth/demo-accounts": true}
	fill := strings.NewReplacer(
		"{path}", "dialog/018f6b2a-0000-7000-8000-000000000001/1.wav",
		"{key}", "time_limit_sec",
	)
	for _, op := range spec {
		method, path, _ := strings.Cut(op, " ")
		p := fill.Replace(path)
		p = reParam.ReplaceAllString(p, "018f6b2a-0000-7000-8000-000000000001")
		req := httptest.NewRequest(method, "https://localhost/api/v1"+p, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Requested-With", "fetch")
		rr := httptest.NewRecorder()
		c.Public.ServeHTTP(rr, req)
		var e struct{ Code, Message string }
		_ = json.Unmarshal(rr.Body.Bytes(), &e)
		switch {
		case public[op]:
			if rr.Code == 401 || rr.Code == 404 || rr.Code >= 500 {
				t.Errorf("%s: публичная операция ответила %d %s", op, rr.Code, rr.Body)
			}
		case rr.Code != http.StatusUnauthorized || e.Code != "unauthorized":
			t.Errorf("%s: без сессии %d %q (ожидался 401 unauthorized — маршрут не найден?)", op, rr.Code, rr.Body)
		}
	}

	// неизвестный путь и чужой метод — ApiError not_found, а не text/plain от ServeMux
	for _, tc := range [][2]string{{"GET", "/api/v1/nope"}, {"DELETE", "/api/v1/auth/me"}} {
		req := httptest.NewRequest(tc[0], "https://localhost"+tc[1], nil)
		req.Header.Set("X-Requested-With", "fetch")
		rr := httptest.NewRecorder()
		c.Public.ServeHTTP(rr, req)
		if rr.Code != 404 || !strings.Contains(rr.Body.String(), `"code":"not_found"`) ||
			rr.Header().Get("Content-Type") != "application/json; charset=utf-8" {
			t.Errorf("%v: %d %q %s", tc, rr.Code, rr.Header().Get("Content-Type"), rr.Body)
		}
	}
	// callback ai-service и /metrics на публичном (HTTPS) входе не раздаются
	for _, p := range []string{"/internal/ai/v1/results", "/metrics"} {
		req := httptest.NewRequest("POST", "https://localhost"+p, strings.NewReader("{}"))
		if p == "/metrics" {
			req = httptest.NewRequest("GET", "https://localhost"+p, nil)
		}
		rr := httptest.NewRecorder()
		c.Public.ServeHTTP(rr, req)
		if rr.Code == 200 || rr.Code == 401 {
			t.Errorf("%s на публичном входе: %d", p, rr.Code)
		}
	}
}

// Регрессия (ревью: transport-security). Открытый HTTP-слушатель по умолчанию отдаёт только
// callback ai-service, /metrics и health — не API (пароль и cookie открытым текстом) и не SPA.
func TestInternalListenerDoesNotServeAPI(t *testing.T) {
	t.Parallel()
	c := buildCore(t, nil)
	get := func(h http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://go-core:8080"+path, strings.NewReader(`{"login":"admin","password":"admin"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Requested-With", "fetch")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/v1/auth/login"},
		{"GET", "/api/v1/auth/demo-accounts"},
		{"GET", "/api/v1/auth/me"},
		{"GET", "/"},
	} {
		if rr := get(c.Internal, tc.method, tc.path, nil); rr.Code != http.StatusNotFound || rr.Header().Get("Set-Cookie") != "" {
			t.Errorf("internal %s %s: %d (API/SPA по открытому HTTP)", tc.method, tc.path, rr.Code)
		}
	}
	if rr := get(c.Internal, "GET", "/healthz", nil); rr.Code != 200 {
		t.Errorf("healthz: %d", rr.Code)
	}
	if rr := get(c.Internal, "GET", "/readyz", nil); rr.Code != 200 {
		t.Errorf("readyz: %d", rr.Code)
	}
	if rr := get(c.Internal, "GET", "/metrics", nil); rr.Code != 200 || !strings.Contains(rr.Body.String(), "go_goroutines") {
		t.Errorf("metrics: %d", rr.Code)
	}
	if rr := get(c.Internal, "POST", "/internal/ai/v1/results", nil); rr.Code != http.StatusUnauthorized {
		t.Errorf("callback без токена: %d", rr.Code)
	}

	// явное включение (за reverse-proxy с TLS) — API доступно и по HTTP
	c2 := buildCore(t, func(cfg *config.Config) { cfg.HTTPServeAPI = true })
	if rr := get(c2.Internal, "GET", "/api/v1/auth/me", nil); rr.Code != http.StatusUnauthorized {
		t.Errorf("GOCORE_HTTP_API=true: /auth/me %d", rr.Code)
	}
}

// Регрессия (ревью: insecure-defaults). serve без демо-режима с общеизвестным токеном не
// стартует — ещё до миграций и подключения к БД.
func TestServeRefusesInsecureConfig(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{DemoMode: false, InternalAPIToken: config.DevInternalToken,
		DatabaseURL: "postgres://nobody@127.0.0.1:1/none", AutoMigrate: true, MigrationsDir: "/nonexistent"}
	err := Serve(context.Background(), cfg, quietLog())
	if err == nil || !strings.Contains(err.Error(), "INTERNAL_API_TOKEN") {
		t.Fatalf("Serve: %v", err)
	}
}

// Смоук сборки: сиды (справочники + демо) идемпотентны, демо-вход работает через публичный
// обработчик, cookie сессии — Secure/HttpOnly, /auth/me отвечает по контракту.
func TestSeedAndDemoLogin(t *testing.T) {
	t.Parallel()
	c := buildCore(t, nil)
	ctx := context.Background()
	c.Audit.Start(ctx)
	t.Cleanup(func() { _ = c.Audit.Stop(context.Background()) })
	for i := 0; i < 2; i++ {
		if err := c.Seed(ctx, true); err != nil {
			t.Fatalf("seed #%d: %v", i+1, err)
		}
	}
	do := func(method, path, body, cookie string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "https://localhost/api/v1"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Requested-With", "fetch")
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		rr := httptest.NewRecorder()
		c.Public.ServeHTTP(rr, req)
		return rr
	}
	rr := do("GET", "/auth/demo-accounts", "", "")
	var demo []map[string]any
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &demo) != nil || len(demo) == 0 {
		t.Fatalf("demo-accounts: %d %s", rr.Code, rr.Body)
	}
	rr = do("POST", "/auth/login", `{"login":"teacher","password":"teacher"}`, "")
	if rr.Code != 200 {
		t.Fatalf("login: %d %s", rr.Code, rr.Body)
	}
	var sess *http.Cookie
	for _, ck := range rr.Result().Cookies() {
		if ck.Name == "lct_session" {
			sess = ck
		}
	}
	if sess == nil || !sess.Secure || !sess.HttpOnly || sess.Value == "" {
		t.Fatalf("cookie сессии: %+v", sess)
	}
	rr = do("GET", "/auth/me", "", "lct_session="+sess.Value)
	var me map[string]any
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &me) != nil || me["role"] != "teacher" || me["login"] != "teacher" {
		t.Fatalf("me: %d %s", rr.Code, rr.Body)
	}
	if rr.Header().Get("X-Request-Id") == "" || rr.Header().Get("X-Content-Type-Options") == "" {
		t.Errorf("заголовки: %v", rr.Header())
	}
	// роль не пускает: преподаватель на админский маршрут — 403 ApiError
	rr = do("GET", "/admin/settings", "", "lct_session="+sess.Value)
	if rr.Code != 403 || !strings.Contains(rr.Body.String(), `"code":"forbidden"`) {
		t.Fatalf("admin/settings преподавателем: %d %s", rr.Code, rr.Body)
	}
	// списки — массив (демо-сценарии засеяны), а не null; фильтр без совпадений — [], не null
	for _, q := range []string{"/scenarios", "/scenarios?status=archived&mode=both"} {
		rr = do("GET", q, "", "lct_session="+sess.Value)
		var list []map[string]any
		if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &list) != nil || list == nil {
			t.Fatalf("%s: %d %.200s", q, rr.Code, rr.Body)
		}
	}
	rr = do("POST", "/auth/logout", "", "lct_session="+sess.Value)
	if rr.Code != 204 && rr.Code != 200 {
		t.Fatalf("logout: %d", rr.Code)
	}
	if rr = do("GET", "/auth/me", "", "lct_session="+sess.Value); rr.Code != 401 {
		t.Fatalf("сессия жива после logout: %d", rr.Code)
	}
}

func TestNormalize(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"GET /media/tts/{path...}":                  "GET /media/tts/{}",
		"GET /media/tts/{path}":                     "GET /media/tts/{}",
		"DELETE /attempts/{attemptId}/services/{x}": "DELETE /attempts/{}/services/{}",
		"GET /auth/me":                              "GET /auth/me",
	} {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q", in, got)
		}
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// Serve целиком: миграции (no-op), сиды, воркеры, два слушателя; HTTPS отдаёт API, HTTP —
// только служебное; отмена контекста — корректная остановка без ошибки.
func TestServeLifecycle(t *testing.T) {
	t.Parallel()
	_, url := pgtest.NewWithURL(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DatabaseURL = url
	cfg.DBMaxConns = 6
	cfg.MigrationsDir = pgtest.MigrationsDir()
	cfg.AutoMigrate = true
	cfg.SeedOnStart = true
	cfg.DemoMode = true
	cfg.StaticDir = ""
	cfg.HTTPServeAPI = false
	cfg.AIServiceURL = "http://127.0.0.1:1" // ai-service нет: задачи ждут, breaker открывается
	cfg.TTSDir, cfg.BackupDir, cfg.TLSDir = t.TempDir(), t.TempDir(), t.TempDir()
	cfg.HTTPAddr, cfg.HTTPSAddr = freeAddr(t), freeAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, cfg, quietLog()) }()

	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} // CA контура самоподписанный
	get := func(u string) (int, string) {
		resp, err := hc.Get(u)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if st, _ := get("http://" + cfg.HTTPAddr + "/readyz"); st == 200 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("Serve завершился: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("сервер не поднялся")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st, body := get("https://" + cfg.HTTPSAddr + "/api/v1/auth/demo-accounts"); st != 200 || !strings.Contains(body, "teacher") {
		t.Errorf("HTTPS demo-accounts: %d %.200s", st, body)
	}
	if st, _ := get("https://" + cfg.HTTPSAddr + "/api/v1/auth/me"); st != 401 {
		t.Errorf("HTTPS me: %d", st)
	}
	if st, _ := get("http://" + cfg.HTTPAddr + "/api/v1/auth/demo-accounts"); st != 404 {
		t.Errorf("API по открытому HTTP: %d", st)
	}
	if st, body := get("http://" + cfg.HTTPAddr + "/metrics"); st != 200 || !strings.Contains(body, "pgxpool_max_conns 6") {
		t.Errorf("metrics: %d", st)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve после отмены: %v", err)
		}
	case <-time.After(shutdownTimeout + 5*time.Second):
		t.Fatal("Serve не остановился")
	}
	if st, _ := get("http://" + cfg.HTTPAddr + "/healthz"); st != 0 {
		t.Errorf("слушатель жив после остановки: %d", st)
	}
}

// Занятый порт — ошибка сразу, до запуска воркеров и сидов.
func TestServeBusyPort(t *testing.T) {
	t.Parallel()
	_, url := pgtest.NewWithURL(t)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DatabaseURL = url
	cfg.AutoMigrate = false
	cfg.SeedOnStart = true
	cfg.DemoMode = true
	cfg.HTTPAddr, cfg.HTTPSAddr = busy.Addr().String(), ""
	cfg.TTSDir, cfg.BackupDir, cfg.TLSDir = t.TempDir(), t.TempDir(), t.TempDir()
	err = Serve(context.Background(), cfg, quietLog())
	if err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("Serve на занятом порту: %v", err)
	}
	// сиды не выполнялись: пользователей нет
	pool := pgtestPool(t, url)
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("сиды выполнены до открытия слушателей: users=%d err=%v", n, err)
	}
}

func pgtestPool(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	pool, err := pg.Connect(context.Background(), url, 2, "app-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
