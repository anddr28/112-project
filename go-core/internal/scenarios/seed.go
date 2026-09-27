package scenarios

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/settings"
)

// Демо-сценарии стенда — дословный перенос фикстур фронта (fixtures/scenarios.ts +
// бриф и чек-лист из fixtures/dialogue.ts), выгруженный из самих TS-фикстур: легенда,
// ключевые факты, реплики, эталон (форма АРМ и контрактная карточка), обязательные поля,
// бриф заявителя и чек-лист разговора. Занятия-демо (lessons) ссылаются на них по названию.
//
//go:embed data/demo_scenarios.json
var demoScenariosJSON []byte

// DemoTeacherLogin — автор и валидатор демо-сценариев (учётку заводит auth.SeedDemoUsers).
const DemoTeacherLogin = "teacher"

type demoFile struct {
	Scenarios []demoScenario `json:"scenarios"`
}

type demoScenario struct {
	FixtureID        string                   `json:"fixtureId"`
	Title            string                   `json:"title"`
	CategoryID       string                   `json:"categoryId"`   // id фикстуры классификатора ("it-101")
	CategoryCode     string                   `json:"categoryCode"` // код классификатора ("101")
	Difficulty       int                      `json:"difficulty"`
	Mode             string                   `json:"mode"`
	Source           string                   `json:"source"`
	Status           string                   `json:"status"`
	CreatedAt        time.Time                `json:"createdAt"`
	ValidatedAt      *time.Time               `json:"validatedAt"`
	NotesForTeacher  string                   `json:"notesForTeacher"`
	CallScript       public.CallScript        `json:"callScript"`
	EtalonCard       public.IncidentCard      `json:"etalonCard"`
	EtalonDraft      json.RawMessage          `json:"etalonDraft"`
	RequiredFields   []string                 `json:"requiredFields"`
	ExpectedDialogue *public.ExpectedDialogue `json:"expectedDialogue"`
	// ExpectedActions — действия диспетчера ДДС по карточке: по их required_facts оценивается
	// смысл текста действия в ракурсе ДДС (факты звонка для этого не годятся).
	ExpectedActions []public.ExpectedAction `json:"expectedActions"`
}

// SeedDemoScenarios — идемпотентный сид демо-сценариев (по названию: существующий —
// в том числе переименованный в архив или правленый преподавателем — не трогается).
// Порядок сидов: classifier.SeedReference → auth.SeedDemoUsers → SeedDemoScenarios →
// lessons. Типы классификатора ищутся SQL-запросом (id фикстуры «it-101» → uuid), поэтому
// сид не зависит от того, успел ли Catalog перечитаться. Подтверждённым сценариям
// проставляются tts_hash и ставятся TTS-задачи (как при approve; без Queue — только хэши).
// Всё — одной транзакцией.
func SeedDemoScenarios(ctx context.Context, d Deps) error {
	if d.Pool == nil {
		return fmt.Errorf("scenarios: seed: pool is required")
	}
	var file demoFile
	if err := json.Unmarshal(demoScenariosJSON, &file); err != nil {
		return fmt.Errorf("scenarios: seed: decode embedded data: %w", err)
	}
	s := New(d)
	snap := s.snap(ctx)

	codes := make([]string, 0, len(file.Scenarios))
	fixtureIDs := make([]string, 0, len(file.Scenarios))
	titles := make([]string, 0, len(file.Scenarios))
	for i := range file.Scenarios {
		ds := &file.Scenarios[i]
		codes = append(codes, ds.CategoryCode)
		fixtureIDs = append(fixtureIDs, ds.CategoryID)
		titles = append(titles, ds.Title)
		for _, t := range draftTypeKeys(ds.EtalonDraft) {
			fixtureIDs = append(fixtureIDs, t)
			codes = append(codes, t)
		}
	}

	created := 0
	err := pg.WithTx(ctx, d.Pool, func(ctx context.Context, tx pgx.Tx) error {
		// Два одновременных сида (SEED_ON_START у двух экземпляров, `gocore seed --demo`
		// при старте сервера) сериализуются: проверка «уже есть» идёт отдельным оператором
		// после блокировки и видит всё, что закоммитил первый.
		if _, err := tx.Exec(ctx, sqlSeedLock); err != nil {
			return fmt.Errorf("scenarios: seed lock: %w", err)
		}
		teacher, cats, existing, err := seedLookups(ctx, tx, codes, fixtureIDs, titles)
		if err != nil {
			return err
		}
		if teacher == nil {
			s.log.Warn("seed scenarios: demo teacher not found; author left empty", "login", DemoTeacherLogin)
		}
		for i := range file.Scenarios {
			ds := &file.Scenarios[i]
			if existing[ds.Title] {
				continue
			}
			if err := s.seedOne(ctx, tx, ds, teacher, cats, snap); err != nil {
				return fmt.Errorf("scenarios: seed %s: %w", ds.FixtureID, err)
			}
			created++
		}
		return nil
	})
	if err != nil {
		return err
	}
	if created > 0 {
		s.log.Info("demo scenarios seeded", "created", created)
	}
	return nil
}

