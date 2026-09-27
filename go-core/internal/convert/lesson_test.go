package convert

import (
	"encoding/json"
	"strings"
	"testing"

	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/settings"
)

func TestVoiceToPublic(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		in        settings.Voice
		wantInput string
		wantTurns int
	}{
		{"дефолты", settings.Voice{Enabled: true, Input: "voice", PushToTalk: true, MaxTurns: 12, TTSEnabled: true}, "voice", 12},
		{"text", settings.Voice{Input: "text", MaxTurns: 5}, "text", 5},
		{"both", settings.Voice{Input: "both", MaxTurns: 40}, "both", 40},
		{"кривой input и 0 реплик", settings.Voice{Input: "голос", MaxTurns: 0}, "voice", 12},
		{"отрицательные реплики", settings.Voice{Input: "", MaxTurns: -3}, "voice", 12},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := VoiceToPublic(tt.in)
			if p.Enabled != tt.in.Enabled || string(p.Input) != tt.wantInput || Deref(p.MaxTurns) != tt.wantTurns ||
				p.PushToTalk == nil || *p.PushToTalk != tt.in.PushToTalk || p.TtsEnabled == nil || *p.TtsEnabled != tt.in.TTSEnabled {
				t.Fatalf("VoiceToPublic(%+v) = %+v", tt.in, p)
			}
			// все опциональные поля заданы явно — фронт не додумывает
			b, _ := json.Marshal(p)
			for _, k := range []string{"enabled", "input", "pushToTalk", "maxTurns", "ttsEnabled"} {
				if !strings.Contains(string(b), `"`+k+`"`) {
					t.Errorf("json без %s: %s", k, b)
				}
			}
		})
	}
}

func TestVoiceFromPublic(t *testing.T) {
	t.Parallel()

	def := settings.Voice{Enabled: false, Input: "voice", PushToTalk: true, MaxTurns: 12, TTSEnabled: true}
	if got := VoiceFromPublic(nil, def); got != def {
		t.Fatalf("nil -> def, got %+v", got)
	}
	got := VoiceFromPublic(&public.VoiceSettings{Enabled: true}, def)
	if want := (settings.Voice{Enabled: true, Input: "voice", PushToTalk: true, MaxTurns: 12, TTSEnabled: true}); got != want {
		t.Fatalf("только enabled: %+v", got)
	}
	ptt, tts, mt := false, false, 20
	got = VoiceFromPublic(&public.VoiceSettings{Enabled: true, Input: "both", PushToTalk: &ptt, TtsEnabled: &tts, MaxTurns: &mt}, def)
	if want := (settings.Voice{Enabled: true, Input: "both", PushToTalk: false, MaxTurns: 20, TTSEnabled: false}); got != want {
		t.Fatalf("всё прислано: %+v", got)
	}
}

func TestLessonSettingsToPublic(t *testing.T) {
	t.Parallel()

	s := model.DefaultLessonSettings(nil)
	s.Weights = settings.Weights{Fields: 0.4, Semantic: 0.2, Grammar: 0.1, Timing: 0.1, Dialogue: 0.2}
	s.CardsPerStudent = 0 // строка в обход приложения
	s.PassThreshold = 72.5
	p := LessonSettingsToPublic(s)
	if p.PassThreshold != 72.5 || p.CardsPerStudent != 1 || p.AllowReplay != s.AllowReplay {
		t.Fatalf("got %+v", p)
	}
	w := p.Weights
	if w.Fields != 0.4 || w.Semantic != 0.2 || w.Grammar != 0.1 || w.Timing != 0.1 || w.Dialogue != 0.2 {
		t.Fatalf("weights = %+v", w)
	}
	b, _ := json.Marshal(p)
	for _, k := range []string{`"passThreshold"`, `"cardsPerStudent"`, `"allowReplay"`, `"voice"`, `"dialogue"`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("json без %s: %s", k, b)
		}
	}
}

func TestPerspective(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]public.ArmPerspective{
		model.PerspectiveDDS:         public.Dds,
		model.PerspectiveOperator112: public.Operator112,
		"":                           public.Operator112,
		"неизвестно":                 public.Operator112,
	} {
		if got := Perspective(model.LessonSettings{Perspective: in}); got != want {
			t.Errorf("Perspective(%q) = %q, want %q", in, got, want)
		}
	}
}
