package users

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/settings"
)

// SessionRevoker — отзыв сессий (реализация: auth.Service; users не импортирует auth).
type SessionRevoker interface {
	// RevokeUserSessions — отозвать все сессии пользователя в транзакции q (кэш чистится после COMMIT).
	RevokeUserSessions(ctx context.Context, q pg.Querier, userID uuid.UUID) error
	// PurgeUser — выбросить закэшированные Principal пользователя (роль/ФИО изменились).
	PurgeUser(userID uuid.UUID)
}

// Deps — зависимости ручек (связывает internal/app).
type Deps struct {
	Pool     *pgxpool.Pool
	Settings *settings.Store
	Auditor  core.Auditor
	Catalog  core.Catalog // подписи полей карточки, проверка службы; может быть nil
	Sessions SessionRevoker
	// HashPassword — argon2id под семафором (= auth.HashPasswordCtx): хэширование живёт в auth,
	// а users не импортирует auth.
	HashPassword func(ctx context.Context, pw string) (string, error)
	Log          *slog.Logger
}

// Handlers — ручки /users*.
type Handlers struct {
	pool     *pgxpool.Pool
	settings *settings.Store
	audit    core.Auditor
	catalog  core.Catalog
	sessions SessionRevoker
	hash     func(ctx context.Context, pw string) (string, error)
	log      *slog.Logger
}

func NewHandlers(d Deps) *Handlers {
	h := &Handlers{pool: d.Pool, settings: d.Settings, audit: d.Auditor, catalog: d.Catalog,
		sessions: d.Sessions, hash: d.HashPassword, log: d.Log}
	if h.log == nil {
		h.log = slog.Default()
	}
	if h.audit == nil {
		h.audit = nopAuditor{}
	}
	return h
}

type nopAuditor struct{}

func (nopAuditor) Log(context.Context, core.AuditEntry) {}

// Register — маршруты /users* (относительно /api/v1).
func (h *Handlers) Register(r *httpx.Router) {
	r.Handle("GET /users", httpx.Roles(core.RoleAdmin, core.RoleTeacher), h.list)
	r.Handle("POST /users", httpx.Roles(core.RoleAdmin), h.create)
	r.Handle("PATCH /users/{userId}", httpx.Roles(core.RoleAdmin), h.update)
	r.Handle("PUT /users/{userId}/blocked", httpx.Roles(core.RoleAdmin), h.setBlocked)
	r.Handle("GET /users/{userId}/progress", httpx.Authenticated, h.progress)
}

func principal(r *http.Request) (*core.Principal, error) {
	p := core.PrincipalFrom(r.Context())
	if p == nil {
		return nil, httpx.Unauthorized()
	}
	return p, nil
}

// ---------------------------------------------------------------- GET /users

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	qs := r.URL.Query()
	f := ListFilter{Role: qs.Get("role"), Q: qs.Get("q")}
	if f.Role != "" && !core.Role(f.Role).Valid() {
		return httpx.Validation("Некорректный фильтр", map[string]string{"role": "Допустимо: admin, teacher, student"})
	}
	if r := []rune(f.Q); len(r) > 100 {
		f.Q = string(r[:100]) // по рунам: обрезка посреди UTF-8 дала бы ошибку кодировки в PG
	}
	if p.Role == core.RoleTeacher {
		f.TeacherID = p.UserID
	}
	page, err := httpx.ParsePage(r, usersPageDefault, usersPageMax)
	if err != nil {
		return err
	}
	if page.Cursor != nil {
		k, ok := ParseNameKey(page.Cursor)
		if !ok {
			return httpx.BadCursor()
		}
		f.After = k
	}
	f.Limit = page.Limit + 1 // +1 строка — есть ли следующая страница, без count(*)
	list, err := List(r.Context(), h.pool, f)
	if err != nil {
		return httpx.Internal(err)
	}
	if len(list) > page.Limit {
		list = list[:page.Limit]
		httpx.SetNextCursor(w, KeyOf(&list[len(list)-1]).Parts()...)
	}
	httpx.WriteJSON(w, http.StatusOK, list)
	return nil
}

