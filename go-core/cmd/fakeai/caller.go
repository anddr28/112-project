package main

import (
	"strings"

	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/components"
)

// Настроение заявителя: от него зависят формулировки (LLM играет роль по
// emotional_state; здесь — три набора фраз).
type mood uint8

const (
	moodAgitated mood = iota // встревожен / взволнован
	moodPanic
	moodCalm
)

func moodOf(state string) mood {
	s := norm(state)
	switch {
	case s == "":
		return moodAgitated
	case strings.Contains(s, "паник") || strings.Contains(s, "испуг") || strings.Contains(s, "напуган") ||
		strings.Contains(s, "истер") || strings.Contains(s, "плач") || strings.Contains(s, "крич"):
		return moodPanic
	case mentionsCalm(s) || strings.Contains(s, "рассудит") || strings.Contains(s, "сдержан") ||
		strings.Contains(s, "облегч"):
		return moodCalm
	default:
		return moodAgitated
	}
}

// mentionsCalm — «спокойна/спокойный/спокойно» и «спокоен» (не содержит «спокой»: о/е —
// а именно так emotionFor пишет состояние заявителя-мужчины); «беспокойна/беспокоится» —
// тревога, а не спокойствие. s уже нормализована (norm).
func mentionsCalm(s string) bool {
	return (strings.Contains(s, "спокой") || strings.Contains(s, "спокоен")) && !strings.Contains(s, "беспоко")
}

var (
	moodPrefix = [3][]string{
		moodAgitated: {"Да. ", "Слушайте. ", ""},
		moodPanic:    {"Ой! ", "Господи! ", "Да-да! "},
		moodCalm:     {"Да. ", "Так. ", ""},
	}
	moodSuffix = [3][]string{
		moodAgitated: {" Поторопитесь, пожалуйста.", "", ""},
		moodPanic:    {" Быстрее, пожалуйста!", " Приезжайте скорее!", ""},
		moodCalm:     {"", " Записывайте.", ""},
	}
	panicLines = [3][]string{
		moodAgitated: {"Вы что-то ещё хотите уточнить? Я на месте.", "Слушаю вас, что ещё сказать?", "Пожалуйста, пришлите кого-нибудь побыстрее."},
		moodPanic:    {"Я не знаю, что делать! Приезжайте скорее!", "Пожалуйста, побыстрее, мне страшно!", "Вы меня слышите? Что мне делать-то?"},
		moodCalm:     {"Да, я вас слушаю.", "Что ещё нужно уточнить?", "Я на месте, жду."},
	}
	unknownLines = [3]string{
		moodAgitated: "Не знаю, не вижу…",
		moodPanic:    "Ой, не знаю я! Не вижу отсюда…",
		moodCalm:     "Этого я не знаю, отсюда не видно.",
	}
	thanksLines = [3]string{
		moodAgitated: "Хорошо, спасибо! Жду, встречу их на месте.",
		moodPanic:    "Спасибо! Я буду ждать и встречу их. Только быстрее, пожалуйста!",
		moodCalm:     "Спасибо, ждём. До свидания.",
	}
	// Оператор сообщил, что помощь направлена, — условие конца разговора.
	dispatchMarkers = []string{"направлен", "направля", "выехал", "выезжа", "выслал", "высыла", "отправил",
		"отправля", "едут", "в пути", "уже едет"}
	calmingPhrases = []string{"не волнуйтесь", "успокойтесь", "все будет хорошо", "оставайтесь на связи",
		"сохраняйте спокойствие"}
)

const (
	defaultMaxReplyWords = 40
	defaultMaxTurns      = 12
	maxFactsPerReply     = 2
	maxTurnsGoodbye      = "Всё, я больше не могу говорить! Приезжайте скорее, пожалуйста!"
)

