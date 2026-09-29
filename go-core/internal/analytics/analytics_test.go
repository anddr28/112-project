package analytics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/pgtest"
)

// ---------------------------------------------------------------- юнит: инсайты

func fptr(v float64) *float64 { return &v }

func baseData(attempts, students int) *overviewData {
	d := &overviewData{}
	d.summary = summaryRow{attempts: attempts, students: students, avgScore: 72, passPct: 60, avgTimeMs: 40000, withinPct: 70}
	for i := range d.summary.layers {
		d.summary.layers[i] = layerAgg{avg: fptr(75), n: attempts}
	}
	return d
}

func testInput(d *overviewData) insightInput {
	names := map[uuid.UUID]string{}
	for _, s := range d.students {
		names[s.id] = fullName(s.last, s.first, s.middle)
	}
	cats := map[uuid.UUID]string{}
	for _, c := range d.categories {
		cats[c.id] = c.name
	}
	return insightInput{data: d, threshold: 70, names: names, catNames: cats,
		label: func(f, l string) string {
			if l != "" {
				return l
			}
			return f
		},
		semAttempts: d.summary.layers[1].n, dlgAttempts: d.summary.layers[4].n}
}

func byKind(ins []public.Insight, kind string) []public.Insight {
	var out []public.Insight
	for _, i := range ins {
		if i.Kind == kind {
			out = append(out, i)
		}
	}
	return out
}

func TestInsightsNoAndFewData(t *testing.T) {
	t.Parallel()
	ins := buildInsights(testInput(baseData(0, 0)))
	if len(ins) != 1 || ins[0].Id != "general:no-data" || ins[0].Severity != sevInfo {
		t.Fatalf("нет данных: %+v", ins)
	}
	ins = buildInsights(testInput(baseData(2, 1)))
	if len(ins) != 1 || ins[0].Id != "general:few-data" || !strings.Contains(ins[0].Body, "2 карточки") {
		t.Fatalf("мало данных: %+v", ins)
	}
}

