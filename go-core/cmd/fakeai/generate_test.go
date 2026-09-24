package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/components"
)

func genReq(t *testing.T, id uuid.UUID, spec obj) *aiservice.GenerateJobRequest {
	t.Helper()
	p := obj{"schema_version": "1", "request_id": id.String(), "spec": spec}
	body := mustJSON(t, p)
	top, err := decodeTop(body, true)
	if err != nil {
		t.Fatal(err)
	}
	pj, err := parseJob(kindGenerate, body, top)
	if err != nil {
		t.Fatalf("parseJob: %v", err)
	}
	return pj.payload.(*aiservice.GenerateJobRequest)
}

func spec(code, name string, diff int, mode string) obj {
	return obj{"category": obj{"code": code, "name": name}, "difficulty": diff, "mode": mode}
}

var placeholderRe = regexp.MustCompile(`\{[A-Za-z0-9]+\}`)

// checkScenario — инварианты сгенерированного сценария, на которые опираются go-core
// (подтверждение сценария, TTS, оценка) и сам имитатор (ходы диалога, оценка разговора).
func checkScenario(t *testing.T, sc components.ScenarioResult, diff int, mode string, tpl *sceneTpl) {
	t.Helper()
	raw, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	if m := placeholderRe.FindString(string(raw)); m != "" {
		t.Fatalf("неподставленный шаблон %s", m)
	}
	if n := utf8.RuneCountInString(sc.Title); n < 3 || n > 300 {
		t.Fatalf("title %q", sc.Title)
	}
	cs := sc.CallScript
	if len(cs.Turns) == 0 || cs.Turns[0].Speaker != components.CallScriptTurnsSpeakerCaller {
		t.Fatalf("turns %+v", cs.Turns)
	}
	for _, tr := range cs.Turns {
		if strings.TrimSpace(tr.Text) == "" || !tr.Speaker.Valid() {
			t.Fatalf("реплика %+v", tr)
		}
	}
	if deref(cs.Caller.Name) == "" || deref(cs.Caller.Phone) == "" || deref(cs.Caller.EmotionalState) == "" || deref(cs.Address.Raw) == "" {
		t.Fatalf("caller/address %+v %+v", cs.Caller, cs.Address)
	}
	b := cs.Dialogue
	if b == nil || b.Persona == "" || deref(b.SpeakingStyle) == "" || deref(b.MaxTurns) < 1 || b.Unknowns == nil {
		t.Fatalf("brief %+v", b)
	}
	ids := map[string]bool{}
	secret := false
	for _, f := range b.Facts {
		if f.Id == "" || ids[f.Id] || strings.TrimSpace(f.Text) == "" || !f.Reveal.Valid() || f.Hints == nil {
			t.Fatalf("факт %+v", f)
		}
		ids[f.Id] = true
		if f.Reveal == components.Never {
			secret = true
		}
	}
	if b.Facts[0].Id != "address" || b.Facts[0].Reveal != components.Volunteer || !ids["phone"] {
		t.Fatalf("факты адреса/телефона: %+v", b.Facts)
	}
	if secret != (diff == 3 && tpl.never != "") {
		t.Fatalf("never-факт при сложности %d: %v", diff, secret)
	}
	if want := len(b.Facts) - 2 - map[bool]int{true: 1}[secret]; len(deref(cs.KeyFacts)) != want {
		t.Fatalf("key_facts %d, фактов шаблона %d", len(deref(cs.KeyFacts)), want)
	}
	if diff == 1 && len(*b.Unknowns) > 1 {
		t.Fatalf("unknowns на сложности 1: %v", *b.Unknowns)
	}

	ed := sc.ExpectedDialogue
	if ed == nil || len(ed.Checklist) < 5 {
		t.Fatalf("чек-лист %+v", ed)
	}
	chk := map[string]bool{}
	required := 0
	for _, it := range ed.Checklist {
		if it.Id == "" || chk[it.Id] || it.Text == "" || !it.Kind.Valid() {
			t.Fatalf("пункт %+v", it)
		}
		chk[it.Id] = true
		if it.Required {
			required++
		}
	}
	if required == 0 || !chk["ask_address"] || !chk["say_dispatched"] {
		t.Fatalf("чек-лист без обязательных пунктов протокола: %v", chk)
	}

	card := sc.EtalonCard
	if card.Address == nil || deref(card.Address.Raw) == "" || len(deref(card.ServicesToNotify)) == 0 ||
		deref(card.Description) == "" || deref(card.ActionsTaken) == "" || card.Applicant == nil || card.Attributes == nil {
		t.Fatalf("эталонная карточка %+v", card)
	}
	if tpl.indoor != (card.Address.Apartment != nil && card.Address.Floor != nil && card.Address.Entrance != nil) {
		t.Fatalf("адрес в доме: %+v", card.Address)
	}
	if deref(sc.DifficultyEstimate) != diff || deref(sc.NotesForTeacher) == "" {
		t.Fatalf("difficulty/notes: %v %q", deref(sc.DifficultyEstimate), deref(sc.NotesForTeacher))
	}
	if mode == "cards" {
		if sc.ExpectedActions != nil {
			t.Fatal("expected_actions в режиме cards")
		}
	} else {
		ea := deref(sc.ExpectedActions)
		if len(ea) != 1 || ea[0].ActionText == "" || len(deref(ea[0].RequiredFacts)) == 0 || ea[0].ForbiddenFacts == nil {
			t.Fatalf("expected_actions %+v", ea)
		}
	}
}

