package realtime

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestHub(t *testing.T) *Hub {
	t.Helper()
	h := NewHub(quietLog())
	t.Cleanup(h.Close)
	return h
}

// ---------------------------------------------------------------- ring

func ent(seq int64, size int) entry {
	return entry{seq: seq, at: seq, data: make([]byte, size)}
}

func ringSeqs(r *ring) []int64 {
	out := make([]int64, r.n)
	for i := range out {
		out[i] = r.at(i).seq
	}
	return out
}

func eqSeqs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRing_PushLimitsAndEviction(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		lim         limits
		sizes       []int
		wantSeqs    []int64
		wantEvicted []int64 // вытесненный seq на каждом push
	}{
		{"under limits", limits{n: 4, bytes: 100}, []int{1, 1, 1}, []int64{1, 2, 3}, []int64{0, 0, 0}},
		{"count limit", limits{n: 3, bytes: 100}, []int{1, 1, 1, 1, 1}, []int64{3, 4, 5}, []int64{0, 0, 0, 1, 2}},
		{"byte limit", limits{n: 100, bytes: 10}, []int{4, 4, 4, 1, 6}, []int64{4, 5}, []int64{0, 0, 1, 0, 3}},
		{"oversized message evicts all but stays", limits{n: 10, bytes: 10}, []int{3, 3, 50}, []int64{3}, []int64{0, 0, 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var r ring
			for i, sz := range tc.sizes {
				if ev := r.push(ent(int64(i+1), sz), tc.lim); ev != tc.wantEvicted[i] {
					t.Fatalf("push %d evicted %d, want %d", i+1, ev, tc.wantEvicted[i])
				}
			}
			if got := ringSeqs(&r); !eqSeqs(got, tc.wantSeqs) {
				t.Fatalf("seqs = %v, want %v", got, tc.wantSeqs)
			}
			total := 0
			for i := 0; i < r.n; i++ {
				total += len(r.at(i).data)
			}
			if r.bytes != total {
				t.Fatalf("bytes = %d, want %d", r.bytes, total)
			}
		})
	}
}

func TestRing_GrowsLazilyAndWraps(t *testing.T) {
	t.Parallel()
	var r ring
	lim := limits{n: 20, bytes: 1 << 20}
	r.push(ent(1, 1), lim)
	if len(r.buf) != 8 {
		t.Fatalf("initial buf = %d, want 8 (lazy)", len(r.buf))
	}
	for i := int64(2); i <= 100; i++ {
		r.push(ent(i, 1), lim)
		if len(r.buf) > lim.n {
			t.Fatalf("buf %d exceeds limit %d", len(r.buf), lim.n)
		}
	}
	want := make([]int64, 0, 20)
	for i := int64(81); i <= 100; i++ {
		want = append(want, i)
	}
	if got := ringSeqs(&r); !eqSeqs(got, want) {
		t.Fatalf("after wrap = %v", got)
	}
	// pop очищает слот (не держит байты).
	head := r.head
	r.pop()
	if r.buf[head].data != nil {
		t.Fatal("popped slot keeps data")
	}
}

func TestRing_ExpireAndAfter(t *testing.T) {
	t.Parallel()
	var r ring
	lim := limits{n: 100, bytes: 1 << 20}
	for _, s := range []int64{3, 5, 6, 10, 11} { // не подряд: классы чередуются
		r.push(entry{seq: s, at: s * 100, data: []byte("x")}, lim)
	}
	afterCases := map[int64]int{0: 0, 2: 0, 3: 1, 4: 1, 5: 2, 6: 3, 9: 3, 10: 4, 11: 5, 99: 5}
	for since, want := range afterCases {
		if got := r.after(since); got != want {
			t.Errorf("after(%d) = %d, want %d", since, got, want)
		}
	}
	if ev := r.expire(550); ev != 5 {
		t.Fatalf("expire evicted %d, want 5", ev)
	}
	if got := ringSeqs(&r); !eqSeqs(got, []int64{6, 10, 11}) {
		t.Fatalf("after expire = %v", got)
	}
	if ev := r.expire(0); ev != 0 {
		t.Fatalf("nothing to expire, got %d", ev)
	}
	var empty ring
	if empty.after(5) != 0 || empty.expire(1<<62) != 0 {
		t.Fatal("empty ring")
	}
}

// ---------------------------------------------------------------- channel.since

