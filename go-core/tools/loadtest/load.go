package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// loadConfig — параметры нагрузочного прогона.
type loadConfig struct {
	Stand      Stand
	AdminLogin string
	AdminPass  string
	Prefix     string
	Password   string
	Students   int
	Teachers   int
	Admins     int
	Window     time.Duration
	Think      float64 // множитель пауз «на подумать» (1 — реалистично, 0.2 — стресс)
	Groups     []string
	Timeout    time.Duration
	EvalWait   time.Duration
	DrainWait  time.Duration
}

// GroupResult — итог одной группы (сериализуется в JSON и в Markdown).
type GroupResult struct {
	ID          string           `json:"id"`
	Title       string           `json:"title"`
	Description string           `json:"description"`
	VUs         map[string]int   `json:"vus"`
	StartedAt   time.Time        `json:"startedAt"`
	WindowSec   float64          `json:"windowSec"`
	Endpoints   []EPSummary      `json:"endpoints"`
	Total       EPSummary        `json:"total"`
	Timeline    []SecBucket      `json:"timeline"`
	Containers  []ContainerUsage `json:"containers"`
	DB          DBWrites         `json:"db"`
	Counters    map[string]int64 `json:"counters"`
	Extra       map[string]any   `json:"extra,omitempty"`
	Pass        bool             `json:"pass"`
	Verdict     string           `json:"verdict"`
}

// DBWrites — скорость записи в БД за окно.
type DBWrites struct {
	WriteRequestsOK   int     `json:"writeRequestsOk"` // 2xx на POST/PUT/DELETE в окне (каждый — ≥1 запись в БД)
	WriteRequestsRate float64 `json:"writeRequestsPerSec"`
	EventRows         int64   `json:"eventRows"`       // строк attempt_events, записанных за окно (по id, точно)
	ClientEventRows   int64   `json:"clientEventRows"` // из них клиентских (client_seq ≥ 1)
	EventRowsRate     float64 `json:"eventRowsPerSec"`
	DraftUpserts      int64   `json:"draftUpserts"` // подтверждённые PUT /draft (upsert attempt_drafts)
	DraftUpsertsRate  float64 `json:"draftUpsertsPerSec"`
	TuplesWritten     int64   `json:"tuplesWritten"` // pg_stat_user_tables ins+upd+del, окно + 11 с досчёта статистики
	XactCommits       int64   `json:"xactCommits"`   // pg_stat_database.xact_commit, так же
	Note              string  `json:"note,omitempty"`
}

type dbSnap struct {
	maxEvent, tuples, xact int64
}

func (s Stand) dbSnapshot(ctx context.Context) (dbSnap, error) {
	v, err := s.psqlInts(ctx, `select (select coalesce(max(id),0) from attempt_events),
 (select coalesce(sum(n_tup_ins+n_tup_upd+n_tup_del),0) from pg_stat_user_tables),
 (select xact_commit from pg_stat_database where datname=current_database())`)
	if err != nil || len(v) != 3 {
		return dbSnap{}, fmt.Errorf("db snapshot: %v %v", v, err)
	}
	return dbSnap{maxEvent: v[0], tuples: v[1], xact: v[2]}, nil
}

// ---------------------------------------------------------------- студент

type counters struct {
	accepts, drafts, eventBatches, eventsAccepted, submits, services, evalPolls atomic.Int64
	noAttempt                                                                   atomic.Int64
}

func (c *counters) snapshot() map[string]int64 {
	return map[string]int64{
		"acceptCalls": c.accepts.Load(), "draftsSaved": c.drafts.Load(), "eventBatches": c.eventBatches.Load(),
		"eventsAccepted": c.eventsAccepted.Load(), "submits": c.submits.Load(), "servicesAdded": c.services.Load(),
		"evaluationPolls": c.evalPolls.Load(), "noAttemptWaits": c.noAttempt.Load(),
	}
}

