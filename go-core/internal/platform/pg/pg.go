// Package pg — доступ к PostgreSQL: пул pgx, транзакции с post-commit хуками, разбор ошибок.
//
// Правила (docs/db-design.md):
//   - клиент БД один — go-core; pgxpool уже пул, PgBouncer не нужен;
//   - prepared statements кэшируются pgx автоматически (QueryExecModeCacheStatement);
//   - побочные эффекты «наружу» (WebSocket, Kick диспетчера) — ТОЛЬКО после COMMIT,
//     через OnCommit: иначе клиент увидит событие, которого в БД нет (откат).
package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier — общее у *pgxpool.Pool и pgx.Tx. Репозитории принимают его, чтобы одна и та же
// функция работала и в транзакции, и вне её.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
	CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error)
}

var (
	_ Querier = (*pgxpool.Pool)(nil)
	_ Querier = (pgx.Tx)(nil)
)

// sessionTimeouts — страховочные таймауты сессии PostgreSQL (мс) для соединений пула.
// Зависшая транзакция (клиент ушёл, горутина встала на чём-то вне БД) иначе держит
// соединение и блокировки строк бесконечно: PostgreSQL сам их не освобождает.
// Значения с запасом над самыми длинными штатными операциями ядра (отчёт, ретеншн батчами,
// импорт); миграции (goose) и pg_dump ходят своими соединениями и под них не попадают.
// Переопределяются параметрами DATABASE_URL (…?statement_timeout=300000), 0 — выключить.
var sessionTimeouts = map[string]string{
	"idle_in_transaction_session_timeout": "60000", // транзакция простаивает > 60 с — сессия рвётся, блокировки снимаются
	"lock_timeout":                        "15000", // ждать блокировку строки > 15 с — ошибка 55P03, а не очередь навсегда
	"statement_timeout":                   "60000", // один запрос > 60 с — отмена
}

// Connect открывает пул. maxConns ~ 20–30 хватает на тысячи горутин (см. §4 db-design).
func Connect(ctx context.Context, url string, maxConns int32, appName string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("pg: parse config: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	cfg.MinConns = min(2, cfg.MaxConns)
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.MaxConnLifetime = time.Hour
	cfg.HealthCheckPeriod = 30 * time.Second
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = appName
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	for k, v := range sessionTimeouts {
		if _, set := cfg.ConnConfig.RuntimeParams[k]; !set { // из DATABASE_URL — приоритетнее
			cfg.ConnConfig.RuntimeParams[k] = v
		}
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pg: connect: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pg: ping: %w", err)
	}
	return pool, nil
}

type hooksKey struct{}

type hooks struct{ fns []func() }

// OnCommit регистрирует действие, которое выполнится после успешного COMMIT транзакции,
// открытой WithTx (ctx должен быть тем, что WithTx передал в fn). Вне транзакции — сразу.
// Хуки выполняются синхронно в горутине вызвавшего, по порядку регистрации; они должны
// быть быстрыми и неблокирующими (Publish в хаб, Kick диспетчера).
func OnCommit(ctx context.Context, fn func()) {
	if h, ok := ctx.Value(hooksKey{}).(*hooks); ok && h != nil {
		h.fns = append(h.fns, fn)
		return
	}
	fn()
}

// InTx сообщает, открыта ли транзакция WithTx в этом контексте.
func InTx(ctx context.Context) bool {
	h, ok := ctx.Value(hooksKey{}).(*hooks)
	return ok && h != nil
}

// TxOptions — параметры WithTxOpts.
type TxOptions struct {
	// AsyncCommit — SET LOCAL synchronous_commit = off. Только для телеметрии
	// (attempt_events): при крахе сервера теряются сотни миллисекунд событий (db-design §3).
	AsyncCommit bool
	// ReadOnly — read only транзакция (консистентный снимок для отчётов).
	ReadOnly bool
}

// WithTx выполняет fn в транзакции READ COMMITTED. Ошибка fn или паника — ROLLBACK.
// После COMMIT выполняются хуки OnCommit.
func WithTx(ctx context.Context, pool *pgxpool.Pool, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return WithTxOpts(ctx, pool, TxOptions{}, fn)
}

func WithTxOpts(ctx context.Context, pool *pgxpool.Pool, opts TxOptions, fn func(ctx context.Context, tx pgx.Tx) error) (err error) {
	txo := pgx.TxOptions{}
	if opts.ReadOnly {
		txo.AccessMode = pgx.ReadOnly
	}
	tx, err := pool.BeginTx(ctx, txo)
	if err != nil {
		return fmt.Errorf("pg: begin: %w", err)
	}
	h := &hooks{}
	txCtx := context.WithValue(ctx, hooksKey{}, h)
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if opts.AsyncCommit {
		if _, err = tx.Exec(txCtx, "SET LOCAL synchronous_commit = off"); err != nil {
			return fmt.Errorf("pg: set async commit: %w", err)
		}
	}
	if err = fn(txCtx, tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("pg: commit: %w", err)
	}
	for _, f := range h.fns {
		f()
	}
	return nil
}

// ---------------------------------------------------------------- ошибки

// Коды SQLSTATE, которые разбирает приложение.
const (
	CodeUniqueViolation     = "23505"
	CodeForeignKeyViolation = "23503"
	CodeCheckViolation      = "23514"
	CodeNotNullViolation    = "23502"
	CodeSerialization       = "40001"
	CodeDeadlock            = "40P01"
	CodeLockNotAvailable    = "55P03"
)

// IsNoRows — строка не найдена (QueryRow.Scan).
func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// PgError достаёт *pgconn.PgError.
func PgError(err error) (*pgconn.PgError, bool) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}

func hasCode(err error, code string) bool {
	pe, ok := PgError(err)
	return ok && pe.Code == code
}

func IsUniqueViolation(err error) bool     { return hasCode(err, CodeUniqueViolation) }
func IsForeignKeyViolation(err error) bool { return hasCode(err, CodeForeignKeyViolation) }
func IsCheckViolation(err error) bool      { return hasCode(err, CodeCheckViolation) }

// ConstraintName — имя нарушенного ограничения ("" если это не ошибка ограничения).
func ConstraintName(err error) string {
	if pe, ok := PgError(err); ok {
		return pe.ConstraintName
	}
	return ""
}