func testChannel(start int64, lim [2]limits) *channel {
	return &channel{lim: lim, seq: start, floor: start}
}

func (ch *channel) pub(telemetry bool, at time.Time) int64 {
	ch.seq++
	ch.deliver(ch.seq, at, []byte{byte(ch.seq)}, telemetry)
	return ch.seq
}

func replaySeqs(t *testing.T, msgs [][]byte) []int64 {
	t.Helper()
	out := make([]int64, len(msgs))
	for i, m := range msgs {
		out[i] = int64(m[0])
	}
	return out
}

func TestChannelSince_Coverage(t *testing.T) {
	t.Parallel()
	now := time.Now()
	lim := [2]limits{{n: 4, bytes: 1 << 20}, {n: 2, bytes: 1 << 20}}
	ch := testChannel(0, lim)

	// Свежий канал: since = seq — покрыт, пусто; since из «другой эпохи» — нет.
	if msgs, ok := ch.since(0); !ok || len(msgs) != 0 {
		t.Fatalf("fresh since=seq: %v %v", msgs, ok)
	}
	if _, ok := ch.since(5); ok {
		t.Fatal("since > seq must not be covered")
	}

	// 1 E, 2 T, 3 T, 4 E, 5 T, 6 E  (E — содержательное, T — телеметрия; телеметрии хранится 2)
	for _, tel := range []bool{false, true, true, false, true, false} {
		ch.pub(tel, now)
	}
	cases := []struct {
		since int64
		ok    bool
		want  []int64
	}{
		{0, true, []int64{1, 3, 4, 5, 6}}, // телеметрия 2 вытеснена (кольцо на 2) — это допустимо
		{1, true, []int64{3, 4, 5, 6}},
		{3, true, []int64{4, 5, 6}},
		{5, true, []int64{6}},
		{6, true, nil},
		{7, false, nil},
	}
	for _, tc := range cases {
		msgs, ok := ch.since(tc.since)
		if ok != tc.ok || !eqSeqs(replaySeqs(t, msgs), tc.want) {
			t.Errorf("since(%d) = %v %v, want %v %v", tc.since, replaySeqs(t, msgs), ok, tc.want, tc.ok)
		}
	}

	// Вытеснение содержательного поднимает floor: since ниже — только snapshot.
	ch.pub(false, now) // 7
	ch.pub(false, now) // 8 — в кольце E: 4,6,7,8 (1 вытеснено)
	if ch.floor != 1 {
		t.Fatalf("floor = %d, want 1", ch.floor)
	}
	if _, ok := ch.since(0); ok {
		t.Fatal("since below floor must not be covered")
	}
	if msgs, ok := ch.since(1); !ok || !eqSeqs(replaySeqs(t, msgs), []int64{3, 4, 5, 6, 7, 8}) {
		t.Fatalf("since(1) = %v %v", replaySeqs(t, msgs), ok)
	}
	// Вытеснение телеметрии floor не трогает.
	for range 5 {
		ch.pub(true, now)
	}
	if ch.floor != 1 {
		t.Fatalf("telemetry eviction moved floor to %d", ch.floor)
	}
}

func TestChannelExpireRaisesFloor(t *testing.T) {
	t.Parallel()
	old := time.Now().Add(-10 * time.Minute)
	ch := testChannel(100, kindLimits[kindMonitor])
	ch.pub(false, old) // 101
	ch.pub(true, old)  // 102
	ch.pub(false, time.Now())
	ch.expire(time.Now().Add(-retention).UnixNano())
	if ch.floor != 101 || ch.ring.n != 1 || ch.live.n != 0 {
		t.Fatalf("floor=%d ring=%d live=%d", ch.floor, ch.ring.n, ch.live.n)
	}
	if _, ok := ch.since(100); ok {
		t.Fatal("expired content must force snapshot")
	}
	if msgs, ok := ch.since(101); !ok || len(msgs) != 1 {
		t.Fatalf("since(101) = %d %v", len(msgs), ok)
	}
}

// Студенческий канал: телеметрии нет, флаг игнорируется — всё в основном кольце.
func TestChannel_StudentHasNoTelemetryRing(t *testing.T) {
	t.Parallel()
	ch := testChannel(0, kindLimits[kindStudent])
	ch.pub(true, time.Now())
	if ch.ring.n != 1 || ch.live.n != 0 {
		t.Fatalf("ring=%d live=%d", ch.ring.n, ch.live.n)
	}
}

