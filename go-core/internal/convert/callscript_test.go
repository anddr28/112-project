package convert

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
)

func sampleScript() model.CallScript {
	return model.CallScript{
		Caller: model.Caller{Name: "Мария Ивановна", Phone: "+79035113367", Role: "соседка", EmotionalState: "паника", Voice: "baya"},
		Address: components.Address{
			Raw: Ptr("Москва, Тверская, 12"), City: Ptr("Москва"),
		},
		KeyFacts: []string{"горит квартира на 5 этаже"},
		Dialogue: &model.DialogueBrief{
			Persona:       "Пожилая соседка, путается в словах",
			SpeakingStyle: "сбивчиво",
			Facts: []model.DialogueFact{
				{ID: "f1", Text: "Горит квартира 45", Reveal: model.RevealVolunteer},
				{ID: "f2", Text: "Внутри может быть сосед", Reveal: model.RevealOnRequest, Hints: []string{"кто внутри"}},
				{ID: "f3", Text: "Сама подожгла", Reveal: model.RevealNever},
			},
			Unknowns:      []string{"номер подъезда"},
			EndConditions: []string{"оператор сказал, что помощь выехала"},
			MaxTurns:      10,
		},
		Turns: []model.Turn{
			{Speaker: model.SpeakerCaller, Text: "Алло! Пожар!", TTSHash: "abc"},
			{Speaker: model.SpeakerOperatorHint, Text: "Уточните адрес"},
		},
	}
}

func TestCallScriptToPublic(t *testing.T) {
	t.Parallel()

	t.Run("nil", func(t *testing.T) {
		t.Parallel()
		p := CallScriptToPublic(nil)
		b, _ := json.Marshal(p)
		if !strings.Contains(string(b), `"keyFacts":[]`) || !strings.Contains(string(b), `"turns":[]`) {
			t.Fatalf("обязательные массивы: %s", b)
		}
	})

	t.Run("пустая легенда", func(t *testing.T) {
		t.Parallel()
		p := CallScriptToPublic(&model.CallScript{})
		if p.KeyFacts == nil || p.Turns == nil || p.Dialogue != nil || p.Caller.Name != nil {
			t.Fatalf("got %+v", p)
		}
	})

	t.Run("полная", func(t *testing.T) {
		t.Parallel()
		c := sampleScript()
		p := CallScriptToPublic(&c)
		if Deref(p.Caller.Name) != "Мария Ивановна" || Deref(p.Caller.Voice) != "baya" || Deref(p.Caller.EmotionalState) != "паника" {
			t.Errorf("caller = %+v", p.Caller)
		}
		if Deref(p.Address.Raw) != "Москва, Тверская, 12" {
			t.Errorf("address = %+v", p.Address)
		}
		if len(p.Turns) != 2 || Deref(p.Turns[0].TtsHash) != "abc" || p.Turns[1].TtsHash != nil ||
			p.Turns[1].Speaker != public.CallTurnSpeaker(model.SpeakerOperatorHint) {
			t.Errorf("turns = %+v", p.Turns)
		}
		if p.Dialogue == nil || len(p.Dialogue.Facts) != 3 || Deref(p.Dialogue.MaxTurns) != 10 ||
			Deref(p.Dialogue.SpeakingStyle) != "сбивчиво" || !reflect.DeepEqual(*p.Dialogue.Facts[1].Hints, []string{"кто внутри"}) {
			b, _ := json.Marshal(p.Dialogue)
			t.Errorf("dialogue = %s", b)
		}
		b, _ := json.Marshal(p)
		if !strings.Contains(string(b), `"keyFacts":["горит квартира на 5 этаже"]`) {
			t.Errorf("json: %s", b)
		}
	})
}

