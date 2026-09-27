package lessons

import (
	"encoding/json"
	"lct/gocore/internal/store"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
)

func TestPickScenario(t *testing.T) {
	t.Parallel()
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	ea, eb, ec := uuid.New(), uuid.New(), uuid.New()
	pool := []uuid.UUID{a, b, c}
	cases := []struct {
		name          string
		pool, etalons []uuid.UUID
		idx, issued   int
		prev          *uuid.UUID
		want          int
		ok            bool
	}{
		{name: "пустой пул", pool: nil, etalons: nil, want: -1},
		{name: "длины не совпадают", pool: pool, etalons: []uuid.UUID{ea}, want: -1},
		{name: "первый участник, первая карточка", pool: pool, etalons: []uuid.UUID{ea, eb, ec}, want: 0, ok: true},
		{name: "второй участник — соседний сценарий", pool: pool, etalons: []uuid.UUID{ea, eb, ec}, idx: 1, want: 1, ok: true},
		{name: "по кругу", pool: pool, etalons: []uuid.UUID{ea, eb, ec}, idx: 2, issued: 2, want: 1, ok: true},
		{name: "большие индексы не переполняют", pool: pool, etalons: []uuid.UUID{ea, eb, ec}, idx: 1<<31 - 1, issued: 1<<31 - 1, want: 2, ok: true},
		{name: "без эталона — пропуск", pool: pool, etalons: []uuid.UUID{uuid.Nil, eb, ec}, want: 1, ok: true},
		{name: "подряд тот же не выдаём", pool: pool, etalons: []uuid.UUID{ea, eb, ec}, prev: &a, want: 1, ok: true},
		{name: "повтор, если другого нет", pool: pool, etalons: []uuid.UUID{ea, uuid.Nil, uuid.Nil}, prev: &a, want: 0, ok: true},
		{name: "пул из одного — повтор допустим", pool: []uuid.UUID{a}, etalons: []uuid.UUID{ea}, issued: 1, prev: &a, want: 0, ok: true},
		{name: "ни у кого нет эталона", pool: pool, etalons: []uuid.UUID{uuid.Nil, uuid.Nil, uuid.Nil}, want: -1},
	}
	for _, tc := range cases {
		got, ok := pickScenario(tc.pool, tc.etalons, nil, tc.idx, tc.issued, tc.prev)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: got (%d, %v), want (%d, %v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPickScenario_NeighboursDiffer(t *testing.T) {
	t.Parallel()
	pool := []uuid.UUID{uuid.New(), uuid.New()}
	et := []uuid.UUID{uuid.New(), uuid.New()}
	first, _ := pickScenario(pool, et, nil, 0, 0, nil)
	second, _ := pickScenario(pool, et, nil, 1, 0, nil)
	if first == second {
		t.Fatal("соседние обучающиеся получили одинаковый сценарий")
	}
	// Вторая карточка первого участника — другой сценарий, чем первая.
	next, _ := pickScenario(pool, et, nil, 0, 1, &pool[first])
	if next == first {
		t.Fatal("вторая карточка повторяет первую")
	}
}

func TestOnlyParticipant(t *testing.T) {
	t.Parallel()
	me, other := uuid.New(), uuid.New()
	l := public.Lesson{Participants: []public.LessonParticipant{{UserId: other, Name: "Петров П. П."}, {UserId: me, Name: "Иванов И. И."}}}
	onlyParticipant(&l, me)
	if len(l.Participants) != 1 || l.Participants[0].UserId != me {
		t.Fatalf("participants %+v", l.Participants)
	}
	l = public.Lesson{Participants: []public.LessonParticipant{{UserId: other}}}
	onlyParticipant(&l, me)
	raw, _ := json.Marshal(l.Participants)
	if string(raw) != "[]" {
		t.Fatalf("не участник: %s, want [] (не null)", raw)
	}
	l = public.Lesson{}
	onlyParticipant(&l, me)
	if l.Participants == nil {
		t.Fatal("nil participants")
	}
}

func TestAssignedRank(t *testing.T) {
	t.Parallel()
	order := []string{core.LessonRunning, core.LessonScheduled, core.LessonDraft, core.LessonFinished, "unknown"}
	for i := 1; i < len(order); i++ {
		if store.AssignedRank(order[i-1]) >= store.AssignedRank(order[i]) {
			t.Errorf("%s должен быть выше %s", order[i-1], order[i])
		}
	}
}

func TestEmptyDraftJSON(t *testing.T) {
	t.Parallel()
	var m map[string]any
	if err := json.Unmarshal(emptyDraftJSON, &m); err != nil || len(m) == 0 {
		t.Fatalf("emptyDraftJSON %s: %v", emptyDraftJSON, err)
	}
}

func TestPublishNilPublisher(t *testing.T) {
	t.Parallel()
	// CLI-сид без веб-сервера: публикации и аудит молча пропускаются.
	s := New(Deps{})
	l := public.Lesson{Id: uuid.New(), Participants: []public.LessonParticipant{{UserId: uuid.New()}}}
	s.publishLesson(&l)
	s.publishAttemptEvent(l.Id, uuid.New(), public.AttemptEvent{})
	s.logAudit(t.Context(), core.AuditEntry{Action: "x"})
	if s.log == nil {
		t.Fatal("логгер по умолчанию")
	}
	if snap := s.snapshot(t.Context()); snap.TimeLimitSec != 30 {
		t.Fatalf("snapshot без Settings — дефолты миграций: %+v", snap)
	}
}

func TestPublishLesson(t *testing.T) {
	t.Parallel()
	pub := &pubRecorder{}
	s := New(Deps{Publisher: pub})
	a1 := uuid.New()
	l := public.Lesson{Id: uuid.New(), Status: public.LessonStatus(core.LessonRunning), Participants: []public.LessonParticipant{
		{UserId: uuid.New(), AttemptId: &a1}, {UserId: uuid.New()},
	}}
	s.publishLesson(&l)
	msgs := pub.monitorMsgs()
	if len(msgs) != 3 || msgs[0].Msg.Type != public.MonitorMessageTypeLessonStatus || *msgs[0].Msg.LessonStatus != l.Status {
		t.Fatalf("msgs %+v", msgs)
	}
	if msgs[1].Msg.Type != public.MonitorMessageTypeParticipantStatus || msgs[1].Msg.AttemptId == nil || *msgs[1].Msg.AttemptId != a1 {
		t.Fatalf("participantStatus %+v", msgs[1].Msg)
	}
	if msgs[2].Msg.AttemptId != nil {
		t.Fatalf("участник без попытки: attemptId %v", *msgs[2].Msg.AttemptId)
	}
}

// ---------------------------------------------------------------- HTTP без БД: RBAC и вход

// Эти проверки срабатывают до первого запроса к БД — пул не нужен.
func TestHandlers_AuthAndRBAC(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil, nil)
	id := uuid.NewString()
	cases := []struct {
		method, path string
		as           who
		status       int
		code         string
	}{
		// нет сессии
		{"GET", "/lessons", who{}, 401, httpx.CodeUnauthorized},
		{"GET", "/lessons/default-settings", who{}, 401, httpx.CodeUnauthorized},
		{"POST", "/lessons", who{}, 401, httpx.CodeUnauthorized},
		{"GET", "/lessons/assigned", who{}, 401, httpx.CodeUnauthorized},
		{"GET", "/lessons/" + id, who{}, 401, httpx.CodeUnauthorized},
		{"POST", "/lessons/" + id + "/start", who{}, 401, httpx.CodeUnauthorized},
		{"POST", "/lessons/" + id + "/finish", who{}, 401, httpx.CodeUnauthorized},
		{"GET", "/lessons/" + id + "/attempts", who{}, 401, httpx.CodeUnauthorized},
		// заблокирован
		{"GET", "/lessons", asRole("blocked"), 423, httpx.CodeUserBlocked},
		// роли
		{"GET", "/lessons", asRole(core.RoleStudent), 403, httpx.CodeForbidden},
		{"GET", "/lessons/default-settings", asRole(core.RoleStudent), 403, httpx.CodeForbidden},
		{"POST", "/lessons", asRole(core.RoleStudent), 403, httpx.CodeForbidden},
		{"POST", "/lessons", asRole(core.RoleAdmin), 403, httpx.CodeForbidden},
		{"GET", "/lessons/assigned", asRole(core.RoleTeacher), 403, httpx.CodeForbidden},
		{"GET", "/lessons/assigned", asRole(core.RoleAdmin), 403, httpx.CodeForbidden},
		{"POST", "/lessons/" + id + "/start", asRole(core.RoleStudent), 403, httpx.CodeForbidden},
		{"POST", "/lessons/" + id + "/finish", asRole(core.RoleStudent), 403, httpx.CodeForbidden},
		{"GET", "/lessons/" + id + "/attempts", asRole(core.RoleStudent), 403, httpx.CodeForbidden},
		// кривой id — 404 «Занятие не найдено», а не 400/500
		{"GET", "/lessons/not-a-uuid", asRole(core.RoleTeacher), 404, httpx.CodeNotFound},
		{"POST", "/lessons/123/start", asRole(core.RoleTeacher), 404, httpx.CodeNotFound},
		{"POST", "/lessons/123/finish", asRole(core.RoleAdmin), 404, httpx.CodeNotFound},
		{"GET", "/lessons/123/attempts", asRole(core.RoleTeacher), 404, httpx.CodeNotFound},
		// фильтр списка
		{"GET", "/lessons?status=paused", asRole(core.RoleTeacher), 400, httpx.CodeValidation},
	}
	for _, tc := range cases {
		var body any
		if tc.method != http.MethodGet {
			body = map[string]any{}
		}
		expectError(t, e.do(t, tc.method, tc.path, tc.as, body), tc.status, tc.code)
	}
}

func TestHandlers_CSRF(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil, nil)
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/lessons", nil)
	req.Header.Set("X-Test-Role", string(core.RoleTeacher))
	res, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("без X-Requested-With: %d", res.StatusCode)
	}
}

