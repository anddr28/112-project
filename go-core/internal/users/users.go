// Package users — учётные записи: /users*, /users/{id}/progress и единственная сборка
// public.User (Get / GetMany / List) — её же использует auth (через Deps.LoadUser) и app.
//
// Все выборки — одним запросом: службы — LEFT JOIN, группы — ARRAY(подзапрос) по индексу
// group_members_user_idx; N+1 нет.
package users

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/pg"
)

// userCols — колонки public.User; порядок = scanUser. Группы — только неархивные.
const userCols = `
SELECT u.id, u.login::text, u.role, u.last_name, u.first_name, u.middle_name, u.service_id,
       COALESCE(NULLIF(s.short_name, ''), s.name), u.status, u.workstation, u.operator_no,
       ARRAY(SELECT gm.group_id FROM group_members gm JOIN groups g ON g.id = gm.group_id
              WHERE gm.user_id = u.id AND g.archived_at IS NULL ORDER BY gm.joined_at, gm.group_id)
  FROM users u
  LEFT JOIN services s ON s.id = u.service_id`

const getSQL = userCols + `
 WHERE u.id = $1 AND u.deleted_at IS NULL`

const getManySQL = userCols + `
 WHERE u.id = ANY($1::uuid[]) AND u.deleted_at IS NULL
 ORDER BY u.last_name, u.first_name, u.id`

// listSQL — постоянный текст (кэш prepared statements): необязательные фильтры — через
// «$n IS NULL OR …». Таблица пользователей — сотни строк, generic-план здесь не проблема.
//
// $3 — преподаватель: видит обучающихся своих групп, тех, кто уже был на его занятиях, и
// активных обучающихся без групп (только что созданных: управления группами в контракте нет,
// а назначить на занятие можно только того, кого видно в списке). Заблокированных «ничьих»
// не показываем: назначить их нельзя, а ПДн лишний раз не раскрываем.
const listSQL = userCols + `
 WHERE u.deleted_at IS NULL
   AND ($1::text IS NULL OR u.role = $1)
   AND ($2::text IS NULL OR u.login::text ILIKE $2
        OR concat_ws(' ', u.last_name, u.first_name, u.middle_name) ILIKE $2)
   AND ($3::uuid IS NULL OR (u.role = 'student' AND (
            (u.status = 'active' AND NOT EXISTS (SELECT 1 FROM group_members gm JOIN groups g ON g.id = gm.group_id
                         WHERE gm.user_id = u.id AND g.archived_at IS NULL))
         OR EXISTS (SELECT 1 FROM group_members gm JOIN groups g ON g.id = gm.group_id
                     WHERE gm.user_id = u.id AND g.teacher_id = $3 AND g.archived_at IS NULL)
         OR EXISTS (SELECT 1 FROM lesson_participants lp JOIN lessons l ON l.id = lp.lesson_id
                     WHERE lp.user_id = u.id AND l.teacher_id = $3))))
   AND (u.last_name, u.first_name, u.id) > ($4::text, $5::text, $6::uuid)
 ORDER BY u.last_name, u.first_name, u.id
 LIMIT $7`

// scanUser — ручной скан строки userCols (без рефлексии).
func scanUser(row pgx.Row) (public.User, error) {
	var (
		u                         public.User
		role, status              string
		middle, svcName, ws, opNo *string
		svcID                     *uuid.UUID
		groups                    []uuid.UUID
	)
	if err := row.Scan(&u.Id, &u.Login, &role, &u.LastName, &u.FirstName, &middle, &svcID,
		&svcName, &status, &ws, &opNo, &groups); err != nil {
		return u, err
	}
	u.Role = public.Role(role)
	u.Status = public.UserStatus(status)
	u.MiddleName = nonEmpty(middle)
	if svcID != nil {
		s := svcID.String()
		u.ServiceId = &s
		u.ServiceName = nonEmpty(svcName)
	}
	u.Workstation = nonEmpty(ws)
	u.OperatorNo = nonEmpty(opNo)
	if groups == nil {
		groups = []uuid.UUID{}
	}
	u.GroupIds = &groups
	return u, nil
}

// Get — пользователь (не удалённый). pgx.ErrNoRows пробрасывается как есть (pg.IsNoRows).
func Get(ctx context.Context, q pg.Querier, id uuid.UUID) (*public.User, error) {
	u, err := scanUser(q.QueryRow(ctx, getSQL, id))
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetMany — пользователи по id одним запросом (неизвестные/удалённые пропускаются),
// порядок — по ФИО. Результат не nil.
func GetMany(ctx context.Context, q pg.Querier, ids []uuid.UUID) ([]public.User, error) {
	out := make([]public.User, 0, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	return collect(ctx, q, out, getManySQL, ids)
}

// ListFilter — фильтры GET /users. Пустые поля — без фильтра.
type ListFilter struct {
	Role string // admin | teacher | student
	Q    string // подстрока логина или ФИО
	// TeacherID — ограничить видимость обучающимися преподавателя (см. listSQL).
	TeacherID uuid.UUID
	// Страница (keyset по users_name_idx): Limit строк после курсора After (нулевой —
	// первая страница; Limit <= 0 — MaxListLimit).
	Limit int
	After NameKey
}

// MaxListLimit — потолок строк, если лимит не задан.
const MaxListLimit = 1000

// NameKey — курсор списка пользователей: (фамилия, имя, id) по возрастанию. Нулевой —
// первая страница: ('', '', 00000000-…) меньше любой строки.
type NameKey struct {
	LastName, FirstName string
	ID                  uuid.UUID
}

// KeyOf — курсор строки списка.
func KeyOf(u *public.User) NameKey {
	return NameKey{LastName: u.LastName, FirstName: u.FirstName, ID: u.Id}
}

// Parts — значения для httpx.EncodeCursor.
func (k NameKey) Parts() []string { return []string{k.LastName, k.FirstName, k.ID.String()} }

// ParseNameKey — курсор из частей (false — не наш формат).
func ParseNameKey(parts []string) (NameKey, bool) {
	if len(parts) != 3 {
		return NameKey{}, false
	}
	id, err := uuid.Parse(parts[2])
	if err != nil {
		return NameKey{}, false
	}
	return NameKey{LastName: parts[0], FirstName: parts[1], ID: id}, true
}

// List — страница пользователей по фильтру, по ФИО. Результат не nil.
func List(ctx context.Context, q pg.Querier, f ListFilter) ([]public.User, error) {
	var role, pattern *string
	if f.Role != "" {
		role = &f.Role
	}
	// NUL и битый UTF-8 PostgreSQL в тексте не принимает (запрос упал бы 500): из поиска выбрасываем.
	if s := strings.TrimSpace(strings.ToValidUTF8(strings.ReplaceAll(f.Q, "\x00", ""), "")); s != "" {
		p := "%" + escapeLike(s) + "%"
		pattern = &p
	}
	var teacher *uuid.UUID
	if f.TeacherID != uuid.Nil {
		teacher = &f.TeacherID
	}
	limit := f.Limit
	if limit <= 0 || limit > MaxListLimit {
		limit = MaxListLimit
	}
	return collect(ctx, q, make([]public.User, 0, 32), listSQL, role, pattern, teacher,
		f.After.LastName, f.After.FirstName, f.After.ID, limit)
}

func collect(ctx context.Context, q pg.Querier, out []public.User, sql string, args ...any) ([]public.User, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("users: query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("users: scan: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("users: rows: %w", err)
	}
	return out, nil
}

// escapeLike экранирует метасимволы LIKE (\ — escape по умолчанию в PostgreSQL).
func escapeLike(s string) string {
	if !strings.ContainsAny(s, `\%_`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for _, r := range s {
		if r == '\\' || r == '%' || r == '_' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func nonEmpty(s *string) *string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	return s
}
