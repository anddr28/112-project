package ops

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lct/gocore/internal/platform/httpx"
)

const indexHTML = "<!doctype html><title>АРМ-112</title>"

func spaDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"index.html":             indexHTML,
		"favicon.svg":            "<svg/>",
		"robots.txt":             "User-agent: *",
		"assets/app-3f2a.js":     "console.log('plain')",
		"assets/app-3f2a.js.br":  "BROTLI",
		"assets/app-3f2a.js.gz":  "GZIP",
		"assets/app-3f2a.css":    "body{}",
		"assets/logo-9c1d.png":   "\x89PNG",
		"sub/dir/.keep":          "",
		"lessons/index.html.bak": "x",
	}
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP-SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func serve(h http.Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSPAHandlerRouting(t *testing.T) {
	t.Parallel()
	h := SPAHandler(spaDir(t))
	tests := []struct {
		name, method, path string
		code               int
		body               string // подстрока; "" — не проверять
		cache              string
	}{
		{"root", "GET", "/", 200, indexHTML, cacheRevalidate},
		{"index explicit", "GET", "/index.html", 200, indexHTML, cacheRevalidate},
		{"client route", "GET", "/lessons/0192f/attempts", 200, indexHTML, cacheRevalidate},
		{"client route trailing slash", "GET", "/admin/", 200, indexHTML, cacheRevalidate},
		{"asset", "GET", "/assets/app-3f2a.css", 200, "body{}", cacheImmutable},
		{"asset dir", "GET", "/assets/", 200, indexHTML, cacheRevalidate},
		{"root file", "GET", "/favicon.svg", 200, "<svg/>", cacheRevalidate},
		{"missing asset is 404", "GET", "/assets/app-0000.js", 404, "", ""},
		{"missing file with ext is 404", "GET", "/missing.png", 404, "", ""},
		{"directory is spa", "GET", "/sub/dir", 200, indexHTML, cacheRevalidate},
		{"traversal cleaned", "GET", "/../../etc/passwd", 200, indexHTML, cacheRevalidate},
		{"symlink escape blocked", "GET", "/leak.txt", 404, "", ""},
		{"metrics reserved", "GET", "/metrics", 404, "", ""},
		{"healthz reserved", "GET", "/healthz", 404, "", ""},
		{"readyz sub reserved", "GET", "/readyz/x", 404, "", ""},
		{"internal reserved", "GET", "/internal/ai/v1/results", 404, "", ""},
		{"metrics-like route is spa", "GET", "/metricsboard", 200, indexHTML, cacheRevalidate},
		{"post not allowed", "POST", "/", 405, "", ""},
		{"head", "HEAD", "/", 200, "", cacheRevalidate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := serve(h, tt.method, tt.path, nil)
			if rec.Code != tt.code {
				t.Fatalf("code = %d, want %d (%s)", rec.Code, tt.code, rec.Body)
			}
			if tt.body != "" && !strings.Contains(rec.Body.String(), tt.body) {
				t.Fatalf("body = %q", rec.Body)
			}
			if tt.cache != "" && rec.Header().Get("Cache-Control") != tt.cache {
				t.Fatalf("Cache-Control = %q, want %q", rec.Header().Get("Cache-Control"), tt.cache)
			}
			if strings.Contains(rec.Body.String(), "TOP-SECRET") {
				t.Fatal("файл вне каталога отдан")
			}
			if tt.method == "HEAD" && rec.Body.Len() != 0 {
				t.Fatal("HEAD с телом")
			}
			if tt.code == 405 && rec.Header().Get("Allow") != "GET, HEAD" {
				t.Fatalf("Allow = %q", rec.Header().Get("Allow"))
			}
		})
	}
}

func TestSPAHandlerAPINotFoundIsJSON(t *testing.T) {
	t.Parallel()
	h := SPAHandler(spaDir(t))
	for _, p := range []string{"/api", "/api/", "/api/v1/nope"} {
		rec := serve(h, "GET", p, nil)
		if rec.Code != 404 || rec.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("%s: %d %v", p, rec.Code, rec.Header())
		}
		var body struct{ Code, Message string }
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Code != httpx.CodeNotFound || body.Message == "" {
			t.Fatalf("%s: body %s", p, rec.Body)
		}
	}
	// POST на /api/… тоже 404 JSON, а не 405 SPA
	if rec := serve(h, "POST", "/api/v1/nope", nil); rec.Code != 404 {
		t.Fatalf("POST /api: %d", rec.Code)
	}
}

