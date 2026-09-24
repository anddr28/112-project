package scenarios

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/classifier"
	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/callbacks"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/platform/pgtest"
	"lct/gocore/internal/settings"
	"lct/gocore/internal/store"
)

// DB-тесты: каждый тест — своя БД с миграциями (pgtest), справочники классификатора из
// classifier.SeedReference, пользователи — SQL. Порты — фейки из helpers_test.go.
// Без PostgreSQL тесты пропускаются.

type dbEnv struct {
	t       *testing.T
	ctx     context.Context
	pool    *pgxpool.Pool
	cat     *classifier.Catalog
	svc     *Service
	q       *fakeQueue
	aud     *fakeAuditor
	ai      *fakeAI
	mux     http.Handler
	teacher uuid.UUID
	admin   uuid.UUID
}

func newDBEnv(t *testing.T) *dbEnv {
	t.Helper()
	pool := pgtest.New(t)
	ctx := context.Background()
	if err := classifier.SeedReference(ctx, pool); err != nil {
		t.Fatalf("SeedReference: %v", err)
	}
	cat := classifier.NewCatalog(pool, discardLog())
	if err := cat.Reload(ctx); err != nil {
		t.Fatalf("catalog reload: %v", err)
	}
	e := &dbEnv{t: t, ctx: ctx, pool: pool, cat: cat, q: &fakeQueue{}, aud: &fakeAuditor{}, ai: &fakeAI{}}
	e.teacher = e.user(DemoTeacherLogin, "teacher")
	e.admin = e.user("admin", "admin")
	e.svc = New(Deps{Pool: pool, Catalog: cat, Queue: e.q, AI: e.ai, Auditor: e.aud, Log: discardLog()})
	e.mux = newMux(e.svc)
	return e
}

