package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/dds"
	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/scoring"
	"lct/gocore/internal/settings"
	"lct/gocore/internal/store"
)

// rulesVersion — версия детерминированных правил go-core (слои fields/timing, итог):
// evaluations.engine.rules_version — чтобы оценку можно было объяснить задним числом.
const rulesVersion = "go-scoring-1"

// Тексты «оценивать нечего» для смыслового слоя без свободного текста (LLM не зовём).
const (
	summaryNoDescription = "Описание со слов заявителя не заполнено, поэтому смысловую полноту оценить не по чему."
	summaryNoActionText  = "Текст действия не заполнен, поэтому смысловую полноту оценить не по чему."
)

// ruleDDSReaction — источник слоя fields в ракурсе dds (evaluations.engine.fields.source):
// протокол реагирования своей службы, а не поля карточки.
const ruleDDSReaction = "dds_reaction"

// Поля свободного текста. Для грамматики — пути формы АРМ (фронт получает замечания с ними
// как есть и может подсветить поле); для семантики — поля контрактной карточки ответа.
const (
	gramFieldDescription  = "description"
	gramFieldActionsTaken = "actionsTaken"
	gramFieldActionText   = "actionText"
	// Комментарии диспетчера ДДС к статусам своей службы (по строке на комментарий).
	gramFieldReactionComments = "reactionComments"

	semFieldDescription  = "description"
	semFieldActionsTaken = "actions_taken"
	semFieldActionText   = "action_text"
)

// startRow — всё для старта оценки (sqlStart).
type startRow struct {
	AttemptID      uuid.UUID
	LessonID       uuid.UUID
	UserID         uuid.UUID
	ScenarioID     uuid.UUID
	EtalonID       uuid.UUID
	Mode           string
	Status         string
	TimeLimitSec   int
	CallAcceptedAt *time.Time
	FirstInputAt   *time.Time
	SubmittedAt    *time.Time
	TimeSpentMs    *int
	Card           json.RawMessage
	ActionText     *string
	CallEndedAt    *time.Time

	LessonStatus string
	Settings     model.LessonSettings

	CategoryID   uuid.UUID
	Difficulty   int
	CallScript   model.CallScript
	CategoryCode string
	CategoryName string

	EtalonCard       components.IncidentCard
	EtalonDraft      json.RawMessage
	Scoring          model.Scoring
	ExpectedActions  []model.ExpectedAction
	ExpectedDialogue *model.ExpectedDialogue

	HasEvaluation bool
	ServiceCode   string // ракурс dds — служба обучающегося; "" — ракурс 112
}

func loadStart(ctx context.Context, q pg.Querier, attemptID uuid.UUID) (*startRow, error) {
	st := new(startRow)
	err := q.QueryRow(ctx, sqlStart, attemptID).Scan(
		&st.AttemptID, &st.LessonID, &st.UserID, &st.ScenarioID, &st.EtalonID, &st.Mode, &st.Status, &st.TimeLimitSec,
		&st.CallAcceptedAt, &st.FirstInputAt, &st.SubmittedAt, &st.TimeSpentMs, &rawCopy{dst: &st.Card}, &st.ActionText,
		&st.CallEndedAt,
		&st.LessonStatus, &settingsInto{dst: &st.Settings},
		&st.CategoryID, &st.Difficulty, &jsonInto{dst: &st.CallScript, col: "scenarios.call_script"},
		&st.CategoryCode, &st.CategoryName,
		&jsonInto{dst: &st.EtalonCard, col: "etalons.card"}, &rawCopy{dst: &st.EtalonDraft},
		&jsonInto{dst: &st.Scoring, col: "etalons.scoring"},
		&jsonInto{dst: &st.ExpectedActions, col: "etalons.expected_actions"},
		&expectedDialogueInto{dst: &st.ExpectedDialogue},
		&st.HasEvaluation, &st.ServiceCode,
	)
	if err != nil {
		return nil, err
	}
	st.CallScript.Normalize()
	return st, nil
}

