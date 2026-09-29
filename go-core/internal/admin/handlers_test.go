package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/ops"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/platform/pgtest"
	"lct/gocore/internal/settings"
)

var adminRoutes = []struct{ method, path string }{
	{http.MethodGet, "/admin/health"},
	{http.MethodGet, "/admin/settings"},
	{http.MethodPut, "/admin/settings/ai"},
	{http.MethodGet, "/admin/audit"},
	{http.MethodGet, "/admin/backups"},
	{http.MethodPost, "/admin/backups"},
	{http.MethodGet, "/admin/logs"},
}

func TestRoutesRegistered(t *testing.T) {
	t.Parallel()
	_, r := newServer(t, New(Deps{Log: discardLog()}))
	got := r.Routes()
	slices.Sort(got)
	want := []string{"GET /admin/audit", "GET /admin/backups", "GET /admin/health", "GET /admin/logs", "GET /admin/settings",
		"POST /admin/backups", "PUT /admin/settings/{key}"}
	if !slices.Equal(got, want) {
		t.Fatalf("routes = %v, want %v", got, want)
	}
}

// RBAC: только admin. Отказ — до хендлера, поэтому зависимости не нужны.
func TestAdminRBAC(t *testing.T) {
	t.Parallel()
	ops := &fakeOps{}
	srv, _ := newServer(t, New(Deps{Ops: ops, Settings: settings.NewStore(nil, discardLog()), Log: discardLog()}))
	for _, rt := range adminRoutes {
		body := ""
		if rt.method == http.MethodPut {
			body = `{"value": {"max_tries": 4}}`
		}
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			t.Parallel()
			expectError(t, do(t, srv, rt.method, rt.path, body, reqOpt{}), http.StatusUnauthorized, "unauthorized")
			expectError(t, do(t, srv, rt.method, rt.path, body, reqOpt{role: "blocked"}), http.StatusLocked, "user_blocked")
			for _, role := range []string{"teacher", "student"} {
				expectError(t, do(t, srv, rt.method, rt.path, body, reqOpt{role: role}), http.StatusForbidden, "forbidden")
			}
			if rt.method != http.MethodGet {
				// CSRF-замок — и для администратора
				expectError(t, do(t, srv, rt.method, rt.path, body, reqOpt{role: "admin", noCSRF: true}), http.StatusForbidden, "forbidden")
			}
		})
	}
	if len(ops.started) != 0 {
		t.Fatalf("бэкап запущен в обход RBAC: %d", len(ops.started))
	}
}

// ---------------------------------------------------------------- резервные копии