func TestInsightsRules(t *testing.T) {
	t.Parallel()
	d := baseData(20, 10)
	s1, s2, s3 := uuid.New(), uuid.New(), uuid.New()
	fire, dtp := uuid.New(), uuid.New()
	d.students = []studentRow{
		{id: s1, last: "Иванов", first: "Иван", attempts: 4, lastVerdicts: []string{"fail", "fail", "fail", "pass"}},
		{id: s2, last: "Петров", first: "Пётр", attempts: 3, lastVerdicts: []string{"fail", "pass", "fail"}},
		{id: s3, last: "Сидоров", first: "Сидор", attempts: 2, lastVerdicts: []string{"fail", "fail"}},
	}
	d.fieldErrs = []fieldErrRow{
		{field: "phones.aon", kind: "missing", label: "Телефон заявителя", count: 9, attempts: 9, students: 4, users: []uuid.UUID{s1, s2}},
		{field: "address.raw", kind: "wrong", label: "Адрес", count: 11, attempts: 11, students: 6, users: []uuid.UUID{s3}},
		{field: "description", kind: "missing", label: "Описание", count: 2, attempts: 2, students: 2}, // 10% — не вывод
	}
	d.heat = []heatRow{{fire, "phones.aon", "", 6}, {dtp, "phones.aon", "", 3}}
	d.categories = []categoryRow{
		{id: fire, name: "Пожар", attempts: 12, avgScore: 45, passPct: 20, timed: 12, overLimit: 9, avgOverMs: fptr(18000)},
		{id: dtp, name: "ДТП", attempts: 8, avgScore: 81, passPct: 90, timed: 8, overLimit: 1, avgOverMs: fptr(3000)},
	}
	d.facts = []factRow{{text: "Дым идёт из окна 5 этажа", count: 8, students: 5, users: []uuid.UUID{s2}}}
	d.grammar = []grammarRow{{rule: "PUNCT", message: "Пропущена запятая", count: 7, students: 6, suggestion: fptrS(", что")}}
	d.dialogue = []factRow{{text: "не уточнил подъезд", count: 9, students: 5}}
	d.summary.layers[1] = layerAgg{avg: fptr(48), n: 20} // смысл — слабый слой
	d.summary.needsReview = 2
	ins := buildInsights(testInput(d))

	// порядок: критичные сверху
	rank := map[public.InsightSeverity]int{sevCritical: 0, sevWarning: 1, sevInfo: 2}
	for i := 1; i < len(ins); i++ {
		if rank[ins[i-1].Severity] > rank[ins[i].Severity] {
			t.Fatalf("порядок важности нарушен: %v", ins)
		}
	}
	risk := byKind(ins, "student_at_risk")
	if len(risk) != 1 || *risk[0].Metric != 1 || (*risk[0].AffectedStudents)[0] != "Иванов Иван" ||
		!strings.HasPrefix(risk[0].Title, "1 обучающийся ниже порога") {
		t.Errorf("student_at_risk: %+v", risk)
	}
	wf := byKind(ins, "weak_field")
	if len(wf) != 2 {
		t.Fatalf("weak_field: %+v", wf)
	}
	// критичный (адрес, 55% карточек) — выше предупреждения (телефон, 45%)
	wf[0], wf[1] = wf[1], wf[0]
	if wf[0].Title != "40% группы не заполняют поле «Телефон заявителя»" || wf[0].Severity != sevWarning ||
		!strings.Contains(wf[0].Body, "9 из 20 карточек (45%)") || !strings.Contains(wf[0].Body, "АОН") {
		t.Errorf("weak_field телефон: %+v", wf[0])
	}
	if ev := *wf[0].Evidence; len(ev) != 2 || !strings.Contains(ev[1], "«Пожар» (6)") {
		t.Errorf("evidence: %v", ev)
	}
	if wf[1].Id != "weak_field:address.raw:wrong" || wf[1].Severity != sevCritical || !strings.Contains(wf[1].Body, "Сравните ответы с эталоном") {
		t.Errorf("weak_field адрес: %+v", wf[1])
	}
	slow := byKind(ins, "slow_timing")
	if len(slow) != 1 || slow[0].Title != "Средний выход за норматив 18 с в категории «Пожар»" || slow[0].Severity != sevCritical {
		t.Errorf("slow_timing: %+v", slow)
	}
	wc := byKind(ins, "weak_category")
	if len(wc) != 1 || wc[0].Title != "Слабая категория «Пожар»: средний балл 45" || wc[0].Severity != sevCritical {
		t.Errorf("weak_category: %+v", wc)
	}
	wl := byKind(ins, "weak_layer")
	if len(wl) != 1 || wl[0].Id != "weak_layer:semantic" || !strings.Contains(wl[0].Title, "«Смысл» — средний балл 48") {
		t.Errorf("weak_layer: %+v", wl)
	}
	if mf := byKind(ins, "missing_fact"); len(mf) != 1 || mf[0].Title != "Факт «Дым идёт из окна 5 этажа» не отражён в 40% карточек" {
		t.Errorf("missing_fact: %+v", mf)
	}
	if dp := byKind(ins, "dialogue_pattern"); len(dp) != 1 || !strings.Contains(dp[0].Body, "9 из 20 разговоров (45%)") {
		t.Errorf("dialogue_pattern: %+v", dp)
	}
	if gp := byKind(ins, "grammar_pattern"); len(gp) != 1 || gp[0].Severity != sevWarning || !strings.Contains(gp[0].Body, "«, что»") {
		t.Errorf("grammar_pattern: %+v", gp)
	}
	if g := byKind(ins, "general"); len(g) != 1 || g[0].Id != "general:needs-review" || !strings.HasPrefix(g[0].Title, "2 оценки") {
		t.Errorf("general: %+v", g)
	}
	seen := map[string]bool{}
	for _, i := range ins {
		if seen[i.Id] || i.Body == "" || i.Title == "" || !i.Severity.Valid() {
			t.Errorf("вывод %+v", i)
		}
		seen[i.Id] = true
	}
}

func fptrS(s string) *string { return &s }

func TestInsightsStrongGroup(t *testing.T) {
	t.Parallel()
	d := baseData(10, 5)
	d.summary.passPct, d.summary.avgScore = 90, 88
	ins := buildInsights(testInput(d))
	if len(ins) != 1 || ins[0].Id != "general:ready-for-harder" {
		t.Fatalf("сильная группа: %+v", ins)
	}
}

