package admin

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"lct/gocore/internal/settings"
)

func TestCanonicalValue(t *testing.T) {
	t.Parallel()
	custom := settings.Defaults()
	custom.AI.ReaperAfterSec = 600
	custom.Voice.Input = "both"

	cases := []struct {
		name string
		base *settings.Snapshot
		key  string
		v    string
		want map[string]any // ожидаемые поля канонического значения (для скаляров — ключ "")
		err  string         // подстрока ошибки
	}{
		{name: "частичное слияние объекта с дефолтами", key: "ai", v: `{"max_tries": 5}`,
			want: map[string]any{"max_tries": 5.0, "reaper_after_sec": 900.0, "dialog_timeout_sec": 20.0, "breaker_open_sec": 30.0, "breaker_failures": 3.0}},
		{name: "слияние с текущим снимком, не с дефолтами", base: &custom, key: "ai", v: `{"max_tries": 4}`,
			want: map[string]any{"max_tries": 4.0, "reaper_after_sec": 600.0}},
		{name: "пустой объект — значение не меняется", base: &custom, key: "voice", v: `{}`,
			want: map[string]any{"input": "both", "max_turns": 12.0, "enabled": false}},
		{name: "вложенная строка", key: "voice", v: `{"input": "text", "enabled": true}`,
			want: map[string]any{"input": "text", "enabled": true, "push_to_talk": true}},
		{name: "скаляр-целое", key: "time_limit_sec", v: `45`, want: map[string]any{"": 45.0}},
		{name: "скаляр-дробное", key: "pass_threshold", v: `72.5`, want: map[string]any{"": 72.5}},
		{name: "скаляр-логическое", key: "allow_replay", v: `false`, want: map[string]any{"": false}},
		{name: "неизвестный ключ", key: "no_such_key", v: `1`, err: "unknown key"},
		{name: "ключ в другом регистре — неизвестен", key: "AI", v: `{}`, err: "unknown key"},
		{name: "опечатка в имени поля", key: "ai", v: `{"maxTries": 5}`, err: "Настройка «ai»: неизвестное поле maxTries"},
		{name: "строка вместо целого поля", key: "ai", v: `{"max_tries": "5"}`, err: "поле max_tries должно быть целым числом"},
		{name: "дробное вместо целого поля", key: "ai", v: `{"max_tries": 3.5}`, err: "поле max_tries должно быть целым числом"},
		{name: "строка вместо объекта", key: "ai", v: `"abc"`, err: "Настройка «ai» должна быть объектом"},
		{name: "массив вместо числа", key: "pass_threshold", v: `[1]`, err: "Настройка «pass_threshold» должна быть числом"},
		{name: "логическое вместо целого", key: "time_limit_sec", v: `true`, err: "Настройка «time_limit_sec» должна быть целым числом"},
		{name: "строка вместо логического", key: "allow_replay", v: `"да"`, err: "должна быть логическим значением (true/false)"},
		{name: "число вместо строки поля", key: "tts", v: `{"voice": 1}`, err: "поле voice должно быть строкой"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var before settings.Snapshot
			if tc.base != nil {
				before = *tc.base
			}
			got, err := canonicalValue(tc.base, tc.key, json.RawMessage(tc.v))
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want containing %q", err, tc.err)
				}
				if tc.err == "unknown key" && !errors.Is(err, settings.ErrUnknownKey) {
					t.Fatalf("err = %v, want settings.ErrUnknownKey", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.base != nil && !reflect.DeepEqual(before, *tc.base) {
				t.Fatalf("canonicalValue изменил исходный снимок")
			}
			if s, ok := tc.want[""]; ok {
				var v any
				if err := json.Unmarshal(got, &v); err != nil || v != s {
					t.Fatalf("value = %s, want %v", got, s)
				}
				return
			}
			var m map[string]any
			if err := json.Unmarshal(got, &m); err != nil {
				t.Fatalf("canonical %s: %v", got, err)
			}
			for k, want := range tc.want {
				if m[k] != want {
					t.Errorf("%s = %v, want %v (value %s)", k, m[k], want, got)
				}
			}
		})
	}
}

// Канонический объект содержит все поля ключа (таблица settings всегда показывает все параметры).
func TestCanonicalValueHasAllFields(t *testing.T) {
	t.Parallel()
	got, err := canonicalValue(nil, "login", json.RawMessage(`{"max_failed": 7}`))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"max_failed", "lock_minutes", "session_ttl_hours"} {
		if _, ok := m[k]; !ok {
			t.Errorf("поле %s отсутствует: %s", k, got)
		}
	}
	if len(m) != 3 {
		t.Errorf("лишние поля: %s", got)
	}
}

