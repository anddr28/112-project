package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/platform/pgtest"
)

// fixture — демо-данные для чтений store: преподаватели, студенты (ФИО по-русски),
// сценарии с эталонами/озвучкой, занятия разных статусов, попытки и транскрипт.
type fixture struct {
	pool *pgxpool.Pool

	teacher, teacher2 uuid.UUID
	st1, st2, st3     uuid.UUID // Яковлева А., Андреев Б. Б., Борисова В. В.

	cat, cat2                         uuid.UUID
	scFire, scGas, scArch, scNoCaller uuid.UUID
	etFire1, etFire2                  uuid.UUID // v1 (не текущая), v2 (текущая)
	fireHash1, fireHash2              string

	lessonRun, lessonDraft, lessonOther, practice, cancelled uuid.UUID

	aSt1Seq1, aSt1Seq2, aSt2, aPractice uuid.UUID
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func mustExec(t *testing.T, q pg.Querier, sql string, args ...any) {
	t.Helper()
	if _, err := q.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func mustID(t *testing.T, q pg.Querier, sql string, args ...any) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := q.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", sql, err)
	}
	return id
}

func insertUser(t *testing.T, q pg.Querier, login, role, last, first string, middle *string) uuid.UUID {
	t.Helper()
	return mustID(t, q, `INSERT INTO users (login, password_hash, role, last_name, first_name, middle_name)
		VALUES ($1, 'x', $2, $3, $4, $5) RETURNING id`, login, role, last, first, middle)
}