func TestCallScriptFromPublic(t *testing.T) {
	t.Parallel()

	if m := CallScriptFromPublic(nil); m.Turns == nil {
		t.Fatal("nil -> turns []")
	}

	var p public.CallScript
	p.Caller.Name = Ptr("  Пётр  ")
	p.Caller.Phone = Ptr(" ")
	p.Caller.Voice = Ptr(" xenia ")
	p.Address = public.Address{Raw: Ptr(" ул. Ленина "), Street: Ptr("")}
	p.KeyFacts = []string{" дым ", "", "  "}
	p.Turns = []public.CallTurn{
		{Speaker: "caller", Text: "Помогите!", TtsHash: Ptr(" h1 ")},
		{Speaker: "operator_hint", Text: "Спросите этаж"},
		{Speaker: "кто-то", Text: "Ещё реплика"},
	}
	p.Dialogue = &public.DialogueBrief{
		Persona: "  Пётр  ",
		Facts:   []public.DialogueFact{{Id: " f1 ", Text: "дым", Reveal: "странное"}, {Id: "f2", Text: "x", Reveal: "never"}},
	}
	m := CallScriptFromPublic(&p)
	if m.Caller.Name != "Пётр" || m.Caller.Phone != "" || m.Caller.Voice != "xenia" {
		t.Errorf("caller = %+v", m.Caller)
	}
	if Deref(m.Address.Raw) != " ул. Ленина " || m.Address.Street != nil {
		t.Errorf("address = %+v (пустые -> nil)", m.Address)
	}
	if !reflect.DeepEqual(m.KeyFacts, []string{"дым"}) {
		t.Errorf("keyFacts = %v", m.KeyFacts)
	}
	if len(m.Turns) != 3 || m.Turns[0].TTSHash != "h1" || m.Turns[1].Speaker != model.SpeakerOperatorHint ||
		m.Turns[2].Speaker != model.SpeakerCaller {
		t.Errorf("turns = %+v (неизвестный говорящий -> caller)", m.Turns)
	}
	if m.Dialogue == nil || m.Dialogue.Persona != "Пётр" || m.Dialogue.Facts[0].ID != "f1" ||
		m.Dialogue.Facts[0].Reveal != model.RevealOnRequest || m.Dialogue.Facts[1].Reveal != model.RevealNever {
		t.Errorf("dialogue = %+v", m.Dialogue)
	}

	// jsonb-форма: snake_case, без voice у реплик, пустые key_facts не пишутся
	b, _ := json.Marshal(m)
	for _, k := range []string{`"tts_hash":"h1"`, `"key_facts":["дым"]`, `"voice":"xenia"`, `"reveal":"on_request"`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("jsonb без %s: %s", k, b)
		}
	}
}

func TestCallScriptToContract(t *testing.T) {
	t.Parallel()

	if c := CallScriptToContract(nil); c.Turns == nil {
		t.Fatal("nil -> turns []")
	}

	c := sampleScript()
	out := CallScriptToContract(&c)
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "baya") || strings.Contains(string(b), `"voice"`) {
		t.Errorf("голос заявителя в ai-service не уходит: %s", b)
	}
	if !reflect.DeepEqual(*out.KeyFacts, []string{"горит квартира на 5 этаже"}) {
		t.Errorf("key_facts как есть: %v", out.KeyFacts)
	}
	if len(out.Turns) != 2 || Deref(out.Turns[0].TtsHash) != "abc" || out.Turns[1].TtsHash != nil {
		t.Errorf("turns = %+v", out.Turns)
	}
	if out.Dialogue == nil || len(out.Dialogue.Facts) != 3 || Deref(out.Dialogue.MaxTurns) != 10 {
		t.Errorf("dialogue = %+v", out.Dialogue)
	}

	// key_facts пуст, есть бриф -> проекция фактов без reveal=never и без пустых
	c.KeyFacts = nil
	c.Dialogue.Facts = append(c.Dialogue.Facts, model.DialogueFact{ID: "f4", Text: "  ", Reveal: model.RevealVolunteer})
	out = CallScriptToContract(&c)
	if out.KeyFacts == nil || !reflect.DeepEqual(*out.KeyFacts, []string{"Горит квартира 45", "Внутри может быть сосед"}) {
		t.Errorf("проекция key_facts = %v", out.KeyFacts)
	}

	// ни key_facts, ни брифа -> поле не передаётся
	c.Dialogue = nil
	out = CallScriptToContract(&c)
	if out.KeyFacts != nil || out.Dialogue != nil {
		t.Errorf("key_facts/dialogue = %v / %v", out.KeyFacts, out.Dialogue)
	}
}

