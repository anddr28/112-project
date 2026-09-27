package main

import (
	"fmt"
	"math"
	"strings"
	"unicode"

	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/components"
)

// forbiddenHit — элемент DialogueResult.forbidden_hits (анонимная структура в gen).
type forbiddenHit = struct {
	Phrase string `json:"phrase"`
	TurnNo int    `json:"turn_no"`
}

// Слова-паразиты (countFillers): «ну», «эээ», «ээ», «э», «короче», «типа», «как бы», «вот», «значит».

var (
	politeMarkers = []string{"пожалуйста", "спасибо", "будьте добры", "извините", "здравствуйте", "добрый день",
		"добрый вечер", "доброе утро", "слушаю вас"}
	rudeMarkers    = []string{"заткнитесь", "не орите", "не кричите на меня", "дура", "идиот", "отстаньте", "быстрее говорите"}
	calmingMarkers = []string{"не волнуйтесь", "оставайтесь на связи", "все будет хорошо", "сохраняйте спокойствие",
		"помощь уже", "мы вам поможем"}
)

// evalDialogue — слой 5 (как computeDialogue мока фронта): чек-лист по подсказкам в
// репликах оператора, запрещённые фразы, детерминированные метрики речи, тон.
// Номера реплик (evidence_turn_no, forbidden_hits.turn_no) — сквозные номера
// транскрипта, как их прислал go-core; обратно в номера обменов переводит go-core.
func evalDialogue(req *aiservice.DialogueJobRequest) components.DialogueResult {
	floor := float32(0.6)
	if req.Options != nil && req.Options.SttConfidenceFloor != nil {
		floor = *req.Options.SttConfidenceFloor
	}

	type opTurn struct {
		turn    *components.DialogueTurn
		norm    string
		lowConf bool
	}
	ops := make([]opTurn, 0, len(req.Transcript)/2+1)
	for i := range req.Transcript {
		t := &req.Transcript[i]
		if t.Speaker != components.DialogueTurnSpeakerOperator {
			continue
		}
		ops = append(ops, opTurn{turn: t, norm: norm(t.Text), lowConf: t.Confidence != nil && *t.Confidence < floor})
	}

	// Запрещённые фразы — каждое вхождение (фраза × реплика).
	exp := &req.Etalon.ExpectedDialogue
	hits := make([]forbiddenHit, 0, 2)
	if exp.Forbidden != nil {
		for _, phrase := range *exp.Forbidden {
			p := norm(strings.TrimSpace(phrase))
			if p == "" {
				continue
			}
			for _, o := range ops {
				if strings.Contains(o.norm, p) {
					hits = append(hits, forbiddenHit{Phrase: phrase, TurnNo: o.turn.TurnNo})
				}
			}
		}
	}

	shouting, polite, rude, calming := false, false, 0, false
	for _, o := range ops {
		if isShouting(o.turn.Text) {
			shouting = true
		}
		if _, ok := containsAny(o.norm, politeMarkers); ok {
			polite = true
		}
		if isRude(o.norm) {
			rude++
		}
		if _, ok := containsAny(o.norm, calmingMarkers); ok {
			calming = true
		}
	}

	// Чек-лист.
	checklist := make([]components.DialogueChecklistResult, 0, len(exp.Checklist))
	missingQ := make([]string, 0, 2)
	missingReq := make([]string, 0, 2)
	var wSum, wDone float64
	for _, item := range exp.Checklist {
		res := components.DialogueChecklistResult{Id: item.Id}
		var credit float64
		if item.Kind == components.Behavior {
			switch {
			case len(ops) == 0:
				res.Status = components.NotApplicable
				res.Comment = ptr("Оператор не произнёс ни одной реплики — поведение не оценить.")
			case len(hits) > 0 || rude > 0:
				res.Status = components.Missed
				res.Comment = ptr("Прозвучали недопустимые фразы.")
			case shouting:
				res.Status = components.Missed
				res.Comment = ptr("В репликах слышен повышенный тон.")
			default:
				res.Status = components.Done
				res.Comment = ptr("Нарушений не выявлено.")
				credit = 1
			}
		} else {
			hints := normList(deref(item.Hints))
			if len(hints) == 0 {
				hints = meaningfulStems(item.Text)
			}
			for _, o := range ops {
				if _, ok := containsAny(o.norm, hints); ok {
					res.Status = components.Done
					res.EvidenceTurnNo = ptr(o.turn.TurnNo)
					c := "Выполнено: «" + snippet(o.turn.Text, 60) + "»."
					if o.lowConf {
						c += " Засчитано по реплике с низкой уверенностью распознавания."
					}
					res.Comment = ptr(c)
					credit = 1
					break
				}
			}
			if res.Status == "" {
				res.Status = components.Missed
				res.Comment = ptr("Не выполнено.")
			}
		}
		if res.Status != components.NotApplicable {
			w := 1.0
			if item.Weight != nil {
				w = float64(*item.Weight)
			}
			if item.Required {
				w *= 1.5
			}
			wSum += w
			wDone += w * credit
		}
		if res.Status == components.Missed && item.Required {
			missingReq = append(missingReq, item.Text)
			if item.Kind == components.Question {
				missingQ = append(missingQ, item.Text)
			}
		}
		checklist = append(checklist, res)
	}
	score := 0.0
	if wSum > 0 {
		score = wDone / wSum * 100
	}
	// Запрещённая фраза — замечание и в балле (как сделала бы LLM-оценка): −5 за каждую.
	score = math.Max(0, math.Round(score)-float64(5*len(hits)))

	speech := speechMetrics(req, floor)
	fillerCount := deref(speech.FillerCount)

	politeness := 70
	if polite {
		politeness = 90
	}
	politeness = clampInt(politeness-15*(rude+len(hits)), 0, 100)
	calmness := 85
	if shouting {
		calmness = 55
	} else if calming {
		calmness = 95
	}
	clarity := clampInt(92-7*fillerCount, 40, 95)
	if speech.OperatorTurns > 0 && speech.OperatorWords/speech.OperatorTurns > 30 {
		clarity = max(40, clarity-10) // слишком длинные реплики хуже воспринимаются на линии
	}
	toneComment := "Обращение вежливое, формулировки понятные."
	if !polite {
		toneComment = "Не хватает вежливых формулировок при обращении к заявителю."
	}
	if shouting {
		toneComment += " В отдельных репликах слышен повышенный тон."
	}
	if fillerCount > 3 {
		toneComment += " Много слов-паразитов."
	}

	conf := float32(0.8)
	if len(ops) == 0 {
		conf = 0.55 // оценивать нечего — пусть посмотрит преподаватель
	}

	return components.DialogueResult{
		Score:            float32(score),
		Confidence:       conf,
		Checklist:        checklist,
		MissingQuestions: &missingQ,
		ForbiddenHits:    &hits,
		Speech:           speech,
		Tone: &components.ToneAssessment{
			Politeness: ptr(float32(politeness)),
			Calmness:   ptr(float32(calmness)),
			Clarity:    ptr(float32(clarity)),
			Comment:    ptr(toneComment),
		},
		SummaryForStudent: ptr(dialogueSummary(missingReq, hits, fillerCount, speech)),
	}
}

