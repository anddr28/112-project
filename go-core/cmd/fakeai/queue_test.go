package main

import (
	"container/heap"
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/callbacks"
	"lct/gocore/internal/gen/components"
)

// bareQueue — очередь без воркеров и без запущенной доставки (unit-тесты планирования).
func bareQueue(t *testing.T, mut func(*config)) *queue {
	t.Helper()
	cfg := testConfig(t)
	cfg.callbackURL = "http://127.0.0.1:1/unused"
	if mut != nil {
		mut(&cfg)
	}
	log := discardLog()
	return newQueue(cfg, log, newDeliverer(cfg, log), &ttsStore{root: cfg.ttsDir})
}

func TestKindByName(t *testing.T) {
	t.Parallel()
	for i, n := range kindNames {
		k, ok := kindByName(n)
		if !ok || k != jobKind(i) {
			t.Errorf("kindByName(%q) = %d %v", n, k, ok)
		}
	}
	for _, n := range []string{"", "Grammar", "evaluate_grammar", "grammar/"} {
		if _, ok := kindByName(n); ok {
			t.Errorf("kindByName(%q) принят", n)
		}
	}
}

// Порядок полосы: priority (1 — важнее) → оценка раньше генерации → FIFO.
func TestLaneOrderingAndPositions(t *testing.T) {
	t.Parallel()
	q := bareQueue(t, nil)
	type sub struct {
		name string
		kind jobKind
		prio int
		pos  int // позиция в ответе 202
	}
	subs := []sub{
		{"A semantic p3", kindSemantic, 3, 0},
		{"B generate p3", kindGenerate, 3, 1}, // за A: генерация после оценки
		{"C dialogue p3", kindDialogue, 3, 1}, // перед B, за A (FIFO среди оценок)
		{"D generate p1", kindGenerate, 1, 0}, // приоритет важнее типа
		{"E semantic p5", kindSemantic, 5, 4},
	}
	ids := map[uuid.UUID]string{}
	for _, s := range subs {
		id := uuid.New()
		ids[id] = s.name
		res, _, full := q.submit(s.kind, id, uuid.New(), s.prio, nil)
		if full || res.status != aiservice.Queued || res.position != s.pos {
			t.Fatalf("%s: %+v full=%v, ждали позицию %d", s.name, res, full, s.pos)
		}
	}
	// Грамматика — другая полоса: LLM-очередь её не задерживает.
	if res, _, _ := q.submit(kindGrammar, uuid.New(), uuid.New(), 9, nil); res.position != 0 {
		t.Fatalf("grammar позиция %d", res.position)
	}
	var order []string
	l := &q.lanes[laneLLM]
	for l.heap.Len() > 0 {
		order = append(order, ids[heap.Pop(&l.heap).(*job).id])
	}
	want := []string{"D generate p1", "A semantic p3", "C dialogue p3", "B generate p3", "E semantic p5"}
	for i := range want {
		if i >= len(order) || order[i] != want[i] {
			t.Fatalf("порядок %v, ждали %v", order, want)
		}
	}
	pending, _, _ := q.snapshot()
	if pending[string(components.EvaluateGrammar)] != 1 || pending[string(components.GenerateScenario)] != 2 {
		t.Fatalf("pending %v", pending)
	}
}

func TestSubmitStates(t *testing.T) {
	t.Parallel()
	q := bareQueue(t, func(c *config) { c.speed = 1 })
	id := uuid.New()
	if res, _, _ := q.submit(kindSemantic, id, uuid.New(), 3, nil); res.status != aiservice.Queued || res.estSec != 2 {
		t.Fatalf("queued: %+v (est: 1 слот × 1.5 с → 2)", res)
	}
	q.mu.Lock()
	q.jobs[id].state = stateRunning
	q.mu.Unlock()
	if res, _, _ := q.submit(kindSemantic, id, uuid.New(), 3, nil); res.status != aiservice.Running || res.position != 0 || res.estSec != 2 {
		t.Fatalf("running: %+v", res)
	}
	q.mu.Lock()
	q.jobs[id].state, q.jobs[id].result = stateDone, []byte(`{"x":1}`)
	q.mu.Unlock()
	if res, _, _ := q.submit(kindSemantic, id, uuid.New(), 3, nil); res.status != aiservice.Done || string(res.resend) != `{"x":1}` {
		t.Fatalf("done: %+v", res)
	}
	// Дубли не увеличивают очередь.
	if pending, _, _ := q.snapshot(); pending[string(components.EvaluateSemantic)] != 1 {
		t.Fatalf("pending %v", pending)
	}
}

