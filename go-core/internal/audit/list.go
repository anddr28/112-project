package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/pg"
)

const (
	DefaultLimit = 100
	MaxLimit     = 500
	// SystemActorName — подпись действий планировщика/фоновых задач в админке.
	SystemActorName = "Система"
)

// Filter — параметры GET /admin/audit (frontend.v1.yaml listAudit).
type Filter struct {
	ActorID    *uuid.UUID
	Action     string // точное имя или префикс, если оканчивается точкой ("user.")
	EntityType string
	EntityID   *uuid.UUID
	From, To   *time.Time // включительно
	BeforeID   *int64     // курсор: записи с id < BeforeID
	Limit      int        // 0 — DefaultLimit; больше MaxLimit — MaxLimit
}

// List — записи аудита, новые сверху (at desc, id desc). Никогда не возвращает nil.
//
// SQL собирается из фиксированных фрагментов по набору заданных фильтров: текст запроса
// для одной комбинации постоянен (кэш prepared statements pgx), а план — конкретный
// (без «$1 IS NULL OR …», ломающего индексы и прюнинг партиций).
func List(ctx context.Context, q pg.Querier, f Filter) ([]public.AuditEntry, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	var sb strings.Builder
	sb.Grow(640)
	sb.WriteString(`SELECT a.id, a.at, a.actor_id, a.actor_role, a.action, a.entity_type, a.entity_id, a.lesson_id,
	       a.before, a.after, host(a.ip), u.last_name, u.first_name, u.middle_name
	  FROM audit_log a
	  LEFT JOIN users u ON u.id = a.actor_id
	 WHERE true`)
	args := make([]any, 0, 8)
	add := func(cond string, v any) {
		args = append(args, v)
		sb.WriteString(" AND ")
		sb.WriteString(cond)
		sb.WriteString(strconv.Itoa(len(args)))
	}
	if f.ActorID != nil {
		add("a.actor_id = $", *f.ActorID)
	}
	if a := strings.TrimSpace(f.Action); a != "" {
		if strings.HasSuffix(a, ".") {
			// starts_with — без экранирования % и _ (в именах действий есть "_": user.login_failed)
			args = append(args, a)
			sb.WriteString(" AND starts_with(a.action, $" + strconv.Itoa(len(args)) + ")")
		} else {
			add("a.action = $", a)
		}
	}
	if et := strings.TrimSpace(f.EntityType); et != "" {
		add("a.entity_type = $", et)
	}
	if f.EntityID != nil {
		add("a.entity_id = $", *f.EntityID)
	}
	if f.From != nil {
		add("a.at >= $", f.From.UTC())
	}
	if f.To != nil {
		add("a.at <= $", f.To.UTC())
	}
	if f.BeforeID != nil {
		add("a.id < $", *f.BeforeID)
	}
	args = append(args, limit)
	sb.WriteString(" ORDER BY a.at DESC, a.id DESC LIMIT $" + strconv.Itoa(len(args)))

	rows, err := q.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("audit: list: %w", err)
	}
	defer rows.Close()
	out := make([]public.AuditEntry, 0, min(limit, 128))
	for rows.Next() {
		var (
			id                          int64
			e                           public.AuditEntry
			actorID, entityID, lessonID *uuid.UUID
			before, after               []byte
			last, first, middle         *string
		)
		if err := rows.Scan(&id, &e.At, &actorID, &e.ActorRole, &e.Action, &e.EntityType, &entityID, &lessonID,
			&before, &after, &e.Ip, &last, &first, &middle); err != nil {
			return nil, fmt.Errorf("audit: list scan: %w", err)
		}
		e.Id = int(id)
		e.At = e.At.UTC()
		e.ActorId, e.EntityId, e.LessonId = actorID, entityID, lessonID
		switch {
		case last != nil:
			name := core.ShortName(*last, deref(first), deref(middle))
			e.ActorName = &name
		case actorID == nil && e.ActorRole != nil && *e.ActorRole == RoleSystem:
			name := SystemActorName
			e.ActorName = &name
		}
		e.Before = decodeObject(before)
		e.After = decodeObject(after)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: list: %w", err)
	}
	return out, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// decodeObject — jsonb before/after в объект контракта. Не-объекты (старые/ручные записи)
// заворачиваются в {"value": …}, битые — пропускаются (nil = поле отсутствует).
func decodeObject(b []byte) *map[string]any {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '{' {
		m := map[string]any{}
		if err := json.Unmarshal(b, &m); err == nil {
			return &m
		}
		return nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil
	}
	m := map[string]any{"value": v}
	return &m
}
