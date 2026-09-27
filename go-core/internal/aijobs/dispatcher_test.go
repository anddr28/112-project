package aijobs

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/settings"
)

func TestDispatchAccepted(t *testing.T) {
	t.Parallel()
	f := newFakeAI(t, accepted202)
	s, pool := newDBService(t, f.URL)
	sem := enqueue(t, s, core.JobEvaluateSemantic)
	gram := enqueue(t, s, core.JobEvaluateGrammar)
	later := enqueue(t, s, core.JobTTS, withRunAfter(time.Now().Add(time.Hour)))

	dispatchOnce(s)

	seen := f.requests()
	if len(seen) != 2 {
		t.Fatalf("sent %d jobs, want 2 (run_after in the future waits)", len(seen))
	}
	paths := map[string]uuid.UUID{}
	for _, r := range seen {
		var body struct {
			RequestID uuid.UUID `json:"request_id"`
		}
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatal(err)
		}
		if r.Method != http.MethodPost || r.Token != testToken || r.ContentType != "application/json" ||
			r.IdempotencyKey != body.RequestID.String() {
			t.Errorf("request %+v", r)
		}
		paths[r.Path] = body.RequestID
	}
	if paths["/v1/jobs/semantic"] != sem || paths["/v1/jobs/grammar"] != gram {
		t.Errorf("paths %v", paths)
	}
	for _, id := range []uuid.UUID{sem, gram} {
		j := readJob(t, pool, id)
		if j.Status != "running" || strp(j.LockedBy) != testInstance || j.LockedAt == nil || j.StartedAt == nil || j.TryCount != 0 {
			t.Errorf("sent job %+v", j)
		}
	}
	if j := readJob(t, pool, later); j.Status != "queued" {
		t.Errorf("delayed job %s", j.Status)
	}
	st := s.Stats()
	if st.Dispatched != 2 || st.Accepted != 2 || st.InFlight != 0 {
		t.Errorf("stats %+v", st)
	}
}

func TestDispatchPriorityOrderAndConcurrencyLimit(t *testing.T) {
	t.Parallel()
	f := newFakeAI(t, accepted202)
	s, _ := newDBService(t, f.URL)
	var want []uuid.UUID
	gen := enqueue(t, s, core.JobGenerateScenario, withPriority(core.PriorityGenerate))
	for i := 0; i < dispatchConcurrency; i++ {
		want = append(want, enqueue(t, s, core.JobEvaluateSemantic, withPriority(core.PrioritySemantic)))
	}
	dispatchOnce(s)
	var got []uuid.UUID
	for _, r := range f.requests() {
		var body struct {
			RequestID uuid.UUID `json:"request_id"`
		}
		_ = json.Unmarshal(r.Body, &body)
		got = append(got, body.RequestID)
	}
	if len(got) != dispatchConcurrency || slices.Contains(got, gen) {
		t.Fatalf("round sent %v; want the %d priority-3 jobs, generation waits", got, dispatchConcurrency)
	}
	dispatchOnce(s)
	if n := len(f.requests()); n != dispatchConcurrency+1 {
		t.Errorf("second round: %d requests total", n)
	}
}

func TestDispatchBusy(t *testing.T) {
	t.Parallel()
	f := newFakeAI(t, replyStatus(http.StatusTooManyRequests, map[string]string{"Retry-After": "30"}, `{"code":"queue_full"}`))
	s, pool := newDBService(t, f.URL)
	id := enqueue(t, s, core.JobEvaluateSemantic)
	before := time.Now()
	dispatchOnce(s)
	j := readJob(t, pool, id)
	if j.Status != "queued" || j.TryCount != 0 || j.LockedBy != nil || !strings.HasPrefix(strp(j.Error), "busy:") {
		t.Errorf("busy job %+v (error %s)", j, strp(j.Error))
	}
	if d := j.RunAfter.Sub(before); d < 25*time.Second || d > 35*time.Second {
		t.Errorf("run_after in %v, want ≈30 s (Retry-After)", d)
	}
	if p := s.pausedTypes(time.Now()); len(p) != 1 || p[0] != string(core.JobEvaluateSemantic) {
		t.Errorf("paused %v", p)
	}
	if s.BreakerState() != BreakerClosed {
		t.Error("429 must not open the breaker")
	}
	// Тип на паузе не захватывается, другие — да.
	exec(t, pool, `UPDATE ai_jobs SET run_after = now() WHERE id = $1`, id)
	f.setRespond(accepted202)
	other := enqueue(t, s, core.JobEvaluateGrammar)
	dispatchOnce(s)
	if readJob(t, pool, id).Status != "queued" || readJob(t, pool, other).Status != "running" {
		t.Error("paused type dispatched or other type blocked")
	}
}

