package convert

import (
	"strings"

	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
)

// contractTurn — элемент components.CallScript.Turns (анонимный тип генератора).
// Алиас обязан совпадать поле в поле с тегами: иначе не скомпилируется — это и есть проверка.
type contractTurn = struct {
	Speaker components.CallScriptTurnsSpeaker `json:"speaker"`
	Text    string                            `json:"text"`
	TtsHash *string                           `json:"tts_hash,omitempty"`
}

// ---------------------------------------------------------------- CallScript

// CallScriptToPublic — легенда для преподавателя (camelCase, с брифом и keyFacts).
func CallScriptToPublic(c *model.CallScript) public.CallScript {
	var out public.CallScript
	if c == nil {
		out.KeyFacts = []string{}
		out.Turns = []public.CallTurn{}
		return out
	}
	out.Caller.Name = NonEmpty(c.Caller.Name)
	out.Caller.Phone = NonEmpty(c.Caller.Phone)
	out.Caller.Role = NonEmpty(c.Caller.Role)
	out.Caller.EmotionalState = NonEmpty(c.Caller.EmotionalState)
	out.Caller.Voice = NonEmpty(c.Caller.Voice)
	out.Address = public.Address(c.Address)
	out.KeyFacts = Slice(c.KeyFacts)
	if c.Dialogue != nil {
		b := DialogueBriefToPublic(c.Dialogue)
		out.Dialogue = &b
	}
	out.Turns = make([]public.CallTurn, len(c.Turns))
	for i := range c.Turns {
		t := &c.Turns[i]
		out.Turns[i] = public.CallTurn{
			Speaker: public.CallTurnSpeaker(t.Speaker),
			Text:    t.Text,
			TtsHash: NonEmpty(t.TTSHash),
		}
	}
	return out
}

// CallScriptFromPublic — легенда из правки преподавателя. Строки заявителя обрезаются;
// tts_hash переносится как прислан — актуальность хэша (текст/голос могли измениться)
// проверяет scenarios при approve.
func CallScriptFromPublic(p *public.CallScript) model.CallScript {
	var out model.CallScript
	if p == nil {
		out.Normalize()
		return out
	}
	out.Caller = model.Caller{
		Name:           Str(p.Caller.Name),
		Phone:          Str(p.Caller.Phone),
		Role:           Str(p.Caller.Role),
		EmotionalState: Str(p.Caller.EmotionalState),
		Voice:          Str(p.Caller.Voice),
	}
	out.Address = compactAddress(components.Address(p.Address))
	out.KeyFacts = nonEmptyStrings(p.KeyFacts)
	if p.Dialogue != nil {
		b := DialogueBriefFromPublic(p.Dialogue)
		out.Dialogue = &b
	}
	out.Turns = make([]model.Turn, 0, len(p.Turns))
	for i := range p.Turns {
		t := &p.Turns[i]
		out.Turns = append(out.Turns, model.Turn{
			Speaker: normSpeaker(string(t.Speaker)),
			Text:    t.Text,
			TTSHash: Str(t.TtsHash),
		})
	}
	return out
}

