package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/settings"
)

// GET /admin/settings — все строки таблицы settings (value — jsonb как есть, snake_case).
func (h *Handlers) listSettings(w http.ResponseWriter, r *http.Request) error {
	rows, err := h.set.List(r.Context())
	if err != nil {
		return err
	}
	out := make([]public.Setting, 0, len(rows))
	for i := range rows {
		out = append(out, settingToPublic(&rows[i]))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

// PUT /admin/settings/{key} — {value}. Значение-объект сливается с текущим (не переданные
// поля сохраняют текущие значения), лишние/чужие поля отклоняются (опечатка вида maxTurns
// вместо max_turns иначе молча ничего бы не изменила). В БД пишется полный канонический
// объект — таблица всегда показывает все параметры ключа.
//
// Чтение–слияние–запись идут под блокировкой ключа и от актуальной строки БД, а не от
// кэша настроек: кэш отстаёт до settings.RefreshEvery (правка руками / другим инстансом),
// а два админа, одновременно меняющие разные поля одного ключа, иначе затирали бы друг друга.
func (h *Handlers) updateSetting(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	key := r.PathValue("key")
	var body struct {
		Value json.RawMessage `json:"value"`
	}
	if err := httpx.ReadJSON(r, &body); err != nil {
		return err
	}
	v := bytes.TrimSpace(body.Value)
	switch {
	case len(v) == 0:
		return httpx.Validation("Не передано значение настройки", map[string]string{"value": "обязательно"})
	case bytes.Equal(v, []byte("null")):
		return httpx.Validation("Значение настройки не может быть пустым", map[string]string{"value": "не может быть null"})
	}
	if !knownSetting(key) {
		return httpx.NotFound("Настройка «" + key + "» не найдена")
	}

	p := core.PrincipalFrom(ctx)
	before, after, err := h.mergeAndUpdate(ctx, key, v, p.UserID)
	if err != nil {
		return err
	}

	if h.aud != nil {
		h.aud.Log(ctx, core.AuditEntry{
			Action:     "setting.update",
			EntityType: "setting",
			Before:     map[string]any{"key": key, "value": rawOrNull(before.Value)},
			After:      map[string]any{"key": key, "value": rawOrNull(after.Value)},
		})
	}
	h.log.Info("настройка изменена", "key", key, "by", p.UserID)
	httpx.WriteJSON(w, http.StatusOK, settingToPublic(&after))
	return nil
}

// sqlSettingLock — транзакционная advisory-блокировка ключа настройки: правки одного ключа
// выполняются строго по очереди и между инстансами go-core. Ожидание ограничено lock_timeout
// сессии (pg.Connect) — дальше 409, а не очередь навсегда.
const sqlSettingLock = `SELECT pg_advisory_xact_lock(hashtext('lct:setting:' || $1))`

// mergeAndUpdate — слияние v с актуальным значением ключа и запись через settings.Store под
// блокировкой ключа. Ошибки проверки — уже httpx-ошибки (400/404/409), остальное — 500.
func (h *Handlers) mergeAndUpdate(ctx context.Context, key string, v json.RawMessage, by uuid.UUID) (before, after settings.Row, err error) {
	base, release, err := h.lockSetting(ctx, key)
	if err != nil {
		if pe, ok := pg.PgError(err); ok && pe.Code == pg.CodeLockNotAvailable {
			return before, after, httpx.Conflict("Настройку «" + key + "» сейчас изменяет другой администратор — повторите через несколько секунд")
		}
		return before, after, err
	}
	// Блокировка снимается после записи: Store.Update коммитит своим соединением раньше,
	// следующий ждущий прочитает уже новую строку.
	defer release()

	canon, err := canonicalValue(base, key, v)
	if err != nil {
		if errors.Is(err, settings.ErrUnknownKey) {
			return before, after, httpx.NotFound("Настройка «" + key + "» не найдена")
		}
		msg := err.Error()
		return before, after, httpx.Validation(msg, map[string]string{"value": msg})
	}

	before, after, err = h.set.Update(ctx, key, canon, by)
	if err != nil {
		switch {
		case errors.Is(err, settings.ErrUnknownKey):
			return before, after, httpx.NotFound("Настройка «" + key + "» не найдена")
		case strings.HasPrefix(err.Error(), "settings: "):
			// ошибки БД settings.Store помечает префиксом пакета; остальное — смысловая проверка
			return before, after, err
		default:
			msg := capitalize(err.Error())
			return before, after, httpx.Validation(msg, map[string]string{"value": msg})
		}
	}
	return before, after, nil
}

// lockSetting — блокировка ключа (в процессе — семафор, между инстансами — advisory-lock
// в транзакции) и снимок настроек, в котором значение ключа взято из строки БД, прочитанной
// уже под блокировкой. release обязателен (снимает обе блокировки). Без пула (тесты без БД) —
// снимок кэша.
func (h *Handlers) lockSetting(ctx context.Context, key string) (*settings.Snapshot, func(), error) {
	if h.pool == nil {
		return h.set.Get(ctx), func() {}, nil
	}
	// Семафор в процессе: ждущие блокировку ключа не занимают по соединению пула каждый
	// (иначе всплеск правок мог бы выбрать пул, а держателю блокировки нужно второе для записи).
	select {
	case h.setMu <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	unlock := func() { <-h.setMu }

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		unlock()
		return nil, nil, fmt.Errorf("admin: settings lock: %w", err)
	}
	release := func() {
		// транзакция ничего не пишет: ROLLBACK лишь снимает advisory-блокировку
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = tx.Rollback(rctx)
		cancel()
		unlock()
	}
	if _, err := tx.Exec(ctx, sqlSettingLock, key); err != nil {
		release()
		return nil, nil, fmt.Errorf("admin: settings lock: %w", err)
	}
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, key).Scan(&raw)
	if err != nil && !pg.IsNoRows(err) {
		release()
		return nil, nil, fmt.Errorf("admin: settings read: %w", err)
	}
	snap := snapshotWith(key, raw)
	return &snap, release, nil
}

// snapshotWith — дефолтный снимок, в котором значение key — из строки БД raw. Битое значение
// (правка руками) игнорируется целиком, как при загрузке настроек (settings.Store): ключ
// остаётся дефолтным, а не наполовину заполненным.
func snapshotWith(key string, raw []byte) settings.Snapshot {
	s := settings.Defaults()
	if len(bytes.TrimSpace(raw)) == 0 {
		return s
	}
	doc, err := json.Marshal(map[string]json.RawMessage{key: raw})
	if err != nil {
		return s
	}
	next := s
	if err := json.Unmarshal(doc, &next); err != nil {
		return s
	}
	return next
}

// knownSetting — ключ есть в схеме настроек (settings.Snapshot).
func knownSetting(key string) bool {
	d := settings.Defaults()
	fields, err := snapshotFields(&d)
	if err != nil {
		return false
	}
	_, ok := fields[key]
	return ok
}

func settingToPublic(r *settings.Row) public.Setting {
	s := public.Setting{Key: r.Key, Value: rawOrNull(r.Value)}
	if r.Description != "" {
		d := r.Description
		s.Description = &d
	}
	if !r.UpdatedAt.IsZero() {
		t := r.UpdatedAt.UTC()
		s.UpdatedAt = &t
	}
	return s
}

// rawOrNull — jsonb как json.RawMessage (кодируется без разбора); пустое — JSON null.
func rawOrNull(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	return json.RawMessage(b)
}

// canonicalValue — значение ключа после слияния v с текущим снимком настроек, в каноническом
// виде (все поля ключа, как их сериализует settings.Snapshot). Ключ, которого нет в схеме
// настроек, — settings.ErrUnknownKey. Ошибки типа — по-русски, с путём поля.
func canonicalValue(snap *settings.Snapshot, key string, v json.RawMessage) (json.RawMessage, error) {
	cur := settings.Defaults()
	if snap != nil {
		cur = *snap // Snapshot — только значения и вложенные структуры: копия глубокая
	}
	fields, err := snapshotFields(&cur)
	if err != nil {
		return nil, err
	}
	if _, ok := fields[key]; !ok {
		return nil, settings.ErrUnknownKey
	}

	doc, err := json.Marshal(map[string]json.RawMessage{key: v})
	if err != nil {
		return nil, errors.New("Некорректное значение настройки «" + key + "»")
	}
	// Декодирование поверх текущего снимка = слияние: encoding/json заполняет только
	// присутствующие поля вложенных структур, остальные остаются текущими.
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cur); err != nil {
		return nil, decodeError(key, err)
	}
	if fields, err = snapshotFields(&cur); err != nil {
		return nil, err
	}
	return fields[key], nil
}

