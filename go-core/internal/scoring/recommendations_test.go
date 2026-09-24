package scoring

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"lct/gocore/internal/gen/public"
)

func recByID(recs []public.Recommendation, id string) (public.Recommendation, bool) {
	for _, r := range recs {
		if r.Id == id {
			return r, true
		}
	}
	return public.Recommendation{}, false
}

func recItems(r public.Recommendation) []string {
	if r.Items == nil {
		return nil
	}
	return *r.Items
}

func recIDs(recs []public.Recommendation) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Id)
	}
	return out
}

var withinNorm = TimingResult{SpentMs: 50000, LimitSec: 60, DeltaMs: -10000, WithinNorm: true}

func TestRecommendationsEmpty(t *testing.T) {
	t.Parallel()
	recs := Recommendations(RecInput{Timing: withinNorm})
	if recs == nil || len(recs) != 0 {
		t.Fatalf("recs = %#v", recs)
	}
	if js := mustJSON(t, recs); js != "[]" {
		t.Fatalf("JSON = %s", js)
	}
}

// Регрессия: evaluations.timing по умолчанию '{}' → нулевой TimingResult (within_norm=false,
// limit_sec=0). Раньше это давало «Норматив заполнения — 0 с, … превышение на 0 с».
func TestRecommendationsNoTimingWithoutLimit(t *testing.T) {
	t.Parallel()
	for _, tr := range []TimingResult{{}, {SpentMs: 90000, DeltaMs: 90000}} {
		if recs := Recommendations(RecInput{Timing: tr}); len(recs) != 0 {
			t.Errorf("timing %+v: recs = %s", tr, mustJSON(t, recs))
		}
	}
	// То, что реально пишет Timing при отсутствии норматива, — тоже без рекомендации.
	_, tr := Timing(90000, 0, 0, defaultTolerance())
	if recs := Recommendations(RecInput{Timing: tr}); len(recs) != 0 {
		t.Errorf("recs = %s", mustJSON(t, recs))
	}
}

func TestRecommendationsTiming(t *testing.T) {
	t.Parallel()
	_, tr := Timing(132000, 60, 0, defaultTolerance())
	recs := Recommendations(RecInput{Timing: tr})
	if len(recs) != 1 {
		t.Fatalf("recs = %s", mustJSON(t, recs))
	}
	r := recs[0]
	want := "Норматив заполнения — 60 с, затрачено 2 мин 12 с (превышение на 1 мин 12 с). " +
		"Начинайте вводить адрес одновременно с разговором, не дожидаясь конца обращения."
	if r.Id != RecTiming || r.Kind != public.SlowTiming || r.Body != want || r.Items != nil {
		t.Fatalf("rec = %s", mustJSON(t, r))
	}
	// Без items в JSON.
	if strings.Contains(mustJSON(t, r), "items") {
		t.Fatalf("JSON = %s", mustJSON(t, r))
	}
	// Отрицательная дельта при within_norm=false (данные извне) — превышение «0 с», не минус.
	recs = Recommendations(RecInput{Timing: TimingResult{SpentMs: 1000, LimitSec: 60, DeltaMs: -59000}})
	if len(recs) != 1 || !strings.Contains(recs[0].Body, "превышение на 0 с") {
		t.Fatalf("recs = %s", mustJSON(t, recs))
	}
}

func TestRecommendationsFieldErrors(t *testing.T) {
	t.Parallel()
	errs := []public.FieldError{
		{Field: "applicant.name", Label: "ФИО заявителя", Kind: public.FieldErrorKindMissing},
		{Field: "address.raw", Label: "Адрес", Kind: public.FieldErrorKindWrong},
		{Field: "description", Label: "Описание со слов заявителя", Kind: public.FieldErrorKindMissing},
		{Field: "applicant.name", Label: "ФИО заявителя", Kind: public.FieldErrorKindMissing}, // повтор
		{Field: "phones.aon", Label: "  ", Kind: public.FieldErrorKindMissing},                // пустая подпись
	}
	recs := Recommendations(RecInput{FieldErrors: errs, Timing: withinNorm})
	if got := recIDs(recs); !reflect.DeepEqual(got, []string{RecMissing, RecWrong}) {
		t.Fatalf("ids = %v", got)
	}
	m, _ := recByID(recs, RecMissing)
	if m.Kind != public.WeakField || m.Body != "Перед сохранением карточки заполните обязательные поля:" ||
		!reflect.DeepEqual(recItems(m), []string{"ФИО заявителя", "Описание со слов заявителя"}) {
		t.Fatalf("missing rec = %s", mustJSON(t, m))
	}
	w, _ := recByID(recs, RecWrong)
	if w.Kind != public.WeakField || w.Body != "Значения не совпадают с эталоном:" || !reflect.DeepEqual(recItems(w), []string{"Адрес"}) {
		t.Fatalf("wrong rec = %s", mustJSON(t, w))
	}
}