const fullSettingsVoice = `{"pass_threshold": 75, "weights": {"fields": 0.4, "semantic": 0.2, "grammar": 0.1, "timing": 0.1, "dialogue": 0.2},
 "cards_per_student": 2, "allow_replay": false, "perspective": "dds",
 "voice": {"enabled": true, "input": "both", "push_to_talk": false, "max_turns": 8, "tts_enabled": true}}`

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := pgtest.New(t)
	f := &fixture{pool: pool}

	f.teacher = insertUser(t, pool, "t_teacher", "teacher", "Учителев", "Пётр", convert.Ptr("Петрович"))
	f.teacher2 = insertUser(t, pool, "t_teacher2", "teacher", "Наставникова", "Ольга", nil)
	f.st1 = insertUser(t, pool, "t_st1", "student", "Яковлева", "Анна", nil)
	f.st2 = insertUser(t, pool, "t_st2", "student", "Андреев", "Борис", convert.Ptr("Борисович"))
	f.st3 = insertUser(t, pool, "t_st3", "student", "Борисова", "Вера", convert.Ptr("Викторовна"))

	f.cat = mustID(t, pool, `INSERT INTO classifier_categories (code, name) VALUES ('t101', 'Пожар') RETURNING id`)
	f.cat2 = mustID(t, pool, `INSERT INTO classifier_categories (code, name) VALUES ('t104', 'Запах газа') RETURNING id`)

	// сценарий «пожар»: две реплики заявителя озвучены, подсказка оператору без хэша
	f.fireHash1 = convert.TTSHash("Алло! Пожар!", "xenia", 1)
	f.fireHash2 = convert.TTSHash("Горит пятый этаж", "xenia", 1)
	fireScript := `{"caller":{"name":"Мария","phone":"+79035113367","voice":"xenia"},"address":{"raw":"Москва, Тверская, 12"},
		"key_facts":["горит квартира"],
		"dialogue":{"persona":"соседка","facts":[{"id":"f1","text":"горит","reveal":"volunteer"}]},
		"turns":[{"speaker":"caller","text":"Алло! Пожар!","tts_hash":"` + f.fireHash1 + `"},
		         {"speaker":"operator_hint","text":"Уточните этаж"},
		         {"speaker":"caller","text":"Горит пятый этаж","tts_hash":"` + f.fireHash2 + `"}]}`
	f.scFire = mustID(t, pool, `INSERT INTO scenarios (title, category_id, difficulty, mode, source, status, call_script,
		generation_meta, author_id, created_at) VALUES ('Пожар в квартире', $1, 2, 'cards', 'generated', 'validated', $2,
		'{"notes_for_teacher":"Проверьте этаж","job_id":"j-1","llm_model":"qwen"}', $3, now() - interval '4 hours') RETURNING id`,
		f.cat, fireScript, f.teacher)
	mustExec(t, pool, `INSERT INTO tts_cache (text_hash, voice, rate, file_path, duration_ms) VALUES
		($1, 'xenia', 1.0, 'ab/1.wav', 2100), ($2, 'xenia', 0.95, 'cd/2.ogg', NULL)`, f.fireHash1, f.fireHash2)
	f.etFire1 = mustID(t, pool, `INSERT INTO etalons (scenario_id, version, is_current, card) VALUES ($1, 1, false, '{}') RETURNING id`, f.scFire)
	f.etFire2 = mustID(t, pool, `INSERT INTO etalons (scenario_id, version, is_current, card, card_draft, scoring,
		expected_actions, expected_dialogue, created_by) VALUES ($1, 2, true,
		'{"category_code":"t101","address":{"raw":"Москва, Тверская, 12"}}',
		'{"incidentTypeIds":["`+f.cat.String()+`"],"address":{"raw":"Москва, Тверская, 12"},"description":"Пожар"}',
		'{"required_fields":["address.raw","description"],"field_weights":{"address.raw":2}}',
		'[{"action_text":"Передать в 101","required_facts":["адрес"]}]',
		'{"checklist":[{"id":"q1","text":"Уточнить этаж","kind":"question","required":true}]}', $2) RETURNING id`,
		f.scFire, f.teacher)

	// «газ»: реплика заявителя с хэшем, которого нет в tts_cache; эталона нет
	f.scGas = mustID(t, pool, `INSERT INTO scenarios (title, category_id, mode, source, status, call_script, created_at)
		VALUES ('Запах газа', $1, 'both', 'manual', 'draft',
		'{"caller":{},"address":{},"turns":[{"speaker":"caller","text":"Пахнет газом","tts_hash":"нет-такого"}]}',
		now() - interval '3 hours') RETURNING id`, f.cat2)
	f.scArch = mustID(t, pool, `INSERT INTO scenarios (title, category_id, mode, source, status, archived_at, created_at)
		VALUES ('Архивный', $1, 'card_actions', 'manual', 'archived', now(), now() - interval '2 hours') RETURNING id`, f.cat)
	f.scNoCaller = mustID(t, pool, `INSERT INTO scenarios (title, category_id, mode, source, status, call_script, parent_id, version, created_at)
		VALUES ('Без реплик', $1, 'card_actions', 'generated', 'generated',
		'{"turns":[{"speaker":"operator_hint","text":"x"}]}', $2, 2, now() - interval '1 hour') RETURNING id`, f.cat, f.scFire)

	insertLesson := func(kind string, teacher *uuid.UUID, createdBy uuid.UUID, title, status, settings, age string) uuid.UUID {
		return mustID(t, pool, `INSERT INTO lessons (kind, title, teacher_id, created_by, mode, difficulty, time_limit_sec, settings, status,
			started_at, created_at) VALUES ($1, $2, $3, $4, 'cards', 2, 45, $5, $6,
			CASE WHEN $6 IN ('running', 'finished') THEN now() END, now() - $7::interval) RETURNING id`,
			kind, title, teacher, createdBy, settings, status, age)
	}
	f.lessonRun = insertLesson("class", &f.teacher, f.teacher, "Идёт", "running", fullSettingsVoice, "1 hour")
	f.lessonDraft = insertLesson("class", &f.teacher, f.teacher, "Черновик", "draft", `{}`, "2 hours")
	f.lessonOther = insertLesson("class", &f.teacher2, f.teacher2, "Чужое", "finished", `{"perspective":"dds"}`, "30 minutes")
	f.practice = insertLesson("practice", nil, f.st1, "Практика", "finished", `{}`, "3 hours")
	f.cancelled = insertLesson("class", &f.teacher, f.teacher, "Отменено", "cancelled", `{}`, "10 minutes")

	mustExec(t, pool, `INSERT INTO lesson_scenarios (lesson_id, scenario_id, sort_order) VALUES
		($1, $2, 0), ($1, $3, 1), ($4, $3, 0), ($5, $3, 0), ($6, $3, 0), ($7, $3, 0)`,
		f.lessonRun, f.scGas, f.scFire, f.lessonDraft, f.lessonOther, f.practice, f.cancelled)
	mustExec(t, pool, `INSERT INTO lesson_participants (lesson_id, user_id, status, joined_at, mic_ready) VALUES
		($1, $2, 'active', now() - interval '50 minutes', false),
		($1, $3, 'joined', now() - interval '40 minutes', true),
		($1, $4, 'assigned', NULL, false),
		($5, $2, 'assigned', NULL, false),
		($6, $3, 'finished', NULL, false),
		($7, $2, 'finished', NULL, false),
		($8, $2, 'assigned', NULL, false)`,
		f.lessonRun, f.st1, f.st2, f.st3, f.lessonDraft, f.lessonOther, f.practice, f.cancelled)

	insertAttempt := func(lesson, user uuid.UUID, seq int, status string, card *string) uuid.UUID {
		return mustID(t, pool, `INSERT INTO attempts (lesson_id, user_id, scenario_id, etalon_id, mode, seq_no, status,
			time_limit_sec, card) VALUES ($1, $2, $3, $4, 'cards', $5, $6, 45, $7) RETURNING id`,
			lesson, user, f.scFire, f.etFire2, seq, status, card)
	}
	f.aSt1Seq1 = insertAttempt(f.lessonRun, f.st1, 1, "evaluated", convert.Ptr(`{"description":"Горит","address":{"raw":"Москва"}}`))
	f.aSt1Seq2 = insertAttempt(f.lessonRun, f.st1, 2, "in_progress", nil)
	f.aSt2 = insertAttempt(f.lessonRun, f.st2, 1, "issued", nil)
	f.aPractice = insertAttempt(f.practice, f.st1, 1, "evaluated", nil)

	// транскрипт второй попытки: порядок вставки перемешан намеренно
	mustExec(t, pool, `INSERT INTO attempt_dialogue_turns (attempt_id, turn_no, speaker, text, source, confidence, audio_path,
		duration_ms, at_ms, emotional_state, revealed_fact_ids, meta) VALUES
		($1, 1, 'caller', 'Горит пятый этаж', 'llm', NULL, 'cd/2.ogg', 1800, 9000, 'паника', '{f1}', '{"latency_ms":900}'),
		($1, 2, 'operator', 'Выезжаем', 'text', NULL, NULL, NULL, 15000, NULL, '{}', '{}'),
		($1, 0, 'caller', 'Алло! Пожар!', 'script', NULL, 'ab/1.wav', 2100, 0, 'паника', '{}', '{}'),
		($1, 1, 'operator', 'Какой этаж?', 'stt', 0.91, NULL, 1500, 5000, NULL, '{}', '{"text_raw":"какой этаж"}')`,
		f.aSt1Seq2)
	mustExec(t, pool, `UPDATE attempts SET dialogue_turns = 4, call_ended_at = now(), call_end_reason = 'operator_hung_up' WHERE id = $1`, f.aSt1Seq2)
	return f
}

