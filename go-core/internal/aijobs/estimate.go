package aijobs

import (
	"math"
	"sync"
	"time"
)

// Оценка ожидания (estWaitSec): скользящее среднее длительности задач по типам × кто
// впереди в той же полосе ai-service. Точность «порядок величины» — фронт показывает
// «осталось около N с»; главное — не ходить за этим в БД на каждый запрос.

const (
	emaAlpha    = 0.2              // вес нового наблюдения
	minObserved = 50.0             // мс: меньше — шум измерения
	maxObserved = float64(3600000) // мс: больше часа — не длительность задачи, а зависание
	loadMaxAge  = 2 * time.Second  // снимок очереди обновляет диспетчер не чаще этого
)

// loadRow — строка снимка очереди (sqlLoad).
type loadRow struct {
	typ     int
	running bool
	prio    int
	n       int
}

type estimator struct {
	mu     sync.Mutex
	avgMs  [numTypes]float64
	load   []loadRow
	loadAt time.Time
}

func newEstimator() *estimator {
	e := &estimator{}
	for i := range e.avgMs {
		e.avgMs[i] = defaultAvgSec[i] * 1000
	}
	return e
}

// observe — фактическая длительность выполненной задачи (мс).
func (e *estimator) observe(typ int, ms float64) {
	if typ < 0 || ms < minObserved || ms > maxObserved {
		return
	}
	e.mu.Lock()
	e.avgMs[typ] = e.avgMs[typ]*(1-emaAlpha) + ms*emaAlpha
	e.mu.Unlock()
}

// seed — начальные средние из БД (перезаписывают дефолты).
func (e *estimator) seed(typ int, ms float64) {
	if typ < 0 || ms < minObserved || ms > maxObserved {
		return
	}
	e.mu.Lock()
	e.avgMs[typ] = ms
	e.mu.Unlock()
}

func (e *estimator) setLoad(rows []loadRow, at time.Time) {
	e.mu.Lock()
	e.load = rows
	e.loadAt = at
	e.mu.Unlock()
}

func (e *estimator) loadStale(now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return now.Sub(e.loadAt) >= loadMaxAge
}

// averages — копия средних (сек) для расчёта вне мьютекса.
func (e *estimator) averages() (avg [numTypes]float64) {
	e.mu.Lock()
	for i, ms := range e.avgMs {
		avg[i] = ms / 1000
	}
	e.mu.Unlock()
	return avg
}

// estimateNew — ожидание новой задачи типа typ: всё, что в её полосе стоит с приоритетом
// не ниже (меньшее число — важнее), плюс её собственная длительность. Running-задача
// в среднем выполнена наполовину.
func (e *estimator) estimateNew(typ int) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return estimateNewJob(&e.avgMs, e.load, typ, defaultPriority[typ])
}

func estimateNewJob(avgMs *[numTypes]float64, load []loadRow, typ, prio int) int {
	lane := laneOf(typ)
	sec := avgMs[typ] / 1000
	for _, r := range load {
		if laneOf(r.typ) != lane || r.prio > prio {
			continue
		}
		w := 1.0
		if r.running {
			w = 0.5
		}
		sec += float64(r.n) * w * avgMs[r.typ] / 1000
	}
	return ceilSec(sec)
}

// aheadRow — строка sqlAhead: сколько задач типа впереди.
type aheadRow struct {
	typ          int
	queuedAhead  int
	runningAhead int
	runningAll   int
}

// estimateQueued — позиция (1 — следующая) и ожидание задачи, стоящей в очереди go-core.
// Отправленные в ai-service (running) стоят впереди неё в полосе, а задержка run_after
// (backoff/Retry-After) добавляется сверху.
func estimateQueued(avg *[numTypes]float64, typ int, rows []aheadRow, runAfterIn time.Duration) (position, waitSec int) {
	sec := avg[typ]
	position = 1
	for _, r := range rows {
		position += r.queuedAhead
		sec += float64(r.queuedAhead)*avg[r.typ] + float64(r.runningAll)*0.5*avg[r.typ]
	}
	if runAfterIn > 0 {
		sec += runAfterIn.Seconds()
	}
	return position, ceilSec(sec)
}

// estimateRunning — сколько осталось задаче, уже переданной в ai-service. Впереди неё
// в полосе — running-задачи с более высоким приоритетом или отправленные раньше; если их
// нет, задача выполняется сама: осталось «среднее − прошедшее». ok=false — оценки нет
// (задача идёт дольше среднего): фронт покажет «формирует…» без числа.
func estimateRunning(avg *[numTypes]float64, typ int, rows []aheadRow, elapsed time.Duration) (waitSec int, ok bool) {
	ahead := 0.0
	n := 0
	for _, r := range rows {
		ahead += float64(r.runningAhead) * avg[r.typ]
		n += r.runningAhead
	}
	sec := avg[typ]
	if n > 0 {
		sec += ahead
	} else {
		sec -= elapsed.Seconds()
	}
	if sec < 1 {
		return 0, false
	}
	return ceilSec(sec), true
}

func ceilSec(sec float64) int {
	if sec < 1 || math.IsNaN(sec) {
		return 1
	}
	if sec > 86400 {
		return 86400
	}
	return int(math.Ceil(sec))
}
