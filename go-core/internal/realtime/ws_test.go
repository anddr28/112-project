package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
)

// ---------------------------------------------------------------- заглушки портов

// stubAuth — core.Authenticator с изменяемым результатом (выход/блокировка/смена роли «на лету»).
type stubAuth struct {
	mu    sync.Mutex
	p     *core.Principal
	err   error
	calls int
}

func (a *stubAuth) Authenticate(*http.Request) (*core.Principal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.err != nil {
		return nil, a.err
	}
	if a.p == nil {
		return nil, core.ErrUnauthenticated
	}
	cp := *a.p
	return &cp, nil
}

func (a *stubAuth) set(p *core.Principal, err error) {
	a.mu.Lock()
	a.p, a.err = p, err
	a.mu.Unlock()
}

func (a *stubAuth) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

type stubMonitor struct {
	mu       sync.Mutex
	canErr   func(p *core.Principal) error
	snapErr  error
	lesson   *public.Lesson
	attempts []public.Attempt
}

func (m *stubMonitor) CanMonitor(_ context.Context, p *core.Principal, _ uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.canErr != nil {
		return m.canErr(p)
	}
	return nil
}

func (m *stubMonitor) Snapshot(context.Context, uuid.UUID) (*public.Lesson, []public.Attempt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lesson, m.attempts, m.snapErr
}

func (m *stubMonitor) setCan(f func(p *core.Principal) error) {
	m.mu.Lock()
	m.canErr = f
	m.mu.Unlock()
}

type presenceCall struct {
	attempt uuid.UUID
	online  bool
}

type stubPresence struct {
	lesson    uuid.UUID
	owner     uuid.UUID
	accessErr error
	calls     chan presenceCall
}

func (s *stubPresence) AttemptAccess(_ context.Context, p *core.Principal, _ uuid.UUID) (uuid.UUID, error) {
	if s.accessErr != nil {
		return uuid.Nil, s.accessErr
	}
	if p.UserID != s.owner {
		return uuid.Nil, httpx.Forbidden("")
	}
	return s.lesson, nil
}

func (s *stubPresence) SetOnline(_ context.Context, attemptID uuid.UUID, online bool) {
	s.calls <- presenceCall{attemptID, online}
}

// ---------------------------------------------------------------- стенд

type wsEnv struct {
	hub  *Hub
	auth *stubAuth
	mon  *stubMonitor
	pres *stubPresence
	srv  *httptest.Server
}

func teacher() *core.Principal {
	return &core.Principal{UserID: uuid.New(), SessionID: uuid.New(), Role: core.RoleTeacher, LastName: "Петрова"}
}

func newWSEnv(t *testing.T, p *core.Principal) *wsEnv {
	t.Helper()
	e := &wsEnv{
		hub:  NewHub(quietLog()),
		auth: &stubAuth{p: p},
		mon:  &stubMonitor{lesson: &public.Lesson{Id: uuid.New(), Title: "Пожары"}},
		pres: &stubPresence{lesson: uuid.New(), calls: make(chan presenceCall, 16)},
	}
	if p != nil {
		e.pres.owner = p.UserID
	}
	mux := http.NewServeMux()
	rt := httpx.NewRouter(mux, "/api/v1", e.auth, quietLog(), nil)
	// Как в internal/app: роли на маршрутах, хендлеры хаба.
	rt.Handle("GET /ws/lessons/{lessonId}/monitor", httpx.Roles(core.RoleTeacher, core.RoleAdmin),
		e.hub.MonitorHandler(e.mon, nil))
	rt.Handle("GET /ws/attempts/{attemptId}", httpx.Roles(core.RoleStudent),
		e.hub.StudentHandler(e.pres, nil))
	e.srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		e.hub.Close()
		e.srv.Close()
	})
	return e
}

func (e *wsEnv) url(path string) string {
	return "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/api/v1" + path
}