func TestSPAHandlerPrecompressed(t *testing.T) {
	t.Parallel()
	h := SPAHandler(spaDir(t))
	tests := []struct {
		ae, enc, body string
	}{
		{"gzip, deflate, br", "br", "BROTLI"},
		{"br;q=0, gzip", "gzip", "GZIP"},
		{"gzip;q=0.5", "gzip", "GZIP"},
		{"identity", "", "console.log('plain')"},
		{"", "", "console.log('plain')"},
		{"BR", "br", "BROTLI"},
		{"br;q=0, gzip;q=0", "", "console.log('plain')"},
	}
	for _, tt := range tests {
		rec := serve(h, "GET", "/assets/app-3f2a.js", map[string]string{"Accept-Encoding": tt.ae})
		if rec.Code != 200 || rec.Header().Get("Content-Encoding") != tt.enc || rec.Body.String() != tt.body {
			t.Errorf("AE %q: %d enc=%q body=%q", tt.ae, rec.Code, rec.Header().Get("Content-Encoding"), rec.Body)
		}
		if rec.Header().Get("Vary") != "Accept-Encoding" || !strings.Contains(rec.Header().Get("Content-Type"), "javascript") {
			t.Errorf("AE %q: headers %v", tt.ae, rec.Header())
		}
	}
	// Картинка не сжимается: без Vary
	if rec := serve(h, "GET", "/assets/logo-9c1d.png", map[string]string{"Accept-Encoding": "br"}); rec.Header().Get("Vary") != "" || rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("png: %v", rec.Header())
	}
}

func TestSPAHandlerETagAndSecurityHeaders(t *testing.T) {
	t.Parallel()
	h := SPAHandler(spaDir(t))
	rec := serve(h, "GET", "/", nil)
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("нет ETag")
	}
	for k, v := range map[string]string{
		"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "same-origin",
		"Permissions-Policy": "microphone=(self)", "Content-Type": "text/html; charset=utf-8",
	} {
		if rec.Header().Get(k) != v {
			t.Errorf("%s = %q, want %q", k, rec.Header().Get(k), v)
		}
	}
	if rec.Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS без TLS")
	}
	if rec := serve(h, "GET", "/", map[string]string{"If-None-Match": etag}); rec.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match: %d", rec.Code)
	}
	// разные варианты сжатия — разные ETag
	e1 := serve(h, "GET", "/assets/app-3f2a.js", nil).Header().Get("ETag")
	e2 := serve(h, "GET", "/assets/app-3f2a.js", map[string]string{"Accept-Encoding": "br"}).Header().Get("ETag")
	if e1 == e2 || !strings.HasSuffix(e2, `-br"`) {
		t.Fatalf("etag identity=%s br=%s", e1, e2)
	}
	// ассеты — без заголовков HTML-документа
	if rec := serve(h, "GET", "/assets/app-3f2a.css", nil); rec.Header().Get("X-Frame-Options") != "" {
		t.Error("X-Frame-Options на css")
	}
}

