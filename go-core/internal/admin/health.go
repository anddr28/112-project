package admin

import (
	"context"
	"net/http"
	"sync"
	"time"

	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
)

const (
	// healthBudget — потолок всей ручки: при лежащем ai-service запрос к нему ждёт до 3 с
	// (таймаут aijobs), БД проверяется параллельно — админка не висит дольше.
	healthBudget = 4 * time.Second
	// dbBudget — все проверки БД вместе (пинг, сессии, бэкап, очередь); упавшая БД
	// не должна держать ручку до таймаута пула.
	dbBudget = 2 * time.Second
	// backupStaleAfter — ТЗ: копия не реже раза в сутки; +2 ч запаса на длительность дампа
	// и сдвиг расписания.
	backupStaleAfter = 26 * time.Hour
	// backupGrace — после старта планировщику нужно время, чтобы догнать пропущенный бэкап
	// (первая минута тика + сам pg_dump): свежий стенд не должен сразу краснеть.
	backupGrace = 15 * time.Minute
)

// Активные сессии — не отозванные и не истёкшие (auth_sessions_expires_idx / частичный
// индекс по revoked_at). Сессий сотни — count дешёвый.
const sqlActiveSessions = `SELECT count(*) FROM auth_sessions WHERE revoked_at IS NULL AND expires_at > now()`

// GET /admin/health — SystemHealth. Отказ любой части не роняет ответ: часть помечается
// ok=false, общий статус — down (нет БД) / degraded (AI, бэкапы) / ok.
func (h *Handlers) health(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), healthBudget)
	defer cancel()
	httpx.WriteJSON(w, http.StatusOK, h.collectHealth(ctx, time.Now()))
	return nil
}

// dbHealth — результат проверок PostgreSQL.
type dbHealth struct {
	ok          bool
	latencyMs   int
	sessions    *int
	lastBackup  *time.Time
	backupKnown bool // LastBackupAt ответил (иначе о бэкапах ничего не утверждаем)
	jobs        map[string]int
}

// aiHealth — результат опроса ai-service (оба вызова кэшируются в aijobs на 5 с).
type aiHealth struct {
	health    *aiservice.Health
	healthErr error
	queue     *aiservice.QueueStatus
	queueErr  error
	breaker   string
}

func (h *Handlers) collectHealth(ctx context.Context, now time.Time) public.SystemHealth {
	// ai-service опрашивается параллельно с БД: при его недоступности каждый вызов ждёт
	// таймаут, последовательно это сложилось бы с проверками БД. Две короткие горутины
	// на редкий админский запрос — осознанно.
	var (
		wg sync.WaitGroup
		ai aiHealth
	)
	if h.ai != nil {
		ai.breaker = h.ai.BreakerState()
		wg.Add(2)
		go func() {
			defer wg.Done()
			ai.health, ai.healthErr = h.ai.Health(ctx)
		}()
		go func() {
			defer wg.Done()
			ai.queue, ai.queueErr = h.ai.Queue(ctx)
		}()
	}
	db := h.checkDB(ctx)
	wg.Wait()
	if ai.healthErr != nil {
		h.log.Debug("ai-service health недоступен", "err", ai.healthErr)
	}

	var out public.SystemHealth

	// ---- go-core
	version := "dev"
	if h.cfg != nil && h.cfg.Version != "" {
		version = h.cfg.Version
	}
	uptime := int(now.Sub(h.started) / time.Second)
	out.GoCore.Version = &version
	out.GoCore.UptimeSec = &uptime
	out.GoCore.ActiveSessions = db.sessions
	if h.ws != nil {
		n := h.ws.Connections()
		out.GoCore.WsConnections = &n
	}

	// ---- PostgreSQL
	pgOK := db.ok
	out.Postgres.Ok = &pgOK
	if db.ok {
		lat := db.latencyMs
		out.Postgres.LatencyMs = &lat
	}
	out.Postgres.LastBackupAt = db.lastBackup

	// ---- ai-service
	aiOK := h.ai != nil && ai.healthErr == nil && ai.health != nil
	out.AiService.Ok = &aiOK
	if ai.breaker != "" {
		if bs := public.SystemHealthAiServiceBreakerState(ai.breaker); bs.Valid() {
			out.AiService.BreakerState = &bs
		}
	}
	if aiOK {
		hl := ai.health
		out.AiService.Ollama = hl.Ollama
		out.AiService.Languagetool = hl.Languagetool
		out.AiService.Tts = hl.Tts
		out.AiService.Stt = hl.Stt
		models := []string{}
		if hl.ModelsAvailable != nil {
			models = append(models, *hl.ModelsAvailable...)
		}
		out.AiService.ModelsAvailable = &models
		out.AiService.Profiles = hl.Profiles
	}
	if ai.queueErr == nil && ai.queue != nil {
		q := ai.queue
		qv := newOf(out.AiService.Queue)
		pending := q.Pending
		if pending == nil {
			pending = map[string]int{}
		}
		running := q.Running
		qv.Pending = &pending
		qv.Running = &running
		qv.DialogWaiting = q.DialogWaiting
		qv.DialogAvgMs = q.DialogAvgMs
		qv.EstWaitSec = q.EstWaitSec
		out.AiService.Queue = qv
	}

	// ---- очередь ai_jobs
	if db.jobs != nil {
		jobs := db.jobs
		out.Jobs = &jobs
	}

	aiDegraded := aiOK && string(ai.health.Status) != "ok"
	backupStale := h.backupStale(ctx, db, now)
	out.Status = overallStatus(pgOK, aiOK, aiDegraded, ai.breaker, backupStale)
	return out
}

