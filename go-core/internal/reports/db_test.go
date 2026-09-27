package reports

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/core"
	"lct/gocore/internal/platform/pgtest"
)

// fixture — занятие преподавателя с попытками всех видов.
type fixture struct {
	pool                          *pgxpool.Pool
	teacher, other, admin         uuid.UUID
	andreev, borisov, vasiliev    uuid.UUID
	grigoriev                     uuid.UUID // попытка без участия (самостоятельно/удалён из списка)
	lesson, practice, empty       uuid.UUID
	attemptA1, attemptA2, attempB uuid.UUID
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
}

func addUser(t *testing.T, pool *pgxpool.Pool, role, last, first string, middle *string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	exec(t, pool, `INSERT INTO users (id, login, password_hash, role, last_name, first_name, middle_name)
		VALUES ($1, $2, 'x', $3, $4, $5, $6)`, id, "u"+id.String()[:8], role, last, first, middle)
	return id
}

func sp(s string) *string { return &s }

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := pgtest.New(t)
	f := &fixture{pool: pool}
	f.teacher = addUser(t, pool, "teacher", "Учителев", "Иван", sp("Петрович"))
	f.other = addUser(t, pool, "teacher", "Другой", "Пётр", nil)
	f.admin = addUser(t, pool, "admin", "Админов", "Админ", nil)
	f.andreev = addUser(t, pool, "student", "Андреев", "Андрей", sp("Андреевич"))
	f.borisov = addUser(t, pool, "student", "Борисов", "Борис", nil)
	f.vasiliev = addUser(t, pool, "student", "Васильев", "Василий", nil)
	f.grigoriev = addUser(t, pool, "student", "Григорьев", "Григорий", nil)

	cat := uuid.New()
	exec(t, pool, `INSERT INTO classifier_categories (id, code, name) VALUES ($1, $2, 'Пожар')`, cat, "c"+cat.String()[:8])
	s1, s2 := uuid.New(), uuid.New()
	exec(t, pool, `INSERT INTO scenarios (id, title, category_id, source, status) VALUES ($1, 'Пожар в квартире', $2, 'manual', 'validated')`, s1, cat)
	exec(t, pool, `INSERT INTO scenarios (id, title, category_id, source, status, version, parent_id) VALUES ($1, 'ДТП', $2, 'manual', 'validated', 2, $3)`, s2, cat, s1)
	e1, e2 := uuid.New(), uuid.New()
	exec(t, pool, `INSERT INTO etalons (id, scenario_id, card) VALUES ($1, $2, '{}'), ($3, $4, '{}')`, e1, s1, e2, s2)

	started := time.Date(2026, 9, 24, 7, 0, 0, 0, time.UTC)
	f.lesson = uuid.New()
	exec(t, pool, `INSERT INTO lessons (id, kind, title, teacher_id, created_by, mode, time_limit_sec, settings, status, started_at, finished_at)
		VALUES ($1, 'class', 'Итоговое занятие', $2, $2, 'cards', 45,
		        '{"pass_threshold": 60, "cards_per_student": 2, "voice": {"enabled": true}}', 'finished', $3, $4)`,
		f.lesson, f.teacher, started, started.Add(time.Hour))
	exec(t, pool, `INSERT INTO lesson_scenarios (lesson_id, scenario_id, sort_order) VALUES ($1, $2, 1), ($1, $3, 0)`, f.lesson, s1, s2)
	exec(t, pool, `INSERT INTO lesson_participants (lesson_id, user_id) VALUES ($1, $2), ($1, $3), ($1, $4)`,
		f.lesson, f.andreev, f.borisov, f.vasiliev)

	acc := started.Add(time.Minute)
	f.attemptA1, f.attemptA2, f.attempB = uuid.New(), uuid.New(), uuid.New()
	g := uuid.New()
	exec(t, pool, `INSERT INTO attempts (id, lesson_id, user_id, scenario_id, etalon_id, mode, seq_no, status, time_limit_sec,
		call_accepted_at, first_input_at, submitted_at, time_spent_ms, replay_count, dialogue_turns, incident_no)
		VALUES ($1, $2, $3, $4, $5, 'cards', 1, 'evaluated', 45, $6, $7, $8, 25000, 1, 4, 100)`,
		f.attemptA1, f.lesson, f.andreev, s1, e1, acc, acc.Add(2*time.Second), acc.Add(25*time.Second))
	exec(t, pool, `INSERT INTO attempts (id, lesson_id, user_id, scenario_id, etalon_id, mode, seq_no, status, time_limit_sec, incident_no)
		VALUES ($1, $2, $3, $4, $5, 'cards', 2, 'in_progress', 45, 101)`, f.attemptA2, f.lesson, f.andreev, s2, e2)
	exec(t, pool, `INSERT INTO attempts (id, lesson_id, user_id, scenario_id, etalon_id, mode, seq_no, status, time_limit_sec, incident_no)
		VALUES ($1, $2, $3, $4, $5, 'cards', 1, 'evaluating', 45, 102)`, f.attempB, f.lesson, f.borisov, s2, e2)
	exec(t, pool, `INSERT INTO attempts (id, lesson_id, user_id, scenario_id, etalon_id, mode, seq_no, status, time_limit_sec, incident_no)
		VALUES ($1, $2, $3, $4, $5, 'cards', 1, 'expired', 45, 103)`, g, f.lesson, f.grigoriev, s1, e1)

	exec(t, pool, `INSERT INTO evaluations (attempt_id, etalon_id, status, fields_score, grammar_score, semantic_score, timing_score,
		total_score, verdict, field_errors, timing)
		VALUES ($1, $2, 'done', 80.4, 90, 69.5, 100, 82.49, 'pass',
		  '[{"field": "address", "label": "Адрес", "kind": "missing", "weight": 0.3},
		    {"field": "phone", "label": "", "kind": "wrong", "weight": 0.5},
		    {"field": "floor", "kind": "extra"},
		    "мусор"]',
		  '{"within_norm": true}')`, f.attemptA1, e1)
	exec(t, pool, `INSERT INTO evaluations (attempt_id, etalon_id, status, total_score, verdict, needs_review, ai_unavailable,
		override_score, override_verdict, override_reason, overridden_by, overridden_at, field_errors, timing)
		VALUES ($1, $2, 'partial', 40, 'fail', true, true, 75, 'pass', 'Учтён шум на линии', $3, now(), '{}', '{"within_norm": "да"}')`,
		f.attempB, e2, f.teacher)

	// самостоятельная практика студента: владелец — сам студент, участников нет
	f.practice = uuid.New()
	exec(t, pool, `INSERT INTO lessons (id, kind, title, created_by, mode) VALUES ($1, 'practice', 'Практика', $2, 'cards')`, f.practice, f.andreev)
	exec(t, pool, `INSERT INTO attempts (lesson_id, user_id, scenario_id, etalon_id, mode, seq_no, time_limit_sec)
		VALUES ($1, $2, $3, $4, 'cards', 1, 30)`, f.practice, f.andreev, s1, e1)

	f.empty = uuid.New()
	exec(t, pool, `INSERT INTO lessons (id, title, teacher_id, created_by, mode, settings) VALUES ($1, 'Пустое', $2, $2, 'card_actions', '"битые"')`,
		f.empty, f.teacher)
	return f
}

func allow(*lessonMeta) error { return nil }

func TestLoadReport(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	meta, rows, truncated, err := loadReport(context.Background(), f.pool, f.lesson, allow)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Error("truncated")
	}
	if meta.ID != f.lesson || meta.Title != "Итоговое занятие" || meta.Teacher != "Учителев И. П." || meta.Participants != 3 ||
		meta.TimeLimitSec != 45 || meta.Status != core.LessonFinished || meta.Kind != core.LessonKindClass {
		t.Errorf("meta = %+v", meta)
	}
	if meta.Settings.PassThreshold != 60 || !meta.Settings.Voice.Enabled || meta.Settings.CardsPerStudent != 2 {
		t.Errorf("settings = %+v", meta.Settings)
	}
	if len(meta.Scenarios) != 2 || meta.Scenarios[0] != "ДТП (версия 2)" || meta.Scenarios[1] != "Пожар в квартире" {
		t.Errorf("сценарии по sort_order = %q", meta.Scenarios)
	}
	if meta.StartedAt == nil || meta.StartedAt.Location() != time.UTC || meta.CreatedAt.Location() != time.UTC {
		t.Errorf("времена не в UTC: %v %v", meta.StartedAt, meta.CreatedAt)
	}

	// участники ∪ владельцы попыток, по ФИО; у одного человека — по номеру карточки
	want := []struct {
		name string
		seq  int
	}{{"Андреев Андрей Андреевич", 1}, {"Андреев Андрей Андреевич", 2}, {"Борисов Борис", 1}, {"Васильев Василий", 0}, {"Григорьев Григорий", 1}}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d: %+v", len(rows), rows)
	}
	for i, w := range want {
		if rows[i].FullName != w.name || rows[i].SeqNo != w.seq {
			t.Errorf("row %d = %s #%d, want %s #%d", i, rows[i].FullName, rows[i].SeqNo, w.name, w.seq)
		}
	}

	a := rows[0]
	if !a.Attempt || a.AttemptID != f.attemptA1 || a.Status != core.AttemptEvaluated || a.IncidentNo != 100 ||
		a.TimeLimitSec != 45 || a.ReplayCount != 1 || a.DialogueTurns != 4 || a.Scenario != "Пожар в квартире" || a.Category != "Пожар" {
		t.Errorf("Андреев #1 = %+v", a)
	}
	if a.SpentMs == nil || *a.SpentMs != 25000 || a.ReactionMs() == nil || *a.ReactionMs() != 2000 {
		t.Errorf("тайминг = %v %v", a.SpentMs, a.ReactionMs())
	}
	// баллы — целые
	if *a.Fields != 80 || *a.Semantic != 70 || *a.Total != 82 || *a.Final != 82 || a.Grammar == nil || a.Dialogue != nil {
		t.Errorf("баллы = f%v s%v t%v fin%v", *a.Fields, *a.Semantic, *a.Total, *a.Final)
	}
	if !a.Evaluated() || a.Verdict != core.VerdictPass || a.WithinNorm == nil || !*a.WithinNorm || a.Overridden {
		t.Errorf("оценка = %+v", a)
	}
	// ошибки по весу; пустая подпись — имя поля; не-объекты не ломают разбор и не дают пустых строк
	wantErr := []string{"phone (неверно)", "Адрес (не заполнено)", "floor (лишнее)"}
	if len(a.Errors) != len(wantErr) {
		t.Fatalf("errors = %q", a.Errors)
	}
	for i, w := range wantErr {
		if a.Errors[i] != w {
			t.Errorf("errors[%d] = %q, want %q", i, a.Errors[i], w)
		}
	}

	a2 := rows[1]
	if !a2.Attempt || a2.Status != core.AttemptInProgress || a2.EvalStatus != "" || a2.Final != nil || a2.Errors != nil ||
		a2.Scenario != "ДТП (версия 2)" || a2.CallAcceptedAt != nil {
		t.Errorf("Андреев #2 = %+v", a2)
	}

	b := rows[2]
	if !b.Overridden || b.OverrideRsn != "Учтён шум на линии" || b.Final == nil || *b.Final != 75 || *b.Total != 40 ||
		b.Verdict != core.VerdictPass || !b.NeedsReview || !b.AIUnavailable || !b.Evaluated() {
		t.Errorf("Борисов = %+v", b)
	}
	if b.WithinNorm != nil || b.Errors != nil {
		t.Errorf("нестандартный timing/field_errors: within=%v errors=%q", b.WithinNorm, b.Errors)
	}

	v := rows[3]
	if v.Attempt || v.UserID != f.vasiliev || v.Short != "Васильев В." {
		t.Errorf("Васильев = %+v", v)
	}
	if g := rows[4]; g.Status != core.AttemptExpired || g.UserID != f.grigoriev {
		t.Errorf("Григорьев = %+v", g)
	}
}

func TestLoadReportPracticeAndEmpty(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	meta, rows, _, err := loadReport(context.Background(), f.pool, f.practice, allow)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Kind != core.LessonKindPractice || meta.TeacherID != nil || meta.CreatedBy != f.andreev ||
		meta.Teacher != "Андреев А. А." || meta.Participants != 0 || len(meta.Scenarios) != 0 {
		t.Errorf("practice meta = %+v", meta)
	}
	if len(rows) != 1 || rows[0].UserID != f.andreev || rows[0].Status != core.AttemptIssued {
		t.Errorf("practice rows = %+v", rows)
	}

	meta, rows, truncated, err := loadReport(context.Background(), f.pool, f.empty, allow)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || truncated || meta.Mode != core.ModeCardActions {
		t.Errorf("empty: %+v %v", rows, truncated)
	}
	if meta.Settings.PassThreshold != 70 {
		t.Errorf("битые настройки — дефолты: %+v", meta.Settings)
	}
}

func TestLoadReportNotFoundAndAuthorize(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if _, _, _, err := loadReport(context.Background(), f.pool, uuid.New(), allow); !errors.Is(err, errLessonNotFound) {
		t.Fatalf("err = %v, want errLessonNotFound", err)
	}
	denied := errors.New("чужое занятие")
	var seen *lessonMeta
	_, rows, _, err := loadReport(context.Background(), f.pool, f.lesson, func(m *lessonMeta) error {
		seen = m
		return denied
	})
	if !errors.Is(err, denied) || rows != nil {
		t.Fatalf("err = %v rows = %v", err, rows)
	}
	if seen == nil || seen.TeacherID == nil || *seen.TeacherID != f.teacher || seen.CreatedBy != f.teacher {
		t.Fatalf("authorize получил %+v", seen)
	}
	// соединение после отказа возвращено в пул исправным
	if _, _, _, err := loadReport(context.Background(), f.pool, f.lesson, allow); err != nil {
		t.Fatal(err)
	}
}
