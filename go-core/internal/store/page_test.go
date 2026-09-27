package store

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/pgtest"
)

// ---------------------------------------------------------------- курсоры

func TestKeysRoundTrip(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 24, 12, 30, 1, 123456000, time.UTC)
	tk := TimeKey{At: at, ID: uuid.New()}
	got, ok := ParseTimeKey(tk.Parts())
	if !ok || !got.At.Equal(tk.At) || got.ID != tk.ID {
		t.Fatalf("time key round trip: %+v %v", got, ok)
	}
	ak := AssignedKey{Rank: 3, TimeKey: tk}
	back, ok := ParseAssignedKey(ak.Parts())
	if !ok || back.Rank != 3 || back.ID != tk.ID || !back.At.Equal(at) {
		t.Fatalf("assigned key round trip: %+v %v", back, ok)
	}
	// через непрозрачную строку курсора — как ходит клиент
	parts, ok := httpx.DecodeCursor(httpx.EncodeCursor(ak.Parts()...))
	if !ok {
		t.Fatal("decode cursor")
	}
	if _, ok := ParseAssignedKey(parts); !ok {
		t.Fatal("assigned key via cursor")
	}
	for _, bad := range [][]string{nil, {"x"}, {"not-time", uuid.NewString()}, {at.Format(time.RFC3339Nano), "nope"}} {
		if _, ok := ParseTimeKey(bad); ok {
			t.Fatalf("ParseTimeKey(%v) accepted", bad)
		}
	}
	for _, bad := range [][]string{{"9", at.Format(time.RFC3339Nano), uuid.NewString()}, {"-1", at.Format(time.RFC3339Nano), uuid.NewString()}} {
		if _, ok := ParseAssignedKey(bad); ok {
			t.Fatalf("ParseAssignedKey(%v) accepted", bad)
		}
	}
	if !(TimeKey{}).IsZero() || tk.IsZero() {
		t.Fatal("IsZero")
	}
}

func TestAssignedRankOrder(t *testing.T) {
	t.Parallel()
	order := []string{"running", "scheduled", "draft", "finished", "cancelled"}
	for i := 1; i < len(order); i++ {
		if AssignedRank(order[i-1]) >= AssignedRank(order[i]) {
			t.Fatalf("%s must rank above %s", order[i-1], order[i])
		}
	}
}

// ---------------------------------------------------------------- страницы на живой БД

// pageLessons обходит список страницами по limit так же, как хендлер (limit+1, курсор последней).
func pageLessons(t *testing.T, q *pgxpool.Pool, f LessonFilter, limit int, assigned bool) []uuid.UUID {
	t.Helper()
	var out []uuid.UUID
	for guard := 0; guard < 100; guard++ {
		f.Limit = limit + 1
		rows, err := ListLessons(ctxT(t), q, f)
		if err != nil {
			t.Fatal(err)
		}
		more := len(rows) > limit
		if more {
			rows = rows[:limit]
		}
		for i := range rows {
			out = append(out, rows[i].ID)
		}
		if !more {
			return out
		}
		last := &rows[len(rows)-1]
		if assigned {
			f.AssignedAfter = last.AssignedKey()
		} else {
			f.After = last.Key()
		}
	}
	t.Fatal("pagination did not terminate")
	return nil
}

func TestDBListLessonsPagination(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	ctx := ctxT(t)
	teacher := insertUser(t, pool, "p_teacher", "teacher", "Учителев", "Пётр", nil)
	other := insertUser(t, pool, "p_other", "teacher", "Другой", "Иван", nil)
	student := insertUser(t, pool, "p_student", "student", "Студентова", "Анна", nil)

	// 9 занятий преподавателя: у четырёх одинаковый created_at (порядок решает id), статусы вперемешку
	same := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	statuses := []string{"running", "finished", "draft", "cancelled", "scheduled", "finished", "running", "draft", "finished"}
	var mine []uuid.UUID
	for i, st := range statuses {
		created := same
		if i >= 4 {
			created = same.Add(time.Duration(i) * time.Minute)
		}
		id := mustID(t, pool, `INSERT INTO lessons (title, teacher_id, created_by, mode, status, created_at)
			VALUES ($1, $2, $2, 'cards', $3, $4) RETURNING id`, "Занятие", teacher, st, created)
		mustExec(t, pool, `INSERT INTO lesson_participants (lesson_id, user_id) VALUES ($1, $2)`, id, student)
		mine = append(mine, id)
	}
	mustID(t, pool, `INSERT INTO lessons (title, teacher_id, created_by, mode, status) VALUES ('Чужое', $1, $1, 'cards', 'draft') RETURNING id`, other)

	// эталон: тот же запрос без пагинации — все строки в порядке сортировки
	all := func(f LessonFilter) []uuid.UUID {
		f.Limit = 0
		rows, err := ListLessons(ctx, pool, f)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]uuid.UUID, len(rows))
		for i := range rows {
			ids[i] = rows[i].ID
		}
		return ids
	}
	for _, tc := range []struct {
		name     string
		f        LessonFilter
		assigned bool
		want     int
	}{
		{"admin", LessonFilter{}, false, 10},
		{"teacher", LessonFilter{TeacherID: &teacher}, false, 9},
		{"teacher+status", LessonFilter{TeacherID: &teacher, Status: "finished"}, false, 3},
		{"assigned", LessonFilter{ParticipantUserID: &student, ExcludeCancelled: true}, true, 8},
	} {
		want := all(tc.f)
		if len(want) != tc.want {
			t.Fatalf("%s: full list %d rows, want %d", tc.name, len(want), tc.want)
		}
		for _, limit := range []int{1, 2, 3, 4, 100} {
			got := pageLessons(t, pool, tc.f, limit, tc.assigned)
			if len(got) != len(want) {
				t.Fatalf("%s limit %d: %d rows, want %d", tc.name, limit, len(got), len(want))
			}
			seen := map[uuid.UUID]bool{}
			for i := range got {
				if got[i] != want[i] || seen[got[i]] {
					t.Fatalf("%s limit %d: row %d = %s, want %s (дубль/пропуск/порядок)", tc.name, limit, i, got[i], want[i])
				}
				seen[got[i]] = true
			}
		}
	}

	// обучающийся: сначала идущие, затем запланированные, черновики, завершённые; без отменённых
	rows, err := ListLessons(ctx, pool, LessonFilter{ParticipantUserID: &student, ExcludeCancelled: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(rows); i++ {
		a, b := AssignedRank(rows[i-1].Status), AssignedRank(rows[i].Status)
		if a > b || (a == b && rows[i-1].CreatedAt.Before(rows[i].CreatedAt)) {
			t.Fatalf("assigned order broken at %d: %s(%v) before %s(%v)", i, rows[i-1].Status, rows[i-1].CreatedAt, rows[i].Status, rows[i].CreatedAt)
		}
		if rows[i].Status == "cancelled" {
			t.Fatal("cancelled lesson in the student list")
		}
	}
	_ = mine
}

func TestDBListScenariosPagination(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	ctx := ctxT(t)
	cat := mustID(t, pool, `INSERT INTO classifier_categories (code, name) VALUES ('pg1', 'Пожар') RETURNING id`)
	same := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	for i := 0; i < 7; i++ {
		created := same
		if i%2 == 0 {
			created = same.Add(time.Duration(i) * time.Second)
		}
		mustExec(t, pool, `INSERT INTO scenarios (title, category_id, source, status, created_at) VALUES ($1, $2, 'manual', 'draft', $3)`,
			"Сценарий", cat, created)
	}
	full, err := ListScenarios(ctx, pool, ScenarioFilter{})
	if err != nil || len(full) != 7 {
		t.Fatalf("full list: %d %v", len(full), err)
	}
	for _, limit := range []int{1, 2, 3, 7} {
		var got []uuid.UUID
		f := ScenarioFilter{}
		for guard := 0; ; guard++ {
			if guard > 20 {
				t.Fatal("no termination")
			}
			f.Limit = limit + 1
			rows, err := ListScenarios(ctx, pool, f)
			if err != nil {
				t.Fatal(err)
			}
			more := len(rows) > limit
			if more {
				rows = rows[:limit]
			}
			for i := range rows {
				got = append(got, rows[i].ID)
			}
			if !more {
				break
			}
			f.After = rows[len(rows)-1].Key()
		}
		if len(got) != len(full) {
			t.Fatalf("limit %d: %d rows", limit, len(got))
		}
		for i := range got {
			if got[i] != full[i].ID {
				t.Fatalf("limit %d: order differs at %d", limit, i)
			}
		}
	}
}

// Страница читается индексом в порядке сортировки: над LIMIT нет Sort всей выборки.
// Проверяется на реалистичном объёме (тысячи строк + ANALYZE), без подсказок планировщику —
// именно это держит базу на больших списках (сотни тысяч занятий/сценариев).
func TestDBListPlansUseIndex(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	ctx := ctxT(t)
	teacher := insertUser(t, pool, "x_teacher", "teacher", "Учителев", "Пётр", nil)
	cat := mustID(t, pool, `INSERT INTO classifier_categories (code, name) VALUES ('px1', 'Пожар') RETURNING id`)
	// 5 преподавателей по 1000 занятий: опасный случай — у преподавателя тысячи занятий, и
	// страница не должна сортировать их все (у «лёгкого» преподавателя сортировка его сотни
	// строк допустима — планировщик вправе её выбрать)
	mustExec(t, pool, `INSERT INTO users (login, password_hash, role, last_name, first_name)
		SELECT 'x_t' || g, 'x', 'teacher', 'Преподаватель' || g, 'Имя' FROM generate_series(1, 4) g`)
	mustExec(t, pool, `INSERT INTO lessons (title, teacher_id, created_by, mode, status, created_at)
		SELECT 'Занятие ' || g, t.id, t.id, 'cards', (ARRAY['draft','running','finished'])[1 + g % 3],
		       now() - make_interval(secs => g)
		  FROM generate_series(1, 5000) g
		  JOIN LATERAL (SELECT id FROM users WHERE role = 'teacher' ORDER BY id OFFSET g % 5 LIMIT 1) t ON true`)
	mustExec(t, pool, `INSERT INTO scenarios (title, category_id, source, status, created_at)
		SELECT 'Сценарий ' || g, $1, 'generated', (ARRAY['draft','generated','validated'])[1 + g % 3],
		       now() - make_interval(secs => g)
		  FROM generate_series(1, 5000) g`, cat)
	for i := 0; i < 200; i++ {
		insertUser(t, pool, "x_u"+strconvI(i), "student", "Фамилия"+strconvI(i%37), "Имя", nil)
	}
	mustExec(t, pool, `ANALYZE lessons, scenarios, users, lesson_participants, lesson_scenarios, etalons`)

	for _, tc := range []struct {
		name, sql string
		indexes   []string // любой из них — порядок из индекса
		args      []any
	}{
		{"admin lessons", sqlListLessons, []string{"lessons_created_idx"}, []any{nil, keysetTop, uuidMax, 101}},
		{"teacher lessons", sqlListLessonsByTeacher, []string{"lessons_teacher_created_idx", "lessons_created_idx"},
			[]any{teacher, nil, keysetTop, uuidMax, 101}},
		{"scenarios", sqlListScenarios, []string{"scenarios_created_idx"}, []any{nil, nil, nil, false, keysetTop, uuidMax, 101}},
	} {
		rows, err := pool.Query(ctx, "EXPLAIN "+tc.sql, tc.args...)
		if err != nil {
			t.Fatalf("%s: explain: %v", tc.name, err)
		}
		var lines []string
		for rows.Next() {
			var l string
			if err := rows.Scan(&l); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, l)
		}
		rows.Close()
		plan := strings.Join(lines, "\n")
		// Скан основной таблицы — упорядоченный индекс (не Backward), и НАД ним в плане нет
		// Sort: предки печатаются выше потомка, Sort внутри SubPlan (участники одного занятия,
		// десятки строк) идёт ниже и допустим.
		scan := -1
		for i, l := range lines {
			for _, ix := range tc.indexes {
				if scan < 0 && strings.Contains(l, "Index Scan using "+ix) && !strings.Contains(l, "Backward") {
					scan = i
				}
			}
		}
		if !strings.HasPrefix(strings.TrimSpace(lines[0]), "Limit") || scan < 0 {
			t.Fatalf("%s: page must be an ordered index scan (%v) under Limit:\n%s", tc.name, tc.indexes, plan)
		}
		for _, l := range lines[:scan] {
			trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "->"))
			if strings.HasPrefix(trimmed, "Sort") {
				t.Fatalf("%s: Sort above the table scan — whole selection is sorted:\n%s", tc.name, plan)
			}
		}
	}
}

func strconvI(i int) string { return strconv.Itoa(i) }
