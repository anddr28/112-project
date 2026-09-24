package eventlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/pg"
)

// SQL — константный текст (pgx кэширует prepared statements по тексту запроса).
const (
	// Групповая вставка: один INSERT на батч из многих запросов. Дубли по (attempt_id, client_seq)
	// отбрасываются индексом attempt_events_dedup_idx, в том числе внутри одного INSERT
	// (первое вхождение выигрывает). RETURNING — только ключ: тип/payload/время известны из входа.
	sqlInsertBatch = `INSERT INTO attempt_events (attempt_id, client_seq, type, payload, at)
SELECT * FROM unnest($1::uuid[], $2::bigint[], $3::text[], $4::jsonb[], $5::timestamptz[])
ON CONFLICT (attempt_id, client_seq) WHERE client_seq IS NOT NULL DO NOTHING
RETURNING id, attempt_id, client_seq`

	// lastSeq контракта — максимальный сохранённый clientSeq попытки. Выполняется после
	// sqlInsertBatch в той же транзакции; по попытке — Index Only Scan Backward по частичному
	// индексу attempt_events_dedup_idx (LIMIT 1), без сканирования ленты.
	sqlLastSeq = `SELECT a.id, COALESCE((SELECT max(e.client_seq) FROM attempt_events e
                 WHERE e.attempt_id = a.id AND e.client_seq IS NOT NULL), 0)
FROM unnest($1::uuid[]) AS a(id)`

	sqlInsertServer = `INSERT INTO attempt_events (attempt_id, type, payload, at)
VALUES ($1, $2, $3, $4)
RETURNING id`

	// Индекс attempt_events_attempt_idx (attempt_id, at) отдаёт строки уже в нужном порядке.
	sqlList = `SELECT id, COALESCE(client_seq, 0), type, payload, at
FROM attempt_events
WHERE attempt_id = $1
ORDER BY at, id`
)

// InsertServer — серверное событие попытки (issued, call_accepted, submitted, dialogue_* …)
// синхронно в транзакции вызывающего: событие фиксируется вместе с изменением состояния.
// client_seq = NULL (в контракте clientSeq = 0). payload — любое JSON-сериализуемое значение,
// дающее объект (map, структура, json.RawMessage); nil — {}. at: нулевое — сейчас.
func InsertServer(ctx context.Context, q pg.Querier, attemptID uuid.UUID, typ string, payload any, at time.Time) (public.AttemptEvent, error) {
	raw, obj, err := encodeServerPayload(payload)
	if err != nil {
		return public.AttemptEvent{}, fmt.Errorf("eventlog: payload %s: %w", typ, err)
	}
	if at.IsZero() {
		at = time.Now()
	}
	at = at.UTC().Truncate(time.Microsecond)

	var id int64
	if err := q.QueryRow(ctx, sqlInsertServer, attemptID, typ, raw, at).Scan(&id); err != nil {
		return public.AttemptEvent{}, fmt.Errorf("eventlog: insert %s: %w", typ, err)
	}
	return public.AttemptEvent{
		Id:        int(id),
		ClientSeq: 0,
		Type:      public.AttemptEventType(typ),
		Payload:   obj,
		At:        at,
	}, nil
}

// List — хронология попытки по времени (при равенстве — по порядку записи). Никогда не nil.
func List(ctx context.Context, q pg.Querier, attemptID uuid.UUID) ([]public.AttemptEvent, error) {
	rows, err := q.Query(ctx, sqlList, attemptID)
	if err != nil {
		return nil, fmt.Errorf("eventlog: list: %w", err)
	}
	defer rows.Close()

	out := make([]public.AttemptEvent, 0, 64)
	var (
		id, seq int64
		typ     string
		payload []byte
		at      time.Time
	)
	for rows.Next() {
		if err := rows.Scan(&id, &seq, &typ, &payload, &at); err != nil {
			return nil, fmt.Errorf("eventlog: list scan: %w", err)
		}
		out = append(out, public.AttemptEvent{
			Id:        int(id),
			ClientSeq: int(seq),
			Type:      public.AttemptEventType(typ),
			Payload:   decodeObject(payload),
			At:        at.UTC(), // pgx отдаёт timestamptz в Local — в JSON нужен UTC
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("eventlog: list: %w", err)
	}
	return out, nil
}

var errNotObject = errors.New("payload должен быть JSON-объектом")

// encodeServerPayload — JSON для jsonb и объект для возвращаемого AttemptEvent.
func encodeServerPayload(payload any) (json.RawMessage, *map[string]any, error) {
	var raw []byte
	switch p := payload.(type) {
	case nil:
		return emptyObject, nil, nil
	case map[string]any:
		if len(p) == 0 {
			return emptyObject, nil, nil
		}
		b, err := json.Marshal(p)
		if err != nil {
			return nil, nil, err
		}
		return withSanitizedNUL(b, &p)
	case *map[string]any:
		if p == nil || len(*p) == 0 {
			return emptyObject, nil, nil
		}
		b, err := json.Marshal(*p)
		if err != nil {
			return nil, nil, err
		}
		return withSanitizedNUL(b, p)
	case json.RawMessage:
		raw = bytes.TrimSpace(p)
	case []byte:
		raw = bytes.TrimSpace(p)
	default:
		b, err := json.Marshal(p)
		if err != nil {
			return nil, nil, err
		}
		raw = b
	}
	if len(raw) == 0 || raw[0] != '{' {
		return nil, nil, errNotObject
	}
	if isEmptyObject(raw) {
		return emptyObject, nil, nil
	}
	// Копия перед санитайзингом: чужой срез (RawMessage вызывающего) не трогаем.
	if bytes.Contains(raw, nulEscape) {
		raw = sanitizeNUL(append([]byte(nil), raw...))
	}
	obj := decodeObject(raw)
	if obj == nil {
		return nil, nil, errNotObject
	}
	return raw, obj, nil
}

// withSanitizedNUL — JSON из json.Marshal и объект для ответа; если NUL пришлось заменить,
// объект разбирается заново, чтобы ответ/WS совпадали с записанным в jsonb.
func withSanitizedNUL(b []byte, obj *map[string]any) (json.RawMessage, *map[string]any, error) {
	if !bytes.Contains(b, nulEscape) {
		return b, obj, nil
	}
	b = sanitizeNUL(b)
	return b, decodeObject(b), nil
}
