package lessons

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/settings"
)

// ---------------------------------------------------------------- POST /lessons

func TestCreateLesson_DB(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e := newEnv(t, f.pool, nil)

	r := e.do(t, "POST", "/lessons", f.teacherW(),
		lessonBody("Пожары и газ", []uuid.UUID{f.scGas, f.scFire}, []uuid.UUID{f.s2, f.s1, f.sFree}))
	obj := rawKeys(t, r.body)
	for _, k := range []string{"id", "kind", "title", "teacherId", "mode", "perspective", "timeLimitSec", "status",
		"scenarioIds", "participants", "settings", "createdAt"} {
		if _, ok := obj[k]; !ok {
			t.Errorf("Lesson без обязательного %q", k)
		}
	}
	requireArray(t, obj, "scenarioIds")
	requireArray(t, obj, "participants")
	l := decode[public.Lesson](t, r, 201)
	if l.Status != core.LessonDraft || l.Kind != "class" || l.TeacherId != f.teacher || l.TimeLimitSec != 60 || l.Title != "Пожары и газ" {
		t.Fatalf("lesson %+v", l)
	}
	if len(l.ScenarioIds) != 2 || l.ScenarioIds[0] != f.scGas || l.ScenarioIds[1] != f.scFire {
		t.Errorf("пул %v: порядок входа = sort_order", l.ScenarioIds)
	}
	if len(l.Participants) != 3 {
		t.Fatalf("participants %+v", l.Participants)
	}
	for _, p := range l.Participants {
		if p.Status != "assigned" || p.Name == "" || p.AttemptId != nil {
			t.Errorf("participant %+v", p)
		}
	}
	w := l.Settings.Weights
	if s := w.Fields + w.Semantic + w.Grammar + w.Timing + w.Dialogue; s < 0.999 || s > 1.001 || w.Dialogue != 0 {
		t.Errorf("weights %+v", w)
	}
	if n := f.scalar(t, `SELECT count(*) FROM lesson_categories WHERE lesson_id = $1`, l.Id).(int64); n != 2 {
		t.Errorf("lesson_categories: %d, want 2 (различные категории пула)", n)
	}
	if a := e.audit.byAction("lesson.create"); len(a) != 1 || a[0].EntityID != l.Id {
		t.Errorf("audit %+v", a)
	}
}

func TestCreateLesson_ParticipantVisibility_DB(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e := newEnv(t, f.pool, nil)
	sc := []uuid.UUID{f.scFire}

	// Регрессия (IDOR): студент чужой группы, заблокированный «ничей», преподаватель — для
	// преподавателя их нет; ни ФИО, ни статус в ответ не попадают.
	for _, tc := range []struct {
		name   string
		id     uuid.UUID
		hidden string
	}{
		{"студент чужой группы", f.sOther, "Чужой"},
		{"заблокированный без группы", f.sFreeBlock, "Скрытный"},
		{"преподаватель", f.teacher2, "Кузнецов"},
		{"админ", f.admin, "Админов"},
	} {
		r := e.do(t, "POST", "/lessons", f.teacherW(), lessonBody("Занятие", sc, []uuid.UUID{f.s1, tc.id}))
		er := expectError(t, r, 400, httpx.CodeValidation)
		if strings.Contains(string(r.body), tc.hidden) {
			t.Errorf("%s: ответ раскрывает ФИО: %s", tc.name, r.body)
		}
		if !strings.Contains(er.field("participantIds"), tc.id.String()) {
			t.Errorf("%s: fields.participantIds %q без id", tc.name, er.field("participantIds"))
		}
	}
	if n := f.scalar(t, `SELECT count(*) FROM lessons`).(int64); n != 0 {
		t.Fatalf("создано %d занятий", n)
	}

	// Свой заблокированный — видим, но назначить нельзя (с ФИО — это его группа).
	r := e.do(t, "POST", "/lessons", f.teacherW(), lessonBody("Занятие", sc, []uuid.UUID{f.sOwnBlocked}))
	er := expectError(t, r, 400, httpx.CodeValidation)
	if !strings.Contains(er.Message, "Блокова") {
		t.Errorf("свой заблокированный: %s", r.body)
	}

	// Тот же студент чужой группы виден своему преподавателю.
	e.createLesson(t, f.teacher2W(), lessonBody("Занятие ДДС-2", sc, []uuid.UUID{f.sOther}))

	// Бывший участник занятий преподавателя виден ему и после ухода в чужую группу.
	mustExec(t, f.pool, `WITH l AS (
		INSERT INTO lessons (kind, title, teacher_id, created_by, mode, status) VALUES ('class', 'Старое', $1, $1, 'cards', 'finished') RETURNING id)
		INSERT INTO lesson_participants (lesson_id, user_id, status) SELECT id, $2, 'finished' FROM l`, f.teacher, f.sOther)
	e.createLesson(t, f.teacherW(), lessonBody("С бывшим участником", sc, []uuid.UUID{f.sOther}))

	// Без группы и активный — виден всем преподавателям (только что созданная учётка).
	e.createLesson(t, f.teacher2W(), lessonBody("Новичок", sc, []uuid.UUID{f.sFree}))

	// Неизвестный id.
	ghost := uuid.New()
	er = expectError(t, e.do(t, "POST", "/lessons", f.teacherW(), lessonBody("Занятие", sc, []uuid.UUID{ghost})), 400, httpx.CodeValidation)
	if !strings.Contains(er.field("participantIds"), ghost.String()) {
		t.Errorf("unknown: %+v", er)
	}
}

