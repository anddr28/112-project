package lessons

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/settings"
)

// ---------------------------------------------------------------- GET /lessons, /lessons/{id}

func TestListAndGetLessons_DB(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e := newEnv(t, f.pool, nil)

	// Преподаватель без занятий — [] (не null).
	r := e.do(t, "GET", "/lessons", f.teacherW(), nil)
	if strings.TrimSpace(string(r.body)) != "[]" {
		t.Fatalf("пустой список: %s", r.body)
	}

	running := f.runningLesson(t, e, 1)
	draft := e.createLesson(t, f.teacherW(), lessonBody("Черновик", []uuid.UUID{f.scFire}, []uuid.UUID{f.s1}))
	other := e.createLesson(t, f.teacher2W(), lessonBody("Чужое", []uuid.UUID{f.scGas}, []uuid.UUID{f.sOther}))

	mine := decode[[]public.Lesson](t, e.do(t, "GET", "/lessons", f.teacherW(), nil), 200)
	if len(mine) != 2 || mine[0].Id != draft.Id || mine[1].Id != running.Id {
		t.Fatalf("свои занятия, новые сверху: %+v", titles(mine))
	}
	onlyRunning := decode[[]public.Lesson](t, e.do(t, "GET", "/lessons?status=running", f.teacherW(), nil), 200)
	if len(onlyRunning) != 1 || onlyRunning[0].Id != running.Id {
		t.Fatalf("фильтр status: %+v", titles(onlyRunning))
	}
	all := decode[[]public.Lesson](t, e.do(t, "GET", "/lessons", f.adminW(), nil), 200)
	if len(all) != 3 {
		t.Fatalf("админ видит все: %+v", titles(all))
	}

	// GET /lessons/{id}
	get := func(as who, id uuid.UUID) resp { return e.do(t, "GET", "/lessons/"+id.String(), as, nil) }
	if l := decode[public.Lesson](t, get(f.teacherW(), running.Id), 200); len(l.Participants) != 2 {
		t.Errorf("владелец видит всех участников: %+v", l.Participants)
	}
	decode[public.Lesson](t, get(f.adminW(), other.Id), 200)
	expectError(t, get(f.teacherW(), other.Id), 403, httpx.CodeForbidden)
	expectError(t, get(f.teacherW(), uuid.New()), 404, httpx.CodeNotFound)
	// Обучающийся-участник видит только себя (одногруппники — ПДн).
	sv := get(as(core.RoleStudent, f.s1), running.Id)
	l := decode[public.Lesson](t, sv, 200)
	if len(l.Participants) != 1 || l.Participants[0].UserId != f.s1 || strings.Contains(string(sv.body), "Петров") {
		t.Errorf("участник видит только себя: %s", sv.body)
	}
	expectError(t, get(as(core.RoleStudent, f.sFree), running.Id), 403, httpx.CodeForbidden)
}

func titles(ls []public.Lesson) []string {
	out := make([]string, len(ls))
	for i := range ls {
		out[i] = ls[i].Title
	}
	return out
}

// ---------------------------------------------------------------- GET /lessons/assigned

