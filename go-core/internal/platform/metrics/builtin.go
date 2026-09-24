package metrics

import (
	"runtime"
	rtm "runtime/metrics"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ================================================================ HTTP

// Латентность и число запросов по маршруту (паттерн роутера, а не сырой путь — кардинальность
// ограничена числом маршрутов) и классу статуса. http_requests_total выводится из той же
// гистограммы (_count) — одна атомарная запись на запрос вместо двух.
var httpDur = NewHistogramVec("http_request_duration_seconds",
	"Длительность обработки HTTP-запроса, секунды (по маршруту и классу статуса).",
	DurationBuckets, "route", "status_class")

type httpCountFamily struct{}

func (httpCountFamily) appendTo(b []byte) []byte {
	const name = "http_requests_total"
	b = appendHeader(b, name, "counter", "Число HTTP-запросов (по маршруту и классу статуса).")
	for _, ch := range httpDur.v.sorted() {
		b = appendUintSample(b, name, ch.lbl, ch.m.Count())
	}
	return b
}

func init() {
	register("http_requests_total", func() httpCountFamily { return httpCountFamily{} }, nil)
	httpDur.v.max = 2000 // ~сотня маршрутов × пара-тройка классов статуса с запасом
	registerRuntime()
}

var statusClasses = [...]string{"unknown", "1xx", "2xx", "3xx", "4xx", "5xx"}

// ObserveHTTP — хук метрик роутера (httpx.NewRouter(..., metrics.ObserveHTTP)).
// Без блокировок и аллокаций на известном маршруте.
func ObserveHTTP(route string, status int, dur time.Duration) {
	c := status / 100
	if c < 1 || c > 5 {
		c = 0
	}
	httpDur.With(route, statusClasses[c]).Observe(dur.Seconds())
}

// ================================================================ Go runtime и процесс

var startTime = time.Now()

// runtime/metrics вместо runtime.ReadMemStats: чтение без stop-the-world.
var rtSamples = []rtm.Sample{
	{Name: "/sched/goroutines:goroutines"},
	{Name: "/memory/classes/heap/objects:bytes"},
	{Name: "/memory/classes/total:bytes"},
	{Name: "/gc/heap/goal:bytes"},
	{Name: "/gc/cycles/total:gc-cycles"},
	{Name: "/cpu/classes/gc/total:cpu-seconds"},
	{Name: "/sched/gomaxprocs:threads"},
}

var rtMu sync.Mutex

func rtValue(s rtm.Sample) float64 {
	switch s.Value.Kind() {
	case rtm.KindUint64:
		return float64(s.Value.Uint64())
	case rtm.KindFloat64:
		return s.Value.Float64()
	}
	return 0 // метрика не поддерживается этой версией рантайма
}

func registerRuntime() {
	goVersion := runtime.Version()
	RegisterCollector("go_runtime", func(e *Emitter) {
		rtMu.Lock()
		rtm.Read(rtSamples)
		v := [7]float64{}
		for i := range rtSamples {
			v[i] = rtValue(rtSamples[i])
		}
		rtMu.Unlock()

		e.Header("go_info", "gauge", "Версия Go.")
		e.Sample("go_info", 1, "version", goVersion)
		e.Gauge("go_goroutines", "Число горутин.", v[0])
		e.Gauge("go_memstats_heap_alloc_bytes", "Байты живых и ещё не собранных объектов кучи.", v[1])
		e.Gauge("go_memstats_sys_bytes", "Память, полученная рантаймом у ОС, байты.", v[2])
		e.Gauge("go_gc_heap_goal_bytes", "Целевой размер кучи следующего цикла GC, байты.", v[3])
		e.Counter("go_gc_cycles_total", "Завершённые циклы GC.", v[4])
		e.Counter("go_gc_cpu_seconds_total", "Оценка процессорного времени GC, секунды.", v[5])
		e.Gauge("go_gomaxprocs", "GOMAXPROCS.", v[6])
	})
	RegisterCollector("process", func(e *Emitter) {
		e.Gauge("process_start_time_seconds", "Время старта процесса (unix), секунды.", float64(startTime.Unix()))
		e.Gauge("process_uptime_seconds", "Время работы процесса, секунды.", time.Since(startTime).Seconds())
		if cpu, ok := cpuSeconds(); ok {
			e.Counter("process_cpu_seconds_total", "Процессорное время (user+system), секунды.", cpu)
		}
		if rss, ok := residentBytes(); ok {
			e.Gauge("process_resident_memory_bytes", "Резидентная память процесса, байты.", rss)
		}
	})
}

// ================================================================ пул PostgreSQL

// RegisterPool — статистика pgxpool (один вызов Stat() на скрейп). Повторный вызов
// заменяет пул (например, после переподключения).
func RegisterPool(pool *pgxpool.Pool) {
	RegisterCollector("pgxpool", func(e *Emitter) {
		if pool == nil {
			return
		}
		s := pool.Stat()
		e.Gauge("pgxpool_acquired_conns", "Соединения, выданные потребителям.", float64(s.AcquiredConns()))
		e.Gauge("pgxpool_idle_conns", "Простаивающие соединения.", float64(s.IdleConns()))
		e.Gauge("pgxpool_constructing_conns", "Соединения в процессе установки.", float64(s.ConstructingConns()))
		e.Gauge("pgxpool_total_conns", "Всего соединений пула.", float64(s.TotalConns()))
		e.Gauge("pgxpool_max_conns", "Предел соединений пула.", float64(s.MaxConns()))
		e.Counter("pgxpool_acquire_total", "Успешные получения соединения.", float64(s.AcquireCount()))
		e.Counter("pgxpool_acquire_duration_seconds_total", "Суммарное время получения соединений, секунды.", s.AcquireDuration().Seconds())
		e.Counter("pgxpool_empty_acquire_total", "Получения, которым пришлось ждать свободное соединение (насыщение пула).", float64(s.EmptyAcquireCount()))
		e.Counter("pgxpool_empty_acquire_wait_seconds_total", "Суммарное ожидание свободного соединения, секунды.", s.EmptyAcquireWaitTime().Seconds())
		e.Counter("pgxpool_canceled_acquire_total", "Получения, отменённые контекстом.", float64(s.CanceledAcquireCount()))
		e.Counter("pgxpool_new_conns_total", "Открытые соединения.", float64(s.NewConnsCount()))
		e.Counter("pgxpool_max_lifetime_destroy_total", "Закрытые по MaxConnLifetime.", float64(s.MaxLifetimeDestroyCount()))
		e.Counter("pgxpool_max_idle_destroy_total", "Закрытые по MaxConnIdleTime.", float64(s.MaxIdleDestroyCount()))
	})
}