func TestHeatmapAndWeakestLayer(t *testing.T) {
	t.Parallel()
	a, b := uuid.New(), uuid.New()
	d := baseData(5, 2)
	d.categories = []categoryRow{{id: a, name: "Пожар", attempts: 3}, {id: b, name: "", attempts: 2}}
	d.heat = []heatRow{{a, "address.raw", "Адрес", 2}, {b, "address.raw", "", 1}, {a, "description", "", 1}, {uuid.New(), "x", "", 5}}
	cats := map[uuid.UUID]string{a: "Пожар", b: "Категория без названия"}
	rows, cols, cells := buildHeatmap(d, cats, func(f, l string) string { return orText(l, f) })
	if len(rows) != 2 || rows[0].Label != "Пожар" || rows[0].Attempts != 3 || rows[1].Label != "Категория без названия" {
		t.Errorf("rows %+v", rows)
	}
	if len(cols) != 3 || cols[0].Id != "x" || cols[1].Id != "address.raw" || cols[1].Label != "Адрес" {
		t.Errorf("cols %+v", cols)
	}
	if cells[0][1] != 2 || cells[1][1] != 1 || cells[0][2] != 1 || cells[0][0] != 0 {
		t.Errorf("cells %v", cells)
	}
	if w := weakestLayer([5]*float64{fptr(80), nil, fptr(40), fptr(90), nil}); w != "grammar" {
		t.Errorf("weakest = %s", w)
	}
}

// ---------------------------------------------------------------- HTTP + БД

type stubAuth struct{}

func (stubAuth) Authenticate(r *http.Request) (*core.Principal, error) {
	role := r.Header.Get("X-Test-Role")
	if role == "" {
		return nil, core.ErrUnauthenticated
	}
	return &core.Principal{UserID: uuid.MustParse(r.Header.Get("X-Test-User")), Role: core.Role(role)}, nil
}

type fixture struct {
	pool                     *pgxpool.Pool
	srv                      *httptest.Server
	teacher, teacher2, admin uuid.UUID
	students                 []uuid.UUID
	fire, dtp                uuid.UUID
	lesson, lesson2, other   uuid.UUID
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
}

func user(t *testing.T, pool *pgxpool.Pool, role, last, first string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	exec(t, pool, `INSERT INTO users (id, login, password_hash, role, last_name, first_name) VALUES ($1, $2, 'x', $3, $4, $5)`,
		id, "u"+id.String()[:8], role, last, first)
	return id
}

type evalSpec struct {
	lesson, student, scenario uuid.UUID
	score                     float64
	pass                      bool
	spentMs, limitSec         int
	fieldErrors               string // JSON-массив
	missing                   []string
	grammar                   string
	questions                 []string
	ago                       time.Duration
}

