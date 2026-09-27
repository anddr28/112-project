package attempts

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/reaction"
)

// ================================================================ GET /attempts/{id}

func TestDB_GetAttempt_Access(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{})
	id := f.newAttempt(t, e.pool, f.student)

	for _, c := range []struct {
		name   string
		p      *core.Principal
		status int
	}{
		{"студент-владелец", f.asStudent(), 200},
		{"другой студент", f.asStudent2(), 403},
		{"преподаватель занятия", f.asTeacher(), 200},
		{"чужой преподаватель", f.asOther(), 403},
		{"админ", f.asAdmin(), 200},
	} {
		r := e.do(c.p, http.MethodGet, path(id, ""), nil)
		if r.code != c.status {
			t.Errorf("%s: %d %s", c.name, r.code, r.body)
		}
	}
	expect(t, e.do(f.asStudent(), http.MethodGet, path(uuid.New(), ""), nil), 404, "not_found")

	r := e.do(f.asStudent(), http.MethodGet, path(id, ""), nil)
	m := r.obj(t)
	for _, k := range []string{"id", "lessonId", "lessonTitle", "userId", "scenarioId", "mode", "seqNo", "status",
		"timeLimitSec", "issuedAt", "replayCount", "serverNow", "incidentNo", "voice", "perspective"} {
		if _, ok := m[k]; !ok {
			t.Errorf("Attempt без обязательного %q: %s", k, r.body)
		}
	}
	if m["status"] != "issued" || m["callAcceptedAt"] != nil {
		t.Fatalf("status: %s", r.body)
	}
}

// ================================================================ accept-call

