// Package aijobs — интеграция go-core ↔ ai-service целиком:
//
//   - очередь ai_jobs в PostgreSQL (core.JobQueue): Enqueue в транзакции бизнес-операции,
//     диспетчер будится после COMMIT;
//   - диспетчер: захват пачки одним UPDATE … FOR UPDATE SKIP LOCKED, отправка
//     POST /v1/jobs/* с ограниченной параллельностью, разбор 202/429/400/401/5xx;
//   - reaper: running без результата дольше settings.ai.reaper_after_sec → повтор/failed;
//   - circuit breaker, общий с синхронным клиентом;
//   - приём callback'ов POST /internal/ai/v1/results и маршрутизация результатов по типу
//     задачи (core.AIResultRouter) в транзакции под FOR UPDATE строки задачи;
//   - синхронный клиент (core.AIClient): ход диалога (multipart-стрим аудио), TTS, health, queue;
//   - GET /ai-jobs/{jobId} и счётчики для админки/метрик.
//
// Контракты: contracts/openapi/ai-service.v1.yaml, go-internal.v1.yaml; docs/contracts.md.
// Ключ идемпотентности на обоих концах — request_id (payload.request_id): первая отправка —
// ai_jobs.id, повтор после failed-callback'а — новый UUIDv7 (иначе идемпотентный ai-service
// вправе вернуть закэшированный отказ вместо пересчёта); id задачи при этом не меняется.
// Переотправки reaper'а и после рестарта идут с прежним request_id.
package aijobs

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"

	"lct/gocore/internal/config"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/settings"
)

// Deps — зависимости сервиса.
type Deps struct {
	Pool     *pgxpool.Pool
	Config   *config.Config
	Settings *settings.Store
	Log      *slog.Logger
	// HTTPClient — клиент к ai-service; nil — собственный настроенный транспорт (тесты
	// подставляют клиент httptest-сервера).
	HTTPClient *http.Client
}

// Параметры диспетчера.
const (
	dispatchConcurrency = 4               // одновременных POST /v1/jobs/* (ответ 202 — миллисекунды)
	dispatchTick        = time.Second     // опрос очереди: отложенные run_after и задачи других инстансов
	reaperEvery         = time.Minute     // проход reaper'а
	settingsEvery       = 5 * time.Second // как часто диспетчер перечитывает settings.ai
	dbOpTimeout         = 10 * time.Second
)

// Service реализует core.JobQueue, core.AIClient и core.AIResultRouter.
type Service struct {
	pool     *pgxpool.Pool
	cfg      *config.Config
	settings *settings.Store
	log      *slog.Logger

	instance string
	base     string // AI_SERVICE_URL без завершающего "/"
	token    []byte // X-Internal-Token в обе стороны
	hc       *http.Client
	ownHC    bool

	ai   atomic.Pointer[settings.AI] // снимок settings.ai: горячие пути не ходят в БД
	aiAt time.Time                   // когда обновлён (только горутина диспетчера)

	br  *breaker
	est *estimator

	hmu      sync.RWMutex
	handlers [numTypes]core.AIResultHandler

	kickCh        chan struct{}
	inflight      atomic.Int32
	lastClaimFull atomic.Bool
	pausedUntil   [numTypes]atomic.Int64 // unix nano: тип на паузе после 429 (не долбим полную очередь)

	breakerHook atomic.Pointer[func(state string)]

	// unavailSince — unix nano начала недоступности ai-service для задач (breaker открылся
	// или токен отвергнут); 0 — доступен. Держится, пока breaker не закроется или задача не
	// будет принята; по нему reaper проваливает оценочные задачи (failUnavailable).
	unavailSince atomic.Int64

	sf     singleflight.Group
	health cached[aiservice.Health]
	queue  cached[aiservice.QueueStatus]

	started atomic.Bool
	stop    context.CancelFunc
	loops   sync.WaitGroup // циклы диспетчера и reaper'а
	sends   sync.WaitGroup // отправки в полёте

	st counters
}

var (
	_ core.JobQueue       = (*Service)(nil)
	_ core.AIClient       = (*Service)(nil)
	_ core.AIResultRouter = (*Service)(nil)
)