type studentVU struct {
	c        *Client
	lessonID string
	rng      *rand.Rand
	fireType string
	think    float64
	cnt      *counters
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (v *studentVU) pause(lo, hi float64) time.Duration {
	return time.Duration((lo + v.rng.Float64()*(hi-lo)) * v.think * float64(time.Second))
}

var phrases = []string{"Горит квартира на пятом этаже,", "из окна идёт густой дым,", "внутри может находиться пожилой мужчина,",
	"соседи эвакуируются по лестнице,", "подъезд номер два, код домофона 25К,", "пострадавших пока не видно,",
	"заявитель находится во дворе,", "запах гари чувствуется на всех этажах,"}

// mutateDraft — правка карточки «как оператор»: описание растёт, заявитель, адрес, тип происшествия.
func mutateDraft(d map[string]any, iter int, fireType string) {
	desc := ""
	for i := 0; i <= iter && i < len(phrases); i++ {
		desc += phrases[i] + " "
	}
	d["description"] = strings.TrimSpace(desc) + fmt.Sprintf(" (правка %d)", iter)
	d["applicant"] = map[string]any{"name": "Мария Иванова", "status": "очевидец"}
	if fireType != "" {
		d["incidentTypeIds"] = []string{fireType}
	}
	if addr, ok := d["address"].(map[string]any); ok {
		addr["raw"] = "Москва, Тверская улица, 12"
		addr["street"] = "Тверская"
		addr["house"] = "12"
	}
}

func fieldEvents(seq *int, n int) []map[string]any {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	evs := make([]map[string]any, n)
	fields := []string{"description", "applicant.name", "address.raw", "incidentTypeIds"}
	for i := range n {
		*seq++
		evs[i] = map[string]any{"clientSeq": *seq, "type": "field_changed", "at": now,
			"payload": map[string]any{"field": fields[i%len(fields)]}}
	}
	return evs
}

// run — цикл обучающегося: взять текущую карточку, принять вызов, заполнять (автосохранение +
// журнал событий + обновление попытки), через cardDur сдать и взять следующую.
func (v *studentVU) run(ctx context.Context, cardDur func() time.Duration) {
	for ctx.Err() == nil {
		a, _ := currentAttempt(ctx, v.c, v.lessonID)
		if a == nil {
			v.cnt.noAttempt.Add(1)
			if !sleepCtx(ctx, 3*time.Second) {
				return
			}
			continue
		}
		v.work(ctx, a.ID, cardDur())
	}
}

func (v *studentVU) work(ctx context.Context, id string, dur time.Duration) {
	base := "/attempts/" + id
	v.c.Call(ctx, http.MethodGet, base+"/call-script", "GET /attempts/{id}/call-script", nil)
	if !sleepCtx(ctx, v.pause(0.5, 1.5)) {
		return
	}
	r := v.c.Call(ctx, http.MethodPost, base+"/accept-call", "POST /attempts/{id}/accept-call", nil)
	if !r.OK() {
		sleepCtx(ctx, time.Second)
		return
	}
	v.cnt.accepts.Add(1)
	var draft map[string]any
	r = v.c.Call(ctx, http.MethodGet, base+"/draft", "GET /attempts/{id}/draft", nil)
	if r.JSON(&draft) != nil || !r.OK() {
		return
	}
	seq := 0
	deadline := time.Now().Add(dur)
	for iter := 1; time.Now().Before(deadline); iter++ {
		if !sleepCtx(ctx, v.pause(1.0, 2.0)) {
			return
		}
		mutateDraft(draft, iter, v.fireType)
		if r := v.c.Call(ctx, http.MethodPut, base+"/draft", "PUT /attempts/{id}/draft", draft); r.OK() {
			v.cnt.drafts.Add(1)
		}
		v.postEvents(ctx, base, fieldEvents(&seq, 1+v.rng.IntN(4)))
		switch {
		case iter == 2:
			if r := v.c.Call(ctx, http.MethodPost, base+"/services", "POST /attempts/{id}/services",
				map[string]string{"serviceCode": "101"}, http.StatusConflict); r.OK() {
				v.cnt.services.Add(1)
			}
		case iter%4 == 0:
			v.c.Call(ctx, http.MethodGet, base, "GET /attempts/{id}", nil)
		}
	}
	if ctx.Err() != nil {
		return
	}
	// Сдача как в SPA: досылка событий, сохранение черновика, submit, экран результата.
	v.postEvents(ctx, base, fieldEvents(&seq, 1))
	v.c.Call(ctx, http.MethodPut, base+"/draft", "PUT /attempts/{id}/draft", draft)
	r = v.c.Call(ctx, http.MethodPost, base+"/submit", "POST /attempts/{id}/submit", map[string]any{"card": draft})
	if r.OK() {
		v.cnt.submits.Add(1)
	}
	if !sleepCtx(ctx, v.pause(0.5, 1.0)) {
		return
	}
	v.c.Call(ctx, http.MethodGet, base+"/evaluation", "GET /attempts/{id}/evaluation", nil)
	v.cnt.evalPolls.Add(1)
	sleepCtx(ctx, v.pause(1.0, 2.0))
}

func (v *studentVU) postEvents(ctx context.Context, base string, evs []map[string]any) {
	r := v.c.Call(ctx, http.MethodPost, base+"/events", "POST /attempts/{id}/events", map[string]any{"events": evs})
	var res struct{ Accepted int }
	if r.OK() && r.JSON(&res) == nil {
		v.cnt.eventBatches.Add(1)
		v.cnt.eventsAccepted.Add(int64(res.Accepted))
	}
}

// ---------------------------------------------------------------- преподаватель, администратор

type wsCounters struct {
	connects, messages, drops atomic.Int64
}

// monitorWS держит /ws/lessons/{id}/monitor, переподключаясь с since (как положено клиенту).
func monitorWS(ctx context.Context, c *Client, lessonID string, ws *wsCounters) {
	var since int64 = -1
	for ctx.Err() == nil {
		p := "/ws/lessons/" + lessonID + "/monitor"
		if since >= 0 {
			p += fmt.Sprintf("?since=%d", since)
		}
		conn, err := c.DialWS(ctx, p, "WS /ws/lessons/{id}/monitor (connect)")
		if err != nil {
			if !sleepCtx(ctx, time.Second) {
				return
			}
			continue
		}
		ws.connects.Add(1)
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				if ctx.Err() == nil {
					ws.drops.Add(1)
				}
				break
			}
			ws.messages.Add(1)
			var m struct{ Seq int64 }
			if json.Unmarshal(data, &m) == nil {
				since = m.Seq
			}
		}
		conn.CloseNow()
	}
}

