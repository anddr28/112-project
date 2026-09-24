package ops

import (
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"lct/gocore/internal/platform/httpx"
)

// Префиксы, которые никогда не отдаются как SPA (index.html вместо 404 сбил бы с толку
// клиентов API и мониторинг).
var reservedPrefixes = []string{"/api/", "/internal/", "/metrics", "/healthz", "/readyz"}

const (
	cacheImmutable  = "public, max-age=31536000, immutable" // хэшированные ассеты Vite (/assets/*)
	cacheRevalidate = "no-cache"                            // index.html и прочее: всегда сверять с сервером
	hstsValue       = "max-age=31536000"
	rootRetry       = 5 * time.Second
)

// SPAHandler — раздача собранного SPA из dir (frontend/dist):
//   - существующий файл — как есть; /assets/* (имена с хэшем) кэшируются навсегда, остальное no-cache;
//   - предсжатые .br/.gz рядом с файлом отдаются, если клиент их принимает;
//   - прочие GET/HEAD — index.html (маршрутизация на клиенте), кроме /api/, /internal/,
//     /metrics, /healthz, /readyz (404) и отсутствующих файлов с расширением или под /assets/
//     (404: иначе браузер получил бы HTML вместо чанка JS и упал с непонятной ошибкой MIME).
//
// Доступ к файлам — через os.Root: выход за dir невозможен ни через "..", ни через симлинки.
func SPAHandler(dir string) http.Handler {
	return &spa{dir: dir}
}

type spa struct {
	dir string

	mu      sync.Mutex
	root    *os.Root
	retryAt time.Time
}

// openRoot — каталог открывается лениво и переоткрывается, если его ещё нет (dev: фронт
// собирается после старта ядра). Неудачные попытки — не чаще раза в rootRetry.
func (s *spa) openRoot() *os.Root {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root != nil {
		return s.root
	}
	if s.dir == "" || time.Now().Before(s.retryAt) {
		return nil
	}
	r, err := os.OpenRoot(s.dir)
	if err != nil {
		s.retryAt = time.Now().Add(rootRetry)
		return nil
	}
	s.root = r
	return r
}

func (s *spa) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if p == "/api" || hasReservedPrefix(p) {
		notFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	root := s.openRoot()
	if root == nil {
		http.Error(w, "Интерфейс не собран", http.StatusNotFound)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+p), "/")
	if name != "" && !strings.HasSuffix(p, "/") {
		if s.serveFile(w, r, root, name) {
			return
		}
		if strings.HasPrefix(name, "assets/") || strings.Contains(path.Base(name), ".") {
			http.NotFound(w, r)
			return
		}
	}
	if !s.serveFile(w, r, root, "index.html") {
		http.Error(w, "Интерфейс не собран", http.StatusNotFound)
	}
}

func hasReservedPrefix(p string) bool {
	for _, pre := range reservedPrefixes {
		if strings.HasPrefix(p, pre) {
			// "/metrics" и "/metricsfoo" — разные вещи: префикс без "/" на конце — только целое слово
			if strings.HasSuffix(pre, "/") || len(p) == len(pre) || p[len(pre)] == '/' {
				return true
			}
		}
	}
	return false
}

func notFound(w http.ResponseWriter, r *http.Request) {
	if p := r.URL.Path; p == "/api" || strings.HasPrefix(p, "/api/") {
		w.Header().Set("Cache-Control", "no-store")
		httpx.WriteError(w, httpx.NotFound("Маршрут API не найден"))
		return
	}
	http.NotFound(w, r)
}

// serveFile отдаёт обычный файл name из root (false — нет такого файла или это каталог).
func (s *spa) serveFile(w http.ResponseWriter, r *http.Request, root *os.Root, name string) bool {
	f, fi, ok := openRegular(root, name)
	if !ok {
		return false
	}
	defer f.Close()

	h := w.Header()
	ext := path.Ext(name)
	if ct := mime.TypeByExtension(ext); ct != "" {
		h.Set("Content-Type", ct)
	}
	if strings.HasPrefix(name, "assets/") {
		h.Set("Cache-Control", cacheImmutable)
	} else {
		h.Set("Cache-Control", cacheRevalidate)
	}
	if ext == ".html" {
		setHTMLSecurityHeaders(h, r)
	}

	var content io.ReadSeeker = f
	mod, size, suffix := fi.ModTime(), fi.Size(), ""
	if compressible(ext) {
		h.Add("Vary", "Accept-Encoding")
		ae := r.Header.Get("Accept-Encoding")
		for _, enc := range [...]struct{ token, ext string }{{"br", ".br"}, {"gzip", ".gz"}} {
			if !acceptsEncoding(ae, enc.token) {
				continue
			}
			if cf, cfi, ok := openRegular(root, name+enc.ext); ok {
				defer cf.Close()
				h.Set("Content-Encoding", enc.token)
				content, mod, size, suffix = cf, cfi.ModTime(), cfi.Size(), "-"+enc.token
				break
			}
		}
	}
	// ETag по времени и размеру: If-None-Match → 304 без чтения файла
	h.Set("ETag", `"`+strconv.FormatInt(mod.UnixNano(), 36)+"-"+strconv.FormatInt(size, 36)+suffix+`"`)
	http.ServeContent(w, r, name, mod, content)
	return true
}

func openRegular(root *os.Root, name string) (*os.File, fs.FileInfo, bool) {
	f, err := root.Open(name)
	if err != nil {
		return nil, nil, false
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, nil, false
	}
	return f, fi, true
}

func compressible(ext string) bool {
	switch ext {
	case ".js", ".mjs", ".css", ".html", ".json", ".svg", ".map", ".txt", ".xml", ".wasm", ".webmanifest", ".ico":
		return true
	}
	return false
}

// acceptsEncoding — token есть в Accept-Encoding и не запрещён q=0.
func acceptsEncoding(ae, token string) bool {
	for part := range strings.SplitSeq(ae, ",") {
		enc, params, _ := strings.Cut(part, ";")
		if !strings.EqualFold(strings.TrimSpace(enc), token) {
			continue
		}
		if q, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(q), 64)
			return err == nil && f > 0
		}
		return true
	}
	return false
}

// setHTMLSecurityHeaders — заголовки для HTML-документа SPA.
func setHTMLSecurityHeaders(h http.Header, r *http.Request) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Permissions-Policy", "microphone=(self)")
	if wantHSTS(r) {
		h.Set("Strict-Transport-Security", hstsValue)
	}
}

// wantHSTS — только по TLS и не для loopback: HSTS на localhost «залипает» в браузере
// разработчика и ломает http-dev-сервер фронта на том же хосте.
func wantHSTS(r *http.Request) bool {
	if r.TLS == nil {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return false
	}
	return true
}

// SecurityHeaders — общие заголовки для всех ответов: nosniff; для /api/ — Cache-Control:
// no-store (ставится ДО обработчика, поэтому обработчик, которому кэш нужен — справочники
// с ETag, аудио TTS, — просто переопределяет его своим Set); HSTS по TLS.
// Без обёртки ResponseWriter: ноль накладных расходов, WebSocket/Flush не затронуты.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		if wantHSTS(r) {
			h.Set("Strict-Transport-Security", hstsValue)
		}
		next.ServeHTTP(w, r)
	})
}
