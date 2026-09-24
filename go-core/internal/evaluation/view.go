package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/scoring"
)

// View — представление оценки попытки (для WS-пушей, отчётов, других доменов). Оценки
// (или попытки) нет — ошибка, для которой pg.IsNoRows == true. Один запрос к БД.
func (s *Service) View(ctx context.Context, q pg.Querier, attemptID uuid.UUID) (*public.Evaluation, error) {
	r, err := loadRow(ctx, q, sqlViewByAttempt, attemptID)
	if err != nil {
		return nil, err
	}
	if !r.Exists {
		return nil, fmt.Errorf("evaluation: attempt %s: %w", attemptID, pgx.ErrNoRows)
	}
	ev := s.buildView(r)
	return &ev, nil
}

// buildView — public.Evaluation из строки в памяти. Все обязательные поля контракта заданы,
// массивы — [] (не null). Разбор ошибок jsonb не роняет ответ: битый слой просто не
// показывается (и пишется в лог) — страница результата важнее одного блока.
func (s *Service) buildView(r *evalRow) public.Evaluation {
	ev := public.Evaluation{
		AttemptId:       r.AttemptID,
		Status:          public.EvaluationStatus(r.Status),
		FieldsScore:     f32(r.FieldsScore),
		TimingScore:     f32(r.TimingScore),
		GrammarScore:    f32(r.GrammarScore),
		SemanticScore:   f32(r.SemanticScore),
		DialogueScore:   f32(r.DialogueScore),
		TotalScore:      float32(r.TotalScore),
		Verdict:         public.Verdict(r.verdictShown()),
		Weights:         scoring.PublicWeights(r.Weights),
		Timing:          r.Timing.ToPublic(),
		NeedsReview:     r.NeedsReview,
		AiUnavailable:   r.AIUnavailable,
		FinalScore:      float32(r.finalScore()),
		Recommendations: []public.Recommendation{},
	}
	if ev.Verdict == "" {
		ev.Verdict = public.VerdictPending
	}

	// Ошибки полей.
	fieldErrs := r.fieldErrs
	if fieldErrs == nil && !isEmptyJSON(r.FieldErrors) {
		if err := json.Unmarshal(r.FieldErrors, &fieldErrs); err != nil {
			s.log.Warn("evaluation: bad field_errors", "attempt", r.AttemptID, "err", err)
			fieldErrs = nil
		}
	}
	if fieldErrs == nil {
		fieldErrs = []public.FieldError{}
	}
	ev.FieldErrors = fieldErrs

	// Состояние слоёв: в БД — терминальные и queued; running — из ai_jobs.
	if len(r.Layers) > 0 {
		layers := make(map[string]public.EvaluationLayers, len(r.Layers))
		for layer, st := range r.Layers {
			if st == core.LayerQueued && r.jobRunning(layer) {
				st = core.LayerRunning
			}
			layers[layer] = public.EvaluationLayers(st)
		}
		ev.Layers = &layers
	}
	if len(r.Engine) > 0 {
		if eng, err := publicEngine(r.Engine); err != nil {
			s.log.Warn("evaluation: bad engine", "attempt", r.AttemptID, "err", err)
		} else {
			ev.Engine = &eng
		}
	}

	// AI-слои: показываются, только если у слоя есть балл.
	var (
		gram *components.GrammarResult
		sem  *components.SemanticResult
		dlg  *components.DialogueResult
	)
	if r.GrammarScore != nil {
		var remarks []components.GrammarRemark
		var stats model.GrammarStats
		if s.decode(r, "grammar_remarks", r.GrammarRemarks, &remarks) && s.decode(r, "grammar_stats", r.GrammarStats, &stats) {
			g := convert.GrammarToPublic(float32(*r.GrammarScore), remarks, stats)
			ev.Grammar = &g
			gram = &components.GrammarResult{Remarks: remarks}
		}
	}
	if r.SemanticScore != nil && !isEmptyJSON(r.Semantic) {
		var res components.SemanticResult
		if s.decode(r, "semantic", r.Semantic, &res) {
			res.Score = float32(*r.SemanticScore) // балл — округлённый из колонки, как у плитки
			p := convert.SemanticToPublic(res)
			if p.PerField != nil {
				// ai-service отвечает путями контрактной карточки (actions_taken, action_text) —
				// наружу пути формы АРМ, как у замечаний грамматики (actionsTaken, actionText).
				for i := range *p.PerField {
					(*p.PerField)[i].Field = scoring.CanonicalPath((*p.PerField)[i].Field)
				}
			}
			ev.Semantic = &p
			sem = &res
		}
	}
	if r.DialogueScore != nil && !isEmptyJSON(r.Dialogue) {
		var res components.DialogueResult
		if s.decode(r, "dialogue", r.Dialogue, &res) {
			res.Score = float32(*r.DialogueScore)
			p := convert.DialogueToPublic(res, r.ExpectedDialogue)
			ev.Dialogue = &p
			dlg = &res
		}
	}

	if r.OverrideScore != nil {
		ov := &struct {
			At      time.Time      `json:"at"`
			By      string         `json:"by"`
			Reason  string         `json:"reason"`
			Score   float32        `json:"score"`
			Verdict public.Verdict `json:"verdict"`
		}{
			By:      r.OverriderName,
			Reason:  deref(r.OverrideReason),
			Score:   float32(*r.OverrideScore),
			Verdict: public.Verdict(deref(r.OverrideVerdict)),
		}
		if r.OverriddenAt != nil {
			ov.At = *r.OverriddenAt
		}
		ev.Override = ov
	}

	// Рекомендации и XP — по окончательной оценке (как мок фронта: пока слои доезжают,
	// советы «мигали» бы с каждым слоем).
	if r.Status == core.EvalDone {
		ev.Recommendations = scoring.Recommendations(recInput(r, fieldErrs, gram, sem, dlg))
		xp := r.XPEarned
		ev.XpEarned = &xp
	}
	return ev
}

