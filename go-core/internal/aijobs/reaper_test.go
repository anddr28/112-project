package aijobs

import (
	"context"
	"strings"
	"testing"
	"time"

	"lct/gocore/internal/core"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/settings"
)

func newReaperService(t *testing.T) (*Service, *recHandler) {
	t.Helper()
	s, _ := newDBService(t, "")
	setAI(s, func(ai *settings.AI) { ai.ReaperAfterSec = 60; ai.MaxTries = 3 })
	return s, registerAll(s)
}

func TestReaperResendsStuckJobWithoutSpendingTries(t *testing.T) {
	t.Parallel()
	s, h := newReaperService(t)
	pool := s.pool
	stuck := enqueue(t, s, core.JobEvaluateSemantic, withRef(core.RefAttempt, ids.New()))
	fresh := enqueue(t, s, core.JobEvaluateSemantic)
	markRunning(t, pool, stuck, 2*time.Minute, 2*time.Minute)
	markRunning(t, pool, fresh, 10*time.Second, 10*time.Second)

	s.reap(context.Background())

	j := readJob(t, pool, stuck)
	if j.Status != "queued" || j.TryCount != 0 || j.LockedBy != nil || !strings.HasPrefix(strp(j.Error), codeResent+": ") ||
		j.RequestID != stuck.String() || j.StartedAt == nil {
		t.Errorf("stuck job after reap: %+v (error %s)", j, strp(j.Error))
	}
	if time.Until(j.RunAfter) > time.Second {
		t.Errorf("resend delayed until %v", j.RunAfter)
	}
	if j := readJob(t, pool, fresh); j.Status != "running" {
		t.Errorf("fresh running job touched: %s", j.Status)
	}
	if _, f := h.counts(); f != 0 {
		t.Error("ApplyFailure called on resend")
	}
	if st := s.Stats(); st.Reaped != 1 || st.Requeued != 1 {
		t.Errorf("stats %+v", st)
	}
	select {
	case <-s.kickCh:
	default:
		t.Error("dispatcher not kicked after reap")
	}
}

// Регрессия (finding: reaper считает ожидание в очереди ai-service отказом): задача, которую
// ai-service держит в очереди (полоса LLM занята ходами диалога), раньше за 3 прохода reaper'а
// (45 мин) исчерпывала try_count и проваливалась reaper_timeout, а её поздний результат
// отбрасывался как дубль. Теперь переотправка попытку не тратит — даже на последней попытке.
func TestReaperKeepsJobHeldByAIServiceOnLastTry(t *testing.T) {
	t.Parallel()
	s, h := newReaperService(t)
	pool := s.pool
	id := enqueue(t, s, core.JobEvaluateDialogue, withRef(core.RefAttempt, ids.New()))
	exec(t, pool, `UPDATE ai_jobs SET try_count = 2 WHERE id = $1`, id) // два настоящих отказа позади
	// Прежний предел 3 × reaper_after уже пройден, до нового (12 ×) далеко.
	markRunning(t, pool, id, 2*time.Minute, 5*time.Minute)

	for i := 0; i < 3; i++ {
		s.reap(context.Background())
		j := readJob(t, pool, id)
		if j.Status != "queued" || j.TryCount != 2 {
			t.Fatalf("reap %d: %+v", i, j)
		}
		markRunning(t, pool, id, 2*time.Minute, 5*time.Minute) // переотправлена, снова ждёт
	}
	if _, f := h.counts(); f != 0 {
		t.Fatal("held job failed by reaper")
	}
	// Результат, пришедший позже, применяется.
	if r := mustCallback(t, s, okCallback(id, core.JobEvaluateDialogue)); r.Duplicate {
		t.Fatal("late result of a held job discarded as duplicate")
	}
	if n, _ := h.counts(); n != 1 || readJob(t, pool, id).Status != "done" {
		t.Error("late result not applied")
	}
}

func TestReaperFailsJobWithoutResultPastHoldLimit(t *testing.T) {
	t.Parallel()
	s, h := newReaperService(t)
	pool := s.pool
	id := enqueue(t, s, core.JobGenerateScenario, withRef(core.RefScenario, ids.New()))
	hold := time.Duration(reaperHoldSec(s.aiSettings())) * time.Second
	markRunning(t, pool, id, 2*time.Minute, hold+time.Minute)

	s.reap(context.Background())

	j := readJob(t, pool, id)
	if j.Status != "failed" || j.TryCount != 1 || j.FinishedAt == nil || !strings.HasPrefix(strp(j.Error), CodeReaperTimeout+": ") {
		t.Errorf("job past hold: %+v (error %s)", j, strp(j.Error))
	}
	fc := h.lastFailure(t)
	if fc.Code != CodeReaperTimeout || fc.Job.ID != id || fc.Job.RefType != core.RefScenario {
		t.Errorf("ApplyFailure %+v", fc)
	}
	if st := s.Stats(); st.Reaped != 1 || st.Failed != 1 {
		t.Errorf("stats %+v", st)
	}
	// Поздний результат после провала — дубль (обработчик уже закрыл слой).
	if r := mustCallback(t, s, okCallback(id, core.JobGenerateScenario)); !r.Duplicate {
		t.Error("result for failed job must be a duplicate")
	}
}