func TestCreateLesson_ScenarioChecks_DB(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e := newEnv(t, f.pool, nil)
	st := []uuid.UUID{f.s1}
	for _, tc := range []struct {
		name   string
		body   map[string]any
		status int
	}{
		{"неподтверждённый", lessonBody("Занятие", []uuid.UUID{f.scFire, f.scDraft}, st), 400},
		{"режим card_actions в cards", lessonBody("Занятие", []uuid.UUID{f.scActions}, st), 400},
		{"неизвестный сценарий", lessonBody("Занятие", []uuid.UUID{uuid.New()}, st), 400},
	} {
		expectError(t, e.do(t, "POST", "/lessons", f.teacherW(), tc.body), tc.status, httpx.CodeValidation)
	}
	// card_actions-занятие принимает card_actions и both.
	body := lessonBody("Действия", []uuid.UUID{f.scActions, f.scFire}, st)
	body["mode"] = core.ModeCardActions
	if l := e.createLesson(t, f.teacherW(), body); l.Mode != core.ModeCardActions {
		t.Fatalf("mode %s", l.Mode)
	}

	// Голос: сценарий без брифа/чек-листа — 422 с перечнем.
	body = lessonBody("Голос", []uuid.UUID{f.scFire, f.scPlain}, st)
	body["voice"] = map[string]any{"enabled": true}
	er := expectError(t, e.do(t, "POST", "/lessons", f.teacherW(), body), 422, httpx.CodeValidation)
	if ids, _ := er.Details["scenarioIds"].([]any); len(ids) != 1 || ids[0] != f.scPlain.String() {
		t.Errorf("details %+v", er.Details)
	}
}

func TestCreateLesson_VoiceWeights_DB(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e := newEnv(t, f.pool, nil)
	voice := map[string]any{"enabled": true, "input": "both", "pushToTalk": true, "maxTurns": 6, "ttsEnabled": true}

	// Регрессия: преподаватель выставил разговору 0% — так и сохраняется (раньше сервер
	// подменял на 25% и ужимал остальные слои).
	body := lessonBody("Голос без оценки разговора", []uuid.UUID{f.scFire}, []uuid.UUID{f.s1})
	body["voice"] = voice
	body["weights"] = map[string]float64{"fields": 0.5, "semantic": 0.2, "grammar": 0.1, "timing": 0.2, "dialogue": 0}
	l := e.createLesson(t, f.teacherW(), body)
	w := l.Settings.Weights
	if w.Dialogue != 0 || w.Fields != 0.5 || w.Semantic != 0.2 || w.Grammar != 0.1 || w.Timing != 0.2 || !l.Settings.Voice.Enabled {
		t.Fatalf("weights %+v voice %+v", w, l.Settings.Voice)
	}
	// И в БД (оттуда их берёт оценка).
	raw := f.scalar(t, `SELECT settings->'weights' FROM lessons WHERE id = $1`, l.Id)
	b, _ := json.Marshal(raw)
	var stored map[string]float64
	_ = json.Unmarshal(b, &stored)
	if stored["dialogue"] != 0 || stored["fields"] != 0.5 {
		t.Fatalf("lessons.settings.weights %s", b)
	}

	// Весов нет — доля разговора по умолчанию.
	body = lessonBody("Голос по умолчанию", []uuid.UUID{f.scFire}, []uuid.UUID{f.s1})
	body["voice"] = voice
	l = e.createLesson(t, f.teacherW(), body)
	if l.Settings.Weights.Dialogue != 0.25 {
		t.Fatalf("weights %+v", l.Settings.Weights)
	}
}