func snapshotFields(s *settings.Snapshot) (map[string]json.RawMessage, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// decodeError — понятное админу сообщение вместо английского текста encoding/json.
func decodeError(key string, err error) error {
	var ute *json.UnmarshalTypeError
	if errors.As(err, &ute) {
		want := kindName(ute.Type)
		field := strings.TrimPrefix(strings.TrimPrefix(ute.Field, key), ".")
		if field == "" {
			return errors.New("Настройка «" + key + "» должна быть " + want)
		}
		return errors.New("Настройка «" + key + "»: поле " + field + " должно быть " + want)
	}
	if s := err.Error(); strings.HasPrefix(s, "json: unknown field ") {
		name := strings.Trim(strings.TrimPrefix(s, "json: unknown field "), `"`)
		return errors.New("Настройка «" + key + "»: неизвестное поле " + name)
	}
	return errors.New("Некорректное значение настройки «" + key + "»")
}

func kindName(t reflect.Type) string {
	if t == nil {
		return "другого типа"
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "целым числом"
	case reflect.Float32, reflect.Float64:
		return "числом"
	case reflect.Bool:
		return "логическим значением (true/false)"
	case reflect.String:
		return "строкой"
	case reflect.Struct, reflect.Map:
		return "объектом"
	case reflect.Slice, reflect.Array:
		return "массивом"
	}
	return "другого типа"
}

// capitalize — первая буква сообщения заглавная (сообщения settings начинаются со строчной).
func capitalize(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError || unicode.IsUpper(r) {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}
