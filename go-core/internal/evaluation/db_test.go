package evaluation

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/callbacks"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/platform/pgtest"
	"lct/gocore/internal/settings"
)

// ---------------------------------------------------------------- фикстура

type dbfx struct {
	pool  *pgxpool.Pool
	svc   *Service
	pub   *pubRec
	queue *queueRec
	aud   *auditRec
	h     http.Handler

	teacher, teacher2, student, student2, admin uuid.UUID
	cat, scenario, etalon                       uuid.UUID
}

const (
	fireScript = `{"caller": {"name": "Мария", "phone": "+79035113367"},
		"address": {"raw": "Москва, Тверская, 12"},
		"key_facts": ["горит квартира", "пятый этаж"],
		"turns": [{"speaker": "caller", "text": "Алло! Пожар!"}]}`
	fireCard     = `{"category_code": "ev101", "address": {"raw": "Москва, Тверская, 12"}, "description": "Горит квартира на пятом этаже"}`
	fireDraft    = `{"address": {"raw": "Москва, Тверская, 12"}, "description": "Горит квартира на пятом этаже"}`
	fireScoring  = `{"required_fields": ["address.raw", "description"], "required_facts": ["горит квартира", "пятый этаж"]}`
	fireDialogue = `{"checklist": [{"id": "q1", "text": "Уточнить этаж", "kind": "question", "required": true},
		{"id": "q2", "text": "Спросить о пострадавших", "kind": "question", "required": true}]}`

	// карточка студента со свободным текстом (слои grammar и semantic уходят в ai-service)
	cardFull = `{"address": {"raw": "Москва, Тверская, 12"}, "description": "Горит квартира на пятом этаже",
		"actionsTaken": "Передал вызов в службу 101"}`
	// без свободного текста: grammar skipped, semantic локально 0 — оценка готова сразу
	cardNoText = `{"address": {"raw": "Москва, Тверская, 12"}}`
)

func newDB(t *testing.T) *dbfx {
	t.Helper()
	pool := pgtest.New(t)
	f := &dbfx{pool: pool, pub: &pubRec{}, queue: &queueRec{}, aud: &auditRec{}}

	user := func(login, role, last, first string) uuid.UUID {
		return f.id(t, `INSERT INTO users (login, password_hash, role, last_name, first_name, middle_name)
			VALUES ($1, 'x', $2, $3, $4, 'Иванович') RETURNING id`, login, role, last, first)
	}
	f.teacher = user("ev_teacher", "teacher", "Учителев", "Пётр")
	f.teacher2 = user("ev_teacher2", "teacher", "Наставников", "Олег")
	f.student = user("ev_student", "student", "Яковлев", "Антон")
	f.student2 = user("ev_student2", "student", "Борисов", "Борис")
	f.admin = user("ev_admin", "admin", "Админов", "Сергей")

	f.cat = f.id(t, `INSERT INTO classifier_categories (code, name) VALUES ('ev101', 'Пожар') RETURNING id`)
	f.scenario = f.id(t, `INSERT INTO scenarios (title, category_id, difficulty, mode, source, status, call_script)
		VALUES ('Пожар в квартире', $1, 2, 'both', 'manual', 'validated', $2) RETURNING id`, f.cat, fireScript)
	f.etalon = f.id(t, `INSERT INTO etalons (scenario_id, version, is_current, card, card_draft, scoring, expected_dialogue)
		VALUES ($1, 1, true, $2, $3, $4, $5) RETURNING id`, f.scenario, fireCard, fireDraft, fireScoring, fireDialogue)

	f.svc = New(Deps{
		Pool: pool, Settings: settings.NewStore(nil, discardLog()), // дефолты = сиды миграций
		Queue: f.queue, Publisher: f.pub, Auditor: f.aud, Log: discardLog(),
	})
	principal := func(id uuid.UUID, role core.Role, last, first string) *core.Principal {
		return &core.Principal{UserID: id, Role: role, LastName: last, FirstName: first, MiddleName: "Иванович"}
	}
	f.h = newRouter(f.svc, map[string]*core.Principal{
		"teacher":  principal(f.teacher, core.RoleTeacher, "Учителев", "Пётр"),
		"teacher2": principal(f.teacher2, core.RoleTeacher, "Наставников", "Олег"),
		"student":  principal(f.student, core.RoleStudent, "Яковлев", "Антон"),
		"student2": principal(f.student2, core.RoleStudent, "Борисов", "Борис"),
		"admin":    principal(f.admin, core.RoleAdmin, "Админов", "Сергей"),
	})
	return f
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func (f *dbfx) id(t *testing.T, sql string, args ...any) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return id
}

func (f *dbfx) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// lesson — занятие преподавателя teacher; settings — lessons.settings (snake_case).
func (f *dbfx) lesson(t *testing.T, status, settingsJSON string, students ...uuid.UUID) uuid.UUID {
	t.Helper()
	id := f.id(t, `INSERT INTO lessons (kind, title, teacher_id, created_by, mode, time_limit_sec, settings, status, started_at)
		VALUES ('class', 'Пожары', $1, $1, 'cards', 45, $2, $3, now()) RETURNING id`, f.teacher, settingsJSON, status)
	for _, s := range students {
		f.exec(t, `INSERT INTO lesson_participants (lesson_id, user_id, status, joined_at) VALUES ($1, $2, 'active', now())`, id, s)
	}
	return id
}

// attempt — карточка студента. Сданная (submitted) получает метки времени: 20 с на звонок,
// реакция 3 с — в нормативе 45 с.
func (f *dbfx) attempt(t *testing.T, lesson, user uuid.UUID, seq int, status, mode string, card, actionText *string) uuid.UUID {
	t.Helper()
	return f.id(t, `INSERT INTO attempts (lesson_id, user_id, scenario_id, etalon_id, mode, seq_no, status, time_limit_sec,
			call_accepted_at, first_input_at, submitted_at, time_spent_ms, card, action_text)
		VALUES ($1, $2, $3, $4, $5, $6, $7::text, 45,
			CASE WHEN $7::text <> 'issued' THEN now() - interval '20 seconds' END,
			CASE WHEN $7::text <> 'issued' THEN now() - interval '17 seconds' END,
			CASE WHEN $7::text IN ('submitted', 'evaluating', 'evaluated') THEN now() END,
			CASE WHEN $7::text IN ('submitted', 'evaluating', 'evaluated') THEN 20000 END,
			$8::jsonb, $9) RETURNING id`,
		lesson, user, f.scenario, f.etalon, mode, seq, status, card, actionText)
}