func TestCallScriptFromContract(t *testing.T) {
	t.Parallel()

	if m := CallScriptFromContract(nil); m.Turns == nil {
		t.Fatal("nil -> turns []")
	}

	var c components.CallScript
	c.Caller.Name = Ptr(" Ольга ")
	c.Caller.Role = Ptr("очевидец")
	c.Address = components.Address{Raw: Ptr("Москва"), House: Ptr(" ")}
	kf := []string{" запах газа ", " "}
	c.KeyFacts = &kf
	c.Turns = []contractTurn{
		{Speaker: "caller", Text: "Пахнет газом!"},
		{Speaker: "caller", Text: "   "}, // LLM иногда отдаёт пустые реплики
		{Speaker: "operator_hint", Text: "Спросите этаж", TtsHash: Ptr("")},
	}
	c.Dialogue = &components.DialogueBrief{Persona: " Ольга ", Facts: []components.DialogueFact{{Id: "f", Text: "газ", Reveal: "volunteer"}}}
	m := CallScriptFromContract(&c)
	if m.Caller.Name != "Ольга" || m.Caller.Role != "очевидец" || m.Caller.Voice != "" {
		t.Errorf("caller = %+v", m.Caller)
	}
	if m.Address.House != nil || Deref(m.Address.Raw) != "Москва" {
		t.Errorf("address = %+v", m.Address)
	}
	if !reflect.DeepEqual(m.KeyFacts, []string{"запах газа"}) {
		t.Errorf("keyFacts = %v", m.KeyFacts)
	}
	if len(m.Turns) != 2 || m.Turns[0].Text != "Пахнет газом!" || m.Turns[1].Speaker != model.SpeakerOperatorHint || m.Turns[1].TTSHash != "" {
		t.Errorf("turns = %+v", m.Turns)
	}
	if m.Dialogue == nil || m.Dialogue.Persona != "Ольга" || m.Dialogue.Facts[0].Reveal != model.RevealVolunteer {
		t.Errorf("dialogue = %+v", m.Dialogue)
	}
}

func TestDialogueBriefConversions(t *testing.T) {
	t.Parallel()

	if p := DialogueBriefToPublic(nil); p.Facts == nil {
		t.Fatal("ToPublic(nil).facts -> []")
	}
	if m := DialogueBriefFromPublic(nil); m.Facts == nil {
		t.Fatal("FromPublic(nil).facts -> []")
	}
	if c := DialogueBriefToContract(nil); c.Facts == nil {
		t.Fatal("ToContract(nil).facts -> []")
	}
	if m := DialogueBriefFromContract(nil); m.Facts == nil {
		t.Fatal("FromContract(nil).facts -> []")
	}

	b := sampleScript().Dialogue
	c := DialogueBriefToContract(b)
	back := DialogueBriefFromContract(&c)
	if !reflect.DeepEqual(&back, b) {
		t.Fatalf("contract round trip:\n got %+v\nwant %+v", back, *b)
	}
	p := DialogueBriefToPublic(b)
	back = DialogueBriefFromPublic(&p)
	if !reflect.DeepEqual(&back, b) {
		t.Fatalf("public round trip:\n got %+v\nwant %+v", back, *b)
	}
	// пустое в JSON не пишется
	empty := DialogueBriefToPublic(&model.DialogueBrief{Persona: "p"})
	js, _ := json.Marshal(empty)
	if string(js) != `{"facts":[],"persona":"p"}` {
		t.Fatalf("json = %s", js)
	}
}

func TestNormRevealSpeaker(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"volunteer": "volunteer", "never": "never", "on_request": "on_request", "": "on_request", "всегда": "on_request",
	} {
		if got := normReveal(in); got != want {
			t.Errorf("normReveal(%q) = %q", in, got)
		}
	}
	for in, want := range map[string]string{
		"caller": "caller", "operator_hint": "operator_hint", "operator": "caller", "": "caller",
	} {
		if got := normSpeaker(in); got != want {
			t.Errorf("normSpeaker(%q) = %q", in, got)
		}
	}
	if nonEmptyStrings(nil) != nil || nonEmptyStrings([]string{" ", ""}) != nil {
		t.Error("nonEmptyStrings пустых -> nil")
	}
	if got := nonEmptyStrings([]string{" а ", "", "б"}); !reflect.DeepEqual(got, []string{"а", "б"}) {
		t.Errorf("nonEmptyStrings = %v", got)
	}
}