func TestAssigned_DB(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e := newEnv(t, f.pool, nil)
	student := as(core.RoleStudent, f.s1)

	if r := e.do(t, "GET", "/lessons/assigned", student, nil); strings.TrimSpace(string(r.body)) != "[]" {
		t.Fatalf("без занятий: %s", r.body)
	}

	draft := e.createLesson(t, f.teacherW(), lessonBody("Черновик", []uuid.UUID{f.scFire}, []uuid.UUID{f.s1, f.s2}))
	running := f.runningLesson(t, e, 1)
	finished := f.runningLesson(t, e, 1)
	decode[public.Lesson](t, e.do(t, "POST", "/lessons/"+finished.Id.String()+"/finish", f.teacherW(), nil), 200)
	cancelled := e.createLesson(t, f.teacherW(), lessonBody("Отменено", []uuid.UUID{f.scFire}, []uuid.UUID{f.s1}))
	mustExec(t, f.pool, `UPDATE lessons SET status = 'cancelled' WHERE id = $1`, cancelled.Id)
	scheduled := e.createLesson(t, f.teacherW(), lessonBody("По расписанию", []uuid.UUID{f.scFire}, []uuid.UUID{f.s1}))
	mustExec(t, f.pool, `UPDATE lessons SET status = 'scheduled' WHERE id = $1`, scheduled.Id)
	// Чужое занятие (s1 не участник) — не видно.
	e.createLesson(t, f.teacherW(), lessonBody("Не моё", []uuid.UUID{f.scFire}, []uuid.UUID{f.s2}))

	r := e.do(t, "GET", "/lessons/assigned", student, nil)
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(r.body, &raw); err != nil {
		t.Fatal(err)
	}
	items := decode[[]assignedItem](t, r, 200)
	var order []uuid.UUID
	for _, it := range items {
		order = append(order, it.Lesson.Id)
	}
	want := []uuid.UUID{running.Id, scheduled.Id, draft.Id, finished.Id}
	if len(order) != len(want) {
		t.Fatalf("занятия %v, want %v (без отменённых и чужих)", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("порядок %v, want %v (running, scheduled, draft, finished)", order, want)
		}
	}
	for i, it := range items {
		if len(it.Lesson.Participants) != 1 || it.Lesson.Participants[0].UserId != f.s1 {
			t.Errorf("%s: участники %+v — только сам обучающийся", it.Lesson.Title, it.Lesson.Participants)
		}
		_, hasAttempt := raw[i]["attempt"]
		switch it.Lesson.Id {
		case running.Id:
			a := it.Attempt
			if a == nil || a.UserId != f.s1 || a.LessonId != running.Id || a.SeqNo != 1 || a.Status != core.AttemptIssued ||
				a.LessonTitle != running.Title || a.IncidentNo == "" || a.TimeLimitSec != 60 {
				t.Errorf("текущая попытка: %+v", a)
			}
		case finished.Id:
			if it.Attempt == nil || it.Attempt.Status != core.AttemptExpired {
				t.Errorf("попытка завершённого: %+v", it.Attempt)
			}
		default:
			if hasAttempt {
				t.Errorf("%s: попытки нет — поля attempt нет (а не null): %s", it.Lesson.Title, raw[i]["attempt"])
			}
		}
	}
}

// ---------------------------------------------------------------- GET /lessons/{id}/attempts

func TestLessonAttempts_DB(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e := newEnv(t, f.pool, nil)
	running := f.runningLesson(t, e, 1)
	draft := e.createLesson(t, f.teacherW(), lessonBody("Черновик", []uuid.UUID{f.scFire}, []uuid.UUID{f.s1}))
	path := func(id uuid.UUID) string { return "/lessons/" + id.String() + "/attempts" }

	list := decode[[]public.Attempt](t, e.do(t, "GET", path(running.Id), f.teacherW(), nil), 200)
	if len(list) != 2 {
		t.Fatalf("attempts %+v", list)
	}
	for _, a := range list {
		if a.LessonId != running.Id || a.Status != core.AttemptIssued || a.IncidentNo == "" {
			t.Errorf("attempt %+v", a)
		}
	}
	decode[[]public.Attempt](t, e.do(t, "GET", path(running.Id), f.adminW(), nil), 200)
	expectError(t, e.do(t, "GET", path(running.Id), f.teacher2W(), nil), 403, httpx.CodeForbidden)
	// Попыток нет — [] (и права всё равно проверяются).
	if r := e.do(t, "GET", path(draft.Id), f.teacherW(), nil); r.status != 200 || strings.TrimSpace(string(r.body)) != "[]" {
		t.Fatalf("пустое занятие: %d %s", r.status, r.body)
	}
	expectError(t, e.do(t, "GET", path(draft.Id), f.teacher2W(), nil), 403, httpx.CodeForbidden)
	expectError(t, e.do(t, "GET", path(uuid.New()), f.teacherW(), nil), 404, httpx.CodeNotFound)
}