func (f *dbfx) submitted(t *testing.T, lesson, user uuid.UUID, seq int, card string) uuid.UUID {
	t.Helper()
	return f.attempt(t, lesson, user, seq, core.AttemptSubmitted, core.ModeCards, &card, nil)
}

func (f *dbfx) start(t *testing.T, attemptID uuid.UUID) {
	t.Helper()
	if err := pg.WithTx(ctxT(t), f.pool, func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.StartEvaluation(ctx, tx, attemptID)
	}); err != nil {
		t.Fatalf("StartEvaluation: %v", err)
	}
}

func (f *dbfx) apply(t *testing.T, attemptID uuid.UUID, typ core.JobType, res *callbacks.AiResult) {
	t.Helper()
	job := core.JobRecord{ID: uuid.New(), Type: typ, Status: core.JobRunning, RefType: core.RefAttempt, RefID: attemptID}
	if err := pg.WithTx(ctxT(t), f.pool, func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.ApplyResult(ctx, tx, job, res)
	}); err != nil {
		t.Fatalf("ApplyResult %s: %v", typ, err)
	}
}

func (f *dbfx) failLayer(t *testing.T, attemptID uuid.UUID, typ core.JobType, code string) {
	t.Helper()
	job := core.JobRecord{ID: uuid.New(), Type: typ, Status: core.JobRunning, RefType: core.RefAttempt, RefID: attemptID}
	if err := pg.WithTx(ctxT(t), f.pool, func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.ApplyFailure(ctx, tx, job, code, "ai-service echo: секретный текст")
	}); err != nil {
		t.Fatalf("ApplyFailure %s: %v", typ, err)
	}
}

// finishLesson — то, что делает lessons.finish: открытые карточки гаснут, занятие и участники
// завершены (сданные карточки остаются на проверке).
func (f *dbfx) finishLesson(t *testing.T, lesson uuid.UUID) {
	t.Helper()
	f.exec(t, `UPDATE attempts SET status = 'expired' WHERE lesson_id = $1 AND status IN ('issued', 'in_progress')`, lesson)
	f.exec(t, `UPDATE lessons SET status = 'finished', finished_at = now() WHERE id = $1`, lesson)
	f.exec(t, `UPDATE lesson_participants SET status = 'finished', finished_at = COALESCE(finished_at, now())
		WHERE lesson_id = $1 AND status <> 'finished'`, lesson)
}

