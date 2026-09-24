package httpx

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"lct/gocore/internal/core"
	"lct/gocore/internal/platform/ids"
)

// HandlerFunc — хендлер публичного API: возвращает ошибку, роутер пишет ApiError.
// Успешный ответ хендлер пишет сам (WriteJSON / WriteRawJSON / NoContent).
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

// Access — кто может вызывать маршрут.
type Access struct {
	public bool
	roles  []core.Role // пусто и !public — любой аутентифицированный
}

// Public — без сессии (login, demo-accounts).
var Public = Access{public: true}

// Authenticated — любой вошедший пользователь.
var Authenticated = Access{}

// Roles — только перечисленные роли (403 остальным).
func Roles(roles ...core.Role) Access { return Access{roles: roles} }

// Router — регистрация маршрутов /api/v1 поверх http.ServeMux (паттерны Go 1.22:
// "POST /attempts/{attemptId}/submit"). Для каждого маршрута: recover, request id,
// CSRF-замок (мутирующие запросы — X-Requested-With: fetch), аутентификация, RBAC,
// лог и метрики.
type Router struct {
	mux     *http.ServeMux
	prefix  string
	auth    core.Authenticator
	log     *slog.Logger
	observe func(route string, status int, dur time.Duration)
	routes  []string
	timeout time.Duration
}

// DefaultRequestTimeout — серверный потолок обработки обычного (не WebSocket) запроса API:
// контекст запроса истекает (запросы к БД и ожидание соединения пула отменяются). С запасом над самыми длинными штатными запросами (реплика
// разговора ≤ 20 с живёт в своём контексте, отчёт, озвучка) — это страховка от зависаний,
// а не SLA: клиент без ответа всё равно сдаётся раньше.
const DefaultRequestTimeout = 60 * time.Second

// NewRouter. prefix — "/api/v1". observe — хук метрик (может быть nil).
func NewRouter(mux *http.ServeMux, prefix string, auth core.Authenticator, log *slog.Logger,
	observe func(route string, status int, dur time.Duration)) *Router {
	if log == nil {
		log = slog.Default()
	}
	return &Router{mux: mux, prefix: strings.TrimRight(prefix, "/"), auth: auth, log: log, observe: observe,
		timeout: DefaultRequestTimeout}
}

// SetRequestTimeout — потолок обработки запроса для маршрутов, зарегистрированных ПОСЛЕ
// вызова (0 — без потолка). WebSocket-апгрейды потолка не имеют никогда.
func (rt *Router) SetRequestTimeout(d time.Duration) { rt.timeout = d }

// Routes — зарегистрированные паттерны (для теста покрытия контракта).
func (rt *Router) Routes() []string { return append([]string(nil), rt.routes...) }

// Handle регистрирует маршрут. pattern — "METHOD /path/{param}" относительно prefix.
func (rt *Router) Handle(pattern string, access Access, h HandlerFunc) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok || !strings.HasPrefix(path, "/") {
		panic("httpx: bad pattern " + pattern)
	}
	full := method + " " + rt.prefix + path
	rt.routes = append(rt.routes, pattern)
	rt.mux.Handle(full, rt.wrap(pattern, method, access, h))
}

// HandleRaw — маршрут с обычным http.Handler (WebSocket, отдача файлов), но с теми же
// аутентификацией и RBAC. Ошибки доступа пишутся как ApiError.
func (rt *Router) HandleRaw(pattern string, access Access, h http.Handler) {
	rt.Handle(pattern, access, func(w http.ResponseWriter, r *http.Request) error {
		h.ServeHTTP(w, r)
		return nil
	})
}

var mutating = map[string]bool{http.MethodPost: true, http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true}

