package convert

import (
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
)

// Результаты AI-слоёв (snake_case, как пришли от ai-service и лежат в evaluations) ->
// public для страницы результата. Массивы в ответе всегда [] (фронт не проверяет на null).

// publicForbiddenHit / publicPerField — анонимные элементы public-типов генератора.
type publicForbiddenHit = struct {
	Phrase string `json:"phrase"`
	TurnNo int    `json:"turnNo"`
}

type publicPerField = struct {
	Comment *string      `json:"comment,omitempty"`
	Field   string       `json:"field"`
	Score   public.Score `json:"score"`
}

// GrammarToPublic — evaluations.grammar_score + grammar_remarks + grammar_stats.
func GrammarToPublic(score float32, remarks []components.GrammarRemark, stats model.GrammarStats) public.GrammarResult {
	out := public.GrammarResult{
		Score:   score,
		Remarks: make([]public.GrammarRemark, len(remarks)),
	}
	for i := range remarks {
		r := &remarks[i]
		out.Remarks[i] = public.GrammarRemark{
			Field:       r.Field,
			Offset:      r.Offset,
			Length:      r.Length,
			Severity:    public.GrammarRemarkSeverity(r.Severity),
			Rule:        r.Rule,
			Message:     r.Message,
			Suggestions: NonNilSlicePtr(SliceFromPtr(r.Suggestions)),
		}
	}
	out.Stats.WordsChecked = stats.WordsChecked
	if len(stats.ErrorsBySeverity) > 0 {
		m := stats.ErrorsBySeverity
		out.Stats.ErrorsBySeverity = &m
	}
	return out
}

// SemanticToPublic — evaluations.semantic.
func SemanticToPublic(s components.SemanticResult) public.SemanticResult {
	out := public.SemanticResult{
		Score:             s.Score,
		Confidence:        s.Confidence,
		MissingFacts:      NonNilSlicePtr(SliceFromPtr(s.MissingFacts)),
		ExtraFacts:        NonNilSlicePtr(SliceFromPtr(s.ExtraFacts)),
		SummaryForStudent: NonEmptyPtr(s.SummaryForStudent),
	}
	src := SliceFromPtr(s.PerField)
	per := make([]publicPerField, len(src))
	for i := range src {
		per[i] = publicPerField{Field: src[i].Field, Score: src[i].Score, Comment: NonEmptyPtr(src[i].Comment)}
	}
	out.PerField = &per
	return out
}

// DialogueToPublic — evaluations.dialogue. Пункты чек-листа получают text/kind/required из
// эталона по id (фронт по id не ищет); evidence_turn_no и forbidden_hits.turn_no переводятся
// из сквозной нумерации транскрипта в номер обмена (SeqToExchange).
func DialogueToPublic(r components.DialogueResult, exp *model.ExpectedDialogue) public.DialogueResult {
	out := public.DialogueResult{
		Score:             r.Score,
		Confidence:        r.Confidence,
		SummaryForStudent: NonEmptyPtr(r.SummaryForStudent),
		MissingQuestions:  NonNilSlicePtr(SliceFromPtr(r.MissingQuestions)),
		Checklist:         make([]public.DialogueChecklistResult, len(r.Checklist)),
	}
	for i := range r.Checklist {
		c := &r.Checklist[i]
		item := public.DialogueChecklistResult{
			Id:      c.Id,
			Status:  public.DialogueChecklistResultStatus(c.Status),
			Comment: NonEmptyPtr(c.Comment),
			Text:    c.Id, // эталона нет (удалён/сменился) — хотя бы id, но не пустая строка
		}
		if c.EvidenceTurnNo != nil {
			n := SeqToExchange(*c.EvidenceTurnNo)
			item.EvidenceTurnNo = &n
		}
		if e, ok := exp.Item(c.Id); ok {
			item.Text = e.Text
			kind := public.DialogueChecklistResultKind(e.Kind)
			item.Kind = &kind
			req := e.Required
			item.Required = &req
		}
		out.Checklist[i] = item
	}

	hitsSrc := SliceFromPtr(r.ForbiddenHits)
	hits := make([]publicForbiddenHit, len(hitsSrc))
	for i := range hitsSrc {
		hits[i] = publicForbiddenHit{Phrase: hitsSrc[i].Phrase, TurnNo: SeqToExchange(hitsSrc[i].TurnNo)}
	}
	out.ForbiddenHits = &hits

	sp := &r.Speech
	out.Speech.OperatorTurns = sp.OperatorTurns
	out.Speech.OperatorWords = sp.OperatorWords
	out.Speech.OperatorTalkMs = sp.OperatorTalkMs
	out.Speech.WordsPerMin = sp.WordsPerMin
	out.Speech.FillerCount = sp.FillerCount
	out.Speech.Fillers = sp.Fillers
	out.Speech.AvgResponseMs = sp.AvgResponseMs
	out.Speech.MaxResponseMs = sp.MaxResponseMs
	out.Speech.LowConfidenceTurns = sp.LowConfidenceTurns

	if t := r.Tone; t != nil {
		out.Tone = &struct {
			Calmness   *public.Score `json:"calmness,omitempty"`
			Clarity    *public.Score `json:"clarity,omitempty"`
			Comment    *string       `json:"comment,omitempty"`
			Politeness *public.Score `json:"politeness,omitempty"`
		}{Calmness: t.Calmness, Clarity: t.Clarity, Comment: NonEmptyPtr(t.Comment), Politeness: t.Politeness}
	}
	return out
}