func allScenes() []*sceneTpl {
	out := make([]*sceneTpl, 0, len(scenes)+1)
	for i := range scenes {
		out = append(out, &scenes[i])
	}
	return append(out, &genericScene)
}

func TestGenerateScenarioAllTemplates(t *testing.T) {
	t.Parallel()
	for _, tpl := range allScenes() {
		code := "zzz-unknown"
		if len(tpl.codes) > 0 {
			code = tpl.codes[0]
		}
		for diff := 1; diff <= 3; diff++ {
			for _, mode := range []string{"cards", "card_actions", "both"} {
				t.Run(fmt.Sprintf("%s/%d/%s", tpl.kind, diff, mode), func(t *testing.T) {
					t.Parallel()
					for seed := range 8 {
						id := uuid.NewSHA1(uuid.NameSpaceOID, fmt.Appendf(nil, "%s-%d-%s-%d", tpl.kind, diff, mode, seed))
						req := genReq(t, id, spec(code, "Обрушение перекрытия", diff, mode))
						if got := pickScene(req.Spec.Category.Code, req.Spec.Category.Name, nil); got != tpl {
							t.Fatalf("шаблон %s, ждали %s", got.kind, tpl.kind)
						}
						sc := generateScenario(req)
						checkScenario(t, sc, diff, mode, tpl)
						checkGeneratedIsUsable(t, sc)
					}
				})
			}
		}
	}
}

