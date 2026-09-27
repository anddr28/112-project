package aijobs

import (
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"lct/gocore/internal/core"
	"lct/gocore/internal/settings"
)

func TestTypeIdxMatchesJobTypes(t *testing.T) {
	t.Parallel()
	for i, typ := range jobTypes {
		if got := typeIdx(typ); got != i {
			t.Errorf("typeIdx(%s) = %d, want %d", typ, got, i)
		}
		if typ.Path() == "" {
			t.Errorf("%s: no ai-service path", typ)
		}
	}
	for _, bad := range []core.JobType{"", "evaluate", "EVALUATE_GRAMMAR", "проверка"} {
		if got := typeIdx(bad); got != -1 {
			t.Errorf("typeIdx(%q) = %d, want -1", bad, got)
		}
	}
}

func TestLanes(t *testing.T) {
	t.Parallel()
	want := map[core.JobType]int{
		core.JobEvaluateGrammar:  laneLT,
		core.JobEvaluateSemantic: laneLLM,
		core.JobEvaluateDialogue: laneLLM,
		core.JobGenerateScenario: laneLLM,
		core.JobTTS:              laneTTS,
	}
	for typ, lane := range want {
		i := typeIdx(typ)
		if got := laneOf(i); got != lane {
			t.Errorf("laneOf(%s) = %d, want %d", typ, got, lane)
		}
		found := false
		for _, name := range laneTypeNames[lane] {
			if name == string(typ) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s missing from laneTypeNames[%d]", typ, lane)
		}
	}
	for _, name := range evaluateTypeNames {
		if typeIdx(core.JobType(name)) < 0 || !strings.HasPrefix(name, "evaluate_") {
			t.Errorf("evaluateTypeNames: bad entry %q", name)
		}
	}
}

func TestClassifyJobStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code int
		want sendClass
	}{
		{200, sendAccepted}, {202, sendAccepted}, {204, sendAccepted},
		{429, sendBusy}, {503, sendBusy},
		{401, sendAuth}, {403, sendAuth},
		{400, sendRejected}, {413, sendRejected}, {415, sendRejected}, {422, sendRejected},
		{404, sendClientErr}, {409, sendClientErr}, {302, sendClientErr},
		{500, sendServerErr}, {502, sendServerErr}, {504, sendServerErr},
	}
	for _, c := range cases {
		if got := classifyJobStatus(c.code); got != c.want {
			t.Errorf("classifyJobStatus(%d) = %d, want %d", c.code, got, c.want)
		}
	}
}

