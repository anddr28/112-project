// Package scenarios — библиотека сценариев преподавателя: список, карточка сценария,
// ручное создание, генерация LLM (ai_jobs generate_scenario), правка с версионированием
// эталона, подтверждение с предозвучкой реплик (ai_jobs tts), отклонение, новая версия
// сценария, прослушивание реплики (синхронный TTS), применение результатов generate/tts
// и демо-сценарии стенда.
//
// Правила (DESIGN §5 «Сценарии»):
//   - библиотека общая для всех преподавателей (и админа) — проверок «чей сценарий» нет;
//   - сценарий, стоящий хотя бы в одном занятии, не правится (409 + details.lessonsCount) —
//     только новой версией (POST /scenarios/{id}/versions);
//   - эталон иммутабелен: изменение эталонных полей — новая строка etalons (старая
//     is_current=false), выставленные оценки остаются объяснимыми;
//   - все записи — в транзакции под блокировкой строки сценария; аудит — после COMMIT.
package scenarios

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/core"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/settings"
	"lct/gocore/internal/store"
)

// Deps — зависимости сервиса (связывает internal/app).
type Deps struct {
	Pool     *pgxpool.Pool
	Settings *settings.Store // голос/темп озвучки (settings.tts)
	Catalog  core.Catalog    // классификатор: категории, коды служб, проекции карточки
	Queue    core.JobQueue   // ai_jobs: generate_scenario, tts
	AI       core.AIClient   // синхронный TTS для прослушивания реплики
	Auditor  core.Auditor
	Log      *slog.Logger
}

// Service — сценарии: HTTP-хендлеры и обработчики результатов AI.
type Service struct {
	pool  *pgxpool.Pool
	st    *settings.Store
	cat   core.Catalog
	queue core.JobQueue
	ai    core.AIClient
	aud   core.Auditor
	log   *slog.Logger
}

// New — без I/O и фоновых горутин.
func New(d Deps) *Service {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		pool:  d.Pool,
		st:    d.Settings,
		cat:   d.Catalog,
		queue: d.Queue,
		ai:    d.AI,
		aud:   d.Auditor,
		log:   log.With("pkg", "scenarios"),
	}
}

// Register — маршруты frontend.v1.yaml §scenarios (относительно /api/v1).
func (s *Service) Register(r *httpx.Router) {
	staff := httpx.Roles(core.RoleTeacher, core.RoleAdmin)
	r.Handle("GET /scenarios", staff, s.handleList)
	r.Handle("POST /scenarios", staff, s.handleCreate)
	r.Handle("POST /scenarios/generate", staff, s.handleGenerate)
	r.Handle("GET /scenarios/{scenarioId}", staff, s.handleGet)
	r.Handle("PATCH /scenarios/{scenarioId}", staff, s.handlePatch)
	r.Handle("POST /scenarios/{scenarioId}/approve", staff, s.handleApprove)
	r.Handle("POST /scenarios/{scenarioId}/reject", staff, s.handleReject)
	r.Handle("POST /scenarios/{scenarioId}/versions", staff, s.handleCreateVersion)
	r.Handle("POST /scenarios/{scenarioId}/tts-preview", staff, s.handleTTSPreview)
}

// RegisterResults — обработчики результатов ai-service: генерация сценария и озвучка.
// Вызывать при сборке приложения, до старта диспетчера aijobs.
func (s *Service) RegisterResults(router core.AIResultRouter) {
	router.Register(core.JobGenerateScenario, generateHandler{s})
	router.Register(core.JobTTS, ttsHandler{s})
}

// ---------------------------------------------------------------- общее

// snap — снимок настроек (никогда не nil; без Store — дефолты миграций).
func (s *Service) snap(ctx context.Context) *settings.Snapshot {
	if s.st != nil {
		return s.st.Get(ctx)
	}
	d := settings.Defaults()
	return &d
}

// audit — запись в журнал (асинхронно, не блокирует). Вызывается после COMMIT.
func (s *Service) audit(ctx context.Context, action, entityType string, id uuid.UUID, before, after any) {
	if s.aud == nil {
		return
	}
	s.aud.Log(ctx, core.AuditEntry{Action: action, EntityType: entityType, EntityID: id, Before: before, After: after})
}

// lockAndLoad — блокировка строки сценария и чтение его целиком (эталон, число занятий,
// готовность озвучки). Чтение — отдельный оператор после блокировки: в READ COMMITTED
// он видит всё, что закоммитили до её получения (в т.ч. новые lesson_scenarios).
func lockAndLoad(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*store.ScenarioRow, error) {
	var got uuid.UUID
	if err := tx.QueryRow(ctx, sqlLockScenario, id).Scan(&got); err != nil {
		if pg.IsNoRows(err) {
			return nil, errNotFound()
		}
		return nil, err
	}
	return store.GetScenario(ctx, tx, id)
}

func errNotFound() *httpx.Error { return httpx.NotFound("Сценарий не найден") }

// archived — сценарий в архиве (провалившаяся генерация или убранный из библиотеки).
func archived(r *store.ScenarioRow) bool {
	return r.Status == core.ScenarioArchived || r.ArchivedAt != nil
}
