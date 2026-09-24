package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/metrics"
	"lct/gocore/internal/platform/pg"
)

// ErrBackupRunning — копирование уже идёт (хендлер отвечает 409 conflict).
var ErrBackupRunning = errors.New("ops: backup already running")

const (
	backupTimeout    = 30 * time.Minute
	backupStaleAfter = 2 * time.Hour // running дольше — зависшая строка (процесс убит), помечается failed
	maxBackupError   = 1000          // байт текста ошибки в backups.error
	backupListLimit  = 100
	// backupLockKey — ключ транзакционной advisory-блокировки: две одновременные попытки
	// запуска (кнопка + расписание) не создадут две строки running.
	backupLockKey int64 = 0x4c43_5442_4b50 // "LCTBKP"
)

var (
	mBackupSeconds = metrics.NewHistogram("lct_backup_duration_seconds", "Длительность резервного копирования (pg_dump + sha256), секунды.", metrics.SlowBuckets)
	mBackupResult  = metrics.NewCounterVec("lct_backups_total", "Завершённые резервные копирования по результату.", "status")
)

const backupCols = `id, started_at, finished_at, status, file_path, size_bytes, sha256, error, triggered_by`

func scanBackup(row pgx.Row) (public.Backup, error) {
	var (
		b                     public.Backup
		status                string
		finished              *time.Time
		filePath, sum, errTxt *string
		size                  *int64
		by                    *uuid.UUID
	)
	if err := row.Scan(&b.Id, &b.StartedAt, &finished, &status, &filePath, &size, &sum, &errTxt, &by); err != nil {
		return public.Backup{}, err
	}
	b.StartedAt = b.StartedAt.UTC()
	if finished != nil {
		t := finished.UTC()
		b.FinishedAt = &t
	}
	b.Status = public.BackupStatus(status)
	b.FilePath, b.SizeBytes, b.Sha256, b.Error, b.TriggeredBy = filePath, size, sum, errTxt, by
	return b, nil
}

// ListBackups — журнал копий, новые сверху (не больше 100). Никогда не nil.
func (o *Ops) ListBackups(ctx context.Context) ([]public.Backup, error) {
	rows, err := o.pool.Query(ctx, `SELECT `+backupCols+` FROM backups ORDER BY started_at DESC LIMIT $1`, backupListLimit)
	if err != nil {
		return nil, fmt.Errorf("ops: list backups: %w", err)
	}
	defer rows.Close()
	out := make([]public.Backup, 0, 16)
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, fmt.Errorf("ops: list backups: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ops: list backups: %w", err)
	}
	return out, nil
}

// LastBackupAt — время окончания последней успешной копии (nil — ни одной).
func (o *Ops) LastBackupAt(ctx context.Context) (*time.Time, error) {
	var t *time.Time
	if err := o.pool.QueryRow(ctx, `SELECT max(finished_at) FROM backups WHERE status = 'done'`).Scan(&t); err != nil {
		return nil, fmt.Errorf("ops: last backup: %w", err)
	}
	if t != nil {
		u := t.UTC()
		t = &u
	}
	return t, nil
}

// StartBackup — POST /admin/backups и расписание: вставляет строку running и сразу её
// возвращает (202), pg_dump идёт в фоне (таймаут 30 мин, отмена — при остановке сервиса).
// by = nil — по расписанию. ErrBackupRunning — копирование уже идёт (409).
func (o *Ops) StartBackup(ctx context.Context, by *uuid.UUID) (public.Backup, error) {
	b, path, err := o.beginBackup(ctx, by)
	if err != nil {
		return public.Backup{}, err
	}
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		runCtx, cancel := context.WithTimeout(o.baseCtx(), backupTimeout)
		defer cancel()
		_ = o.runBackup(runCtx, b.Id, path)
	}()
	return b, nil
}

