package aijobs

import (
	"math"
	"testing"
	"time"

	"lct/gocore/internal/core"
)

func TestCeilSec(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   float64
		want int
	}{
		{math.NaN(), 1}, {-5, 1}, {0, 1}, {0.99, 1}, {1, 1}, {1.01, 2}, {59.2, 60}, {86400, 86400}, {1e9, 86400},
		{math.Inf(1), 86400},
	}
	for _, c := range cases {
		if got := ceilSec(c.in); got != c.want {
			t.Errorf("ceilSec(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestEstimatorObserveAndSeed(t *testing.T) {
	t.Parallel()
	e := newEstimator()
	sem := typeIdx(core.JobEvaluateSemantic)
	if got := e.averages()[sem]; got != defaultAvgSec[sem] {
		t.Fatalf("default avg = %v, want %v", got, defaultAvgSec[sem])
	}
	e.observe(sem, 10_000) // EMA: 20 000·0.8 + 10 000·0.2 = 18 000 мс
	if got := e.averages()[sem]; math.Abs(got-18) > 1e-9 {
		t.Errorf("after observe avg = %v, want 18", got)
	}
	// Шум и зависания не учитываются; неизвестный тип не паникует.
	for _, ms := range []float64{0, 49, maxObserved + 1} {
		e.observe(sem, ms)
	}
	e.observe(-1, 5000)
	if got := e.averages()[sem]; math.Abs(got-18) > 1e-9 {
		t.Errorf("out-of-range observations changed avg to %v", got)
	}
	e.seed(sem, 4000)
	if got := e.averages()[sem]; got != 4 {
		t.Errorf("seed: avg = %v, want 4", got)
	}
	e.seed(sem, 1) // шум — игнор
	e.seed(-1, 4000)
	if got := e.averages()[sem]; got != 4 {
		t.Errorf("bad seed changed avg to %v", got)
	}
}

func TestEstimatorLoadStale(t *testing.T) {
	t.Parallel()
	e := newEstimator()
	now := time.Now()
	if !e.loadStale(now) {
		t.Fatal("fresh estimator must want a load snapshot")
	}
	e.setLoad(nil, now)
	if e.loadStale(now.Add(loadMaxAge / 2)) {
		t.Error("snapshot is stale too early")
	}
	if !e.loadStale(now.Add(loadMaxAge)) {
		t.Error("snapshot never goes stale")
	}
}

func TestEstimateNewJob(t *testing.T) {
	t.Parallel()
	var avg [numTypes]float64
	for i := range avg {
		avg[i] = 10_000 // 10 с на любую задачу
	}
	sem, gram, gen, tts := typeIdx(core.JobEvaluateSemantic), typeIdx(core.JobEvaluateGrammar),
		typeIdx(core.JobGenerateScenario), typeIdx(core.JobTTS)

	if got := estimateNewJob(&avg, nil, sem, core.PrioritySemantic); got != 10 {
		t.Errorf("empty queue: %d, want 10", got)
	}
	load := []loadRow{
		{typ: sem, prio: 3, n: 2},                // впереди: 20 с
		{typ: sem, prio: 3, n: 2, running: true}, // выполняются: 2·0.5·10 = 10 с
		{typ: gen, prio: 7, n: 5},                // приоритет ниже — не мешает
		{typ: gram, prio: 2, n: 9},               // другая полоса (LT)
		{typ: tts, prio: 3, n: 9},                // другая полоса (TTS)
	}
	if got := estimateNewJob(&avg, load, sem, core.PrioritySemantic); got != 40 {
		t.Errorf("semantic with load: %d, want 40", got)
	}
	// Генерация (priority 7) ждёт всю полосу LLM.
	if got := estimateNewJob(&avg, load, gen, core.PriorityGenerate); got != 90 {
		t.Errorf("generate with load: %d, want 90", got)
	}
	if got := estimateNewJob(&avg, load, gram, core.PriorityGrammar); got != 100 {
		t.Errorf("grammar lane: %d, want 100", got)
	}
}

func TestEstimateQueuedAndRunning(t *testing.T) {
	t.Parallel()
	var avg [numTypes]float64
	for i := range avg {
		avg[i] = 10 // секунды
	}
	sem := typeIdx(core.JobEvaluateSemantic)
	dlg := typeIdx(core.JobEvaluateDialogue)

	pos, wait := estimateQueued(&avg, sem, nil, 0)
	if pos != 1 || wait != 10 {
		t.Errorf("alone: pos=%d wait=%d, want 1, 10", pos, wait)
	}
	rows := []aheadRow{{typ: sem, queuedAhead: 2, runningAll: 2}, {typ: dlg, queuedAhead: 1}}
	pos, wait = estimateQueued(&avg, sem, rows, 5*time.Second)
	// 10 своя + 3·10 впереди + 2·0.5·10 выполняются + 5 run_after
	if pos != 4 || wait != 55 {
		t.Errorf("queued: pos=%d wait=%d, want 4, 55", pos, wait)
	}
	if _, wait = estimateQueued(&avg, sem, nil, -time.Minute); wait != 10 {
		t.Errorf("past run_after must not reduce wait: %d", wait)
	}

	// running: впереди нет — осталось среднее − прошедшее.
	if w, ok := estimateRunning(&avg, sem, nil, 4*time.Second); !ok || w != 6 {
		t.Errorf("running alone: %d,%v, want 6,true", w, ok)
	}
	if _, ok := estimateRunning(&avg, sem, nil, 30*time.Second); ok {
		t.Error("running longer than average must give no estimate")
	}
	// впереди выполняются 2 задачи — прошедшее не вычитается.
	if w, ok := estimateRunning(&avg, sem, []aheadRow{{typ: dlg, runningAhead: 2}}, time.Hour); !ok || w != 30 {
		t.Errorf("running behind others: %d,%v, want 30,true", w, ok)
	}
}