func (e *dbEnv) user(login, role string) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO users (id, login, password_hash, role, last_name, first_name)
		VALUES ($1, $2, 'x', $3, 'Тестов', 'Тест')`, id, login, role); err != nil {
		e.t.Fatalf("insert user: %v", err)
	}
	return id
}

// do — запрос от имени преподавателя.
func (e *dbEnv) do(method, path string, body any) testResp {
	e.t.Helper()
	return call(e.t, e.mux, method, path, "teacher", e.teacher, body)
}

func (e *dbEnv) ok(r testResp, status int) testResp {
	e.t.Helper()
	if r.status != status {
		e.t.Fatalf("status %d, want %d: %s", r.status, status, r.body)
	}
	return r
}

func (e *dbEnv) scenario(r testResp) public.Scenario {
	e.t.Helper()
	var s public.Scenario
	r.json(e.t, &s)
	return s
}

func (e *dbEnv) get(id uuid.UUID) public.Scenario {
	e.t.Helper()
	return e.scenario(e.ok(e.do(http.MethodGet, "/scenarios/"+id.String(), nil), http.StatusOK))
}

func (e *dbEnv) typeID(code string) string {
	e.t.Helper()
	info, ok := e.cat.TypeByCode(code)
	if !ok {
		e.t.Fatalf("classifier type %s not seeded", code)
	}
	return info.ID
}

// create — ручной сценарий через API.
func (e *dbEnv) create(title, category, mode string) public.Scenario {
	e.t.Helper()
	r := e.ok(e.do(http.MethodPost, "/scenarios", map[string]any{
		"title": title, "categoryId": category, "difficulty": 2, "mode": mode,
	}), http.StatusCreated)
	return e.scenario(r)
}

// completeDraft — форма АРМ, достаточная для подтверждения (все обязательные по умолчанию поля).
func (e *dbEnv) completeDraft(services ...string) public.IncidentCardDraft {
	d := convert.EmptyDraft()
	d.IncidentTypeIds = []string{e.typeID("101")}
	d.Address.Raw = "Москва, Тверская улица, 12"
	*d.Address.Street = "Тверская"
	*d.Address.House = "12"
	d.Applicant.Name = ptr("Мария")
	d.Applicant.Status = ptr(public.Очевидец)
	d.Description = "Задымление в квартире на 5 этаже"
	for _, c := range services {
		info, ok := e.cat.ServiceByCode(c)
		if !ok {
			e.t.Fatalf("service %s not seeded", c)
		}
		d.Services = append(d.Services, public.AssignedService{
			ServiceId: info.ID, Code: info.Code, Name: info.Name, ShortName: info.ShortName,
			Source: public.AssignedServiceSourceAuto, CurrentStatus: public.ReactionStatus(convert.ReactionAdded),
			CurrentStatusAt: convert.EtalonServiceTime, History: []public.ReactionStatusEntry{}, AllowedNext: []public.AllowedTransition{},
		})
	}
	return d
}

func callScript(turns ...string) map[string]any {
	ts := make([]map[string]any, 0, len(turns))
	for _, t := range turns {
		ts = append(ts, map[string]any{"speaker": "caller", "text": t})
	}
	return map[string]any{
		"caller":   map[string]any{"name": "Мария", "phone": "+7 (903) 511-33-67", "role": "соседка"},
		"address":  map[string]any{"raw": "Москва, Тверская улица, 12"},
		"keyFacts": []string{"Дым из окна"},
		"turns":    ts,
	}
}

// makeComplete — сценарий, который можно подтвердить.
func (e *dbEnv) makeComplete(id uuid.UUID, turns ...string) public.Scenario {
	e.t.Helper()
	if len(turns) == 0 {
		turns = []string{"Алло, у нас пожар!"}
	}
	r := e.ok(e.do(http.MethodPatch, "/scenarios/"+id.String(), map[string]any{
		"callScript": callScript(turns...), "etalonDraft": e.completeDraft(),
	}), http.StatusOK)
	return e.scenario(r)
}

// inLesson — сценарий попадает в занятие (как lessons при создании занятия).
func (e *dbEnv) inLesson(scenarioID uuid.UUID) {
	e.t.Helper()
	lessonID := uuid.New()
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO lessons (id, title, teacher_id, created_by, mode) VALUES ($1, 'Занятие', $2, $2, 'cards')`,
		lessonID, e.teacher); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO lesson_scenarios (lesson_id, scenario_id) VALUES ($1, $2)`, lessonID, scenarioID); err != nil {
		e.t.Fatal(err)
	}
}

func (e *dbEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *dbEnv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (e *dbEnv) inTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	return pg.WithTx(e.ctx, e.pool, fn)
}

// requireArrays — обязательные массивы ответа — [] (не null и не пропущены).
func requireArrays(t *testing.T, m map[string]any, paths ...string) {
	t.Helper()
	for _, p := range paths {
		var cur any = m
		for _, k := range strings.Split(p, ".") {
			obj, _ := cur.(map[string]any)
			cur = obj[k]
		}
		if _, ok := cur.([]any); !ok {
			t.Errorf("%s = %#v, want an array", p, cur)
		}
	}
}

// ---------------------------------------------------------------- создание, чтение, список

func TestDBCreateGetList(t *testing.T) {
	t.Parallel()
	e := newDBEnv(t)

	r := e.ok(e.do(http.MethodPost, "/scenarios", map[string]any{
		"title": "  Пожар в подвале  ", "categoryId": "it-101", "difficulty": 2, "mode": "cards",
	}), http.StatusCreated)
	m := r.obj(t)
	requireArrays(t, m, "requiredFields", "etalonDraft.incidentTypeIds", "etalonDraft.services",
		"callScript.turns", "callScript.keyFacts")
	for _, k := range []string{"id", "title", "categoryId", "categoryName", "difficulty", "mode", "source", "status",
		"callScript", "etalonCard", "etalonDraft", "etalonVersion", "requiredFields", "createdAt"} {
		if _, ok := m[k]; !ok {
			t.Errorf("required field %s missing: %s", k, r.body)
		}
	}
	created := e.scenario(r)
	if created.Title != "Пожар в подвале" || created.Source != "manual" || created.Status != "draft" ||
		created.EtalonVersion != 1 || created.CategoryId != e.typeID("101") || created.CategoryName == "" {
		t.Errorf("created %+v", created)
	}
	if deref(created.AuthorId) != e.teacher || deref(created.Version) != 1 || deref(created.InUse) || deref(created.TtsReady) {
		t.Errorf("author/version/inUse/ttsReady %+v", created)
	}
	if !slices.Equal(created.RequiredFields, []string{"applicant.name", "applicant.status", "address.raw", "incidentTypeIds", "description"}) {
		t.Errorf("required %v", created.RequiredFields)
	}
	if !slices.Equal(created.EtalonDraft.IncidentTypeIds, []string{e.typeID("101")}) || deref(created.EtalonCard.CategoryCode) != "101" {
		t.Errorf("etalon %+v / %+v", created.EtalonDraft.IncidentTypeIds, created.EtalonCard)
	}
	if e.aud.count("scenario.create") != 1 {
		t.Errorf("audit %v", e.aud.actions())
	}

	// GET отдаёт то же, что вернул POST (ответ POST собран в памяти).
	got := e.ok(e.do(http.MethodGet, "/scenarios/"+created.Id.String(), nil), http.StatusOK)
	var a, b any
	_ = json.Unmarshal(r.body, &a)
	_ = json.Unmarshal(got.body, &b)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Errorf("GET differs from POST:\nPOST %s\nGET  %s", ja, jb)
	}
	expectError(t, e.do(http.MethodGet, "/scenarios/"+uuid.NewString(), nil), http.StatusNotFound, httpx.CodeNotFound)

	both := e.create("Утечка газа", "104", "both")

	list := func(q string) []string {
		t.Helper()
		r := e.ok(e.do(http.MethodGet, "/scenarios"+q, nil), http.StatusOK)
		if strings.TrimSpace(string(r.body)) == "null" {
			t.Fatalf("list %s is null", q)
		}
		var out []public.Scenario
		r.json(t, &out)
		ids := make([]string, len(out))
		for i := range out {
			ids[i] = out[i].Id.String()
		}
		return ids
	}
	c, bo := created.Id.String(), both.Id.String()
	cases := []struct {
		q    string
		want []string
	}{
		{"", []string{bo, c}}, // новые сверху
		{"?status=draft", []string{bo, c}},
		{"?status=validated", []string{}},
		{"?mode=cards", []string{bo, c}}, // both годится и для cards
		{"?mode=card_actions", []string{bo}},
		{"?mode=both", []string{bo}},
		{"?categoryId=101", []string{c}},
		{"?categoryId=it-104", []string{bo}},
		{"?categoryId=" + e.typeID("104"), []string{bo}},
		{"?categoryId=" + uuid.NewString(), []string{}},
	}
	for _, tc := range cases {
		if got := list(tc.q); !slices.Equal(got, tc.want) {
			t.Errorf("list%s = %v, want %v", tc.q, got, tc.want)
		}
	}

	// Архив скрыт по умолчанию и виден по status=archived.
	e.exec(`UPDATE scenarios SET status = 'archived', archived_at = now() WHERE id = $1`, both.Id)
	if got := list(""); !slices.Equal(got, []string{c}) {
		t.Errorf("archived must be hidden: %v", got)
	}
	if got := list("?status=archived"); !slices.Equal(got, []string{bo}) {
		t.Errorf("status=archived: %v", got)
	}

	// Админ видит ту же общую библиотеку.
	r = e.ok(call(t, e.mux, http.MethodGet, "/scenarios", "admin", e.admin, nil), http.StatusOK)
	var all []public.Scenario
	r.json(t, &all)
	if len(all) != 1 {
		t.Errorf("admin list %d", len(all))
	}
}

// ---------------------------------------------------------------- правка

func TestDBPatchSimpleFieldsAndEtalonVersions(t *testing.T) {
	t.Parallel()
	e := newDBEnv(t)
	s := e.create("Пожар в подвале", "101", "cards")
	path := "/scenarios/" + s.Id.String()

	r := e.ok(e.do(http.MethodPatch, path, map[string]any{
		"title": " Пожар в подвале дома ", "difficulty": 3, "mode": "both", "teacherComment": "  проверить адрес ",
	}), http.StatusOK)
	got := e.scenario(r)
	if got.Title != "Пожар в подвале дома" || got.Difficulty != 3 || got.Mode != "both" || deref(got.TeacherComment) != "проверить адрес" {
		t.Errorf("patched %+v", got)
	}
	if got.EtalonVersion != 1 || e.aud.count("etalon.version") != 0 {
		t.Errorf("simple fields must not version the etalon: v%d", got.EtalonVersion)
	}
	if e.aud.count("scenario.update") != 1 {
		t.Errorf("audit %v", e.aud.actions())
	}

	// Пустой комментарий очищает поле; повтор того же — без изменений и без аудита.
	got = e.scenario(e.ok(e.do(http.MethodPatch, path, map[string]any{"teacherComment": ""}), http.StatusOK))
	if got.TeacherComment != nil {
		t.Errorf("comment not cleared: %q", *got.TeacherComment)
	}
	e.ok(e.do(http.MethodPatch, path, map[string]any{"teacherComment": "", "title": "Пожар в подвале дома"}), http.StatusOK)
	if e.aud.count("scenario.update") != 2 {
		t.Errorf("no-op patch audited: %v", e.aud.actions())
	}

	// Та же форма АРМ и те же обязательные поля (редактор шлёт их всегда) — версия не растёт.
	cur := e.get(s.Id)
	got = e.scenario(e.ok(e.do(http.MethodPatch, path, map[string]any{
		"title": "Пожар в подвале", "etalonDraft": cur.EtalonDraft, "requiredFields": cur.RequiredFields,
	}), http.StatusOK))
	if got.EtalonVersion != 1 {
		t.Errorf("unchanged etalon got a new version: v%d", got.EtalonVersion)
	}

	// Службы в эталоне — версия 2, карточка с кодами служб.
	got = e.scenario(e.ok(e.do(http.MethodPatch, path, map[string]any{"etalonDraft": e.completeDraft("101", "103")}), http.StatusOK))
	if got.EtalonVersion != 2 || !slices.Equal(deref(got.EtalonCard.ServicesToNotify), []string{"101", "103"}) {
		t.Fatalf("v%d services %v", got.EtalonVersion, deref(got.EtalonCard.ServicesToNotify))
	}

	// Регрессия (review): преподаватель снял все службы — старый список не возвращается.
	rf := append(slices.Clone(got.RequiredFields), "services")
	r = e.ok(e.do(http.MethodPatch, path, map[string]any{"etalonDraft": e.completeDraft(), "requiredFields": rf}), http.StatusOK)
	got = e.scenario(r)
	if got.EtalonVersion != 3 || len(got.EtalonDraft.Services) != 0 || got.EtalonCard.ServicesToNotify != nil {
		t.Fatalf("services removed: v%d draft %d card %v", got.EtalonVersion, len(got.EtalonDraft.Services), deref(got.EtalonCard.ServicesToNotify))
	}
	var stored string
	if err := e.pool.QueryRow(e.ctx, `SELECT card::text FROM etalons WHERE scenario_id = $1 AND is_current`, s.Id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "services_to_notify") {
		t.Errorf("stored card keeps removed services: %s", stored)
	}
	if n := e.count(`SELECT count(*)::int FROM etalons WHERE scenario_id = $1`, s.Id); n != 3 {
		t.Errorf("etalon versions %d", n)
	}
	if n := e.count(`SELECT count(*)::int FROM etalons WHERE scenario_id = $1 AND is_current`, s.Id); n != 1 {
		t.Errorf("current etalons %d", n)
	}
	if e.aud.count("etalon.version") != 2 {
		t.Errorf("etalon.version audit %v", e.aud.actions())
	}

	// Черновик: реплики сохраняются, озвучка не ставится (только при подтверждении).
	got = e.scenario(e.ok(e.do(http.MethodPatch, path, map[string]any{"callScript": callScript("Алло!", "  ")}), http.StatusOK))
	if len(got.CallScript.Turns) != 1 || got.CallScript.Turns[0].TtsHash != nil {
		t.Errorf("turns %+v", got.CallScript.Turns)
	}
	if n := len(e.q.byType(core.JobTTS)); n != 0 {
		t.Errorf("draft must not enqueue TTS: %d", n)
	}

	expectError(t, e.do(http.MethodPatch, "/scenarios/"+uuid.NewString(), map[string]any{"title": "Новое"}), http.StatusNotFound, httpx.CodeNotFound)
}

func TestDBPatchConflicts(t *testing.T) {
	t.Parallel()
	e := newDBEnv(t)

	used := e.create("Пожар в школе", "101", "cards")
	e.inLesson(used.Id)
	r := e.do(http.MethodPatch, "/scenarios/"+used.Id.String(), map[string]any{"title": "Другое"})
	er := expectError(t, r, http.StatusConflict, httpx.CodeConflict)
	if er.Details["lessonsCount"] != float64(1) {
		t.Errorf("details %v", er.Details)
	}
	got := e.get(used.Id)
	if got.Title != "Пожар в школе" || !deref(got.InUse) || deref(got.LessonsCount) != 1 {
		t.Errorf("in-use scenario %+v", got)
	}

	arch := e.create("Пожар в гараже", "101", "cards")
	e.exec(`UPDATE scenarios SET status = 'archived', archived_at = now() WHERE id = $1`, arch.Id)
	expectError(t, e.do(http.MethodPatch, "/scenarios/"+arch.Id.String(), map[string]any{"title": "Другое"}), http.StatusConflict, httpx.CodeConflict)
	expectError(t, e.do(http.MethodPost, "/scenarios/"+arch.Id.String()+"/approve", nil), http.StatusConflict, httpx.CodeConflict)
	expectError(t, e.do(http.MethodPost, "/scenarios/"+arch.Id.String()+"/reject", map[string]any{"reason": "плохой"}), http.StatusConflict, httpx.CodeConflict)
	expectError(t, e.do(http.MethodPost, "/scenarios/"+arch.Id.String()+"/versions", nil), http.StatusConflict, httpx.CodeConflict)
}

// ---------------------------------------------------------------- подтверждение

func TestDBApproveFlow(t *testing.T) {
	t.Parallel()
	e := newDBEnv(t)
	s := e.create("Пожар в квартире", "101", "cards")
	path := "/scenarios/" + s.Id.String()

	er := expectError(t, e.do(http.MethodPost, path+"/approve", nil), http.StatusUnprocessableEntity, httpx.CodeValidation)
	fields, _ := er.Details["fields"].([]any)
	var names []string
	for _, f := range fields {
		names = append(names, f.(string))
	}
	for _, want := range []string{"адрес в эталоне", "ФИО заявителя в эталоне", "реплики заявителя"} {
		if !slices.Contains(names, want) {
			t.Errorf("gaps %v lack %q", names, want)
		}
	}
	if e.get(s.Id).Status != "draft" {
		t.Fatal("failed approve changed status")
	}

	e.makeComplete(s.Id, "Алло, у нас пожар!", "Пятый этаж", "Алло, у нас пожар!")
	got := e.scenario(e.ok(e.do(http.MethodPost, path+"/approve", nil), http.StatusOK))
	if got.Status != "validated" || deref(got.ValidatedBy) != e.teacher || got.ValidatedAt == nil {
		t.Fatalf("approved %+v", got)
	}
	snap := settings.Defaults()
	h1 := convert.TTSHash("Алло, у нас пожар!", snap.TTS.Voice, snap.TTS.Rate)
	h2 := convert.TTSHash("Пятый этаж", snap.TTS.Voice, snap.TTS.Rate)
	var hashes []string
	for _, tr := range got.CallScript.Turns {
		hashes = append(hashes, deref(tr.TtsHash))
	}
	if !slices.Equal(hashes, []string{h1, h2, h1}) {
		t.Errorf("tts hashes %v", hashes)
	}
	jobs := e.q.byType(core.JobTTS)
	if len(jobs) != 2 {
		t.Fatalf("tts jobs %d, want 2 (one per unique caller turn)", len(jobs))
	}
	req, ok := jobs[0].Payload.(*aiservice.TtsJobRequest)
	if !ok || req.TextHash != h1 || req.Text != "Алло, у нас пожар!" || deref(req.Voice) != snap.TTS.Voice ||
		req.RequestId != jobs[0].ID || jobs[0].DedupKey != "tts:"+h1 || jobs[0].Priority != core.PriorityTTS ||
		jobs[0].RefID != s.Id || jobs[0].RefType != core.RefScenario {
		t.Errorf("tts job %+v payload %+v", jobs[0], req)
	}
	if deref(got.TtsReady) {
		t.Error("ttsReady before audio exists")
	}

	// Озвучка готова — ttsReady.
	e.exec(`INSERT INTO tts_cache (text_hash, voice, rate, file_path) VALUES ($1, 'xenia', 1, 'a.wav'), ($2, 'xenia', 1, 'b.wav')`, h1, h2)
	if !deref(e.get(s.Id).TtsReady) {
		t.Error("ttsReady must be true when every caller turn is cached")
	}

	// Повтор — 200, без смены статуса, новых задач и аудита.
	got = e.scenario(e.ok(e.do(http.MethodPost, path+"/approve", nil), http.StatusOK))
	if got.Status != "validated" || len(e.q.byType(core.JobTTS)) != 2 || e.aud.count("scenario.approve") != 1 {
		t.Errorf("repeat approve: %s jobs %d audit %v", got.Status, len(e.q.byType(core.JobTTS)), e.aud.actions())
	}

	// Правка подтверждённого: новая реплика сразу получает хэш и задачу озвучки.
	got = e.scenario(e.ok(e.do(http.MethodPatch, path, map[string]any{"callScript": callScript("Алло, у нас пожар!", "Шестой этаж")}), http.StatusOK))
	h3 := convert.TTSHash("Шестой этаж", snap.TTS.Voice, snap.TTS.Rate)
	if deref(got.CallScript.Turns[1].TtsHash) != h3 || got.Status != "validated" {
		t.Errorf("edited validated turns %+v", got.CallScript.Turns)
	}
	if jobs := e.q.byType(core.JobTTS); len(jobs) != 3 || jobs[2].Payload.(*aiservice.TtsJobRequest).TextHash != h3 {
		t.Errorf("tts jobs after edit %d", len(jobs))
	}

	// Подтверждённый сценарий должен остаться полным: убрать реплики заявителя нельзя.
	r := e.do(http.MethodPatch, path, map[string]any{"callScript": map[string]any{
		"caller": map[string]any{}, "address": map[string]any{}, "keyFacts": []string{},
		"turns": []map[string]any{{"speaker": "operator_hint", "text": "Спросите адрес"}},
	}})
	er = expectError(t, r, http.StatusBadRequest, httpx.CodeValidation)
	if f, _ := er.Details["fields"].([]any); len(f) == 0 {
		t.Errorf("details.fields missing: %s", r.body)
	}
	if turns := e.get(s.Id).CallScript.Turns; len(turns) != 2 {
		t.Errorf("rejected patch must roll back, turns %+v", turns)
	}
}

// ---------------------------------------------------------------- отклонение и версии

func TestDBRejectAndVersions(t *testing.T) {
	t.Parallel()
	e := newDBEnv(t)

	s := e.create("Пожар в подъезде", "101", "cards")
	got := e.scenario(e.ok(e.do(http.MethodPost, "/scenarios/"+s.Id.String()+"/reject", map[string]any{"reason": "  Нет адреса  "}), http.StatusOK))
	if got.Status != "rejected" || deref(got.TeacherComment) != "Нет адреса" || got.GenerationMeta == nil ||
		(*got.GenerationMeta)["rejected_reason"] != "Нет адреса" {
		t.Errorf("rejected %+v meta %v", got, got.GenerationMeta)
	}
	if e.aud.count("scenario.reject") != 1 {
		t.Errorf("audit %v", e.aud.actions())
	}
	expectError(t, e.do(http.MethodPost, "/scenarios/"+uuid.NewString()+"/reject", map[string]any{"reason": "Нет адреса"}), http.StatusNotFound, httpx.CodeNotFound)

	used := e.create("Пожар в школе", "101", "cards")
	e.inLesson(used.Id)
	er := expectError(t, e.do(http.MethodPost, "/scenarios/"+used.Id.String()+"/reject", map[string]any{"reason": "Устарел"}), http.StatusConflict, httpx.CodeConflict)
	if er.Details["lessonsCount"] != float64(1) {
		t.Errorf("details %v", er.Details)
	}

	// Новая версия занятого сценария: копия-черновик от текущего пользователя, эталон v1 — копия.
	src := e.get(used.Id)
	r := e.ok(call(t, e.mux, http.MethodPost, "/scenarios/"+used.Id.String()+"/versions", "admin", e.admin, nil), http.StatusCreated)
	v2 := e.scenario(r)
	requireArrays(t, r.obj(t), "requiredFields", "etalonDraft.services", "callScript.turns")
	if deref(v2.ParentScenarioId) != used.Id || deref(v2.Version) != 2 || v2.Status != "draft" || deref(v2.AuthorId) != e.admin ||
		v2.ValidatedBy != nil || v2.EtalonVersion != 1 || v2.Title != src.Title || deref(v2.InUse) {
		t.Errorf("version %+v", v2)
	}
	ja, _ := json.Marshal(src.EtalonDraft)
	jb, _ := json.Marshal(v2.EtalonDraft)
	if string(ja) != string(jb) || !slices.Equal(src.RequiredFields, v2.RequiredFields) {
		t.Errorf("etalon not copied:\n%s\n%s", ja, jb)
	}
	if (*v2.GenerationMeta)["parent_id"] != used.Id.String() {
		t.Errorf("meta %v", v2.GenerationMeta)
	}
	// Номер — максимум по всей цепочке + 1, от какого бы звена ни создавали.
	v3 := e.scenario(e.ok(e.do(http.MethodPost, "/scenarios/"+used.Id.String()+"/versions", nil), http.StatusCreated))
	v4 := e.scenario(e.ok(e.do(http.MethodPost, "/scenarios/"+v2.Id.String()+"/versions", nil), http.StatusCreated))
	if deref(v3.Version) != 3 || deref(v4.Version) != 4 || deref(v4.ParentScenarioId) != v2.Id {
		t.Errorf("chain versions %d %d parent %v", deref(v3.Version), deref(v4.Version), v4.ParentScenarioId)
	}
	if p := e.get(used.Id); deref(p.Version) != 1 || deref(p.LessonsCount) != 1 {
		t.Errorf("parent changed: %+v", p)
	}
	// Копия правится (занятий у неё нет).
	e.ok(e.do(http.MethodPatch, "/scenarios/"+v2.Id.String(), map[string]any{"title": "Пожар в школе, v2"}), http.StatusOK)
	expectError(t, e.do(http.MethodPost, "/scenarios/"+uuid.NewString()+"/versions", nil), http.StatusNotFound, httpx.CodeNotFound)

	// Параллельные «новые версии» одной цепочки получают разные номера.
	var wg sync.WaitGroup
	versions := make([]int, 4)
	for i := range versions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := call(t, e.mux, http.MethodPost, "/scenarios/"+v3.Id.String()+"/versions", "teacher", e.teacher, nil)
			if r.status == http.StatusCreated {
				var s public.Scenario
				_ = json.Unmarshal(r.body, &s)
				versions[i] = deref(s.Version)
			}
		}(i)
	}
	wg.Wait()
	slices.Sort(versions)
	if !slices.Equal(versions, []int{5, 6, 7, 8}) {
		t.Errorf("concurrent versions %v", versions)
	}
}

// ---------------------------------------------------------------- генерация

func genResult(t *testing.T) *callbacks.AiResult {
	t.Helper()
	return &callbacks.AiResult{
		Status:   callbacks.Ok,
		Scenario: generatedResult(t, genResultJSON),
		Engine:   components.Engine{DurationMs: 900, LlmModel: ptr("qwen2.5:7b")},
	}
}

func TestDBGenerate(t *testing.T) {
	t.Parallel()
	e := newDBEnv(t)
	e.create("Запах газа в подвале", "104", "cards")

	r := e.ok(e.do(http.MethodPost, "/scenarios/generate", map[string]any{
		"categoryId": "it-104", "difficulty": 2, "mode": "both", "teacherComment": "  без пострадавших ",
	}), http.StatusAccepted)
	var acc struct {
		JobID      uuid.UUID `json:"jobId"`
		ScenarioID uuid.UUID `json:"scenarioId"`
		EstWaitSec int       `json:"estWaitSec"`
	}
	r.json(t, &acc)
	jobs := e.q.byType(core.JobGenerateScenario)
	if len(jobs) != 1 || jobs[0].ID != acc.JobID || jobs[0].RefID != acc.ScenarioID || acc.EstWaitSec != 42 ||
		jobs[0].DedupKey != "generate:"+acc.ScenarioID.String() || jobs[0].Priority != core.PriorityGenerate {
		t.Fatalf("accepted %+v jobs %+v", acc, jobs)
	}
	req := jobs[0].Payload.(*aiservice.GenerateJobRequest)
	if req.Spec.Category.Code != "104" || deref(req.Spec.TeacherComment) != "без пострадавших" ||
		!slices.Contains(deref(req.Spec.AvoidTitles), "Запах газа в подвале") || req.RequestId != acc.JobID {
		t.Errorf("generate payload %+v", req.Spec)
	}

	stub := e.get(acc.ScenarioID)
	if stub.Status != "draft" || stub.Source != "generated" || !strings.HasSuffix(stub.Title, "— генерируется…") ||
		stub.EtalonVersion != 0 || deref(stub.TeacherComment) != "без пострадавших" {
		t.Errorf("stub %+v", stub)
	}
	requireArrays(t, e.ok(e.do(http.MethodGet, "/scenarios/"+acc.ScenarioID.String(), nil), http.StatusOK).obj(t),
		"requiredFields", "etalonDraft.services", "etalonDraft.incidentTypeIds", "callScript.turns")

	h := generateHandler{e.svc}
	job := core.JobRecord{ID: acc.JobID, Type: core.JobGenerateScenario, RefType: core.RefScenario, RefID: acc.ScenarioID}
	if err := e.inTx(func(ctx context.Context, tx pgx.Tx) error { return h.ApplyResult(ctx, tx, job, genResult(t)) }); err != nil {
		t.Fatal(err)
	}
	got := e.get(acc.ScenarioID)
	if got.Status != "generated" || got.Title != "Сильный запах газа на лестничной клетке" || got.EtalonVersion != 1 {
		t.Fatalf("generated %+v", got)
	}
	if !slices.Equal(got.EtalonDraft.IncidentTypeIds, []string{e.typeID("104")}) || deref(got.EtalonCard.CategoryCode) != "104" {
		t.Errorf("etalon types %v card code %q", got.EtalonDraft.IncidentTypeIds, deref(got.EtalonCard.CategoryCode))
	}
	if got.EtalonDraft.Applicant.Status == nil || *got.EtalonDraft.Applicant.Status != public.Родственник {
		t.Errorf("applicant status %v", got.EtalonDraft.Applicant.Status)
	}
	if len(got.EtalonDraft.Services) != 2 || got.ExpectedDialogue == nil || len(got.ExpectedDialogue.Checklist) != 2 {
		t.Errorf("services %d dialogue %+v", len(got.EtalonDraft.Services), got.ExpectedDialogue)
	}
	if b := got.CallScript.Dialogue; b == nil || len(b.Facts) != 2 || b.Facts[1].Id == b.Facts[0].Id {
		t.Errorf("brief %+v", got.CallScript.Dialogue)
	}
	if deref(got.NotesForTeacher) != "проверьте адрес" || (*got.GenerationMeta)["job_id"] != acc.JobID.String() {
		t.Errorf("meta %v notes %q", got.GenerationMeta, deref(got.NotesForTeacher))
	}

	// Сохранение редактора без правки формы АРМ не создаёт новую версию эталона и не
	// подменяет карточку LLM проекцией формы.
	before := e.ok(e.do(http.MethodGet, "/scenarios/"+acc.ScenarioID.String(), nil), http.StatusOK).obj(t)["etalonCard"]
	got = e.scenario(e.ok(e.do(http.MethodPatch, "/scenarios/"+acc.ScenarioID.String(), map[string]any{
		"title": "Запах газа на лестнице", "etalonDraft": got.EtalonDraft, "requiredFields": got.RequiredFields,
	}), http.StatusOK))
	after := e.ok(e.do(http.MethodGet, "/scenarios/"+acc.ScenarioID.String(), nil), http.StatusOK).obj(t)["etalonCard"]
	jbef, _ := json.Marshal(before)
	jaft, _ := json.Marshal(after)
	if got.EtalonVersion != 1 || string(jbef) != string(jaft) {
		t.Errorf("title-only save: v%d\nbefore %s\nafter  %s", got.EtalonVersion, jbef, jaft)
	}

	// Повторный результат по уже применённой генерации — игнорируется.
	if err := e.inTx(func(ctx context.Context, tx pgx.Tx) error { return h.ApplyResult(ctx, tx, job, genResult(t)) }); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*)::int FROM etalons WHERE scenario_id = $1`, acc.ScenarioID); n != 1 {
		t.Errorf("repeat result created etalon versions: %d", n)
	}
	// Результат для удалённого сценария — не ошибка.
	missing := job
	missing.RefID = uuid.New()
	if err := e.inTx(func(ctx context.Context, tx pgx.Tx) error { return h.ApplyResult(ctx, tx, missing, genResult(t)) }); err != nil {
		t.Errorf("missing scenario: %v", err)
	}
}