// checkGeneratedIsUsable — сгенерированные легенда и чек-лист проходят проверку payload'ов
// самого имитатора: go-core пришлёт их обратно в /v1/dialog/turn и /v1/jobs/dialogue.
func checkGeneratedIsUsable(t *testing.T, sc components.ScenarioResult) {
	t.Helper()
	turn := obj{
		"schema_version": "1", "request_id": uuid.NewString(), "attempt_id": uuid.NewString(), "turn_no": 1,
		"call_script": sc.CallScript, "history": []any{}, "operator_text": "Что случилось?",
	}
	req, err := parseDialogTurn(mustJSON(t, turn))
	if err != nil {
		t.Fatalf("call_script не принимается /v1/dialog/turn: %v", err)
	}
	if r := callerReply(&req, "Что случилось? Назовите адрес."); strings.TrimSpace(r.Text) == "" {
		t.Fatal("пустой ответ заявителя")
	}
	dj := obj{
		"schema_version": "1", "request_id": uuid.NewString(), "attempt_id": uuid.NewString(),
		"call_script": sc.CallScript, "etalon": obj{"expected_dialogue": sc.ExpectedDialogue},
		"transcript": []any{obj{"turn_no": 1, "speaker": "caller", "text": sc.CallScript.Turns[0].Text}},
	}
	body := mustJSON(t, dj)
	top, _ := decodeTop(body, true)
	if _, err := parseJob(kindDialogue, body, top); err != nil {
		t.Fatalf("expected_dialogue не принимается /v1/jobs/dialogue: %v", err)
	}
	sem := obj{
		"schema_version": "1", "request_id": uuid.NewString(), "attempt_id": uuid.NewString(), "mode": "cards",
		"call_script": sc.CallScript, "etalon": obj{"card": sc.EtalonCard}, "answer": obj{"card": sc.EtalonCard},
		"free_text_fields": []any{"description"},
	}
	body = mustJSON(t, sem)
	top, _ = decodeTop(body, true)
	pj, err := parseJob(kindSemantic, body, top)
	if err != nil {
		t.Fatalf("эталон не принимается /v1/jobs/semantic: %v", err)
	}
	// Регрессия: go-core берёт required_facts = key_facts легенды (scenarios.requiredFacts),
	// поэтому ответ, дословно повторяющий эталонное описание, должен покрывать их все —
	// иначе на стенде с имитатором «идеальная» карточка получала 33 балла.
	if res := evalSemantic(pj.payload.(*aiservice.SemanticJobRequest)); res.Score != 100 {
		t.Fatalf("эталон против своей легенды: %v, не отражено %q\nописание: %s",
			res.Score, *res.MissingFacts, deref(sc.EtalonCard.Description))
	}
	// Режим 2 — как шлёт go-core: scoring.required_facts = key_facts + expected_actions.
	// Эталонный текст действий покрывает свои required_facts и не содержит своих forbidden_facts.
	if sc.ExpectedActions == nil {
		return
	}
	ea := *sc.ExpectedActions
	act := obj{
		"schema_version": "1", "request_id": uuid.NewString(), "attempt_id": uuid.NewString(), "mode": "card_actions",
		"call_script": sc.CallScript, "free_text_fields": []any{"action_text"},
		"etalon": obj{"card": sc.EtalonCard, "expected_actions": ea, "scoring": obj{"required_facts": deref(sc.CallScript.KeyFacts)}},
		"answer": obj{"action_text": ea[0].ActionText},
	}
	body = mustJSON(t, act)
	top, _ = decodeTop(body, true)
	if pj, err = parseJob(kindSemantic, body, top); err != nil {
		t.Fatalf("card_actions не принимается: %v", err)
	}
	if res := evalSemantic(pj.payload.(*aiservice.SemanticJobRequest)); res.Score != 100 || len(*res.ExtraFacts) != 0 {
		t.Fatalf("эталонные действия: %v, не отражено %q, домыслы %q\nтекст: %s",
			res.Score, *res.MissingFacts, *res.ExtraFacts, ea[0].ActionText)
	}
}

func TestGenerateScenarioDeterministicByRequestID(t *testing.T) {
	t.Parallel()
	s := spec("101", "Пожар в жилом доме", 2, "both")
	id := uuid.MustParse("018f6b2a-7c1e-7f00-a000-000000000031")
	a := generateScenario(genReq(t, id, s))
	b := generateScenario(genReq(t, id, s))
	if !reflect.DeepEqual(a, b) {
		t.Fatal("один request_id — разные сценарии")
	}
	// Разные request_id — разные сценарии (адреса/имена из PRNG).
	seen := map[string]bool{}
	for i := range 20 {
		id := uuid.NewSHA1(uuid.NameSpaceURL, fmt.Appendf(nil, "gen-%d", i))
		sc := generateScenario(genReq(t, id, s))
		seen[deref(sc.CallScript.Address.Raw)+"|"+deref(sc.CallScript.Caller.Name)] = true
	}
	if len(seen) < 10 {
		t.Fatalf("слишком мало разнообразия: %d из 20", len(seen))
	}
}