func TestBackoff(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n    int
		want time.Duration
	}{
		{-1, 5 * time.Second}, {0, 5 * time.Second}, {1, 20 * time.Second}, {2, 80 * time.Second},
		{3, 320 * time.Second}, {4, 10 * time.Minute}, {50, 10 * time.Minute},
	}
	for _, c := range cases {
		if got := backoff(c.n); got != c.want {
			t.Errorf("backoff(%d) = %v, want %v", c.n, got, c.want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in     string
		want   time.Duration
		wantOK bool
	}{
		{"", 0, false},
		{"   ", 0, false},
		{"7", 7 * time.Second, true},
		{" 12 ", 12 * time.Second, true},
		{"0", 0, true},
		{"-3", 0, false},
		{"через минуту", 0, false},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second, true},
		{now.Add(-time.Hour).Format(http.TimeFormat), 0, true},
	}
	for _, c := range cases {
		got, ok := parseRetryAfter(c.in, now)
		if got != c.want || ok != c.wantOK {
			t.Errorf("parseRetryAfter(%q) = %v,%v; want %v,%v", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

func TestBusyDelay(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cases := map[string]time.Duration{
		"":        busyDefault,
		"мусор":   busyDefault,
		"0":       time.Second,
		"3":       3 * time.Second,
		"100000":  busyMax,
		"300":     5 * time.Minute,
		"299":     299 * time.Second,
		" 1 ":     time.Second,
		"-1":      busyDefault,
		"1.5":     busyDefault,
		"9999999": busyMax,
	}
	for in, want := range cases {
		if got := busyDelay(in, now); got != want {
			t.Errorf("busyDelay(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestShouldRetry(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		code      string
		retryable bool
		try, max  int
		prev      string
		want      bool
	}{
		{"retryable first failure", "llm_timeout", true, 0, 3, "", true},
		{"retryable second failure", "model_unavailable", true, 1, 3, "llm_timeout", true},
		{"tries exhausted", "llm_timeout", true, 2, 3, "llm_timeout", false},
		{"max_tries 1 — no retries", "llm_timeout", true, 0, 1, "", false},
		{"not retryable", "internal", false, 0, 3, "", false},
		{"bad_payload never retried even if flagged retryable", "bad_payload", true, 0, 5, "", false},
		{"invalid output: first — one retry", "llm_invalid_output", true, 0, 3, "", true},
		{"invalid output twice in a row — stop", "llm_invalid_output", true, 1, 3, "llm_invalid_output", false},
		{"invalid output twice in a row — stop even with many tries", "llm_invalid_output", true, 1, 10, "llm_invalid_output", false},
		// Регрессия: раньше предел llm_invalid_output (2) сравнивался с общим try_count, и после
		// одного llm_timeout повтора для невалидного ответа не было вовсе.
		{"invalid output after timeout — one retry", "llm_invalid_output", true, 1, 3, "llm_timeout", true},
		{"invalid output after timeout — still bounded by max_tries", "llm_invalid_output", true, 2, 3, "llm_timeout", false},
		{"invalid output with max_tries 2", "llm_invalid_output", true, 0, 2, "", true},
		{"invalid output not retryable", "llm_invalid_output", false, 0, 3, "", false},
		{"timeout after invalid output retried", "llm_timeout", true, 1, 3, "llm_invalid_output", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldRetry(c.code, c.retryable, c.try, c.max, c.prev); got != c.want {
				t.Errorf("shouldRetry(%s, %v, %d, %d, %q) = %v, want %v", c.code, c.retryable, c.try, c.max, c.prev, got, c.want)
			}
		})
	}
}

func TestReaperHoldSec(t *testing.T) {
	t.Parallel()
	if got := reaperHoldSec(&settings.AI{ReaperAfterSec: 900}); got != 900*reaperMaxResends {
		t.Errorf("hold(900) = %v", got)
	}
	if got := reaperHoldSec(&settings.AI{ReaperAfterSec: 0}); got != reaperMaxResends {
		t.Errorf("hold(0) = %v, want %d", got, reaperMaxResends)
	}
	// Удержание должно быть заметно дольше прежних max_tries × reaper_after (45 мин).
	if d := settings.Defaults().AI; reaperHoldSec(&d) < float64(3*d.MaxTries*d.ReaperAfterSec) {
		t.Errorf("default hold %v s is too short", reaperHoldSec(&d))
	}
}

func TestErrorText(t *testing.T) {
	t.Parallel()
	if got := errorText("llm_timeout", ""); got != "llm_timeout" {
		t.Errorf("empty message: %q", got)
	}
	if got := errorText("llm_timeout", "  таймаут 120 с \n"); got != "llm_timeout: таймаут 120 с" {
		t.Errorf("trim: %q", got)
	}
	long := strings.Repeat("ошибка ", 200) // 2 байта на букву — обрезка может попасть в середину руны
	got := errorText("internal", long)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated text is not valid UTF-8: %q", got[len(got)-10:])
	}
	if !strings.HasPrefix(got, "internal: ошибка") || !strings.HasSuffix(got, "…") {
		t.Errorf("long message: %q…", got[:40])
	}
	if n := len(got) - len("internal: ") - len("…"); n > maxErrText {
		t.Errorf("message part is %d bytes, limit %d", n, maxErrText)
	}
}

func TestTruncateUTF8(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"", 5, ""},
		{"abc", 5, "abc"},
		{"abc", 3, "abc"},
		{"abcdef", 3, "abc"},
		{"привет", 3, "п"},  // 3-й байт — середина «р»
		{"привет", 4, "пр"}, // ровно на границе
		{"a😀b", 2, "a"},     // эмодзи 4 байта
		{"a😀b", 5, "a😀"},
		{"ёж", 1, ""},
		{"ёж", 0, ""},
	}
	for _, c := range cases {
		got := truncateUTF8(c.in, c.n)
		if got != c.want || !utf8.ValidString(got) {
			t.Errorf("truncateUTF8(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestHumanError(t *testing.T) {
	t.Parallel()
	codes := []string{"llm_timeout", "llm_invalid_output", "model_unavailable", "lt_unavailable", "tts_failed",
		"stt_failed", CodeBadPayload, CodeDispatchFailed, CodeReaperTimeout, CodeAIUnavailable, "busy", "unreachable", "auth",
		codeResent}
	generic := humanError("internal")
	seen := map[string]string{}
	for _, c := range codes {
		msg := humanError(c + ": технические подробности HTTP 500")
		if msg == generic {
			t.Errorf("%s: falls back to generic message", c)
		}
		if strings.Contains(msg, "HTTP") || strings.Contains(msg, c) {
			t.Errorf("%s: technical details leak into user message: %q", c, msg)
		}
		if prev, dup := seen[msg]; dup {
			t.Errorf("%s and %s share message %q", c, prev, msg)
		}
		seen[msg] = c
		if humanError(c) != msg {
			t.Errorf("%s: message depends on details", c)
		}
	}
	for _, s := range []string{"", "что-то странное", ": без кода"} {
		if humanError(s) != generic {
			t.Errorf("humanError(%q) = %q, want generic", s, humanError(s))
		}
	}
}