func TestDBGenerateWithoutDialogueAndFailure(t *testing.T) {
	t.Parallel()
	e := newDBEnv(t)
	h := generateHandler{e.svc}

	start := func(withDialogue bool) (uuid.UUID, core.JobRecord) {
		t.Helper()
		r := e.ok(e.do(http.MethodPost, "/scenarios/generate", map[string]any{
			"categoryId": "104", "difficulty": 1, "mode": "cards", "withDialogue": withDialogue,
		}), http.StatusAccepted)
		var acc struct {
			JobID      uuid.UUID `json:"jobId"`
			ScenarioID uuid.UUID `json:"scenarioId"`
		}
		r.json(t, &acc)
		return acc.ScenarioID, core.JobRecord{ID: acc.JobID, Type: core.JobGenerateScenario, RefType: core.RefScenario, RefID: acc.ScenarioID}
	}

	id, job := start(false)
	if err := e.inTx(func(ctx context.Context, tx pgx.Tx) error { return h.ApplyResult(ctx, tx, job, genResult(t)) }); err != nil {
		t.Fatal(err)
	}
	got := e.get(id)
	if got.CallScript.Dialogue != nil || got.ExpectedDialogue != nil {
		t.Errorf("withDialogue=false kept brief/checklist: %+v %+v", got.CallScript.Dialogue, got.ExpectedDialogue)
	}

	// Провал генерации: заглушка в архив с причиной; из списка по умолчанию пропадает.
	id2, job2 := start(true)
	if err := e.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return h.ApplyFailure(ctx, tx, job2, "llm_timeout", strings.Repeat("ошибка ", 300))
	}); err != nil {
		t.Fatal(err)
	}
	got = e.get(id2)
	if got.Status != "archived" || (*got.GenerationMeta)["error_code"] != "llm_timeout" ||
		len([]rune((*got.GenerationMeta)["error"].(string))) != 1000 {
		t.Errorf("failed generation %+v", got)
	}
	var list []public.Scenario
	e.ok(e.do(http.MethodGet, "/scenarios", nil), http.StatusOK).json(t, &list)
	for _, s := range list {
		if s.Id == id2 {
			t.Error("archived stub listed by default")
		}
	}
	// Результат, пришедший после провала/отклонения, не воскрешает заглушку.
	if err := e.inTx(func(ctx context.Context, tx pgx.Tx) error { return h.ApplyResult(ctx, tx, job2, genResult(t)) }); err != nil {
		t.Fatal(err)
	}
	if e.get(id2).Status != "archived" {
		t.Error("late result applied to archived stub")
	}

	// Провал уже применённой генерации ничего не трогает.
	if err := e.inTx(func(ctx context.Context, tx pgx.Tx) error { return h.ApplyFailure(ctx, tx, job, "internal", "x") }); err != nil {
		t.Fatal(err)
	}
	if e.get(id).Status != "generated" {
		t.Error("failure archived an applied scenario")
	}

	// Очередь вернула ошибку — сценарий-заглушка не остаётся (одна транзакция).
	e.q.mu.Lock()
	e.q.err = errors.New("queue down")
	e.q.mu.Unlock()
	before := e.count(`SELECT count(*)::int FROM scenarios`)
	expectError(t, e.do(http.MethodPost, "/scenarios/generate", map[string]any{"categoryId": "104", "difficulty": 1, "mode": "cards"}),
		http.StatusInternalServerError, httpx.CodeInternal)
	if after := e.count(`SELECT count(*)::int FROM scenarios`); after != before {
		t.Errorf("stub left after enqueue failure: %d -> %d", before, after)
	}
}

