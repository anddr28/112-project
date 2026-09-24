// Package realtime — WebSocket-хаб (core.Publisher): каналы мониторинга занятия
// (/ws/lessons/{id}/monitor) и попытки (/ws/attempts/{id}).
//
// Устройство (DESIGN §5 WebSocket):
//   - сообщение кодируется в JSON ОДИН раз, подписчикам уходят одни и те же байты;
//   - у канала — монотонный seq и кольцевой буфер последних сообщений (5 мин) для ?since=.
//     Буфер двухклассовый: содержательные сообщения (статусы, реплики, оценки, службы…) хранятся
//     все 5 минут; частая телеметрия ввода (attemptEvent field_changed/choose_value — её и так
//     прореживают до 2/с на попытку) — в отдельном небольшом кольце и при нехватке места
//     вытесняется первой, не выдавливая содержательные сообщения;
//   - у подписчика — ограниченная очередь; переполнилась — отключаем («slow consumer»),
//     клиент переподключится с since и доберёт пропущенное. Publish никогда не блокируется;
//   - пустые каналы (нет подписчиков, в буфере только просроченное) убирает уборщик раз в минуту;
//   - права проверяются при upgrade, а с SetAuthenticator — и дальше, раз в 10 с: выход,
//     блокировка, смена пароля или роли закрывают открытое соединение (1008 «session revoked»).
package realtime

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
)

const (
	retention    = 5 * time.Minute // буфер для since-replay (контракт: 5 мин)
	janitorEvery = time.Minute
	sendQueue    = 256 // очередь подписчика; полна — отключаем

	// Содержательные сообщения монитора: 200 студентов в голосовом режиме дают ~100 сообщений/с
	// (реплики + события хода + статусы) — 5 минут это ~30 тыс. сообщений по ~0.4 КБ.
	// Кольцо растёт по мере надобности: у тихого занятия — килобайты, а не потолок.
	monitorRingMax   = 1 << 15
	monitorRingBytes = 16 << 20 // потолок памяти содержательной части буфера одного занятия
	// Телеметрия ввода: до 2/с на попытку (attempts.liveEvery) — 400/с на 200 студентов;
	// хранится, пока помещается (секунды–минуты), и не вытесняет содержательное.
	monitorLiveMax   = 4096
	monitorLiveBytes = 2 << 20
	studentRingMax   = 256
	studentRingBytes = 1 << 20

	maxEncodeBuf = 1 << 20 // гигантские буферы кодировщика не держим в пуле
)

type kind uint8

const (
	kindMonitor kind = iota
	kindStudent
	kindCount
)

type limits struct{ n, bytes int }

// kindLimits — [0] содержательные сообщения, [1] телеметрия ввода (у студента её нет).
var kindLimits = [kindCount][2]limits{
	kindMonitor: {{n: monitorRingMax, bytes: monitorRingBytes}, {n: monitorLiveMax, bytes: monitorLiveBytes}},
	kindStudent: {{n: studentRingMax, bytes: studentRingBytes}, {}},
}

// Hub — реализация core.Publisher и WS-эндпоинтов.
type Hub struct {
	log *slog.Logger

	// auth — повторная проверка сессии открытых соединений (SetAuthenticator); nil — только при
	// подключении. revalidate — период проверки (в тестах короче).
	auth       atomic.Pointer[authHolder]
	revalidate time.Duration

	mu    sync.RWMutex
	chans [kindCount]map[uuid.UUID]*channel

	// Присутствие обучающихся: число открытых WS по участнику занятия.
	presMu   sync.Mutex
	presence map[presenceKey]*presence

	// Жизненный цикл соединений (Close ждёт, пока все закроются).
	connMu  sync.Mutex
	active  int
	closed  bool
	drained chan struct{}
	closing chan struct{} // закрыт — хаб останавливается
}

var _ core.Publisher = (*Hub)(nil)

// NewHub создаёт хаб и запускает уборщик каналов (останавливается в Close).
func NewHub(log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	h := &Hub{
		log:        log,
		revalidate: revalidateEvery,
		presence:   make(map[presenceKey]*presence),
		closing:    make(chan struct{}),
	}
	for k := range h.chans {
		h.chans[k] = make(map[uuid.UUID]*channel)
	}
	go h.janitor()
	return h
}

type authHolder struct{ a core.Authenticator }

// SetAuthenticator включает повторную проверку сессии открытых WS (раз в revalidateEvery):
// выход из системы, блокировка, смена пароля или роли закрывают соединение (1008), а не
// оставляют поток мониторинга/попытки открытым до ухода клиента. Вызывать при сборке
// приложения (тот же аутентификатор, что у httpx.Router); nil выключает проверку.
func (h *Hub) SetAuthenticator(a core.Authenticator) {
	if a == nil {
		h.auth.Store(nil)
		return
	}
	h.auth.Store(&authHolder{a: a})
}

