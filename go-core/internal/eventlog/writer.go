package eventlog

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/pg"
)

// Параметры group commit (db-design §3): 1000 соб/с превращаются в ~20 транзакций/с.
const (
	flushWindow  = 50 * time.Millisecond // окно накопления от первого события в очереди
	flushRows    = 500                   // столько строк в очереди — пишем, не дожидаясь окна
	maxTxRows    = 2000                  // потолок строк в одной транзакции (латентность и размер массивов)
	maxPending   = 10000                 // потолок событий в очереди + в полёте (backpressure, не drop)
	flushTimeout = 15 * time.Second      // одна транзакция; при зависшей БД ждущие получат ошибку
)

// ErrStopped — писатель остановлен (graceful shutdown), новые события не принимаются.
var ErrStopped = errors.New("eventlog: writer stopped")

// errNotStarted — Append без Start: без писателя запрос ждал бы вечно — ошибка сборки app.
var errNotStarted = errors.New("eventlog: writer not started")

// Result — итог записи клиентского батча.
type Result struct {
	// Inserted — только новые события (дубли по clientSeq отброшены) в порядке батча:
	// для WS attemptEvent и для вычисления first_input_at.
	Inserted []public.AttemptEvent
	// Accepted — сколько событий записано впервые (контракт: accepted).
	Accepted int
	// LastSeq — максимальный сохранённый clientSeq ПОПЫТКИ (контракт: lastSeq), а не только
	// этого батча: клиент, начавший нумерацию заново (новая вкладка, потерянный sessionStorage),
	// узнаёт по нему настоящую верхнюю границу — в том числе из батча, где всё оказалось дублями.
	LastSeq int64
}

// request — клиентский батч одной попытки, ожидающий групповой записи.
type request struct {
	attemptID uuid.UUID
	events    []Input
	ids       []int64 // id вставленных строк по индексу события; 0 — дубль
	lastSeq   int64   // max(client_seq) попытки после вставки (в той же транзакции)
	err       error
	done      chan struct{}
}

// Writer — group commit клиентских событий: запросы многих студентов копятся до 50 мс
// (или до 500 строк) и пишутся одним INSERT … SELECT unnest(...) в транзакции с
// synchronous_commit=off. Append блокируется до коммита батча, в который попали его события,
// поэтому ответ клиенту «принято» честный. Один фоновый писатель — одна транзакция за раз.
type Writer struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	mu      sync.Mutex
	pending []*request    // очередь, ещё не взятая писателем
	queued  int           // строк в pending
	inUse   int           // строк в pending + в транзакции (для backpressure)
	space   chan struct{} // закрывается, когда место в очереди освободилось (broadcast)
	stopped bool
	started bool

	wake     chan struct{} // cap 1: очередь стала непустой / набрала flushRows
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once

	// exec — запись набора запросов одной транзакцией (w.write; в тестах подменяется без БД).
	exec func(ctx context.Context, reqs []*request) error

	// Буферы писателя — принадлежат горутине run, переиспользуются между батчами.
	batch    []*request
	aids     []uuid.UUID
	seqs     []int64
	types    []string
	payloads []json.RawMessage
	ats      []time.Time
	refs     map[eventKey]eventRef
	uniq     []uuid.UUID         // попытки батча без повторов — для sqlLastSeq
	last     map[uuid.UUID]int64 // попытка -> max(client_seq) после вставки
}

type eventKey struct {
	attempt uuid.UUID
	seq     int64
}

type eventRef struct {
	req *request
	idx int
}

func NewWriter(pool *pgxpool.Pool, log *slog.Logger) *Writer {
	if log == nil {
		log = slog.Default()
	}
	w := &Writer{
		pool:  pool,
		log:   log,
		space: make(chan struct{}),
		wake:  make(chan struct{}, 1),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
		refs:  make(map[eventKey]eventRef, 256),
		last:  make(map[uuid.UUID]int64, 64),
	}
	w.exec = w.write
	return w
}

// Start запускает фоновый писатель. Отмена ctx равносильна Stop: хвост дописывается.
func (w *Writer) Start(ctx context.Context) {
	w.mu.Lock()
	if w.started || w.stopped {
		w.mu.Unlock()
		return
	}
	w.started = true
	w.mu.Unlock()
	go w.run(ctx)
}

// Stop прекращает приём событий, дописывает очередь и ждёт писателя (или отмены ctx).
func (w *Writer) Stop(ctx context.Context) error {
	w.stopOnce.Do(func() {
		w.mu.Lock()
		w.stopped = true
		started := w.started
		close(w.space) // разбудить ждущих места: они увидят stopped
		w.space = make(chan struct{})
		w.mu.Unlock()
		close(w.stop)
		if !started {
			// Писатель не запускался — дописываем хвост здесь же.
			w.drain(ctx)
			close(w.done)
		}
	})
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Append ставит события попытки в групповую запись и ждёт коммита. Отмена ctx прекращает
// ожидание, но не отменяет запись (события уже в очереди). Очередь переполнена — ждём места
// (backpressure), события не теряются.
func (w *Writer) Append(ctx context.Context, attemptID uuid.UUID, events []Input) (Result, error) {
	if len(events) == 0 {
		return Result{Inserted: []public.AttemptEvent{}}, nil
	}
	req := &request{attemptID: attemptID, events: events, done: make(chan struct{})}
	n := len(events)

	w.mu.Lock()
	for {
		if w.stopped {
			w.mu.Unlock()
			return Result{}, ErrStopped
		}
		if !w.started {
			w.mu.Unlock()
			return Result{}, errNotStarted
		}
		if w.inUse == 0 || w.inUse+n <= maxPending {
			break
		}
		sp := w.space
		w.mu.Unlock()
		select {
		case <-sp:
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
		w.mu.Lock()
	}
	w.pending = append(w.pending, req)
	w.queued += n
	w.inUse += n
	signal := len(w.pending) == 1 || w.queued >= flushRows
	w.mu.Unlock()
	if signal {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}

	select {
	case <-req.done:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	if req.err != nil {
		return Result{}, req.err
	}
	return req.result(), nil
}

// result собирается в горутине вызывающего (не писателя): CPU размазан по запросам.
func (r *request) result() Result {
	res := Result{Inserted: make([]public.AttemptEvent, 0, len(r.events)), LastSeq: r.lastSeq}
	for i := range r.events {
		e := &r.events[i]
		if e.ClientSeq > res.LastSeq { // все события батча после коммита в БД — нижняя граница
			res.LastSeq = e.ClientSeq
		}
		if i >= len(r.ids) || r.ids[i] == 0 {
			continue
		}
		res.Inserted = append(res.Inserted, public.AttemptEvent{
			Id:        int(r.ids[i]),
			ClientSeq: int(e.ClientSeq),
			Type:      public.AttemptEventType(e.Type),
			Payload:   e.payloadMap(),
			At:        e.At,
		})
	}
	res.Accepted = len(res.Inserted)
	return res
}

// run — цикл писателя: ждём первое событие, выдерживаем окно (или до flushRows), пишем.
func (w *Writer) run(ctx context.Context) {
	defer close(w.done)
	// Транзакции не должны обрываться отменой ctx приложения — хвост дописывается при остановке.
	base := context.WithoutCancel(ctx)
	timer := time.NewTimer(time.Hour)
	timer.Stop()

	for {
		select {
		case <-w.wake:
		case <-w.stop:
			w.drain(base)
			return
		case <-ctx.Done():
			w.markStopped()
			w.drain(base)
			return
		}

		if w.queuedRows() < flushRows {
			timer.Reset(flushWindow)
		window:
			for {
				select {
				case <-timer.C:
					break window
				case <-w.wake:
					if w.queuedRows() >= flushRows {
						timer.Stop()
						break window
					}
				case <-w.stop:
					timer.Stop()
					break window // пишем набранное, на следующем круге — drain
				case <-ctx.Done():
					timer.Stop()
					break window
				}
			}
		}

		// Пишем набранное; если за время записи снова набралось flushRows — сразу следующий батч.
		for {
			if !w.take() {
				break
			}
			w.flush(base)
			if w.queuedRows() < flushRows {
				break
			}
		}
		// Остаток (< flushRows) мог остаться без сигнала: Append будит писателя только при
		// первом запросе в пустой очереди или по flushRows, а сигналы, пришедшие во время окна,
		// уже съедены. Без этого хвост (и ждущие его запросы) висел бы до следующих 500 строк.
		if w.queuedRows() > 0 {
			select {
			case w.wake <- struct{}{}:
			default:
			}
		}
	}
}

func (w *Writer) markStopped() {
	w.mu.Lock()
	if !w.stopped {
		w.stopped = true
		close(w.space)
		w.space = make(chan struct{})
	}
	w.mu.Unlock()
}

// drain дописывает всё, что осталось в очереди (остановка).
func (w *Writer) drain(ctx context.Context) {
	for w.take() {
		w.flush(ctx)
	}
}

func (w *Writer) queuedRows() int {
	w.mu.Lock()
	n := w.queued
	w.mu.Unlock()
	return n
}

// take переносит из очереди в w.batch целые запросы, пока не наберётся maxTxRows строк
// (минимум один запрос). false — очередь пуста.
func (w *Writer) take() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) == 0 {
		return false
	}
	rows, k := 0, 0
	for k < len(w.pending) && (k == 0 || rows+len(w.pending[k].events) <= maxTxRows) {
		rows += len(w.pending[k].events)
		k++
	}
	w.batch = append(w.batch[:0], w.pending[:k]...)
	rest := copy(w.pending, w.pending[k:])
	clear(w.pending[rest:]) // не держим ссылки на отданные запросы
	w.pending = w.pending[:rest]
	w.queued -= rows
	return true
}

// flush пишет w.batch одной транзакцией и будит ждущих. При ошибке группового INSERT
// (например, FK: попытка исчезла) батч переписывается по одному запросу — чужая ошибка
// не валит события остальных студентов.
func (w *Writer) flush(ctx context.Context) {
	batch := w.batch
	rows := 0
	for _, r := range batch {
		rows += len(r.events)
	}

	err := w.exec(ctx, batch)
	if err != nil && len(batch) > 1 {
		w.log.Warn("eventlog: group insert failed, retrying per request", "requests", len(batch), "rows", rows, "err", err)
		for _, r := range batch {
			r.err = w.exec(ctx, []*request{r})
			if r.err != nil {
				w.log.Error("eventlog: insert events", "attempt", r.attemptID, "events", len(r.events), "err", r.err)
			}
		}
	} else {
		if err != nil {
			w.log.Error("eventlog: insert events", "attempt", batch[0].attemptID, "events", rows, "err", err)
		}
		for _, r := range batch {
			r.err = err
		}
	}
	for _, r := range batch {
		close(r.done)
	}
	clear(w.batch)
	w.batch = w.batch[:0]

	w.mu.Lock()
	w.inUse -= rows
	close(w.space)
	w.space = make(chan struct{})
	w.mu.Unlock()
}

// write — одна транзакция на набор запросов. ids запросов валидны только при успешном коммите.
func (w *Writer) write(ctx context.Context, reqs []*request) error {
	w.aids, w.seqs, w.types, w.payloads, w.ats = w.aids[:0], w.seqs[:0], w.types[:0], w.payloads[:0], w.ats[:0]
	clear(w.refs)
	clear(w.last)
	w.uniq = w.uniq[:0]
	for _, r := range reqs {
		r.lastSeq = 0
		if _, seen := w.last[r.attemptID]; !seen {
			w.last[r.attemptID] = 0
			w.uniq = append(w.uniq, r.attemptID)
		}
		if cap(r.ids) < len(r.events) {
			r.ids = make([]int64, len(r.events))
		} else {
			r.ids = r.ids[:len(r.events)]
			clear(r.ids)
		}
		for i := range r.events {
			e := &r.events[i]
			k := eventKey{r.attemptID, e.ClientSeq}
			if _, dup := w.refs[k]; !dup {
				w.refs[k] = eventRef{req: r, idx: i} // дубль внутри батча: вставится первое вхождение
			}
			payload := e.Payload
			if len(payload) == 0 {
				payload = emptyObject
			}
			w.aids = append(w.aids, r.attemptID)
			w.seqs = append(w.seqs, e.ClientSeq)
			w.types = append(w.types, e.Type)
			w.payloads = append(w.payloads, payload)
			w.ats = append(w.ats, e.At)
		}
	}

	fctx, cancel := context.WithTimeout(ctx, flushTimeout)
	defer cancel()
	err := pg.WithTxOpts(fctx, w.pool, pg.TxOptions{AsyncCommit: true}, func(ctx context.Context, tx pgx.Tx) error {
		// Вставка и lastSeq попыток — один round-trip (pipeline): второй запрос в той же
		// транзакции видит только что вставленные строки.
		var b pgx.Batch
		b.Queue(sqlInsertBatch, w.aids, w.seqs, w.types, w.payloads, w.ats)
		b.Queue(sqlLastSeq, w.uniq)
		br := tx.SendBatch(ctx, &b)
		if err := w.scanInserted(br); err != nil {
			_ = br.Close()
			return err
		}
		if err := w.scanLastSeq(br); err != nil {
			_ = br.Close()
			return err
		}
		return br.Close()
	})

	// Большие массивы (после пика) не держим: писатель живёт весь процесс.
	if cap(w.aids) > 4*maxTxRows {
		w.aids, w.seqs, w.types, w.payloads, w.ats = nil, nil, nil, nil, nil
	} else {
		// Не держим payload'ы и строки отданных запросов до следующего батча.
		clear(w.payloads[:cap(w.payloads)])
		clear(w.types[:cap(w.types)])
	}
	if err != nil {
		for _, r := range reqs {
			clear(r.ids)
			r.lastSeq = 0
		}
		return err
	}
	for _, r := range reqs {
		r.lastSeq = w.last[r.attemptID]
	}
	return nil
}

// scanInserted — ключи вставленных строк -> ids запросов (первое вхождение дубля в батче).
func (w *Writer) scanInserted(br pgx.BatchResults) error {
	rows, err := br.Query()
	if err != nil {
		return err
	}
	defer rows.Close()
	var (
		id, seq int64
		aid     uuid.UUID
	)
	for rows.Next() {
		if err := rows.Scan(&id, &aid, &seq); err != nil {
			return err
		}
		if ref, ok := w.refs[eventKey{aid, seq}]; ok {
			ref.req.ids[ref.idx] = id
		}
	}
	return rows.Err()
}

// scanLastSeq — max(client_seq) каждой попытки батча -> w.last.
func (w *Writer) scanLastSeq(br pgx.BatchResults) error {
	rows, err := br.Query()
	if err != nil {
		return err
	}
	defer rows.Close()
	var (
		aid uuid.UUID
		seq int64
	)
	for rows.Next() {
		if err := rows.Scan(&aid, &seq); err != nil {
			return err
		}
		w.last[aid] = seq
	}
	return rows.Err()
}