func TestDispatchRejectedPayloadFailsWithoutRetry(t *testing.T) {
	t.Parallel()
	f := newFakeAI(t, replyStatus(http.StatusBadRequest, nil, `{"code":"validation","message":"schema_version"}`))
	s, pool := newDBService(t, f.URL)
	h := registerAll(s)
	id := enqueue(t, s, core.JobEvaluateSemantic, withRef(core.RefAttempt, ids.New()))
	dispatchOnce(s)
	j := readJob(t, pool, id)
	if j.Status != "failed" || j.TryCount != 1 || j.FinishedAt == nil || !strings.HasPrefix(strp(j.Error), CodeBadPayload+": HTTP 400") {
		t.Errorf("rejected job %+v (error %s)", j, strp(j.Error))
	}
	fc := h.lastFailure(t)
	if fc.Code != CodeBadPayload || fc.Job.ID != id || fc.Job.RefType != core.RefAttempt {
		t.Errorf("ApplyFailure %+v", fc)
	}
}

func TestDispatchServerErrorRetriesThenFails(t *testing.T) {
	t.Parallel()
	f := newFakeAI(t, replyStatus(http.StatusInternalServerError, nil, `Internal Server Error`))
	s, pool := newDBService(t, f.URL)
	setAI(s, func(ai *settings.AI) { ai.BreakerFailures = 100 })
	h := registerAll(s)
	id := enqueue(t, s, core.JobTTS, withMaxTries(2))

	dispatchOnce(s)
	j := readJob(t, pool, id)
	if j.Status != "queued" || j.TryCount != 1 || !strings.HasPrefix(strp(j.Error), CodeDispatchFailed) {
		t.Fatalf("after first 5xx: %+v", j)
	}
	if d := time.Until(j.RunAfter); d < 3*time.Second || d > 6*time.Second {
		t.Errorf("backoff %v, want ≈5 s", d)
	}
	exec(t, pool, `UPDATE ai_jobs SET run_after = now() WHERE id = $1`, id)
	dispatchOnce(s)
	j = readJob(t, pool, id)
	if j.Status != "failed" || j.TryCount != 2 {
		t.Fatalf("after last 5xx: %+v", j)
	}
	if fc := h.lastFailure(t); fc.Code != CodeDispatchFailed || !strings.Contains(fc.Message, "HTTP 500") {
		t.Errorf("ApplyFailure %+v", fc)
	}
}

func TestDispatchClientErrorCountsTry(t *testing.T) {
	t.Parallel()
	f := newFakeAI(t, replyStatus(http.StatusNotFound, nil, `{"detail":"Not Found"}`))
	s, pool := newDBService(t, f.URL)
	id := enqueue(t, s, core.JobEvaluateDialogue)
	dispatchOnce(s)
	if j := readJob(t, pool, id); j.Status != "queued" || j.TryCount != 1 {
		t.Errorf("404: %+v", j)
	}
	if s.BreakerState() != BreakerClosed {
		t.Error("4xx must not open the breaker")
	}
}

