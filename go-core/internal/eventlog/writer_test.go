package eventlog

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeStore — имитация attempt_events для писателя без БД: уникальность (attempt, clientSeq),
// атомарность транзакции (ошибка — ничего не записано), lastSeq попытки.
type fakeStore struct {
	mu      sync.Mutex
	next    int64
	rows    map[eventKey]int64
	max     map[uuid.UUID]int64
	calls   []int // число запросов в каждой транзакции
	fail    map[uuid.UUID]bool
	gate    chan struct{} // не nil — первая транзакция ждёт закрытия
	entered chan struct{} // закрывается, когда первая транзакция началась
	once    sync.Once
}

var errFakeFK = errors.New("fake: attempt does not exist")

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[eventKey]int64{}, max: map[uuid.UUID]int64{}, fail: map[uuid.UUID]bool{},
		entered: make(chan struct{})}
}

func (f *fakeStore) exec(_ context.Context, reqs []*request) error {
	first := false
	f.once.Do(func() { first = true; close(f.entered) })
	if first && f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, len(reqs))
	for _, r := range reqs {
		if f.fail[r.attemptID] {
			for _, r := range reqs {
				clear(r.ids)
				r.lastSeq = 0
			}
			return errFakeFK
		}
	}
	for _, r := range reqs {
		r.ids = make([]int64, len(r.events))
		for i, e := range r.events {
			k := eventKey{r.attemptID, e.ClientSeq}
			if _, dup := f.rows[k]; dup {
				continue
			}
			f.next++
			f.rows[k] = f.next
			r.ids[i] = f.next
			if e.ClientSeq > f.max[r.attemptID] {
				f.max[r.attemptID] = e.ClientSeq
			}
		}
	}
	for _, r := range reqs {
		r.lastSeq = f.max[r.attemptID]
	}
	return nil
}

func (f *fakeStore) callSizes() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.calls...)
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newFakeWriter(t *testing.T, f *fakeStore) *Writer {
	t.Helper()
	w := NewWriter(nil, quietLog())
	w.exec = f.exec
	return w
}

func startWriter(t *testing.T, w *Writer) {
	t.Helper()
	w.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := w.Stop(ctx); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
}

func inputs(seqs ...int64) []Input {
	out := make([]Input, len(seqs))
	for i, s := range seqs {
		out[i] = Input{ClientSeq: s, Type: "field_changed", Payload: emptyObject, At: testNow}
	}
	return out
}

func rangeInputs(from int64, n int) []Input {
	seqs := make([]int64, n)
	for i := range seqs {
		seqs[i] = from + int64(i)
	}
	return inputs(seqs...)
}

func appendCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestWriter_AppendAndDuplicates(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	w := newFakeWriter(t, f)
	startWriter(t, w)
	a := uuid.New()
	ctx := appendCtx(t)

	res, err := w.Append(ctx, a, inputs(1, 2, 3))
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != 3 || res.LastSeq != 3 || len(res.Inserted) != 3 {
		t.Fatalf("first = %+v", res)
	}
	for i, ev := range res.Inserted {
		if ev.ClientSeq != i+1 || ev.Id == 0 || string(ev.Type) != "field_changed" || !ev.At.Equal(testNow) {
			t.Fatalf("inserted[%d] = %+v", i, ev)
		}
	}

	// Повтор с перекрытием: новое только 4.
	res, err = w.Append(ctx, a, inputs(2, 3, 4))
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != 1 || res.Inserted[0].ClientSeq != 4 || res.LastSeq != 4 {
		t.Fatalf("overlap = %+v", res)
	}

	// Дубль внутри одного батча: первое вхождение.
	res, err = w.Append(ctx, a, inputs(10, 10, 11))
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != 2 {
		t.Fatalf("in-batch dup = %+v", res)
	}
}

// Регрессия (review: lastSeq): ответ несёт максимальный сохранённый clientSeq попытки, а не
// максимум текущего батча — и для батча, где всё оказалось дублями.
func TestWriter_LastSeqIsAttemptMax(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	w := newFakeWriter(t, f)
	startWriter(t, w)
	a, b := uuid.New(), uuid.New()
	ctx := appendCtx(t)

	if res, err := w.Append(ctx, a, inputs(5)); err != nil || res.LastSeq != 5 {
		t.Fatalf("[5] -> %+v %v", res, err)
	}
	res, err := w.Append(ctx, a, inputs(3))
	if err != nil || res.Accepted != 1 || res.LastSeq != 5 {
		t.Fatalf("[3] -> %+v %v, want accepted 1 lastSeq 5", res, err)
	}
	res, err = w.Append(ctx, a, inputs(3))
	if err != nil || res.Accepted != 0 || res.LastSeq != 5 {
		t.Fatalf("[3] again -> %+v %v, want accepted 0 lastSeq 5", res, err)
	}
	// Другая попытка — своя граница.
	if res, err := w.Append(ctx, b, inputs(2)); err != nil || res.LastSeq != 2 {
		t.Fatalf("other attempt -> %+v %v", res, err)
	}
}

func TestWriter_EmptyAndLifecycleErrors(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	w := newFakeWriter(t, f)
	ctx := appendCtx(t)

	res, err := w.Append(ctx, uuid.New(), nil)
	if err != nil || res.Inserted == nil || res.Accepted != 0 {
		t.Fatalf("empty append = %+v %v", res, err)
	}
	if _, err := w.Append(ctx, uuid.New(), inputs(1)); !errors.Is(err, errNotStarted) {
		t.Fatalf("not started: err = %v", err)
	}
	w.Start(context.Background())
	w.Start(context.Background()) // повторный Start — no-op
	if _, err := w.Append(ctx, uuid.New(), inputs(1)); err != nil {
		t.Fatal(err)
	}
	if err := w.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Stop(ctx); err != nil { // идемпотентен
		t.Fatal(err)
	}
	if _, err := w.Append(ctx, uuid.New(), inputs(1)); !errors.Is(err, ErrStopped) {
		t.Fatalf("after stop: err = %v", err)
	}
}

func TestWriter_StopWithoutStart(t *testing.T) {
	t.Parallel()
	w := newFakeWriter(t, newFakeStore())
	ctx := appendCtx(t)
	if err := w.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(ctx, uuid.New(), inputs(1)); !errors.Is(err, ErrStopped) {
		t.Fatalf("err = %v", err)
	}
}

func TestWriter_ContextCancelStopsWriter(t *testing.T) {
	t.Parallel()
	w := newFakeWriter(t, newFakeStore())
	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	if _, err := w.Append(appendCtx(t), uuid.New(), inputs(1)); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-w.done:
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not stop on ctx cancel")
	}
	if _, err := w.Append(appendCtx(t), uuid.New(), inputs(2)); !errors.Is(err, ErrStopped) {
		t.Fatalf("err = %v", err)
	}
}

// waitQueued ждёт, пока в очереди наберётся rows строк.
func waitQueued(t *testing.T, w *Writer, rows int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for w.queuedRows() < rows {
		if time.Now().After(deadline) {
			t.Fatalf("queued = %d, want %d", w.queuedRows(), rows)
		}
		time.Sleep(time.Millisecond)
	}
}

type appendResult struct {
	res Result
	err error
}

func appendAsync(ctx context.Context, w *Writer, a uuid.UUID, in []Input) <-chan appendResult {
	ch := make(chan appendResult, 1)
	go func() {
		res, err := w.Append(ctx, a, in)
		ch <- appendResult{res, err}
	}()
	return ch
}

func TestWriter_GroupCommit(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	f.gate = make(chan struct{})
	w := newFakeWriter(t, f)
	startWriter(t, w)
	ctx := appendCtx(t)

	// Первая транзакция «висит» — за это время копятся запросы многих студентов.
	first := appendAsync(ctx, w, uuid.New(), inputs(1))
	<-f.entered
	const n = 30
	pending := make([]<-chan appendResult, n)
	for i := range pending {
		pending[i] = appendAsync(ctx, w, uuid.New(), inputs(1, 2))
	}
	waitQueued(t, w, 2*n)
	close(f.gate)

	for _, ch := range append(pending, first) {
		r := <-ch
		if r.err != nil {
			t.Fatal(r.err)
		}
	}
	sizes := f.callSizes()
	if len(sizes) != 2 || sizes[0] != 1 || sizes[1] != n {
		t.Fatalf("transactions = %v, want [1 %d] (group commit)", sizes, n)
	}
}

// Ошибка чужой попытки (FK) не валит события остальных: батч переписывается по запросам.
func TestWriter_PerRequestRetryOnGroupFailure(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	f.gate = make(chan struct{})
	bad := uuid.New()
	f.fail[bad] = true
	w := newFakeWriter(t, f)
	startWriter(t, w)
	ctx := appendCtx(t)

	first := appendAsync(ctx, w, uuid.New(), inputs(1))
	<-f.entered
	good1 := appendAsync(ctx, w, uuid.New(), inputs(1))
	badCh := appendAsync(ctx, w, bad, inputs(1))
	good2 := appendAsync(ctx, w, uuid.New(), inputs(1, 2))
	waitQueued(t, w, 4)
	close(f.gate)

	if r := <-first; r.err != nil {
		t.Fatal(r.err)
	}
	for _, ch := range []<-chan appendResult{good1, good2} {
		r := <-ch
		if r.err != nil || r.res.Accepted == 0 {
			t.Fatalf("good request failed: %+v", r)
		}
	}
	if r := <-badCh; !errors.Is(r.err, errFakeFK) {
		t.Fatalf("bad request err = %v", r.err)
	}
	// 1 (первая) + 1 (групповая, упала) + 3 (по одному).
	if sizes := f.callSizes(); len(sizes) != 5 || sizes[1] != 3 {
		t.Fatalf("transactions = %v", sizes)
	}
}

func TestWriter_SingleRequestFailure(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	bad := uuid.New()
	f.fail[bad] = true
	w := newFakeWriter(t, f)
	startWriter(t, w)
	res, err := w.Append(appendCtx(t), bad, inputs(1))
	if !errors.Is(err, errFakeFK) || res.Inserted != nil {
		t.Fatalf("res = %+v err = %v", res, err)
	}
}

// Регрессия: хвост очереди (< flushRows), оставшийся после записи транзакции-потолка
// maxTxRows, должен уйти без новых запросов. Раньше писатель засыпал до следующего сигнала,
// а Append шлёт сигнал только первому запросу в пустой очереди или при flushRows — хвост и
// его HTTP-запросы висели до прихода ещё ~500 строк.
func TestWriter_LeftoverAfterMaxTxRowsIsFlushed(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	w := newFakeWriter(t, f)
	// Очередь набирается, пока писатель ещё не запущен (детерминированно воспроизводит
	// «всплеск успел прийти до того, как писатель проснулся»).
	w.mu.Lock()
	w.started = true
	w.mu.Unlock()
	ctx := appendCtx(t)

	const reqs, per = 11, 200 // 2200 строк > maxTxRows
	chans := make([]<-chan appendResult, reqs)
	for i := range chans {
		chans[i] = appendAsync(ctx, w, uuid.New(), rangeInputs(1, per))
	}
	waitQueued(t, w, reqs*per)

	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()
	go w.run(runCtx)

	timeout := time.After(3 * time.Second)
	for i, ch := range chans {
		select {
		case r := <-ch:
			if r.err != nil || r.res.Accepted != per {
				t.Fatalf("request %d: %+v", i, r)
			}
		case <-timeout:
			t.Fatalf("request %d stuck in queue (%d rows queued): leftover not flushed", i, w.queuedRows())
		}
	}
	stopRun()
	<-w.done
}

// Backpressure: очередь переполнена — Append ждёт места (и уважает свой ctx), события не теряются.
func TestWriter_Backpressure(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	f.gate = make(chan struct{})
	w := newFakeWriter(t, f)
	startWriter(t, w)
	ctx := appendCtx(t)

	first := appendAsync(ctx, w, uuid.New(), rangeInputs(1, MaxBatch))
	<-f.entered
	full := make([]<-chan appendResult, 0, maxPending/MaxBatch)
	for i := 0; i < maxPending/MaxBatch-1; i++ { // + первый = ровно maxPending строк
		full = append(full, appendAsync(ctx, w, uuid.New(), rangeInputs(1, MaxBatch)))
	}
	waitQueued(t, w, maxPending-MaxBatch)

	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := w.Append(short, uuid.New(), inputs(1)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("overflow append err = %v, want deadline exceeded", err)
	}
	waiting := appendAsync(ctx, w, uuid.New(), inputs(1)) // дождётся места
	close(f.gate)

	for _, ch := range append(full, first, waiting) {
		if r := <-ch; r.err != nil {
			t.Fatal(r.err)
		}
	}
}

// Stop дописывает очередь: всё, что принято в Append до остановки, записано.
func TestWriter_StopDrainsQueue(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	f.gate = make(chan struct{})
	w := newFakeWriter(t, f)
	w.Start(context.Background())
	ctx := appendCtx(t)

	first := appendAsync(ctx, w, uuid.New(), inputs(1))
	<-f.entered
	rest := []<-chan appendResult{
		appendAsync(ctx, w, uuid.New(), inputs(1)),
		appendAsync(ctx, w, uuid.New(), inputs(1, 2, 3)),
	}
	waitQueued(t, w, 4)
	stopped := make(chan error, 1)
	go func() { stopped <- w.Stop(ctx) }()
	close(f.gate)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	for _, ch := range append(rest, first) {
		if r := <-ch; r.err != nil || r.res.Accepted == 0 {
			t.Fatalf("drained request: %+v", r)
		}
	}
}

// Отмена ctx вызывающего прекращает ожидание, но не запись.
func TestWriter_CallerCancelDoesNotDropEvents(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	f.gate = make(chan struct{})
	w := newFakeWriter(t, f)
	startWriter(t, w)
	a := uuid.New()

	first := appendAsync(appendCtx(t), w, uuid.New(), inputs(1))
	<-f.entered
	cctx, cancel := context.WithCancel(context.Background())
	ch := appendAsync(cctx, w, a, inputs(7))
	waitQueued(t, w, 1)
	cancel()
	if r := <-ch; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("err = %v", r.err)
	}
	close(f.gate)
	<-first
	// Повтор клиента после обрыва — дубль: событие уже записано.
	res, err := w.Append(appendCtx(t), a, inputs(7))
	if err != nil || res.Accepted != 0 || res.LastSeq != 7 {
		t.Fatalf("retry = %+v %v", res, err)
	}
}

func TestWriter_ConcurrentAppendsRace(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	w := newFakeWriter(t, f)
	startWriter(t, w)
	ctx := appendCtx(t)
	a := uuid.New()

	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				// Пересекающиеся диапазоны: часть событий — дубли чужих запросов.
				res, err := w.Append(ctx, a, rangeInputs(int64(g*5+i), 3))
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				total += res.Accepted
				mu.Unlock()
			}
		}(g)
	}
	wg.Wait()
	// Уникальных seq: от 0 до 19*5+9+2 = 106 включительно.
	if total != 107 {
		t.Fatalf("accepted total = %d, want 107 unique", total)
	}
}
