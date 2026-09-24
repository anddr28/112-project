package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"lct/gocore/internal/gen/public"
)

func TestLibpqDSN(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, dsn, pass string
	}{
		{"postgres://lct:secret@db:5432/lct?sslmode=disable", "postgres://lct@db:5432/lct?sslmode=disable", "secret"},
		{"postgresql://u:p%40ss%2Fw@h/d", "postgresql://u@h/d", "p@ss/w"},
		{"postgres://u@h/d?pool_max_conns=10&default_query_exec_mode=exec&statement_cache_capacity=1&sslmode=require",
			"postgres://u@h/d?sslmode=require", ""},
		{"postgres://h/d", "postgres://h/d", ""},
		{"host=db user=lct password=x dbname=lct", "host=db user=lct password=x dbname=lct", ""},
		{"mysql://u:p@h/d", "mysql://u:p@h/d", ""},
	}
	for _, tt := range tests {
		dsn, pass := libpqDSN(tt.in)
		if dsn != tt.dsn || pass != tt.pass {
			t.Errorf("libpqDSN(%q) = %q,%q want %q,%q", tt.in, dsn, pass, tt.dsn, tt.pass)
		}
	}
}

func TestInsideDir(t *testing.T) {
	t.Parallel()
	dir := filepath.FromSlash("/data/backups")
	tests := map[string]bool{
		"/data/backups/lct-1.dump":        true,
		"/data/backups/sub/lct.dump":      true,
		"/data/backups":                   false,
		"/data/backups/../etc/passwd":     false,
		"/data/backups2/lct.dump":         false,
		"/etc/passwd":                     false,
		"/data/backups/./x/../lct.dump":   true,
		"/data/backups/..hidden/lct.dump": true,
	}
	for p, want := range tests {
		if got := insideDir(dir, filepath.FromSlash(p)); got != want {
			t.Errorf("insideDir(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestTruncateText(t *testing.T) {
	t.Parallel()
	if got := truncateText("ok"); got != "ok" {
		t.Fatalf("got %q", got)
	}
	if got := truncateText("a\x00b\xffc"); got != "ab�c" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("ошибка ", 200)
	got := truncateText(long)
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "…") || len(got) > maxBackupError+len("…") {
		t.Fatalf("len=%d valid=%v", len(got), utf8.ValidString(got))
	}
	if !strings.HasPrefix(long, strings.TrimSuffix(got, "…")) {
		t.Fatal("обрезка не по префиксу")
	}
}

func TestTailBuffer(t *testing.T) {
	t.Parallel()
	b := &tailBuffer{max: 8}
	for _, s := range []string{"hello ", "world", "!!"} {
		if n, err := b.Write([]byte(s)); err != nil || n != len(s) {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if got := b.String(); got != " world!!" { // последние 8 байт "hello world!!"
		t.Fatalf("tail = %q", got)
	}
}

func TestBackupErrorText(t *testing.T) {
	t.Parallel()
	base := errors.New("pg_dump: exit status 1")
	dl, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if got := backupErrorText(dl, base); !strings.HasPrefix(got, "Превышено время копирования") || !strings.Contains(got, "exit status 1") {
		t.Errorf("deadline: %q", got)
	}
	cc, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if got := backupErrorText(cc, base); got != "Прервано: сервер останавливается" {
		t.Errorf("canceled: %q", got)
	}
	if got := backupErrorText(context.Background(), base); got != base.Error() {
		t.Errorf("plain: %q", got)
	}
}

func TestHashFile(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "f")
	data := []byte(strings.Repeat("резервная копия ", 50000))
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	n, sum, err := hashFile(p)
	want := sha256.Sum256(data)
	if err != nil || n != int64(len(data)) || sum != hex.EncodeToString(want[:]) {
		t.Fatalf("hashFile = %d, %s, %v", n, sum, err)
	}
	if _, _, err := hashFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("want error")
	}
}

// waitBackup — ждать, пока строка перестанет быть running.
func waitBackup(t *testing.T, e *env, id uuid.UUID) public.Backup {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		list, err := e.o.ListBackups(ctxT(t))
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range list {
			if b.Id == id && b.Status != public.BackupStatusRunning {
				return b
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("бэкап %s не завершился", id)
	return public.Backup{}
}

func TestStartBackupSuccess(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	if list, err := e.o.ListBackups(ctxT(t)); err != nil || list == nil || len(list) != 0 {
		t.Fatalf("пустой журнал: %v %v", list, err)
	}
	if at, err := e.o.LastBackupAt(ctxT(t)); err != nil || at != nil {
		t.Fatalf("LastBackupAt = %v, %v", at, err)
	}
	admin := uuid.New()
	e.exec(t, `INSERT INTO users (id, login, password_hash, role, last_name, first_name) VALUES ($1, 'adm', 'x', 'admin', 'Админ', 'Иван')`, admin)

	b, err := e.o.StartBackup(ctxT(t), &admin)
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != public.BackupStatusRunning || b.TriggeredBy == nil || *b.TriggeredBy != admin || b.StartedAt.Location() != time.UTC {
		t.Fatalf("started = %+v", b)
	}
	got := waitBackup(t, e, b.Id)
	if got.Status != public.BackupStatusDone || got.FilePath == nil || got.SizeBytes == nil || got.Sha256 == nil || got.FinishedAt == nil || got.Error != nil {
		t.Fatalf("done = %+v", got)
	}
	dir, _ := filepath.Abs(e.cfg.BackupDir)
	if !insideDir(dir, *got.FilePath) || !strings.HasPrefix(filepath.Base(*got.FilePath), "lct-") || !strings.HasSuffix(*got.FilePath, ".dump") {
		t.Fatalf("file = %s", *got.FilePath)
	}
	data, err := os.ReadFile(*got.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if *got.Sha256 != hex.EncodeToString(sum[:]) || *got.SizeBytes != int64(len(data)) {
		t.Fatal("sha256/size не совпадают с файлом")
	}
	// пароль — через PGPASSWORD, не в argv; параметры pgx вычищены
	parts := strings.SplitN(string(data), "|", 4)
	if len(parts) != 4 || parts[1] != "lct" || parts[2] != "lct-gocore-backup" {
		t.Fatalf("dump = %q", data)
	}
	if strings.Contains(parts[3], ":lct@") || !strings.Contains(parts[3], "-Fc") || !strings.Contains(parts[3], "--no-owner") {
		t.Fatalf("argv = %q", parts[3])
	}
	if fi, err := os.Stat(*got.FilePath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("права файла копии: %v %v", fi.Mode(), err)
	}
	if at, err := e.o.LastBackupAt(ctxT(t)); err != nil || at == nil || !at.Equal(*got.FinishedAt) {
		t.Fatalf("LastBackupAt = %v, %v", at, err)
	}
	// аудит backup.run: ручной запуск — актор-админ
	aud := e.aud.all()
	if len(aud) != 1 || aud[0].Action != "backup.run" || aud[0].EntityID != b.Id || aud[0].ActorSystem || aud[0].ActorID == nil || *aud[0].ActorID != admin {
		t.Fatalf("audit = %+v", aud)
	}
	// контракт Backup: camelCase, обязательные поля
	raw, _ := json.Marshal(got)
	for _, k := range []string{`"id"`, `"startedAt"`, `"status":"done"`, `"sha256"`, `"sizeBytes"`, `"triggeredBy"`} {
		if !strings.Contains(string(raw), k) {
			t.Errorf("нет %s в %s", k, raw)
		}
	}
}

func TestStartBackupConflictAndShutdown(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	e.cfg.PgDumpPath = writeScript(t, fakeDumpSlow)
	ctx, cancel := context.WithCancel(context.Background())
	e.o.mu.Lock()
	e.o.base = ctx // как после Start: фоновый бэкап живёт до остановки сервиса
	e.o.mu.Unlock()

	b, err := e.o.StartBackup(ctxT(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.o.StartBackup(ctxT(t), nil); !errors.Is(err, ErrBackupRunning) {
		t.Fatalf("второй запуск: %v, want ErrBackupRunning", err)
	}
	cancel() // остановка сервиса: pg_dump получает SIGTERM
	wctx, wcancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer wcancel()
	if err := e.o.Wait(wctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	got := waitBackup(t, e, b.Id)
	if got.Status != public.BackupStatusFailed || got.Error == nil || *got.Error != "Прервано: сервер останавливается" {
		t.Fatalf("got %+v", got)
	}
	if got.TriggeredBy != nil {
		t.Fatal("плановый бэкап без triggeredBy")
	}
	parts, _ := filepath.Glob(filepath.Join(e.cfg.BackupDir, "*.part"))
	if len(parts) != 0 {
		t.Fatalf("недописанные файлы остались: %v", parts)
	}
	if aud := e.aud.all(); len(aud) != 1 || !aud[0].ActorSystem {
		t.Fatalf("audit = %+v", aud)
	}
}

func TestRunBackupSyncFailures(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	e.cfg.PgDumpPath = writeScript(t, fakeDumpFail)
	got, err := e.o.RunBackupSync(ctxT(t), nil)
	if err == nil || got.Status != public.BackupStatusFailed || got.Error == nil || !strings.Contains(*got.Error, "Connection refused") {
		t.Fatalf("fail script: %+v, %v", got, err)
	}
	e.cfg.PgDumpPath = filepath.Join(t.TempDir(), "no-such-pg_dump")
	got, err = e.o.RunBackupSync(ctxT(t), nil)
	if err == nil || got.Error == nil || !strings.Contains(*got.Error, "pg_dump не найден") {
		t.Fatalf("missing binary: %+v, %v", got, err)
	}
	if n := e.count(t, `SELECT count(*) FROM backups WHERE status = 'failed'`); n != 2 {
		t.Fatalf("failed rows = %d", n)
	}
}

func TestBeginBackupMarksStaleRunning(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	stale := uuid.New()
	e.exec(t, `INSERT INTO backups (id, started_at, status) VALUES ($1, now() - interval '3 hours', 'running')`, stale)
	got, err := e.o.RunBackupSync(ctxT(t), nil)
	if err != nil || got.Status != public.BackupStatusDone {
		t.Fatalf("got %+v, %v", got, err)
	}
	var status, msg string
	if err := e.pool.QueryRow(ctxT(t), `SELECT status, error FROM backups WHERE id = $1`, stale).Scan(&status, &msg); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || !strings.HasPrefix(msg, "Прервано") {
		t.Fatalf("stale = %s %q", status, msg)
	}
}

func TestBackupFileNameCollision(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	dir, _ := filepath.Abs(e.cfg.BackupDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// занять имена на несколько секунд вокруг «сейчас»: копия не должна их затереть
	now := time.Now()
	for d := -3; d <= 5; d++ {
		p := filepath.Join(dir, "lct-"+now.Add(time.Duration(d)*time.Second).Format("20060102-150405")+".dump")
		if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := e.o.RunBackupSync(ctxT(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(*got.FilePath)
	if strings.Count(base, "-") != 3 { // lct-YYYYMMDD-HHMMSS-<хвост uuid>.dump
		t.Fatalf("имя без суффикса: %s", base)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "lct-*.dump"))
	for _, m := range matches {
		if m == *got.FilePath {
			continue
		}
		if b, _ := os.ReadFile(m); string(b) != "old" {
			t.Fatalf("затёрт чужой файл %s", m)
		}
	}
}

func TestBackupRetention(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	e.exec(t, `UPDATE settings SET value = '{"enabled": true, "hour": 3, "keep": 2}' WHERE key = 'backup'`)
	e.o = New(Deps{Pool: e.pool, Config: e.cfg, Settings: newStore(e), Log: discardLog()})

	outside := filepath.Join(t.TempDir(), "manual.dump")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.exec(t, `INSERT INTO backups (started_at, finished_at, status, file_path) VALUES
	            (now() - interval '10 days', now() - interval '10 days', 'done', $1),
	            (now() - interval '40 days', now() - interval '40 days', 'failed', NULL),
	            (now() - interval '5 days', now() - interval '5 days', 'failed', NULL)`, outside)

	var files []string
	for range 3 {
		got, err := e.o.RunBackupSync(ctxT(t), nil)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, *got.FilePath)
		time.Sleep(10 * time.Millisecond)
	}
	if n := e.count(t, `SELECT count(*) FROM backups WHERE status = 'done'`); n != 2 {
		t.Fatalf("done rows = %d, want keep=2", n)
	}
	if exists(files[0]) || !exists(files[1]) || !exists(files[2]) {
		t.Fatalf("файлы: %v %v %v", exists(files[0]), exists(files[1]), exists(files[2]))
	}
	if !exists(outside) {
		t.Fatal("файл вне каталога копий удалён")
	}
	if n := e.count(t, `SELECT count(*) FROM backups WHERE status = 'failed'`); n != 1 {
		t.Fatalf("failed rows = %d, want 1 (старше %d дней удаляются)", n, failedBackupsKeepDays)
	}
}

func TestRecoverInterruptedBackups(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	dir, _ := filepath.Abs(e.cfg.BackupDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old, fresh := uuid.New(), uuid.New()
	e.exec(t, `INSERT INTO backups (id, started_at, status) VALUES ($1, now() - interval '5 minutes', 'running')`, old)
	e.exec(t, `INSERT INTO backups (id, started_at, status) VALUES ($1, now() + interval '1 minute', 'running')`, fresh)
	part := filepath.Join(dir, "lct-20260101-030000.dump.part")
	keep := filepath.Join(dir, "lct-20260101-030000.dump")
	for _, p := range []string{part, keep} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e.o.recoverInterruptedBackups(ctxT(t))
	st := func(id uuid.UUID) string {
		var s string
		if err := e.pool.QueryRow(ctxT(t), `SELECT status FROM backups WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if st(old) != "failed" || st(fresh) != "running" {
		t.Fatalf("old=%s fresh=%s", st(old), st(fresh))
	}
	if exists(part) || !exists(keep) {
		t.Fatalf(".part=%v dump=%v", exists(part), exists(keep))
	}
}

func TestMaybeScheduledBackup(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	wctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	today := time.Now()
	at := func(day, h, m int) time.Time {
		return time.Date(today.Year(), today.Month(), today.Day()+day, h, m, 0, 0, today.Location())
	}
	rows := func(e *env) int { return e.count(t, `SELECT count(*) FROM backups`) }

	e.o.maybeScheduledBackup(ctxT(t), at(0, 0, 30), 23) // до часа X — ничего
	if rows(e) != 0 {
		t.Fatal("бэкап раньше расписания")
	}
	e.o.maybeScheduledBackup(ctxT(t), at(0, 12, 0), 0) // час X прошёл, копий с него нет — запуск
	if err := e.o.Wait(wctx); err != nil {
		t.Fatal(err)
	}
	if rows(e) != 1 || e.o.backupTries != 1 {
		t.Fatalf("rows=%d tries=%d", rows(e), e.o.backupTries)
	}
	if n := e.count(t, `SELECT count(*) FROM backups WHERE status = 'done' AND triggered_by IS NULL`); n != 1 {
		t.Fatal("плановая копия не выполнена")
	}
	// копия с часа X уже есть — через час повтор не нужен
	e.o.maybeScheduledBackup(ctxT(t), at(0, 13, 1), 0)
	_ = e.o.Wait(wctx)
	if rows(e) != 1 {
		t.Fatalf("лишняя копия: %d", rows(e))
	}

	// неудачи: повтор не чаще раза в час и не больше maxScheduledBackupTries за сутки;
	// час вне 0..23 — как 3:00. День — в прошлом: копии «сегодня» (реального) после него
	// не должно быть успешных, а они и не успешны.
	e2 := newEnv(t, false)
	e2.cfg.PgDumpPath = writeScript(t, fakeDumpFail)
	for i := range 8 {
		e2.o.maybeScheduledBackup(ctxT(t), at(-30, 4, 0).Add(time.Duration(i)*30*time.Minute), 99)
		_ = e2.o.Wait(wctx)
	}
	if n := rows(e2); n != maxScheduledBackupTries {
		t.Fatalf("попыток %d, want %d", n, maxScheduledBackupTries)
	}
	e2.o.maybeScheduledBackup(ctxT(t), at(-29, 4, 0), 99) // новые сутки — счётчик сброшен
	_ = e2.o.Wait(wctx)
	if n := rows(e2); n != maxScheduledBackupTries+1 {
		t.Fatalf("после смены суток попыток %d", n)
	}
	e2.o.maybeScheduledBackup(ctxT(t), at(-28, 2, 59), 99) // до 3:00 — рано
	_ = e2.o.Wait(wctx)
	if n := rows(e2); n != maxScheduledBackupTries+1 {
		t.Fatalf("запуск до 3:00: %d", n)
	}
}
