// Package eventlog — журнал событий попытки (attempt_events): group commit клиентских
// батчей (db-design §3, Р5–Р6) и синхронная запись серверных событий в транзакции.
//
// Идемпотентность — по (attempt_id, client_seq): повтор батча после обрыва безопасен
// (ON CONFLICT DO NOTHING по частичному уникальному индексу). Серверные события пишутся
// с client_seq = NULL (в публичном контракте clientSeq = 0).
package eventlog

import (
	"bytes"
	"encoding/json"
	"strconv"
	"time"

	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
)

// Лимиты входного батча (frontend.v1.yaml: POST /attempts/{id}/events, 1..200 событий).
const (
	MaxBatch = 200
	// MaxPayloadBytes — потолок одного payload в БД. Больше — не отклоняем (клиент повторял бы
	// батч бесконечно и застрял бы весь поток событий), а заменяем заглушкой {truncated, bytes, field}.
	MaxPayloadBytes = 8 << 10

	// Санитарные границы клиентского времени: часы клиента могут врать, но событие из будущего
	// или «позавчера» ломает тайминг и сортировку ленты — такие метки заменяются временем сервера.
	maxFutureSkew = 5 * time.Minute
	maxPastSkew   = 24 * time.Hour

	maxFieldErrors = 10 // сколько ошибок по полям отдаём клиенту (ответ ограничен)
)

// emptyObject — payload по умолчанию (колонка NOT NULL DEFAULT '{}'); общий срез только для чтения.
var emptyObject = json.RawMessage(`{}`)

// Input — провалидированное клиентское событие, готовое к записи.
type Input struct {
	ClientSeq int64
	Type      string
	Payload   json.RawMessage // JSON-объект; nil/пусто — '{}'
	At        time.Time       // UTC, с точностью до микросекунд (как timestamptz)

	// obj — исходный объект payload: отдаётся в WS-событии без повторного разбора JSON.
	// nil — payload пуст ({}), либо Input собран вручную (тогда разбирается из Payload).
	obj *map[string]any
}

// ParseInputs валидирует тело POST /attempts/{id}/events и готовит события к записи.
// Ошибка — *httpx.Error (400 validation) с details.fields вида "events[3].type".
func ParseInputs(body []public.AttemptEventInput, now time.Time) ([]Input, error) {
	if len(body) == 0 || len(body) > MaxBatch {
		return nil, httpx.Validation("В пачке должно быть от 1 до 200 событий",
			map[string]string{"events": "от 1 до 200 событий"})
	}
	now = now.UTC()
	out := make([]Input, len(body))
	var fields map[string]string
	fail := func(i int, field, msg string) {
		if fields == nil {
			fields = make(map[string]string, 2)
		}
		if len(fields) < maxFieldErrors {
			fields["events["+strconv.Itoa(i)+"]."+field] = msg
		}
	}
	for i := range body {
		e := &body[i]
		if e.ClientSeq < 1 {
			fail(i, "clientSeq", "должен быть не меньше 1")
			continue
		}
		if !e.Type.Valid() {
			fail(i, "type", "неизвестный тип события")
			continue
		}
		raw, obj, err := encodeClientPayload(e.Payload)
		if err != nil {
			fail(i, "payload", "некорректный объект")
			continue
		}
		out[i] = Input{
			ClientSeq: int64(e.ClientSeq),
			Type:      string(e.Type),
			Payload:   raw,
			At:        clampAt(e.At, now),
			obj:       obj,
		}
	}
	if fields != nil {
		return nil, httpx.Validation("Некорректные события в пачке", fields)
	}
	return out, nil
}

// clampAt — метка клиента, если она правдоподобна, иначе время сервера. Точность — микросекунды:
// ровно то, что хранит timestamptz, чтобы WS-событие и последующее чтение из БД совпадали.
func clampAt(at, now time.Time) time.Time {
	if at.IsZero() || at.After(now.Add(maxFutureSkew)) || at.Before(now.Add(-maxPastSkew)) {
		at = now
	}
	return at.UTC().Truncate(time.Microsecond)
}

// encodeClientPayload — JSON payload для jsonb и объект для WS.
func encodeClientPayload(p *map[string]any) (json.RawMessage, *map[string]any, error) {
	if p == nil || len(*p) == 0 {
		return emptyObject, nil, nil
	}
	raw, err := json.Marshal(*p)
	if err != nil {
		return nil, nil, err
	}
	obj := p
	if len(raw) > MaxPayloadBytes {
		stub := truncatedStub(*p, len(raw))
		if raw, err = json.Marshal(stub); err != nil {
			return nil, nil, err
		}
		obj = &stub
	}
	// NUL — и в заглушке (имя поля); WS-копия должна совпадать с тем, что ляжет в jsonb.
	// Редкий путь: повторный разбор только если было что заменять.
	if bytes.Contains(raw, nulEscape) {
		raw = sanitizeNUL(raw)
		obj = decodeObject(raw)
	}
	return raw, obj, nil
}

// truncatedStub — заглушка вместо слишком большого payload: сохраняем факт и время события
// (тайминг и аналитика «где тормозит» не страдают) и имя поля, если оно есть.
func truncatedStub(p map[string]any, size int) map[string]any {
	stub := map[string]any{"truncated": true, "bytes": size}
	if f, ok := p["field"].(string); ok && len(f) <= 200 {
		stub["field"] = f
	}
	return stub
}

var nulEscape = []byte(`\u0000`)

// sanitizeNUL заменяет экранированный NUL (\u0000) на U+FFFD: jsonb его не принимает
// («unsupported Unicode escape sequence»), и один такой символ уронил бы весь групповой INSERT.
// Работает по выходу json.Marshal: обратная косая черта встречается только внутри строк и всегда
// начинает escape-последовательность, поэтому достаточно пропускать escape'ы целиком.
func sanitizeNUL(raw []byte) []byte {
	if !bytes.Contains(raw, nulEscape) {
		return raw
	}
	for i := 0; i < len(raw)-1; i++ {
		if raw[i] != '\\' {
			continue
		}
		if raw[i+1] == 'u' && i+6 <= len(raw) && bytes.Equal(raw[i+2:i+6], []byte("0000")) {
			copy(raw[i+2:i+6], "fffd")
			i += 5
			continue
		}
		i++ // \\, \", \n и т.п. — пропускаем второй символ последовательности
	}
	return raw
}

// payloadMap — объект payload для публичного AttemptEvent (nil — пустой payload, поле опускается).
func (in *Input) payloadMap() *map[string]any {
	if in.obj != nil {
		return in.obj
	}
	return decodeObject(in.Payload)
}

// decodeObject разбирает jsonb-объект; пустой объект и мусор — nil (payload в ответе опускается).
func decodeObject(raw []byte) *map[string]any {
	if isEmptyObject(raw) {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 { // null, [], "…", { } — не объект с данными
		return nil
	}
	return &m
}

func isEmptyObject(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || (len(raw) == 2 && raw[0] == '{' && raw[1] == '}')
}
