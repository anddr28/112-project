package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/pg"
)

// ScenarioRow — сценарий со всем, что нужно public.Scenario, одной строкой:
// категория, текущий эталон, число занятий и готовность озвучки.
type ScenarioRow struct {
	ID             uuid.UUID
	Title          string
	CategoryID     uuid.UUID
	CategoryName   string
	CategoryCode   string
	Difficulty     int
	Mode           string // cards | card_actions | both
	Source         string // generated | manual | ticket | student
	Status         string // draft | generated | validated | rejected | archived
	CallScript     model.CallScript
	AuthorID       *uuid.UUID
	ValidatedBy    *uuid.UUID
	ValidatedAt    *time.Time
	TeacherComment *string
	GenerationMeta model.GenerationMeta
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ArchivedAt     *time.Time
	ParentID       *uuid.UUID
	Version        int
	Etalon         *EtalonRow // текущая версия эталона; nil — эталона нет
	LessonsCount   int        // в скольких занятиях стоит (lesson_scenarios)
	TTSReady       bool       // у каждой реплики заявителя есть tts_hash и строка tts_cache
}

// InUse — сценарий стоит хотя бы в одном занятии (правка только новой версией).
func (r *ScenarioRow) InUse() bool { return r.LessonsCount > 0 }

// EtalonRow — версия эталона (etalons).
type EtalonRow struct {
	ID               uuid.UUID
	ScenarioID       uuid.UUID
	Version          int
	IsCurrent        bool
	Card             components.IncidentCard  // etalons.card (snake) — уходит в ai-service
	CardDraft        public.IncidentCardDraft // etalons.card_draft (camelCase); '{}' -> EmptyDraft
	CardDraftRaw     json.RawMessage          // card_draft как в БД — для сверки без перекодирования
	Scoring          model.Scoring
	ExpectedActions  []model.ExpectedAction
	ExpectedDialogue *model.ExpectedDialogue // nil — чек-листа нет ('{}')
	CreatedBy        *uuid.UUID
	CreatedAt        time.Time
}

// RequiredFields — пути обязательных полей (не nil).
func (e *EtalonRow) RequiredFields() []string {
	if e == nil || e.Scoring.RequiredFields == nil {
		return []string{}
	}
	return e.Scoring.RequiredFields
}

// ScenarioFilter — фильтры списка. Mode cards|card_actions — сценарии, пригодные для
// режима (включая both); both — только both. Status=archived включает архив сам.
type ScenarioFilter struct {
	Status          string
	CategoryID      *uuid.UUID
	Mode            string
	IncludeArchived bool
	// Страница: Limit строк после курсора After (нулевой — первая; Limit <= 0 — MaxListLimit).
	Limit int
	After TimeKey
}

// Key — курсор этой строки в списке сценариев.
func (r *ScenarioRow) Key() TimeKey { return TimeKey{At: r.CreatedAt, ID: r.ID} }

// tts_ready: реплики заявителя берутся из call_script.turns; пустой набор -> false
// (bool_and по нулю строк = NULL). Проверка tts_cache — по PK, дёшево.
const scenarioColumns = `
SELECT s.id, s.title, s.category_id, c.name, c.code, s.difficulty, s.mode, s.source, s.status,
       s.call_script, s.author_id, s.validated_by, s.validated_at, s.teacher_comment, s.generation_meta,
       s.created_at, s.updated_at, s.archived_at, s.parent_id, s.version,
       e.id, e.version, e.card, e.card_draft, e.scoring, e.expected_actions, e.expected_dialogue,
       e.created_by, e.created_at,
       (SELECT count(*)::int FROM lesson_scenarios ls WHERE ls.scenario_id = s.id),
       COALESCE((SELECT bool_and(COALESCE(t->>'tts_hash', '') <> ''
                                 AND EXISTS (SELECT 1 FROM tts_cache tc WHERE tc.text_hash = t->>'tts_hash'))
                   FROM jsonb_array_elements(CASE WHEN jsonb_typeof(s.call_script->'turns') = 'array'
                                                  THEN s.call_script->'turns' ELSE '[]'::jsonb END) AS t
                  WHERE t->>'speaker' = 'caller'), false)
  FROM scenarios s
  JOIN classifier_categories c ON c.id = s.category_id
  LEFT JOIN etalons e ON e.scenario_id = s.id AND e.is_current`

const sqlGetScenario = scenarioColumns + `
 WHERE s.id = $1`

const sqlListScenarios = scenarioColumns + `
 WHERE ($1::text IS NULL OR s.status = $1)
   AND ($2::uuid IS NULL OR s.category_id = $2)
   AND ($3::text IS NULL OR s.mode = $3 OR ($3 <> 'both' AND s.mode = 'both'))
   AND ($4::bool OR (s.archived_at IS NULL AND s.status <> 'archived'))
   AND (s.created_at, s.id) < ($5::timestamptz, $6::uuid)
 ORDER BY s.created_at DESC, s.id DESC
 LIMIT $7`