type lessonAttempt struct {
	ID     string `json:"id"`
	UserID string `json:"userId"`
	Status string `json:"status"`
}

func teacherLoop(ctx context.Context, c *Client, lessonID string, rng *rand.Rand, think float64, scenarios []string, ws *wsCounters) {
	go monitorWS(ctx, c, lessonID, ws)
	pause := func(lo, hi float64) time.Duration {
		return time.Duration((lo + rng.Float64()*(hi-lo)) * think * float64(time.Second))
	}
	if !sleepCtx(ctx, pause(0, 1)) {
		return
	}
	for ctx.Err() == nil {
		// Экран занятия обновляется раз в 3 с (LessonDetailPage): занятие + попытки.
		c.Call(ctx, http.MethodGet, "/lessons/"+lessonID, "GET /lessons/{id}", nil)
		r := c.Call(ctx, http.MethodGet, "/lessons/"+lessonID+"/attempts", "GET /lessons/{id}/attempts", nil)
		var atts []lessonAttempt
		_ = r.JSON(&atts)
		var done []lessonAttempt
		for _, a := range atts {
			if a.Status == "evaluating" || a.Status == "evaluated" {
				done = append(done, a)
			}
		}
		x := rng.Float64()
		switch {
		case x < 0.30 && len(done) > 0:
			a := done[rng.IntN(len(done))]
			c.Call(ctx, http.MethodGet, "/attempts/"+a.ID+"/evaluation", "GET /attempts/{id}/evaluation", nil)
		case x < 0.50 && len(atts) > 0:
			a := atts[rng.IntN(len(atts))]
			c.Call(ctx, http.MethodGet, "/attempts/"+a.ID+"/events", "GET /attempts/{id}/events", nil)
		case x < 0.65:
			c.Call(ctx, http.MethodGet, "/scenarios?limit=100", "GET /scenarios", nil)
			c.Call(ctx, http.MethodGet, "/scenarios/"+scenarios[rng.IntN(len(scenarios))], "GET /scenarios/{id}", nil)
		case x < 0.75:
			c.Call(ctx, http.MethodGet, "/lessons?limit=100", "GET /lessons", nil)
		case x < 0.85 && len(atts) > 0:
			a := atts[rng.IntN(len(atts))]
			c.Call(ctx, http.MethodGet, "/users/"+a.UserID+"/progress", "GET /users/{id}/progress", nil)
		case x < 0.90:
			c.Call(ctx, http.MethodGet, "/lessons/"+lessonID+"/report?format=csv", "GET /lessons/{id}/report?format=csv", nil)
		}
		if !sleepCtx(ctx, pause(2.5, 3.5)) {
			return
		}
	}
}

