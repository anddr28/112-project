package scenarios

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/settings"
	"lct/gocore/internal/store"
)

// Юнит-тесты чистой логики пакета: правка эталона, статус заявителя по роли, проверки
// полноты и ввода, очистка брифа/чек-листа, легенда и хэши озвучки.

func unitService() *Service { return New(Deps{Catalog: newStubCatalog(), Log: discardLog()}) }

func svc(code string) public.AssignedService {
	c := newStubCatalog()
	info, _ := c.ServiceByCode(code)
	return public.AssignedService{ServiceId: info.ID, Code: info.Code, Name: info.Name, ShortName: info.ShortName,
		History: []public.ReactionStatusEntry{}, AllowedNext: []public.AllowedTransition{}}
}

// fireDraft — форма АРМ эталона пожара (тип по uuid, адрес, описание, пострадавший).
func fireDraft(services ...string) public.IncidentCardDraft {
	d := convert.EmptyDraft()
	d.IncidentTypeIds = []string{"3021058b-40ef-4aed-9e0c-78b1e8184831"}
	d.Address.Raw = "Москва, Тверская улица, 12"
	*d.Address.Street = "Тверская"
	*d.Address.House = "12"
	d.Applicant.Name = ptr("Мария")
	d.Applicant.Status = ptr(public.Очевидец)
	d.Description = "Задымление в квартире на 5 этаже"
	d.Flags.VictimsPresent = true
	d.Flags.VictimsCount = ptr(1)
	for _, c := range services {
		d.Services = append(d.Services, svc(c))
	}
	return d
}

func codes(c components.IncidentCard) []string {
	if c.ServicesToNotify == nil {
		return nil
	}
	return *c.ServicesToNotify
}

// ---------------------------------------------------------------- правка эталона

func TestApplyEtalonPatchServices(t *testing.T) {
	t.Parallel()
	s := unitService()

	// Эталон, в форме АРМ которого преподаватель видел службы (сгенерированный или правленый).
	withServices := func() etalonData {
		d := fireDraft("104", "zhkh", "101", "103")
		return etalonData{Card: convert.DraftToCard(&d, s.cat), Draft: d, Actions: []model.ExpectedAction{}}
	}
	// Эталон фикстуры: в форме АРМ служб нет, они живут только в services_to_notify.
	fixture := func() etalonData {
		d := fireDraft()
		card := convert.DraftToCard(&d, s.cat)
		card.ServicesToNotify = &[]string{"101", "103", "104", "zhkh", "cemp"}
		card.Casualties = &struct {
			Dead    *int `json:"dead,omitempty"`
			Injured *int `json:"injured,omitempty"`
			Trapped *int `json:"trapped,omitempty"`
		}{Trapped: ptr(1)}
		return etalonData{Card: card, Draft: d, Actions: []model.ExpectedAction{}}
	}

	cases := []struct {
		name  string
		cur   func() etalonData
		draft public.IncidentCardDraft
		want  []string
	}{
		// Регрессия: снятые все службы не должны возвращаться из старой карточки.
		{"все службы сняты", withServices, fireDraft(), nil},
		{"часть служб снята", withServices, fireDraft("101"), []string{"101"}},
		{"службы изменены", withServices, fireDraft("104", "101"), []string{"104", "101"}},
		// Фикстура: редактор служб не показывал — пустой список их не снимает.
		{"фикстура: правка описания", fixture, func() public.IncidentCardDraft {
			d := fireDraft()
			d.Description = "Задымление в квартире на 5 этаже, внутри пожилой человек"
			return d
		}(), []string{"101", "103", "104", "zhkh", "cemp"}},
		{"фикстура: преподаватель отметил службу", fixture, fireDraft("101"), []string{"101"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cur := tc.cur()
			d := tc.draft
			next := s.applyEtalonPatch(cur, &public.ScenarioPatch{EtalonDraft: &d})
			if got := codes(next.Card); !slices.Equal(got, tc.want) {
				t.Fatalf("servicesToNotify = %v, want %v", got, tc.want)
			}
			if len(next.Draft.Services) != len(tc.draft.Services) {
				t.Fatalf("draft services %d, want %d", len(next.Draft.Services), len(tc.draft.Services))
			}
		})
	}
}

// Карточка с пустым списком служб уходит в JSON без services_to_notify (а не со старым списком).
func TestApplyEtalonPatchRemovedServicesEncoding(t *testing.T) {
	t.Parallel()
	s := unitService()
	d := fireDraft("101", "103")
	cur := etalonData{Card: convert.DraftToCard(&d, s.cat), Draft: d}
	empty := fireDraft()
	next := s.applyEtalonPatch(cur, &public.ScenarioPatch{EtalonDraft: &empty})
	enc, err := next.encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(enc.card), "services_to_notify") {
		t.Fatalf("card still lists services: %s", enc.card)
	}
}

// Редактор присылает etalonDraft при каждом сохранении: неизменная форма АРМ не должна
// пересобирать контрактную карточку (иначе у фикстур — новая версия эталона на ровном месте).
func TestApplyEtalonPatchUnchangedDraftKeepsCard(t *testing.T) {
	t.Parallel()
	s := unitService()
	d := fireDraft()
	card := components.IncidentCard{
		CategoryCode:     ptr("101"),
		Address:          &components.Address{Raw: ptr("Москва, Тверская улица, 12"), City: ptr("Москва")},
		ServicesToNotify: &[]string{"101", "103"},
		Description:      ptr("Карточка LLM богаче формы АРМ"),
	}
	cur := etalonData{Card: card, Draft: d, Actions: []model.ExpectedAction{}}
	oldEnc, err := fromCopy(cur).encode()
	if err != nil {
		t.Fatal(err)
	}

	// Клиент прислал ту же форму (через JSON, как фронт) и те же обязательные поля.
	var same public.IncidentCardDraft
	raw, _ := json.Marshal(d)
	if err := json.Unmarshal(raw, &same); err != nil {
		t.Fatal(err)
	}
	rf := []string{}
	next := s.applyEtalonPatch(cur, &public.ScenarioPatch{EtalonDraft: &same, RequiredFields: &rf})
	nextEnc, err := next.encode()
	if err != nil {
		t.Fatal(err)
	}
	if !nextEnc.equal(oldEnc) {
		t.Fatalf("unchanged draft changed the etalon:\nold card %s\nnew card %s", oldEnc.card, nextEnc.card)
	}

	// Изменённая форма — карточка пересобирается из неё.
	changed := same
	changed.Description = "Новое описание"
	next = s.applyEtalonPatch(cur, &public.ScenarioPatch{EtalonDraft: &changed})
	if got := deref(next.Card.Description); got != "Новое описание" {
		t.Fatalf("description = %q", got)
	}
	if next.Card.Address == nil || deref(next.Card.Address.City) != "" {
		t.Fatalf("card must be rebuilt from the draft, address %+v", next.Card.Address)
	}
}