func ids(rows []LessonRow) []uuid.UUID {
	out := make([]uuid.UUID, len(rows))
	for i := range rows {
		out[i] = rows[i].ID
	}
	return out
}

func TestLessonsDB(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	t.Run("GetLesson: участники по ФИО, последняя попытка, пул по sort_order", func(t *testing.T) {
		t.Parallel()
		r, err := GetLesson(ctxT(t), f.pool, f.lessonRun)
		if err != nil {
			t.Fatal(err)
		}
		if r.Kind != "class" || r.Title != "Идёт" || r.Status != "running" || r.OwnerID() != f.teacher ||
			r.TimeLimitSec != 45 || convert.Deref(r.Difficulty) != 2 || r.StartedAt == nil || r.FinishedAt != nil {
			t.Fatalf("lesson = %+v", r)
		}
		if !reflect.DeepEqual(r.ScenarioIDs, []uuid.UUID{f.scGas, f.scFire}) {
			t.Fatalf("scenarioIds = %v", r.ScenarioIDs)
		}
		if s := r.Settings; s.PassThreshold != 75 || s.Perspective != model.PerspectiveDDS || !s.Voice.Enabled ||
			s.Voice.Input != model.VoiceInputBoth || s.Voice.MaxTurns != 8 || s.CardsPerStudent != 2 || s.Weights.Dialogue != 0.2 {
			t.Fatalf("settings = %+v", s)
		}
		var names []string
		for _, p := range r.Participants {
			names = append(names, p.Name)
		}
		if !reflect.DeepEqual(names, []string{"Андреев Б. Б.", "Борисова В. В.", "Яковлева А."}) {
			t.Fatalf("участники по ФИО: %v", names)
		}
		byUser := map[uuid.UUID]ParticipantRow{}
		for _, p := range r.Participants {
			byUser[p.UserID] = p
		}
		if a := byUser[f.st1].AttemptID; a == nil || *a != f.aSt1Seq2 {
			t.Fatalf("attempt st1 = %v, want последняя по seq_no %v", a, f.aSt1Seq2)
		}
		if a := byUser[f.st2].AttemptID; a == nil || *a != f.aSt2 {
			t.Fatalf("attempt st2 = %v", a)
		}
		if byUser[f.st3].AttemptID != nil || byUser[f.st3].JoinedAt != nil {
			t.Fatal("st3 без попытки")
		}
		if p := byUser[f.st2]; !p.MicReady || p.Status != "joined" || p.JoinedAt == nil || p.JoinedAt.Location() != time.UTC {
			t.Fatalf("st2 = %+v", p)
		}
		m := asJSONMap(t, LessonToPublic(r))
		requireKeys(t, "Lesson", m, reqLesson)
	})

	t.Run("GetLesson: неполные settings добиваются дефолтами", func(t *testing.T) {
		t.Parallel()
		r, err := GetLesson(ctxT(t), f.pool, f.lessonDraft)
		if err != nil {
			t.Fatal(err)
		}
		if r.Settings != defaultLessonSettings {
			t.Fatalf("settings '{}' -> дефолты, got %+v", r.Settings)
		}
		if len(r.Participants) != 1 || r.Participants[0].AttemptID != nil {
			t.Fatalf("participants = %+v", r.Participants)
		}
		o, err := GetLesson(ctxT(t), f.pool, f.lessonOther)
		if err != nil {
			t.Fatal(err)
		}
		if o.Settings.Perspective != model.PerspectiveDDS || o.Settings.PassThreshold != defaultLessonSettings.PassThreshold {
			t.Fatalf("частичные settings: %+v", o.Settings)
		}
	})

	t.Run("GetLesson: практика и отсутствие", func(t *testing.T) {
		t.Parallel()
		r, err := GetLesson(ctxT(t), f.pool, f.practice)
		if err != nil {
			t.Fatal(err)
		}
		if r.TeacherID != nil || r.OwnerID() != f.st1 || LessonToPublic(r).TeacherId != f.st1 {
			t.Fatalf("practice = %+v", r)
		}
		if _, err := GetLesson(ctxT(t), f.pool, uuid.New()); !pg.IsNoRows(err) {
			t.Fatalf("нет занятия -> ErrNoRows, got %v", err)
		}
	})

	t.Run("ListLessons: администратор", func(t *testing.T) {
		t.Parallel()
		rows, err := ListLessons(ctxT(t), f.pool, LessonFilter{})
		if err != nil {
			t.Fatal(err)
		}
		want := []uuid.UUID{f.cancelled, f.lessonOther, f.lessonRun, f.lessonDraft, f.practice}
		if !reflect.DeepEqual(ids(rows), want) {
			t.Fatalf("все занятия, новые сверху: %v, want %v", ids(rows), want)
		}
		for _, r := range rows {
			if r.ID == f.lessonRun && len(r.Participants) != 3 {
				t.Fatalf("администратор видит всех участников: %d", len(r.Participants))
			}
		}
		rows, err = ListLessons(ctxT(t), f.pool, LessonFilter{Status: "finished"})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ids(rows), []uuid.UUID{f.lessonOther, f.practice}) {
			t.Fatalf("status=finished: %v", ids(rows))
		}
	})

	t.Run("ListLessons: преподаватель", func(t *testing.T) {
		t.Parallel()
		rows, err := ListLessons(ctxT(t), f.pool, LessonFilter{TeacherID: &f.teacher})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ids(rows), []uuid.UUID{f.cancelled, f.lessonRun, f.lessonDraft}) {
			t.Fatalf("свои занятия: %v", ids(rows))
		}
		if len(rows[1].Participants) != 3 {
			t.Fatalf("преподаватель видит всех участников: %+v", rows[1].Participants)
		}
		rows, err = ListLessons(ctxT(t), f.pool, LessonFilter{TeacherID: &f.teacher, Status: "running"})
		if err != nil || !reflect.DeepEqual(ids(rows), []uuid.UUID{f.lessonRun}) {
			t.Fatalf("teacher+running: %v %v", ids(rows), err)
		}
		rows, err = ListLessons(ctxT(t), f.pool, LessonFilter{TeacherID: &f.teacher2})
		if err != nil || !reflect.DeepEqual(ids(rows), []uuid.UUID{f.lessonOther}) {
			t.Fatalf("teacher2: %v %v", ids(rows), err)
		}
		nobody := uuid.New()
		rows, err = ListLessons(ctxT(t), f.pool, LessonFilter{TeacherID: &nobody})
		if err != nil || rows == nil || len(rows) != 0 {
			t.Fatalf("пусто -> [] (не nil): %v %v", rows, err)
		}
	})

	// Регрессия (review: /lessons/assigned строил json_agg всех участников каждого занятия
	// истории студента и выбрасывал их): в представлении участника — только он сам.
	t.Run("ListLessons: участник видит только себя", func(t *testing.T) {
		t.Parallel()
		rows, err := ListLessons(ctxT(t), f.pool, LessonFilter{ParticipantUserID: &f.st1})
		if err != nil {
			t.Fatal(err)
		}
		// порядок дашборда обучающегося: идущие → черновики → завершённые → отменённые
		if !reflect.DeepEqual(ids(rows), []uuid.UUID{f.lessonRun, f.lessonDraft, f.practice, f.cancelled}) {
			t.Fatalf("занятия st1: %v", ids(rows))
		}
		for _, r := range rows {
			if len(r.Participants) != 1 || r.Participants[0].UserID != f.st1 {
				t.Fatalf("занятие %s: участники %+v, want только st1", r.Title, r.Participants)
			}
			p := r.Participants[0]
			if p.Name != "Яковлева А." || p.LastName != "Яковлева" {
				t.Fatalf("participant = %+v", p)
			}
			want := map[uuid.UUID]*uuid.UUID{f.lessonRun: &f.aSt1Seq2, f.practice: &f.aPractice}[r.ID]
			if !reflect.DeepEqual(p.AttemptID, want) {
				t.Fatalf("занятие %s: attempt %v, want %v", r.Title, p.AttemptID, want)
			}
			if r.ID == f.lessonRun {
				// всё остальное — как у полного чтения
				full, err := GetLesson(ctxT(t), f.pool, r.ID)
				if err != nil {
					t.Fatal(err)
				}
				mine, _ := full.Participant(f.st1)
				if !reflect.DeepEqual(*mine, p) {
					t.Fatalf("строка участника расходится с GetLesson:\n got %+v\nwant %+v", p, *mine)
				}
				full.Participants, r.Participants = nil, nil
				if !reflect.DeepEqual(*full, r) {
					t.Fatalf("занятие расходится с GetLesson:\n got %+v\nwant %+v", r, *full)
				}
			}
		}

		rows, err = ListLessons(ctxT(t), f.pool, LessonFilter{ParticipantUserID: &f.st3})
		if err != nil || len(rows) != 1 || rows[0].ID != f.lessonRun || len(rows[0].Participants) != 1 ||
			rows[0].Participants[0].AttemptID != nil || rows[0].Participants[0].Name != "Борисова В. В." {
			t.Fatalf("st3: %+v %v", rows, err)
		}
	})

	t.Run("ListLessons: участник + фильтры", func(t *testing.T) {
		t.Parallel()
		rows, err := ListLessons(ctxT(t), f.pool, LessonFilter{ParticipantUserID: &f.st1, Status: "draft"})
		if err != nil || !reflect.DeepEqual(ids(rows), []uuid.UUID{f.lessonDraft}) {
			t.Fatalf("st1+draft: %v %v", ids(rows), err)
		}
		rows, err = ListLessons(ctxT(t), f.pool, LessonFilter{ParticipantUserID: &f.st2, TeacherID: &f.teacher2})
		if err != nil || !reflect.DeepEqual(ids(rows), []uuid.UUID{f.lessonOther}) {
			t.Fatalf("st2+teacher2: %v %v", ids(rows), err)
		}
		rows, err = ListLessons(ctxT(t), f.pool, LessonFilter{ParticipantUserID: &f.st1, TeacherID: &f.teacher2})
		if err != nil || rows == nil || len(rows) != 0 {
			t.Fatalf("пусто -> []: %v %v", rows, err)
		}
		nobody := uuid.New()
		rows, err = ListLessons(ctxT(t), f.pool, LessonFilter{ParticipantUserID: &nobody})
		if err != nil || rows == nil || len(rows) != 0 {
			t.Fatalf("неизвестный пользователь -> []: %v %v", rows, err)
		}
	})

	t.Run("ListLessons: отменённый контекст", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := ListLessons(ctx, f.pool, LessonFilter{ParticipantUserID: &f.st1}); err == nil {
			t.Fatal("ошибка запроса должна вернуться")
		}
	})
}