// sqlGetScenarios — несколько сценариев одним запросом в порядке списка id (выгрузка пакета).
const sqlGetScenarios = scenarioColumns + `
 WHERE s.id = ANY($1::uuid[])
 ORDER BY array_position($1::uuid[], s.id)`

// GetScenarios — сценарии по списку id (одним запросом, в порядке ids; неизвестные пропущены).
func GetScenarios(ctx context.Context, q pg.Querier, ids []uuid.UUID) ([]ScenarioRow, error) {
	rows, err := q.Query(ctx, sqlGetScenarios, ids)
	if err != nil {
		return nil, fmt.Errorf("store: get scenarios: %w", err)
	}
	defer rows.Close()
	out := make([]ScenarioRow, 0, len(ids))
	for rows.Next() {
		out = append(out, ScenarioRow{})
		if err := scanScenario(rows, &out[len(out)-1]); err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: get scenarios: %w", err)
	}
	return out, nil
}

// GetScenario — сценарий по id (pgx.ErrNoRows, если нет).
func GetScenario(ctx context.Context, q pg.Querier, id uuid.UUID) (*ScenarioRow, error) {
	r := new(ScenarioRow)
	if err := scanScenario(q.QueryRow(ctx, sqlGetScenario, id), r); err != nil {
		return nil, err
	}
	return r, nil
}

// ListScenarios — страница списка, новые сверху (keyset по scenarios_created_idx).
func ListScenarios(ctx context.Context, q pg.Querier, f ScenarioFilter) ([]ScenarioRow, error) {
	includeArchived := f.IncludeArchived || f.Status == "archived"
	at, id := f.After.args()
	rows, err := q.Query(ctx, sqlListScenarios, nullStr(f.Status), f.CategoryID, nullStr(f.Mode), includeArchived,
		at, id, limitOr(f.Limit))
	if err != nil {
		return nil, fmt.Errorf("store: list scenarios: %w", err)
	}
	defer rows.Close()
	out := make([]ScenarioRow, 0, 32)
	for rows.Next() {
		out = append(out, ScenarioRow{})
		if err := scanScenario(rows, &out[len(out)-1]); err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list scenarios: %w", err)
	}
	return out, nil
}

func scanScenario(row pgx.Row, r *ScenarioRow) error {
	var (
		e        EtalonRow
		eID      *uuid.UUID
		eVersion *int
		eCreated *time.Time
		eScan    = etalonScanners(&e)
	)
	err := row.Scan(
		&r.ID, &r.Title, &r.CategoryID, &r.CategoryName, &r.CategoryCode, &r.Difficulty, &r.Mode, &r.Source, &r.Status,
		&jsonScan{dst: &r.CallScript, name: "scenarios.call_script"},
		&r.AuthorID, &r.ValidatedBy, &r.ValidatedAt, &r.TeacherComment,
		&jsonScan{dst: &r.GenerationMeta, name: "scenarios.generation_meta"},
		&r.CreatedAt, &r.UpdatedAt, &r.ArchivedAt, &r.ParentID, &r.Version,
		&eID, &eVersion, eScan.card, eScan.draft, eScan.scoring, eScan.actions, eScan.dialogue,
		&e.CreatedBy, &eCreated,
		&r.LessonsCount, &r.TTSReady,
	)
	if err != nil {
		return err
	}
	r.CallScript.Normalize()
	if eID != nil {
		e.ID, e.ScenarioID, e.IsCurrent = *eID, r.ID, true
		e.Version = convert.Deref(eVersion)
		e.CreatedAt = convert.Deref(eCreated)
		finishEtalon(&e)
		r.Etalon = &e
	}
	return nil
}

// ---------------------------------------------------------------- эталоны

const etalonColumns = `
SELECT e.id, e.scenario_id, e.version, e.is_current, e.card, e.card_draft, e.scoring,
       e.expected_actions, e.expected_dialogue, e.created_by, e.created_at
  FROM etalons e`

const sqlGetCurrentEtalon = etalonColumns + `
 WHERE e.scenario_id = $1 AND e.is_current`

const sqlGetEtalon = etalonColumns + `
 WHERE e.id = $1`

// GetCurrentEtalon — текущая версия эталона сценария (pgx.ErrNoRows, если нет).
func GetCurrentEtalon(ctx context.Context, q pg.Querier, scenarioID uuid.UUID) (*EtalonRow, error) {
	return getEtalon(ctx, q, sqlGetCurrentEtalon, scenarioID)
}

// GetEtalon — конкретная версия (attempts.etalon_id / evaluations.etalon_id).
func GetEtalon(ctx context.Context, q pg.Querier, etalonID uuid.UUID) (*EtalonRow, error) {
	return getEtalon(ctx, q, sqlGetEtalon, etalonID)
}

