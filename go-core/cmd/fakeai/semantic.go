package main

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/components"
)

// evalSemantic — слой 3 (как computeSemantic в моке фронта): доля обязательных
// фактов эталона, отражённых в свободном тексте ответа. Факт засчитан, если в тексте
// нашлись основы ≥40% его значимых слов. Запрещённые факты (домыслы) снижают балл.
func evalSemantic(req *aiservice.SemanticJobRequest) components.SemanticResult {
	fields := fieldTexts(req)
	var sb strings.Builder
	for _, f := range fields {
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(f.text)
	}
	combined := strings.TrimSpace(sb.String())
	normText := norm(combined)
	textToks := tokens(normText)

	required, forbidden := semanticFacts(req)
	missing := make([]string, 0, len(required))
	for _, f := range required {
		if !factCovered(normText, textToks, f) {
			missing = append(missing, f)
		}
	}
	extra := make([]string, 0, 2)
	for _, f := range forbidden {
		if forbiddenPresent(textToks, f) {
			extra = append(extra, f)
		}
	}
	covered := len(required) - len(missing)

	var score int
	switch {
	case combined == "":
		score = 0
	case len(required) == 0:
		score = 100
	default:
		score = int(math.Round(float64(covered) / float64(len(required)) * 100))
	}
	// Домысел — существенная ошибка карточки: −10 за каждый.
	score = max(0, score-10*len(extra))

	conf := float32(0.86)
	if utf8.RuneCountInString(combined) < 40 {
		// Слишком мало текста — модель не уверена, go-core пометит «требует ревью».
		conf = 0.58
	}

	perField := make([]components.SemanticPerField, 0, len(fields))
	for _, f := range fields {
		if strings.TrimSpace(f.text) == "" {
			continue
		}
		if f.name == "actions_taken" && req.Mode == aiservice.SemanticJobRequestModeCards {
			perField = append(perField, actionsPerField(f.text))
			continue
		}
		comment := "Ключевые обстоятельства происшествия переданы полностью."
		if len(missing) > 0 {
			comment = "Не зафиксировано: " + strings.Join(missing, "; ") + "."
		}
		perField = append(perField, components.SemanticPerField{Field: f.name, Score: float32(score), Comment: ptr(comment)})
	}

	summary := semanticSummary(combined, covered, len(required), missing)
	if len(extra) > 0 {
		summary += " В тексте есть сведения, которых заявитель не сообщал: " + extra[0] + "."
	}
	return components.SemanticResult{
		Score:             float32(score),
		Confidence:        conf,
		MissingFacts:      &missing,
		ExtraFacts:        &extra,
		PerField:          &perField,
		SummaryForStudent: ptr(summary),
	}
}

type fieldText struct{ name, text string }

// fieldTexts — свободный текст ответа по free_text_fields: карточка (режим 1)
// или action_text (режим 2). Пустой список полей — берём все свободнотекстовые.
func fieldTexts(req *aiservice.SemanticJobRequest) []fieldText {
	names := req.FreeTextFields
	if len(names) == 0 {
		if req.Mode == aiservice.SemanticJobRequestModeCardActions {
			names = []string{"action_text"}
		} else {
			names = []string{"description", "actions_taken"}
		}
	}
	card := req.Answer.Card
	out := make([]fieldText, 0, len(names))
	for _, n := range names {
		var t string
		switch n {
		case "description":
			if card != nil {
				t = deref(card.Description)
			}
		case "actions_taken":
			if card != nil {
				t = deref(card.ActionsTaken)
			}
		case "action_text":
			t = deref(req.Answer.ActionText)
		case "answer_turns", "turns":
			if req.Answer.Turns != nil {
				parts := make([]string, 0, len(*req.Answer.Turns))
				for _, at := range *req.Answer.Turns {
					parts = append(parts, at.AnswerText)
				}
				t = strings.Join(parts, "\n")
			}
		default:
			// Произвольное поле опросной карты со свободным текстом.
			if card != nil && card.Attributes != nil {
				if s, ok := (*card.Attributes)[n].(string); ok {
					t = s
				}
			}
		}
		out = append(out, fieldText{name: n, text: t})
	}
	// Режим 2 без action_text, но с карточкой — оцениваем то, что есть.
	if req.Mode == aiservice.SemanticJobRequestModeCardActions && card != nil {
		empty := true
		for _, f := range out {
			if strings.TrimSpace(f.text) != "" {
				empty = false
				break
			}
		}
		if empty {
			out = append(out, fieldText{"description", deref(card.Description)}, fieldText{"actions_taken", deref(card.ActionsTaken)})
		}
	}
	return out
}

