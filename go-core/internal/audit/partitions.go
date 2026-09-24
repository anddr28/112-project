package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/platform/pg"
)

// MinRetentionDays — нижняя граница хранения аудита (ТЗ: журналы не менее 6 месяцев).
// Даже если в настройку попадёт меньшее значение, партиции моложе не удаляются.
const MinRetentionDays = 180

// monthsAhead — сколько будущих месяцев держать созданными (кроме текущего): задел на
// случай, если сервис долго не запускался и ежедневный job не отработал.
const monthsAhead = 3

var partName = regexp.MustCompile(`^audit_log_(\d{4})_(\d{2})$`)

// monthPart — нужная помесячная партиция: имя и «естественные» границы месяца в UTC.
type monthPart struct {
	name   string    // audit_log_YYYY_MM
	lo, hi time.Time // полночь UTC первого числа месяца и следующего
}

// partInfo — существующая партиция audit_log и её границы (нулевые — не FROM/TO, т.е. DEFAULT).
type partInfo struct {
	name   string
	lo, hi time.Time
}

// EnsurePartitions — помесячные партиции audit_log (db-design Р11): текущий месяц и
// monthsAhead следующих; удаление партиций, весь диапазон которых старше retentionDays;
// предупреждение, если в audit_log_default появились строки. Идемпотентна.
// Лог — slog.Default(); с явным логгером — EnsurePartitionsLog.
func EnsurePartitions(ctx context.Context, pool *pgxpool.Pool, retentionDays int) error {
	return EnsurePartitionsLog(ctx, pool, retentionDays, slog.Default())
}