func (f *fixture) attempt(t *testing.T, s evalSpec) {
	t.Helper()
	id := uuid.New()
	sub := time.Now().UTC().Add(-s.ago)
	acc := sub.Add(-time.Duration(s.spentMs) * time.Millisecond)
	exec(t, f.pool, `INSERT INTO attempts (id, lesson_id, user_id, scenario_id, etalon_id, mode, seq_no, status, time_limit_sec,
		call_accepted_at, first_input_at, submitted_at, time_spent_ms)
		SELECT $1, $2, $3, $4, e.id, 'cards', (SELECT count(*) + 1 FROM attempts WHERE lesson_id = $2 AND user_id = $3),
		       'evaluated', $5, $6::timestamptz, $6::timestamptz + interval '3 seconds', $7, $8
		  FROM etalons e WHERE e.scenario_id = $4`, id, s.lesson, s.student, s.scenario, s.limitSec, acc, sub, s.spentMs)
	verdict := "fail"
	if s.pass {
		verdict = "pass"
	}
	within := s.spentMs <= s.limitSec*1000
	sem, _ := json.Marshal(map[string]any{"score": 60, "confidence": 0.9, "missing_facts": s.missing})
	dlg, _ := json.Marshal(map[string]any{"score": 50, "missing_questions": s.questions})
	fe := s.fieldErrors
	if fe == "" {
		fe = "[]"
	}
	gr := s.grammar
	if gr == "" {
		gr = "[]"
	}
	exec(t, f.pool, `INSERT INTO evaluations (attempt_id, etalon_id, status, fields_score, grammar_score, semantic_score,
		timing_score, dialogue_score, total_score, verdict, field_errors, grammar_remarks, semantic, dialogue, timing, layers,
		evaluated_at)
		SELECT $1, etalon_id, 'done', $2, 80, 60, 100, 50, $2, $3, $4::jsonb, $5::jsonb, $6::jsonb, $7::jsonb,
		       jsonb_build_object('within_norm', $8::bool, 'spent_ms', $9::int),
		       '{"grammar": "done", "semantic": "done", "dialogue": "done"}', now()
		  FROM attempts WHERE id = $1`, id, s.score, verdict, fe, gr, string(sem), string(dlg), within, s.spentMs)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := pgtest.New(t)
	f := &fixture{pool: pool}
	f.teacher = user(t, pool, "teacher", "Учителев", "Иван")
	f.teacher2 = user(t, pool, "teacher", "Другой", "Пётр")
	f.admin = user(t, pool, "admin", "Админов", "Админ")
	for _, n := range []string{"Андреев", "Борисов", "Васильев", "Григорьев"} {
		f.students = append(f.students, user(t, pool, "student", n, "Тест"))
	}
	f.fire, f.dtp = uuid.New(), uuid.New()
	exec(t, pool, `INSERT INTO classifier_categories (id, code, name) VALUES ($1, 't101', 'Пожар'), ($2, 't201', 'ДТП')`, f.fire, f.dtp)
	scFire, scDtp := uuid.New(), uuid.New()
	exec(t, pool, `INSERT INTO scenarios (id, title, category_id, source, status) VALUES ($1, 'Пожар', $2, 'manual', 'validated'),
		($3, 'ДТП', $4, 'manual', 'validated')`, scFire, f.fire, scDtp, f.dtp)
	exec(t, pool, `INSERT INTO etalons (scenario_id, card) VALUES ($1, '{}'), ($2, '{}')`, scFire, scDtp)
	lesson := func(teacher uuid.UUID) uuid.UUID {
		id := uuid.New()
		exec(t, pool, `INSERT INTO lessons (id, title, teacher_id, created_by, mode, status) VALUES ($1, 'Занятие', $2, $2, 'cards', 'finished')`,
			id, teacher)
		return id
	}
	f.lesson, f.lesson2, f.other = lesson(f.teacher), lesson(f.teacher), lesson(f.teacher2)

	phone := `[{"field": "phones.aon", "label": "Телефон (АОН)", "kind": "missing", "weight": 1}]`
	phoneAddr := `[{"field": "phones.aon", "label": "Телефон (АОН)", "kind": "missing"}, {"field": "address.raw", "label": "Адрес", "kind": "wrong"}]`
	comma := `[{"rule": "COMMA", "message": "Пропущена запятая", "suggestions": [", который"]}]`
	a, b, c, d := f.students[0], f.students[1], f.students[2], f.students[3]
	for _, s := range []evalSpec{
		// Андреев: три незачёта подряд, пожар, выходит за норматив
		{lesson: f.lesson, student: a, scenario: scFire, score: 40, spentMs: 60000, limitSec: 30, fieldErrors: phoneAddr, missing: []string{"Горит кухня"}, grammar: comma, ago: 3 * time.Hour},
		{lesson: f.lesson, student: a, scenario: scFire, score: 45, spentMs: 50000, limitSec: 30, fieldErrors: phone, missing: []string{"Горит кухня"}, grammar: comma, ago: 2 * time.Hour},
		{lesson: f.lesson, student: a, scenario: scFire, score: 50, spentMs: 45000, limitSec: 30, fieldErrors: phone, questions: []string{"не уточнил этаж"}, ago: time.Hour},
		// Борисов: пожар, есть пропуски
		{lesson: f.lesson, student: b, scenario: scFire, score: 65, spentMs: 40000, limitSec: 30, fieldErrors: phone, missing: []string{"Горит кухня"}, grammar: comma, questions: []string{"не уточнил этаж"}, ago: 26 * time.Hour},
		{lesson: f.lesson2, student: b, scenario: scDtp, score: 85, pass: true, spentMs: 20000, limitSec: 30, ago: 2 * time.Hour},
		// Васильев: ДТП, хорошо
		{lesson: f.lesson2, student: c, scenario: scDtp, score: 90, pass: true, spentMs: 25000, limitSec: 30, ago: time.Hour},
		// Григорьев — в занятии другого преподавателя: учителю не виден
		{lesson: f.other, student: d, scenario: scFire, score: 10, spentMs: 90000, limitSec: 30, fieldErrors: phone, ago: time.Hour},
		// старое — за окном 30 дней
		{lesson: f.lesson, student: c, scenario: scFire, score: 20, spentMs: 90000, limitSec: 30, fieldErrors: phone, ago: 40 * 24 * time.Hour},
	} {
		f.attempt(t, s)
	}
	// попытка не оценена — не учитывается
	exec(t, pool, `INSERT INTO attempts (lesson_id, user_id, scenario_id, etalon_id, mode, seq_no, status, time_limit_sec)
		SELECT $1, $2, $3, e.id, 'cards', 99, 'in_progress', 30 FROM etalons e WHERE e.scenario_id = $3`, f.lesson, c, scFire)

	h := New(Deps{Pool: pool, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	mux := http.NewServeMux()
	r := httpx.NewRouter(mux, "/api/v1", stubAuth{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	h.Register(r)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) get(t *testing.T, role string, id uuid.UUID, query string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", f.srv.URL+"/api/v1/analytics/overview"+query, nil)
	req.Header.Set("X-Test-Role", role)
	req.Header.Set("X-Test-User", id.String())
	res, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

func (f *fixture) overview(t *testing.T, role string, id uuid.UUID, query string) public.AnalyticsOverview {
	t.Helper()
	st, b := f.get(t, role, id, query)
	if st != 200 {
		t.Fatalf("%s %s: %d %s", role, query, st, b)
	}
	var o public.AnalyticsOverview
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatal(err)
	}
	// обязательные массивы — [] (не null)
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	for _, k := range []string{"layers", "trend", "topFieldErrors", "topMissingFacts", "topGrammarRules", "students", "categories", "insights"} {
		if _, ok := raw[k].([]any); !ok {
			t.Errorf("%s = %v, want array", k, raw[k])
		}
	}
	return o
}

func TestOverview_DB(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	o := f.overview(t, "teacher", f.teacher, "")

	// сводка: 6 попыток преподавателя за 30 дней, 3 обучающихся
	if o.Summary.Attempts != 6 || o.Summary.Students != 3 || o.Scope.Days != 30 {
		t.Fatalf("summary %+v scope %+v", o.Summary, o.Scope)
	}
	if o.Summary.AvgScore != 62.5 || o.Summary.PassRatePct != 33.3 || o.Summary.WithinNormPct != 33.3 ||
		o.Summary.AvgReactionMs == nil || *o.Summary.AvgReactionMs != 3000 {
		t.Errorf("summary %+v", o.Summary)
	}
	if len(o.Layers) != 5 || o.Layers[0].Layer != "fields" || o.Layers[0].Attempts != 6 || o.Layers[4].AvgScore != 50 {
		t.Errorf("layers %+v", o.Layers)
	}
	// тепловая карта: строки — категории, столбцы — поля
	if len(o.Heatmap.Rows) != 2 || o.Heatmap.Rows[0].Label != "Пожар" || o.Heatmap.Rows[0].Attempts != 4 {
		t.Fatalf("heatmap rows %+v", o.Heatmap.Rows)
	}
	if len(o.Heatmap.Cols) != 2 || o.Heatmap.Cols[0].Id != "phones.aon" || o.Heatmap.Cols[0].Label != "Телефон (АОН)" {
		t.Fatalf("heatmap cols %+v", o.Heatmap.Cols)
	}
	if fmt.Sprint(o.Heatmap.Cells) != "[[4 1] [0 0]]" {
		t.Errorf("cells %v", o.Heatmap.Cells)
	}
	if len(o.TopFieldErrors) != 2 || o.TopFieldErrors[0].Field != "phones.aon" || o.TopFieldErrors[0].SharePct != 66.7 {
		t.Errorf("top field errors %+v", o.TopFieldErrors)
	}
	if len(o.TopMissingFacts) != 1 || o.TopMissingFacts[0].Fact != "Горит кухня" || o.TopMissingFacts[0].Count != 3 || o.TopMissingFacts[0].SharePct != 50 {
		t.Errorf("missing facts %+v", o.TopMissingFacts)
	}
	if len(o.TopGrammarRules) != 1 || o.TopGrammarRules[0].Rule != "COMMA" || o.TopGrammarRules[0].Count != 3 ||
		o.TopGrammarRules[0].Example == nil || !strings.Contains(*o.TopGrammarRules[0].Example, "который") {
		t.Errorf("grammar %+v", o.TopGrammarRules)
	}
	if len(o.Trend) < 2 {
		t.Errorf("trend %+v", o.Trend)
	}
	if len(o.Students) != 3 || o.Students[0].Name != "Андреев Тест" || o.Students[0].Attempts != 3 || o.Students[0].AvgScore != 45 {
		t.Errorf("students %+v", o.Students)
	}
	if w := o.Students[0].WeakestLayer; w == nil || *w != "fields" {
		t.Errorf("weakest %v", w)
	}
	if len(o.Categories) != 2 || o.Categories[0].CategoryName != "Пожар" || o.Categories[0].PassRatePct != 0 {
		t.Errorf("categories %+v", o.Categories)
	}
	// инсайты: Андреев в зоне риска, телефон не заполняют, выход за норматив в «Пожаре»
	kinds := map[string]public.Insight{}
	for _, i := range o.Insights {
		kinds[i.Kind] = i
	}
	if r, ok := kinds["student_at_risk"]; !ok || r.AffectedStudents == nil || (*r.AffectedStudents)[0] != "Андреев Тест" {
		t.Errorf("student_at_risk: %+v", o.Insights)
	}
	if w, ok := kinds["weak_field"]; !ok || !strings.Contains(w.Title, "«Телефон (АОН)»") || !strings.HasPrefix(w.Title, "67% группы") {
		t.Errorf("weak_field: %+v", w)
	}
	if s, ok := kinds["slow_timing"]; !ok || !strings.Contains(s.Title, "«Пожар»") {
		t.Errorf("slow_timing: %+v", s)
	}
	if _, ok := kinds["missing_fact"]; !ok {
		t.Errorf("missing_fact: %+v", o.Insights)
	}

	// админ видит и занятие другого преподавателя
	if a := f.overview(t, "admin", f.admin, ""); a.Summary.Attempts != 7 || a.Summary.Students != 4 {
		t.Errorf("admin summary %+v", a.Summary)
	}
	// фильтры
	if x := f.overview(t, "teacher", f.teacher, "?categoryId=t201"); x.Summary.Attempts != 2 || *x.Scope.CategoryId != "t201" {
		t.Errorf("категория по коду: %+v", x.Summary)
	}
	if x := f.overview(t, "teacher", f.teacher, "?studentId="+f.students[1].String()); x.Summary.Attempts != 2 {
		t.Errorf("обучающийся: %+v", x.Summary)
	}
	// занятие без days — всё занятие (включая попытку 40-дневной давности)
	if x := f.overview(t, "teacher", f.teacher, "?lessonId="+f.lesson.String()); x.Summary.Attempts != 5 || *x.Scope.LessonId != f.lesson {
		t.Errorf("занятие целиком: %+v", x.Summary)
	}
	if x := f.overview(t, "teacher", f.teacher, "?lessonId="+f.lesson.String()+"&days=30"); x.Summary.Attempts != 4 {
		t.Errorf("занятие за 30 дней: %+v", x.Summary)
	}
	if x := f.overview(t, "teacher", f.teacher, "?days=1"); x.Summary.Attempts != 5 {
		t.Errorf("days=1: %+v", x.Summary)
	}
	// пустая выборка — нули и вывод «нет данных»
	if x := f.overview(t, "teacher", f.teacher2, "?categoryId=t201"); x.Summary.Attempts != 0 || len(x.Insights) != 1 ||
		len(x.Heatmap.Rows) != 0 || x.Heatmap.Cells == nil {
		t.Errorf("пусто: %+v", x)
	}

	// доступ и ошибки
	for _, tc := range []struct {
		role  string
		id    uuid.UUID
		query string
		code  int
	}{
		{"teacher", f.teacher, "?lessonId=" + f.other.String(), 403},
		{"teacher", f.teacher, "?lessonId=" + uuid.NewString(), 404},
		{"teacher", f.teacher, "?studentId=" + f.teacher2.String(), 404},
		{"teacher", f.teacher, "?categoryId=nope", 404},
		{"teacher", f.teacher, "?days=0", 400},
		{"teacher", f.teacher, "?days=400", 400},
		{"teacher", f.teacher, "?lessonId=abc", 400},
		{"student", f.students[0], "", 403},
		{"admin", f.admin, "?lessonId=" + f.other.String(), 200},
	} {
		if st, b := f.get(t, tc.role, tc.id, tc.query); st != tc.code {
			t.Errorf("%s %s: %d %s, want %d", tc.role, tc.query, st, b, tc.code)
		}
	}
}