// xp — начисления по условию: reason → сумма delta. Строки читаются до конца и закрываются
// до любых t.Fatal (иначе незакрытое соединение не дало бы pool.Close в Cleanup завершиться).
func (f *dbfx) xp(t *testing.T, where string, arg uuid.UUID) (sums, counts map[string]int) {
	t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT reason, sum(delta)::int, count(*)::int FROM xp_ledger WHERE `+where+` GROUP BY reason`, arg)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	sums, counts = map[string]int{}, map[string]int{}
	for rows.Next() {
		var r string
		var d, n int
		if err := rows.Scan(&r, &d, &n); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		sums[r], counts[r] = d, n
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	return sums, counts
}

// xpByAttempt — начисления за попытку (каждая причина — не больше одного раза).
func (f *dbfx) xpByAttempt(t *testing.T, a uuid.UUID) map[string]int {
	t.Helper()
	sums, counts := f.xp(t, "attempt_id = $1", a)
	for r, n := range counts {
		if n != 1 {
			t.Fatalf("XP %q for attempt written %d times", r, n)
		}
	}
	return sums
}

// xpByLesson — начисления занятия по причинам (суммы по всем карточкам); lesson_completed —
// не больше одного раза.
func (f *dbfx) xpByLesson(t *testing.T, l uuid.UUID) map[string]int {
	t.Helper()
	sums, counts := f.xp(t, "lesson_id = $1", l)
	if counts["lesson_completed"] > 1 {
		t.Fatalf("lesson_completed written %d times", counts["lesson_completed"])
	}
	return sums
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func (f *dbfx) str(t *testing.T, sql string, args ...any) string {
	t.Helper()
	var s *string
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return deref(s)
}

// view — оценка, как её видит преподаватель занятия (GET).
func (f *dbfx) view(t *testing.T, a uuid.UUID) map[string]any {
	t.Helper()
	r := do(f.h, http.MethodGet, "/api/v1/attempts/"+a.String()+"/evaluation", "teacher", nil, false)
	if r.Code != http.StatusOK {
		t.Fatalf("GET evaluation: %d %s", r.Code, r.Body)
	}
	return r.json(t)
}

func (f *dbfx) override(t *testing.T, a uuid.UUID, user string, score float64) resp {
	t.Helper()
	return do(f.h, http.MethodPost, "/api/v1/attempts/"+a.String()+"/evaluation/override", user,
		map[string]any{"score": score, "reason": "Проверено преподавателем вручную"}, true)
}

// ---------------------------------------------------------------- результаты ai-service

func grammarRes(score float32) *callbacks.AiResult {
	g := &components.GrammarResult{Score: score, Remarks: []components.GrammarRemark{
		{Field: "description", Offset: 0, Length: 6, Message: "Возможно, опечатка", Severity: "minor", Suggestions: &[]string{"Горит"}},
	}}
	g.Stats.WordsChecked = 9
	return &callbacks.AiResult{Grammar: g, Engine: components.Engine{DurationMs: 35, LtVersion: ptr("6.4"), QueueWaitMs: ptr(12)}}
}

func semanticRes(score, confidence float32) *callbacks.AiResult {
	return &callbacks.AiResult{
		Semantic: &components.SemanticResult{
			Score: score, Confidence: confidence,
			MissingFacts: &[]string{"пятый этаж"}, ExtraFacts: &[]string{},
			PerField: &[]components.SemanticPerField{
				{Field: "description", Score: 80}, {Field: "actions_taken", Score: 60, Comment: ptr("Не указан расчёт")},
			},
			SummaryForStudent: ptr("Адрес зафиксирован, этаж — нет."),
		},
		Engine: components.Engine{DurationMs: 900, LlmModel: ptr("qwen2.5:3b"), PromptVersion: ptr("sem-v3"), TokensIn: ptr(1500)},
	}
}

// ---------------------------------------------------------------- старт оценки

func TestDBStartEvaluationCards(t *testing.T) {
	t.Parallel()
	f := newDB(t)
	lesson := f.lesson(t, core.LessonRunning, `{"pass_threshold": 70, "cards_per_student": 1}`, f.student)
	a := f.submitted(t, lesson, f.student, 1, cardFull)

	f.start(t, a)

	jobs := f.queue.byAttempt(a)
	if len(jobs) != 2 {
		t.Fatalf("jobs = %v, want grammar + semantic", jobs)
	}
	gj, sj := jobs[core.JobEvaluateGrammar], jobs[core.JobEvaluateSemantic]
	if gj.DedupKey != "eval:"+a.String()+":grammar" || gj.RefType != core.RefAttempt || gj.RefID != a || gj.ID == uuid.Nil {
		t.Fatalf("grammar job = %+v", gj)
	}
	graw, _ := json.Marshal(gj.Payload)
	var gp struct {
		RequestID string `json:"request_id"`
		AttemptID string `json:"attempt_id"`
		Texts     []struct{ Field, Text string }
	}
	if err := json.Unmarshal(graw, &gp); err != nil || gp.RequestID != gj.ID.String() || gp.AttemptID != a.String() ||
		len(gp.Texts) != 2 || gp.Texts[0].Field != "description" || gp.Texts[1].Field != "actionsTaken" ||
		gp.Texts[1].Text != "Передал вызов в службу 101" {
		t.Fatalf("grammar payload = %s (%v)", graw, err)
	}
	sraw, _ := json.Marshal(sj.Payload)
	for _, want := range []string{`"mode":"cards"`, `"free_text_fields":["description","actions_taken"]`, `"code":"ev101"`,
		`"description":"Горит квартира на пятом этаже"`} {
		if !strings.Contains(string(sraw), want) {
			t.Errorf("semantic payload lacks %s: %s", want, sraw)
		}
	}
	for _, raw := range [][]byte{graw, sraw} {
		for _, leak := range []string{f.student.String(), lesson.String(), "Яковлев"} {
			if strings.Contains(string(raw), leak) {
				t.Fatalf("payload leaks personal data %q: %s", leak, raw)
			}
		}
	}

	// после COMMIT — evaluationUpdated преподавателю и студенту
	if m := f.pub.monitorOf(public.MonitorMessageTypeEvaluationUpdated); len(m) != 1 || m[0].Lesson != lesson ||
		m[0].Msg.Evaluation == nil || m[0].Msg.Evaluation.Status != public.EvaluationStatusPartial || *m[0].Msg.AttemptId != a {
		t.Fatalf("monitor pushes = %+v", m)
	}
	if s := f.pub.studentFor(a); len(s) != 1 || s[0].Msg.Type != public.StudentMessageTypeEvaluationUpdated {
		t.Fatalf("student pushes = %+v", s)
	}

	ev := f.view(t, a)
	if ev["status"] != "partial" || ev["verdict"] != "pending" {
		t.Fatalf("status=%v verdict=%v", ev["status"], ev["verdict"])
	}
	layers, _ := ev["layers"].(map[string]any)
	want := map[string]any{"fields": "done", "timing": "done", "grammar": "queued", "semantic": "queued", "dialogue": "skipped"}
	if !maps.Equal(layers, want) {
		t.Fatalf("layers = %v, want %v", layers, want)
	}
	if _, ok := ev["xpEarned"]; ok {
		t.Fatal("xpEarned before final evaluation")
	}
	if ev["fieldsScore"] != float64(100) || ev["timingScore"] != float64(100) {
		t.Fatalf("fields=%v timing=%v", ev["fieldsScore"], ev["timingScore"])
	}
	if tm := ev["timing"].(map[string]any); tm["spentMs"] != float64(20000) || tm["withinNorm"] != true || tm["limitSec"] != float64(45) {
		t.Fatalf("timing = %v", tm)
	}

	// running — из ai_jobs
	f.exec(t, `INSERT INTO ai_jobs (type, status, ref_type, ref_id, payload) VALUES ('evaluate_grammar', 'running', 'attempt', $1, '{}')`, a)
	if l := f.view(t, a)["layers"].(map[string]any); l["grammar"] != "running" || l["semantic"] != "queued" {
		t.Fatalf("layers with running job = %v", l)
	}

	// повторный старт (ретрай submit) — ни второй оценки, ни задач
	f.start(t, a)
	if f.queue.count() != 2 {
		t.Fatalf("jobs after repeated start = %d", f.queue.count())
	}
}

func TestDBStartEvaluationRejectsOpenAttempt(t *testing.T) {
	t.Parallel()
	f := newDB(t)
	lesson := f.lesson(t, core.LessonRunning, `{}`, f.student)
	a := f.attempt(t, lesson, f.student, 1, core.AttemptInProgress, core.ModeCards, nil, nil)
	err := pg.WithTx(ctxT(t), f.pool, func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.StartEvaluation(ctx, tx, a)
	})
	if err == nil || !strings.Contains(err.Error(), "not submitted") {
		t.Fatalf("err = %v, want refusal for in_progress attempt", err)
	}
	if err := pg.WithTx(ctxT(t), f.pool, func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.StartEvaluation(ctx, tx, uuid.New())
	}); err == nil {
		t.Fatal("unknown attempt must fail")
	}
}

func TestDBStartEvaluationWithoutFreeTextFinalizesAtOnce(t *testing.T) {
	t.Parallel()
	f := newDB(t)
	lesson := f.lesson(t, core.LessonRunning, `{"pass_threshold": 70, "cards_per_student": 1}`, f.student)
	a := f.submitted(t, lesson, f.student, 1, cardNoText)

	f.start(t, a)

	if n := f.queue.count(); n != 0 {
		t.Fatalf("no free text — no AI jobs, got %d", n)
	}
	ev := f.view(t, a)
	if ev["status"] != "done" || ev["verdict"] != "fail" || ev["semanticScore"] != float64(0) {
		t.Fatalf("status=%v verdict=%v semantic=%v", ev["status"], ev["verdict"], ev["semanticScore"])
	}
	layers := ev["layers"].(map[string]any)
	if layers["grammar"] != "skipped" || layers["semantic"] != "done" {
		t.Fatalf("layers = %v", layers)
	}
	sem := ev["semantic"].(map[string]any)
	if mf := requireArray(t, sem, "missingFacts"); len(mf) != 2 || sem["summaryForStudent"] != summaryNoDescription {
		t.Fatalf("local semantic = %v", sem)
	}
	fe := requireArray(t, ev, "fieldErrors")
	if len(fe) != 1 || fe[0].(map[string]any)["field"] != "description" || fe[0].(map[string]any)["kind"] != "missing" {
		t.Fatalf("fieldErrors = %v", fe)
	}
	if st := f.str(t, `SELECT status FROM attempts WHERE id = $1`, a); st != core.AttemptEvaluated {
		t.Fatalf("attempt status = %s", st)
	}
	xa := f.xpByAttempt(t, a)
	if !maps.Equal(xa, map[string]int{"attempt_evaluated": 10, "within_norm": 5}) {
		t.Fatalf("attempt XP = %v", xa)
	}
	if ev["xpEarned"] != float64(15) {
		t.Fatalf("xpEarned = %v", ev["xpEarned"])
	}
	if lx := f.xpByLesson(t, lesson); lx["lesson_completed"] != 30 {
		t.Fatalf("single card evaluated — lesson completed: %v", lx)
	}
	if ps := f.pub.monitorOf(public.MonitorMessageTypeParticipantStatus); len(ps) != 1 ||
		ps[0].Msg.Participant.Status != public.ParticipantStatusFinished || ps[0].Msg.Participant.UserId != f.student {
		t.Fatalf("participantStatus = %+v", ps)
	}
}

func TestDBStartEvaluationCardActions(t *testing.T) {
	t.Parallel()
	f := newDB(t)
	lesson := f.lesson(t, core.LessonRunning, `{}`, f.student)
	text := "Направил пожарный расчёт по адресу Тверская, 12"
	a := f.attempt(t, lesson, f.student, 1, core.AttemptSubmitted, core.ModeCardActions, nil, &text)

	f.start(t, a)

	jobs := f.queue.byAttempt(a)
	graw, _ := json.Marshal(jobs[core.JobEvaluateGrammar].Payload)
	sraw, _ := json.Marshal(jobs[core.JobEvaluateSemantic].Payload)
	if !strings.Contains(string(graw), `"field":"actionText"`) || !strings.Contains(string(sraw), `"mode":"card_actions"`) ||
		!strings.Contains(string(sraw), `"action_text":"`+text+`"`) {
		t.Fatalf("payloads:\n%s\n%s", graw, sraw)
	}
	ev := f.view(t, a)
	if ev["layers"].(map[string]any)["fields"] != "skipped" {
		t.Fatalf("fields layer in card_actions = %v", ev["layers"])
	}
	if _, ok := ev["fieldsScore"]; ok {
		t.Fatal("fieldsScore must be absent in card_actions mode")
	}
}

func TestDBVoiceDialogueLayer(t *testing.T) {
	t.Parallel()
	f := newDB(t)
	// lessons пишет эффективные веса при создании (с долей разговора) — оценка их только нормирует
	lesson := f.lesson(t, core.LessonRunning, `{"cards_per_student": 2, "voice": {"enabled": true},
		"weights": {"fields": 0.375, "semantic": 0.1875, "grammar": 0.075, "timing": 0.1125, "dialogue": 0.25}}`, f.student)
	withTalk := f.submitted(t, lesson, f.student, 1, cardFull)
	f.exec(t, `INSERT INTO attempt_dialogue_turns (attempt_id, turn_no, speaker, text, source, at_ms) VALUES
		($1, 0, 'caller', 'Алло! Пожар!', 'script', 0),
		($1, 1, 'operator', 'Какой этаж?', 'text', 4000),
		($1, 1, 'caller', 'Пятый этаж', 'llm', 6000)`, withTalk)
	silent := f.submitted(t, lesson, f.student, 2, cardFull)

	f.start(t, withTalk)
	f.start(t, silent)

	dj, ok := f.queue.byAttempt(withTalk)[core.JobEvaluateDialogue]
	if !ok {
		t.Fatal("dialogue job expected")
	}
	raw, _ := json.Marshal(dj.Payload)
	var p struct {
		Transcript []struct {
			TurnNo  int    `json:"turn_no"`
			Speaker string `json:"speaker"`
		} `json:"transcript"`
		Etalon struct {
			ExpectedDialogue struct {
				Checklist []struct{ ID string } `json:"checklist"`
			} `json:"expected_dialogue"`
		} `json:"etalon"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || len(p.Transcript) != 3 || p.Transcript[0].TurnNo != 1 ||
		p.Transcript[1].TurnNo != 2 || p.Transcript[2].TurnNo != 3 || len(p.Etalon.ExpectedDialogue.Checklist) != 2 {
		t.Fatalf("dialogue payload = %s (%v)", raw, err)
	}
	if _, ok := f.queue.byAttempt(silent)[core.JobEvaluateDialogue]; ok {
		t.Fatal("no conversation — no dialogue job")
	}
	if l := f.view(t, silent)["layers"].(map[string]any); l["dialogue"] != "skipped" {
		t.Fatalf("silent attempt layers = %v", l)
	}
	ev := f.view(t, withTalk)
	if w := ev["weights"].(map[string]any); w["dialogue"] != float64(0.25) {
		t.Fatalf("voice lesson weights = %v", w)
	}

	f.apply(t, withTalk, core.JobEvaluateGrammar, grammarRes(90))
	f.apply(t, withTalk, core.JobEvaluateSemantic, semanticRes(80, 0.9))
	f.apply(t, withTalk, core.JobEvaluateDialogue, &callbacks.AiResult{Dialogue: &components.DialogueResult{
		Score: 55, Confidence: 0.8,
		Checklist: []components.DialogueChecklistResult{
			{Id: "q1", Status: "done", EvidenceTurnNo: ptr(2)},
			{Id: "q2", Status: "missed"},
		},
		SummaryForStudent: ptr("Не спросили о пострадавших."),
	}, Engine: components.Engine{DurationMs: 1200}})

	ev = f.view(t, withTalk)
	if ev["status"] != "done" || ev["dialogueScore"] != float64(55) {
		t.Fatalf("status=%v dialogue=%v", ev["status"], ev["dialogueScore"])
	}
	cl := requireArray(t, ev["dialogue"].(map[string]any), "checklist")
	first, second := cl[0].(map[string]any), cl[1].(map[string]any)
	if first["text"] != "Уточнить этаж" || first["evidenceTurnNo"] != float64(1) || second["status"] != "missed" {
		t.Fatalf("checklist = %v", cl)
	}
}

