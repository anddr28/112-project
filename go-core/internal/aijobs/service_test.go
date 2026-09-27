package aijobs

import (
	"context"
	"testing"
	"time"

	"lct/gocore/internal/core"
	"lct/gocore/internal/settings"
)

func TestCounts(t *testing.T) {
	t.Parallel()
	s, pool := newDBService(t, "")
	m, err := s.Counts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{core.JobQueued, core.JobRunning, core.JobDone, core.JobFailed, core.JobCancelled} {
		if v, ok := m[k]; !ok || v != 0 {
			t.Errorf("empty queue: %s = %d (present %v)", k, v, ok)
		}
	}
	enqueue(t, s, core.JobTTS)
	enqueue(t, s, core.JobTTS)
	run := enqueue(t, s, core.JobTTS)
	markRunning(t, pool, run, time.Second, time.Second)
	done := enqueue(t, s, core.JobTTS)
	exec(t, pool, `UPDATE ai_jobs SET status = 'done', finished_at = now() WHERE id = $1`, done)
	oldDone := enqueue(t, s, core.JobTTS)
	exec(t, pool, `UPDATE ai_jobs SET status = 'done', finished_at = now() - interval '2 days' WHERE id = $1`, oldDone)
	failed := enqueue(t, s, core.JobTTS)
	exec(t, pool, `UPDATE ai_jobs SET status = 'failed', finished_at = now() WHERE id = $1`, failed)

	m, err = s.Counts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"queued": 2, "running": 1, "done": 1, "failed": 1, "cancelled": 0}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %d, want %d (all: %v)", k, m[k], v, m)
		}
	}
}

func TestSeedAveragesAndRefreshLoad(t *testing.T) {
	t.Parallel()
	s, pool := newDBService(t, "")
	sem := typeIdx(core.JobEvaluateSemantic)
	for _, ms := range []int{3000, 5000} {
		id := enqueue(t, s, core.JobEvaluateSemantic)
		exec(t, pool, `UPDATE ai_jobs SET status = 'done', result = jsonb_build_object('engine', jsonb_build_object('duration_ms', $2::int))
		  WHERE id = $1`, id, ms)
	}
	junk := enqueue(t, s, core.JobEvaluateSemantic) // результат без длительности — не учитывается
	exec(t, pool, `UPDATE ai_jobs SET status = 'done', result = '{"engine":{}}' WHERE id = $1`, junk)
	s.seedAverages(context.Background())
	if avg := s.est.averages()[sem]; avg != 4 {
		t.Errorf("seeded semantic avg = %v, want 4", avg)
	}

	for i := 0; i < 3; i++ {
		enqueue(t, s, core.JobEvaluateSemantic, withPriority(core.PrioritySemantic))
	}
	now := time.Now()
	s.refreshLoad(context.Background(), now)
	if s.est.loadStale(now) {
		t.Error("load snapshot not stored")
	}
	// 3 в очереди по 4 с + своя 4 с.
	if got := s.EstWaitSec(core.JobEvaluateSemantic); got != 16 {
		t.Errorf("EstWaitSec = %d, want 16", got)
	}
	// Грамматика в своей полосе — очередь LLM её не касается.
	if got := s.EstWaitSec(core.JobEvaluateGrammar); got != int(defaultAvgSec[typeIdx(core.JobEvaluateGrammar)]) {
		t.Errorf("grammar EstWaitSec = %d", got)
	}
}

func TestRefreshSettingsClampsBadValues(t *testing.T) {
	t.Parallel()
	s, pool := newDBService(t, "")
	s.settings = settings.NewStore(pool, discardLog())
	exec(t, pool, `UPDATE settings SET value = '{"reaper_after_sec": 5, "max_tries": 0, "dialog_timeout_sec": 7,
	        "breaker_open_sec": 0, "breaker_failures": -1}' WHERE key = 'ai'`)
	if err := s.settings.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.refreshSettings(context.Background())
	ai := s.aiSettings()
	d := settings.Defaults().AI
	if ai.ReaperAfterSec != d.ReaperAfterSec || ai.MaxTries != d.MaxTries || ai.BreakerOpenSec != d.BreakerOpenSec ||
		ai.BreakerFailures != d.BreakerFailures || ai.DialogTimeoutSec != 7 {
		t.Errorf("settings snapshot %+v", *ai)
	}
}

func TestRegisterUnknownTypePanics(t *testing.T) {
	t.Parallel()
	s := newTestService(t, nil, "")
	defer func() {
		if recover() == nil {
			t.Error("Register with unknown type must panic")
		}
	}()
	s.Register("translate", &recHandler{})
}

// Полный цикл на живых циклах: Start → задача уходит в ai-service → callback → done → Stop.
func TestStartDispatchesAndStops(t *testing.T) {
	t.Parallel()
	f := newFakeAI(t, accepted202)
	s, pool := newDBService(t, f.URL)
	h := registerAll(s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	s.Start(ctx) // повторный Start — no-op

	id := enqueue(t, s, core.JobEvaluateGrammar)
	deadline := time.Now().Add(10 * time.Second)
	for readJob(t, pool, id).Status != "running" {
		if time.Now().After(deadline) {
			t.Fatal("job was not dispatched by the running dispatcher")
		}
		time.Sleep(20 * time.Millisecond)
	}
	mustCallback(t, s, okCallback(id, core.JobEvaluateGrammar))
	if n, _ := h.counts(); n != 1 {
		t.Errorf("results %d", n)
	}
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	if err := s.Stop(sctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if st := s.Stats(); st.Dispatched != 1 || st.Accepted != 1 || st.Breaker != BreakerClosed {
		t.Errorf("stats %+v", st)
	}
}

func TestStopWithoutStart(t *testing.T) {
	t.Parallel()
	s := newTestService(t, nil, "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Errorf("stop without start: %v", err)
	}
	s.Wait()
}