func TestDB_AcceptCall(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{})
	id := f.newAttempt(t, e.pool, f.student)

	expect(t, e.do(f.asStudent2(), http.MethodPost, path(id, "/accept-call"), nil), 403, "forbidden")
	expect(t, e.do(f.asStudent(), http.MethodPost, path(uuid.New(), "/accept-call"), nil), 404, "not_found")

	r := e.do(f.asStudent(), http.MethodPost, path(id, "/accept-call"), nil)
	expect(t, r, 200, "")
	var out struct {
		Attempt public.Attempt           `json:"attempt"`
		Opening *public.DialogueTurnView `json:"opening"`
	}
	r.json(t, &out)
	if out.Attempt.Status != public.AttemptStatusInProgress || out.Attempt.CallAcceptedAt == nil || out.Opening != nil {
		t.Fatalf("accept: %s", r.body)
	}
	// Инструкция п.4.1: АОН в черновике
	if d := e.getDraft(f, id); d.Phones.Aon == nil || *d.Phones.Aon != "+79991234567" {
		t.Fatalf("АОН: %+v", d.Phones)
	}
	if got := strings.Join(e.eventTypes(id), ","); got != "call_accepted,open_card" {
		t.Fatalf("события: %s", got)
	}
	var pst string
	var joined *time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT status, joined_at FROM lesson_participants WHERE lesson_id=$1 AND user_id=$2`,
		f.lesson, f.student).Scan(&pst, &joined); err != nil || pst != "active" || joined == nil {
		t.Fatalf("участник: %s %v %v", pst, joined, err)
	}
	if e.pub.count(public.MonitorMessageTypeAttemptEvent, "call_accepted") != 1 ||
		e.pub.count(public.MonitorMessageTypeParticipantStatus, "") != 1 {
		t.Fatalf("мониторинг: %+v", e.pub.monitor)
	}
	if a := e.aud.actions(); len(a) != 1 || a[0] != "attempt.accept" {
		t.Fatalf("аудит: %v", a)
	}

	// Повтор — та же попытка, ничего нового
	r2 := e.do(f.asStudent(), http.MethodPost, path(id, "/accept-call"), nil)
	expect(t, r2, 200, "")
	var out2 struct {
		Attempt public.Attempt `json:"attempt"`
	}
	r2.json(t, &out2)
	if !out2.Attempt.CallAcceptedAt.Equal(*out.Attempt.CallAcceptedAt) || len(e.eventTypes(id)) != 2 || len(e.aud.actions()) != 1 {
		t.Fatal("accept-call не идемпотентен")
	}

	// Закрытая попытка — 409
	e.setStatus(id, core.AttemptExpired)
	expect(t, e.do(f.asStudent(), http.MethodPost, path(id, "/accept-call"), nil), 409, "conflict")
}

func TestDB_AcceptCall_LessonNotRunning(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{status: "finished"})
	id := f.newAttempt(t, e.pool, f.student)
	expect(t, e.do(f.asStudent(), http.MethodPost, path(id, "/accept-call"), nil), 409, "conflict")
	if a := e.attemptRow(id); a.status != "issued" || a.callAcceptedAt != nil {
		t.Fatalf("попытка изменилась: %+v", a)
	}
}

func TestDB_AcceptCall_VoiceOpening(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{voice: true})
	e.dlg.opening = &public.DialogueTurnView{TurnNo: 0, Speaker: public.DialogueTurnViewSpeakerCaller, Text: "Алло! Горит!"}
	id := f.newAttempt(t, e.pool, f.student)
	r := e.do(f.asStudent(), http.MethodPost, path(id, "/accept-call"), nil)
	expect(t, r, 200, "")
	var out struct {
		Opening *public.DialogueTurnView `json:"opening"`
	}
	r.json(t, &out)
	if out.Opening == nil || out.Opening.Text != "Алло! Горит!" {
		t.Fatalf("opening: %s", r.body)
	}
	if e.pub.count(public.MonitorMessageTypeDialogueTurn, "") != 1 {
		t.Fatal("вступление — в живой транскрипт преподавателя")
	}
	// повтор — та же реплика, без повторной публикации
	r = e.do(f.asStudent(), http.MethodPost, path(id, "/accept-call"), nil)
	r.json(t, &out)
	if out.Opening == nil || e.pub.count(public.MonitorMessageTypeDialogueTurn, "") != 1 {
		t.Fatal("повтор accept-call")
	}
}

// ================================================================ call-script

// Регрессия (review, security): до приёма вызова легенда не отдаётся — иначе студент читает
// её с экрана входящего вызова, заполняет карточку и сдаёт через секунду после приёма.
func TestDB_CallScript_HiddenBeforeAccept(t *testing.T) {
	t.Parallel()
	for _, voice := range []bool{false, true} {
		e := newEnv(t)
		f := seed(t, e.pool, lessonOpts{voice: voice})
		id := f.newAttempt(t, e.pool, f.student)

		r := e.do(f.asStudent(), http.MethodGet, path(id, "/call-script"), nil)
		expect(t, r, 200, "")
		var cs public.StudentCallScript
		r.json(t, &cs)
		if len(cs.Turns) != 0 || !bytes.Contains(r.body, []byte(`"turns":[]`)) {
			t.Fatalf("voice=%v: реплики до приёма вызова: %s", voice, r.body)
		}
		if cs.Caller.Phone == nil || *cs.Caller.Phone != "+79991234567" {
			t.Fatalf("экрану входящего нужен номер: %s", r.body)
		}
		if bytes.Contains(r.body, []byte("Тверской")) {
			t.Fatalf("адрес из легенды до приёма: %s", r.body)
		}

		expect(t, e.do(f.asStudent(), http.MethodPost, path(id, "/accept-call"), nil), 200, "")
		r = e.do(f.asStudent(), http.MethodGet, path(id, "/call-script"), nil)
		r.json(t, &cs)
		want := 3
		if voice {
			want = 1
		}
		if len(cs.Turns) != want {
			t.Fatalf("voice=%v: после приёма %d реплик: %s", voice, len(cs.Turns), r.body)
		}
	}
}

func TestDB_CallScript(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	no := false
	f := seed(t, e.pool, lessonOpts{allowReplay: &no})
	id := e.accepted(f, f.student)

	r := e.do(f.asStudent(), http.MethodGet, path(id, "/call-script"), nil)
	expect(t, r, 200, "")
	var cs public.StudentCallScript
	r.json(t, &cs)
	if cs.AllowReplay || cs.Voice.Enabled || len(cs.Turns) != 3 {
		t.Fatalf("легенда: %s", r.body)
	}
	if cs.Turns[0].AudioUrl == nil || !strings.HasSuffix(*cs.Turns[0].AudioUrl, "ab/h-open.ogg") || *cs.Turns[0].DurationMs != 3100 {
		t.Fatalf("озвучка из tts_cache: %+v", cs.Turns[0])
	}
	if cs.Turns[2].AudioUrl != nil || cs.Turns[2].Index != 3 {
		t.Fatalf("неозвученная реплика: %+v", cs.Turns[2])
	}
	for _, leak := range []string{"СЕКРЕТНЫЙ", "keyFacts", "key_facts", "persona", "emotional", "baya"} {
		if bytes.Contains(r.body, []byte(leak)) {
			t.Errorf("утечка %q: %s", leak, r.body)
		}
	}
	expect(t, e.do(f.asTeacher(), http.MethodGet, path(id, "/call-script"), nil), 200, "")
	expect(t, e.do(f.asStudent2(), http.MethodGet, path(id, "/call-script"), nil), 403, "forbidden")
	expect(t, e.do(f.asOther(), http.MethodGet, path(id, "/call-script"), nil), 403, "forbidden")
	expect(t, e.do(f.asStudent(), http.MethodGet, path(uuid.New(), "/call-script"), nil), 404, "not_found")
}

// ================================================================ черновик

func TestDB_Draft_GetAccess(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{})
	id := f.newAttempt(t, e.pool, f.student)

	r := e.do(f.asStudent(), http.MethodGet, path(id, "/draft"), nil)
	expect(t, r, 200, "")
	m := r.obj(t)
	for _, k := range []string{"phones", "applicant", "address", "incidentTypeIds", "attributes", "description", "actionsTaken", "flags", "services"} {
		if _, ok := m[k]; !ok {
			t.Errorf("нет %q: %s", k, r.body)
		}
	}
	for _, k := range []string{"services", "incidentTypeIds"} {
		if arr, ok := m[k].([]any); !ok || arr == nil {
			t.Fatalf("%s — [] (не null): %s", k, r.body)
		}
	}
	expect(t, e.do(f.asTeacher(), http.MethodGet, path(id, "/draft"), nil), 200, "")
	expect(t, e.do(f.asAdmin(), http.MethodGet, path(id, "/draft"), nil), 200, "")
	expect(t, e.do(f.asStudent2(), http.MethodGet, path(id, "/draft"), nil), 403, "forbidden")
	expect(t, e.do(f.asOther(), http.MethodGet, path(id, "/draft"), nil), 403, "forbidden")
	expect(t, e.do(f.asStudent(), http.MethodGet, path(uuid.New(), "/draft"), nil), 404, "not_found")

	// строки черновика нет или в ней '{}' — пустая карточка
	mustExec(t, e.pool, `UPDATE attempt_drafts SET data = '{}' WHERE attempt_id = $1`, id)
	if d := e.getDraft(f, id); d.Services == nil || d.IncidentTypeIds == nil {
		t.Fatal("'{}' — пустая карточка")
	}
	mustExec(t, e.pool, `DELETE FROM attempt_drafts WHERE attempt_id = $1`, id)
	r = e.do(f.asStudent(), http.MethodGet, path(id, "/draft"), nil)
	if !bytes.Equal(bytes.TrimSpace(r.body), bytes.TrimSpace(emptyDraftJSON)) {
		t.Fatalf("нет строки — пустая карточка: %s", r.body)
	}
}

func TestDB_Draft_Put(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{})
	id := e.accepted(f, f.student)

	card := e.getDraft(f, id)
	card.Description = "Дым из окна пятого этажа"
	card.Attributes = map[string]any{"where": "квартира", "floorCount": 9.0}
	r := e.do(f.asStudent(), http.MethodPut, path(id, "/draft"), card)
	expect(t, r, 200, "")
	var saved struct {
		SavedAt time.Time `json:"savedAt"`
	}
	r.json(t, &saved)
	if saved.SavedAt.IsZero() || saved.SavedAt.Location() != time.UTC {
		t.Fatalf("savedAt: %s", r.body)
	}
	got := e.getDraft(f, id)
	if got.Description != card.Description || got.Attributes["where"] != "квартира" || got.Phones.Aon == nil {
		t.Fatalf("черновик: %+v", got)
	}

	expect(t, e.do(f.asStudent2(), http.MethodPut, path(id, "/draft"), card), 403, "forbidden")
	expect(t, e.do(f.asStudent(), http.MethodPut, path(uuid.New(), "/draft"), card), 404, "not_found")

	// после сдачи — 409 (и из кэша метаданных, и мимо него)
	expect(t, e.do(f.asStudent(), http.MethodPost, path(id, "/submit"), map[string]any{"card": card}), 200, "")
	expect(t, e.do(f.asStudent(), http.MethodPut, path(id, "/draft"), card), 409, "conflict")
	e.svc.meta = newMetaCache(metaMaxEntries)
	expect(t, e.do(f.asStudent(), http.MethodPut, path(id, "/draft"), card), 409, "conflict")
}

// Регрессия (review): PUT принимал любой JSON-объект, и GET /draft потом отдавал не карточку.
func TestDB_Draft_PutRejectsForeignShape(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{})
	id := e.accepted(f, f.student)
	before := e.getDraft(f, id)

	for _, body := range []string{`{"foo":1}`, `{"phones":{},"applicant":{},"address":"x","incidentTypeIds":[],"attributes":{},"description":"","actionsTaken":"","flags":{},"services":[]}`} {
		expect(t, e.do(f.asStudent(), http.MethodPut, path(id, "/draft"), body), 400, "validation")
	}
	after := e.getDraft(f, id)
	if after.Phones.Aon == nil || *after.Phones.Aon != *before.Phones.Aon || after.Services == nil {
		t.Fatalf("черновик испорчен: %+v", after)
	}
	r := e.do(f.asStudent(), http.MethodGet, path(id, "/draft"), nil)
	if bytes.Contains(r.body, []byte(`"foo"`)) {
		t.Fatalf("GET отдал чужую форму: %s", r.body)
	}
}

// Регрессия (review, security): до приёма вызова черновик не принимается — таймер не обойти.
func TestDB_Draft_PutBeforeAccept(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{})
	id := f.newAttempt(t, e.pool, f.student)
	card := convert.EmptyDraft()
	card.Description = "Заполнено до приёма вызова"
	r := e.do(f.asStudent(), http.MethodPut, path(id, "/draft"), card)
	expect(t, r, 409, "conflict")
	if _, msg, _ := r.apiError(t); msg != "Сначала примите вызов" {
		t.Fatalf("сообщение: %q", msg)
	}
	if d := e.draft(id); d.Description != "" {
		t.Fatalf("черновик записан до приёма: %q", d.Description)
	}
}

// Регрессия (review): NUL в тексте (вставка из PDF) — автосохранение не ломается, символ вырезается.
func TestDB_Draft_PutStripsNUL(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{})
	id := e.accepted(f, f.student)
	card := e.getDraft(f, id)
	card.Description = "Дым\x00 из окна"
	card.Attributes = map[string]any{"k\x00": "v\x00"}
	expect(t, e.do(f.asStudent(), http.MethodPut, path(id, "/draft"), card), 200, "")
	got := e.getDraft(f, id)
	if got.Description != "Дым из окна" || got.Attributes["k"] != "v" {
		t.Fatalf("NUL: %q %v", got.Description, got.Attributes)
	}
	// одиночный суррогат jsonb не принимает — 400, а не 500
	raw := strings.Replace(string(emptyDraftJSON), `"description":""`, `"description":"\ud800"`, 1)
	expect(t, e.do(f.asStudent(), http.MethodPut, path(id, "/draft"), raw), 400, "validation")
}

// ================================================================ службы и черновик (lost update)

func (e *env) addService(f fixture, id uuid.UUID, code string) public.AssignedService {
	e.t.Helper()
	r := e.do(f.asStudent(), http.MethodPost, path(id, "/services"), map[string]string{"serviceCode": code})
	expect(e.t, r, 200, "")
	var as public.AssignedService
	r.json(e.t, &as)
	return as
}

func (e *env) changeStatus(f fixture, id uuid.UUID, key string, body map[string]string) resp {
	e.t.Helper()
	return e.do(f.asStudent(), http.MethodPost, path(id, "/services/"+key+"/status"), body)
}

// Регрессия (review, integrity): устаревший снимок автосохранения затирал службы, которые
// сервер меняет под замком (добавление вручную, смена статуса).
func TestDB_Draft_StaleAutosaveKeepsServerServices(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{})
	id := e.accepted(f, f.student)

	stale := e.getDraft(f, id) // снимок до действий со службами
	stale.Description = "Печатаю описание"

	e.addService(f, id, "101")
	// устаревший PUT: служб нет
	expect(t, e.do(f.asStudent(), http.MethodPut, path(id, "/draft"), stale), 200, "")
	got := e.getDraft(f, id)
	if strings.Join(codes(got.Services), ",") != "101" || got.Description != "Печатаю описание" {
		t.Fatalf("ручная служба потеряна или текст не сохранён: %v %q", codes(got.Services), got.Description)
	}

	// смена статуса, затем PUT со снимком, где служба ещё «Получена службой»
	stale2 := e.getDraft(f, id)
	expect(t, e.changeStatus(f, id, "svc-101", map[string]string{"status": "Принята"}), 200, "")
	stale2.Description = "Ещё текст"
	expect(t, e.do(f.asStudent(), http.MethodPut, path(id, "/draft"), stale2), 200, "")
	got = e.getDraft(f, id)
	if len(got.Services) != 1 || got.Services[0].CurrentStatus != reaction.StatusAccepted || len(got.Services[0].History) != 3 {
		t.Fatalf("смена статуса потеряна: %+v", got.Services)
	}
	if got.Services[0].Source != "manual" || got.Description != "Ещё текст" {
		t.Fatalf("%+v %q", got.Services[0], got.Description)
	}
}

// Состав автоопределённых служб — клиентский (resolve-services): PUT добавляет и убирает их;
// ручные и приступившие к реагированию клиент не убирает и не подделывает.
func TestDB_Draft_ServicesMergeRules(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{})
	id := e.accepted(f, f.student)
	now := time.Now().UTC().Truncate(time.Second)

	card := e.getDraft(f, id)
	auto101 := reaction.NewAssigned(catalogServices["101"], reaction.SourceAuto, true, "пожар", now)
	auto102 := reaction.NewAssigned(catalogServices["102"], reaction.SourceAuto, false, "", now)
	card.Services = []public.AssignedService{auto101, auto102}
	expect(t, e.do(f.asStudent(), http.MethodPut, path(id, "/draft"), card), 200, "")
	if got := codes(e.getDraft(f, id).Services); strings.Join(got, ",") != "101,102" {
		t.Fatalf("автоопределённые службы: %v", got)
	}

	// 101 приступила к реагированию (сервер), 103 добавлена вручную
	expect(t, e.changeStatus(f, id, "101", map[string]string{"status": "Принята"}), 200, "")
	e.addService(f, id, "103")

	// клиент (автоопределение) оставил только 102, выдав её за ручную: источник — серверный,
	// закреплённые 101 (в реагировании) и 103 (ручная) остаются в конце списка
	forged := reaction.NewAssigned(catalogServices["102"], reaction.SourceManual, false, "", now)
	card.Services = []public.AssignedService{forged}
	expect(t, e.do(f.asStudent(), http.MethodPut, path(id, "/draft"), card), 200, "")
	got := e.getDraft(f, id)
	if strings.Join(codes(got.Services), ",") != "102,101,103" || got.Services[0].Source != "auto" {
		t.Fatalf("закреплённые службы остаются, источник серверный: %v %+v", codes(got.Services), got.Services[0])
	}
	if got.Services[1].CurrentStatus != reaction.StatusAccepted || !got.Services[1].IsPrimary {
		t.Fatalf("101: %+v", got.Services[1])
	}

	// автоопределение убрало всё: автоматическая 102 уходит, закреплённые — нет
	card.Services = []public.AssignedService{}
	expect(t, e.do(f.asStudent(), http.MethodPut, path(id, "/draft"), card), 200, "")
	if got := codes(e.getDraft(f, id).Services); strings.Join(got, ",") != "101,103" {
		t.Fatalf("после снятия автоопределением: %v", got)
	}
	// ручную службу, которой нет на сервере (подделка / снята DELETE), клиент не добавит;
	// как и службу «в реагировании», которой сервер не видел
	advanced := reaction.NewAssigned(catalogServices["102"], reaction.SourceAuto, false, "", now)
	advanced.CurrentStatus = reaction.StatusArrived
	for _, bad := range []public.AssignedService{forged, advanced} {
		card.Services = []public.AssignedService{bad}
		expect(t, e.do(f.asStudent(), http.MethodPut, path(id, "/draft"), card), 200, "")
		if got := codes(e.getDraft(f, id).Services); strings.Join(got, ",") != "101,103" {
			t.Fatalf("подделка %s/%s принята: %v", bad.Source, bad.CurrentStatus, got)
		}
	}
	got = e.getDraft(f, id)

	// isPrimary/reason — от клиента, статус — от сервера; дубликаты по коду отбрасываются
	c101 := got.Services[0]
	c101.IsPrimary = false
	c101.CurrentStatus = reaction.StatusDone
	reason := "уточнено"
	c101.Reason = &reason
	card.Services = []public.AssignedService{got.Services[1], c101, c101}
	expect(t, e.do(f.asStudent(), http.MethodPut, path(id, "/draft"), card), 200, "")
	got = e.getDraft(f, id)
	if strings.Join(codes(got.Services), ",") != "103,101" {
		t.Fatalf("порядок тела, без дублей: %v", codes(got.Services))
	}
	if s := got.Services[1]; s.CurrentStatus != reaction.StatusAccepted || s.IsPrimary || s.Reason == nil || *s.Reason != "уточнено" {
		t.Fatalf("слияние полей 101: %+v", s)
	}
}

// Регрессия (review, business/timing): действия со службами до приёма вызова отвергаются, и
// first_input_at не встаёт раньше call_accepted_at (время реакции 0 навсегда).
func TestDB_Services_BeforeAccept(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{})
	id := f.newAttempt(t, e.pool, f.student)

	r := e.do(f.asStudent(), http.MethodPost, path(id, "/services"), map[string]string{"serviceCode": "101"})
	expect(t, r, 409, "conflict")
	if _, msg, _ := r.apiError(t); msg != "Сначала примите вызов" {
		t.Fatalf("сообщение: %q", msg)
	}
	expect(t, e.do(f.asStudent(), http.MethodDelete, path(id, "/services/101"), nil), 409, "conflict")
	expect(t, e.changeStatus(f, id, "101", map[string]string{"status": "Принята"}), 409, "conflict")
	if a := e.attemptRow(id); a.firstInputAt != nil {
		t.Fatalf("first_input_at до приёма вызова: %v", a.firstInputAt)
	}
	if n := len(e.draft(id).Services); n != 0 {
		t.Fatalf("службы до приёма: %d", n)
	}

	// принял — действие со службой проставляет first_input_at не раньше приёма
	expect(t, e.do(f.asStudent(), http.MethodPost, path(id, "/accept-call"), nil), 200, "")
	e.addService(f, id, "101")
	a := e.attemptRow(id)
	if a.firstInputAt == nil || a.firstInputAt.Before(*a.callAcceptedAt) {
		t.Fatalf("first_input_at=%v call_accepted_at=%v", a.firstInputAt, a.callAcceptedAt)
	}
}

// sqlFirstInputTx сам по себе не ставит время без приёма вызова (защита в глубину).
func TestDB_FirstInputTxGuard(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{})
	id := f.newAttempt(t, e.pool, f.student)
	mustExec(t, e.pool, sqlFirstInputTx, id, time.Now())
	if a := e.attemptRow(id); a.firstInputAt != nil {
		t.Fatalf("first_input_at без call_accepted_at: %v", a.firstInputAt)
	}
}

func TestDB_Services(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{})
	id := e.accepted(f, f.student)

	// по коду из справочника; запасной вариант — id службы
	as := e.addService(f, id, "101")
	if as.Code != "101" || as.ServiceId != "svc-101" || as.Source != "manual" || as.CurrentStatus != reaction.StatusReceived ||
		len(as.History) != 2 || as.History[0].Operator != reaction.OperatorSystem || len(as.AllowedNext) != 2 || !as.Editable {
		t.Fatalf("добавленная служба: %+v", as)
	}
	e.addService(f, id, "svc-103")
	expect(t, e.do(f.asStudent(), http.MethodPost, path(id, "/services"), map[string]string{"serviceCode": "101"}), 409, "conflict")
	expect(t, e.do(f.asStudent2(), http.MethodPost, path(id, "/services"), map[string]string{"serviceCode": "102"}), 403, "forbidden")
	expect(t, e.do(f.asStudent(), http.MethodPost, path(uuid.New(), "/services"), map[string]string{"serviceCode": "102"}), 404, "not_found")

	// переходы: недопустимый — 409, без комментария — 400 с полем, допустимый — 200 и история
	expect(t, e.changeStatus(f, id, "svc-101", map[string]string{"status": "Работы завершены"}), 409, "conflict")
	r := e.changeStatus(f, id, "svc-101", map[string]string{"status": "Не принята"})
	expect(t, r, 400, "validation")
	if _, _, fields := r.apiError(t); fields["comment"] == nil {
		t.Fatalf("поле comment: %s", r.body)
	}
	expect(t, e.changeStatus(f, id, "404", map[string]string{"status": "Принята"}), 404, "not_found")
	r = e.changeStatus(f, id, "svc-101", map[string]string{"status": "Принята", "comment": "  принято  "})
	expect(t, r, 200, "")
	r.json(t, &as)
	if as.CurrentStatus != reaction.StatusAccepted || len(as.History) != 3 || as.History[2].Operator != "оп. 227" ||
		as.History[2].Comment == nil || *as.History[2].Comment != "принято" || as.AllowedNext == nil {
		t.Fatalf("после перехода: %+v", as)
	}
	if !bytes.Contains(r.body, []byte(`"allowedNext":[`)) {
		t.Fatalf("allowedNext — массив: %s", r.body)
	}

	// снять: приступившую — 409, неназначенную — 404, «Получена службой» — 204
	expect(t, e.do(f.asStudent(), http.MethodDelete, path(id, "/services/svc-101"), nil), 409, "conflict")
	expect(t, e.do(f.asStudent(), http.MethodDelete, path(id, "/services/102"), nil), 404, "not_found")
	r = e.do(f.asStudent(), http.MethodDelete, path(id, "/services/103"), nil)
	if r.code != http.StatusNoContent {
		t.Fatalf("снятие: %d %s", r.code, r.body)
	}
	if got := codes(e.draft(id).Services); strings.Join(got, ",") != "101" {
		t.Fatalf("службы: %v", got)
	}
	types := strings.Join(e.eventTypes(id), ",")
	if types != "call_accepted,open_card,service_assigned,service_assigned,service_status_changed,service_removed" {
		t.Fatalf("хронология: %s", types)
	}
	if e.pub.count(public.MonitorMessageTypeAttemptEvent, "service_removed") != 1 {
		t.Fatal("мониторинг")
	}

	// закрытая попытка — 409
	e.setStatus(id, core.AttemptEvaluated)
	expect(t, e.do(f.asStudent(), http.MethodPost, path(id, "/services"), map[string]string{"serviceCode": "102"}), 409, "conflict")
}

// Черновика нет (попытка создана в обход IssueNext) — служба всё равно добавляется.
func TestDB_Services_NoDraftRow(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{})
	id := e.accepted(f, f.student)
	mustExec(t, e.pool, `DELETE FROM attempt_drafts WHERE attempt_id = $1`, id)
	e.addService(f, id, "102")
	if got := codes(e.draft(id).Services); strings.Join(got, ",") != "102" {
		t.Fatalf("%v", got)
	}
}

// ================================================================ события

func TestDB_Events(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{})
	id := e.accepted(f, f.student)
	acc := e.attemptRow(id).callAcceptedAt

	early := acc.Add(-10 * time.Second) // часы клиента отстают
	body := map[string]any{"events": []map[string]any{
		{"clientSeq": 1, "type": "save", "at": early},
		{"clientSeq": 2, "type": "field_changed", "at": early, "payload": map[string]any{"field": "description"}},
		{"clientSeq": 3, "type": "mic_check", "at": early, "payload": map[string]any{"ok": true}},
	}}
	r := e.do(f.asStudent(), http.MethodPost, path(id, "/events"), body)
	expect(t, r, 200, "")
	var res eventsAccepted
	r.json(t, &res)
	if res.Accepted != 3 || res.LastSeq != 3 {
		t.Fatalf("%+v", res)
	}
	a := e.attemptRow(id)
	if a.firstInputAt == nil || !a.firstInputAt.Equal(*acc) {
		t.Fatalf("first_input_at не раньше приёма: %v vs %v", a.firstInputAt, acc)
	}
	var mic bool
	if err := e.pool.QueryRow(context.Background(), `SELECT mic_ready FROM lesson_participants WHERE lesson_id=$1 AND user_id=$2`,
		f.lesson, f.student).Scan(&mic); err != nil || !mic {
		t.Fatalf("mic_ready: %v %v", mic, err)
	}

	// повтор пачки — дубли отброшены молча
	r = e.do(f.asStudent(), http.MethodPost, path(id, "/events"), body)
	r.json(t, &res)
	if res.Accepted != 0 || res.LastSeq != 3 {
		t.Fatalf("повтор: %+v", res)
	}

	r = e.do(f.asTeacher(), http.MethodGet, path(id, "/events"), nil)
	expect(t, r, 200, "")
	var list []public.AttemptEvent
	r.json(t, &list)
	if len(list) != 5 { // call_accepted, open_card + 3 клиентских
		t.Fatalf("хронология: %s", r.body)
	}
	expect(t, e.do(f.asStudent2(), http.MethodGet, path(id, "/events"), nil), 403, "forbidden")
	expect(t, e.do(f.asOther(), http.MethodGet, path(id, "/events"), nil), 403, "forbidden")
	expect(t, e.do(f.asStudent2(), http.MethodPost, path(id, "/events"), body), 403, "forbidden")
	expect(t, e.do(f.asStudent(), http.MethodPost, path(uuid.New(), "/events"), body), 404, "not_found")

	// пустая хронология — [] (не null)
	other := f.newAttempt(t, e.pool, f.student2)
	r = e.do(f.asAdmin(), http.MethodGet, path(other, "/events"), nil)
	if strings.TrimSpace(string(r.body)) != "[]" {
		t.Fatalf("пустая хронология: %s", r.body)
	}
}

// Ввод до приёма вызова не проставляет first_input_at (он считается от приёма).
func TestDB_Events_BeforeAccept(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{})
	id := f.newAttempt(t, e.pool, f.student)
	body := map[string]any{"events": []map[string]any{{"clientSeq": 1, "type": "field_changed", "at": time.Now()}}}
	expect(t, e.do(f.asStudent(), http.MethodPost, path(id, "/events"), body), 200, "")
	if a := e.attemptRow(id); a.firstInputAt != nil {
		t.Fatalf("first_input_at: %v", a.firstInputAt)
	}
}

// ================================================================ replay

func TestDB_Replay(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{})
	id := f.newAttempt(t, e.pool, f.student)

	expect(t, e.do(f.asStudent(), http.MethodPost, path(id, "/replay"), nil), 409, "conflict") // не принят
	expect(t, e.do(f.asStudent(), http.MethodPost, path(id, "/accept-call"), nil), 200, "")
	for want := 1; want <= 2; want++ {
		r := e.do(f.asStudent(), http.MethodPost, path(id, "/replay"), nil)
		expect(t, r, 200, "")
		var out replayResponse
		r.json(t, &out)
		if out.ReplayCount != want {
			t.Fatalf("replayCount=%d, want %d", out.ReplayCount, want)
		}
	}
	expect(t, e.do(f.asStudent2(), http.MethodPost, path(id, "/replay"), nil), 403, "forbidden")
	expect(t, e.do(f.asStudent(), http.MethodPost, path(uuid.New(), "/replay"), nil), 404, "not_found")
	if e.pub.count(public.MonitorMessageTypeAttemptEvent, "replay") != 2 {
		t.Fatal("мониторинг replay")
	}
	e.setStatus(id, core.AttemptExpired)
	expect(t, e.do(f.asStudent(), http.MethodPost, path(id, "/replay"), nil), 409, "conflict")
}

func TestDB_Replay_Disallowed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	no := false
	f := seed(t, e.pool, lessonOpts{allowReplay: &no})
	id := e.accepted(f, f.student)
	r := e.do(f.asStudent(), http.MethodPost, path(id, "/replay"), nil)
	expect(t, r, 409, "conflict")
	if a := e.attemptRow(id); a.replayCount != 0 {
		t.Fatal("replay_count изменён")
	}
}

// ================================================================ submit

func TestDB_Submit(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{})
	id := f.newAttempt(t, e.pool, f.student)
	card := convert.EmptyDraft()
	card.Description = "Горит квартира"

	expect(t, e.do(f.asStudent(), http.MethodPost, path(id, "/submit"), map[string]any{"card": card}), 409, "conflict") // не принят
	expect(t, e.do(f.asStudent(), http.MethodPost, path(id, "/accept-call"), nil), 200, "")
	expect(t, e.do(f.asStudent2(), http.MethodPost, path(id, "/submit"), map[string]any{"card": card}), 403, "forbidden")
	expect(t, e.do(f.asStudent(), http.MethodPost, path(uuid.New(), "/submit"), map[string]any{"card": card}), 404, "not_found")

	r := e.do(f.asStudent(), http.MethodPost, path(id, "/submit"), map[string]any{"card": card, "actionText": "  Направил пожарных "})
	expect(t, r, 200, "")
	var att public.Attempt
	r.json(t, &att)
	if att.Status != public.AttemptStatusEvaluating || att.SubmittedAt == nil || att.TimeSpentMs == nil || att.Card == nil {
		t.Fatalf("попытка: %s", r.body)
	}
	if att.Card.Services == nil || att.Card.Description != "Горит квартира" {
		t.Fatalf("карточка в ответе: %+v", att.Card)
	}
	a := e.attemptRow(id)
	if a.actionText == nil || *a.actionText != "Направил пожарных" || a.timeSpentMs == nil || *a.timeSpentMs < 0 {
		t.Fatalf("строка: %+v", a)
	}
	if d := e.draft(id); d.Description != "Горит квартира" {
		t.Fatal("черновик = карточка")
	}
	types := strings.Join(e.eventTypes(id), ",")
	if !strings.HasSuffix(types, "save,submitted") {
		t.Fatalf("события: %s", types)
	}
	if e.eval.n() != 1 || e.issuer.called != 1 || len(e.dlg.closed) != 1 {
		t.Fatalf("порты: eval=%d issue=%d close=%d", e.eval.n(), e.issuer.called, len(e.dlg.closed))
	}
	var pst string
	var fin *time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT status, finished_at FROM lesson_participants WHERE lesson_id=$1 AND user_id=$2`,
		f.lesson, f.student).Scan(&pst, &fin); err != nil || pst != "finished" || fin == nil {
		t.Fatalf("участник закончил: %s %v", pst, err)
	}
	if acts := strings.Join(e.aud.actions(), ","); acts != "attempt.accept,attempt.submit" {
		t.Fatalf("аудит: %s", acts)
	}

	// повтор — 200 та же попытка, без повторной оценки
	r = e.do(f.asStudent(), http.MethodPost, path(id, "/submit"), map[string]any{"card": card})
	expect(t, r, 200, "")
	if e.eval.n() != 1 || len(e.eventTypes(id)) != len(strings.Split(types, ",")) {
		t.Fatal("submit не идемпотентен")
	}

	// истекшая попытка — 409
	other := e.accepted(f, f.student)
	e.setStatus(other, core.AttemptExpired)
	expect(t, e.do(f.asStudent(), http.MethodPost, path(other, "/submit"), map[string]any{"card": card}), 409, "conflict")
}