// RunBackupSync — копирование в текущей горутине (CLI). Возвращает итоговую строку;
// при неудаче — строку со status=failed и ошибку.
func (o *Ops) RunBackupSync(ctx context.Context, by *uuid.UUID) (public.Backup, error) {
	b, path, err := o.beginBackup(ctx, by)
	if err != nil {
		return public.Backup{}, err
	}
	runCtx, cancel := context.WithTimeout(ctx, backupTimeout)
	defer cancel()
	dumpErr := o.runBackup(runCtx, b.Id, path)
	got, err := scanBackup(o.pool.QueryRow(context.WithoutCancel(ctx), `SELECT `+backupCols+` FROM backups WHERE id = $1`, b.Id))
	if err != nil {
		return b, fmt.Errorf("ops: read backup: %w", err)
	}
	return got, dumpErr
}

// beginBackup — под advisory-блокировкой: зависшие running → failed, проверка «уже идёт»,
// вставка строки running. Аудит backup.run — после коммита.
func (o *Ops) beginBackup(ctx context.Context, by *uuid.UUID) (public.Backup, string, error) {
	dir, err := filepath.Abs(o.cfg.BackupDir)
	if err != nil {
		return public.Backup{}, "", fmt.Errorf("ops: backup dir: %w", err)
	}
	id := ids.New()
	var b public.Backup
	var path string
	err = pg.WithTx(ctx, o.pool, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, backupLockKey); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE backups SET status = 'failed', finished_at = now(),
			       error = 'Прервано: копирование не завершилось (сбой или перезапуск сервера)'
			 WHERE status = 'running' AND started_at < now() - make_interval(secs => $1::int)`,
			int(backupStaleAfter/time.Second)); err != nil {
			return err
		}
		var running bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM backups WHERE status = 'running')`).Scan(&running); err != nil {
			return err
		}
		if running {
			return ErrBackupRunning
		}
		var started time.Time
		if err := tx.QueryRow(ctx, `
			INSERT INTO backups (id, started_at, status, triggered_by) VALUES ($1, now(), 'running', $2)
			RETURNING started_at`, id, by).Scan(&started); err != nil {
			return err
		}
		// имя — по локальному времени сервера: админ ищет «копию за 03:00»; две копии в одну
		// секунду (ручная сразу после плановой) не должны затереть друг друга — хвост из
		// случайной части UUIDv7 (начало v7 — время, оно у них совпало бы)
		path = filepath.Join(dir, "lct-"+started.Local().Format("20060102-150405")+".dump")
		if _, err := os.Stat(path); err == nil {
			path = strings.TrimSuffix(path, ".dump") + "-" + id.String()[28:] + ".dump"
		}
		b = public.Backup{Id: id, StartedAt: started.UTC(), Status: public.BackupStatusRunning, TriggeredBy: by}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrBackupRunning) {
			return public.Backup{}, "", ErrBackupRunning
		}
		return public.Backup{}, "", fmt.Errorf("ops: begin backup: %w", err)
	}
	if o.aud != nil {
		o.aud.Log(ctx, core.AuditEntry{
			Action:      "backup.run",
			EntityType:  "backup",
			EntityID:    id,
			After:       map[string]any{"status": "running", "scheduled": by == nil},
			ActorID:     by,
			ActorSystem: by == nil,
		})
	}
	o.log.Info("резервное копирование запущено", "backup", id, "scheduled", by == nil)
	return b, path, nil
}

// runBackup — pg_dump, sha256, итог в строке backups, ретеншн. Ошибка — в строку и в лог.
func (o *Ops) runBackup(ctx context.Context, id uuid.UUID, path string) error {
	start := time.Now()
	size, sum, err := o.dump(ctx, path)
	// итог пишем даже при отменённом ctx (остановка сервиса): строка не должна висеть running
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err != nil {
		msg := backupErrorText(ctx, err)
		mBackupResult.With("failed").Inc()
		o.log.Error("резервное копирование не удалось", "backup", id, "err", msg)
		if _, uerr := o.pool.Exec(wctx, `
			UPDATE backups SET status = 'failed', finished_at = now(), error = $2 WHERE id = $1`, id, msg); uerr != nil {
			o.log.Error("не удалось записать итог бэкапа", "backup", id, "err", uerr)
		}
		return errors.New(msg)
	}
	mBackupResult.With("done").Inc()
	mBackupSeconds.Observe(time.Since(start).Seconds())
	if _, err := o.pool.Exec(wctx, `
		UPDATE backups SET status = 'done', finished_at = now(), file_path = $2, size_bytes = $3, sha256 = $4, error = NULL
		 WHERE id = $1`, id, path, size, sum); err != nil {
		o.log.Error("не удалось записать итог бэкапа", "backup", id, "err", err)
		return fmt.Errorf("ops: finish backup: %w", err)
	}
	o.log.Info("резервное копирование выполнено", "backup", id, "file", path, "size_bytes", size,
		"dur_ms", time.Since(start).Milliseconds())
	o.applyRetention(wctx)
	return nil
}

