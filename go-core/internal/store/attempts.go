package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/pg"
)

// AttemptRow — попытка + то, что нужно от занятия (заголовок, статус, владелец, настройки).
type AttemptRow struct {
	ID             uuid.UUID
	LessonID       uuid.UUID
	UserID         uuid.UUID
	ScenarioID     uuid.UUID
	EtalonID       uuid.UUID
	Mode           string // cards | card_actions
	SeqNo          int
	Status         string
	TimeLimitSec   int
	IssuedAt       time.Time
	CallAcceptedAt *time.Time
	FirstInputAt   *time.Time
	SubmittedAt    *time.Time
	TimeSpentMs    *int
	ReplayCount    int
	Card           json.RawMessage // attempts.card — IncidentCardDraft (camelCase) как прислал фронт; nil до submit
	ActionText     *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	IncidentNo     int64
	CallEndedAt    *time.Time
	CallEndReason  *string
	DialogueTurns  int

	LessonTitle     string
	LessonStatus    string
	LessonKind      string
	LessonTeacherID *uuid.UUID
	LessonCreatedBy uuid.UUID
	LessonSettings  model.LessonSettings
}

// LessonOwnerID — владелец занятия попытки (teacher_id; у практики — автор).
func (a *AttemptRow) LessonOwnerID() uuid.UUID {
	if a.LessonTeacherID != nil {
		return *a.LessonTeacherID
	}
	return a.LessonCreatedBy
}

// VoiceEnabled — голосовой режим занятия включён.
func (a *AttemptRow) VoiceEnabled() bool { return a.LessonSettings.Voice.Enabled }

// CallEnded — разговор завершён.
func (a *AttemptRow) CallEnded() bool { return a.CallEndedAt != nil }

// DecodeCard — attempts.card как IncidentCardDraft (nil — карточки нет или она битая).
func (a *AttemptRow) DecodeCard() *public.IncidentCardDraft {
	if isEmptyJSON(a.Card) {
		return nil
	}
	d := new(public.IncidentCardDraft)
	if json.Unmarshal(a.Card, d) != nil {
		return nil
	}
	convert.NormalizeDraft(d)
	return d
}

// AttemptFilter — хотя бы один фильтр обязателен (полный скан attempts не нужен никому).
type AttemptFilter struct {
	LessonID *uuid.UUID
	UserID   *uuid.UUID
}

var ErrAttemptFilter = errors.New("store: ListAttempts требует LessonID и/или UserID")

const attemptColumns = `
SELECT a.id, a.lesson_id, a.user_id, a.scenario_id, a.etalon_id, a.mode, a.seq_no, a.status,
       a.time_limit_sec, a.issued_at, a.call_accepted_at, a.first_input_at, a.submitted_at,
       a.time_spent_ms, a.replay_count, a.card, a.action_text, a.created_at, a.updated_at,
       a.incident_no, a.call_ended_at, a.call_end_reason, a.dialogue_turns,
       l.title, l.status, l.kind, l.teacher_id, l.created_by, l.settings
  FROM attempts a
  JOIN lessons l ON l.id = a.lesson_id`

const (
	sqlGetAttempt  = attemptColumns + ` WHERE a.id = $1`
	sqlLockAttempt = attemptColumns + ` WHERE a.id = $1 FOR NO KEY UPDATE OF a`

	// Отдельный текст на каждое сочетание фильтров: у каждого свой индекс
	// (attempts_lesson_idx / attempts_user_idx / уникальный (lesson_id, user_id, seq_no)),
	// generic-план prepared statement не деградирует из-за "$1 IS NULL OR ...".
	sqlAttemptsByLesson     = attemptColumns + ` WHERE a.lesson_id = $1 ORDER BY a.user_id, a.seq_no`
	sqlAttemptsByUser       = attemptColumns + ` WHERE a.user_id = $1 ORDER BY a.lesson_id, a.seq_no`
	sqlAttemptsByLessonUser = attemptColumns + ` WHERE a.lesson_id = $1 AND a.user_id = $2 ORDER BY a.seq_no`
)

// GetAttempt — попытка по id (pgx.ErrNoRows, если нет).
func GetAttempt(ctx context.Context, q pg.Querier, id uuid.UUID) (*AttemptRow, error) {
	r := new(AttemptRow)
	if err := scanAttempt(q.QueryRow(ctx, sqlGetAttempt, id), r); err != nil {
		return nil, err
	}
	return r, nil
}