// Регрессия (review, integrity): сдача устаревшей карточки (ответ на «добавить службу» ещё не
// перечитан) теряла ручную службу и статус — сдаётся слияние с черновиком.
func TestDB_Submit_StaleCardKeepsServerServices(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{})
	id := e.accepted(f, f.student)
	stale := e.getDraft(f, id)
	now := time.Now().UTC()
	stale.Services = []public.AssignedService{reaction.NewAssigned(catalogServices["102"], reaction.SourceAuto, true, "", now)}

	e.addService(f, id, "101")
	expect(t, e.changeStatus(f, id, "101", map[string]string{"status": "Принята"}), 200, "")

	r := e.do(f.asStudent(), http.MethodPost, path(id, "/submit"), map[string]any{"card": stale})
	expect(t, r, 200, "")
	var att public.Attempt
	r.json(t, &att)
	if got := strings.Join(codes(att.Card.Services), ","); got != "102,101" {
		t.Fatalf("службы сданной карточки: %s", got)
	}
	if s := att.Card.Services[1]; s.CurrentStatus != reaction.StatusAccepted || s.Source != "manual" || s.AllowedNext == nil {
		t.Fatalf("101: %+v", s)
	}
	var stored public.IncidentCardDraft
	if err := json.Unmarshal(e.attemptRow(id).card, &stored); err != nil || strings.Join(codes(stored.Services), ",") != "102,101" {
		t.Fatalf("attempts.card: %v %v", codes(stored.Services), err)
	}
}