// Регрессия производительности: список «моих занятий» студента идёт от его строк
// lesson_participants по индексу, без seq scan всех lessons (раньше — "$3 IS NULL OR l.id IN
// (подзапрос)": подзапрос под OR не становится semi-join, план — seq scan + hashed SubPlan).
func TestListLessonsByParticipantPlan(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	ctx := ctxT(t)

	teacher := insertUser(t, pool, "p_teacher", "teacher", "Учителев", "Пётр", nil)
	me := insertUser(t, pool, "p_me", "student", "Студентова", "Анна", nil)
	other := insertUser(t, pool, "p_other", "student", "Другой", "Иван", nil)
	mustExec(t, pool, `
		WITH l AS (
		  INSERT INTO lessons (kind, title, teacher_id, created_by, mode, status, created_at)
		  SELECT 'class', 'Занятие ' || g, $1, $1, 'cards', 'finished', now() - make_interval(mins => g)
		    FROM generate_series(1, 3000) g
		  RETURNING id, title)
		INSERT INTO lesson_participants (lesson_id, user_id, status)
		SELECT id, CASE WHEN title IN ('Занятие 7', 'Занятие 700', 'Занятие 2900') THEN $2::uuid ELSE $3::uuid END, 'finished'
		  FROM l`, teacher, me, other)
	mustExec(t, pool, `ANALYZE lessons, lesson_participants, users, attempts, lesson_scenarios`)

	var plan []byte
	if err := pool.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+sqlListLessonsByParticipant, me, nil, nil,
		false, -1, keysetTop, uuidMax, 51).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	var nodes []map[string]any
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if _, ok := x["Node Type"]; ok {
				nodes = append(nodes, x)
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	var doc any
	if err := json.Unmarshal(plan, &doc); err != nil {
		t.Fatal(err)
	}
	walk(doc)
	for _, n := range nodes {
		if n["Relation Name"] == "lessons" && n["Node Type"] == "Seq Scan" {
			t.Fatalf("seq scan по lessons в плане списка участника:\n%s", plan)
		}
	}

	rows, err := ListLessons(ctx, pool, LessonFilter{ParticipantUserID: &me})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].Title != "Занятие 7" || rows[2].Title != "Занятие 2900" {
		t.Fatalf("занятия: %d %v", len(rows), ids(rows))
	}
}