// ---------------------------------------------------------------- доезд результатов

func TestDBResultsFlow(t *testing.T) {
	t.Parallel()
	f := newDB(t)
	lesson := f.lesson(t, core.LessonRunning, `{"pass_threshold": 70, "cards_per_student": 1}`, f.student)
	a := f.submitted(t, lesson, f.student, 1, cardFull)
	f.start(t, a)
	f.pub.reset()

	f.apply(t, a, core.JobEvaluateGrammar, grammarRes(90))
	ev := f.view(t, a)
	if ev["status"] != "partial" || ev["grammarScore"] != float64(90) || ev["layers"].(map[string]any)["grammar"] != "done" {
		t.Fatalf("after grammar: %v", ev)
	}
	if n := len(f.pub.studentFor(a)); n != 1 {
		t.Fatalf("student pushes after grammar = %d", n)
	}

	// Регрессия: confidence ровно на пороге (0.7) — не «требует ревью».
	f.apply(t, a, core.JobEvaluateSemantic, semanticRes(80, 0.7))
	ev = f.view(t, a)
	if ev["status"] != "done" || ev["needsReview"] != false || ev["aiUnavailable"] != false {
		t.Fatalf("after semantic: status=%v needsReview=%v", ev["status"], ev["needsReview"])
	}
	// 100·0.5 + 80·0.25 + 90·0.1 + 100·0.15 = 94
	if ev["totalScore"] != float64(94) || ev["finalScore"] != float64(94) || ev["verdict"] != "pass" {
		t.Fatalf("total=%v final=%v verdict=%v", ev["totalScore"], ev["finalScore"], ev["verdict"])
	}
	if st := f.str(t, `SELECT status FROM attempts WHERE id = $1`, a); st != core.AttemptEvaluated {
		t.Fatalf("attempt status = %s", st)
	}
	wantXP := map[string]int{"attempt_evaluated": 10, "within_norm": 5, "pass_bonus": 9}
	if got := f.xpByAttempt(t, a); !maps.Equal(got, wantXP) {
		t.Fatalf("attempt XP = %v, want %v", got, wantXP)
	}
	if ev["xpEarned"] != float64(24) {
		t.Fatalf("xpEarned = %v (lesson_completed не входит: не привязан к попытке)", ev["xpEarned"])
	}
	if lx := f.xpByLesson(t, lesson); lx["lesson_completed"] != 30 {
		t.Fatalf("lesson XP = %v", lx)
	}
	requireArray(t, ev, "recommendations")

	// engine — camelCase на проводе, perField — пути формы АРМ
	eng := ev["engine"].(map[string]any)
	walkKeys(eng, func(k string) {
		if strings.Contains(k, "_") {
			t.Errorf("snake_case key %q in engine %v", k, eng)
		}
	})
	if eng["rulesVersion"] != rulesVersion || eng["semantic"].(map[string]any)["promptVersion"] != "sem-v3" ||
		eng["grammar"].(map[string]any)["durationMs"] != float64(35) {
		t.Fatalf("engine = %v", eng)
	}
	pf := requireArray(t, ev["semantic"].(map[string]any), "perField")
	if pf[0].(map[string]any)["field"] != "description" || pf[1].(map[string]any)["field"] != "actionsTaken" {
		t.Fatalf("perField = %v", pf)
	}
	gr := ev["grammar"].(map[string]any)
	if rm := requireArray(t, gr, "remarks"); len(rm) != 1 || rm[0].(map[string]any)["field"] != "description" {
		t.Fatalf("grammar remarks = %v", rm)
	}

	// дубль результата (переотправка) — ничего не меняет
	pushes := len(f.pub.studentFor(a))
	f.apply(t, a, core.JobEvaluateGrammar, grammarRes(10))
	if ev2 := f.view(t, a); ev2["grammarScore"] != float64(90) || ev2["totalScore"] != float64(94) {
		t.Fatalf("duplicate result changed the evaluation: %v", ev2)
	}
	if len(f.pub.studentFor(a)) != pushes {
		t.Fatal("duplicate result must not push")
	}
	if got := f.xpByAttempt(t, a); !maps.Equal(got, wantXP) {
		t.Fatalf("XP after duplicate = %v", got)
	}
}

