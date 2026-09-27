package scoring

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"

	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/settings"
)

func TestTiming(t *testing.T) {
	t.Parallel()
	tol := settings.Defaults().TimingTolerance // soft 20%, hard 100%
	cases := []struct {
		name                 string
		spent, limit, react  int
		tol                  settings.TimingTolerance
		score                float64
		within               bool
		wantSpent, wantDelta int
		wantLimit, wantReact int
	}{
		{"instant", 0, 60, 1500, tol, 100, true, 0, -60000, 60, 1500},
		{"exactly at limit", 60000, 60, 0, tol, 100, true, 60000, 0, 60, 0},
		// как во фронте: withinNorm = spent ≤ limit·1.2
		{"at soft bound", 72000, 60, 0, tol, 100, true, 72000, 12000, 60, 0},
		{"just over soft bound: rounds to 100 but not within norm", 72001, 60, 0, tol, 100, false, 72001, 12001, 60, 0},
		{"halfway to hard", 96000, 60, 0, tol, 50, false, 96000, 36000, 60, 0},
		{"quarter way", 84000, 60, 0, tol, 75, false, 84000, 24000, 60, 0},
		{"at hard bound", 120000, 60, 0, tol, 0, false, 120000, 60000, 60, 0},
		{"far beyond hard", 500000, 60, 0, tol, 0, false, 500000, 440000, 60, 0},
		{"negative spent → 0", -5000, 60, 0, tol, 100, true, 0, -60000, 60, 0},
		{"negative reaction → 0", 1000, 60, -300, tol, 100, true, 1000, -59000, 60, 0},
		{"no limit: nothing to penalize", 999000, 0, 10, tol, 100, true, 999000, 999000, 0, 10},
		{"negative limit is treated as none", 5000, -30, 0, tol, 100, true, 5000, 5000, 0, 0},
		{"hard below soft collapses to soft", 90001, 60, 0, settings.TimingTolerance{SoftPct: 50, HardPct: 10}, 0, false, 90001, 30001, 60, 0},
		{"hard below soft, at soft", 90000, 60, 0, settings.TimingTolerance{SoftPct: 50, HardPct: 10}, 100, true, 90000, 30000, 60, 0},
		{"negative tolerance is zero", 60001, 60, 0, settings.TimingTolerance{SoftPct: -5, HardPct: -5}, 0, false, 60001, 1, 60, 0},
		{"NaN tolerance is zero", 60000, 60, 0, settings.TimingTolerance{SoftPct: math.NaN(), HardPct: math.NaN()}, 100, true, 60000, 0, 60, 0},
		{"zero tolerance, linear to hard", 90000, 60, 0, settings.TimingTolerance{SoftPct: 0, HardPct: 100}, 50, false, 90000, 30000, 60, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			score, r := Timing(c.spent, c.limit, c.react, c.tol)
			if score != c.score {
				t.Errorf("score = %v, want %v", score, c.score)
			}
			want := TimingResult{SpentMs: c.wantSpent, LimitSec: c.wantLimit, DeltaMs: c.wantDelta, WithinNorm: c.within, ReactionMs: c.wantReact}
			if r != want {
				t.Errorf("result = %+v, want %+v", r, want)
			}
		})
	}
}