const servicesExtraBody = "Назначены службы сверх необходимых — лишний выезд отвлекает силы от других вызовов:"

// Лишние службы — по именам из FieldErrors: и из свежего расчёта ([]string), и после
// чтения field_errors из jsonb ([]any).
func TestRecommendationsExtraServices(t *testing.T) {
	t.Parallel()
	st, et := studentFire(), etalonFire()
	st.Services = append(st.Services, public.AssignedService{Code: "103"}, public.AssignedService{Code: "gormost"})
	errs := FieldErrors(st, et, FieldSpec{Required: []string{"services"}}, testCatalog())
	if len(errs) != 1 || errs[0].Kind != public.FieldErrorKindExtra {
		t.Fatalf("errs = %s", mustJSON(t, errs))
	}

	var fromDB []public.FieldError
	if err := json.Unmarshal([]byte(mustJSON(t, errs)), &fromDB); err != nil {
		t.Fatal(err)
	}
	if _, ok := fromDB[0].Actual.([]any); !ok {
		t.Fatalf("jsonb actual is %T", fromDB[0].Actual)
	}

	for name, in := range map[string][]public.FieldError{"fresh": errs, "from jsonb": fromDB} {
		recs := Recommendations(RecInput{FieldErrors: in, Timing: withinNorm})
		r, ok := recByID(recs, RecExtra)
		if !ok || len(recs) != 1 {
			t.Fatalf("%s: recs = %s", name, mustJSON(t, recs))
		}
		if r.Kind != public.WeakField || r.Body != servicesExtraBody ||
			!reflect.DeepEqual(recItems(r), []string{"Служба 103", "Гормост"}) {
			t.Fatalf("%s: rec = %s", name, mustJSON(t, r))
		}
	}
}

// Регрессия: лишние службы не выделить по именам (две разные службы с одинаковым именем) —
// раньше рекомендация молча пропадала, хотя балл за поле снижен.
func TestRecommendationsExtraServicesFallbackToLabel(t *testing.T) {
	t.Parallel()
	errs := []public.FieldError{{
		Field: "services", Label: "Службы", Kind: public.FieldErrorKindExtra,
		Expected: []string{"Аварийная"}, Actual: []string{"Аварийная", "Аварийная"},
	}}
	recs := Recommendations(RecInput{FieldErrors: errs, Timing: withinNorm})
	r, ok := recByID(recs, RecExtra)
	if !ok || !reflect.DeepEqual(recItems(r), []string{"Службы"}) || r.Body != servicesExtraBody {
		t.Fatalf("recs = %s", mustJSON(t, recs))
	}
	// Пустой actual (например, повреждённый jsonb) — тоже не теряем.
	errs[0].Actual, errs[0].Expected = nil, nil
	if r, ok := recByID(Recommendations(RecInput{FieldErrors: errs, Timing: withinNorm}), RecExtra); !ok || len(recItems(r)) != 1 {
		t.Fatalf("nil actual: rec = %s", mustJSON(t, r))
	}
}

func TestRecommendationsExtraGeneric(t *testing.T) {
	t.Parallel()
	errs := []public.FieldError{
		{Field: "services_to_notify", Label: "Службы", Kind: public.FieldErrorKindExtra,
			Expected: []any{"Служба 101"}, Actual: []any{"Служба 101", "Служба 104"}},
		{Field: "attributes.indoor_objects", Label: "Объекты в помещении", Kind: public.FieldErrorKindExtra},
	}
	recs := Recommendations(RecInput{FieldErrors: errs, Timing: withinNorm})
	r, ok := recByID(recs, RecExtra)
	if !ok || r.Body != "Указано больше, чем требует ситуация по эталону:" ||
		!reflect.DeepEqual(recItems(r), []string{"Служба 104", "Объекты в помещении"}) {
		t.Fatalf("recs = %s", mustJSON(t, recs))
	}
}

