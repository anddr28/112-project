package ops

import (
	"context"
	"testing"
	"time"
)

// Start: при старте — пометка прерванных бэкапов и партиции аудита; остановка — отменой ctx.
func TestStartAndWait(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	e.exec(t, `INSERT INTO backups (started_at, status) VALUES (now() - interval '1 minute', 'running')`)
	now := time.Now().UTC()
	part := time.Date(now.Year(), now.Month()+2, 1, 0, 0, 0, 0, time.UTC).Format("audit_log_2006_01")
	e.exec(t, `DROP TABLE IF EXISTS `+part) // одна из партиций «вперёд» пропала
	ctx, cancel := context.WithCancel(context.Background())
	e.o.Start(ctx)
	ready := func() bool {
		return e.count(t, `SELECT count(*) FROM backups WHERE status = 'running'`) == 0 &&
			e.count(t, `SELECT count(*) FROM pg_class WHERE relname = $1`, part) == 1
	}
	for deadline := time.Now().Add(10 * time.Second); !ready() && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if !ready() {
		t.Fatal("Start не выполнил стартовые шаги")
	}
	if n := e.count(t, `SELECT count(*) FROM backups WHERE status = 'failed' AND error LIKE 'Прервано%'`); n != 1 {
		t.Fatalf("прерванный бэкап не помечен: %d", n)
	}
	cancel()
	wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer wcancel()
	if err := e.o.Wait(wctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if e.o.baseCtx() != ctx {
		t.Fatal("фоновые бэкапы должны жить в ctx сервиса")
	}
}

func TestWaitTimeout(t *testing.T) {
	t.Parallel()
	o := New(Deps{Log: discardLog()})
	o.wg.Add(1)
	defer o.wg.Done()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := o.Wait(ctx); err == nil {
		t.Fatal("Wait должен вернуть ошибку по таймауту")
	}
}

func TestSettingsDefaultsWithoutStore(t *testing.T) {
	t.Parallel()
	o := New(Deps{})
	s := o.settings(context.Background())
	if s == nil || !s.Backup.Enabled || s.Backup.Hour != 3 || s.AuditRetentionDays < 180 {
		t.Fatalf("defaults = %+v", s)
	}
	if o.log == nil {
		t.Fatal("логгер по умолчанию")
	}
}