func TestCreateLesson_TimeLimitFromSettings_DB(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// Администратор поменял норматив по умолчанию на 45 с.
	mustExec(t, f.pool, `UPDATE settings SET value = '45' WHERE key = 'time_limit_sec'`)
	st := settings.NewStore(f.pool, discardLog())
	if err := st.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, f.pool, st)
	body := lessonBody("Без норматива", []uuid.UUID{f.scFire}, []uuid.UUID{f.s1})
	delete(body, "timeLimitSec")
	if l := e.createLesson(t, f.teacherW(), body); l.TimeLimitSec != 45 {
		t.Fatalf("timeLimitSec %d, want 45 из settings.time_limit_sec", l.TimeLimitSec)
	}
	// Явный норматив важнее настройки.
	body["timeLimitSec"] = 90
	if l := e.createLesson(t, f.teacherW(), body); l.TimeLimitSec != 90 {
		t.Fatalf("timeLimitSec %d", l.TimeLimitSec)
	}
}

// ---------------------------------------------------------------- POST /lessons/{id}/start

func TestStartLesson_DB(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e := newEnv(t, f.pool, nil)
	body := lessonBody("Старт", []uuid.UUID{f.scFire, f.scGas}, []uuid.UUID{f.s1, f.s2})
	body["cardsPerStudent"] = 2
	l := e.createLesson(t, f.teacherW(), body)
	path := "/lessons/" + l.Id.String() + "/start"

	expectError(t, e.do(t, "POST", path, f.teacher2W(), nil), 403, httpx.CodeForbidden)
	expectError(t, e.do(t, "POST", "/lessons/"+uuid.NewString()+"/start", f.teacherW(), nil), 404, httpx.CodeNotFound)
	e.pub.reset()

	out := decode[public.Lesson](t, e.do(t, "POST", path, f.teacherW(), nil), 200)
	if out.Status != core.LessonRunning || out.StartedAt == nil {
		t.Fatalf("lesson %+v", out)
	}
	for _, p := range out.Participants {
		if p.AttemptId == nil {
			t.Errorf("участник %s без попытки", p.Name)
		}
	}
	atts := f.attempts(t, l.Id)
	if len(atts) != 2 {
		t.Fatalf("attempts %+v", atts)
	}
	// Участники по user_id: первый получает pool[0], второй — pool[1].
	if atts[0].ScenarioID != f.scFire || atts[1].ScenarioID != f.scGas {
		t.Errorf("сценарии %v / %v: соседям — разные, по кругу", atts[0].ScenarioID, atts[1].ScenarioID)
	}
	for _, a := range atts {
		if a.SeqNo != 1 || a.Status != core.AttemptIssued || a.TimeLimit != 60 {
			t.Errorf("attempt %+v", a)
		}
		want := f.etFire
		if a.ScenarioID == f.scGas {
			want = f.etGas
		}
		if a.EtalonID != want {
			t.Errorf("etalon %s, want текущий %s", a.EtalonID, want)
		}
		if n := f.scalar(t, `SELECT count(*) FROM attempt_drafts WHERE attempt_id = $1`, a.ID).(int64); n != 1 {
			t.Errorf("черновик попытки: %d", n)
		}
		sc := f.scalar(t, `SELECT payload->>'scenarioId' FROM attempt_events WHERE attempt_id = $1 AND type = 'issued'`, a.ID)
		if sc != a.ScenarioID.String() {
			t.Errorf("событие issued: %v", sc)
		}
	}
	if n := e.pub.countMonitor(l.Id, public.MonitorMessageTypeLessonStatus); n != 1 {
		t.Errorf("lessonStatus: %d", n)
	}
	if n := e.pub.countMonitor(l.Id, public.MonitorMessageTypeParticipantStatus); n != 2 {
		t.Errorf("participantStatus: %d", n)
	}
	if n := e.pub.countMonitor(l.Id, public.MonitorMessageTypeAttemptEvent); n != 2 {
		t.Errorf("attemptEvent: %d", n)
	}
	if a := e.audit.byAction("lesson.start"); len(a) != 1 || a[0].After.(lessonAudit).AttemptsIssued != 2 {
		t.Errorf("audit %+v", a)
	}

	// Повторный старт — 409, ничего не выдаётся.
	er := expectError(t, e.do(t, "POST", path, f.teacherW(), nil), 409, httpx.CodeConflict)
	if !strings.Contains(er.Message, "запущено") {
		t.Errorf("message %q", er.Message)
	}
	if n := len(f.attempts(t, l.Id)); n != 2 {
		t.Errorf("после повторного старта попыток %d", n)
	}

	// Админ может запустить чужое занятие; пустой пул — 409.
	empty := mustID(t, f.pool, `INSERT INTO lessons (kind, title, teacher_id, created_by, mode, status)
		VALUES ('class', 'Пустое', $1, $1, 'cards', 'draft') RETURNING id`, f.teacher)
	expectError(t, e.do(t, "POST", "/lessons/"+empty.String()+"/start", f.adminW(), nil), 409, httpx.CodeConflict)
	finished := mustID(t, f.pool, `INSERT INTO lessons (kind, title, teacher_id, created_by, mode, status)
		VALUES ('class', 'Готово', $1, $1, 'cards', 'finished') RETURNING id`, f.teacher)
	er = expectError(t, e.do(t, "POST", "/lessons/"+finished.String()+"/start", f.teacherW(), nil), 409, httpx.CodeConflict)
	if !strings.Contains(er.Message, "завершено") {
		t.Errorf("message %q", er.Message)
	}
}