func TestBackupsHandlers(t *testing.T) {
	t.Parallel()
	admin := uuid.New()
	opt := reqOpt{role: "admin", user: admin}

	t.Run("пустой журнал — [] а не null", func(t *testing.T) {
		t.Parallel()
		srv, _ := newServer(t, New(Deps{Ops: &fakeOps{}, Log: discardLog()}))
		r := do(t, srv, http.MethodGet, "/admin/backups", "", opt)
		if r.status != http.StatusOK || strings.TrimSpace(string(r.body)) != "[]" {
			t.Fatalf("%d %s", r.status, r.body)
		}
	})
	t.Run("журнал", func(t *testing.T) {
		t.Parallel()
		size := int64(1024)
		list := []public.Backup{{Id: uuid.New(), StartedAt: time.Now().UTC(), Status: "done", SizeBytes: &size}}
		srv, _ := newServer(t, New(Deps{Ops: &fakeOps{list: list}, Log: discardLog()}))
		r := do(t, srv, http.MethodGet, "/admin/backups", "", opt)
		got := decode[[]map[string]any](t, r)
		if r.status != http.StatusOK || len(got) != 1 || got[0]["status"] != "done" || got[0]["id"] == nil || got[0]["startedAt"] == nil {
			t.Fatalf("%d %s", r.status, r.body)
		}
	})
	t.Run("ошибка журнала — 500", func(t *testing.T) {
		t.Parallel()
		srv, _ := newServer(t, New(Deps{Ops: &fakeOps{listErr: errors.New("db down")}, Log: discardLog()}))
		expectError(t, do(t, srv, http.MethodGet, "/admin/backups", "", opt), http.StatusInternalServerError, "internal")
	})
	t.Run("запуск — 202, инициатор — текущий админ", func(t *testing.T) {
		t.Parallel()
		f := &fakeOps{}
		srv, _ := newServer(t, New(Deps{Ops: f, Log: discardLog()}))
		r := do(t, srv, http.MethodPost, "/admin/backups", "", opt)
		if r.status != http.StatusAccepted {
			t.Fatalf("%d %s", r.status, r.body)
		}
		b := decode[public.Backup](t, r)
		if b.Status != "running" || b.TriggeredBy == nil || *b.TriggeredBy != admin {
			t.Fatalf("backup = %+v", b)
		}
		if len(f.started) != 1 || f.started[0] == nil || *f.started[0] != admin {
			t.Fatalf("started = %v", f.started)
		}
	})
	t.Run("уже идёт — 409", func(t *testing.T) {
		t.Parallel()
		srv, _ := newServer(t, New(Deps{Ops: &fakeOps{startErr: fmt.Errorf("begin: %w", ops.ErrBackupRunning)}, Log: discardLog()}))
		expectError(t, do(t, srv, http.MethodPost, "/admin/backups", "", opt), http.StatusConflict, "conflict")
	})
	t.Run("прочая ошибка — 500", func(t *testing.T) {
		t.Parallel()
		srv, _ := newServer(t, New(Deps{Ops: &fakeOps{startErr: errors.New("disk full")}, Log: discardLog()}))
		expectError(t, do(t, srv, http.MethodPost, "/admin/backups", "", opt), http.StatusInternalServerError, "internal")
	})
	t.Run("копирование не настроено — 404", func(t *testing.T) {
		t.Parallel()
		srv, _ := newServer(t, New(Deps{Log: discardLog()}))
		expectError(t, do(t, srv, http.MethodGet, "/admin/backups", "", opt), http.StatusNotFound, "not_found")
		expectError(t, do(t, srv, http.MethodPost, "/admin/backups", "", opt), http.StatusNotFound, "not_found")
	})
}

// ---------------------------------------------------------------- настройки

// Проверки ввода, срабатывающие до записи (БД не нужна: снимок — дефолты).
func TestUpdateSettingValidation(t *testing.T) {
	t.Parallel()
	aud := &recAuditor{}
	srv, _ := newServer(t, New(Deps{Settings: settings.NewStore(nil, discardLog()), Auditor: aud, Log: discardLog()}))
	opt := reqOpt{role: "admin"}
	cases := []struct {
		name, key, body string
		status          int
		code            string
		fields          []string
	}{
		{"нет value", "ai", `{}`, 400, "validation", []string{"value"}},
		{"value null", "ai", `{"value": null}`, 400, "validation", []string{"value"}},
		{"не JSON", "ai", `{"value": `, 400, "validation", nil},
		{"пустое тело", "ai", ``, 400, "validation", nil},
		{"неизвестный ключ", "no_such_key", `{"value": 1}`, 404, "not_found", nil},
		{"ключ в другом регистре", "AI", `{"value": {}}`, 404, "not_found", nil},
		{"неизвестное поле", "ai", `{"value": {"maxTries": 4}}`, 400, "validation", []string{"value"}},
		{"тип поля", "ai", `{"value": {"max_tries": "4"}}`, 400, "validation", []string{"value"}},
		{"тип ключа", "time_limit_sec", `{"value": "тридцать"}`, 400, "validation", []string{"value"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := expectError(t, do(t, srv, http.MethodPut, "/admin/settings/"+tc.key, tc.body, opt), tc.status, tc.code, tc.fields...)
			if strings.Contains(e.Message, "json:") {
				t.Errorf("сообщение не переведено: %q", e.Message)
			}
		})
	}
	if n := len(aud.all()); n != 0 {
		t.Fatalf("отклонённые правки попали в аудит: %d", n)
	}
}

func TestUpdateSettingWrongContentType(t *testing.T) {
	t.Parallel()
	srv, _ := newServer(t, New(Deps{Settings: settings.NewStore(nil, discardLog()), Log: discardLog()}))
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/v1/admin/settings/ai", strings.NewReader(`value=1`))
	req.Header.Set("X-Test-Role", "admin")
	req.Header.Set("X-Requested-With", "fetch")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status %d, want 415", res.StatusCode)
	}
}