// Регрессия (review): NUL в карточке или в тексте действий давал 500 при сдаче.
func TestDB_Submit_StripsNUL(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{})
	id := e.accepted(f, f.student)
	card := convert.EmptyDraft()
	card.Description = "Дым\x00 из окна"
	name := "Мар\x00ия"
	card.Applicant.Name = &name
	card.ActionsTaken = "Направил\x00"
	card.Attributes = map[string]any{"where\x00": "квар\x00тира"}
	svc := reaction.NewAssigned(catalogServices["101"], reaction.SourceAuto, false, "", time.Now())
	svc.Name = "Пожарная\x00"
	card.Services = []public.AssignedService{svc}

	r := e.do(f.asStudent(), http.MethodPost, path(id, "/submit"), map[string]any{"card": card, "actionText": "Отправил\x00 расчёт"})
	expect(t, r, 200, "")
	a := e.attemptRow(id)
	if bytes.Contains(a.card, []byte(`\u0000`)) || a.actionText == nil || *a.actionText != "Отправил расчёт" {
		t.Fatalf("card=%s action=%v", a.card, a.actionText)
	}
	var stored public.IncidentCardDraft
	if err := json.Unmarshal(a.card, &stored); err != nil || stored.Description != "Дым из окна" || *stored.Applicant.Name != "Мария" ||
		stored.Attributes["where"] != "квартира" || len(stored.Services) != 1 || stored.Services[0].Name != "Пожарная" {
		t.Fatalf("карточка: %+v %v", stored, err)
	}
}

