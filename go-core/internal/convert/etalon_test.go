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

func TestIncidentCardPublicRoundTrip(t *testing.T) {
	t.Parallel()

	if got := IncidentCardToPublic(nil); !reflect.DeepEqual(got, public.IncidentCard{}) {
		t.Fatal("nil -> пустая карточка")
	}
	if got := IncidentCardFromPublic(nil); !reflect.DeepEqual(got, components.IncidentCard{}) {
		t.Fatal("nil -> пустая карточка")
	}

	svc := []string{"101"}
	attrs := map[string]any{"where": "house"}
	c := components.IncidentCard{
		CategoryCode:     Ptr("101"),
		Address:          &components.Address{Raw: Ptr("Москва, Тверская, 12"), House: Ptr("12к1")},
		ServicesToNotify: &svc,
		Description:      Ptr("Пожар в квартире"),
		Attributes:       &attrs,
	}
	c.Applicant = &struct {
		Name  *string `json:"name,omitempty"`
		Phone *string `json:"phone,omitempty"`
	}{Name: Ptr("Мария")}
	p := IncidentCardToPublic(&c)
	b, _ := json.Marshal(p)
	for _, k := range []string{`"categoryCode":"101"`, `"servicesToNotify":["101"]`, `"description":"Пожар в квартире"`, `"house":"12к1"`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("camelCase карточка без %s: %s", k, b)
		}
	}
	back := IncidentCardFromPublic(&p)
	if !reflect.DeepEqual(back, c) {
		bb, _ := json.Marshal(back)
		cb, _ := json.Marshal(c)
		t.Fatalf("round trip:\n got %s\nwant %s", bb, cb)
	}
}

func TestIncidentCardFromPublicCompacts(t *testing.T) {
	t.Parallel()

	p := public.IncidentCard{
		CategoryCode: Ptr("  "),
		Description:  Ptr(""),
		ActionsTaken: Ptr(" вызвана бригада "),
		Address:      &public.Address{Raw: Ptr("г. Москва"), City: Ptr(" "), Street: Ptr("")},
	}
	c := IncidentCardFromPublic(&p)
	if c.CategoryCode != nil || c.Description != nil {
		t.Error("пустые строки -> nil")
	}
	if Deref(c.ActionsTaken) != " вызвана бригада " {
		t.Error("непустая строка сохраняется как есть")
	}
	if c.Address == nil || Deref(c.Address.Raw) != "г. Москва" || c.Address.City != nil || c.Address.Street != nil {
		t.Errorf("address = %+v", c.Address)
	}
	b, _ := json.Marshal(c)
	if strings.Contains(string(b), `"city"`) || strings.Contains(string(b), `"street"`) {
		t.Errorf("пустые поля адреса в jsonb: %s", b)
	}
}