func (h *Hub) authenticator() core.Authenticator {
	if p := h.auth.Load(); p != nil {
		return p.a
	}
	return nil
}

// Monitor — сообщение преподавателю в канал занятия. Seq и At проставляет хаб.
func (h *Hub) Monitor(lessonID uuid.UUID, m public.MonitorMessage) {
	ch := h.acquire(kindMonitor, lessonID)
	defer ch.mu.Unlock()
	seq, at := ch.seq+1, time.Now().UTC()
	m.Seq, m.At = int(seq), at
	data, err := encodeJSON(&m)
	if err != nil {
		h.log.Error("realtime: encode monitor message", "type", string(m.Type), "err", err)
		return
	}
	ch.seq = seq
	ch.deliver(seq, at, data, isTelemetry(&m))
}

// isTelemetry — частое событие ввода в мониторе (прореженное attempts до 2/с на попытку):
// его потеря при reconnect не искажает картину — следующее такое же придёт через секунду.
func isTelemetry(m *public.MonitorMessage) bool {
	if m.Type != public.MonitorMessageTypeAttemptEvent || m.Event == nil {
		return false
	}
	return m.Event.Type == core.EventFieldChanged || m.Event.Type == core.EventChooseValue
}

// Student — сообщение обучающемуся в канал попытки. Seq и At проставляет хаб.
func (h *Hub) Student(attemptID uuid.UUID, m public.StudentMessage) {
	ch := h.acquire(kindStudent, attemptID)
	defer ch.mu.Unlock()
	seq, at := ch.seq+1, time.Now().UTC()
	m.Seq, m.At = int(seq), at
	data, err := encodeJSON(&m)
	if err != nil {
		h.log.Error("realtime: encode student message", "type", string(m.Type), "err", err)
		return
	}
	ch.seq = seq
	ch.deliver(seq, at, data, false)
}

// Connections — открытые WS-соединения (метрики, health).
func (h *Hub) Connections() int {
	h.connMu.Lock()
	n := h.active
	h.connMu.Unlock()
	return n
}

// closeWait — сколько Close ждёт закрытия соединений (close-рукопожатие ≤ 5 с у библиотеки).
const closeWait = 6 * time.Second

// Close закрывает все соединения (StatusGoingAway) и останавливает уборщик. http.Server.Shutdown
// hijacked-соединения не трогает — вызывать отдельно (например, srv.RegisterOnShutdown(hub.Close)).
// Идемпотентен.
func (h *Hub) Close() {
	h.connMu.Lock()
	if h.closed {
		h.connMu.Unlock()
		return
	}
	h.closed = true
	var wait chan struct{}
	if h.active > 0 {
		wait = make(chan struct{})
		h.drained = wait
	}
	h.connMu.Unlock()
	close(h.closing)
	if wait == nil {
		return
	}
	t := time.NewTimer(closeWait)
	defer t.Stop()
	select {
	case <-wait:
	case <-t.C:
		h.log.Warn("realtime: close timeout, connections still open", "conns", h.Connections())
	}
}

// enter регистрирует соединение; false — хаб останавливается.
func (h *Hub) enter() bool {
	h.connMu.Lock()
	defer h.connMu.Unlock()
	if h.closed {
		return false
	}
	h.active++
	return true
}

func (h *Hub) leave() {
	h.connMu.Lock()
	h.active--
	if h.active == 0 && h.drained != nil {
		close(h.drained)
		h.drained = nil
	}
	h.connMu.Unlock()
}

// ---------------------------------------------------------------- каналы

// channel — поток сообщений одного занятия (монитор) или одной попытки (студент).
type channel struct {
	mu  sync.Mutex
	lim [2]limits
	seq int64 // последний выданный seq
	// floor — наибольший seq содержательного сообщения, которого в буфере уже нет (вытеснено
	// или просрочено), либо начало «эпохи» канала. since ≥ floor — всё содержательное после
	// since есть в ring (телеметрия — сколько сохранилось), иначе клиенту нужен snapshot.
	floor int64
	ring  ring // содержательные сообщения
	live  ring // телеметрия ввода (только монитор)
	subs  []*subscriber
	dead  bool // удалён уборщиком: держатель указателя должен взять канал заново
}

// newChannel. Начальный seq — время создания в микросекундах, а не 0: канал может быть
// удалён уборщиком или сервер перезапущен, и новые seq не должны совпасть со старыми —
// иначе клиент со старым since получил бы «replay» чужих сообщений вместо snapshot.
// Значения < 2^53 (безопасно для JS number) до 2255 года.
func newChannel(k kind) *channel {
	seq := time.Now().UnixMicro()
	return &channel{lim: kindLimits[k], seq: seq, floor: seq}
}

