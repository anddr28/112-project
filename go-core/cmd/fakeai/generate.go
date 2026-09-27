package main

import (
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/components"
)

// Анонимные структуры сгенерированных типов — псевдонимы с теми же тегами, чтобы
// собирать литералы (идентичность типа в Go включает теги полей).
type (
	scriptTurn = struct {
		Speaker components.CallScriptTurnsSpeaker `json:"speaker"`
		Text    string                            `json:"text"`
		TtsHash *string                           `json:"tts_hash,omitempty"`
	}
	applicantT = struct {
		Name  *string `json:"name,omitempty"`
		Phone *string `json:"phone,omitempty"`
	}
	casualtiesT = struct {
		Dead    *int `json:"dead,omitempty"`
		Injured *int `json:"injured,omitempty"`
		Trapped *int `json:"trapped,omitempty"`
	}
)

// pickScene — шаблон по коду категории, затем по основам слов в названии/пути.
func pickScene(code, name string, path []string) *sceneTpl {
	c := strings.ToLower(strings.TrimSpace(code))
	for i := range scenes {
		for _, sc := range scenes[i].codes {
			if c == sc {
				return &scenes[i]
			}
		}
	}
	hay := norm(name + " " + strings.Join(path, " "))
	for i := range scenes {
		if _, ok := containsAny(hay, scenes[i].match); ok {
			return &scenes[i]
		}
	}
	return &genericScene
}

