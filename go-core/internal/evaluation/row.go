package evaluation

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/scoring"
	"lct/gocore/internal/settings"
	"lct/gocore/internal/store"
)

// evalRow — оценка попытки в памяти: то, что читается из БД одним запросом (evalSelect),
// меняется при доезде слоёв и из чего строится public.Evaluation без повторного чтения.
// Так же собирается и новая оценка в StartEvaluation — представление одно.
type evalRow struct {
	// попытка и занятие (есть всегда)
	AttemptID     uuid.UUID
	LessonID      uuid.UUID
	UserID        uuid.UUID
	AttemptStatus string
	TeacherID     *uuid.UUID
	CreatedBy     uuid.UUID
	LessonStatus  string
	Settings      model.LessonSettings
	CategoryName  string

	// оценка (Exists=false — оценки ещё нет; остальные поля пусты)
	Exists        bool
	ID            uuid.UUID
	EtalonID      uuid.UUID
	Status        string
	FieldsScore   *float64
	GrammarScore  *float64
	SemanticScore *float64
	TimingScore   *float64
	DialogueScore *float64
	TotalScore    float64
	Verdict       string
	// jsonb как в БД: разбираются только при сборке представления, переписываются —
	// только те, что поменял доехавший слой.
	FieldErrors    json.RawMessage // public.FieldError[] (camelCase)
	GrammarRemarks json.RawMessage // components.GrammarRemark[] (snake)
	GrammarStats   json.RawMessage // model.GrammarStats
	Semantic       json.RawMessage // components.SemanticResult
	Dialogue       json.RawMessage // components.DialogueResult
	Timing         scoring.TimingResult
	Layers         model.Layers
	Weights        settings.Weights
	Engine         map[string]any
	NeedsReview    bool
	AIUnavailable  bool
	EvaluatedAt    *time.Time

	OverrideScore   *float64
	OverrideVerdict *string
	OverrideReason  *string
	OverriddenAt    *time.Time
	OverriderName   string

	ExpectedDialogue *model.ExpectedDialogue // чек-лист эталона (тексты пунктов для UI)
	XPEarned         int                     // сумма xp_ledger по попытке
	Running          []string                // типы задач ai_jobs в статусе running

	// fieldErrs — уже разобранные field_errors (новая оценка: без круга через JSON).
	fieldErrs []public.FieldError
}

// accessRow — минимальная AttemptRow для правил internal/access (владелец, владелец занятия).
func (r *evalRow) accessRow() *store.AttemptRow {
	return &store.AttemptRow{UserID: r.UserID, LessonTeacherID: r.TeacherID, LessonCreatedBy: r.CreatedBy}
}

// finalScore — итог с учётом ручной корректировки (= evaluations.final_score).
func (r *evalRow) finalScore() float64 {
	if r.OverrideScore != nil {
		return *r.OverrideScore
	}
	return r.TotalScore
}

// verdictShown — вердикт для UI: корректировка преподавателя важнее автоматического.
func (r *evalRow) verdictShown() string {
	if r.OverrideVerdict != nil && *r.OverrideVerdict != "" {
		return *r.OverrideVerdict
	}
	return r.Verdict
}

// loadRow — оценка попытки: sqlViewByAttempt (без блокировки; попытки нет — pgx.ErrNoRows,
// оценки нет — Exists=false) или sqlLockEvaluation (FOR UPDATE; оценки нет — pgx.ErrNoRows).
func loadRow(ctx context.Context, q pg.Querier, sql string, attemptID uuid.UUID) (*evalRow, error) {
	r := &evalRow{Layers: model.Layers{}, Engine: map[string]any{}}
	var (
		id, etalonID               *uuid.UUID
		status, verdict            *string
		total                      *float64
		needsReview, aiUnavailable *bool
		ovLast, ovFirst, ovMiddle  *string
		running                    []string
	)
	err := q.QueryRow(ctx, sql, attemptID).Scan(
		&r.AttemptID, &r.LessonID, &r.UserID, &r.AttemptStatus, &r.TeacherID, &r.CreatedBy, &r.LessonStatus,
		&settingsInto{dst: &r.Settings}, &r.CategoryName,
		&id, &etalonID, &status, &r.FieldsScore, &r.GrammarScore, &r.SemanticScore, &r.TimingScore,
		&r.DialogueScore, &total, &verdict,
		&rawCopy{dst: &r.FieldErrors}, &rawCopy{dst: &r.GrammarRemarks}, &rawCopy{dst: &r.GrammarStats},
		&rawCopy{dst: &r.Semantic}, &rawCopy{dst: &r.Dialogue},
		&jsonInto{dst: &r.Timing, col: "evaluations.timing"},
		&jsonInto{dst: &r.Layers, col: "evaluations.layers"},
		&jsonInto{dst: &r.Weights, col: "evaluations.weights"},
		&jsonInto{dst: &r.Engine, col: "evaluations.engine"},
		&needsReview, &aiUnavailable,
		&r.EvaluatedAt, &r.OverrideScore, &r.OverrideVerdict, &r.OverrideReason, &r.OverriddenAt,
		&ovLast, &ovFirst, &ovMiddle,
		&expectedDialogueInto{dst: &r.ExpectedDialogue},
		&r.XPEarned, &running,
	)
	if err != nil {
		return nil, err
	}
	if id == nil {
		return r, nil
	}
	r.Exists = true
	r.ID = *id
	if etalonID != nil {
		r.EtalonID = *etalonID
	}
	r.Status = deref(status)
	r.Verdict = deref(verdict)
	if total != nil {
		r.TotalScore = *total
	}
	r.NeedsReview = needsReview != nil && *needsReview
	r.AIUnavailable = aiUnavailable != nil && *aiUnavailable
	r.EvaluatedAt = utcPtr(r.EvaluatedAt)
	r.OverriddenAt = utcPtr(r.OverriddenAt)
	if r.OverrideScore != nil {
		r.OverriderName = core.ShortName(deref(ovLast), deref(ovFirst), deref(ovMiddle))
		if r.OverriderName == "" {
			r.OverriderName = "Преподаватель"
		}
	}
	if r.Layers == nil {
		r.Layers = model.Layers{}
	}
	if r.Engine == nil {
		r.Engine = map[string]any{}
	}
	r.Running = running
	return r, nil
}

