package aijobs

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
	"lct/gocore/internal/platform/ids"
)

func TestCallbackOkAppliesResult(t *testing.T) {
	t.Parallel()
	s, pool := newDBService(t, "")
	h := registerAll(s)
	ref := ids.New()
	id := enqueue(t, s, core.JobEvaluateSemantic, withRef(core.RefAttempt, ref))
	markRunning(t, pool, id, time.Second, time.Second)

	if r := mustCallback(t, s, okCallback(id, core.JobEvaluateSemantic)); !r.Accepted || r.Duplicate {
		t.Fatalf("reply %+v", r)
	}
	j := readJob(t, pool, id)
	if j.Status != "done" || j.Error != nil || j.FinishedAt == nil || j.LockedBy != nil {
		t.Errorf("done job %+v", j)
	}
	var stored map[string]any
	if err := json.Unmarshal(j.Result, &stored); err != nil || stored["status"] != "ok" {
		t.Errorf("stored result %s", j.Result)
	}
	if n, _ := h.counts(); n != 1 || h.results[0].ID != id || h.results[0].RefID != ref || h.results[0].Type != core.JobEvaluateSemantic {
		t.Errorf("ApplyResult calls %+v", h.results)
	}
	// Повтор callback'а — дубль, обработчик не зовётся второй раз.
	if r := mustCallback(t, s, okCallback(id, core.JobEvaluateSemantic)); !r.Accepted || !r.Duplicate {
		t.Errorf("repeat reply %+v", r)
	}
	if n, _ := h.counts(); n != 1 {
		t.Errorf("handler called %d times", n)
	}
	st := s.Stats()
	if st.Callbacks != 2 || st.CallbackDups != 1 {
		t.Errorf("stats %+v", st)
	}
	// Оценка ожидания учла engine.duration_ms (1500 мс).
	if avg := s.est.averages()[typeIdx(core.JobEvaluateSemantic)]; avg >= defaultAvgSec[typeIdx(core.JobEvaluateSemantic)] {
		t.Errorf("average not updated: %v", avg)
	}
}

func TestCallbackUnknownJobAccepted(t *testing.T) {
	t.Parallel()
	s, _ := newDBService(t, "")
	if r := mustCallback(t, s, okCallback(ids.New(), core.JobTTS)); !r.Accepted || r.Duplicate {
		t.Errorf("unknown job reply %+v", r)
	}
}

func TestCallbackTypeMismatch(t *testing.T) {
	t.Parallel()
	s, pool := newDBService(t, "")
	registerAll(s)
	id := enqueue(t, s, core.JobEvaluateSemantic)
	rec := postCallback(t, s, okCallback(id, core.JobTTS))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "не совпадает") {
		t.Errorf("mismatch: %d %s", rec.Code, rec.Body)
	}
	if j := readJob(t, pool, id); j.Status != "queued" {
		t.Errorf("job changed on mismatch: %s", j.Status)
	}
}

// Ошибка обработчика — 500 (ai-service повторит), транзакция откатывается целиком.
func TestCallbackHandlerErrorRollsBack(t *testing.T) {
	t.Parallel()
	s, pool := newDBService(t, "")
	h := registerAll(s)
	h.resultErr = errors.New("deadlock detected")
	id := enqueue(t, s, core.JobEvaluateGrammar)
	markRunning(t, pool, id, time.Second, time.Second)
	rec := postCallback(t, s, okCallback(id, core.JobEvaluateGrammar))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "deadlock") {
		t.Error("internal error details leaked to the response")
	}
	if j := readJob(t, pool, id); j.Status != "running" || j.Result != nil {
		t.Errorf("job after failed apply: %+v", j)
	}
	h.resultErr = nil
	if r := mustCallback(t, s, okCallback(id, core.JobEvaluateGrammar)); r.Duplicate {
		t.Error("retry after 500 treated as duplicate")
	}
	if j := readJob(t, pool, id); j.Status != "done" {
		t.Errorf("retry: status %s", j.Status)
	}
}

func TestCallbackNoHandlerMarksDone(t *testing.T) {
	t.Parallel()
	s, pool := newDBService(t, "")
	id := enqueue(t, s, core.JobTTS)
	mustCallback(t, s, okCallback(id, core.JobTTS))
	if j := readJob(t, pool, id); j.Status != "done" {
		t.Errorf("status %s", j.Status)
	}
}

