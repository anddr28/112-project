package dialogue

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/store"
)

// SQL — константные тексты (pgx кэширует prepared statements по тексту), колонки явно.
// Горячий путь хода: контекст попытки и транскрипт уходят одним pgx.Batch (один round-trip),
// запись хода — транзакция «блокировка + один батч записей».

// Контекст попытки для хода: попытка + занятие (владелец, настройки) + легенда целиком.
const sqlTurnContext = `
SELECT a.user_id, a.lesson_id, a.scenario_id, a.status, a.call_accepted_at, a.call_ended_at, a.call_end_reason,
       l.teacher_id, l.created_by, l.settings, s.call_script
  FROM attempts a
  JOIN lessons l ON l.id = a.lesson_id
  JOIN scenarios s ON s.id = a.scenario_id
 WHERE a.id = $1`

// Контекст для состояния разговора: из легенды нужен только лимит реплик брифа —
// jsonb не тащим и не разбираем.
const stateContextColumns = `
SELECT a.user_id, a.lesson_id, a.scenario_id, a.status, a.call_accepted_at, a.call_ended_at, a.call_end_reason,
       l.teacher_id, l.created_by, l.settings, s.call_script #>> '{dialogue,max_turns}'
  FROM attempts a
  JOIN lessons l ON l.id = a.lesson_id
  JOIN scenarios s ON s.id = a.scenario_id
 WHERE a.id = $1`

// Попытка блокируется FOR NO KEY UPDATE, а не FOR UPDATE (как store.LockAttempt и пакет
// attempts): транзакции разговора меняют только неключевые колонки, а INSERT в
// attempt_events (group commit событий ВСЕХ студентов одной пачкой) проверяет FK через
// FOR KEY SHARE, который конфликтует лишь с FOR UPDATE — иначе каждый ход разговора
// останавливал бы общую пачку событий до своего COMMIT.
const (
	sqlStateContext     = stateContextColumns
	sqlStateContextLock = stateContextColumns + ` FOR NO KEY UPDATE OF a`
)

// Транскрипт: оператор раньше заявителя внутри обмена (как store.ListDialogueTurns).
const sqlTurns = `
SELECT turn_no, speaker, text, source, confidence, audio_path, duration_ms, at, at_ms,
       emotional_state, revealed_fact_ids, meta
  FROM attempt_dialogue_turns
 WHERE attempt_id = $1
 ORDER BY turn_no, speaker = 'caller'`

// Блокировка попытки под запись хода + последний номер реплики оператора. Ходы одной
// попытки сериализует flight-guard; от гонки между инстансами страхует PK транскрипта.
// FOR NO KEY UPDATE — см. sqlStateContextLock (не тормозить group commit событий).
const sqlLockForTurn = `
SELECT a.status, a.call_ended_at,
       COALESCE((SELECT max(t.turn_no) FROM attempt_dialogue_turns t
                  WHERE t.attempt_id = a.id AND t.speaker = 'operator'), 0)
  FROM attempts a
 WHERE a.id = $1
   FOR NO KEY UPDATE`

// Обмен целиком: реплика оператора и ответ заявителя одним INSERT.
const sqlInsertExchange = `
INSERT INTO attempt_dialogue_turns
       (attempt_id, turn_no, speaker, text, source, confidence, audio_path, duration_ms, at, at_ms,
        emotional_state, revealed_fact_ids, meta)
VALUES ($1, $2, 'operator', $3, $4, $5, NULL, $6, $7, $8, NULL, '{}', $9),
       ($1, $2, 'caller', $10, $11, NULL, $12, $13, $14, $15, $16, $17, $18)`

// Счётчик реплик (Attempt.dialogue.turnsCount) и, если ход закрыл разговор, его завершение.
// NULL в $2/$3 — разговор продолжается (COALESCE оставляет как было).
const sqlUpdateAfterTurn = `
UPDATE attempts
   SET dialogue_turns = dialogue_turns + 2,
       call_ended_at = COALESCE(call_ended_at, $2),
       call_end_reason = COALESCE(call_end_reason, $3)
 WHERE id = $1`

