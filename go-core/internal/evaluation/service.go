// Package evaluation — оценка попытки (DESIGN §5 «Оценка»): слои fields и timing мгновенно
// при сдаче, AI-слои grammar/semantic/dialogue — задачами ai_jobs с доездом результата
// callback'ом; итог по весам занятия, вердикт, XP, завершение занятия для студента;
// ручная корректировка и комментарии преподавателя.
//
// Реализует core.Evaluator (вызывается из транзакции submit) и core.AIResultHandler
// (вызывается aijobs в транзакции callback'а под блокировкой задачи). Наружу (WebSocket) —
// только после COMMIT через pg.OnCommit: evaluationUpdated в канал попытки (студент) и в
// канал мониторинга занятия (преподаватель).
package evaluation

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/settings"
)

// Deps — зависимости (связывает internal/app).
type Deps struct {
	Pool      *pgxpool.Pool
	Settings  *settings.Store
	Catalog   core.Catalog   // подписи полей, коды категорий для ai-service
	Queue     core.JobQueue  // ai_jobs (aijobs.Service)
	Publisher core.Publisher // WS-хаб; nil — без пушей (CLI, тесты)
	Auditor   core.Auditor   // nil — без аудита (CLI, тесты)
	Log       *slog.Logger
}

// Service — оценка попыток.
type Service struct {
	pool  *pgxpool.Pool
	st    *settings.Store
	cat   core.Catalog
	queue core.JobQueue
	pub   core.Publisher
	aud   core.Auditor
	log   *slog.Logger
}

var (
	_ core.Evaluator       = (*Service)(nil)
	_ core.AIResultHandler = (*Service)(nil)
)

// New — без I/O и фоновых горутин.
func New(d Deps) *Service {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	return &Service{pool: d.Pool, st: d.Settings, cat: d.Catalog, queue: d.Queue, pub: d.Publisher, aud: d.Auditor, log: log}
}

// RegisterResults — обработчики результатов AI-слоёв оценки (вызвать до aijobs.Start).
func (s *Service) RegisterResults(router core.AIResultRouter) {
	router.Register(core.JobEvaluateGrammar, s)
	router.Register(core.JobEvaluateSemantic, s)
	router.Register(core.JobEvaluateDialogue, s)
}

// Register — маршруты оценки и обратной связи (frontend.v1.yaml, тег evaluation).
// Роль проверяет роутер; «чьё» (владелец попытки / преподаватель занятия) — internal/access.
func (s *Service) Register(r *httpx.Router) {
	r.Handle("GET /attempts/{attemptId}/evaluation", httpx.Authenticated, s.handleGet)
	r.Handle("POST /attempts/{attemptId}/evaluation/override", httpx.Roles(core.RoleTeacher, core.RoleAdmin), s.handleOverride)
	r.Handle("GET /attempts/{attemptId}/feedback", httpx.Authenticated, s.handleListFeedback)
	r.Handle("POST /attempts/{attemptId}/feedback", httpx.Roles(core.RoleTeacher, core.RoleAdmin), s.handleAddFeedback)
}

// publish — evaluationUpdated студенту (канал попытки) и преподавателю (канал занятия).
// Вызывать только из pg.OnCommit: клиент не должен увидеть оценку, которой нет в БД.
func (s *Service) publish(lessonID, attemptID uuid.UUID, ev *public.Evaluation) {
	if s.pub == nil {
		return
	}
	aid := attemptID
	s.pub.Monitor(lessonID, public.MonitorMessage{
		Type:       public.MonitorMessageTypeEvaluationUpdated,
		AttemptId:  &aid,
		Evaluation: ev,
	})
	s.pub.Student(attemptID, public.StudentMessage{
		Type:       public.StudentMessageTypeEvaluationUpdated,
		Evaluation: ev,
	})
}

// publishParticipant — участник завершил занятие (participantStatus в монитор).
func (s *Service) publishParticipant(lessonID uuid.UUID, p *public.LessonParticipant) {
	if s.pub == nil || p == nil {
		return
	}
	s.pub.Monitor(lessonID, public.MonitorMessage{
		Type:        public.MonitorMessageTypeParticipantStatus,
		AttemptId:   p.AttemptId,
		Participant: p,
	})
}

// audit — запись аудита (nil-безопасно; Log не блокирует).
func (s *Service) audit(ctx context.Context, e core.AuditEntry) {
	if s.aud != nil {
		s.aud.Log(ctx, e)
	}
}