// StartEvaluation — core.Evaluator: вызывается в транзакции submit после записи карточки,
// времени и статуса попытки. Считает fields и timing, создаёт evaluations (partial), ставит
// AI-задачи; если AI-слоёв ждать не нужно — сразу финализирует. Идемпотентно: оценка уже
// есть — ничего не делает. evaluationUpdated уходит после COMMIT.
func (s *Service) StartEvaluation(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID) error {
	st, err := loadStart(ctx, tx, attemptID)
	if err != nil {
		return fmt.Errorf("evaluation: load attempt %s: %w", attemptID, err)
	}
	if st.HasEvaluation {
		return nil
	}
	switch st.Status {
	case core.AttemptSubmitted, core.AttemptEvaluating, core.AttemptEvaluated:
	default:
		return fmt.Errorf("evaluation: attempt %s is %q, not submitted", attemptID, st.Status)
	}
	snap := s.st.Get(ctx)
	now := time.Now().UTC()

	r := &evalRow{
		AttemptID:        st.AttemptID,
		LessonID:         st.LessonID,
		UserID:           st.UserID,
		AttemptStatus:    st.Status,
		LessonStatus:     st.LessonStatus,
		Settings:         st.Settings,
		CategoryName:     st.CategoryName,
		DDS:              st.ServiceCode != "",
		Exists:           true,
		ID:               ids.New(),
		EtalonID:         st.EtalonID,
		Status:           core.EvalPartial,
		Verdict:          core.VerdictPending,
		Layers:           model.Layers{},
		Engine:           map[string]any{"rules_version": rulesVersion},
		ExpectedDialogue: st.ExpectedDialogue,
	}
	// Веса уже эффективные (lessons пишет их через scoring.EffectiveWeights при создании), но
	// занятие могли поправить руками — только нормируем к 1. Долю разговора по умолчанию НЕ
	// добавляем (dialogueDefault = 0): dialogue = 0 при включённом голосе — выбор преподавателя.
	r.Weights = scoring.RoundWeights(scoring.EffectiveWeights(st.Settings.Weights, st.Settings.Voice.Enabled, 0))

	card := decodeDraft(st.Card)

	// ---- слой 1: поля карточки (режим «действия с карточками» — по тексту действия, не по
	// полям; ракурс dds — протокол реагирования своей службы: решение, норматив, статусы)
	switch {
	case st.ServiceCode != "": // ракурс dds: у попытки есть служба обучающегося
		score, errs := dds.Evaluate(card, st.ServiceCode, st.Scoring.Reaction, st.CallAcceptedAt)
		r.FieldsScore = ptr(round0(score))
		r.fieldErrs = errs
		r.Layers[core.LayerFields] = core.LayerDone
		r.Engine[core.LayerFields] = map[string]any{"source": ruleDDSReaction, "service": st.ServiceCode}
	case st.Mode == core.ModeCards:
		etalon := decodeDraft(st.EtalonDraft)
		if etalon == nil {
			e := convert.EmptyDraft()
			etalon = &e
		}
		score, errs := scoring.EvaluateFields(card, etalon, fieldSpec(st, etalon), s.cat)
		r.FieldsScore = ptr(round0(score))
		r.fieldErrs = errs
		r.Layers[core.LayerFields] = core.LayerDone
	default:
		r.fieldErrs = []public.FieldError{}
		r.Layers[core.LayerFields] = core.LayerSkipped
	}
	if r.FieldErrors, err = json.Marshal(r.fieldErrs); err != nil {
		return fmt.Errorf("evaluation: encode field errors: %w", err)
	}

	// ---- слой 4: тайминг
	tScore, tRes := scoring.Timing(spentMs(st), st.TimeLimitSec, reactionMs(st), snap.TimingTolerance)
	r.TimingScore = ptr(round0(tScore))
	r.Timing = tRes
	r.Layers[core.LayerTiming] = core.LayerDone

	// ---- AI-слои
	jobs := make([]core.NewJob, 0, 3)
	sc := s.scenarioContext(st)

	if texts := grammarTexts(st, card); len(texts) > 0 {
		jobs = append(jobs, grammarJob(st, texts))
		r.Layers[core.LayerGrammar] = core.LayerQueued
	} else {
		// Нечего проверять — слой не оценивается и в итог не входит (100 за пустое поле
		// противоречило бы «проверено слов: 0»; так же делает мок фронта).
		r.Layers[core.LayerGrammar] = core.LayerSkipped
	}

	if job, ok := s.semanticJob(st, card, sc); ok {
		jobs = append(jobs, job)
		r.Layers[core.LayerSemantic] = core.LayerQueued
	} else if err := r.setLocalSemantic(st); err != nil {
		return err
	}

	if st.Settings.Voice.Enabled && st.ExpectedDialogue != nil && len(st.ExpectedDialogue.Checklist) > 0 {
		turns, err := store.ListDialogueTurns(ctx, tx, attemptID)
		if err != nil {
			return fmt.Errorf("evaluation: %w", err)
		}
		if len(turns) > 0 {
			jobs = append(jobs, dialogueJob(st, sc, turns, snap))
			r.Layers[core.LayerDialogue] = core.LayerQueued
		} else {
			r.Layers[core.LayerDialogue] = core.LayerSkipped // разговора не было — оценивать нечего
		}
	} else {
		r.Layers[core.LayerDialogue] = core.LayerSkipped
	}

	finalized := r.recompute(now)

	inserted, err := insertEvaluation(ctx, tx, r)
	if err != nil {
		return err
	}
	if !inserted {
		return nil // параллельный submit уже создал оценку и поставил задачи
	}
	for i := range jobs {
		if _, _, err := s.queue.Enqueue(ctx, tx, jobs[i]); err != nil {
			return fmt.Errorf("evaluation: enqueue %s: %w", jobs[i].Type, err)
		}
	}
	if finalized {
		if err := s.finalize(ctx, tx, r, snap); err != nil {
			return err
		}
	}
	ev := s.buildView(r)
	pg.OnCommit(ctx, func() { s.publish(r.LessonID, r.AttemptID, &ev) })
	return nil
}

