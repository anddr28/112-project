package scoring

import (
	"math"
	"testing"

	"lct/gocore/internal/core"
	"lct/gocore/internal/settings"
)

func TestRound2(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want float64
	}{
		{0, 0},
		{12.344, 12.34},
		{12.346, 12.35},
		{100, 100},
		{-3.456, -3.46},
		{-0.001, 0},
		{math.NaN(), 0},
		{math.Inf(1), 0},
		{math.Inf(-1), 0},
	}
	for _, c := range cases {
		got := Round2(c.in)
		if got != c.want {
			t.Errorf("Round2(%v) = %v, want %v", c.in, got, c.want)
		}
		if got == 0 && math.Signbit(got) {
			t.Errorf("Round2(%v) = -0, want +0", c.in)
		}
	}
}

func TestRound4(t *testing.T) {
	t.Parallel()
	if got := round4(1.0 / 3); got != 0.3333 {
		t.Errorf("round4(1/3) = %v", got)
	}
	if got := round4(-0.00001); got != 0 || math.Signbit(got) {
		t.Errorf("round4(-0.00001) = %v, want +0", got)
	}
	if got := round4(math.NaN()); got != 0 {
		t.Errorf("round4(NaN) = %v", got)
	}
}

func TestVerdict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		final, threshold float64
		want             string
	}{
		{70, 70, core.VerdictPass},
		{85.5, 70, core.VerdictPass},
		{69.99, 70, core.VerdictFail},
		{69.999, 70, core.VerdictPass}, // сравнение по сотым: 69.999 → 70.00
		{0, 0, core.VerdictPass},
		{0, 0.01, core.VerdictFail},
		{100, 100, core.VerdictPass},
		{math.NaN(), 50, core.VerdictFail},
	}
	for _, c := range cases {
		if got := Verdict(c.final, c.threshold); got != c.want {
			t.Errorf("Verdict(%v, %v) = %q, want %q", c.final, c.threshold, got, c.want)
		}
	}
}

func weightsApprox(a, b settings.Weights, eps float64) bool {
	return approx(a.Fields, b.Fields, eps) && approx(a.Semantic, b.Semantic, eps) &&
		approx(a.Grammar, b.Grammar, eps) && approx(a.Timing, b.Timing, eps) &&
		approx(a.Dialogue, b.Dialogue, eps)
}