// generateScenario — «LLM-генерация» сценария: шаблон категории + детерминированный
// PRNG с зерном из request_id (повтор задачи даёт тот же сценарий — идемпотентность).
func generateScenario(req *aiservice.GenerateJobRequest) components.ScenarioResult {
	spec := &req.Spec
	id := req.RequestId
	rng := rand.New(rand.NewPCG(binary.BigEndian.Uint64(id[:8]), binary.BigEndian.Uint64(id[8:])))
	pick := func(list []string) string { return list[rng.IntN(len(list))] }

	sc := pickScene(spec.Category.Code, spec.Category.Name, deref(spec.Category.Path))
	diff := min(max(spec.Difficulty, 1), 3)
	comment := trimmed(spec.TeacherComment)

	// Адрес и место в доме.
	a := moscowAddresses[rng.IntN(len(moscowAddresses))]
	entrance, floor := 1+rng.IntN(6), 2+rng.IntN(7) // этажи 2..8: {floor2} остаётся в пределах 9
	apt := (entrance-1)*36 + (floor-1)*4 + 1 + rng.IntN(4)
	spoken := a.street + ", " + a.house
	full := a.streetFull + ", дом " + a.house
	raw := "Москва, " + a.streetFull + ", " + a.house
	if a.corp != "" {
		spoken += ", корпус " + a.corp
		full += ", корпус " + a.corp
		raw += ", корпус " + a.corp
	}
	if sc.indoor {
		spoken += ", подъезд " + strconv.Itoa(entrance)
	}

	// Заявитель.
	role := sc.roles[rng.IntN(len(sc.roles))]
	age := 22 + rng.IntN(55)
	var name string
	if role.female {
		name = pick(femaleNames)
		if age >= 55 {
			name += " " + pick(femalePatrons)
		}
	} else {
		name = pick(maleNames)
		if age >= 55 {
			name += " " + pick(malePatrons)
		}
	}
	p := phonePrefixes[rng.IntN(len(phonePrefixes))]
	phone := fmt.Sprintf("+7 (%d) %03d-%02d-%02d", p, rng.IntN(1000), rng.IntN(100), rng.IntN(100))
	emotion := emotionFor(diff, role.female, comment)

	categoryLow := lowerFirst(spec.Category.Name)
	repl := strings.NewReplacer(
		"{addr}", spoken, "{full}", full, "{entrance}", strconv.Itoa(entrance), "{floor}", strconv.Itoa(floor),
		"{floor2}", strconv.Itoa(floor+1), "{floorWord}", capitalize(floorWords[floor]), "{floorP}", floorPrep[floor],
		"{apt}", strconv.Itoa(apt), "{landmark}", sc.landmark, "{category}", spec.Category.Name,
		"{categoryLow}", categoryLow, "{phone}", phone,
	)
	r := repl.Replace

	// Факты брифа: адрес (сам называет) → факты шаблона по сложности → телефон → «никогда».
	addrFact := full
	if sc.indoor {
		addrFact += ", подъезд " + strconv.Itoa(entrance)
	} else if sc.landmark != "" {
		addrFact += ", " + sc.landmark
	}
	facts := []components.DialogueFact{{Id: "address", Text: addrFact, Reveal: components.Volunteer, Hints: ptr(addressHints)}}
	keyFacts := make([]string, 0, len(sc.facts))
	var desc strings.Builder
	desc.WriteString(r(sc.desc))
	for _, f := range sc.facts {
		if f.minDiff > diff {
			continue
		}
		text := r(f.text)
		facts = append(facts, components.DialogueFact{Id: f.id, Text: text, Reveal: f.reveal, Hints: ptr(f.hints)})
		keyFacts = append(keyFacts, text)
		if f.desc != "" {
			desc.WriteByte(' ')
			desc.WriteString(r(f.desc))
		}
	}
	facts = append(facts, components.DialogueFact{Id: "phone", Text: "Звоню со своего мобильного, номер " + phone,
		Reveal: components.OnRequest, Hints: ptr(phoneHints)})
	if diff == 3 && sc.never != "" {
		facts = append(facts, components.DialogueFact{Id: "secret", Text: sc.never, Reveal: components.Never, Hints: &[]string{}})
	}
	unknowns := sc.unknowns
	if diff == 1 && len(unknowns) > 1 {
		unknowns = unknowns[:1]
	}
	unknowns = append([]string(nil), unknowns...)

	md := moodOf(emotion)
	style := [3]string{
		moodAgitated: "Говорит быстро, волнуется, но отвечает по существу",
		moodPanic:    "Говорит сбивчиво, перебивает себя, повторяет просьбы поторопиться",
		moodCalm:     "Говорит рассудительно, отвечает коротко и по делу",
	}[md]
	persona := fmt.Sprintf("%s, %s, %d %s; %s", name, role.role, age, yearsWord(age), sc.where)

	// Легенда: вступление + 3 реплики на случай недоступности LLM (fallback по кругу).
	turns := make([]scriptTurn, 0, 1+len(sc.fallback))
	turns = append(turns, scriptTurn{Speaker: components.CallScriptTurnsSpeakerCaller, Text: r(sc.opening)})
	for _, l := range sc.fallback {
		turns = append(turns, scriptTurn{Speaker: components.CallScriptTurnsSpeakerCaller, Text: r(l)})
	}

	var cs components.CallScript
	cs.Caller.Name = ptr(name)
	cs.Caller.Phone = ptr(phone)
	cs.Caller.Role = ptr(role.role)
	cs.Caller.EmotionalState = ptr(emotion)
	cs.Address = components.Address{Raw: ptr(spoken), City: ptr("Москва"), Street: ptr(a.street), House: ptr(a.house)}
	if sc.indoor {
		cs.Address.Entrance = ptr(strconv.Itoa(entrance))
	}
	cs.KeyFacts = &keyFacts
	cs.Turns = turns
	cs.Dialogue = &components.DialogueBrief{
		Persona:       persona,
		SpeakingStyle: ptr(style),
		Facts:         facts,
		Unknowns:      &unknowns,
		EndConditions: &[]string{"Оператор сообщил, что помощь направлена", "Прошло 12 реплик"},
		MaxTurns:      ptr(defaultMaxTurns),
	}

	// Эталонная карточка.
	services := sc.services
	if spec.Category.Services != nil && len(*spec.Category.Services) > 0 {
		services = *spec.Category.Services
	}
	services = append([]string(nil), services...)
	addr := components.Address{Raw: ptr(raw), City: ptr("Москва"), Street: ptr(a.street), House: ptr(a.house)}
	if sc.indoor {
		addr.Entrance = ptr(strconv.Itoa(entrance))
		addr.Floor = ptr(strconv.Itoa(floor))
		addr.Apartment = ptr(strconv.Itoa(apt))
	} else if sc.landmark != "" {
		addr.Landmark = ptr(sc.landmark)
	}
	card := components.IncidentCard{
		CategoryCode:     ptr(spec.Category.Code),
		Address:          &addr,
		Applicant:        &applicantT{Name: ptr(name), Phone: ptr(phone)},
		ServicesToNotify: &services,
		Description:      ptr(desc.String()),
		ActionsTaken:     ptr(r(sc.actions)),
		Attributes:       &map[string]any{},
	}
	injured, trapped := sc.injured, sc.trapped
	if strings.Contains(norm(comment), "пострадав") && injured == 0 {
		injured = 1 // преподаватель попросил сценарий с пострадавшими
	}
	if injured > 0 || trapped > 0 {
		cas := &casualtiesT{}
		if injured > 0 {
			cas.Injured = ptr(injured)
		}
		if trapped > 0 {
			cas.Trapped = ptr(trapped)
		}
		card.Casualties = cas
	}

	res := components.ScenarioResult{
		Title:              pickTitle(sc, deref(spec.AvoidTitles), rng, r),
		CallScript:         cs,
		EtalonCard:         card,
		ExpectedDialogue:   expectedDialogue(sc),
		DifficultyEstimate: ptr(diff),
		NotesForTeacher:    ptr(teacherNotes(comment, sc)),
	}
	if spec.Mode != aiservice.GenerateJobRequestSpecModeCards {
		reqFacts := make([]string, 0, len(sc.expFacts))
		for _, f := range sc.expFacts {
			reqFacts = append(reqFacts, r(f))
		}
		forb := append([]string{}, sc.forbidden...)
		res.ExpectedActions = &[]components.ExpectedAction{{
			ActionText: r(sc.expected), RequiredFacts: &reqFacts, ForbiddenFacts: &forb,
		}}
	}
	return res
}