func TestRecommendationsSemantic(t *testing.T) {
	t.Parallel()
	facts := []string{"этаж пожара", " ", "угроза людям", "этаж пожара", ""}
	orig := append([]string(nil), facts...)
	recs := Recommendations(RecInput{SemanticMissingFacts: facts, CategoryName: " Пожар ", Timing: withinNorm})
	r, ok := recByID(recs, RecFacts)
	if !ok || r.Kind != public.General ||
		r.Body != "Отработайте тему «Пожар». В описании со слов заявителя не зафиксировано:" ||
		!reflect.DeepEqual(recItems(r), []string{"этаж пожара", "угроза людям"}) {
		t.Fatalf("rec = %s", mustJSON(t, recs))
	}
	if !reflect.DeepEqual(facts, orig) {
		t.Fatalf("input mutated: %q", facts)
	}
	// Без категории — без «Отработайте тему».
	recs = Recommendations(RecInput{SemanticMissingFacts: []string{"адрес"}, Timing: withinNorm})
	if r, _ := recByID(recs, RecFacts); r.Body != "В описании со слов заявителя не зафиксировано:" {
		t.Fatalf("rec = %s", mustJSON(t, r))
	}
	// Только пустые факты — рекомендации нет.
	if recs := Recommendations(RecInput{SemanticMissingFacts: []string{" ", ""}, Timing: withinNorm}); len(recs) != 0 {
		t.Fatalf("recs = %s", mustJSON(t, recs))
	}
}

func TestRecommendationsDialogue(t *testing.T) {
	t.Parallel()
	low := 45.5
	cases := []struct {
		name  string
		in    RecInput
		ids   []string
		check func(t *testing.T, recs []public.Recommendation)
	}{
		{name: "missing questions", in: RecInput{DialogueMissing: []string{"Уточнить адрес", "", "Уточнить адрес", "Есть ли пострадавшие"}},
			ids: []string{RecDialogueQuestions},
			check: func(t *testing.T, recs []public.Recommendation) {
				r := recs[0]
				if r.Kind != public.DialoguePattern || !strings.HasPrefix(r.Body, "В разговоре с заявителем не заданы обязательные вопросы протокола.") ||
					!reflect.DeepEqual(recItems(r), []string{"Уточнить адрес", "Есть ли пострадавшие"}) {
					t.Fatalf("rec = %s", mustJSON(t, r))
				}
			}},
		{name: "forbidden phrases", in: RecInput{DialogueForbiddenHits: []string{"успокойтесь", "успокойтесь"}},
			ids: []string{RecDialogueForbidden},
			check: func(t *testing.T, recs []public.Recommendation) {
				if !reflect.DeepEqual(recItems(recs[0]), []string{"успокойтесь"}) {
					t.Fatalf("rec = %s", mustJSON(t, recs[0]))
				}
			}},
		{name: "fillers above threshold", in: RecInput{SpeechFillerCount: 9, SpeechFillers: map[string]int{
			"ну": 3, "это": 2, "как бы": 2, "вот": 1, "короче": 1, "типа": 1, " ": 5, "значит": 0,
		}}, ids: []string{RecDialogueFillers},
			check: func(t *testing.T, recs []public.Recommendation) {
				r := recs[0]
				if !strings.HasPrefix(r.Body, "Слов-паразитов в речи: 9.") {
					t.Fatalf("body = %q", r.Body)
				}
				// по убыванию частоты, при равенстве — по алфавиту, не больше 5
				want := []string{"«ну» — 3", "«как бы» — 2", "«это» — 2", "«вот» — 1", "«короче» — 1"}
				if !reflect.DeepEqual(recItems(r), want) {
					t.Fatalf("items = %q, want %q", recItems(r), want)
				}
			}},
		{name: "fillers at threshold → none", in: RecInput{SpeechFillerCount: FillerThreshold, SpeechFillers: map[string]int{"ну": 3}}},
		{name: "fillers without breakdown", in: RecInput{SpeechFillerCount: 4}, ids: []string{RecDialogueFillers},
			check: func(t *testing.T, recs []public.Recommendation) {
				if recs[0].Items != nil {
					t.Fatalf("items = %v", recItems(recs[0]))
				}
			}},
		{name: "low score without specifics", in: RecInput{DialogueScore: &low}, ids: []string{RecDialogueScore},
			check: func(t *testing.T, recs []public.Recommendation) {
				if !strings.HasPrefix(recs[0].Body, "Разговор с заявителем оценён на 45.5 из 100.") {
					t.Fatalf("body = %q", recs[0].Body)
				}
			}},
		{name: "low score with specifics → no generic rec", in: RecInput{DialogueScore: &low, DialogueMissing: []string{"Адрес"}},
			ids: []string{RecDialogueQuestions}},
		{name: "score at threshold → none", in: RecInput{DialogueScore: ptr(float64(DialogueLowScore))}},
		{name: "all dialogue recs", in: RecInput{DialogueMissing: []string{"a"}, DialogueForbiddenHits: []string{"b"}, SpeechFillerCount: 4},
			ids: []string{RecDialogueQuestions, RecDialogueForbidden, RecDialogueFillers}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.in.Timing = withinNorm
			recs := Recommendations(c.in)
			if got := recIDs(recs); !reflect.DeepEqual(got, append([]string{}, c.ids...)) {
				t.Fatalf("ids = %v, want %v", got, c.ids)
			}
			if c.check != nil {
				c.check(t, recs)
			}
		})
	}
}