// LockAttempt — попытка под FOR UPDATE (блокируется только строка attempts).
func LockAttempt(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*AttemptRow, error) {
	r := new(AttemptRow)
	if err := scanAttempt(tx.QueryRow(ctx, sqlLockAttempt, id), r); err != nil {
		return nil, err
	}
	return r, nil
}

// ListAttempts — попытки занятия и/или пользователя, по занятию/пользователю/seq_no.
func ListAttempts(ctx context.Context, q pg.Querier, f AttemptFilter) ([]AttemptRow, error) {
	var (
		rows pgx.Rows
		err  error
	)
	switch {
	case f.LessonID != nil && f.UserID != nil:
		rows, err = q.Query(ctx, sqlAttemptsByLessonUser, *f.LessonID, *f.UserID)
	case f.LessonID != nil:
		rows, err = q.Query(ctx, sqlAttemptsByLesson, *f.LessonID)
	case f.UserID != nil:
		rows, err = q.Query(ctx, sqlAttemptsByUser, *f.UserID)
	default:
		return nil, ErrAttemptFilter
	}
	if err != nil {
		return nil, fmt.Errorf("store: list attempts: %w", err)
	}
	defer rows.Close()
	out := make([]AttemptRow, 0, 32)
	for rows.Next() {
		out = append(out, AttemptRow{})
		if err := scanAttempt(rows, &out[len(out)-1]); err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list attempts: %w", err)
	}
	return out, nil
}

func scanAttempt(row pgx.Row, r *AttemptRow) error {
	return row.Scan(
		&r.ID, &r.LessonID, &r.UserID, &r.ScenarioID, &r.EtalonID, &r.Mode, &r.SeqNo, &r.Status,
		&r.TimeLimitSec, &r.IssuedAt, &r.CallAcceptedAt, &r.FirstInputAt, &r.SubmittedAt,
		&r.TimeSpentMs, &r.ReplayCount, &rawScan{dst: &r.Card}, &r.ActionText, &r.CreatedAt, &r.UpdatedAt,
		&r.IncidentNo, &r.CallEndedAt, &r.CallEndReason, &r.DialogueTurns,
		&r.LessonTitle, &r.LessonStatus, &r.LessonKind, &r.LessonTeacherID, &r.LessonCreatedBy,
		&settingsScan{dst: &r.LessonSettings},
	)
}

// ---------------------------------------------------------------- public

// AttemptToPublic — попытка для API. serverNow = now (фронт синхронизирует таймер);
// perspective и voice — из настроек занятия; card — если есть; краткое состояние
// разговора — только в голосовом режиме.
func AttemptToPublic(r *AttemptRow, now time.Time) public.Attempt {
	out := public.Attempt{
		Id:             r.ID,
		LessonId:       r.LessonID,
		LessonTitle:    r.LessonTitle,
		UserId:         r.UserID,
		ScenarioId:     r.ScenarioID,
		Mode:           public.LessonMode(r.Mode),
		Perspective:    convert.Perspective(r.LessonSettings),
		SeqNo:          r.SeqNo,
		Status:         public.AttemptStatus(r.Status),
		TimeLimitSec:   r.TimeLimitSec,
		IssuedAt:       r.IssuedAt.UTC(),
		CallAcceptedAt: convert.UTC(r.CallAcceptedAt),
		FirstInputAt:   convert.UTC(r.FirstInputAt),
		SubmittedAt:    convert.UTC(r.SubmittedAt),
		TimeSpentMs:    r.TimeSpentMs,
		ReplayCount:    r.ReplayCount,
		Card:           r.DecodeCard(),
		IncidentNo:     strconv.FormatInt(r.IncidentNo, 10),
		ServerNow:      now.UTC(),
		Voice:          convert.VoiceToPublic(r.LessonSettings.Voice),
	}
	if r.LessonSettings.Voice.Enabled {
		turns, ended := r.DialogueTurns, r.CallEndedAt != nil
		out.Dialogue = &struct {
			CallEnded   *bool      `json:"callEnded,omitempty"`
			CallEndedAt *time.Time `json:"callEndedAt,omitempty"`
			TurnsCount  *int       `json:"turnsCount,omitempty"`
		}{CallEnded: &ended, CallEndedAt: convert.UTC(r.CallEndedAt), TurnsCount: &turns}
	}
	return out
}

// AttemptsToPublic — список (всегда не nil).
func AttemptsToPublic(rows []AttemptRow, now time.Time) []public.Attempt {
	out := make([]public.Attempt, len(rows))
	for i := range rows {
		out[i] = AttemptToPublic(&rows[i], now)
	}
	return out
}
