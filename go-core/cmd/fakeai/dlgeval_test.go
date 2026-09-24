package main

import (
	"reflect"
	"strings"
	"testing"

	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/components"
)

func dlgReq(t *testing.T, p obj) *aiservice.DialogueJobRequest {
	t.Helper()
	body := mustJSON(t, p)
	top, err := decodeTop(body, true)
	if err != nil {
		t.Fatal(err)
	}
	pj, err := parseJob(kindDialogue, body, top) // тот же путь проверки, что у хендлера
	if err != nil {
		t.Fatalf("parseJob: %v", err)
	}
	return pj.payload.(*aiservice.DialogueJobRequest)
}

// dlgWithOps — dialoguePayload с репликами оператора (заявитель между ними).
func dlgWithOps(t *testing.T, ops ...obj) obj {
	t.Helper()
	p := dialoguePayload()
	tr := []any{obj{"turn_no": 1, "speaker": "caller", "text": "Алло! Дым!", "at_ms": 0}}
	n := 2
	for _, o := range ops {
		o["turn_no"], o["speaker"] = n, "operator"
		tr = append(tr, o, obj{"turn_no": n + 1, "speaker": "caller", "text": "Да.", "at_ms": deref2(o["at_ms"]) + 3000})
		n += 2
	}
	p["transcript"] = tr
	return p
}

func deref2(v any) int {
	if f, ok := v.(int); ok {
		return f
	}
	return 0
}

func statuses(res components.DialogueResult) map[string]components.DialogueChecklistResultStatus {
	m := map[string]components.DialogueChecklistResultStatus{}
	for _, c := range res.Checklist {
		m[c.Id] = c.Status
	}
	return m
}

// Пример из контракта (ai-service.v1.yaml, fire_dialogue): полностью посчитанный результат.
func TestEvalDialogueContractExample(t *testing.T) {
	t.Parallel()
	res := evalDialogue(dlgReq(t, dialoguePayload()))

	want := map[string]components.DialogueChecklistResultStatus{
		"ask_address": components.Done, "ask_people": components.Missed, "ask_phone": components.Missed,
		"say_dispatched": components.Done, "calm": components.Done,
	}
	if got := statuses(res); !reflect.DeepEqual(got, want) {
		t.Fatalf("checklist %v", got)
	}
	// Порядок пунктов — как в эталоне; доказательства — сквозные номера транскрипта.
	ids := []string{}
	for _, c := range res.Checklist {
		ids = append(ids, c.Id)
		if c.Comment == nil || *c.Comment == "" {
			t.Errorf("%s без комментария", c.Id)
		}
	}
	if !reflect.DeepEqual(ids, []string{"ask_address", "ask_people", "ask_phone", "say_dispatched", "calm"}) {
		t.Fatalf("порядок %v", ids)
	}
	if deref(res.Checklist[0].EvidenceTurnNo) != 2 || deref(res.Checklist[3].EvidenceTurnNo) != 4 || res.Checklist[1].EvidenceTurnNo != nil {
		t.Fatalf("evidence: %+v", res.Checklist)
	}
	// Веса: обязательные ×1.5 → (1.5+1.5+1.5)/(1.5+1.5+1+1.5+1.5) = 64%.
	if res.Score != 64 || res.Confidence != 0.8 {
		t.Fatalf("score %v conf %v", res.Score, res.Confidence)
	}
	if !reflect.DeepEqual(*res.MissingQuestions, []string{"Спросил, есть ли люди в квартире"}) || len(*res.ForbiddenHits) != 0 {
		t.Fatalf("missing %v hits %v", *res.MissingQuestions, *res.ForbiddenHits)
	}
	sp := res.Speech
	if sp.OperatorTurns != 2 || sp.OperatorWords != 13 || deref(sp.OperatorTalkMs) != 6000 || deref(sp.WordsPerMin) != 130 ||
		deref(sp.AvgResponseMs) != 4750 || deref(sp.MaxResponseMs) != 5400 || deref(sp.FillerCount) != 0 ||
		deref(sp.LowConfidenceTurns) != 0 || sp.Fillers == nil {
		t.Fatalf("speech %+v", sp)
	}
	tone := res.Tone
	if deref(tone.Politeness) != 70 || deref(tone.Calmness) != 95 || deref(tone.Clarity) != 92 {
		t.Fatalf("tone %+v", tone)
	}
	wantSummary := "Не выполнен обязательный пункт: «Спросил, есть ли люди в квартире». Речь чёткая, без слов-паразитов."
	if deref(res.SummaryForStudent) != wantSummary {
		t.Fatalf("summary %q", deref(res.SummaryForStudent))
	}
	// Детерминизм.
	if again := evalDialogue(dlgReq(t, dialoguePayload())); !reflect.DeepEqual(again, res) {
		t.Fatal("недетерминированная оценка")
	}
}

