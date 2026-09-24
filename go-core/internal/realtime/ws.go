package realtime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
)

const (
	pingInterval    = 20 * time.Second // контракт: пинг каждые 20 с
	revalidateEvery = 10 * time.Second // повторная проверка сессии открытого соединения
	revalidateTime  = 5 * time.Second  // таймаут одной проверки
	pingTimeout     = 10 * time.Second
	writeTimeout    = 10 * time.Second
	snapshotTimeout = 10 * time.Second
	presenceTimeout = 5 * time.Second
	readLimit       = 4096 // клиенты ничего содержательного не шлют
	maxBurst        = 64   // сообщений под одним дедлайном записи
)

// MonitorHandler — GET /ws/lessons/{lessonId}/monitor?since=seq (регистрируется с
// httpx.Roles(teacher, admin); Principal уже в контексте). Права проверяются ДО upgrade
// (403/404 — обычным ApiError). Первое сообщение — snapshot (seq = текущий seq канала),
// либо, если since покрыт буфером, — пропущенные сообщения; далее живой поток.
func (h *Hub) MonitorHandler(lm core.LessonMonitor, origins []string) httpx.HandlerFunc {
	opts := acceptOptions(origins)
	return func(w http.ResponseWriter, r *http.Request) error {
		p := core.PrincipalFrom(r.Context())
		if p == nil {
			return httpx.Unauthorized()
		}
		lessonID, err := httpx.PathUUID(r, "lessonId")
		if err != nil {
			return err
		}
		if err := lm.CanMonitor(r.Context(), p, lessonID); err != nil {
			return err
		}
		if err := requireUpgrade(r); err != nil {
			return err
		}
		since, hasSince := parseSince(r)
		if !h.enter() {
			return errShuttingDown()
		}
		defer h.leave()

		conn, err := websocket.Accept(w, r, opts)
		if err != nil {
			// Accept уже ответил клиенту (403 origin / 426 и т.п.).
			h.log.Debug("realtime: monitor accept", "lesson", lessonID, "err", err)
			return nil
		}
		// После hijack контекст запроса ненадёжен: значения (Principal, RequestMeta) берём,
		// отмену — нет; все операции ниже — со своими таймаутами.
		base := context.WithoutCancel(r.Context())

		s := h.subscribe(kindMonitor, lessonID, since, hasSince)
		defer h.unsubscribe(s)

		first := s.replay
		if !s.covered {
			data, err := h.snapshot(base, lm, lessonID, s.seq)
			if err != nil {
				h.log.Error("realtime: monitor snapshot", "lesson", lessonID, "err", err)
				_ = conn.Close(websocket.StatusInternalError, "snapshot unavailable")
				return nil
			}
			first = [][]byte{data}
		}
		g := h.newGuard(r, base, p, []core.Role{core.RoleTeacher, core.RoleAdmin},
			func(ctx context.Context, np *core.Principal) error { return lm.CanMonitor(ctx, np, lessonID) })
		h.serve(conn, s.sub, first, g, "lesson", lessonID)
		return nil
	}
}

// snapshotBody — тип поля MonitorMessage.Snapshot (анонимная структура в сгенерированном коде;
// алиас с теми же полями и тегами идентичен ей).
type snapshotBody = struct {
	Attempts *[]public.Attempt `json:"attempts,omitempty"`
	Lesson   *public.Lesson    `json:"lesson,omitempty"`
}

// snapshot — первое сообщение монитора. seq = граница подписки (не инкремент): следующее
// живое сообщение придёт с seq+1, клиент сможет переподключиться с since = этот seq.
func (h *Hub) snapshot(ctx context.Context, lm core.LessonMonitor, lessonID uuid.UUID, seq int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()
	lesson, attempts, err := lm.Snapshot(ctx, lessonID)
	if err != nil {
		return nil, err
	}
	if attempts == nil {
		attempts = []public.Attempt{}
	}
	m := public.MonitorMessage{
		Seq:      int(seq),
		Type:     public.MonitorMessageTypeSnapshot,
		At:       time.Now().UTC(),
		Snapshot: &snapshotBody{Lesson: lesson, Attempts: &attempts},
	}
	return encodeJSON(&m)
}