type settingsEnv struct {
	pool  *pgxpool.Pool
	store *settings.Store
	aud   *recAuditor
	h     *Handlers
	admin uuid.UUID
	opt   reqOpt
}

func newSettingsEnv(t *testing.T, pool *pgxpool.Pool) *settingsEnv {
	t.Helper()
	store := settings.NewStore(pool, discardLog())
	if err := store.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	aud := &recAuditor{}
	admin := seedUser(t, pool, "admin", "Админов")
	h := New(Deps{Pool: pool, Settings: store, Auditor: aud, Log: discardLog()})
	return &settingsEnv{pool: pool, store: store, aud: aud, h: h, admin: admin, opt: reqOpt{role: "admin", user: admin}}
}

func (e *settingsEnv) dbValue(t *testing.T, key string) map[string]any {
	t.Helper()
	var raw []byte
	if err := e.pool.QueryRow(context.Background(), `SELECT value FROM settings WHERE key = $1`, key).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return m
}

func TestListSettings(t *testing.T) {
	t.Parallel()
	env := newSettingsEnv(t, pgtest.New(t))
	srv, _ := newServer(t, env.h)
	r := do(t, srv, http.MethodGet, "/admin/settings", "", env.opt)
	if r.status != http.StatusOK {
		t.Fatalf("%d %s", r.status, r.body)
	}
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(r.body, &list); err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, s := range list {
		var k string
		_ = json.Unmarshal(s["key"], &k)
		keys[k] = true
		if _, ok := s["value"]; !ok {
			t.Errorf("setting %s без value", k)
		}
		if !knownSetting(k) {
			t.Errorf("в таблице ключ вне схемы: %s", k)
		}
	}
	for _, k := range []string{"ai", "voice", "backup", "time_limit_sec", "score_weights"} {
		if !keys[k] {
			t.Errorf("нет ключа %s в %v", k, keys)
		}
	}
	// value — jsonb как есть (snake_case)
	if !strings.Contains(string(r.body), `"max_tries"`) {
		t.Errorf("значение ai не в snake_case: %s", r.body)
	}
}

func TestListSettingsEmptyTable(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	if _, err := pool.Exec(context.Background(), `DELETE FROM settings`); err != nil {
		t.Fatal(err)
	}
	h := New(Deps{Pool: pool, Settings: settings.NewStore(pool, discardLog()), Log: discardLog()})
	srv, _ := newServer(t, h)
	r := do(t, srv, http.MethodGet, "/admin/settings", "", reqOpt{role: "admin"})
	if r.status != http.StatusOK || strings.TrimSpace(string(r.body)) != "[]" {
		t.Fatalf("%d %s", r.status, r.body)
	}
}

