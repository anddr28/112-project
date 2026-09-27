// Package app — сборка ядра: зависимости, маршруты, фоновые воркеры, graceful shutdown.
// Единственное место, где доменные пакеты встречаются друг с другом (через порты core).
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"lct/gocore/internal/admin"
	"lct/gocore/internal/aijobs"
	"lct/gocore/internal/attempts"
	"lct/gocore/internal/audit"
	"lct/gocore/internal/auth"
	"lct/gocore/internal/classifier"
	"lct/gocore/internal/config"
	"lct/gocore/internal/core"
	"lct/gocore/internal/dialogue"
	"lct/gocore/internal/evaluation"
	"lct/gocore/internal/eventlog"
	"lct/gocore/internal/lessons"
	"lct/gocore/internal/ops"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/metrics"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/realtime"
	"lct/gocore/internal/reports"
	"lct/gocore/internal/scenarios"
	"lct/gocore/internal/settings"
	"lct/gocore/internal/users"
)

const (
	apiPrefix       = "/api/v1"
	shutdownTimeout = 15 * time.Second
)

// Core — собранное ядро (для serve и для интеграционных тестов).
type Core struct {
	Cfg      *config.Config
	Log      *slog.Logger
	Pool     *pgxpool.Pool
	Settings *settings.Store
	Audit    *audit.Writer
	Catalog  *classifier.Catalog
	AI       *aijobs.Service
	Hub      *realtime.Hub
	Events   *eventlog.Writer
	Auth     *auth.Service
	Ops      *ops.Ops

	Users      *users.Handlers
	Classifier *classifier.Handlers
	Scenarios  *scenarios.Service
	Lessons    *lessons.Service
	Dialogue   *dialogue.Service
	Evaluation *evaluation.Service
	Attempts   *attempts.Service
	Admin      *admin.Handlers
	Reports    *reports.Handlers

	Router *httpx.Router
	// Public — для браузеров (HTTPS): /api/v1 (и WS), SPA, health. Internal — открытый HTTP
	// внутри docker-сети: callback ai-service, /metrics, health; API и SPA — только при
	// GOCORE_HTTP_API=true (за reverse-proxy с TLS), иначе пароль и cookie шли бы открытым текстом.
	Public   http.Handler
	Internal http.Handler

	startedAt time.Time
}

// Build создаёт все компоненты (без запуска фоновых воркеров и серверов).
func Build(ctx context.Context, cfg *config.Config, log *slog.Logger) (*Core, error) {
	pool, err := pg.Connect(ctx, cfg.DatabaseURL, cfg.DBMaxConns, "go-core")
	if err != nil {
		return nil, err
	}
	c := &Core{Cfg: cfg, Log: log, Pool: pool, startedAt: time.Now()}

	c.Settings = settings.NewStore(pool, log)
	if err := c.Settings.Reload(ctx); err != nil {
		log.Warn("settings: using defaults", "err", err)
	}
	c.Audit = audit.NewWriter(pool, log)
	c.Catalog = classifier.NewCatalog(pool, log)
	c.Hub = realtime.NewHub(log)
	c.AI = aijobs.New(aijobs.Deps{Pool: pool, Config: cfg, Settings: c.Settings, Log: log})
	c.Events = eventlog.NewWriter(pool, log)
	c.Ops = ops.New(ops.Deps{Pool: pool, Config: cfg, Settings: c.Settings, Auditor: c.Audit, Log: log})

	c.Auth = auth.New(auth.Deps{Pool: pool, Config: cfg, Settings: c.Settings, Auditor: c.Audit, Log: log, LoadUser: users.Get})
	// открытые WS перепроверяют сессию: блокировка/выход закрывают сокет (≤ 10 с), а не живут до обрыва
	c.Hub.SetAuthenticator(c.Auth)
	c.Users = users.NewHandlers(users.Deps{Pool: pool, Settings: c.Settings, Auditor: c.Audit, Catalog: c.Catalog,
		Sessions: c.Auth, HashPassword: auth.HashPasswordCtx, Log: log})
	c.Classifier = classifier.NewHandlers(c.Catalog, log)

	c.Scenarios = scenarios.New(scenarios.Deps{Pool: pool, Settings: c.Settings, Catalog: c.Catalog, Queue: c.AI,
		AI: c.AI, Auditor: c.Audit, Log: log})
	c.Lessons = lessons.New(lessons.Deps{Pool: pool, Settings: c.Settings, Publisher: c.Hub, Auditor: c.Audit, Log: log})
	c.Dialogue = dialogue.New(dialogue.Deps{Pool: pool, Config: cfg, Settings: c.Settings, AI: c.AI, Queue: c.AI,
		Publisher: c.Hub, Log: log})
	c.Evaluation = evaluation.New(evaluation.Deps{Pool: pool, Settings: c.Settings, Catalog: c.Catalog, Queue: c.AI,
		Publisher: c.Hub, Auditor: c.Audit, Log: log})
	c.Attempts = attempts.New(attempts.Deps{Pool: pool, Settings: c.Settings, Catalog: c.Catalog, Events: c.Events,
		Publisher: c.Hub, Auditor: c.Audit, Evaluator: c.Evaluation, Issuer: c.Lessons, Dialogue: c.Dialogue, Log: log})
	c.Admin = admin.New(admin.Deps{Pool: pool, Config: cfg, Settings: c.Settings, AI: c.AI, Jobs: c.AI, Ops: c.Ops,
		WS: c.Hub, Publisher: c.Hub, Auditor: c.Audit, StartedAt: c.startedAt, Log: log})
	c.Reports = reports.New(reports.Deps{Pool: pool, Catalog: c.Catalog, Auditor: c.Audit, Log: log})

	// Результаты AI-задач — доменам, которые их ждут.
	c.Scenarios.RegisterResults(c.AI)
	c.Evaluation.RegisterResults(c.AI)

	c.routes()
	return c, nil
}