// EnsurePartitionsLog — EnsurePartitions с логгером. Ошибки отдельных партиций не прерывают
// остальные шаги и возвращаются вместе (errors.Join).
//
// Границы новой партиции стыкуются с соседями, а не считаются заново: миграция 00001
// создала первые 12 партиций в часовом поясе сервера PostgreSQL (в compose — Europe/Moscow,
// границы «00:00+03»), а пул go-core работает в UTC. Партиция «с полуночи UTC» оставила бы
// щель в 3 часа между месяцами (строки уходили бы в DEFAULT навсегда, алерт горел бы
// постоянно), а при отрицательном смещении — перекрытие и отказ CREATE каждый день.
// Поэтому: нижняя граница = верхней границе предыдущего месяца, верхняя = нижней границе
// следующего; соседей нет — полночь UTC со смещением, как у последней существующей партиции.
func EnsurePartitionsLog(ctx context.Context, pool *pgxpool.Pool, retentionDays int, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	if retentionDays < MinRetentionDays {
		retentionDays = MinRetentionDays
	}
	months, err := wantedMonths(ctx, pool)
	if err != nil {
		return err
	}
	existing, err := existingPartitions(ctx, pool)
	if err != nil {
		return err
	}
	byName := make(map[string]partInfo, len(existing)+len(months))
	for _, p := range existing {
		byName[p.name] = p
	}
	off := schemeOffset(existing)

	var errs []error
	for _, m := range months {
		if _, ok := byName[m.name]; ok {
			continue
		}
		lo, hi := m.lo.Add(off), m.hi.Add(off)
		if prev, ok := byName[monthName(m.lo.AddDate(0, -1, 0))]; ok && !prev.hi.IsZero() {
			lo = prev.hi
		}
		if next, ok := byName[monthName(m.lo.AddDate(0, 1, 0))]; ok && !next.lo.IsZero() {
			hi = next.lo
		}
		if !lo.Before(hi) {
			errs = append(errs, fmt.Errorf("audit: create %s: пустой диапазон %s .. %s", m.name, lo, hi))
			continue
		}
		if err := createPartition(ctx, pool, m.name, lo, hi, log); err != nil {
			errs = append(errs, err)
			continue
		}
		byName[m.name] = partInfo{name: m.name, lo: lo, hi: hi}
		log.Info("создана партиция аудита", "partition", m.name, "from", lo, "to", hi)
	}

	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays)
	for _, p := range existing {
		hi, ok := partitionUpper(p.name)
		if !ok {
			continue // audit_log_default и чужие имена не трогаем
		}
		if !p.hi.IsZero() {
			hi = p.hi
		}
		if hi.After(cutoff) {
			continue
		}
		// DROP партиции — мгновенный ретеншн без vacuum-долга (Р11). Имя проверено регэкспом.
		if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS "+pgx.Identifier{p.name}.Sanitize(), pgx.QueryExecModeSimpleProtocol); err != nil {
			errs = append(errs, fmt.Errorf("audit: drop %s: %w", p.name, err))
			continue
		}
		log.Info("удалена устаревшая партиция аудита", "partition", p.name, "retention_days", retentionDays)
	}

	var hasDefault bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_log_default)`).Scan(&hasDefault); err != nil {
		errs = append(errs, fmt.Errorf("audit: check default partition: %w", err))
	} else if hasDefault {
		log.Warn("в audit_log_default есть строки: для их периода нет помесячной партиции — ретеншн по DROP к ним не применяется")
	}
	return errors.Join(errs...)
}

// wantedMonths — текущий месяц и monthsAhead следующих (по часам сервера БД, в UTC).
func wantedMonths(ctx context.Context, pool *pgxpool.Pool) ([]monthPart, error) {
	var now time.Time
	if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return nil, fmt.Errorf("audit: months: %w", err)
	}
	return monthsFrom(now, monthsAhead), nil
}

// monthsFrom — месяц now (UTC) и ahead следующих.
func monthsFrom(now time.Time, ahead int) []monthPart {
	now = now.UTC()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	out := make([]monthPart, 0, ahead+1)
	for i := 0; i <= ahead; i++ {
		lo := first.AddDate(0, i, 0)
		out = append(out, monthPart{name: monthName(lo), lo: lo, hi: lo.AddDate(0, 1, 0)})
	}
	return out
}

func monthName(t time.Time) string { return t.UTC().Format("audit_log_2006_01") }

// schemeOffset — смещение границ существующих помесячных партиций от полуночи UTC (по
// самой поздней из них): -3 ч для партиций миграции, созданных в Europe/Moscow; 0 — UTC.
func schemeOffset(parts []partInfo) time.Duration {
	var best partInfo
	for _, p := range parts {
		if p.lo.IsZero() || !partName.MatchString(p.name) {
			continue
		}
		if best.name == "" || p.name > best.name {
			best = p
		}
	}
	if best.name == "" {
		return 0
	}
	hi, _ := partitionUpper(best.name)
	off := best.lo.Sub(hi.AddDate(0, -1, 0))
	if off <= -24*time.Hour || off >= 24*time.Hour {
		return 0 // не похоже на «полночь в каком-то поясе» — не тиражируем странность
	}
	return off
}

// sqlPartitions — партиции audit_log с границами. pg_get_expr печатает литералы границ в
// поясе сессии со смещением ('2026-08-31 21:00:00+00'), приведение к timestamptz их читает;
// у DEFAULT (и MINVALUE/MAXVALUE) regexp_match даёт NULL.
const sqlPartitions = `
SELECT c.relname::text,
       (regexp_match(pg_get_expr(c.relpartbound, c.oid), 'FROM \(''([^'']+)''\)'))[1]::timestamptz,
       (regexp_match(pg_get_expr(c.relpartbound, c.oid), 'TO \(''([^'']+)''\)'))[1]::timestamptz
  FROM pg_inherits i
  JOIN pg_class c ON c.oid = i.inhrelid
 WHERE i.inhparent = 'audit_log'::regclass
 ORDER BY 1`

func existingPartitions(ctx context.Context, pool *pgxpool.Pool) ([]partInfo, error) {
	rows, err := pool.Query(ctx, sqlPartitions)
	if err != nil {
		return nil, fmt.Errorf("audit: list partitions: %w", err)
	}
	defer rows.Close()
	out := make([]partInfo, 0, 16)
	for rows.Next() {
		var p partInfo
		var lo, hi *time.Time
		if err := rows.Scan(&p.name, &lo, &hi); err != nil {
			return nil, fmt.Errorf("audit: list partitions: %w", err)
		}
		if lo != nil && hi != nil {
			p.lo, p.hi = lo.UTC(), hi.UTC()
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: list partitions: %w", err)
	}
	return out, nil
}

// partitionUpper — верхняя граница (исключительная) партиции по суффиксу имени (полночь UTC);
// false — не помесячная партиция (audit_log_default и чужие имена не трогаем).
func partitionUpper(name string) (time.Time, bool) {
	m := partName.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	y, _ := strconv.Atoi(m[1])
	mo, _ := strconv.Atoi(m[2])
	if mo < 1 || mo > 12 {
		return time.Time{}, false
	}
	return time.Date(y, time.Month(mo)+1, 1, 0, 0, 0, 0, time.UTC), true
}

// boundLiteral — граница партиции для DDL (параметры в DDL не передаются): UTC с микросекундами.
func boundLiteral(t time.Time) string {
	return "'" + t.UTC().Format("2006-01-02 15:04:05.999999") + "+00'"
}

// createPartition создаёт партицию [lo, hi). Если строки этого периода уже осели в DEFAULT
// (job долго не работал), PostgreSQL откажет (23514) — тогда в одной транзакции переносим их:
// DELETE из default → временная таблица → CREATE PARTITION → INSERT обратно (с теми же id).
func createPartition(ctx context.Context, pool *pgxpool.Pool, name string, lo, hi time.Time, log *slog.Logger) error {
	ident := pgx.Identifier{name}.Sanitize()
	bounds := " PARTITION OF audit_log FOR VALUES FROM (" + boundLiteral(lo) + ") TO (" + boundLiteral(hi) + ")"
	_, err := pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+ident+bounds, pgx.QueryExecModeSimpleProtocol)
	if err == nil {
		return nil
	}
	if pe, ok := pg.PgError(err); !ok || pe.Code != pg.CodeCheckViolation {
		return fmt.Errorf("audit: create %s: %w", name, err)
	}
	var moved int64
	err = pg.WithTx(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE audit_log_move (LIKE audit_log) ON COMMIT DROP`, pgx.QueryExecModeSimpleProtocol); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			WITH d AS (DELETE FROM audit_log_default WHERE at >= $1::timestamptz AND at < $2::timestamptz RETURNING *)
			INSERT INTO audit_log_move SELECT * FROM d`, pgx.QueryExecModeSimpleProtocol, lo, hi)
		if err != nil {
			return err
		}
		moved = tag.RowsAffected()
		if _, err := tx.Exec(ctx, "CREATE TABLE "+ident+bounds, pgx.QueryExecModeSimpleProtocol); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO audit_log OVERRIDING SYSTEM VALUE SELECT * FROM audit_log_move`, pgx.QueryExecModeSimpleProtocol)
		return err
	})
	if err != nil {
		return fmt.Errorf("audit: create %s with move from default: %w", name, err)
	}
	log.Warn("строки аудита перенесены из audit_log_default в новую партицию", "partition", name, "rows", moved)
	return nil
}