// ---------------------------------------------------------------- мониторинг и присутствие

func TestMonitorAndPresence_DB(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e := newEnv(t, f.pool, nil)
	ctx := context.Background()
	l := f.runningLesson(t, e, 1)
	teacher := &core.Principal{UserID: f.teacher, Role: core.RoleTeacher}
	admin := &core.Principal{UserID: f.admin, Role: core.RoleAdmin}
	teacher2 := &core.Principal{UserID: f.teacher2, Role: core.RoleTeacher}
	st1 := &core.Principal{UserID: f.s1, Role: core.RoleStudent}
	st2 := &core.Principal{UserID: f.s2, Role: core.RoleStudent}

	status := func(err error) int {
		if err == nil {
			return 200
		}
		return httpx.AsError(err).Status
	}
	for _, tc := range []struct {
		p    *core.Principal
		id   uuid.UUID
		want int
	}{
		{teacher, l.Id, 200}, {admin, l.Id, 200}, {teacher2, l.Id, 403}, {st1, l.Id, 403}, {teacher, uuid.New(), 404},
	} {
		if got := status(e.svc.CanMonitor(ctx, tc.p, tc.id)); got != tc.want {
			t.Errorf("CanMonitor(%s, %s) = %d, want %d", tc.p.Role, tc.id, got, tc.want)
		}
	}

	lesson, atts, err := e.svc.Snapshot(ctx, l.Id)
	if err != nil || lesson.Id != l.Id || len(atts) != 2 || len(lesson.Participants) != 2 {
		t.Fatalf("Snapshot: %v %+v %d", err, lesson, len(atts))
	}
	if _, _, err := e.svc.Snapshot(ctx, uuid.New()); status(err) != 404 {
		t.Errorf("Snapshot(unknown) = %v", err)
	}

	a1, a2 := f.attemptOf(t, l.Id, f.s1), f.attemptOf(t, l.Id, f.s2)
	if got, err := e.svc.AttemptAccess(ctx, st1, a1.ID); err != nil || got != l.Id {
		t.Errorf("AttemptAccess(владелец) = %s %v", got, err)
	}
	for _, tc := range []struct {
		p    *core.Principal
		id   uuid.UUID
		want int
	}{
		{st2, a1.ID, 403}, {teacher, a1.ID, 403}, {st1, uuid.New(), 404},
	} {
		if _, err := e.svc.AttemptAccess(ctx, tc.p, tc.id); status(err) != tc.want {
			t.Errorf("AttemptAccess(%s, %s) = %v, want %d", tc.p.Role, tc.id, err, tc.want)
		}
	}

	// Присутствие: до принятого вызова — joined, после — active, офлайн — disconnected.
	lastParticipant := func() *public.LessonParticipant {
		var p *public.LessonParticipant
		for _, m := range e.pub.monitorMsgs() {
			if m.LessonID == l.Id && m.Msg.Type == public.MonitorMessageTypeParticipantStatus {
				p = m.Msg.Participant
			}
		}
		return p
	}
	e.pub.reset()
	e.svc.SetOnline(ctx, a1.ID, true)
	if got := f.participantStatus(t, l.Id, f.s1); got != "joined" {
		t.Errorf("онлайн до accept: %s", got)
	}
	if p := lastParticipant(); p == nil || p.Status != "joined" || p.UserId != f.s1 || p.AttemptId == nil || *p.AttemptId != a1.ID ||
		p.Name != "Иванов И. И." {
		t.Errorf("participantStatus %+v", p)
	}
	mustExec(t, f.pool, `UPDATE lesson_participants SET joined_at = now() WHERE lesson_id = $1 AND user_id = $2`, l.Id, f.s1)
	e.svc.SetOnline(ctx, a1.ID, false)
	if got := f.participantStatus(t, l.Id, f.s1); got != "disconnected" {
		t.Errorf("офлайн: %s", got)
	}
	e.svc.SetOnline(ctx, a1.ID, true)
	if got := f.participantStatus(t, l.Id, f.s1); got != "active" {
		t.Errorf("снова онлайн после accept: %s", got)
	}
	if n := e.pub.countMonitor(l.Id, public.MonitorMessageTypeParticipantStatus); n != 3 {
		t.Errorf("participantStatus: %d, want 3", n)
	}

	// Закончивший участник не превращается в «нет связи»; неизвестная попытка — тихо.
	mustExec(t, f.pool, `UPDATE lesson_participants SET status = 'finished' WHERE lesson_id = $1 AND user_id = $2`, l.Id, f.s2)
	e.pub.reset()
	e.svc.SetOnline(ctx, a2.ID, false)
	e.svc.SetOnline(ctx, uuid.New(), true)
	if got := f.participantStatus(t, l.Id, f.s2); got != "finished" {
		t.Errorf("finished → %s", got)
	}
	if n := len(e.pub.monitorMsgs()); n != 0 {
		t.Errorf("публикаций %d, want 0", n)
	}
	// Занятие завершено — присутствие больше не пишется.
	decode[public.Lesson](t, e.do(t, "POST", "/lessons/"+l.Id.String()+"/finish", f.teacherW(), nil), 200)
	e.pub.reset()
	e.svc.SetOnline(ctx, a1.ID, true)
	if got := f.participantStatus(t, l.Id, f.s1); got != "finished" || len(e.pub.monitorMsgs()) != 0 {
		t.Errorf("после finish: %s, публикаций %d", got, len(e.pub.monitorMsgs()))
	}
}