func adminLoop(ctx context.Context, c *Client, rng *rand.Rand, think float64) {
	calls := []struct{ path, label string }{
		{"/admin/health", "GET /admin/health"},
		{"/users?limit=500", "GET /users"},
		{"/admin/audit?limit=100", "GET /admin/audit"},
		{"/admin/settings", "GET /admin/settings"},
		{"/lessons?limit=100", "GET /lessons"},
	}
	i := rng.IntN(len(calls))
	for ctx.Err() == nil {
		if !sleepCtx(ctx, time.Duration((2+rng.Float64()*2)*think*float64(time.Second))) {
			return
		}
		x := calls[i%len(calls)]
		c.Call(ctx, http.MethodGet, x.path, x.label, nil)
		i++
	}
}

// ---------------------------------------------------------------- прогон

type loadRun struct {
	cfg       loadConfig
	admin     *Client
	students  []user
	teachers  []user
	admins    []user
	sc        []*Client // клиенты студентов
	tc        []*Client
	ac        []*Client
	scenarios []string
	fireType  string
}

func runLoad(ctx context.Context, cfg loadConfig) (map[string]any, []GroupResult, error) {
	lr := &loadRun{cfg: cfg}
	st := cfg.Stand
	log.Printf("подготовка: администратор %s, учётки %s_* (%d студентов, %d преподавателей, %d админов)",
		cfg.AdminLogin, cfg.Prefix, cfg.Students, cfg.Teachers, cfg.Admins)
	lr.admin = newClient(st.Base, "admin", nil, cfg.Timeout)
	if err := lr.admin.Login(ctx, cfg.AdminLogin, cfg.AdminPass); err != nil {
		return nil, nil, err
	}
	var err error
	if lr.students, err = ensureUsers(ctx, lr.admin, cfg.Prefix, "student", "s", cfg.Students, cfg.Password); err != nil {
		return nil, nil, err
	}
	if lr.teachers, err = ensureUsers(ctx, lr.admin, cfg.Prefix, "teacher", "t", cfg.Teachers, cfg.Password); err != nil {
		return nil, nil, err
	}
	if lr.admins, err = ensureUsers(ctx, lr.admin, cfg.Prefix, "admin", "a", cfg.Admins, cfg.Password); err != nil {
		return nil, nil, err
	}
	t0 := time.Now()
	all := append(append(append([]user{}, lr.students...), lr.teachers...), lr.admins...)
	clients, err := loginAll(ctx, st.Base, all, cfg.Password, cfg.Timeout)
	if err != nil {
		return nil, nil, err
	}
	lr.sc = clients[:len(lr.students)]
	lr.tc = clients[len(lr.students) : len(lr.students)+len(lr.teachers)]
	lr.ac = clients[len(lr.students)+len(lr.teachers):]
	log.Printf("вход %d пользователей: %s", len(all), time.Since(t0).Round(time.Millisecond))

	finishStale(ctx, lr.tc, "LT ")
	if lr.scenarios, err = scenarioPool(ctx, lr.tc[0], 5); err != nil {
		return nil, nil, err
	}
	var types []struct{ ID string }
	if r := lr.tc[0].Call(ctx, http.MethodGet, "/classifier/types/search?q=%D0%BF%D0%BE%D0%B6%D0%B0%D1%80", "", nil); r.JSON(&types) == nil && len(types) > 0 {
		lr.fireType = types[0].ID
	}
	var health map[string]any
	_ = lr.admin.Call(ctx, http.MethodGet, "/admin/health", "", nil).JSON(&health)
	meta := map[string]any{"bench": benchInfo(ctx, st, health), "config": map[string]any{
		"base": st.Base, "project": st.Project, "window": cfg.Window.String(), "think": cfg.Think,
		"students": cfg.Students, "teachers": cfg.Teachers, "admins": cfg.Admins, "scenarios": lr.scenarios,
		"requestTimeout": cfg.Timeout.String()}}

	var results []GroupResult
	for i, g := range cfg.Groups {
		if i > 0 {
			d, ok := waitAIDrain(ctx, st.Ops, cfg.DrainWait)
			log.Printf("очередь оценок перед группой %s: %s (drained=%v)", g, d.Round(time.Second), ok)
		}
		var res GroupResult
		switch g {
		case "1":
			res, err = lr.group1(ctx)
		case "2":
			res, err = lr.group2(ctx)
		case "3":
			res, err = lr.group3(ctx)
		default:
			err = fmt.Errorf("неизвестная группа %q", g)
		}
		if err != nil {
			return meta, results, fmt.Errorf("группа %s: %w", g, err)
		}
		log.Printf("группа %s: %d запросов, p95 %.0f мс, p99 %.0f мс, max %.0f мс, ошибок %d — %s",
			g, res.Total.Count, res.Total.P95, res.Total.P99, res.Total.Max, res.Total.Errors, res.Verdict)
		results = append(results, res)
	}
	return meta, results, nil
}

