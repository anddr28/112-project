package lessons

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/access"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/store"
)

// ---------------------------------------------------------------- GET /lessons

// Размеры страниц списков (контракт v1.2: ?limit, по умолчанию/максимум). Список занятий
// тяжёлый (участники каждого занятия), поэтому потолок — 200 строк на запрос.
const (
	lessonsPageDefault  = 100
	lessonsPageMax      = 200
	assignedPageDefault = 50
	assignedPageMax     = 200
)

func (s *Service) list(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	f := store.LessonFilter{Status: r.URL.Query().Get("status")}
	if f.Status != "" && !public.LessonStatus(f.Status).Valid() {
		return httpx.Validation("Некорректный фильтр", map[string]string{"status": "Допустимо: draft, scheduled, running, finished, cancelled"})
	}
	// Преподаватель видит свои занятия, администратор — все (постранично: keyset, без OFFSET).
	if p.Role != core.RoleAdmin {
		id := p.UserID
		f.TeacherID = &id
	}
	page, err := httpx.ParsePage(r, lessonsPageDefault, lessonsPageMax)
	if err != nil {
		return err
	}
	if page.Cursor != nil {
		k, ok := store.ParseTimeKey(page.Cursor)
		if !ok {
			return httpx.BadCursor()
		}
		f.After = k
	}
	f.Limit = page.Limit + 1 // +1 строка — узнать, есть ли следующая страница, без count(*)
	rows, err := store.ListLessons(r.Context(), s.pool, f)
	if err != nil {
		return httpx.Internal(err)
	}
	if len(rows) > page.Limit {
		rows = rows[:page.Limit]
		httpx.SetNextCursor(w, rows[len(rows)-1].Key().Parts()...)
	}
	httpx.WriteJSON(w, http.StatusOK, store.LessonsToPublic(rows))
	return nil
}

// ---------------------------------------------------------------- GET /lessons/{lessonId}

