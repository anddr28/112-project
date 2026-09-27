package convert

import (
	"strings"

	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
)

// ---------------------------------------------------------------- IncidentCard

// IncidentCardToPublic — контрактная карточка (snake) -> camelCase для редактора сценария.
// Anonymous-структуры applicant/casualties у обеих сторон совпадают (теги name/phone,
// dead/injured/trapped одинаковы) — присваиваются напрямую.
func IncidentCardToPublic(c *components.IncidentCard) public.IncidentCard {
	if c == nil {
		return public.IncidentCard{}
	}
	out := public.IncidentCard{
		CategoryCode:     c.CategoryCode,
		Applicant:        c.Applicant,
		Casualties:       c.Casualties,
		ServicesToNotify: c.ServicesToNotify,
		Description:      c.Description,
		ActionsTaken:     c.ActionsTaken,
		Attributes:       c.Attributes,
	}
	if c.Address != nil {
		a := public.Address(*c.Address)
		out.Address = &a
	}
	return out
}

// IncidentCardFromPublic — camelCase -> контрактная карточка (etalons.card).
func IncidentCardFromPublic(p *public.IncidentCard) components.IncidentCard {
	if p == nil {
		return components.IncidentCard{}
	}
	out := components.IncidentCard{
		CategoryCode:     NonEmptyPtr(p.CategoryCode),
		Applicant:        p.Applicant,
		Casualties:       p.Casualties,
		ServicesToNotify: p.ServicesToNotify,
		Description:      NonEmptyPtr(p.Description),
		ActionsTaken:     NonEmptyPtr(p.ActionsTaken),
		Attributes:       p.Attributes,
	}
	if p.Address != nil {
		a := compactAddress(components.Address(*p.Address))
		out.Address = &a
	}
	return out
}

// ---------------------------------------------------------------- Scoring

// ScoringToPublic — правила оценки для редактора (requiredFields дублируются и в
// Scenario.requiredFields — это делает store).
func ScoringToPublic(s *model.Scoring) public.Scoring {
	if s == nil {
		return public.Scoring{}
	}
	out := public.Scoring{
		RequiredFields: SlicePtr(copyStrings(s.RequiredFields)),
		RequiredFacts:  SlicePtr(copyStrings(s.RequiredFacts)),
		ForbiddenFacts: SlicePtr(copyStrings(s.ForbiddenFacts)),
	}
	if len(s.FieldWeights) > 0 {
		m := make(map[string]float32, len(s.FieldWeights))
		for k, v := range s.FieldWeights {
			m[k] = float32(v)
		}
		out.FieldWeights = &m
	}
	out.Reaction = ReactionToPublic(s.Reaction)
	return out
}

// ReactionToPublic — эталон работы диспетчера ДДС для редактора (nil — не задан: действуют
// значения по умолчанию, редактор их показывает сам).
func ReactionToPublic(r *model.ReactionExpectation) *public.ReactionExpectation {
	if r == nil {
		return nil
	}
	out := &public.ReactionExpectation{}
	if r.Decision != "" {
		d := public.ReactionExpectationDecision(r.Decision)
		out.Decision = &d
	}
	if r.DecisionWithinSec > 0 {
		v := r.DecisionWithinSec
		out.DecisionWithinSec = &v
	}
	if len(r.RequiredStatuses) > 0 {
		st := make([]public.ReactionStatus, len(r.RequiredStatuses))
		for i, v := range r.RequiredStatuses {
			st[i] = public.ReactionStatus(v)
		}
		out.RequiredStatuses = &st
	}
	return out
}

// ReactionFromPublic — эталон работы диспетчера ДДС из правки (значения уже проверены
// scenarios.validate). Статусы — без повторов, порядок сохраняется.
func ReactionFromPublic(p *public.ReactionExpectation) *model.ReactionExpectation {
	if p == nil {
		return nil
	}
	out := &model.ReactionExpectation{}
	if p.Decision != nil {
		out.Decision = string(*p.Decision)
	}
	if p.DecisionWithinSec != nil {
		out.DecisionWithinSec = *p.DecisionWithinSec
	}
	if p.RequiredStatuses != nil {
		seen := make(map[public.ReactionStatus]bool, len(*p.RequiredStatuses))
		for _, st := range *p.RequiredStatuses {
			if !seen[st] {
				seen[st] = true
				out.RequiredStatuses = append(out.RequiredStatuses, string(st))
			}
		}
	}
	return out
}