func (rt *Router) wrap(route, method string, access Access, h HandlerFunc) http.Handler {
	csrf := mutating[method]
	timeout := rt.timeout
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		meta := core.RequestMeta{RequestID: ids.New(), IP: ClientIP(r), UserAgent: r.UserAgent()}
		client := r.Context() // отменяется, когда клиент ушёл (без серверного потолка)
		ctx := core.WithRequestMeta(client, meta)
		sw.Header().Set("X-Request-Id", meta.RequestID.String())
		if timeout > 0 && !IsWebSocketUpgrade(r) {
			// Без потолка зависший запрос (клиент ушёл, а тело не прочитано — net/http тогда
			// не замечает разрыв) держал бы соединение пула и блокировки строк бесконечно.
			// Дедлайн чтения соединения здесь НЕ ставим: его срабатывание в фоновом чтении
			// net/http отменяет r.Context(), и ответ 503 уже не был бы записан.
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}

		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				rt.log.Error("panic in handler", "route", route, "panic", p, "stack", string(debug.Stack()))
				if !sw.wrote {
					WriteError(sw, Internal(nil))
				}
			}
			dur := time.Since(start)
			if sw.status == 0 {
				sw.status = http.StatusOK
			}
			// WebSocket (101) живёт минутами: его длительность — не латентность запроса,
			// в метрики и «медленные запросы» не попадает.
			if sw.status == http.StatusSwitchingProtocols {
				return
			}
			if rt.observe != nil {
				rt.observe(route, sw.status, dur)
			}
			if sw.status >= 500 || dur > 2*time.Second {
				rt.log.Warn("http", "route", route, "status", sw.status, "dur_ms", dur.Milliseconds(), "req", meta.RequestID)
			} else if rt.log.Enabled(ctx, slog.LevelDebug) {
				rt.log.Debug("http", "route", route, "status", sw.status, "dur_ms", dur.Milliseconds())
			}
		}()

		if csrf && r.Header.Get("X-Requested-With") != "fetch" {
			WriteError(sw, Forbidden("Запрос отклонён: нет заголовка X-Requested-With (защита от CSRF)"))
			return
		}

		r = r.WithContext(ctx) // RequestMeta и потолок времени — и аутентификации, и хендлеру
		if !access.public {
			p, err := rt.auth.Authenticate(r)
			if err != nil {
				switch {
				case errors.Is(err, core.ErrUserBlocked):
					WriteError(sw, UserBlocked())
				case errors.Is(err, core.ErrUnauthenticated):
					WriteError(sw, Unauthorized())
				default:
					rt.log.Error("authenticate", "err", err, "req", meta.RequestID)
					WriteError(sw, Internal(err))
				}
				return
			}
			if len(access.roles) > 0 && !p.Is(access.roles...) {
				WriteError(sw, Forbidden(""))
				return
			}
			ctx = core.WithPrincipal(ctx, p)
		}

		if err := h(sw, r.WithContext(ctx)); err != nil {
			he := AsError(err)
			if he.Code == CodeInternal && errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil &&
				client.Err() == nil {
				he = RequestTimeout(err) // истёк серверный потолок запроса, а не сломалось ядро
			}
			switch {
			case he.Code == CodeCallerBusy || he.Code == CodeAIUnavailable:
				// штатный backpressure AI-полосы — не авария ядра
				rt.log.Warn("ai unavailable", "route", route, "code", he.Code, "req", meta.RequestID)
			case he.Status >= 500:
				rt.log.Error("handler error", "route", route, "code", he.Code, "err", he.Error(), "req", meta.RequestID)
			}
			if errors.Is(err, context.Canceled) && client.Err() != nil {
				return // клиент ушёл — отвечать некому
			}
			if !sw.wrote {
				WriteError(sw, he)
			}
		}
	})
}

// IsWebSocketUpgrade — запрос на апгрейд до WebSocket (живёт минутами: без потолка времени).
func IsWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// statusWriter запоминает статус для лога/метрик. Поддерживает http.Flusher/Hijacker
// через Unwrap (http.ResponseController) — нужно WebSocket'у и ServeContent.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status = http.StatusOK
		w.wrote = true
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack — WebSocket (coder/websocket проверяет http.Hijacker). После hijack статус — 101.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if !w.wrote {
		w.status = http.StatusSwitchingProtocols
		w.wrote = true
	}
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

// Flush — потоковые ответы.
func (w *statusWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }
