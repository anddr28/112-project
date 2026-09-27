package dialogue

import (
	"context"
	"strings"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/model"
	"lct/gocore/internal/settings"
	"lct/gocore/internal/store"
)

// panicLines — запасные реплики, когда в легенде нет реплик заявителя кроме вступления.
// Нейтральны к категории происшествия и ничего не раскрывают.
var panicLines = []string{
	"Алло! Вы меня слышите? Помогите, пожалуйста!",
	"Пожалуйста, побыстрее, мне очень страшно!",
	"Я не знаю, что делать! Что мне сейчас делать?",
	"Алло? Вы ещё на связи? Приезжайте скорее!",
}

// localReply — ответ заявителя без ai-service (DESIGN §5: fallback=true, source=script):
// сценарные реплики заявителя после вступления по кругу по номеру хода, иначе запасные
// фразы. Факты не раскрываются, разговор не завершается. Озвучка — из tts_cache, если
// реплика уже озвучена (approve предгенерирует реплики сценария).
func (s *Service) localReply(ctx context.Context, a *attemptCtx, turnNo int, snap *settings.Snapshot) callerPart {
	cs := a.Script
	openIdx, _, _ := cs.OpeningTurn()
	var pool []model.Turn
	for i := range cs.Turns {
		t := &cs.Turns[i]
		if i != openIdx && t.Speaker == model.SpeakerCaller && strings.TrimSpace(t.Text) != "" {
			pool = append(pool, *t)
		}
	}
	idx := max(turnNo-1, 0)
	var text, hash string
	if len(pool) > 0 {
		t := pool[idx%len(pool)]
		text, hash = strings.TrimSpace(t.Text), t.TTSHash
	} else {
		text = panicLines[idx%len(panicLines)]
	}

	c := callerPart{
		text:      text,
		source:    core.TurnSourceScript,
		fallback:  true,
		emotional: convert.NonEmpty(cs.Caller.EmotionalState),
		revealed:  []string{},
	}
	if a.Settings.Voice.TTSEnabled {
		if hash == "" {
			hash = convert.TTSHash(text, cs.VoiceOr(snap.TTS.Voice), snap.TTS.Rate)
		}
		files, err := store.GetTTSFiles(ctx, s.pool, []string{hash})
		if err != nil {
			s.log.Warn("fallback reply: tts lookup failed", "attempt", a.ID, "err", err)
		} else if f, ok := files[hash]; ok {
			if p, ok := cleanMediaPath(f.FilePath); ok {
				c.audioPath = &p
				c.durationMs = f.DurationMs
			}
		}
	}
	if c.durationMs == nil || *c.durationMs <= 0 {
		d := speechDurationMs(text)
		c.durationMs = &d
	}
	return c
}