// seedLookups — учётка преподавателя, типы классификатора и уже существующие названия
// одним batch (один round-trip).
func seedLookups(ctx context.Context, tx pgx.Tx, codes, fixtureIDs, titles []string) (*uuid.UUID, map[string]uuid.UUID, map[string]bool, error) {
	b := &pgx.Batch{}
	b.Queue(sqlSeedTeacher, DemoTeacherLogin)
	b.Queue(sqlSeedCategories, codes, fixtureIDs)
	b.Queue(sqlSeedExisting, titles)
	br := tx.SendBatch(ctx, b)
	defer br.Close()

	var teacher *uuid.UUID
	var tid uuid.UUID
	switch err := br.QueryRow().Scan(&tid); {
	case err == nil:
		teacher = &tid
	case !pg.IsNoRows(err):
		return nil, nil, nil, fmt.Errorf("scenarios: seed teacher: %w", err)
	}

	cats := make(map[string]uuid.UUID, len(codes)*2) // и код, и id фикстуры -> uuid
	rows, err := br.Query()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("scenarios: seed categories: %w", err)
	}
	for rows.Next() {
		var (
			id            uuid.UUID
			code, fixture string
		)
		if err := rows.Scan(&id, &code, &fixture); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		cats[code] = id
		if fixture != "" {
			cats[fixture] = id
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, nil, err
	}

	existing := make(map[string]bool, len(titles))
	rows, err = br.Query()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("scenarios: seed existing: %w", err)
	}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		existing[t] = true
	}
	rows.Close()
	return teacher, cats, existing, rows.Err()
}