// acquire возвращает канал (создаёт при необходимости) с ЗАХВАЧЕННЫМ ch.mu.
func (h *Hub) acquire(k kind, id uuid.UUID) *channel {
	for {
		h.mu.RLock()
		ch := h.chans[k][id]
		h.mu.RUnlock()
		if ch == nil {
			h.mu.Lock()
			if ch = h.chans[k][id]; ch == nil {
				ch = newChannel(k)
				h.chans[k][id] = ch
			}
			h.mu.Unlock()
		}
		ch.mu.Lock()
		if !ch.dead {
			return ch
		}
		ch.mu.Unlock() // гонка с уборщиком: канал только что удалён — берём новый
	}
}

// deliver кладёт сообщение в буфер и раздаёт подписчикам без блокировки (под ch.mu).
func (ch *channel) deliver(seq int64, at time.Time, data []byte, telemetry bool) {
	e := entry{seq: seq, at: at.UnixNano(), data: data}
	if telemetry && ch.lim[1].n > 0 {
		ch.live.push(e, ch.lim[1])
	} else {
		ch.raiseFloor(ch.ring.push(e, ch.lim[0]))
	}
	for i := 0; i < len(ch.subs); {
		s := ch.subs[i]
		select {
		case s.send <- data:
			i++
		default:
			// Очередь полна: клиент не успевает. Отключаем, он вернётся с since.
			close(s.slow)
			ch.removeAt(i)
		}
	}
}

func (ch *channel) raiseFloor(evicted int64) {
	if evicted > ch.floor {
		ch.floor = evicted
	}
}

// expire выбрасывает просроченное из обеих частей буфера.
func (ch *channel) expire(cutoff int64) {
	ch.raiseFloor(ch.ring.expire(cutoff))
	ch.live.expire(cutoff)
}

// since — сообщения с seq > since в порядке seq, если буфер покрывает (since, seq] по
// содержательным сообщениям. since > seq — клиент из другой «эпохи» канала (перезапуск,
// уборка): не покрыто. Телеметрия досылается та, что сохранилась.
func (ch *channel) since(since int64) ([][]byte, bool) {
	switch {
	case since > ch.seq || since < ch.floor:
		return nil, false
	case since == ch.seq:
		return nil, true
	}
	a, b := ch.ring.after(since), ch.live.after(since)
	out := make([][]byte, 0, (ch.ring.n-a)+(ch.live.n-b))
	for a < ch.ring.n || b < ch.live.n { // слияние двух упорядоченных по seq колец
		if b >= ch.live.n || (a < ch.ring.n && ch.ring.at(a).seq < ch.live.at(b).seq) {
			out = append(out, ch.ring.at(a).data)
			a++
		} else {
			out = append(out, ch.live.at(b).data)
			b++
		}
	}
	return out, true
}

func (ch *channel) removeAt(i int) {
	last := len(ch.subs) - 1
	ch.subs[i] = ch.subs[last]
	ch.subs[last] = nil
	ch.subs = ch.subs[:last]
}

// subscriber — одно WS-соединение канала.
type subscriber struct {
	send chan []byte
	slow chan struct{} // закрыт — подписчик отключён за переполнение (под ch.mu, один раз)
}

// subscription — результат подписки: граница и то, что нужно отправить до живого потока.
type subscription struct {
	ch  *channel
	sub *subscriber
	// seq — последний seq канала на момент подписки: всё с большим seq придёт в sub.send.
	seq int64
	// replay — сообщения с seq > since (если буфер их покрывает), в порядке seq.
	replay [][]byte
	// covered — since задан и буфер содержит всё после него (replay может быть пуст).
	covered bool
}

// subscribe атомарно (под ch.mu) подключает подписчика и фиксирует границу: сообщения
// с seq ≤ границы — в replay/snapshot, с большим — в очереди. Ни дыр, ни дублей.
func (h *Hub) subscribe(k kind, id uuid.UUID, since int64, hasSince bool) subscription {
	sub := &subscriber{send: make(chan []byte, sendQueue), slow: make(chan struct{})}
	ch := h.acquire(k, id)
	defer ch.mu.Unlock()
	ch.subs = append(ch.subs, sub)
	ch.expire(time.Now().Add(-retention).UnixNano())
	s := subscription{ch: ch, sub: sub, seq: ch.seq}
	if hasSince {
		s.replay, s.covered = ch.since(since)
	}
	return s
}

// unsubscribe — отписка (идемпотентно: подписчик мог быть уже снят за переполнение).
func (h *Hub) unsubscribe(s subscription) {
	ch := s.ch
	ch.mu.Lock()
	for i, x := range ch.subs {
		if x == s.sub {
			ch.removeAt(i)
			break
		}
	}
	ch.mu.Unlock()
}