func TestScoringConversions(t *testing.T) {
	t.Parallel()

	t.Run("ToPublic", func(t *testing.T) {
		t.Parallel()
		if got := ScoringToPublic(nil); !reflect.DeepEqual(got, public.Scoring{}) {
			t.Fatal("nil")
		}
		s := &model.Scoring{
			RequiredFields: []string{"address.raw"},
			FieldWeights:   map[string]float64{"address.raw": 2.5},
			RequiredFacts:  []string{"горит квартира"},
		}
		p := ScoringToPublic(s)
		if !reflect.DeepEqual(*p.RequiredFields, []string{"address.raw"}) || (*p.FieldWeights)["address.raw"] != 2.5 ||
			!reflect.DeepEqual(*p.RequiredFacts, []string{"горит квартира"}) || p.ForbiddenFacts != nil {
			t.Fatalf("got %+v", p)
		}
		(*p.RequiredFields)[0] = "испорчено"
		if s.RequiredFields[0] != "address.raw" {
			t.Fatal("ScoringToPublic должен копировать срезы")
		}
	})

	t.Run("FromPublic", func(t *testing.T) {
		t.Parallel()
		if got := ScoringFromPublic(nil, nil); !reflect.DeepEqual(got, model.Scoring{}) {
			t.Fatalf("nil -> %+v", got)
		}
		w := map[string]float32{" address.raw ": 2, "  ": 5}
		p := &public.Scoring{
			RequiredFields: &[]string{"address.raw", " ", "address.raw", " description "},
			RequiredFacts:  &[]string{" горит ", ""},
			ForbiddenFacts: &[]string{},
			FieldWeights:   &w,
		}
		got := ScoringFromPublic(p, nil)
		if !reflect.DeepEqual(got.RequiredFields, []string{"address.raw", "description"}) {
			t.Errorf("requiredFields = %v (trim, без пустых, без повторов)", got.RequiredFields)
		}
		if !reflect.DeepEqual(got.RequiredFacts, []string{"горит"}) || got.ForbiddenFacts != nil {
			t.Errorf("facts = %v / %v", got.RequiredFacts, got.ForbiddenFacts)
		}
		if !reflect.DeepEqual(got.FieldWeights, map[string]float64{"address.raw": 2}) {
			t.Errorf("weights = %v", got.FieldWeights)
		}

		// верхнеуровневое requiredFields важнее scoring.requiredFields
		top := []string{"incidentTypeIds", "incidentTypeIds", ""}
		got = ScoringFromPublic(p, &top)
		if !reflect.DeepEqual(got.RequiredFields, []string{"incidentTypeIds"}) {
			t.Errorf("override = %v", got.RequiredFields)
		}
		// пустой верхнеуровневый список — «очистить»
		empty := []string{}
		if got = ScoringFromPublic(p, &empty); got.RequiredFields != nil {
			t.Errorf("пустой override = %v", got.RequiredFields)
		}
		// только requiredFields без scoring
		got = ScoringFromPublic(nil, &[]string{"description"})
		if !reflect.DeepEqual(got.RequiredFields, []string{"description"}) {
			t.Errorf("без scoring = %v", got.RequiredFields)
		}
	})

	t.Run("Contract", func(t *testing.T) {
		t.Parallel()
		if got := ScoringToContract(nil); !reflect.DeepEqual(got, components.Scoring{}) {
			t.Fatal("nil")
		}
		if got := ScoringFromContract(nil); !reflect.DeepEqual(got, model.Scoring{}) {
			t.Fatal("nil")
		}
		s := &model.Scoring{
			RequiredFields: []string{"description"},
			FieldWeights:   map[string]float64{"description": 0.5},
			ForbiddenFacts: []string{"есть погибшие"},
		}
		c := ScoringToContract(s)
		b, _ := json.Marshal(c)
		if !strings.Contains(string(b), `"required_fields":["description"]`) || !strings.Contains(string(b), `"field_weights":{"description":0.5}`) ||
			!strings.Contains(string(b), `"forbidden_facts":["есть погибшие"]`) || strings.Contains(string(b), "required_facts") {
			t.Fatalf("contract json: %s", b)
		}
		back := ScoringFromContract(&c)
		if !reflect.DeepEqual(back, *s) {
			t.Fatalf("round trip: %+v", back)
		}
		dup := []string{"a", " a ", "a", ""}
		if got := ScoringFromContract(&components.Scoring{RequiredFields: &dup}); !reflect.DeepEqual(got.RequiredFields, []string{"a"}) {
			t.Fatalf("dedupe: %v", got.RequiredFields)
		}
	})
}