// card_actions: текст действий берётся из card.actionsTaken, если actionText не прислан.
func TestDB_Submit_CardActionsFallback(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{})
	id := e.accepted(f, f.student)
	mustExec(t, e.pool, `UPDATE attempts SET mode = 'card_actions' WHERE id = $1`, id)
	card := convert.EmptyDraft()
	card.ActionsTaken = " Направил 01 и 03 "
	expect(t, e.do(f.asStudent(), http.MethodPost, path(id, "/submit"), map[string]any{"card": card}), 200, "")
	if a := e.attemptRow(id); a.actionText == nil || *a.actionText != "Направил 01 и 03" {
		t.Fatalf("action_text: %v", a.actionText)
	}
}

// Следующая карточка выдана — участник не завершает занятие.
func TestDB_Submit_NextIssued(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{})
	e.issuer.issue, e.issuer.next = true, uuid.New()
	id := e.accepted(f, f.student)
	expect(t, e.do(f.asStudent(), http.MethodPost, path(id, "/submit"), map[string]any{"card": convert.EmptyDraft()}), 200, "")
	var pst string
	if err := e.pool.QueryRow(context.Background(), `SELECT status FROM lesson_participants WHERE lesson_id=$1 AND user_id=$2`,
		f.lesson, f.student).Scan(&pst); err != nil || pst != "active" {
		t.Fatalf("участник: %s %v", pst, err)
	}
	e.aud.mu.Lock()
	after := e.aud.entries[len(e.aud.entries)-1].After.(map[string]any)
	e.aud.mu.Unlock()
	if after["nextAttemptId"] != e.issuer.next {
		t.Fatalf("аудит nextAttemptId: %v", after)
	}
}