func TestGenerateScenarioAvoidTitles(t *testing.T) {
	t.Parallel()
	fire := &scenes[0]
	avoid := make([]any, 0, len(fire.titles)+1)
	for _, tl := range fire.titles {
		avoid = append(avoid, "  "+strings.ToUpper(tl)+" ") // сравнение без регистра и пробелов
	}
	s := spec("101", "Пожар", 1, "cards")
	s["avoid_titles"] = avoid
	id := uuid.MustParse("018f6b2a-7c1e-7f00-a000-000000000032")
	sc := generateScenario(genReq(t, id, s))
	if !strings.HasSuffix(sc.Title, " (вариант 2)") {
		t.Fatalf("title %q", sc.Title)
	}
	avoid = append(avoid, sc.Title)
	s["avoid_titles"] = avoid
	if sc2 := generateScenario(genReq(t, id, s)); !strings.HasSuffix(sc2.Title, " (вариант 3)") {
		t.Fatalf("title %q", sc2.Title)
	}
	// Часть заголовков занята — берётся свободный.
	s["avoid_titles"] = []any{fire.titles[0], fire.titles[1]}
	for i := range 10 {
		id := uuid.NewSHA1(uuid.NameSpaceURL, fmt.Appendf(nil, "avoid-%d", i))
		if got := generateScenario(genReq(t, id, s)).Title; got != fire.titles[2] {
			t.Fatalf("title %q", got)
		}
	}
}

func TestGenerateScenarioTeacherCommentAndServices(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("018f6b2a-7c1e-7f00-a000-000000000033")
	s := spec("101", "Пожар", 1, "cards")
	s["teacher_comment"] = "Сделай панику, пусть будут пострадавшие"
	sc := generateScenario(genReq(t, id, s))
	if deref(sc.CallScript.Caller.EmotionalState) != "паника" || sc.EtalonCard.Casualties == nil ||
		deref(sc.EtalonCard.Casualties.Injured) != 1 || deref(sc.EtalonCard.Casualties.Trapped) != 1 {
		t.Fatalf("комментарий: %v %+v", deref(sc.CallScript.Caller.EmotionalState), sc.EtalonCard.Casualties)
	}
	if !strings.Contains(deref(sc.NotesForTeacher), "Учтён комментарий преподавателя") {
		t.Fatalf("notes %q", deref(sc.NotesForTeacher))
	}
	if want := []string{"101", "103", "104", "zhkh"}; !reflect.DeepEqual(deref(sc.EtalonCard.ServicesToNotify), want) {
		t.Fatalf("services %v", deref(sc.EtalonCard.ServicesToNotify))
	}

	// Регрессия: спокойный заявитель (в т.ч. мужчина — «спокоен») играется спокойным.
	s["teacher_comment"] = "Заявитель спокоен"
	for i := range 12 {
		id := uuid.NewSHA1(uuid.NameSpaceURL, fmt.Appendf(nil, "calm-%d", i))
		sc = generateScenario(genReq(t, id, s))
		st := deref(sc.CallScript.Caller.EmotionalState)
		if st != "спокоен" && st != "спокойна" {
			t.Fatalf("emotional_state %q", st)
		}
		if moodOf(st) != moodCalm || deref(sc.CallScript.Dialogue.SpeakingStyle) != "Говорит рассудительно, отвечает коротко и по делу" {
			t.Fatalf("%q: speaking_style %q", st, deref(sc.CallScript.Dialogue.SpeakingStyle))
		}
	}

	// Службы классификатора важнее шаблона.
	s = spec("101", "Пожар", 2, "both")
	s["category"].(obj)["services"] = []any{"101", "112"}
	sc = generateScenario(genReq(t, id, s))
	if !reflect.DeepEqual(deref(sc.EtalonCard.ServicesToNotify), []string{"101", "112"}) {
		t.Fatalf("services %v", deref(sc.EtalonCard.ServicesToNotify))
	}
	// Шаблон не портится между вызовами (копии, а не общие срезы).
	if !reflect.DeepEqual(scenes[0].services, []string{"101", "103", "104", "zhkh"}) {
		t.Fatalf("шаблон изменён: %v", scenes[0].services)
	}
}

func TestGenerateGenericSceneUsesCategoryName(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("018f6b2a-7c1e-7f00-a000-000000000034")
	sc := generateScenario(genReq(t, id, spec("9.9.9", "БПЛА над жилым кварталом", 2, "both")))
	if !strings.HasPrefix(sc.Title, "БПЛА над жилым кварталом") {
		t.Fatalf("title %q", sc.Title)
	}
	if !strings.Contains(deref(sc.NotesForTeacher), "нет отдельного шаблона") {
		t.Fatalf("notes %q", deref(sc.NotesForTeacher))
	}
	if deref(sc.EtalonCard.CategoryCode) != "9.9.9" || deref(sc.EtalonCard.Address.Landmark) == "" {
		t.Fatalf("карточка %+v", sc.EtalonCard)
	}
}