// callerReply — ответ «LLM-заявителя» (как callerReply мока фронта, но по контракту
// DialogueBrief). Порядок: факты, о которых спросили (reveal≠never, ещё не раскрыты,
// подсказки встретились в реплике) → вопрос о неизвестном («не знаю») → «помощь
// направлена» после ≥2 реплик оператора (should_end) → факт, который заявитель
// сообщает сам (volunteer) → реплики по кругу. TTS здесь не делается.
func callerReply(req *aiservice.DialogTurnRequest, operatorText string) components.CallerReply {
	cs := &req.CallScript
	text := norm(operatorText)
	state := strings.TrimSpace(deref(cs.Caller.EmotionalState))
	md := moodOf(state)
	v := max(req.TurnNo, 1)

	opTurns := 1 // текущая реплика оператора
	for i := range req.History {
		if req.History[i].Speaker == components.DialogueTurnSpeakerOperator {
			opTurns++
		}
	}
	revealed := make(map[string]bool, 8)
	if req.RevealedFactIds != nil {
		for _, id := range *req.RevealedFactIds {
			revealed[id] = true
		}
	}
	// go-core присылает агрегат, но и сама история несёт revealed_fact_ids — страхуемся.
	for i := range req.History {
		if ids := req.History[i].RevealedFactIds; ids != nil {
			for _, id := range *ids {
				revealed[id] = true
			}
		}
	}

	maxWords := defaultMaxReplyWords
	if req.Options != nil && req.Options.MaxReplyWords != nil && *req.Options.MaxReplyWords > 0 {
		maxWords = *req.Options.MaxReplyWords
	}
	brief := cs.Dialogue
	maxTurns := defaultMaxTurns
	if brief != nil && brief.MaxTurns != nil && *brief.MaxTurns > 0 {
		maxTurns = *brief.MaxTurns
	}

	if md == moodPanic {
		if _, ok := containsAny(text, calmingPhrases); ok {
			state = "успокаивается"
		}
	}

	newIDs := make([]string, 0, maxFactsPerReply)
	var out string
	unknown, end := false, false
	endReason := ""

	if brief != nil {
		var facts []*components.DialogueFact
		for i := range brief.Facts {
			f := &brief.Facts[i]
			if f.Reveal == components.Never || revealed[f.Id] {
				continue
			}
			hints := normList(deref(f.Hints))
			if len(hints) == 0 {
				hints = meaningfulStems(f.Text)
			}
			if _, ok := containsAny(text, hints); ok {
				facts = append(facts, f)
				if len(facts) == maxFactsPerReply {
					break
				}
			}
		}
		if len(facts) > 0 {
			out = factReply(facts, md, v)
			for _, f := range facts {
				newIDs = append(newIDs, f.Id)
			}
		} else if askedUnknown(deref(brief.Unknowns), text) {
			out = unknownLines[md]
			unknown = true
		}
	}

	if out == "" && opTurns >= 2 {
		if _, ok := containsAny(text, dispatchMarkers); ok {
			out = thanksLines[md]
			end = true
			endReason = "оператор сообщил, что помощь направлена"
			if brief != nil && brief.EndConditions != nil && len(*brief.EndConditions) > 0 {
				endReason = (*brief.EndConditions)[0]
			}
			// Спокойный заявитель остаётся в своём состоянии (в т.ч. в своём роде:
			// «спокойна» не превращается в «спокоен»), остальным становится легче.
			if md != moodCalm {
				state = "облегчение"
			}
		}
	}

	if out == "" && brief != nil {
		for i := range brief.Facts {
			f := &brief.Facts[i]
			if f.Reveal == components.Volunteer && !revealed[f.Id] {
				out = factReply([]*components.DialogueFact{f}, md, v)
				newIDs = append(newIDs, f.Id)
				break
			}
		}
	}

	if out == "" {
		out = fallbackLine(cs, md, opTurns)
	}

	if !end && opTurns >= maxTurns {
		out = maxTurnsGoodbye
		end = true
		endReason = "max_turns"
		unknown = false
		newIDs = newIDs[:0]
	}

	reply := components.CallerReply{
		Text:            truncateWords(out, maxWords),
		RevealedFactIds: &newIDs,
		IsUnknownAnswer: ptr(unknown),
		ShouldEnd:       ptr(end),
	}
	if state != "" {
		reply.EmotionalState = ptr(state)
	}
	if end {
		reply.EndReason = ptr(endReason)
	}
	return reply
}