func TestEffectiveWeights(t *testing.T) {
	t.Parallel()
	def := settings.Defaults().ScoreWeights // 0.5 / 0.25 / 0.1 / 0.15 / 0
	voiceDef := settings.Weights{Fields: 0.375, Semantic: 0.1875, Grammar: 0.075, Timing: 0.1125, Dialogue: 0.25}
	cases := []struct {
		name  string
		in    settings.Weights
		voice bool
		dd    float64
		want  settings.Weights
	}{
		{"defaults, voice off", def, false, 0.25, def},
		// как withDialogueWeight во фронте: остальные ужимаются пропорционально до 0.75
		{"defaults, voice on", def, true, 0.25, voiceDef},
		{"voice off drops dialogue and renormalizes",
			settings.Weights{Fields: 0.4, Semantic: 0.2, Grammar: 0.1, Timing: 0.1, Dialogue: 0.2}, false, 0.25,
			settings.Weights{Fields: 0.5, Semantic: 0.25, Grammar: 0.125, Timing: 0.125}},
		{"voice on keeps explicit dialogue",
			settings.Weights{Fields: 0.4, Semantic: 0.2, Grammar: 0.1, Timing: 0.1, Dialogue: 0.2}, true, 0.25,
			settings.Weights{Fields: 0.4, Semantic: 0.2, Grammar: 0.1, Timing: 0.1, Dialogue: 0.2}},
		{"not normalized input",
			settings.Weights{Fields: 2, Semantic: 1, Grammar: 1}, false, 0.25,
			settings.Weights{Fields: 0.5, Semantic: 0.25, Grammar: 0.25}},
		{"negative, NaN and Inf count as zero",
			settings.Weights{Fields: -1, Semantic: math.NaN(), Grammar: math.Inf(1), Timing: 3}, false, 0.25,
			settings.Weights{Timing: 1}},
		{"all zero, voice off → settings defaults", settings.Weights{}, false, 0.25, def},
		// фронт (withDialogueWeight + normalizeWeights) даёт то же: весь итог — разговор
		{"all zero, voice on → dialogue only", settings.Weights{}, true, 0.25, settings.Weights{Dialogue: 1}},
		{"all zero, voice on, no dialogue default → settings defaults with voice", settings.Weights{}, true, 0, voiceDef},
		{"dialogue default >= 1 falls back to settings default", def, true, 1.5, voiceDef},
		{"dialogue default 0 keeps dialogue at zero", def, true, 0, def},
		{"dialogue default NaN keeps dialogue at zero", def, true, math.NaN(), def},
		{"dialogue default negative keeps dialogue at zero", def, true, -0.3, def},
		{"only dialogue with voice on",
			settings.Weights{Dialogue: 5}, true, 0.25, settings.Weights{Dialogue: 1}},
		{"only dialogue with voice off → defaults",
			settings.Weights{Dialogue: 5}, false, 0.25, def},
		{"huge finite weights",
			settings.Weights{Fields: 1e300, Semantic: 1e300}, false, 0.25,
			settings.Weights{Fields: 0.5, Semantic: 0.5}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := EffectiveWeights(c.in, c.voice, c.dd)
			if !weightsApprox(got, c.want, 1e-9) {
				t.Fatalf("EffectiveWeights(%+v, voice=%v, dd=%v) = %+v, want %+v", c.in, c.voice, c.dd, got, c.want)
			}
			if s := got.Sum(); !approx(s, 1, 1e-9) {
				t.Fatalf("sum = %v, want 1", s)
			}
			if !c.voice && got.Dialogue != 0 {
				t.Fatalf("voice off, dialogue = %v", got.Dialogue)
			}
		})
	}
}

// Оценка повторно прогоняет веса занятия (уже эффективные и округлённые) через
// EffectiveWeights — результат не должен «уплывать».
func TestEffectiveWeightsIdempotent(t *testing.T) {
	t.Parallel()
	inputs := []settings.Weights{
		settings.Defaults().ScoreWeights,
		{Fields: 0.35, Semantic: 0.2, Grammar: 0.1, Timing: 0.1, Dialogue: 0.25},
		{Fields: 1, Semantic: 1, Grammar: 1},
		{Fields: 0.3, Timing: 0.7},
	}
	for _, voice := range []bool{false, true} {
		for _, w := range inputs {
			once := RoundWeights(EffectiveWeights(w, voice, 0.25))
			twice := RoundWeights(EffectiveWeights(once, voice, 0.25))
			if !weightsApprox(once, twice, 1e-4) {
				t.Errorf("voice=%v w=%+v: once %+v, twice %+v", voice, w, once, twice)
			}
		}
	}
}

func TestRoundWeights(t *testing.T) {
	t.Parallel()
	got := RoundWeights(settings.Weights{Fields: 1.0 / 3, Semantic: 1.0 / 6, Grammar: 0.35000000000000003, Timing: 0.1234567, Dialogue: math.NaN()})
	want := settings.Weights{Fields: 0.3333, Semantic: 0.1667, Grammar: 0.35, Timing: 0.1235}
	if got != want {
		t.Fatalf("RoundWeights = %+v, want %+v", got, want)
	}
}

func TestPublicWeights(t *testing.T) {
	t.Parallel()
	got := PublicWeights(settings.Weights{Fields: 0.375, Semantic: 0.1875, Grammar: 0.075, Timing: 0.1125})
	want := map[string]float32{
		core.LayerFields: 0.375, core.LayerSemantic: 0.1875, core.LayerGrammar: 0.075,
		core.LayerTiming: 0.1125, core.LayerDialogue: 0,
	}
	if len(got) != 5 {
		t.Fatalf("PublicWeights has %d keys, want all 5 layers: %v", len(got), got)
	}
	for k, v := range want {
		gv, ok := got[k]
		if !ok || gv != v {
			t.Errorf("PublicWeights[%q] = %v (present %v), want %v", k, gv, ok, v)
		}
	}
	// JSON: dialogue присутствует даже с нулём (UI подписывает плитки по ключам).
	if js := mustJSON(t, got); js != `{"dialogue":0,"fields":0.375,"grammar":0.075,"semantic":0.1875,"timing":0.1125}` {
		t.Errorf("JSON = %s", js)
	}
}

func TestWeightsFromMap(t *testing.T) {
	t.Parallel()
	got := WeightsFromMap(map[string]float64{"fields": 0.5, "timing": 0.2, "dialogue": 0.3, "unknown": 9})
	want := settings.Weights{Fields: 0.5, Timing: 0.2, Dialogue: 0.3}
	if got != want {
		t.Fatalf("WeightsFromMap = %+v, want %+v", got, want)
	}
	if got := WeightsFromMap(nil); got != (settings.Weights{}) {
		t.Fatalf("WeightsFromMap(nil) = %+v", got)
	}
}

func TestTotal(t *testing.T) {
	t.Parallel()
	def := settings.Defaults().ScoreWeights
	cases := []struct {
		name string
		s    LayerScores
		w    settings.Weights
		want float64
	}{
		{"no layers", LayerScores{}, def, 0},
		{"partial: only fields", LayerScores{Fields: ptr(80.0)}, def, 80},
		// (80·0.5 + 100·0.15) / 0.65
		{"partial: fields + timing", LayerScores{Fields: ptr(80.0), Timing: ptr(100.0)}, def, 84.62},
		{"all text layers", LayerScores{Fields: ptr(100.0), Semantic: ptr(50.0), Grammar: ptr(80.0), Timing: ptr(100.0)}, def, 85.5},
		{"score above 100 is clamped", LayerScores{Fields: ptr(150.0)}, def, 100},
		{"negative score is clamped", LayerScores{Fields: ptr(-20.0), Timing: ptr(100.0)}, def, 23.08},
		{"NaN score is skipped", LayerScores{Fields: ptr(math.NaN()), Timing: ptr(60.0)}, def, 60},
		{"zero-weight layer does not count", LayerScores{Fields: ptr(90.0), Dialogue: ptr(0.0)}, def, 90},
		{"only zero-weight layers → 0", LayerScores{Dialogue: ptr(40.0)}, def, 0},
		{"Inf weight is ignored", LayerScores{Fields: ptr(10.0), Timing: ptr(90.0)},
			settings.Weights{Fields: math.Inf(1), Timing: 1}, 90},
		{"NaN weight is ignored", LayerScores{Fields: ptr(10.0), Timing: ptr(90.0)},
			settings.Weights{Fields: math.NaN(), Timing: 1}, 90},
		{"negative weight is ignored", LayerScores{Fields: ptr(10.0), Timing: ptr(90.0)},
			settings.Weights{Fields: -1, Timing: 1}, 90},
		{"voice lesson with dialogue",
			// 37.5 + 15 + 6.75 + 11.25 + 12.5
			LayerScores{Fields: ptr(100.0), Semantic: ptr(80.0), Grammar: ptr(90.0), Timing: ptr(100.0), Dialogue: ptr(50.0)},
			settings.Weights{Fields: 0.375, Semantic: 0.1875, Grammar: 0.075, Timing: 0.1125, Dialogue: 0.25}, 83},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := TotalOf(c.s, c.w); got != c.want {
				t.Errorf("TotalOf = %v, want %v", got, c.want)
			}
			m := map[string]*float64{
				core.LayerFields: c.s.Fields, core.LayerSemantic: c.s.Semantic, core.LayerGrammar: c.s.Grammar,
				core.LayerTiming: c.s.Timing, core.LayerDialogue: c.s.Dialogue,
			}
			if got := Total(m, c.w); got != c.want {
				t.Errorf("Total(map) = %v, want %v", got, c.want)
			}
		})
	}
	if got := Total(nil, def); got != 0 {
		t.Errorf("Total(nil) = %v", got)
	}
}