// routes — все маршруты. Публичный API — через httpx.Router (RBAC, CSRF, ApiError, метрики).
func (c *Core) routes() {
	apiMux := http.NewServeMux()
	rt := httpx.NewRouter(apiMux, apiPrefix, c.Auth, c.Log, metrics.ObserveHTTP)
	c.Router = rt

	c.Auth.Register(rt)
	c.Users.Register(rt)
	c.Classifier.Register(rt)
	c.AI.RegisterRoutes(rt)
	c.Scenarios.Register(rt)
	c.Lessons.Register(rt)
	c.Attempts.Register(rt)
	c.Dialogue.Register(rt)
	c.Evaluation.Register(rt)
	c.Admin.Register(rt)
	c.Reports.Register(rt)

	rt.Handle("GET /ws/lessons/{lessonId}/monitor", httpx.Roles(core.RoleTeacher, core.RoleAdmin),
		c.Hub.MonitorHandler(c.Lessons, c.Cfg.WSOrigins))
	rt.Handle("GET /ws/attempts/{attemptId}", httpx.Roles(core.RoleStudent),
		c.Hub.StudentHandler(c.Lessons, c.Cfg.WSOrigins))

	// Неизвестный путь API — ApiError, а не text/plain от ServeMux.
	apiMux.HandleFunc(apiPrefix+"/", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, httpx.NotFound("Метод API не найден"))
	})

	var spa http.Handler = http.NotFoundHandler()
	if c.Cfg.StaticDir != "" {
		spa = ops.SPAHandler(c.Cfg.StaticDir)
	}

	public := http.NewServeMux()
	public.Handle("/api/", apiMux)
	public.Handle("GET /healthz", c.Ops.Healthz())
	public.Handle("GET /readyz", c.Ops.Readyz())
	public.Handle("/", spa)
	c.Public = ops.SecurityHeaders(public)

	internal := http.NewServeMux()
	internal.Handle("POST /internal/ai/v1/results", c.AI.CallbackHandler())
	internal.Handle("GET /metrics", metrics.Handler())
	internal.Handle("GET /healthz", c.Ops.Healthz())
	internal.Handle("GET /readyz", c.Ops.Readyz())
	if c.Cfg.HTTPServeAPI {
		internal.Handle("/api/", apiMux)
		internal.Handle("/", spa)
	}
	c.Internal = ops.SecurityHeaders(internal)
}