// Размеры страниц GET /users (контракт v1.2: ?limit). Строка лёгкая, а форма занятия
// выбирает участников из этого списка — страница по умолчанию крупная.
const (
	usersPageDefault = 500
	usersPageMax     = 1000
)

// ---------------------------------------------------------------- POST /users

const insertUserSQL = `
INSERT INTO users (id, login, password_hash, role, last_name, first_name, middle_name, service_id,
                   must_change_password, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true, $9)`

const addMembersSQL = `
INSERT INTO group_members (group_id, user_id) SELECT unnest($1::uuid[]), $2 ON CONFLICT DO NOTHING`

const countGroupsSQL = `SELECT count(*) FROM groups WHERE id = ANY($1::uuid[]) AND archived_at IS NULL`

// userAudit — компактный снимок для audit_log (без пароля/хэша).
type userAudit struct {
	Login           string      `json:"login,omitempty"`
	Role            string      `json:"role,omitempty"`
	LastName        string      `json:"lastName,omitempty"`
	FirstName       string      `json:"firstName,omitempty"`
	MiddleName      string      `json:"middleName,omitempty"`
	ServiceID       string      `json:"serviceId,omitempty"`
	Status          string      `json:"status,omitempty"`
	GroupIDs        []uuid.UUID `json:"groupIds"`
	PasswordChanged bool        `json:"passwordChanged,omitempty"`
}

func snapshot(u *public.User) userAudit {
	a := userAudit{Login: u.Login, Role: string(u.Role), LastName: u.LastName, FirstName: u.FirstName,
		Status: string(u.Status), GroupIDs: []uuid.UUID{}}
	if u.MiddleName != nil {
		a.MiddleName = *u.MiddleName
	}
	if u.ServiceId != nil {
		a.ServiceID = *u.ServiceId
	}
	if u.GroupIds != nil {
		a.GroupIDs = *u.GroupIds
	}
	return a
}

func (h *Handlers) create(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	var in createInput
	if err := httpx.ReadJSON(r, &in); err != nil {
		return err
	}
	fields := map[string]string{}
	login := strings.TrimSpace(in.Login)
	set := func(k, msg string) {
		if msg != "" {
			fields[k] = msg
		}
	}
	set("login", checkLogin(login))
	set("password", checkPassword(in.Password))
	set("role", checkRole(in.Role))
	last, first, middle := strings.TrimSpace(in.LastName), strings.TrimSpace(in.FirstName), trimPtr(in.MiddleName)
	set("lastName", checkName(last, "фамилию"))
	set("firstName", checkName(first, "имя"))
	set("middleName", checkMiddle(middle))
	svc, msg := h.parseService(trimPtr(in.ServiceID))
	set("serviceId", msg)
	groups, msg := parseGroupIDs(in.GroupIDs)
	set("groupIds", msg)
	if len(fields) > 0 {
		return httpx.Validation(msgValidation, fields)
	}

	// argon2 — до транзакции: ~30 мс CPU не должны держать соединение пула.
	hash, err := h.hashPassword(ctx, in.Password)
	if err != nil {
		return err
	}

	id := ids.New()
	var u *public.User
	err = pg.WithTx(ctx, h.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := checkGroupsExist(ctx, tx, groups); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, insertUserSQL, id, login, hash, in.Role, last, first, nilIfEmpty(middle), svc, p.UserID); err != nil {
			return mapWriteErr(err)
		}
		if len(groups) > 0 {
			if _, err := tx.Exec(ctx, addMembersSQL, groups, id); err != nil {
				return fmt.Errorf("users: add members: %w", err)
			}
		}
		var err error
		u, err = Get(ctx, tx, id)
		return err
	})
	if err != nil {
		return asHTTP(err)
	}
	h.audit.Log(ctx, core.AuditEntry{Action: "user.create", EntityType: "user", EntityID: id, After: snapshot(u)})
	httpx.WriteJSON(w, http.StatusCreated, u)
	return nil
}

