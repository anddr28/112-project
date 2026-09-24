package dialogue

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/platform/pgtest"
)

// Сид DB-тестов: пользователи, категория, сценарий с брифом, голосовое занятие и попытка.

// demoScript — легенда «пожар в квартире» с брифом (факты всех видов раскрытия).
func demoScript(briefMax int) model.CallScript {
	return model.CallScript{
		Caller: model.Caller{Name: "Иванова Мария Петровна", Phone: "+79990001122", EmotionalState: "паника"},
		Address: components.Address{
			City: ptr("Москва"), Street: ptr("улица Ленина"), House: ptr("12"), Entrance: ptr("3"), Apartment: ptr("45"),
		},
		Dialogue: &model.DialogueBrief{
			Persona: "соседка, пожилая женщина",
			Facts: []model.DialogueFact{
				{ID: "f_smoke", Text: "Дым из окна четвёртого этажа", Reveal: model.RevealVolunteer, Hints: []string{"дым", "этаж"}},
				{ID: "f_entrance", Text: "Подъезд третий", Reveal: model.RevealOnRequest, Hints: []string{"подъезд"}},
				{ID: "f_secret", Text: "Сосед курил на кухне", Reveal: model.RevealNever},
			},
			MaxTurns: briefMax,
		},
		Turns: []model.Turn{
			{Speaker: model.SpeakerCaller, Text: "Алло! Пожар! У соседей дым валит!"},
			{Speaker: model.SpeakerOperatorHint, Text: "Назовите адрес"},
			{Speaker: model.SpeakerCaller, Text: "Улица Ленина, дом двенадцать!"},
			{Speaker: model.SpeakerCaller, Text: "Приезжайте быстрее, пожалуйста!"},
		},
	}
}

type seedOpts struct {
	voiceOff    bool
	input       string // voice | text | both (по умолчанию both)
	maxTurns    int    // занятие (по умолчанию 12)
	tts         bool
	briefMax    int
	status      string // по умолчанию in_progress
	notAccepted bool
	script      *model.CallScript
	noOpening   bool // не создавать вступление (turn 0)
}

// world — ids сида.
type world struct {
	pool                              *pgxpool.Pool
	teacher, otherTeacher, admin      uuid.UUID
	student, student2                 uuid.UUID
	lesson, scenario, etalon, attempt uuid.UUID
	acceptedAt                        time.Time
	opening                           *public.DialogueTurnView
}

func (w *world) asStudent() as  { return as{role: "student", user: w.student} }
func (w *world) asStudent2() as { return as{role: "student", user: w.student2} }
func (w *world) asTeacher() as  { return as{role: "teacher", user: w.teacher} }
func (w *world) asOtherT() as   { return as{role: "teacher", user: w.otherTeacher} }
func (w *world) asAdmin() as    { return as{role: "admin", user: w.admin} }

func mustExec(t testing.TB, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %s: %v", sql, err)
	}
}

// newWorld — свежая БД и попытка студента в голосовом занятии (по умолчанию: разговор
// принят, вступление записано через OnCallAccepted).
func newWorld(t testing.TB, hs *harness, o seedOpts) *world {
	t.Helper()
	pool := hs.pool
	w := &world{pool: pool,
		teacher: uuid.New(), otherTeacher: uuid.New(), admin: uuid.New(), student: uuid.New(), student2: uuid.New(),
		lesson: uuid.New(), scenario: uuid.New(), etalon: uuid.New(), attempt: uuid.New()}
	for _, u := range []struct {
		id   uuid.UUID
		role string
	}{{w.teacher, "teacher"}, {w.otherTeacher, "teacher"}, {w.admin, "admin"}, {w.student, "student"}, {w.student2, "student"}} {
		mustExec(t, pool, `INSERT INTO users (id, login, password_hash, role, last_name, first_name)
		                   VALUES ($1, $2, 'x', $3, 'Тестов', 'Тест')`, u.id, "u_"+u.id.String()[:8], u.role)
	}
	cat := uuid.New()
	mustExec(t, pool, `INSERT INTO classifier_categories (id, code, name) VALUES ($1, $2, 'Пожар')`, cat, "c_"+cat.String()[:8])

	cs := demoScript(o.briefMax)
	if o.script != nil {
		cs = *o.script
	}
	csJSON, _ := json.Marshal(cs)
	mustExec(t, pool, `INSERT INTO scenarios (id, title, category_id, source, status, call_script)
	                   VALUES ($1, 'Пожар в квартире', $2, 'manual', 'validated', $3)`, w.scenario, cat, csJSON)
	mustExec(t, pool, `INSERT INTO etalons (id, scenario_id, card) VALUES ($1, $2, '{}')`, w.etalon, w.scenario)

	input := o.input
	if input == "" {
		input = model.VoiceInputBoth
	}
	maxTurns := o.maxTurns
	if maxTurns == 0 {
		maxTurns = 12
	}
	settingsJSON, _ := json.Marshal(map[string]any{
		"voice": map[string]any{"enabled": !o.voiceOff, "input": input, "push_to_talk": true, "max_turns": maxTurns, "tts_enabled": o.tts},
	})
	mustExec(t, pool, `INSERT INTO lessons (id, kind, title, teacher_id, created_by, mode, settings, status)
	                   VALUES ($1, 'class', 'Голосовое занятие', $2, $2, 'cards', $3, 'running')`, w.lesson, w.teacher, settingsJSON)

	status := o.status
	if status == "" {
		status = "in_progress"
	}
	var accepted *time.Time
	if !o.notAccepted {
		at := time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)
		accepted, w.acceptedAt = &at, at
	}
	mustExec(t, pool, `INSERT INTO attempts (id, lesson_id, user_id, scenario_id, etalon_id, mode, seq_no, status, time_limit_sec, call_accepted_at)
	                   VALUES ($1, $2, $3, $4, $5, 'cards', 1, $6, 30, $7)`, w.attempt, w.lesson, w.student, w.scenario, w.etalon, status, accepted)

	if !o.notAccepted && !o.noOpening {
		err := pg.WithTx(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
			v, err := hs.svc.OnCallAccepted(ctx, tx, w.attempt)
			w.opening = v
			return err
		})
		if err != nil {
			t.Fatalf("OnCallAccepted: %v", err)
		}
	}
	return w
}

// dbWorld — pgtest + харнесс + сид (t.Skip без PostgreSQL).
func dbWorld(t *testing.T, o seedOpts) (*harness, *world) {
	t.Helper()
	pool := pgtest.New(t)
	hs := newHarness(t, pool)
	return hs, newWorld(t, hs, o)
}

// attemptRow — состояние разговора в attempts.
type attemptRow struct {
	endedAt   *time.Time
	endReason *string
	turns     int
}

func readAttempt(t testing.TB, pool *pgxpool.Pool, id uuid.UUID) attemptRow {
	t.Helper()
	var r attemptRow
	if err := pool.QueryRow(context.Background(),
		`SELECT call_ended_at, call_end_reason, dialogue_turns FROM attempts WHERE id = $1`, id).
		Scan(&r.endedAt, &r.endReason, &r.turns); err != nil {
		t.Fatal(err)
	}
	return r
}

// eventTypes — события попытки по порядку вставки.
func eventTypes(t testing.TB, pool *pgxpool.Pool, id uuid.UUID) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT type FROM attempt_events WHERE attempt_id = $1 ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func countTurns(t testing.TB, pool *pgxpool.Pool, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM attempt_dialogue_turns WHERE attempt_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
