package evaluation

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/callbacks"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/scoring"
	"lct/gocore/internal/settings"
)

// ---------------------------------------------------------------- уверенность модели

// Регрессия: уверенность, равная порогу, — не «ниже порога» (контракт: ревью при confidence <
// порога). float32(0.7) = 0.699999988 < float64(0.7) давал ложное «требует ревью».
func TestLowConfidence(t *testing.T) {
	t.Parallel()
	nan := float32(math.NaN())
	cases := []struct {
		name string
		c    float32
		thr  float64
		want bool
	}{
		{"ровно на пороге 0.7", 0.7, 0.7, false},
		{"ровно на пороге 0.9", 0.9, 0.9, false},
		{"ровно на пороге 0.6", 0.6, 0.6, false},
		{"чуть ниже порога", 0.69, 0.7, true},
		{"сильно ниже", 0.2, 0.7, true},
		{"выше порога", 0.71, 0.7, false},
		{"единица", 1, 0.7, false},
		{"ноль", 0, 0.7, true},
		{"нулевой порог", 0, 0, false},
		{"NaN — доверять нечему", nan, 0.7, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lowConfidence(tc.c, tc.thr); got != tc.want {
				t.Fatalf("lowConfidence(%v, %v) = %v, want %v", tc.c, tc.thr, got, tc.want)
			}
		})
	}
}

// Регрессия на пути «JSON callback'а → float32 → сравнение»: 0.7 и 0.4+0.3 из Python.
func TestLowConfidenceFromCallbackJSON(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"0.7", "0.7000000000000001", "0.70"} {
		var sm components.SemanticResult
		if err := json.Unmarshal([]byte(`{"score": 80, "confidence": `+raw+`}`), &sm); err != nil {
			t.Fatal(err)
		}
		if lowConfidence(sm.Confidence, settings.Defaults().ConfidenceThreshold) {
			t.Errorf("confidence %s at default threshold flagged as low", raw)
		}
		r := newTestRow()
		r.Layers[core.LayerSemantic] = core.LayerQueued
		if p := r.applyAI(core.LayerSemantic, &callbacks.AiResult{Semantic: &sm}, 0.7); p != "" {
			t.Fatalf("applyAI: %s", p)
		}
		if r.NeedsReview {
			t.Errorf("confidence %s: NeedsReview = true, want false", raw)
		}
	}
}

// ---------------------------------------------------------------- баллы

func TestRound0(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want float64 }{
		{0, 0}, {59.5, 60}, {59.49, 59}, {100, 100}, {-0.4, 0}, {math.Copysign(0, -1), 0},
		{math.NaN(), 0}, {math.Inf(1), 0}, {math.Inf(-1), 0}, {84.615, 85},
	}
	for _, tc := range cases {
		got := round0(tc.in)
		if got != tc.want || math.Signbit(got) {
			t.Errorf("round0(%v) = %v, want %v (без -0)", tc.in, got, tc.want)
		}
	}
}