func fromCopy(e etalonData) *etalonData { return &e }

func TestApplyEtalonPatchCardOnlyAndScoring(t *testing.T) {
	t.Parallel()
	s := unitService()
	cur := etalonData{
		Draft:   fireDraft(),
		Scoring: model.Scoring{RequiredFields: []string{"address.raw", "description"}, RequiredFacts: []string{"дым"}},
		Actions: []model.ExpectedAction{},
	}

	t.Run("только etalonCard — форма АРМ из карточки", func(t *testing.T) {
		t.Parallel()
		card := public.IncidentCard{
			CategoryCode:     ptr("104"),
			Address:          &public.Address{Raw: ptr("Москва, Ленинский проспект, 5к2"), House: ptr("5к2")},
			ServicesToNotify: &[]string{"104", "unknown"},
		}
		next := s.applyEtalonPatch(cur, &public.ScenarioPatch{EtalonCard: &card})
		if !slices.Equal(next.Draft.IncidentTypeIds, []string{"397fea9c-c875-4996-8d79-cd89309230dc"}) {
			t.Errorf("incidentTypeIds %v", next.Draft.IncidentTypeIds)
		}
		if *next.Draft.Address.House != "5" || *next.Draft.Address.Building != "2" {
			t.Errorf("house/building %q/%q", *next.Draft.Address.House, *next.Draft.Address.Building)
		}
		if len(next.Draft.Services) != 1 || next.Draft.Services[0].Code != "104" {
			t.Errorf("services %+v", next.Draft.Services)
		}
		if !slices.Equal(codes(next.Card), []string{"104", "unknown"}) {
			t.Errorf("card services %v", codes(next.Card))
		}
	})

	t.Run("etalonDraft и etalonCard — карточка как прислана", func(t *testing.T) {
		t.Parallel()
		d := fireDraft("101")
		card := public.IncidentCard{Description: ptr("своя карточка")}
		next := s.applyEtalonPatch(cur, &public.ScenarioPatch{EtalonDraft: &d, EtalonCard: &card})
		if deref(next.Card.Description) != "своя карточка" || next.Card.ServicesToNotify != nil {
			t.Errorf("card %+v", next.Card)
		}
		if len(next.Draft.Services) != 1 {
			t.Errorf("draft must be taken as is")
		}
	})

	t.Run("requiredFields важнее scoring.requiredFields", func(t *testing.T) {
		t.Parallel()
		rf := []string{" applicant.name ", "applicant.name", "", "services"}
		sc := public.Scoring{RequiredFields: &[]string{"description"}, RequiredFacts: &[]string{"газ"}}
		next := s.applyEtalonPatch(cur, &public.ScenarioPatch{Scoring: &sc, RequiredFields: &rf})
		if !slices.Equal(next.Scoring.RequiredFields, []string{"applicant.name", "services"}) {
			t.Errorf("required %v", next.Scoring.RequiredFields)
		}
		if !slices.Equal(next.Scoring.RequiredFacts, []string{"газ"}) {
			t.Errorf("facts %v", next.Scoring.RequiredFacts)
		}
	})

	t.Run("scoring без списков не стирает обязательные поля", func(t *testing.T) {
		t.Parallel()
		sc := public.Scoring{RequiredFacts: &[]string{"газ"}}
		next := s.applyEtalonPatch(cur, &public.ScenarioPatch{Scoring: &sc})
		if !slices.Equal(next.Scoring.RequiredFields, []string{"address.raw", "description"}) {
			t.Errorf("required %v", next.Scoring.RequiredFields)
		}
	})

	t.Run("только requiredFields — прочие правила оценки сохраняются", func(t *testing.T) {
		t.Parallel()
		rf := []string{"address.raw", " address.raw", "incidentTypeIds"}
		next := s.applyEtalonPatch(cur, &public.ScenarioPatch{RequiredFields: &rf})
		if !slices.Equal(next.Scoring.RequiredFields, []string{"address.raw", "incidentTypeIds"}) {
			t.Errorf("required %v", next.Scoring.RequiredFields)
		}
		if !slices.Equal(next.Scoring.RequiredFacts, []string{"дым"}) {
			t.Errorf("facts lost: %v", next.Scoring.RequiredFacts)
		}
	})

	t.Run("чек-лист и действия заменяются целиком", func(t *testing.T) {
		t.Parallel()
		dlg := public.ExpectedDialogue{Checklist: []public.DialogueChecklistItem{
			{Id: "q", Text: "Уточнить адрес", Kind: public.DialogueChecklistItemKindQuestion, Required: true},
			{Id: "q", Text: "Уточнить этаж", Kind: public.DialogueChecklistItemKindQuestion},
			{Id: "", Text: "Пункт без id отбрасывается", Kind: public.DialogueChecklistItemKindPhrase},
			{Id: "c", Text: "Сказать, что помощь выехала", Kind: public.DialogueChecklistItemKindPhrase},
		}}
		acts := []public.ExpectedAction{{ActionText: "Передать в 101"}}
		next := s.applyEtalonPatch(cur, &public.ScenarioPatch{ExpectedDialogue: &dlg, ExpectedActions: &acts})
		if next.Dialogue == nil || len(next.Dialogue.Checklist) != 3 {
			t.Fatalf("dialogue %+v", next.Dialogue)
		}
		ids := []string{next.Dialogue.Checklist[0].ID, next.Dialogue.Checklist[1].ID, next.Dialogue.Checklist[2].ID}
		if !slices.Equal(ids, []string{"q", "q-2", "c"}) {
			t.Errorf("checklist ids %v", ids)
		}
		if len(next.Actions) != 1 || next.Actions[0].ActionText != "Передать в 101" {
			t.Errorf("actions %+v", next.Actions)
		}
		if len(cur.Actions) != 0 || cur.Dialogue != nil {
			t.Errorf("current etalon mutated")
		}
	})
}