// ---------------------------------------------------------------- Hub

func monitorMsg(typ public.MonitorMessageType) public.MonitorMessage {
	return public.MonitorMessage{Type: typ}
}

func telemetryMsg(attemptID uuid.UUID, i int) public.MonitorMessage {
	p := map[string]any{"field": "applicant.name", "value": strings.Repeat("Иванов ", 3)}
	return public.MonitorMessage{Type: public.MonitorMessageTypeAttemptEvent, AttemptId: &attemptID,
		Event: &public.AttemptEvent{Id: i, ClientSeq: i, Type: public.AttemptEventTypeFieldChanged, Payload: &p, At: time.Now().UTC()}}
}

func decodeMonitor(t *testing.T, b []byte) public.MonitorMessage {
	t.Helper()
	var m public.MonitorMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return m
}

func TestIsTelemetry(t *testing.T) {
	t.Parallel()
	ev := func(typ public.AttemptEventType) *public.AttemptEvent { return &public.AttemptEvent{Type: typ} }
	cases := []struct {
		m    public.MonitorMessage
		want bool
	}{
		{public.MonitorMessage{Type: public.MonitorMessageTypeAttemptEvent, Event: ev(public.AttemptEventTypeFieldChanged)}, true},
		{public.MonitorMessage{Type: public.MonitorMessageTypeAttemptEvent, Event: ev(public.AttemptEventTypeChooseValue)}, true},
		{public.MonitorMessage{Type: public.MonitorMessageTypeAttemptEvent, Event: ev(public.AttemptEventTypeSubmitted)}, false},
		{public.MonitorMessage{Type: public.MonitorMessageTypeAttemptEvent, Event: ev(public.AttemptEventTypeDialogueOperator)}, false},
		{public.MonitorMessage{Type: public.MonitorMessageTypeAttemptEvent}, false},
		{public.MonitorMessage{Type: public.MonitorMessageTypeDialogueTurn, Event: ev(public.AttemptEventTypeFieldChanged)}, false},
		{public.MonitorMessage{Type: public.MonitorMessageTypeParticipantStatus}, false},
	}
	for i, tc := range cases {
		if got := isTelemetry(&tc.m); got != tc.want {
			t.Errorf("case %d: isTelemetry = %v, want %v", i, got, tc.want)
		}
	}
}

func TestHub_MonitorSeqAndShape(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	lesson := uuid.New()
	s := h.subscribe(kindMonitor, lesson, 0, false)
	defer h.unsubscribe(s)
	s2 := h.subscribe(kindMonitor, lesson, 0, false)
	defer h.unsubscribe(s2)

	if s.seq < time.Now().Add(-time.Minute).UnixMicro() {
		t.Fatalf("epoch seq %d must be time based", s.seq)
	}
	aid := uuid.New()
	st := public.LessonStatus("running")
	h.Monitor(lesson, public.MonitorMessage{Type: public.MonitorMessageTypeLessonStatus, LessonStatus: &st, Seq: 999})
	h.Monitor(lesson, telemetryMsg(aid, 1))

	b1, b2 := <-s.sub.send, <-s.sub.send
	if o := <-s2.sub.send; &o[0] != &b1[0] {
		t.Fatal("subscribers must share the same encoded bytes")
	}
	m1, m2 := decodeMonitor(t, b1), decodeMonitor(t, b2)
	if int64(m1.Seq) != s.seq+1 || int64(m2.Seq) != s.seq+2 {
		t.Fatalf("seq = %d,%d want %d,%d (caller's Seq overwritten)", m1.Seq, m2.Seq, s.seq+1, s.seq+2)
	}
	if m1.At.IsZero() || time.Since(m1.At) > time.Minute || m1.At.Location() != time.UTC {
		t.Fatalf("at = %v", m1.At)
	}
	if m2.AttemptId == nil || *m2.AttemptId != aid || m2.Event == nil || m2.Event.Type != public.AttemptEventTypeFieldChanged {
		t.Fatalf("m2 = %+v", m2)
	}
	if strings.HasSuffix(string(b1), "\n") {
		t.Fatal("trailing newline in message")
	}
	if !strings.Contains(string(b2), "Иванов") {
		t.Fatalf("Cyrillic must not be escaped: %s", b2)
	}
	// Каналы разных занятий и студента независимы.
	other := h.subscribe(kindMonitor, uuid.New(), 0, false)
	defer h.unsubscribe(other)
	stu := h.subscribe(kindStudent, lesson, 0, false) // тот же uuid, другой вид канала
	defer h.unsubscribe(stu)
	h.Student(lesson, public.StudentMessage{Type: public.StudentMessageTypeLessonFinished})
	select {
	case b := <-stu.sub.send:
		var m public.StudentMessage
		if err := json.Unmarshal(b, &m); err != nil || m.Type != public.StudentMessageTypeLessonFinished || int64(m.Seq) != stu.seq+1 {
			t.Fatalf("student msg = %s %v", b, err)
		}
	default:
		t.Fatal("student message not delivered")
	}
	select {
	case b := <-other.sub.send:
		t.Fatalf("foreign lesson received %s", b)
	case b := <-s.sub.send:
		t.Fatalf("monitor received student message %s", b)
	default:
	}
}

