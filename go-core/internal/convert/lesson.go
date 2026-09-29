package convert

import (
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/settings"
)

// VoiceToPublic — голосовой режим занятия; все опциональные поля заданы явно, чтобы
// фронт не додумывал дефолты.
func VoiceToPublic(v settings.Voice) public.VoiceSettings {
	input := v.Input
	switch input {
	case model.VoiceInputVoice, model.VoiceInputText, model.VoiceInputBoth:
	default:
		input = model.VoiceInputVoice
	}
	maxTurns := v.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 12
	}
	ptt, tts := v.PushToTalk, v.TTSEnabled
	return public.VoiceSettings{
		Enabled:    v.Enabled,
		Input:      public.VoiceSettingsInput(input),
		PushToTalk: &ptt,
		MaxTurns:   &maxTurns,
		TtsEnabled: &tts,
	}
}

// VoiceFromPublic — голос из запроса поверх def (не присланные опциональные поля — из def).
func VoiceFromPublic(p *public.VoiceSettings, def settings.Voice) settings.Voice {
	if p == nil {
		return def
	}
	out := def
	out.Enabled = p.Enabled
	if p.Input != "" {
		out.Input = string(p.Input)
	}
	if p.PushToTalk != nil {
		out.PushToTalk = *p.PushToTalk
	}
	if p.MaxTurns != nil {
		out.MaxTurns = *p.MaxTurns
	}
	if p.TtsEnabled != nil {
		out.TTSEnabled = *p.TtsEnabled
	}
	return out
}

// LessonSettingsToPublic — lessons.settings -> LessonSettings (camelCase, веса со слоем dialogue).
func LessonSettingsToPublic(s model.LessonSettings) public.LessonSettings {
	out := public.LessonSettings{
		PassThreshold:   float32(s.PassThreshold),
		CardsPerStudent: s.CardsPerStudent,
		AllowReplay:     s.AllowReplay,
		Voice:           VoiceToPublic(s.Voice),
	}
	cs := public.CardSource(s.CardSource)
	if !cs.Valid() {
		cs = public.CardSourceMixed
	}
	out.CardSource = &cs
	out.Weights.Fields = float32(s.Weights.Fields)
	out.Weights.Semantic = float32(s.Weights.Semantic)
	out.Weights.Grammar = float32(s.Weights.Grammar)
	out.Weights.Timing = float32(s.Weights.Timing)
	out.Weights.Dialogue = float32(s.Weights.Dialogue)
	if out.CardsPerStudent < 1 {
		out.CardsPerStudent = 1
	}
	return out
}

// Perspective — ракурс АРМ занятия как enum контракта (operator112 по умолчанию).
func Perspective(s model.LessonSettings) public.ArmPerspective {
	if s.Perspective == model.PerspectiveDDS {
		return public.Dds
	}
	return public.Operator112
}
