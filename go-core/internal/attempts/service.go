// Package attempts — попытка обучающегося: просмотр, приём вызова, легенда для студента,
// черновик карточки (автосохранение), журнал событий, службы реагирования на карточке,
// «переспросить заявителя» и сдача карточки (frontend.v1.yaml, тег attempts).
//
// Горячие пути (DESIGN §1, §6): PUT черновика — каждые 1–2 с на студента, POST событий —
// батч раз в ~0,5 с. Оба — один запрос к БД на успешном пути: черновик — один guarded upsert
// (тело проверяется на форму карточки, но не перекодируется; службы сливаются с серверными
// в том же операторе), события — group commit eventlog.Writer. Проверка «чья попытка» на
// горячих путях опирается на маленький кэш метаданных попытки (cache.go).
//
// Межпакетные эффекты — только через порты core: разговор (DialogueHooks), оценка
// (Evaluator), выдача следующей карточки (AttemptIssuer), WebSocket (Publisher), аудит.
package attempts

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/core"
	"lct/gocore/internal/eventlog"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/settings"
)

// Deps — зависимости пакета (связывает internal/app).
type Deps struct {
	Pool      *pgxpool.Pool
	Settings  *settings.Store
	Catalog   core.Catalog
	Events    *eventlog.Writer
	Publisher core.Publisher
	Auditor   core.Auditor
	Evaluator core.Evaluator
	Issuer    core.AttemptIssuer
	Dialogue  core.DialogueHooks
	Log       *slog.Logger
}

// Service — хендлеры попытки.
type Service struct {
	pool     *pgxpool.Pool
	settings *settings.Store
	cat      core.Catalog
	events   *eventlog.Writer
	pub      core.Publisher
	aud      core.Auditor
	eval     core.Evaluator
	issuer   core.AttemptIssuer
	dialogue core.DialogueHooks
	log      *slog.Logger

	meta *metaCache
	now  func() time.Time
}

// New собирает сервис. Необязательные порты (Publisher, Auditor, Dialogue, Evaluator, Issuer)
// заменяются заглушками: пакет собирается и тестируется без остального ядра.
func New(d Deps) *Service {
	s := &Service{
		pool:     d.Pool,
		settings: d.Settings,
		cat:      d.Catalog,
		events:   d.Events,
		pub:      d.Publisher,
		aud:      d.Auditor,
		eval:     d.Evaluator,
		issuer:   d.Issuer,
		dialogue: d.Dialogue,
		log:      d.Log,
		meta:     newMetaCache(metaMaxEntries),
		now:      nowUTC,
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.pub == nil {
		s.pub = nopPublisher{}
	}
	if s.aud == nil {
		s.aud = nopAuditor{}
	}
	if s.dialogue == nil {
		s.dialogue = nopDialogue{}
	}
	if s.eval == nil {
		s.eval = nopEvaluator{}
	}
	if s.issuer == nil {
		s.issuer = nopIssuer{}
	}
	return s
}

// Register — маршруты попытки (относительно /api/v1). Роль проверяет роутер,
// «чья попытка» — хендлер через internal/access.
func (s *Service) Register(r *httpx.Router) {
	// Смотреть: студент-владелец, преподаватель-владелец занятия, админ (access.ViewAttempt).
	r.Handle("GET /attempts/{attemptId}", httpx.Authenticated, s.getAttempt)
	r.Handle("GET /attempts/{attemptId}/call-script", httpx.Authenticated, s.callScript)
	r.Handle("GET /attempts/{attemptId}/draft", httpx.Authenticated, s.getDraft)
	r.Handle("GET /attempts/{attemptId}/events", httpx.Authenticated, s.listEvents)

	// Действовать от имени обучающегося: только сам студент (access.OwnAttempt).
	student := httpx.Roles(core.RoleStudent)
	r.Handle("POST /attempts/{attemptId}/accept-call", student, s.acceptCall)
	r.Handle("PUT /attempts/{attemptId}/draft", student, s.putDraft)
	r.Handle("POST /attempts/{attemptId}/events", student, s.postEvents)
	r.Handle("POST /attempts/{attemptId}/services", student, s.addService)
	r.Handle("DELETE /attempts/{attemptId}/services/{serviceId}", student, s.removeService)
	r.Handle("POST /attempts/{attemptId}/services/{serviceId}/status", student, s.changeServiceStatus)
	r.Handle("POST /attempts/{attemptId}/replay", student, s.replay)
	r.Handle("POST /attempts/{attemptId}/submit", student, s.submit)
}

// nowUTC — время сервера с точностью timestamptz (µs): значение в ответе совпадает
// с тем, что потом прочитается из БД.
func nowUTC() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// ---------------------------------------------------------------- ошибки (по-русски)

// *httpx.Error изменяем (WithDetails), поэтому каждый раз — новый экземпляр.

func errNotFound() error { return httpx.NotFound("Попытка не найдена") }

// errClosed — попытка в терминальном статусе (сдана, оценивается, истекла, прервана).
func errClosed(what string) error {
	return httpx.Conflict("Попытка уже завершена — " + what)
}

// errNotAccepted — попытка в issued: работать с карточкой до «Принять вызов» нельзя
// (таймер и время реакции считаются от приёма вызова).
func errNotAccepted() error { return httpx.Conflict("Сначала примите вызов") }

// ---------------------------------------------------------------- заглушки портов

type nopPublisher struct{}

func (nopPublisher) Monitor(uuid.UUID, public.MonitorMessage) {}
func (nopPublisher) Student(uuid.UUID, public.StudentMessage) {}

type nopAuditor struct{}

func (nopAuditor) Log(context.Context, core.AuditEntry) {}

type nopDialogue struct{}

func (nopDialogue) OnCallAccepted(context.Context, pgx.Tx, uuid.UUID) (*public.DialogueTurnView, error) {
	return nil, nil
}
func (nopDialogue) CloseOnSubmit(context.Context, pgx.Tx, uuid.UUID, time.Time) error { return nil }

type nopEvaluator struct{}

func (nopEvaluator) StartEvaluation(context.Context, pgx.Tx, uuid.UUID) error { return nil }

type nopIssuer struct{}

func (nopIssuer) IssueNext(context.Context, pgx.Tx, uuid.UUID, uuid.UUID) (uuid.UUID, bool, error) {
	return uuid.Nil, false, nil
}