// ScoringFromPublic — правила из правки. requiredFields (верхнеуровневое поле
// ScenarioPatch.requiredFields) важнее scoring.requiredFields; nil — не прислано.
func ScoringFromPublic(p *public.Scoring, requiredFields *[]string) model.Scoring {
	var out model.Scoring
	if p != nil {
		out.RequiredFields = nonEmptyStrings(SliceFromPtr(p.RequiredFields))
		out.RequiredFacts = nonEmptyStrings(SliceFromPtr(p.RequiredFacts))
		out.ForbiddenFacts = nonEmptyStrings(SliceFromPtr(p.ForbiddenFacts))
		if p.FieldWeights != nil && len(*p.FieldWeights) > 0 {
			out.FieldWeights = make(map[string]float64, len(*p.FieldWeights))
			for k, v := range *p.FieldWeights {
				if k = strings.TrimSpace(k); k != "" {
					out.FieldWeights[k] = float64(v)
				}
			}
		}
		out.Reaction = ReactionFromPublic(p.Reaction)
	}
	if requiredFields != nil {
		out.RequiredFields = dedupe(nonEmptyStrings(*requiredFields))
	} else {
		out.RequiredFields = dedupe(out.RequiredFields)
	}
	return out
}

// ScoringToContract — правила для ai-service (semantic: required/forbidden facts).
func ScoringToContract(s *model.Scoring) components.Scoring {
	if s == nil {
		return components.Scoring{}
	}
	out := components.Scoring{
		RequiredFields: SlicePtr(copyStrings(s.RequiredFields)),
		RequiredFacts:  SlicePtr(copyStrings(s.RequiredFacts)),
		ForbiddenFacts: SlicePtr(copyStrings(s.ForbiddenFacts)),
	}
	if len(s.FieldWeights) > 0 {
		m := make(map[string]float32, len(s.FieldWeights))
		for k, v := range s.FieldWeights {
			m[k] = float32(v)
		}
		out.FieldWeights = &m
	}
	return out
}

// ScoringFromContract — правила из результата генерации (если LLM их прислала).
func ScoringFromContract(c *components.Scoring) model.Scoring {
	var out model.Scoring
	if c == nil {
		return out
	}
	out.RequiredFields = dedupe(nonEmptyStrings(SliceFromPtr(c.RequiredFields)))
	out.RequiredFacts = nonEmptyStrings(SliceFromPtr(c.RequiredFacts))
	out.ForbiddenFacts = nonEmptyStrings(SliceFromPtr(c.ForbiddenFacts))
	if c.FieldWeights != nil && len(*c.FieldWeights) > 0 {
		out.FieldWeights = make(map[string]float64, len(*c.FieldWeights))
		for k, v := range *c.FieldWeights {
			out.FieldWeights[k] = float64(v)
		}
	}
	return out
}

// ---------------------------------------------------------------- ExpectedAction

func ExpectedActionsToPublic(a []model.ExpectedAction) []public.ExpectedAction {
	out := make([]public.ExpectedAction, len(a))
	for i := range a {
		out[i] = public.ExpectedAction{
			ActionText:     a[i].ActionText,
			RequiredFacts:  SlicePtr(a[i].RequiredFacts),
			ForbiddenFacts: SlicePtr(a[i].ForbiddenFacts),
		}
	}
	return out
}

// ExpectedActionsFromPublic — пустые action_text отбрасываются.
func ExpectedActionsFromPublic(p []public.ExpectedAction) []model.ExpectedAction {
	out := make([]model.ExpectedAction, 0, len(p))
	for i := range p {
		t := strings.TrimSpace(p[i].ActionText)
		if t == "" {
			continue
		}
		out = append(out, model.ExpectedAction{
			ActionText:     t,
			RequiredFacts:  nonEmptyStrings(SliceFromPtr(p[i].RequiredFacts)),
			ForbiddenFacts: nonEmptyStrings(SliceFromPtr(p[i].ForbiddenFacts)),
		})
	}
	return out
}

func ExpectedActionsToContract(a []model.ExpectedAction) []components.ExpectedAction {
	out := make([]components.ExpectedAction, len(a))
	for i := range a {
		out[i] = components.ExpectedAction{
			ActionText:     a[i].ActionText,
			RequiredFacts:  SlicePtr(a[i].RequiredFacts),
			ForbiddenFacts: SlicePtr(a[i].ForbiddenFacts),
		}
	}
	return out
}

func ExpectedActionsFromContract(c []components.ExpectedAction) []model.ExpectedAction {
	out := make([]model.ExpectedAction, 0, len(c))
	for i := range c {
		t := strings.TrimSpace(c[i].ActionText)
		if t == "" {
			continue
		}
		out = append(out, model.ExpectedAction{
			ActionText:     t,
			RequiredFacts:  nonEmptyStrings(SliceFromPtr(c[i].RequiredFacts)),
			ForbiddenFacts: nonEmptyStrings(SliceFromPtr(c[i].ForbiddenFacts)),
		})
	}
	return out
}

// ---------------------------------------------------------------- ExpectedDialogue

