package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type outageConfig struct {
	Stand      Stand
	AdminLogin string
	AdminPass  string
	Prefix     string
	Password   string
	Scenarios  []string // kind:duration, например proxy:5s, pause:20s, restart, pgrestart:10s
	Timeout    time.Duration
	EvalWait   time.Duration
	Recovery   time.Duration // потолок восстановления по ТЗ (30 с)
}

// OutageResult — итог одного сценария сбоя.
type OutageResult struct {
	Name        string  `json:"name"`
	Kind        string  `json:"kind"`
	Description string  `json:"description"`
	OutageSec   float64 `json:"outageSec"` // фактическая длительность сбоя (от обрыва до снятия)
	// Время от снятия сбоя (секунды); -1 — не наступило за отведённое время.
	FirstOKSec       float64  `json:"firstSuccessfulRequestSec"`
	EventsFlushSec   float64  `json:"eventsOutboxFlushedSec"`
	DraftFlushSec    float64  `json:"draftSyncedSec"`
	WSReconnectSec   float64  `json:"wsReconnectSec"`
	WSCatchupSec     float64  `json:"wsMissedMessagesReceivedSec"`
	ReadyzOKSec      float64  `json:"readyzOkSec"`
	ReadyzDownSec    float64  `json:"readyzDownSec"`
	ReadyzTimeline   []string `json:"readyzTimeline"` // переходы readyz: «+t ok|fail» от начала сбоя
	WSHeartbeatFails int      `json:"wsHeartbeatFails"`
	RecoverySec      float64  `json:"recoverySec"` // max из FirstOK/EventsFlush/DraftFlush/WSReconnect
	EventsProduced   int      `json:"eventsProduced"`
	BacklogAtRestore int      `json:"eventsBacklogAtRestore"` // неподтверждённых событий в outbox в момент восстановления
	DraftVersions    int      `json:"draftVersions"`
	FailedRequests   int      `json:"failedRequests"` // неуспешных HTTP-запросов клиента за сценарий
	EventsResent     int      `json:"eventsResentDuplicates"`
	WSMissed         int      `json:"wsMessagesPublishedDuringOutage"`
	WSReplayed       int      `json:"wsMessagesReceivedAfterReconnect"`
	WSGaps           int      `json:"wsSeqGaps"`
	WSReconnects     int      `json:"wsReconnects"`
	WSLiveAfter      bool     `json:"wsLiveAfterRecovery"`
	// Целостность
	DBEvents        [4]int64 `json:"dbClientEvents"` // count, distinct, min, max clientSeq в attempt_events
	APIEvents       [4]int64 `json:"apiClientEvents"`
	ExactlyOnce     bool     `json:"eventsExactlyOnce"`
	DraftMatches    bool     `json:"draftMatchesClient"`
	SubmitOK        bool     `json:"submitOk"`
	EvaluationDone  bool     `json:"evaluationDone"`
	EvaluationSec   float64  `json:"evaluationDoneSec"`
	OverrideApplied bool     `json:"overrideApplied"`
	Pass            bool     `json:"pass"`
	Notes           []string `json:"notes,omitempty"`
}

// ---------------------------------------------------------------- клиент «как SPA»

type wsTrack struct {
	name, attempt  string
	mu             sync.Mutex
	lastSeq        int64
	msgs           []wsMsg
	connects       []time.Time
	disconnects    []time.Time
	gaps           int
	heartbeatFails int
}

type wsMsg struct {
	seq   int64
	typ   string
	at    time.Time
	score float64
}

type reqEntry struct {
	start, end time.Time
	ok         bool
	status     int
}

type spaClient struct {
	c *Client

	mu         sync.Mutex
	attempt    string
	seq        int
	outbox     []map[string]any
	ackedMax   int
	sent       int
	accepted   int
	draft      map[string]any
	draftVer   int
	ackVer     int
	reqs       []reqEntry
	producing  bool
	sendersRun bool
	// backoff — политика повторов исходящей очереди SPA (frontend outbox.ts: 1, 2, 4, 8, 10 с);
	// без неё — повтор на каждом тике (500/900 мс), как при непрерывном вводе в SPA.
	backoff        bool
	evNext, drNext time.Time
	evFail, drFail int
}

var spaRetry = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second}

func (s *spaClient) due(next time.Time) bool { return !s.backoff || !time.Now().Before(next) }