// checkDB — пинг (латентность SELECT 1, включая взятие соединения из пула — это и есть
// задержка, которую видят запросы), затем счётчики. Пинг не прошёл — остальное не пробуем:
// каждый запрос ждал бы тот же таймаут.
func (h *Handlers) checkDB(ctx context.Context) dbHealth {
	var st dbHealth
	if h.pool == nil {
		return st
	}
	ctx, cancel := context.WithTimeout(ctx, dbBudget)
	defer cancel()

	start := time.Now()
	var one int
	if err := h.pool.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		h.log.Warn("health: PostgreSQL не отвечает", "err", err)
		return st
	}
	st.ok = true
	st.latencyMs = ceilMs(time.Since(start))

	var n int
	if err := h.pool.QueryRow(ctx, sqlActiveSessions).Scan(&n); err != nil {
		h.log.Warn("health: активные сессии", "err", err)
	} else {
		st.sessions = &n
	}
	if h.ops != nil {
		if t, err := h.ops.LastBackupAt(ctx); err != nil {
			h.log.Warn("health: последний бэкап", "err", err)
		} else {
			st.lastBackup, st.backupKnown = t, true
		}
	}
	if h.jobs != nil {
		if m, err := h.jobs.Counts(ctx); err != nil {
			h.log.Warn("health: очередь AI", "err", err)
		} else {
			st.jobs = m
		}
	}
	return st
}

// backupStale — ежедневное копирование включено, а успешной копии нет дольше суток.
func (h *Handlers) backupStale(ctx context.Context, db dbHealth, now time.Time) bool {
	if !db.ok || !db.backupKnown || h.set == nil {
		return false
	}
	if !h.set.Get(ctx).Backup.Enabled || now.Sub(h.started) < backupGrace {
		return false
	}
	return db.lastBackup == nil || now.Sub(*db.lastBackup) > backupStaleAfter
}

// overallStatus — сводный статус контура. down — без БД ядро не работает вовсе;
// degraded — занятия идут, но AI-слои/голос/бэкапы под угрозой.
func overallStatus(pgOK, aiOK, aiDegraded bool, breaker string, backupStale bool) public.SystemHealthStatus {
	switch {
	case !pgOK:
		return public.Down
	case !aiOK || aiDegraded || (breaker != "" && breaker != "closed") || backupStale:
		return public.Degraded
	}
	return public.Ok
}

// ceilMs — миллисекунды с округлением вверх: реальный round-trip не показывается как «0 мс».
func ceilMs(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int((d + time.Millisecond - 1) / time.Millisecond)
}