// Серверные события попытки пачкой (client_seq NULL). Число событий 1..3 — текст один.
const sqlInsertEvents = `
INSERT INTO attempt_events (attempt_id, type, payload, at)
SELECT $1, e.type, e.payload, e.at
  FROM unnest($2::text[], $3::jsonb[], $4::timestamptz[]) AS e(type, payload, at)
RETURNING id, type`

// «Положить трубку».
const sqlEndCall = `
UPDATE attempts
   SET call_ended_at = $2, call_end_reason = $3
 WHERE id = $1 AND call_ended_at IS NULL`

// Вступительная реплика: попытка (уже заблокирована accept-call), настройки, легенда и
// реплика 0, если она уже есть (повторный accept-call идемпотентен).
const sqlOpeningContext = `
SELECT a.lesson_id, a.scenario_id, a.call_accepted_at, l.settings, s.call_script,
       t.text, t.source, t.audio_path, t.duration_ms, t.at, t.at_ms, t.emotional_state
  FROM attempts a
  JOIN lessons l ON l.id = a.lesson_id
  JOIN scenarios s ON s.id = a.scenario_id
  LEFT JOIN attempt_dialogue_turns t ON t.attempt_id = a.id AND t.turn_no = 0 AND t.speaker = 'caller'
 WHERE a.id = $1`

// Реплика 0 + счётчик одним запросом; конфликт PK (параллельный accept) — ничего не меняем.
const sqlInsertOpening = `
WITH ins AS (
  INSERT INTO attempt_dialogue_turns
         (attempt_id, turn_no, speaker, text, source, audio_path, duration_ms, at, at_ms, emotional_state)
  VALUES ($1, 0, 'caller', $2, 'script', $3, $4, $5, 0, $6)
  ON CONFLICT (attempt_id, turn_no, speaker) DO NOTHING
  RETURNING 1
)
UPDATE attempts SET dialogue_turns = dialogue_turns + 1
 WHERE id = $1 AND EXISTS (SELECT 1 FROM ins)`

// Озвучка вступления доехала после первого accept-call — дописываем её при повторном.
// Длительность файла неизвестна (NULL в tts_cache) — остаётся прежняя оценка.
const sqlPatchOpeningAudio = `
UPDATE attempt_dialogue_turns
   SET audio_path = $2, duration_ms = COALESCE($3, duration_ms)
 WHERE attempt_id = $1 AND turn_no = 0 AND speaker = 'caller' AND audio_path IS NULL`

// Закрытие разговора при submit: только голосовое занятие и только открытый разговор;
// событие dialogue_ended — тем же запросом (один round-trip в транзакции submit).
const sqlCloseOnSubmit = `
WITH upd AS (
  UPDATE attempts a
     SET call_ended_at = $2, call_end_reason = 'submitted'
    FROM lessons l
   WHERE a.id = $1 AND l.id = a.lesson_id AND a.call_ended_at IS NULL
     AND (l.settings #> '{voice,enabled}') = 'true'::jsonb
  RETURNING a.id, a.lesson_id
)
INSERT INTO attempt_events (attempt_id, type, payload, at)
SELECT upd.id, 'dialogue_ended', '{"reason":"submitted"}'::jsonb, $2 FROM upd
RETURNING id, (SELECT lesson_id FROM upd)`

// ---------------------------------------------------------------- модели чтения

// attemptCtx — то, что разговору нужно знать о попытке.
type attemptCtx struct {
	ID              uuid.UUID
	UserID          uuid.UUID
	LessonID        uuid.UUID
	ScenarioID      uuid.UUID
	Status          string
	CallAcceptedAt  *time.Time
	CallEndedAt     *time.Time
	CallEndReason   *string
	LessonTeacherID *uuid.UUID
	LessonCreatedBy uuid.UUID
	Settings        model.LessonSettings
	Script          *model.CallScript // только в контексте хода
	BriefMaxTurns   int               // call_script.dialogue.max_turns (0 — не задан)
}