func (s *spaClient) failed(next *time.Time, fails *int) {
	*next = time.Now().Add(spaRetry[min(*fails, len(spaRetry)-1)])
	*fails++
}

func (s *spaClient) logReq(start time.Time, r Resp) {
	s.mu.Lock()
	s.reqs = append(s.reqs, reqEntry{start: start, end: time.Now(), ok: r.OK(), status: r.Status})
	s.mu.Unlock()
}

func (s *spaClient) call(ctx context.Context, method, path string, body any, expect ...int) Resp {
	start := time.Now()
	r := s.c.Call(ctx, method, path, "", body, expect...)
	if ctx.Err() == nil {
		s.logReq(start, r)
	}
	return r
}

// producer — ввод оператора: событие каждые 250 мс и правка черновика.
func (s *spaClient) producer(ctx context.Context) {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.mu.Lock()
		if s.producing {
			s.seq++
			s.outbox = append(s.outbox, map[string]any{"clientSeq": s.seq, "type": "field_changed",
				"at": time.Now().UTC().Format(time.RFC3339Nano), "payload": map[string]any{"field": "description", "n": s.seq}})
			s.draftVer++
			s.draft["description"] = fmt.Sprintf("Горит квартира, дым из окна. Версия черновика %d", s.draftVer)
			s.draft["applicant"] = map[string]any{"name": fmt.Sprintf("Заявитель %d", s.draftVer), "status": "очевидец"}
		}
		s.mu.Unlock()
	}
}

// sender — outbox событий: пачка до 200 раз в 500 мс; при сбое пачка остаётся и уходит
// повторно (сервер отбрасывает уже принятые clientSeq).
func (s *spaClient) sender(ctx context.Context) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.flushEvents(ctx)
	}
}

func (s *spaClient) flushEvents(ctx context.Context) bool {
	s.mu.Lock()
	n := min(len(s.outbox), 200)
	batch := append([]map[string]any(nil), s.outbox[:n]...)
	id := s.attempt
	due := s.due(s.evNext)
	s.mu.Unlock()
	if n == 0 || !due {
		return n == 0
	}
	r := s.call(ctx, http.MethodPost, "/attempts/"+id+"/events", map[string]any{"events": batch})
	var res struct {
		Accepted int
		LastSeq  int
	}
	if !r.OK() || r.JSON(&res) != nil {
		s.mu.Lock()
		s.failed(&s.evNext, &s.evFail)
		s.mu.Unlock()
		return false
	}
	s.mu.Lock()
	s.evFail = 0
	s.outbox = s.outbox[n:]
	s.ackedMax = batch[n-1]["clientSeq"].(int)
	s.sent += n
	s.accepted += res.Accepted
	s.mu.Unlock()
	return true
}

// saver — автосохранение черновика целиком, если есть неподтверждённая версия (раз в 900 мс).
func (s *spaClient) saver(ctx context.Context) {
	t := time.NewTicker(900 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.saveDraft(ctx)
	}
}

func (s *spaClient) saveDraft(ctx context.Context) bool {
	s.mu.Lock()
	if s.draftVer == s.ackVer || !s.due(s.drNext) {
		s.mu.Unlock()
		return s.draftVer == s.ackVer
	}
	ver := s.draftVer
	body, _ := json.Marshal(s.draft)
	id := s.attempt
	s.mu.Unlock()
	r := s.call(ctx, http.MethodPut, "/attempts/"+id+"/draft", body)
	if !r.OK() {
		s.mu.Lock()
		s.failed(&s.drNext, &s.drFail)
		s.mu.Unlock()
		return false
	}
	s.mu.Lock()
	s.drFail = 0
	s.ackVer = max(s.ackVer, ver)
	s.mu.Unlock()
	return true
}