func TestEvalDialogueForbiddenAndRude(t *testing.T) {
	t.Parallel()
	p := dlgWithOps(t,
		obj{"text": "Какой подъезд и этаж? Есть люди внутри?", "at_ms": 3000},
		obj{"text": "Ждите. Перезвоните позже, помощь направлена.", "at_ms": 9000},
	)
	res := evalDialogue(dlgReq(t, p))
	hits := *res.ForbiddenHits
	if len(hits) != 2 || hits[0].Phrase != "перезвоните позже" || hits[0].TurnNo != 4 || hits[1].Phrase != "ждите" || hits[1].TurnNo != 4 {
		t.Fatalf("forbidden_hits %+v", hits)
	}
	st := statuses(res)
	if st["calm"] != components.Missed || st["ask_people"] != components.Done {
		t.Fatalf("checklist %v", st)
	}
	// done: address 1.5 + people 1.5 + dispatched 1.5 = 4.5 из 7 → 64 − 5×2.
	if res.Score != 54 {
		t.Fatalf("score %v", res.Score)
	}
	if deref(res.Tone.Politeness) != 70-15*2 {
		t.Fatalf("politeness %v", deref(res.Tone.Politeness))
	}
	if !strings.Contains(deref(res.SummaryForStudent), "Прозвучали недопустимые фразы: «перезвоните позже»") {
		t.Fatalf("summary %q", deref(res.SummaryForStudent))
	}

	rude := dlgWithOps(t, obj{"text": "Не орите, говорите адрес, какой подъезд?"})
	res = evalDialogue(dlgReq(t, rude))
	if statuses(res)["calm"] != components.Missed || deref(res.Tone.Politeness) != 55 {
		t.Fatalf("грубость: %v %v", statuses(res), deref(res.Tone.Politeness))
	}
}

func TestEvalDialogueShoutingAndPoliteness(t *testing.T) {
	t.Parallel()
	res := evalDialogue(dlgReq(t, dlgWithOps(t, obj{"text": "ГДЕ ВЫ НАХОДИТЕСЬ? КАКОЙ ПОДЪЕЗД?"})))
	if statuses(res)["calm"] != components.Missed || deref(res.Tone.Calmness) != 55 ||
		!strings.Contains(deref(res.Tone.Comment), "повышенный тон") {
		t.Fatalf("крик: %v %+v", statuses(res), res.Tone)
	}
	res = evalDialogue(dlgReq(t, dlgWithOps(t, obj{"text": "Здравствуйте, слушаю вас. Какой подъезд, пожалуйста?"})))
	if deref(res.Tone.Politeness) != 90 || deref(res.Tone.Calmness) != 85 ||
		deref(res.Tone.Comment) != "Обращение вежливое, формулировки понятные." {
		t.Fatalf("вежливость: %+v", res.Tone)
	}
}

func TestEvalDialogueNoOperatorTurns(t *testing.T) {
	t.Parallel()
	p := dialoguePayload()
	p["transcript"] = []any{obj{"turn_no": 1, "speaker": "caller", "text": "Алло! Алло?"}}
	res := evalDialogue(dlgReq(t, p))
	st := statuses(res)
	if st["calm"] != components.NotApplicable || st["ask_address"] != components.Missed {
		t.Fatalf("checklist %v", st)
	}
	if res.Score != 0 || res.Confidence != 0.55 || res.Speech.OperatorTurns != 0 {
		t.Fatalf("score %v conf %v", res.Score, res.Confidence)
	}
	if deref(res.SummaryForStudent) != "Разговор с заявителем не состоялся: реплик оператора нет." {
		t.Fatalf("summary %q", deref(res.SummaryForStudent))
	}
	if deref(res.Speech.WordsPerMin) != 0 || deref(res.Speech.AvgResponseMs) != 0 {
		t.Fatalf("speech %+v", res.Speech)
	}
}

func TestEvalDialogueLowConfidence(t *testing.T) {
	t.Parallel()
	p := dlgWithOps(t, obj{"text": "Какой подъезд?", "confidence": 0.3, "audio_duration_ms": 1500})
	res := evalDialogue(dlgReq(t, p))
	if deref(res.Speech.LowConfidenceTurns) != 1 || !strings.Contains(deref(res.Checklist[0].Comment), "низкой уверенностью") {
		t.Fatalf("low conf: %+v %q", res.Speech, deref(res.Checklist[0].Comment))
	}
	// Порог из options.
	p["options"] = obj{"stt_confidence_floor": 0.2}
	res = evalDialogue(dlgReq(t, p))
	if deref(res.Speech.LowConfidenceTurns) != 0 || strings.Contains(deref(res.Checklist[0].Comment), "низкой") {
		t.Fatalf("floor 0.2: %+v", res.Speech)
	}
}

func TestEvalDialogueSpeechMetrics(t *testing.T) {
	t.Parallel()
	p := dlgWithOps(t,
		obj{"text": "Ну, эээ, короче, как бы, вот, э, ээ, значит, типа, ээээ, где вы?", "at_ms": 9000},
		obj{"text": "Какой подъезд?", "at_ms": 20000, "audio_duration_ms": 1000},
	)
	res := evalDialogue(dlgReq(t, p))
	sp := res.Speech
	wantFillers := map[string]int{"ну": 1, "эээ": 2, "короче": 1, "как бы": 1, "вот": 1, "э": 1, "ээ": 1, "значит": 1, "типа": 1}
	if !reflect.DeepEqual(*sp.Fillers, wantFillers) || deref(sp.FillerCount) != 10 {
		t.Fatalf("fillers %v (%d)", *sp.Fillers, deref(sp.FillerCount))
	}
	// Паузы: 9000−0 и 20000−12000 (заявитель at 9000+3000, без озвучки).
	if deref(sp.AvgResponseMs) != 8500 || deref(sp.MaxResponseMs) != 9000 {
		t.Fatalf("паузы %d/%d", deref(sp.AvgResponseMs), deref(sp.MaxResponseMs))
	}
	// Слова: 13 + 2; речь: 13×400 (текст без длительности) + 1000.
	if sp.OperatorWords != 15 || deref(sp.OperatorTalkMs) != 13*400+1000 {
		t.Fatalf("words %d talk %d", sp.OperatorWords, deref(sp.OperatorTalkMs))
	}
	if deref(res.Tone.Clarity) != 40 || !strings.Contains(deref(res.Tone.Comment), "Много слов-паразитов") {
		t.Fatalf("tone %+v", res.Tone)
	}
	s := deref(res.SummaryForStudent)
	if !strings.Contains(s, "Слов-паразитов в речи: 10") || !strings.Contains(s, "Паузы перед ответом длинные (в среднем 8.5 с)") {
		t.Fatalf("summary %q", s)
	}
	// Озвучка заявителя сокращает паузу: пауза считается от конца его реплики.
	p = dialoguePayload()
	p["transcript"] = []any{
		obj{"turn_no": 1, "speaker": "caller", "text": "Алло!", "at_ms": 0, "audio_duration_ms": 3000},
		obj{"turn_no": 2, "speaker": "operator", "text": "Слушаю.", "at_ms": 3500},
		obj{"turn_no": 3, "speaker": "operator", "text": "Где вы?", "at_ms": 9000}, // подряд — не пауза реакции
	}
	res = evalDialogue(dlgReq(t, p))
	if deref(res.Speech.AvgResponseMs) != 500 || deref(res.Speech.MaxResponseMs) != 500 {
		t.Fatalf("пауза от конца озвучки: %d", deref(res.Speech.AvgResponseMs))
	}
}