// Seed — справочники (всегда) и демо-данные (demo). Идемпотентно; порядок важен:
// службы/классификатор → пользователи → сценарии → занятия.
func (c *Core) Seed(ctx context.Context, demo bool) error {
	if err := classifier.SeedReference(ctx, c.Pool); err != nil {
		return fmt.Errorf("seed reference: %w", err)
	}
	if err := c.Catalog.Reload(ctx); err != nil {
		return fmt.Errorf("catalog reload: %w", err)
	}
	if !demo {
		return nil
	}
	if err := auth.SeedDemoUsers(ctx, c.Pool); err != nil {
		return fmt.Errorf("seed demo users: %w", err)
	}
	if err := scenarios.SeedDemoScenarios(ctx, scenarios.Deps{Pool: c.Pool, Settings: c.Settings, Catalog: c.Catalog,
		Queue: c.AI, AI: c.AI, Auditor: c.Audit, Log: c.Log}); err != nil {
		return fmt.Errorf("seed demo scenarios: %w", err)
	}
	if err := lessons.SeedDemoLessons(ctx, lessons.Deps{Pool: c.Pool, Settings: c.Settings, Publisher: c.Hub,
		Auditor: c.Audit, Log: c.Log}, c.Lessons); err != nil {
		return fmt.Errorf("seed demo lessons: %w", err)
	}
	return nil
}