func TestExpectedActions(t *testing.T) {
	t.Parallel()

	if got := ExpectedActionsToPublic(nil); got == nil || len(got) != 0 {
		t.Fatal("nil -> []")
	}
	if got := ExpectedActionsToContract(nil); got == nil || len(got) != 0 {
		t.Fatal("nil -> []")
	}
	p := []public.ExpectedAction{
		{ActionText: "  Передать в 101  ", RequiredFacts: &[]string{" адрес ", ""}},
		{ActionText: "   "},
		{ActionText: "Оповестить 103", ForbiddenFacts: &[]string{}},
	}
	m := ExpectedActionsFromPublic(p)
	want := []model.ExpectedAction{
		{ActionText: "Передать в 101", RequiredFacts: []string{"адрес"}},
		{ActionText: "Оповестить 103"},
	}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("FromPublic = %+v", m)
	}
	pub := ExpectedActionsToPublic(m)
	if len(pub) != 2 || pub[0].ActionText != "Передать в 101" || !reflect.DeepEqual(*pub[0].RequiredFacts, []string{"адрес"}) ||
		pub[1].RequiredFacts != nil || pub[1].ForbiddenFacts != nil {
		t.Fatalf("ToPublic = %+v", pub)
	}
	c := ExpectedActionsToContract(m)
	back := ExpectedActionsFromContract(append(c, components.ExpectedAction{ActionText: " "}))
	if !reflect.DeepEqual(back, want) {
		t.Fatalf("contract round trip = %+v", back)
	}
	if got := ExpectedActionsFromPublic(nil); got == nil {
		t.Fatal("FromPublic(nil) -> [] (пишется в jsonb)")
	}
}

func TestExpectedDialogue(t *testing.T) {
	t.Parallel()

	t.Run("nil", func(t *testing.T) {
		t.Parallel()
		p := ExpectedDialogueToPublic(nil)
		if p.Checklist == nil || len(p.Checklist) != 0 {
			t.Fatal("ToPublic(nil).checklist -> []")
		}
		if m := ExpectedDialogueFromPublic(nil); m.Checklist == nil {
			t.Fatal("FromPublic(nil).checklist -> []")
		}
		if c := ExpectedDialogueToContract(nil); c.Checklist == nil {
			t.Fatal("ToContract(nil).checklist -> []")
		}
		if m := ExpectedDialogueFromContract(nil); m.Checklist == nil {
			t.Fatal("FromContract(nil).checklist -> []")
		}
	})

	t.Run("из правки", func(t *testing.T) {
		t.Parallel()
		w0, w2 := float32(0), float32(2)
		mt := 8
		p := &public.ExpectedDialogue{
			Forbidden:        &[]string{" успокойтесь ", ""},
			MaxOperatorTurns: &mt,
			Checklist: []public.DialogueChecklistItem{
				{Id: " addr ", Text: " Уточнить адрес ", Kind: "question", Required: true, Weight: &w2, Hints: &[]string{"где вы?", " "}},
				{Id: "", Text: "без id"},
				{Id: "x", Text: "  "},
				{Id: "leave", Text: "Покинуть помещение", Kind: "instruction", Weight: &w0},
				{Id: "odd", Text: "Странный вид", Kind: "поведение"},
			},
		}
		m := ExpectedDialogueFromPublic(p)
		if len(m.Checklist) != 3 {
			t.Fatalf("пункты без id/текста отбрасываются: %+v", m.Checklist)
		}
		c0 := m.Checklist[0]
		if c0.ID != "addr" || c0.Text != "Уточнить адрес" || c0.Kind != model.ChecklistQuestion || !c0.Required ||
			c0.Weight == nil || *c0.Weight != 2 || !reflect.DeepEqual(c0.Hints, []string{"где вы?"}) {
			t.Errorf("item0 = %+v", c0)
		}
		if w := m.Checklist[1].Weight; w == nil || *w != 0 {
			t.Error("weight 0 сохраняется (пункт без веса), а не превращается в nil")
		}
		if m.Checklist[2].Kind != model.ChecklistQuestion {
			t.Errorf("неизвестный kind -> question, got %q", m.Checklist[2].Kind)
		}
		if !reflect.DeepEqual(m.Forbidden, []string{"успокойтесь"}) || m.MaxOperatorTurns != 8 {
			t.Errorf("forbidden/max = %v %d", m.Forbidden, m.MaxOperatorTurns)
		}

		// обратно: веса, подсказки, maxOperatorTurns
		back := ExpectedDialogueToPublic(&m)
		if len(back.Checklist) != 3 || *back.Checklist[0].Weight != 2 || back.Checklist[2].Weight != nil ||
			Deref(back.MaxOperatorTurns) != 8 || !reflect.DeepEqual(*back.Forbidden, []string{"успокойтесь"}) {
			b, _ := json.Marshal(back)
			t.Errorf("ToPublic = %s", b)
		}
		// контракт
		c := ExpectedDialogueToContract(&m)
		cb, _ := json.Marshal(c)
		if !strings.Contains(string(cb), `"max_operator_turns":8`) || !strings.Contains(string(cb), `"kind":"instruction"`) {
			t.Errorf("contract = %s", cb)
		}
		if got := ExpectedDialogueFromContract(&c); !reflect.DeepEqual(got, m) {
			t.Errorf("contract round trip = %+v", got)
		}
	})

	t.Run("нулевой maxOperatorTurns не отдаётся", func(t *testing.T) {
		t.Parallel()
		p := ExpectedDialogueToPublic(&model.ExpectedDialogue{Checklist: []model.ChecklistItem{}})
		if p.MaxOperatorTurns != nil || p.Forbidden != nil {
			t.Fatalf("got %+v", p)
		}
	})
}

