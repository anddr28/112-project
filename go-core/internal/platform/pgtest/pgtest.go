// Package pgtest — PostgreSQL для тестов: каждому тесту своя чистая БД с применёнными миграциями.
//
// Шаблон `lct_test_tpl_<hash миграций>` создаётся один раз (под advisory lock — пакеты тестов
// идут параллельно), дальше CREATE DATABASE … TEMPLATE — это копирование файлов, миллисекунды.
// Сервер — LCT_TEST_DB_URL или postgres://lct:lct@localhost:55432/postgres (make db-up).
// Нет сервера — тест пропускается (t.Skip), а не падает: юнит-тесты остаются зелёными на
// машине без Docker; в CI сервер обязателен (LCT_TEST_DB_REQUIRED=1 -> t.Fatal).
package pgtest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
)

// AdminURL — URL служебной БД сервера (не той, что создаётся для теста).
func AdminURL() string {
	if v := os.Getenv("LCT_TEST_DB_URL"); v != "" {
		return v
	}
	return "postgres://lct:lct@localhost:55432/postgres?sslmode=disable"
}

// MigrationsDir — db/migrations репозитория (по пути этого файла).
func MigrationsDir() string {
	if v := os.Getenv("GOCORE_MIGRATIONS_DIR"); v != "" {
		return v
	}
	_, file, _, _ := runtime.Caller(0)
	// go-core/internal/platform/pgtest/pgtest.go -> <repo>/db/migrations
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "db", "migrations")
}

var (
	tplOnce sync.Once
	tplName string
	tplErr  error
)

// New — пул к свежей БД с миграциями; БД удаляется в t.Cleanup.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	pool, _ := NewWithURL(t)
	return pool
}

// NewWithURL — то же + DSN созданной БД (для goose/CLI/приложения в тесте).
func NewWithURL(t testing.TB) (*pgxpool.Pool, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, AdminURL())
	if err != nil {
		if os.Getenv("LCT_TEST_DB_REQUIRED") != "" {
			t.Fatalf("pgtest: нет PostgreSQL (%s): %v", AdminURL(), err)
		}
		t.Skipf("pgtest: PostgreSQL недоступен (%v) — запустите make db-up", err)
	}
	defer admin.Close(context.Background())

	tplOnce.Do(func() { tplName, tplErr = ensureTemplate(ctx, admin) })
	if tplErr != nil {
		t.Fatalf("pgtest: шаблон БД: %v", tplErr)
	}

	name := "lct_t_" + strings.ReplaceAll(ids.New().String(), "-", "")[:20]
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s TEMPLATE %s`, name, tplName)); err != nil {
		t.Fatalf("pgtest: create database: %v", err)
	}
	url := withDB(AdminURL(), name)
	pool, err := pg.Connect(ctx, url, 4, "pgtest") // пакеты тестов идут параллельно — бережём max_connections
	if err != nil {
		t.Fatalf("pgtest: connect: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, err := pgx.Connect(context.Background(), AdminURL())
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name))
	})
	return pool, url
}

// ensureTemplate — шаблонная БД для текущего набора миграций (имя зависит от их содержимого:
// новая миграция -> новый шаблон, старые остаются и не мешают).
func ensureTemplate(ctx context.Context, admin *pgx.Conn) (string, error) {
	dir := MigrationsDir()
	h, err := hashDir(dir)
	if err != nil {
		return "", err
	}
	name := "lct_test_tpl_" + h[:12]
	// параллельные пакеты тестов: шаблон создаёт один, остальные ждут
	if _, err := admin.Exec(ctx, `SELECT pg_advisory_lock(771122)`); err != nil {
		return "", err
	}
	defer admin.Exec(context.Background(), `SELECT pg_advisory_unlock(771122)`) //nolint:errcheck

	var exists bool
	if err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
		return "", err
	}
	if exists {
		return name, nil
	}
	tmp := name + "_build"
	_, _ = admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, tmp))
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, tmp)); err != nil {
		return "", err
	}
	db, err := sql.Open("pgx", withDB(AdminURL(), tmp))
	if err != nil {
		return "", err
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, os.DirFS(dir))
	if err == nil {
		_, err = p.Up(ctx)
	}
	db.Close()
	if err != nil {
		return "", fmt.Errorf("migrate template: %w", err)
	}
	// CREATE DATABASE … TEMPLATE требует, чтобы к шаблону никто не был подключён. Бэкенд
	// закрытого соединения goose завершается асинхронно — RENAME может на миг увидеть его
	// («is being accessed by other users»): повторяем.
	var renameErr error
	for i := 0; i < 50; i++ {
		if _, renameErr = admin.Exec(ctx, fmt.Sprintf(`ALTER DATABASE %s RENAME TO %s`, tmp, name)); renameErr == nil {
			return name, nil
		}
		if pe, ok := pg.PgError(renameErr); !ok || pe.Code != "55006" { // object_in_use
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", renameErr
}

func hashDir(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("migrations dir %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return "", err
		}
		h.Write([]byte(n))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// withDB — тот же DSN с другой БД: postgres[ql]://user:pass@host:port/<db>?params.
func withDB(dsn, db string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" || u.Host == "" {
		// не URL (key=value DSN): dbname последним параметром перекрывает прежний
		return dsn + " dbname=" + db
	}
	u.Path = "/" + db
	u.RawPath = ""
	return u.String()
}

var _ = stdlib.GetDefaultDriver // регистрирует драйвер "pgx" для goose