func TestReaperLeavesQueuedAndFinishedJobs(t *testing.T) {
	t.Parallel()
	s, _ := newReaperService(t)
	pool := s.pool
	queued := enqueue(t, s, core.JobTTS)
	done := enqueue(t, s, core.JobTTS)
	exec(t, pool, `UPDATE ai_jobs SET created_at = now() - interval '1 day' WHERE id = $1`, queued)
	exec(t, pool, `UPDATE ai_jobs SET status = 'done', locked_at = now() - interval '1 day',
	        started_at = now() - interval '1 day' WHERE id = $1`, done)
	s.reap(context.Background())
	if readJob(t, pool, queued).Status != "queued" || readJob(t, pool, done).Status != "done" {
		t.Error("reaper touched non-running jobs")
	}
}

// ---------------------------------------------------------------- недоступность ai-service

// Регрессия (finding: при недоступном ai-service оценки «частичные» бесконечно): оценочные
// задачи, ждущие дольше reaper_after_sec, пока ai-service недоступен столько же, проваливаются
// ai_unavailable — оценка закрывается по доступным слоям с флагами aiUnavailable/needsReview.
func TestFailUnavailableEvaluationJobs(t *testing.T) {
	t.Parallel()
	s, h := newReaperService(t)
	pool := s.pool
	old := func(typ core.JobType) string {
		id := enqueue(t, s, typ, withRef(core.RefAttempt, ids.New()))
		exec(t, pool, `UPDATE ai_jobs SET created_at = now() - interval '5 minutes' WHERE id = $1`, id)
		return id.String()
	}
	gram, sem, dlg := old(core.JobEvaluateGrammar), old(core.JobEvaluateSemantic), old(core.JobEvaluateDialogue)
	gen, tts := old(core.JobGenerateScenario), old(core.JobTTS)
	recent := enqueue(t, s, core.JobEvaluateSemantic)
	running := enqueue(t, s, core.JobEvaluateGrammar)
	exec(t, pool, `UPDATE ai_jobs SET created_at = now() - interval '5 minutes' WHERE id = $1`, running)
	markRunning(t, pool, running, 30*time.Second, 30*time.Second)

	// ai-service доступен — ничего не трогаем.
	s.failUnavailable(context.Background())
	if _, f := h.counts(); f != 0 {
		t.Fatal("jobs failed while ai-service is available")
	}
	// Недоступен меньше порога — тоже.
	s.unavailSince.Store(time.Now().Add(-30 * time.Second).UnixNano())
	s.failUnavailable(context.Background())
	if _, f := h.counts(); f != 0 {
		t.Fatal("jobs failed before the outage threshold")
	}

	s.unavailSince.Store(time.Now().Add(-2 * time.Minute).UnixNano())
	s.failUnavailable(context.Background())

	for _, id := range []string{gram, sem, dlg} {
		j := readJob(t, pool, uuidMust(t, id))
		if j.Status != "failed" || j.TryCount != 0 || j.FinishedAt == nil || !strings.HasPrefix(strp(j.Error), CodeAIUnavailable+": ") {
			t.Errorf("evaluation job %s: %+v (error %s)", id, j, strp(j.Error))
		}
	}
	for _, id := range []string{gen, tts} {
		if j := readJob(t, pool, uuidMust(t, id)); j.Status != "queued" {
			t.Errorf("background job %s failed: %s", id, j.Status)
		}
	}
	if j := readJob(t, pool, recent); j.Status != "queued" {
		t.Errorf("recent evaluation job failed: %s", j.Status)
	}
	if j := readJob(t, pool, running); j.Status != "running" {
		t.Errorf("job held by ai-service failed: %s", j.Status)
	}
	if _, f := h.counts(); f != 3 {
		t.Errorf("ApplyFailure calls = %d, want 3", f)
	}
	for _, fc := range h.failures {
		if fc.Code != CodeAIUnavailable || fc.Job.RefType != core.RefAttempt {
			t.Errorf("ApplyFailure %+v", fc)
		}
	}
	// Повторный проход ничего не дублирует.
	s.failUnavailable(context.Background())
	if _, f := h.counts(); f != 3 {
		t.Errorf("second pass: ApplyFailure calls = %d", f)
	}
}

func TestOutageClock(t *testing.T) {
	t.Parallel()
	s := newTestService(t, nil, "")
	now := time.Now()
	if s.unavailableFor(now) != 0 {
		t.Fatal("fresh service reports an outage")
	}
	s.onBreakerChange(stateClosed, stateOpen)
	start := s.unavailSince.Load()
	if start == 0 {
		t.Fatal("breaker open did not start the outage clock")
	}
	// open → half_open → open (упавшая проба) — та же авария: начало не сдвигается.
	s.onBreakerChange(stateOpen, stateHalfOpen)
	s.onBreakerChange(stateHalfOpen, stateOpen)
	if s.unavailSince.Load() != start {
		t.Error("failed probe restarted the outage clock")
	}
	if d := s.unavailableFor(time.Unix(0, start).Add(time.Minute)); d != time.Minute {
		t.Errorf("unavailableFor = %v", d)
	}
	// Поздний 202 при открытом breaker'е аварию не отменяет.
	s.br.Failure()
	s.br.Failure()
	s.br.Failure()
	s.markReachable()
	if s.unavailSince.Load() == 0 {
		t.Error("late accept with open breaker cleared the outage")
	}
	s.onBreakerChange(stateHalfOpen, stateClosed)
	if s.unavailSince.Load() != 0 {
		t.Error("closed breaker did not clear the outage clock")
	}
	s.markUnreachable()
	if s.unavailableFor(time.Now().Add(-time.Hour)) != 0 {
		t.Error("negative outage duration")
	}
}