// speechMetrics — детерминированные метрики речи оператора (контракт: без LLM,
// считаются всегда).
func speechMetrics(req *aiservice.DialogueJobRequest, floor float32) components.SpeechMetrics {
	var turns, words, talkMs, lowConf int
	fillers := map[string]int{}
	gaps := make([]int, 0, 8)
	var prev *components.DialogueTurn
	for i := range req.Transcript {
		t := &req.Transcript[i]
		if t.Speaker != components.DialogueTurnSpeakerOperator {
			prev = t
			continue
		}
		turns++
		w := countWords(t.Text)
		words += w
		if t.AudioDurationMs != nil && *t.AudioDurationMs > 0 {
			talkMs += *t.AudioDurationMs
		} else {
			talkMs += w * 400 // введено текстом — оцениваем по среднему темпу 150 слов/мин
		}
		if t.Confidence != nil && *t.Confidence < floor {
			lowConf++
		}
		countFillers(norm(t.Text), fillers)
		// Пауза: от конца реплики заявителя (at_ms + длительность озвучки) до реплики оператора.
		if prev != nil && prev.Speaker == components.DialogueTurnSpeakerCaller && prev.AtMs != nil && t.AtMs != nil {
			end := *prev.AtMs + deref(prev.AudioDurationMs)
			gaps = append(gaps, max(0, *t.AtMs-end))
		}
		prev = t
	}
	fillerCount := 0
	for _, n := range fillers {
		fillerCount += n
	}
	wpm := 0.0
	if talkMs > 0 {
		wpm = math.Round(float64(words)/(float64(talkMs)/60000)*10) / 10
	}
	avg, mx := 0, 0
	if len(gaps) > 0 {
		sum := 0
		for _, g := range gaps {
			sum += g
			mx = max(mx, g)
		}
		avg = sum / len(gaps)
	}
	return components.SpeechMetrics{
		OperatorTurns:      turns,
		OperatorWords:      words,
		OperatorTalkMs:     ptr(talkMs),
		WordsPerMin:        ptr(float32(wpm)),
		FillerCount:        ptr(fillerCount),
		Fillers:            &fillers,
		AvgResponseMs:      ptr(avg),
		MaxResponseMs:      ptr(mx),
		LowConfidenceTurns: ptr(lowConf),
	}
}