// measure — общий каркас группы: снимки БД, docker stats, окно измерения, сводка.
func (lr *loadRun) measure(ctx context.Context, res *GroupResult, rec *Recorder, body func(wctx context.Context, from, to time.Time)) {
	st := lr.cfg.Stand
	before, errB := st.dbSnapshot(ctx)
	sampler := startStats(st)
	from := time.Now().Add(200 * time.Millisecond)
	to := from.Add(lr.cfg.Window)
	rec.SetWindow(from, to)
	res.StartedAt = from
	res.WindowSec = lr.cfg.Window.Seconds()
	wctx, cancel := context.WithDeadline(ctx, to)
	body(wctx, from, to)
	cancel()
	// Окно всегда полное: фоновые действия группы (опрос оценки и т. п.) идут до его конца.
	sleepCtx(ctx, time.Until(to))
	res.Containers = sampler.Stop()
	time.Sleep(1500 * time.Millisecond) // group commit событий (≤50 мс) + хвост
	res.Endpoints, res.Total, res.Timeline = rec.Summary()
	w := lr.cfg.Window.Seconds()
	for _, e := range res.Endpoints {
		if strings.HasPrefix(e.Endpoint, "POST") || strings.HasPrefix(e.Endpoint, "PUT") || strings.HasPrefix(e.Endpoint, "DELETE") {
			for k, v := range e.Statuses {
				if strings.HasPrefix(k, "2") {
					res.DB.WriteRequestsOK += v
				}
			}
		}
		if e.Endpoint == "PUT /attempts/{id}/draft" {
			res.DB.DraftUpserts = int64(e.Statuses["200"])
		}
	}
	res.DB.WriteRequestsRate = round1(float64(res.DB.WriteRequestsOK) / w)
	res.DB.DraftUpsertsRate = round1(float64(res.DB.DraftUpserts) / w)
	if errB == nil {
		if v, err := st.psqlInts(ctx, fmt.Sprintf(`select count(*), count(*) filter (where client_seq >= 1)
  from attempt_events where id > %d`, before.maxEvent)); err == nil && len(v) == 2 {
			res.DB.EventRows, res.DB.ClientEventRows = v[0], v[1]
			res.DB.EventRowsRate = round1(float64(v[0]) / w)
		}
		time.Sleep(11 * time.Second) // статистика pg_stat сбрасывается бэкендами с задержкой до 10 с
		if after, err := st.dbSnapshot(ctx); err == nil {
			res.DB.TuplesWritten = after.tuples - before.tuples
			res.DB.XactCommits = after.xact - before.xact
		}
	} else {
		res.DB.Note = errB.Error()
	}
	res.Pass = res.Total.Max <= 2000 && res.Total.Errors == 0
	res.Verdict = fmt.Sprintf("max %.0f мс, p99 %.0f мс, ошибок %d", res.Total.Max, res.Total.P99, res.Total.Errors)
	if res.Pass {
		res.Verdict = "PASS: " + res.Verdict
	} else {
		res.Verdict = "FAIL: " + res.Verdict
	}
}

func round1(x float64) float64 { return float64(int64(x*10+0.5)) / 10 }