func TestScore100(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   float32
		want float64
		ok   bool
	}{
		{float32(math.NaN()), 0, false},
		{float32(math.Inf(1)), 0, false},
		{float32(math.Inf(-1)), 0, false},
		{-5, 0, true},
		{150, 100, true},
		{79.5, 80, true},
		{42.4, 42, true},
		{0, 0, true},
	}
	for _, tc := range cases {
		got, ok := score100(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("score100(%v) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPassBonus(t *testing.T) {
	t.Parallel()
	rules := settings.Defaults().XPRules // 1 XP за каждые 10 баллов при pass
	cases := []struct {
		name  string
		in    scoring.XPInput
		rules settings.XPRules
		want  int
	}{
		{"pass 95", scoring.XPInput{Final: 95, Passed: true}, rules, 9},
		{"pass 100", scoring.XPInput{Final: 100, Passed: true}, rules, 10},
		{"pass 59.5 (порог ниже)", scoring.XPInput{Final: 59.5, Passed: true}, rules, 5},
		{"fail 95", scoring.XPInput{Final: 95}, rules, 0},
		{"pass 9.99", scoring.XPInput{Final: 9.99, Passed: true}, rules, 0},
		{"правило выключено", scoring.XPInput{Final: 95, Passed: true}, settings.XPRules{AttemptEvaluated: 10}, 0},
	}
	for _, tc := range cases {
		if got := passBonus(tc.in, tc.rules); got != tc.want {
			t.Errorf("%s: passBonus = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------- engine: camelCase на проводе

func TestSnakeToCamelKey(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"duration_ms":        "durationMs",
		"rules_version":      "rulesVersion",
		"llm_model":          "llmModel",
		"tokens_in":          "tokensIn",
		"queue_wait_ms":      "queueWaitMs",
		"grammar":            "grammar",
		"alreadyCamel":       "alreadyCamel",
		"_leading":           "leading",
		"double__underscore": "doubleUnderscore",
		"trailing_":          "trailing",
		"a_b_c":              "aBC",
		"":                   "",
		"ключ_значение":      "ключЗначение",
	}
	for in, want := range cases {
		if got := snakeToCamelKey(in); got != want {
			t.Errorf("snakeToCamelKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// Регрессия: Evaluation.engine отдавался как в БД (snake_case) — провод API camelCase.
func TestPublicEngine(t *testing.T) {
	t.Parallel()
	in := map[string]any{
		"rules_version": rulesVersion,
		core.LayerGrammar: components.Engine{
			DurationMs: 120, LtVersion: ptr("6.4"), QueueWaitMs: ptr(5),
		},
		core.LayerSemantic: map[string]any{"source": "go-core", "reason": "no_free_text"},
		"errors":           map[string]any{core.LayerDialogue: "dispatch_failed"},
		"list":             []any{map[string]any{"inner_key": 1}},
	}
	out, err := publicEngine(in)
	if err != nil {
		t.Fatal(err)
	}
	walkKeys(out, func(k string) {
		if strings.Contains(k, "_") {
			t.Errorf("snake_case key %q leaked to API: %v", k, out)
		}
	})
	if out["rulesVersion"] != rulesVersion {
		t.Errorf("rulesVersion = %v", out["rulesVersion"])
	}
	g, _ := out[core.LayerGrammar].(map[string]any)
	if g["durationMs"] != float64(120) || g["ltVersion"] != "6.4" || g["queueWaitMs"] != float64(5) {
		t.Errorf("grammar engine = %v", g)
	}
	if sem, _ := out[core.LayerSemantic].(map[string]any); sem["reason"] != "no_free_text" {
		t.Errorf("values must stay as is: %v", sem)
	}
	if errs, _ := out["errors"].(map[string]any); errs[core.LayerDialogue] != "dispatch_failed" {
		t.Errorf("errors = %v", errs)
	}
	if _, ok := in["rules_version"]; !ok {
		t.Error("input map must not be modified")
	}
}

// ---------------------------------------------------------------- строка оценки

func newTestRow() *evalRow {
	return &evalRow{
		AttemptID: uuid.New(),
		LessonID:  uuid.New(),
		UserID:    uuid.New(),
		CreatedBy: uuid.New(),
		Settings:  model.DefaultLessonSettings(nil),
		Exists:    true,
		ID:        uuid.New(),
		Status:    core.EvalPartial,
		Verdict:   core.VerdictPending,
		Layers:    model.Layers{},
		Engine:    map[string]any{},
		Weights:   settings.Weights{Fields: 0.5, Semantic: 0.25, Grammar: 0.1, Timing: 0.15},
	}
}

func TestRecompute(t *testing.T) {
	t.Parallel()
	r := newTestRow()
	r.Settings.PassThreshold = 70
	r.FieldsScore, r.TimingScore = ptr(80.0), ptr(100.0)
	r.Layers = model.Layers{core.LayerFields: core.LayerDone, core.LayerTiming: core.LayerDone,
		core.LayerGrammar: core.LayerQueued, core.LayerSemantic: core.LayerQueued, core.LayerDialogue: core.LayerSkipped}

	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	if r.recompute(now) {
		t.Fatal("finalized with queued layers")
	}
	// (80·0.5 + 100·0.15) / 0.65 = 84.6 → 85 — частичный итог по доступным слоям
	if r.TotalScore != 85 || r.Status != core.EvalPartial || r.Verdict != core.VerdictPending || r.EvaluatedAt != nil {
		t.Fatalf("partial: total=%v status=%s verdict=%s at=%v", r.TotalScore, r.Status, r.Verdict, r.EvaluatedAt)
	}

	r.GrammarScore, r.SemanticScore = ptr(50.0), ptr(60.0)
	r.Layers[core.LayerGrammar], r.Layers[core.LayerSemantic] = core.LayerDone, core.LayerDone
	if !r.recompute(now) {
		t.Fatal("not finalized with all layers terminal")
	}
	// 40 + 15 + 5 + 15 = 75 → pass при пороге 70
	if r.TotalScore != 75 || r.Status != core.EvalDone || r.Verdict != core.VerdictPass || r.EvaluatedAt == nil || !r.EvaluatedAt.Equal(now) {
		t.Fatalf("done: total=%v status=%s verdict=%s at=%v", r.TotalScore, r.Status, r.Verdict, r.EvaluatedAt)
	}
	if r.recompute(now.Add(time.Minute)) {
		t.Fatal("second recompute of a done evaluation must not finalize again")
	}
	if !r.EvaluatedAt.Equal(now) {
		t.Fatal("evaluated_at changed on recompute")
	}

	// failed тоже терминален: итог по оставшимся слоям, вердикт по порогу
	f := newTestRow()
	f.Settings.PassThreshold = 90
	f.FieldsScore, f.TimingScore = ptr(80.0), ptr(100.0)
	f.Layers = model.Layers{core.LayerFields: core.LayerDone, core.LayerTiming: core.LayerDone, core.LayerGrammar: core.LayerFailed}
	if !f.recompute(now) || f.Verdict != core.VerdictFail || f.TotalScore != 85 {
		t.Fatalf("failed layer: status=%s verdict=%s total=%v", f.Status, f.Verdict, f.TotalScore)
	}
}

func TestMarkFailed(t *testing.T) {
	t.Parallel()
	r := newTestRow()
	r.Layers[core.LayerGrammar] = core.LayerQueued
	r.Layers[core.LayerSemantic] = core.LayerQueued
	r.markFailed(core.LayerGrammar, "timeout")
	r.markFailed(core.LayerSemantic, "")
	if r.Layers[core.LayerGrammar] != core.LayerFailed || r.Layers[core.LayerSemantic] != core.LayerFailed {
		t.Fatalf("layers = %v", r.Layers)
	}
	if !r.AIUnavailable || !r.NeedsReview {
		t.Fatal("failed AI layer must set aiUnavailable and needsReview")
	}
	errs, _ := r.Engine["errors"].(map[string]any)
	if errs[core.LayerGrammar] != "timeout" || errs[core.LayerSemantic] != "failed" {
		t.Fatalf("engine.errors = %v", errs)
	}
}

func TestFinalScoreVerdictAndXPInput(t *testing.T) {
	t.Parallel()
	r := newTestRow()
	r.TotalScore, r.Verdict = 33, core.VerdictFail
	r.Timing.WithinNorm = true
	if in := r.xpInput(); in.Final != 33 || in.Passed || !in.WithinNorm {
		t.Fatalf("no override: %+v", in)
	}
	r.OverrideScore, r.OverrideVerdict = ptr(95.0), ptr(core.VerdictPass)
	if r.finalScore() != 95 || r.verdictShown() != core.VerdictPass {
		t.Fatalf("override: final=%v verdict=%s", r.finalScore(), r.verdictShown())
	}
	if in := r.xpInput(); in.Final != 95 || !in.Passed {
		t.Fatalf("xpInput must use the override: %+v", in)
	}
	r.OverrideVerdict = ptr("")
	if r.verdictShown() != core.VerdictFail {
		t.Fatal("empty override verdict must fall back to the automatic one")
	}
}

func TestLayerTerminal(t *testing.T) {
	t.Parallel()
	for st, want := range map[string]bool{
		core.LayerDone: true, core.LayerFailed: true, core.LayerSkipped: true,
		core.LayerQueued: false, core.LayerRunning: false, "": false,
	} {
		if layerTerminal(st) != want {
			t.Errorf("layerTerminal(%q) != %v", st, want)
		}
	}
	r := newTestRow()
	if !r.allTerminal() {
		t.Error("no layers — nothing to wait for")
	}
}

// ---------------------------------------------------------------- применение результата AI

func TestApplyAI(t *testing.T) {
	t.Parallel()
	eng := components.Engine{DurationMs: 42, LlmModel: ptr("qwen2.5:3b")}
	cases := []struct {
		name    string
		layer   string
		res     *callbacks.AiResult
		problem bool
		check   func(t *testing.T, r *evalRow)
	}{
		{name: "grammar без результата", layer: core.LayerGrammar, res: &callbacks.AiResult{}, problem: true},
		{name: "grammar NaN", layer: core.LayerGrammar,
			res: &callbacks.AiResult{Grammar: &components.GrammarResult{Score: float32(math.NaN())}}, problem: true},
		{name: "grammar без замечаний — [] а не null", layer: core.LayerGrammar,
			res: func() *callbacks.AiResult {
				g := &components.GrammarResult{Score: 97.6}
				g.Stats.WordsChecked = 12
				g.Stats.ErrorsBySeverity = &map[string]int{"minor": 1}
				return &callbacks.AiResult{Grammar: g, Engine: eng}
			}(),
			check: func(t *testing.T, r *evalRow) {
				if deref(r.GrammarScore) != 98 || string(r.GrammarRemarks) != "[]" {
					t.Fatalf("score=%v remarks=%s", deref(r.GrammarScore), r.GrammarRemarks)
				}
				var st model.GrammarStats
				if err := json.Unmarshal(r.GrammarStats, &st); err != nil || st.WordsChecked != 12 || st.ErrorsBySeverity["minor"] != 1 {
					t.Fatalf("stats = %s (%v)", r.GrammarStats, err)
				}
				if r.Layers[core.LayerGrammar] != core.LayerDone {
					t.Fatalf("layer = %s", r.Layers[core.LayerGrammar])
				}
				if e, ok := r.Engine[core.LayerGrammar].(components.Engine); !ok || e.DurationMs != 42 {
					t.Fatalf("engine = %#v", r.Engine[core.LayerGrammar])
				}
			}},
		{name: "semantic без результата", layer: core.LayerSemantic, res: &callbacks.AiResult{}, problem: true},
		{name: "semantic Inf", layer: core.LayerSemantic,
			res: &callbacks.AiResult{Semantic: &components.SemanticResult{Score: float32(math.Inf(1)), Confidence: 1}}, problem: true},
		{name: "semantic низкая уверенность → ревью", layer: core.LayerSemantic,
			res: &callbacks.AiResult{Semantic: &components.SemanticResult{Score: 120, Confidence: 0.5}},
			check: func(t *testing.T, r *evalRow) {
				if !r.NeedsReview || deref(r.SemanticScore) != 100 {
					t.Fatalf("needsReview=%v score=%v", r.NeedsReview, deref(r.SemanticScore))
				}
			}},
		{name: "semantic уверенно", layer: core.LayerSemantic,
			res: &callbacks.AiResult{Semantic: &components.SemanticResult{Score: 64.4, Confidence: 0.95,
				SummaryForStudent: ptr("Адрес указан, этаж — нет.")}},
			check: func(t *testing.T, r *evalRow) {
				if r.NeedsReview || deref(r.SemanticScore) != 64 {
					t.Fatalf("needsReview=%v score=%v", r.NeedsReview, deref(r.SemanticScore))
				}
				if !strings.Contains(string(r.Semantic), "этаж") {
					t.Fatalf("semantic raw = %s", r.Semantic)
				}
			}},
		{name: "dialogue без результата", layer: core.LayerDialogue, res: &callbacks.AiResult{}, problem: true},
		{name: "dialogue низкая уверенность", layer: core.LayerDialogue,
			res: &callbacks.AiResult{Dialogue: &components.DialogueResult{Score: 70, Confidence: 0.3, Checklist: []components.DialogueChecklistResult{}}},
			check: func(t *testing.T, r *evalRow) {
				if !r.NeedsReview || deref(r.DialogueScore) != 70 || r.Layers[core.LayerDialogue] != core.LayerDone {
					t.Fatalf("needsReview=%v score=%v layer=%s", r.NeedsReview, deref(r.DialogueScore), r.Layers[core.LayerDialogue])
				}
			}},
		{name: "неизвестный слой", layer: "tone", res: &callbacks.AiResult{}, problem: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newTestRow()
			r.Layers[tc.layer] = core.LayerQueued
			p := r.applyAI(tc.layer, tc.res, 0.7)
			if (p != "") != tc.problem {
				t.Fatalf("problem = %q, want problem=%v", p, tc.problem)
			}
			if tc.problem && r.Layers[tc.layer] != core.LayerQueued {
				t.Fatalf("unusable result must not mark the layer done: %v", r.Layers)
			}
			if tc.check != nil {
				tc.check(t, r)
			}
		})
	}
}

func TestJobTarget(t *testing.T) {
	t.Parallel()
	s := New(Deps{Log: discardLog()})
	att := uuid.New()
	if l, id, ok := s.jobTarget(core.JobRecord{Type: core.JobEvaluateSemantic, RefType: core.RefAttempt, RefID: att}, nil); !ok || l != core.LayerSemantic || id != att {
		t.Fatalf("attempt ref: %s %s %v", l, id, ok)
	}
	// ссылки нет — attempt_id из результата
	if l, id, ok := s.jobTarget(core.JobRecord{Type: core.JobEvaluateGrammar}, &callbacks.AiResult{AttemptId: &att}); !ok || l != core.LayerGrammar || id != att {
		t.Fatalf("attempt from result: %s %s %v", l, id, ok)
	}
	if _, _, ok := s.jobTarget(core.JobRecord{Type: core.JobTTS, RefType: core.RefAttempt, RefID: att}, nil); ok {
		t.Fatal("non-evaluation job must be ignored")
	}
	if _, _, ok := s.jobTarget(core.JobRecord{Type: core.JobEvaluateGrammar}, &callbacks.AiResult{}); ok {
		t.Fatal("job without target must be ignored")
	}
}

// ---------------------------------------------------------------- представление

func TestBuildViewContractShape(t *testing.T) {
	t.Parallel()
	s := New(Deps{Log: discardLog()})
	r := newTestRow()
	r.Verdict = "" // старая строка без вердикта
	ev := s.buildView(r)
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	requireKeys(t, m, "attemptId", "status", "totalScore", "verdict", "fieldErrors", "timing", "needsReview",
		"aiUnavailable", "recommendations", "finalScore", "weights")
	requireArray(t, m, "fieldErrors")
	requireArray(t, m, "recommendations")
	requireKeys(t, m["timing"].(map[string]any), "spentMs", "limitSec", "deltaMs", "withinNorm", "reactionMs")
	requireKeys(t, m["weights"].(map[string]any), "fields", "semantic", "grammar", "timing", "dialogue")
	if m["verdict"] != "pending" {
		t.Errorf("verdict = %v, want pending", m["verdict"])
	}
	for _, k := range []string{"xpEarned", "override", "grammar", "semantic", "dialogue", "engine", "layers"} {
		if _, ok := m[k]; ok {
			t.Errorf("%q must be absent for an empty partial evaluation: %s", k, raw)
		}
	}
}

func TestBuildViewLayersAndRunning(t *testing.T) {
	t.Parallel()
	s := New(Deps{Log: discardLog()})
	r := newTestRow()
	r.Layers = model.Layers{core.LayerFields: core.LayerDone, core.LayerGrammar: core.LayerQueued,
		core.LayerSemantic: core.LayerQueued, core.LayerDialogue: core.LayerSkipped}
	r.Running = []string{string(core.JobEvaluateGrammar), string(core.JobGenerateScenario)}
	ev := s.buildView(r)
	if ev.Layers == nil {
		t.Fatal("layers missing")
	}
	got := *ev.Layers
	if got[core.LayerGrammar] != "running" || got[core.LayerSemantic] != "queued" || got[core.LayerFields] != "done" ||
		got[core.LayerDialogue] != "skipped" {
		t.Fatalf("layers = %v", got)
	}
}

func TestBuildViewOverride(t *testing.T) {
	t.Parallel()
	s := New(Deps{Log: discardLog()})
	r := newTestRow()
	r.Status, r.TotalScore, r.Verdict = core.EvalDone, 75, core.VerdictPass
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	r.OverrideScore, r.OverrideVerdict, r.OverrideReason = ptr(59.5), ptr(core.VerdictFail), ptr("Адрес указан неверно")
	r.OverriddenAt, r.OverriderName = &at, "Учителев П. П."
	r.XPEarned = 15
	ev := s.buildView(r)
	if ev.Override == nil || ev.Override.Score != 59.5 || ev.Override.Verdict != public.VerdictFail ||
		ev.Override.Reason != "Адрес указан неверно" || ev.Override.By != "Учителев П. П." || !ev.Override.At.Equal(at) {
		t.Fatalf("override = %+v", ev.Override)
	}
	if ev.Verdict != public.VerdictFail || ev.FinalScore != 59.5 || ev.TotalScore != 75 {
		t.Fatalf("verdict=%s final=%v total=%v", ev.Verdict, ev.FinalScore, ev.TotalScore)
	}
	if ev.XpEarned == nil || *ev.XpEarned != 15 {
		t.Fatalf("xpEarned = %v", ev.XpEarned)
	}
}

// Регрессия: perField семантики — пути формы АРМ (как у замечаний грамматики), не контрактной карточки.
func TestBuildViewSemanticPerFieldPaths(t *testing.T) {
	t.Parallel()
	s := New(Deps{Log: discardLog()})
	r := newTestRow()
	r.SemanticScore = ptr(64.0)
	r.Semantic = json.RawMessage(`{"score": 64.4, "confidence": 0.9, "missing_facts": ["этаж"],
		"per_field": [{"field": "actions_taken", "score": 50, "comment": "нет службы"},
		              {"field": "description", "score": 80},
		              {"field": "action_text", "score": 40}]}`)
	ev := s.buildView(r)
	if ev.Semantic == nil || ev.Semantic.PerField == nil {
		t.Fatalf("semantic = %+v", ev.Semantic)
	}
	var fields []string
	for _, f := range *ev.Semantic.PerField {
		fields = append(fields, f.Field)
	}
	if strings.Join(fields, ",") != "actionsTaken,description,actionText" {
		t.Fatalf("perField paths = %v", fields)
	}
	if ev.Semantic.Score != 64 {
		t.Fatalf("score must come from the column: %v", ev.Semantic.Score)
	}
	if ev.Semantic.MissingFacts == nil || (*ev.Semantic.MissingFacts)[0] != "этаж" {
		t.Fatalf("missingFacts = %v", ev.Semantic.MissingFacts)
	}
}

func TestBuildViewEngineCamelCase(t *testing.T) {
	t.Parallel()
	s := New(Deps{Log: discardLog()})
	r := newTestRow()
	r.Engine = map[string]any{
		"rules_version":    rulesVersion,
		core.LayerSemantic: components.Engine{DurationMs: 900, PromptVersion: ptr("sem-v3"), TokensIn: ptr(1200)},
	}
	ev := s.buildView(r)
	if ev.Engine == nil {
		t.Fatal("engine missing")
	}
	raw, _ := json.Marshal(ev.Engine)
	for _, want := range []string{`"rulesVersion":"go-scoring-1"`, `"promptVersion":"sem-v3"`, `"durationMs":900`, `"tokensIn":1200`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("engine %s lacks %s", raw, want)
		}
	}
	if strings.Contains(string(raw), "_") {
		t.Errorf("snake_case in engine: %s", raw)
	}
}

func TestBuildViewBrokenJSONDoesNotBreakResponse(t *testing.T) {
	t.Parallel()
	s := New(Deps{Log: discardLog()})
	r := newTestRow()
	r.FieldErrors = json.RawMessage(`{"not":"an array"}`)
	r.GrammarScore = ptr(90.0)
	r.GrammarRemarks = json.RawMessage(`[{"offset":"x"}]`)
	r.SemanticScore = ptr(50.0)
	r.Semantic = json.RawMessage(`{"score": "nope"`)
	r.DialogueScore = ptr(50.0)
	r.Dialogue = json.RawMessage(`[]`) // пустое значение колонки — слоя нет
	ev := s.buildView(r)
	if ev.FieldErrors == nil || len(ev.FieldErrors) != 0 {
		t.Fatalf("fieldErrors = %v, want []", ev.FieldErrors)
	}
	if ev.Grammar != nil || ev.Semantic != nil || ev.Dialogue != nil {
		t.Fatalf("broken layers must be hidden: %+v %+v %+v", ev.Grammar, ev.Semantic, ev.Dialogue)
	}
	if ev.GrammarScore == nil || *ev.GrammarScore != 90 {
		t.Fatal("the tile score is still shown")
	}
}

func TestBuildViewDoneWithAllLayers(t *testing.T) {
	t.Parallel()
	s := New(Deps{Log: discardLog()})
	r := newTestRow()
	r.Status, r.TotalScore, r.Verdict = core.EvalDone, 55, core.VerdictFail
	r.CategoryName = "Пожар"
	r.XPEarned = 10
	r.fieldErrs = []public.FieldError{{Field: "address.raw", Kind: "missing", Label: "Адрес", Weight: 1}}
	r.GrammarScore = ptr(80.0)
	r.GrammarRemarks = json.RawMessage(`[{"field":"description","offset":0,"length":5,"message":"Опечатка","severity":"minor"}]`)
	r.GrammarStats = json.RawMessage(`{"words_checked": 7}`)
	r.DialogueScore = ptr(40.0)
	r.ExpectedDialogue = &model.ExpectedDialogue{Checklist: []model.ChecklistItem{
		{ID: "q1", Text: "Уточнить этаж", Kind: "question", Required: true},
		{ID: "q2", Text: "Назвать своё имя", Kind: "phrase", Required: false},
	}}
	r.Dialogue = json.RawMessage(`{"score": 40, "confidence": 0.9, "speech": {"filler_count": 3, "fillers": {"ну": 3}},
		"checklist": [{"id": "q1", "status": "missed"}, {"id": "q2", "status": "missed"}]}`)
	ev := s.buildView(r)
	if ev.XpEarned == nil || *ev.XpEarned != 10 {
		t.Fatalf("xpEarned = %v", ev.XpEarned)
	}
	if ev.Grammar == nil || len(ev.Grammar.Remarks) != 1 || ev.Grammar.Stats.WordsChecked != 7 {
		t.Fatalf("grammar = %+v", ev.Grammar)
	}
	if ev.Dialogue == nil || len(ev.Dialogue.Checklist) != 2 || ev.Dialogue.Checklist[0].Text != "Уточнить этаж" {
		t.Fatalf("dialogue = %+v", ev.Dialogue)
	}
	if len(ev.FieldErrors) != 1 {
		t.Fatalf("fieldErrors = %+v", ev.FieldErrors)
	}
	if len(ev.Recommendations) == 0 {
		t.Fatal("done evaluation with errors must have recommendations")
	}
}

func TestDialogueMissing(t *testing.T) {
	t.Parallel()
	exp := &model.ExpectedDialogue{Checklist: []model.ChecklistItem{
		{ID: "q1", Text: "Уточнить этаж", Required: true},
		{ID: "q2", Text: "Спросить про пострадавших", Required: true},
		{ID: "q3", Text: "Представиться", Required: false},
	}}
	d := &components.DialogueResult{Checklist: []components.DialogueChecklistResult{
		{Id: "q1", Status: "missed"}, {Id: "q2", Status: "done"}, {Id: "q3", Status: "missed"}, {Id: "zz", Status: "missed"},
	}}
	if got := dialogueMissing(d, exp); len(got) != 1 || got[0] != "Уточнить этаж" {
		t.Fatalf("from checklist: %v", got)
	}
	d.MissingQuestions = &[]string{"не уточнил подъезд"}
	if got := dialogueMissing(d, exp); len(got) != 1 || got[0] != "не уточнил подъезд" {
		t.Fatalf("missing_questions preferred: %v", got)
	}
	if got := dialogueMissing(&components.DialogueResult{Checklist: []components.DialogueChecklistResult{{Id: "q1", Status: "missed"}}}, nil); len(got) != 0 {
		t.Fatalf("no etalon: %v", got)
	}
}

// ---------------------------------------------------------------- входы слоёв и payload'ы

func testStart(mode string) *startRow {
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	return &startRow{
		AttemptID: uuid.New(), LessonID: uuid.New(), UserID: uuid.New(), ScenarioID: uuid.New(), EtalonID: uuid.New(),
		Mode: mode, Status: core.AttemptSubmitted, TimeLimitSec: 45,
		CallAcceptedAt: ptr(now), FirstInputAt: ptr(now.Add(3 * time.Second)), SubmittedAt: ptr(now.Add(40 * time.Second)),
		CategoryID: uuid.New(), Difficulty: 2, CategoryCode: "ev101", CategoryName: "Пожар",
		CallScript: model.CallScript{KeyFacts: []string{"горит квартира"}},
	}
}

func TestGrammarTexts(t *testing.T) {
	t.Parallel()
	st := testStart(core.ModeCards)
	card := &public.IncidentCardDraft{Description: "  Горит квартира на пятом этаже ", ActionsTaken: " \n\t "}
	got := grammarTexts(st, card)
	if len(got) != 1 || got[0].Field != "description" || got[0].Text != card.Description {
		t.Fatalf("cards: %+v (текст без trim — смещения замечаний совпадают с формой)", got)
	}
	card.ActionsTaken = "Передал в 101"
	if got := grammarTexts(st, card); len(got) != 2 || got[1].Field != "actionsTaken" {
		t.Fatalf("cards with actions: %+v", got)
	}
	if got := grammarTexts(st, nil); len(got) != 0 {
		t.Fatalf("nil card: %+v", got)
	}

	ca := testStart(core.ModeCardActions)
	ca.ActionText = ptr("Направил пожарный расчёт")
	if got := grammarTexts(ca, card); len(got) != 1 || got[0].Field != "actionText" {
		t.Fatalf("card_actions: %+v (карточка в этом режиме не проверяется)", got)
	}
	ca.ActionText = ptr("   ")
	if got := grammarTexts(ca, card); len(got) != 0 {
		t.Fatalf("blank action text: %+v", got)
	}
}

func TestSemanticJob(t *testing.T) {
	t.Parallel()
	s := New(Deps{Log: discardLog()})
	st := testStart(core.ModeCards)
	sc := s.scenarioContext(st)

	if _, ok := s.semanticJob(st, nil, sc); ok {
		t.Fatal("no card — no LLM job")
	}
	if _, ok := s.semanticJob(st, &public.IncidentCardDraft{Description: "  "}, sc); ok {
		t.Fatal("blank free text — no LLM job")
	}
	job, ok := s.semanticJob(st, &public.IncidentCardDraft{ActionsTaken: "Передал в 101"}, sc)
	if !ok {
		t.Fatal("actionsTaken present — job expected")
	}
	if job.Type != core.JobEvaluateSemantic || job.RefType != core.RefAttempt || job.RefID != st.AttemptID ||
		job.DedupKey != "eval:"+st.AttemptID.String()+":semantic" || job.Priority != core.PrioritySemantic {
		t.Fatalf("job = %+v", job)
	}
	raw, _ := json.Marshal(job.Payload)
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	if p["request_id"] != job.ID.String() || p["attempt_id"] != st.AttemptID.String() || p["mode"] != "cards" {
		t.Fatalf("payload = %s", raw)
	}
	if ff, _ := p["free_text_fields"].([]any); len(ff) != 1 || ff[0] != "actions_taken" {
		t.Fatalf("free_text_fields = %v", p["free_text_fields"])
	}
	// ПДн в ai-service не уходят
	for _, leak := range []string{st.UserID.String(), st.LessonID.String(), "user_id", "lesson_id"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("payload leaks %q: %s", leak, raw)
		}
	}

	ca := testStart(core.ModeCardActions)
	ca.ActionText = ptr("Направил расчёт")
	job, ok = s.semanticJob(ca, nil, s.scenarioContext(ca))
	if !ok {
		t.Fatal("card_actions with text — job expected")
	}
	raw, _ = json.Marshal(job.Payload)
	if !strings.Contains(string(raw), `"mode":"card_actions"`) || !strings.Contains(string(raw), `"action_text":"Направил расчёт"`) {
		t.Fatalf("card_actions payload = %s", raw)
	}
	ca.ActionText = nil
	if _, ok := s.semanticJob(ca, nil, s.scenarioContext(ca)); ok {
		t.Fatal("card_actions without text — no job")
	}
}

func TestSetLocalSemantic(t *testing.T) {
	t.Parallel()
	decode := func(t *testing.T, r *evalRow) components.SemanticResult {
		t.Helper()
		var res components.SemanticResult
		if err := json.Unmarshal(r.Semantic, &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	// факты — из scoring.required_facts
	st := testStart(core.ModeCards)
	st.Scoring.RequiredFacts = []string{"адрес", "этаж"}
	st.ExpectedActions = []model.ExpectedAction{{ActionText: "x", RequiredFacts: []string{"другое"}}}
	r := newTestRow()
	if err := r.setLocalSemantic(st); err != nil {
		t.Fatal(err)
	}
	res := decode(t, r)
	if deref(r.SemanticScore) != 0 || r.Layers[core.LayerSemantic] != core.LayerDone || res.Confidence != 1 ||
		strings.Join(convertSlice(res.MissingFacts), ",") != "адрес,этаж" || deref(res.SummaryForStudent) != summaryNoDescription {
		t.Fatalf("required_facts: score=%v res=%+v", deref(r.SemanticScore), res)
	}
	if e, _ := r.Engine[core.LayerSemantic].(map[string]any); e["source"] != "go-core" {
		t.Fatalf("engine = %v", r.Engine)
	}
	// иначе — из expected_actions
	st.Scoring.RequiredFacts = nil
	r = newTestRow()
	_ = r.setLocalSemantic(st)
	if got := convertSlice(decode(t, r).MissingFacts); strings.Join(got, ",") != "другое" {
		t.Fatalf("expected_actions facts = %v", got)
	}
	// иначе — key_facts легенды; card_actions — своё пояснение
	ca := testStart(core.ModeCardActions)
	r = newTestRow()
	_ = r.setLocalSemantic(ca)
	res = decode(t, r)
	if strings.Join(convertSlice(res.MissingFacts), ",") != "горит квартира" || deref(res.SummaryForStudent) != summaryNoActionText {
		t.Fatalf("key_facts: %+v", res)
	}
	// фактов нет совсем — [] (не null)
	empty := testStart(core.ModeCards)
	empty.CallScript = model.CallScript{}
	r = newTestRow()
	_ = r.setLocalSemantic(empty)
	if !strings.Contains(string(r.Semantic), `"missing_facts":[]`) {
		t.Fatalf("no facts: %s", r.Semantic)
	}
}

// semanticPayloadFacts — источники фактов в payload смысловой задачи.
type semanticPayloadFacts struct {
	Mode       string `json:"mode"`
	CallScript struct {
		KeyFacts []string `json:"key_facts"`
	} `json:"call_script"`
	Etalon struct {
		Scoring *struct {
			RequiredFacts []string `json:"required_facts"`
		} `json:"scoring"`
		ExpectedActions []struct {
			RequiredFacts []string `json:"required_facts"`
		} `json:"expected_actions"`
	} `json:"etalon"`
}

func decodeSemanticPayload(t *testing.T, job core.NewJob) semanticPayloadFacts {
	t.Helper()
	raw, _ := json.Marshal(job.Payload)
	var p semanticPayloadFacts
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("payload %s: %v", raw, err)
	}
	return p
}

func ddsActions() []model.ExpectedAction {
	return []model.ExpectedAction{{
		ActionText:    "Карточка принята в работу, аварийная бригада направлена на место",
		RequiredFacts: []string{"Карточка принята в работу", "Аварийная бригада направлена на место"},
	}}
}

// Ракурс ДДС: факты смыслового слоя — только expected_actions; факты звонка
// (scoring.required_facts, call_script.key_facts) в ai-service не уходят.
func TestSemanticJobDDSUsesOnlyExpectedActions(t *testing.T) {
	t.Parallel()
	s := New(Deps{Log: discardLog()})
	st := testStart(core.ModeCardActions)
	st.ServiceCode = "zhkh"
	st.ActionText = ptr("Карточка принята в работу, бригада направлена")
	st.Scoring.RequiredFacts = []string{"горит квартира"}
	st.ExpectedActions = ddsActions()

	job, ok := s.semanticJob(st, nil, s.scenarioContext(st))
	if !ok {
		t.Fatal("DDS with action text and expected_actions — job expected")
	}
	p := decodeSemanticPayload(t, job)
	if p.Mode != "card_actions" || len(p.Etalon.ExpectedActions) != 1 || len(p.Etalon.ExpectedActions[0].RequiredFacts) != 2 {
		t.Fatalf("payload = %+v", p)
	}
	if len(p.CallScript.KeyFacts) != 0 {
		t.Fatalf("DDS payload leaks call key_facts: %v", p.CallScript.KeyFacts)
	}
	if p.Etalon.Scoring != nil && len(p.Etalon.Scoring.RequiredFacts) != 0 {
		t.Fatalf("DDS payload leaks scoring.required_facts: %v", p.Etalon.Scoring.RequiredFacts)
	}
	// Эталон попытки не меняется — чистится только копия для payload.
	if len(st.Scoring.RequiredFacts) != 1 || len(st.CallScript.KeyFacts) != 1 {
		t.Fatalf("startRow mutated: scoring=%v key_facts=%v", st.Scoring.RequiredFacts, st.CallScript.KeyFacts)
	}
}

// Оператор 112 (card_actions и cards): payload смысловой задачи прежний — факты звонка на месте.
func TestSemanticJob112KeepsCallFacts(t *testing.T) {
	t.Parallel()
	s := New(Deps{Log: discardLog()})
	ca := testStart(core.ModeCardActions)
	ca.ActionText = ptr("Направил расчёт")
	ca.Scoring.RequiredFacts = []string{"горит квартира"}
	ca.ExpectedActions = ddsActions()
	job, ok := s.semanticJob(ca, nil, s.scenarioContext(ca))
	if !ok {
		t.Fatal("112 card_actions — job expected")
	}
	p := decodeSemanticPayload(t, job)
	if strings.Join(p.CallScript.KeyFacts, ",") != "горит квартира" || p.Etalon.Scoring == nil ||
		strings.Join(p.Etalon.Scoring.RequiredFacts, ",") != "горит квартира" || len(p.Etalon.ExpectedActions) != 1 {
		t.Fatalf("112 card_actions payload changed: %+v", p)
	}

	cards := testStart(core.ModeCards)
	cards.Scoring.RequiredFacts = []string{"горит квартира"}
	job, ok = s.semanticJob(cards, &public.IncidentCardDraft{Description: "Горит квартира"}, s.scenarioContext(cards))
	if !ok {
		t.Fatal("112 cards — job expected")
	}
	p = decodeSemanticPayload(t, job)
	if p.Mode != "cards" || strings.Join(p.CallScript.KeyFacts, ",") != "горит квартира" || p.Etalon.Scoring == nil ||
		strings.Join(p.Etalon.Scoring.RequiredFacts, ",") != "горит квартира" {
		t.Fatalf("112 cards payload changed: %+v", p)
	}
}

// Ракурс ДДС без текста действия: локальный 0 — с фактами только из expected_actions.
func TestSetLocalSemanticDDS(t *testing.T) {
	t.Parallel()
	st := testStart(core.ModeCardActions)
	st.ServiceCode = "zhkh"
	st.Scoring.RequiredFacts = []string{"горит квартира"}
	st.ExpectedActions = ddsActions()
	r := newTestRow()
	if err := r.setLocalSemantic(st); err != nil {
		t.Fatal(err)
	}
	var res components.SemanticResult
	if err := json.Unmarshal(r.Semantic, &res); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(convertSlice(res.MissingFacts), ","); got != "Карточка принята в работу,Аварийная бригада направлена на место" {
		t.Fatalf("DDS missing facts = %q (факты звонка не должны попадать)", got)
	}
	if deref(res.SummaryForStudent) != summaryNoActionText || deref(r.SemanticScore) != 0 {
		t.Fatalf("res = %+v", res)
	}
}

// Ракурс ДДС без эталонных действий: слой пропущен, итог — по остальным слоям с перенормировкой.
func TestSkipDDSSemanticRecompute(t *testing.T) {
	t.Parallel()
	r := newTestRow()
	r.skipDDSSemantic()
	if r.Layers[core.LayerSemantic] != core.LayerSkipped || r.SemanticScore != nil || len(r.Semantic) != 0 {
		t.Fatalf("layers=%v score=%v semantic=%s", r.Layers, r.SemanticScore, r.Semantic)
	}
	if e, _ := r.Engine[core.LayerSemantic].(map[string]any); e["reason"] != reasonDDSNoExpectedActions {
		t.Fatalf("engine = %v", r.Engine)
	}
	r.FieldsScore, r.GrammarScore, r.TimingScore = ptr(80.0), ptr(90.0), ptr(100.0)
	r.Layers[core.LayerFields], r.Layers[core.LayerGrammar], r.Layers[core.LayerTiming] = core.LayerDone, core.LayerDone, core.LayerDone
	if !r.recompute(time.Now()) || r.Status != core.EvalDone {
		t.Fatalf("status = %v", r.Status)
	}
	// веса 0.5/0.1/0.15 без смыслового 0.25: (40+9+15)/0.75 = 85.33 → 85
	if r.TotalScore != 85 {
		t.Fatalf("total = %v, want 85 (semantic excluded, weights renormalized)", r.TotalScore)
	}
}

func convertSlice(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}

func TestTimingInputs(t *testing.T) {
	t.Parallel()
	st := testStart(core.ModeCards)
	if got := spentMs(st); got != 40000 {
		t.Fatalf("spent from timestamps = %d", got)
	}
	st.TimeSpentMs = ptr(12345)
	if got := spentMs(st); got != 12345 {
		t.Fatalf("spent from column = %d", got)
	}
	if got := reactionMs(st); got != 3000 {
		t.Fatalf("reaction = %d", got)
	}
	early := st.CallAcceptedAt.Add(-time.Second)
	st.FirstInputAt = &early
	if got := reactionMs(st); got != 0 {
		t.Fatalf("negative reaction must clamp to 0: %d", got)
	}
	st.FirstInputAt = nil
	if reactionMs(st) != 0 || spentMs(&startRow{}) != 0 {
		t.Fatal("missing timestamps → 0")
	}
	if msSince(nil, st.CallAcceptedAt) != nil || msSince(st.SubmittedAt, nil) != nil {
		t.Fatal("msSince without marks must be nil")
	}
	if got := msSince(st.SubmittedAt, st.CallAcceptedAt); got == nil || *got != 40000 {
		t.Fatalf("msSince = %v", got)
	}
}

func TestDecodeDraftAndFieldSpec(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "null", "{}", "[]", "{broken"} {
		if d := decodeDraft(json.RawMessage(raw)); d != nil {
			t.Errorf("decodeDraft(%q) = %+v, want nil", raw, d)
		}
	}
	d := decodeDraft(json.RawMessage(`{"description":"Горит дом","address":{"raw":"Москва"}}`))
	if d == nil || d.Description != "Горит дом" || d.Address.Raw != "Москва" {
		t.Fatalf("decodeDraft = %+v", d)
	}

	st := testStart(core.ModeCards)
	spec := fieldSpec(st, &public.IncidentCardDraft{})
	if strings.Join(spec.Required, ",") != strings.Join(model.DefaultRequiredFields(), ",") {
		t.Fatalf("default required = %v", spec.Required)
	}
	st.Scoring.RequiredFields = []string{"address.raw"}
	st.EtalonCard.ServicesToNotify = &[]string{"101"}
	spec = fieldSpec(st, &public.IncidentCardDraft{})
	if strings.Join(spec.Required, ",") != "address.raw" || strings.Join(spec.ServiceCodes, ",") != "101" {
		t.Fatalf("spec = %+v", spec)
	}
	withServices := &public.IncidentCardDraft{Services: []public.AssignedService{{ServiceId: "s1"}}}
	if spec := fieldSpec(st, withServices); len(spec.ServiceCodes) != 0 {
		t.Fatalf("etalon form has services — contract codes not needed: %+v", spec)
	}
}

func TestSmallHelpers(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]bool{"": true, " null ": true, "{}": true, "[]": true, `{"a":1}`: false, "[1]": false} {
		if isEmptyJSON([]byte(raw)) != want {
			t.Errorf("isEmptyJSON(%q) != %v", raw, want)
		}
	}
	if string(orDefault(nil, jsonEmptyArray)) != "[]" || string(orDefault(json.RawMessage("  "), jsonEmptyObject)) != "{}" ||
		string(orDefault(json.RawMessage(`[1]`), jsonEmptyArray)) != "[1]" {
		t.Error("orDefault")
	}
	if nonBlank(nil) != nil || nonBlank(ptr("  ")) != nil || deref(nonBlank(ptr("адрес"))) != "адрес" {
		t.Error("nonBlank")
	}
	if nullIfEmpty("") != nil || deref(nullIfEmpty("x")) != "x" {
		t.Error("nullIfEmpty")
	}
	id := uuid.MustParse("0192a3b4-0000-7000-8000-000000000001")
	if dedupKey(id, core.LayerGrammar) != "eval:0192a3b4-0000-7000-8000-000000000001:grammar" {
		t.Error("dedupKey")
	}
	if f32(nil) != nil || *f32(ptr(59.5)) != 59.5 {
		t.Error("f32")
	}
	if utcPtr(nil) != nil {
		t.Error("utcPtr(nil)")
	}
	msk := time.FixedZone("MSK", 3*3600)
	if u := utcPtr(ptr(time.Date(2026, 9, 24, 15, 0, 0, 0, msk))); u.Location() != time.UTC || u.Hour() != 12 {
		t.Errorf("utcPtr = %v", u)
	}
}

// ---------------------------------------------------------------- порты

type routerRec struct {
	got map[core.JobType]core.AIResultHandler
}

func (r *routerRec) Register(t core.JobType, h core.AIResultHandler) { r.got[t] = h }

func TestRegisterResults(t *testing.T) {
	t.Parallel()
	s := New(Deps{})
	rr := &routerRec{got: map[core.JobType]core.AIResultHandler{}}
	s.RegisterResults(rr)
	for _, jt := range []core.JobType{core.JobEvaluateGrammar, core.JobEvaluateSemantic, core.JobEvaluateDialogue} {
		if rr.got[jt] != s {
			t.Errorf("%s not registered", jt)
		}
	}
	if len(rr.got) != 3 {
		t.Errorf("registered %d handlers, want 3", len(rr.got))
	}
}

// Без Publisher/Auditor (CLI, сид) пуши и аудит — no-op, без паники.
func TestNilPortsAreNoop(t *testing.T) {
	t.Parallel()
	s := New(Deps{})
	ev := s.buildView(newTestRow())
	s.publish(uuid.New(), uuid.New(), &ev)
	s.publishParticipant(uuid.New(), &public.LessonParticipant{})
	s.audit(context.Background(), core.AuditEntry{Action: "evaluation.override"})

	pub := &pubRec{}
	s = New(Deps{Publisher: pub, Log: discardLog()})
	s.publishParticipant(uuid.New(), nil)
	if len(pub.monitor) != 0 {
		t.Fatal("nil participant must not be pushed")
	}
	lesson, attempt := uuid.New(), uuid.New()
	s.publish(lesson, attempt, &ev)
	if len(pub.monitor) != 1 || pub.monitor[0].Lesson != lesson || *pub.monitor[0].Msg.AttemptId != attempt ||
		len(pub.student) != 1 || pub.student[0].Attempt != attempt || pub.student[0].Msg.Evaluation != &ev {
		t.Fatalf("pushes: %+v %+v", pub.monitor, pub.student)
	}
}

// catalogStub — core.Catalog: путь категории для scenario_context.
type catalogStub struct {
	types map[string]core.IncidentTypeInfo
}

func (c catalogStub) TypeByID(id string) (core.IncidentTypeInfo, bool) {
	t, ok := c.types[id]
	return t, ok
}
func (c catalogStub) TypeByCode(string) (core.IncidentTypeInfo, bool) {
	return core.IncidentTypeInfo{}, false
}
func (c catalogStub) ServiceByID(string) (core.ServiceInfo, bool)   { return core.ServiceInfo{}, false }
func (c catalogStub) ServiceByCode(string) (core.ServiceInfo, bool) { return core.ServiceInfo{}, false }
func (c catalogStub) FieldLabel(string) string                      { return "Поле карточки" }
func (c catalogStub) AttributeLabel(string) (string, bool)          { return "", false }
func (c catalogStub) AttributeValueLabel(string, string) (string, bool) {
	return "", false
}

func TestScenarioContext(t *testing.T) {
	t.Parallel()
	st := testStart(core.ModeCards)
	cat := catalogStub{types: map[string]core.IncidentTypeInfo{
		st.CategoryID.String(): {ID: st.CategoryID.String(), Code: "ev101", Name: "Пожар", Path: []string{"Происшествия", "Пожар"}},
	}}
	sc := New(Deps{Catalog: cat}).scenarioContext(st)
	raw, _ := json.Marshal(sc)
	if string(raw) != `{"category":{"code":"ev101","name":"Пожар","path":["Происшествия","Пожар"]},"difficulty":2}` {
		t.Fatalf("scenario_context = %s", raw)
	}
	st.Difficulty = 0 // вне 1..3 — не отправляется
	sc = New(Deps{}).scenarioContext(st)
	raw, _ = json.Marshal(sc)
	if string(raw) != `{"category":{"code":"ev101","name":"Пожар"}}` {
		t.Fatalf("without catalog = %s", raw)
	}
}
