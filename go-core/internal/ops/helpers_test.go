package ops

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/config"
	"lct/gocore/internal/core"
	"lct/gocore/internal/platform/pgtest"
	"lct/gocore/internal/settings"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// auditRecorder — core.Auditor, запоминающий записи.
type auditRecorder struct {
	mu      sync.Mutex
	entries []core.AuditEntry
}

func (a *auditRecorder) Log(_ context.Context, e core.AuditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
}

func (a *auditRecorder) all() []core.AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]core.AuditEntry(nil), a.entries...)
}

// Скрипты-заменители pg_dump: пишут в файл из -f пароль (из окружения) и argv — тест
// проверяет, что пароль не попал в argv.
const (
	fakeDumpOK = `#!/bin/sh
out=""
all="$*"
while [ $# -gt 0 ]; do
  case "$1" in
    -f) out="$2"; shift 2 ;;
    *) shift ;;
  esac
done
printf 'PGDMP|%s|%s|%s' "$PGPASSWORD" "$PGAPPNAME" "$all" > "$out"
`
	fakeDumpFail = `#!/bin/sh
echo "pg_dump: error: connection to server failed: Connection refused" >&2
exit 1
`
	fakeDumpSlow = `#!/bin/sh
out=""
while [ $# -gt 0 ]; do
  case "$1" in
    -f) out="$2"; shift 2 ;;
    *) shift ;;
  esac
done
printf 'partial' > "$out"
exec sleep 30
`
)

func writeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pg_dump")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

type env struct {
	pool *pgxpool.Pool
	url  string
	cfg  *config.Config
	aud  *auditRecorder
	o    *Ops
}

// newEnv — Ops поверх свежей БД; set != nil — с хранилищем настроек.
func newEnv(t *testing.T, withSettings bool) *env {
	t.Helper()
	pool, url := pgtest.NewWithURL(t)
	cfg := &config.Config{
		DatabaseURL: url,
		BackupDir:   filepath.Join(t.TempDir(), "backups"),
		TTSDir:      t.TempDir(),
		PgDumpPath:  writeScript(t, fakeDumpOK),
	}
	var st *settings.Store
	if withSettings {
		st = settings.NewStore(pool, discardLog())
	}
	aud := &auditRecorder{}
	o := New(Deps{Pool: pool, Config: cfg, Settings: st, Auditor: aud, Log: discardLog()})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = o.Wait(ctx)
	})
	return &env{pool: pool, url: url, cfg: cfg, aud: aud, o: o}
}

// newStore — новое хранилище настроек (перечитает таблицу при первом Get).
func newStore(e *env) *settings.Store { return settings.NewStore(e.pool, discardLog()) }

func (e *env) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(ctxT(t), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func (e *env) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(ctxT(t), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// seed — минимальная цепочка для попыток: пользователь, категория, сценарий, эталон.
type seed struct {
	user, scenario, etalon uuid.UUID
	seq                    int
}

func (e *env) seedBase(t *testing.T) *seed {
	t.Helper()
	s := &seed{user: uuid.New(), scenario: uuid.New(), etalon: uuid.New()}
	cat := uuid.New()
	e.exec(t, `INSERT INTO users (id, login, password_hash, role, last_name, first_name)
	            VALUES ($1, $2, 'x', 'teacher', 'Иванова', 'Мария')`, s.user, "u"+s.user.String()[:8])
	e.exec(t, `INSERT INTO classifier_categories (id, code, name) VALUES ($1, $2, 'Пожар')`, cat, "c"+cat.String()[:8])
	e.exec(t, `INSERT INTO scenarios (id, title, category_id, source) VALUES ($1, 'Пожар в квартире', $2, 'manual')`, s.scenario, cat)
	e.exec(t, `INSERT INTO etalons (id, scenario_id, card) VALUES ($1, $2, '{}')`, s.etalon, s.scenario)
	return s
}

func (e *env) seedLesson(t *testing.T, s *seed, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	e.exec(t, `INSERT INTO lessons (id, title, teacher_id, created_by, mode, status)
	            VALUES ($1, 'Занятие', $2, $2, 'cards', $3)`, id, s.user, status)
	return id
}

func (e *env) seedAttempt(t *testing.T, s *seed, lesson uuid.UUID, status string, updatedAgo time.Duration) uuid.UUID {
	t.Helper()
	id := uuid.New()
	s.seq++
	e.exec(t, `INSERT INTO attempts (id, lesson_id, user_id, scenario_id, etalon_id, mode, seq_no, status, time_limit_sec, updated_at)
	            VALUES ($1, $2, $3, $4, $5, 'cards', $6, $7, 30, now() - make_interval(secs => $8::int))`,
		id, lesson, s.user, s.scenario, s.etalon, s.seq, status, int(updatedAgo/time.Second))
	return id
}
