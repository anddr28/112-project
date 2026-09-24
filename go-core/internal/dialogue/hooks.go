package dialogue

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/store"
)

// openingFallback — вступление, если в легенде нет ни одной реплики заявителя.
const openingFallback = "Алло! Алло, помогите!"

// OnCallAccepted — вступительная реплика заявителя (turn 0) в голосовом режиме; nil —
// голос выключен. Вызывается в транзакции accept-call ПОСЛЕ того, как попытка
// заблокирована и call_accepted_at записан. Идемпотентно: повторный accept вернёт ту же
// реплику (и допишет озвучку, если она доехала после первого раза).
func (s *Service) OnCallAccepted(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID) (*public.DialogueTurnView, error) {
	var (
		lessonID, scenarioID uuid.UUID
		acceptedAt           *time.Time
		ls                   model.LessonSettings
		cs                   model.CallScript
		t0Text, t0Source     *string
		t0Audio, t0Emotional *string
		t0Dur                *int
		t0At                 *time.Time
		t0AtMs               *int
	)
	err := tx.QueryRow(ctx, sqlOpeningContext, attemptID).Scan(&lessonID, &scenarioID, &acceptedAt,
		&settingsInto{dst: &ls}, &jsonInto{dst: &cs, name: "scenarios.call_script"},
		&t0Text, &t0Source, &t0Audio, &t0Dur, &t0At, &t0AtMs, &t0Emotional)
	if err != nil {
		return nil, fmt.Errorf("dialogue: opening context: %w", err)
	}
	if !ls.Voice.Enabled {
		return nil, nil
	}
	cs.Normalize()
	snap := s.snapshot(ctx)

	// Реплика уже есть — повторный accept-call.
	if t0Text != nil {
		row := store.DialogueTurnRow{
			AttemptID: attemptID, TurnNo: 0, Speaker: core.SpeakerCaller, Text: *t0Text,
			Source: convert.StrOr(t0Source, core.TurnSourceScript), AudioPath: t0Audio, DurationMs: t0Dur,
			AtMs: convert.Deref(t0AtMs), EmotionalState: t0Emotional,
		}
		if t0At != nil {
			row.At = t0At.UTC()
		}
		if row.AudioPath == nil && ls.Voice.TTSEnabled {
			// Озвучка вступления могла доехать после первого accept — дописываем.
			hash, _, _ := openingHash(&cs, snap.TTS.Voice, snap.TTS.Rate)
			if f, ok, err := s.ttsFile(ctx, tx, hash); err != nil {
				return nil, err
			} else if ok {
				if _, err := tx.Exec(ctx, sqlPatchOpeningAudio, attemptID, f.path, f.durationMs); err != nil {
					return nil, fmt.Errorf("dialogue: patch opening audio: %w", err)
				}
				row.AudioPath = &f.path
				if f.durationMs != nil {
					row.DurationMs = f.durationMs
				}
			}
		}
		v := store.DialogueTurnToView(&row)
		return &v, nil
	}

	at := time.Now()
	if acceptedAt != nil {
		at = *acceptedAt
	}
	at = eventTime(at)
	hash, text, voice := openingHash(&cs, snap.TTS.Voice, snap.TTS.Rate)
	row := store.DialogueTurnRow{
		AttemptID: attemptID, TurnNo: 0, Speaker: core.SpeakerCaller, Text: cleanText(text), Source: core.TurnSourceScript,
		At: at, AtMs: 0, EmotionalState: convert.NonEmpty(cs.Caller.EmotionalState),
	}
	if ls.Voice.TTSEnabled {
		f, ok, err := s.ttsFile(ctx, tx, hash)
		if err != nil {
			return nil, err
		}
		if ok {
			row.AudioPath, row.DurationMs = &f.path, f.durationMs
		} else if err := s.enqueueTTS(ctx, tx, scenarioID, hash, text, voice, snap.TTS.Rate); err != nil {
			// Ошибка SQL уже сломала транзакцию accept-call — продолжать нельзя.
			return nil, fmt.Errorf("dialogue: opening tts: %w", err)
		}
	}
	if row.DurationMs == nil || *row.DurationMs <= 0 {
		d := speechDurationMs(row.Text)
		row.DurationMs = &d
	}
	if _, err := tx.Exec(ctx, sqlInsertOpening, attemptID, row.Text, row.AudioPath, row.DurationMs, row.At, row.EmotionalState); err != nil {
		return nil, fmt.Errorf("dialogue: insert opening: %w", err)
	}
	v := store.DialogueTurnToView(&row)
	pub := v
	pg.OnCommit(ctx, func() {
		s.publishMonitor(lessonID, public.MonitorMessage{Type: public.MonitorMessageTypeDialogueTurn, AttemptId: &attemptID, Turn: &pub})
	})
	return &v, nil
}