func TestQueueFullRetryAfter(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		speed float64
		want  int
	}{
		{0, 1},    // мгновенная обработка — всё равно не меньше 1 с
		{1, 1},    // (1 впереди / 2 воркера + 1) × 0.3 с = 0.45 → 1
		{100, 30}, // 45 с → потолок 30
		{10, 5},   // 4.5 → 5
	} {
		q := bareQueue(t, func(cfg *config) { cfg.queueMax = 1; cfg.speed = c.speed })
		if _, _, full := q.submit(kindGrammar, uuid.New(), uuid.New(), 5, nil); full {
			t.Fatal("первая задача отклонена")
		}
		_, ra, full := q.submit(kindGrammar, uuid.New(), uuid.New(), 5, nil)
		if !full || ra != c.want {
			t.Errorf("speed %v: full=%v retryAfter=%d, ждали %d", c.speed, full, ra, c.want)
		}
	}
}

func TestComputeRecoversFromPanic(t *testing.T) {
	t.Parallel()
	q := bareQueue(t, nil)
	// text_hash короче 2 символов роняет hashPath — эвристика упала, воркер нет.
	j := &job{id: uuid.New(), kind: kindTTS, payload: &aiservice.TtsJobRequest{Text: "Алло", TextHash: ""}}
	res := q.compute(j)
	body, _ := json.Marshal(&res)
	got := checkEnvelope(t, body, components.Tts)
	if got.Status != callbacks.Failed || got.Error.Code != callbacks.Internal || !got.Error.Retryable {
		t.Fatalf("panic → %+v", got.Error)
	}
}

func TestComputeUnknownPayload(t *testing.T) {
	t.Parallel()
	q := bareQueue(t, nil)
	res := q.compute(&job{id: uuid.New(), attempt: uuid.New(), kind: kindGrammar, payload: "мусор"})
	body, _ := json.Marshal(&res)
	got := checkEnvelope(t, body, components.EvaluateGrammar)
	if got.Status != callbacks.Failed || got.Error.Code != callbacks.BadPayload || got.Error.Retryable {
		t.Fatalf("неизвестный payload → %+v", got.Error)
	}
}

func TestFailedEnvelope(t *testing.T) {
	t.Parallel()
	full := callbacks.AiResult{
		SchemaVersion: components.N1, RequestId: uuid.New(), Type: components.EvaluateGrammar, Status: callbacks.Ok,
		Grammar: &components.GrammarResult{}, Semantic: &components.SemanticResult{}, Dialogue: &components.DialogueResult{},
		Scenario: &components.ScenarioResult{}, Tts: &components.TtsResult{},
	}
	for code, retryable := range map[callbacks.AiJobErrorCode]bool{
		callbacks.BadPayload: false, callbacks.LlmTimeout: true, callbacks.Internal: true, callbacks.TtsFailed: true,
		callbacks.LtUnavailable: true,
	} {
		res := failed(full, code, "msg")
		if res.Status != callbacks.Failed || res.Grammar != nil || res.Semantic != nil || res.Dialogue != nil ||
			res.Scenario != nil || res.Tts != nil || res.Error == nil || res.Error.Code != code ||
			res.Error.Retryable != retryable || res.Error.Message != "msg" {
			t.Errorf("failed(%s): %+v", code, res)
		}
	}
	if full.Grammar == nil {
		t.Fatal("failed изменил исходный результат")
	}
}

// Остановка посреди обработки: задача забывается (её вернёт reaper go-core), callback не уходит.
func TestWorkerShutdownMidJob(t *testing.T) {
	t.Parallel()
	q := bareQueue(t, func(c *config) { c.speed = 1000 }) // 300 с «работы»
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	q.start(ctx, &wg)
	id := uuid.New()
	req := new(aiservice.GrammarJobRequest)
	if err := decodeInto(mustJSON(t, grammarPayload()), req); err != nil {
		t.Fatal(err)
	}
	q.submit(kindGrammar, id, uuid.New(), 5, req)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, running, _ := q.snapshot(); running == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("задача не взята в работу")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	q.mu.Lock()
	_, known := q.jobs[id]
	q.mu.Unlock()
	if known || len(q.cb.ch) != 0 {
		t.Fatalf("после остановки: known=%v callbacks=%d", known, len(q.cb.ch))
	}
	if _, running, _ := q.snapshot(); running != 0 {
		t.Fatalf("running %d", running)
	}
}

// Воркеры полосы работают параллельно и не теряют задачи при конкурентной постановке.
func TestConcurrentSubmitsAllDelivered(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, nil)
	const n = 40
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			if r := h.postJSON(t, "/v1/jobs/grammar", grammarPayload()); r.status != 202 {
				t.Errorf("submit: %d", r.status)
			}
		})
	}
	wg.Wait()
	seen := map[uuid.UUID]bool{}
	for range n {
		res := checkEnvelope(t, h.sink.next(t, cbWait), components.EvaluateGrammar)
		if seen[res.RequestId] {
			t.Fatalf("дубль callback'а %s", res.RequestId)
		}
		seen[res.RequestId] = true
	}
	h.sink.none(t, 50*time.Millisecond)
}