func TestPickScene(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		code, name string
		path       []string
		kind       string
	}{
		{"101", "", nil, "fire"},
		{" FIRE ", "", nil, "fire"},
		{"2.3.1", "Пожар в жилом доме", nil, "fire"},
		{"x", "Утечка газа", nil, "gas"},
		{"x", "Прочее", []string{"Дорожные происшествия", "ДТП"}, "dtp"},
		{"x", "Застрял в лифте", nil, "elevator"},
		{"x", "Консультация", nil, "generic"},
		{"", "", nil, "generic"},
	} {
		if got := pickScene(c.code, c.name, c.path); got.kind != c.kind {
			t.Errorf("pickScene(%q, %q, %v) = %s, ждали %s", c.code, c.name, c.path, got.kind, c.kind)
		}
	}
}

func TestEmotionFor(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		diff    int
		female  bool
		comment string
		want    string
	}{
		{1, false, "", "встревожен"}, {1, true, "", "встревожена"},
		{2, false, "", "взволнован"}, {2, true, "", "взволнована"},
		{3, false, "", "паника"}, {3, true, "", "паника"},
		{1, false, "пусть будет истерика", "паника"},
		{3, true, "спокойная женщина", "спокойна"},
		{3, false, "спокоен и собран", "спокоен"}, // регрессия: «спокоен» не содержит «спокой»
		{2, false, "беспокойный сосед", "взволнован"},
	} {
		if got := emotionFor(c.diff, c.female, c.comment); got != c.want {
			t.Errorf("emotionFor(%d, %v, %q) = %q, ждали %q", c.diff, c.female, c.comment, got, c.want)
		}
	}
}

func TestYearsWord(t *testing.T) {
	t.Parallel()
	for n, want := range map[int]string{1: "год", 2: "года", 4: "года", 5: "лет", 11: "лет", 12: "лет", 14: "лет",
		21: "год", 22: "года", 25: "лет", 101: "год", 111: "лет", 112: "лет", 0: "лет"} {
		if got := yearsWord(n); got != want {
			t.Errorf("yearsWord(%d) = %q", n, got)
		}
	}
}

func TestLowerFirst(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"Пожар в жилом доме": "пожар в жилом доме", "ДТП с пострадавшими": "ДТП с пострадавшими",
		"БПЛА": "БПЛА", "": "", "пожар": "пожар", "Я": "я", "112": "112",
	} {
		if got := lowerFirst(in); got != want {
			t.Errorf("lowerFirst(%q) = %q", in, got)
		}
	}
}

func TestSceneTemplatesWellFormed(t *testing.T) {
	t.Parallel()
	codes := map[string]string{}
	for _, tpl := range allScenes() {
		if len(tpl.titles) == 0 || len(tpl.roles) == 0 || tpl.opening == "" || len(tpl.fallback) == 0 ||
			tpl.desc == "" || tpl.actions == "" || len(tpl.services) == 0 || tpl.what.id == "" || tpl.expected == "" ||
			len(tpl.expFacts) == 0 {
			t.Errorf("шаблон %s неполный", tpl.kind)
		}
		for _, c := range tpl.codes {
			if prev, ok := codes[c]; ok {
				t.Errorf("код %q у %s и %s", c, prev, tpl.kind)
			}
			codes[c] = tpl.kind
		}
		ids := map[string]bool{"address": true, "phone": true, "secret": true}
		for _, f := range tpl.facts {
			if ids[f.id] || f.minDiff < 1 || f.minDiff > 3 || !f.reveal.Valid() || f.reveal == components.Never {
				t.Errorf("%s: факт %+v", tpl.kind, f)
			}
			ids[f.id] = true
		}
	}
	if len(floorWords) != 10 || len(floorPrep) != 10 {
		t.Fatal("этажи 2..9 должны иметь словоформы")
	}
}