func TestNormKindAndDedupe(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"question": "question", "instruction": "instruction", "phrase": "phrase", "behavior": "behavior",
		"": "question", "QUESTION": "question", "вопрос": "question",
	} {
		if got := normKind(in); got != want {
			t.Errorf("normKind(%q) = %q, want %q", in, got, want)
		}
	}

	in := []string{"b", "a", "b", "c", "a"}
	out := dedupe(in)
	if !reflect.DeepEqual(out, []string{"b", "a", "c"}) {
		t.Fatalf("dedupe = %v", out)
	}
	if !reflect.DeepEqual(in, []string{"b", "a", "b", "c", "a"}) {
		t.Fatal("dedupe не должен портить вход")
	}
	if dedupe(nil) != nil || len(dedupe([]string{"x"})) != 1 {
		t.Fatal("короткие входы")
	}
}

// Эталон работы диспетчера ДДС (Scoring.reaction, v1.3): туда-обратно без потерь, статусы
// без повторов, в ai-service не уходит.
func TestReactionRoundTrip(t *testing.T) {
	t.Parallel()
	if ReactionToPublic(nil) != nil || ReactionFromPublic(nil) != nil {
		t.Fatal("nil должен оставаться nil")
	}
	dec := public.ReactionExpectationDecision("reject")
	within := 45
	st := []public.ReactionStatus{"Начало реагирования", "Прибытие", "Начало реагирования"}
	p := &public.Scoring{Reaction: &public.ReactionExpectation{Decision: &dec, DecisionWithinSec: &within, RequiredStatuses: &st}}
	m := ScoringFromPublic(p, nil)
	want := &model.ReactionExpectation{Decision: "reject", DecisionWithinSec: 45, RequiredStatuses: []string{"Начало реагирования", "Прибытие"}}
	if !reflect.DeepEqual(m.Reaction, want) {
		t.Fatalf("from public: %+v", m.Reaction)
	}
	if m.IsZero() {
		t.Error("scoring только с reaction — не пустой")
	}
	back := ScoringToPublic(&m).Reaction
	if back == nil || *back.Decision != "reject" || *back.DecisionWithinSec != 45 || len(*back.RequiredStatuses) != 2 {
		t.Fatalf("to public: %+v", back)
	}
	raw, _ := json.Marshal(ScoringToContract(&m))
	if strings.Contains(string(raw), "reaction") {
		t.Errorf("reaction ушёл в ai-service: %s", raw)
	}
}