// ---------------------------------------------------------------- IssueNext

func TestIssueNext_DB(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e := newEnv(t, f.pool, nil)
	l := f.runningLesson(t, e, 2)
	ctx := context.Background()
	issue := func(lessonID, userID uuid.UUID) (uuid.UUID, bool, error) {
		var (
			id uuid.UUID
			ok bool
		)
		err := pg.WithTx(ctx, f.pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			id, ok, err = e.svc.IssueNext(ctx, tx, lessonID, userID)
			return err
		})
		return id, ok, err
	}

	first := f.attemptOf(t, l.Id, f.s1)
	e.pub.reset()
	id, ok, err := issue(l.Id, f.s1)
	if err != nil || !ok {
		t.Fatalf("IssueNext: %v %v", ok, err)
	}
	second := f.attemptOf(t, l.Id, f.s1)
	if second.ID != id || second.SeqNo != 2 || second.Status != core.AttemptIssued {
		t.Fatalf("вторая карточка %+v (id %s)", second, id)
	}
	if second.ScenarioID == first.ScenarioID {
		t.Errorf("подряд тот же сценарий %s", first.ScenarioID)
	}
	if n := f.scalar(t, `SELECT count(*) FROM attempt_drafts WHERE attempt_id = $1`, id).(int64); n != 1 {
		t.Errorf("черновик: %d", n)
	}
	// После COMMIT — participantStatus с новым attemptId и attemptEvent issued.
	var sawParticipant, sawEvent bool
	for _, m := range e.pub.monitorMsgs() {
		switch m.Msg.Type {
		case public.MonitorMessageTypeParticipantStatus:
			sawParticipant = m.Msg.Participant != nil && m.Msg.Participant.AttemptId != nil && *m.Msg.Participant.AttemptId == id &&
				m.Msg.Participant.Name == "Иванов И. И."
		case public.MonitorMessageTypeAttemptEvent:
			sawEvent = m.Msg.AttemptId != nil && *m.Msg.AttemptId == id && m.Msg.Event != nil && m.Msg.Event.Type == core.EventIssued
		}
	}
	if !sawParticipant || !sawEvent {
		t.Errorf("публикации %+v", e.pub.monitorMsgs())
	}

	// Лимит карточек исчерпан.
	if id, ok, err := issue(l.Id, f.s1); err != nil || ok || id != uuid.Nil {
		t.Errorf("сверх лимита: %s %v %v", id, ok, err)
	}
	// Не участник.
	if _, ok, err := issue(l.Id, f.sFree); err != nil || ok {
		t.Errorf("не участник: %v %v", ok, err)
	}
	// Занятие не идёт.
	draft := e.createLesson(t, f.teacherW(), lessonBody("Черновик", []uuid.UUID{f.scFire}, []uuid.UUID{f.s1}))
	if _, ok, err := issue(draft.Id, f.s1); err != nil || ok {
		t.Errorf("draft: %v %v", ok, err)
	}
	// Неизвестное занятие — ошибка.
	if _, _, err := issue(uuid.New(), f.s1); err == nil {
		t.Error("неизвестное занятие: нет ошибки")
	}
	// У сценариев пула нет текущего эталона — ошибка (а не попытка без эталона).
	l2 := f.runningLessonOnly(t, e, f.scPlain, f.s2, 3)
	mustExec(t, f.pool, `UPDATE etalons SET is_current = false WHERE scenario_id = $1`, f.scPlain)
	if _, _, err := issue(l2, f.s2); err == nil || !strings.Contains(err.Error(), "эталон") {
		t.Errorf("без эталона: %v", err)
	}
}

// runningLessonOnly — запущенное занятие из одного сценария для одного обучающегося.
func (f *fixture) runningLessonOnly(t *testing.T, e *testEnv, sc, user uuid.UUID, cards int) uuid.UUID {
	t.Helper()
	body := lessonBody("Один сценарий", []uuid.UUID{sc}, []uuid.UUID{user})
	body["cardsPerStudent"] = cards
	l := e.createLesson(t, f.teacherW(), body)
	e.startLesson(t, f.teacherW(), l.Id)
	return l.Id
}

// Две транзакции выдают один и тот же seq_no (гонка двух submit): вторая не падает, а
// возвращает уже выданную карточку и ничего не публикует.
func TestIssueNext_ConcurrentSameSeq_DB(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e := newEnv(t, f.pool, nil)
	l := f.runningLesson(t, e, 3)
	ctx := context.Background()

	issued1, release := make(chan uuid.UUID, 1), make(chan struct{})
	res1 := make(chan error, 1)
	go func() {
		res1 <- pg.WithTx(ctx, f.pool, func(ctx context.Context, tx pgx.Tx) error {
			id, ok, err := e.svc.IssueNext(ctx, tx, l.Id, f.s1)
			if err != nil || !ok {
				issued1 <- uuid.Nil
				return err
			}
			issued1 <- id
			<-release
			return nil
		})
	}()
	id1 := <-issued1
	if id1 == uuid.Nil {
		t.Fatal("первая выдача не удалась")
	}
	e.pub.reset()
	type result struct {
		id  uuid.UUID
		ok  bool
		err error
	}
	res2 := make(chan result, 1)
	go func() {
		var r result
		r.err = pg.WithTx(ctx, f.pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			r.id, r.ok, err = e.svc.IssueNext(ctx, tx, l.Id, f.s1)
			return err
		})
		res2 <- r
	}()
	waitLockWaiters(t, f.pool, 1) // вторая ждёт уникальный индекс (lesson, user, seq)
	close(release)
	if err := <-res1; err != nil {
		t.Fatal(err)
	}
	r := <-res2
	if r.err != nil || !r.ok || r.id != id1 {
		t.Fatalf("вторая выдача: %+v, want (%s, true)", r, id1)
	}
	if n := f.scalar(t, `SELECT count(*) FROM attempts WHERE lesson_id = $1 AND user_id = $2`, l.Id, f.s1).(int64); n != 2 {
		t.Fatalf("попыток %d, want 2", n)
	}
	// Публикует только победитель (после своего COMMIT): одна выдача — одно событие issued.
	if n, m := e.pub.countMonitor(l.Id, public.MonitorMessageTypeAttemptEvent),
		e.pub.countMonitor(l.Id, public.MonitorMessageTypeParticipantStatus); n != 1 || m != 1 {
		t.Errorf("attemptEvent %d, participantStatus %d — want 1 и 1 (проигравшая гонку транзакция молчит)", n, m)
	}
}