// Регрессия (review: буфер since): при полной нагрузке телеметрией ввода содержательные
// сообщения (реплики, статусы) держатся весь срок буфера. Раньше общее кольцо на 4096
// сообщений при ~400 сообщений/с покрывало ~10 с: reconnect через 30 с получал snapshot
// и терял реплики из живого транскрипта.
func TestHub_TelemetryDoesNotEvictContent(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	lesson := uuid.New()
	s := h.subscribe(kindMonitor, lesson, 0, false)
	since := s.seq
	h.unsubscribe(s)

	aid := uuid.New()
	var content []int
	const total = 30000 // ~75 с телеметрии 200 студентов, втрое больше старого кольца
	for i := 0; i < total; i++ {
		if i%1000 == 0 {
			turn := public.DialogueTurnView{Text: "Горит квартира на третьем этаже", Speaker: "caller", Source: "llm"}
			h.Monitor(lesson, public.MonitorMessage{Type: public.MonitorMessageTypeDialogueTurn, AttemptId: &aid, Turn: &turn})
			content = append(content, i)
			continue
		}
		h.Monitor(lesson, telemetryMsg(aid, i))
	}

	re := h.subscribe(kindMonitor, lesson, since, true)
	defer h.unsubscribe(re)
	if !re.covered {
		t.Fatal("since must be covered: content messages are all buffered")
	}
	turns, last := 0, int64(since)
	for _, b := range re.replay {
		m := decodeMonitor(t, b)
		if int64(m.Seq) <= last {
			t.Fatalf("replay out of order: %d after %d", m.Seq, last)
		}
		last = int64(m.Seq)
		if m.Type == public.MonitorMessageTypeDialogueTurn {
			turns++
		}
	}
	if turns != len(content) {
		t.Fatalf("replayed %d dialogue turns, want %d", turns, len(content))
	}
	if last != re.seq {
		t.Fatalf("replay must end at current seq %d, got %d", re.seq, last)
	}
	if n := len(re.replay) - turns; n == 0 || n > monitorLiveMax {
		t.Fatalf("telemetry replayed %d, want 1..%d", n, monitorLiveMax)
	}
}

func TestHub_SlowConsumerDisconnected(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	lesson := uuid.New()
	slow := h.subscribe(kindMonitor, lesson, 0, false)
	fast := h.subscribe(kindMonitor, lesson, 0, false)
	defer h.unsubscribe(fast)
	defer h.unsubscribe(slow)

	for i := 0; i < sendQueue+10; i++ {
		h.Monitor(lesson, monitorMsg(public.MonitorMessageTypeAiHealth))
		select {
		case <-fast.sub.send: // быстрый читатель успевает
		default:
			t.Fatalf("fast subscriber missed message %d", i)
		}
		if i < sendQueue {
			select {
			case <-slow.sub.slow:
				t.Fatalf("slow marked too early at %d", i)
			default:
			}
		}
	}
	select {
	case <-slow.sub.slow:
	default:
		t.Fatal("slow subscriber must be marked slow")
	}
	if n := len(slow.sub.send); n != sendQueue {
		t.Fatalf("slow queue = %d, want full %d (nothing after the gap)", n, sendQueue)
	}
	slow.ch.mu.Lock()
	subs := len(slow.ch.subs)
	slow.ch.mu.Unlock()
	if subs != 1 {
		t.Fatalf("subs = %d, want 1 (slow removed)", subs)
	}
	h.unsubscribe(slow) // повторная отписка снятого — без паники
}