// factReply — реплика из текстов фактов с эмоциональной окраской по настроению.
func factReply(facts []*components.DialogueFact, md mood, v int) string {
	var b strings.Builder
	b.WriteString(moodPrefix[md][v%len(moodPrefix[md])])
	for i, f := range facts {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(endSentence(capitalize(strings.TrimSpace(f.Text))))
	}
	b.WriteString(moodSuffix[md][v%len(moodSuffix[md])])
	return b.String()
}

// askedUnknown — вопрос о том, чего заявитель не знает (brief.unknowns): найдена
// хотя бы половина основ значимых слов пункта.
func askedUnknown(unknowns []string, text string) bool {
	for _, u := range unknowns {
		st := meaningfulStems(u)
		if len(st) == 0 {
			continue
		}
		hits := 0
		for _, s := range st {
			if strings.Contains(text, s) {
				hits++
			}
		}
		if hits > 0 && hits*2 >= len(st) {
			return true
		}
	}
	return false
}

// fallbackLine — ничего по существу не спросили. Без брифа — сценарные реплики
// заявителя turns[1:] по кругу; с брифом — «паника» по настроению.
func fallbackLine(cs *components.CallScript, md mood, opTurns int) string {
	if cs.Dialogue == nil {
		lines := make([]string, 0, len(cs.Turns))
		for i := 1; i < len(cs.Turns); i++ {
			if cs.Turns[i].Speaker == components.CallScriptTurnsSpeakerCaller && strings.TrimSpace(cs.Turns[i].Text) != "" {
				lines = append(lines, cs.Turns[i].Text)
			}
		}
		if len(lines) > 0 {
			return lines[(opTurns-1)%len(lines)]
		}
	}
	pl := panicLines[md]
	return pl[(opTurns-1)%len(pl)]
}

// sttPhrases — «распознанные» реплики оператора, когда пришло только аудио: протокол
// приёма вызова по порядку, чтобы демо без Python шло по сценарию (адрес → люди →
// телефон → «помощь направлена»).
var sttPhrases = []string{
	"Служба 112, слушаю вас. Что у вас случилось?",
	"Назовите точный адрес: улица, дом, подъезд, этаж.",
	"Есть ли пострадавшие? Люди внутри остались?",
	"Назовите, пожалуйста, ваш контактный телефон.",
	"Помощь направлена, бригада уже выехала. Оставайтесь на связи.",
}

// fakeSTT — «распознавание» по номеру хода: реплика из sttPhrases по кругу,
// confidence 0.5 (ниже порога 0.6 — оценка разговора не штрафует за содержание).
// Аудио короче 200 байт — тишина (no_speech).
func fakeSTT(turnNo int, a audioInfo) components.SttResult {
	if a.size < minSpeechBytes {
		return components.SttResult{Text: "", Confidence: ptr(float32(0)), AudioDurationMs: a.durationMs,
			NoSpeech: ptr(true), Language: ptr("ru"), Segments: &[]components.SttSegment{}}
	}
	phrase := sttPhrases[(max(turnNo, 1)-1)%len(sttPhrases)]
	return components.SttResult{
		Text:            phrase,
		TextRaw:         ptr(rawASR(phrase)),
		Confidence:      ptr(float32(0.5)),
		AudioDurationMs: a.durationMs,
		NoSpeech:        ptr(false),
		Language:        ptr("ru"),
		Segments: &[]components.SttSegment{{
			StartMs: 0, EndMs: a.durationMs, Text: phrase, Confidence: ptr(float32(0.5)),
		}},
	}
}

// rawASR — «сырой» выход модели: нижний регистр, без пунктуации, числа словами.
func rawASR(s string) string {
	s = strings.ReplaceAll(s, "112", "сто двенадцать")
	return strings.Join(tokens(strings.ToLower(s)), " ")
}
