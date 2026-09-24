package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"lct/gocore/internal/config"
	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/pgtest"
	"lct/gocore/internal/settings"
)

func TestOverallStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name               string
		pg, ai, aiDegraded bool
		breaker            string
		backupStale        bool
		want               public.SystemHealthStatus
	}{
		{"всё хорошо", true, true, false, "closed", false, public.Ok},
		{"breaker неизвестен (нет AI-клиента) — не деградация сам по себе", true, true, false, "", false, public.Ok},
		{"нет БД — down, даже если AI в порядке", false, true, false, "closed", false, public.Down},
		{"нет БД и нет AI — down", false, false, false, "open", true, public.Down},
		{"AI недоступен", true, false, false, "closed", false, public.Degraded},
		{"AI сообщает degraded", true, true, true, "closed", false, public.Degraded},
		{"breaker open", true, true, false, "open", false, public.Degraded},
		{"breaker half_open", true, true, false, "half_open", false, public.Degraded},
		{"бэкап устарел", true, true, false, "closed", true, public.Degraded},
	}
	for _, tc := range cases {
		if got := overallStatus(tc.pg, tc.ai, tc.aiDegraded, tc.breaker, tc.backupStale); got != tc.want {
			t.Errorf("%s: overallStatus = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestCeilMs(t *testing.T) {
	t.Parallel()
	cases := map[time.Duration]int{
		-time.Second:            0,
		0:                       0,
		time.Nanosecond:         1,
		time.Millisecond:        1,
		time.Millisecond + 1:    2,
		1500 * time.Microsecond: 2,
		2 * time.Second:         2000,
	}
	for d, want := range cases {
		if got := ceilMs(d); got != want {
			t.Errorf("ceilMs(%v) = %d, want %d", d, got, want)
		}
	}
}

// Без БД: ответ всё равно собирается (часть postgres — ok=false), статус down.
func TestCollectHealthWithoutDB(t *testing.T) {
	t.Parallel()
	ai := healthyAI()
	h := New(Deps{AI: ai, WS: &fakeWS{conns: 4}, Config: &config.Config{Version: "1.2.3"},
		StartedAt: time.Now().Add(-90 * time.Second), Log: discardLog()})
	out := h.collectHealth(context.Background(), time.Now())

	if out.Status != public.Down {
		t.Errorf("status = %s, want down", out.Status)
	}
	if out.Postgres.Ok == nil || *out.Postgres.Ok || out.Postgres.LatencyMs != nil {
		t.Errorf("postgres = %+v", out.Postgres)
	}
	if out.GoCore.Version == nil || *out.GoCore.Version != "1.2.3" {
		t.Errorf("version = %v", out.GoCore.Version)
	}
	if out.GoCore.UptimeSec == nil || *out.GoCore.UptimeSec < 89 || *out.GoCore.UptimeSec > 120 {
		t.Errorf("uptime = %v", out.GoCore.UptimeSec)
	}
	if out.GoCore.WsConnections == nil || *out.GoCore.WsConnections != 4 {
		t.Errorf("ws = %v", out.GoCore.WsConnections)
	}
	if out.GoCore.ActiveSessions != nil || out.Jobs != nil {
		t.Errorf("без БД сессии и очередь не известны: %+v %+v", out.GoCore.ActiveSessions, out.Jobs)
	}
	a := out.AiService
	if a.Ok == nil || !*a.Ok || a.BreakerState == nil || *a.BreakerState != public.Closed {
		t.Fatalf("aiService = %+v", a)
	}
	if a.ModelsAvailable == nil || len(*a.ModelsAvailable) != 1 || a.Profiles == nil || a.Stt == nil || *a.Stt {
		t.Errorf("aiService health fields = %+v", a)
	}
	if a.Queue == nil || a.Queue.Running == nil || *a.Queue.Running != 1 || (*a.Queue.Pending)["evaluate_semantic"] != 3 ||
		*a.Queue.DialogWaiting != 2 || *a.Queue.DialogAvgMs != 1500 || *a.Queue.EstWaitSec != 7 {
		t.Errorf("queue = %+v", a.Queue)
	}
}

func TestCollectHealthAIVariants(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)

	cases := []struct {
		name   string
		ai     func() *fakeAI // nil — AI-клиента нет
		status public.SystemHealthStatus
		check  func(t *testing.T, out public.SystemHealth)
	}{
		{name: "всё в порядке", ai: healthyAI, status: public.Ok},
		{name: "ai-service недоступен", ai: func() *fakeAI {
			f := healthyAI()
			f.health, f.healthErr = nil, errors.New("connection refused")
			f.queue, f.queueErr = nil, errors.New("connection refused")
			return f
		}, status: public.Degraded, check: func(t *testing.T, out public.SystemHealth) {
			a := out.AiService
			if a.Ok == nil || *a.Ok || a.ModelsAvailable != nil || a.Queue != nil || a.Ollama != nil {
				t.Errorf("aiService = %+v", a)
			}
		}},
		{name: "ai-service degraded", ai: func() *fakeAI {
			f := healthyAI()
			f.health.Status = aiservice.Degraded
			return f
		}, status: public.Degraded, check: func(t *testing.T, out public.SystemHealth) {
			if out.AiService.Ok == nil || !*out.AiService.Ok {
				t.Errorf("degraded — ai-service отвечает (ok=true): %+v", out.AiService)
			}
		}},
		{name: "breaker half_open", ai: func() *fakeAI {
			f := healthyAI()
			f.breaker = "half_open"
			return f
		}, status: public.Degraded, check: func(t *testing.T, out public.SystemHealth) {
			if out.AiService.BreakerState == nil || *out.AiService.BreakerState != public.HalfOpen {
				t.Errorf("breakerState = %v", out.AiService.BreakerState)
			}
		}},
		{name: "неизвестное состояние breaker'а не попадает в ответ", ai: func() *fakeAI {
			f := healthyAI()
			f.breaker = "weird"
			return f
		}, status: public.Degraded, check: func(t *testing.T, out public.SystemHealth) {
			if out.AiService.BreakerState != nil {
				t.Errorf("breakerState = %v", *out.AiService.BreakerState)
			}
		}},
		{name: "пустые модели и очередь без pending — массив и объект, не null", ai: func() *fakeAI {
			f := healthyAI()
			f.health.ModelsAvailable = nil
			f.queue.Pending = nil
			return f
		}, status: public.Ok, check: func(t *testing.T, out public.SystemHealth) {
			b, _ := json.Marshal(out.AiService)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			if models, ok := m["modelsAvailable"].([]any); !ok || len(models) != 0 {
				t.Errorf("modelsAvailable = %v", m["modelsAvailable"])
			}
			q, _ := m["queue"].(map[string]any)
			if pending, ok := q["pending"].(map[string]any); !ok || len(pending) != 0 {
				t.Errorf("queue.pending = %v", q["pending"])
			}
		}},
		{name: "AI-клиента нет", ai: nil, status: public.Degraded, check: func(t *testing.T, out public.SystemHealth) {
			if out.AiService.Ok == nil || *out.AiService.Ok || out.AiService.BreakerState != nil {
				t.Errorf("aiService = %+v", out.AiService)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			recent := time.Now().Add(-time.Hour)
			d := Deps{Pool: pool, Ops: &fakeOps{last: &recent}, Jobs: &fakeJobs{m: map[string]int{"queued": 2}},
				Settings: settings.NewStore(pool, discardLog()), StartedAt: time.Now().Add(-48 * time.Hour), Log: discardLog()}
			if tc.ai != nil {
				d.AI = tc.ai()
			}
			out := New(d).collectHealth(context.Background(), time.Now())
			if out.Status != tc.status {
				t.Errorf("status = %s, want %s (%+v)", out.Status, tc.status, out.AiService)
			}
			if out.Postgres.Ok == nil || !*out.Postgres.Ok {
				t.Errorf("postgres = %+v", out.Postgres)
			}
			if tc.check != nil {
				tc.check(t, out)
			}
		})
	}
}

// Проверки БД: латентность, активные сессии (не отозванные и не истёкшие), очередь, бэкап.
func TestCollectHealthDB(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	ctx := context.Background()
	u := seedUser(t, pool, "student", "Сессионов")
	for i, q := range []string{
		`INSERT INTO auth_sessions (user_id, refresh_token_hash, expires_at) VALUES ($1, $2, now() + interval '1 hour')`,
		`INSERT INTO auth_sessions (user_id, refresh_token_hash, expires_at) VALUES ($1, $2, now() + interval '2 hour')`,
		`INSERT INTO auth_sessions (user_id, refresh_token_hash, expires_at, revoked_at) VALUES ($1, $2, now() + interval '1 hour', now())`,
		`INSERT INTO auth_sessions (user_id, refresh_token_hash, expires_at) VALUES ($1, $2, now() - interval '1 minute')`,
	} {
		if _, err := pool.Exec(ctx, q, u, "hash-"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	last := time.Date(2026, 9, 24, 3, 0, 5, 0, time.UTC)
	now := last.Add(2 * time.Hour)
	h := New(Deps{Pool: pool, AI: healthyAI(), Ops: &fakeOps{last: &last},
		Jobs:     &fakeJobs{m: map[string]int{"queued": 3, "running": 1, "failed": 0}},
		Settings: settings.NewStore(pool, discardLog()), StartedAt: now.Add(-24 * time.Hour), Log: discardLog()})
	out := h.collectHealth(ctx, now)

	if out.Status != public.Ok {
		t.Errorf("status = %s", out.Status)
	}
	if out.Postgres.Ok == nil || !*out.Postgres.Ok || out.Postgres.LatencyMs == nil || *out.Postgres.LatencyMs < 1 {
		t.Errorf("postgres = %+v", out.Postgres)
	}
	if out.Postgres.LastBackupAt == nil || !out.Postgres.LastBackupAt.Equal(last) {
		t.Errorf("lastBackupAt = %v", out.Postgres.LastBackupAt)
	}
	if out.GoCore.ActiveSessions == nil || *out.GoCore.ActiveSessions != 2 {
		t.Errorf("activeSessions = %v, want 2", out.GoCore.ActiveSessions)
	}
	if out.Jobs == nil || (*out.Jobs)["queued"] != 3 || (*out.Jobs)["running"] != 1 {
		t.Errorf("jobs = %v", out.Jobs)
	}
	if out.GoCore.Version == nil || *out.GoCore.Version != "dev" {
		t.Errorf("version без конфига = %v, want dev", out.GoCore.Version)
	}
}

// Ошибки вспомогательных проверок не роняют ответ и не выдумывают значений.
func TestCollectHealthPartialFailures(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	h := New(Deps{Pool: pool, AI: healthyAI(), Ops: &fakeOps{lastErr: errors.New("boom")},
		Jobs: &fakeJobs{err: errors.New("boom")}, Settings: settings.NewStore(pool, discardLog()),
		StartedAt: time.Now().Add(-48 * time.Hour), Log: discardLog()})
	out := h.collectHealth(context.Background(), time.Now())
	if out.Status != public.Ok {
		t.Errorf("status = %s: неизвестный бэкап не должен краснеть", out.Status)
	}
	if out.Jobs != nil || out.Postgres.LastBackupAt != nil {
		t.Errorf("jobs/lastBackupAt = %v %v", out.Jobs, out.Postgres.LastBackupAt)
	}
}

func TestBackupStale(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	disabled := settings.NewStore(pool, discardLog())
	if _, err := pool.Exec(context.Background(), `UPDATE settings SET value = '{"enabled": false, "hour": 3, "keep": 14}' WHERE key = 'backup'`); err != nil {
		t.Fatal(err)
	}
	if err := disabled.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	enabled := settings.NewStore(nil, discardLog()) // дефолты: backup.enabled = true

	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-25 * time.Hour)
	old := now.Add(-27 * time.Hour)
	okDB := func(last *time.Time) dbHealth { return dbHealth{ok: true, backupKnown: true, lastBackup: last} }

	cases := []struct {
		name    string
		set     *settings.Store
		started time.Time
		db      dbHealth
		want    bool
	}{
		{"свежая копия", enabled, now.Add(-72 * time.Hour), okDB(&fresh), false},
		{"копия старше суток с запасом", enabled, now.Add(-72 * time.Hour), okDB(&old), true},
		{"копий нет", enabled, now.Add(-72 * time.Hour), okDB(nil), true},
		{"копий нет, но сервис только стартовал", enabled, now.Add(-5 * time.Minute), okDB(nil), false},
		{"копирование выключено", disabled, now.Add(-72 * time.Hour), okDB(nil), false},
		{"БД недоступна — о бэкапах не судим", enabled, now.Add(-72 * time.Hour), dbHealth{}, false},
		{"LastBackupAt не ответил", enabled, now.Add(-72 * time.Hour), dbHealth{ok: true}, false},
		{"без хранилища настроек", nil, now.Add(-72 * time.Hour), okDB(nil), false},
	}
	for _, tc := range cases {
		h := New(Deps{Settings: tc.set, StartedAt: tc.started, Log: discardLog()})
		if got := h.backupStale(context.Background(), tc.db, now); got != tc.want {
			t.Errorf("%s: backupStale = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ai-service и БД опрашиваются параллельно, а вся ручка ограничена healthBudget.
func TestCollectHealthRespectsBudget(t *testing.T) {
	t.Parallel()
	ai := healthyAI()
	ai.delay = time.Minute // «повис» — ждём только до отмены контекста
	h := New(Deps{AI: ai, Log: discardLog()})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	out := h.collectHealth(ctx, time.Now())
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("collectHealth ждал %v", d)
	}
	if out.AiService.Ok == nil || *out.AiService.Ok {
		t.Fatalf("зависший ai-service — ok=false: %+v", out.AiService)
	}
}

// GET /admin/health — форма контракта SystemHealth: обязательные status, goCore, postgres, aiService.
func TestHealthHandlerContract(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	h := New(Deps{Pool: pool, AI: healthyAI(), Ops: &fakeOps{}, Jobs: &fakeJobs{m: map[string]int{}},
		WS: &fakeWS{}, Settings: settings.NewStore(pool, discardLog()), Log: discardLog()})
	srv, _ := newServer(t, h)
	r := do(t, srv, http.MethodGet, "/admin/health", "", reqOpt{role: "admin"})
	if r.status != http.StatusOK {
		t.Fatalf("status %d: %s", r.status, r.body)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"status", "goCore", "postgres", "aiService"} {
		if _, ok := m[k]; !ok {
			t.Errorf("нет обязательного поля %s: %s", k, r.body)
		}
	}
	var st string
	_ = json.Unmarshal(m["status"], &st)
	if !public.SystemHealthStatus(st).Valid() {
		t.Errorf("status = %q", st)
	}
	var pgPart struct {
		Ok        *bool `json:"ok"`
		LatencyMs *int  `json:"latencyMs"`
	}
	_ = json.Unmarshal(m["postgres"], &pgPart)
	if pgPart.Ok == nil || !*pgPart.Ok || pgPart.LatencyMs == nil {
		t.Errorf("postgres = %s", m["postgres"])
	}
}