func (s *Service) get(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	id, err := httpx.PathUUID(r, "lessonId")
	if err != nil {
		return errLessonNotFound()
	}
	row, err := store.GetLesson(r.Context(), s.pool, id)
	if err != nil {
		if pg.IsNoRows(err) {
			return errLessonNotFound()
		}
		return httpx.Internal(err)
	}
	if err := access.ViewLesson(p, row); err != nil {
		return err
	}
	out := store.LessonToPublic(row)
	if p.Role == core.RoleStudent {
		onlyParticipant(&out, p.UserID)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

// ---------------------------------------------------------------- GET /lessons/assigned

// assignedItem — элемент ответа /lessons/assigned.
type assignedItem struct {
	Lesson  public.Lesson   `json:"lesson"`
	Attempt *public.Attempt `json:"attempt,omitempty"`
}

func (s *Service) assigned(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	me := p.UserID
	page, err := httpx.ParsePage(r, assignedPageDefault, assignedPageMax)
	if err != nil {
		return err
	}
	f := store.LessonFilter{ParticipantUserID: &me, ExcludeCancelled: true, Limit: page.Limit + 1}
	if page.Cursor != nil {
		k, ok := store.ParseAssignedKey(page.Cursor)
		if !ok {
			return httpx.BadCursor()
		}
		f.AssignedAfter = k
	}
	// Порядок «что можно делать сейчас — сверху» (идёт → запланировано → черновик → завершено)
	// считает SQL: страница режется по нему же, без сортировки в памяти.
	lessons, err := store.ListLessons(ctx, s.pool, f)
	if err != nil {
		return httpx.Internal(err)
	}
	if len(lessons) > page.Limit {
		lessons = lessons[:page.Limit]
		httpx.SetNextCursor(w, lessons[len(lessons)-1].AssignedKey().Parts()...)
	}

	// Текущая попытка — последняя по seq_no; её id уже посчитан в строке участника
	// (store: backward scan по уникальному индексу), сами попытки — одним запросом по PK.
	var attemptIDs []uuid.UUID
	for i := range lessons {
		if pr, ok := lessons[i].Participant(me); ok && pr.AttemptID != nil {
			attemptIDs = append(attemptIDs, *pr.AttemptID)
		}
	}

	byLesson := make(map[uuid.UUID]*store.LessonRow, len(lessons))
	for i := range lessons {
		byLesson[lessons[i].ID] = &lessons[i]
	}
	attempts, err := loadCurrentAttempts(ctx, s.pool, me, attemptIDs, byLesson)
	if err != nil {
		return httpx.Internal(err)
	}

	now := time.Now()
	out := make([]assignedItem, len(lessons))
	for i := range lessons {
		out[i].Lesson = store.LessonToPublic(&lessons[i])
		onlyParticipant(&out[i].Lesson, me)
		if a, ok := attempts[lessons[i].ID]; ok {
			v := store.AttemptToPublic(a, now)
			out[i].Attempt = &v
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

// Колонки attempts — в порядке store (AttemptRow); поля занятия берутся из уже
// загруженной строки занятия, поэтому JOIN lessons не нужен. user_id — страховка:
// чужая попытка не попадёт в ответ, даже если id пришёл не тот.
const sqlAttemptsByIDs = `
SELECT a.id, a.lesson_id, a.user_id, a.scenario_id, a.etalon_id, a.mode, a.seq_no, a.status,
       a.time_limit_sec, a.issued_at, a.call_accepted_at, a.first_input_at, a.submitted_at,
       a.time_spent_ms, a.replay_count, a.card, a.action_text, a.created_at, a.updated_at,
       a.incident_no, a.call_ended_at, a.call_end_reason, a.dialogue_turns,
       sv.id::text, sv.code, sv.name, sv.short_name, sv.kind
  FROM attempts a
  LEFT JOIN services sv ON sv.id = a.service_id
 WHERE a.id = ANY($1::uuid[]) AND a.user_id = $2`

// loadCurrentAttempts — попытки по id, разложенные по занятиям (поля занятия — из lessons).
func loadCurrentAttempts(ctx context.Context, q pg.Querier, userID uuid.UUID, attemptIDs []uuid.UUID,
	lessons map[uuid.UUID]*store.LessonRow) (map[uuid.UUID]*store.AttemptRow, error) {
	out := make(map[uuid.UUID]*store.AttemptRow, len(attemptIDs))
	if len(attemptIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, sqlAttemptsByIDs, attemptIDs, userID)
	if err != nil {
		return nil, fmt.Errorf("lessons: current attempts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		a := new(store.AttemptRow)
		var (
			card                                 []byte
			svID, svCode, svName, svShort, svKnd *string
		)
		if err := rows.Scan(&a.ID, &a.LessonID, &a.UserID, &a.ScenarioID, &a.EtalonID, &a.Mode, &a.SeqNo, &a.Status,
			&a.TimeLimitSec, &a.IssuedAt, &a.CallAcceptedAt, &a.FirstInputAt, &a.SubmittedAt,
			&a.TimeSpentMs, &a.ReplayCount, &card, &a.ActionText, &a.CreatedAt, &a.UpdatedAt,
			&a.IncidentNo, &a.CallEndedAt, &a.CallEndReason, &a.DialogueTurns,
			&svID, &svCode, &svName, &svShort, &svKnd); err != nil {
			return nil, fmt.Errorf("lessons: current attempts: %w", err)
		}
		a.Service = store.ServiceRef(svID, svCode, svName, svShort, svKnd)
		l, ok := lessons[a.LessonID]
		if !ok {
			continue
		}
		a.Card = card
		a.LessonTitle, a.LessonStatus, a.LessonKind = l.Title, l.Status, l.Kind
		a.LessonTeacherID, a.LessonCreatedBy, a.LessonSettings = l.TeacherID, l.CreatedBy, l.Settings
		out[a.LessonID] = a
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lessons: current attempts: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------- GET /lessons/{lessonId}/attempts

// Владелец занятия — для ответа 404/403, когда попыток ещё нет.
const sqlLessonOwner = `SELECT teacher_id, created_by FROM lessons WHERE id = $1`

func (s *Service) attempts(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	id, err := httpx.PathUUID(r, "lessonId")
	if err != nil {
		return errLessonNotFound()
	}
	ctx := r.Context()
	rows, err := store.ListAttempts(ctx, s.pool, store.AttemptFilter{LessonID: &id})
	if err != nil {
		return httpx.Internal(err)
	}
	// Горячий путь (монитор опрашивает раз в 3 с): владелец берётся из самих строк попыток
	// (в них есть teacher_id/created_by занятия) — отдельный запрос только для пустого списка.
	if len(rows) > 0 {
		if err := access.ManageLesson(p, ownerRow(rows[0].LessonTeacherID, rows[0].LessonCreatedBy)); err != nil {
			return err
		}
	} else if err := s.checkManage(ctx, p, id); err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, store.AttemptsToPublic(rows, time.Now()))
	return nil
}

// checkManage — занятие существует (404) и субъект — его преподаватель или админ (403).
func (s *Service) checkManage(ctx context.Context, p *core.Principal, lessonID uuid.UUID) error {
	var (
		teacherID *uuid.UUID
		createdBy uuid.UUID
	)
	if err := s.pool.QueryRow(ctx, sqlLessonOwner, lessonID).Scan(&teacherID, &createdBy); err != nil {
		if pg.IsNoRows(err) {
			return errLessonNotFound()
		}
		return httpx.Internal(err)
	}
	return access.ManageLesson(p, ownerRow(teacherID, createdBy))
}