// CloseOnSubmit — закрыть разговор при сдаче (reason=submitted), если занятие голосовое и
// разговор ещё открыт; событие dialogue_ended. Один запрос в транзакции submit.
func (s *Service) CloseOnSubmit(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, at time.Time) error {
	at = eventTime(at)
	var (
		eventID  int64
		lessonID uuid.UUID
	)
	err := tx.QueryRow(ctx, sqlCloseOnSubmit, attemptID, at).Scan(&eventID, &lessonID)
	if pg.IsNoRows(err) {
		return nil // голос выключен или разговор уже завершён
	}
	if err != nil {
		return fmt.Errorf("dialogue: close on submit: %w", err)
	}
	payload := map[string]any{"reason": core.EndSubmitted}
	ev := public.AttemptEvent{Id: int(eventID), Type: public.AttemptEventType(core.EventDialogueEnded), Payload: &payload, At: at}
	pg.OnCommit(ctx, func() { s.publishEvents(lessonID, attemptID, []public.AttemptEvent{ev}) })
	return nil
}

// openingHash — вступление (первая непустая реплика заявителя легенды, иначе запасная
// фраза) как есть в легенде, его голос и ключ tts_cache (tts_hash из approve, иначе
// вычисленный канонически по тому же тексту — им же озвучивается задача TTS).
func openingHash(cs *model.CallScript, defVoice string, rate float64) (hash, text, voice string) {
	voice = cs.VoiceOr(defVoice)
	_, t, ok := cs.OpeningTurn()
	if !ok {
		return convert.TTSHash(openingFallback, voice, rate), openingFallback, voice
	}
	hash = strings.TrimSpace(t.TTSHash)
	if hash == "" {
		hash = convert.TTSHash(t.Text, voice, rate)
	}
	return hash, t.Text, voice
}

type ttsHit struct {
	path       string
	durationMs *int
}

// ttsFile — готовый файл озвучки по ключу (ok=false — ещё не озвучено).
func (s *Service) ttsFile(ctx context.Context, q pg.Querier, hash string) (ttsHit, bool, error) {
	files, err := store.GetTTSFiles(ctx, q, []string{hash})
	if err != nil {
		return ttsHit{}, false, err
	}
	f, ok := files[hash]
	if !ok {
		return ttsHit{}, false, nil
	}
	p, ok := cleanMediaPath(f.FilePath)
	if !ok {
		return ttsHit{}, false, nil
	}
	d := f.DurationMs
	if d != nil && *d <= 0 {
		d = nil // длительность неизвестна
	}
	return ttsHit{path: p, durationMs: d}, true, nil
}

// enqueueTTS — озвучка, нужная прямо на занятии (приоритет выше предгенерации approve);
// dedup по хэшу общий со сценариями: уже стоящая задача не дублируется.
func (s *Service) enqueueTTS(ctx context.Context, tx pgx.Tx, scenarioID uuid.UUID, hash, text, voice string, rate float64) error {
	if s.queue == nil {
		return nil
	}
	jobID := ids.New()
	prio := core.PriorityTTSLive
	r := float32(rate)
	v := voice
	_, _, err := s.queue.Enqueue(ctx, tx, core.NewJob{
		ID:       jobID,
		Type:     core.JobTTS,
		Priority: prio,
		DedupKey: "tts:" + hash,
		RefType:  core.RefScenario,
		RefID:    scenarioID,
		Payload: aiservice.TtsJobRequest{
			SchemaVersion: components.N1,
			RequestId:     jobID,
			Priority:      &prio,
			Text:          text,
			TextHash:      hash,
			Voice:         &v,
			Rate:          &r,
		},
	})
	return err
}