// New создаёт сервис. Фоновые циклы запускает Start.
func New(d Deps) *Service {
	s := &Service{
		pool:     d.Pool,
		cfg:      d.Config,
		settings: d.Settings,
		log:      d.Log,
		instance: d.Config.InstanceID,
		base:     d.Config.AIServiceURL,
		token:    []byte(d.Config.InternalAPIToken),
		hc:       d.HTTPClient,
		est:      newEstimator(),
		kickCh:   make(chan struct{}, 1),
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.instance == "" {
		s.instance = "go-core"
	}
	if s.hc == nil {
		s.hc = &http.Client{Transport: newTransport()}
		s.ownHC = true
	}
	def := settings.Defaults().AI
	s.ai.Store(&def)
	s.br = newBreaker(
		func() int { return s.aiSettings().BreakerFailures },
		func() time.Duration { return time.Duration(max(1, s.aiSettings().BreakerOpenSec)) * time.Second },
		s.onBreakerChange,
	)
	return s
}

// newTransport — один транспорт на все обращения к ai-service: keep-alive пул к одному хосту,
// HTTP/1.1 (h2c внутри docker-сети не нужен), без сжатия (локальная сеть — CPU дороже байтов).
func newTransport() *http.Transport {
	return &http.Transport{
		Proxy: nil, // внутренняя docker-сеть: переменные HTTP_PROXY не должны перехватывать трафик
		DialContext: (&net.Dialer{
			Timeout:   3 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 0,
		DisableCompression:    true,
		WriteBufferSize:       32 << 10,
		ReadBufferSize:        16 << 10,
	}
}

// aiSettings — актуальный снимок settings.ai (никогда не nil).
func (s *Service) aiSettings() *settings.AI { return s.ai.Load() }

// refreshSettings перечитывает settings.ai (Store сам кэширует и ходит в БД раз в 30 с).
func (s *Service) refreshSettings(ctx context.Context) {
	if s.settings == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout) // Get может перечитать таблицу
	defer cancel()
	snap := s.settings.Get(ctx)
	ai := snap.AI
	d := settings.Defaults().AI
	if ai.MaxTries < 1 {
		ai.MaxTries = d.MaxTries
	}
	if ai.ReaperAfterSec < 60 {
		ai.ReaperAfterSec = d.ReaperAfterSec
	}
	if ai.BreakerFailures < 1 {
		ai.BreakerFailures = d.BreakerFailures
	}
	if ai.BreakerOpenSec < 1 {
		ai.BreakerOpenSec = d.BreakerOpenSec
	}
	s.ai.Store(&ai)
	s.aiAt = time.Now()
}

// OnBreakerChange — подписка на смену состояния breaker'а (например, WS aiHealth в мониторинг).
// fn вызывается синхронно и должна быть неблокирующей. Вызывать до Start.
func (s *Service) OnBreakerChange(fn func(state string)) {
	if fn == nil {
		s.breakerHook.Store(nil)
		return
	}
	s.breakerHook.Store(&fn)
}

func (s *Service) onBreakerChange(from, to breakerState) {
	switch to {
	case stateOpen:
		s.log.Warn("ai-service circuit breaker opened", "from", from.String(), "open_sec", s.aiSettings().BreakerOpenSec)
	case stateClosed:
		s.log.Info("ai-service circuit breaker closed", "from", from.String())
	default:
		s.log.Info("ai-service circuit breaker half-open")
	}
	switch to {
	case stateOpen:
		s.markUnreachable() // open → half_open → open — та же недоступность: момент начала не сдвигаем
	case stateClosed:
		s.unavailSince.Store(0)
	}
	if fn := s.breakerHook.Load(); fn != nil {
		(*fn)(to.String())
	}
	if to == stateClosed {
		s.kick() // очередь могла накопиться, пока сервис лежал
	}
}

// markUnreachable — начало недоступности ai-service (если ещё не отмечено).
func (s *Service) markUnreachable() {
	s.unavailSince.CompareAndSwap(0, time.Now().UnixNano())
}

// markReachable — ai-service принял задачу (или честно ответил «занято»). Поздний ответ
// запроса, начатого до открытия breaker'а, недоступность не отменяет.
func (s *Service) markReachable() {
	if s.unavailSince.Load() != 0 && s.br.State() == stateClosed {
		s.unavailSince.Store(0)
	}
}

// unavailableFor — сколько ai-service недоступен для задач (0 — доступен).
func (s *Service) unavailableFor(now time.Time) time.Duration {
	since := s.unavailSince.Load()
	if since == 0 {
		return 0
	}
	return max(now.Sub(time.Unix(0, since)), 0)
}

// Register — обработчик результатов типа задачи (core.AIResultRouter). Регистрация — при сборке
// приложения до Start; повторная регистрация заменяет обработчик.
func (s *Service) Register(t core.JobType, h core.AIResultHandler) {
	i := typeIdx(t)
	if i < 0 {
		panic("aijobs: unknown job type " + string(t))
	}
	s.hmu.Lock()
	s.handlers[i] = h
	s.hmu.Unlock()
}

func (s *Service) handler(i int) core.AIResultHandler {
	s.hmu.RLock()
	h := s.handlers[i]
	s.hmu.RUnlock()
	return h
}

// Start запускает диспетчер и reaper. Останавливаются по отмене ctx или Stop.
func (s *Service) Start(ctx context.Context) {
	if !s.started.CompareAndSwap(false, true) {
		return
	}
	ctx, s.stop = context.WithCancel(ctx)
	s.refreshSettings(ctx)
	s.seedAverages(ctx)
	s.requeueOwn(ctx)
	s.loops.Add(2)
	go s.dispatchLoop(ctx)
	go s.reaperLoop(ctx)
}

// Stop — graceful shutdown: новые задачи не захватываются, отправки в полёте дожидаются
// ответа (не дольше AIJobTimeout) и записывают итог. Возвращает ctx.Err(), если не успели.
func (s *Service) Stop(ctx context.Context) error {
	if s.stop != nil {
		s.stop()
	}
	done := make(chan struct{})
	go func() {
		s.loops.Wait()
		s.sends.Wait()
		close(done)
	}()
	select {
	case <-done:
		if s.ownHC {
			s.hc.CloseIdleConnections()
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Wait блокируется до остановки фоновых циклов и отправок (после отмены ctx из Start).
func (s *Service) Wait() {
	s.loops.Wait()
	s.sends.Wait()
}

// kick будит диспетчер (неблокирующе; вызывается после COMMIT через pg.OnCommit).
func (s *Service) kick() {
	select {
	case s.kickCh <- struct{}{}:
	default:
	}
}

// ---------------------------------------------------------------- статистика

type counters struct {
	dispatched   atomic.Uint64
	accepted     atomic.Uint64
	busy         atomic.Uint64
	failures     atomic.Uint64
	rejected     atomic.Uint64
	requeued     atomic.Uint64
	failed       atomic.Uint64
	callbacks    atomic.Uint64
	callbackDups atomic.Uint64
	reaped       atomic.Uint64
	syncCalls    atomic.Uint64
	syncBusy     atomic.Uint64
	syncFailures atomic.Uint64
}

// DispatchStats — счётчики с момента старта (для /metrics и админки).
type DispatchStats struct {
	Breaker      string // closed | open | half_open
	InFlight     int    // POST /v1/jobs/* в полёте
	Dispatched   uint64 // отправлено задач
	Accepted     uint64 // 202
	Busy         uint64 // 429/503 на отправку задачи
	Failures     uint64 // refused/timeout/5xx на отправку задачи
	Rejected     uint64 // 4xx на отправку задачи (кроме 429)
	Requeued     uint64 // возвращено в очередь (backoff, 429, ретрай по callback'у, reaper)
	Failed       uint64 // переведено в failed
	Callbacks    uint64 // принято callback'ов (включая дубли)
	CallbackDups uint64 // дубли / по неизвестным задачам
	Reaped       uint64 // обработано reaper'ом
	SyncCalls    uint64 // синхронных вызовов (диалог, TTS)
	SyncBusy     uint64 // из них 503/429
	SyncFailures uint64 // из них недоступность (refused/timeout/5xx/breaker)
}

func (s *Service) Stats() DispatchStats {
	c := &s.st
	return DispatchStats{
		Breaker:      s.br.State().String(),
		InFlight:     int(s.inflight.Load()),
		Dispatched:   c.dispatched.Load(),
		Accepted:     c.accepted.Load(),
		Busy:         c.busy.Load(),
		Failures:     c.failures.Load(),
		Rejected:     c.rejected.Load(),
		Requeued:     c.requeued.Load(),
		Failed:       c.failed.Load(),
		Callbacks:    c.callbacks.Load(),
		CallbackDups: c.callbackDups.Load(),
		Reaped:       c.reaped.Load(),
		SyncCalls:    c.syncCalls.Load(),
		SyncBusy:     c.syncBusy.Load(),
		SyncFailures: c.syncFailures.Load(),
	}
}