func insertEvaluation(ctx context.Context, tx pgx.Tx, r *evalRow) (bool, error) {
	timing, err := json.Marshal(r.Timing)
	if err != nil {
		return false, err
	}
	layers, err := json.Marshal(r.Layers)
	if err != nil {
		return false, err
	}
	weights, err := json.Marshal(r.Weights)
	if err != nil {
		return false, err
	}
	engine, err := json.Marshal(r.Engine)
	if err != nil {
		return false, err
	}
	var id uuid.UUID
	err = tx.QueryRow(ctx, sqlInsertEvaluation,
		r.ID, r.AttemptID, r.EtalonID, r.Status, r.FieldsScore, r.GrammarScore, r.SemanticScore,
		r.TimingScore, r.DialogueScore, r.TotalScore, r.Verdict, orDefault(r.FieldErrors, jsonEmptyArray),
		orDefault(r.GrammarRemarks, jsonEmptyArray), orDefault(r.GrammarStats, jsonEmptyObject),
		orDefault(r.Semantic, jsonEmptyObject), orDefault(r.Dialogue, jsonEmptyObject),
		json.RawMessage(timing), json.RawMessage(layers), json.RawMessage(weights), json.RawMessage(engine),
		r.NeedsReview, r.AIUnavailable, r.EvaluatedAt,
	).Scan(&id)
	switch {
	case err == nil:
		return true, nil
	case pg.IsNoRows(err):
		return false, nil
	default:
		return false, fmt.Errorf("evaluation: insert: %w", err)
	}
}

// ---------------------------------------------------------------- входы слоёв

// decodeDraft — карточка АРМ из jsonb (attempts.card / etalons.card_draft); пусто/битая — nil.
func decodeDraft(raw json.RawMessage) *public.IncidentCardDraft {
	if isEmptyJSON(raw) {
		return nil
	}
	d := new(public.IncidentCardDraft)
	if json.Unmarshal(raw, d) != nil {
		return nil
	}
	convert.NormalizeDraft(d)
	return d
}

// fieldSpec — правила слоя 1 из эталона. Нет required_fields — дефолтный набор (как у нового
// сценария). Эталон без служб в форме АРМ (так у всех фикстур и у старых эталонов) — коды
// служб берутся из контрактной карточки эталона (services_to_notify).
func fieldSpec(st *startRow, etalon *public.IncidentCardDraft) scoring.FieldSpec {
	spec := scoring.FieldSpec{Required: st.Scoring.RequiredFields, Weights: st.Scoring.FieldWeights}
	if len(spec.Required) == 0 {
		spec.Required = model.DefaultRequiredFields()
	}
	if len(etalon.Services) == 0 && st.EtalonCard.ServicesToNotify != nil {
		spec.ServiceCodes = *st.EtalonCard.ServicesToNotify
	}
	return spec
}

// spentMs — затраченное время: колонка submit'а, иначе из меток.
func spentMs(st *startRow) int {
	if st.TimeSpentMs != nil {
		return *st.TimeSpentMs
	}
	if st.SubmittedAt != nil && st.CallAcceptedAt != nil {
		return int(st.SubmittedAt.Sub(*st.CallAcceptedAt).Milliseconds())
	}
	return 0
}

// reactionMs — от принятия вызова до первого содержательного ввода (0 — ввода не было).
func reactionMs(st *startRow) int {
	if st.FirstInputAt == nil || st.CallAcceptedAt == nil {
		return 0
	}
	return max(0, int(st.FirstInputAt.Sub(*st.CallAcceptedAt).Milliseconds()))
}

// msSince — смещение метки от принятия вызова (мс; nil — метки нет).
func msSince(t, base *time.Time) *int {
	if t == nil || base == nil {
		return nil
	}
	return ptr(max(0, int(t.Sub(*base).Milliseconds())))
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }

// ---------------------------------------------------------------- payload'ы ai-service
//
// Только содержание ответа и attempt_id: ни user_id, ни ФИО, ни lesson_id в ai-service
// не уходят (docs/contracts.md, ПДн). request_id = id задачи ai_jobs (идемпотентность).

// Анонимные типы сгенерированных запросов — алиасами (литерал обязан совпасть поле в поле,
// иначе не скомпилируется: это и есть проверка против генератора).
type (
	grammarText = struct {
		Field string `json:"field"`
		Text  string `json:"text"`
	}
	grammarOptions = struct {
		ExcludeRules *[]string `json:"exclude_rules,omitempty"`
		Lang         *string   `json:"lang,omitempty"`
		Strict       *bool     `json:"strict,omitempty"`
	}
	langOptions = struct {
		Lang *string `json:"lang,omitempty"`
	}
	dialogueOptions = struct {
		Lang               *string  `json:"lang,omitempty"`
		SttConfidenceFloor *float32 `json:"stt_confidence_floor,omitempty"`
	}
	dialogueTiming = struct {
		CallAcceptedAtMs *int `json:"call_accepted_at_ms,omitempty"`
		CallEndedAtMs    *int `json:"call_ended_at_ms,omitempty"`
		SubmittedAtMs    *int `json:"submitted_at_ms,omitempty"`
	}
	scCategory = struct {
		Code *string   `json:"code,omitempty"`
		Name *string   `json:"name,omitempty"`
		Path *[]string `json:"path,omitempty"`
	}
	scenarioContext = struct {
		Category   *scCategory `json:"category,omitempty"`
		Difficulty *int        `json:"difficulty,omitempty"`
	}
)

const lang = "ru"

func dedupKey(attemptID uuid.UUID, layer string) string {
	return "eval:" + attemptID.String() + ":" + layer
}