func TestScenariosDB(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	t.Run("GetScenario: полный", func(t *testing.T) {
		t.Parallel()
		r, err := GetScenario(ctxT(t), f.pool, f.scFire)
		if err != nil {
			t.Fatal(err)
		}
		if r.Title != "Пожар в квартире" || r.CategoryID != f.cat || r.CategoryName != "Пожар" || r.CategoryCode != "t101" ||
			r.Difficulty != 2 || r.Mode != "cards" || r.Source != "generated" || r.Status != "validated" ||
			r.AuthorID == nil || *r.AuthorID != f.teacher || r.Version != 1 || r.ParentID != nil {
			t.Fatalf("scenario = %+v", r)
		}
		if r.LessonsCount != 5 || !r.InUse() {
			t.Fatalf("lessonsCount = %d, want 5", r.LessonsCount)
		}
		if !r.TTSReady {
			t.Fatal("ttsReady: обе реплики заявителя озвучены")
		}
		if r.CallScript.Caller.Voice != "xenia" || len(r.CallScript.Turns) != 3 || r.CallScript.Dialogue == nil ||
			r.CallScript.Turns[0].TTSHash != f.fireHash1 {
			t.Fatalf("call_script = %+v", r.CallScript)
		}
		if r.GenerationMeta.NotesForTeacher != "Проверьте этаж" || r.GenerationMeta.JobID != "j-1" || r.GenerationMeta.Extra["llm_model"] != "qwen" {
			t.Fatalf("generation_meta = %+v", r.GenerationMeta)
		}
		e := r.Etalon
		if e == nil || e.ID != f.etFire2 || e.Version != 2 || !e.IsCurrent || e.ScenarioID != f.scFire || e.CreatedAt.IsZero() ||
			e.CreatedBy == nil || *e.CreatedBy != f.teacher {
			t.Fatalf("etalon = %+v", e)
		}
		if convert.Deref(e.Card.CategoryCode) != "t101" || e.CardDraft.Description != "Пожар" ||
			!reflect.DeepEqual(e.CardDraft.IncidentTypeIds, []string{f.cat.String()}) || e.CardDraft.Services == nil {
			t.Fatalf("card/card_draft = %+v / %+v", e.Card, e.CardDraft)
		}
		if !reflect.DeepEqual(e.RequiredFields(), []string{"address.raw", "description"}) || e.Scoring.FieldWeight("address.raw") != 2 {
			t.Fatalf("scoring = %+v", e.Scoring)
		}
		if len(e.ExpectedActions) != 1 || e.ExpectedActions[0].ActionText != "Передать в 101" {
			t.Fatalf("expected_actions = %+v", e.ExpectedActions)
		}
		if e.ExpectedDialogue == nil || len(e.ExpectedDialogue.Checklist) != 1 || !e.ExpectedDialogue.Checklist[0].Required {
			t.Fatalf("expected_dialogue = %+v", e.ExpectedDialogue)
		}
		var raw map[string]any
		if err := json.Unmarshal(e.CardDraftRaw, &raw); err != nil || raw["description"] != "Пожар" {
			t.Fatalf("card_draft raw = %s", e.CardDraftRaw)
		}
		m := asJSONMap(t, ScenarioToPublic(r))
		requireKeys(t, "Scenario", m, reqScenario)
	})

	t.Run("GetScenario: без эталона и без озвучки", func(t *testing.T) {
		t.Parallel()
		r, err := GetScenario(ctxT(t), f.pool, f.scGas)
		if err != nil {
			t.Fatal(err)
		}
		if r.Etalon != nil || r.TTSReady || r.LessonsCount != 1 || r.CategoryCode != "t104" {
			t.Fatalf("gas = %+v", r)
		}
		if r.CallScript.Turns == nil || r.GenerationMeta.IsZero() == false {
			t.Fatalf("нормализация: %+v", r)
		}
		m := asJSONMap(t, ScenarioToPublic(r))
		requireKeys(t, "Scenario", m, reqScenario)

		r, err = GetScenario(ctxT(t), f.pool, f.scNoCaller)
		if err != nil {
			t.Fatal(err)
		}
		if r.TTSReady || r.LessonsCount != 0 || r.ParentID == nil || *r.ParentID != f.scFire || r.Version != 2 {
			t.Fatalf("без реплик заявителя ttsReady=false: %+v", r)
		}
		if _, err := GetScenario(ctxT(t), f.pool, uuid.New()); !pg.IsNoRows(err) {
			t.Fatalf("нет сценария -> ErrNoRows, got %v", err)
		}
	})

	t.Run("ListScenarios: фильтры", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name string
			f    ScenarioFilter
			want []uuid.UUID
		}{
			{"по умолчанию без архива", ScenarioFilter{}, []uuid.UUID{f.scNoCaller, f.scGas, f.scFire}},
			{"status=archived включает архив", ScenarioFilter{Status: "archived"}, []uuid.UUID{f.scArch}},
			{"IncludeArchived", ScenarioFilter{IncludeArchived: true}, []uuid.UUID{f.scNoCaller, f.scArch, f.scGas, f.scFire}},
			{"status=validated", ScenarioFilter{Status: "validated"}, []uuid.UUID{f.scFire}},
			{"категория", ScenarioFilter{CategoryID: &f.cat2}, []uuid.UUID{f.scGas}},
			{"mode=cards включает both", ScenarioFilter{Mode: "cards"}, []uuid.UUID{f.scGas, f.scFire}},
			{"mode=card_actions включает both", ScenarioFilter{Mode: "card_actions"}, []uuid.UUID{f.scNoCaller, f.scGas}},
			{"mode=both — только both", ScenarioFilter{Mode: "both"}, []uuid.UUID{f.scGas}},
			{"ничего", ScenarioFilter{Status: "rejected"}, []uuid.UUID{}},
		}
		for _, tt := range tests {
			rows, err := ListScenarios(ctxT(t), f.pool, tt.f)
			if err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			got := make([]uuid.UUID, len(rows))
			for i := range rows {
				got[i] = rows[i].ID
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
			}
		}
	})

	t.Run("эталоны по версии", func(t *testing.T) {
		t.Parallel()
		cur, err := GetCurrentEtalon(ctxT(t), f.pool, f.scFire)
		if err != nil || cur.ID != f.etFire2 || cur.Version != 2 {
			t.Fatalf("current = %+v %v", cur, err)
		}
		old, err := GetEtalon(ctxT(t), f.pool, f.etFire1)
		if err != nil {
			t.Fatal(err)
		}
		if old.IsCurrent || old.Version != 1 || old.ScenarioID != f.scFire {
			t.Fatalf("v1 = %+v", old)
		}
		// '{}' в card_draft / expected_dialogue -> пустая форма АРМ / nil
		if old.ExpectedDialogue != nil || old.ExpectedActions == nil || len(old.ExpectedActions) != 0 ||
			!reflect.DeepEqual(old.CardDraft, convert.EmptyDraft()) || !old.Scoring.IsZero() {
			t.Fatalf("пустой эталон = %+v", old)
		}
		if _, err := GetCurrentEtalon(ctxT(t), f.pool, f.scGas); !pg.IsNoRows(err) {
			t.Fatalf("нет эталона -> ErrNoRows, got %v", err)
		}
		if _, err := GetEtalon(ctxT(t), f.pool, uuid.New()); !pg.IsNoRows(err) {
			t.Fatalf("нет версии -> ErrNoRows, got %v", err)
		}
	})
}