// janitor раз в минуту выбрасывает просроченные сообщения и пустые каналы.
func (h *Hub) janitor() {
	t := time.NewTicker(janitorEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			h.sweep(time.Now())
		case <-h.closing:
			return
		}
	}
}

func (h *Hub) sweep(now time.Time) {
	cutoff := now.Add(-retention).UnixNano()
	h.mu.Lock()
	defer h.mu.Unlock()
	for k := range h.chans {
		for id, ch := range h.chans[k] {
			ch.mu.Lock()
			ch.expire(cutoff)
			if len(ch.subs) == 0 && ch.ring.n == 0 && ch.live.n == 0 {
				ch.dead = true
				ch.ring, ch.live = ring{}, ring{}
				delete(h.chans[k], id)
			}
			ch.mu.Unlock()
		}
	}
}

// ---------------------------------------------------------------- кольцевой буфер

type entry struct {
	seq  int64
	at   int64 // UnixNano
	data []byte
}

// ring — последние сообщения одного класса, по возрастанию seq (удаляем только старейшие;
// seq внутри кольца не обязательно подряд — классы чередуются).
// Растёт по мере надобности до лимита: у тихого канала попытки — десятки байт, не 10 КБ.
type ring struct {
	buf   []entry
	head  int // индекс старейшего
	n     int
	bytes int
}

func (r *ring) at(i int) *entry { return &r.buf[(r.head+i)%len(r.buf)] }

// push добавляет сообщение, вытесняя старейшие сверх лимита; возвращает наибольший
// вытесненный seq (0 — ничего не вытеснено).
func (r *ring) push(e entry, lim limits) (evicted int64) {
	for r.n > 0 && (r.n >= lim.n || r.bytes+len(e.data) > lim.bytes) {
		evicted = r.pop()
	}
	if r.n == len(r.buf) {
		r.grow(lim.n)
	}
	*r.at(r.n) = e
	r.n++
	r.bytes += len(e.data)
	return evicted
}

func (r *ring) grow(max int) {
	size := len(r.buf) * 2
	if size < 8 {
		size = 8
	}
	if size > max {
		size = max
	}
	nb := make([]entry, size)
	for i := 0; i < r.n; i++ {
		nb[i] = *r.at(i)
	}
	r.buf, r.head = nb, 0
}

// pop удаляет старейшее сообщение и возвращает его seq.
func (r *ring) pop() int64 {
	e := r.at(0)
	seq := e.seq
	r.bytes -= len(e.data)
	*e = entry{}
	r.head = (r.head + 1) % len(r.buf)
	r.n--
	return seq
}

// expire выбрасывает сообщения старше cutoff; возвращает наибольший выброшенный seq (0 — ничего).
func (r *ring) expire(cutoff int64) (evicted int64) {
	for r.n > 0 && r.at(0).at < cutoff {
		evicted = r.pop()
	}
	return evicted
}

// after — индекс первого сообщения с seq > since (r.n — таких нет). Двоичный поиск.
func (r *ring) after(since int64) int {
	lo, hi := 0, r.n
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if r.at(mid).seq <= since {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// ---------------------------------------------------------------- JSON

type encoder struct {
	buf bytes.Buffer
	enc *json.Encoder
}

var encPool = sync.Pool{New: func() any {
	e := &encoder{}
	e.enc = json.NewEncoder(&e.buf)
	e.enc.SetEscapeHTML(false) // русский текст и "<>" из карточек — как есть, как в httpx
	return e
}}

// encodeJSON — точная по размеру копия JSON без завершающего '\n' (живёт в буфере канала).
func encodeJSON(v any) ([]byte, error) {
	e := encPool.Get().(*encoder)
	e.buf.Reset()
	err := e.enc.Encode(v)
	var out []byte
	if err == nil {
		b := bytes.TrimSuffix(e.buf.Bytes(), []byte{'\n'})
		out = make([]byte, len(b))
		copy(out, b)
	}
	if e.buf.Cap() <= maxEncodeBuf {
		encPool.Put(e)
	}
	return out, err
}

// WatchedLessons — занятия, у которых сейчас есть хотя бы один подключённый монитор
// (кому слать периодический aiHealth, не создавая каналы впустую).
func (h *Hub) WatchedLessons() []uuid.UUID {
	h.mu.RLock()
	chans := make(map[uuid.UUID]*channel, len(h.chans[kindMonitor]))
	for id, ch := range h.chans[kindMonitor] {
		chans[id] = ch
	}
	h.mu.RUnlock()
	out := make([]uuid.UUID, 0, len(chans))
	for id, ch := range chans {
		ch.mu.Lock()
		if !ch.dead && len(ch.subs) > 0 {
			out = append(out, id)
		}
		ch.mu.Unlock()
	}
	return out
}