// StudentHandler — GET /ws/attempts/{attemptId}?since=seq (регистрируется с
// httpx.Roles(student)). Доступ — только владелец попытки (403/404 до upgrade).
// Snapshot студенту не нужен: since покрыт буфером — досылаем пропущенное, иначе только
// живой поток (состояние фронт перечитывает REST'ом). Открытое соединение = участник онлайн.
func (h *Hub) StudentHandler(ap core.AttemptPresence, origins []string) httpx.HandlerFunc {
	opts := acceptOptions(origins)
	return func(w http.ResponseWriter, r *http.Request) error {
		p := core.PrincipalFrom(r.Context())
		if p == nil {
			return httpx.Unauthorized()
		}
		attemptID, err := httpx.PathUUID(r, "attemptId")
		if err != nil {
			return err
		}
		lessonID, err := ap.AttemptAccess(r.Context(), p, attemptID)
		if err != nil {
			return err
		}
		if err := requireUpgrade(r); err != nil {
			return err
		}
		since, hasSince := parseSince(r)
		if !h.enter() {
			return errShuttingDown()
		}
		defer h.leave()

		base := context.WithoutCancel(r.Context())

		// Подписка и присутствие — ДО ответа 101: когда клиент считает сокет открытым, он уже
		// подписан (сообщение, опубликованное сразу после рукопожатия, копится в очереди
		// подписчика, а не теряется) и уже учтён в присутствии (закрытие старой вкладки после
		// открытия новой не даёт ложного «disconnected»). Accept не прошёл — defer всё вернёт.
		s := h.subscribe(kindStudent, attemptID, since, hasSince)
		defer h.unsubscribe(s)

		// Присутствие — по участнику занятия, а не по попытке: после сдачи фронт открывает WS
		// следующей карточки, и порядок закрытия старого/открытия нового не важен.
		key := presenceKey{lesson: lessonID, user: p.UserID}
		h.presenceChange(base, ap, key, attemptID, +1)
		defer h.presenceChange(base, ap, key, attemptID, -1)

		conn, err := websocket.Accept(w, r, opts)
		if err != nil {
			h.log.Debug("realtime: student accept", "attempt", attemptID, "err", err)
			return nil
		}

		g := h.newGuard(r, base, p, []core.Role{core.RoleStudent},
			func(ctx context.Context, np *core.Principal) error {
				_, err := ap.AttemptAccess(ctx, np, attemptID)
				return err
			})
		h.serve(conn, s.sub, s.replay, g, "attempt", attemptID)
		return nil
	}
}

func acceptOptions(origins []string) *websocket.AcceptOptions {
	return &websocket.AcceptOptions{
		OriginPatterns:  append([]string(nil), origins...), // свой хост библиотека пускает всегда
		CompressionMode: websocket.CompressionDisabled,     // сообщения мелкие, CPU дороже трафика
	}
}

// requireUpgrade — не-WebSocket запрос получает контрактный ApiError, а не text/plain библиотеки.
func requireUpgrade(r *http.Request) error {
	if !headerHasToken(r.Header.Get("Upgrade"), "websocket") {
		return &httpx.Error{Status: http.StatusUpgradeRequired, Code: httpx.CodeValidation,
			Message: "Ожидается подключение по WebSocket"}
	}
	return nil
}