func (lr *loadRun) group1(ctx context.Context) (GroupResult, error) {
	res := GroupResult{ID: "1", Title: "Занятие: 100 обучающихся заполняют карточки",
		Description: "Все обучающиеся в одном занятии (cardsPerStudent=50): список занятий → легенда → accept-call → " +
			"цикл «правка → PUT /draft → POST /events (1–4 события, clientSeq) → изредка GET /attempts/{id}» с паузой 1–2 с; " +
			"через 15–25 с сдача (события, черновик, submit, GET /evaluation) и следующая карточка.",
		VUs: map[string]int{"students": len(lr.sc)}}
	l, err := createLesson(ctx, lr.tc[0], "LT Группа 1 "+time.Now().Format("15:04:05"), lr.scenarios, lr.students, 50, 600)
	if err != nil {
		return res, err
	}
	defer finishLesson(context.Background(), lr.tc[0], l.ID)
	rec := NewRecorder()
	cnt := &counters{}
	lr.measure(ctx, &res, rec, func(wctx context.Context, from, _ time.Time) {
		var wg sync.WaitGroup
		for i, c := range lr.sc {
			c.Rec = rec
			v := &studentVU{c: c, lessonID: l.ID, rng: rand.New(rand.NewPCG(uint64(i), 1)), fireType: lr.fireType, think: lr.cfg.Think, cnt: cnt}
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(time.Until(from) + time.Duration(v.rng.IntN(1000))*time.Millisecond)
				v.run(wctx, func() time.Duration { return time.Duration(15+v.rng.IntN(11)) * time.Second })
			}()
		}
		wg.Wait()
	})
	res.Counters = cnt.snapshot()
	return res, nil
}

func (lr *loadRun) group2(ctx context.Context) (GroupResult, error) {
	nStud := min(80, len(lr.sc))
	nT := min(15, len(lr.tc))
	nA := min(5, len(lr.ac))
	res := GroupResult{ID: "2", Title: "Смешанная нагрузка ролей",
		Description: fmt.Sprintf("%d обучающихся заполняют карточки (как в группе 1) в %d занятиях разных преподавателей; "+
			"%d преподавателей: экран занятия с обновлением раз в 3 с (занятие + попытки) и WebSocket-монитор, "+
			"просмотр оценок, хронологии, сценариев, прогресса, CSV-отчёта; %d администраторов: здоровье контура, "+
			"пользователи, аудит, настройки, занятия (пауза 2–4 с).", nStud, nT, nT, nA),
		VUs: map[string]int{"students": nStud, "teachers": nT, "admins": nA}}
	lessonOf := make([]string, nStud)
	for t := range nT {
		var part []user
		var idx []int
		for i := t; i < nStud; i += nT {
			part = append(part, lr.students[i])
			idx = append(idx, i)
		}
		l, err := createLesson(ctx, lr.tc[t], fmt.Sprintf("LT Группа 2 / %02d %s", t+1, time.Now().Format("15:04:05")), lr.scenarios, part, 50, 600)
		if err != nil {
			return res, err
		}
		defer finishLesson(context.Background(), lr.tc[t], l.ID)
		for _, i := range idx {
			lessonOf[i] = l.ID
		}
	}
	lessonOfTeacher := make([]string, nT)
	for t := range nT {
		lessonOfTeacher[t] = lessonOf[t]
	}
	rec := NewRecorder()
	cnt := &counters{}
	ws := &wsCounters{}
	lr.measure(ctx, &res, rec, func(wctx context.Context, from, _ time.Time) {
		var wg sync.WaitGroup
		for i := range nStud {
			c := lr.sc[i]
			c.Rec = rec
			v := &studentVU{c: c, lessonID: lessonOf[i], rng: rand.New(rand.NewPCG(uint64(i), 2)), fireType: lr.fireType, think: lr.cfg.Think, cnt: cnt}
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(time.Until(from) + time.Duration(v.rng.IntN(1000))*time.Millisecond)
				v.run(wctx, func() time.Duration { return time.Duration(15+v.rng.IntN(11)) * time.Second })
			}()
		}
		for t := range nT {
			c := lr.tc[t]
			c.Rec = rec
			rng := rand.New(rand.NewPCG(uint64(t), 3))
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(time.Until(from))
				teacherLoop(wctx, c, lessonOfTeacher[t], rng, lr.cfg.Think, lr.scenarios, ws)
			}()
		}
		for a := range nA {
			c := lr.ac[a]
			c.Rec = rec
			rng := rand.New(rand.NewPCG(uint64(a), 4))
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(time.Until(from))
				adminLoop(wctx, c, rng, lr.cfg.Think)
			}()
		}
		wg.Wait()
	})
	res.Counters = cnt.snapshot()
	res.Counters["wsMonitorConnects"] = ws.connects.Load()
	res.Counters["wsMonitorMessages"] = ws.messages.Load()
	res.Counters["wsMonitorDrops"] = ws.drops.Load()
	return res, nil
}

