package settings

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/platform/pgtest"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Все ключи снимка (как их видит JSON) — ровно те, что знает apply.
func snapshotKeys(t *testing.T) []string {
	t.Helper()
	b, err := json.Marshal(Defaults())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestDefaultsRoundTripAndValid(t *testing.T) {
	t.Parallel()
	d := Defaults()
	b, _ := json.Marshal(d)
	var m map[string]json.RawMessage
	_ = json.Unmarshal(b, &m)
	for _, k := range snapshotKeys(t) {
		probe := Defaults()
		if err := apply(&probe, k, m[k]); err != nil {
			t.Errorf("apply(%s): %v", k, err)
		}
		if err := validate(&probe, k); err != nil {
			t.Errorf("дефолт %s не проходит свою же проверку: %v", k, err)
		}
	}
	if s := d.ScoreWeights.Sum(); s < 0.99 || s > 1.01 {
		t.Errorf("сумма весов по умолчанию = %v", s)
	}
	if got := d.ScoreWeights.Map(); len(got) != 5 || got["fields"] != 0.5 || got["dialogue"] != 0 {
		t.Errorf("Map = %v", got)
	}
}

func TestApplyUnknownAndBadType(t *testing.T) {
	t.Parallel()
	s := Defaults()
	if err := apply(&s, "no_such_key", json.RawMessage(`1`)); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown key: %v", err)
	}
	if err := apply(&s, "time_limit_sec", json.RawMessage(`"тридцать"`)); err == nil || !strings.Contains(err.Error(), "time_limit_sec") {
		t.Fatalf("bad type: %v", err)
	}
	if err := apply(&s, "time_limit_sec", json.RawMessage(`45`)); err != nil || s.TimeLimitSec != 45 {
		t.Fatalf("apply ok: %v %d", err, s.TimeLimitSec)
	}
	// частичный объект — недостающие поля остаются прежними
	if err := apply(&s, "voice", json.RawMessage(`{"enabled": true}`)); err != nil || !s.Voice.Enabled || s.Voice.MaxTurns != 12 {
		t.Fatalf("partial voice: %v %+v", err, s.Voice)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key, value string
		ok         bool
	}{
		{"time_limit_sec", `30`, true},
		{"time_limit_sec", `9`, false},
		{"time_limit_sec", `3600`, true},
		{"time_limit_sec", `3601`, false},
		{"pass_threshold", `0`, true},
		{"pass_threshold", `100`, true},
		{"pass_threshold", `100.5`, false},
		{"pass_threshold", `-1`, false},
		{"score_weights", `{"fields":1,"semantic":0,"grammar":0,"timing":0,"dialogue":0}`, true},
		{"score_weights", `{"fields":0,"semantic":0,"grammar":0,"timing":0,"dialogue":0}`, false},
		{"score_weights", `{"fields":-0.1,"semantic":1}`, false},
		{"dialogue_weight_default", `0`, true},
		{"dialogue_weight_default", `0.99`, true},
		{"dialogue_weight_default", `1`, false},
		{"confidence_threshold", `1`, true},
		{"confidence_threshold", `1.01`, false},
		{"audit_retention_days", `180`, true},
		{"audit_retention_days", `179`, false},
		{"voice", `{"max_turns": 2, "input": "both"}`, true},
		{"voice", `{"max_turns": 41}`, false},
		{"voice", `{"max_turns": 1}`, false},
		{"voice", `{"input": "голос"}`, false},
		{"cards_per_student", `50`, true},
		{"cards_per_student", `0`, false},
		{"backup", `{"hour": 23, "keep": 1}`, true},
		{"backup", `{"hour": 24}`, false},
		{"backup", `{"keep": 0}`, false},
		{"login", `{"max_failed": 1, "lock_minutes": 1, "session_ttl_hours": 1}`, true},
		{"login", `{"session_ttl_hours": 0}`, false},
		{"ai", `{"max_tries": 1, "reaper_after_sec": 60}`, true},
		{"ai", `{"reaper_after_sec": 59}`, false},
		{"ai", `{"dialog_timeout_sec": -1}`, false},
		// ключи, которые раньше не проверялись вовсе
		{"timing_tolerance", `{"soft_pct": 0, "hard_pct": 0}`, true},
		{"timing_tolerance", `{"soft_pct": 50, "hard_pct": 20}`, false},
		{"timing_tolerance", `{"soft_pct": -5}`, false},
		{"xp_rules", `{"attempt_evaluated": 0}`, true},
		{"xp_rules", `{"within_norm": -5}`, false},
		{"tts", `{"voice": "baya", "rate": 1.5, "sample_rate": 48000}`, true},
		{"tts", `{"voice": "  "}`, false},
		{"tts", `{"rate": 0}`, false},
		{"tts", `{"rate": 10}`, false},
		{"tts", `{"sample_rate": 0}`, false},
		{"stt", `{"confidence_floor": 0}`, true},
		{"stt", `{"confidence_floor": 1.2}`, false},
		{"events_retention_days", `0`, true},
		{"events_retention_days", `-1`, false},
		{"allow_replay", `false`, true},
	}
	for _, c := range cases {
		probe := Defaults()
		if err := apply(&probe, c.key, json.RawMessage(c.value)); err != nil {
			t.Fatalf("%s=%s: apply: %v", c.key, c.value, err)
		}
		err := validate(&probe, c.key)
		if (err == nil) != c.ok {
			t.Errorf("%s=%s: ok=%v, err=%v", c.key, c.value, c.ok, err)
		}
		if err != nil && !strings.Contains(err.Error(), c.key) {
			t.Errorf("сообщение без имени ключа: %v", err)
		}
	}
}