func headerHasToken(v, token string) bool {
	for part := range strings.SplitSeq(v, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

func errShuttingDown() error {
	return &httpx.Error{Status: http.StatusServiceUnavailable, Code: httpx.CodeInternal,
		Message: "Сервер перезапускается, повторите подключение", RetryAfter: 3}
}

// parseSince — ?since=N (N ≥ 0); мусор — как отсутствие (клиент получит snapshot).
func parseSince(r *http.Request) (int64, bool) {
	v := r.URL.Query().Get("since")
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// ---------------------------------------------------------------- цикл соединения

var (
	errSlowConsumer = errors.New("slow consumer")
	errRevoked      = errors.New("session revoked")
)

// ---------------------------------------------------------------- повторная проверка сессии

// guard — повторная проверка права на открытое соединение: права проверяются при upgrade,
// а соединение живёт часами. Сессия отозвана (выход, блокировка, смена пароля), роль больше
// не допускает канал или доступ к занятию/попытке пропал — соединение закрывается.
type guard struct {
	auth    core.Authenticator
	req     *http.Request   // исходный запрос upgrade: cookie сессии
	base    context.Context // значения запроса без его отмены
	userID  uuid.UUID
	session uuid.UUID
	role    core.Role // роль, для которой доступ проверен последним (меняется только в check)
	roles   []core.Role
	access  func(ctx context.Context, p *core.Principal) error // проверка доступа к каналу
}

// newGuard — nil, если аутентификатор не задан (SetAuthenticator): проверка только при подключении.
func (h *Hub) newGuard(r *http.Request, base context.Context, p *core.Principal, roles []core.Role,
	access func(ctx context.Context, p *core.Principal) error) *guard {
	a := h.authenticator()
	if a == nil {
		return nil
	}
	return &guard{auth: a, req: r, base: base, userID: p.UserID, session: p.SessionID, role: p.Role,
		roles: roles, access: access}
}

// check — nil: доступ сохраняется; errRevoked: закрыть соединение; иное — временный сбой
// (БД недоступна и т.п.): соединение не рвём, проверим на следующем круге.
// Одновременно выполняется не больше одной проверки соединения (см. loop).
func (g *guard) check() error {
	ctx, cancel := context.WithTimeout(g.base, revalidateTime)
	defer cancel()
	p, err := g.auth.Authenticate(g.req.WithContext(ctx))
	switch {
	case errors.Is(err, core.ErrUnauthenticated), errors.Is(err, core.ErrUserBlocked):
		return errRevoked
	case err != nil:
		return err
	case p == nil, p.UserID != g.userID, p.SessionID != g.session, !p.Is(g.roles...):
		return errRevoked
	}
	if p.Role == g.role || g.access == nil {
		return nil
	}
	// Роль сменилась, но канал ей допустим (admin -> teacher): доступ к занятию — заново.
	if err := g.access(ctx, p); err != nil {
		var he *httpx.Error
		if errors.As(err, &he) && he.Status < http.StatusInternalServerError {
			return errRevoked
		}
		return err
	}
	g.role = p.Role
	return nil
}

// serve — жизнь соединения: сначала first (replay/snapshot), затем очередь подписчика,
// пинг каждые 20 с. Одна горутина пишет (эта), одна читает (обязательна: pong/close-кадры).
// g != nil — сессия перепроверяется каждые h.revalidate.
func (h *Hub) serve(conn *websocket.Conn, sub *subscriber, first [][]byte, g *guard, idKey string, id uuid.UUID) {
	conn.SetReadLimit(readLimit)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		// Входящие сообщения клиента не нужны: вычитываем и выбрасываем (терпимо к
		// прикладным heartbeat'ам). context.Background — без таймера на каждое чтение;
		// горутина завершается при закрытии соединения.
		for {
			_, rd, err := conn.Reader(context.Background())
			if err != nil {
				return
			}
			if _, err := io.Copy(io.Discard, rd); err != nil {
				return
			}
		}
	}()

	err := h.loop(conn, sub, first, g, readDone)
	switch {
	case err == nil: // остановка хаба
		_ = conn.Close(websocket.StatusGoingAway, "server shutdown")
	case errors.Is(err, errSlowConsumer):
		h.log.Info("realtime: slow consumer disconnected", idKey, id)
		_ = conn.Close(websocket.StatusPolicyViolation, "slow consumer")
	case errors.Is(err, errRevoked):
		h.log.Info("realtime: session revoked, disconnected", idKey, id)
		_ = conn.Close(websocket.StatusPolicyViolation, "session revoked")
	default:
		// Клиент ушёл, не ответил на пинг или не принимает данные — рукопожатие бессмысленно.
		_ = conn.CloseNow()
	}
	<-readDone
}

func (h *Hub) loop(conn *websocket.Conn, sub *subscriber, first [][]byte, g *guard, readDone <-chan struct{}) error {
	if err := writeAll(conn, first); err != nil {
		return err
	}
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()

	// Проверка сессии — в отдельной короткой горутине: промах кэша сессий идёт в БД, и писатель
	// не должен стоять (очередь подписчика переполнилась бы). Не больше одной проверки за раз;
	// verdict с буфером 1 — горутина не зависнет, если соединение уже закрыто.
	var recheck <-chan time.Time
	verdict := make(chan error, 1)
	checking := false
	if g != nil {
		t := time.NewTicker(h.revalidate)
		defer t.Stop()
		recheck = t.C
	}

	for {
		// Отключённый за переполнение подписчик не должен писать дальше: после снятия с канала
		// в его очереди «дыра» — клиент обязан переподключиться с since.
		select {
		case <-sub.slow:
			return errSlowConsumer
		default:
		}
		select {
		case data := <-sub.send:
			if err := writeBurst(conn, sub, data); err != nil {
				return err
			}
		case <-ping.C:
			ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
			err := conn.Ping(ctx)
			cancel()
			if err != nil {
				return err
			}
		case <-recheck:
			if !checking {
				checking = true
				go func() { verdict <- g.check() }()
			}
		case err := <-verdict:
			checking = false
			if errors.Is(err, errRevoked) {
				return errRevoked
			}
			if err != nil {
				h.log.Warn("realtime: session recheck failed, keeping connection", "err", err)
			}
		case <-sub.slow:
			return errSlowConsumer
		case <-readDone:
			return io.EOF
		case <-h.closing:
			return nil
		}
	}
}

// writeBurst пишет сообщение и всё, что уже скопилось в очереди (до maxBurst), под одним
// дедлайном: меньше таймеров и контекстов при всплесках.
func writeBurst(conn *websocket.Conn, sub *subscriber, data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		return err
	}
	for range maxBurst - 1 {
		select {
		case data = <-sub.send:
			if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
				return err
			}
		default:
			return nil
		}
	}
	return nil
}

