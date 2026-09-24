package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func mkAudio(t *testing.T, root, rel string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("RIFF....WAVE"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func age(t *testing.T, p string, d time.Duration) {
	t.Helper()
	old := time.Now().Add(-d)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
}

func (e *env) addTurn(t *testing.T, attempt uuid.UUID, turn int, speaker, audio string) {
	t.Helper()
	var ap *string
	if audio != "" {
		ap = &audio
	}
	e.exec(t, `INSERT INTO attempt_dialogue_turns (attempt_id, turn_no, speaker, text, source, audio_path)
	            VALUES ($1, $2, $3, 'Алло, у нас пожар!', 'llm', $4)`, attempt, turn, speaker, ap)
}

func (e *env) audioPaths(t *testing.T, attempt uuid.UUID) []string {
	t.Helper()
	rows, err := e.pool.Query(ctxT(t), `
		SELECT COALESCE(audio_path, '') FROM attempt_dialogue_turns WHERE attempt_id = $1 ORDER BY turn_no, speaker`, attempt)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// Находка ревью: озвучка реплик диалога (dialog/<attempt>/<turn>.wav) копилась в volume
// без предела. Теперь ежедневное обслуживание удаляет её у завершённых попыток.
func TestCleanupDialogAudio(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	s := e.seedBase(t)
	root := e.cfg.TTSDir
	finished := e.seedLesson(t, s, "finished")
	running := e.seedLesson(t, s, "running")

	done := e.seedAttempt(t, s, finished, "evaluated", time.Minute)     // занятие закрыто — чистим
	live := e.seedAttempt(t, s, running, "in_progress", 3*24*time.Hour) // звонок идёт — не трогаем
	stale := e.seedAttempt(t, s, running, "submitted", 2*24*time.Hour)  // занятие не закрыли, прошли сутки
	fresh := e.seedAttempt(t, s, running, "submitted", time.Hour)       // только что сдана — ждём
	cancelled := e.seedAttempt(t, s, e.seedLesson(t, s, "cancelled"), "aborted", time.Minute)
	stuck := e.seedAttempt(t, s, finished, "in_progress", time.Minute)       // занятие закрыто, статус завис
	abandoned := e.seedAttempt(t, s, running, "in_progress", 8*24*time.Hour) // браузер закрыли, неделя тишины

	opening := "tts/ab/abcdef0123456789.wav" // вступление — общий кэш озвучки
	mkAudio(t, root, opening)
	for _, a := range []uuid.UUID{done, live, stale, fresh, cancelled, stuck, abandoned} {
		e.addTurn(t, a, 0, "caller", opening)
		for turn := 1; turn <= 2; turn++ {
			rel := "dialog/" + a.String() + "/" + string(rune('0'+turn)) + ".wav"
			mkAudio(t, root, rel)
			e.addTurn(t, a, turn, "operator", "")
			e.addTurn(t, a, turn, "caller", rel)
		}
	}
	orphanOld, orphanNew := uuid.New(), uuid.New()
	mkAudio(t, root, "dialog/"+orphanOld.String()+"/1.wav")
	age(t, filepath.Join(root, "dialog", orphanOld.String()), 48*time.Hour)
	mkAudio(t, root, "dialog/"+orphanNew.String()+"/1.wav")
	// чужое содержимое dialog/ не трогаем
	mkAudio(t, root, "dialog/not-a-uuid/1.wav")
	upper := strings.ToUpper(uuid.New().String())
	mkAudio(t, root, "dialog/"+upper+"/1.wav")
	age(t, filepath.Join(root, "dialog", upper), 48*time.Hour)
	mkAudio(t, root, "dialog/loose.wav")
	// симлинк с именем-UUID наружу: удаление не должно пройти по нему
	outside := t.TempDir()
	mkAudio(t, outside, "keep/secret.wav")
	link := uuid.New().String()
	if err := os.Symlink(filepath.Join(outside, "keep"), filepath.Join(root, "dialog", link)); err != nil {
		t.Fatal(err)
	}

	n, err := e.o.cleanupDialogAudio(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if n != 6 { // done, stale, cancelled, stuck, abandoned, orphanOld
		t.Fatalf("removed = %d, want 6", n)
	}
	dir := func(id uuid.UUID) string { return filepath.Join(root, "dialog", id.String()) }
	for _, id := range []uuid.UUID{done, stale, cancelled, stuck, abandoned, orphanOld} {
		if exists(dir(id)) {
			t.Errorf("каталог %s должен быть удалён", id)
		}
	}
	for _, id := range []uuid.UUID{live, fresh, orphanNew} {
		if !exists(filepath.Join(dir(id), "1.wav")) {
			t.Errorf("каталог %s удалён зря", id)
		}
	}
	for _, rel := range []string{opening, "dialog/not-a-uuid/1.wav", "dialog/" + upper + "/1.wav", "dialog/loose.wav", "dialog/" + link} {
		if !exists(filepath.Join(root, filepath.FromSlash(rel))) {
			t.Errorf("%s удалён зря", rel)
		}
	}
	if !exists(filepath.Join(outside, "keep", "secret.wav")) {
		t.Fatal("удаление прошло по симлинку за пределы TTS_DIR")
	}

	// ссылки на удалённые файлы обнулены, вступление (общий кэш) и живые попытки — как были
	for _, id := range []uuid.UUID{done, stale, cancelled, stuck, abandoned} {
		got := strings.Join(e.audioPaths(t, id), ",")
		if got != opening+",,,," {
			t.Errorf("%s: audio paths = %q", id, got)
		}
	}
	for _, id := range []uuid.UUID{live, fresh} {
		for _, p := range e.audioPaths(t, id)[1:] {
			if p != "" && !strings.HasPrefix(p, "dialog/"+id.String()+"/") {
				t.Errorf("%s: path %q", id, p)
			}
		}
		if cnt := e.count(t, `SELECT count(*) FROM attempt_dialogue_turns WHERE attempt_id = $1 AND audio_path LIKE 'dialog/%'`, id); cnt != 2 {
			t.Errorf("%s: dialog paths = %d, want 2", id, cnt)
		}
	}
	// транскрипт не тронут
	if cnt := e.count(t, `SELECT count(*) FROM attempt_dialogue_turns WHERE attempt_id = $1`, done); cnt != 5 {
		t.Fatalf("реплик = %d, want 5", cnt)
	}

	// повторный проход — ничего нового
	if n, err := e.o.cleanupDialogAudio(ctxT(t)); err != nil || n != 0 {
		t.Fatalf("second pass: %d, %v", n, err)
	}
}

func TestCleanupDialogAudioNoDirs(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	if n, err := e.o.cleanupDialogAudio(ctxT(t)); err != nil || n != 0 {
		t.Fatalf("empty volume: %d, %v", n, err)
	}
	e.cfg.TTSDir = filepath.Join(t.TempDir(), "missing")
	if n, err := e.o.cleanupDialogAudio(ctxT(t)); err != nil || n != 0 {
		t.Fatalf("missing volume: %d, %v", n, err)
	}
	e.cfg.TTSDir = ""
	if n, err := e.o.cleanupDialogAudio(ctxT(t)); err != nil || n != 0 {
		t.Fatalf("no volume: %d, %v", n, err)
	}
}

// Больше dialogAudioChunk каталогов — несколько запросов к БД, удаляются все.
func TestCleanupDialogAudioChunks(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	root := e.cfg.TTSDir
	total := dialogAudioChunk + 37
	for range total {
		d := filepath.Join(root, "dialog", uuid.New().String())
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		age(t, d, 72*time.Hour)
	}
	n, err := e.o.cleanupDialogAudio(ctxT(t))
	if err != nil || n != total {
		t.Fatalf("removed %d (%v), want %d", n, err, total)
	}
}

func TestCleanupDialogAudioRemoveError(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root игнорирует права каталога")
	}
	e := newEnv(t, false)
	s := e.seedBase(t)
	a := e.seedAttempt(t, s, e.seedLesson(t, s, "finished"), "evaluated", time.Minute)
	rel := "dialog/" + a.String() + "/1.wav"
	mkAudio(t, e.cfg.TTSDir, rel)
	e.addTurn(t, a, 1, "caller", rel)
	d := filepath.Join(e.cfg.TTSDir, "dialog", a.String())
	if err := os.Chmod(d, 0o500); err != nil { // файл внутри не удалить
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(d, 0o755) })

	n, err := e.o.cleanupDialogAudio(ctxT(t))
	if err == nil || n != 0 {
		t.Fatalf("want error, got %d, %v", n, err)
	}
	// ссылка всё равно обнулена: лишний файл лучше ссылки в пустоту
	if p := e.audioPaths(t, a); len(p) != 1 || p[0] != "" {
		t.Fatalf("audio paths = %v", p)
	}
}

func TestCleanupDialogAudioCanceled(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	mkAudio(t, e.cfg.TTSDir, "dialog/"+uuid.New().String()+"/1.wav")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.o.cleanupDialogAudio(ctx); err == nil {
		t.Fatal("want ctx error")
	}
}

func TestRunMaintenanceRetention(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	s := e.seedBase(t)
	a := e.seedAttempt(t, s, e.seedLesson(t, s, "finished"), "evaluated", time.Minute)

	// события попытки: ретеншн 365 дней по умолчанию
	e.exec(t, `INSERT INTO attempt_events (attempt_id, type, at) VALUES
	            ($1, 'issued', now() - interval '400 days'), ($1, 'submitted', now() - interval '10 days')`, a)
	// сессии
	e.exec(t, `INSERT INTO auth_sessions (user_id, refresh_token_hash, expires_at, revoked_at) VALUES
	            ($1, 'h1', now() - interval '10 days', NULL),
	            ($1, 'h2', now() - interval '1 day', NULL),
	            ($1, 'h3', now() + interval '1 day', now() - interval '10 days'),
	            ($1, 'h4', now() + interval '1 day', NULL)`, s.user)
	// AI-задачи
	job := func(status string, finishedAgo, createdAgo string) {
		t.Helper()
		e.exec(t, `INSERT INTO ai_jobs (type, status, payload, created_at, finished_at)
		            VALUES ('evaluate_grammar', $1, '{}', now() - $3::interval,
		                    CASE WHEN $2 = '' THEN NULL ELSE now() - $2::interval END)`, status, finishedAgo, createdAgo)
	}
	job("done", "40 days", "41 days")     // удалить
	job("done", "10 days", "11 days")     // оставить
	job("cancelled", "", "45 days")       // удалить (finished_at NULL -> created_at)
	job("failed", "40 days", "41 days")   // оставить: проваленные держим 90 дней
	job("failed", "100 days", "101 days") // удалить (раньше failed копились вечно)
	job("queued", "", "200 days")         // живая задача — не трогать
	job("running", "", "200 days")        // живая задача — не трогать
	// озвучка завершённой попытки
	rel := "dialog/" + a.String() + "/1.wav"
	mkAudio(t, e.cfg.TTSDir, rel)
	e.addTurn(t, a, 1, "caller", rel)

	if err := e.o.RunMaintenance(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT count(*) FROM attempt_events WHERE attempt_id = $1`, a); n != 1 {
		t.Errorf("attempt_events = %d, want 1", n)
	}
	var tokens string
	if err := e.pool.QueryRow(ctxT(t), `SELECT string_agg(refresh_token_hash, ',' ORDER BY refresh_token_hash) FROM auth_sessions`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if tokens != "h2,h4" {
		t.Errorf("sessions = %s, want h2,h4", tokens)
	}
	var jobs string
	if err := e.pool.QueryRow(ctxT(t), `
		SELECT string_agg(status || ':' || COALESCE(extract(day FROM now() - finished_at)::int::text, '-'), ',' ORDER BY status, finished_at)
		  FROM ai_jobs`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != "done:10,failed:40,queued:-,running:-" {
		t.Errorf("ai_jobs = %s", jobs)
	}
	if exists(filepath.Join(e.cfg.TTSDir, filepath.FromSlash(rel))) {
		t.Error("озвучка завершённой попытки не удалена")
	}
	// партиции аудита — шаг обслуживания
	if n := e.count(t, `SELECT count(*) FROM pg_inherits WHERE inhparent = 'audit_log'::regclass`); n < 5 {
		t.Errorf("audit partitions = %d", n)
	}
}

func TestRunMaintenanceEventsRetentionSetting(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	s := e.seedBase(t)
	a := e.seedAttempt(t, s, e.seedLesson(t, s, "finished"), "evaluated", time.Minute)
	e.exec(t, `INSERT INTO attempt_events (attempt_id, type, at) VALUES
	            ($1, 'issued', now() - interval '20 days'), ($1, 'x', now() - interval '40 days'), ($1, 'y', now() - interval '5000 days')`, a)

	// 0 — хранить бессрочно
	e.exec(t, `UPDATE settings SET value = '0' WHERE key = 'events_retention_days'`)
	o := New(Deps{Pool: e.pool, Config: e.cfg, Settings: newStore(e), Log: discardLog()})
	if err := o.RunMaintenance(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT count(*) FROM attempt_events`); n != 3 {
		t.Fatalf("retention 0: events = %d, want 3", n)
	}
	// опечатка «5 дней» поднимается до minEventsRetentionDays (30)
	e.exec(t, `UPDATE settings SET value = '5' WHERE key = 'events_retention_days'`)
	o = New(Deps{Pool: e.pool, Config: e.cfg, Settings: newStore(e), Log: discardLog()})
	if err := o.RunMaintenance(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT count(*) FROM attempt_events`); n != 1 {
		t.Fatalf("retention 5->30: events = %d, want 1", n)
	}
}

func TestBatchedDeleteLoops(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	e.exec(t, `INSERT INTO ai_jobs (type, status, payload, created_at, finished_at)
	            SELECT 'tts', 'done', '{}', now() - interval '60 days', now() - interval '60 days' FROM generate_series(1, $1::int)`,
		deleteBatch+10)
	n, err := e.o.batchedDelete(ctxT(t), "ai_jobs", `
		DELETE FROM ai_jobs WHERE id IN (SELECT id FROM ai_jobs WHERE status = 'done' LIMIT $1)`, deleteBatch)
	if err != nil || n != deleteBatch+10 {
		t.Fatalf("deleted %d (%v), want %d", n, err, deleteBatch+10)
	}
	if _, err := e.o.batchedDelete(ctxT(t), "nope", `DELETE FROM no_such_table`); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("want error, got %v", err)
	}
}

func TestTickRunsMaintenanceOncePerDay(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	e.exec(t, `UPDATE settings SET value = '{"enabled": false, "hour": 3, "keep": 14}' WHERE key = 'backup'`)
	o := New(Deps{Pool: e.pool, Config: e.cfg, Settings: newStore(e), Log: discardLog()})
	oldJob := func() {
		t.Helper()
		e.exec(t, `INSERT INTO ai_jobs (type, status, payload, finished_at) VALUES ('tts', 'done', '{}', now() - interval '60 days')`)
	}
	jobs := func() int { return e.count(t, `SELECT count(*) FROM ai_jobs`) }

	early := time.Date(2026, 9, 24, maintHour, maintMinute-1, 0, 0, time.Local)
	oldJob()
	o.tick(ctxT(t), early)
	if jobs() != 1 || o.maintDay != "" {
		t.Fatalf("обслуживание раньше %02d:%02d", maintHour, maintMinute)
	}
	o.tick(ctxT(t), early.Add(2*time.Minute))
	if jobs() != 0 || o.maintDay != "2026-09-24" {
		t.Fatalf("обслуживание не выполнено: jobs=%d day=%q", jobs(), o.maintDay)
	}
	oldJob()
	o.tick(ctxT(t), early.Add(5*time.Hour)) // тот же день — второй раз не запускается
	if jobs() != 1 {
		t.Fatal("обслуживание запущено дважды за день")
	}
	o.tick(ctxT(t), early.Add(24*time.Hour+time.Minute)) // следующий день, после 04:30
	if jobs() != 0 || o.maintDay != "2026-09-25" {
		t.Fatalf("обслуживание следующего дня: jobs=%d day=%q", jobs(), o.maintDay)
	}
	if n := e.count(t, `SELECT count(*) FROM backups`); n != 0 {
		t.Fatalf("бэкап выключен, а строк: %d", n)
	}
}
