package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/pg"
)

// DialogueTurnRow — реплика транскрипта (attempt_dialogue_turns). TurnNo — номер ОБМЕНА:
// 0 — вступление заявителя; k ≥ 1 — k-я реплика оператора и ответ заявителя на неё.
type DialogueTurnRow struct {
	AttemptID       uuid.UUID
	TurnNo          int
	Speaker         string // operator | caller
	Text            string
	Source          string // stt | text | script | llm
	Confidence      *float32
	AudioPath       *string // только caller: относительный путь в volume tts_cache
	DurationMs      *int
	At              time.Time
	AtMs            int
	EmotionalState  *string
	RevealedFactIDs []string
	Meta            json.RawMessage // model.TurnMeta как в БД
}

// Реплики обмена: оператор раньше заявителя (в PK speaker идёт по алфавиту — caller < operator,
// поэтому явная сортировка; строк десятки).
const sqlListDialogueTurns = `
SELECT attempt_id, turn_no, speaker, text, source, confidence, audio_path, duration_ms,
       at, at_ms, emotional_state, revealed_fact_ids, meta
  FROM attempt_dialogue_turns
 WHERE attempt_id = $1
 ORDER BY turn_no, speaker = 'caller'`

// ListDialogueTurns — транскрипт попытки (не nil).
func ListDialogueTurns(ctx context.Context, q pg.Querier, attemptID uuid.UUID) ([]DialogueTurnRow, error) {
	rows, err := q.Query(ctx, sqlListDialogueTurns, attemptID)
	if err != nil {
		return nil, fmt.Errorf("store: list dialogue turns: %w", err)
	}
	defer rows.Close()
	out := make([]DialogueTurnRow, 0, 16)
	for rows.Next() {
		out = append(out, DialogueTurnRow{})
		t := &out[len(out)-1]
		if err := rows.Scan(&t.AttemptID, &t.TurnNo, &t.Speaker, &t.Text, &t.Source, &t.Confidence,
			&t.AudioPath, &t.DurationMs, &t.At, &t.AtMs, &t.EmotionalState, &t.RevealedFactIDs,
			&rawScan{dst: &t.Meta}); err != nil {
			return nil, fmt.Errorf("store: scan dialogue turn: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list dialogue turns: %w", err)
	}
	return out, nil
}

// DialogueTurnToView — реплика для фронта: аудио заявителя — URL /api/v1/media/tts/…,
// confidence — только у оператора, emotionalState — только у заявителя.
func DialogueTurnToView(r *DialogueTurnRow) public.DialogueTurnView {
	at := r.At.UTC()
	out := public.DialogueTurnView{
		TurnNo:     r.TurnNo,
		Speaker:    public.DialogueTurnViewSpeaker(r.Speaker),
		Text:       r.Text,
		AtMs:       r.AtMs,
		At:         &at,
		DurationMs: r.DurationMs,
		Source:     public.DialogueTurnViewSource(r.Source),
	}
	switch r.Speaker {
	case "operator":
		out.Confidence = r.Confidence
	case "caller":
		out.EmotionalState = convert.NonEmptyPtr(r.EmotionalState)
		if u := convert.MediaURL(convert.StrOr(r.AudioPath, "")); u != "" {
			out.Audio = &public.AudioRef{
				AudioUrl:   u,
				DurationMs: convert.Deref(r.DurationMs),
				Mime:       convert.NonEmpty(convert.AudioMime(*r.AudioPath)),
			}
		}
	}
	return out
}

// DialogueTurnsToView — транскрипт для DialogueState (не nil).
func DialogueTurnsToView(rows []DialogueTurnRow) []public.DialogueTurnView {
	out := make([]public.DialogueTurnView, len(rows))
	for i := range rows {
		out[i] = DialogueTurnToView(&rows[i])
	}
	return out
}

// DialogueTurnToContract — реплика для ai-service: сквозной turn_no (convert.ExchangeToSeq),
// revealed_fact_ids — только у заявителя.
func DialogueTurnToContract(r *DialogueTurnRow) components.DialogueTurn {
	src := components.DialogueTurnSource(r.Source)
	atMs := r.AtMs
	out := components.DialogueTurn{
		TurnNo:          convert.ExchangeToSeq(r.TurnNo, r.Speaker),
		Speaker:         components.DialogueTurnSpeaker(r.Speaker),
		Text:            r.Text,
		Source:          &src,
		AtMs:            &atMs,
		AudioDurationMs: r.DurationMs,
	}
	if r.Speaker == "operator" {
		out.Confidence = r.Confidence
	} else {
		out.RevealedFactIds = convert.SlicePtr(r.RevealedFactIDs)
	}
	return out
}

// DialogueTurnsToContract — транскрипт/история для ai-service (не nil).
func DialogueTurnsToContract(rows []DialogueTurnRow) []components.DialogueTurn {
	out := make([]components.DialogueTurn, len(rows))
	for i := range rows {
		out[i] = DialogueTurnToContract(&rows[i])
	}
	return out
}