// CallScriptToContract — легенда для ai-service (snake_case): голос заявителя не
// передаётся (им управляет go-core через options.tts), tts_hash сохраняется.
// key_facts пуст, а бриф есть — отдаём проекцию facts[].text без reveal=never
// (voice-mode §5: keyFacts — плоская проекция фактов, которые заявитель может сообщить).
func CallScriptToContract(c *model.CallScript) components.CallScript {
	var out components.CallScript
	if c == nil {
		out.Turns = []contractTurn{}
		return out
	}
	out.Caller.Name = NonEmpty(c.Caller.Name)
	out.Caller.Phone = NonEmpty(c.Caller.Phone)
	out.Caller.Role = NonEmpty(c.Caller.Role)
	out.Caller.EmotionalState = NonEmpty(c.Caller.EmotionalState)
	out.Address = c.Address
	keyFacts := c.KeyFacts
	if len(keyFacts) == 0 && c.Dialogue != nil && len(c.Dialogue.Facts) > 0 {
		keyFacts = make([]string, 0, len(c.Dialogue.Facts))
		for i := range c.Dialogue.Facts {
			f := &c.Dialogue.Facts[i]
			if f.Reveal != model.RevealNever && strings.TrimSpace(f.Text) != "" {
				keyFacts = append(keyFacts, f.Text)
			}
		}
	}
	out.KeyFacts = SlicePtr(keyFacts)
	if c.Dialogue != nil {
		b := DialogueBriefToContract(c.Dialogue)
		out.Dialogue = &b
	}
	out.Turns = make([]contractTurn, len(c.Turns))
	for i := range c.Turns {
		t := &c.Turns[i]
		out.Turns[i] = contractTurn{
			Speaker: components.CallScriptTurnsSpeaker(t.Speaker),
			Text:    t.Text,
			TtsHash: NonEmpty(t.TTSHash),
		}
	}
	return out
}

// CallScriptFromContract — легенда из результата генерации (ScenarioResult.call_script).
func CallScriptFromContract(c *components.CallScript) model.CallScript {
	var out model.CallScript
	if c == nil {
		out.Normalize()
		return out
	}
	out.Caller = model.Caller{
		Name:           Str(c.Caller.Name),
		Phone:          Str(c.Caller.Phone),
		Role:           Str(c.Caller.Role),
		EmotionalState: Str(c.Caller.EmotionalState),
	}
	out.Address = compactAddress(c.Address)
	if c.KeyFacts != nil {
		out.KeyFacts = nonEmptyStrings(*c.KeyFacts)
	}
	if c.Dialogue != nil {
		b := DialogueBriefFromContract(c.Dialogue)
		out.Dialogue = &b
	}
	out.Turns = make([]model.Turn, 0, len(c.Turns))
	for i := range c.Turns {
		t := &c.Turns[i]
		if strings.TrimSpace(t.Text) == "" {
			continue // LLM иногда отдаёт пустые реплики — в легенде им не место
		}
		out.Turns = append(out.Turns, model.Turn{
			Speaker: normSpeaker(string(t.Speaker)),
			Text:    t.Text,
			TTSHash: Str(t.TtsHash),
		})
	}
	return out
}

// ---------------------------------------------------------------- DialogueBrief

func DialogueBriefToPublic(b *model.DialogueBrief) public.DialogueBrief {
	if b == nil {
		return public.DialogueBrief{Facts: []public.DialogueFact{}}
	}
	out := public.DialogueBrief{
		Persona:       b.Persona,
		SpeakingStyle: NonEmpty(b.SpeakingStyle),
		Unknowns:      SlicePtr(b.Unknowns),
		EndConditions: SlicePtr(b.EndConditions),
		MaxTurns:      IntPtrIf(b.MaxTurns),
		Facts:         make([]public.DialogueFact, len(b.Facts)),
	}
	for i := range b.Facts {
		f := &b.Facts[i]
		out.Facts[i] = public.DialogueFact{
			Id:     f.ID,
			Text:   f.Text,
			Reveal: public.DialogueFactReveal(f.Reveal),
			Hints:  SlicePtr(f.Hints),
		}
	}
	return out
}

func DialogueBriefFromPublic(p *public.DialogueBrief) model.DialogueBrief {
	if p == nil {
		return model.DialogueBrief{Facts: []model.DialogueFact{}}
	}
	out := model.DialogueBrief{
		Persona:       strings.TrimSpace(p.Persona),
		SpeakingStyle: Str(p.SpeakingStyle),
		Unknowns:      nonEmptyStrings(SliceFromPtr(p.Unknowns)),
		EndConditions: nonEmptyStrings(SliceFromPtr(p.EndConditions)),
		MaxTurns:      Deref(p.MaxTurns),
		Facts:         make([]model.DialogueFact, 0, len(p.Facts)),
	}
	for i := range p.Facts {
		f := &p.Facts[i]
		out.Facts = append(out.Facts, model.DialogueFact{
			ID:     strings.TrimSpace(f.Id),
			Text:   f.Text,
			Reveal: normReveal(string(f.Reveal)),
			Hints:  nonEmptyStrings(SliceFromPtr(f.Hints)),
		})
	}
	return out
}