// ---------------------------------------------------------------- озвучка

func TestDBTTSPreview(t *testing.T) {
	t.Parallel()
	e := newDBEnv(t)
	s := e.create("Пожар в квартире", "101", "cards")
	path := "/scenarios/" + s.Id.String() + "/tts-preview"
	snap := settings.Defaults()
	text := "Алло, у нас пожар!"
	hash := convert.TTSHash(text, snap.TTS.Voice, snap.TTS.Rate)

	var ref public.AudioRef
	e.ok(e.do(http.MethodPost, path, map[string]any{"text": "  " + text + "  "}), http.StatusOK).json(t, &ref)
	if ref.AudioUrl != convert.MediaURL("ab/"+hash+".wav") || ref.DurationMs != 1500 || deref(ref.Mime) != "audio/wav" {
		t.Errorf("audio %+v", ref)
	}
	if e.ai.callCount() != 1 || e.ai.calls[0].TextHash != hash || deref(e.ai.calls[0].Voice) != snap.TTS.Voice || e.ai.calls[0].Text != text {
		t.Errorf("ai calls %+v", e.ai.calls)
	}
	if n := e.count(`SELECT count(*)::int FROM tts_cache WHERE text_hash = $1`, hash); n != 1 {
		t.Errorf("preview not cached")
	}

	// Повтор — из кэша, ai-service не зовётся.
	e.ok(e.do(http.MethodPost, path, map[string]any{"text": text}), http.StatusOK)
	if e.ai.callCount() != 1 {
		t.Errorf("cache miss on repeat: %d calls", e.ai.callCount())
	}

	// Голос заявителя из легенды важнее голоса по умолчанию; явный voice — важнее обоих.
	e.exec(`UPDATE scenarios SET call_script = jsonb_set(call_script, '{caller}', '{"voice":"baya"}') WHERE id = $1`, s.Id)
	e.ok(e.do(http.MethodPost, path, map[string]any{"text": text}), http.StatusOK)
	e.ok(e.do(http.MethodPost, path, map[string]any{"text": text, "voice": "aidar"}), http.StatusOK)
	if e.ai.callCount() != 3 || deref(e.ai.calls[1].Voice) != "baya" || deref(e.ai.calls[2].Voice) != "aidar" {
		t.Errorf("voices %d", e.ai.callCount())
	}

	// ai-service занят — 503 ai_unavailable с Retry-After.
	e.ai.mu.Lock()
	e.ai.err = &core.AIError{Kind: core.AIBusy, RetryAfter: 7, Message: "busy"}
	e.ai.mu.Unlock()
	r := e.do(http.MethodPost, path, map[string]any{"text": "Новая реплика"})
	expectError(t, r, http.StatusServiceUnavailable, httpx.CodeAIUnavailable)
	if r.header.Get("Retry-After") != "7" {
		t.Errorf("Retry-After %q", r.header.Get("Retry-After"))
	}

	// Небезопасный путь файла — 503 и не в кэш.
	e.ai.mu.Lock()
	e.ai.err = nil
	e.ai.res = &components.TtsResult{FilePath: "../../etc/passwd", DurationMs: 10}
	e.ai.mu.Unlock()
	expectError(t, e.do(http.MethodPost, path, map[string]any{"text": "Ещё реплика"}), http.StatusServiceUnavailable, httpx.CodeAIUnavailable)
	if n := e.count(`SELECT count(*)::int FROM tts_cache WHERE file_path LIKE '%..%'`); n != 0 {
		t.Error("unsafe path cached")
	}

	expectError(t, e.do(http.MethodPost, "/scenarios/"+uuid.NewString()+"/tts-preview", map[string]any{"text": text}), http.StatusNotFound, httpx.CodeNotFound)

	// Без ai-клиента: из кэша — 200, мимо кэша — 503.
	noAI := newMux(New(Deps{Pool: e.pool, Catalog: e.cat, Log: discardLog()}))
	e.ok(call(t, noAI, http.MethodPost, path, "teacher", e.teacher, map[string]any{"text": text, "voice": snap.TTS.Voice}), http.StatusOK)
	expectError(t, call(t, noAI, http.MethodPost, path, "teacher", e.teacher, map[string]any{"text": "Не озвучено"}),
		http.StatusServiceUnavailable, httpx.CodeAIUnavailable)
}