// ---------------------------------------------------------------- демо-занятия

func TestSeedDemoLessons_DB(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	pub := &pubRecorder{}
	svc := New(Deps{Pool: f.pool, Publisher: pub, Log: discardLog()})
	ctx := context.Background()
	d := Deps{Pool: f.pool, Log: slog.New(slog.DiscardHandler)}

	if err := SeedDemoLessons(ctx, d, svc); err != nil {
		t.Fatal(err)
	}
	type row struct {
		id           uuid.UUID
		status       string
		timeLimit    int
		voice        bool
		dialogue     float64
		participants int
		attempts     int
		scenarios    int
	}
	load := func() map[string]row {
		rows, err := f.pool.Query(ctx, `
			SELECT l.title, l.id, l.status, l.time_limit_sec,
			       COALESCE((l.settings #>> '{voice,enabled}')::boolean, false),
			       COALESCE((l.settings #>> '{weights,dialogue}')::float8, 0),
			       (SELECT count(*)::int FROM lesson_participants p WHERE p.lesson_id = l.id),
			       (SELECT count(*)::int FROM attempts a WHERE a.lesson_id = l.id),
			       (SELECT count(*)::int FROM lesson_scenarios s WHERE s.lesson_id = l.id)
			  FROM lessons l WHERE l.teacher_id = $1`, f.teacher)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]row{}
		for rows.Next() {
			var (
				title string
				r     row
			)
			if err := rows.Scan(&title, &r.id, &r.status, &r.timeLimit, &r.voice, &r.dialogue, &r.participants, &r.attempts, &r.scenarios); err != nil {
				t.Fatal(err)
			}
			out[title] = r
		}
		return out
	}
	got := load()
	if len(got) != len(demoLessons) {
		t.Fatalf("демо-занятий %d, want %d: %+v", len(got), len(demoLessons), got)
	}
	for _, dl := range demoLessons {
		r, ok := got[dl.title]
		if !ok {
			t.Fatalf("нет %q", dl.title)
		}
		wantStatus, wantAttempts := core.LessonDraft, 0
		if dl.running {
			wantStatus, wantAttempts = core.LessonRunning, len(dl.participants)
		}
		if r.status != wantStatus || r.attempts != wantAttempts || r.participants != len(dl.participants) ||
			r.scenarios != len(dl.scenarios) || r.timeLimit != dl.timeLimitSec || r.voice != dl.voice {
			t.Errorf("%q: %+v", dl.title, r)
		}
		if dl.voice && r.dialogue != 0.25 {
			t.Errorf("%q: вес разговора %v", dl.title, r.dialogue)
		}
		if !dl.voice && r.dialogue != 0 {
			t.Errorf("%q: вес разговора без голоса %v", dl.title, r.dialogue)
		}
	}
	if n := pub.countMonitor(got[demoLessons[0].title].id, public.MonitorMessageTypeAttemptEvent); n != 2 {
		t.Errorf("выдача сида публикует attemptEvent после COMMIT: %d", n)
	}

	// Идемпотентно: повторный сид ничего не добавляет.
	if err := SeedDemoLessons(ctx, d, svc); err != nil {
		t.Fatal(err)
	}
	again := load()
	for title, r := range got {
		if again[title] != r {
			t.Errorf("%q изменилось после повторного сида: %+v → %+v", title, r, again[title])
		}
	}
}