// accessRow — минимальная store.AttemptRow для правил internal/access.
func (a *attemptCtx) accessRow() *store.AttemptRow {
	return &store.AttemptRow{
		ID:              a.ID,
		LessonID:        a.LessonID,
		UserID:          a.UserID,
		LessonTeacherID: a.LessonTeacherID,
		LessonCreatedBy: a.LessonCreatedBy,
	}
}

// maxTurns — лимит реплик оператора: из занятия, ужатый лимитом брифа, если он задан.
func (a *attemptCtx) maxTurns() int {
	n := a.Settings.Voice.MaxTurns
	if n <= 0 {
		n = 12
	}
	if a.BriefMaxTurns > 0 && a.BriefMaxTurns < n {
		n = a.BriefMaxTurns
	}
	return n
}

var defaultLessonSettings = model.DefaultLessonSettings(nil)

// settingsInto — lessons.settings с добивкой дефолтами (как store): кривое значение не
// должно ронять разговор — берётся best-effort результат разбора.
type settingsInto struct{ dst *model.LessonSettings }

func (s *settingsInto) ScanBytes(b []byte) error {
	*s.dst, _ = model.ParseLessonSettings(b, defaultLessonSettings)
	return nil
}

// jsonInto — jsonb прямо из буфера драйвера в dst (NULL оставляет dst как есть).
type jsonInto struct {
	dst  any
	name string
}

func (j *jsonInto) ScanBytes(b []byte) error {
	if b == nil {
		return nil
	}
	if err := json.Unmarshal(b, j.dst); err != nil {
		return fmt.Errorf("dialogue: jsonb %s: %w", j.name, err)
	}
	return nil
}

func scanTurnContext(row pgx.Row, a *attemptCtx) error {
	cs := new(model.CallScript)
	if err := row.Scan(&a.UserID, &a.LessonID, &a.ScenarioID, &a.Status, &a.CallAcceptedAt, &a.CallEndedAt,
		&a.CallEndReason, &a.LessonTeacherID, &a.LessonCreatedBy, &settingsInto{dst: &a.Settings},
		&jsonInto{dst: cs, name: "scenarios.call_script"}); err != nil {
		return err
	}
	cs.Normalize()
	a.Script = cs
	if cs.Dialogue != nil {
		a.BriefMaxTurns = cs.Dialogue.MaxTurns
	}
	return nil
}

func scanStateContext(row pgx.Row, a *attemptCtx) error {
	var briefMax *string
	if err := row.Scan(&a.UserID, &a.LessonID, &a.ScenarioID, &a.Status, &a.CallAcceptedAt, &a.CallEndedAt,
		&a.CallEndReason, &a.LessonTeacherID, &a.LessonCreatedBy, &settingsInto{dst: &a.Settings}, &briefMax); err != nil {
		return err
	}
	if briefMax != nil {
		if n, err := strconv.Atoi(*briefMax); err == nil && n > 0 {
			a.BriefMaxTurns = n
		}
	}
	return nil
}

// scanTurns — транскрипт (не nil).
func scanTurns(rows pgx.Rows, attemptID uuid.UUID) ([]store.DialogueTurnRow, error) {
	defer rows.Close()
	out := make([]store.DialogueTurnRow, 0, 16)
	for rows.Next() {
		out = append(out, store.DialogueTurnRow{AttemptID: attemptID})
		t := &out[len(out)-1]
		var meta []byte
		if err := rows.Scan(&t.TurnNo, &t.Speaker, &t.Text, &t.Source, &t.Confidence, &t.AudioPath,
			&t.DurationMs, &t.At, &t.AtMs, &t.EmotionalState, &t.RevealedFactIDs, &meta); err != nil {
			return nil, fmt.Errorf("dialogue: scan turn: %w", err)
		}
		t.At = t.At.UTC()
		t.Meta = meta
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dialogue: list turns: %w", err)
	}
	return out, nil
}