func newJob(id uuid.UUID, t core.JobType, prio int, st *startRow, payload any) core.NewJob {
	return core.NewJob{
		ID:       id,
		Type:     t,
		Priority: prio,
		DedupKey: dedupKey(st.AttemptID, t.Layer()),
		RefType:  core.RefAttempt,
		RefID:    st.AttemptID,
		Payload:  payload,
	}
}

// scenarioContext — категория и сложность для качества замечаний LLM (путь — из каталога).
func (s *Service) scenarioContext(st *startRow) *scenarioContext {
	cat := &scCategory{Code: convert.NonEmpty(st.CategoryCode), Name: convert.NonEmpty(st.CategoryName)}
	if s.cat != nil {
		if t, ok := s.cat.TypeByID(st.CategoryID.String()); ok {
			cat.Path = convert.SlicePtr(t.Path)
		}
	}
	sc := &scenarioContext{Category: cat}
	if st.Difficulty >= 1 && st.Difficulty <= 3 {
		sc.Difficulty = ptr(st.Difficulty)
	}
	return sc
}

// grammarTexts — только свободный текст (адреса и ФИО LanguageTool подчеркнул бы целиком).
// Текст уходит как есть (без trim): смещения замечаний совпадают с тем, что видит студент.
func grammarTexts(st *startRow, card *public.IncidentCardDraft) []grammarText {
	var out []grammarText
	if st.Mode == core.ModeCardActions {
		if t := convert.Deref(st.ActionText); !blank(t) {
			out = append(out, grammarText{Field: gramFieldActionText, Text: t})
		}
		if st.ServiceCode != "" {
			// Комментарии к статусам — тоже свободный текст диспетчера (причина отказа,
			// результаты реагирования): Памятка требует их внятными.
			if t := dds.Comments(card, st.ServiceCode); !blank(t) {
				out = append(out, grammarText{Field: gramFieldReactionComments, Text: t})
			}
		}
		return out
	}
	if card != nil {
		if !blank(card.Description) {
			out = append(out, grammarText{Field: gramFieldDescription, Text: card.Description})
		}
		if !blank(card.ActionsTaken) {
			out = append(out, grammarText{Field: gramFieldActionsTaken, Text: card.ActionsTaken})
		}
	}
	return out
}

func grammarJob(st *startRow, texts []grammarText) core.NewJob {
	id := ids.New()
	prio := core.PriorityGrammar
	req := &aiservice.GrammarJobRequest{
		SchemaVersion: components.N1,
		RequestId:     id,
		AttemptId:     st.AttemptID,
		Priority:      &prio,
		Texts:         texts,
		Options:       &grammarOptions{Lang: ptr(lang)},
	}
	return newJob(id, core.JobEvaluateGrammar, prio, st, req)
}

// semanticJob — задача смыслового слоя; ok=false — свободного текста нет, LLM звать не о чем
// (слой считается локально, setLocalSemantic).
func (s *Service) semanticJob(st *startRow, card *public.IncidentCardDraft, sc *scenarioContext) (core.NewJob, bool) {
	var (
		answerCard *components.IncidentCard
		actionText *string
		fields     []string
		mode       aiservice.SemanticJobRequestMode
	)
	if st.Mode == core.ModeCardActions {
		t := convert.Deref(st.ActionText)
		if blank(t) {
			return core.NewJob{}, false
		}
		mode, actionText, fields = aiservice.SemanticJobRequestModeCardActions, &t, []string{semFieldActionText}
	} else {
		if card == nil || (blank(card.Description) && blank(card.ActionsTaken)) {
			return core.NewJob{}, false
		}
		c := convert.DraftToCard(card, s.cat)
		answerCard, mode = &c, aiservice.SemanticJobRequestModeCards
		if !blank(card.Description) {
			fields = append(fields, semFieldDescription)
		}
		if !blank(card.ActionsTaken) {
			fields = append(fields, semFieldActionsTaken)
		}
	}

	id := ids.New()
	prio := core.PrioritySemantic
	profile := components.EvalFast
	cs := convert.CallScriptToContract(&st.CallScript)
	cs.Dialogue = nil // бриф заявителя смысловой проверке не нужен (key_facts уже спроецированы)
	req := &aiservice.SemanticJobRequest{
		SchemaVersion:   components.N1,
		RequestId:       id,
		AttemptId:       st.AttemptID,
		Priority:        &prio,
		Profile:         &profile,
		Mode:            mode,
		ScenarioContext: sc,
		CallScript:      &cs,
		FreeTextFields:  fields,
		Options:         &langOptions{Lang: ptr(lang)},
	}
	req.Etalon.Card = st.EtalonCard
	if len(st.ExpectedActions) > 0 {
		a := convert.ExpectedActionsToContract(st.ExpectedActions)
		req.Etalon.ExpectedActions = &a
	}
	if !st.Scoring.IsZero() {
		scoringC := convert.ScoringToContract(&st.Scoring)
		req.Etalon.Scoring = &scoringC
	}
	req.Answer.Card = answerCard
	req.Answer.ActionText = actionText
	return newJob(id, core.JobEvaluateSemantic, prio, st, req), true
}