func TestDBTTSResultHandler(t *testing.T) {
	t.Parallel()
	e := newDBEnv(t)
	h := ttsHandler{e.svc}
	rate := float32(1.25)
	payload, _ := json.Marshal(aiservice.TtsJobRequest{TextHash: "job-hash", Voice: ptr("baya"), Rate: &rate, Text: "Алло"})
	job := core.JobRecord{ID: uuid.New(), Type: core.JobTTS, RefType: core.RefScenario, RefID: uuid.New(), Payload: payload}

	apply := func(res *callbacks.AiResult) {
		t.Helper()
		if err := e.inTx(func(ctx context.Context, tx pgx.Tx) error { return h.ApplyResult(ctx, tx, job, res) }); err != nil {
			t.Fatal(err)
		}
	}
	apply(&callbacks.AiResult{Tts: &components.TtsResult{FilePath: "ab/1.wav", DurationMs: 900, TextHash: "other-hash"}})
	var (
		voice, file string
		rateDB      float64
		dur         *int
	)
	if err := e.pool.QueryRow(e.ctx, `SELECT voice, rate::float8, file_path, duration_ms FROM tts_cache WHERE text_hash = 'job-hash'`).
		Scan(&voice, &rateDB, &file, &dur); err != nil {
		t.Fatalf("cache row by job hash: %v", err)
	}
	if voice != "baya" || rateDB != 1.25 || file != "ab/1.wav" || deref(dur) != 900 {
		t.Errorf("row %s %v %s %v", voice, rateDB, file, dur)
	}
	// Переотправка обновляет путь.
	apply(&callbacks.AiResult{Tts: &components.TtsResult{FilePath: "ab/2.ogg", TextHash: "job-hash"}})
	if err := e.pool.QueryRow(e.ctx, `SELECT file_path, duration_ms FROM tts_cache WHERE text_hash = 'job-hash'`).Scan(&file, &dur); err != nil {
		t.Fatal(err)
	}
	if file != "ab/2.ogg" || dur != nil {
		t.Errorf("upsert %s %v", file, dur)
	}
	// Пустой результат и небезопасный путь — тихо отбрасываются.
	apply(&callbacks.AiResult{})
	job.Payload = nil
	apply(&callbacks.AiResult{Tts: &components.TtsResult{FilePath: "/abs/x.wav", TextHash: "h2"}})
	apply(&callbacks.AiResult{Tts: &components.TtsResult{FilePath: "x.wav", TextHash: ""}})
	if n := e.count(`SELECT count(*)::int FROM tts_cache`); n != 1 {
		t.Errorf("tts_cache rows %d", n)
	}
	// Без payload задачи — хэш из результата, голос и темп — из настроек.
	apply(&callbacks.AiResult{Tts: &components.TtsResult{FilePath: "c/3.wav", TextHash: "h3", DurationMs: 5}})
	if err := e.pool.QueryRow(e.ctx, `SELECT voice FROM tts_cache WHERE text_hash = 'h3'`).Scan(&voice); err != nil || voice != settings.Defaults().TTS.Voice {
		t.Errorf("fallback voice %q %v", voice, err)
	}
	if err := h.ApplyFailure(e.ctx, nil, job, "tts_failed", "boom"); err != nil {
		t.Errorf("ApplyFailure: %v", err)
	}
}