func TestStoreWithoutPool(t *testing.T) {
	t.Parallel()
	s := NewStore(nil, nil)
	got := s.Get(context.Background())
	if got == nil || got.TimeLimitSec != 30 {
		t.Fatalf("Get без БД — дефолты, получено %+v", got)
	}
	if err := s.Reload(context.Background()); err == nil {
		t.Fatal("Reload без пула должен вернуть ошибку, а не паниковать")
	}
	s.Run(context.Background(), time.Millisecond) // без пула — сразу выходит
}

// ---------------------------------------------------------------- с БД

func newStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	pool := pgtest.New(t)
	s := NewStore(pool, quietLog())
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, pool
}

func setRaw(t *testing.T, pool *pgxpool.Pool, key, value string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE settings SET value = $2::jsonb WHERE key = $1`, key, value); err != nil {
		t.Fatal(err)
	}
}

func makeStale(s *Store) { s.loadedAt.Store(time.Now().Add(-2 * RefreshEvery).UnixNano()) }

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("не дождались: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReloadFromTable(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	// сиды миграций = Defaults()
	if got, want := *s.Get(ctx), Defaults(); got != want {
		t.Fatalf("снимок из сидов отличается от Defaults:\n%+v\n%+v", got, want)
	}
	rows, err := s.List(ctx)
	if err != nil || len(rows) != len(snapshotKeys(t)) {
		t.Fatalf("List: %d строк, err=%v (ключей схемы %d)", len(rows), err, len(snapshotKeys(t)))
	}
	for _, r := range rows {
		if r.Description == "" || r.UpdatedAt.IsZero() || len(r.Value) == 0 {
			t.Errorf("строка %s неполная: %+v", r.Key, r)
		}
	}

	setRaw(t, pool, "time_limit_sec", `45`)
	// битое значение: ошибка типа в одном поле не должна частично применить соседние
	setRaw(t, pool, "voice", `{"enabled": true, "max_turns": "много"}`)
	if _, err := pool.Exec(ctx, `INSERT INTO settings (key, value) VALUES ('legacy_key', '1')`); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	snap := s.Get(ctx)
	if snap.TimeLimitSec != 45 {
		t.Errorf("time_limit_sec = %d", snap.TimeLimitSec)
	}
	if snap.Voice != Defaults().Voice {
		t.Errorf("битое значение voice применилось частично: %+v", snap.Voice)
	}
}

func TestUpdate(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	var admin uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO users (login, password_hash, role, last_name, first_name)
		VALUES ('adm', 'x', 'admin', 'Админов', 'Админ') RETURNING id`).Scan(&admin); err != nil {
		t.Fatal(err)
	}

	before, after, err := s.Update(ctx, "pass_threshold", json.RawMessage(`80`), admin)
	if err != nil {
		t.Fatal(err)
	}
	if string(before.Value) != "70" || string(after.Value) != "80" || after.UpdatedBy == nil || *after.UpdatedBy != admin {
		t.Fatalf("before=%s after=%s by=%v", before.Value, after.Value, after.UpdatedBy)
	}
	if s.Get(ctx).PassThreshold != 80 {
		t.Fatal("снимок не перечитан после Update")
	}

	if _, _, err := s.Update(ctx, "nope", json.RawMessage(`1`), admin); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown key: %v", err)
	}
	if _, _, err := s.Update(ctx, "pass_threshold", json.RawMessage(`101`), admin); err == nil {
		t.Fatal("валидация не сработала")
	}
	if _, _, err := s.Update(ctx, "pass_threshold", json.RawMessage(`"x"`), admin); err == nil {
		t.Fatal("ошибка типа не сработала")
	}
	var v string
	_ = pool.QueryRow(ctx, `SELECT value::text FROM settings WHERE key = 'pass_threshold'`).Scan(&v)
	if v != "80" {
		t.Fatalf("отклонённое значение попало в БД: %s", v)
	}

	// ключ схемы, которого нет в таблице: before пустой, строка создаётся
	if _, err := pool.Exec(ctx, `DELETE FROM settings WHERE key = 'allow_replay'`); err != nil {
		t.Fatal(err)
	}
	before, after, err = s.Update(ctx, "allow_replay", json.RawMessage(`false`), admin)
	if err != nil || before.Key != "allow_replay" || before.Value != nil || string(after.Value) != "false" {
		t.Fatalf("upsert: %v %+v %+v", err, before, after)
	}
}