// writeAll — начальные сообщения (replay/snapshot) пачками по maxBurst под общим дедлайном.
func writeAll(conn *websocket.Conn, msgs [][]byte) error {
	for len(msgs) > 0 {
		n := min(len(msgs), maxBurst)
		ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
		for _, data := range msgs[:n] {
			if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
				cancel()
				return err
			}
		}
		cancel()
		msgs = msgs[n:]
	}
	return nil
}

// ---------------------------------------------------------------- присутствие

type presenceKey struct {
	lesson, user uuid.UUID
}

// presence — счётчик открытых WS участника. mu сериализует переходы 0↔1 вместе с вызовом
// SetOnline: иначе «офлайн» от закрывшейся вкладки мог бы записаться позже «онлайн» новой.
type presence struct {
	mu   sync.Mutex
	n    int // открытых соединений (под mu)
	refs int // горутин, держащих указатель (под Hub.presMu) — для удаления из карты
}

func (h *Hub) presenceChange(base context.Context, ap core.AttemptPresence, key presenceKey, attemptID uuid.UUID, delta int) {
	h.presMu.Lock()
	p := h.presence[key]
	if p == nil {
		p = &presence{}
		h.presence[key] = p
	}
	p.refs++
	h.presMu.Unlock()

	p.mu.Lock()
	before := p.n
	p.n += delta
	after := p.n
	if (before == 0) != (after == 0) {
		// SetOnline публикует participantStatus в монитор (через этот же хаб) — под p.mu это
		// безопасно: хаб не берёт блокировок присутствия при публикации.
		ctx, cancel := context.WithTimeout(base, presenceTimeout)
		ap.SetOnline(ctx, attemptID, after > 0)
		cancel()
	}
	p.mu.Unlock()

	h.presMu.Lock()
	p.refs--
	if p.refs == 0 && p.n == 0 {
		delete(h.presence, key)
	}
	h.presMu.Unlock()
}