func TestUpdateSettingHappyPath(t *testing.T) {
	t.Parallel()
	env := newSettingsEnv(t, pgtest.New(t))
	srv, _ := newServer(t, env.h)

	r := do(t, srv, http.MethodPut, "/admin/settings/ai", `{"value": {"max_tries": 5}}`, env.opt)
	if r.status != http.StatusOK {
		t.Fatalf("%d %s", r.status, r.body)
	}
	var got struct {
		Key         string         `json:"key"`
		Value       map[string]any `json:"value"`
		Description *string        `json:"description"`
		UpdatedAt   *time.Time     `json:"updatedAt"`
	}
	if err := json.Unmarshal(r.body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Key != "ai" || got.Value["max_tries"] != 5.0 || got.Value["reaper_after_sec"] != 900.0 || len(got.Value) != 5 {
		t.Fatalf("setting = %+v", got)
	}
	if got.Description == nil || *got.Description == "" || got.UpdatedAt == nil {
		t.Fatalf("description/updatedAt: %s", r.body)
	}
	if db := env.dbValue(t, "ai"); db["max_tries"] != 5.0 || db["breaker_failures"] != 3.0 {
		t.Fatalf("db = %v", db)
	}
	var by uuid.UUID
	if err := env.pool.QueryRow(context.Background(), `SELECT updated_by FROM settings WHERE key = 'ai'`).Scan(&by); err != nil || by != env.admin {
		t.Fatalf("updated_by = %v (%v), want %v", by, err, env.admin)
	}
	// кэш настроек обновлён сразу
	if n := env.store.Get(context.Background()).AI.MaxTries; n != 5 {
		t.Fatalf("snapshot max_tries = %d", n)
	}
	// аудит: before/after с ключом и полным значением
	entries := env.aud.all()
	if len(entries) != 1 || entries[0].Action != "setting.update" || entries[0].EntityType != "setting" {
		t.Fatalf("audit = %+v", entries)
	}
	before, _ := json.Marshal(entries[0].Before)
	after, _ := json.Marshal(entries[0].After)
	if !strings.Contains(string(before), `"max_tries":3`) || !strings.Contains(string(after), `"max_tries":5`) ||
		!strings.Contains(string(after), `"key":"ai"`) {
		t.Fatalf("audit before=%s after=%s", before, after)
	}

	// скалярный ключ
	r = do(t, srv, http.MethodPut, "/admin/settings/time_limit_sec", `{"value": 45}`, env.opt)
	if r.status != http.StatusOK || !strings.Contains(string(r.body), `"value":45`) {
		t.Fatalf("%d %s", r.status, r.body)
	}
	if env.store.Get(context.Background()).TimeLimitSec != 45 {
		t.Fatal("time_limit_sec не применён")
	}
}

// Смысловая проверка settings.Store (границы) — 400 с сообщением по-русски с заглавной.
func TestUpdateSettingSemanticValidation(t *testing.T) {
	t.Parallel()
	env := newSettingsEnv(t, pgtest.New(t))
	srv, _ := newServer(t, env.h)
	cases := []struct{ key, body string }{
		{"time_limit_sec", `{"value": 5}`},
		{"ai", `{"value": {"max_tries": 0}}`},
		{"voice", `{"value": {"input": "telepathy"}}`},
		{"audit_retention_days", `{"value": 30}`},
		{"score_weights", `{"value": {"fields": -1}}`},
	}
	for _, tc := range cases {
		e := expectError(t, do(t, srv, http.MethodPut, "/admin/settings/"+tc.key, tc.body, env.opt), 400, "validation", "value")
		if !strings.HasPrefix(e.Message, "Настройка") {
			t.Errorf("%s: message = %q", tc.key, e.Message)
		}
	}
	if db := env.dbValue(t, "ai"); db["max_tries"] != 3.0 {
		t.Fatalf("отклонённая правка записана: %v", db)
	}
	if len(env.aud.all()) != 0 {
		t.Fatal("отклонённые правки попали в аудит")
	}
}

// Ключ из схемы, строки которого нет в таблице: создаётся полным каноническим значением.
func TestUpdateSettingMissingRow(t *testing.T) {
	t.Parallel()
	env := newSettingsEnv(t, pgtest.New(t))
	if _, err := env.pool.Exec(context.Background(), `DELETE FROM settings WHERE key = 'login'`); err != nil {
		t.Fatal(err)
	}
	srv, _ := newServer(t, env.h)
	r := do(t, srv, http.MethodPut, "/admin/settings/login", `{"value": {"max_failed": 7}}`, env.opt)
	if r.status != http.StatusOK {
		t.Fatalf("%d %s", r.status, r.body)
	}
	if db := env.dbValue(t, "login"); db["max_failed"] != 7.0 || db["lock_minutes"] != 15.0 || db["session_ttl_hours"] != 12.0 {
		t.Fatalf("db = %v", db)
	}
}

// Регрессия (lost update): правка сливается с актуальной строкой БД, а не с кэшем настроек.
// Строку изменили в обход этого инстанса (другой инстанс / руками) меньше RefreshEvery назад —
// частичная правка другого поля не должна вернуть старое значение.
func TestUpdateSettingMergesWithFreshRow(t *testing.T) {
	t.Parallel()
	env := newSettingsEnv(t, pgtest.New(t))
	ctx := context.Background()
	if _, err := env.pool.Exec(ctx, `UPDATE settings SET value = jsonb_set(value, '{reaper_after_sec}', '600') WHERE key = 'ai'`); err != nil {
		t.Fatal(err)
	}
	if env.store.Get(ctx).AI.ReaperAfterSec != 900 {
		t.Fatal("предусловие: кэш ещё не видит правку")
	}
	srv, _ := newServer(t, env.h)
	r := do(t, srv, http.MethodPut, "/admin/settings/ai", `{"value": {"max_tries": 5}}`, env.opt)
	if r.status != http.StatusOK {
		t.Fatalf("%d %s", r.status, r.body)
	}
	db := env.dbValue(t, "ai")
	if db["max_tries"] != 5.0 || db["reaper_after_sec"] != 600.0 {
		t.Fatalf("правка потеряна: %v", db)
	}
}

// Регрессия (lost update): правки одного ключа идут строго по очереди. Пока ключ заблокирован
// (другой админ в середине своей правки), запрос ждёт и потом сливается с его результатом.
func TestUpdateSettingWaitsForConcurrentEdit(t *testing.T) {
	t.Parallel()
	env := newSettingsEnv(t, pgtest.New(t))
	ctx := context.Background()
	srv, _ := newServer(t, env.h)

	other, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Rollback(ctx) //nolint:errcheck
	if _, err := other.Exec(ctx, sqlSettingLock, "ai"); err != nil {
		t.Fatal(err)
	}

	done := make(chan resp, 1)
	go func() {
		done <- do(t, srv, http.MethodPut, "/admin/settings/ai", `{"value": {"max_tries": 5}}`, env.opt)
	}()
	select {
	case r := <-done:
		t.Fatalf("правка не дождалась блокировки ключа: %d %s", r.status, r.body)
	case <-time.After(300 * time.Millisecond):
	}
	// «другой админ» дописывает своё поле и коммитит — блокировка снимается
	if _, err := other.Exec(ctx, `UPDATE settings SET value = jsonb_set(value, '{breaker_open_sec}', '45') WHERE key = 'ai'`); err != nil {
		t.Fatal(err)
	}
	if err := other.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.status != http.StatusOK {
			t.Fatalf("%d %s", r.status, r.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("правка не завершилась после снятия блокировки")
	}
	db := env.dbValue(t, "ai")
	if db["max_tries"] != 5.0 || db["breaker_open_sec"] != 45.0 {
		t.Fatalf("правка другого админа потеряна: %v", db)
	}
}

// Одновременные правки разных полей одного ключа — все доезжают до БД.
func TestUpdateSettingConcurrentPartialEdits(t *testing.T) {
	t.Parallel()
	env := newSettingsEnv(t, pgtest.New(t))
	srv, _ := newServer(t, env.h)
	edits := map[string]float64{"reaper_after_sec": 600, "max_tries": 5, "dialog_timeout_sec": 25, "breaker_open_sec": 40, "breaker_failures": 4}
	var wg sync.WaitGroup
	for field, v := range edits {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := do(t, srv, http.MethodPut, "/admin/settings/ai", fmt.Sprintf(`{"value": {%q: %v}}`, field, v), env.opt)
			if r.status != http.StatusOK {
				t.Errorf("%s: %d %s", field, r.status, r.body)
			}
		}()
	}
	wg.Wait()
	db := env.dbValue(t, "ai")
	for field, v := range edits {
		if db[field] != v {
			t.Errorf("%s = %v, want %v (lost update): %v", field, db[field], v, db)
		}
	}
	if n := len(env.aud.all()); n != len(edits) {
		t.Errorf("audit entries = %d, want %d", n, len(edits))
	}
}

// Ключ заблокирован дольше lock_timeout — 409, а не зависший запрос.
func TestUpdateSettingLockTimeout(t *testing.T) {
	t.Parallel()
	pool, url := pgtest.NewWithURL(t)
	ctx := context.Background()
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	short, err := pg.Connect(ctx, url+sep+"lock_timeout=200", 4, "admin-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(short.Close)
	store := settings.NewStore(short, discardLog())
	h := New(Deps{Pool: short, Settings: store, Log: discardLog()})
	srv, _ := newServer(t, h)

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err := holder.Exec(ctx, sqlSettingLock, "voice"); err != nil {
		t.Fatal(err)
	}
	admin := seedUser(t, pool, "admin", "Ждущий")
	expectError(t, do(t, srv, http.MethodPut, "/admin/settings/voice", `{"value": {"max_turns": 10}}`, reqOpt{role: "admin", user: admin}),
		http.StatusConflict, "conflict")
	// другой ключ не заблокирован
	r := do(t, srv, http.MethodPut, "/admin/settings/stt", `{"value": {"confidence_floor": 0.5}}`, reqOpt{role: "admin", user: admin})
	if r.status != http.StatusOK {
		t.Fatalf("%d %s", r.status, r.body)
	}
	// после ошибки семафор в процессе освобождён
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	r = do(t, srv, http.MethodPut, "/admin/settings/voice", `{"value": {"max_turns": 10}}`, reqOpt{role: "admin", user: admin})
	if r.status != http.StatusOK {
		t.Fatalf("после снятия блокировки: %d %s", r.status, r.body)
	}
}

// Отменённый запрос, ждущий семафор, не занимает его навсегда.
func TestLockSettingContextCancel(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	h := New(Deps{Pool: pool, Settings: settings.NewStore(pool, discardLog()), Log: discardLog()})
	_, release, err := h.lockSetting(context.Background(), "ai")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := h.lockSetting(ctx, "ai"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	release()
	snap, release2, err := h.lockSetting(context.Background(), "ai")
	if err != nil {
		t.Fatal(err)
	}
	release2()
	if snap.AI.MaxTries != 3 {
		t.Fatalf("snapshot = %+v", snap.AI)
	}
}

// ---------------------------------------------------------------- журнал аудита

func TestListAudit(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	ctx := context.Background()
	actor := seedUser(t, pool, "teacher", "Петров")
	lesson := uuid.New()
	base := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	rows := []struct {
		at     time.Time
		actor  *uuid.UUID
		action string
		etype  string
	}{
		{base, &actor, "user.login", "session"},
		{base.Add(time.Minute), &actor, "lesson.start", "lesson"},
		{base.Add(2 * time.Minute), nil, "backup.run", "backup"},
		{base.Add(3 * time.Minute), &actor, "user.login_failed", "user"},
	}
	for _, r := range rows {
		if _, err := pool.Exec(ctx, `INSERT INTO audit_log (at, actor_id, actor_role, action, entity_type, lesson_id, after, ip)
			VALUES ($1, $2, 'teacher', $3, $4, $5, '{"k": "значение"}', '10.0.0.1')`, r.at, r.actor, r.action, r.etype, lesson); err != nil {
			t.Fatal(err)
		}
	}
	h := New(Deps{Pool: pool, Log: discardLog()})
	srv, _ := newServer(t, h)
	opt := reqOpt{role: "admin"}
	list := func(q string) []public.AuditEntry {
		t.Helper()
		r := do(t, srv, http.MethodGet, "/admin/audit"+q, "", opt)
		if r.status != http.StatusOK {
			t.Fatalf("%s: %d %s", q, r.status, r.body)
		}
		return decode[[]public.AuditEntry](t, r)
	}

	all := list("")
	if len(all) != 4 || all[0].Action != "user.login_failed" || all[3].Action != "user.login" {
		t.Fatalf("новые сверху: %+v", all)
	}
	if all[0].ActorName == nil || !strings.Contains(*all[0].ActorName, "Петров") || all[0].Ip == nil || *all[0].Ip != "10.0.0.1" {
		t.Errorf("entry = %+v", all[0])
	}
	if got := list("?action=user."); len(got) != 2 {
		t.Errorf("префикс user.: %d", len(got))
	}
	if got := list("?action=user.login"); len(got) != 1 {
		t.Errorf("точное user.login: %d", len(got))
	}
	if got := list("?actorId=" + actor.String() + "&entityType=lesson"); len(got) != 1 || got[0].Action != "lesson.start" {
		t.Errorf("actor+entityType: %+v", got)
	}
	if got := list("?limit=2"); len(got) != 2 {
		t.Errorf("limit: %d", len(got))
	}
	page := list("?limit=2&beforeId=" + fmt.Sprint(all[1].Id))
	if len(page) != 2 || page[0].Id != all[2].Id {
		t.Errorf("курсор: %+v", page)
	}
	if got := list("?from=2026-09-24T09:01:30Z&to=2026-09-24T09:02:30Z"); len(got) != 1 || got[0].Action != "backup.run" {
		t.Errorf("период: %+v", got)
	}
	// пустой результат — [] а не null
	r := do(t, srv, http.MethodGet, "/admin/audit?action=nothing.here", "", opt)
	if r.status != http.StatusOK || strings.TrimSpace(string(r.body)) != "[]" {
		t.Fatalf("пусто: %d %s", r.status, r.body)
	}
	expectError(t, do(t, srv, http.MethodGet, "/admin/audit?actorId=bad&limit=0", "", opt), 400, "validation", "actorId", "limit")
}