// wsClient — клиент с постоянным фоновым чтением: у coder/websocket отмена ctx в Read
// закрывает соединение, поэтому «подождать и убедиться, что открыто» делается так.
type wsClient struct {
	c    *websocket.Conn
	msgs chan []byte
	done chan struct{} // закрыт — чтение завершилось с err
	err  error
}

func newWSClient(c *websocket.Conn) *wsClient {
	w := &wsClient{c: c, msgs: make(chan []byte, 8192), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		for {
			typ, b, err := c.Read(context.Background())
			if err != nil {
				w.err = err
				return
			}
			if typ == websocket.MessageText {
				w.msgs <- b
			}
		}
	}()
	return w
}

func (e *wsEnv) dial(t *testing.T, path string) *wsClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, e.url(path), nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial %s: %v (status %d)", path, err, status)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return newWSClient(c)
}

// dialFail — upgrade отклонён до 101: статус и ApiError.code.
func (e *wsEnv) dialFail(t *testing.T, path string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, e.url(path), nil)
	if err == nil {
		_ = c.CloseNow()
		t.Fatalf("dial %s must fail", path)
	}
	if resp == nil {
		t.Fatalf("dial %s: no response: %v", path, err)
	}
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatalf("error body is not ApiError JSON: %q", b)
	}
	if body.Message == "" {
		t.Fatalf("ApiError without message: %s", b)
	}
	return resp.StatusCode, body.Code
}

// read — следующее сообщение (сначала уже полученные, потом ожидание).
func read(t *testing.T, c *wsClient) []byte {
	t.Helper()
	select {
	case b := <-c.msgs:
		return b
	default:
	}
	select {
	case b := <-c.msgs:
		return b
	case <-c.done:
		select {
		case b := <-c.msgs:
			return b
		default:
		}
		t.Fatalf("read: connection closed: %v", c.err)
	case <-time.After(5 * time.Second):
		t.Fatal("read: timeout")
	}
	return nil
}

// readClose — ждёт закрытия соединения сервером, возвращает код закрытия.
func readClose(t *testing.T, c *wsClient, within time.Duration) websocket.StatusCode {
	t.Helper()
	select {
	case <-c.done:
		return websocket.CloseStatus(c.err)
	case <-time.After(within):
		t.Fatalf("connection still open after %v", within)
	}
	return -1
}

// staysOpen — за d сервер соединение не закрыл (сообщения допускаются).
func staysOpen(t *testing.T, c *wsClient, d time.Duration) {
	t.Helper()
	select {
	case <-c.done:
		t.Fatalf("connection closed unexpectedly: %v", c.err)
	case <-time.After(d):
	}
}

func monitorPath(lesson uuid.UUID) string { return "/ws/lessons/" + lesson.String() + "/monitor" }

// ---------------------------------------------------------------- доступ до upgrade

func TestMonitorWS_AccessErrors(t *testing.T) {
	t.Parallel()
	student := &core.Principal{UserID: uuid.New(), SessionID: uuid.New(), Role: core.RoleStudent}
	cases := []struct {
		name   string
		p      *core.Principal
		err    error
		can    func(*core.Principal) error
		path   string
		status int
		code   string
	}{
		{"no session", nil, nil, nil, monitorPath(uuid.New()), 401, httpx.CodeUnauthorized},
		{"blocked", teacher(), core.ErrUserBlocked, nil, monitorPath(uuid.New()), http.StatusLocked, httpx.CodeUserBlocked},
		{"student role", student, nil, nil, monitorPath(uuid.New()), 403, httpx.CodeForbidden},
		{"foreign lesson", teacher(), nil, func(*core.Principal) error { return httpx.Forbidden("") }, monitorPath(uuid.New()), 403, httpx.CodeForbidden},
		{"no lesson", teacher(), nil, func(*core.Principal) error { return httpx.NotFound("Занятие не найдено") }, monitorPath(uuid.New()), 404, httpx.CodeNotFound},
		{"bad id", teacher(), nil, nil, "/ws/lessons/not-a-uuid/monitor", 404, httpx.CodeNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newWSEnv(t, tc.p)
			e.auth.set(tc.p, tc.err)
			e.mon.setCan(tc.can)
			status, code := e.dialFail(t, tc.path)
			if status != tc.status || code != tc.code {
				t.Fatalf("status/code = %d/%s, want %d/%s", status, code, tc.status, tc.code)
			}
			if e.hub.Connections() != 0 {
				t.Fatal("rejected request must not count as connection")
			}
		})
	}
}

