package scenarios

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/store"
)

// Пакетный импорт/экспорт сценариев (контракт v1.4, ScenarioBundle lct112.scenarios.v1;
// ТЗ: «механизм импорта обновлений в ручном режиме»).
//
// Переносимость между стендами: id классификатора — uuid, которые на другом стенде другие.
// Поэтому в пакете категория и типы происшествия эталона — **коды** классификатора, службы
// эталона — коды служб, хэши озвучки не выгружаются (голос и темп — настройки стенда,
// озвучка заново при approve). Импорт принимает и uuid, и id фикстуры, и код.

const (
	maxBundleItems = 500
	// maxBundleBody — 500 сценариев с легендой, брифом и эталоном; обычный лимит JSON (1 МБ)
	// мал для пакета.
	maxBundleBody = 16 << 20
)

// ---------------------------------------------------------------- экспорт

// handleExport — GET /scenarios/export?ids=a,b,c. Без ids — все подтверждённые (новые
// сверху, не больше 500). Неизвестные id пропускаются: пакет — выгрузка того, что есть.
func (s *Service) handleExport(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	var (
		rows []store.ScenarioRow
		err  error
	)
	if raw := strings.TrimSpace(r.URL.Query().Get("ids")); raw != "" {
		list, herr := parseIDList(raw)
		if herr != nil {
			return herr
		}
		rows, err = store.GetScenarios(ctx, s.pool, list)
	} else {
		rows, err = store.ListScenarios(ctx, s.pool, store.ScenarioFilter{Status: core.ScenarioValidated, Limit: maxBundleItems})
	}
	if err != nil {
		return err
	}
	now := time.Now().UTC().Truncate(time.Second)
	bundle := public.ScenarioBundle{Format: public.Lct112ScenariosV1, ExportedAt: &now,
		Scenarios: make([]public.ScenarioBundleItem, 0, len(rows))}
	for i := range rows {
		bundle.Scenarios = append(bundle.Scenarios, s.bundleItem(&rows[i]))
	}
	w.Header().Set("Content-Disposition", `attachment; filename="scenarios-`+now.Format("20060102-150405")+`.json"`)
	w.Header().Set("Cache-Control", "no-store")
	s.audit(ctx, "scenario.export", "scenario", uuid.Nil, nil, map[string]any{"count": len(rows)})
	httpx.WriteJSON(w, http.StatusOK, bundle)
	return nil
}

