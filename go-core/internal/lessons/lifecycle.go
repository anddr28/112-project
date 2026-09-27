package lessons

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/access"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/store"
)

// ---------------------------------------------------------------- POST /lessons/{lessonId}/start

// Занятие под FOR UPDATE вместе со всем, что нужно для выдачи: пул и эталоны.
var sqlLockForStart = `
SELECT l.status, l.teacher_id, l.created_by, l.mode, l.time_limit_sec, l.settings,` + poolColumns + `
  FROM lessons l
 WHERE l.id = $1
   FOR UPDATE OF l`

const sqlMarkRunning = `UPDATE lessons SET status = 'running', started_at = now() WHERE id = $1`

func (s *Service) start(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	lessonID, err := httpx.PathUUID(r, "lessonId")
	if err != nil {
		return errLessonNotFound()
	}
	ctx := r.Context()
	var (
		out        public.Lesson
		issued     int
		prevStatus string
	)
	err = pg.WithTx(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		var (
			status, mode  string
			teacherID     *uuid.UUID
			createdBy     uuid.UUID
			timeLimit     int
			rawSettings   []byte
			pool, etalons []uuid.UUID
			codes         []string
		)
		err := tx.QueryRow(ctx, sqlLockForStart, lessonID).Scan(&status, &teacherID, &createdBy, &mode,
			&timeLimit, &rawSettings, &pool, &etalons, &codes)
		if err != nil {
			if pg.IsNoRows(err) {
				return errLessonNotFound()
			}
			return httpx.Internal(err)
		}
		if err := access.ManageLesson(p, ownerRow(teacherID, createdBy)); err != nil {
			return err
		}
		prevStatus = status
		switch status {
		case core.LessonDraft, core.LessonScheduled:
		case core.LessonRunning:
			return httpx.Conflict("Занятие уже запущено")
		default:
			return httpx.Conflict("Занятие уже завершено")
		}
		if len(pool) == 0 {
			return httpx.Conflict("В занятии нет ни одного сценария")
		}
		ls, _ := model.ParseLessonSettings(rawSettings, defaultLessonSettings)

		// Смена статуса и план выдачи — одним пакетом (занятие уже под блокировкой).
		b := &pgx.Batch{}
		b.Queue(sqlMarkRunning, lessonID)
		b.Queue(sqlParticipantsPlan, lessonID)
		br := tx.SendBatch(ctx, b)
		if _, err := br.Exec(); err != nil {
			br.Close()
			return httpx.Internal(fmt.Errorf("lessons: start: %w", err))
		}
		rows, err := br.Query()
		var plans []issuePlan
		if err == nil {
			plans, err = planFromRows(rows, lessonID, &ls, pool, etalons, codes)
		}
		if cerr := br.Close(); err == nil && cerr != nil {
			err = cerr
		}
		if err != nil {
			return httpx.Internal(err)
		}

		events, err := insertPlans(ctx, tx, lessonID, mode, timeLimit, plans)
		if err != nil {
			return httpx.Internal(err)
		}
		issued = len(events)

		row, err := store.GetLesson(ctx, tx, lessonID)
		if err != nil {
			return httpx.Internal(err)
		}
		out = store.LessonToPublic(row)
		lesson := out
		pg.OnCommit(ctx, func() {
			s.publishLesson(&lesson)
			for _, e := range events {
				s.publishAttemptEvent(lessonID, e.AttemptID, e.Event)
			}
		})
		return nil
	})
	if err != nil {
		return err
	}
	s.logAudit(ctx, core.AuditEntry{
		Action: "lesson.start", EntityType: "lesson", EntityID: lessonID, LessonID: lessonID,
		Before: lessonAudit{Status: prevStatus},
		After:  lessonAudit{Status: core.LessonRunning, Participants: len(out.Participants), AttemptsIssued: issued},
	})
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

// ---------------------------------------------------------------- POST /lessons/{lessonId}/finish

// Незавершённые попытки -> expired. Открытый голосовой разговор закрывается как timeout,
// чтобы Attempt.dialogue не показывал «звонок идёт» у закрытой попытки.
const sqlExpireAttempts = `
UPDATE attempts
   SET status = 'expired',
       call_ended_at   = CASE WHEN dialogue_turns > 0 AND call_ended_at IS NULL THEN now() ELSE call_ended_at END,
       call_end_reason = CASE WHEN dialogue_turns > 0 AND call_ended_at IS NULL THEN 'timeout' ELSE call_end_reason END
 WHERE lesson_id = $1 AND status IN ('issued', 'in_progress')`

const sqlMarkFinished = `
UPDATE lessons SET status = 'finished', finished_at = now() WHERE id = $1 AND status = 'running'`