// evaluations.timing — snake_case jsonb (VIEW прогресса читает within_norm); фронту — camelCase.
func TestTimingResultJSON(t *testing.T) {
	t.Parallel()
	r := TimingResult{SpentMs: 132000, LimitSec: 60, DeltaMs: 72000, WithinNorm: false, ReactionMs: 2100}
	if got, want := mustJSON(t, r), `{"spent_ms":132000,"limit_sec":60,"delta_ms":72000,"within_norm":false,"reaction_ms":2100}`; got != want {
		t.Errorf("db JSON = %s, want %s", got, want)
	}
	var ev public.Evaluation
	ev.Timing = r.ToPublic()
	b, err := json.Marshal(ev.Timing)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]any{"spentMs": 132000.0, "limitSec": 60.0, "deltaMs": 72000.0, "withinNorm": false, "reactionMs": 2100.0} {
		if m[k] != v {
			t.Errorf("public timing %q = %v, want %v (json %s)", k, m[k], v, b)
		}
	}
	if len(m) != 5 {
		t.Errorf("public timing has %d keys: %s", len(m), b)
	}

	// Пустой jsonb по умолчанию ('{}') разбирается в нулевое значение без ошибки.
	var z TimingResult
	if err := json.Unmarshal([]byte(`{}`), &z); err != nil || z != (TimingResult{}) {
		t.Errorf("unmarshal {} = %+v, %v", z, err)
	}
	// Round-trip.
	var back TimingResult
	if err := json.Unmarshal([]byte(mustJSON(t, r)), &back); err != nil || back != r {
		t.Errorf("round-trip = %+v, %v", back, err)
	}
}

func TestFormatDuration(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ms   int
		want string
	}{
		{-5000, "0 с"},
		{0, "0 с"},
		{499, "0 с"},
		{500, "1 с"},
		{45000, "45 с"},
		{59499, "59 с"},
		{59500, "1 мин"}, // округление до секунд, как Math.round во фронте
		{60000, "1 мин"},
		{83000, "1 мин 23 с"},
		{72400, "1 мин 12 с"},
		{120000, "2 мин"},
		{3723000, "62 мин 3 с"},
	}
	for _, c := range cases {
		if got := FormatDuration(c.ms); got != c.want {
			t.Errorf("FormatDuration(%d) = %q, want %q", c.ms, got, c.want)
		}
	}
}

// Регрессия: (ms+500) переполнялось на больших значениях, и строка уходила в минус.
func TestFormatDurationHuge(t *testing.T) {
	t.Parallel()
	total := math.MaxInt/1000 + 1 // остаток 807 мс ≥ 500 — округление вверх
	want := strconv.Itoa(total/60) + " мин"
	if ss := total % 60; ss != 0 {
		want += " " + strconv.Itoa(ss) + " с"
	}
	if got := FormatDuration(math.MaxInt); got != want {
		t.Errorf("FormatDuration(MaxInt) = %q, want %q", got, want)
	}
}

func TestFormatDelta(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ms   int
		want string
	}{
		{72000, "+1 мин 12 с"},
		{0, "+0 с"},
		{-4000, "−4 с"}, // типографский минус
		{-400, "−0 с"},  // как formatDelta во фронте
		{-83000, "−1 мин 23 с"},
	}
	for _, c := range cases {
		if got := FormatDelta(c.ms); got != c.want {
			t.Errorf("FormatDelta(%d) = %q, want %q", c.ms, got, c.want)
		}
	}
	// Регрессия: MinInt → «−» и корректная длительность, без ASCII-минуса внутри.
	got := FormatDelta(math.MinInt)
	if !strings.HasPrefix(got, "−") || strings.Contains(got, "-") || !strings.Contains(got, "мин") {
		t.Errorf("FormatDelta(MinInt) = %q", got)
	}
	if got := FormatDelta(math.MaxInt); !strings.HasPrefix(got, "+") || strings.Contains(got, "-") {
		t.Errorf("FormatDelta(MaxInt) = %q", got)
	}
}

func TestFormatClock(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ms   int
		want string
	}{
		{-1, "00:00"},
		{0, "00:00"},
		{999, "00:00"},
		{5999, "00:05"}, // таймер отсекает, а не округляет
		{65000, "01:05"},
		{600000, "10:00"},
		{3599000, "59:59"},
		{6000000, "100:00"},
	}
	for _, c := range cases {
		if got := FormatClock(c.ms); got != c.want {
			t.Errorf("FormatClock(%d) = %q, want %q", c.ms, got, c.want)
		}
	}
}