// ---------------------------------------------------------------- слои, итог

// layerTerminal — слой больше не изменится (done/failed/skipped).
func layerTerminal(state string) bool {
	return state == core.LayerDone || state == core.LayerFailed || state == core.LayerSkipped
}

// allTerminal — все ожидаемые слои на месте: оценку можно финализировать.
func (r *evalRow) allTerminal() bool {
	for _, st := range r.Layers {
		if !layerTerminal(st) {
			return false
		}
	}
	return true
}

// recompute — итог по доступным слоям (частичный, пока слои доезжают); все слои терминальны —
// финализация: status=done, вердикт по порогу занятия, evaluated_at. true — оценка только что
// финализирована (дальше — XP, статус попытки, завершение занятия).
func (r *evalRow) recompute(now time.Time) bool {
	r.TotalScore = round0(scoring.TotalOf(scoring.LayerScores{
		Fields:   r.FieldsScore,
		Semantic: r.SemanticScore,
		Grammar:  r.GrammarScore,
		Timing:   r.TimingScore,
		Dialogue: r.DialogueScore,
	}, r.Weights))
	if r.Status == core.EvalDone || !r.allTerminal() {
		return false
	}
	r.Status = core.EvalDone
	r.Verdict = scoring.Verdict(r.TotalScore, r.Settings.PassThreshold)
	r.EvaluatedAt = &now
	return true
}

// markFailed — AI-слой окончательно не посчитан: итог по остальным слоям, флаги ревью.
// Код ошибки — в engine.errors (для QA), не студенту.
func (r *evalRow) markFailed(layer, code string) {
	r.Layers[layer] = core.LayerFailed
	r.AIUnavailable = true
	r.NeedsReview = true
	errs, _ := r.Engine["errors"].(map[string]any)
	if errs == nil {
		errs = map[string]any{}
	}
	if code == "" {
		code = "failed"
	}
	errs[layer] = code
	r.Engine["errors"] = errs
}

// xpInput — вход правил XP: итог с корректировкой, вердикт с корректировкой, тайминг.
func (r *evalRow) xpInput() scoring.XPInput {
	return scoring.XPInput{
		Final:      r.finalScore(),
		Passed:     r.verdictShown() == core.VerdictPass,
		WithinNorm: r.Timing.WithinNorm,
	}
}

// passBonus — сколько pass_bonus положено по входу правил XP (0 — не положено).
func passBonus(in scoring.XPInput, rules settings.XPRules) int {
	for _, e := range scoring.XPEntries(in, rules) {
		if e.Reason == scoring.XPReasonPassBonus {
			return e.Delta
		}
	}
	return 0
}

// ---------------------------------------------------------------- мелочи

// round0 — баллы храним и отдаём целыми (UI рисует их как есть и рассчитан на целые).
// NaN/Inf → 0, без «-0».
func round0(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	r := math.Round(x)
	if r == 0 {
		return 0
	}
	return r
}

// score100 — балл AI-слоя: NaN/Inf — недопустимый результат (ok=false), иначе 0..100 целым.
func score100(x float32) (float64, bool) {
	f := float64(x)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return round0(min(max(f, 0), 100)), true
}

func deref[T any](p *T) T {
	if p == nil {
		var z T
		return z
	}
	return *p
}

func ptr[T any](v T) *T { return &v }

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// f32 — *float64 → *public.Score (float32).
func f32(p *float64) *float32 {
	if p == nil {
		return nil
	}
	v := float32(*p)
	return &v
}