func DialogueBriefToContract(b *model.DialogueBrief) components.DialogueBrief {
	if b == nil {
		return components.DialogueBrief{Facts: []components.DialogueFact{}}
	}
	out := components.DialogueBrief{
		Persona:       b.Persona,
		SpeakingStyle: NonEmpty(b.SpeakingStyle),
		Unknowns:      SlicePtr(b.Unknowns),
		EndConditions: SlicePtr(b.EndConditions),
		MaxTurns:      IntPtrIf(b.MaxTurns),
		Facts:         make([]components.DialogueFact, len(b.Facts)),
	}
	for i := range b.Facts {
		f := &b.Facts[i]
		out.Facts[i] = components.DialogueFact{
			Id:     f.ID,
			Text:   f.Text,
			Reveal: components.DialogueFactReveal(f.Reveal),
			Hints:  SlicePtr(f.Hints),
		}
	}
	return out
}

func DialogueBriefFromContract(c *components.DialogueBrief) model.DialogueBrief {
	if c == nil {
		return model.DialogueBrief{Facts: []model.DialogueFact{}}
	}
	out := model.DialogueBrief{
		Persona:       strings.TrimSpace(c.Persona),
		SpeakingStyle: Str(c.SpeakingStyle),
		Unknowns:      nonEmptyStrings(SliceFromPtr(c.Unknowns)),
		EndConditions: nonEmptyStrings(SliceFromPtr(c.EndConditions)),
		MaxTurns:      Deref(c.MaxTurns),
		Facts:         make([]model.DialogueFact, 0, len(c.Facts)),
	}
	for i := range c.Facts {
		f := &c.Facts[i]
		out.Facts = append(out.Facts, model.DialogueFact{
			ID:     strings.TrimSpace(f.Id),
			Text:   f.Text,
			Reveal: normReveal(string(f.Reveal)),
			Hints:  nonEmptyStrings(SliceFromPtr(f.Hints)),
		})
	}
	return out
}

// ---------------------------------------------------------------- мелочи

// compactAddress — пустые строки адреса -> nil (компактный jsonb, «не задано» однозначно).
func compactAddress(a components.Address) components.Address {
	return components.Address{
		Raw:       NonEmptyPtr(a.Raw),
		City:      NonEmptyPtr(a.City),
		Street:    NonEmptyPtr(a.Street),
		House:     NonEmptyPtr(a.House),
		Entrance:  NonEmptyPtr(a.Entrance),
		Floor:     NonEmptyPtr(a.Floor),
		Apartment: NonEmptyPtr(a.Apartment),
		Landmark:  NonEmptyPtr(a.Landmark),
	}
}

// nonEmptyStrings — без пустых (после trim) элементов; nil для пустого результата.
func nonEmptyStrings(s []string) []string {
	n := 0
	for _, v := range s {
		if strings.TrimSpace(v) != "" {
			n++
		}
	}
	if n == 0 {
		return nil
	}
	out := make([]string, 0, n)
	for _, v := range s {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// normSpeaker — неизвестный говорящий в легенде считается заявителем (реплики-подсказки
// оператору помечены явно).
func normSpeaker(s string) string {
	if s == model.SpeakerOperatorHint {
		return model.SpeakerOperatorHint
	}
	return model.SpeakerCaller
}

// normReveal — неизвестное значение -> on_request (безопасно: факт не выдаётся сам).
func normReveal(s string) string {
	switch s {
	case model.RevealVolunteer, model.RevealNever:
		return s
	}
	return model.RevealOnRequest
}