// seedOne — сценарий и эталон v1 одним оператором, затем озвучка (для подтверждённых).
func (s *Service) seedOne(ctx context.Context, tx pgx.Tx, ds *demoScenario, teacher *uuid.UUID, cats map[string]uuid.UUID, snap *settings.Snapshot) error {
	catID, ok := cats[ds.CategoryCode]
	if !ok {
		if catID, ok = cats[ds.CategoryID]; !ok {
			return fmt.Errorf("classifier type %q (%s) not found — run classifier seed first", ds.CategoryCode, ds.CategoryID)
		}
	}

	// Форма АРМ: пустая карточка + то, что задала фикстура (etalon() фронта — тот же
	// поверхностный merge поверх emptyCard()); id типов фикстур -> uuid БД.
	draft := convert.EmptyDraft()
	if len(ds.EtalonDraft) > 0 {
		if err := json.Unmarshal(ds.EtalonDraft, &draft); err != nil {
			return fmt.Errorf("decode etalonDraft: %w", err)
		}
	}
	convert.NormalizeDraft(&draft)
	typeIDs := make([]string, 0, len(draft.IncidentTypeIds))
	for _, key := range draft.IncidentTypeIds {
		if id, ok := cats[key]; ok {
			typeIDs = append(typeIDs, id.String())
		} else {
			s.log.Warn("seed scenarios: unknown incident type in etalon skipped", "scenario", ds.FixtureID, "type", key)
		}
	}
	draft.IncidentTypeIds = typeIDs
	draft.Services = s.seedServices(draft.Services)

	cs := convert.CallScriptFromPublic(&ds.CallScript)
	sanitizeBrief(cs.Dialogue)
	cs.Normalize()

	status := ds.Status
	if !validStatus(status) {
		status = core.ScenarioDraft
	}
	var plan ttsPlan
	if status == core.ScenarioValidated {
		plan, _ = assignHashes(&cs, snap)
	}

	e := etalonData{
		Card:    convert.IncidentCardFromPublic(&ds.EtalonCard),
		Draft:   draft,
		Scoring: model.Scoring{RequiredFields: cleanStrings(ds.RequiredFields)},
		Actions: convert.ExpectedActionsFromPublic(ds.ExpectedActions),
	}
	if len(e.Actions) > 0 {
		// Оператор 112 оценивается по фактам звонка: без явного required_facts запасной путь
		// смыслового слоя дошёл бы до expected_actions (действий ДДС). Фиксируем те же факты,
		// что и при генерации (requiredFacts) — для 112 набор фактов не меняется.
		e.Scoring.RequiredFacts = requiredFacts(&cs)
	}
	if ds.ExpectedDialogue != nil {
		dlg := convert.ExpectedDialogueFromPublic(ds.ExpectedDialogue)
		sanitizeChecklist(&dlg)
		e.Dialogue = &dlg
	}
	enc, err := e.encode()
	if err != nil {
		return err
	}
	csJSON, err := json.Marshal(&cs)
	if err != nil {
		return err
	}
	meta := model.GenerationMeta{
		NotesForTeacher: strings.TrimSpace(ds.NotesForTeacher),
		Extra:           map[string]any{"fixture_id": ds.FixtureID},
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	createdAt := ds.CreatedAt.UTC()
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	var validatedBy *uuid.UUID
	var validatedAt *time.Time
	if status == core.ScenarioValidated {
		validatedBy = teacher
		at := createdAt
		if ds.ValidatedAt != nil {
			at = ds.ValidatedAt.UTC()
		}
		validatedAt = &at
	}
	source := ds.Source
	switch source {
	case "generated", "manual", "ticket", "student":
	default:
		source = "manual"
	}
	mode := ds.Mode
	if !validMode(mode) {
		mode = core.ModeCards
	}
	difficulty := min(max(ds.Difficulty, 1), 3)

	id := ids.New()
	if _, err := tx.Exec(ctx, sqlInsertScenarioWithEtalon,
		id, strings.TrimSpace(ds.Title), catID, difficulty, mode, source, status, csJSON, teacher,
		validatedBy, validatedAt, nil, metaJSON, createdAt,
		ids.New(), enc.card, enc.draft, enc.scoring, enc.actions, enc.dialogue); err != nil {
		return err
	}
	if status == core.ScenarioValidated {
		if _, err := s.enqueueMissing(ctx, tx, id, plan, core.PriorityTTS); err != nil {
			return err
		}
	}
	return nil
}

// seedServices — службы эталона фикстуры: id фикстуры "svc-101" / код -> служба справочника
// (неизвестные отбрасываются). В текущих фикстурах эталоны без служб — службы эталона
// живут в etalonCard.servicesToNotify; код оставлен на случай их появления.
func (s *Service) seedServices(in []public.AssignedService) []public.AssignedService {
	if len(in) == 0 || s.cat == nil {
		return []public.AssignedService{}
	}
	out := make([]public.AssignedService, 0, len(in))
	for _, sv := range in {
		code := strings.TrimSpace(sv.Code)
		if code == "" {
			code = strings.TrimPrefix(strings.TrimSpace(sv.ServiceId), "svc-")
		}
		info, ok := s.cat.ServiceByCode(code)
		if !ok {
			continue
		}
		sv.ServiceId, sv.Code, sv.Name, sv.ShortName = info.ID, info.Code, info.Name, info.ShortName
		out = append(out, sv)
	}
	return out
}

// draftTypeKeys — id типов из etalonDraft фикстуры (для одного запроса к классификатору).
func draftTypeKeys(raw json.RawMessage) []string {
	var probe struct {
		IncidentTypeIds []string `json:"incidentTypeIds"`
	}
	_ = json.Unmarshal(raw, &probe)
	return probe.IncidentTypeIds
}