func backupErrorText(ctx context.Context, err error) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return truncateText("Превышено время копирования (30 мин): " + err.Error())
	case errors.Is(ctx.Err(), context.Canceled):
		return "Прервано: сервер останавливается"
	}
	return truncateText(err.Error())
}

// dump — pg_dump -Fc во временный .part, права 0600 (в копии — хэши паролей и ПДн),
// sha256 и размер одним последовательным чтением, затем атомарный rename.
func (o *Ops) dump(ctx context.Context, path string) (size int64, sum string, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, "", fmt.Errorf("каталог копий %s: %w", filepath.Dir(path), err)
	}
	bin, err := exec.LookPath(o.cfg.PgDumpPath)
	if err != nil {
		return 0, "", fmt.Errorf("pg_dump не найден (GOCORE_PG_DUMP=%q): %w", o.cfg.PgDumpPath, err)
	}
	dsn, password := libpqDSN(o.cfg.DatabaseURL)
	part := path + ".part"
	cmd := exec.CommandContext(ctx, bin, "-Fc", "--no-owner", "-d", dsn, "-f", part)
	// пароль — через окружение, а не в argv (argv виден в ps всем пользователям хоста)
	cmd.Env = append(os.Environ(), "PGCONNECT_TIMEOUT=15", "PGAPPNAME=lct-gocore-backup")
	if password != "" {
		cmd.Env = append(cmd.Env, "PGPASSWORD="+password)
	}
	stderr := &tailBuffer{max: 4 << 10}
	cmd.Stderr = stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) } // дать pg_dump закрыть соединение
	cmd.WaitDelay = 10 * time.Second                                         // затем — SIGKILL
	if err := cmd.Run(); err != nil {
		_ = os.Remove(part)
		if s := strings.TrimSpace(stderr.String()); s != "" {
			return 0, "", fmt.Errorf("pg_dump: %v: %s", err, s)
		}
		return 0, "", fmt.Errorf("pg_dump: %w", err)
	}
	if err := os.Chmod(part, 0o600); err != nil {
		_ = os.Remove(part)
		return 0, "", fmt.Errorf("права на файл копии: %w", err)
	}
	size, sum, err = hashFile(part)
	if err != nil {
		_ = os.Remove(part)
		return 0, "", err
	}
	if err := os.Rename(part, path); err != nil {
		_ = os.Remove(part)
		return 0, "", fmt.Errorf("переименование копии: %w", err)
	}
	return size, sum, nil
}