func TestSeedDemoLessons_MissingData_DB(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	d := Deps{Pool: f.pool, Log: slog.New(slog.DiscardHandler)}

	// Сценарий «газ» не подтверждён, у «воды» нет эталона: занятия с ними пропускаются,
	// голосовое (пожар) создаётся — сид не падает.
	mustExec(t, f.pool, `UPDATE scenarios SET status = 'generated' WHERE id = $1`, f.scGas)
	mustExec(t, f.pool, `DELETE FROM etalons WHERE scenario_id = $1`, f.scPlain)
	if err := SeedDemoLessons(ctx, d, nil); err != nil {
		t.Fatal(err)
	}
	var titles []string
	rows, err := f.pool.Query(ctx, `SELECT title FROM lessons ORDER BY title`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		titles = append(titles, s)
	}
	rows.Close()
	if len(titles) != 1 || titles[0] != demoLessons[2].title {
		t.Fatalf("созданы %v, want только голосовое", titles)
	}
	// issuer == nil — запущенное занятие без попыток (CLI-сид без сервиса выдачи).
	if n := f.scalar(t, `SELECT count(*) FROM attempts`).(int64); n != 0 {
		t.Errorf("попыток %d", n)
	}

	// Нет демо-преподавателя — тихо ничего не делаем.
	f2 := newFixture(t)
	mustExec(t, f2.pool, `UPDATE users SET deleted_at = now(), login = 'gone' WHERE id = $1`, f2.teacher)
	if err := SeedDemoLessons(ctx, Deps{Pool: f2.pool, Log: slog.New(slog.DiscardHandler)}, nil); err != nil {
		t.Fatal(err)
	}
	if n := f2.scalar(t, `SELECT count(*) FROM lessons`).(int64); n != 0 {
		t.Errorf("без преподавателя создано %d", n)
	}

	// Настройки платформы: норматив и порог демо-занятий — из settings (не из дефолтов).
	f3 := newFixture(t)
	mustExec(t, f3.pool, `UPDATE settings SET value = '55' WHERE key = 'pass_threshold'`)
	st := settings.NewStore(f3.pool, discardLog())
	if err := st.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if err := SeedDemoLessons(ctx, Deps{Pool: f3.pool, Settings: st, Log: slog.New(slog.DiscardHandler)}, nil); err != nil {
		t.Fatal(err)
	}
	if v := f3.scalar(t, `SELECT min((settings->>'pass_threshold')::float8) FROM lessons`).(float64); v != 55 {
		t.Errorf("pass_threshold демо-занятий %v, want 55", v)
	}
}