func TestSPAHandlerMissingDir(t *testing.T) {
	t.Parallel()
	if rec := serve(SPAHandler(""), "GET", "/", nil); rec.Code != 404 {
		t.Fatalf("пустой каталог: %d", rec.Code)
	}
	dir := filepath.Join(t.TempDir(), "dist")
	h := SPAHandler(dir).(*spa)
	if rec := serve(h, "GET", "/", nil); rec.Code != 404 || !strings.Contains(rec.Body.String(), "не собран") {
		t.Fatalf("нет каталога: %d %s", rec.Code, rec.Body)
	}
	// фронт собрали после старта ядра: после паузы rootRetry каталог подхватывается
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(indexHTML), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := serve(h, "GET", "/", nil); rec.Code != 404 {
		t.Fatalf("повтор раньше rootRetry: %d", rec.Code)
	}
	h.mu.Lock()
	h.retryAt = time.Now().Add(-time.Second)
	h.mu.Unlock()
	if rec := serve(h, "GET", "/", nil); rec.Code != 200 {
		t.Fatalf("после rootRetry: %d", rec.Code)
	}
	// каталог без index.html
	empty := t.TempDir()
	if rec := serve(SPAHandler(empty), "GET", "/some/route", nil); rec.Code != 404 {
		t.Fatalf("без index.html: %d", rec.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	t.Parallel()
	h := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/classifier" {
			w.Header().Set("Cache-Control", "private, max-age=60") // справочник со своим кэшем
		}
		w.WriteHeader(http.StatusOK)
	}))
	rec := serve(h, "GET", "/api/v1/me", nil)
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("api: %v", rec.Header())
	}
	if rec := serve(h, "GET", "/api/v1/classifier", nil); rec.Header().Get("Cache-Control") != "private, max-age=60" {
		t.Errorf("override: %v", rec.Header())
	}
	if rec := serve(h, "GET", "/lessons", nil); rec.Header().Get("Cache-Control") != "" {
		t.Errorf("spa: %v", rec.Header())
	}

	req := httptest.NewRequest("GET", "https://trainer.local/api/v1/me", nil)
	req.TLS = &tls.ConnectionState{}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Strict-Transport-Security") != hstsValue {
		t.Errorf("HSTS по TLS: %v", rec.Header())
	}
}

func TestWantHSTS(t *testing.T) {
	t.Parallel()
	tests := []struct {
		host string
		tls  bool
		want bool
	}{
		{"trainer.local", false, false},
		{"trainer.local", true, true},
		{"trainer.local:8443", true, true},
		{"10.0.0.5:8443", true, true},
		{"localhost:8443", true, false},
		{"LOCALHOST", true, false},
		{"127.0.0.1:8443", true, false},
		{"127.8.8.8", true, false},
		{"[::1]:8443", true, false},
		{"::1", true, false},
	}
	for _, tt := range tests {
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = tt.host
		if tt.tls {
			r.TLS = &tls.ConnectionState{}
		}
		if got := wantHSTS(r); got != tt.want {
			t.Errorf("wantHSTS(%q, tls=%v) = %v, want %v", tt.host, tt.tls, got, tt.want)
		}
	}
}

func TestAcceptsEncoding(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ae, tok string
		want    bool
	}{
		{"gzip, deflate, br", "br", true},
		{"gzip, deflate, br", "gzip", true},
		{"br;q=0", "br", false},
		{"br ; q=0.0", "br", false},
		{"br;q=0.001", "br", true},
		{"br;q=abc", "br", false},
		{"BR", "br", true},
		{"x-gzip", "gzip", false},
		{"", "gzip", false},
		{"*", "gzip", false},
		{"gzip;level=1", "gzip", true},
	}
	for _, tt := range tests {
		if got := acceptsEncoding(tt.ae, tt.tok); got != tt.want {
			t.Errorf("acceptsEncoding(%q, %q) = %v, want %v", tt.ae, tt.tok, got, tt.want)
		}
	}
}

func TestHasReservedPrefix(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		"/api/": true, "/api/v1/x": true, "/internal/": true, "/metrics": true, "/metrics/": true,
		"/healthz": true, "/readyz": true, "/readyz/deep": true,
		"/api": false, "/apix": false, "/metricsfoo": false, "/healthzz": false, "/": false, "/lessons": false,
	}
	for p, want := range tests {
		if got := hasReservedPrefix(p); got != want {
			t.Errorf("hasReservedPrefix(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestCompressible(t *testing.T) {
	t.Parallel()
	for _, ext := range []string{".js", ".mjs", ".css", ".html", ".json", ".svg", ".map", ".wasm", ".webmanifest"} {
		if !compressible(ext) {
			t.Errorf("%s должен сжиматься", ext)
		}
	}
	for _, ext := range []string{".png", ".jpg", ".woff2", ".br", ".gz", ""} {
		if compressible(ext) {
			t.Errorf("%s не должен сжиматься", ext)
		}
	}
}
