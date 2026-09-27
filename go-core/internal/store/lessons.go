package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/pg"
)

// LessonRow — занятие с пулом сценариев и участниками (один запрос).
type LessonRow struct {
	ID             uuid.UUID
	Kind           string // class | practice
	Title          string
	TeacherID      *uuid.UUID // NULL только у practice
	CreatedBy      uuid.UUID
	GroupID        *uuid.UUID
	Mode           string // cards | card_actions
	ScenarioSource string
	Difficulty     *int
	TimeLimitSec   int
	Settings       model.LessonSettings // с добивкой дефолтами
	Status         string
	ScheduledAt    *time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ScenarioIDs    []uuid.UUID      // по lesson_scenarios.sort_order
	Participants   []ParticipantRow // по ФИО
}

// OwnerID — «преподаватель» занятия: teacher_id, у самостоятельной практики — автор.
func (l *LessonRow) OwnerID() uuid.UUID {
	if l.TeacherID != nil {
		return *l.TeacherID
	}
	return l.CreatedBy
}

// Participant — участник по user_id.
func (l *LessonRow) Participant(userID uuid.UUID) (*ParticipantRow, bool) {
	for i := range l.Participants {
		if l.Participants[i].UserID == userID {
			return &l.Participants[i], true
		}
	}
	return nil, false
}

// ParticipantRow — lesson_participants + ФИО + последняя попытка участника (по seq_no).
// JSON-теги — форма json_agg в запросе (snake_case), не API.
type ParticipantRow struct {
	UserID     uuid.UUID  `json:"user_id"`
	LastName   string     `json:"last_name"`
	FirstName  string     `json:"first_name"`
	MiddleName string     `json:"middle_name"`
	Status     string     `json:"status"`
	JoinedAt   *time.Time `json:"joined_at"`
	FinishedAt *time.Time `json:"finished_at"`
	LastSeenAt *time.Time `json:"last_seen_at"`
	MicReady   bool       `json:"mic_ready"`
	AttemptID  *uuid.UUID `json:"attempt_id"`
	Name       string     `json:"-"` // «Фамилия И. О.» (core.ShortName)
}

// LessonFilter — фильтры списка; пустые/nil — без фильтра.
type LessonFilter struct {
	TeacherID *uuid.UUID // занятия преподавателя (teacher_id)
	Status    string
	// ParticipantUserID — занятия, где пользователь — участник (представление обучающегося).
	// В Participants тогда ТОЛЬКО сам этот участник: одногруппники обучающемуся не отдаются
	// (lessons.onlyParticipant), и собирать json_agg всех участников каждого занятия его
	// истории — лишние сотни миллисекунд и мегабайт на дашборде. Порядок тогда — «что можно
	// делать сейчас» сверху (AssignedRank), курсор — AssignedAfter.
	ParticipantUserID *uuid.UUID
	// ExcludeCancelled — без отменённых занятий (дашборд обучающегося).
	ExcludeCancelled bool

	// Страница: Limit строк после курсора (нулевой курсор — первая страница; Limit <= 0 —
	// MaxListLimit). After — для списков преподавателя/админа, AssignedAfter — для
	// ParticipantUserID.
	Limit         int
	After         TimeKey
	AssignedAfter AssignedKey
}

// Key — курсор этой строки в списке преподавателя/админа.
func (r *LessonRow) Key() TimeKey { return TimeKey{At: r.CreatedAt, ID: r.ID} }

// AssignedKey — курсор этой строки в списке обучающегося.
func (r *LessonRow) AssignedKey() AssignedKey {
	return AssignedKey{Rank: AssignedRank(r.Status), TimeKey: r.Key()}
}

// Колонки занятия без участников (общие для всех чтений).
const lessonHead = `
SELECT l.id, l.kind, l.title, l.teacher_id, l.created_by, l.group_id, l.mode, l.scenario_source,
       l.difficulty, l.time_limit_sec, l.settings, l.status, l.scheduled_at, l.started_at, l.finished_at,
       l.created_at, l.updated_at,
       ARRAY(SELECT ls.scenario_id FROM lesson_scenarios ls
              WHERE ls.lesson_id = l.id ORDER BY ls.sort_order, ls.scenario_id),`

