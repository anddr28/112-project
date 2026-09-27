package convert

import (
	"encoding/json"
	"strings"
	"testing"

	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/model"
)

func TestGrammarToPublic(t *testing.T) {
	t.Parallel()

	t.Run("пусто", func(t *testing.T) {
		t.Parallel()
		g := GrammarToPublic(100, nil, model.GrammarStats{})
		b, _ := json.Marshal(g)
		if string(b) != `{"remarks":[],"score":100,"stats":{"wordsChecked":0}}` {
			t.Fatalf("json = %s", b)
		}
	})

	t.Run("замечания", func(t *testing.T) {
		t.Parallel()
		sugg := []string{"пожар"}
		remarks := []components.GrammarRemark{
			{Field: "description", Offset: 5, Length: 5, Severity: "error", Message: "Опечатка", Rule: Ptr("MORFOLOGIK"), Suggestions: &sugg},
			{Field: "actionsTaken", Offset: 0, Length: 3, Severity: "style", Message: "Стиль"},
		}
		stats := model.GrammarStats{WordsChecked: 42, ErrorsBySeverity: map[string]int{"error": 1, "style": 1}}
		g := GrammarToPublic(87.5, remarks, stats)
		if g.Score != 87.5 || len(g.Remarks) != 2 || g.Stats.WordsChecked != 42 || (*g.Stats.ErrorsBySeverity)["error"] != 1 {
			t.Fatalf("got %+v", g)
		}
		r0, r1 := g.Remarks[0], g.Remarks[1]
		if r0.Field != "description" || r0.Offset != 5 || r0.Length != 5 || string(r0.Severity) != "error" ||
			Deref(r0.Rule) != "MORFOLOGIK" || (*r0.Suggestions)[0] != "пожар" {
			t.Errorf("remark0 = %+v", r0)
		}
		if r1.Suggestions == nil || *r1.Suggestions == nil {
			t.Error("suggestions — [] (не null)")
		}
		b, _ := json.Marshal(g)
		if !strings.Contains(string(b), `"suggestions":[]`) {
			t.Errorf("json = %s", b)
		}
	})
}

func TestSemanticToPublic(t *testing.T) {
	t.Parallel()

	s := SemanticToPublic(components.SemanticResult{Score: 70, Confidence: 0.8, SummaryForStudent: Ptr("  ")})
	b, _ := json.Marshal(s)
	for _, k := range []string{`"missingFacts":[]`, `"extraFacts":[]`, `"perField":[]`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("json без %s: %s", k, b)
		}
	}
	if strings.Contains(string(b), "summaryForStudent") {
		t.Errorf("пустой summary не отдаётся: %s", b)
	}

	missing := []string{"этаж"}
	per := []components.SemanticPerField{{Field: "description", Score: 50, Comment: Ptr("мало деталей")}, {Field: "actionsTaken", Score: 90, Comment: Ptr("")}}
	s = SemanticToPublic(components.SemanticResult{
		Score: 65, Confidence: 0.55, MissingFacts: &missing, PerField: &per, SummaryForStudent: Ptr("Уточняйте этаж"),
	})
	if s.Score != 65 || s.Confidence != 0.55 || (*s.MissingFacts)[0] != "этаж" || Deref(s.SummaryForStudent) != "Уточняйте этаж" {
		t.Fatalf("got %+v", s)
	}
	pf := *s.PerField
	if len(pf) != 2 || pf[0].Field != "description" || pf[0].Score != 50 || Deref(pf[0].Comment) != "мало деталей" || pf[1].Comment != nil {
		t.Fatalf("perField = %+v", pf)
	}
}

func TestDialogueToPublic(t *testing.T) {
	t.Parallel()

	w := 2.0
	exp := &model.ExpectedDialogue{Checklist: []model.ChecklistItem{
		{ID: "addr", Text: "Уточнить адрес", Kind: model.ChecklistQuestion, Required: true, Weight: &w},
		{ID: "calm", Text: "Успокоить заявителя", Kind: model.ChecklistBehavior},
	}}
	ev1, ev4 := 1, 4
	hits := []struct {
		Phrase string `json:"phrase"`
		TurnNo int    `json:"turn_no"`
	}{{Phrase: "успокойтесь", TurnNo: 6}, {Phrase: "не знаю", TurnNo: 5}}
	calm, clar := float32(80), float32(60)
	fillers := map[string]int{"ну": 3}
	avg := 1200
	r := components.DialogueResult{
		Score:      75,
		Confidence: 0.9,
		Checklist: []components.DialogueChecklistResult{
			{Id: "addr", Status: "done", EvidenceTurnNo: &ev4, Comment: Ptr("спросил на 2-й реплике")},
			{Id: "calm", Status: "missed", EvidenceTurnNo: &ev1, Comment: Ptr(" ")},
			{Id: "удалён", Status: "partial"},
		},
		ForbiddenHits:     &hits,
		SummaryForStudent: Ptr("Хорошо"),
		Speech:            components.SpeechMetrics{OperatorTurns: 4, OperatorWords: 57, FillerCount: Ptr(3), Fillers: &fillers, AvgResponseMs: &avg},
		Tone:              &components.ToneAssessment{Calmness: &calm, Clarity: &clar, Comment: Ptr("")},
	}
	d := DialogueToPublic(r, exp)

	if d.Score != 75 || d.Confidence != 0.9 || Deref(d.SummaryForStudent) != "Хорошо" {
		t.Fatalf("got %+v", d)
	}
	if len(d.Checklist) != 3 {
		t.Fatalf("checklist = %+v", d.Checklist)
	}
	c0, c1, c2 := d.Checklist[0], d.Checklist[1], d.Checklist[2]
	if c0.Text != "Уточнить адрес" || c0.Kind == nil || string(*c0.Kind) != "question" || !Deref(c0.Required) ||
		Deref(c0.EvidenceTurnNo) != 2 || Deref(c0.Comment) != "спросил на 2-й реплике" {
		t.Errorf("c0 = %+v (текст/вид из эталона, evidence 4 -> обмен 2)", c0)
	}
	if c1.Text != "Успокоить заявителя" || Deref(c1.EvidenceTurnNo) != 0 || c1.Comment != nil || Deref(c1.Required) {
		t.Errorf("c1 = %+v (evidence 1 -> вступление 0)", c1)
	}
	if c2.Text != "удалён" || c2.Kind != nil || c2.Required != nil || c2.EvidenceTurnNo != nil {
		t.Errorf("c2 = %+v (нет в эталоне -> текст = id)", c2)
	}
	fh := *d.ForbiddenHits
	if len(fh) != 2 || fh[0].TurnNo != 3 || fh[1].TurnNo != 2 || fh[0].Phrase != "успокойтесь" {
		t.Errorf("forbiddenHits = %+v (6 -> 3, 5 -> 2)", fh)
	}
	if d.Speech.OperatorTurns != 4 || d.Speech.OperatorWords != 57 || Deref(d.Speech.FillerCount) != 3 ||
		(*d.Speech.Fillers)["ну"] != 3 || Deref(d.Speech.AvgResponseMs) != 1200 {
		t.Errorf("speech = %+v", d.Speech)
	}
	if d.Tone == nil || *d.Tone.Calmness != 80 || *d.Tone.Clarity != 60 || d.Tone.Politeness != nil || d.Tone.Comment != nil {
		t.Errorf("tone = %+v", d.Tone)
	}
	b, _ := json.Marshal(d)
	if !strings.Contains(string(b), `"missingQuestions":[]`) {
		t.Errorf("json = %s", b)
	}
}

func TestDialogueToPublicNoEtalonNoData(t *testing.T) {
	t.Parallel()

	d := DialogueToPublic(components.DialogueResult{Score: 0}, nil)
	b, _ := json.Marshal(d)
	for _, k := range []string{`"checklist":[]`, `"forbiddenHits":[]`, `"missingQuestions":[]`, `"speech":{`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("json без %s: %s", k, b)
		}
	}
	if strings.Contains(string(b), `"tone"`) {
		t.Errorf("tone без данных не отдаётся: %s", b)
	}
}