func TestMergeCard(t *testing.T) {
	t.Parallel()
	type cas = struct {
		Dead    *int `json:"dead,omitempty"`
		Injured *int `json:"injured,omitempty"`
		Trapped *int `json:"trapped,omitempty"`
	}
	old := components.IncidentCard{
		CategoryCode:     ptr("101"),
		ServicesToNotify: &[]string{"101", "103"},
		Casualties:       &cas{Injured: ptr(1), Trapped: ptr(1)},
	}
	cases := []struct {
		name         string
		next         components.IncidentCard
		keep         bool
		wantServices []string
		wantCode     string
		wantCas      *cas
	}{
		{"службы сохраняются у формы без служб", components.IncidentCard{}, true, []string{"101", "103"}, "101", nil},
		{"службы сняты преподавателем", components.IncidentCard{}, false, nil, "101", nil},
		{"свои службы не подменяются", components.IncidentCard{ServicesToNotify: &[]string{"104"}}, true, []string{"104"}, "101", nil},
		{"свой код категории", components.IncidentCard{CategoryCode: ptr("104")}, true, []string{"101", "103"}, "104", nil},
		{"разбивка пострадавших при той же сумме", components.IncidentCard{Casualties: &cas{Injured: ptr(2)}}, true, []string{"101", "103"}, "101", &cas{Injured: ptr(1), Trapped: ptr(1)}},
		{"сумма изменилась — число из формы", components.IncidentCard{Casualties: &cas{Injured: ptr(3)}}, true, []string{"101", "103"}, "101", &cas{Injured: ptr(3)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := mergeCard(tc.next, &old, tc.keep)
			if !slices.Equal(codes(got), tc.wantServices) {
				t.Errorf("services %v, want %v", codes(got), tc.wantServices)
			}
			if deref(got.CategoryCode) != tc.wantCode {
				t.Errorf("category %q, want %q", deref(got.CategoryCode), tc.wantCode)
			}
			gj, _ := json.Marshal(got.Casualties)
			wj, _ := json.Marshal(tc.wantCas)
			if string(gj) != string(wj) {
				t.Errorf("casualties %s, want %s", gj, wj)
			}
		})
	}
	if got := mergeCard(components.IncidentCard{Description: ptr("x")}, nil, true); deref(got.Description) != "x" {
		t.Errorf("nil old: %+v", got)
	}
}

func TestSameDraft(t *testing.T) {
	t.Parallel()
	a := fireDraft("101")
	b := fireDraft("101")
	b.Services[0].History = nil // из jsonb без истории — нормализуется
	if !sameDraft(&a, &b) {
		t.Fatal("equal drafts reported as different")
	}
	if b.Services[0].History != nil {
		t.Fatal("sameDraft must not mutate its argument")
	}
	b.Attributes = map[string]any{"where": "house"}
	if sameDraft(&a, &b) {
		t.Fatal("different attributes reported as equal")
	}
}

func TestEtalonEncodeNormalizes(t *testing.T) {
	t.Parallel()
	e := etalonData{}
	enc, err := e.encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(enc.actions) != "[]" || string(enc.dialogue) != "{}" {
		t.Errorf("actions %s dialogue %s", enc.actions, enc.dialogue)
	}
	var d map[string]any
	if err := json.Unmarshal(enc.draft, &d); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"incidentTypeIds", "services"} {
		if arr, ok := d[k].([]any); !ok || arr == nil {
			t.Errorf("draft.%s = %v, want []", k, d[k])
		}
	}
	e2 := etalonData{Dialogue: &model.ExpectedDialogue{}}
	enc2, _ := e2.encode()
	if !strings.Contains(string(enc2.dialogue), `"checklist":[]`) {
		t.Errorf("dialogue %s", enc2.dialogue)
	}
	if !enc.equal(enc) || enc.equal(enc2) {
		t.Error("equal() is wrong")
	}
}

func TestHasEtalonPatch(t *testing.T) {
	t.Parallel()
	if hasEtalonPatch(&public.ScenarioPatch{Title: ptr("x"), CallScript: &public.CallScript{}}) {
		t.Error("title/callScript are not etalon fields")
	}
	for _, p := range []public.ScenarioPatch{
		{EtalonCard: &public.IncidentCard{}}, {EtalonDraft: &public.IncidentCardDraft{}}, {Scoring: &public.Scoring{}},
		{RequiredFields: &[]string{}}, {ExpectedActions: &[]public.ExpectedAction{}}, {ExpectedDialogue: &public.ExpectedDialogue{}},
	} {
		if !hasEtalonPatch(&p) {
			t.Errorf("%+v must be an etalon patch", p)
		}
	}
}

// ---------------------------------------------------------------- статус заявителя