// wsLoop держит канал попытки и переподключается с since = последний полученный seq.
func (s *spaClient) wsLoop(ctx context.Context, w *wsTrack) {
	for ctx.Err() == nil {
		w.mu.Lock()
		p := "/ws/attempts/" + w.attempt
		if w.lastSeq > 0 {
			p += "?since=" + strconv.FormatInt(w.lastSeq, 10)
		}
		w.mu.Unlock()
		conn, err := s.c.DialWS(ctx, p, "")
		if err != nil {
			sleepCtx(ctx, 500*time.Millisecond)
			continue
		}
		w.mu.Lock()
		w.connects = append(w.connects, time.Now())
		w.mu.Unlock()
		// Пульс клиента: WebSocket-ping раз в 5 с, нет pong за 3 с — соединение мёртвое
		// («зомби» после смены маршрута), рвём и переподключаемся с since.
		hbCtx, hbStop := context.WithCancel(ctx)
		go func() {
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-hbCtx.Done():
					return
				case <-t.C:
				}
				pctx, cancel := context.WithTimeout(hbCtx, 3*time.Second)
				err := conn.Ping(pctx)
				cancel()
				if err != nil && hbCtx.Err() == nil {
					w.mu.Lock()
					w.heartbeatFails++
					w.mu.Unlock()
					conn.CloseNow()
					return
				}
			}
		}()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				break
			}
			var m struct {
				Seq        int64
				Type       string
				Evaluation *struct{ FinalScore float64 }
			}
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			w.mu.Lock()
			if w.lastSeq > 0 && m.Seq != w.lastSeq+1 {
				w.gaps++
			}
			w.lastSeq = m.Seq
			msg := wsMsg{seq: m.Seq, typ: m.Type, at: time.Now()}
			if m.Evaluation != nil {
				msg.score = m.Evaluation.FinalScore
			}
			w.msgs = append(w.msgs, msg)
			w.mu.Unlock()
		}
		hbStop()
		conn.CloseNow()
		w.mu.Lock()
		w.disconnects = append(w.disconnects, time.Now())
		w.mu.Unlock()
		sleepCtx(ctx, 500*time.Millisecond)
	}
}

// ---------------------------------------------------------------- сценарии

type faultKind struct {
	kind string
	dur  time.Duration
}

func parseScenario(s string) (faultKind, error) {
	k, d, _ := strings.Cut(s, ":")
	f := faultKind{kind: k}
	if d != "" {
		var err error
		if f.dur, err = time.ParseDuration(d); err != nil {
			return f, err
		}
	}
	switch k {
	case "proxy", "proxyb", "blackhole", "pause", "netdisconnect", "pgrestart", "pgkill":
		if f.dur <= 0 {
			return f, fmt.Errorf("%s: нужна длительность (%s:20s)", s, k)
		}
	case "restart":
	default:
		return f, fmt.Errorf("неизвестный сценарий %q", s)
	}
	return f, nil
}

func (f faultKind) name() string {
	if f.dur > 0 {
		return fmt.Sprintf("%s:%s", f.kind, f.dur)
	}
	return f.kind
}

func (f faultKind) describe() string {
	switch f.kind {
	case "proxy":
		return fmt.Sprintf("Обрыв сети клиента на %s: TCP-прокси рвёт все соединения клиента (HTTP/2 и WebSocket) и отвечает "+
			"connection refused на новые; ввод продолжается, во время обрыва преподаватель трижды меняет оценку прошлой карточки "+
			"(3 сообщения в канал WebSocket, которые клиент пропускает).", f.dur)
	case "proxyb":
		return fmt.Sprintf("Обрыв сети клиента на %s, как proxy, но клиент повторяет по политике исходящей очереди SPA "+
			"(frontend outbox.ts: пауза 1, 2, 4, 8, 10 с) и не ускоряет повтор при новом вводе — верхняя оценка времени досылки у SPA.", f.dur)
	case "blackhole":
		return fmt.Sprintf("«Чёрная дыра» на %s: соединения не рвутся, но пакеты не проходят (зависшие запросы, таймаут клиента 15 с); "+
			"во время обрыва — 3 изменения оценки прошлой карточки.", f.dur)
	case "pause":
		return fmt.Sprintf("Заморозка контейнера go-core (docker pause) на %s: сервер не отвечает, соединения висят.", f.dur)
	case "netdisconnect":
		return fmt.Sprintf("Отключение контейнера go-core от сети compose (docker network disconnect) на %s: недоступны клиент, "+
			"PostgreSQL и ai-service; затем docker network connect.", f.dur)
	case "restart":
		return "Перезапуск go-core (docker restart, graceful shutdown): сессии, очередь AI и события должны пережить рестарт."
	case "pgrestart":
		return fmt.Sprintf("Остановка PostgreSQL (docker stop, штатное завершение) на %s и запуск: go-core остаётся жив, "+
			"запросы к БД падают, затем пул восстанавливается.", f.dur)
	case "pgkill":
		return fmt.Sprintf("Аварийное падение PostgreSQL (docker kill -s KILL) на %s и запуск (восстановление по WAL).", f.dur)
	}
	return ""
}