func TestEvalDialogueWeightsAndLongTurns(t *testing.T) {
	t.Parallel()
	p := dlgWithOps(t, obj{"text": "Какой подъезд? " + strings.Repeat("слово ", 35)})
	p["etalon"] = obj{"expected_dialogue": obj{"checklist": []any{
		obj{"id": "a", "text": "Подъезд", "kind": "question", "required": true, "weight": 2, "hints": []any{"подъезд"}},
		obj{"id": "b", "text": "Телефон", "kind": "question", "required": false, "weight": 1, "hints": []any{"телефон"}},
	}}}
	res := evalDialogue(dlgReq(t, p))
	// a: 2×1.5 = 3 выполнено; b: 1 не выполнено → 75.
	if res.Score != 75 || len(*res.MissingQuestions) != 0 {
		t.Fatalf("score %v missing %v", res.Score, *res.MissingQuestions)
	}
	// Реплика длиннее 30 слов — ясность ниже.
	if deref(res.Tone.Clarity) != 82 {
		t.Fatalf("clarity %v", deref(res.Tone.Clarity))
	}
	if !strings.HasPrefix(deref(res.SummaryForStudent), "Протокол опроса выполнен") {
		t.Fatalf("summary %q", deref(res.SummaryForStudent))
	}
}

// Регрессия: однословные маркеры грубости — по началу слова, а не подстрокой
// («процедура» содержит «дура», но грубостью не является).
func TestIsRude(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]bool{
		"такая процедура, назовите адрес": false,
		"по процедуре нужен адрес":        false,
		"ты дура?":               true,
		"дурак какой-то":         true,
		"идиотка":                true,
		"не орите на меня":       true,
		"не кричите на меня":     true,
		"быстрее говорите адрес": true,
		"говорите быстрее":       false,
		"отстаньте":              true,
		"заткнитесь уже":         true,
		"":                       false,
	} {
		if got := isRude(norm(in)); got != want {
			t.Errorf("isRude(%q) = %v", in, got)
		}
	}
	res := evalDialogue(dlgReq(t, dlgWithOps(t, obj{"text": "Такая процедура: назовите адрес и подъезд, пожалуйста."})))
	if statuses(res)["calm"] != components.Done || deref(res.Tone.Politeness) != 90 {
		t.Fatalf("«процедура» сочтена грубостью: %v %v", statuses(res), deref(res.Tone.Politeness))
	}
}

func TestIsShouting(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]bool{
		"ГДЕ ВЫ НАХОДИТЕСЬ": true, "Где вы?": false, "Помогите!!": true, "SOS SOS": false, "112": false,
		"ПОЖАР": false, "": false, "ГДЕ ВЫ, 112 ПОДЪЕЗД?": true, "ГДЕ ВЫ НАХОДИТЕСь": false,
	} {
		if got := isShouting(in); got != want {
			t.Errorf("isShouting(%q) = %v", in, got)
		}
	}
}

func TestCountFillers(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]map[string]int{
		"":                      {},
		"ну вот":                {"ну": 1, "вот": 1},
		"как бы да, как бы нет": {"как бы": 2},
		"как дела":              {},
		"ну-ну":                 {}, // одно слово через дефис
		"э ээ эээ ээээээ":       {"э": 1, "ээ": 1, "эээ": 2},
		"значит, типа, короче, ладно": {"значит": 1, "типа": 1, "короче": 1},
		"вотще нунчаки":               {},
	} {
		got := map[string]int{}
		countFillers(norm(in), got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("countFillers(%q) = %v, ждали %v", in, got, want)
		}
	}
}

func TestClampInt(t *testing.T) {
	t.Parallel()
	for _, c := range [][4]int{{-5, 0, 100, 0}, {50, 0, 100, 50}, {150, 0, 100, 100}, {40, 40, 95, 40}} {
		if got := clampInt(c[0], c[1], c[2]); got != c[3] {
			t.Errorf("clampInt(%d,%d,%d) = %d", c[0], c[1], c[2], got)
		}
	}
}