func TestCallbackFailedPermanent(t *testing.T) {
	t.Parallel()
	s, pool := newDBService(t, "")
	h := registerAll(s)
	cases := []struct {
		name      string
		code      string
		retryable bool
		wantCode  string
	}{
		{"not retryable", "model_unavailable", false, "model_unavailable"},
		{"bad_payload even if retryable", "bad_payload", true, "bad_payload"},
		{"unknown code treated as internal", "gpu_on_fire", false, "internal"},
	}
	for _, c := range cases {
		id := enqueue(t, s, core.JobEvaluateSemantic, withRef(core.RefAttempt, ids.New()))
		markRunning(t, pool, id, time.Second, time.Second)
		if r := mustCallback(t, s, failedCallback(id, core.JobEvaluateSemantic, c.code, c.retryable)); r.Duplicate {
			t.Errorf("%s: duplicate", c.name)
		}
		j := readJob(t, pool, id)
		if j.Status != "failed" || j.TryCount != 1 || !strings.HasPrefix(strp(j.Error), c.wantCode+": ") || j.ResultCode == nil {
			t.Errorf("%s: job %+v (error %s)", c.name, j, strp(j.Error))
		}
		if fc := h.lastFailure(t); fc.Code != c.wantCode || fc.Job.ID != id || fc.Message != "Модель не ответила вовремя" {
			t.Errorf("%s: ApplyFailure %+v", c.name, fc)
		}
	}
}

func TestCallbackFailedRetryable(t *testing.T) {
	t.Parallel()
	s, pool := newDBService(t, "")
	h := registerAll(s)
	id := enqueue(t, s, core.JobTTS)
	markRunning(t, pool, id, time.Second, time.Second)
	before := time.Now()
	if r := mustCallback(t, s, failedCallback(id, core.JobTTS, "tts_failed", true)); r.Duplicate {
		t.Fatal("duplicate")
	}
	j := readJob(t, pool, id)
	if j.Status != "queued" || j.TryCount != 1 || j.LockedBy != nil || j.StartedAt != nil ||
		!strings.HasPrefix(strp(j.Error), "tts_failed: ") || strp(j.ResultCode) != "tts_failed" {
		t.Errorf("retried job %+v", j)
	}
	if d := j.RunAfter.Sub(before); d < 3*time.Second || d > 7*time.Second {
		t.Errorf("retry delay %v, want ≈5 s", d)
	}
	if j.RequestID == id.String() || j.RequestID == "" {
		t.Errorf("retry must use a new request_id, got %q", j.RequestID)
	}
	if _, f := h.counts(); f != 0 {
		t.Error("ApplyFailure called for a retried job")
	}
	select {
	case <-s.kickCh:
	default:
		t.Error("dispatcher not kicked after retry")
	}
}

// Регрессия (finding: ретрай с тем же request_id): ai-service по контракту идемпотентен по
// request_id и на повтор уже завершённой (отказом) задачи вправе переслать тот же отказ из
// кэша. Поэтому повтор уходит под новым request_id, результат по нему находит задачу,
// а повторная доставка старого отказа — дубль и не тратит попытку.
func TestCallbackRetryUnderNewRequestID(t *testing.T) {
	t.Parallel()
	s, pool := newDBService(t, "")
	h := registerAll(s)
	id := enqueue(t, s, core.JobEvaluateSemantic, withRef(core.RefAttempt, ids.New()))
	markRunning(t, pool, id, time.Second, time.Second)

	mustCallback(t, s, failedCallback(id, core.JobEvaluateSemantic, "llm_timeout", true))
	retryReq := readJob(t, pool, id).RequestID

	// ai-service повторно доставил старый отказ (его 200 от нас потерялся).
	if r := mustCallback(t, s, failedCallback(id, core.JobEvaluateSemantic, "llm_timeout", true)); !r.Duplicate {
		t.Error("replayed failure of the superseded request must be a duplicate")
	}
	if j := readJob(t, pool, id); j.TryCount != 1 || j.RequestID != retryReq || j.Status != "queued" {
		t.Errorf("replayed failure changed the job: %+v", j)
	}

	// Повтор отправлен и посчитан заново: результат по новому request_id.
	markRunning(t, pool, id, time.Second, time.Second)
	if r := mustCallback(t, s, okCallback(uuidMust(t, retryReq), core.JobEvaluateSemantic)); r.Duplicate {
		t.Fatal("result of the retry treated as duplicate")
	}
	j := readJob(t, pool, id)
	if j.Status != "done" || j.TryCount != 1 {
		t.Errorf("after retry result: %+v", j)
	}
	if n, f := h.counts(); n != 1 || f != 0 || h.results[0].ID != id {
		t.Errorf("handler results=%d failures=%d", n, f)
	}
	// Повтор доставки результата ретрая после закрытия — принять и забыть.
	if r := mustCallback(t, s, okCallback(uuidMust(t, retryReq), core.JobEvaluateSemantic)); !r.Accepted {
		t.Error("late duplicate of retry result rejected")
	}
	if n, _ := h.counts(); n != 1 {
		t.Error("late duplicate applied twice")
	}
}