func TestHandlers_CreateValidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil, nil)
	teacher := asRole(core.RoleTeacher)

	r := e.do(t, "POST", "/lessons", teacher, "{не json")
	expectError(t, r, 400, httpx.CodeValidation)

	r = e.do(t, "POST", "/lessons", teacher, map[string]any{
		"title": "Пж", "mode": "cards", "timeLimitSec": 5,
		"scenarioIds": []string{"x"}, "participantIds": []string{},
		"weights": map[string]any{"fields": -1},
		"voice":   map[string]any{"input": "мысленно"},
	})
	er := expectError(t, r, 400, httpx.CodeValidation)
	for _, f := range []string{"title", "timeLimitSec", "scenarioIds[0]", "participantIds", "weights.fields", "voice.input"} {
		if er.field(f) == "" {
			t.Errorf("details.fields.%s нет: %v", f, er.Details)
		}
	}
}

func TestHandlers_DefaultSettings(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil, nil)
	for _, role := range []core.Role{core.RoleTeacher, core.RoleAdmin} {
		r := e.do(t, "GET", "/lessons/default-settings", asRole(role), nil)
		obj := rawKeys(t, r.body)
		for _, k := range []string{"passThreshold", "weights", "cardsPerStudent", "allowReplay", "voice"} {
			if _, ok := obj[k]; !ok {
				t.Errorf("нет %q: %s", k, r.body)
			}
		}
		ls := decode[public.LessonSettings](t, r, 200)
		w := ls.Weights
		sum := w.Fields + w.Semantic + w.Grammar + w.Timing + w.Dialogue
		if sum < 0.999 || sum > 1.001 || ls.PassThreshold != 70 || ls.CardsPerStudent < 1 {
			t.Errorf("default-settings %+v", ls)
		}
		var raw struct {
			Weights map[string]float64 `json:"weights"`
		}
		_ = json.Unmarshal(r.body, &raw)
		for _, k := range []string{"fields", "semantic", "grammar", "timing", "dialogue"} {
			if _, ok := raw.Weights[k]; !ok {
				t.Errorf("weights.%s отсутствует", k)
			}
		}
	}
}

func TestMonitor_NilPrincipal(t *testing.T) {
	t.Parallel()
	s := New(Deps{})
	if err := s.CanMonitor(t.Context(), nil, uuid.New()); httpx.AsError(err).Status != 401 {
		t.Fatalf("CanMonitor(nil) = %v", err)
	}
	if _, err := s.AttemptAccess(t.Context(), nil, uuid.New()); httpx.AsError(err).Status != 401 {
		t.Fatalf("AttemptAccess(nil) = %v", err)
	}
}
