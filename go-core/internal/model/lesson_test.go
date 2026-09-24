package model

import (
	"encoding/json"
	"testing"

	"lct/gocore/internal/settings"
)

func TestDefaultLessonSettingsMigrationDefaults(t *testing.T) {
	t.Parallel()

	d := settings.Defaults()
	got := DefaultLessonSettings(nil)
	if got.PassThreshold != d.PassThreshold || got.Weights != d.ScoreWeights ||
		got.CardsPerStudent != d.CardsPerStudent || got.AllowReplay != d.AllowReplay || got.Voice != d.Voice {
		t.Fatalf("DefaultLessonSettings(nil) = %+v, defaults %+v", got, d)
	}
	if got.Perspective != PerspectiveOperator112 {
		t.Fatalf("perspective = %q", got.Perspective)
	}
}

func TestDefaultLessonSettingsFromSnapshotFixesInvariants(t *testing.T) {
	t.Parallel()

	s := settings.Defaults()
	s.PassThreshold = 55
	s.CardsPerStudent = 0                                                 // сломанная настройка платформы
	s.Voice = settings.Voice{Enabled: true, Input: "шёпот", MaxTurns: -1} // кривой ввод
	s.ScoreWeights = settings.Weights{Fields: 1}
	got := DefaultLessonSettings(&s)
	if got.PassThreshold != 55 || got.Weights != (settings.Weights{Fields: 1}) {
		t.Fatalf("значения снимка не взяты: %+v", got)
	}
	if got.CardsPerStudent != 1 {
		t.Errorf("cards_per_student = %d, want 1 (деление на 0 недопустимо)", got.CardsPerStudent)
	}
	if got.Voice.Input != VoiceInputVoice || got.Voice.MaxTurns != 12 || !got.Voice.Enabled {
		t.Errorf("voice = %+v", got.Voice)
	}
}

func TestParseLessonSettings(t *testing.T) {
	t.Parallel()

	def := DefaultLessonSettings(nil)

	t.Run("пусто", func(t *testing.T) {
		t.Parallel()
		for _, raw := range []string{"", "  ", "null", "{}", " {} "} {
			got, err := ParseLessonSettings([]byte(raw), def)
			if err != nil || got != def {
				t.Errorf("%q -> %+v, %v", raw, got, err)
			}
		}
		got, err := ParseLessonSettings(nil, def)
		if err != nil || got != def {
			t.Errorf("nil -> %+v, %v", got, err)
		}
	})

	t.Run("частичные ключи добиваются дефолтами, вложенные тоже", func(t *testing.T) {
		t.Parallel()
		raw := `{"pass_threshold": 80, "weights": {"fields": 0.7}, "voice": {"enabled": true}, "perspective": "dds"}`
		got, err := ParseLessonSettings([]byte(raw), def)
		if err != nil {
			t.Fatal(err)
		}
		if got.PassThreshold != 80 || got.Perspective != PerspectiveDDS {
			t.Fatalf("got %+v", got)
		}
		if got.Weights.Fields != 0.7 || got.Weights.Semantic != def.Weights.Semantic || got.Weights.Timing != def.Weights.Timing {
			t.Fatalf("weights = %+v", got.Weights)
		}
		if !got.Voice.Enabled || got.Voice.Input != def.Voice.Input || got.Voice.MaxTurns != def.Voice.MaxTurns ||
			got.Voice.PushToTalk != def.Voice.PushToTalk || got.Voice.TTSEnabled != def.Voice.TTSEnabled {
			t.Fatalf("voice = %+v", got.Voice)
		}
		if got.CardsPerStudent != def.CardsPerStudent || got.AllowReplay != def.AllowReplay {
			t.Fatalf("не заданные ключи: %+v", got)
		}
	})

	t.Run("инварианты чинятся", func(t *testing.T) {
		t.Parallel()
		raw := `{"cards_per_student": 0, "perspective": "космос", "voice": {"input": "telepathy", "max_turns": 0}}`
		got, err := ParseLessonSettings([]byte(raw), def)
		if err != nil {
			t.Fatal(err)
		}
		if got.CardsPerStudent != 1 || got.Perspective != PerspectiveOperator112 ||
			got.Voice.Input != VoiceInputVoice || got.Voice.MaxTurns != 12 {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("все допустимые input сохраняются", func(t *testing.T) {
		t.Parallel()
		for _, in := range []string{VoiceInputVoice, VoiceInputText, VoiceInputBoth} {
			got, err := ParseLessonSettings([]byte(`{"voice":{"input":"`+in+`"}}`), def)
			if err != nil || got.Voice.Input != in {
				t.Errorf("input %q -> %q, %v", in, got.Voice.Input, err)
			}
		}
	})

	t.Run("ошибка типа — best effort", func(t *testing.T) {
		t.Parallel()
		raw := `{"pass_threshold": "восемьдесят", "cards_per_student": 3, "perspective": "dds"}`
		got, err := ParseLessonSettings([]byte(raw), def)
		if err == nil {
			t.Fatal("ожидалась ошибка разбора")
		}
		if got.PassThreshold != def.PassThreshold || got.CardsPerStudent != 3 || got.Perspective != PerspectiveDDS {
			t.Fatalf("остальные поля должны разобраться: %+v", got)
		}
	})

	t.Run("синтаксическая ошибка — дефолты", func(t *testing.T) {
		t.Parallel()
		got, err := ParseLessonSettings([]byte(`{"pass_threshold": 80,`), def)
		if err == nil {
			t.Fatal("ожидалась ошибка")
		}
		if got != def {
			t.Fatalf("got %+v, want def", got)
		}
	})

	t.Run("def не мутируется", func(t *testing.T) {
		t.Parallel()
		d := DefaultLessonSettings(nil)
		before := d
		_, _ = ParseLessonSettings([]byte(`{"weights":{"fields":0.9},"voice":{"enabled":true}}`), d)
		if d != before {
			t.Fatal("ParseLessonSettings изменил def")
		}
	})
}

func TestLessonSettingsJSONShape(t *testing.T) {
	t.Parallel()

	b, err := json.Marshal(DefaultLessonSettings(nil))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"pass_threshold", "weights", "cards_per_student", "allow_replay", "voice", "perspective"} {
		if _, ok := m[k]; !ok {
			t.Errorf("lessons.settings без ключа %q: %s", k, b)
		}
	}
	w, _ := m["weights"].(map[string]any)
	for _, k := range []string{"fields", "semantic", "grammar", "timing", "dialogue"} {
		if _, ok := w[k]; !ok {
			t.Errorf("weights без %q", k)
		}
	}
	v, _ := m["voice"].(map[string]any)
	for _, k := range []string{"enabled", "input", "push_to_talk", "max_turns", "tts_enabled"} {
		if _, ok := v[k]; !ok {
			t.Errorf("voice без %q", k)
		}
	}
	// записанное целиком читается обратно без потерь
	got, err := ParseLessonSettings(b, LessonSettings{})
	if err != nil || got != DefaultLessonSettings(nil) {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
}