// semanticFacts — обязательные факты: (режим 2) expected_actions[].required_facts →
// scoring.required_facts → call_script.key_facts. Режим 2 оценивает текст действий,
// поэтому его факты — из эталонных действий, как в ai-service (PY-05: «card_actions:
// required_facts берутся из expected_actions[].required_facts»); scoring.required_facts
// go-core заполняет key_facts легенды — это обстоятельства звонка, а не действия.
// Запрещённые — из scoring и expected_actions, без дублей.
func semanticFacts(req *aiservice.SemanticJobRequest) (required, forbidden []string) {
	et := &req.Etalon
	if req.Mode == aiservice.SemanticJobRequestModeCardActions && et.ExpectedActions != nil {
		for _, a := range *et.ExpectedActions {
			if a.RequiredFacts != nil {
				required = append(required, *a.RequiredFacts...)
			}
		}
	}
	if len(required) == 0 && et.Scoring != nil && et.Scoring.RequiredFacts != nil {
		required = append(required, *et.Scoring.RequiredFacts...)
	}
	if len(required) == 0 && req.CallScript != nil && req.CallScript.KeyFacts != nil {
		required = append(required, *req.CallScript.KeyFacts...)
	}
	seen := map[string]bool{}
	if et.Scoring != nil && et.Scoring.ForbiddenFacts != nil {
		for _, f := range *et.Scoring.ForbiddenFacts {
			if !seen[f] {
				seen[f] = true
				forbidden = append(forbidden, f)
			}
		}
	}
	if et.ExpectedActions != nil {
		for _, a := range *et.ExpectedActions {
			if a.ForbiddenFacts == nil {
				continue
			}
			for _, f := range *a.ForbiddenFacts {
				if !seen[f] {
					seen[f] = true
					forbidden = append(forbidden, f)
				}
			}
		}
	}
	return dedupeNonEmpty(required), forbidden
}

func dedupeNonEmpty(in []string) []string {
	out := in[:0]
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		if strings.TrimSpace(s) == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// factCovered — эвристика мока фронта: основы значимых слов факта ищутся в тексте,
// числа — как цифрой, так и словом («5» ~ «пятом»). Факт без значимых слов засчитан.
func factCovered(normText string, textToks []string, fact string) bool {
	total, hits := 0, 0
	for _, w := range tokens(norm(fact)) {
		if isNumber(w) {
			total++
			if numberInText(normText, textToks, w) {
				hits++
			}
			continue
		}
		if utf8.RuneCountInString(w) < 4 {
			continue
		}
		total++
		if strings.Contains(normText, stem(w)) {
			hits++
		}
	}
	if total == 0 {
		return true
	}
	return float64(hits)/float64(total) >= 0.4
}

func numberInText(normText string, textToks []string, num string) bool {
	for _, t := range textToks {
		if t == num {
			return true
		}
	}
	for _, s := range numberStems[num] {
		for _, t := range textToks {
			if strings.HasPrefix(t, s) {
				return true
			}
		}
	}
	return false
}

// forbiddenPresent — домысел засчитывается только без отрицания рядом:
// «открытого пламени не видно» не то же самое, что «открытое пламя».
func forbiddenPresent(textToks []string, fact string) bool {
	total, hits := 0, 0
	for _, w := range tokens(norm(fact)) {
		if utf8.RuneCountInString(w) < 4 {
			continue
		}
		total++
		st := stem(w)
		for i, t := range textToks {
			if strings.HasPrefix(t, st) {
				if negated(textToks, i) {
					return false
				}
				hits++
				break
			}
		}
	}
	return total > 0 && float64(hits)/float64(total) >= 0.6
}

func negated(toks []string, i int) bool {
	for d := 1; d <= 2; d++ {
		if i-d >= 0 {
			switch toks[i-d] {
			case "не", "нет", "без", "ни":
				return true
			}
		}
	}
	if i+1 < len(toks) && (toks[i+1] == "не" || toks[i+1] == "нет") {
		return true
	}
	return false
}

// actionMarkers — признаки того, что в «Принятых мерах» описаны действия оператора.
var actionMarkers = []string{"направ", "передан", "переда", "вызва", "сообщ", "оповещ", "выслан", "служб", "бригад", "рекоменд"}

func actionsPerField(text string) components.SemanticPerField {
	if _, ok := containsAny(norm(text), actionMarkers); ok {
		return components.SemanticPerField{Field: "actions_taken", Score: 100,
			Comment: ptr("Принятые меры описаны: указано направление служб и данные указания.")}
	}
	return components.SemanticPerField{Field: "actions_taken", Score: 60,
		Comment: ptr("Не указано, какие службы направлены и какие указания даны заявителю.")}
}

// semanticSummary — текст строится по фактическому покрытию (как semanticSummary мока).
func semanticSummary(text string, covered, total int, missing []string) string {
	switch {
	case strings.TrimSpace(text) == "":
		return "Описание со слов заявителя не заполнено, поэтому смысловую полноту оценить не по чему."
	case total == 0:
		return "Для этого сценария существенные факты не заданы."
	case covered == 0:
		return fmt.Sprintf("Ни один из %d существенных фактов обращения в описании не зафиксирован.", total)
	case len(missing) == 0:
		return "Описание полное: все существенные обстоятельства из обращения зафиксированы."
	default:
		return fmt.Sprintf("Зафиксировано %d из %d существенных фактов. Не отражено: %s.", covered, total, missing[0])
	}
}