// group3 — пик: все принимают вызов в один момент, заполняют, сдают одновременно и ждут оценку
// (опрос GET /evaluation, как ResultPage, + WebSocket канала попытки).
func (lr *loadRun) group3(ctx context.Context) (GroupResult, error) {
	res := GroupResult{ID: "3", Title: "Пик: одновременный старт и сдача",
		Description: "Все обучающиеся одного занятия (1 карточка) по общему сигналу одновременно нажимают «Принять вызов» " +
			"(t=1 с), открывают WebSocket попытки, легенду и черновик; 4 правки через 2 с (PUT /draft + POST /events); " +
			"по второму сигналу (t=11 с) одновременно досылают события, сохраняют черновик и сдают карточку; далее опрос " +
			"GET /evaluation раз в 1,5 с до конца окна. После окна (вне замера) — ожидание итоговой оценки всех AI-слоёв.",
		VUs: map[string]int{"students": len(lr.sc)}}
	l, err := createLesson(ctx, lr.tc[0], "LT Группа 3 "+time.Now().Format("15:04:05"), lr.scenarios, lr.students, 1, 600)
	if err != nil {
		return res, err
	}
	defer finishLesson(context.Background(), lr.tc[0], l.ID)
	ids := make([]string, len(lr.sc))
	for i, c := range lr.sc {
		c.Rec = nil
		a, r := currentAttempt(ctx, c, l.ID)
		if a == nil {
			return res, fmt.Errorf("нет попытки у %s: %s", c.Name, r)
		}
		ids[i] = a.ID
	}
	rec := NewRecorder()
	cnt := &counters{}
	ws := &wsCounters{}
	type evalRes struct {
		mu               sync.Mutex
		submitAt, doneAt time.Time
		viaWS            bool
	}
	out := make([]evalRes, len(lr.sc))
	var wsDone atomic.Int64
	var accMax, subMax atomic.Int64
	var evalWG sync.WaitGroup // опрос оценки и WebSocket живут и после окна (вне замера)
	lr.measure(ctx, &res, rec, func(wctx context.Context, from, to time.Time) {
		t0 := from.Add(time.Second)
		var wg sync.WaitGroup
		for i, c := range lr.sc {
			c.Rec = rec
			wg.Add(1)
			go func() {
				defer wg.Done()
				rng := rand.New(rand.NewPCG(uint64(i), 5))
				base := "/attempts/" + ids[i]
				o := &out[i]
				time.Sleep(time.Until(t0))
				r := c.Call(wctx, http.MethodPost, base+"/accept-call", "POST /attempts/{id}/accept-call", nil)
				atomicMax(&accMax, int64(r.Dur/time.Millisecond))
				if !r.OK() {
					return
				}
				cnt.accepts.Add(1)
				// ectx — ожидание итоговой оценки (WebSocket + опрос), живёт и после окна.
				ectx, ecancel := context.WithTimeout(ctx, time.Until(to)+lr.cfg.EvalWait)
				submitted := false
				defer func() {
					if !submitted {
						ecancel()
					}
				}()
				evalWG.Add(1)
				go func() {
					defer evalWG.Done()
					conn, err := c.DialWS(ectx, "/ws/attempts/"+ids[i], "WS /ws/attempts/{id} (connect)")
					if err != nil {
						return
					}
					ws.connects.Add(1)
					defer conn.CloseNow()
					for {
						_, data, err := conn.Read(ectx)
						if err != nil {
							return
						}
						ws.messages.Add(1)
						var m struct {
							Type       string
							Evaluation *struct{ Status string }
						}
						if json.Unmarshal(data, &m) == nil && m.Type == "evaluationUpdated" && m.Evaluation != nil && m.Evaluation.Status == "done" {
							o.mu.Lock()
							if o.doneAt.IsZero() {
								o.doneAt, o.viaWS = time.Now(), true
								wsDone.Add(1)
							}
							o.mu.Unlock()
							ecancel()
							return
						}
					}
				}()
				c.Call(wctx, http.MethodGet, base+"/call-script", "GET /attempts/{id}/call-script", nil)
				var draft map[string]any
				if r := c.Call(wctx, http.MethodGet, base+"/draft", "GET /attempts/{id}/draft", nil); r.JSON(&draft) != nil {
					return
				}
				seq := 0
				v := &studentVU{c: c, rng: rng, cnt: cnt}
				for k := 1; k <= 4; k++ {
					if !sleepCtx(wctx, time.Until(t0.Add(time.Duration(2*k)*time.Second+time.Duration(rng.IntN(300))*time.Millisecond))) {
						return
					}
					mutateDraft(draft, k, lr.fireType)
					if r := c.Call(wctx, http.MethodPut, base+"/draft", "PUT /attempts/{id}/draft", draft); r.OK() {
						cnt.drafts.Add(1)
					}
					v.postEvents(wctx, base, fieldEvents(&seq, 1+rng.IntN(4)))
				}
				if !sleepCtx(wctx, time.Until(t0.Add(10*time.Second))) {
					return
				}
				v.postEvents(wctx, base, fieldEvents(&seq, 1))
				c.Call(wctx, http.MethodPut, base+"/draft", "PUT /attempts/{id}/draft", draft)
				r = c.Call(wctx, http.MethodPost, base+"/submit", "POST /attempts/{id}/submit", map[string]any{"card": draft})
				atomicMax(&subMax, int64(r.Dur/time.Millisecond))
				if !r.OK() {
					return
				}
				cnt.submits.Add(1)
				o.mu.Lock()
				o.submitAt = time.Now()
				o.mu.Unlock()
				submitted = true
				// Экран результата: опрос каждые 1,5 с (ResultPage) — в окне и после него (вне замера).
				evalWG.Add(1)
				go func() {
					defer evalWG.Done()
					defer ecancel()
					pollCtx := ectx
					for {
						o.mu.Lock()
						done := !o.doneAt.IsZero()
						o.mu.Unlock()
						if done || !sleepCtx(pollCtx, 1500*time.Millisecond) {
							return
						}
						r := c.Call(pollCtx, http.MethodGet, base+"/evaluation", "GET /attempts/{id}/evaluation", nil)
						cnt.evalPolls.Add(1)
						var ev struct{ Status string }
						if r.OK() && r.JSON(&ev) == nil && ev.Status == "done" {
							o.mu.Lock()
							if o.doneAt.IsZero() {
								o.doneAt = time.Now()
							}
							o.mu.Unlock()
							return
						}
					}
				}()
			}()
		}
		wg.Wait()
	})
	evalWG.Wait()
	res.Counters = cnt.snapshot()
	res.Counters["wsAttemptConnects"] = ws.connects.Load()
	res.Counters["wsAttemptMessages"] = ws.messages.Load()
	res.Counters["evaluationDoneViaWS"] = wsDone.Load()
	res.Counters["acceptCallMaxMs"] = accMax.Load()
	res.Counters["submitMaxMs"] = subMax.Load()
	var durs []time.Duration
	done, submitted := 0, 0
	for i := range out {
		o := &out[i]
		if !o.submitAt.IsZero() {
			submitted++
			if !o.doneAt.IsZero() {
				done++
				durs = append(durs, o.doneAt.Sub(o.submitAt))
			}
		}
	}
	res.Extra = map[string]any{
		"submitted":              submitted,
		"evaluationDone":         done,
		"submitToEvaluationDone": durStats(durs),
		"note": "Время до итоговой оценки определяется AI-очередью (здесь — имитатор fakeai: 2 воркера LLM × 1,5 с на смысловую " +
			"проверку), это не время отклика интерфейса: поля и тайминг оцениваются мгновенно в submit (status=partial).",
	}
	return res, nil
}

func atomicMax(a *atomic.Int64, v int64) {
	for {
		cur := a.Load()
		if v <= cur || a.CompareAndSwap(cur, v) {
			return
		}
	}
}
