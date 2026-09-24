// Package lessons — занятия: создание, запуск и завершение, выдача попыток
// (core.AttemptIssuer), живой мониторинг (core.LessonMonitor, core.AttemptPresence)
// и демо-занятия.
//
// Правила пакета:
//   - представления Lesson/Attempt собирает только store (LessonToPublic/AttemptToPublic):
//     одна форма ответа на весь сервис;
//   - доступ к объекту — через internal/access (владелец/админ/участник);
//   - побочные эффекты наружу (WebSocket) — только после COMMIT (pg.OnCommit);
//   - порядок блокировок согласован с attempts/evaluation: сначала строка lessons, потом
//     attempts, потом lesson_participants (см. finish; accept/submit берут занятие FOR SHARE
//     до попытки) — иначе submit и завершение занятия взаимно блокировались бы.
package lessons

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/settings"
	"lct/gocore/internal/store"
)

// Deps — зависимости сервиса (связывает internal/app). Publisher и Auditor могут быть nil
// (CLI-сид без веб-сервера) — тогда публикации и аудит просто пропускаются.
type Deps struct {
	Pool      *pgxpool.Pool
	Settings  *settings.Store
	Publisher core.Publisher
	Auditor   core.Auditor
	Log       *slog.Logger
}

// Service — занятия. Реализует core.AttemptIssuer, core.LessonMonitor, core.AttemptPresence.
type Service struct {
	pool     *pgxpool.Pool
	settings *settings.Store
	pub      core.Publisher
	audit    core.Auditor
	log      *slog.Logger
}

var (
	_ core.AttemptIssuer   = (*Service)(nil)
	_ core.LessonMonitor   = (*Service)(nil)
	_ core.AttemptPresence = (*Service)(nil)
)

// New — без фоновых горутин и без I/O.
func New(d Deps) *Service {
	s := &Service{pool: d.Pool, settings: d.Settings, pub: d.Publisher, audit: d.Auditor, log: d.Log}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s
}

// Register — маршруты /lessons* (относительно /api/v1). Объектный доступ («своё ли занятие»)
// проверяется в хендлерах через internal/access.
func (s *Service) Register(r *httpx.Router) {
	teacherAdmin := httpx.Roles(core.RoleTeacher, core.RoleAdmin)
	r.Handle("GET /lessons/default-settings", teacherAdmin, s.defaultSettings)
	r.Handle("GET /lessons", teacherAdmin, s.list)
	r.Handle("POST /lessons", httpx.Roles(core.RoleTeacher), s.create)
	r.Handle("GET /lessons/assigned", httpx.Roles(core.RoleStudent), s.assigned)
	r.Handle("GET /lessons/{lessonId}", httpx.Authenticated, s.get)
	r.Handle("POST /lessons/{lessonId}/start", teacherAdmin, s.start)
	r.Handle("POST /lessons/{lessonId}/finish", teacherAdmin, s.finish)
	r.Handle("GET /lessons/{lessonId}/attempts", teacherAdmin, s.attempts)
}

// ---------------------------------------------------------------- общие помощники

// defaultLessonSettings — чем добиваются отсутствующие ключи lessons.settings при чтении
// (как в store: дефолты миграций, а не живой снимок).
var defaultLessonSettings = model.DefaultLessonSettings(nil)

func principal(r *http.Request) (*core.Principal, error) {
	p := core.PrincipalFrom(r.Context())
	if p == nil {
		return nil, httpx.Unauthorized()
	}
	return p, nil
}

// snapshot — настройки платформы (без Settings — дефолты миграций, для CLI/тестов).
func (s *Service) snapshot(ctx context.Context) *settings.Snapshot {
	if s.settings == nil {
		d := settings.Defaults()
		return &d
	}
	return s.settings.Get(ctx)
}

func (s *Service) logAudit(ctx context.Context, e core.AuditEntry) {
	if s.audit != nil {
		s.audit.Log(ctx, e)
	}
}

func errLessonNotFound() *httpx.Error { return httpx.NotFound("Занятие не найдено") }

// ownerRow — минимальная строка занятия для access.ManageLesson (нужен только владелец).
func ownerRow(teacherID *uuid.UUID, createdBy uuid.UUID) *store.LessonRow {
	return &store.LessonRow{TeacherID: teacherID, CreatedBy: createdBy}
}

// ---------------------------------------------------------------- публикации в WS

func (s *Service) publishParticipant(lessonID uuid.UUID, p public.LessonParticipant) {
	if s.pub == nil {
		return
	}
	s.pub.Monitor(lessonID, public.MonitorMessage{
		Type:        public.MonitorMessageTypeParticipantStatus,
		Participant: &p,
		AttemptId:   p.AttemptId,
	})
}

func (s *Service) publishLessonStatus(lessonID uuid.UUID, status string) {
	if s.pub == nil {
		return
	}
	st := public.LessonStatus(status)
	s.pub.Monitor(lessonID, public.MonitorMessage{Type: public.MonitorMessageTypeLessonStatus, LessonStatus: &st})
}

func (s *Service) publishAttemptEvent(lessonID, attemptID uuid.UUID, ev public.AttemptEvent) {
	if s.pub == nil {
		return
	}
	id := attemptID
	s.pub.Monitor(lessonID, public.MonitorMessage{Type: public.MonitorMessageTypeAttemptEvent, AttemptId: &id, Event: &ev})
}

// publishLesson — состояние занятия в монитор: статус и каждый участник (после старта/финиша
// у участников меняются attemptId/статус — монитор не должен ждать нового снимка).
func (s *Service) publishLesson(l *public.Lesson) {
	if s.pub == nil {
		return
	}
	s.publishLessonStatus(l.Id, string(l.Status))
	for i := range l.Participants {
		s.publishParticipant(l.Id, l.Participants[i])
	}
}

// onlyParticipant — обучающемуся не отдаём ФИО одногруппников (минимизация ПДн): в его
// представлении занятия — только он сам. Интерфейсу студента список участников не нужен.
func onlyParticipant(l *public.Lesson, userID uuid.UUID) {
	out := make([]public.LessonParticipant, 0, 1)
	for _, p := range l.Participants {
		if p.UserId == userID {
			out = append(out, p)
		}
	}
	l.Participants = out
}