// publicEngine — evaluations.engine для API. В БД он лежит как пришёл от ai-service
// (components.Engine, snake_case: {rules_version, grammar: {duration_ms, lt_version}, …}),
// а провод публичного API — camelCase: ключи переводятся на любой глубине, значения
// (коды ошибок, имена моделей) не трогаются. Входные значения не меняются: r.Engine
// после применения слоя содержит и структуры components.Engine — поэтому через JSON.
func publicEngine(m map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return camelKeys(v).(map[string]any), nil
}

// camelKeys — ключи объектов snake_case → camelCase рекурсивно (массивы — поэлементно).
func camelKeys(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[snakeToCamelKey(k)] = camelKeys(x)
		}
		return out
	case []any:
		for i := range t {
			t[i] = camelKeys(t[i])
		}
		return t
	default:
		return v
	}
}

// snakeToCamelKey — "duration_ms" → "durationMs", "rules_version" → "rulesVersion";
// ключ без "_" (в том числе уже camelCase) — как есть; ведущие/двойные "_" схлопываются.
func snakeToCamelKey(k string) string {
	if !strings.Contains(k, "_") {
		return k
	}
	var b strings.Builder
	b.Grow(len(k))
	up := false
	for _, c := range k {
		switch {
		case c == '_':
			up = b.Len() > 0
		case up:
			b.WriteRune(unicode.ToUpper(c))
			up = false
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// jobRunning — задача слоя сейчас выполняется ai-service (ai_jobs.status = running).
func (r *evalRow) jobRunning(layer string) bool {
	for _, t := range r.Running {
		if core.JobType(t).Layer() == layer {
			return true
		}
	}
	return false
}

// decode — jsonb слоя в dst; ошибка — лог и false (слой не показывается).
func (s *Service) decode(r *evalRow, col string, raw json.RawMessage, dst any) bool {
	if isEmptyJSON(raw) {
		return true
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		s.log.Warn("evaluation: bad jsonb", "attempt", r.AttemptID, "column", col, "err", err)
		return false
	}
	return true
}

// recInput — факты попытки для scoring.Recommendations.
func recInput(r *evalRow, fieldErrs []public.FieldError, gram *components.GrammarResult,
	sem *components.SemanticResult, dlg *components.DialogueResult) scoring.RecInput {
	in := scoring.RecInput{
		FieldErrors:  fieldErrs,
		Timing:       r.Timing,
		CategoryName: r.CategoryName,
	}
	if gram != nil {
		in.GrammarRemarks = len(gram.Remarks)
	}
	if sem != nil {
		in.SemanticMissingFacts = convert.SliceFromPtr(sem.MissingFacts)
	}
	if dlg != nil {
		in.DialogueMissing = dialogueMissing(dlg, r.ExpectedDialogue)
		for _, h := range convert.SliceFromPtr(dlg.ForbiddenHits) {
			in.DialogueForbiddenHits = append(in.DialogueForbiddenHits, h.Phrase)
		}
		score := float64(dlg.Score)
		in.DialogueScore = &score
		in.SpeechFillerCount = convert.Deref(dlg.Speech.FillerCount)
		if dlg.Speech.Fillers != nil {
			in.SpeechFillers = *dlg.Speech.Fillers
		}
	}
	return in
}

// dialogueMissing — пропущенные вопросы протокола: missing_questions от ai-service, а если
// их нет — обязательные пункты чек-листа со статусом missed (тексты — из эталона).
func dialogueMissing(d *components.DialogueResult, exp *model.ExpectedDialogue) []string {
	if q := convert.SliceFromPtr(d.MissingQuestions); len(q) > 0 {
		return q
	}
	var out []string
	for i := range d.Checklist {
		c := &d.Checklist[i]
		if c.Status != "missed" {
			continue
		}
		item, ok := exp.Item(c.Id)
		if !ok || !item.Required {
			continue
		}
		out = append(out, item.Text)
	}
	return out
}
