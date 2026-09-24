package main

import (
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/callbacks"
	"lct/gocore/internal/gen/components"
)

// jobKind — тип асинхронной задачи (сегмент пути /v1/jobs/{kind}).
type jobKind uint8

const (
	kindGrammar jobKind = iota
	kindSemantic
	kindDialogue
	kindGenerate
	kindTTS
	numKinds
)

// Полосы исполнения — как в настоящем ai-service: LanguageTool, LLM (одна модель
// на все LLM-задачи), TTS. Грамматика не стоит в очереди за семантикой.
const (
	laneLT = iota
	laneLLM
	laneTTS
	numLanes
)

var (
	kindNames = [numKinds]string{"grammar", "semantic", "dialogue", "generate", "tts"}
	kindTypes = [numKinds]components.JobType{components.EvaluateGrammar, components.EvaluateSemantic,
		components.EvaluateDialogue, components.GenerateScenario, components.Tts}
	// Базовые задержки обработки (×FAKEAI_SPEED) — порядок величин настоящих движков.
	kindDelay = [numKinds]time.Duration{300 * time.Millisecond, 1500 * time.Millisecond,
		2000 * time.Millisecond, 3000 * time.Millisecond, 200 * time.Millisecond}
	kindLane = [numKinds]int{laneLT, laneLLM, laneLLM, laneLLM, laneTTS}
	// Внутри полосы при равном priority оценка идёт раньше генерации (контракт:
	// dialog_turn > evaluate_* > generate).
	kindRank = [numKinds]int{0, 0, 0, 1, 0}
	// Код ошибки для FAKEAI_FAIL_EVERY — как отказал бы соответствующий движок.
	kindFailCode = [numKinds]callbacks.AiJobErrorCode{callbacks.LtUnavailable, callbacks.LlmTimeout,
		callbacks.LlmTimeout, callbacks.LlmTimeout, callbacks.TtsFailed}
)

func kindByName(s string) (jobKind, bool) {
	for k, n := range kindNames {
		if n == s {
			return jobKind(k), true
		}
	}
	return 0, false
}

type jobState uint8

const (
	stateQueued jobState = iota
	stateRunning
	stateDone
)

type job struct {
	id       uuid.UUID
	attempt  uuid.UUID // uuid.Nil у generate/tts
	kind     jobKind
	priority int
	seq      uint64
	payload  any // *aiservice.XxxJobRequest; обнуляется после обработки
	enqueued time.Time
	state    jobState
	result   []byte // закодированный AiResult — для повторной отправки при дубле
	doneAt   time.Time
	index    int // позиция в куче полосы
}

// jobHeap — очередь полосы: priority (1 — важнее) → тип → FIFO.
type jobHeap []*job

func jobLess(a, b *job) bool {
	if a.priority != b.priority {
		return a.priority < b.priority
	}
	if kindRank[a.kind] != kindRank[b.kind] {
		return kindRank[a.kind] < kindRank[b.kind]
	}
	return a.seq < b.seq
}

func (h jobHeap) Len() int           { return len(h) }
func (h jobHeap) Less(i, j int) bool { return jobLess(h[i], h[j]) }
func (h jobHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i]; h[i].index = i; h[j].index = j }
func (h *jobHeap) Push(x any)        { j := x.(*job); j.index = len(*h); *h = append(*h, j) }
func (h *jobHeap) Pop() any {
	old := *h
	n := len(old)
	j := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	j.index = -1
	return j
}

type laneState struct {
	heap    jobHeap
	ready   chan struct{} // по токену на задачу в куче: воркер спит на канале, а не опрашивает
	workers int
	running int
}

// queue — реестр задач (идемпотентность по request_id) + полосы исполнения.
// Один мьютекс на всё: операции O(log n) над сотней-другой задач, конкуренции нет.
type queue struct {
	cfg   config
	log   *slog.Logger
	cb    *deliverer
	tts   *ttsStore
	speed float64

	mu        sync.Mutex
	jobs      map[uuid.UUID]*job
	lanes     [numLanes]laneState
	pending   int
	pendingBy [numKinds]int
	seq       uint64

	processed atomic.Int64 // для FAKEAI_FAIL_EVERY
}

func newQueue(cfg config, log *slog.Logger, cb *deliverer, tts *ttsStore) *queue {
	q := &queue{cfg: cfg, log: log, cb: cb, tts: tts, speed: cfg.speed, jobs: make(map[uuid.UUID]*job, 256)}
	workers := [numLanes]int{laneLT: cfg.ltWorkers, laneLLM: cfg.llmWorkers, laneTTS: cfg.ttsWorkers}
	for i := range q.lanes {
		// Ёмкость = лимит очереди: в полосе не бывает больше задач, чем всего в очереди,
		// поэтому отправка токена никогда не блокирует.
		q.lanes[i].ready = make(chan struct{}, cfg.queueMax)
		q.lanes[i].workers = workers[i]
	}
	return q
}

func (q *queue) start(ctx context.Context, wg *sync.WaitGroup) {
	for l := range q.lanes {
		for range q.lanes[l].workers {
			wg.Go(func() { q.worker(ctx, l) })
		}
	}
	wg.Go(func() { q.sweeper(ctx) })
}

// submitResult — ответ на POST /v1/jobs/*.
type submitResult struct {
	status   aiservice.JobAcceptedStatus
	position int
	estSec   int
	resend   []byte // задача уже выполнена — callback уйдёт повторно
}

// submit ставит задачу или отвечает по уже известной (идемпотентность по request_id).
// full=true — очередь полна (429), retryAfter — через сколько секунд повторить.
func (q *queue) submit(kind jobKind, id, attempt uuid.UUID, prio int, payload any) (res submitResult, retryAfter int, full bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if j, ok := q.jobs[id]; ok {
		switch j.state {
		case stateQueued:
			pos := q.aheadLocked(j)
			return submitResult{status: aiservice.Queued, position: pos, estSec: q.estLocked(j.kind, pos)}, 0, false
		case stateRunning:
			return submitResult{status: aiservice.Running, position: 0, estSec: q.secs(kindDelay[j.kind])}, 0, false
		default:
			return submitResult{status: aiservice.Done, resend: j.result}, 0, false
		}
	}
	if q.pending >= q.cfg.queueMax {
		lane := kindLane[kind]
		wait := q.estLocked(kind, len(q.lanes[lane].heap))
		return submitResult{}, min(max(wait, 1), 30), true
	}
	q.seq++
	j := &job{id: id, attempt: attempt, kind: kind, priority: prio, seq: q.seq, payload: payload,
		enqueued: time.Now(), state: stateQueued}
	q.jobs[id] = j
	l := &q.lanes[kindLane[kind]]
	heap.Push(&l.heap, j)
	q.pending++
	q.pendingBy[kind]++
	pos := q.aheadLocked(j)
	select {
	case l.ready <- struct{}{}:
	default:
		// Недостижимо при ёмкости = queueMax; если всё же — воркер подберёт задачу со следующим токеном.
		q.log.Warn("канал готовности полосы переполнен", "kind", kindNames[kind])
	}
	return submitResult{status: aiservice.Queued, position: pos, estSec: q.estLocked(kind, pos)}, 0, false
}

// aheadLocked — сколько задач полосы будут взяты раньше j (O(n), n ≤ queueMax).
func (q *queue) aheadLocked(j *job) int {
	n := 0
	for _, o := range q.lanes[kindLane[j.kind]].heap {
		if o != j && jobLess(o, j) {
			n++
		}
	}
	return n
}

// estLocked — оценка ожидания новой задачи: очередь впереди + занятые воркеры,
// поделённые на параллелизм полосы, плюс собственная обработка.
func (q *queue) estLocked(kind jobKind, ahead int) int {
	l := &q.lanes[kindLane[kind]]
	slots := float64(ahead+l.running) / float64(l.workers)
	return q.secs(time.Duration((slots + 1) * float64(kindDelay[kind])))
}

func (q *queue) secs(d time.Duration) int {
	return int(math.Ceil(d.Seconds() * q.speed))
}

func (q *queue) scaled(d time.Duration) time.Duration {
	return time.Duration(float64(d) * q.speed)
}

// queueSnapshot — для GET /v1/queue.
func (q *queue) snapshot() (pending map[string]int, running int, semEst int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	pending = make(map[string]int, numKinds)
	for k := range numKinds {
		pending[string(kindTypes[k])] = q.pendingBy[k]
	}
	for i := range q.lanes {
		running += q.lanes[i].running
	}
	return pending, running, q.estLocked(kindSemantic, len(q.lanes[laneLLM].heap))
}

func (q *queue) worker(ctx context.Context, lane int) {
	l := &q.lanes[lane]
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.ready:
		}
		q.mu.Lock()
		if l.heap.Len() == 0 {
			q.mu.Unlock()
			continue
		}
		j := heap.Pop(&l.heap).(*job)
		j.state = stateRunning
		q.pending--
		q.pendingBy[j.kind]--
		l.running++
		q.mu.Unlock()

		body, ok := q.process(ctx, j)

		q.mu.Lock()
		l.running--
		switch {
		case body == nil:
			// Остановка сервиса посреди обработки: задачу забываем, go-core вернёт её reaper'ом.
			delete(q.jobs, j.id)
		case ok:
			j.state, j.result, j.doneAt, j.payload = stateDone, body, time.Now(), nil
		default:
			// Отказ retryable: повтор с тем же request_id должен считать заново, а не
			// получить тот же отказ из кэша.
			delete(q.jobs, j.id)
		}
		q.mu.Unlock()
		if body != nil {
			q.cb.enqueue(delivery{id: j.id, body: body})
		}
	}
}

// process — вычисление результата, «время работы движка», кодирование callback'а.
// nil — обработка прервана остановкой; ok=false — результат status=failed.
func (q *queue) process(ctx context.Context, j *job) (body []byte, ok bool) {
	start := time.Now()
	wait := start.Sub(j.enqueued)
	res := q.compute(j)

	if !sleepCtx(ctx, q.scaled(kindDelay[j.kind])-time.Since(start)) {
		return nil, false
	}
	res.Engine.DurationMs = int(time.Since(start).Milliseconds())
	res.Engine.QueueWaitMs = ptr(int(wait.Milliseconds()))

	body, err := json.Marshal(&res)
	if err != nil {
		// Сгенерированные типы кодируются всегда; на всякий случай — честный отказ.
		q.log.Error("кодирование результата", "request_id", j.id, "err", err)
		res = failed(res, callbacks.Internal, "encode: "+err.Error())
		body, _ = json.Marshal(&res)
	}
	ok = res.Status == callbacks.Ok
	q.log.Info("задача выполнена", "request_id", j.id, "type", kindTypes[j.kind], "status", res.Status,
		"duration_ms", res.Engine.DurationMs, "queue_wait_ms", wait.Milliseconds())
	return body, ok
}

// compute — чистая логика по типу задачи (без сна и сети). Паника в эвристике
// превращается в status=failed/internal, а не роняет воркер.
func (q *queue) compute(j *job) (res callbacks.AiResult) {
	res = callbacks.AiResult{SchemaVersion: components.N1, RequestId: j.id, Type: kindTypes[j.kind], Status: callbacks.Ok}
	if j.attempt != uuid.Nil {
		res.AttemptId = ptr(j.attempt)
	}
	defer func() {
		if p := recover(); p != nil {
			q.log.Error("паника при обработке задачи", "request_id", j.id, "panic", p)
			res = failed(res, callbacks.Internal, fmt.Sprint("panic: ", p))
		}
	}()

	if n := q.cfg.failEvery; n > 0 && q.processed.Add(1)%int64(n) == 0 {
		return failed(res, kindFailCode[j.kind], "имитация отказа движка (FAKEAI_FAIL_EVERY)")
	}

	switch p := j.payload.(type) {
	case *aiservice.GrammarJobRequest:
		g := checkGrammar(p)
		res.Grammar = &g
		res.Engine = components.Engine{LtVersion: ptr("fake-lt-1"), RulesVersion: ptr("fake-rules-1")}
	case *aiservice.SemanticJobRequest:
		s := evalSemantic(p)
		res.Semantic = &s
		res.Engine = llmEngine("fake-sem-v1", 900, 160)
	case *aiservice.DialogueJobRequest:
		d := evalDialogue(p)
		res.Dialogue = &d
		res.Engine = llmEngine("fake-dlg-v1", 400+120*len(p.Transcript), 260)
	case *aiservice.GenerateJobRequest:
		s := generateScenario(p)
		res.Scenario = &s
		res.Engine = llmEngine("fake-gen-v1", 1200, 900)
	case *aiservice.TtsJobRequest:
		voice := deref(p.Voice)
		if voice == "" {
			voice = defaultVoice
		}
		rate := float32(1)
		if p.Rate != nil && *p.Rate > 0 {
			rate = *p.Rate
		}
		rel := hashPath(p.TextHash)
		dur, err := q.tts.write(rel, p.Text, true)
		if err != nil {
			q.log.Error("tts: запись файла", "request_id", j.id, "err", err)
			return failed(res, callbacks.TtsFailed, "не удалось записать файл озвучки")
		}
		res.Tts = &components.TtsResult{TextHash: p.TextHash, FilePath: rel, DurationMs: dur, Voice: ptr(voice), Rate: ptr(rate)}
		res.Engine = components.Engine{TtsVersion: ptr("fake-tts")}
	default:
		return failed(res, callbacks.BadPayload, "неизвестный тип задачи")
	}
	return res
}

func llmEngine(prompt string, tokIn, tokOut int) components.Engine {
	return components.Engine{LlmModel: ptr(modelLLM), PromptVersion: ptr(prompt), RulesVersion: ptr("fake-rules-1"),
		TokensIn: ptr(tokIn), TokensOut: ptr(tokOut)}
}

// failed — конверт status=failed: ровно поле error, без полей результата (инвариант go-internal).
func failed(res callbacks.AiResult, code callbacks.AiJobErrorCode, msg string) callbacks.AiResult {
	res.Status = callbacks.Failed
	res.Grammar, res.Semantic, res.Dialogue, res.Scenario, res.Tts = nil, nil, nil, nil, nil
	res.Error = &callbacks.AiJobError{Code: code, Message: msg, Retryable: code != callbacks.BadPayload}
	return res
}

// sweeper забывает выполненные задачи старше FAKEAI_DONE_TTL: память ограничена,
// а переотправка через столько времени — уже работа reaper'а go-core (посчитаем заново).
func (q *queue) sweeper(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			q.mu.Lock()
			for id, j := range q.jobs {
				if j.state == stateDone && now.Sub(j.doneAt) > q.cfg.doneTTL {
					delete(q.jobs, id)
				}
			}
			q.mu.Unlock()
		}
	}
}

// sleepCtx — «работа движка»; false — сервис останавливается.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
