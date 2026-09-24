package audit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/platform/pgtest"
)

func dbNow(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	if err := pool.QueryRow(ctxT(t), `SELECT now()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now.UTC()
}

func partsByName(t *testing.T, pool *pgxpool.Pool) map[string]partInfo {
	t.Helper()
	list, err := existingPartitions(ctxT(t), pool)
	if err != nil {
		t.Fatal(err)
	}
	m := make(map[string]partInfo, len(list))
	for _, p := range list {
		m[p.name] = p
	}
	return m
}

// dropMonthlyPartitions — в свежей тестовой БД аудит пуст: помесячные партиции можно
// снести, чтобы разыграть нужную схему границ.
func dropMonthlyPartitions(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for name := range partsByName(t, pool) {
		if partName.MatchString(name) {
			if _, err := pool.Exec(ctxT(t), "DROP TABLE "+pgx.Identifier{name}.Sanitize()); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func createPart(t *testing.T, pool *pgxpool.Pool, name string, lo, hi time.Time) {
	t.Helper()
	sql := "CREATE TABLE " + pgx.Identifier{name}.Sanitize() + " PARTITION OF audit_log FOR VALUES FROM (" +
		boundLiteral(lo) + ") TO (" + boundLiteral(hi) + ")"
	if _, err := pool.Exec(ctxT(t), sql); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
}

func partitionOf(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var rel string
	if err := pool.QueryRow(ctxT(t), `SELECT tableoid::regclass::text FROM audit_log WHERE id = $1`, id).Scan(&rel); err != nil {
		t.Fatal(err)
	}
	return rel
}

func insertAt(t *testing.T, pool *pgxpool.Pool, at time.Time) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctxT(t), `INSERT INTO audit_log (at, action) VALUES ($1, 'test.row') RETURNING id`, at).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestEnsurePartitionsIdempotent(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	for range 2 {
		if err := EnsurePartitionsLog(ctxT(t), pool, 365, discardLog()); err != nil {
			t.Fatal(err)
		}
	}
	parts := partsByName(t, pool)
	for _, m := range monthsFrom(dbNow(t, pool), monthsAhead) {
		p, ok := parts[m.name]
		if !ok {
			t.Fatalf("нет партиции %s", m.name)
		}
		if p.hi.IsZero() {
			t.Fatalf("%s без границ", m.name)
		}
	}
	if _, ok := parts["audit_log_default"]; !ok {
		t.Fatal("DEFAULT-партиция пропала")
	}
	// EnsurePartitions — та же функция с логгером по умолчанию
	if err := EnsurePartitions(ctxT(t), pool, 365); err != nil {
		t.Fatal(err)
	}
}

// Регрессия: миграция 00001 создаёт партиции в поясе сервера PostgreSQL (compose:
// Europe/Moscow, границы 00:00+03), а раньше job создавал следующие с полуночи UTC —
// щель в 3 часа на стыке, строки уходили в DEFAULT навсегда.
func TestEnsurePartitionsChainsWithServerTimezoneBounds(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	dropMonthlyPartitions(t, pool)

	msk := time.FixedZone("MSK", 3*3600)
	months := monthsFrom(dbNow(t, pool), monthsAhead)
	mskMonth := func(m monthPart) (time.Time, time.Time) {
		lo := time.Date(m.lo.Year(), m.lo.Month(), 1, 0, 0, 0, 0, msk)
		return lo, lo.AddDate(0, 1, 0)
	}
	lo0, hi0 := mskMonth(months[0])
	createPart(t, pool, months[0].name, lo0, hi0)
	lo2, hi2 := mskMonth(months[2])
	createPart(t, pool, months[2].name, lo2, hi2)

	if err := EnsurePartitionsLog(ctxT(t), pool, 365, discardLog()); err != nil {
		t.Fatal(err)
	}
	parts := partsByName(t, pool)
	for i := 1; i < len(months); i++ {
		prev, cur := parts[months[i-1].name], parts[months[i].name]
		if cur.hi.IsZero() {
			t.Fatalf("нет партиции %s", months[i].name)
		}
		if !cur.lo.Equal(prev.hi) {
			t.Fatalf("щель/перекрытие: %s начинается %v, %s кончается %v", months[i].name, cur.lo, months[i-1].name, prev.hi)
		}
	}
	// созданные job'ом партиции — в той же схеме: полночь MSK
	if want, _ := mskMonth(months[3]); !parts[months[3].name].lo.Equal(want) {
		t.Fatalf("%s: lo=%v want %v", months[3].name, parts[months[3].name].lo, want)
	}
	if _, want := mskMonth(months[3]); !parts[months[3].name].hi.Equal(want) {
		t.Fatalf("%s: hi=%v want %v", months[3].name, parts[months[3].name].hi, want)
	}

	// строки на стыках месяцев — в помесячных партициях, не в DEFAULT
	for i := 1; i < len(months); i++ {
		at := parts[months[i].name].lo
		for _, x := range []time.Time{at.Add(-time.Microsecond), at, at.Add(time.Hour), at.Add(2*time.Hour + 59*time.Minute)} {
			if rel := partitionOf(t, pool, insertAt(t, pool, x)); rel == "audit_log_default" {
				t.Fatalf("строка %v ушла в DEFAULT", x)
			}
		}
	}
	var def bool
	if err := pool.QueryRow(ctxT(t), `SELECT EXISTS (SELECT 1 FROM audit_log_default)`).Scan(&def); err != nil || def {
		t.Fatalf("DEFAULT не пуста: %v %v", def, err)
	}
}

// Строки периода без партиции осели в DEFAULT — при создании партиции они переносятся
// (с теми же id), DEFAULT пустеет.
func TestEnsurePartitionsMovesRowsFromDefault(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	months := monthsFrom(dbNow(t, pool), monthsAhead)
	target := months[2]
	if _, err := pool.Exec(ctxT(t), "DROP TABLE IF EXISTS "+pgx.Identifier{target.name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	id := insertAt(t, pool, target.lo.Add(36*time.Hour))
	if rel := partitionOf(t, pool, id); rel != "audit_log_default" {
		t.Fatalf("строка до создания партиции в %s", rel)
	}
	if err := EnsurePartitionsLog(ctxT(t), pool, 365, discardLog()); err != nil {
		t.Fatal(err)
	}
	if rel := partitionOf(t, pool, id); rel != target.name {
		t.Fatalf("строка в %s, want %s", rel, target.name)
	}
	var n int
	if err := pool.QueryRow(ctxT(t), `SELECT count(*) FROM audit_log WHERE id = $1`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("строк с id %d: %d (%v)", id, n, err)
	}
}

func TestEnsurePartitionsRetention(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	now := dbNow(t, pool)
	cur := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	old := cur.AddDate(-3, 0, 0) // давно за пределами хранения
	oldName := monthName(old)
	createPart(t, pool, oldName, old, old.AddDate(0, 1, 0))
	insertAt(t, pool, old.Add(time.Hour))

	recent := cur.AddDate(0, -2, 0) // моложе MinRetentionDays: не трогать даже при retention=1
	recentName := monthName(recent)
	if _, ok := partsByName(t, pool)[recentName]; !ok {
		createPart(t, pool, recentName, recent, recent.AddDate(0, 1, 0))
	}
	insertAt(t, pool, recent.Add(time.Hour))

	if err := EnsurePartitionsLog(ctxT(t), pool, 1, discardLog()); err != nil {
		t.Fatal(err)
	}
	parts := partsByName(t, pool)
	if _, ok := parts[oldName]; ok {
		t.Fatalf("%s должна быть удалена", oldName)
	}
	if _, ok := parts[recentName]; !ok {
		t.Fatalf("%s удалена раньше %d дней", recentName, MinRetentionDays)
	}
	if _, ok := parts["audit_log_default"]; !ok {
		t.Fatal("DEFAULT удалена")
	}
}

func TestEnsurePartitionsCanceledContext(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := EnsurePartitionsLog(ctx, pool, 365, nil); err == nil {
		t.Fatal("want error on canceled ctx")
	}
}