func TestDBLowConfidenceNeedsReview(t *testing.T) {
	t.Parallel()
	f := newDB(t)
	lesson := f.lesson(t, core.LessonRunning, `{}`, f.student)
	a := f.submitted(t, lesson, f.student, 1, cardFull)
	f.start(t, a)
	f.apply(t, a, core.JobEvaluateSemantic, semanticRes(80, 0.69))
	if ev := f.view(t, a); ev["needsReview"] != true || ev["aiUnavailable"] != false {
		t.Fatalf("needsReview=%v aiUnavailable=%v", ev["needsReview"], ev["aiUnavailable"])
	}
}

func TestDBFailuresAndIgnoredResults(t *testing.T) {
	t.Parallel()
	f := newDB(t)
	lesson := f.lesson(t, core.LessonRunning, `{"cards_per_student": 1}`, f.student)
	a := f.submitted(t, lesson, f.student, 1, cardFull)
	f.start(t, a)

	f.failLayer(t, a, core.JobEvaluateGrammar, "timeout")
	ev := f.view(t, a)
	if ev["status"] != "partial" || ev["aiUnavailable"] != true || ev["needsReview"] != true ||
		ev["layers"].(map[string]any)["grammar"] != "failed" {
		t.Fatalf("after grammar failure: %v", ev)
	}
	if e := ev["engine"].(map[string]any)["errors"].(map[string]any); e["grammar"] != "timeout" {
		t.Fatalf("engine.errors = %v", e)
	}

	// непригодный результат (балл не число) — слой failed, не ретрай
	bad := semanticRes(0, 0.9)
	bad.Semantic.Score = float32(nanF())
	f.apply(t, a, core.JobEvaluateSemantic, bad)
	ev = f.view(t, a)
	if ev["status"] != "done" || ev["layers"].(map[string]any)["semantic"] != "failed" {
		t.Fatalf("after invalid semantic: %v", ev)
	}
	if e := ev["engine"].(map[string]any)["errors"].(map[string]any); e["semantic"] != codeInvalidResult {
		t.Fatalf("engine.errors = %v", e)
	}
	if _, ok := ev["semanticScore"]; ok {
		t.Fatal("failed layer has no score")
	}
	// итог — по доступным слоям (fields и timing по 100)
	if ev["totalScore"] != float64(100) || ev["verdict"] != "pass" {
		t.Fatalf("total=%v verdict=%v", ev["totalScore"], ev["verdict"])
	}

	// провал уже терминального слоя (reaper после результата) — пропуск
	f.failLayer(t, a, core.JobEvaluateSemantic, "reaper")
	if e := f.view(t, a)["engine"].(map[string]any)["errors"].(map[string]any); e["semantic"] != codeInvalidResult {
		t.Fatalf("settled layer changed: %v", e)
	}

	// попытка без оценки и задача без цели — без ошибок и без эффектов
	noEval := f.attempt(t, lesson, f.student2, 1, core.AttemptInProgress, core.ModeCards, nil, nil)
	f.apply(t, noEval, core.JobEvaluateGrammar, grammarRes(50))
	f.failLayer(t, noEval, core.JobEvaluateGrammar, "timeout")
	if err := pg.WithTx(ctxT(t), f.pool, func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.ApplyResult(ctx, tx, core.JobRecord{ID: uuid.New(), Type: core.JobEvaluateGrammar, RefType: "scenario", RefID: uuid.New()}, grammarRes(50))
	}); err != nil {
		t.Fatalf("job without evaluation target: %v", err)
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM evaluations`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("evaluations = %d (%v)", n, err)
	}
}

func nanF() float64 {
	z := 0.0
	return z / z
}

// ---------------------------------------------------------------- lesson_completed

// Регрессия: lesson_completed не зависит от того, доехал AI-слой до или после завершения
// занятия. Было: оценка после finish давала +30 (вторая карточка уже expired, «занятие не
// идёт»), оценка до finish — нет (вторая карточка ещё открыта, потом её гасил finish).
func TestDBLessonCompletedDoesNotDependOnAILatency(t *testing.T) {
	t.Parallel()
	f := newDB(t)
	run := func(finishFirst bool) map[string]int {
		lesson := f.lesson(t, core.LessonRunning, `{"pass_threshold": 60, "cards_per_student": 2}`, f.student)
		a1 := f.submitted(t, lesson, f.student, 1, cardFull)
		f.attempt(t, lesson, f.student, 2, core.AttemptIssued, core.ModeCards, nil, nil) // IssueNext после submit
		f.start(t, a1)
		if finishFirst {
			f.finishLesson(t, lesson)
		}
		f.apply(t, a1, core.JobEvaluateGrammar, grammarRes(90))
		f.apply(t, a1, core.JobEvaluateSemantic, semanticRes(80, 0.9))
		if !finishFirst {
			f.finishLesson(t, lesson)
		}
		return f.xpByLesson(t, lesson)
	}
	evalFirst := run(false)
	finishFirst := run(true)
	if !maps.Equal(evalFirst, finishFirst) {
		t.Fatalf("XP depends on AI latency: evaluated before finish %v, after finish %v", evalFirst, finishFirst)
	}
	if _, ok := finishFirst["lesson_completed"]; ok {
		t.Fatalf("one of two cards done — lesson not completed: %v", finishFirst)
	}
}

func TestDBLessonCompletedWhenAllCardsEvaluated(t *testing.T) {
	t.Parallel()
	f := newDB(t)
	lesson := f.lesson(t, core.LessonRunning, `{"cards_per_student": 2}`, f.student)
	a1 := f.submitted(t, lesson, f.student, 1, cardFull)
	a2 := f.submitted(t, lesson, f.student, 2, cardFull)
	f.start(t, a1)
	f.start(t, a2)

	f.apply(t, a1, core.JobEvaluateGrammar, grammarRes(90))
	f.apply(t, a1, core.JobEvaluateSemantic, semanticRes(80, 0.9))
	if lx := f.xpByLesson(t, lesson); lx["lesson_completed"] != 0 {
		t.Fatalf("card 2 still on review — not completed: %v", lx)
	}
	if ps := f.pub.monitorOf(public.MonitorMessageTypeParticipantStatus); len(ps) != 0 {
		t.Fatalf("participant must not finish yet: %+v", ps)
	}

	// занятие завершено, пока карточка 2 на проверке: сданная карточка не гаснет и выполнена
	f.finishLesson(t, lesson)
	f.apply(t, a2, core.JobEvaluateGrammar, grammarRes(70))
	f.apply(t, a2, core.JobEvaluateSemantic, semanticRes(60, 0.9))
	lx := f.xpByLesson(t, lesson)
	if lx["lesson_completed"] != 30 {
		t.Fatalf("all cards evaluated — lesson completed: %v", lx)
	}
}

func TestDBLessonCompletedWhileRunning(t *testing.T) {
	t.Parallel()
	f := newDB(t)
	lesson := f.lesson(t, core.LessonRunning, `{"cards_per_student": 2}`, f.student)
	a1 := f.submitted(t, lesson, f.student, 1, cardNoText) // оценка готова сразу, карточка 2 ещё не выдана
	f.start(t, a1)
	if lx := f.xpByLesson(t, lesson); lx["lesson_completed"] != 0 {
		t.Fatalf("1 of 2 — not completed: %v", lx)
	}
	a2 := f.submitted(t, lesson, f.student, 2, cardNoText)
	f.start(t, a2)
	if lx := f.xpByLesson(t, lesson); lx["lesson_completed"] != 30 {
		t.Fatalf("2 of 2 — completed: %v", lx)
	}
	ps := f.pub.monitorOf(public.MonitorMessageTypeParticipantStatus)
	if len(ps) != 1 || ps[0].Msg.Participant.Status != public.ParticipantStatusFinished || *ps[0].Msg.AttemptId != a2 {
		t.Fatalf("participantStatus = %+v", ps)
	}
	if st := f.str(t, `SELECT status FROM lesson_participants WHERE lesson_id = $1 AND user_id = $2`, lesson, f.student); st != core.ParticipantFinished {
		t.Fatalf("participant = %s", st)
	}
}

// ---------------------------------------------------------------- ручная корректировка

// Регрессия: балл корректировки не округляется до целого — 59.5 при пороге 60 не «зачёт».
func TestDBOverrideKeepsHundredths(t *testing.T) {
	t.Parallel()
	f := newDB(t)
	lesson := f.lesson(t, core.LessonRunning, `{"pass_threshold": 60}`, f.student)
	a := f.submitted(t, lesson, f.student, 1, cardNoText)
	f.start(t, a)

	r := f.override(t, a, "teacher", 59.5)
	if r.Code != http.StatusOK {
		t.Fatalf("override: %d %s", r.Code, r.Body)
	}
	ev := r.json(t)
	ov := ev["override"].(map[string]any)
	if ov["score"] != 59.5 || ov["verdict"] != "fail" || ev["finalScore"] != 59.5 || ev["verdict"] != "fail" {
		t.Fatalf("override=%v final=%v verdict=%v", ov, ev["finalScore"], ev["verdict"])
	}
	if s := f.str(t, `SELECT override_score::text || '/' || final_score::text || '/' || override_verdict FROM evaluations WHERE attempt_id = $1`, a); s != "59.50/59.50/fail" {
		t.Fatalf("db = %s", s)
	}
	for _, tc := range []struct {
		score   float64
		stored  float64
		verdict string
	}{{59.994, 59.99, "fail"}, {59.996, 60, "pass"}, {60, 60, "pass"}, {0, 0, "fail"}, {100, 100, "pass"}} {
		ev := f.override(t, a, "teacher", tc.score).json(t)
		if ov := ev["override"].(map[string]any); ov["score"] != tc.stored || ov["verdict"] != tc.verdict {
			t.Errorf("override %v: %v, want %v %s", tc.score, ov, tc.stored, tc.verdict)
		}
	}
}

// Регрессия: XP следует итогу с корректировкой одинаково — корректировка до финализации
// (учитывалась) и после (раньше pass_bonus не появлялся никогда).
func TestDBOverrideXPDoesNotDependOnOrder(t *testing.T) {
	t.Parallel()
	f := newDB(t)
	// порог 90: без корректировки итог ≤ 78.5 — «не зачтено»
	lesson := f.lesson(t, core.LessonRunning, `{"pass_threshold": 90, "cards_per_student": 1}`, f.student, f.student2)

	after := f.submitted(t, lesson, f.student, 1, cardFull)
	f.start(t, after)
	f.apply(t, after, core.JobEvaluateGrammar, grammarRes(10))
	f.apply(t, after, core.JobEvaluateSemantic, semanticRes(50, 0.9))
	if ev := f.view(t, after); ev["verdict"] != "fail" || ev["xpEarned"] != float64(15) {
		t.Fatalf("auto: verdict=%v xp=%v", ev["verdict"], ev["xpEarned"])
	}
	r := f.override(t, after, "teacher", 95)
	if r.Code != http.StatusOK {
		t.Fatalf("override: %d %s", r.Code, r.Body)
	}
	if ev := r.json(t); ev["verdict"] != "pass" || ev["xpEarned"] != float64(24) {
		t.Fatalf("override after final: verdict=%v xpEarned=%v", ev["verdict"], ev["xpEarned"])
	}

	before := f.submitted(t, lesson, f.student2, 1, cardFull)
	f.start(t, before)
	f.apply(t, before, core.JobEvaluateGrammar, grammarRes(10))
	if r := f.override(t, before, "teacher", 95); r.Code != http.StatusOK {
		t.Fatalf("override partial: %d %s", r.Code, r.Body)
	} else if _, ok := r.json(t)["xpEarned"]; ok {
		t.Fatal("partial evaluation has no xpEarned yet")
	}
	f.apply(t, before, core.JobEvaluateSemantic, semanticRes(50, 0.9))

	xa, xb := f.xpByAttempt(t, after), f.xpByAttempt(t, before)
	if !maps.Equal(xa, xb) {
		t.Fatalf("XP depends on override order: after final %v, before final %v", xa, xb)
	}
	if xa["pass_bonus"] != 9 {
		t.Fatalf("pass_bonus = %v", xa)
	}

	// pass → fail снимает бонус; новый балл — пересчитывает; ответ = журнал
	steps := []struct {
		score   float64
		verdict string
		bonus   int
	}{{40, "fail", 0}, {100, "pass", 10}, {92.5, "pass", 9}, {95, "pass", 9}}
	for _, st := range steps {
		ev := f.override(t, after, "teacher", st.score).json(t)
		got := f.xpByAttempt(t, after)
		if ev["verdict"] != st.verdict || got["pass_bonus"] != st.bonus || ev["xpEarned"] != float64(sum(got)) {
			t.Fatalf("override %v: verdict=%v xpEarned=%v ledger=%v, want bonus %d", st.score, ev["verdict"], ev["xpEarned"], got, st.bonus)
		}
		if gv := f.view(t, after); gv["xpEarned"] != ev["xpEarned"] {
			t.Fatalf("GET xpEarned %v != override response %v", gv["xpEarned"], ev["xpEarned"])
		}
	}
	if got := f.xpByAttempt(t, after); got["attempt_evaluated"] != 10 || got["within_norm"] != 5 {
		t.Fatalf("other entries must not change: %v", got)
	}

	// аудит: before/after с XP
	entries := f.aud.byAction("evaluation.override")
	if len(entries) != 2+len(steps) {
		t.Fatalf("audit entries = %d", len(entries))
	}
	first := entries[0]
	b, _ := first.Before.(overrideAudit)
	a, _ := first.After.(overrideAudit)
	if first.EntityType != "evaluation" || first.LessonID != lesson || b.Verdict != "fail" || a.Verdict != "pass" ||
		b.XPEarned != 15 || a.XPEarned != 24 || a.OverrideScore == nil || *a.OverrideScore != 95 {
		t.Fatalf("audit = %+v before=%+v after=%+v", first, b, a)
	}
}

// ---------------------------------------------------------------- доступ к оценке

func TestDBEvaluationAccess(t *testing.T) {
	t.Parallel()
	f := newDB(t)
	lesson := f.lesson(t, core.LessonRunning, `{}`, f.student, f.student2)
	a := f.submitted(t, lesson, f.student, 1, cardNoText)
	f.start(t, a)
	open := f.attempt(t, lesson, f.student2, 1, core.AttemptInProgress, core.ModeCards, nil, nil)
	path := func(id uuid.UUID) string { return "/api/v1/attempts/" + id.String() + "/evaluation" }

	for _, tc := range []struct {
		user   string
		id     uuid.UUID
		status int
		msg    string
	}{
		{"student", a, 200, ""},
		{"teacher", a, 200, ""},
		{"admin", a, 200, ""},
		{"student2", a, 403, ""},
		{"teacher2", a, 403, ""},
		{"admin", uuid.New(), 404, "Попытка не найдена"},
		{"student2", open, 404, "Оценки ещё нет"},
		{"student", open, 403, ""},
	} {
		r := do(f.h, http.MethodGet, path(tc.id), tc.user, nil, false)
		if r.Code != tc.status {
			t.Errorf("%s GET %s: %d %s, want %d", tc.user, tc.id, r.Code, r.Body, tc.status)
			continue
		}
		if tc.msg != "" {
			if _, msg, _ := apiErr(t, r); msg != tc.msg {
				t.Errorf("%s: message %q, want %q", tc.user, msg, tc.msg)
			}
		}
		if tc.status == 200 {
			ev := r.json(t)
			requireKeys(t, ev, "attemptId", "status", "totalScore", "verdict", "fieldErrors", "timing", "needsReview",
				"aiUnavailable", "recommendations", "finalScore", "weights")
			requireArray(t, ev, "fieldErrors")
			requireArray(t, ev, "recommendations")
		}
	}

	ov := func(id uuid.UUID) string { return "/api/v1/attempts/" + id.String() + "/evaluation/override" }
	body := map[string]any{"score": 88, "reason": "Уточнено по записи разговора"}
	for _, tc := range []struct {
		user   string
		id     uuid.UUID
		status int
	}{
		{"teacher2", a, 403},
		{"teacher", uuid.New(), 404},
		{"teacher", open, 404},
		{"teacher2", open, 403},
		{"admin", a, 409}, // ТЗ: администратор не меняет оценки во время активного занятия
	} {
		if r := do(f.h, http.MethodPost, ov(tc.id), tc.user, body, true); r.Code != tc.status {
			t.Errorf("%s override %s: %d %s, want %d", tc.user, tc.id, r.Code, r.Body, tc.status)
		}
	}
	if v := f.view(t, a); v["override"] != nil {
		t.Fatalf("оценка изменена администратором во время занятия: %v", v["override"])
	}
	// После завершения занятия администратор может скорректировать оценку.
	f.finishLesson(t, lesson)
	if r := do(f.h, http.MethodPost, ov(a), "admin", body, true); r.Code != 200 {
		t.Fatalf("admin override после завершения: %d %s", r.Code, r.Body)
	}
	ev := f.view(t, a)
	o := ev["override"].(map[string]any)
	if o["by"] != "Админов С. И." || o["reason"] != "Уточнено по записи разговора" || o["score"] != float64(88) {
		t.Fatalf("override = %v", o)
	}
	if _, err := time.Parse(time.RFC3339Nano, o["at"].(string)); err != nil {
		t.Fatalf("override.at = %v", o["at"])
	}
	if n := len(f.aud.byAction("evaluation.override")); n != 1 {
		t.Fatalf("denied overrides must not be audited: %d entries", n)
	}
	if m := f.pub.monitorOf(public.MonitorMessageTypeEvaluationUpdated); m[len(m)-1].Msg.Evaluation.Override == nil {
		t.Fatal("override must be pushed to the monitor")
	}

	// View для других доменов
	if v, err := f.svc.View(ctxT(t), f.pool, a); err != nil || v.AttemptId != a || v.Override == nil {
		t.Fatalf("View = %+v, %v", v, err)
	}
	if _, err := f.svc.View(ctxT(t), f.pool, open); !pg.IsNoRows(err) {
		t.Fatalf("View without evaluation: %v", err)
	}
	if _, err := f.svc.View(ctxT(t), f.pool, uuid.New()); !pg.IsNoRows(err) {
		t.Fatalf("View unknown attempt: %v", err)
	}
}

// ---------------------------------------------------------------- комментарии преподавателя

func TestDBFeedback(t *testing.T) {
	t.Parallel()
	f := newDB(t)
	lesson := f.lesson(t, core.LessonRunning, `{}`, f.student, f.student2)
	a := f.submitted(t, lesson, f.student, 1, cardNoText)
	other := f.submitted(t, lesson, f.student2, 1, cardNoText)
	path := func(id uuid.UUID) string { return "/api/v1/attempts/" + id.String() + "/feedback" }

	// пусто — [] (не null)
	if r := do(f.h, http.MethodGet, path(a), "student", nil, false); r.Code != 200 || strings.TrimSpace(string(r.Body)) != "[]" {
		t.Fatalf("empty list: %d %s", r.Code, r.Body)
	}

	r := do(f.h, http.MethodPost, path(a), "teacher", map[string]any{
		"field": " address.raw ", "comment": "  Адрес записан без номера дома  ", "recommendation": "Переспрашивайте номер дома",
	}, true)
	if r.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", r.Code, r.Body)
	}
	fb := r.json(t)
	requireKeys(t, fb, "id", "attemptId", "teacherName", "comment", "createdAt")
	if fb["attemptId"] != a.String() || fb["teacherName"] != "Учителев П. И." || fb["comment"] != "Адрес записан без номера дома" ||
		fb["field"] != "address.raw" || fb["recommendation"] != "Переспрашивайте номер дома" {
		t.Fatalf("feedback = %v", fb)
	}
	r = do(f.h, http.MethodPost, path(a), "admin", map[string]any{"comment": "Общий комментарий", "field": "", "recommendation": "  "}, true)
	if r.Code != http.StatusCreated {
		t.Fatalf("admin add: %d %s", r.Code, r.Body)
	}
	if fb := r.json(t); fb["field"] != nil || fb["recommendation"] != nil {
		t.Fatalf("empty optional fields must be absent: %v", fb)
	}
	// скрытый от студента комментарий
	f.exec(t, `INSERT INTO teacher_feedback (attempt_id, teacher_id, comment, is_visible_to_student) VALUES ($1, $2, 'Только для себя', false)`, a, f.teacher)

	for _, tc := range []struct {
		user   string
		id     uuid.UUID
		status int
	}{
		{"teacher2", a, 403},
		{"teacher", uuid.New(), 404},
	} {
		if r := do(f.h, http.MethodPost, path(tc.id), tc.user, map[string]any{"comment": "Замечание"}, true); r.Code != tc.status {
			t.Errorf("%s add to %s: %d %s, want %d", tc.user, tc.id, r.Code, r.Body, tc.status)
		}
	}
	if n := len(f.aud.byAction("feedback.add")); n != 2 {
		t.Fatalf("feedback.add audit entries = %d", n)
	}

	student := do(f.h, http.MethodGet, path(a), "student", nil, false)
	teacher := do(f.h, http.MethodGet, path(a), "teacher", nil, false)
	if student.Code != 200 || teacher.Code != 200 {
		t.Fatalf("list: %d %d", student.Code, teacher.Code)
	}
	sl, tl := student.list(t), teacher.list(t)
	if len(sl) != 2 || len(tl) != 3 {
		t.Fatalf("student sees %d (want 2 visible), teacher sees %d (want 3)", len(sl), len(tl))
	}
	if sl[0]["comment"] != "Адрес записан без номера дома" || sl[1]["teacherName"] != "Админов С. И." {
		t.Fatalf("order/content = %v", sl)
	}
	for _, it := range sl {
		if it["comment"] == "Только для себя" {
			t.Fatal("hidden feedback leaked to the student")
		}
	}

	for _, tc := range []struct {
		user   string
		id     uuid.UUID
		status int
	}{
		{"student2", a, 403},
		{"teacher2", a, 403},
		{"admin", a, 200},
		{"student2", other, 200},
		{"admin", uuid.New(), 404},
	} {
		if r := do(f.h, http.MethodGet, path(tc.id), tc.user, nil, false); r.Code != tc.status {
			t.Errorf("%s list %s: %d %s, want %d", tc.user, tc.id, r.Code, r.Body, tc.status)
		}
	}
}