// ================================================================ servicesMergeSQL на «грязных» данных

// Слияние не падает на любой форме jsonb (черновики, сохранённые до проверки формы).
func TestDB_ServicesMerge_Garbage(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	q := `SELECT ` + servicesMergeSQL("$1::jsonb", "$2::jsonb")
	cases := []struct {
		stored, body, want string
	}{
		{`null`, `null`, `[]`},
		{`"x"`, `{"a":1}`, `[]`},
		{`[1, "s", null, {"code": 5}]`, `[{"code":"101","source":"auto","currentStatus":"Получена службой"}]`, `101`},
		{`[{"code":"101","source":"manual","history":"x"}]`, `[]`, `101`},
		{`[{"code":"101","source":"auto","currentStatus":"Добавлена"}]`, `[{"code":"102"}]`, `102`},
		{`[{"code":"ABC","source":"manual","currentStatus":"Принята"}]`, `[{"code":"abc","isPrimary":true}]`, `abc`},
	}
	for _, c := range cases {
		var out []byte
		if err := e.pool.QueryRow(context.Background(), q, c.stored, c.body).Scan(&out); err != nil {
			t.Fatalf("stored=%s body=%s: %v", c.stored, c.body, err)
		}
		var list []map[string]any
		if err := json.Unmarshal(out, &list); err != nil {
			t.Fatalf("%s", out)
		}
		var got []string
		for _, s := range list {
			if c, ok := s["code"].(string); ok {
				got = append(got, c)
			}
		}
		if strings.Join(got, ",") != c.want && !(c.want == "[]" && len(list) == 0) {
			t.Errorf("stored=%s body=%s: %s", c.stored, c.body, out)
		}
	}
}

// Регрессия: попытку практики приняли и сдали через месяц — time_spent_ms (int4) не переполняется.
func TestDB_Submit_LongSpentClamped(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{})
	id := e.accepted(f, f.student)
	mustExec(t, e.pool, `UPDATE attempts SET call_accepted_at = now() - interval '30 days' WHERE id = $1`, id)
	expect(t, e.do(f.asStudent(), http.MethodPost, path(id, "/submit"), map[string]any{"card": convert.EmptyDraft()}), 200, "")
	if a := e.attemptRow(id); a.timeSpentMs == nil || *a.timeSpentMs != 2147483647 {
		t.Fatalf("time_spent_ms = %v", a.timeSpentMs)
	}
}