// yearsWord — «1 год», «3 года», «53 года», «65 лет».
func yearsWord(n int) string {
	switch d, dd := n%10, n%100; {
	case dd >= 11 && dd <= 14:
		return "лет"
	case d == 1:
		return "год"
	case d >= 2 && d <= 4:
		return "года"
	default:
		return "лет"
	}
}

// emotionFor — эмоциональное состояние по сложности; комментарий преподавателя
// («сделай панику», «спокойный заявитель») важнее.
func emotionFor(diff int, female bool, comment string) string {
	c := norm(comment)
	switch {
	case strings.Contains(c, "паник") || strings.Contains(c, "истер"):
		return "паника"
	case mentionsCalm(c):
		if female {
			return "спокойна"
		}
		return "спокоен"
	}
	switch diff {
	case 1:
		if female {
			return "встревожена"
		}
		return "встревожен"
	case 2:
		if female {
			return "взволнована"
		}
		return "взволнован"
	default:
		return "паника"
	}
}

// pickTitle — заголовок, не совпадающий с avoid_titles (сценарии категории уже есть).
func pickTitle(sc *sceneTpl, avoidTitles []string, rng *rand.Rand, r func(string) string) string {
	avoid := map[string]bool{}
	for _, t := range avoidTitles {
		avoid[norm(strings.TrimSpace(t))] = true
	}
	start := rng.IntN(len(sc.titles))
	for i := range sc.titles {
		t := r(sc.titles[(start+i)%len(sc.titles)])
		if !avoid[norm(t)] {
			return t
		}
	}
	base := r(sc.titles[start])
	for k := 2; ; k++ {
		t := base + " (вариант " + strconv.Itoa(k) + ")"
		if !avoid[norm(t)] {
			return t
		}
	}
}

// expectedDialogue — чек-лист протокола приёма вызова: адрес, суть (по шаблону),
// люди, телефон, указание (если есть), «помощь направлена», спокойный тон.
func expectedDialogue(sc *sceneTpl) *components.ExpectedDialogue {
	addrText := "Уточнил адрес: улица и дом"
	if sc.indoor {
		addrText = "Уточнил адрес: дом, подъезд, этаж"
	}
	items := []itemTpl{
		{"ask_address", addrText, components.Question, true, 2, []string{"адрес", "улиц", "дом", "подъезд", "этаж", "где"}},
		sc.what,
		{"ask_people", "Спросил о пострадавших и людях в опасности", components.Question, true, 2,
			[]string{"люди", "пострадав", "кто-нибудь", "внутри", "ранен"}},
		{"ask_phone", "Уточнил контактный телефон заявителя", components.Question, false, 1, []string{"телефон", "номер"}},
	}
	if sc.safety != nil {
		items = append(items, *sc.safety)
	}
	items = append(items,
		itemTpl{"say_dispatched", "Сообщил, что помощь направлена", components.Phrase, true, 2,
			[]string{"направлен", "выехал", "выезжа", "едут", "высыла"}},
		itemTpl{"calm", "Говорил спокойно и вежливо, не перебивал", components.Behavior, true, 1, nil},
	)
	list := make([]components.DialogueChecklistItem, 0, len(items))
	for _, it := range items {
		ci := components.DialogueChecklistItem{Id: it.id, Text: it.text, Kind: it.kind, Required: it.required, Weight: ptr(it.weight)}
		if len(it.hints) > 0 {
			ci.Hints = ptr(append([]string(nil), it.hints...))
		}
		list = append(list, ci)
	}
	return &components.ExpectedDialogue{
		Checklist:        list,
		Forbidden:        &[]string{"перезвоните позже", "ждите"},
		MaxOperatorTurns: ptr(10),
	}
}

func teacherNotes(comment string, sc *sceneTpl) string {
	var b strings.Builder
	b.WriteString("Черновик сгенерирован имитатором ai-service (fakeai) детерминированно по request_id. ")
	b.WriteString("Проверьте адрес, состав служб, факты брифа заявителя и чек-лист разговора перед подтверждением.")
	if sc.kind == "generic" {
		b.WriteString(" Для этой категории нет отдельного шаблона — легенда общая, её стоит конкретизировать.")
	}
	if comment != "" {
		b.WriteString(" Учтён комментарий преподавателя: «" + snippet(comment, 200) + "».")
	}
	return b.String()
}

// lowerFirst — «Пожар в жилом доме» → «пожар в жилом доме», но «ДТП» и «БПЛА» не трогаем.
func lowerFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError || !unicode.IsUpper(r) {
		return s
	}
	if r2, _ := utf8.DecodeRuneInString(s[n:]); unicode.IsUpper(r2) {
		return s
	}
	return string(unicode.ToLower(r)) + s[n:]
}