func TestSnapshotWith(t *testing.T) {
	t.Parallel()
	def := settings.Defaults()
	cases := []struct {
		name  string
		key   string
		raw   string
		check func(s settings.Snapshot) bool
	}{
		{"нет строки — дефолты", "ai", "", func(s settings.Snapshot) bool { return reflect.DeepEqual(s, def) }},
		{"пробелы — дефолты", "ai", "  ", func(s settings.Snapshot) bool { return reflect.DeepEqual(s, def) }},
		{"частичный объект поверх дефолтов", "ai", `{"max_tries": 7}`, func(s settings.Snapshot) bool {
			return s.AI.MaxTries == 7 && s.AI.ReaperAfterSec == def.AI.ReaperAfterSec
		}},
		{"битое значение игнорируется целиком", "ai", `{"reaper_after_sec": 100, "max_tries": "x"}`, func(s settings.Snapshot) bool {
			return reflect.DeepEqual(s.AI, def.AI)
		}},
		{"невалидный JSON — дефолты", "ai", `{not json`, func(s settings.Snapshot) bool { return reflect.DeepEqual(s, def) }},
		{"скаляр", "time_limit_sec", `45`, func(s settings.Snapshot) bool { return s.TimeLimitSec == 45 }},
		{"лишние поля строки допускаются (как при загрузке)", "backup", `{"hour": 5, "legacy": 1}`, func(s settings.Snapshot) bool {
			return s.Backup.Hour == 5 && s.Backup.Enabled == def.Backup.Enabled
		}},
		{"другие ключи не трогаются", "voice", `{"input": "text"}`, func(s settings.Snapshot) bool {
			return s.Voice.Input == "text" && s.AI == def.AI && s.TimeLimitSec == def.TimeLimitSec
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var raw []byte
			if tc.raw != "" {
				raw = []byte(tc.raw)
			}
			if s := snapshotWith(tc.key, raw); !tc.check(s) {
				t.Fatalf("snapshotWith(%q, %s) = %+v", tc.key, tc.raw, s)
			}
		})
	}
}

func TestKnownSetting(t *testing.T) {
	t.Parallel()
	for _, k := range []string{"time_limit_sec", "pass_threshold", "score_weights", "dialogue_weight_default",
		"timing_tolerance", "xp_rules", "tts", "audit_retention_days", "voice", "stt", "confidence_threshold",
		"cards_per_student", "allow_replay", "ai", "login", "backup", "events_retention_days"} {
		if !knownSetting(k) {
			t.Errorf("knownSetting(%q) = false", k)
		}
	}
	for _, k := range []string{"", "AI", "ai.max_tries", "unknown", "настройка"} {
		if knownSetting(k) {
			t.Errorf("knownSetting(%q) = true", k)
		}
	}
}

func TestKindName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		t    reflect.Type
		want string
	}{
		{nil, "другого типа"},
		{reflect.TypeOf(int64(0)), "целым числом"},
		{reflect.TypeOf(uint8(0)), "целым числом"},
		{reflect.TypeOf(0.5), "числом"},
		{reflect.TypeOf(true), "логическим значением (true/false)"},
		{reflect.TypeOf(""), "строкой"},
		{reflect.TypeOf(settings.AI{}), "объектом"},
		{reflect.TypeOf(map[string]int{}), "объектом"},
		{reflect.TypeOf([]int{}), "массивом"},
		{reflect.TypeOf([2]int{}), "массивом"},
		{reflect.TypeOf(func() {}), "другого типа"},
	}
	for _, tc := range cases {
		if got := kindName(tc.t); got != tc.want {
			t.Errorf("kindName(%v) = %q, want %q", tc.t, got, tc.want)
		}
	}
}

func TestDecodeErrorFallback(t *testing.T) {
	t.Parallel()
	err := decodeError("ai", errors.New("unexpected EOF"))
	if err.Error() != "Некорректное значение настройки «ai»" {
		t.Fatalf("decodeError = %q", err)
	}
}

func TestCapitalize(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":                        "",
		"настройка \"ai\": плохо": "Настройка \"ai\": плохо",
		"Уже с заглавной":         "Уже с заглавной",
		"value must be set":       "Value must be set",
		"1-е значение":            "1-е значение",
		"ёлка":                    "Ёлка",
		string([]byte{0xff, 'a'}): string([]byte{0xff, 'a'}), // невалидный UTF-8 — как есть
	}
	for in, want := range cases {
		if got := capitalize(in); got != want {
			t.Errorf("capitalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSettingToPublic(t *testing.T) {
	t.Parallel()
	msk := time.FixedZone("MSK", 3*3600)
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, msk)
	s := settingToPublic(&settings.Row{Key: "ai", Value: json.RawMessage(`{"max_tries":3}`), Description: "Очередь AI", UpdatedAt: at})
	if s.Key != "ai" || s.Description == nil || *s.Description != "Очередь AI" {
		t.Fatalf("setting = %+v", s)
	}
	if s.UpdatedAt == nil || !s.UpdatedAt.Equal(at) || s.UpdatedAt.Location() != time.UTC {
		t.Fatalf("updatedAt = %v, want %v in UTC", s.UpdatedAt, at)
	}
	b, _ := json.Marshal(s)
	if !strings.Contains(string(b), `"value":{"max_tries":3}`) {
		t.Fatalf("value не как есть: %s", b)
	}

	empty := settingToPublic(&settings.Row{Key: "x"})
	if empty.Description != nil || empty.UpdatedAt != nil {
		t.Fatalf("пустые поля должны опускаться: %+v", empty)
	}
	b, _ = json.Marshal(empty)
	if !strings.Contains(string(b), `"value":null`) {
		t.Fatalf("value обязателен (null): %s", b)
	}
}