// Serve — основной режим: миграции, сиды, воркеры, HTTP(S), graceful shutdown по ctx.
func Serve(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	if err := cfg.CheckServe(); err != nil {
		return fmt.Errorf("небезопасная конфигурация: %w", err)
	}
	for _, w := range cfg.SecurityWarnings() {
		log.Warn("SECURITY: " + w)
	}
	if cfg.AutoMigrate {
		if err := Migrate(ctx, cfg, log, "up"); err != nil {
			return err
		}
	}
	c, err := Build(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer c.Pool.Close()

	// Слушатели (и TLS) — до запуска воркеров и сидов: занятый порт или битый сертификат —
	// выход сразу, без запущенных и не остановленных писателей аудита/событий/диспетчера.
	servers, err := c.servers()
	if err != nil {
		return err
	}

	// Воркеры живут в собственном контексте: при остановке сначала перестаём принимать
	// запросы, затем гасим воркеры в правильном порядке (они пишут в БД до конца).
	bg, stopBG := context.WithCancel(context.WithoutCancel(ctx))
	defer stopBG()

	c.Audit.Start(bg)
	c.Events.Start(bg)
	if cfg.SeedOnStart {
		if err := c.Seed(ctx, cfg.DemoMode); err != nil {
			log.Error("seed failed — продолжаем без сидов", "err", err)
		}
	} else if err := c.Catalog.Reload(ctx); err != nil {
		log.Error("catalog reload", "err", err)
	}
	c.Ops.RegisterMetrics()
	metrics.RegisterPool(c.Pool)
	c.AI.OnBreakerChange(func(state string) {
		log.Warn("ai-service circuit breaker", "state", state)
		c.Admin.OnBreakerChange(state) // мгновенный aiHealth в открытые мониторы
	})
	c.AI.Start(bg)
	c.Ops.Start(bg)
	go c.Settings.Run(bg, settings.RefreshEvery)
	go c.Catalog.Run(bg, 30*time.Second)
	go c.Admin.RunAIHealthBroadcast(bg)

	g, gctx := errgroup.WithContext(ctx)
	for _, s := range servers {
		s := s
		g.Go(func() error {
			log.Info("listening", "addr", s.srv.Addr, "tls", s.tls)
			var err error
			if s.tls {
				err = s.srv.ServeTLS(s.ln, "", "")
			} else {
				err = s.srv.Serve(s.ln)
			}
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return fmt.Errorf("server %s: %w", s.srv.Addr, err)
		})
	}
	g.Go(func() error {
		<-gctx.Done()
		log.Info("shutting down")
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		// все слушатели разом: пока один дожидается активных запросов, другой не должен
		// принимать новые
		var wg sync.WaitGroup
		for _, s := range servers {
			wg.Go(func() { _ = s.srv.Shutdown(sctx) })
		}
		wg.Wait()
		c.Hub.Close() // hijacked WS не закрывает Shutdown; до закрытия пула (SetOnline пишет в БД)
		stopBG()
		_ = c.AI.Stop(sctx)
		_ = c.Events.Stop(sctx)
		_ = c.Ops.Wait(sctx)
		_ = c.Audit.Stop(sctx)
		return nil
	})
	return g.Wait()
}

type server struct {
	srv *http.Server
	ln  net.Listener
	tls bool
}

func (c *Core) servers() ([]server, error) {
	var out []server
	mk := func(addr string, h http.Handler) *http.Server {
		return &http.Server{
			Addr:              addr,
			Handler:           h,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    64 << 10,
			ErrorLog:          slog.NewLogLogger(c.Log.Handler(), slog.LevelWarn),
		}
	}
	if c.Cfg.HTTPAddr != "" {
		ln, err := net.Listen("tcp", c.Cfg.HTTPAddr)
		if err != nil {
			return nil, fmt.Errorf("listen %s: %w", c.Cfg.HTTPAddr, err)
		}
		out = append(out, server{srv: mk(c.Cfg.HTTPAddr, c.Internal), ln: ln})
	}
	if c.Cfg.HTTPSAddr != "" {
		tlsCfg, err := ops.EnsureTLS(c.Cfg, c.Log)
		if err != nil {
			return nil, fmt.Errorf("tls: %w", err)
		}
		ln, err := net.Listen("tcp", c.Cfg.HTTPSAddr)
		if err != nil {
			return nil, fmt.Errorf("listen %s: %w", c.Cfg.HTTPSAddr, err)
		}
		s := mk(c.Cfg.HTTPSAddr, c.Public)
		s.TLSConfig = tlsCfg
		out = append(out, server{srv: s, ln: ln, tls: true})
	}
	if len(out) == 0 {
		return nil, errors.New("no listeners: set GOCORE_HTTP_ADDR and/or GOCORE_HTTPS_ADDR")
	}
	return out, nil
}

// ---------------------------------------------------------------- CLI

// Seed — команда `gocore seed`.
func Seed(ctx context.Context, cfg *config.Config, log *slog.Logger, demo bool) error {
	if cfg.AutoMigrate {
		if err := Migrate(ctx, cfg, log, "up"); err != nil {
			return err
		}
	}
	c, err := Build(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer c.Pool.Close()
	c.Audit.Start(ctx)
	defer func() { _ = c.Audit.Stop(context.WithoutCancel(ctx)) }()
	if err := c.Seed(ctx, demo); err != nil {
		return err
	}
	log.Info("seed done", "demo", demo)
	return nil
}

// GenCert — команда `gocore gencert`.
func GenCert(cfg *config.Config, log *slog.Logger) error {
	ca, err := ops.GenCert(cfg)
	if err != nil {
		return err
	}
	fmt.Printf("Сертификат контура готов. Установите CA в доверенные корневые центры браузеров стенда:\n  %s\n", ca)
	return nil
}

// ImportClassifier — команда `gocore import-classifier <file>`.
func ImportClassifier(ctx context.Context, cfg *config.Config, log *slog.Logger, path string) error {
	pool, err := pg.Connect(ctx, cfg.DatabaseURL, 4, "go-core-import")
	if err != nil {
		return err
	}
	defer pool.Close()
	// import_batches.created_by — NOT NULL FK: от имени первого администратора.
	var by uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE role = 'admin' AND deleted_at IS NULL ORDER BY created_at LIMIT 1`).Scan(&by); err != nil {
		return fmt.Errorf("нужен хотя бы один администратор (gocore seed --demo или создать вручную): %w", err)
	}
	aw := audit.NewWriter(pool, log)
	aw.Start(ctx)
	defer func() { _ = aw.Stop(context.WithoutCancel(ctx)) }()
	stats, err := classifier.ImportClassifierXLSX(ctx, pool, path, by)
	aw.Log(ctx, core.AuditEntry{Action: "classifier.import", EntityType: "import_batch", EntityID: stats.BatchID, ActorID: &by,
		ActorRole: core.RoleAdmin, After: map[string]any{"file": path, "stats": stats, "ok": err == nil}})
	if err != nil {
		return err
	}
	fmt.Printf("Импорт завершён: %+v\n", stats)
	return nil
}

// Backup — команда `gocore backup`.
func Backup(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	pool, err := pg.Connect(ctx, cfg.DatabaseURL, 2, "go-core-backup")
	if err != nil {
		return err
	}
	defer pool.Close()
	st := settings.NewStore(pool, log)
	aw := audit.NewWriter(pool, log)
	aw.Start(ctx)
	defer func() { _ = aw.Stop(context.WithoutCancel(ctx)) }()
	o := ops.New(ops.Deps{Pool: pool, Config: cfg, Settings: st, Auditor: aw, Log: log})
	b, err := o.RunBackupSync(ctx, nil)
	if err != nil {
		return err
	}
	fmt.Printf("Резервная копия: %s (%v байт)\n", deref(b.FilePath), deref(b.SizeBytes))
	return nil
}

// HashPassword — команда `gocore hash-password`.
func HashPassword(pw string) (string, error) { return auth.HashPassword(pw) }

func deref[T any](p *T) T {
	var z T
	if p == nil {
		return z
	}
	return *p
}