func chanOf(h *Hub, k kind, id uuid.UUID) *channel {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.chans[k][id]
}

func chanCount(h *Hub, k kind) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.chans[k])
}

func TestHub_SweepAndWatchedLessons(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	watched, idle := uuid.New(), uuid.New()
	s := h.subscribe(kindMonitor, watched, 0, false)
	h.Monitor(idle, monitorMsg(public.MonitorMessageTypeAiHealth))
	h.Student(uuid.New(), public.StudentMessage{Type: public.StudentMessageTypeTimerExpired})

	got := h.WatchedLessons()
	if len(got) != 1 || got[0] != watched {
		t.Fatalf("watched = %v, want [%s]", got, watched)
	}

	// Буфер свежий — каналы живут (reconnect с since).
	h.sweep(time.Now())
	if nm, ns := chanCount(h, kindMonitor), chanCount(h, kindStudent); nm != 2 || ns != 1 {
		t.Fatalf("after fresh sweep: monitor=%d student=%d", nm, ns)
	}

	// Всё просрочено: остаётся только канал с подписчиком.
	idleCh := chanOf(h, kindMonitor, idle)
	h.sweep(time.Now().Add(2 * retention))
	if nm, ns := chanCount(h, kindMonitor), chanCount(h, kindStudent); nm != 1 || ns != 0 {
		t.Fatalf("after expiry sweep: monitor=%d student=%d", nm, ns)
	}
	idleCh.mu.Lock()
	dead := idleCh.dead
	idleCh.mu.Unlock()
	if !dead {
		t.Fatal("swept channel must be marked dead")
	}
	// Публикация после уборки создаёт новый канал новой эпохи.
	h.Monitor(idle, monitorMsg(public.MonitorMessageTypeAiHealth))
	if chanOf(h, kindMonitor, idle) == idleCh {
		t.Fatal("publish must not reuse a dead channel")
	}
	h.unsubscribe(s)
	if len(h.WatchedLessons()) != 0 {
		t.Fatal("no monitors after unsubscribe")
	}
}

func TestHub_ConcurrentPublishSubscribeRace(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	lesson := uuid.New()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				h.Monitor(lesson, telemetryMsg(uuid.New(), 1))
				h.Student(lesson, public.StudentMessage{Type: public.StudentMessageTypeTimerExpired})
			}
		}()
	}
	for i := 0; i < 50; i++ {
		s := h.subscribe(kindMonitor, lesson, int64(i), i%2 == 0)
		var last int64
		for _, b := range s.replay {
			m := decodeMonitor(t, b)
			if int64(m.Seq) <= last {
				t.Fatal("replay out of order")
			}
			last = int64(m.Seq)
		}
		h.unsubscribe(s)
		h.sweep(time.Now())
		_ = h.WatchedLessons()
	}
	close(stop)
	wg.Wait()
}

func TestHub_CloseIdempotentWithoutConnections(t *testing.T) {
	t.Parallel()
	h := NewHub(nil) // nil логгер — slog.Default
	done := make(chan struct{})
	go func() {
		h.Close()
		h.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close without connections must not wait")
	}
	if h.enter() {
		t.Fatal("enter after Close must fail")
	}
	if h.Connections() != 0 {
		t.Fatal("connections")
	}
}

func TestEncodeJSON(t *testing.T) {
	t.Parallel()
	b, err := encodeJSON(map[string]string{"text": "<Пожар> & дым"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"text":"<Пожар> & дым"}` {
		t.Fatalf("encoded = %s", b)
	}
	if _, err := encodeJSON(map[string]any{"f": func() {}}); err == nil {
		t.Fatal("want error")
	}
	// Буфер пула переиспользуется: результат — независимая копия.
	a, _ := encodeJSON("first")
	_, _ = encodeJSON("second-longer")
	if string(a) != `"first"` {
		t.Fatalf("result aliased pool buffer: %s", a)
	}
}

func TestSetAuthenticator(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	if h.authenticator() != nil {
		t.Fatal("no authenticator by default")
	}
	a := &stubAuth{}
	h.SetAuthenticator(a)
	if h.authenticator() != core.Authenticator(a) {
		t.Fatal("authenticator not set")
	}
	h.SetAuthenticator(nil)
	if h.authenticator() != nil {
		t.Fatal("nil must disable")
	}
}
