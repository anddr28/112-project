// Package admin — администрирование контура (frontend.v1.yaml, тег admin): здоровье
// (go-core, PostgreSQL, ai-service, очередь AI), настройки платформы, журнал аудита,
// резервные копии; плюс периодическое сообщение aiHealth в открытые мониторы занятий.
//
// Пакет не владеет данными: настройки — settings.Store, бэкапы — ops, аудит — audit,
// очередь и ai-service — aijobs (через узкие интерфейсы ниже). Здесь только HTTP-граница,
// проверка ввода и сборка ответов контракта.
package admin

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/config"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/settings"
)

// JobCounter — ai_jobs по статусам (реализация: *aijobs.Service).
type JobCounter interface {
	Counts(ctx context.Context) (map[string]int, error)
}

// OpsAPI — резервное копирование (реализация: *ops.Ops). StartBackup возвращает
// ops.ErrBackupRunning, если копирование уже идёт.
type OpsAPI interface {
	StartBackup(ctx context.Context, by *uuid.UUID) (public.Backup, error)
	ListBackups(ctx context.Context) ([]public.Backup, error)
	LastBackupAt(ctx context.Context) (*time.Time, error)
}

// WSStats — состояние WebSocket-хаба (реализация: *realtime.Hub).
type WSStats interface {
	Connections() int
	// WatchedLessons — занятия, у которых сейчас открыт хотя бы один монитор.
	WatchedLessons() []uuid.UUID
}

// Deps — зависимости пакета (связывает internal/app).
type Deps struct {
	Pool      *pgxpool.Pool
	Config    *config.Config
	Settings  *settings.Store
	AI        core.AIClient // Health/Queue — с кэшем 5 с внутри aijobs
	Jobs      JobCounter
	Ops       OpsAPI
	WS        WSStats
	Publisher core.Publisher
	Auditor   core.Auditor
	StartedAt time.Time // момент старта процесса — для goCore.uptimeSec
	Log       *slog.Logger

	// BroadcastEvery — период aiHealth в мониторы занятий (0 — 10 с).
	BroadcastEvery time.Duration
}

// Handlers — HTTP-ручки /admin/* и фоновая рассылка aiHealth.
type Handlers struct {
	pool    *pgxpool.Pool
	cfg     *config.Config
	set     *settings.Store
	ai      core.AIClient
	jobs    JobCounter
	ops     OpsAPI
	ws      WSStats
	pub     core.Publisher
	aud     core.Auditor
	started time.Time
	log     *slog.Logger

	every time.Duration
	// kick — внеочередная рассылка aiHealth (смена состояния breaker'а). Буфер 1:
	// несколько толчков подряд схлопываются в одну рассылку, отправитель не блокируется.
	kick chan struct{}
	// setMu — правки настроек в процессе по одной (семафор с отменой по ctx; см. lockSetting).
	setMu chan struct{}
}

const defaultBroadcastEvery = 10 * time.Second

// New — без фоновых горутин и I/O; рассылку запускает RunAIHealthBroadcast.
func New(d Deps) *Handlers {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	started := d.StartedAt
	if started.IsZero() {
		started = time.Now()
	}
	every := d.BroadcastEvery
	if every <= 0 {
		every = defaultBroadcastEvery
	}
	return &Handlers{
		pool:    d.Pool,
		cfg:     d.Config,
		set:     d.Settings,
		ai:      d.AI,
		jobs:    d.Jobs,
		ops:     d.Ops,
		ws:      d.WS,
		pub:     d.Publisher,
		aud:     d.Auditor,
		started: started,
		log:     log.With("component", "admin"),
		every:   every,
		kick:    make(chan struct{}, 1),
		setMu:   make(chan struct{}, 1),
	}
}

// Register — маршруты администратора (относительно /api/v1). Все — только роль admin.
func (h *Handlers) Register(r *httpx.Router) {
	admin := httpx.Roles(core.RoleAdmin)
	r.Handle("GET /admin/health", admin, h.health)
	r.Handle("GET /admin/settings", admin, h.listSettings)
	r.Handle("PUT /admin/settings/{key}", admin, h.updateSetting)
	r.Handle("GET /admin/audit", admin, h.listAudit)
	r.Handle("GET /admin/backups", admin, h.listBackups)
	r.Handle("POST /admin/backups", admin, h.runBackup)
}

// newOf — новый нулевой T по указателю нужного типа. Нужен для анонимных вложенных
// структур сгенерированных типов (SystemHealth.aiService.queue, MonitorMessage.aiHealth):
// тип выводится из поля, и при перегенерации контракта код не расходится с ним молча.
func newOf[T any](_ *T) *T { return new(T) }
