// Команда fakeai — детерминированный имитатор ai-service по контракту
// contracts/openapi/ai-service.v1.yaml (+ callback go-internal.v1.yaml).
//
// Зачем: go-core, фронт и QA должны гоняться end-to-end без Python/Ollama —
// dev-стенд, CI, запасной вариант на демо. Имитатор ведёт себя как настоящий
// сервис на уровне протокола: 202 + асинхронный callback с ретраями 1/5/30 с,
// идемпотентность по request_id, 429 при переполнении очереди, 503 «полоса занята»
// в диалоге, 401/400/413/415 по контракту. Содержательно — эвристики, повторяющие
// моки фронта (frontend/src/shared/mocks): грамматика, покрытие фактов, чек-лист
// разговора, заявитель по брифу, генерация сценария из шаблонов категории.
//
// Всё детерминировано: одинаковый вход — одинаковый результат (генерация сценария —
// PRNG с зерном из request_id). Задержки обработки масштабируются FAKEAI_SPEED
// (0 — мгновенно, удобно для CI).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// config — env-конфиг имитатора. Имена переменных общие с go-core там, где смысл
// тот же (INTERNAL_API_TOKEN, TTS_DIR), чтобы docker-compose не раздваивался.
type config struct {
	addr        string        // FAKEAI_ADDR, :8000
	token       string        // INTERNAL_API_TOKEN — X-Internal-Token в обе стороны
	callbackURL string        // GOCORE_CALLBACK_URL — куда слать AiResult
	ttsDir      string        // TTS_DIR — shared volume tts_cache (go-core раздаёт /media/tts)
	speed       float64       // FAKEAI_SPEED — множитель задержек обработки (0 — мгновенно)
	busyEvery   int           // FAKEAI_BUSY_EVERY — каждый N-й ход диалога отвечает 503 (0 — никогда)
	failEvery   int           // FAKEAI_FAIL_EVERY — каждая N-я задача завершается status=failed (0 — никогда)
	queueMax    int           // FAKEAI_QUEUE_MAX — сколько задач может ждать в очереди, дальше 429
	llmWorkers  int           // FAKEAI_LLM_WORKERS — параллелизм полосы LLM (semantic/dialogue/generate)
	ltWorkers   int           // FAKEAI_LT_WORKERS — параллелизм полосы LanguageTool (grammar)
	ttsWorkers  int           // FAKEAI_TTS_WORKERS — параллелизм полосы TTS
	doneTTL     time.Duration // FAKEAI_DONE_TTL — сколько помнить выполненные задачи (повторный callback)
	logLevel    string        // FAKEAI_LOG_LEVEL: debug|info|warn|error
}

func loadConfig() config {
	c := config{
		addr:        env("FAKEAI_ADDR", ":8000"),
		token:       env("INTERNAL_API_TOKEN", "dev-internal-token"),
		callbackURL: env("GOCORE_CALLBACK_URL", "http://localhost:8080/internal/ai/v1/results"),
		ttsDir:      env("TTS_DIR", "./data/tts_cache"),
		speed:       envFloat("FAKEAI_SPEED", 1),
		busyEvery:   envInt("FAKEAI_BUSY_EVERY", 0),
		failEvery:   envInt("FAKEAI_FAIL_EVERY", 0),
		queueMax:    envInt("FAKEAI_QUEUE_MAX", 200),
		llmWorkers:  envInt("FAKEAI_LLM_WORKERS", 2),
		ltWorkers:   envInt("FAKEAI_LT_WORKERS", 2),
		ttsWorkers:  envInt("FAKEAI_TTS_WORKERS", 2),
		doneTTL:     envDuration("FAKEAI_DONE_TTL", 30*time.Minute),
		logLevel:    env("FAKEAI_LOG_LEVEL", "info"),
	}
	// Некорректные значения не валят старт: имитатор — инструмент стенда, а не прод.
	c.speed = max(c.speed, 0)
	c.busyEvery = max(c.busyEvery, 0)
	c.failEvery = max(c.failEvery, 0)
	c.queueMax = max(c.queueMax, 1)
	c.llmWorkers = max(c.llmWorkers, 1)
	c.ltWorkers = max(c.ltWorkers, 1)
	c.ttsWorkers = max(c.ttsWorkers, 1)
	if c.doneTTL <= 0 {
		c.doneTTL = 30 * time.Minute
	}
	return c
}

func main() {
	cfg := loadConfig()
	log := newLogger(cfg.logLevel)
	if err := run(cfg, log); err != nil {
		log.Error("fakeai остановлен с ошибкой", "err", err)
		os.Exit(1)
	}
}

func run(cfg config, log *slog.Logger) error {
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.ttsDir, 0o755); err != nil {
		return fmt.Errorf("TTS_DIR %q: %w", cfg.ttsDir, err)
	}

	// Фоновые циклы живут в отдельном контексте: сначала гасим приём HTTP,
	// потом останавливаем воркеры — иначе принятая 202-задача потеряется молча.
	workCtx, cancelWork := context.WithCancel(context.Background())
	defer cancelWork()
	var wg sync.WaitGroup

	store := &ttsStore{root: cfg.ttsDir}
	cb := newDeliverer(cfg, log)
	q := newQueue(cfg, log, cb, store)
	cb.start(workCtx, &wg)
	q.start(workCtx, &wg)

	srv := newServer(cfg, log, q, store)
	httpSrv := &http.Server{
		Addr:              cfg.addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("fakeai слушает", "addr", cfg.addr, "callback", cfg.callbackURL, "tts_dir", cfg.ttsDir,
			"speed", cfg.speed, "busy_every", cfg.busyEvery, "fail_every", cfg.failEvery, "queue_max", cfg.queueMax)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		cancelWork()
		wg.Wait()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-sigCtx.Done():
	}

	log.Info("остановка fakeai")
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := httpSrv.Shutdown(shCtx)
	cancelWork()
	wg.Wait()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return strings.TrimSpace(v)
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(env(key, "")); err == nil {
		return v
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(env(key, ""), 64); err == nil {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(env(key, "")); err == nil {
		return v
	}
	return def
}