// ---------------------------------------------------------------- POST /lessons/{id}/finish

func TestFinishLesson_DB(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e := newEnv(t, f.pool, nil)
	l := f.runningLesson(t, e, 1)
	a1, a2 := f.attemptOf(t, l.Id, f.s1), f.attemptOf(t, l.Id, f.s2)
	// s1 разговаривает (открытый звонок), s2 уже сдал — его попытка не истекает.
	mustExec(t, f.pool, `UPDATE attempts SET status = 'in_progress', call_accepted_at = now(), dialogue_turns = 3 WHERE id = $1`, a1.ID)
	mustExec(t, f.pool, `UPDATE attempts SET status = 'evaluating', call_accepted_at = now(), submitted_at = now() WHERE id = $1`, a2.ID)
	path := "/lessons/" + l.Id.String() + "/finish"

	expectError(t, e.do(t, "POST", path, f.teacher2W(), nil), 403, httpx.CodeForbidden)
	expectError(t, e.do(t, "POST", "/lessons/"+uuid.NewString()+"/finish", f.teacherW(), nil), 404, httpx.CodeNotFound)
	draft := e.createLesson(t, f.teacherW(), lessonBody("Черновик", []uuid.UUID{f.scFire}, []uuid.UUID{f.s1}))
	er := expectError(t, e.do(t, "POST", "/lessons/"+draft.Id.String()+"/finish", f.teacherW(), nil), 409, httpx.CodeConflict)
	if !strings.Contains(er.Message, "не запущено") {
		t.Errorf("message %q", er.Message)
	}
	mustExec(t, f.pool, `UPDATE lessons SET status = 'cancelled' WHERE id = $1`, draft.Id)
	expectError(t, e.do(t, "POST", "/lessons/"+draft.Id.String()+"/finish", f.teacherW(), nil), 409, httpx.CodeConflict)
	e.pub.reset()

	out := decode[public.Lesson](t, e.do(t, "POST", path, f.teacherW(), nil), 200)
	if out.Status != core.LessonFinished || out.FinishedAt == nil {
		t.Fatalf("lesson %+v", out)
	}
	for _, p := range out.Participants {
		if p.Status != "finished" || p.FinishedAt == nil {
			t.Errorf("participant %+v", p)
		}
	}
	got1, got2 := f.attemptOf(t, l.Id, f.s1), f.attemptOf(t, l.Id, f.s2)
	if got1.Status != core.AttemptExpired || got1.CallEndedAt == nil || got1.CallEndReason == nil || *got1.CallEndReason != "timeout" {
		t.Errorf("открытая попытка: %+v", got1)
	}
	if got2.Status != core.AttemptEvaluating || got2.CallEndedAt != nil {
		t.Errorf("сданная попытка не трогается: %+v", got2)
	}
	students := map[uuid.UUID]bool{}
	for _, m := range e.pub.studentMsgs() {
		if m.Msg.Type == public.StudentMessageTypeLessonFinished {
			students[m.AttemptID] = true
		}
	}
	if !students[a1.ID] || !students[a2.ID] {
		t.Errorf("lessonFinished студентам: %v", students)
	}
	if n := e.pub.countMonitor(l.Id, public.MonitorMessageTypeLessonStatus); n != 1 {
		t.Errorf("lessonStatus: %d", n)
	}
	fin := e.audit.byAction("lesson.finish")
	if len(fin) != 1 || fin[0].After.(lessonAudit).AttemptsExpired != 1 {
		t.Fatalf("audit %+v", fin)
	}

	// Повтор после обрыва связи — 200 с текущим состоянием, без повторных побочных эффектов.
	e.pub.reset()
	again := decode[public.Lesson](t, e.do(t, "POST", path, f.adminW(), nil), 200)
	if again.Status != core.LessonFinished {
		t.Fatalf("повтор: %+v", again)
	}
	if len(e.pub.monitorMsgs())+len(e.pub.studentMsgs()) != 0 || len(e.audit.byAction("lesson.finish")) != 1 {
		t.Error("повторный finish публикует/аудирует ещё раз")
	}
	// После завершения новых карточек нет.
	err := pg.WithTx(context.Background(), f.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, ok, err := e.svc.IssueNext(ctx, tx, l.Id, f.s2)
		if ok {
			t.Error("выдача после finish")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Регрессия (deadlock finish ↔ submit/accept): accept-call и submit (attempts) берут занятие
// FOR SHARE, затем попытку FOR NO KEY UPDATE; finish раньше блокировал попытки (UPDATE
// attempts) и лишь потом занятие — встречный порядок, PostgreSQL рвал одну из транзакций
// (40P01): 500 у преподавателя или у сдающего, карточка терялась. Здесь транзакция S
// воспроизводит ровно операторы attempts.lockLessonThenAttempt.
func TestFinish_NoDeadlockWithSubmitOrAccept_DB(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		sIsS2          bool   // S — попытка s2 (выдана), иначе s1 (вызов принят)
		write          string // что S пишет в попытку под замком
		wantS1, wantS2 string // статусы попыток после finish
	}{
		{
			name:   "submit",
			write:  `UPDATE attempts SET status = 'submitted', submitted_at = now() WHERE id = $1`,
			wantS1: core.AttemptSubmitted, wantS2: core.AttemptExpired,
		},
		{
			// принятая до finish попытка затем истекает вместе с остальными
			name: "accept-call", sIsS2: true,
			write:  `UPDATE attempts SET status = 'in_progress', call_accepted_at = now() WHERE id = $1`,
			wantS1: core.AttemptExpired, wantS2: core.AttemptExpired,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			e := newEnv(t, f.pool, nil)
			l := f.runningLesson(t, e, 1)
			a1, a2 := f.attemptOf(t, l.Id, f.s1), f.attemptOf(t, l.Id, f.s2)
			mustExec(t, f.pool, `UPDATE attempts SET status = 'in_progress', call_accepted_at = now() WHERE id = $1`, a1.ID)
			target := a1
			if tc.sIsS2 {
				target = a2
			}
			ctx := context.Background()

			tx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx) //nolint:errcheck // после Commit — no-op
			mustExec(t, tx, `SET LOCAL lock_timeout = '20s'`)
			// 1) занятие FOR SHARE (как attempts.sqlLockLessonOfAttempt)
			mustExec(t, tx, `SELECT l.status FROM lessons l
				 WHERE l.id = (SELECT a.lesson_id FROM attempts a WHERE a.id = $1) FOR SHARE`, target.ID)

			// 2) преподаватель жмёт «Завершить занятие»
			type result struct {
				r   resp
				err error
			}
			finished := make(chan result, 1)
			go func() {
				r, err := e.send("POST", "/lessons/"+l.Id.String()+"/finish", f.teacherW(), nil)
				finished <- result{r, err}
			}()
			waitLockWaiters(t, f.pool, 1)

			// 3) попытка FOR NO KEY UPDATE — при встречном порядке здесь deadlock
			if _, err := tx.Exec(ctx, `SELECT status FROM attempts WHERE id = $1 FOR NO KEY UPDATE`, target.ID); err != nil {
				t.Fatalf("замок попытки (как в attempts): %v", err)
			}
			if _, err := tx.Exec(ctx, tc.write, target.ID); err != nil {
				t.Fatalf("запись попытки: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit: %v", err)
			}

			res := <-finished
			if res.err != nil {
				t.Fatal(res.err)
			}
			if out := decode[public.Lesson](t, res.r, 200); out.Status != core.LessonFinished {
				t.Fatalf("finish: %+v", out)
			}
			if got := f.attemptOf(t, l.Id, f.s1); got.Status != tc.wantS1 {
				t.Errorf("попытка s1: %s, want %s", got.Status, tc.wantS1)
			}
			if got := f.attemptOf(t, l.Id, f.s2); got.Status != tc.wantS2 {
				t.Errorf("попытка s2: %s, want %s", got.Status, tc.wantS2)
			}
		})
	}
}
