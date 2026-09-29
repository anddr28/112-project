package scenarios

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"lct/gocore/internal/gen/public"
)

// Экспорт → импорт: содержание сценариев переносится без потерь (новые id, статус draft,
// озвучка не переносится), категория и типы в пакете — коды классификатора.
func TestDBBundleRoundTrip(t *testing.T) {
	t.Parallel()
	e := newDBEnv(t)
	if err := SeedDemoScenarios(e.ctx, e.seedDeps()); err != nil {
		t.Fatal(err)
	}
	validated := e.count(`SELECT count(*) FROM scenarios WHERE status = 'validated'`)
	if validated < 2 {
		t.Fatalf("демо-сценариев мало: %d", validated)
	}

	r := e.ok(e.do(http.MethodGet, "/scenarios/export", nil), http.StatusOK)
	if cd := r.header.Get("Content-Disposition"); !strings.HasPrefix(cd, `attachment; filename="scenarios-`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	var bundle public.ScenarioBundle
	r.json(t, &bundle)
	if bundle.Format != public.Lct112ScenariosV1 || bundle.ExportedAt == nil || len(bundle.Scenarios) != validated {
		t.Fatalf("пакет: %s, %d из %d", bundle.Format, len(bundle.Scenarios), validated)
	}
	for _, it := range bundle.Scenarios {
		if _, err := uuid.Parse(it.CategoryId); err == nil {
			t.Errorf("%s: категория uuid, а не код (%s)", it.Title, it.CategoryId)
		}
		for _, id := range it.EtalonDraft.IncidentTypeIds {
			if _, err := uuid.Parse(id); err == nil {
				t.Errorf("%s: тип происшествия uuid в пакете", it.Title)
			}
		}
		for _, tr := range it.CallScript.Turns {
			if tr.TtsHash != nil {
				t.Errorf("%s: хэш озвучки в пакете", it.Title)
			}
		}
	}

	// импорт того же пакета
	r = e.ok(call(t, e.mux, http.MethodPost, "/scenarios/import", "admin", e.admin, bundle), http.StatusOK)
	var res public.ScenarioImportResult
	r.json(t, &res)
	if len(res.Created) != len(bundle.Scenarios) || len(res.Rejected) != 0 {
		t.Fatalf("импорт: created %d rejected %+v", len(res.Created), res.Rejected)
	}
	srcByTitle := map[string]public.Scenario{}
	var list []public.Scenario
	e.ok(e.do(http.MethodGet, "/scenarios", nil), http.StatusOK).json(t, &list)
	for _, s := range list {
		if s.Status == "validated" {
			srcByTitle[s.Title] = s
		}
	}
	for i, c := range res.Created {
		if c.Index != i || c.Title != bundle.Scenarios[i].Title {
			t.Errorf("created[%d] = %+v", i, c)
		}
		got, src := e.get(c.ScenarioId), srcByTitle[c.Title]
		if got.Status != "draft" || got.Id == src.Id || got.EtalonVersion != 1 || got.AuthorId == nil || *got.AuthorId != e.admin {
			t.Errorf("%s: status %s, version %d, author %v", c.Title, got.Status, got.EtalonVersion, got.AuthorId)
		}
		if got.CategoryId != src.CategoryId || got.Difficulty != src.Difficulty || got.Mode != src.Mode || got.Source != src.Source {
			t.Errorf("%s: шапка %s/%d/%s/%s vs %s/%d/%s/%s", c.Title, got.CategoryId, got.Difficulty, got.Mode, got.Source,
				src.CategoryId, src.Difficulty, src.Mode, src.Source)
		}
		if !slices.Equal(got.RequiredFields, src.RequiredFields) ||
			!slices.Equal(got.EtalonDraft.IncidentTypeIds, src.EtalonDraft.IncidentTypeIds) {
			t.Errorf("%s: эталон %v/%v vs %v/%v", c.Title, got.RequiredFields, got.EtalonDraft.IncidentTypeIds,
				src.RequiredFields, src.EtalonDraft.IncidentTypeIds)
		}
		for _, pair := range [][2]any{
			{stripHashes(got.CallScript), stripHashes(src.CallScript)},
			{got.EtalonCard, src.EtalonCard},
			{withoutServiceState(got.EtalonDraft), withoutServiceState(src.EtalonDraft)},
			{got.Scoring, src.Scoring},
			{got.ExpectedActions, src.ExpectedActions},
			{got.ExpectedDialogue, src.ExpectedDialogue},
			{got.NotesForTeacher, src.NotesForTeacher},
		} {
			a, _ := json.Marshal(pair[0])
			b, _ := json.Marshal(pair[1])
			if string(a) != string(b) {
				t.Errorf("%s: содержимое разошлось:\n got %s\nwant %s", c.Title, a, b)
			}
		}
	}
	if e.aud.count("scenario.import") != 1 || e.aud.count("scenario.export") != 1 {
		t.Errorf("аудит: %v", e.aud.actions())
	}
	if n := e.count(`SELECT count(*) FROM import_batches WHERE kind = 'scenarios' AND status = 'done'`); n != 1 {
		t.Errorf("import_batches = %d", n)
	}

	// выгрузка выбранных по ids — в порядке запроса
	first, second := res.Created[1].ScenarioId, res.Created[0].ScenarioId
	var sub public.ScenarioBundle
	e.ok(e.do(http.MethodGet, "/scenarios/export?ids="+first.String()+","+second.String()+","+uuid.NewString(), nil),
		http.StatusOK).json(t, &sub)
	if len(sub.Scenarios) != 2 || sub.Scenarios[0].Title != res.Created[1].Title {
		t.Errorf("выгрузка по ids: %d", len(sub.Scenarios))
	}
	expectError(t, e.do(http.MethodGet, "/scenarios/export?ids=abc", nil), http.StatusBadRequest, "validation", "ids")
}

func stripHashes(cs public.CallScript) public.CallScript {
	out := cs
	out.Turns = slices.Clone(cs.Turns)
	for i := range out.Turns {
		out.Turns[i].TtsHash = nil
	}
	return out
}

func withoutServiceState(d public.IncidentCardDraft) public.IncidentCardDraft {
	out := d
	out.Services = slices.Clone(d.Services)
	for i := range out.Services {
		out.Services[i].History, out.Services[i].AllowedNext = nil, nil
	}
	return out
}

// Импорт с частичным успехом: плохие элементы отклоняются с индексом и причиной.
func TestDBImportPartial(t *testing.T) {
	t.Parallel()
	e := newDBEnv(t)
	good := map[string]any{
		"title": "Пожар в гараже", "categoryId": "101", "difficulty": 2, "mode": "cards", "source": "ticket",
		"callScript":  callScript("Горит гараж!"),
		"etalonDraft": e.completeDraft("101"),
	}
	bundle := map[string]any{"format": "lct112.scenarios.v1", "scenarios": []any{
		good,
		map[string]any{"title": "Неизвестная категория", "categoryId": "999999", "difficulty": 1, "mode": "cards",
			"callScript": callScript("Алло"), "etalonDraft": e.completeDraft()},
		map[string]any{"title": "аб", "categoryId": "101", "difficulty": 1, "mode": "cards",
			"callScript": callScript("Алло"), "etalonDraft": e.completeDraft()},
		map[string]any{"title": "Плохой тип в эталоне", "categoryId": "101", "difficulty": 1, "mode": "voice",
			"callScript": callScript("Алло"), "etalonDraft": e.completeDraft()},
	}}
	var res public.ScenarioImportResult
	e.ok(e.do(http.MethodPost, "/scenarios/import", bundle), http.StatusOK).json(t, &res)
	if len(res.Created) != 1 || res.Created[0].Index != 0 || len(res.Rejected) != 3 {
		t.Fatalf("result %+v", res)
	}
	for i, rj := range res.Rejected {
		if rj.Index != i+1 || rj.Message == "" {
			t.Errorf("rejected[%d] = %+v", i, rj)
		}
	}
	if !strings.Contains(res.Rejected[0].Message, "категори") || !strings.Contains(res.Rejected[2].Message, "Режим") {
		t.Errorf("причины: %+v", res.Rejected)
	}
	sc := e.get(res.Created[0].ScenarioId)
	if sc.Source != "ticket" || sc.Status != "draft" || len(sc.EtalonDraft.Services) != 1 || sc.EtalonDraft.Services[0].Code != "101" {
		t.Errorf("импортированный: %+v", sc)
	}

	expectError(t, e.do(http.MethodPost, "/scenarios/import", map[string]any{"format": "other", "scenarios": []any{}}),
		http.StatusBadRequest, "validation", "format")
	// пустой пакет — пустой результат, массивы — []
	r := e.ok(e.do(http.MethodPost, "/scenarios/import", map[string]any{"format": "lct112.scenarios.v1", "scenarios": []any{}}), http.StatusOK)
	if s := strings.TrimSpace(string(r.body)); s != `{"created":[],"rejected":[]}` {
		t.Errorf("пустой импорт: %s", s)
	}
}

// ---------------------------------------------------------------- сценарий из карточки

// attemptFixture — оценённая попытка режима cards занятия преподавателя e.teacher.
func (e *dbEnv) attemptFixture(scenarioID uuid.UUID, status, mode string, dds bool) uuid.UUID {
	e.t.Helper()
	lesson := uuid.New()
	e.exec(`INSERT INTO lessons (id, title, teacher_id, created_by, mode, status) VALUES ($1, 'Занятие', $2, $2, $3, 'finished')`,
		lesson, e.teacher, mode)
	student := e.user("st"+lesson.String()[:8], "student")
	card, _ := json.Marshal(e.completeDraft("101"))
	id := uuid.New()
	var service *uuid.UUID
	if dds {
		var s uuid.UUID
		if err := e.pool.QueryRow(e.ctx, `SELECT id FROM services WHERE code = '101'`).Scan(&s); err != nil {
			e.t.Fatal(err)
		}
		service = &s
	}
	e.exec(`INSERT INTO attempts (id, lesson_id, user_id, scenario_id, etalon_id, mode, seq_no, status, time_limit_sec, card, service_id)
		SELECT $1, $2, $3, $4, et.id, $5, 1, $6, 30, $7, $8 FROM etalons et WHERE et.scenario_id = $4 AND et.is_current`,
		id, lesson, student, scenarioID, mode, status, card, service)
	e.exec(`INSERT INTO evaluations (attempt_id, etalon_id, status, total_score, verdict)
		SELECT $1, etalon_id, 'done', 82.5, 'pass' FROM attempts WHERE id = $1`, id)
	return id
}

func TestDBAttemptToScenario(t *testing.T) {
	t.Parallel()
	e := newDBEnv(t)
	src := e.create("Пожар на кухне", "101", "cards")
	e.makeComplete(src.Id)
	e.ok(e.do(http.MethodPost, "/scenarios/"+src.Id.String()+"/approve", nil), http.StatusOK)
	a := e.attemptFixture(src.Id, "evaluated", "cards", false)
	path := "/attempts/" + a.String() + "/to-scenario"

	// доступ: чужой преподаватель — 403, админ и студент — 403 (роль), нет попытки — 404
	other := e.user("teacher2", "teacher")
	expectError(t, call(t, e.mux, http.MethodPost, path, "teacher", other, nil), http.StatusForbidden, "forbidden")
	expectError(t, call(t, e.mux, http.MethodPost, path, "admin", e.admin, nil), http.StatusForbidden, "forbidden")
	expectError(t, e.do(http.MethodPost, "/attempts/"+uuid.NewString()+"/to-scenario", nil), http.StatusNotFound, "not_found")

	r := e.ok(e.do(http.MethodPost, path, nil), http.StatusCreated)
	sc := e.scenario(r)
	if sc.Source != "student" || sc.Status != "draft" || sc.Mode != "card_actions" || sc.CategoryId != src.CategoryId ||
		sc.EtalonVersion != 1 || !strings.Contains(sc.Title, "Пожар на кухне") {
		t.Fatalf("сценарий из карточки: %+v", sc)
	}
	if sc.EtalonDraft.Address.Raw != "Москва, Тверская улица, 12" || len(sc.EtalonDraft.Services) != 1 ||
		len(sc.EtalonDraft.Services[0].History) != 0 {
		t.Errorf("эталон — карточка обучающегося: %+v", sc.EtalonDraft)
	}
	if len(sc.CallScript.Turns) == 0 || sc.CallScript.Turns[0].Text != "Алло, у нас пожар!" {
		t.Errorf("легенда исходного сценария: %+v", sc.CallScript)
	}
	if got := e.count(`SELECT count(*) FROM scenarios WHERE source_attempt_id = $1`, a); got != 1 {
		t.Errorf("source_attempt_id: %d", got)
	}

	// повтор — 409 с details.scenarioId
	er := expectError(t, e.do(http.MethodPost, path, map[string]any{"title": "Другое название"}), http.StatusConflict, "conflict")
	if er.Details["scenarioId"] != sc.Id.String() {
		t.Errorf("details = %+v", er.Details)
	}

	// не оценена / ракурс ДДС / режим card_actions — 409
	for _, tc := range []struct {
		status, mode string
		dds          bool
	}{{"evaluating", "cards", false}, {"evaluated", "card_actions", true}, {"evaluated", "card_actions", false}} {
		x := e.attemptFixture(src.Id, tc.status, tc.mode, tc.dds)
		expectError(t, e.do(http.MethodPost, "/attempts/"+x.String()+"/to-scenario", nil), http.StatusConflict, "conflict")
	}

	// своё название; одновременные запросы — один 201, остальные 409 с тем же id
	b := e.attemptFixture(src.Id, "evaluated", "cards", false)
	var (
		wg    sync.WaitGroup
		codes = make([]int, 4)
		ids   = make([]string, 4)
	)
	for i := range codes {
		wg.Go(func() {
			rr := e.do(http.MethodPost, "/attempts/"+b.String()+"/to-scenario", map[string]any{"title": "Моя карточка"})
			codes[i] = rr.status
			var m map[string]any
			_ = json.Unmarshal(rr.body, &m)
			if rr.status == 201 {
				ids[i], _ = m["id"].(string)
			} else if d, ok := m["details"].(map[string]any); ok {
				ids[i], _ = d["scenarioId"].(string)
			}
		})
	}
	wg.Wait()
	created := 0
	for i, c := range codes {
		if c == 201 {
			created++
		} else if c != 409 {
			t.Errorf("параллельный запрос: %d", c)
		}
		if ids[i] == "" || ids[i] != ids[0] {
			t.Errorf("разные id: %v", ids)
		}
	}
	if created != 1 {
		t.Errorf("создано %d сценариев, codes %v", created, codes)
	}
	if e.get(uuid.MustParse(ids[0])).Title != "Моя карточка" {
		t.Error("своё название не применено")
	}
}

// ТЗ: администратор не может менять сценарии во время активного занятия — сценарий, стоящий
// в занятии, не правится и не отклоняется никем (только новой версией).
func TestDBAdminCannotChangeScenarioInRunningLesson(t *testing.T) {
	t.Parallel()
	e := newDBEnv(t)
	sc := e.create("Пожар в подъезде", "101", "cards")
	e.makeComplete(sc.Id)
	e.ok(e.do(http.MethodPost, "/scenarios/"+sc.Id.String()+"/approve", nil), http.StatusOK)
	lesson := uuid.New()
	e.exec(`INSERT INTO lessons (id, title, teacher_id, created_by, mode, status, started_at)
		VALUES ($1, 'Идущее занятие', $2, $2, 'cards', 'running', now())`, lesson, e.teacher)
	e.exec(`INSERT INTO lesson_scenarios (lesson_id, scenario_id) VALUES ($1, $2)`, lesson, sc.Id)

	as := func(method, path string, body any) testResp {
		return call(t, e.mux, method, path, "admin", e.admin, body)
	}
	er := expectError(t, as(http.MethodPatch, "/scenarios/"+sc.Id.String(), map[string]any{"title": "Правка админа"}),
		http.StatusConflict, "conflict")
	if er.Details["lessonsCount"] != float64(1) {
		t.Errorf("details %+v", er.Details)
	}
	expectError(t, as(http.MethodPatch, "/scenarios/"+sc.Id.String(), map[string]any{"etalonDraft": e.completeDraft("101")}),
		http.StatusConflict, "conflict")
	expectError(t, as(http.MethodPost, "/scenarios/"+sc.Id.String()+"/reject", map[string]any{"reason": "Не нравится"}),
		http.StatusConflict, "conflict")
	if got := e.get(sc.Id); got.Title != "Пожар в подъезде" || got.Status != "validated" || got.EtalonVersion != 2 {
		t.Errorf("сценарий изменён: %s %s v%d", got.Title, got.Status, got.EtalonVersion)
	}
	// новая версия — отдельный сценарий: исходный в занятии не меняется
	v := e.scenario(e.ok(as(http.MethodPost, "/scenarios/"+sc.Id.String()+"/versions", nil), http.StatusCreated))
	if v.Id == sc.Id || v.Status != "draft" {
		t.Errorf("версия %+v", v)
	}
}