func TestAttemptsDB(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	t.Run("GetAttempt: попытка и поля занятия", func(t *testing.T) {
		t.Parallel()
		a, err := GetAttempt(ctxT(t), f.pool, f.aSt1Seq2)
		if err != nil {
			t.Fatal(err)
		}
		if a.LessonID != f.lessonRun || a.UserID != f.st1 || a.ScenarioID != f.scFire || a.EtalonID != f.etFire2 ||
			a.SeqNo != 2 || a.Status != "in_progress" || a.TimeLimitSec != 45 || a.Mode != "cards" {
			t.Fatalf("attempt = %+v", a)
		}
		if a.LessonTitle != "Идёт" || a.LessonStatus != "running" || a.LessonKind != "class" || a.LessonOwnerID() != f.teacher {
			t.Fatalf("lesson fields = %+v", a)
		}
		if !a.VoiceEnabled() || a.LessonSettings.Perspective != model.PerspectiveDDS {
			t.Fatalf("settings = %+v", a.LessonSettings)
		}
		if a.IncidentNo < 36814852 || a.DialogueTurns != 4 || !a.CallEnded() || convert.Deref(a.CallEndReason) != "operator_hung_up" {
			t.Fatalf("voice fields = %+v", a)
		}
		if a.Card != nil || a.DecodeCard() != nil {
			t.Fatal("карточки до submit нет")
		}
		m := asJSONMap(t, AttemptToPublic(a, time.Now()))
		requireKeys(t, "Attempt", m, reqAttempt)
		if d, ok := m["dialogue"].(map[string]any); !ok || d["turnsCount"] != float64(4) || d["callEnded"] != true {
			t.Fatalf("dialogue = %v", m["dialogue"])
		}

		a, err = GetAttempt(ctxT(t), f.pool, f.aSt1Seq1)
		if err != nil {
			t.Fatal(err)
		}
		if d := a.DecodeCard(); d == nil || d.Description != "Горит" || d.IncidentTypeIds == nil {
			t.Fatalf("card = %s", a.Card)
		}
		if a.IncidentNo == 0 {
			t.Fatal("incident_no из sequence")
		}

		p, err := GetAttempt(ctxT(t), f.pool, f.aPractice)
		if err != nil || p.LessonTeacherID != nil || p.LessonOwnerID() != f.st1 || p.LessonKind != "practice" ||
			p.LessonSettings != defaultLessonSettings {
			t.Fatalf("practice = %+v %v", p, err)
		}
		if _, err := GetAttempt(ctxT(t), f.pool, uuid.New()); !pg.IsNoRows(err) {
			t.Fatalf("нет попытки -> ErrNoRows, got %v", err)
		}
	})

	t.Run("LockAttempt в транзакции", func(t *testing.T) {
		t.Parallel()
		err := pg.WithTx(ctxT(t), f.pool, func(ctx context.Context, tx pgx.Tx) error {
			a, err := LockAttempt(ctx, tx, f.aSt2)
			if err != nil {
				return err
			}
			if a.ID != f.aSt2 || a.LessonTitle != "Идёт" {
				t.Errorf("locked = %+v", a)
			}
			if _, err := LockAttempt(ctx, tx, uuid.New()); !pg.IsNoRows(err) {
				t.Errorf("нет попытки -> ErrNoRows, got %v", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ListAttempts: фильтры и порядок", func(t *testing.T) {
		t.Parallel()
		ctx := ctxT(t)
		if _, err := ListAttempts(ctx, f.pool, AttemptFilter{}); !errors.Is(err, ErrAttemptFilter) {
			t.Fatalf("без фильтра -> ErrAttemptFilter, got %v", err)
		}

		rows, err := ListAttempts(ctx, f.pool, AttemptFilter{LessonID: &f.lessonRun})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 3 {
			t.Fatalf("попытки занятия: %d", len(rows))
		}
		for i := 1; i < len(rows); i++ {
			a, b := rows[i-1], rows[i]
			if a.UserID.String() > b.UserID.String() || (a.UserID == b.UserID && a.SeqNo > b.SeqNo) {
				t.Fatalf("порядок (user_id, seq_no) нарушен: %v", rows)
			}
		}

		rows, err = ListAttempts(ctx, f.pool, AttemptFilter{UserID: &f.st1})
		if err != nil || len(rows) != 3 {
			t.Fatalf("попытки st1: %d %v", len(rows), err)
		}
		for _, r := range rows {
			if r.UserID != f.st1 {
				t.Fatalf("чужая попытка: %+v", r)
			}
		}

		rows, err = ListAttempts(ctx, f.pool, AttemptFilter{LessonID: &f.lessonRun, UserID: &f.st1})
		if err != nil || len(rows) != 2 || rows[0].ID != f.aSt1Seq1 || rows[1].ID != f.aSt1Seq2 {
			t.Fatalf("lesson+user по seq_no: %v %v", rows, err)
		}

		nobody := uuid.New()
		rows, err = ListAttempts(ctx, f.pool, AttemptFilter{UserID: &nobody})
		if err != nil || rows == nil || len(rows) != 0 {
			t.Fatalf("пусто -> []: %v %v", rows, err)
		}
		if b, _ := json.Marshal(AttemptsToPublic(rows, time.Now())); string(b) != "[]" {
			t.Fatalf("json = %s", b)
		}
	})
}

func TestDialogueAndMediaDB(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	t.Run("ListDialogueTurns: по обмену, оператор раньше заявителя", func(t *testing.T) {
		t.Parallel()
		turns, err := ListDialogueTurns(ctxT(t), f.pool, f.aSt1Seq2)
		if err != nil {
			t.Fatal(err)
		}
		type key struct {
			no      int
			speaker string
		}
		var got []key
		for _, tr := range turns {
			got = append(got, key{tr.TurnNo, tr.Speaker})
		}
		want := []key{{0, "caller"}, {1, "operator"}, {1, "caller"}, {2, "operator"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("порядок = %v, want %v", got, want)
		}
		op, cl := turns[1], turns[2]
		if op.Confidence == nil || *op.Confidence < 0.9 || op.AudioPath != nil || op.Source != "stt" || op.AtMs != 5000 ||
			op.RevealedFactIDs == nil || len(op.RevealedFactIDs) != 0 {
			t.Fatalf("operator = %+v", op)
		}
		var meta model.TurnMeta
		if err := json.Unmarshal(op.Meta, &meta); err != nil || meta.TextRaw != "какой этаж" {
			t.Fatalf("meta = %s", op.Meta)
		}
		if convert.Deref(cl.AudioPath) != "cd/2.ogg" || convert.Deref(cl.DurationMs) != 1800 ||
			!reflect.DeepEqual(cl.RevealedFactIDs, []string{"f1"}) || convert.Deref(cl.EmotionalState) != "паника" || cl.At.IsZero() {
			t.Fatalf("caller = %+v", cl)
		}
		views := DialogueTurnsToView(turns)
		for i := range views {
			requireKeys(t, "DialogueTurnView", asJSONMap(t, views[i]), reqTurnView)
		}
		if views[2].Audio == nil || views[2].Audio.AudioUrl != "/api/v1/media/tts/cd/2.ogg" {
			t.Fatalf("audio = %+v", views[2].Audio)
		}
		c := DialogueTurnsToContract(turns)
		if c[0].TurnNo != 1 || c[1].TurnNo != 2 || c[2].TurnNo != 3 || c[3].TurnNo != 4 {
			t.Fatalf("сквозная нумерация: %+v", c)
		}

		empty, err := ListDialogueTurns(ctxT(t), f.pool, f.aSt2)
		if err != nil || empty == nil || len(empty) != 0 {
			t.Fatalf("без реплик -> []: %v %v", empty, err)
		}
	})

	t.Run("GetCallScript", func(t *testing.T) {
		t.Parallel()
		cs, err := GetCallScript(ctxT(t), f.pool, f.scFire)
		if err != nil {
			t.Fatal(err)
		}
		if cs.Caller.Name != "Мария" || len(cs.Turns) != 3 || !cs.HasCallerTurn() || cs.VoiceOr("baya") != "xenia" {
			t.Fatalf("call_script = %+v", cs)
		}
		cs, err = GetCallScript(ctxT(t), f.pool, f.scArch) // call_script = '{}' (дефолт колонки)
		if err != nil || cs.Turns == nil || len(cs.Turns) != 0 {
			t.Fatalf("'{}' -> turns []: %+v %v", cs, err)
		}
		if _, err := GetCallScript(ctxT(t), f.pool, uuid.New()); !pg.IsNoRows(err) {
			t.Fatalf("нет сценария -> ErrNoRows, got %v", err)
		}
	})

	t.Run("GetTTSFiles", func(t *testing.T) {
		t.Parallel()
		// пустой вход — без запроса (nil Querier не трогается)
		m, err := GetTTSFiles(ctxT(t), nil, nil)
		if err != nil || m == nil || len(m) != 0 {
			t.Fatalf("пустой вход: %v %v", m, err)
		}
		m, err = GetTTSFiles(ctxT(t), f.pool, []string{f.fireHash1, "нет-такого", f.fireHash2})
		if err != nil {
			t.Fatal(err)
		}
		if len(m) != 2 {
			t.Fatalf("files = %+v", m)
		}
		a, b := m[f.fireHash1], m[f.fireHash2]
		if a.Hash != f.fireHash1 || a.Voice != "xenia" || a.Rate != 1 || a.FilePath != "ab/1.wav" || convert.Deref(a.DurationMs) != 2100 {
			t.Fatalf("file1 = %+v", a)
		}
		if b.Rate != 0.95 || b.DurationMs != nil || b.FilePath != "cd/2.ogg" {
			t.Fatalf("file2 = %+v (numeric -> float64, NULL длительность)", b)
		}
	})
}