// Экспирация + все попытки занятия для lessonFinished. Идёт после UPDATE lessons: оператор
// берёт новый снимок, в котором уже есть попытки, выданные submit'ами, закоммиченными до
// блокировки занятия (новых после неё не будет — IssueNext увидит finished).
const sqlExpireAndList = `
WITH exp AS (` + sqlExpireAttempts + `
  RETURNING 1
)
SELECT (SELECT count(*)::int FROM exp), ARRAY(SELECT a.id FROM attempts a WHERE a.lesson_id = $1)`

const sqlFinishParticipants = `
UPDATE lesson_participants
   SET status = 'finished', finished_at = COALESCE(finished_at, now())
 WHERE lesson_id = $1 AND status <> 'finished'`

const sqlLessonState = `SELECT status, teacher_id, created_by FROM lessons WHERE id = $1`

// finish — порядок блокировок общий для всего ядра: lessons -> attempts -> lesson_participants.
// accept-call и submit (attempts.lockLessonThenAttempt) сначала берут занятие FOR SHARE, потом
// попытку; IssueNext в submit — снова занятие FOR SHARE (уже взятое). Если бы finish сначала
// блокировал попытки, а потом занятие, он и сдающий ждали бы друг друга (deadlock 40P01 ровно
// в момент «время вышло — все сдают, преподаватель завершает»). Поэтому:
//  1. UPDATE lessons -> finished: ждёт submit/accept, уже держащих занятие FOR SHARE (они
//     доходят до COMMIT, не нуждаясь ни в чём, что держит finish); новые после этого ждут
//     finish и видят finished;
//  2. expire попыток (свежий снимок: в нём и карточки, выданные теми submit'ами) + список;
//  3. участники -> finished.
//
// Все операторы уходят одним пакетом: сервер исполняет их по порядку, семантика блокировок
// та же, round-trip один.
func (s *Service) finish(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	lessonID, err := httpx.PathUUID(r, "lessonId")
	if err != nil {
		return errLessonNotFound()
	}
	ctx := r.Context()

	var (
		status    string
		teacherID *uuid.UUID
		createdBy uuid.UUID
	)
	if err := s.pool.QueryRow(ctx, sqlLessonState, lessonID).Scan(&status, &teacherID, &createdBy); err != nil {
		if pg.IsNoRows(err) {
			return errLessonNotFound()
		}
		return httpx.Internal(err)
	}
	if err := access.ManageLesson(p, ownerRow(teacherID, createdBy)); err != nil {
		return err
	}
	switch status {
	case core.LessonRunning:
	case core.LessonFinished:
		// Повтор после обрыва связи — занятие уже завершено: 200 с текущим состоянием.
		return s.writeLesson(ctx, w, lessonID)
	case core.LessonCancelled:
		return httpx.Conflict("Занятие отменено")
	default:
		return httpx.Conflict("Занятие ещё не запущено")
	}

	var (
		out      public.Lesson
		expired  int
		finished bool
	)
	err = pg.WithTx(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		b := &pgx.Batch{}
		b.Queue(sqlMarkFinished, lessonID)
		b.Queue(sqlExpireAndList, lessonID)
		b.Queue(sqlFinishParticipants, lessonID)
		br := tx.SendBatch(ctx, b)
		var attemptIDs []uuid.UUID
		err := func() error {
			defer br.Close()
			tag, err := br.Exec()
			if err != nil {
				return err
			}
			finished = tag.RowsAffected() == 1
			if err := br.QueryRow().Scan(&expired, &attemptIDs); err != nil {
				return err
			}
			_, err = br.Exec()
			return err
		}()
		if err != nil {
			return httpx.Internal(fmt.Errorf("lessons: finish: %w", err))
		}

		row, err := store.GetLesson(ctx, tx, lessonID)
		if err != nil {
			return httpx.Internal(err)
		}
		out = store.LessonToPublic(row)
		if !finished {
			// Параллельный finish успел раньше — он и публикует.
			return nil
		}
		lesson := out
		pg.OnCommit(ctx, func() {
			s.publishLesson(&lesson)
			if s.pub == nil {
				return
			}
			for _, id := range attemptIDs {
				s.pub.Student(id, public.StudentMessage{Type: public.StudentMessageTypeLessonFinished})
			}
		})
		return nil
	})
	if err != nil {
		return err
	}
	if finished {
		s.logAudit(ctx, core.AuditEntry{
			Action: "lesson.finish", EntityType: "lesson", EntityID: lessonID, LessonID: lessonID,
			Before: lessonAudit{Status: core.LessonRunning},
			After:  lessonAudit{Status: core.LessonFinished, AttemptsExpired: expired},
		})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

// writeLesson — 200 с текущим представлением занятия.
func (s *Service) writeLesson(ctx context.Context, w http.ResponseWriter, lessonID uuid.UUID) error {
	row, err := store.GetLesson(ctx, s.pool, lessonID)
	if err != nil {
		if pg.IsNoRows(err) {
			return errLessonNotFound()
		}
		return httpx.Internal(err)
	}
	httpx.WriteJSON(w, http.StatusOK, store.LessonToPublic(row))
	return nil
}