func TestRecommendationsGrammar(t *testing.T) {
	t.Parallel()
	if recs := Recommendations(RecInput{GrammarRemarks: GrammarRemarksThreshold, Timing: withinNorm}); len(recs) != 0 {
		t.Fatalf("at threshold: %s", mustJSON(t, recs))
	}
	recs := Recommendations(RecInput{GrammarRemarks: 3, Timing: withinNorm})
	if len(recs) != 1 || recs[0].Id != RecGrammar || recs[0].Kind != public.GrammarPattern ||
		!strings.HasPrefix(recs[0].Body, "Замечаний к тексту: 3.") || recs[0].Items != nil {
		t.Fatalf("recs = %s", mustJSON(t, recs))
	}
}

// Полный набор: порядок как на экране результатов, id уникальны и стабильны между расчётами,
// kind — из CHECK recommendations.kind.
func TestRecommendationsOrderAndStableIDs(t *testing.T) {
	t.Parallel()
	_, tr := Timing(200000, 60, 0, defaultTolerance())
	low := 30.0
	in := RecInput{
		FieldErrors: []public.FieldError{
			{Field: "services", Label: "Службы", Kind: public.FieldErrorKindExtra, Expected: []string{"A"}, Actual: []string{"A", "B"}},
			{Field: "address.raw", Label: "Адрес", Kind: public.FieldErrorKindWrong},
			{Field: "applicant.name", Label: "ФИО заявителя", Kind: public.FieldErrorKindMissing},
		},
		Timing:                tr,
		CategoryName:          "Пожар",
		SemanticMissingFacts:  []string{"этаж"},
		GrammarRemarks:        5,
		DialogueMissing:       []string{"адрес"},
		DialogueForbiddenHits: []string{"не кричите"},
		DialogueScore:         &low,
		SpeechFillerCount:     7,
		SpeechFillers:         map[string]int{"ну": 7},
	}
	recs := Recommendations(in)
	want := []string{RecMissing, RecWrong, RecExtra, RecTiming, RecFacts, RecDialogueQuestions,
		RecDialogueForbidden, RecDialogueFillers, RecGrammar}
	if got := recIDs(recs); !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	if again := Recommendations(in); !reflect.DeepEqual(again, recs) {
		t.Fatal("recommendations differ between runs")
	}
	allowed := map[public.RecommendationKind]bool{
		public.WeakCategory: true, public.WeakField: true, public.SlowTiming: true,
		public.GrammarPattern: true, public.DialoguePattern: true, public.General: true,
	}
	for _, r := range recs {
		if !allowed[r.Kind] {
			t.Errorf("rec %s: kind %q not allowed by schema", r.Id, r.Kind)
		}
		if strings.TrimSpace(r.Body) == "" {
			t.Errorf("rec %s: empty body", r.Id)
		}
		if r.Items != nil && len(*r.Items) == 0 {
			t.Errorf("rec %s: empty items should be omitted", r.Id)
		}
	}
}

func TestTopFillers(t *testing.T) {
	t.Parallel()
	if got := topFillers(nil); got != nil {
		t.Fatalf("topFillers(nil) = %v", got)
	}
	if got := topFillers(map[string]int{"": 3, "ну": -1}); len(got) != 0 {
		t.Fatalf("topFillers(junk) = %v", got)
	}
	got := topFillers(map[string]int{" ну ": 2, "эм": 2, "а": 5})
	if want := []string{"«а» — 5", "«ну» — 2", "«эм» — 2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("topFillers = %q, want %q", got, want)
	}
}

func TestListDiff(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b any
		want []string
	}{
		{[]string{"a", "b"}, []string{"a"}, []string{"b"}},
		{[]any{"a", "b", 3}, []any{"b"}, []string{"a"}},
		{"x", nil, []string{"x"}},
		{"", nil, nil},
		{nil, []string{"a"}, nil},
		{map[string]any{"a": 1}, nil, nil},
	}
	for _, c := range cases {
		if got := listDiff(c.a, c.b); !reflect.DeepEqual(got, c.want) {
			t.Errorf("listDiff(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