// ---------------------------------------------------------------- PATCH /users/{userId}

// Частичное обновление одним UPDATE: $n::boolean — «поле передано».
const updateUserSQL = `
UPDATE users SET
  role                 = COALESCE($2::text, role),
  last_name            = COALESCE($3::text, last_name),
  first_name           = COALESCE($4::text, first_name),
  middle_name          = CASE WHEN $5::boolean THEN $6::text ELSE middle_name END,
  service_id           = CASE WHEN $7::boolean THEN $8::uuid ELSE service_id END,
  password_hash        = COALESCE($9::text, password_hash),
  password_changed_at  = CASE WHEN $9::text IS NULL THEN password_changed_at ELSE now() END,
  must_change_password = CASE WHEN $9::text IS NULL THEN must_change_password ELSE $10::boolean END,
  failed_login_count   = CASE WHEN $9::text IS NULL THEN failed_login_count ELSE 0 END,
  locked_until         = CASE WHEN $9::text IS NULL THEN locked_until ELSE NULL END
WHERE id = $1`

// Замена набора групп: удаляем членство только в неархивных группах (архив — история).
const removeMembersSQL = `
DELETE FROM group_members gm
 USING groups g
 WHERE gm.user_id = $1 AND g.id = gm.group_id AND g.archived_at IS NULL
   AND NOT (gm.group_id = ANY($2::uuid[]))`

const lockUserSQL = `SELECT role, status FROM users WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`

func (h *Handlers) update(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	id, err := httpx.PathUUID(r, "userId")
	if err != nil {
		return httpx.NotFound("Пользователь не найден")
	}
	var in patchInput
	if err := httpx.ReadJSON(r, &in); err != nil {
		return err
	}
	if in.empty() {
		u, err := Get(ctx, h.pool, id)
		if err != nil {
			if pg.IsNoRows(err) {
				return httpx.NotFound("Пользователь не найден")
			}
			return httpx.Internal(err)
		}
		httpx.WriteJSON(w, http.StatusOK, u)
		return nil
	}

	fields := map[string]string{}
	set := func(k, msg string) {
		if msg != "" {
			fields[k] = msg
		}
	}
	var role, last, first *string
	if in.Role != nil {
		set("role", checkRole(*in.Role))
		role = in.Role
	}
	if in.LastName != nil {
		v := strings.TrimSpace(*in.LastName)
		set("lastName", checkName(v, "фамилию"))
		last = &v
	}
	if in.FirstName != nil {
		v := strings.TrimSpace(*in.FirstName)
		set("firstName", checkName(v, "имя"))
		first = &v
	}
	middleSet := in.MiddleName != nil
	middle := trimPtr(in.MiddleName)
	set("middleName", checkMiddle(middle))
	var svc *uuid.UUID
	if in.ServiceID.Set && !in.ServiceID.Null {
		var msg string
		svc, msg = h.parseService(strings.TrimSpace(in.ServiceID.Value))
		set("serviceId", msg)
	}
	if in.Password != nil {
		set("password", checkPassword(*in.Password))
	}
	var groups []uuid.UUID
	if in.GroupIDs != nil {
		var msg string
		groups, msg = parseGroupIDs(*in.GroupIDs)
		set("groupIds", msg)
	}
	if len(fields) > 0 {
		return httpx.Validation(msgValidation, fields)
	}

	var hash *string
	if in.Password != nil {
		hv, err := h.hashPassword(ctx, *in.Password)
		if err != nil {
			return err
		}
		hash = &hv
	}

	var before, after *public.User
	err = pg.WithTx(ctx, h.pool, func(ctx context.Context, tx pgx.Tx) error {
		var curRole, curStatus string
		if err := tx.QueryRow(ctx, lockUserSQL, id).Scan(&curRole, &curStatus); err != nil {
			if pg.IsNoRows(err) {
				return httpx.NotFound("Пользователь не найден")
			}
			return fmt.Errorf("users: lock: %w", err)
		}
		if role != nil && *role != curRole && id == p.UserID {
			return httpx.Conflict("Нельзя изменить роль текущей учётной записи")
		}
		var err error
		if before, err = Get(ctx, tx, id); err != nil {
			return fmt.Errorf("users: before: %w", err)
		}
		if in.GroupIDs != nil {
			if err := checkGroupsExist(ctx, tx, groups); err != nil {
				return err
			}
		}
		// Сменили пароль себе — временным он не считается; чужой — «сменить при входе».
		mustChange := id != p.UserID
		if _, err := tx.Exec(ctx, updateUserSQL, id, role, last, first, middleSet, nilIfEmpty(middle),
			in.ServiceID.Set, svc, hash, mustChange); err != nil {
			return mapWriteErr(err)
		}
		if in.GroupIDs != nil {
			if _, err := tx.Exec(ctx, removeMembersSQL, id, groups); err != nil {
				return fmt.Errorf("users: remove members: %w", err)
			}
			if len(groups) > 0 {
				if _, err := tx.Exec(ctx, addMembersSQL, groups, id); err != nil {
					return fmt.Errorf("users: add members: %w", err)
				}
			}
		}
		if hash != nil && h.sessions != nil {
			// Новый пароль — все входы по старому закрываются (включая текущий, если меняли себе).
			if err := h.sessions.RevokeUserSessions(ctx, tx, id); err != nil {
				return err
			}
		}
		if after, err = Get(ctx, tx, id); err != nil {
			return fmt.Errorf("users: after: %w", err)
		}
		h.purgeOnCommit(ctx, id)
		return nil
	})
	if err != nil {
		return asHTTP(err)
	}
	b, a := snapshot(before), snapshot(after)
	a.PasswordChanged = hash != nil
	h.audit.Log(ctx, core.AuditEntry{Action: "user.update", EntityType: "user", EntityID: id, Before: b, After: a})
	httpx.WriteJSON(w, http.StatusOK, after)
	return nil
}

// ---------------------------------------------------------------- PUT /users/{userId}/blocked

const blockUserSQL = `UPDATE users SET status = 'blocked' WHERE id = $1`

const unblockUserSQL = `
UPDATE users SET status = 'active', blocked_reason = NULL, failed_login_count = 0, locked_until = NULL
 WHERE id = $1`

type statusAudit struct {
	Status string `json:"status"`
}

func (h *Handlers) setBlocked(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	id, err := httpx.PathUUID(r, "userId")
	if err != nil {
		return httpx.NotFound("Пользователь не найден")
	}
	var in struct {
		Blocked *bool `json:"blocked"`
	}
	if err := httpx.ReadJSON(r, &in); err != nil {
		return err
	}
	if in.Blocked == nil {
		return httpx.Validation(msgValidation, map[string]string{"blocked": "Обязательное поле"})
	}
	blocked := *in.Blocked
	if blocked && id == p.UserID {
		return httpx.Conflict("Нельзя заблокировать текущую учётную запись")
	}

	var (
		u       *public.User
		wasStat string
	)
	err = pg.WithTx(ctx, h.pool, func(ctx context.Context, tx pgx.Tx) error {
		var curRole string
		if err := tx.QueryRow(ctx, lockUserSQL, id).Scan(&curRole, &wasStat); err != nil {
			if pg.IsNoRows(err) {
				return httpx.NotFound("Пользователь не найден")
			}
			return fmt.Errorf("users: lock: %w", err)
		}
		if blocked {
			if _, err := tx.Exec(ctx, blockUserSQL, id); err != nil {
				return fmt.Errorf("users: block: %w", err)
			}
			// Блокировка убивает вход немедленно (db-design §6): сессии — в отзыв.
			if h.sessions != nil {
				if err := h.sessions.RevokeUserSessions(ctx, tx, id); err != nil {
					return err
				}
			}
		} else if _, err := tx.Exec(ctx, unblockUserSQL, id); err != nil {
			return fmt.Errorf("users: unblock: %w", err)
		}
		var err error
		if u, err = Get(ctx, tx, id); err != nil {
			return fmt.Errorf("users: get: %w", err)
		}
		h.purgeOnCommit(ctx, id)
		return nil
	})
	if err != nil {
		return asHTTP(err)
	}
	if string(u.Status) != wasStat {
		action := "user.unblock"
		if blocked {
			action = "user.block"
		}
		h.audit.Log(ctx, core.AuditEntry{Action: action, EntityType: "user", EntityID: id,
			Before: statusAudit{Status: wasStat}, After: statusAudit{Status: string(u.Status)}})
	}
	httpx.WriteJSON(w, http.StatusOK, u)
	return nil
}

// ---------------------------------------------------------------- helpers

func (h *Handlers) hashPassword(ctx context.Context, pw string) (string, error) {
	if h.hash == nil {
		return "", httpx.Internal(errors.New("users: HashPassword not configured"))
	}
	hv, err := h.hash(ctx, pw)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", httpx.Internal(fmt.Errorf("users: hash password: %w", err))
	}
	return hv, nil
}

// purgeOnCommit — Principal в кэше сессий перечитается (роль/ФИО/статус) после COMMIT:
// чистка до COMMIT дала бы окно, в котором кэш заново наполнится старыми данными.
func (h *Handlers) purgeOnCommit(ctx context.Context, id uuid.UUID) {
	if h.sessions == nil {
		return
	}
	pg.OnCommit(ctx, func() { h.sessions.PurgeUser(id) })
}

// parseService — id службы профиля ("" — без службы). Проверка по справочнику в памяти;
// без Catalog подстрахует FK (mapWriteErr).
func (h *Handlers) parseService(s string) (*uuid.UUID, string) {
	if s == "" {
		return nil, ""
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return nil, "Служба не найдена"
	}
	if h.catalog != nil {
		if _, ok := h.catalog.ServiceByID(id.String()); !ok {
			return nil, "Служба не найдена"
		}
	}
	return &id, ""
}

func checkGroupsExist(ctx context.Context, q pg.Querier, groups []uuid.UUID) error {
	if len(groups) == 0 {
		return nil
	}
	var n int
	if err := q.QueryRow(ctx, countGroupsSQL, groups).Scan(&n); err != nil {
		return fmt.Errorf("users: check groups: %w", err)
	}
	if n != len(groups) {
		return httpx.Validation(msgValidation, map[string]string{"groupIds": "Группа не найдена"})
	}
	return nil
}

// mapWriteErr — нарушения ограничений users в ошибки контракта.
func mapWriteErr(err error) error {
	switch {
	case pg.IsUniqueViolation(err) && pg.ConstraintName(err) == "users_login_active_idx":
		return httpx.Conflict("Логин занят")
	case pg.IsForeignKeyViolation(err) && pg.ConstraintName(err) == "users_service_id_fkey":
		return httpx.Validation(msgValidation, map[string]string{"serviceId": "Служба не найдена"})
	case pg.IsCheckViolation(err):
		return httpx.Validation(msgValidation, nil).Wrap(err)
	}
	return fmt.Errorf("users: write: %w", err)
}

// asHTTP — ошибки транзакции: *httpx.Error как есть, прочее — 500.
func asHTTP(err error) error {
	var he *httpx.Error
	if errors.As(err, &he) {
		return he
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	return httpx.Internal(err)
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