// countFillers — паразиты как отдельные слова; «э/ээ/эээ» различаем по длине
// (протяжное «ээээ» — это «эээ»), «как бы» — два слова подряд.
func countFillers(normText string, into map[string]int) {
	toks := tokens(normText)
	for i, t := range toks {
		switch {
		case allRune(t, 'э'):
			switch n := len([]rune(t)); {
			case n == 1:
				into["э"]++
			case n == 2:
				into["ээ"]++
			default:
				into["эээ"]++
			}
		case t == "ну" || t == "короче" || t == "типа" || t == "вот" || t == "значит":
			into[t]++
		case t == "как" && i+1 < len(toks) && toks[i+1] == "бы":
			into["как бы"]++
		}
	}
}

func allRune(s string, want rune) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r != want {
			return false
		}
	}
	return true
}

// isRude — грубость в реплике (текст нормализован). Однословные маркеры ищутся как
// начало слова: «дура» не должна находиться в «процедура», а «идиот» — находиться в
// «идиотка»; фразы («не орите») — подстрокой.
func isRude(normText string) bool {
	var toks []string
	for _, m := range rudeMarkers {
		if strings.Contains(m, " ") {
			if strings.Contains(normText, m) {
				return true
			}
			continue
		}
		if toks == nil {
			toks = tokens(normText)
		}
		for _, t := range toks {
			if strings.HasPrefix(t, m) {
				return true
			}
		}
	}
	return false
}

// isShouting — реплика целиком капсом (≥10 букв) или с «!!» — «крик» в тексте.
func isShouting(s string) bool {
	if strings.Contains(s, "!!") {
		return true
	}
	letters := 0
	for _, r := range s {
		if unicode.IsLetter(r) {
			if unicode.IsLower(r) {
				return false
			}
			letters++
		}
	}
	return letters >= 10
}

func dialogueSummary(missingReq []string, hits []forbiddenHit, fillers int, sp components.SpeechMetrics) string {
	var b strings.Builder
	if sp.OperatorTurns == 0 {
		return "Разговор с заявителем не состоялся: реплик оператора нет."
	}
	switch len(missingReq) {
	case 0:
		b.WriteString("Протокол опроса выполнен: все обязательные пункты отмечены.")
	case 1:
		b.WriteString("Не выполнен обязательный пункт: «" + missingReq[0] + "».")
	default:
		fmt.Fprintf(&b, "Не выполнено обязательных пунктов: %d (например, «%s»).", len(missingReq), missingReq[0])
	}
	switch {
	case len(hits) > 0:
		b.WriteString(" Прозвучали недопустимые фразы: «" + hits[0].Phrase + "».")
	case fillers > 0:
		fmt.Fprintf(&b, " Слов-паразитов в речи: %d — старайтесь говорить короче и чётче.", fillers)
	default:
		b.WriteString(" Речь чёткая, без слов-паразитов.")
	}
	if avg := deref(sp.AvgResponseMs); avg > 5000 {
		fmt.Fprintf(&b, " Паузы перед ответом длинные (в среднем %.1f с) — отвечайте заявителю быстрее.", float64(avg)/1000)
	}
	return b.String()
}

func clampInt(v, lo, hi int) int { return min(max(v, lo), hi) }