func ExpectedDialogueToPublic(e *model.ExpectedDialogue) public.ExpectedDialogue {
	if e == nil {
		return public.ExpectedDialogue{Checklist: []public.DialogueChecklistItem{}}
	}
	out := public.ExpectedDialogue{
		Forbidden:        SlicePtr(e.Forbidden),
		MaxOperatorTurns: IntPtrIf(e.MaxOperatorTurns),
		Checklist:        make([]public.DialogueChecklistItem, len(e.Checklist)),
	}
	for i := range e.Checklist {
		it := &e.Checklist[i]
		out.Checklist[i] = public.DialogueChecklistItem{
			Id:       it.ID,
			Text:     it.Text,
			Kind:     public.DialogueChecklistItemKind(it.Kind),
			Required: it.Required,
			Weight:   f32Ptr(it.Weight),
			Hints:    SlicePtr(it.Hints),
		}
	}
	return out
}

// ExpectedDialogueFromPublic — чек-лист из правки: пункты без id/текста отбрасываются,
// неизвестный kind -> question.
func ExpectedDialogueFromPublic(p *public.ExpectedDialogue) model.ExpectedDialogue {
	if p == nil {
		return model.ExpectedDialogue{Checklist: []model.ChecklistItem{}}
	}
	out := model.ExpectedDialogue{
		Forbidden:        nonEmptyStrings(SliceFromPtr(p.Forbidden)),
		MaxOperatorTurns: Deref(p.MaxOperatorTurns),
		Checklist:        make([]model.ChecklistItem, 0, len(p.Checklist)),
	}
	for i := range p.Checklist {
		it := &p.Checklist[i]
		id, text := strings.TrimSpace(it.Id), strings.TrimSpace(it.Text)
		if id == "" || text == "" {
			continue
		}
		out.Checklist = append(out.Checklist, model.ChecklistItem{
			ID:       id,
			Text:     text,
			Kind:     normKind(string(it.Kind)),
			Required: it.Required,
			Weight:   f64Ptr(it.Weight),
			Hints:    nonEmptyStrings(SliceFromPtr(it.Hints)),
		})
	}
	return out
}

func ExpectedDialogueToContract(e *model.ExpectedDialogue) components.ExpectedDialogue {
	if e == nil {
		return components.ExpectedDialogue{Checklist: []components.DialogueChecklistItem{}}
	}
	out := components.ExpectedDialogue{
		Forbidden:        SlicePtr(e.Forbidden),
		MaxOperatorTurns: IntPtrIf(e.MaxOperatorTurns),
		Checklist:        make([]components.DialogueChecklistItem, len(e.Checklist)),
	}
	for i := range e.Checklist {
		it := &e.Checklist[i]
		out.Checklist[i] = components.DialogueChecklistItem{
			Id:       it.ID,
			Text:     it.Text,
			Kind:     components.DialogueChecklistItemKind(it.Kind),
			Required: it.Required,
			Weight:   f32Ptr(it.Weight),
			Hints:    SlicePtr(it.Hints),
		}
	}
	return out
}

func ExpectedDialogueFromContract(c *components.ExpectedDialogue) model.ExpectedDialogue {
	if c == nil {
		return model.ExpectedDialogue{Checklist: []model.ChecklistItem{}}
	}
	out := model.ExpectedDialogue{
		Forbidden:        nonEmptyStrings(SliceFromPtr(c.Forbidden)),
		MaxOperatorTurns: Deref(c.MaxOperatorTurns),
		Checklist:        make([]model.ChecklistItem, 0, len(c.Checklist)),
	}
	for i := range c.Checklist {
		it := &c.Checklist[i]
		id, text := strings.TrimSpace(it.Id), strings.TrimSpace(it.Text)
		if id == "" || text == "" {
			continue
		}
		out.Checklist = append(out.Checklist, model.ChecklistItem{
			ID:       id,
			Text:     text,
			Kind:     normKind(string(it.Kind)),
			Required: it.Required,
			Weight:   f64Ptr(it.Weight),
			Hints:    nonEmptyStrings(SliceFromPtr(it.Hints)),
		})
	}
	return out
}

// ---------------------------------------------------------------- мелочи

func f32Ptr(p *float64) *float32 {
	if p == nil {
		return nil
	}
	v := float32(*p)
	return &v
}

func f64Ptr(p *float32) *float64 {
	if p == nil {
		return nil
	}
	v := float64(*p)
	return &v
}

func normKind(s string) string {
	switch s {
	case model.ChecklistQuestion, model.ChecklistInstruction, model.ChecklistPhrase, model.ChecklistBehavior:
		return s
	}
	return model.ChecklistQuestion
}

// dedupe — без повторов, порядок первого вхождения (пути required_fields).
func dedupe(s []string) []string {
	if len(s) < 2 {
		return s
	}
	out := s[:0:0]
	for i, v := range s {
		dup := false
		for _, w := range s[:i] {
			if w == v {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, v)
		}
	}
	return out
}
