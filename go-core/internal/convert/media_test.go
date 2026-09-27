package convert

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestSeqExchangeNumbering(t *testing.T) {
	t.Parallel()

	// DESIGN §5 «Разговор»: вступление = 1, оператор k = 2k, заявитель k = 2k+1.
	tests := []struct {
		exchange int
		speaker  string
		seq      int
	}{
		{0, "caller", 1},
		{1, "operator", 2},
		{1, "caller", 3},
		{2, "operator", 4},
		{2, "caller", 5},
		{12, "operator", 24},
		{12, "caller", 25},
	}
	for _, tt := range tests {
		if got := ExchangeToSeq(tt.exchange, tt.speaker); got != tt.seq {
			t.Errorf("ExchangeToSeq(%d, %s) = %d, want %d", tt.exchange, tt.speaker, got, tt.seq)
		}
		if got := SeqToExchange(tt.seq); got != tt.exchange {
			t.Errorf("SeqToExchange(%d) = %d, want %d", tt.seq, got, tt.exchange)
		}
	}
	// граничные значения
	for _, s := range []int{-5, 0, 1} {
		if SeqToExchange(s) != 0 {
			t.Errorf("SeqToExchange(%d) != 0", s)
		}
	}
	if ExchangeToSeq(-3, "caller") != 1 || ExchangeToSeq(-3, "operator") != 0 {
		t.Error("отрицательный обмен -> 0")
	}
	// любой другой говорящий считается заявителем
	if ExchangeToSeq(3, "") != 7 {
		t.Error("неизвестный speaker")
	}
	// свойство: обратимость для всех k
	for k := 0; k < 100; k++ {
		for _, sp := range []string{"operator", "caller"} {
			if sp == "operator" && k == 0 {
				continue
			}
			if SeqToExchange(ExchangeToSeq(k, sp)) != k {
				t.Fatalf("не обратимо: k=%d %s", k, sp)
			}
		}
	}
}

func TestMediaURL(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"/", ""},
		{"ab/cd.wav", "/api/v1/media/tts/ab/cd.wav"},
		{"/ab/cd.wav/", "/api/v1/media/tts/ab/cd.wav"},
		{"ab//cd.wav", "/api/v1/media/tts/ab/cd.wav"},
		{" ab/cd.ogg ", "/api/v1/media/tts/ab/cd.ogg"},
		{"голос/реплика 1.wav", "/api/v1/media/tts/%D0%B3%D0%BE%D0%BB%D0%BE%D1%81/%D1%80%D0%B5%D0%BF%D0%BB%D0%B8%D0%BA%D0%B0%201.wav"},
		{"a?b#c.wav", "/api/v1/media/tts/a%3Fb%23c.wav"},
	}
	for _, tt := range tests {
		if got := MediaURL(tt.in); got != tt.want {
			t.Errorf("MediaURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestAudioMime(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"a/b.wav":  "audio/wav",
		"a/b.WAV":  "audio/wav",
		"b.ogg":    "audio/ogg",
		"b.opus":   "audio/ogg",
		"b.oga":    "audio/ogg",
		"b.mp3":    "audio/mpeg",
		"b.webm":   "audio/webm",
		"b.flac":   "",
		"без_расш": "",
		"":         "",
	}
	for in, want := range tests {
		if got := AudioMime(in); got != want {
			t.Errorf("AudioMime(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTTSHash(t *testing.T) {
	t.Parallel()

	ref := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:])
	}
	tests := []struct {
		text, voice string
		rate        float64
		canon       string
	}{
		{"Алло! У нас пожар!", "xenia", 1, "Алло! У нас пожар!|xenia|1.00"},
		{"Алло! У нас пожар!", "xenia", 1.0, "Алло! У нас пожар!|xenia|1.00"},
		{"Помогите", "baya", 0.9, "Помогите|baya|0.90"},
		{"Помогите", "baya", 1.25, "Помогите|baya|1.25"},
		{"", "", 0, "||0.00"},
		{"a|b", "c", 2, "a|b|c|2.00"},
	}
	for _, tt := range tests {
		got := TTSHash(tt.text, tt.voice, tt.rate)
		if got != ref(tt.canon) {
			t.Errorf("TTSHash(%q,%q,%v) = %s, want sha256(%q)", tt.text, tt.voice, tt.rate, got, tt.canon)
		}
		if len(got) != 64 {
			t.Errorf("длина хэша %d", len(got))
		}
	}
	if TTSHash("x", "xenia", 1) == TTSHash("x", "baya", 1) {
		t.Error("голос должен входить в ключ")
	}
	if TTSHash("x", "xenia", 1) == TTSHash("x", "xenia", 1.1) {
		t.Error("темп должен входить в ключ")
	}
}

func BenchmarkTTSHash(b *testing.B) {
	for b.Loop() {
		_ = TTSHash("Алло! Горит квартира на третьем этаже, срочно приезжайте!", "xenia", 1)
	}
}