func TestWS_PlainHTTPGetsUpgradeRequired(t *testing.T) {
	t.Parallel()
	e := newWSEnv(t, teacher())
	resp, err := http.Get(e.srv.URL + "/api/v1" + monitorPath(uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct{ Code, Message string }
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != http.StatusUpgradeRequired || body.Code != httpx.CodeValidation || body.Message == "" {
		t.Fatalf("status %d body %+v", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
}

// ---------------------------------------------------------------- монитор

func TestMonitorWS_SnapshotThenLive(t *testing.T) {
	t.Parallel()
	e := newWSEnv(t, teacher())
	lesson := uuid.New()
	c := e.dial(t, monitorPath(lesson))

	raw := read(t, c)
	var shape map[string]any
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"seq", "type", "at", "snapshot"} {
		if _, ok := shape[k]; !ok {
			t.Fatalf("snapshot message lacks %q: %s", k, raw)
		}
	}
	snap := shape["snapshot"].(map[string]any)
	if arr, ok := snap["attempts"].([]any); !ok || len(arr) != 0 {
		t.Fatalf("attempts must be [] (not null/absent): %s", raw)
	}
	if l, ok := snap["lesson"].(map[string]any); !ok || l["title"] != "Пожары" {
		t.Fatalf("lesson = %v", snap["lesson"])
	}
	first := decodeMonitor(t, raw)
	if first.Type != public.MonitorMessageTypeSnapshot {
		t.Fatalf("first message type %s", first.Type)
	}
	if e.hub.Connections() != 1 {
		t.Fatalf("connections = %d", e.hub.Connections())
	}
	if got := e.hub.WatchedLessons(); len(got) != 1 || got[0] != lesson {
		t.Fatalf("watched = %v", got)
	}

	aid := uuid.New()
	e.hub.Monitor(lesson, telemetryMsg(aid, 1))
	e.hub.Monitor(uuid.New(), monitorMsg(public.MonitorMessageTypeAiHealth)) // чужое занятие
	st := public.LessonStatus("finished")
	e.hub.Monitor(lesson, public.MonitorMessage{Type: public.MonitorMessageTypeLessonStatus, LessonStatus: &st})
	m1, m2 := decodeMonitor(t, read(t, c)), decodeMonitor(t, read(t, c))
	if m1.Seq != first.Seq+1 || m2.Seq != first.Seq+2 {
		t.Fatalf("live seqs %d,%d after snapshot %d", m1.Seq, m2.Seq, first.Seq)
	}
	if m1.Type != public.MonitorMessageTypeAttemptEvent || *m1.AttemptId != aid || m2.LessonStatus == nil || *m2.LessonStatus != st {
		t.Fatalf("m1=%+v m2=%+v", m1, m2)
	}

	// Клиентские сообщения (heartbeat) допустимы и игнорируются.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.c.Write(ctx, websocket.MessageText, []byte(`{"type":"ping"}`)); err != nil {
		t.Fatal(err)
	}
	e.hub.Monitor(lesson, monitorMsg(public.MonitorMessageTypeAiHealth))
	if m := decodeMonitor(t, read(t, c)); m.Seq != first.Seq+3 {
		t.Fatalf("after client message seq %d", m.Seq)
	}
}

func TestMonitorWS_SnapshotAttemptsAndError(t *testing.T) {
	t.Parallel()
	e := newWSEnv(t, teacher())
	e.mon.attempts = []public.Attempt{{Id: uuid.New()}}
	c := e.dial(t, monitorPath(uuid.New()))
	m := decodeMonitor(t, read(t, c))
	if m.Snapshot == nil || m.Snapshot.Attempts == nil || len(*m.Snapshot.Attempts) != 1 {
		t.Fatalf("snapshot = %+v", m.Snapshot)
	}

	e2 := newWSEnv(t, teacher())
	e2.mon.snapErr = errors.New("db down")
	c2 := e2.dial(t, monitorPath(uuid.New()))
	if code := readClose(t, c2, 5*time.Second); code != websocket.StatusInternalError {
		t.Fatalf("close code %v, want internal error", code)
	}
}

func TestMonitorWS_ReconnectWithSince(t *testing.T) {
	t.Parallel()
	e := newWSEnv(t, teacher())
	lesson := uuid.New()
	c := e.dial(t, monitorPath(lesson))
	snap := decodeMonitor(t, read(t, c))
	e.hub.Monitor(lesson, monitorMsg(public.MonitorMessageTypeAiHealth)) // seq+1 — клиент получил
	got := decodeMonitor(t, read(t, c))
	_ = c.c.CloseNow()

	// Пропущено, пока клиента не было:
	aid := uuid.New()
	turn := public.DialogueTurnView{Text: "Алло, пожар!", Speaker: "caller", Source: "llm"}
	e.hub.Monitor(lesson, public.MonitorMessage{Type: public.MonitorMessageTypeDialogueTurn, AttemptId: &aid, Turn: &turn})
	e.hub.Monitor(lesson, telemetryMsg(aid, 2))

	c2 := e.dial(t, monitorPath(lesson)+"?since="+strconv.Itoa(got.Seq))
	r1, r2 := decodeMonitor(t, read(t, c2)), decodeMonitor(t, read(t, c2))
	if r1.Type != public.MonitorMessageTypeDialogueTurn || r1.Seq != got.Seq+1 || r2.Seq != got.Seq+2 {
		t.Fatalf("replay = %+v / %+v", r1, r2)
	}
	if r1.Turn == nil || r1.Turn.Text != "Алло, пожар!" {
		t.Fatalf("turn = %+v", r1.Turn)
	}
	e.hub.Monitor(lesson, monitorMsg(public.MonitorMessageTypeAiHealth))
	if m := decodeMonitor(t, read(t, c2)); m.Seq != got.Seq+3 || m.Type != public.MonitorMessageTypeAiHealth {
		t.Fatalf("live after replay = %+v", m)
	}

	// since = текущий seq: ничего не пропущено — ни snapshot, ни replay, сразу живой поток.
	cur := got.Seq + 3
	c3 := e.dial(t, monitorPath(lesson)+"?since="+strconv.Itoa(cur))
	e.hub.Monitor(lesson, monitorMsg(public.MonitorMessageTypeAiHealth))
	if m := decodeMonitor(t, read(t, c3)); m.Seq != cur+1 {
		t.Fatalf("since=cur: first message %+v", m)
	}

	// since не покрыт (другая эпоха / мусор) — snapshot.
	for _, q := range []string{"?since=1", "?since=" + strconv.Itoa(cur+100), "?since=abc", "?since=-1"} {
		cx := e.dial(t, monitorPath(lesson)+q)
		if m := decodeMonitor(t, read(t, cx)); m.Type != public.MonitorMessageTypeSnapshot {
			t.Fatalf("%s: first message %s, want snapshot", q, m.Type)
		}
	}
	_ = snap
}

// ---------------------------------------------------------------- студент

func studentPath(attempt uuid.UUID) string { return "/ws/attempts/" + attempt.String() }

func TestStudentWS_AccessAndPresence(t *testing.T) {
	t.Parallel()
	stu := &core.Principal{UserID: uuid.New(), SessionID: uuid.New(), Role: core.RoleStudent}
	e := newWSEnv(t, stu)
	attempt := uuid.New()

	// Чужая попытка / нет попытки / преподаватель — до upgrade.
	other := &core.Principal{UserID: uuid.New(), SessionID: uuid.New(), Role: core.RoleStudent}
	e.auth.set(other, nil)
	if st, code := e.dialFail(t, studentPath(attempt)); st != 403 || code != httpx.CodeForbidden {
		t.Fatalf("foreign attempt: %d %s", st, code)
	}
	e.auth.set(teacher(), nil)
	if st, _ := e.dialFail(t, studentPath(attempt)); st != 403 {
		t.Fatalf("teacher on student channel: %d", st)
	}
	e.auth.set(stu, nil)
	e.pres.accessErr = httpx.NotFound("Попытка не найдена")
	if st, code := e.dialFail(t, studentPath(attempt)); st != 404 || code != httpx.CodeNotFound {
		t.Fatalf("missing attempt: %d %s", st, code)
	}
	e.pres.accessErr = nil

	c1 := e.dial(t, studentPath(attempt))
	if pc := <-e.pres.calls; !pc.online || pc.attempt != attempt {
		t.Fatalf("presence = %+v, want online", pc)
	}
	// Вторая вкладка того же участника (следующая карточка) — без ложных переходов.
	next := uuid.New()
	c2 := e.dial(t, studentPath(next))
	e.hub.Student(attempt, public.StudentMessage{Type: public.StudentMessageTypeTimerExpired})
	var m public.StudentMessage
	if err := json.Unmarshal(read(t, c1), &m); err != nil || m.Type != public.StudentMessageTypeTimerExpired || m.Seq == 0 || m.At.IsZero() {
		t.Fatalf("student message = %+v %v", m, err)
	}
	_ = c1.c.Close(websocket.StatusNormalClosure, "")
	select {
	case pc := <-e.pres.calls:
		t.Fatalf("unexpected presence change %+v while second tab is open", pc)
	case <-time.After(200 * time.Millisecond):
	}
	_ = c2.c.Close(websocket.StatusNormalClosure, "")
	select {
	case pc := <-e.pres.calls:
		if pc.online || pc.attempt != next {
			t.Fatalf("presence = %+v, want offline for last attempt", pc)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("offline not reported")
	}
	waitConns(t, e.hub, 0)
	e.hub.presMu.Lock()
	n := len(e.hub.presence)
	e.hub.presMu.Unlock()
	if n != 0 {
		t.Fatalf("presence map leaks %d entries", n)
	}
}

func TestStudentWS_ReplayWithoutSnapshot(t *testing.T) {
	t.Parallel()
	stu := &core.Principal{UserID: uuid.New(), SessionID: uuid.New(), Role: core.RoleStudent}
	e := newWSEnv(t, stu)
	attempt := uuid.New()
	e.hub.Student(attempt, public.StudentMessage{Type: public.StudentMessageTypeEvaluationUpdated})
	s := e.hub.subscribe(kindStudent, attempt, 0, false)
	cur := s.seq
	e.hub.unsubscribe(s)

	// Без since: только живой поток (без snapshot и без старого).
	c := e.dial(t, studentPath(attempt))
	<-e.pres.calls
	e.hub.Student(attempt, public.StudentMessage{Type: public.StudentMessageTypeLessonFinished})
	var m public.StudentMessage
	_ = json.Unmarshal(read(t, c), &m)
	if m.Type != public.StudentMessageTypeLessonFinished || int64(m.Seq) != cur+1 {
		t.Fatalf("live = %+v", m)
	}
	// С since: пропущенное.
	c2 := e.dial(t, studentPath(attempt)+"?since="+strconv.FormatInt(cur-1, 10))
	_ = json.Unmarshal(read(t, c2), &m)
	if m.Type != public.StudentMessageTypeEvaluationUpdated || int64(m.Seq) != cur {
		t.Fatalf("replay = %+v", m)
	}
}

func waitConns(t *testing.T, h *Hub, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for h.Connections() != n {
		if time.Now().After(deadline) {
			t.Fatalf("connections = %d, want %d", h.Connections(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---------------------------------------------------------------- повторная проверка сессии

// Регрессия (review: WS после выхода): после выхода, блокировки или смены роли открытый
// WS закрывается сервером (1008), а не продолжает отдавать поток мониторинга.
func TestWS_RevalidationClosesRevokedSessions(t *testing.T) {
	t.Parallel()
	blockedErr := core.ErrUserBlocked
	cases := []struct {
		name    string
		student bool
		change  func(e *wsEnv, p *core.Principal)
	}{
		{"monitor logout", false, func(e *wsEnv, _ *core.Principal) { e.auth.set(nil, core.ErrUnauthenticated) }},
		{"monitor blocked", false, func(e *wsEnv, _ *core.Principal) { e.auth.set(nil, blockedErr) }},
		{"monitor demoted to student", false, func(e *wsEnv, p *core.Principal) {
			np := *p
			np.Role = core.RoleStudent
			e.auth.set(&np, nil)
		}},
		{"monitor admin demoted to teacher without access", false, func(e *wsEnv, p *core.Principal) {
			np := *p
			np.Role = core.RoleTeacher
			e.mon.setCan(func(q *core.Principal) error {
				if q.Role == core.RoleTeacher {
					return httpx.Forbidden("")
				}
				return nil
			})
			e.auth.set(&np, nil)
		}},
		{"monitor other session cookie", false, func(e *wsEnv, p *core.Principal) {
			np := *p
			np.SessionID = uuid.New()
			e.auth.set(&np, nil)
		}},
		{"student logout", true, func(e *wsEnv, _ *core.Principal) { e.auth.set(nil, core.ErrUnauthenticated) }},
		{"student blocked", true, func(e *wsEnv, _ *core.Principal) { e.auth.set(nil, blockedErr) }},
		{"student promoted to teacher", true, func(e *wsEnv, p *core.Principal) {
			np := *p
			np.Role = core.RoleTeacher
			e.auth.set(&np, nil)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := teacher()
			if strings.Contains(tc.name, "admin") {
				p.Role = core.RoleAdmin
			}
			if tc.student {
				p.Role = core.RoleStudent
			}
			e := newWSEnv(t, p)
			e.hub.revalidate = 20 * time.Millisecond
			e.hub.SetAuthenticator(e.auth)

			var c *wsClient
			if tc.student {
				c = e.dial(t, studentPath(uuid.New()))
				<-e.pres.calls
			} else {
				lesson := uuid.New()
				c = e.dial(t, monitorPath(lesson))
				read(t, c) // snapshot
			}
			staysOpen(t, c, 100*time.Millisecond) // пока сессия жива — соединение живёт
			before := e.auth.callCount()
			if before < 2 {
				t.Fatalf("session was not re-checked (calls=%d)", before)
			}
			tc.change(e, p)
			if code := readClose(t, c, 3*time.Second); code != websocket.StatusPolicyViolation {
				t.Fatalf("close code = %v, want policy violation", code)
			}
			waitConns(t, e.hub, 0)
			if tc.student {
				if pc := <-e.pres.calls; pc.online {
					t.Fatal("student must go offline after revocation")
				}
			}
		})
	}
}

func TestWS_RevalidationKeepsConnection(t *testing.T) {
	t.Parallel()
	t.Run("transient error", func(t *testing.T) {
		t.Parallel()
		p := teacher()
		e := newWSEnv(t, p)
		e.hub.revalidate = 20 * time.Millisecond
		e.hub.SetAuthenticator(e.auth)
		lesson := uuid.New()
		c := e.dial(t, monitorPath(lesson))
		read(t, c)
		e.auth.set(nil, errors.New("database unavailable"))
		staysOpen(t, c, 150*time.Millisecond)
		e.auth.set(p, nil)
		e.hub.Monitor(lesson, monitorMsg(public.MonitorMessageTypeAiHealth))
		if m := decodeMonitor(t, read(t, c)); m.Type != public.MonitorMessageTypeAiHealth {
			t.Fatalf("message after transient error: %+v", m)
		}
	})
	t.Run("role change still allowed", func(t *testing.T) {
		t.Parallel()
		p := teacher()
		e := newWSEnv(t, p)
		e.hub.revalidate = 20 * time.Millisecond
		e.hub.SetAuthenticator(e.auth)
		c := e.dial(t, monitorPath(uuid.New()))
		read(t, c)
		np := *p
		np.Role = core.RoleAdmin
		e.auth.set(&np, nil)
		staysOpen(t, c, 150*time.Millisecond)
	})
	t.Run("transient access check error", func(t *testing.T) {
		t.Parallel()
		p := teacher()
		e := newWSEnv(t, p)
		e.hub.revalidate = 20 * time.Millisecond
		e.hub.SetAuthenticator(e.auth)
		c := e.dial(t, monitorPath(uuid.New()))
		read(t, c)
		e.mon.setCan(func(*core.Principal) error { return httpx.Internal(errors.New("db")) })
		np := *p
		np.Role = core.RoleAdmin
		e.auth.set(&np, nil)
		staysOpen(t, c, 150*time.Millisecond)
	})
	t.Run("no authenticator", func(t *testing.T) {
		t.Parallel()
		p := teacher()
		e := newWSEnv(t, p)
		e.hub.revalidate = 20 * time.Millisecond
		c := e.dial(t, monitorPath(uuid.New()))
		read(t, c)
		calls := e.auth.callCount()
		e.auth.set(nil, core.ErrUnauthenticated)
		staysOpen(t, c, 100*time.Millisecond)
		if e.auth.callCount() != calls {
			t.Fatal("without SetAuthenticator the hub must not re-check")
		}
	})
}

// ---------------------------------------------------------------- остановка

func TestWS_HubCloseDisconnectsAndRejects(t *testing.T) {
	t.Parallel()
	e := newWSEnv(t, teacher())
	lesson := uuid.New()
	c := e.dial(t, monitorPath(lesson))
	read(t, c)
	waitConns(t, e.hub, 1)

	closed := make(chan struct{})
	go func() { e.hub.Close(); close(closed) }()
	if code := readClose(t, c, 5*time.Second); code != websocket.StatusGoingAway {
		t.Fatalf("close code = %v, want going away", code)
	}
	select {
	case <-closed:
	case <-time.After(closeWait + time.Second):
		t.Fatal("Close did not return")
	}
	if e.hub.Connections() != 0 {
		t.Fatalf("connections = %d", e.hub.Connections())
	}
	status, code := e.dialFail(t, monitorPath(lesson))
	if status != http.StatusServiceUnavailable || code != httpx.CodeInternal {
		t.Fatalf("after close: %d %s", status, code)
	}
}

func TestParseSinceAndHeaderToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		q   string
		n   int64
		has bool
	}{
		{"", 0, false},
		{"since=0", 0, true},
		{"since=1727000000000000", 1727000000000000, true},
		{"since=-3", 0, false},
		{"since=abc", 0, false},
		{"since=1.5", 0, false},
		{"since=99999999999999999999", 0, false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/x?"+tc.q, nil)
		n, has := parseSince(r)
		if n != tc.n || has != tc.has {
			t.Errorf("parseSince(%q) = %d,%v want %d,%v", tc.q, n, has, tc.n, tc.has)
		}
	}
	tokens := []struct {
		v    string
		want bool
	}{
		{"websocket", true},
		{"WebSocket", true},
		{"h2c, websocket", true},
		{" keep-alive ,  WEBSOCKET ", true},
		{"", false},
		{"websockets", false},
		{"h2c", false},
	}
	for _, tc := range tokens {
		if got := headerHasToken(tc.v, "websocket"); got != tc.want {
			t.Errorf("headerHasToken(%q) = %v", tc.v, got)
		}
	}
}