// ---------------------------------------------------------------- сид демо-сценариев

func (e *dbEnv) seedDeps() Deps {
	return Deps{Pool: e.pool, Catalog: e.cat, Queue: e.q, Log: discardLog()}
}

func TestDBSeedDemoScenarios(t *testing.T) {
	t.Parallel()
	e := newDBEnv(t)
	if err := SeedDemoScenarios(e.ctx, e.seedDeps()); err != nil {
		t.Fatal(err)
	}
	var list []public.Scenario
	e.ok(e.do(http.MethodGet, "/scenarios", nil), http.StatusOK).json(t, &list)
	if len(list) != 4 {
		t.Fatalf("demo scenarios %d, want 4", len(list))
	}
	validated := 0
	for _, s := range list {
		if s.EtalonVersion != 1 || len(s.RequiredFields) == 0 || s.EtalonCard.ServicesToNotify == nil || len(s.EtalonDraft.IncidentTypeIds) == 0 {
			t.Errorf("%s: etalon %+v", s.Title, s)
		}
		for _, id := range s.EtalonDraft.IncidentTypeIds {
			if _, err := uuid.Parse(id); err != nil {
				t.Errorf("%s: fixture type id %q not mapped to uuid", s.Title, id)
			}
		}
		if deref(s.AuthorId) != e.teacher {
			t.Errorf("%s: author %v", s.Title, s.AuthorId)
		}
		if s.Status == "validated" {
			validated++
			if deref(s.ValidatedBy) != e.teacher || s.ValidatedAt == nil || deref(s.CallScript.Turns[0].TtsHash) == "" {
				t.Errorf("%s: validated fields %+v", s.Title, s)
			}
			if gaps := approveGaps(mustRow(t, e, s.Id)); len(gaps) > 0 {
				t.Errorf("%s: demo scenario incomplete: %v", s.Title, gaps)
			}
		}
	}
	if validated == 0 || len(e.q.byType(core.JobTTS)) == 0 {
		t.Errorf("validated %d tts jobs %d", validated, len(e.q.byType(core.JobTTS)))
	}

	// Повтор — ничего нового: сценарии узнаются по названию.
	jobs := len(e.q.jobs)
	if err := SeedDemoScenarios(e.ctx, e.seedDeps()); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*)::int FROM scenarios`); n != 4 || len(e.q.jobs) != jobs {
		t.Errorf("reseed: scenarios %d jobs %d->%d", n, jobs, len(e.q.jobs))
	}

	// Фикстура: сохранение редактора без правки формы АРМ — эталон тот же; правка описания —
	// новая версия, но службы фикстуры (их нет в форме АРМ) сохраняются.
	var fixture public.Scenario
	for _, s := range list {
		if s.Status == "validated" {
			fixture = s
			break
		}
	}
	path := "/scenarios/" + fixture.Id.String()
	turns := make([]map[string]any, 0, len(fixture.CallScript.Turns))
	for _, tr := range fixture.CallScript.Turns { // как toPatch фронта: без ttsHash и голоса
		turns = append(turns, map[string]any{"speaker": tr.Speaker, "text": tr.Text})
	}
	cs := map[string]any{"caller": map[string]any{"name": deref(fixture.CallScript.Caller.Name), "phone": deref(fixture.CallScript.Caller.Phone),
		"role": deref(fixture.CallScript.Caller.Role), "emotionalState": deref(fixture.CallScript.Caller.EmotionalState)},
		"address": fixture.CallScript.Address, "keyFacts": fixture.CallScript.KeyFacts, "turns": turns, "dialogue": fixture.CallScript.Dialogue}
	got := e.scenario(e.ok(e.do(http.MethodPatch, path, map[string]any{
		"title": fixture.Title + " (ред.)", "difficulty": fixture.Difficulty, "callScript": cs,
		"etalonDraft": fixture.EtalonDraft, "requiredFields": fixture.RequiredFields,
	}), http.StatusOK))
	if got.EtalonVersion != 1 || got.Status != "validated" {
		t.Errorf("title-only save of a fixture: v%d %s", got.EtalonVersion, got.Status)
	}
	for i, tr := range got.CallScript.Turns {
		if deref(tr.TtsHash) != deref(fixture.CallScript.Turns[i].TtsHash) {
			t.Errorf("turn %d hash changed", i)
		}
	}
	d := fixture.EtalonDraft
	d.Description += " Уточнение."
	got = e.scenario(e.ok(e.do(http.MethodPatch, path, map[string]any{"etalonDraft": d}), http.StatusOK))
	if got.EtalonVersion != 2 || !slices.Equal(deref(got.EtalonCard.ServicesToNotify), deref(fixture.EtalonCard.ServicesToNotify)) {
		t.Errorf("fixture edit: v%d services %v, want %v", got.EtalonVersion, deref(got.EtalonCard.ServicesToNotify), deref(fixture.EtalonCard.ServicesToNotify))
	}
}

func mustRow(t *testing.T, e *dbEnv, id uuid.UUID) *store.ScenarioRow {
	t.Helper()
	r, err := store.GetScenario(e.ctx, e.pool, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Регрессия (review): сид сериализуется advisory-блокировкой — пока её держит другая
// транзакция (второй экземпляр посреди сида), сид ждёт, а не вставляет копии.
func TestDBSeedDemoScenariosWaitsForLock(t *testing.T) {
	t.Parallel()
	e := newDBEnv(t)

	holder, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(e.ctx) //nolint:errcheck
	if _, err := holder.Exec(e.ctx, sqlSeedLock); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- SeedDemoScenarios(e.ctx, e.seedDeps()) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("seed finished while another seed held the lock (err=%v)", err)
		default:
		}
		var waiting bool
		if err := e.pool.QueryRow(e.ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND NOT granted)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("seed neither waited for the lock nor finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := holder.Rollback(e.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*)::int FROM scenarios`); n != 4 {
		t.Errorf("scenarios %d", n)
	}
}

func TestDBSeedDemoScenariosConcurrent(t *testing.T) {
	t.Parallel()
	e := newDBEnv(t)
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = SeedDemoScenarios(e.ctx, e.seedDeps())
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := e.count(`SELECT count(*)::int FROM scenarios`); n != 4 {
		t.Errorf("concurrent seeds produced %d scenarios, want 4", n)
	}
	if n := e.count(`SELECT count(DISTINCT title)::int FROM scenarios`); n != 4 {
		t.Errorf("distinct titles %d", n)
	}
}

func TestDBSeedWithoutClassifierFails(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	err := SeedDemoScenarios(context.Background(), Deps{Pool: pool, Log: discardLog()})
	if err == nil || !strings.Contains(err.Error(), "classifier") {
		t.Fatalf("err = %v, want a classifier hint", err)
	}
	if err := SeedDemoScenarios(context.Background(), Deps{}); err == nil {
		t.Fatal("nil pool must fail")
	}
}