func getEtalon(ctx context.Context, q pg.Querier, sql string, id uuid.UUID) (*EtalonRow, error) {
	e := new(EtalonRow)
	s := etalonScanners(e)
	err := q.QueryRow(ctx, sql, id).Scan(
		&e.ID, &e.ScenarioID, &e.Version, &e.IsCurrent,
		s.card, s.draft, s.scoring, s.actions, s.dialogue,
		&e.CreatedBy, &e.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	finishEtalon(e)
	return e, nil
}

type etalonScan struct {
	card, draft, scoring, actions, dialogue any
}

// etalonScanners — приёмники jsonb-колонок эталона (card_draft копируется: нужен raw).
func etalonScanners(e *EtalonRow) etalonScan {
	return etalonScan{
		card:     &jsonScan{dst: &e.Card, name: "etalons.card"},
		draft:    &rawScan{dst: &e.CardDraftRaw},
		scoring:  &jsonScan{dst: &e.Scoring, name: "etalons.scoring"},
		actions:  &jsonScan{dst: &e.ExpectedActions, name: "etalons.expected_actions"},
		dialogue: &dialogueScan{dst: &e.ExpectedDialogue},
	}
}

// finishEtalon — card_draft из raw ('{}' — пустая карточка АРМ) и нормализация массивов.
func finishEtalon(e *EtalonRow) {
	if isEmptyJSON(e.CardDraftRaw) {
		e.CardDraft = convert.EmptyDraft()
	} else if err := json.Unmarshal(e.CardDraftRaw, &e.CardDraft); err != nil {
		// Битый card_draft не должен ронять чтение сценария: отдаём пустую форму,
		// эталон всё равно не пройдёт approve (нет обязательных полей).
		e.CardDraft = convert.EmptyDraft()
	} else {
		convert.NormalizeDraft(&e.CardDraft)
	}
	if e.ExpectedActions == nil {
		e.ExpectedActions = []model.ExpectedAction{}
	}
}

// dialogueScan — etalons.expected_dialogue: '{}' / NULL -> nil.
type dialogueScan struct{ dst **model.ExpectedDialogue }

func (s *dialogueScan) ScanBytes(b []byte) error {
	if isEmptyJSON(b) {
		*s.dst = nil
		return nil
	}
	d := new(model.ExpectedDialogue)
	if err := json.Unmarshal(b, d); err != nil {
		return fmt.Errorf("store: jsonb etalons.expected_dialogue: %w", err)
	}
	d.Normalize()
	*s.dst = d
	return nil
}

// ---------------------------------------------------------------- public

// ScenarioToPublic — полное представление для преподавателя: все обязательные поля
// контракта заданы (эталона нет — etalonCard {}, etalonDraft пустая форма,
// requiredFields [], etalonVersion 0).
func ScenarioToPublic(r *ScenarioRow) public.Scenario {
	lessons, inUse, version := r.LessonsCount, r.LessonsCount > 0, max(r.Version, 1)
	tts := r.TTSReady
	out := public.Scenario{
		Id:               r.ID,
		Title:            r.Title,
		CategoryId:       r.CategoryID.String(),
		CategoryName:     r.CategoryName,
		Difficulty:       public.Difficulty(r.Difficulty),
		Mode:             public.ScenarioMode(r.Mode),
		Source:           public.ScenarioSource(r.Source),
		Status:           public.ScenarioStatus(r.Status),
		CallScript:       convert.CallScriptToPublic(&r.CallScript),
		TeacherComment:   convert.NonEmptyPtr(r.TeacherComment),
		NotesForTeacher:  convert.NonEmpty(r.GenerationMeta.NotesForTeacher),
		AuthorId:         r.AuthorID,
		ValidatedBy:      r.ValidatedBy,
		ValidatedAt:      convert.UTC(r.ValidatedAt),
		CreatedAt:        r.CreatedAt.UTC(),
		TtsReady:         &tts,
		Version:          &version,
		ParentScenarioId: r.ParentID,
		InUse:            &inUse,
		LessonsCount:     &lessons,
	}
	if !r.GenerationMeta.IsZero() {
		m := r.GenerationMeta.Map()
		out.GenerationMeta = &m
	}
	e := r.Etalon
	if e == nil {
		out.EtalonDraft = convert.EmptyDraft()
		out.RequiredFields = []string{}
		return out
	}
	out.EtalonVersion = e.Version
	out.EtalonCard = convert.IncidentCardToPublic(&e.Card)
	out.EtalonDraft = e.CardDraft
	out.RequiredFields = e.RequiredFields()
	if !e.Scoring.IsZero() {
		s := convert.ScoringToPublic(&e.Scoring)
		out.Scoring = &s
	}
	if len(e.ExpectedActions) > 0 {
		a := convert.ExpectedActionsToPublic(e.ExpectedActions)
		out.ExpectedActions = &a
	}
	if e.ExpectedDialogue != nil {
		d := convert.ExpectedDialogueToPublic(e.ExpectedDialogue)
		out.ExpectedDialogue = &d
	}
	return out
}

// ScenariosToPublic — список (всегда не nil).
func ScenariosToPublic(rows []ScenarioRow) []public.Scenario {
	out := make([]public.Scenario, len(rows))
	for i := range rows {
		out[i] = ScenarioToPublic(&rows[i])
	}
	return out
}
