package ops

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/platform/metrics"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/platform/pgtest"
)

func TestHealthz(t *testing.T) {
	t.Parallel()
	o := New(Deps{Log: discardLog()}) // живость не трогает БД
	rec := httptest.NewRecorder()
	o.Healthz().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"status":"ok"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	h := rec.Header()
	if h.Get("Content-Type") != "application/json; charset=utf-8" || h.Get("Cache-Control") != "no-store" || h.Get("Content-Length") != "15" {
		t.Fatalf("headers = %v", h)
	}
}

func TestReadyz(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	o := New(Deps{Pool: pool, Log: discardLog()})
	rec := httptest.NewRecorder()
	o.Readyz().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"status":"ok"}` {
		t.Fatalf("up: %d %s", rec.Code, rec.Body)
	}

	// БД недоступна — 503
	down := pgtest.New(t)
	down.Close()
	o2 := New(Deps{Pool: down, Log: discardLog()})
	rec = httptest.NewRecorder()
	o2.Readyz().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != `{"status":"unavailable","db":"down"}` {
		t.Fatalf("down: %d %s", rec.Code, rec.Body)
	}
	// метрики из недоступной БД — lct_db_metrics_up 0, без паники
	if snap := o2.readDBSnapshot(); snap.ok {
		t.Fatal("snapshot из закрытого пула")
	}
}

func TestReadDBSnapshot(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	e.exec(t, `INSERT INTO ai_jobs (type, status, payload, created_at) VALUES
	            ('tts', 'queued', '{}', now() - interval '90 seconds'),
	            ('tts', 'queued', '{}', now()),
	            ('evaluate_grammar', 'running', '{}', now()),
	            ('evaluate_grammar', 'done', '{}', now()),
	            ('evaluate_semantic', 'failed', '{}', now())`)
	e.exec(t, `INSERT INTO backups (started_at, finished_at, status) VALUES (now() - interval '1 hour', now() - interval '50 minutes', 'done'),
	            (now(), NULL, 'running')`)

	s := e.o.readDBSnapshot()
	if !s.ok {
		t.Fatal("snapshot не собран")
	}
	var jobs []string
	for _, j := range s.jobs {
		jobs = append(jobs, j.typ+"/"+j.status+"="+strconv.FormatFloat(j.n, 'f', -1, 64))
	}
	if strings.Join(jobs, ",") != "evaluate_grammar/running=1,tts/queued=2" {
		t.Fatalf("jobs = %v", jobs)
	}
	if s.oldestQueued < 85 || s.oldestQueued > 600 {
		t.Errorf("oldestQueued = %v", s.oldestQueued)
	}
	if s.lastBackup == 0 || s.backupRunning != 1 || s.databaseBytes <= 0 || s.auditDefaultRows != 0 {
		t.Errorf("snapshot = %+v", s)
	}
	var tables []string
	for _, r := range s.tables {
		tables = append(tables, r.name)
	}
	if !slices.Equal(tables, slices.Sorted(slices.Values(watchedTables))) {
		t.Errorf("tables = %v", tables)
	}
	var parts []string
	for _, r := range s.auditParts {
		parts = append(parts, r.name)
	}
	if !slices.Contains(parts, "audit_log_default") || len(parts) < 2 {
		t.Errorf("audit parts = %v", parts)
	}
}

// Находка ревью: счётчик задач через IN ('queued','running') не попадал ни в один из двух
// частичных индексов и читал всю ai_jobs каждые 10 с. С OR — BitmapOr по индексам.
func TestMetricsJobsQueryUsesPartialIndexes(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	var plan string
	err := pg.WithTx(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, "EXPLAIN "+sqlMetricsJobs)
		if err != nil {
			return err
		}
		defer rows.Close()
		var sb strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				return err
			}
			sb.WriteString(line + "\n")
		}
		plan = sb.String()
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan, "Seq Scan") || !strings.Contains(plan, "ai_jobs_queue_idx") || !strings.Contains(plan, "ai_jobs_running_idx") {
		t.Fatalf("план не по частичным индексам:\n%s", plan)
	}
}

func TestRegisterMetricsEmits(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	e.exec(t, `INSERT INTO ai_jobs (type, status, payload) VALUES ('tts', 'queued', '{}')`)
	e.o.RegisterMetrics()
	var buf bytes.Buffer
	if err := metrics.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"lct_db_metrics_up 1",
		`lct_ai_jobs{type="tts",status="queued"} 1`,
		`lct_evaluations_open{status="pending"} 0`,
		"lct_backup_last_success_timestamp_seconds 0",
		"# TYPE lct_table_bytes gauge",
		`lct_audit_partition_bytes{partition="audit_log_default"}`,
		"lct_audit_default_partition_has_rows 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("нет %q в /metrics", want)
		}
	}
}