func TestDispatchAuthErrorMarksOutage(t *testing.T) {
	t.Parallel()
	f := newFakeAI(t, replyStatus(http.StatusUnauthorized, nil, `{"code":"unauthorized"}`))
	s, pool := newDBService(t, f.URL)
	id := enqueue(t, s, core.JobEvaluateSemantic)
	dispatchOnce(s)
	j := readJob(t, pool, id)
	if j.Status != "queued" || j.TryCount != 0 || !strings.HasPrefix(strp(j.Error), "auth:") {
		t.Errorf("401 job %+v", j)
	}
	if s.unavailSince.Load() == 0 {
		t.Error("token rejection must start the outage clock")
	}
	if s.BreakerState() != BreakerClosed {
		t.Error("401 must not open the breaker")
	}
	// Токен поправили — задача принята, авария закончилась.
	exec(t, pool, `UPDATE ai_jobs SET run_after = now() WHERE id = $1`, id)
	s.pausedUntil[typeIdx(core.JobEvaluateSemantic)].Store(0)
	f.setRespond(accepted202)
	dispatchOnce(s)
	if s.unavailSince.Load() != 0 {
		t.Error("accepted job must clear the outage clock")
	}
}

func TestDispatchUnreachable(t *testing.T) {
	t.Parallel()
	s, pool := newDBService(t, "") // connection refused
	setAI(s, func(ai *settings.AI) { ai.BreakerFailures = 1; ai.BreakerOpenSec = 3600 })
	h := registerAll(s)
	id := enqueue(t, s, core.JobEvaluateSemantic)
	dispatchOnce(s)
	j := readJob(t, pool, id)
	if j.Status != "queued" || j.TryCount != 0 || !strings.HasPrefix(strp(j.Error), "unreachable:") {
		t.Errorf("unreachable job %+v", j)
	}
	if s.BreakerState() != BreakerOpen || s.unavailSince.Load() == 0 {
		t.Errorf("breaker %s, outage start %d", s.BreakerState(), s.unavailSince.Load())
	}
	// Breaker open — задачи не захватываются вовсе.
	exec(t, pool, `UPDATE ai_jobs SET run_after = now() WHERE id = $1`, id)
	dispatchOnce(s)
	if j := readJob(t, pool, id); j.Status != "queued" || j.LockedBy != nil {
		t.Errorf("claimed while breaker open: %+v", j)
	}
	if _, f := h.counts(); f != 0 {
		t.Error("network errors must not fail the job")
	}
}

// Half-open: проба /v1/health, при успехе breaker закрывается и задачи уходят.
func TestDispatchHalfOpenProbe(t *testing.T) {
	t.Parallel()
	f := newFakeAI(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		if r.URL.Path == "/v1/health" {
			replyStatus(200, nil, `{"status":"ok"}`)(w, r, body)
			return
		}
		accepted202(w, r, body)
	})
	s, pool := newDBService(t, f.URL)
	setAI(s, func(ai *settings.AI) { ai.BreakerFailures = 1; ai.BreakerOpenSec = 1 })
	s.br.Failure()
	s.br.now = func() time.Time { return time.Now().Add(time.Hour) } // open-период истёк
	id := enqueue(t, s, core.JobTTS)
	dispatchOnce(s)
	seen := f.requests()
	if len(seen) != 2 || seen[0].Path != "/v1/health" || seen[1].Path != "/v1/jobs/tts" {
		t.Fatalf("requests %+v", seen)
	}
	if s.BreakerState() != BreakerClosed || s.unavailSince.Load() != 0 || readJob(t, pool, id).Status != "running" {
		t.Error("successful probe must close the breaker and dispatch")
	}
}

