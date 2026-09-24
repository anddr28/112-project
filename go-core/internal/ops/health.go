package ops

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/platform/metrics"
)

var (
	bodyOK       = []byte(`{"status":"ok"}`)
	bodyNotReady = []byte(`{"status":"unavailable","db":"down"}`)
)

func writeHealth(w http.ResponseWriter, status int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// Healthz — живость процесса (liveness): 200, пока сервер отвечает. БД не трогает —
// иначе оркестратор перезапускал бы здоровое ядро при кратком сбое PostgreSQL.
func (o *Ops) Healthz() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeHealth(w, http.StatusOK, bodyOK)
	})
}

// Readyz — готовность (readiness): ping БД с таймаутом 1 с → 200 / 503.
func (o *Ops) Readyz() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := o.pool.Ping(ctx); err != nil {
			writeHealth(w, http.StatusServiceUnavailable, bodyNotReady)
			return
		}
		writeHealth(w, http.StatusOK, bodyOK)
	})
}

// ================================================================ метрики из БД

const (
	dbMetricsTTL     = 10 * time.Second // скрейп раз в 15 с попадает в кэш через раз — не чаще 1 запроса/10 с
	dbMetricsTimeout = 2 * time.Second
)

type jobCount struct {
	typ, status string
	n           float64
}

type sizeRow struct {
	name  string
	bytes float64
}

type dbSnapshot struct {
	ok            bool
	jobs          []jobCount
	oldestQueued  float64
	evalPending   float64
	evalPartial   float64
	evalLag       float64
	lastBackup    float64 // unix; 0 — не было
	backupRunning float64
	tables        []sizeRow
	auditParts    []sizeRow
	databaseBytes float64
	// 1 — в audit_log_default есть строки
	auditDefaultRows float64
}

// dbCollector — агрегаты из БД для /metrics (db-design §7: очередь ai_jobs, лаг оценок,
// размеры партиций, свежесть бэкапа). Один round-trip (pgx.Batch) не чаще раза в dbMetricsTTL;
// все запросы — по частичным индексам или каталогу, без сканирования больших таблиц.
type dbCollector struct {
	o    *Ops
	mu   sync.Mutex
	at   time.Time
	snap dbSnapshot
}

// RegisterMetrics — зарегистрировать метрики ops в /metrics (lct_ai_jobs, lct_evaluation_lag_seconds,
// lct_backup_last_success_timestamp_seconds, lct_table_bytes, lct_audit_partition_bytes, …).
func (o *Ops) RegisterMetrics() {
	c := &dbCollector{o: o}
	metrics.RegisterCollector("lct_ops_db", c.emit)
}

func (c *dbCollector) emit(e *metrics.Emitter) {
	c.mu.Lock()
	if time.Since(c.at) >= dbMetricsTTL {
		c.snap = c.o.readDBSnapshot()
		c.at = time.Now()
	}
	s := c.snap
	c.mu.Unlock()

	up := 0.0
	if s.ok {
		up = 1
	}
	e.Gauge("lct_db_metrics_up", "Сбор метрик из БД удался (1) или нет (0).", up)
	if !s.ok {
		return
	}
	e.Header("lct_ai_jobs", "gauge", "AI-задачи в очереди и в работе по типу и статусу.")
	for _, j := range s.jobs {
		e.Sample("lct_ai_jobs", j.n, "type", j.typ, "status", j.status)
	}
	e.Gauge("lct_ai_jobs_oldest_queued_seconds", "Возраст самой старой задачи в очереди, секунды.", s.oldestQueued)
	e.Header("lct_evaluations_open", "gauge", "Незавершённые оценки по статусу.")
	e.Sample("lct_evaluations_open", s.evalPending, "status", "pending")
	e.Sample("lct_evaluations_open", s.evalPartial, "status", "partial")
	e.Gauge("lct_evaluation_lag_seconds", "Возраст самой старой незавершённой оценки (pending/partial), секунды.", s.evalLag)
	e.Gauge("lct_backup_last_success_timestamp_seconds", "Время окончания последней успешной резервной копии (unix), 0 — не было.", s.lastBackup)
	e.Gauge("lct_backup_running", "Идёт резервное копирование (1/0).", s.backupRunning)
	e.Gauge("lct_database_bytes", "Размер базы данных, байты.", s.databaseBytes)
	e.Header("lct_table_bytes", "gauge", "Размер таблицы с индексами и TOAST, байты.")
	for _, t := range s.tables {
		e.Sample("lct_table_bytes", t.bytes, "table", t.name)
	}
	e.Gauge("lct_audit_default_partition_has_rows", "В audit_log_default есть строки (1) — нет помесячной партиции для их периода (алерт админу, Р11).", s.auditDefaultRows)
	e.Header("lct_audit_partition_bytes", "gauge", "Размер партиции журнала аудита, байты.")
	for _, p := range s.auditParts {
		e.Sample("lct_audit_partition_bytes", p.bytes, "partition", p.name)
	}
}