// Годный результат прежней отправки принимается, даже если задача уже ушла ретраем.
func TestCallbackOkFromSupersededRequestAccepted(t *testing.T) {
	t.Parallel()
	s, pool := newDBService(t, "")
	h := registerAll(s)
	id := enqueue(t, s, core.JobTTS)
	mustCallback(t, s, failedCallback(id, core.JobTTS, "tts_failed", true))
	if r := mustCallback(t, s, okCallback(id, core.JobTTS)); r.Duplicate {
		t.Fatal("ok result dropped")
	}
	if j := readJob(t, pool, id); j.Status != "done" {
		t.Errorf("status %s", j.Status)
	}
	if n, _ := h.counts(); n != 1 {
		t.Errorf("results %d", n)
	}
}

// Три отказа подряд при max_tries=3 — failed; каждый ретрай — свой request_id.
func TestCallbackRetriesExhausted(t *testing.T) {
	t.Parallel()
	s, pool := newDBService(t, "")
	h := registerAll(s)
	id := enqueue(t, s, core.JobEvaluateDialogue, withMaxTries(3), withRef(core.RefAttempt, ids.New()))
	seenReq := map[string]bool{}
	req := id.String()
	for i := 0; i < 3; i++ {
		seenReq[req] = true
		markRunning(t, pool, id, time.Second, time.Second)
		mustCallback(t, s, failedCallback(uuidMust(t, req), core.JobEvaluateDialogue, "model_unavailable", true))
		req = readJob(t, pool, id).RequestID
	}
	j := readJob(t, pool, id)
	if j.Status != "failed" || j.TryCount != 3 {
		t.Fatalf("after 3 failures: %+v", j)
	}
	if len(seenReq) != 3 {
		t.Errorf("request ids reused: %v", seenReq)
	}
	if fc := h.lastFailure(t); fc.Code != "model_unavailable" {
		t.Errorf("ApplyFailure %+v", fc)
	}
}

// Регрессия (finding: llm_invalid_output ограничен общим try_count): после llm_timeout
// невалидный ответ LLM получает свой один повтор; второй невалидный подряд — отказ.
func TestCallbackInvalidOutputRetryPolicy(t *testing.T) {
	t.Parallel()
	s, pool := newDBService(t, "")
	h := registerAll(s)
	id := enqueue(t, s, core.JobEvaluateSemantic, withMaxTries(5), withRef(core.RefAttempt, ids.New()))
	step := func(code string) jobState {
		t.Helper()
		j := readJob(t, pool, id)
		markRunning(t, pool, id, time.Second, time.Second)
		mustCallback(t, s, failedCallback(uuidMust(t, j.RequestID), core.JobEvaluateSemantic, code, true))
		return readJob(t, pool, id)
	}
	if j := step("llm_timeout"); j.Status != "queued" || j.TryCount != 1 {
		t.Fatalf("after timeout: %+v", j)
	}
	if j := step("llm_invalid_output"); j.Status != "queued" || j.TryCount != 2 {
		t.Fatalf("invalid output after timeout must be retried once: %+v", j)
	}
	if j := step("llm_invalid_output"); j.Status != "failed" || j.TryCount != 3 {
		t.Fatalf("second invalid output in a row must fail: %+v", j)
	}
	if fc := h.lastFailure(t); fc.Code != "llm_invalid_output" {
		t.Errorf("ApplyFailure %+v", fc)
	}
}

func uuidMust(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, ok := ids.Parse(s)
	if !ok {
		t.Fatalf("bad uuid %q", s)
	}
	return id
}