// parseIDList — "uuid,uuid" (без повторов, не больше 500); ошибки — 400 по полю ids.
func parseIDList(raw string) ([]uuid.UUID, *httpx.Error) {
	parts := strings.Split(raw, ",")
	if len(parts) > maxBundleItems {
		return nil, httpx.Validation("Слишком много сценариев в выгрузке", map[string]string{"ids": "Не больше 500"})
	}
	out := make([]uuid.UUID, 0, len(parts))
	seen := make(map[uuid.UUID]bool, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id, ok := ids.Parse(p)
		if !ok {
			return nil, httpx.Validation("Некорректный список сценариев", map[string]string{"ids": "Ожидаются UUID через запятую"})
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

// bundleItem — переносимое содержание сценария (без id, статусов, авторов и озвучки).
func (s *Service) bundleItem(r *store.ScenarioRow) public.ScenarioBundleItem {
	cs := convert.CallScriptToPublic(&r.CallScript)
	for i := range cs.Turns {
		cs.Turns[i].TtsHash = nil
	}
	cat := r.CategoryCode
	if cat == "" {
		cat = r.CategoryID.String()
	}
	src := public.ScenarioSource(r.Source)
	it := public.ScenarioBundleItem{
		Title:           r.Title,
		CategoryId:      cat,
		Difficulty:      public.Difficulty(r.Difficulty),
		Mode:            public.ScenarioBundleItemMode(r.Mode),
		Source:          &src,
		CallScript:      cs,
		NotesForTeacher: convert.NonEmpty(r.GenerationMeta.NotesForTeacher),
	}
	e := r.Etalon
	if e == nil {
		it.EtalonDraft = convert.EmptyDraft()
		return it
	}
	card := convert.IncidentCardToPublic(&e.Card)
	it.EtalonCard = &card
	it.EtalonDraft = s.portableDraft(e.CardDraft)
	rf := e.RequiredFields()
	it.RequiredFields = &rf
	if !e.Scoring.IsZero() {
		sc := convert.ScoringToPublic(&e.Scoring)
		it.Scoring = &sc
	}
	if len(e.ExpectedActions) > 0 {
		a := convert.ExpectedActionsToPublic(e.ExpectedActions)
		it.ExpectedActions = &a
	}
	if e.ExpectedDialogue != nil {
		d := convert.ExpectedDialogueToPublic(e.ExpectedDialogue)
		it.ExpectedDialogue = &d
	}
	return it
}

// portableDraft — эталонная форма АРМ для пакета: типы происшествия — коды классификатора,
// у служб — только справочные поля (история статусов эталону не нужна).
func (s *Service) portableDraft(d public.IncidentCardDraft) public.IncidentCardDraft {
	out := d
	out.IncidentTypeIds = make([]string, 0, len(d.IncidentTypeIds))
	for _, id := range d.IncidentTypeIds {
		if s.cat != nil {
			if t, ok := s.cat.TypeByID(id); ok && t.Code != "" {
				id = t.Code
			}
		}
		out.IncidentTypeIds = append(out.IncidentTypeIds, id)
	}
	out.Services = make([]public.AssignedService, len(d.Services))
	for i, sv := range d.Services {
		sv.History, sv.AllowedNext = []public.ReactionStatusEntry{}, []public.AllowedTransition{}
		out.Services[i] = sv
	}
	return out
}

// ---------------------------------------------------------------- импорт

type importCreated = struct {
	Index      int       `json:"index"`
	ScenarioId uuid.UUID `json:"scenarioId"`
	Title      string    `json:"title"`
}

type importRejected = struct {
	Index   int     `json:"index"`
	Message string  `json:"message"`
	Title   *string `json:"title,omitempty"`
}

// handleImport — POST /scenarios/import. Каждый сценарий — новый (status=draft, эталон v1,
// автор — импортирующий); элементы проверяются и записываются независимо: ошибка одного не
// отменяет остальные (каждый — один атомарный оператор INSERT … WITH). Аудит scenario.import
// и строка import_batches (история импортов, db-design §1).
func (s *Service) handleImport(w http.ResponseWriter, r *http.Request) error {
	var in public.ScenarioBundle
	if err := httpx.ReadJSONLimit(r, &in, maxBundleBody); err != nil {
		return err
	}
	switch {
	case in.Format != public.Lct112ScenariosV1:
		return httpx.Validation("Неизвестный формат пакета", map[string]string{"format": "Ожидается lct112.scenarios.v1"})
	case len(in.Scenarios) > maxBundleItems:
		return httpx.Validation("Слишком большой пакет", map[string]string{"scenarios": "Не больше 500 сценариев"})
	}
	ctx := r.Context()
	p := core.PrincipalFrom(ctx)
	res := public.ScenarioImportResult{Created: []importCreated{}, Rejected: []importRejected{}}
	reject := func(i int, title, msg string) {
		rj := importRejected{Index: i, Message: msg}
		if t := strings.TrimSpace(title); t != "" {
			rj.Title = &t
		}
		res.Rejected = append(res.Rejected, rj)
	}
	for i := range in.Scenarios {
		item := &in.Scenarios[i]
		prep, msg := s.prepareImport(item, i)
		if msg != "" {
			reject(i, item.Title, msg)
			continue
		}
		id, err := s.insertImported(ctx, prep, p.UserID)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if he := httpx.AsError(err); he.Status < 500 {
				reject(i, item.Title, he.Message)
				continue
			}
			s.log.Error("scenario import: insert", "index", i, "err", err)
			reject(i, item.Title, "Не удалось сохранить сценарий")
			continue
		}
		res.Created = append(res.Created, importCreated{Index: i, ScenarioId: id, Title: prep.title})
	}

	stats := map[string]any{"total": len(in.Scenarios), "created": len(res.Created), "rejected": len(res.Rejected)}
	batchID := ids.New()
	status := "done"
	if len(res.Created) == 0 && len(res.Rejected) > 0 {
		status = "failed"
	}
	// История импорта — вне основного пути: ошибка записи журнала результат не отменяет.
	if _, err := s.pool.Exec(ctx, sqlImportBatch, batchID, status, stats, p.UserID); err != nil {
		s.log.Warn("scenario import: import_batches", "err", err)
	}
	created := make([]string, 0, len(res.Created))
	for _, c := range res.Created {
		created = append(created, c.ScenarioId.String())
	}
	s.audit(ctx, "scenario.import", "import_batch", batchID, nil, map[string]any{
		"total": len(in.Scenarios), "created": created, "rejected": len(res.Rejected),
	})
	httpx.WriteJSON(w, http.StatusOK, res)
	return nil
}

const sqlImportBatch = `
INSERT INTO import_batches (id, kind, file_name, status, stats, created_by, finished_at)
VALUES ($1, 'scenarios', 'scenario-bundle.json', $2, $3, $4, now())`

// imported — проверенный элемент пакета, готовый к записи.
type imported struct {
	title, mode, source string
	categoryID          uuid.UUID
	difficulty          int
	callScript          []byte
	etalon              etalonJSON
	meta                []byte
}

var validSources = map[string]bool{"generated": true, "manual": true, "ticket": true, "student": true}

// prepareImport — проверка и нормализация элемента; msg != "" — причина отказа (по-русски).
func (s *Service) prepareImport(it *public.ScenarioBundleItem, index int) (*imported, string) {
	title := strings.TrimSpace(it.Title)
	if msg := checkTitle(title); msg != "" {
		return nil, msg
	}
	info, catID, ok := s.resolveCategory(it.CategoryId)
	if !ok {
		return nil, "Неизвестная категория классификатора «" + strings.TrimSpace(it.CategoryId) + "»"
	}
	if !validDifficulty(int(it.Difficulty)) {
		return nil, "Сложность — 1, 2 или 3"
	}
	if !validMode(string(it.Mode)) {
		return nil, "Режим — cards, card_actions или both"
	}
	source := "manual"
	if it.Source != nil && *it.Source != "" {
		if !validSources[string(*it.Source)] {
			return nil, "Неизвестный источник сценария «" + string(*it.Source) + "»"
		}
		source = string(*it.Source)
	}

	// Легенда: хэши озвучки не переносятся (approve озвучит заново голосом этого стенда).
	patch := public.ScenarioPatch{CallScript: &it.CallScript, RequiredFields: it.RequiredFields,
		ExpectedDialogue: it.ExpectedDialogue, Scoring: it.Scoring}
	if fields := validatePatch(&patch); len(fields) > 0 {
		return nil, "Проверьте сценарий: " + joinFields(fields)
	}
	cs := convert.CallScriptFromPublic(&it.CallScript)
	sanitizeBrief(cs.Dialogue)
	for i := range cs.Turns {
		cs.Turns[i].TTSHash = ""
	}
	cs.Normalize()
	if len(cs.Turns) > maxListItems || (cs.Dialogue != nil && len(cs.Dialogue.Facts) > maxListItems) {
		return nil, "Слишком много реплик или фактов в легенде"
	}

	// Эталон: типы происшествия — uuid/код/id фикстуры → uuid этого стенда; службы — по коду.
	d := it.EtalonDraft
	convert.NormalizeDraft(&d)
	types := make([]string, 0, len(d.IncidentTypeIds))
	for _, key := range d.IncidentTypeIds {
		t, _, ok := s.resolveCategory(key)
		if !ok {
			return nil, "Неизвестный тип происшествия в эталоне «" + key + "»"
		}
		types = append(types, t.ID)
	}
	if len(types) == 0 {
		types = append(types, info.ID)
	}
	d.IncidentTypeIds = types
	d.Services = s.seedServices(d.Services)
	for i := range d.Services {
		d.Services[i].History, d.Services[i].AllowedNext = []public.ReactionStatusEntry{}, []public.AllowedTransition{}
	}

	e := etalonData{Draft: d, Actions: []model.ExpectedAction{}}
	if it.EtalonCard != nil {
		e.Card = convert.IncidentCardFromPublic(it.EtalonCard)
	} else {
		e.Card = convert.DraftToCard(&d, s.cat)
	}
	switch {
	case it.Scoring != nil:
		e.Scoring = convert.ScoringFromPublic(it.Scoring, it.RequiredFields)
	case it.RequiredFields != nil:
		e.Scoring = model.Scoring{RequiredFields: cleanStrings(*it.RequiredFields)}
	}
	if len(e.Scoring.RequiredFields) == 0 {
		e.Scoring.RequiredFields = model.DefaultRequiredFields()
	}
	if it.ExpectedActions != nil {
		if len(*it.ExpectedActions) > maxListItems {
			return nil, "Слишком много эталонных действий"
		}
		e.Actions = convert.ExpectedActionsFromPublic(*it.ExpectedActions)
	}
	if it.ExpectedDialogue != nil {
		dlg := convert.ExpectedDialogueFromPublic(it.ExpectedDialogue)
		sanitizeChecklist(&dlg)
		e.Dialogue = &dlg
	}
	enc, err := e.encode()
	if err != nil {
		return nil, "Эталон не удалось сохранить"
	}
	csJSON, err := json.Marshal(&cs)
	if err != nil {
		return nil, "Легенду не удалось сохранить"
	}
	meta := model.GenerationMeta{Extra: map[string]any{"imported": true, "import_index": index}}
	if it.NotesForTeacher != nil {
		meta.NotesForTeacher = strings.TrimSpace(*it.NotesForTeacher)
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, "Метаданные не удалось сохранить"
	}
	return &imported{title: title, mode: string(it.Mode), source: source, categoryID: catID,
		difficulty: int(it.Difficulty), callScript: csJSON, etalon: enc, meta: metaJSON}, ""
}

func (s *Service) insertImported(ctx context.Context, in *imported, author uuid.UUID) (uuid.UUID, error) {
	id := ids.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := s.pool.Exec(ctx, sqlInsertScenarioWithEtalon,
		id, in.title, in.categoryID, in.difficulty, in.mode, in.source, core.ScenarioDraft, in.callScript, author,
		nil, nil, nil, in.meta, now,
		ids.New(), in.etalon.card, in.etalon.draft, in.etalon.scoring, in.etalon.actions, in.etalon.dialogue); err != nil {
		if pg.IsCheckViolation(err) || pg.IsForeignKeyViolation(err) {
			return uuid.Nil, httpx.BadRequest("Сценарий не прошёл проверку базы данных").Wrap(err)
		}
		return uuid.Nil, fmt.Errorf("scenarios: import insert: %w", err)
	}
	return id, nil
}

// joinFields — «поле: сообщение; …» в стабильном порядке (для сообщения об отказе элемента).
func joinFields(fields map[string]string) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+fields[k])
	}
	return strings.Join(parts, "; ")
}