func TestXPEntries(t *testing.T) {
	t.Parallel()
	rules := settings.Defaults().XPRules // 10 / 5 / 30 / 1
	cases := []struct {
		name  string
		in    XPInput
		r     settings.XPRules
		want  []XPEntry
		total int
	}{
		{"passed within norm", XPInput{Final: 87.5, Passed: true, WithinNorm: true}, rules,
			[]XPEntry{{XPReasonAttemptEvaluated, 10}, {XPReasonWithinNorm, 5}, {XPReasonPassBonus, 8}}, 23},
		{"failed, over norm", XPInput{Final: 42, Passed: false, WithinNorm: false}, rules,
			[]XPEntry{{XPReasonAttemptEvaluated, 10}}, 10},
		{"failed within norm: no pass bonus", XPInput{Final: 95, Passed: false, WithinNorm: true}, rules,
			[]XPEntry{{XPReasonAttemptEvaluated, 10}, {XPReasonWithinNorm, 5}}, 15},
		{"perfect", XPInput{Final: 100, Passed: true}, rules,
			[]XPEntry{{XPReasonAttemptEvaluated, 10}, {XPReasonPassBonus, 10}}, 20},
		{"bonus rounds to hundredths first", XPInput{Final: 69.999, Passed: true}, rules,
			[]XPEntry{{XPReasonAttemptEvaluated, 10}, {XPReasonPassBonus, 7}}, 17},
		{"below 10 points: no +0 bonus row", XPInput{Final: 9.99, Passed: true}, rules,
			[]XPEntry{{XPReasonAttemptEvaluated, 10}}, 10},
		{"final above 100 is clamped", XPInput{Final: 150, Passed: true}, rules,
			[]XPEntry{{XPReasonAttemptEvaluated, 10}, {XPReasonPassBonus, 10}}, 20},
		{"NaN final gives no bonus", XPInput{Final: math.NaN(), Passed: true}, rules,
			[]XPEntry{{XPReasonAttemptEvaluated, 10}}, 10},
		{"bonus multiplier", XPInput{Final: 75, Passed: true},
			settings.XPRules{PassBonusPer10Points: 3}, []XPEntry{{XPReasonPassBonus, 21}}, 21},
		{"zero rules → nothing", XPInput{Final: 90, Passed: true, WithinNorm: true}, settings.XPRules{}, []XPEntry{}, 0},
		{"negative rules are not written", XPInput{Final: 90, Passed: true, WithinNorm: true},
			settings.XPRules{AttemptEvaluated: -5, WithinNorm: -1, PassBonusPer10Points: -2}, []XPEntry{}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := XPEntries(c.in, c.r)
			if got == nil {
				t.Fatal("XPEntries returned nil")
			}
			if len(got) != len(c.want) {
				t.Fatalf("XPEntries = %+v, want %+v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("XPEntries[%d] = %+v, want %+v", i, got[i], c.want[i])
				}
			}
			if tot := XPTotal(got); tot != c.total {
				t.Fatalf("XPTotal = %d, want %d", tot, c.total)
			}
		})
	}
	if XPTotal(nil) != 0 {
		t.Error("XPTotal(nil) != 0")
	}
}

// Причины XP — ровно значения CHECK xp_ledger.reason (00002).
func TestXPReasonsMatchSchema(t *testing.T) {
	t.Parallel()
	for _, r := range []string{XPReasonAttemptEvaluated, XPReasonWithinNorm, XPReasonPassBonus, XPReasonLessonCompleted} {
		switch r {
		case "attempt_evaluated", "within_norm", "pass_bonus", "lesson_completed":
		default:
			t.Errorf("unexpected reason %q", r)
		}
	}
}