// Регрессия (ревью: pool-starvation/deadlock). Get вызывается внутри открытых транзакций;
// раньше при устаревшем снимке он синхронно перечитывал таблицу через пул (второе
// соединение) под общим мьютексом — при насыщенном пуле все соединения повисали на мьютексе,
// а перечитывающему соединения не доставалось. Теперь Get возвращается сразу.
func TestGetDoesNotBlockInsideTxWhenPoolExhausted(t *testing.T) {
	t.Parallel()
	_, url := pgtest.NewWithURL(t)
	const conns = 3
	pool, err := pg.Connect(context.Background(), url, conns, "settings-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s := NewStore(pool, quietLog())
	if err := s.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	setRaw(t, pool, "time_limit_sec", `55`)
	makeStale(s)

	var (
		ready, start sync.WaitGroup
		done         sync.WaitGroup
		mu           sync.Mutex
		slowest      time.Duration
		seen         []int
	)
	ready.Add(conns)
	start.Add(1)
	errs := make(chan error, conns)
	for i := 0; i < conns; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			tx, err := pool.Begin(ctx) // все соединения пула заняты транзакциями
			if err != nil {
				ready.Done()
				errs <- err
				return
			}
			defer tx.Rollback(context.Background()) //nolint:errcheck
			if _, err := tx.Exec(ctx, `SELECT 1`); err != nil {
				ready.Done()
				errs <- err
				return
			}
			ready.Done()
			start.Wait()
			t0 := time.Now()
			snap := s.Get(ctx)
			d := time.Since(t0)
			mu.Lock()
			slowest = max(slowest, d)
			seen = append(seen, snap.TimeLimitSec)
			mu.Unlock()
			if _, err := tx.Exec(ctx, `SELECT 1`); err != nil {
				errs <- err
			}
		}()
	}
	ready.Wait()
	start.Done()
	done.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if slowest > time.Second {
		t.Fatalf("Get внутри транзакции ждал %v при занятом пуле", slowest)
	}
	for _, v := range seen {
		if v != 30 && v != 55 {
			t.Fatalf("странный снимок: %d", v)
		}
	}
	// фоновое перечитывание дождалось соединения и подхватило новое значение
	eventually(t, "фоновый reload", func() bool { return s.Get(context.Background()).TimeLimitSec == 55 })
}

// Хранилище, которому не вызвали Reload, загружается синхронно при первом Get — но не
// дольше firstLoad, даже если соединений нет; ждавшие не перечитывают повторно.
func TestFirstLoadIsBounded(t *testing.T) {
	t.Parallel()
	_, url := pgtest.NewWithURL(t)
	pool, err := pg.Connect(context.Background(), url, 1, "settings-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	setRaw(t, pool, "time_limit_sec", `40`)

	s := NewStore(pool, quietLog())
	s.firstLoad = 200 * time.Millisecond
	ctx := context.Background()
	tx, err := pool.Begin(ctx) // единственное соединение занято
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	snap := s.Get(ctx)
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("первая загрузка без соединения ждала %v", d)
	}
	if snap.TimeLimitSec != 30 {
		t.Fatalf("без БД — дефолты, получено %d", snap.TimeLimitSec)
	}
	_ = tx.Rollback(ctx)
	// после ошибки повтор — через retryAfterError, а не через полный RefreshEvery
	if age := time.Since(time.Unix(0, s.loadedAt.Load())); age < RefreshEvery-retryAfterError-time.Second {
		t.Fatalf("после ошибки повтор отложен слишком надолго: age=%v", age)
	}

	s2 := NewStore(pool, quietLog())
	if got := s2.Get(ctx).TimeLimitSec; got != 40 {
		t.Fatalf("первая загрузка со свободным пулом: %d", got)
	}
}

func TestGetStaleRefreshesInBackground(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	setRaw(t, pool, "cards_per_student", `3`)
	makeStale(s)
	if got := s.Get(context.Background()).CardsPerStudent; got != 1 && got != 3 {
		t.Fatalf("got %d", got)
	}
	eventually(t, "cards_per_student=3", func() bool { return s.Get(context.Background()).CardsPerStudent == 3 })
	eventually(t, "refreshing сброшен", func() bool { return !s.refreshing.Load() })
}

func TestRunReloadsPeriodically(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { s.Run(ctx, 20*time.Millisecond); close(stopped) }()
	setRaw(t, pool, "allow_replay", `false`)
	eventually(t, "Run подхватил allow_replay=false", func() bool { return !s.Get(context.Background()).AllowReplay })
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run не остановился по ctx")
	}
}
