package pg_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/platform/pgtest"
)

func show(t *testing.T, q pg.Querier, name string) string {
	t.Helper()
	var v string
	if err := q.QueryRow(context.Background(), "SHOW "+name).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Регрессия (ревью: missing-timeouts). У соединений пула есть страховочные таймауты
// сессии: зависшая транзакция не держит соединение и блокировки строк бесконечно.
func TestConnectSessionParams(t *testing.T) {
	t.Parallel()
	_, url := pgtest.NewWithURL(t)
	pool, err := pg.Connect(context.Background(), url, 2, "pg-test")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for name, want := range map[string]string{
		"idle_in_transaction_session_timeout": "1min",
		"lock_timeout":                        "15s",
		"statement_timeout":                   "1min",
		"application_name":                    "pg-test",
		"TimeZone":                            "UTC",
	} {
		if got := show(t, pool, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	// параметры из DATABASE_URL приоритетнее умолчаний; maxConns=1 (< MinConns по умолчанию) — не ошибка
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	p2, err := pg.Connect(context.Background(), url+sep+"statement_timeout=0&lock_timeout=2500", 1, "pg-test-2")
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	if got := show(t, p2, "statement_timeout"); got != "0" {
		t.Errorf("statement_timeout из URL: %q", got)
	}
	if got := show(t, p2, "lock_timeout"); got != "2500ms" {
		t.Errorf("lock_timeout из URL: %q", got)
	}
	if got := p2.Config().MaxConns; got != 1 {
		t.Errorf("MaxConns = %d", got)
	}
}

// Механизм, ради которого таймауты: транзакция, брошенная с блокировкой строки, через
// idle_in_transaction_session_timeout рвётся, и строка освобождается для других.
func TestIdleTransactionReleasesLocks(t *testing.T) {
	t.Parallel()
	_, url := pgtest.NewWithURL(t)
	ctx := context.Background()
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	pool, err := pg.Connect(ctx, url+sep+"idle_in_transaction_session_timeout=300", 4, "pg-test")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `CREATE TABLE t_lock (id int PRIMARY KEY); INSERT INTO t_lock VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	stuck, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stuck.Rollback(ctx) //nolint:errcheck
	if _, err := stuck.Exec(ctx, `SELECT id FROM t_lock WHERE id = 1 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	// «зависли» — ничего не делаем; другой должен получить строку, а не ждать вечно
	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	t0 := time.Now()
	err = pg.WithTx(lctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT id FROM t_lock WHERE id = 1 FOR UPDATE`)
		return err
	})
	if err != nil {
		t.Fatalf("строка не освободилась: %v (через %v)", err, time.Since(t0))
	}
}

type fixture struct {
	pool *pgxpool.Pool
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	pool := pgtest.New(t)
	_, err := pool.Exec(context.Background(), `
		CREATE TABLE t_items (
			id   int PRIMARY KEY,
			v    int NOT NULL CHECK (v >= 0),
			ref  int REFERENCES t_items (id),
			name text,
			CONSTRAINT t_items_name_key UNIQUE (name)
		)`)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{pool: pool}
}

func (f fixture) count(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM t_items`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWithTx(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	// COMMIT: хуки — после коммита, по порядку, видят закоммиченные данные
	var order []string
	err := pg.WithTx(ctx, f.pool, func(ctx context.Context, tx pgx.Tx) error {
		if !pg.InTx(ctx) {
			t.Error("InTx внутри WithTx = false")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO t_items (id, v) VALUES (1, 1)`); err != nil {
			return err
		}
		pg.OnCommit(ctx, func() { order = append(order, "a:"+strconv.Itoa(f.count(t))) })
		pg.OnCommit(ctx, func() { order = append(order, "b") })
		if len(order) != 0 {
			t.Error("хук выполнен до COMMIT")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "a:1,b" {
		t.Fatalf("хуки: %v", order)
	}

	// ошибка fn — ROLLBACK, хуки не выполняются, ошибка возвращается как есть
	sentinel := errors.New("стоп")
	ran := false
	err = pg.WithTx(ctx, f.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, _ = tx.Exec(ctx, `INSERT INTO t_items (id, v) VALUES (2, 1)`)
		pg.OnCommit(ctx, func() { ran = true })
		return sentinel
	})
	if !errors.Is(err, sentinel) || ran || f.count(t) != 1 {
		t.Fatalf("rollback: err=%v ran=%v count=%d", err, ran, f.count(t))
	}

	// паника — ROLLBACK и паника дальше; соединение возвращается в пул
	func() {
		defer func() {
			if recover() == nil {
				t.Error("паника проглочена")
			}
		}()
		_ = pg.WithTx(ctx, f.pool, func(ctx context.Context, tx pgx.Tx) error {
			_, _ = tx.Exec(ctx, `INSERT INTO t_items (id, v) VALUES (3, 1)`)
			panic("boom")
		})
	}()
	if f.count(t) != 1 {
		t.Fatal("паника не откатила транзакцию")
	}

	// вне транзакции OnCommit выполняется сразу
	now := false
	pg.OnCommit(ctx, func() { now = true })
	if !now || pg.InTx(ctx) {
		t.Fatal("OnCommit вне транзакции")
	}
}

func TestWithTxOpts(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	err := pg.WithTxOpts(ctx, f.pool, pg.TxOptions{ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO t_items (id, v) VALUES (10, 1)`)
		return err
	})
	if err == nil {
		t.Fatal("запись в read only транзакции прошла")
	}
	err = pg.WithTxOpts(ctx, f.pool, pg.TxOptions{AsyncCommit: true}, func(ctx context.Context, tx pgx.Tx) error {
		if got := show(t, tx, "synchronous_commit"); got != "off" {
			t.Errorf("synchronous_commit = %q", got)
		}
		_, err := tx.Exec(ctx, `INSERT INTO t_items (id, v) VALUES (11, 1)`)
		return err
	})
	if err != nil || f.count(t) != 1 {
		t.Fatalf("async commit: %v", err)
	}
	// SET LOCAL не протекает в следующее использование соединения
	if got := show(t, f.pool, "synchronous_commit"); got != "on" {
		t.Errorf("synchronous_commit после транзакции = %q", got)
	}
	// отменённый контекст — ошибка begin, не паника
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := pg.WithTx(cctx, f.pool, func(context.Context, pgx.Tx) error { return nil }); err == nil {
		t.Fatal("begin с отменённым контекстом")
	}
}

func TestErrorHelpers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `INSERT INTO t_items (id, v, name) VALUES (1, 1, 'Пожар')`); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string) error {
		_, err := f.pool.Exec(ctx, sql)
		return err
	}
	dup := exec(`INSERT INTO t_items (id, v, name) VALUES (2, 1, 'Пожар')`)
	if !pg.IsUniqueViolation(dup) || pg.ConstraintName(dup) != "t_items_name_key" {
		t.Errorf("unique: %v / %q", dup, pg.ConstraintName(dup))
	}
	fk := exec(`INSERT INTO t_items (id, v, ref) VALUES (3, 1, 999)`)
	if !pg.IsForeignKeyViolation(fk) || pg.IsUniqueViolation(fk) {
		t.Errorf("fk: %v", fk)
	}
	chk := exec(`INSERT INTO t_items (id, v) VALUES (4, -1)`)
	if !pg.IsCheckViolation(chk) {
		t.Errorf("check: %v", chk)
	}
	var n int
	noRows := f.pool.QueryRow(ctx, `SELECT v FROM t_items WHERE id = 404`).Scan(&n)
	if !pg.IsNoRows(noRows) || pg.IsNoRows(dup) {
		t.Errorf("no rows: %v", noRows)
	}
	plain := errors.New("x")
	if _, ok := pg.PgError(plain); ok || pg.ConstraintName(plain) != "" || pg.IsUniqueViolation(nil) {
		t.Error("не-pg ошибка распознана как pg")
	}
	if pe, ok := pg.PgError(errors.Join(plain, chk)); !ok || pe.Code != pg.CodeCheckViolation {
		t.Error("обёрнутая pg-ошибка не распознана")
	}
}

// Хуки выполняются в горутине вызвавшего — параллельные транзакции не путают свои хуки.
func TestOnCommitIsolation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[int]int{}
	for i := 1; i <= 8; i++ {
		wg.Go(func() {
			_ = pg.WithTx(context.Background(), f.pool, func(ctx context.Context, tx pgx.Tx) error {
				pg.OnCommit(ctx, func() { mu.Lock(); got[i]++; mu.Unlock() })
				_, err := tx.Exec(ctx, `INSERT INTO t_items (id, v) VALUES ($1, 0)`, i)
				return err
			})
		})
	}
	wg.Wait()
	for i := 1; i <= 8; i++ {
		if got[i] != 1 {
			t.Fatalf("хуки перепутаны: %v", got)
		}
	}
}