func TestStatusByRole(t *testing.T) {
	t.Parallel()
	cases := []struct {
		role string
		want public.ApplicantStatus
	}{
		{"", public.Очевидец},
		{"очевидец", public.Очевидец},
		{"соседка", public.Очевидец},
		{"житель дома", public.Очевидец},
		{"жительница дома", public.Очевидец},
		{"прохожая", public.Очевидец},
		{"пешеход", public.Очевидец},
		{"пострадавший", public.Пострадавший},
		{"Пострадавшая, водитель", public.Пострадавший},
		{"потерпевший", public.Пострадавший},
		{"раненый водитель", public.Пострадавший},
		{"родственница", public.Родственник},
		{"мать", public.Родственник},
		{"мама ребёнка", public.Родственник},
		{"жена пострадавшего", public.Родственник},
		{"муж", public.Родственник},
		{"сестра", public.Родственник},
		{"бабушка", public.Родственник},
		{"внучка", public.Родственник},
		{"сын хозяйки квартиры", public.Родственник},
		{"ребёнок", public.Ребенок},
		{"ребенок 10 лет", public.Ребенок},
		{"школьница", public.Ребенок},
		{"подросток", public.Ребенок},
		{"водитель", public.Участник},
		{"участник ДТП", public.Участник},
		{"знакомая", public.Знакомый},
		{"друг", public.Знакомый},
		{"подруга хозяйки", public.Знакомый},
		{"коллега", public.Знакомый},
		// Регрессия: подстроки давали ложные совпадения.
		{"мужчина средних лет", public.Очевидец},
		{"пожилая женщина", public.Очевидец},
		{"медсестра поликлиники", public.Очевидец},
		{"другой водитель", public.Участник},
		{"сосед пострадавшего", public.Очевидец},
		{"ПРОХОЖИЙ", public.Очевидец},
	}
	for _, tc := range cases {
		if got := statusByRole(tc.role); got != tc.want {
			t.Errorf("statusByRole(%q) = %q, want %q", tc.role, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------- эталон из генерации

func generatedResult(t *testing.T, raw string) *components.ScenarioResult {
	t.Helper()
	var r components.ScenarioResult
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

const genResultJSON = `{
  "title": "Сильный запах газа на лестничной клетке",
  "call_script": {
    "caller": {"name": "Ольга", "phone": "+7 (915) 111-22-33", "role": "жена пострадавшего"},
    "address": {"raw": "Москва, ул. Лесная, 5"},
    "key_facts": ["Пахнет газом на 3 этаже"],
    "dialogue": {"persona": "взволнованная женщина", "facts": [
      {"id": "f1", "text": "Пахнет газом", "reveal": "volunteer"},
      {"id": "f1", "text": "Муж без сознания", "reveal": "on_request"},
      {"id": "f3", "text": "   ", "reveal": "on_request"}
    ]},
    "turns": [{"speaker": "caller", "text": "Алло, пахнет газом!"}]
  },
  "etalon_card": {
    "category_code": "999",
    "address": {"raw": "Москва, ул. Лесная, 5", "house": "5"},
    "applicant": {"name": "Ольга"},
    "services_to_notify": ["104", "103"],
    "description": "Запах газа в подъезде"
  },
  "expected_actions": [{"action_text": "Передать в 104"}],
  "expected_dialogue": {"checklist": [
    {"id": "c1", "text": "Уточнить адрес", "kind": "question", "required": true},
    {"id": "c1", "text": "Уточнить этаж", "kind": "question", "required": false}
  ]},
  "notes_for_teacher": "  проверьте адрес  ",
  "difficulty_estimate": 2
}`

func TestGeneratedEtalon(t *testing.T) {
	t.Parallel()
	s := unitService()
	res := generatedResult(t, genResultJSON)
	info, _ := s.cat.TypeByCode("104")
	cs := convert.CallScriptFromContract(&res.CallScript)

	e := s.generatedEtalon(res, &cs, info, true)
	if deref(e.Card.CategoryCode) != "104" {
		t.Errorf("unknown category code must fall back to the scenario's: %q", deref(e.Card.CategoryCode))
	}
	if !slices.Equal(e.Draft.IncidentTypeIds, []string{info.ID}) {
		t.Errorf("incidentTypeIds %v", e.Draft.IncidentTypeIds)
	}
	if deref(e.Draft.Phones.Aon) != "+7 (915) 111-22-33" {
		t.Errorf("aon %q", deref(e.Draft.Phones.Aon))
	}
	if e.Draft.Applicant.Status == nil || *e.Draft.Applicant.Status != public.Родственник {
		t.Errorf("status %v", e.Draft.Applicant.Status)
	}
	if len(e.Draft.Services) != 2 || !e.Draft.Services[0].IsPrimary || e.Draft.Services[0].Code != "104" {
		t.Errorf("services %+v", e.Draft.Services)
	}
	if !slices.Equal(e.Scoring.RequiredFields, model.DefaultRequiredFields()) {
		t.Errorf("required %v", e.Scoring.RequiredFields)
	}
	if !slices.Equal(e.Scoring.RequiredFacts, []string{"Пахнет газом на 3 этаже"}) {
		t.Errorf("facts %v", e.Scoring.RequiredFacts)
	}
	if len(e.Actions) != 1 {
		t.Errorf("actions %+v", e.Actions)
	}
	if e.Dialogue == nil || e.Dialogue.Checklist[1].ID != "c1-2" {
		t.Errorf("dialogue %+v", e.Dialogue)
	}

	noDlg := s.generatedEtalon(res, &cs, info, false)
	if noDlg.Dialogue != nil {
		t.Error("withDialogue=false must not keep the checklist")
	}

	// Известный код категории от LLM не подменяется.
	res2 := generatedResult(t, strings.Replace(genResultJSON, `"category_code": "999"`, `"category_code": "101"`, 1))
	if e2 := s.generatedEtalon(res2, &cs, info, true); deref(e2.Card.CategoryCode) != "101" {
		t.Errorf("known code replaced: %q", deref(e2.Card.CategoryCode))
	}
}

func TestEnrichDraft(t *testing.T) {
	t.Parallel()
	info := core.IncidentTypeInfo{ID: "type-1"}
	t.Run("пустая форма", func(t *testing.T) {
		t.Parallel()
		d := convert.EmptyDraft()
		cs := model.CallScript{Caller: model.Caller{Name: " Неизвестен ", Phone: " +7 900 ", Role: "школьник"}}
		enrichDraft(&d, &cs, info)
		if !slices.Equal(d.IncidentTypeIds, []string{"type-1"}) || deref(d.Phones.Aon) != "+7 900" {
			t.Errorf("draft %+v", d)
		}
		if d.Applicant.Name != nil {
			t.Errorf("«неизвестен» must not become a name: %q", *d.Applicant.Name)
		}
		if *d.Applicant.Status != public.Ребенок {
			t.Errorf("status %q", *d.Applicant.Status)
		}
	})
	t.Run("заполненное не трогается", func(t *testing.T) {
		t.Parallel()
		d := convert.EmptyDraft()
		d.IncidentTypeIds = []string{"other"}
		d.Phones.Aon = ptr("+7 111")
		d.Applicant.Name = ptr("Иван")
		d.Applicant.Status = ptr(public.Участник)
		cs := model.CallScript{Caller: model.Caller{Name: "Пётр", Phone: "+7 222", Role: "мать"}}
		enrichDraft(&d, &cs, info)
		if d.IncidentTypeIds[0] != "other" || *d.Phones.Aon != "+7 111" || *d.Applicant.Name != "Иван" || *d.Applicant.Status != public.Участник {
			t.Errorf("draft overwritten: %+v", d)
		}
	})
}

func TestRequiredFacts(t *testing.T) {
	t.Parallel()
	if got := requiredFacts(&model.CallScript{KeyFacts: []string{"a", "b"}}); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("key facts: %v", got)
	}
	if got := requiredFacts(&model.CallScript{}); got != nil {
		t.Errorf("no facts: %v", got)
	}
	cs := model.CallScript{Dialogue: &model.DialogueBrief{Facts: []model.DialogueFact{
		{ID: "1", Text: "Дым из окна", Reveal: model.RevealVolunteer},
		{ID: "2", Text: "Секрет", Reveal: model.RevealNever},
		{ID: "3", Text: "  ", Reveal: model.RevealOnRequest},
		{ID: "4", Text: "Этаж пятый", Reveal: model.RevealOnRequest},
	}}}
	if got := requiredFacts(&cs); !slices.Equal(got, []string{"Дым из окна", "Этаж пятый"}) {
		t.Errorf("brief projection: %v", got)
	}
}

// ---------------------------------------------------------------- проверки ввода

func TestCheckTitle(t *testing.T) {
	t.Parallel()
	cases := []struct {
		title string
		ok    bool
	}{
		{"", false},
		{"ДТ", false},
		{"ДТП", true}, // 3 символа кириллицы (6 байт) — годится
		{strings.Repeat("я", 300), true},
		{strings.Repeat("я", 301), false},
	}
	for _, tc := range cases {
		if got := checkTitle(tc.title) == ""; got != tc.ok {
			t.Errorf("checkTitle(%d runes) ok=%v, want %v", len([]rune(tc.title)), got, tc.ok)
		}
	}
}

func TestValidators(t *testing.T) {
	t.Parallel()
	for _, m := range []string{"cards", "card_actions", "both"} {
		if !validMode(m) {
			t.Errorf("mode %q", m)
		}
	}
	for _, m := range []string{"", "Cards", "voice"} {
		if validMode(m) {
			t.Errorf("mode %q must be invalid", m)
		}
	}
	for _, s := range []string{"draft", "generated", "validated", "rejected", "archived"} {
		if !validStatus(s) {
			t.Errorf("status %q", s)
		}
	}
	if validStatus("deleted") || validStatus("") {
		t.Error("unknown status accepted")
	}
	for d, ok := range map[int]bool{0: false, 1: true, 2: true, 3: true, 4: false, -1: false} {
		if validDifficulty(d) != ok {
			t.Errorf("difficulty %d", d)
		}
	}
}

func TestResolveCategory(t *testing.T) {
	t.Parallel()
	s := unitService()
	for _, key := range []string{"3021058b-40ef-4aed-9e0c-78b1e8184831", "it-101", "101", " 101 "} {
		info, id, ok := s.resolveCategory(key)
		if !ok || info.Code != "101" || id != uuid.MustParse("3021058b-40ef-4aed-9e0c-78b1e8184831") {
			t.Errorf("resolveCategory(%q) = %+v %v %v", key, info, id, ok)
		}
	}
	for _, key := range []string{"", "nope", "it-999"} {
		if _, _, ok := s.resolveCategory(key); ok {
			t.Errorf("resolveCategory(%q) must fail", key)
		}
	}
	if _, _, ok := New(Deps{}).resolveCategory("101"); ok {
		t.Error("no catalog — nothing resolves")
	}
}

func TestValidatePatch(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("ж", commentMax+1)
	many := make([]string, maxListItems+1)
	turns := make([]public.CallTurn, maxListItems+1)
	cases := []struct {
		name  string
		patch public.ScenarioPatch
		want  []string
	}{
		{"пустая правка", public.ScenarioPatch{}, nil},
		{"всё верно", public.ScenarioPatch{Title: ptr("  Пожар  "), Difficulty: ptr(public.Difficulty(2)), Mode: ptr(public.ScenarioPatchMode("both")), TeacherComment: ptr("ок")}, nil},
		{"короткое название", public.ScenarioPatch{Title: ptr("  аб  ")}, []string{"title"}},
		{"сложность", public.ScenarioPatch{Difficulty: ptr(public.Difficulty(5))}, []string{"difficulty"}},
		{"режим", public.ScenarioPatch{Mode: ptr(public.ScenarioPatchMode("voice"))}, []string{"mode"}},
		{"комментарий", public.ScenarioPatch{TeacherComment: &long}, []string{"teacherComment"}},
		{"реплики", public.ScenarioPatch{CallScript: &public.CallScript{Turns: turns}}, []string{"callScript.turns"}},
		{"maxTurns", public.ScenarioPatch{CallScript: &public.CallScript{Dialogue: &public.DialogueBrief{MaxTurns: ptr(1)}}}, []string{"callScript.dialogue.maxTurns"}},
		{"обязательные поля", public.ScenarioPatch{RequiredFields: &many}, []string{"requiredFields"}},
		{"чек-лист", public.ScenarioPatch{ExpectedDialogue: &public.ExpectedDialogue{Checklist: make([]public.DialogueChecklistItem, maxListItems+1)}}, []string{"expectedDialogue.checklist"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := validatePatch(&tc.patch)
			if len(got) != len(tc.want) {
				t.Fatalf("fields %v, want %v", got, tc.want)
			}
			for _, f := range tc.want {
				if got[f] == "" {
					t.Errorf("no message for %s: %v", f, got)
				}
			}
		})
	}
}

// ---------------------------------------------------------------- полнота для подтверждения

func completeRow() *store.ScenarioRow {
	d := fireDraft()
	return &store.ScenarioRow{
		Title:      "Пожар",
		CallScript: model.CallScript{Turns: []model.Turn{{Speaker: model.SpeakerCaller, Text: "Алло, пожар!"}}},
		Etalon: &store.EtalonRow{
			CardDraft: d,
			Scoring:   model.Scoring{RequiredFields: model.DefaultRequiredFields()},
		},
	}
}

func TestApproveGaps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(r *store.ScenarioRow)
		want   []string
	}{
		{"полный сценарий", func(*store.ScenarioRow) {}, nil},
		{"нет эталона и реплик", func(r *store.ScenarioRow) { r.Etalon = nil; r.CallScript.Turns = nil },
			[]string{"эталон карточки", "реплики заявителя"}},
		{"только подсказки оператору", func(r *store.ScenarioRow) {
			r.CallScript.Turns = []model.Turn{{Speaker: model.SpeakerOperatorHint, Text: "Спросите адрес"}}
		}, []string{"реплики заявителя"}},
		{"пустая форма АРМ", func(r *store.ScenarioRow) { r.Etalon.CardDraft = convert.EmptyDraft() },
			[]string{"тип происшествия в эталоне", "адрес в эталоне", "ФИО заявителя в эталоне", "статус заявителя в эталоне", "описание со слов заявителя в эталоне"}},
		{"ФИО не обязательно — не требуется", func(r *store.ScenarioRow) {
			r.Etalon.CardDraft.Applicant.Name = nil
			r.Etalon.Scoring.RequiredFields = []string{"address.raw"}
		}, nil},
		{"нет обязательных полей", func(r *store.ScenarioRow) { r.Etalon.Scoring.RequiredFields = nil }, []string{"обязательные поля"}},
		{"пустые типы происшествия", func(r *store.ScenarioRow) { r.Etalon.CardDraft.IncidentTypeIds = []string{" "} },
			[]string{"тип происшествия в эталоне"}},
		{"бриф без фактов и чек-листа", func(r *store.ScenarioRow) {
			r.CallScript.Dialogue = &model.DialogueBrief{Facts: []model.DialogueFact{{ID: "f", Text: "  "}}}
		}, []string{"факты в брифе заявителя", "чек-лист разговора"}},
		{"бриф с фактами и чек-листом", func(r *store.ScenarioRow) {
			r.CallScript.Dialogue = &model.DialogueBrief{Facts: []model.DialogueFact{{ID: "f", Text: "Дым"}}}
			r.Etalon.ExpectedDialogue = &model.ExpectedDialogue{Checklist: []model.ChecklistItem{{ID: "c", Text: "Адрес"}}}
		}, nil},
		{"пустое название", func(r *store.ScenarioRow) { r.Title = " " }, []string{"название"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := completeRow()
			tc.mutate(r)
			got := approveGaps(r)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("gaps %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------- бриф, чек-лист, списки

func TestSanitizeBrief(t *testing.T) {
	t.Parallel()
	sanitizeBrief(nil) // не паникует
	b := &model.DialogueBrief{Facts: []model.DialogueFact{
		{ID: "fact-1", Text: "Дым"},
		{ID: "", Text: "   "},
		{ID: " fact-1 ", Text: "Огонь"},
		{ID: "", Text: "Пятый этаж"},
		{ID: "fact-2", Text: "Газ"},
	}}
	sanitizeBrief(b)
	var ids []string
	for _, f := range b.Facts {
		ids = append(ids, f.ID)
	}
	if !slices.Equal(ids, []string{"fact-1", "fact-1-2", "fact-3", "fact-2"}) {
		t.Errorf("ids %v", ids)
	}
	empty := &model.DialogueBrief{Facts: []model.DialogueFact{{Text: ""}}}
	sanitizeBrief(empty)
	if empty.Facts == nil || len(empty.Facts) != 0 {
		t.Errorf("facts must be [] not nil: %#v", empty.Facts)
	}
	nilFacts := &model.DialogueBrief{}
	sanitizeBrief(nilFacts)
	if nilFacts.Facts == nil {
		t.Error("nil facts must become []")
	}
}

func TestSanitizeChecklist(t *testing.T) {
	t.Parallel()
	sanitizeChecklist(nil)
	d := &model.ExpectedDialogue{Checklist: []model.ChecklistItem{{ID: "a"}, {ID: "a"}, {ID: ""}, {ID: "a-2"}}}
	sanitizeChecklist(d)
	var ids []string
	for _, c := range d.Checklist {
		ids = append(ids, c.ID)
	}
	if !slices.Equal(ids, []string{"a", "a-2", "chk-3", "a-2-2"}) {
		t.Errorf("ids %v", ids)
	}
	e := &model.ExpectedDialogue{}
	sanitizeChecklist(e)
	if e.Checklist == nil {
		t.Error("checklist must be []")
	}
}

func TestCleanStrings(t *testing.T) {
	t.Parallel()
	if got := cleanStrings(nil); got == nil || len(got) != 0 {
		t.Errorf("nil -> %#v", got)
	}
	if got := cleanStrings([]string{" a ", "", "a", "b", "  "}); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("got %v", got)
	}
}

func TestSmallHelpers(t *testing.T) {
	t.Parallel()
	if truncate("привет", 3) != "при" || truncate("да", 3) != "да" {
		t.Error("truncate counts runes")
	}
	if nullIfEmpty("") != nil || *nullIfEmpty("x") != "x" {
		t.Error("nullIfEmpty")
	}
	if firstNonEmpty("", "  ", " b ", "c") != "b" || firstNonEmpty() != "" {
		t.Error("firstNonEmpty")
	}
	if deref[int](nil) != 0 || deref(ptr("x")) != "x" {
		t.Error("deref")
	}
	if !hasNonEmpty([]string{"", "x"}) || hasNonEmpty([]string{" "}) || hasNonEmpty(nil) {
		t.Error("hasNonEmpty")
	}
}

// ---------------------------------------------------------------- легенда и озвучка

func TestMergeCallScript(t *testing.T) {
	t.Parallel()
	old := model.CallScript{
		Caller: model.Caller{Name: "Мария", Voice: "baya"},
		Turns: []model.Turn{
			{Speaker: model.SpeakerCaller, Text: "Алло, пожар!", TTSHash: "h1"},
			{Speaker: model.SpeakerCaller, Text: "Пятый этаж", TTSHash: "h2"},
			{Speaker: model.SpeakerOperatorHint, Text: "Спросите адрес"},
		},
	}
	p := public.CallScript{
		KeyFacts: []string{" дым ", ""},
		Turns: []public.CallTurn{
			{Speaker: public.CallTurnSpeakerCaller, Text: "Алло, пожар!"},
			{Speaker: public.CallTurnSpeakerCaller, Text: "   "},
			{Speaker: public.CallTurnSpeakerCaller, Text: "Шестой этаж"},
			{Speaker: public.CallTurnSpeakerOperatorHint, Text: "Спросите адрес", TtsHash: ptr("bogus")},
		},
		Dialogue: &public.DialogueBrief{Persona: "паникует", Facts: []public.DialogueFact{
			{Id: "f", Text: "Дым", Reveal: public.Volunteer}, {Id: "f", Text: "Огонь", Reveal: public.OnRequest}, {Id: "g", Text: " ", Reveal: public.Never},
		}},
	}
	p.Caller.Name = ptr("Мария Ивановна")
	cs := mergeCallScript(&old, &p)
	if cs.Caller.Voice != "baya" {
		t.Errorf("voice not sent — must be kept: %q", cs.Caller.Voice)
	}
	if len(cs.Turns) != 3 {
		t.Fatalf("turns %+v", cs.Turns)
	}
	if cs.Turns[0].TTSHash != "h1" || cs.Turns[1].TTSHash != "" || cs.Turns[2].TTSHash != "" {
		t.Errorf("hashes %q %q %q", cs.Turns[0].TTSHash, cs.Turns[1].TTSHash, cs.Turns[2].TTSHash)
	}
	if cs.Dialogue == nil || len(cs.Dialogue.Facts) != 2 || cs.Dialogue.Facts[1].ID != "f-2" {
		t.Errorf("brief %+v", cs.Dialogue)
	}

	// Смена голоса сбрасывает все хэши.
	p.Caller.Voice = ptr("xenia")
	cs = mergeCallScript(&old, &p)
	if cs.Caller.Voice != "xenia" || cs.Turns[0].TTSHash != "" {
		t.Errorf("voice change must drop hashes: %+v", cs)
	}
}

func TestAssignHashes(t *testing.T) {
	t.Parallel()
	snap := settings.Defaults()
	cs := model.CallScript{Caller: model.Caller{Voice: "baya"}, Turns: []model.Turn{
		{Speaker: model.SpeakerCaller, Text: "Алло!"},
		{Speaker: model.SpeakerOperatorHint, Text: "Подсказка", TTSHash: "stale"},
		{Speaker: model.SpeakerCaller, Text: "Алло!"},
		{Speaker: model.SpeakerCaller, Text: "  "},
		{Speaker: model.SpeakerCaller, Text: "Помогите"},
	}}
	plan, changed := assignHashes(&cs, &snap)
	if !changed {
		t.Fatal("changed must be true")
	}
	h1 := convert.TTSHash("Алло!", "baya", snap.TTS.Rate)
	if cs.Turns[0].TTSHash != h1 || cs.Turns[2].TTSHash != h1 || cs.Turns[1].TTSHash != "" || cs.Turns[3].TTSHash != "" {
		t.Errorf("turns %+v", cs.Turns)
	}
	if len(plan.hashes) != 2 || plan.hashes[0] != h1 || plan.texts[h1] != "Алло!" || plan.voice != "baya" {
		t.Errorf("plan %+v", plan)
	}
	if _, changed := assignHashes(&cs, &snap); changed {
		t.Error("second pass must report no change")
	}
	// Без голоса заявителя — голос из настроек.
	cs2 := model.CallScript{Turns: []model.Turn{{Speaker: model.SpeakerCaller, Text: "Алло!"}}}
	plan2, _ := assignHashes(&cs2, &snap)
	if plan2.voice != snap.TTS.Voice || cs2.Turns[0].TTSHash != convert.TTSHash("Алло!", snap.TTS.Voice, snap.TTS.Rate) {
		t.Errorf("plan %+v", plan2)
	}
}

func TestSafeRelPath(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"ab/cd.wav":       true,
		"x.ogg":           true,
		"a/./b.wav":       true,
		"":                false,
		"  ":              false,
		"/etc/passwd":     false,
		"../secret.wav":   false,
		"a/../../b.wav":   false,
		"a\\b.wav":        false,
		"a\x00.wav":       false,
		"..":              false,
		"a/..":            false,
		"dir/..hidden.wv": true,
	}
	for p, want := range cases {
		if got := safeRelPath(p); got != want {
			t.Errorf("safeRelPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestAudioRef(t *testing.T) {
	t.Parallel()
	r := audioRef("ab/cd.ogg", 1200)
	if r.AudioUrl != convert.MediaURL("ab/cd.ogg") || r.DurationMs != 1200 || deref(r.Mime) != "audio/ogg" {
		t.Errorf("%+v", r)
	}
	if r := audioRef("ab/cd.bin", 0); deref(r.Mime) != "audio/wav" {
		t.Errorf("unknown extension must default to audio/wav: %+v", r)
	}
}

func TestBuildGenerateRequest(t *testing.T) {
	t.Parallel()
	jobID := uuid.New()
	info := core.IncidentTypeInfo{Code: "101", Name: "Пожар", Path: []string{"Пожары", "Пожар"}, Services: []string{"101"}}
	req := buildGenerateRequest(jobID, info, 2, "both", "", nil)
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if m["schema_version"] != "1" || m["request_id"] != jobID.String() || m["priority"] != float64(core.PriorityGenerate) || m["profile"] != "generate" {
		t.Errorf("envelope %s", raw)
	}
	spec := m["spec"].(map[string]any)
	if spec["difficulty"] != float64(2) || spec["mode"] != "both" {
		t.Errorf("spec %v", spec)
	}
	cat := spec["category"].(map[string]any)
	if cat["code"] != "101" || cat["name"] != "Пожар" {
		t.Errorf("category %v", cat)
	}
	if _, ok := cat["attributes"]; ok {
		t.Error("empty attributes must be omitted")
	}
	if _, ok := spec["teacher_comment"]; ok {
		t.Error("empty comment must be omitted")
	}
	if _, ok := spec["avoid_titles"]; ok {
		t.Error("empty avoid list must be omitted")
	}
	req = buildGenerateRequest(jobID, info, 1, "cards", "без жертв", []string{"Пожар в школе"})
	if deref(req.Spec.TeacherComment) != "без жертв" || len(deref(req.Spec.AvoidTitles)) != 1 {
		t.Errorf("spec %+v", req.Spec)
	}
}

type recordingRouter struct {
	got map[core.JobType]core.AIResultHandler
}

func (r *recordingRouter) Register(t core.JobType, h core.AIResultHandler) { r.got[t] = h }

func TestRegisterResults(t *testing.T) {
	t.Parallel()
	r := &recordingRouter{got: map[core.JobType]core.AIResultHandler{}}
	unitService().RegisterResults(r)
	if _, ok := r.got[core.JobGenerateScenario].(generateHandler); !ok {
		t.Error("generate_scenario handler not registered")
	}
	if _, ok := r.got[core.JobTTS].(ttsHandler); !ok {
		t.Error("tts handler not registered")
	}
	if len(r.got) != 2 {
		t.Errorf("handlers %v", r.got)
	}
}

func TestSeedServices(t *testing.T) {
	t.Parallel()
	s := unitService()
	if got := s.seedServices(nil); got == nil || len(got) != 0 {
		t.Errorf("nil -> %#v", got)
	}
	if got := New(Deps{}).seedServices([]public.AssignedService{{Code: "101"}}); got == nil || len(got) != 0 {
		t.Errorf("no catalog -> %#v", got)
	}
	got := s.seedServices([]public.AssignedService{
		{ServiceId: "svc-101", IsPrimary: true}, // id фикстуры -> служба справочника
		{Code: " 104 "},
		{ServiceId: "svc-unknown"},
	})
	if len(got) != 2 || got[0].Code != "101" || got[0].ServiceId != "8bfd7c9a-436b-497f-a30a-f9c3d543eb02" || !got[0].IsPrimary ||
		got[0].ShortName != "Служба 101" || got[1].Code != "104" {
		t.Errorf("services %+v", got)
	}
}

// Scoring.reaction (v1.3): решение из enum, норматив 5..600 с, обязательные статусы — только
// те, что диспетчер ставит после решения.
func TestValidatePatch_Reaction(t *testing.T) {
	t.Parallel()
	mk := func(dec string, within int, st ...public.ReactionStatus) *public.ScenarioPatch {
		r := &public.ReactionExpectation{}
		if dec != "" {
			d := public.ReactionExpectationDecision(dec)
			r.Decision = &d
		}
		if within != 0 {
			r.DecisionWithinSec = &within
		}
		if st != nil {
			r.RequiredStatuses = &st
		}
		return &public.ScenarioPatch{Scoring: &public.Scoring{Reaction: r}}
	}
	cases := []struct {
		name  string
		in    *public.ScenarioPatch
		field string
	}{
		{"корректный", mk("accept", 30, "Начало реагирования", "Прибытие"), ""},
		{"неизвестное решение", mk("maybe", 0), "scoring.reaction.decision"},
		{"норматив мал", mk("", 2), "scoring.reaction.decisionWithinSec"},
		{"норматив велик", mk("", 601), "scoring.reaction.decisionWithinSec"},
		{"системный статус", mk("", 0, "Получена службой"), "scoring.reaction.requiredStatuses"},
		{"решение статусом", mk("", 0, "Принята"), "scoring.reaction.requiredStatuses"},
		{"неизвестный статус", mk("", 0, "Выехали"), "scoring.reaction.requiredStatuses"},
	}
	for _, tc := range cases {
		fields := validatePatch(tc.in)
		if tc.field == "" && len(fields) != 0 {
			t.Errorf("%s: лишние ошибки %v", tc.name, fields)
		}
		if tc.field != "" && fields[tc.field] == "" {
			t.Errorf("%s: нет ошибки %s: %v", tc.name, tc.field, fields)
		}
	}
}

// Редактор без поддержки ракурса ДДС присылает scoring без reaction — эталон работы
// диспетчера сохраняется; присланный reaction — заменяет.
func TestApplyEtalonPatch_KeepsReaction(t *testing.T) {
	t.Parallel()
	s := unitService()
	d := fireDraft("101")
	keep := &model.ReactionExpectation{Decision: model.DecisionReject}
	cur := etalonData{Card: convert.DraftToCard(&d, s.cat), Draft: d,
		Scoring: model.Scoring{RequiredFields: []string{"address.raw"}, Reaction: keep}}

	next := s.applyEtalonPatch(cur, &public.ScenarioPatch{Scoring: &public.Scoring{RequiredFacts: &[]string{"пожар"}}})
	if next.Scoring.Reaction == nil || next.Scoring.Reaction.Decision != model.DecisionReject {
		t.Errorf("reaction потерян: %+v", next.Scoring)
	}
	dec := public.ReactionExpectationDecision("accept")
	next = s.applyEtalonPatch(cur, &public.ScenarioPatch{Scoring: &public.Scoring{Reaction: &public.ReactionExpectation{Decision: &dec}}})
	if next.Scoring.Reaction == nil || next.Scoring.Reaction.Decision != model.DecisionAccept {
		t.Errorf("reaction не заменён: %+v", next.Scoring)
	}
}