type outageRun struct {
	cfg     outageConfig
	admin   *Client
	teacher *Client
	student user
	scen    []string
}

func runOutage(ctx context.Context, cfg outageConfig) (map[string]any, []OutageResult, error) {
	st := cfg.Stand
	or := &outageRun{cfg: cfg}
	or.admin = newClient(st.Base, "admin", nil, cfg.Timeout)
	if err := or.admin.Login(ctx, cfg.AdminLogin, cfg.AdminPass); err != nil {
		return nil, nil, err
	}
	stud, err := ensureUsers(ctx, or.admin, cfg.Prefix+"_out", "student", "s", 1, cfg.Password)
	if err != nil {
		return nil, nil, err
	}
	teach, err := ensureUsers(ctx, or.admin, cfg.Prefix+"_out", "teacher", "t", 1, cfg.Password)
	if err != nil {
		return nil, nil, err
	}
	or.student = stud[0]
	or.teacher = newClient(st.Base, teach[0].Login, nil, cfg.Timeout)
	if err := or.teacher.Login(ctx, teach[0].Login, cfg.Password); err != nil {
		return nil, nil, err
	}
	finishStale(ctx, []*Client{or.teacher}, "LT ")
	if or.scen, err = scenarioPool(ctx, or.teacher, 2); err != nil {
		return nil, nil, err
	}
	var health map[string]any
	_ = or.admin.Call(ctx, http.MethodGet, "/admin/health", "", nil).JSON(&health)
	meta := map[string]any{"bench": benchInfo(ctx, st, health), "config": map[string]any{
		"base": st.Base, "project": st.Project, "scenarios": cfg.Scenarios, "requestTimeout": cfg.Timeout.String(),
		"recoveryLimit": cfg.Recovery.String()}}
	var results []OutageResult
	for _, s := range cfg.Scenarios {
		f, err := parseScenario(s)
		if err != nil {
			return meta, results, err
		}
		log.Printf("сценарий %s: %s", f.name(), f.describe())
		res, err := or.run(ctx, f)
		if err != nil {
			return meta, results, fmt.Errorf("%s: %w", f.name(), err)
		}
		log.Printf("сценарий %s: восстановление %.2f с (первый успешный %.2f, события %.2f, черновик %.2f, WS %.2f/%.2f), "+
			"exactly-once=%v, черновик=%v, submit=%v, оценка=%v → pass=%v %v",
			res.Name, res.RecoverySec, res.FirstOKSec, res.EventsFlushSec, res.DraftFlushSec, res.WSReconnectSec, res.WSCatchupSec,
			res.ExactlyOnce, res.DraftMatches, res.SubmitOK, res.EvaluationDone, res.Pass, res.Notes)
		results = append(results, res)
		// между сценариями — стенд должен быть готов
		waitReady(ctx, st.Ops, 120*time.Second)
	}
	return meta, results, nil
}

func waitReady(ctx context.Context, ops string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if readyz(ctx, ops) {
			return true
		}
		sleepCtx(ctx, 200*time.Millisecond)
	}
	return false
}

func secSince(t0, t time.Time) float64 {
	if t.IsZero() {
		return -1
	}
	return math.Round(t.Sub(t0).Seconds()*100) / 100
}

func (or *outageRun) override(ctx context.Context, attemptID string, score float64) Resp {
	return or.teacher.Call(ctx, http.MethodPost, "/attempts/"+attemptID+"/evaluation/override", "",
		map[string]any{"score": score, "reason": fmt.Sprintf("Проверка устойчивости: %.0f", score)})
}

func waitEvalDone(ctx context.Context, c *Client, attemptID string, timeout time.Duration) (map[string]any, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r := c.Call(ctx, http.MethodGet, "/attempts/"+attemptID+"/evaluation", "", nil, http.StatusNotFound)
		var ev map[string]any
		if r.OK() && r.JSON(&ev) == nil && ev["status"] == "done" {
			return ev, true
		}
		if !sleepCtx(ctx, time.Second) {
			break
		}
	}
	return nil, false
}