// Регрессия (ретрай под новым request_id): после failed-callback'а повтор уходит с НОВЫМ
// request_id в теле и Idempotency-Key, id задачи прежний.
func TestDispatchRetrySendsNewRequestID(t *testing.T) {
	t.Parallel()
	f := newFakeAI(t, accepted202)
	s, pool := newDBService(t, f.URL)
	registerAll(s)
	id := enqueue(t, s, core.JobEvaluateSemantic)
	dispatchOnce(s)
	if r := mustCallback(t, s, failedCallback(id, core.JobEvaluateSemantic, "llm_timeout", true)); r.Duplicate {
		t.Fatal("first failure reported as duplicate")
	}
	j := readJob(t, pool, id)
	if j.RequestID == id.String() || j.Status != "queued" {
		t.Fatalf("retry kept request_id %s (status %s)", j.RequestID, j.Status)
	}
	exec(t, pool, `UPDATE ai_jobs SET run_after = now() WHERE id = $1`, id)
	dispatchOnce(s)
	seen := f.requests()
	last := seen[len(seen)-1]
	var body struct {
		RequestID string `json:"request_id"`
		Text      string `json:"text"`
	}
	_ = json.Unmarshal(last.Body, &body)
	if body.RequestID != j.RequestID || last.IdempotencyKey != j.RequestID || body.Text != "Проверка связи" {
		t.Errorf("retry sent request_id=%s key=%s text=%q, want %s", body.RequestID, last.IdempotencyKey, body.Text, j.RequestID)
	}
	if j2 := readJob(t, pool, id); j2.Status != "running" || j2.ID != id {
		t.Errorf("retry dispatch: %+v", j2)
	}
}

// Запрос в полёте, а callback уже закрыл задачу: итог отправки не должен её трогать.
func TestDispatchOutcomeAfterCallbackIsNoop(t *testing.T) {
	t.Parallel()
	s, pool := newDBService(t, "")
	registerAll(s)
	id := enqueue(t, s, core.JobTTS)
	jobs, err := s.claim(context.Background(), 1)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim: %v %v", jobs, err)
	}
	mustCallback(t, s, okCallback(id, core.JobTTS))
	s.applyOutcome(context.Background(), jobs[0], sendOutcome{class: sendServerErr, status: 500, detail: "late"})
	s.applyOutcome(context.Background(), jobs[0], sendOutcome{class: sendNetErr, detail: "late"})
	if j := readJob(t, pool, id); j.Status != "done" || j.TryCount != 0 {
		t.Errorf("late send outcome changed a finished job: %+v", j)
	}
}

func TestRequeueOwnOnStart(t *testing.T) {
	t.Parallel()
	s, pool := newDBService(t, "")
	mine := enqueue(t, s, core.JobTTS)
	theirs := enqueue(t, s, core.JobTTS)
	markRunning(t, pool, mine, time.Minute, time.Minute)
	markRunning(t, pool, theirs, time.Minute, time.Minute)
	exec(t, pool, `UPDATE ai_jobs SET locked_by = 'other-instance' WHERE id = $1`, theirs)
	s.requeueOwn(context.Background())
	if j := readJob(t, pool, mine); j.Status != "queued" || j.TryCount != 0 || j.LockedBy != nil {
		t.Errorf("own running job after restart: %+v", j)
	}
	if j := readJob(t, pool, theirs); j.Status != "running" {
		t.Errorf("foreign running job touched: %s", j.Status)
	}
}

func TestUUIDv7Floor(t *testing.T) {
	t.Parallel()
	now := time.Now()
	floor := uuidV7Floor(now.Add(-time.Second))
	if floor.Version() != 7 || floor.Variant() != uuid.RFC4122 {
		t.Errorf("floor %s: version %d variant %v", floor, floor.Version(), floor.Variant())
	}
	id := ids.New()
	if strings.Compare(floor.String(), id.String()) >= 0 {
		t.Errorf("floor %s is not below fresh id %s", floor, id)
	}
	if future := uuidV7Floor(now.Add(time.Hour)); strings.Compare(future.String(), id.String()) <= 0 {
		t.Errorf("future floor %s is not above %s", future, id)
	}
	if z := uuidV7Floor(time.Unix(-100, 0)); z.String()[:12] != "00000000-000" {
		t.Errorf("pre-epoch floor %s", z)
	}
}