func (o *Ops) readDBSnapshot() dbSnapshot {
	ctx, cancel := context.WithTimeout(context.Background(), dbMetricsTimeout)
	defer cancel()
	var s dbSnapshot
	b := o.pool.SendBatch(ctx, newDBMetricsBatch())
	defer b.Close()

	rows, err := b.Query()
	if err != nil {
		return dbSnapshot{}
	}
	for rows.Next() {
		var j jobCount
		var n int64
		if err := rows.Scan(&j.typ, &j.status, &n); err != nil {
			rows.Close()
			return dbSnapshot{}
		}
		j.n = float64(n)
		s.jobs = append(s.jobs, j)
	}
	rows.Close()
	if rows.Err() != nil {
		return dbSnapshot{}
	}
	var pend, part int64
	var lastBackup *time.Time
	var running, auditDefault bool
	if err := b.QueryRow().Scan(&s.oldestQueued, &pend, &part, &s.evalLag, &lastBackup, &running, &s.databaseBytes, &auditDefault); err != nil {
		return dbSnapshot{}
	}
	if auditDefault {
		s.auditDefaultRows = 1
	}
	s.evalPending, s.evalPartial = float64(pend), float64(part)
	if lastBackup != nil {
		s.lastBackup = float64(lastBackup.Unix())
	}
	if running {
		s.backupRunning = 1
	}
	if s.tables, err = scanSizes(b); err != nil {
		return dbSnapshot{}
	}
	if s.auditParts, err = scanSizes(b); err != nil {
		return dbSnapshot{}
	}
	s.ok = true
	return s
}

// Таблицы, размер которых стоит видеть в мониторинге (растущие журналы и горячие таблицы).
var watchedTables = []string{"attempt_events", "attempt_drafts", "attempts", "evaluations",
	"attempt_dialogue_turns", "ai_jobs", "auth_sessions", "tts_cache", "xp_ledger"}

// sqlMetricsJobs — задачи в очереди и в работе. Статусы через OR, а не IN: так планировщик
// берёт BitmapOr по двум частичным индексам (ai_jobs_queue_idx, ai_jobs_running_idx);
// IN ('queued','running') ни одному из них не соответствует и читает всю таблицу, которая
// растёт вместе с ретеншном выполненных задач (см. aijobs/sql.go).
const sqlMetricsJobs = `SELECT type, status, count(*) FROM ai_jobs WHERE status = 'queued' OR status = 'running'
          GROUP BY type, status ORDER BY type, status`

func newDBMetricsBatch() *pgx.Batch {
	b := &pgx.Batch{}
	b.Queue(sqlMetricsJobs)
	b.Queue(`SELECT COALESCE(EXTRACT(EPOCH FROM now() - (SELECT min(created_at) FROM ai_jobs WHERE status = 'queued')), 0)::float8,
	                (SELECT count(*) FROM evaluations WHERE status = 'pending'),
	                (SELECT count(*) FROM evaluations WHERE status = 'partial'),
	                COALESCE(EXTRACT(EPOCH FROM now() - (SELECT min(created_at) FROM evaluations
	                                                      WHERE status IN ('pending', 'partial'))), 0)::float8,
	                (SELECT max(finished_at) FROM backups WHERE status = 'done'),
	                EXISTS (SELECT 1 FROM backups WHERE status = 'running'),
	                pg_database_size(current_database())::float8,
	                EXISTS (SELECT 1 FROM audit_log_default)`)
	b.Queue(`SELECT c.relname::text, pg_total_relation_size(c.oid)::float8
	           FROM pg_class c
	          WHERE c.relname = ANY($1::text[]) AND c.relkind = 'r' AND c.relnamespace = 'public'::regnamespace
	          ORDER BY 1`, watchedTables)
	b.Queue(`SELECT c.relname::text, pg_total_relation_size(c.oid)::float8
	           FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
	          WHERE i.inhparent = 'audit_log'::regclass
	          ORDER BY 1`)
	return b
}

func scanSizes(b pgx.BatchResults) ([]sizeRow, error) {
	rows, err := b.Query()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]sizeRow, 0, 16)
	for rows.Next() {
		var r sizeRow
		if err := rows.Scan(&r.name, &r.bytes); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