// setLocalSemantic — смысловой слой без LLM: свободного текста нет → 0 с пояснением, все
// обязательные факты — «не зафиксировано» (DESIGN §5). Слой готов сразу, confidence 1.
func (r *evalRow) setLocalSemantic(st *startRow) error {
	facts := st.Scoring.RequiredFacts
	if len(facts) == 0 {
		for i := range st.ExpectedActions {
			facts = append(facts, st.ExpectedActions[i].RequiredFacts...)
		}
	}
	if len(facts) == 0 {
		cs := convert.CallScriptToContract(&st.CallScript) // key_facts или проекция брифа без reveal=never
		facts = convert.SliceFromPtr(cs.KeyFacts)
	}
	summary := summaryNoDescription
	if st.Mode == core.ModeCardActions {
		summary = summaryNoActionText
	}
	res := components.SemanticResult{
		Score:             0,
		Confidence:        1,
		MissingFacts:      convert.NonNilSlicePtr(append([]string(nil), facts...)),
		ExtraFacts:        &[]string{},
		SummaryForStudent: &summary,
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("evaluation: encode local semantic: %w", err)
	}
	r.Semantic = raw
	r.SemanticScore = ptr(0.0)
	r.Layers[core.LayerSemantic] = core.LayerDone
	r.Engine[core.LayerSemantic] = map[string]any{"source": "go-core", "reason": "no_free_text"}
	return nil
}

// dialogueJob — оценка разговора по транскрипту (сквозная нумерация реплик 1..n) и
// чек-листу эталона. Метки — в мс от принятия вызова.
func dialogueJob(st *startRow, sc *scenarioContext, turns []store.DialogueTurnRow, snap *settings.Snapshot) core.NewJob {
	id := ids.New()
	prio := core.PriorityDialogue
	profile := components.EvalDialogue
	floor := float32(snap.STT.ConfidenceFloor)
	req := &aiservice.DialogueJobRequest{
		SchemaVersion:   components.N1,
		RequestId:       id,
		AttemptId:       st.AttemptID,
		Priority:        &prio,
		Profile:         &profile,
		ScenarioContext: sc,
		CallScript:      convert.CallScriptToContract(&st.CallScript),
		Transcript:      store.DialogueTurnsToContract(turns),
		Timing: &dialogueTiming{
			CallAcceptedAtMs: ptr(0),
			SubmittedAtMs:    msSince(st.SubmittedAt, st.CallAcceptedAt),
			CallEndedAtMs:    msSince(st.CallEndedAt, st.CallAcceptedAt),
		},
		Options: &dialogueOptions{Lang: ptr(lang), SttConfidenceFloor: &floor},
	}
	req.Etalon.ExpectedDialogue = convert.ExpectedDialogueToContract(st.ExpectedDialogue)
	return newJob(id, core.JobEvaluateDialogue, prio, st, req)
}