func hashFile(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", fmt.Errorf("чтение копии: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.CopyBuffer(h, f, make([]byte, 256<<10))
	if err != nil {
		return 0, "", fmt.Errorf("sha256 копии: %w", err)
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// libpqDSN — DATABASE_URL для pg_dump: пароль вынимается (уйдёт в PGPASSWORD), параметры,
// которые понимает только pgx/pgxpool, убираются (libpq на них падает:
// «invalid URI query parameter»). Строка вида key=value передаётся как есть.
func libpqDSN(raw string) (dsn, password string) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return raw, ""
	}
	if u.User != nil {
		if p, ok := u.User.Password(); ok {
			password = p
			u.User = url.User(u.User.Username())
		}
	}
	q := u.Query()
	for k := range q {
		switch {
		case strings.HasPrefix(k, "pool_"),
			k == "default_query_exec_mode", k == "statement_cache_capacity", k == "description_cache_capacity",
			k == "min_read_buffer_size", k == "servicefile":
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), password
}

// applyRetention — хранить settings.backup.keep последних успешных копий: старые файлы и
// их строки удаляются; неудачные попытки старше 30 дней — тоже. Файл удаляется, только если
// лежит в каталоге копий (строку могли поправить руками — чужие пути не трогаем).
func (o *Ops) applyRetention(ctx context.Context) {
	keep := max(o.settings(ctx).Backup.Keep, 1)
	dir, err := filepath.Abs(o.cfg.BackupDir)
	if err != nil {
		return
	}
	rows, err := o.pool.Query(ctx, `
		SELECT id, file_path FROM backups WHERE status = 'done' ORDER BY started_at DESC OFFSET $1`, keep)
	if err != nil {
		o.log.Warn("ретеншн бэкапов: выборка", "err", err)
		return
	}
	var drop []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		var p *string
		if err := rows.Scan(&id, &p); err != nil {
			rows.Close()
			o.log.Warn("ретеншн бэкапов: чтение", "err", err)
			return
		}
		if p != nil && *p != "" {
			if !insideDir(dir, *p) {
				o.log.Warn("ретеншн бэкапов: файл вне каталога копий не удаляется", "backup", id, "file", *p)
			} else if err := os.Remove(*p); err != nil && !errors.Is(err, os.ErrNotExist) {
				o.log.Warn("ретеншн бэкапов: файл не удалён — строка сохранена до следующей попытки", "backup", id, "err", err)
				continue
			}
		}
		drop = append(drop, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		o.log.Warn("ретеншн бэкапов: выборка", "err", err)
		return
	}
	if len(drop) > 0 {
		if _, err := o.pool.Exec(ctx, `DELETE FROM backups WHERE id = ANY($1)`, drop); err != nil {
			o.log.Warn("ретеншн бэкапов: удаление строк", "err", err)
		} else {
			o.log.Info("удалены старые резервные копии", "count", len(drop), "keep", keep)
		}
	}
	if _, err := o.pool.Exec(ctx, `
		DELETE FROM backups WHERE status = 'failed' AND started_at < now() - make_interval(days => $1::int)`,
		failedBackupsKeepDays); err != nil {
		o.log.Warn("ретеншн бэкапов: неудачные попытки", "err", err)
	}
}

// recoverInterruptedBackups — при старте: running-строки, начатые до запуска процесса,
// принадлежали прошлому (убитому) процессу — помечаем failed, чтобы кнопка «Запустить» не
// ждала 2 часа; недописанные .part удаляем. Допущение: go-core — один экземпляр (db-design §4).
func (o *Ops) recoverInterruptedBackups(ctx context.Context) {
	tag, err := o.pool.Exec(ctx, `
		UPDATE backups SET status = 'failed', finished_at = now(),
		       error = 'Прервано: сервер был перезапущен во время копирования'
		 WHERE status = 'running' AND started_at < $1`, o.started)
	if err != nil {
		if ctx.Err() == nil {
			o.log.Warn("пометка прерванных бэкапов", "err", err)
		}
		return
	}
	if n := tag.RowsAffected(); n > 0 {
		o.log.Warn("найдены прерванные резервные копирования", "count", n)
	}
	dir, err := filepath.Abs(o.cfg.BackupDir)
	if err != nil {
		return
	}
	parts, _ := filepath.Glob(filepath.Join(dir, "lct-*.dump.part"))
	for _, p := range parts {
		_ = os.Remove(p)
	}
}

func insideDir(dir, p string) bool {
	rel, err := filepath.Rel(dir, filepath.Clean(p))
	return err == nil && rel != "." && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// truncateText — не длиннее maxBackupError байт по границе руны, валидный UTF-8 без NUL
// (сообщения pg_dump бывают в локальной кодировке).
func truncateText(s string) string {
	s = strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), string(utf8.RuneError))
	if len(s) <= maxBackupError {
		return s
	}
	cut := maxBackupError
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// tailBuffer — последние max байт вывода (stderr pg_dump для текста ошибки).
type tailBuffer struct {
	b   []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append(t.b[:0], t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }
