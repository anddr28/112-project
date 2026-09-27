package lessons

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/platform/pgtest"
)

// fixture — свежая БД с пользователями, группами и сценариями для тестов занятий.
type fixture struct {
	pool *pgxpool.Pool

	cat, cat2 uuid.UUID

	teacher, teacher2, admin uuid.UUID

	s1, s2      uuid.UUID // обучающиеся группы teacher
	sOwnBlocked uuid.UUID // группа teacher, заблокирован
	sOther      uuid.UUID // группа teacher2
	sFree       uuid.UUID // без группы, активен
	sFreeBlock  uuid.UUID // без группы, заблокирован

	scFire, scGas uuid.UUID // validated, both, эталон, бриф и чек-лист (годны для голоса)
	scPlain       uuid.UUID // validated, cards, эталон, без брифа
	scDraft       uuid.UUID // generated
	scActions     uuid.UUID // validated, card_actions
	etFire, etGas uuid.UUID
}

func mustID(t testing.TB, q pg.Querier, sql string, args ...any) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := q.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return id
}

func mustExec(t testing.TB, q pg.Querier, sql string, args ...any) {
	t.Helper()
	if _, err := q.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

const voiceScript = `{"caller": {"name": "Иван", "phone": "+79990001122", "role": "очевидец"},
 "turns": [{"speaker": "caller", "text": "Алло, у нас пожар!"}],
 "dialogue": {"facts": [{"id": "f1", "text": "Горит кухня на 3 этаже"}], "max_turns": 8}}`

const checklist = `{"checklist": [{"id": "c1", "text": "Уточнить адрес"}]}`

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := pgtest.New(t)
	f := &fixture{pool: pool}
	user := func(login, role, last, first, middle, status string) uuid.UUID {
		return mustID(t, pool, `INSERT INTO users (login, password_hash, role, last_name, first_name, middle_name, status)
			VALUES ($1, 'x', $2, $3, $4, NULLIF($5, ''), $6) RETURNING id`, login, role, last, first, middle, status)
	}
	f.teacher = user("teacher", "teacher", "Сидорова", "Анна", "Петровна", "active")
	f.teacher2 = user("teacher2", "teacher", "Кузнецов", "Олег", "", "active")
	f.admin = user("admin", "admin", "Админов", "Админ", "", "active")
	f.s1 = user("student", "student", "Иванов", "Иван", "Иванович", "active")
	f.s2 = user("student2", "student", "Петров", "Пётр", "Петрович", "active")
	f.sOwnBlocked = user("st_blocked", "student", "Блокова", "Вера", "", "blocked")
	f.sOther = user("st_other", "student", "Чужой", "Семён", "", "active")
	f.sFree = user("st_free", "student", "Новиков", "Николай", "", "active")
	f.sFreeBlock = user("st_free_blocked", "student", "Скрытный", "Глеб", "", "blocked")

	g1 := mustID(t, pool, `INSERT INTO groups (name, teacher_id) VALUES ('ДДС-1', $1) RETURNING id`, f.teacher)
	g2 := mustID(t, pool, `INSERT INTO groups (name, teacher_id) VALUES ('ДДС-2', $1) RETURNING id`, f.teacher2)
	mustExec(t, pool, `INSERT INTO group_members (group_id, user_id) SELECT $1, unnest($2::uuid[])`, g1,
		[]uuid.UUID{f.s1, f.s2, f.sOwnBlocked})
	mustExec(t, pool, `INSERT INTO group_members (group_id, user_id) VALUES ($1, $2)`, g2, f.sOther)

	f.cat = mustID(t, pool, `INSERT INTO classifier_categories (code, name) VALUES ('t101', 'Пожар') RETURNING id`)
	f.cat2 = mustID(t, pool, `INSERT INTO classifier_categories (code, name) VALUES ('t104', 'Запах газа') RETURNING id`)

	scenario := func(title string, cat uuid.UUID, mode, status, script string) uuid.UUID {
		return mustID(t, pool, `INSERT INTO scenarios (title, category_id, mode, source, status, call_script)
			VALUES ($1, $2, $3, 'manual', $4, $5::jsonb) RETURNING id`, title, cat, mode, status, script)
	}
	// Список оповещения — как у демо-сценариев: ДДС ЖКХ есть везде (ракурс dds).
	etalon := func(sc uuid.UUID, dialogue string) uuid.UUID {
		return mustID(t, pool, `INSERT INTO etalons (scenario_id, version, is_current, card, expected_dialogue)
			VALUES ($1, 1, true, '{"services_to_notify": ["101", "zhkh"]}', $2::jsonb) RETURNING id`, sc, dialogue)
	}
	f.scFire = scenario(demoFireTitle, f.cat, core.ModeBoth, core.ScenarioValidated, voiceScript)
	f.etFire = etalon(f.scFire, checklist)
	f.scGas = scenario(demoGasTitle, f.cat2, core.ModeBoth, core.ScenarioValidated, voiceScript)
	f.etGas = etalon(f.scGas, checklist)
	f.scPlain = scenario(demoWaterTitle, f.cat, core.ModeCards, core.ScenarioValidated, `{"turns": []}`)
	etalon(f.scPlain, `{}`)
	f.scDraft = scenario("Черновик сценария", f.cat, core.ModeBoth, core.ScenarioGenerated, `{}`)
	etalon(f.scDraft, `{}`)
	f.scActions = scenario("Только действия", f.cat, core.ModeCardActions, core.ScenarioValidated, `{}`)
	etalon(f.scActions, `{}`)
	return f
}

func as(role core.Role, id uuid.UUID) who { return who{Role: role, ID: id} }

func (f *fixture) teacherW() who  { return as(core.RoleTeacher, f.teacher) }
func (f *fixture) teacher2W() who { return as(core.RoleTeacher, f.teacher2) }
func (f *fixture) adminW() who    { return as(core.RoleAdmin, f.admin) }

func idStrings(ids ...uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// lessonBody — CreateLessonInput (как шлёт форма создания).
func lessonBody(title string, scen []uuid.UUID, parts []uuid.UUID) map[string]any {
	return map[string]any{
		"title": title, "mode": "cards", "perspective": "operator112", "timeLimitSec": 60,
		"scenarioIds": idStrings(scen...), "participantIds": idStrings(parts...),
		"passThreshold": 60, "allowReplay": true,
	}
}

func (e *testEnv) createLesson(t *testing.T, as who, body map[string]any) public.Lesson {
	t.Helper()
	return decode[public.Lesson](t, e.do(t, "POST", "/lessons", as, body), 201)
}

func (e *testEnv) startLesson(t *testing.T, as who, id uuid.UUID) public.Lesson {
	t.Helper()
	return decode[public.Lesson](t, e.do(t, "POST", "/lessons/"+id.String()+"/start", as, nil), 200)
}

// runningLesson — занятие пожар+газ для s1 и s2, запущенное (по одной карточке выдано).
func (f *fixture) runningLesson(t *testing.T, e *testEnv, cards int) public.Lesson {
	t.Helper()
	body := lessonBody("Практическое занятие", []uuid.UUID{f.scFire, f.scGas}, []uuid.UUID{f.s1, f.s2})
	body["cardsPerStudent"] = cards
	l := e.createLesson(t, f.teacherW(), body)
	return e.startLesson(t, f.teacherW(), l.Id)
}

type attemptRow struct {
	ID, UserID, ScenarioID, EtalonID uuid.UUID
	SeqNo                            int
	Status                           string
	TimeLimit                        int
	CallEndedAt                      *time.Time
	CallEndReason                    *string
}

func (f *fixture) attempts(t *testing.T, lessonID uuid.UUID) []attemptRow {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT id, user_id, scenario_id, etalon_id, seq_no, status, time_limit_sec, call_ended_at, call_end_reason
		  FROM attempts WHERE lesson_id = $1 ORDER BY user_id, seq_no`, lessonID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []attemptRow
	for rows.Next() {
		var a attemptRow
		if err := rows.Scan(&a.ID, &a.UserID, &a.ScenarioID, &a.EtalonID, &a.SeqNo, &a.Status, &a.TimeLimit,
			&a.CallEndedAt, &a.CallEndReason); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *fixture) attemptOf(t *testing.T, lessonID, userID uuid.UUID) attemptRow {
	t.Helper()
	var last attemptRow
	found := false
	for _, a := range f.attempts(t, lessonID) {
		if a.UserID == userID {
			last, found = a, true
		}
	}
	if !found {
		t.Fatalf("у %s нет попытки в занятии %s", userID, lessonID)
	}
	return last
}

func (f *fixture) scalar(t *testing.T, sql string, args ...any) any {
	t.Helper()
	var v any
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}

func (f *fixture) participantStatus(t *testing.T, lessonID, userID uuid.UUID) string {
	t.Helper()
	return f.scalar(t, `SELECT status FROM lesson_participants WHERE lesson_id = $1 AND user_id = $2`, lessonID, userID).(string)
}

// waitLockWaiters — ждём, пока хотя бы n бэкендов этой БД встанут в ожидание блокировки.
func waitLockWaiters(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var c int
		if err := pool.QueryRow(context.Background(), `
			SELECT count(*)::int FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&c); err != nil {
			t.Fatal(err)
		}
		if c >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("за 15 с никто не встал в ожидание блокировки (ждали %d)", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
