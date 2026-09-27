// Package ops — эксплуатация go-core (db-design §7): планировщик (ежедневный бэкап и
// обслуживание), резервное копирование pg_dump с журналом в backups, чистки (события,
// сессии, AI-задачи), партиции аудита, TLS с локальным CA контура, раздача SPA,
// health/ready и метрики из БД.
//
// Всё фоновое — одна горутина планировщика с минутным тиком плюс не более одной горутины
// бэкапа: без пулов воркеров и без работы «на всякий случай».
package ops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/audit"
	"lct/gocore/internal/config"
	"lct/gocore/internal/core"
	"lct/gocore/internal/settings"
)

// Расписание обслуживания (время сервера): после ночного бэкапа, в минимум нагрузки.
const (
	maintHour   = 4
	maintMinute = 30

	deleteBatch      = 5000                   // строк за один DELETE ретеншна
	deletePause      = 100 * time.Millisecond // пауза между батчами: не душим живую нагрузку
	deleteMaxRuntime = 15 * time.Minute       // потолок одной чистки таблицы за проход

	minEventsRetentionDays = 30 // защита от опечатки в настройке (события — источник истины попытки)
	sessionsKeepDays       = 7  // истёкшие/отозванные сессии держим неделю — для расследований
	aiJobsKeepDays         = 30 // выполненные и отменённые AI-задачи
	failedAiJobsKeepDays   = 90 // проваленные — дольше (разбор сбоев ai-service), но не вечно
	failedBackupsKeepDays  = 30

	maxScheduledBackupTries = 3 // попыток планового бэкапа в сутки (дальше — до завтра)
)

// Deps — зависимости ops.
type Deps struct {
	Pool     *pgxpool.Pool
	Config   *config.Config
	Settings *settings.Store
	Auditor  core.Auditor
	Log      *slog.Logger
}

// Ops — планировщик и эксплуатационные операции.
type Ops struct {
	pool *pgxpool.Pool
	cfg  *config.Config
	set  *settings.Store
	aud  core.Auditor
	log  *slog.Logger

	mu   sync.Mutex
	base context.Context // ctx из Start: фоновый бэкап останавливается вместе с сервисом
	wg   sync.WaitGroup

	// состояние планировщика (только его горутина)
	backupDay   string
	backupTries int
	lastTry     time.Time
	maintDay    string

	started time.Time
}

// New создаёт Ops. Auditor может быть nil (CLI без аудита).
func New(d Deps) *Ops {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	return &Ops{
		pool:    d.Pool,
		cfg:     d.Config,
		set:     d.Settings,
		aud:     d.Auditor,
		log:     log.With("component", "ops"),
		base:    context.Background(),
		started: time.Now(),
	}
}

// Start запускает планировщик (одна горутина, минутный тик). Сразу при старте: партиции
// аудита (запись в новый месяц не должна осесть в DEFAULT), пометка бэкапов, прерванных
// прошлым запуском, и удаление их недописанных файлов. Остановка — отменой ctx, ожидание — Wait.
func (o *Ops) Start(ctx context.Context) {
	o.mu.Lock()
	o.base = ctx
	o.mu.Unlock()
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		o.recoverInterruptedBackups(ctx)
		o.ensurePartitions(ctx)
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				o.tick(ctx, now.Local())
			}
		}
	}()
}