func (or *outageRun) run(ctx context.Context, f faultKind) (OutageResult, error) {
	st := or.cfg.Stand
	res := OutageResult{Name: f.name(), Kind: f.kind, Description: f.describe(),
		FirstOKSec: -1, EventsFlushSec: -1, DraftFlushSec: -1, WSReconnectSec: -1, WSCatchupSec: -1, ReadyzOKSec: -1}
	note := func(format string, a ...any) { res.Notes = append(res.Notes, fmt.Sprintf(format, a...)) }

	u, err := url.Parse(st.Base)
	if err != nil {
		return res, err
	}
	proxy, err := newProxy(u.Host)
	if err != nil {
		return res, err
	}
	defer proxy.Close()
	_, port, _ := strings.Cut(proxy.addr, ":")
	spaBase := u.Scheme + "://" + u.Hostname() + ":" + port

	// Занятие на 2 карточки: W — сдана и оценена (канал WebSocket для сообщений во время сбоя),
	// X — в работе (ввод, outbox событий, автосохранение).
	l, err := createLesson(ctx, or.teacher, "LT Сбой "+f.name()+" "+time.Now().Format("15:04:05"), or.scen, []user{or.student}, 2, 900)
	if err != nil {
		return res, err
	}
	defer finishLesson(context.Background(), or.teacher, l.ID)
	sc := newClient(spaBase, or.student.Login, nil, or.cfg.Timeout)
	if err := sc.Login(ctx, or.student.Login, or.cfg.Password); err != nil {
		return res, err
	}
	w, r := currentAttempt(ctx, sc, l.ID)
	if w == nil {
		return res, fmt.Errorf("нет первой попытки: %s", r)
	}
	if r := sc.Call(ctx, http.MethodPost, "/attempts/"+w.ID+"/accept-call", "", nil); !r.OK() {
		return res, fmt.Errorf("accept W: %s", r)
	}
	var d0 map[string]any
	_ = sc.Call(ctx, http.MethodGet, "/attempts/"+w.ID+"/draft", "", nil).JSON(&d0)
	mutateDraft(d0, 3, "")
	if r := sc.Call(ctx, http.MethodPost, "/attempts/"+w.ID+"/submit", "", map[string]any{"card": d0}); !r.OK() {
		return res, fmt.Errorf("submit W: %s", r)
	}
	if _, ok := waitEvalDone(ctx, sc, w.ID, or.cfg.EvalWait); !ok {
		return res, fmt.Errorf("оценка W не завершилась за %s", or.cfg.EvalWait)
	}
	x, r := currentAttempt(ctx, sc, l.ID)
	if x == nil {
		return res, fmt.Errorf("нет второй попытки: %s", r)
	}
	if r := sc.Call(ctx, http.MethodPost, "/attempts/"+x.ID+"/accept-call", "", nil); !r.OK() {
		return res, fmt.Errorf("accept X: %s", r)
	}
	spa := &spaClient{c: sc, attempt: x.ID, producing: true, backoff: f.kind == "proxyb"}
	if err := sc.Call(ctx, http.MethodGet, "/attempts/"+x.ID+"/draft", "", nil).JSON(&spa.draft); err != nil {
		return res, err
	}

	lctx, stopLoops := context.WithCancel(ctx)
	defer stopLoops()
	wsW := &wsTrack{name: "W", attempt: w.ID}
	wsX := &wsTrack{name: "X", attempt: x.ID}
	var wg sync.WaitGroup
	for _, fn := range []func(context.Context){spa.producer, spa.sender, spa.saver,
		func(c context.Context) { spa.wsLoop(c, wsW) }, func(c context.Context) { spa.wsLoop(c, wsX) }} {
		wg.Add(1)
		go func() { defer wg.Done(); fn(lctx) }()
	}
	// readyz — отдельно, каждые 200 мс
	var readyMu sync.Mutex
	type readyPoint struct {
		at time.Time
		ok bool
	}
	var ready []readyPoint
	wg.Add(1)
	go func() {
		defer wg.Done()
		for lctx.Err() == nil {
			ok := readyz(lctx, st.Ops)
			readyMu.Lock()
			ready = append(ready, readyPoint{time.Now(), ok})
			readyMu.Unlock()
			sleepCtx(lctx, 200*time.Millisecond)
		}
	}()

	// Разогрев: канал W получает сообщение (клиент узнаёт текущий seq — иначе since не с чего взять).
	sleepCtx(ctx, 1500*time.Millisecond)
	if r := or.override(ctx, w.ID, 50); !r.OK() {
		return res, fmt.Errorf("override W: %s", r)
	}
	waitUntil(ctx, 5*time.Second, func() bool { wsW.mu.Lock(); defer wsW.mu.Unlock(); return w != nil && len(wsW.msgs) > 0 })
	sleepCtx(ctx, 2*time.Second)
	wsW.mu.Lock()
	msgsBefore := len(wsW.msgs)
	if msgsBefore == 0 {
		note("канал W не получил сообщение до сбоя — проверка since неполная")
	}
	wsW.mu.Unlock()

	// ------------------------------------------------ сбой
	gc, _ := st.container(ctx, "go-core")
	pg, _ := st.container(ctx, "postgres")
	var netName string
	var overridesSent atomic.Int64
	var overrideWG sync.WaitGroup
	tc := time.Now()
	switch f.kind {
	case "proxy", "proxyb":
		proxy.Cut("refuse")
	case "blackhole":
		proxy.Cut("blackhole")
	case "pause":
		_, err = docker(ctx, "pause", gc)
	case "netdisconnect":
		var nets string
		if nets, err = docker(ctx, "inspect", "--format", "{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}", gc); err == nil {
			if fs := strings.Fields(nets); len(fs) > 0 {
				netName = fs[0]
				_, err = docker(ctx, "network", "disconnect", netName, gc)
				// страховка: если прогон прервётся, контейнер вернётся в сеть
				defer func() { _, _ = docker(context.Background(), "network", "connect", "--alias", "go-core", netName, gc) }()
			}
		}
	case "pgrestart":
		_, err = docker(ctx, "stop", "-t", "10", pg)
	case "pgkill":
		_, err = docker(ctx, "kill", "-s", "KILL", pg)
	case "restart":
		_, err = docker(ctx, "restart", "-t", "15", gc)
	}
	if err != nil {
		return res, fmt.Errorf("внести сбой: %w", err)
	}
	if f.kind == "proxy" || f.kind == "proxyb" || f.kind == "blackhole" {
		// Во время обрыва сервер жив: преподаватель трижды меняет оценку W — клиент их пропускает.
		for i, at := range []time.Duration{time.Second, f.dur / 2, f.dur - time.Second} {
			overrideWG.Add(1)
			go func() {
				defer overrideWG.Done()
				sleepCtx(ctx, time.Until(tc.Add(at)))
				if r := or.override(ctx, w.ID, float64(61+11*i)); r.OK() {
					overridesSent.Add(1)
				}
			}()
		}
	}
	if f.dur > 0 {
		sleepCtx(ctx, time.Until(tc.Add(f.dur)))
	}
	switch {
	case f.kind == "proxy" || f.kind == "proxyb" || f.kind == "blackhole":
		err = proxy.Restore()
	case f.kind == "pause":
		_, err = docker(ctx, "unpause", gc)
	case f.kind == "netdisconnect":
		_, err = docker(ctx, "network", "connect", "--alias", "go-core", netName, gc)
	case f.kind == "pgrestart" || f.kind == "pgkill":
		_, err = docker(ctx, "start", pg)
	}
	tr := time.Now()
	if err != nil {
		return res, fmt.Errorf("снять сбой: %w", err)
	}
	overrideWG.Wait()
	res.OutageSec = math.Round(tr.Sub(tc).Seconds()*100) / 100
	spa.mu.Lock()
	seqAtRestore, verAtRestore := spa.seq, spa.draftVer
	res.BacklogAtRestore = len(spa.outbox)
	spa.mu.Unlock()
	res.WSMissed = int(overridesSent.Load())

	// ------------------------------------------------ восстановление
	var firstOK, evFlush, drFlush, wsDisc, wsRe, wsCatch time.Time
	waitUntil(ctx, 90*time.Second, func() bool {
		spa.mu.Lock()
		if firstOK.IsZero() {
			for _, q := range spa.reqs {
				if q.ok && q.end.After(tr) && (firstOK.IsZero() || q.end.Before(firstOK)) {
					firstOK = q.end
				}
			}
		}
		if evFlush.IsZero() && spa.ackedMax >= seqAtRestore {
			evFlush = time.Now()
		}
		if drFlush.IsZero() && spa.ackVer >= verAtRestore {
			drFlush = time.Now()
		}
		spa.mu.Unlock()
		wsW.mu.Lock()
		if wsDisc.IsZero() {
			for _, d := range wsW.disconnects {
				if d.After(tc) {
					wsDisc = d
					break
				}
			}
		}
		if !wsDisc.IsZero() && wsRe.IsZero() {
			for _, c := range wsW.connects {
				if c.After(wsDisc) {
					wsRe = c
					break
				}
			}
		}
		if wsCatch.IsZero() && res.WSMissed > 0 && len(wsW.msgs)-msgsBefore >= res.WSMissed {
			wsCatch = wsW.msgs[msgsBefore+res.WSMissed-1].at
		}
		wsW.mu.Unlock()
		wsDone := wsDisc.IsZero() || !wsRe.IsZero() // соединение не рвалось или уже восстановлено
		return !firstOK.IsZero() && !evFlush.IsZero() && !drFlush.IsZero() && wsDone && (res.WSMissed == 0 || !wsCatch.IsZero())
	})
	res.FirstOKSec, res.EventsFlushSec, res.DraftFlushSec = secSince(tr, firstOK), secSince(tr, evFlush), secSince(tr, drFlush)
	res.WSReconnectSec, res.WSCatchupSec = secSince(tr, wsRe), secSince(tr, wsCatch)
	if !wsRe.IsZero() && wsRe.Before(tr) {
		res.WSReconnectSec = 0
	}
	if !wsCatch.IsZero() && wsCatch.Before(tr) {
		res.WSCatchupSec = 0
	}
	if wsDisc.IsZero() {
		note("WebSocket не обрывался (соединение пережило сбой)")
		res.WSReconnectSec = 0
	}
	readyMu.Lock()
	var downStart time.Time
	var downTotal time.Duration
	for i, p := range ready {
		if i == 0 || p.ok != ready[i-1].ok {
			res.ReadyzTimeline = append(res.ReadyzTimeline, fmt.Sprintf("%+.2f %s", p.at.Sub(tc).Seconds(), map[bool]string{true: "ok", false: "fail"}[p.ok]))
		}
		if !p.ok && downStart.IsZero() {
			downStart = p.at
		}
		if p.ok && !downStart.IsZero() {
			downTotal += p.at.Sub(downStart)
			downStart = time.Time{}
		}
		if p.ok && p.at.After(tr) && res.ReadyzOKSec < 0 {
			res.ReadyzOKSec = secSince(tr, p.at)
		}
	}
	readyMu.Unlock()
	res.ReadyzDownSec = math.Round(downTotal.Seconds()*100) / 100

	// Живой поток после восстановления: ещё одна правка оценки должна прийти по WebSocket.
	if r := or.override(ctx, w.ID, 94); r.OK() {
		res.WSLiveAfter = waitUntil(ctx, 10*time.Second, func() bool {
			wsW.mu.Lock()
			defer wsW.mu.Unlock()
			return len(wsW.msgs) > 0 && wsW.msgs[len(wsW.msgs)-1].score == 94
		})
	} else {
		note("override после восстановления: %s", r)
	}

	// Ещё 2 с ввода, затем остановка ввода и досылка всего.
	sleepCtx(ctx, 2*time.Second)
	spa.mu.Lock()
	spa.producing = false
	spa.mu.Unlock()
	drained := waitUntil(ctx, 30*time.Second, func() bool {
		spa.mu.Lock()
		defer spa.mu.Unlock()
		return len(spa.outbox) == 0 && spa.ackVer == spa.draftVer
	})
	if !drained {
		note("outbox/черновик не досланы за 30 с после остановки ввода")
	}
	stopLoops()
	wg.Wait()

	spa.mu.Lock()
	res.EventsProduced, res.DraftVersions = spa.seq, spa.draftVer
	res.EventsResent = spa.sent - spa.accepted
	for _, q := range spa.reqs {
		if !q.ok {
			res.FailedRequests++
		}
	}
	finalDraft := spa.draft
	spa.mu.Unlock()
	wsW.mu.Lock()
	res.WSGaps = wsW.gaps
	res.WSHeartbeatFails = wsW.heartbeatFails
	res.WSReconnects = max(0, len(wsW.connects)-1)
	res.WSReplayed = max(0, len(wsW.msgs)-msgsBefore-1) // без сообщения «после восстановления»
	wsW.mu.Unlock()

	// ------------------------------------------------ проверка целостности
	vc := newClient(st.Base, "verify", nil, or.cfg.Timeout)
	if err := vc.Login(ctx, or.student.Login, or.cfg.Password); err != nil {
		return res, err
	}
	var evs []struct {
		ClientSeq int64 `json:"clientSeq"`
	}
	if r := vc.Call(ctx, http.MethodGet, "/attempts/"+x.ID+"/events", "", nil); r.JSON(&evs) == nil {
		seen := map[int64]bool{}
		res.APIEvents[2] = math.MaxInt64
		for _, e := range evs {
			if e.ClientSeq < 1 {
				continue
			}
			res.APIEvents[0]++
			seen[e.ClientSeq] = true
			res.APIEvents[2] = min(res.APIEvents[2], e.ClientSeq)
			res.APIEvents[3] = max(res.APIEvents[3], e.ClientSeq)
		}
		res.APIEvents[1] = int64(len(seen))
		if res.APIEvents[0] == 0 {
			res.APIEvents[2] = 0
		}
	}
	if v, err := st.psqlInts(ctx, fmt.Sprintf(`select count(*), count(distinct client_seq), coalesce(min(client_seq),0), coalesce(max(client_seq),0)
  from attempt_events where attempt_id = '%s' and client_seq >= 1`, x.ID)); err == nil && len(v) == 4 {
		copy(res.DBEvents[:], v)
	} else {
		note("psql: %v", err)
	}
	n := int64(res.EventsProduced)
	res.ExactlyOnce = res.DBEvents == [4]int64{n, n, 1, n} && res.APIEvents == [4]int64{n, n, 1, n}
	var srvDraft map[string]any
	if r := vc.Call(ctx, http.MethodGet, "/attempts/"+x.ID+"/draft", "", nil); r.JSON(&srvDraft) == nil {
		a1, _ := json.Marshal([]any{finalDraft["description"], finalDraft["applicant"]})
		a2, _ := json.Marshal([]any{srvDraft["description"], srvDraft["applicant"]})
		res.DraftMatches = string(a1) == string(a2)
		if !res.DraftMatches {
			note("черновик на сервере %s ≠ клиент %s", a2, a1)
		}
	}
	t0 := time.Now()
	if r := vc.Call(ctx, http.MethodPost, "/attempts/"+x.ID+"/submit", "", map[string]any{"card": finalDraft}); r.OK() {
		res.SubmitOK = true
		_, res.EvaluationDone = waitEvalDone(ctx, vc, x.ID, or.cfg.EvalWait)
		res.EvaluationSec = secSince(t0, time.Now())
	} else {
		note("submit X: %s", r)
	}
	var evW map[string]any
	if r := vc.Call(ctx, http.MethodGet, "/attempts/"+w.ID+"/evaluation", "", nil); r.JSON(&evW) == nil {
		res.OverrideApplied = evW["finalScore"] == 94.0
	}

	vals := []float64{res.FirstOKSec, res.EventsFlushSec, res.DraftFlushSec, res.WSReconnectSec}
	res.RecoverySec = 0
	ok := true
	for _, v := range vals {
		if v < 0 {
			ok = false
		}
		res.RecoverySec = max(res.RecoverySec, v)
	}
	if res.WSMissed > 0 {
		if res.WSCatchupSec < 0 || res.WSReplayed < res.WSMissed {
			ok = false
			note("пропущенные сообщения WebSocket не получены: опубликовано %d, получено %d", res.WSMissed, res.WSReplayed)
		}
		res.RecoverySec = max(res.RecoverySec, res.WSCatchupSec)
	}
	if res.WSGaps > 0 {
		note("разрывы seq в канале WebSocket: %d (ожидаемо после рестарта go-core — новая «эпоха» канала)", res.WSGaps)
	}
	res.Pass = ok && res.RecoverySec <= or.cfg.Recovery.Seconds() && res.ExactlyOnce && res.DraftMatches &&
		res.SubmitOK && res.EvaluationDone && res.WSLiveAfter
	return res, nil
}

func waitUntil(ctx context.Context, timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		if !sleepCtx(ctx, 50*time.Millisecond) {
			return false
		}
	}
	return cond()
}