// load — контекст попытки и транскрипт одним батчем (один round-trip). withScript —
// легенда целиком (ход), иначе только лимит брифа (состояние). 404 — попытки нет.
func load(ctx context.Context, q pg.Querier, attemptID uuid.UUID, withScript bool) (*attemptCtx, []store.DialogueTurnRow, error) {
	b := &pgx.Batch{}
	if withScript {
		b.Queue(sqlTurnContext, attemptID)
	} else {
		b.Queue(sqlStateContext, attemptID)
	}
	b.Queue(sqlTurns, attemptID)
	br := q.SendBatch(ctx, b)
	defer br.Close()

	a := &attemptCtx{ID: attemptID}
	var err error
	if withScript {
		err = scanTurnContext(br.QueryRow(), a)
	} else {
		err = scanStateContext(br.QueryRow(), a)
	}
	if err != nil {
		if pg.IsNoRows(err) {
			return nil, nil, httpx.NotFound("Попытка не найдена")
		}
		return nil, nil, fmt.Errorf("dialogue: attempt context: %w", err)
	}
	rows, err := br.Query()
	if err != nil {
		return nil, nil, fmt.Errorf("dialogue: list turns: %w", err)
	}
	turns, err := scanTurns(rows, attemptID)
	if err != nil {
		return nil, nil, err
	}
	return a, turns, nil
}

// insertEvents — результат sqlInsertEvents из батча; события в виде для WS (порядок входа).
// RETURNING не гарантирует порядок строк — сопоставляем по типу (в пачке он уникален).
func insertEvents(br pgx.BatchResults, evs []pendingEvent) ([]public.AttemptEvent, error) {
	rows, err := br.Query()
	if err != nil {
		return nil, fmt.Errorf("dialogue: insert events: %w", err)
	}
	defer rows.Close()
	ids := make(map[string]int64, len(evs))
	for rows.Next() {
		var (
			id  int64
			typ string
		)
		if err := rows.Scan(&id, &typ); err != nil {
			return nil, fmt.Errorf("dialogue: scan event: %w", err)
		}
		ids[typ] = id
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dialogue: insert events: %w", err)
	}
	out := make([]public.AttemptEvent, 0, len(evs))
	for i := range evs {
		e := &evs[i]
		payload := e.payload
		out = append(out, public.AttemptEvent{
			Id:      int(ids[e.typ]),
			Type:    public.AttemptEventType(e.typ),
			Payload: &payload,
			At:      e.at,
		})
	}
	return out, nil
}

// pendingEvent — серверное событие к вставке (тип уникален в пределах одной пачки).
type pendingEvent struct {
	typ     string
	payload map[string]any
	at      time.Time
}

// queueEvents ставит вставку событий в батч (unnest — один текст на 1..3 события).
func queueEvents(b *pgx.Batch, attemptID uuid.UUID, evs []pendingEvent) error {
	types := make([]string, len(evs))
	payloads := make([]json.RawMessage, len(evs))
	ats := make([]time.Time, len(evs))
	for i := range evs {
		raw, err := json.Marshal(evs[i].payload)
		if err != nil {
			return fmt.Errorf("dialogue: event payload: %w", err)
		}
		types[i] = evs[i].typ
		payloads[i] = raw
		ats[i] = evs[i].at
	}
	b.Queue(sqlInsertEvents, attemptID, types, payloads, ats)
	return nil
}

// eventTime — время события так, как его вернёт БД (UTC, микросекунды): WS-копия и строка
// в attempt_events совпадают.
func eventTime(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }
