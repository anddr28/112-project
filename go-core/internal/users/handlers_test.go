package users

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/platform/pgtest"
)

// ---------------------------------------------------------------- фейки

// stubAuth — субъект из заголовков X-Test-User / X-Test-Role (без общего состояния: параллельно).
type stubAuth struct{}

func (stubAuth) Authenticate(r *http.Request) (*core.Principal, error) {
	id, err := uuid.Parse(r.Header.Get("X-Test-User"))
	if err != nil {
		return nil, core.ErrUnauthenticated
	}
	return &core.Principal{UserID: id, Role: core.Role(r.Header.Get("X-Test-Role")), SessionID: uuid.New()}, nil
}

type auditRec struct {
	mu      sync.Mutex
	entries []core.AuditEntry
}

func (a *auditRec) Log(_ context.Context, e core.AuditEntry) {
	a.mu.Lock()
	a.entries = append(a.entries, e)
	a.mu.Unlock()
}

func (a *auditRec) byAction(action string) []core.AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []core.AuditEntry
	for _, e := range a.entries {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// revokerRec — SessionRevoker: запоминает вызовы и был ли отзыв внутри транзакции.
type revokerRec struct {
	mu       sync.Mutex
	revoked  []uuid.UUID
	inTx     []bool
	purged   []uuid.UUID
	failWith error
}

func (r *revokerRec) RevokeUserSessions(ctx context.Context, _ pg.Querier, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failWith != nil {
		return r.failWith
	}
	r.revoked = append(r.revoked, id)
	r.inTx = append(r.inTx, pg.InTx(ctx))
	return nil
}

func (r *revokerRec) PurgeUser(id uuid.UUID) {
	r.mu.Lock()
	r.purged = append(r.purged, id)
	r.mu.Unlock()
}

func (r *revokerRec) snapshot() (revoked, purged []uuid.UUID, inTx []bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.revoked), slices.Clone(r.purged), slices.Clone(r.inTx)
}

type fakeCatalog struct{ services map[string]bool }

func (fakeCatalog) TypeByID(string) (core.IncidentTypeInfo, bool) {
	return core.IncidentTypeInfo{}, false
}
func (fakeCatalog) TypeByCode(string) (core.IncidentTypeInfo, bool) {
	return core.IncidentTypeInfo{}, false
}
func (c fakeCatalog) ServiceByID(id string) (core.ServiceInfo, bool) {
	return core.ServiceInfo{ID: id}, c.services[id]
}
func (fakeCatalog) ServiceByCode(string) (core.ServiceInfo, bool) { return core.ServiceInfo{}, false }
func (fakeCatalog) FieldLabel(p string) string {
	if p == "address.raw" {
		return "Адрес"
	}
	return "Поле карточки"
}
func (fakeCatalog) AttributeLabel(string) (string, bool)              { return "", false }
func (fakeCatalog) AttributeValueLabel(string, string) (string, bool) { return "", false }

func fakeHash(_ context.Context, pw string) (string, error) { return "fake$" + pw, nil }

// ---------------------------------------------------------------- окружение

type uenv struct {
	mux   *http.ServeMux
	pool  *pgxpool.Pool
	audit *auditRec
	rev   *revokerRec
}

func newUEnv(t *testing.T, pool *pgxpool.Pool, mut ...func(*Deps)) *uenv {
	t.Helper()
	e := &uenv{pool: pool, audit: &auditRec{}, rev: &revokerRec{}}
	d := Deps{Pool: pool, Auditor: e.audit, Sessions: e.rev, HashPassword: fakeHash, Log: slog.New(slog.DiscardHandler)}
	for _, m := range mut {
		m(&d)
	}
	e.mux = http.NewServeMux()
	NewHandlers(d).Register(httpx.NewRouter(e.mux, "/api/v1", stubAuth{}, slog.New(slog.DiscardHandler), nil))
	return e
}

func as(id uuid.UUID, role core.Role) *core.Principal { return &core.Principal{UserID: id, Role: role} }

func (e *uenv) do(t *testing.T, p *core.Principal, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if s, ok := body.(string); ok {
		rd = strings.NewReader(s)
	} else if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, "/api/v1"+path, rd)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Requested-With", "fetch")
	if p != nil {
		r.Header.Set("X-Test-User", p.UserID.String())
		r.Header.Set("X-Test-Role", string(p.Role))
	}
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, r)
	return w
}

type apiErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details struct {
		Fields map[string]string `json:"fields"`
	} `json:"details"`
}

func expectErr(t *testing.T, w *httptest.ResponseRecorder, status int, code string) apiErr {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d, want %d: %s", w.Code, status, w.Body)
	}
	var e apiErr
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || e.Code != code || e.Message == "" {
		t.Fatalf("ApiError code=%q, want %q: %s", e.Code, code, w.Body)
	}
	return e
}

func decodeUser(t *testing.T, w *httptest.ResponseRecorder, status int) public.User {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d, want %d: %s", w.Code, status, w.Body)
	}
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"id", "login", "role", "lastName", "firstName", "status"} {
		if _, ok := raw[f]; !ok {
			t.Fatalf("в User нет обязательного %s: %s", f, w.Body)
		}
	}
	if g, ok := raw["groupIds"].([]any); !ok || g == nil {
		t.Fatalf("groupIds должен быть массивом: %s", w.Body)
	}
	var u public.User
	_ = json.Unmarshal(w.Body.Bytes(), &u)
	return u
}

func decodeUsers(t *testing.T, w *httptest.ResponseRecorder) []public.User {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", w.Code, w.Body)
	}
	if b := bytes.TrimSpace(w.Body.Bytes()); len(b) == 0 || b[0] != '[' {
		t.Fatalf("ожидался массив (не null): %s", w.Body)
	}
	var out []public.User
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func logins(us []public.User) []string {
	out := make([]string, 0, len(us))
	for _, u := range us {
		out = append(out, u.Login)
	}
	slices.Sort(out)
	return out
}

// ---------------------------------------------------------------- сид

type seed struct {
	t    *testing.T
	pool *pgxpool.Pool
}

func (s seed) exec(sql string, args ...any) {
	s.t.Helper()
	if _, err := s.pool.Exec(context.Background(), sql, args...); err != nil {
		s.t.Fatalf("seed: %v\n%s", err, sql)
	}
}

func (s seed) user(login, role, last, status string) uuid.UUID {
	s.t.Helper()
	id := ids.New()
	s.exec(`INSERT INTO users (id, login, password_hash, role, last_name, first_name, middle_name, status)
VALUES ($1, $2, 'x', $3, $4, 'Имя', 'Отчество', $5)`, id, login, role, last, status)
	return id
}

func (s seed) group(name string, teacher uuid.UUID, archived bool, members ...uuid.UUID) uuid.UUID {
	s.t.Helper()
	id := ids.New()
	s.exec(`INSERT INTO groups (id, name, teacher_id, archived_at) VALUES ($1, $2, $3, CASE WHEN $4 THEN now() END)`, id, name, teacher, archived)
	for _, m := range members {
		s.exec(`INSERT INTO group_members (group_id, user_id) VALUES ($1, $2)`, id, m)
	}
	return id
}

func (s seed) lesson(teacher uuid.UUID, participants ...uuid.UUID) uuid.UUID {
	s.t.Helper()
	id := ids.New()
	s.exec(`INSERT INTO lessons (id, title, teacher_id, created_by, mode) VALUES ($1, 'Занятие', $2, $2, 'cards')`, id, teacher)
	for _, p := range participants {
		s.exec(`INSERT INTO lesson_participants (lesson_id, user_id) VALUES ($1, $2)`, id, p)
	}
	return id
}

func (s seed) serviceID(code string) uuid.UUID {
	s.t.Helper()
	var id uuid.UUID
	if err := s.pool.QueryRow(context.Background(), `SELECT id FROM services WHERE code = $1`, code).Scan(&id); err != nil {
		s.t.Fatal(err)
	}
	return id
}

// ---------------------------------------------------------------- без БД: RBAC и валидация

func TestRBACAndValidationWithoutDB(t *testing.T) {
	t.Parallel()
	e := newUEnv(t, nil) // до БД дело не доходит: nil pool упал бы паникой
	admin, teacher, student := as(uuid.New(), core.RoleAdmin), as(uuid.New(), core.RoleTeacher), as(uuid.New(), core.RoleStudent)
	other := uuid.NewString()
	validCreate := map[string]any{"login": "new.user", "password": "Secret123", "role": "student", "lastName": "Тестов", "firstName": "Тест"}

	cases := []struct {
		name         string
		p            *core.Principal
		method, path string
		body         any
		status       int
		code         string
		fields       []string
	}{
		{"список без входа", nil, "GET", "/users", nil, 401, "unauthorized", nil},
		{"список студенту", student, "GET", "/users", nil, 403, "forbidden", nil},
		{"кривая роль в фильтре", admin, "GET", "/users?role=boss", nil, 400, "validation", []string{"role"}},
		{"создание преподавателем", teacher, "POST", "/users", validCreate, 403, "forbidden", nil},
		{"создание студентом", student, "POST", "/users", validCreate, 403, "forbidden", nil},
		{"правка преподавателем", teacher, "PATCH", "/users/" + other, map[string]any{"lastName": "X"}, 403, "forbidden", nil},
		{"блокировка преподавателем", teacher, "PUT", "/users/" + other + "/blocked", map[string]any{"blocked": true}, 403, "forbidden", nil},
		{"прогресс без входа", nil, "GET", "/users/" + other + "/progress", nil, 401, "unauthorized", nil},
		{"чужой прогресс студенту", student, "GET", "/users/" + other + "/progress", nil, 403, "forbidden", nil},
		{"прогресс: не uuid", admin, "GET", "/users/not-a-uuid/progress", nil, 404, "not_found", nil},
		{"правка: не uuid", admin, "PATCH", "/users/42", map[string]any{"lastName": "X"}, 404, "not_found", nil},
		{"блокировка: не uuid", admin, "PUT", "/users/42/blocked", map[string]any{"blocked": true}, 404, "not_found", nil},
		{"создание: всё пусто", admin, "POST", "/users", map[string]any{}, 400, "validation",
			[]string{"login", "password", "role", "lastName", "firstName"}},
		{"создание: плохие поля", admin, "POST", "/users", map[string]any{
			"login": "иванов", "password": "short", "role": "root", "lastName": "  ", "firstName": strings.Repeat("я", 101),
			"middleName": "a\x00b", "serviceId": "zhkh", "groupIds": []string{"bad"}}, 400, "validation",
			[]string{"login", "password", "role", "lastName", "firstName", "middleName", "serviceId", "groupIds"}},
		// регрессия: NUL в ФИО раньше доходил до PostgreSQL и давал 500
		{"создание: NUL в фамилии", admin, "POST", "/users", map[string]any{
			"login": "nul.user", "password": "Secret123", "role": "student", "lastName": "Ива\x00нов", "firstName": "Тест"}, 400, "validation",
			[]string{"lastName"}},
		{"создание: кривой JSON", admin, "POST", "/users", `{"login":`, 400, "validation", nil},
		{"создание: groupIds не массив", admin, "POST", "/users", `{"groupIds": "x"}`, 400, "validation", nil},
		{"правка: плохие поля", admin, "PATCH", "/users/" + other, map[string]any{
			"role": "boss", "lastName": "", "firstName": " ", "password": "1234567", "groupIds": []string{"x"}, "serviceId": "nope"}, 400, "validation",
			[]string{"role", "lastName", "firstName", "password", "groupIds", "serviceId"}},
		{"правка: NUL в отчестве", admin, "PATCH", "/users/" + other, map[string]any{"middleName": "\x00"}, 400, "validation", []string{"middleName"}},
		{"блокировка: нет поля", admin, "PUT", "/users/" + other + "/blocked", map[string]any{}, 400, "validation", []string{"blocked"}},
		{"блокировка: не bool", admin, "PUT", "/users/" + other + "/blocked", `{"blocked": "yes"}`, 400, "validation", nil},
		{"блокировка себя", admin, "PUT", "/users/" + admin.UserID.String() + "/blocked", map[string]any{"blocked": true}, 409, "conflict", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := e.do(t, c.p, c.method, c.path, c.body)
			er := expectErr(t, w, c.status, c.code)
			if c.fields != nil {
				got := make([]string, 0, len(er.Details.Fields))
				for k := range er.Details.Fields {
					got = append(got, k)
				}
				slices.Sort(got)
				want := slices.Clone(c.fields)
				slices.Sort(want)
				if !slices.Equal(got, want) {
					t.Fatalf("поля с ошибками %v, want %v: %s", got, want, w.Body)
				}
			}
		})
	}

	t.Run("без X-Requested-With", func(t *testing.T) {
		t.Parallel()
		r := httptest.NewRequest("POST", "/api/v1/users", strings.NewReader(`{}`))
		r.Header.Set("X-Test-User", admin.UserID.String())
		r.Header.Set("X-Test-Role", "admin")
		w := httptest.NewRecorder()
		e.mux.ServeHTTP(w, r)
		expectErr(t, w, 403, "forbidden")
	})

	t.Run("служба проверяется по справочнику", func(t *testing.T) {
		t.Parallel()
		known := uuid.NewString()
		ec := newUEnv(t, nil, func(d *Deps) { d.Catalog = fakeCatalog{services: map[string]bool{known: true}} })
		body := map[string]any{"login": "svc.user", "password": "Secret123", "role": "student", "lastName": "Т", "firstName": "Т",
			"serviceId": uuid.NewString()}
		er := expectErr(t, ec.do(t, admin, "POST", "/users", body), 400, "validation")
		if er.Details.Fields["serviceId"] != "Служба не найдена" {
			t.Fatalf("%+v", er)
		}
	})

	t.Run("хэширование не настроено — 500, не паника", func(t *testing.T) {
		t.Parallel()
		en := newUEnv(t, nil, func(d *Deps) { d.HashPassword = nil })
		expectErr(t, en.do(t, admin, "POST", "/users", validCreate), 500, "internal")
	})

	t.Run("ошибка хэширования — 500", func(t *testing.T) {
		t.Parallel()
		en := newUEnv(t, nil, func(d *Deps) {
			d.HashPassword = func(context.Context, string) (string, error) { return "", errors.New("boom") }
		})
		expectErr(t, en.do(t, admin, "POST", "/users", validCreate), 500, "internal")
	})
}

// ---------------------------------------------------------------- с БД

func TestCreateUser(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	e := newUEnv(t, pool)
	s := seed{t, pool}
	adminID := s.user("root", "admin", "Админов", "active")
	admin := as(adminID, core.RoleAdmin)
	teacher := s.user("t1", "teacher", "Учителев", "active")
	g1 := s.group("Группа 1", teacher, false)
	gArch := s.group("Архив", teacher, true)
	zhkh := s.serviceID("zhkh")

	w := e.do(t, admin, "POST", "/users", map[string]any{
		"login": "  New.User  ", "password": "Пароль-123", "role": "student", "lastName": " Рожкова ", "firstName": "Ольга",
		"middleName": "  Ивановна ", "serviceId": zhkh.String(), "groupIds": []string{g1.String(), g1.String()},
	})
	u := decodeUser(t, w, http.StatusCreated)
	if u.Login != "New.User" || u.Role != "student" || u.LastName != "Рожкова" || u.MiddleName == nil || *u.MiddleName != "Ивановна" ||
		u.Status != "active" || u.ServiceId == nil || *u.ServiceId != zhkh.String() || u.ServiceName == nil ||
		len(*u.GroupIds) != 1 || (*u.GroupIds)[0] != g1 {
		t.Fatalf("созданный пользователь: %s", w.Body)
	}
	var hash string
	var mustChange bool
	var createdBy uuid.UUID
	if err := pool.QueryRow(context.Background(), `SELECT password_hash, must_change_password, created_by FROM users WHERE id = $1`, u.Id).
		Scan(&hash, &mustChange, &createdBy); err != nil {
		t.Fatal(err)
	}
	if hash != "fake$Пароль-123" || !mustChange || createdBy != adminID {
		t.Fatalf("в БД: hash=%q mustChange=%v createdBy=%v", hash, mustChange, createdBy)
	}
	au := e.audit.byAction("user.create")
	if len(au) != 1 || au[0].EntityID != u.Id {
		t.Fatalf("аудит: %+v", au)
	}
	if b, _ := json.Marshal(au[0].After); strings.Contains(string(b), "Пароль") || strings.Contains(string(b), "fake$") {
		t.Fatalf("пароль в аудите: %s", b)
	}

	base := func(login string) map[string]any {
		return map[string]any{"login": login, "password": "Secret123", "role": "teacher", "lastName": "Т", "firstName": "Т"}
	}
	// без групп — groupIds: []
	u2 := decodeUser(t, e.do(t, admin, "POST", "/users", base("plain")), http.StatusCreated)
	if len(*u2.GroupIds) != 0 || u2.MiddleName != nil || u2.ServiceId != nil {
		t.Fatalf("без групп/службы: %+v", u2)
	}
	// логин занят (citext: регистр не важен)
	er := expectErr(t, e.do(t, admin, "POST", "/users", base("NEW.user")), http.StatusConflict, "conflict")
	if er.Message != "Логин занят" {
		t.Fatalf("%+v", er)
	}
	// удалённый не держит логин
	s.exec(`UPDATE users SET deleted_at = now() WHERE login = 'plain'`)
	decodeUser(t, e.do(t, admin, "POST", "/users", base("plain")), http.StatusCreated)

	for name, body := range map[string]map[string]any{
		"неизвестная группа":     {"groupIds": []string{uuid.NewString()}},
		"архивная группа":        {"groupIds": []string{gArch.String()}},
		"неизвестная служба":     {"serviceId": uuid.NewString()}, // без Catalog — страхует FK
		"одна группа неизвестна": {"groupIds": []string{g1.String(), uuid.NewString()}},
	} {
		b := base("x" + strings.ReplaceAll(uuid.NewString()[:8], "-", ""))
		for k, v := range body {
			b[k] = v
		}
		er := expectErr(t, e.do(t, admin, "POST", "/users", b), http.StatusBadRequest, "validation")
		if len(er.Details.Fields) != 1 {
			t.Fatalf("%s: %+v", name, er)
		}
	}
	if n := len(e.audit.byAction("user.create")); n != 3 {
		t.Fatalf("аудит create: %d, want 3 (неудачные попытки не пишутся)", n)
	}
}

func TestUpdateUser(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	e := newUEnv(t, pool)
	s := seed{t, pool}
	adminID := s.user("root", "admin", "Админов", "active")
	admin := as(adminID, core.RoleAdmin)
	teacher := s.user("t1", "teacher", "Учителев", "active")
	stud := s.user("stud", "student", "Рожкова", "active")
	g1 := s.group("Г1", teacher, false, stud)
	g2 := s.group("Г2", teacher, false)
	gArch := s.group("Архив", teacher, true, stud)
	zhkh := s.serviceID("zhkh")
	s.exec(`UPDATE users SET service_id = $2, failed_login_count = 3, locked_until = now() + interval '1 hour', must_change_password = false WHERE id = $1`, stud, zhkh)
	path := "/users/" + stud.String()

	// пустое тело — текущее состояние без записи и аудита
	u := decodeUser(t, e.do(t, admin, "PATCH", path, map[string]any{}), http.StatusOK)
	if u.Id != stud || len(*u.GroupIds) != 1 || (*u.GroupIds)[0] != g1 {
		t.Fatalf("пустой PATCH: %+v", u)
	}
	if len(e.audit.byAction("user.update")) != 0 {
		t.Fatal("пустой PATCH попал в аудит")
	}
	expectErr(t, e.do(t, admin, "PATCH", "/users/"+uuid.NewString(), map[string]any{}), 404, "not_found")
	expectErr(t, e.do(t, admin, "PATCH", "/users/"+uuid.NewString(), map[string]any{"lastName": "X"}), 404, "not_found")

	// ФИО, снятие отчества и службы (null), смена групп: архивное членство — история, остаётся
	w := e.do(t, admin, "PATCH", path, map[string]any{
		"lastName": "  Никитина ", "middleName": "", "serviceId": nil, "groupIds": []string{g2.String()},
	})
	u = decodeUser(t, w, http.StatusOK)
	if u.LastName != "Никитина" || u.FirstName != "Имя" || u.MiddleName != nil || u.ServiceId != nil ||
		len(*u.GroupIds) != 1 || (*u.GroupIds)[0] != g2 {
		t.Fatalf("после PATCH: %s", w.Body)
	}
	var inArch bool
	_ = pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM group_members WHERE group_id = $1 AND user_id = $2)`, gArch, stud).Scan(&inArch)
	if !inArch {
		t.Fatal("членство в архивной группе удалено")
	}
	if rev, _, _ := e.rev.snapshot(); len(rev) != 0 {
		t.Fatal("без смены пароля сессии отозваны")
	}
	au := e.audit.byAction("user.update")
	if len(au) != 1 || au[0].Before.(userAudit).LastName != "Рожкова" || au[0].After.(userAudit).LastName != "Никитина" {
		t.Fatalf("аудит update: %+v", au)
	}
	if _, purged, _ := e.rev.snapshot(); !slices.Contains(purged, stud) {
		t.Fatal("кэш сессий не сброшен после изменения")
	}

	// serviceId "" — тоже снять; groupIds [] — выйти из неархивных групп
	s.exec(`UPDATE users SET service_id = $2 WHERE id = $1`, stud, zhkh)
	u = decodeUser(t, e.do(t, admin, "PATCH", path, map[string]any{"serviceId": "", "groupIds": []string{}}), http.StatusOK)
	if u.ServiceId != nil || len(*u.GroupIds) != 0 {
		t.Fatalf("снятие службы/групп: %+v", u)
	}
	// неизвестная служба через FK -> 400 поля, не 500
	er := expectErr(t, e.do(t, admin, "PATCH", path, map[string]any{"serviceId": uuid.NewString()}), 400, "validation")
	if er.Details.Fields["serviceId"] == "" {
		t.Fatalf("%+v", er)
	}

	// смена пароля: хэш, «сменить при входе», сброс lockout, отзыв сессий в транзакции
	decodeUser(t, e.do(t, admin, "PATCH", path, map[string]any{"password": "НовыйПароль1"}), http.StatusOK)
	var (
		hash       string
		mustChange bool
		fails      int
		lockedNull bool
	)
	_ = pool.QueryRow(context.Background(), `SELECT password_hash, must_change_password, failed_login_count, locked_until IS NULL FROM users WHERE id = $1`, stud).
		Scan(&hash, &mustChange, &fails, &lockedNull)
	if hash != "fake$НовыйПароль1" || !mustChange || fails != 0 || !lockedNull {
		t.Fatalf("смена пароля: hash=%q must=%v fails=%d lockedNull=%v", hash, mustChange, fails, lockedNull)
	}
	rev, _, inTx := e.rev.snapshot()
	if len(rev) != 1 || rev[0] != stud || !inTx[0] {
		t.Fatalf("отзыв сессий: %v inTx=%v", rev, inTx)
	}
	au = e.audit.byAction("user.update")
	if last := au[len(au)-1].After.(userAudit); !last.PasswordChanged {
		t.Fatalf("аудит без passwordChanged: %+v", last)
	}

	// свой пароль — не временный; свою роль менять нельзя (та же роль — можно)
	decodeUser(t, e.do(t, admin, "PATCH", "/users/"+adminID.String(), map[string]any{"password": "МойПароль1"}), http.StatusOK)
	_ = pool.QueryRow(context.Background(), `SELECT must_change_password FROM users WHERE id = $1`, adminID).Scan(&mustChange)
	if mustChange {
		t.Fatal("свой новый пароль помечен временным")
	}
	expectErr(t, e.do(t, admin, "PATCH", "/users/"+adminID.String(), map[string]any{"role": "student"}), 409, "conflict")
	decodeUser(t, e.do(t, admin, "PATCH", "/users/"+adminID.String(), map[string]any{"role": "admin"}), http.StatusOK)

	// смена роли другого — ок, группа должна существовать
	u = decodeUser(t, e.do(t, admin, "PATCH", path, map[string]any{"role": "teacher"}), http.StatusOK)
	if u.Role != "teacher" {
		t.Fatalf("роль: %+v", u)
	}
	expectErr(t, e.do(t, admin, "PATCH", path, map[string]any{"groupIds": []string{uuid.NewString()}}), 400, "validation")

	// ошибка отзыва сессий откатывает смену пароля
	e.rev.mu.Lock()
	e.rev.failWith = errors.New("revoke failed")
	e.rev.mu.Unlock()
	expectErr(t, e.do(t, admin, "PATCH", path, map[string]any{"password": "ЕщёОдин123"}), 500, "internal")
	_ = pool.QueryRow(context.Background(), `SELECT password_hash FROM users WHERE id = $1`, stud).Scan(&hash)
	if hash != "fake$НовыйПароль1" {
		t.Fatal("пароль сменён, хотя сессии не отозваны")
	}
}

func TestSetBlocked(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	e := newUEnv(t, pool)
	s := seed{t, pool}
	admin := as(s.user("root", "admin", "Админов", "active"), core.RoleAdmin)
	stud := s.user("stud", "student", "Рожкова", "active")
	path := "/users/" + stud.String() + "/blocked"

	u := decodeUser(t, e.do(t, admin, "PUT", path, map[string]any{"blocked": true}), http.StatusOK)
	if u.Status != "blocked" {
		t.Fatalf("%+v", u)
	}
	rev, purged, inTx := e.rev.snapshot()
	if len(rev) != 1 || !inTx[0] || !slices.Contains(purged, stud) {
		t.Fatalf("блокировка не отозвала сессии: %v %v %v", rev, inTx, purged)
	}
	// повтор — идемпотентно, без второго аудита
	decodeUser(t, e.do(t, admin, "PUT", path, map[string]any{"blocked": true}), http.StatusOK)
	if n := len(e.audit.byAction("user.block")); n != 1 {
		t.Fatalf("аудит block: %d", n)
	}

	s.exec(`UPDATE users SET failed_login_count = 4, locked_until = now() + interval '1 hour', blocked_reason = 'x' WHERE id = $1`, stud)
	u = decodeUser(t, e.do(t, admin, "PUT", path, map[string]any{"blocked": false}), http.StatusOK)
	if u.Status != "active" {
		t.Fatalf("%+v", u)
	}
	var fails int
	var lockedNull, reasonNull bool
	_ = pool.QueryRow(context.Background(), `SELECT failed_login_count, locked_until IS NULL, blocked_reason IS NULL FROM users WHERE id = $1`, stud).
		Scan(&fails, &lockedNull, &reasonNull)
	if fails != 0 || !lockedNull || !reasonNull {
		t.Fatal("разблокировка не сняла lockout")
	}
	ub := e.audit.byAction("user.unblock")
	if len(ub) != 1 || ub[0].Before.(statusAudit).Status != "blocked" || ub[0].After.(statusAudit).Status != "active" {
		t.Fatalf("аудит unblock: %+v", ub)
	}
	// снять блокировку с активного — ок и без аудита (снимает и lockout после серии неудач)
	decodeUser(t, e.do(t, admin, "PUT", path, map[string]any{"blocked": false}), http.StatusOK)
	if n := len(e.audit.byAction("user.unblock")); n != 1 {
		t.Fatalf("аудит unblock: %d", n)
	}
	// себя разблокировать можно (409 только на блокировку себя)
	decodeUser(t, e.do(t, admin, "PUT", "/users/"+admin.UserID.String()+"/blocked", map[string]any{"blocked": false}), http.StatusOK)
	expectErr(t, e.do(t, admin, "PUT", "/users/"+uuid.NewString()+"/blocked", map[string]any{"blocked": true}), 404, "not_found")
	s.exec(`UPDATE users SET deleted_at = now() WHERE id = $1`, stud)
	expectErr(t, e.do(t, admin, "PUT", path, map[string]any{"blocked": true}), 404, "not_found")
}

func TestListVisibility(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	e := newUEnv(t, pool)
	s := seed{t, pool}
	admin := as(s.user("root", "admin", "Админов", "active"), core.RoleAdmin)
	tA := s.user("teacher.a", "teacher", "Учитель-А", "active")
	tB := s.user("teacher.b", "teacher", "Учитель-Б", "active")
	tNew := s.user("teacher.new", "teacher", "Новенький", "active")
	sA := s.user("s.mine", "student", "Рожкова", "active")
	sAblocked := s.user("s.mine.blocked", "student", "Блокова", "blocked")
	sB := s.user("s.other", "student", "Чужой", "active")
	s.user("s.none", "student", "Ничейный", "active")
	s.user("s.none.blocked", "student", "Ничейный-блок", "blocked")
	sArchOnly := s.user("s.arch", "student", "Архивный", "active")
	sLesson := s.user("s.lesson", "student", "Занятой", "blocked")
	sDeleted := s.user("s.deleted", "student", "Удалённый", "active")
	s.group("A", tA, false, sA, sAblocked)
	s.group("B", tB, false, sB, sLesson)
	s.group("B-архив", tB, true, sArchOnly)
	s.lesson(tA, sLesson)
	s.exec(`UPDATE users SET deleted_at = now() WHERE id = $1`, sDeleted)

	t.Run("преподаватель", func(t *testing.T) {
		t.Parallel()
		got := logins(decodeUsers(t, e.do(t, as(tA, core.RoleTeacher), "GET", "/users", nil)))
		// свои группы (включая заблокированных своих), активные «ничьи», участники его занятий
		want := []string{"s.arch", "s.lesson", "s.mine", "s.mine.blocked", "s.none"}
		if !slices.Equal(got, want) {
			t.Fatalf("видно %v, want %v", got, want)
		}
	})
	// регрессия (review): новый преподаватель без групп видел заблокированных «ничьих» обучающихся
	t.Run("новый преподаватель", func(t *testing.T) {
		t.Parallel()
		got := logins(decodeUsers(t, e.do(t, as(tNew, core.RoleTeacher), "GET", "/users", nil)))
		if want := []string{"s.arch", "s.none"}; !slices.Equal(got, want) {
			t.Fatalf("видно %v, want %v", got, want)
		}
		if got := decodeUsers(t, e.do(t, as(tNew, core.RoleTeacher), "GET", "/users?role=teacher", nil)); len(got) != 0 {
			t.Fatalf("преподаватель видит преподавателей: %v", logins(got))
		}
	})
	t.Run("администратор", func(t *testing.T) {
		t.Parallel()
		all := decodeUsers(t, e.do(t, admin, "GET", "/users", nil))
		if len(all) != 11 || slices.Contains(logins(all), "s.deleted") || !slices.Contains(logins(all), "s.none.blocked") {
			t.Fatalf("админ видит %v", logins(all))
		}
		for i := 1; i < len(all); i++ {
			if all[i-1].LastName > all[i].LastName {
				t.Fatalf("не отсортировано по фамилии: %q > %q", all[i-1].LastName, all[i].LastName)
			}
		}
		for _, u := range all {
			if u.GroupIds == nil {
				t.Fatalf("%s: groupIds nil", u.Login)
			}
			if u.Id == sA && (len(*u.GroupIds) != 1) || u.Id == sArchOnly && len(*u.GroupIds) != 0 {
				t.Fatalf("%s: группы %v (архивные не показываются)", u.Login, *u.GroupIds)
			}
		}
		teachers := logins(decodeUsers(t, e.do(t, admin, "GET", "/users?role=teacher", nil)))
		if !slices.Equal(teachers, []string{"teacher.a", "teacher.b", "teacher.new"}) {
			t.Fatalf("role=teacher: %v", teachers)
		}
	})
	t.Run("поиск", func(t *testing.T) {
		t.Parallel()
		for q, want := range map[string][]string{
			"Рожк":           {"s.mine"},
			"Рожкова Имя":    {"s.mine"},                   // ФИО целиком
			"S.MINE":         {"s.mine", "s.mine.blocked"}, // логин без учёта регистра (citext/ILIKE)
			"%":              {},                           // метасимвол LIKE — буквально
			"_":              {},                           // и этот
			"s_none":         {},                           // _ не «любой символ»
			"s.none\x00":     {"s.none", "s.none.blocked"}, // регрессия: NUL -> 500; NUL выбрасывается
			"несуществующий": {},
		} {
			got := logins(decodeUsers(t, e.do(t, admin, "GET", "/users?q="+urlEscape(q), nil)))
			if !slices.Equal(got, want) {
				t.Fatalf("q=%q: %v, want %v", q, got, want)
			}
		}
		// только NUL / только пробелы — фильтра нет, весь список
		for _, q := range []string{"%00", "%20%20", "%00%20"} {
			if got := decodeUsers(t, e.do(t, admin, "GET", "/users?q="+q, nil)); len(got) != 11 {
				t.Fatalf("q=%s: %d", q, len(got))
			}
		}
		// битый UTF-8 в запросе — не 500
		decodeUsers(t, e.do(t, admin, "GET", "/users?q=%FF%FE", nil))
		// длинный запрос режется по рунам, не посреди UTF-8
		decodeUsers(t, e.do(t, admin, "GET", "/users?q="+urlEscape(strings.Repeat("ж", 150)), nil))
		// преподаватель + поиск: фильтр видимости сохраняется
		got := logins(decodeUsers(t, e.do(t, as(tA, core.RoleTeacher), "GET", "/users?q=%D0%A7%D1%83%D0%B6", nil))) // «Чуж»
		if len(got) != 0 {
			t.Fatalf("поиск преподавателя нашёл чужого: %v", got)
		}
	})
}

func urlEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' {
			b.WriteByte(c)
		} else {
			b.WriteString("%")
			b.WriteString(strings.ToUpper(string("0123456789abcdef"[c>>4]) + string("0123456789abcdef"[c&15])))
		}
	}
	return b.String()
}

func TestGetMany(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	s := seed{t, pool}
	a := s.user("a", "student", "Бобров", "active")
	b := s.user("b", "student", "Аксёнова", "active")
	d := s.user("d", "student", "Удалов", "active")
	s.exec(`UPDATE users SET deleted_at = now() WHERE id = $1`, d)
	got, err := GetMany(context.Background(), pool, []uuid.UUID{a, b, d, uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Id != b || got[1].Id != a {
		t.Fatalf("GetMany: %+v", got)
	}
	empty, err := GetMany(context.Background(), pool, nil)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("GetMany(nil): %v %v", empty, err)
	}
	if _, err := Get(context.Background(), pool, d); !pg.IsNoRows(err) {
		t.Fatalf("Get удалённого: %v", err)
	}
}

func TestProgress(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	e := newUEnv(t, pool, func(d *Deps) { d.Catalog = fakeCatalog{} })
	s := seed{t, pool}
	admin := as(s.user("root", "admin", "Админов", "active"), core.RoleAdmin)
	tA := s.user("teacher.a", "teacher", "Учитель-А", "active")
	tB := s.user("teacher.b", "teacher", "Учитель-Б", "active")
	st := s.user("stud", "student", "Рожкова", "active")
	st2 := s.user("stud2", "student", "Никитин", "active")
	stArch := s.user("stud.arch", "student", "Архивов", "active")
	stLesson := s.user("stud.lesson", "student", "Занятов", "active")
	s.group("A", tA, false, st)
	s.group("A-архив", tA, true, stArch)
	s.lesson(tA, stLesson)
	s.exec(`INSERT INTO xp_ledger (user_id, delta, reason, created_at) VALUES
  ($1, 100, 'attempt_evaluated', now() - interval '1 hour'),
  ($1, 20, 'manual', now())`, st)
	s.exec(`INSERT INTO recommendations (scope, user_id, source, kind, body, evidence) VALUES
  ('student', $1, 'teacher', 'general', 'Повторите опросную карту', '{"items": ["Пожар", "ДТП"]}'),
  ('student', $1, 'system', 'weak_field', 'Скрытая', '{}')`, st)
	s.exec(`UPDATE recommendations SET dismissed_at = now() WHERE body = 'Скрытая'`)

	get := func(p *core.Principal, id uuid.UUID) *httptest.ResponseRecorder {
		return e.do(t, p, "GET", "/users/"+id.String()+"/progress", nil)
	}

	w := get(as(st, core.RoleStudent), st)
	if w.Code != http.StatusOK {
		t.Fatalf("свой прогресс: %d %s", w.Code, w.Body)
	}
	var raw map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	for _, k := range []string{"categories", "fieldErrors"} {
		if arr, ok := raw[k].([]any); !ok || len(arr) != 0 {
			t.Fatalf("%s должен быть []: %s", k, w.Body)
		}
	}
	var sp public.StudentProgress
	if err := json.Unmarshal(w.Body.Bytes(), &sp); err != nil {
		t.Fatal(err)
	}
	if sp.UserId != st || sp.Name != "Рожкова Имя Отчество" || sp.Xp != 120 || sp.AttemptsDone != 0 ||
		sp.Level.No != 2 || sp.Level.Title != "Стажёр" || sp.Level.NextXpRequired == nil || *sp.Level.NextXpRequired != 300 {
		t.Fatalf("прогресс: %s", w.Body)
	}
	if len(sp.XpLog) != 2 || sp.XpLog[0].Delta != 20 || sp.XpLog[0].Reason != "manual" || sp.XpLog[0].At.Location() != time.UTC {
		t.Fatalf("xpLog (новые сверху, UTC): %+v", sp.XpLog)
	}
	if len(sp.Recommendations) != 1 || sp.Recommendations[0].Kind != public.General || sp.Recommendations[0].Items == nil ||
		strings.Join(*sp.Recommendations[0].Items, ",") != "Пожар,ДТП" {
		t.Fatalf("рекомендации (скрытые не показываются): %+v", sp.Recommendations)
	}

	// доступ
	for _, c := range []struct {
		name   string
		p      *core.Principal
		id     uuid.UUID
		status int
	}{
		{"преподаватель группы", as(tA, core.RoleTeacher), st, 200},
		{"преподаватель архивной группы", as(tA, core.RoleTeacher), stArch, 200},
		{"преподаватель занятия", as(tA, core.RoleTeacher), stLesson, 200},
		{"чужой преподаватель", as(tB, core.RoleTeacher), st, 403},
		{"не в группах и не на занятиях", as(tA, core.RoleTeacher), st2, 403},
		{"админ", admin, st2, 200},
		{"другой студент", as(st2, core.RoleStudent), st, 403},
		{"не обучающийся", admin, tA, 404},
		{"неизвестный", admin, uuid.New(), 404},
	} {
		w := get(c.p, c.id)
		if w.Code != c.status {
			t.Fatalf("%s: %d, want %d: %s", c.name, w.Code, c.status, w.Body)
		}
	}

	// нулевой прогресс: уровень 1 (xp_required 0), пустые массивы
	w = get(admin, st2)
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	for _, k := range []string{"categories", "fieldErrors", "recommendations", "xpLog"} {
		if arr, ok := raw[k].([]any); !ok || len(arr) != 0 {
			t.Fatalf("%s должен быть []: %s", k, w.Body)
		}
	}
	if lvl := raw["level"].(map[string]any); lvl["no"] != float64(1) || lvl["title"] != "Новичок" {
		t.Fatalf("уровень: %v", lvl)
	}
}