// Участник p (lesson_participants) + u (users); последняя попытка — по уникальному индексу
// (lesson_id, user_id, seq_no), backward scan, LIMIT 1.
const participantJSON = `json_build_object(
                 'user_id', p.user_id, 'last_name', u.last_name, 'first_name', u.first_name,
                 'middle_name', u.middle_name, 'status', p.status, 'joined_at', p.joined_at,
                 'finished_at', p.finished_at, 'last_seen_at', p.last_seen_at, 'mic_ready', p.mic_ready,
                 'attempt_id', (SELECT a.id FROM attempts a
                                 WHERE a.lesson_id = p.lesson_id AND a.user_id = p.user_id
                                 ORDER BY a.seq_no DESC LIMIT 1))`

// Все участники — json_agg одним подзапросом (их десятки).
const lessonColumns = lessonHead + `
       (SELECT json_agg(` + participantJSON + `
               ORDER BY u.last_name, u.first_name, u.middle_name, p.user_id)
          FROM lesson_participants p
          JOIN users u ON u.id = p.user_id
         WHERE p.lesson_id = l.id)
  FROM lessons l`

const lessonOrder = `
 ORDER BY l.created_at DESC, l.id DESC`

// sqlAssignedRank — AssignedRank в SQL (одна шкала с Go: курсор строится по статусу строки).
const sqlAssignedRank = `CASE l.status WHEN 'running' THEN 0 WHEN 'scheduled' THEN 1
                     WHEN 'draft' THEN 2 WHEN 'finished' THEN 3 ELSE 4 END`

const sqlGetLesson = lessonColumns + `
 WHERE l.id = $1`

// Списки — отдельный текст на каждый ведущий фильтр (как ListAttempts): у каждого свой
// индекс, и нет "$n IS NULL OR l.id IN (подзапрос)" — подзапрос под OR PostgreSQL не
// превращает в semi-join, получался seq scan всех lessons с hashed SubPlan (и при тысячах
// занятий — завышенная оценка стоимости, вплоть до JIT). "$n IS NULL OR <колонка> = $n"
// остаются только как остаточные фильтры строки, уже найденной по индексу.
const (
	// администратор: все занятия — страница по lessons_created_idx (created_at DESC, id DESC):
	// индексный диапазон «после курсора» + LIMIT, сколько бы занятий ни накопилось.
	sqlListLessons = lessonColumns + `
 WHERE ($1::text IS NULL OR l.status = $1)
   AND (l.created_at, l.id) < ($2::timestamptz, $3::uuid)` + lessonOrder + `
 LIMIT $4`

	// преподаватель: lessons_teacher_created_idx (teacher_id, created_at DESC, id DESC)
	sqlListLessonsByTeacher = lessonColumns + `
 WHERE l.teacher_id = $1
   AND ($2::text IS NULL OR l.status = $2)
   AND (l.created_at, l.id) < ($3::timestamptz, $4::uuid)` + lessonOrder + `
 LIMIT $5`

	// обучающийся: от его строк lesson_participants (lesson_participants_user_idx), участник —
	// только он сам (без json_agg по всем участникам каждого занятия).
	sqlListLessonsByParticipant = lessonHead + `
       json_build_array(` + participantJSON + `)
  FROM lesson_participants p
  JOIN lessons l ON l.id = p.lesson_id
  JOIN users u ON u.id = p.user_id
 WHERE p.user_id = $1
   AND ($2::uuid IS NULL OR l.teacher_id = $2)
   AND ($3::text IS NULL OR l.status = $3)
   AND NOT ($4::bool AND l.status = 'cancelled')
   AND (` + sqlAssignedRank + ` > $5
        OR (` + sqlAssignedRank + ` = $5 AND (l.created_at, l.id) < ($6::timestamptz, $7::uuid)))
 ORDER BY ` + sqlAssignedRank + `, l.created_at DESC, l.id DESC
 LIMIT $8`
)

// GetLesson — занятие по id (pgx.ErrNoRows, если нет).
func GetLesson(ctx context.Context, q pg.Querier, id uuid.UUID) (*LessonRow, error) {
	r := new(LessonRow)
	if err := scanLesson(q.QueryRow(ctx, sqlGetLesson, id), r); err != nil {
		return nil, err
	}
	return r, nil
}

// ListLessons — страница списка: новые сверху (у обучающегося — сначала идущие). С
// ParticipantUserID в Participants каждого занятия — только этот участник (см. LessonFilter).
func ListLessons(ctx context.Context, q pg.Querier, f LessonFilter) ([]LessonRow, error) {
	var (
		rows pgx.Rows
		err  error
	)
	limit := limitOr(f.Limit)
	switch {
	case f.ParticipantUserID != nil:
		rank, at, id := f.AssignedAfter.args()
		rows, err = q.Query(ctx, sqlListLessonsByParticipant, *f.ParticipantUserID, f.TeacherID, nullStr(f.Status),
			f.ExcludeCancelled, rank, at, id, limit)
	case f.TeacherID != nil:
		at, id := f.After.args()
		rows, err = q.Query(ctx, sqlListLessonsByTeacher, *f.TeacherID, nullStr(f.Status), at, id, limit)
	default:
		at, id := f.After.args()
		rows, err = q.Query(ctx, sqlListLessons, nullStr(f.Status), at, id, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("store: list lessons: %w", err)
	}
	defer rows.Close()
	out := make([]LessonRow, 0, 16)
	for rows.Next() {
		out = append(out, LessonRow{})
		if err := scanLesson(rows, &out[len(out)-1]); err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list lessons: %w", err)
	}
	return out, nil
}

func scanLesson(row pgx.Row, r *LessonRow) error {
	err := row.Scan(
		&r.ID, &r.Kind, &r.Title, &r.TeacherID, &r.CreatedBy, &r.GroupID, &r.Mode, &r.ScenarioSource,
		&r.Difficulty, &r.TimeLimitSec, &settingsScan{dst: &r.Settings}, &r.Status,
		&r.ScheduledAt, &r.StartedAt, &r.FinishedAt, &r.CreatedAt, &r.UpdatedAt,
		&r.ScenarioIDs, &participantsScan{dst: &r.Participants},
	)
	if err != nil {
		return err
	}
	if r.ScenarioIDs == nil {
		r.ScenarioIDs = []uuid.UUID{}
	}
	if r.Participants == nil {
		r.Participants = []ParticipantRow{}
	}
	return nil
}

// participantsScan — json_agg участников (NULL — участников нет).
type participantsScan struct{ dst *[]ParticipantRow }

func (s *participantsScan) ScanBytes(b []byte) error {
	if b == nil {
		*s.dst = nil
		return nil
	}
	var ps []ParticipantRow
	if err := json.Unmarshal(b, &ps); err != nil {
		return fmt.Errorf("store: lesson participants: %w", err)
	}
	for i := range ps {
		p := &ps[i]
		p.Name = core.ShortName(p.LastName, p.FirstName, p.MiddleName)
		p.JoinedAt = convert.UTC(p.JoinedAt)
		p.FinishedAt = convert.UTC(p.FinishedAt)
		p.LastSeenAt = convert.UTC(p.LastSeenAt)
	}
	*s.dst = ps
	return nil
}

// ---------------------------------------------------------------- public

// LessonToPublic — занятие для API: teacherId у практики — автор; массивы не nil.
func LessonToPublic(r *LessonRow) public.Lesson {
	out := public.Lesson{
		Id:           r.ID,
		Kind:         public.LessonKind(r.Kind),
		Title:        r.Title,
		TeacherId:    r.OwnerID(),
		Mode:         public.LessonMode(r.Mode),
		Perspective:  convert.Perspective(r.Settings),
		TimeLimitSec: r.TimeLimitSec,
		Status:       public.LessonStatus(r.Status),
		ScenarioIds:  convert.Slice(r.ScenarioIDs),
		Participants: make([]public.LessonParticipant, len(r.Participants)),
		Settings:     convert.LessonSettingsToPublic(r.Settings),
		CreatedAt:    r.CreatedAt.UTC(),
		StartedAt:    convert.UTC(r.StartedAt),
		FinishedAt:   convert.UTC(r.FinishedAt),
	}
	if r.Difficulty != nil {
		d := public.Difficulty(*r.Difficulty)
		out.Difficulty = &d
	}
	for i := range r.Participants {
		out.Participants[i] = ParticipantToPublic(&r.Participants[i])
	}
	return out
}

// LessonsToPublic — список (всегда не nil).
func LessonsToPublic(rows []LessonRow) []public.Lesson {
	out := make([]public.Lesson, len(rows))
	for i := range rows {
		out[i] = LessonToPublic(&rows[i])
	}
	return out
}

// ParticipantToPublic — участник (для списка и WS participantStatus).
func ParticipantToPublic(p *ParticipantRow) public.LessonParticipant {
	mic := p.MicReady
	name := p.Name
	if name == "" {
		name = core.ShortName(p.LastName, p.FirstName, p.MiddleName)
	}
	return public.LessonParticipant{
		UserId:     p.UserID,
		Name:       name,
		Status:     public.ParticipantStatus(p.Status),
		AttemptId:  p.AttemptID,
		JoinedAt:   convert.UTC(p.JoinedAt),
		FinishedAt: convert.UTC(p.FinishedAt),
		MicReady:   &mic,
	}
}
