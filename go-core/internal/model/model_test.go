package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"lct/gocore/internal/gen/components"
)

func TestCallScriptNormalize(t *testing.T) {
	t.Parallel()

	var c CallScript
	c.Normalize()
	if c.Turns == nil || len(c.Turns) != 0 {
		t.Fatalf("turns = %#v, want []", c.Turns)
	}
	if c.Dialogue != nil {
		t.Fatal("Normalize не должен создавать бриф")
	}

	c.Dialogue = &DialogueBrief{Persona: "Соседка"}
	c.Normalize()
	if c.Dialogue.Facts == nil {
		t.Fatal("dialogue.facts должен стать [], а не nil")
	}

	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"turns":[]`, `"facts":[]`} {
		if !strings.Contains(s, want) {
			t.Errorf("json %s: нет %s", s, want)
		}
	}
	// опциональное не пишется
	for _, absent := range []string{`"key_facts"`, `"tts_hash"`, `"voice"`, `"unknowns"`, `"max_turns"`} {
		if strings.Contains(s, absent) {
			t.Errorf("json %s: лишний ключ %s", s, absent)
		}
	}
}

func TestCallScriptOpeningTurn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		turns   []Turn
		wantIdx int
		wantOK  bool
		want    string
	}{
		{name: "нет реплик", turns: nil, wantIdx: -1},
		{name: "только подсказки", turns: []Turn{{Speaker: SpeakerOperatorHint, Text: "Уточните адрес"}}, wantIdx: -1},
		{name: "пустые реплики заявителя", turns: []Turn{{Speaker: SpeakerCaller, Text: "  \t"}, {Speaker: SpeakerCaller}}, wantIdx: -1},
		{
			name: "первая непустая реплика заявителя",
			turns: []Turn{
				{Speaker: SpeakerOperatorHint, Text: "Представьтесь"},
				{Speaker: SpeakerCaller, Text: " "},
				{Speaker: SpeakerCaller, Text: "Алло! У нас пожар!"},
				{Speaker: SpeakerCaller, Text: "Горит третий этаж"},
			},
			wantIdx: 2, wantOK: true, want: "Алло! У нас пожар!",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := CallScript{Turns: tt.turns}
			idx, turn, ok := c.OpeningTurn()
			if idx != tt.wantIdx || ok != tt.wantOK || turn.Text != tt.want {
				t.Fatalf("OpeningTurn = (%d, %q, %v), want (%d, %q, %v)", idx, turn.Text, ok, tt.wantIdx, tt.want, tt.wantOK)
			}
			if c.HasCallerTurn() != tt.wantOK {
				t.Fatalf("HasCallerTurn = %v, want %v", c.HasCallerTurn(), tt.wantOK)
			}
		})
	}
}

func TestCallScriptVoiceOr(t *testing.T) {
	t.Parallel()

	c := CallScript{}
	if got := c.VoiceOr("xenia"); got != "xenia" {
		t.Fatalf("пустой голос: %q", got)
	}
	c.Caller.Voice = "   "
	if got := c.VoiceOr("xenia"); got != "xenia" {
		t.Fatalf("голос из пробелов: %q", got)
	}
	c.Caller.Voice = " baya "
	if got := c.VoiceOr("xenia"); got != "baya" {
		t.Fatalf("голос заявителя: %q", got)
	}
}

func TestDefaultRequiredFieldsFreshSlice(t *testing.T) {
	t.Parallel()

	a := DefaultRequiredFields()
	want := []string{"applicant.name", "applicant.status", "address.raw", "incidentTypeIds", "description"}
	if !reflect.DeepEqual(a, want) {
		t.Fatalf("DefaultRequiredFields = %v", a)
	}
	a[0] = "испорчено"
	if DefaultRequiredFields()[0] != "applicant.name" {
		t.Fatal("DefaultRequiredFields должен возвращать новый срез")
	}
}

func TestScoringFieldWeight(t *testing.T) {
	t.Parallel()

	var nilScoring *Scoring
	if w := nilScoring.FieldWeight("address.raw"); w != 1 {
		t.Fatalf("nil scoring: %v", w)
	}
	s := &Scoring{FieldWeights: map[string]float64{"address.raw": 3, "description": 0, "applicant.name": -2}}
	tests := map[string]float64{
		"address.raw":     3,
		"description":     0, // явный 0 — поле без веса
		"applicant.name":  0, // отрицательный -> 0
		"incidentTypeIds": 1, // нет в карте -> 1
	}
	for path, want := range tests {
		if got := s.FieldWeight(path); got != want {
			t.Errorf("FieldWeight(%q) = %v, want %v", path, got, want)
		}
	}
	if w := (&Scoring{}).FieldWeight("x"); w != 1 {
		t.Fatalf("пустые веса: %v", w)
	}
}

func TestScoringIsZero(t *testing.T) {
	t.Parallel()

	var nilScoring *Scoring
	cases := []struct {
		s    *Scoring
		want bool
	}{
		{nilScoring, true},
		{&Scoring{}, true},
		{&Scoring{RequiredFields: []string{}}, true},
		{&Scoring{RequiredFields: []string{"address.raw"}}, false},
		{&Scoring{FieldWeights: map[string]float64{"a": 1}}, false},
		{&Scoring{RequiredFacts: []string{"горит квартира"}}, false},
		{&Scoring{ForbiddenFacts: []string{"есть погибшие"}}, false},
	}
	for i, c := range cases {
		if got := c.s.IsZero(); got != c.want {
			t.Errorf("#%d IsZero = %v, want %v", i, got, c.want)
		}
	}
}

func TestExpectedDialogue(t *testing.T) {
	t.Parallel()

	var nilED *ExpectedDialogue
	nilED.Normalize() // не паникует
	if _, ok := nilED.Item("q1"); ok {
		t.Fatal("nil чек-лист не содержит пунктов")
	}

	e := &ExpectedDialogue{}
	e.Normalize()
	if e.Checklist == nil {
		t.Fatal("checklist должен стать []")
	}
	b, _ := json.Marshal(e)
	if string(b) != `{"checklist":[]}` {
		t.Fatalf("json = %s", b)
	}

	e.Checklist = []ChecklistItem{
		{ID: "q1", Text: "Уточнить адрес", Kind: ChecklistQuestion, Required: true},
		{ID: "i1", Text: "Покинуть помещение", Kind: ChecklistInstruction},
	}
	it, ok := e.Item("i1")
	if !ok || it.Text != "Покинуть помещение" {
		t.Fatalf("Item(i1) = %+v, %v", it, ok)
	}
	it.Required = true // указатель на элемент среза, не копию
	if !e.Checklist[1].Required {
		t.Fatal("Item должен возвращать указатель на элемент")
	}
	if _, ok := e.Item("нет"); ok {
		t.Fatal("неизвестный id")
	}
}

func TestChecklistItemWeightJSON(t *testing.T) {
	t.Parallel()

	var it ChecklistItem
	if err := json.Unmarshal([]byte(`{"id":"q1","text":"т","kind":"question","required":true}`), &it); err != nil {
		t.Fatal(err)
	}
	if it.Weight != nil {
		t.Fatal("нет weight -> nil (контракт: default 1)")
	}
	if err := json.Unmarshal([]byte(`{"id":"q1","text":"т","kind":"question","required":true,"weight":0}`), &it); err != nil {
		t.Fatal(err)
	}
	if it.Weight == nil || *it.Weight != 0 {
		t.Fatal("weight 0 должен сохраниться как 0, а не пропасть")
	}
}

func TestGenerationMetaRoundTrip(t *testing.T) {
	t.Parallel()

	raw := `{"notes_for_teacher":"Проверьте адрес","difficulty_estimate":2,"job_id":"j-1",
	         "rejected_reason":"мало фактов","with_dialogue":false,"llm_model":"qwen2.5","tokens_in":120,
	         "nested":{"a":[1,2]}}`
	var g GenerationMeta
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		t.Fatal(err)
	}
	if g.NotesForTeacher != "Проверьте адрес" || g.DifficultyEstimate != 2 || g.JobID != "j-1" ||
		g.RejectedReason != "мало фактов" || g.WithDialogue == nil || *g.WithDialogue {
		t.Fatalf("известные поля: %+v", g)
	}
	if g.Extra["llm_model"] != "qwen2.5" || g.Extra["tokens_in"] != float64(120) || g.Extra["nested"] == nil {
		t.Fatalf("extra: %+v", g.Extra)
	}
	for _, k := range []string{"notes_for_teacher", "job_id", "with_dialogue"} {
		if _, dup := g.Extra[k]; dup {
			t.Fatalf("известный ключ %q попал в Extra", k)
		}
	}
	if g.IsZero() {
		t.Fatal("IsZero для заполненных метаданных")
	}

	b, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	var back, orig map[string]any
	_ = json.Unmarshal(b, &back)
	_ = json.Unmarshal([]byte(raw), &orig)
	if !reflect.DeepEqual(back, orig) {
		t.Fatalf("round trip:\n got %v\nwant %v", back, orig)
	}

	// указатель тоже маршалится плоско (MarshalJSON на значении)
	b2, _ := json.Marshal(&g)
	if string(b2) != string(b) {
		t.Fatalf("pointer marshal: %s vs %s", b2, b)
	}
}

func TestGenerationMetaZeroAndMap(t *testing.T) {
	t.Parallel()

	var g GenerationMeta
	if !g.IsZero() {
		t.Fatal("пустые метаданные")
	}
	b, _ := json.Marshal(g)
	if string(b) != `{}` {
		t.Fatalf("пустые -> %s, want {}", b)
	}
	if err := json.Unmarshal([]byte(`null`), &g); err != nil || !g.IsZero() {
		t.Fatalf("null: %v %+v", err, g)
	}
	if err := json.Unmarshal([]byte(`[1]`), &g); err == nil {
		t.Fatal("не объект -> ошибка")
	}
	// переиспользование: старые поля не протекают
	g = GenerationMeta{JobID: "old", Extra: map[string]any{"x": 1}}
	if err := json.Unmarshal([]byte(`{"notes_for_teacher":"н"}`), &g); err != nil {
		t.Fatal(err)
	}
	if g.JobID != "" || g.Extra != nil || g.NotesForTeacher != "н" {
		t.Fatalf("Unmarshal должен сбрасывать старое: %+v", g)
	}

	// Map — новая карта
	g = GenerationMeta{Extra: map[string]any{"k": "v"}}
	m := g.Map()
	m["k"] = "испорчено"
	m["new"] = 1
	if g.Extra["k"] != "v" || len(g.Extra) != 1 {
		t.Fatal("Map должна возвращать копию")
	}
	// неверные типы известных ключей — игнорируются, не паника
	if err := json.Unmarshal([]byte(`{"difficulty_estimate":"2","with_dialogue":"да","job_id":7}`), &g); err != nil {
		t.Fatal(err)
	}
	if g.DifficultyEstimate != 0 || g.WithDialogue != nil || g.JobID != "" {
		t.Fatalf("кривые типы: %+v", g)
	}
}

func TestGenerationMetaSetEngine(t *testing.T) {
	t.Parallel()

	var g GenerationMeta
	g.SetEngine(nil)
	if !g.IsZero() {
		t.Fatal("nil engine не меняет метаданные")
	}
	model, pv, tin := "qwen2.5:14b", "gen-v3", 900
	g.Extra = map[string]any{"llm_model": "old", "keep": true}
	g.SetEngine(&components.Engine{DurationMs: 1500, LlmModel: &model, PromptVersion: &pv, TokensIn: &tin})
	if g.Extra["llm_model"] != model || g.Extra["prompt_version"] != pv ||
		g.Extra["duration_ms"] != float64(1500) || g.Extra["tokens_in"] != float64(900) || g.Extra["keep"] != true {
		t.Fatalf("extra: %+v", g.Extra)
	}
	if _, ok := g.Extra["stt_model"]; ok {
		t.Fatal("пустые поля engine не пишутся")
	}
	// плоско в jsonb
	b, _ := json.Marshal(g)
	if !strings.Contains(string(b), `"llm_model":"qwen2.5:14b"`) || strings.Contains(string(b), `"engine"`) {
		t.Fatalf("json: %s", b)
	}
}

func TestTurnMetaOmitEmpty(t *testing.T) {
	t.Parallel()

	b, _ := json.Marshal(TurnMeta{})
	if string(b) != `{}` {
		t.Fatalf("пустая meta: %s", b)
	}
	b, _ = json.Marshal(TurnMeta{Fallback: true, TextRaw: "двадцать три", LatencyMs: 800})
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["fallback"] != true || m["text_raw"] != "двадцать три" || m["latency_ms"] != float64(800) {
		t.Fatalf("meta: %s", b)
	}
}

func TestTimingBlobKeys(t *testing.T) {
	t.Parallel()

	// VIEW student_progress читает within_norm — ключи snake_case и без omitempty
	b, _ := json.Marshal(TimingBlob{})
	for _, k := range []string{`"spent_ms":0`, `"limit_sec":0`, `"delta_ms":0`, `"within_norm":false`, `"reaction_ms":0`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("timing %s: нет %s", b, k)
		}
	}
}
