package lessons

import (
	"context"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/access"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/store"
)

// ---------------------------------------------------------------- core.LessonMonitor

// CanMonitor — мониторинг занятия: преподаватель-владелец или админ. Лёгкий запрос по PK —
// проверка идёт до WebSocket-апгрейда, тяжёлый снимок строится уже после.
func (s *Service) CanMonitor(ctx context.Context, p *core.Principal, lessonID uuid.UUID) error {
	if p == nil {
		return httpx.Unauthorized()
	}
	return s.checkManage(ctx, p, lessonID)
}

// Snapshot — первое сообщение канала мониторинга: занятие и все его попытки.
func (s *Service) Snapshot(ctx context.Context, lessonID uuid.UUID) (*public.Lesson, []public.Attempt, error) {
	row, err := store.GetLesson(ctx, s.pool, lessonID)
	if err != nil {
		if pg.IsNoRows(err) {
			return nil, nil, errLessonNotFound()
		}
		return nil, nil, err
	}
	rows, err := store.ListAttempts(ctx, s.pool, store.AttemptFilter{LessonID: &lessonID})
	if err != nil {
		return nil, nil, err
	}
	l := store.LessonToPublic(row)
	return &l, store.AttemptsToPublic(rows, time.Now()), nil
}

// ---------------------------------------------------------------- core.AttemptPresence

const sqlAttemptOwner = `SELECT lesson_id, user_id FROM attempts WHERE id = $1`

// AttemptAccess — канал попытки открывает только обучающийся, которому она выдана.
func (s *Service) AttemptAccess(ctx context.Context, p *core.Principal, attemptID uuid.UUID) (uuid.UUID, error) {
	if p == nil {
		return uuid.Nil, httpx.Unauthorized()
	}
	var lessonID, userID uuid.UUID
	if err := s.pool.QueryRow(ctx, sqlAttemptOwner, attemptID).Scan(&lessonID, &userID); err != nil {
		if pg.IsNoRows(err) {
			return uuid.Nil, httpx.NotFound("Попытка не найдена")
		}
		return uuid.Nil, httpx.Internal(err)
	}
	if err := access.OwnAttempt(p, &store.AttemptRow{UserID: userID}); err != nil {
		return uuid.Nil, err
	}
	return lessonID, nil
}

// Присутствие — одним оператором: строка участника меняется, только если занятие идёт и
// участник не закончил (завершившего не превращаем в «нет связи»). Онлайн до первого
// принятого вызова — joined («Подключился»), после — active; офлайн — disconnected.
// RETURNING отдаёт всё для participantStatus (ФИО, текущая попытка).
const sqlSetPresence = `
UPDATE lesson_participants p
   SET status = CASE WHEN $2 THEN CASE WHEN p.joined_at IS NULL THEN 'joined' ELSE 'active' END
                     ELSE 'disconnected' END,
       last_seen_at = now()
  FROM attempts a
  JOIN lessons l ON l.id = a.lesson_id
  JOIN users u ON u.id = a.user_id
 WHERE a.id = $1
   AND p.lesson_id = a.lesson_id AND p.user_id = a.user_id
   AND l.status = 'running' AND p.status <> 'finished'
RETURNING p.lesson_id, p.user_id, u.last_name, u.first_name, COALESCE(u.middle_name, ''),
          p.status, p.joined_at, p.finished_at, p.mic_ready,
          (SELECT x.id FROM attempts x WHERE x.lesson_id = p.lesson_id AND x.user_id = p.user_id
            ORDER BY x.seq_no DESC LIMIT 1)`

// SetOnline — WS попытки открыт/закрыт (хаб зовёт только на переходах 0↔1 по участнику).
// Ошибки не возвращаются: присутствие — телеметрия, логируем и живём дальше.
func (s *Service) SetOnline(ctx context.Context, attemptID uuid.UUID, online bool) {
	var (
		lessonID uuid.UUID
		pr       store.ParticipantRow
	)
	err := s.pool.QueryRow(ctx, sqlSetPresence, attemptID, online).Scan(
		&lessonID, &pr.UserID, &pr.LastName, &pr.FirstName, &pr.MiddleName,
		&pr.Status, &pr.JoinedAt, &pr.FinishedAt, &pr.MicReady, &pr.AttemptID)
	if err != nil {
		if !pg.IsNoRows(err) { // нет строки — занятие не идёт или участник закончил: норма
			s.log.Warn("lessons: presence update failed", "attempt", attemptID, "online", online, "err", err)
		}
		return
	}
	s.publishParticipant(lessonID, store.ParticipantToPublic(&pr))
}