// Wait ждёт завершения фоновых горутин (после отмены ctx Start). Идущий pg_dump при отмене
// получает SIGTERM, строка backups помечается failed.
func (o *Ops) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { o.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// settings — текущие настройки; без Store (CLI) — значения по умолчанию из миграций.
func (o *Ops) settings(ctx context.Context) *settings.Snapshot {
	if o.set == nil {
		d := settings.Defaults()
		return &d
	}
	return o.set.Get(ctx)
}

func (o *Ops) baseCtx() context.Context {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.base
}

func (o *Ops) tick(ctx context.Context, now time.Time) {
	defer func() {
		if p := recover(); p != nil {
			o.log.Error("panic в планировщике", "panic", p)
		}
	}()
	snap := o.settings(ctx)
	if snap.Backup.Enabled {
		o.maybeScheduledBackup(ctx, now, snap.Backup.Hour)
	}
	day := now.Format(time.DateOnly)
	due := time.Date(now.Year(), now.Month(), now.Day(), maintHour, maintMinute, 0, 0, now.Location())
	if o.maintDay != day && !now.Before(due) {
		o.maintDay = day
		if err := o.RunMaintenance(ctx); err != nil && ctx.Err() == nil {
			o.log.Error("ежедневное обслуживание завершилось с ошибками", "err", err)
		}
	}
}

// maybeScheduledBackup — ежедневный бэкап в settings.backup.hour (время сервера), если с
// этого момента ещё нет успешной или идущей копии (ручная копия после часа X тоже засчитывается).
// Сервер был выключен в час X — копия снимется при первом тике после включения.
// Неудача — повтор через час, не больше maxScheduledBackupTries раз за сутки.
func (o *Ops) maybeScheduledBackup(ctx context.Context, now time.Time, hour int) {
	if hour < 0 || hour > 23 {
		hour = 3
	}
	sched := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
	if now.Before(sched) {
		return
	}
	day := now.Format(time.DateOnly)
	if o.backupDay != day {
		o.backupDay, o.backupTries = day, 0
	}
	if o.backupTries >= maxScheduledBackupTries || (!o.lastTry.IsZero() && now.Sub(o.lastTry) < time.Hour) {
		return
	}
	var have bool
	err := o.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM backups
		                WHERE started_at >= $1
		                  AND (status = 'done' OR (status = 'running' AND started_at > now() - interval '2 hours')))`,
		sched).Scan(&have)
	if err != nil {
		if ctx.Err() == nil {
			o.log.Warn("проверка планового бэкапа не удалась", "err", err)
		}
		return
	}
	if have {
		return
	}
	o.backupTries++
	o.lastTry = now
	if _, err := o.StartBackup(ctx, nil); err != nil && !errors.Is(err, ErrBackupRunning) {
		o.log.Error("плановый бэкап не запущен", "err", err)
	}
}

// ---------------------------------------------------------------- обслуживание

func (o *Ops) ensurePartitions(ctx context.Context) {
	days := o.settings(ctx).AuditRetentionDays
	if err := audit.EnsurePartitionsLog(ctx, o.pool, days, o.log); err != nil && ctx.Err() == nil {
		o.log.Error("партиции аудита", "err", err)
	}
}

// RunMaintenance — ежедневное обслуживание: партиции аудита и ретеншн, чистка событий
// попыток, сессий, завершённых AI-задач и озвучки реплик диалога завершённых попыток.
// Шаги независимы: ошибка одного не отменяет остальные. Экспортирована для CLI и тестов.
func (o *Ops) RunMaintenance(ctx context.Context) error {
	start := time.Now()
	snap := o.settings(ctx)
	var errs []error

	if err := audit.EnsurePartitionsLog(ctx, o.pool, snap.AuditRetentionDays, o.log); err != nil {
		errs = append(errs, err)
	}

	var events int64
	if days := snap.EventsRetentionDays; days > 0 { // 0 и меньше — хранить бессрочно
		days = max(days, minEventsRetentionDays)
		n, err := o.batchedDelete(ctx, "attempt_events", `
			DELETE FROM attempt_events
			 WHERE id IN (SELECT id FROM attempt_events
			               WHERE at < now() - make_interval(days => $1::int)
			               LIMIT $2)`, days, deleteBatch)
		events = n
		if err != nil {
			errs = append(errs, err)
		}
	}

	var sessions int64
	tag, err := o.pool.Exec(ctx, `
		DELETE FROM auth_sessions
		 WHERE expires_at < now() - make_interval(days => $1::int)
		    OR revoked_at < now() - make_interval(days => $1::int)`, sessionsKeepDays)
	if err != nil {
		errs = append(errs, fmt.Errorf("ops: auth_sessions cleanup: %w", err))
	} else {
		sessions = tag.RowsAffected()
	}

	// failed тоже терминален: без чистки такие задачи копились бы вечно (ai-service лежал
	// неделю — тысячи строк с payload). Повторная постановка с тем же dedup_key после
	// удаления просто вставит новую задачу.
	jobs, err := o.batchedDelete(ctx, "ai_jobs", `
		DELETE FROM ai_jobs
		 WHERE id IN (SELECT id FROM ai_jobs
		               WHERE ((status = 'done' OR status = 'cancelled')
		                      AND COALESCE(finished_at, created_at) < now() - make_interval(days => $1::int))
		                  OR (status = 'failed'
		                      AND COALESCE(finished_at, created_at) < now() - make_interval(days => $2::int))
		               LIMIT $3)`, aiJobsKeepDays, failedAiJobsKeepDays, deleteBatch)
	if err != nil {
		errs = append(errs, err)
	}

	audio, err := o.cleanupDialogAudio(ctx)
	if err != nil {
		errs = append(errs, err)
	}

	o.log.Info("обслуживание выполнено", "attempt_events_deleted", events, "sessions_deleted", sessions,
		"ai_jobs_deleted", jobs, "dialog_audio_dirs_deleted", audio,
		"dur_ms", time.Since(start).Milliseconds(), "errors", len(errs))
	return errors.Join(errs...)
}

// batchedDelete — DELETE батчами по deleteBatch с паузами: короткие транзакции, без
// долгих блокировок и всплеска WAL; останавливается по ctx и по deleteMaxRuntime
// (остаток дочистится завтра).
func (o *Ops) batchedDelete(ctx context.Context, table, sql string, args ...any) (int64, error) {
	var total int64
	deadline := time.Now().Add(deleteMaxRuntime)
	for {
		tag, err := o.pool.Exec(ctx, sql, args...)
		if err != nil {
			return total, fmt.Errorf("ops: %s cleanup: %w", table, err)
		}
		n := tag.RowsAffected()
		total += n
		if n < deleteBatch || time.Now().After(deadline) {
			return total, nil
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(deletePause):
		}
	}
}
