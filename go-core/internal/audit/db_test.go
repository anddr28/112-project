package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/core"
	"lct/gocore/internal/platform/pgtest"
)

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func stopWriter(t *testing.T, w *Writer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := w.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func countAudit(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctxT(t), `SELECT count(*) FROM audit_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func seedUser(t *testing.T, pool *pgxpool.Pool, login, role, last, first, middle string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var mid *string
	if middle != "" {
		mid = &middle
	}
	if _, err := pool.Exec(ctxT(t), `
		INSERT INTO users (id, login, password_hash, role, last_name, first_name, middle_name)
		VALUES ($1, $2, 'x', $3, $4, $5, $6)`, id, login, role, last, first, mid); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func TestWriterPersistsEntries(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	w := NewWriter(pool, discardLog())
	w.Start(context.Background())

	user := seedUser(t, pool, "petrov", "teacher", "Петров", "Пётр", "Петрович")
	lesson, entity, reqID := uuid.New(), uuid.New(), uuid.New()
	ctx := core.WithRequestMeta(context.Background(), core.RequestMeta{RequestID: reqID, IP: "192.168.1.20", UserAgent: "Firefox"})
	ctx = core.WithPrincipal(ctx, &core.Principal{UserID: user, Role: core.RoleTeacher})
	w.Log(ctx, core.AuditEntry{
		Action: "evaluation.override", EntityType: "evaluation", EntityID: entity, LessonID: lesson,
		Before: map[string]any{"score": 50}, After: map[string]any{"score": 80, "reason": "Исправлена ошибка распознавания", "token": "t"},
	})
	w.Log(context.Background(), core.AuditEntry{Action: "backup.run", ActorSystem: true})
	stopWriter(t, w)

	var (
		actorID, entityID, lessonID, requestID *uuid.UUID
		role, action, etype, ip, ua            *string
		before, after                          []byte
	)
	err := pool.QueryRow(ctxT(t), `
		SELECT actor_id, actor_role, action, entity_type, entity_id, lesson_id, before, after, host(ip), user_agent, request_id
		  FROM audit_log WHERE action = 'evaluation.override'`).
		Scan(&actorID, &role, &action, &etype, &entityID, &lessonID, &before, &after, &ip, &ua, &requestID)
	if err != nil {
		t.Fatal(err)
	}
	if *actorID != user || *role != "teacher" || *etype != "evaluation" || *entityID != entity || *lessonID != lesson ||
		*ip != "192.168.1.20" || *ua != "Firefox" || *requestID != reqID {
		t.Fatalf("row mismatch: actor=%v role=%v etype=%v entity=%v lesson=%v ip=%v ua=%v req=%v",
			*actorID, *role, *etype, *entityID, *lessonID, *ip, *ua, *requestID)
	}
	var a map[string]any
	if err := json.Unmarshal(after, &a); err != nil {
		t.Fatal(err)
	}
	if a["token"] != Redacted || a["reason"] != "Исправлена ошибка распознавания" {
		t.Fatalf("after = %s", after)
	}

	var sysActor *uuid.UUID
	var sysRole string
	if err := pool.QueryRow(ctxT(t), `SELECT actor_id, actor_role FROM audit_log WHERE action = 'backup.run'`).Scan(&sysActor, &sysRole); err != nil {
		t.Fatal(err)
	}
	if sysActor != nil || sysRole != RoleSystem {
		t.Fatalf("system row: actor=%v role=%q", sysActor, sysRole)
	}
	if got := w.dropped.Load(); got != 0 {
		t.Fatalf("dropped = %d", got)
	}
}

// Stop без Start (CLI) всё равно дописывает накопленное.
func TestWriterStopWithoutStartFlushes(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	w := NewWriter(pool, discardLog())
	for i := range 300 { // больше batchMax: несколько COPY
		w.Log(context.Background(), core.AuditEntry{Action: fmt.Sprintf("cli.step_%d", i)})
	}
	stopWriter(t, w)
	if n := countAudit(t, pool); n != 300 {
		t.Fatalf("rows = %d, want 300", n)
	}
	// повторный Stop — no-op
	stopWriter(t, w)
	w.Log(context.Background(), core.AuditEntry{Action: "late.entry"})
	if w.dropped.Load() != 1 {
		t.Fatalf("запись после Stop должна считаться отброшенной, dropped=%d", w.dropped.Load())
	}
}

// Регрессия: одна запись с невалидным для jsonb текстом (сырой RawMessage с битым UTF-8,
// одиночный суррогат, \u0000) раньше роняла COPY всего батча — теряли и соседние записи.
func TestWriterBadJSONDoesNotLoseBatch(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	w := NewWriter(pool, discardLog())
	values := []any{
		json.RawMessage("{\"a\":\"\xff\xfe\"}"),
		json.RawMessage(`{"a":"\ud800"}`),
		json.RawMessage(`{"a":"x\u0000y"}`),
		[]byte("\xc3\x28 не json"),
		map[string]string{"a": "нормальная запись"},
		"просто строка\x00",
	}
	for i, v := range values {
		w.Log(context.Background(), core.AuditEntry{Action: fmt.Sprintf("bad.value_%d", i), After: v, Before: v})
	}
	stopWriter(t, w)
	if n := countAudit(t, pool); n != len(values) {
		t.Fatalf("rows = %d, want %d (dropped=%d)", n, len(values), w.dropped.Load())
	}
}

// Регрессия: часы шагают назад (или параллельные Log ставят строки в очередь не в порядке
// меток) — at не должен убывать при росте id, иначе курсор beforeId (ORDER BY at DESC,
// id DESC) пропускает записи. Страницы по 2 обязаны собрать все 5.
func TestWriterCursorPaginationWithClockStepBack(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	w := NewWriter(pool, discardLog())
	base := time.Now().UTC().Truncate(time.Second)
	var mu sync.Mutex
	step := 0
	w.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		step++
		return base.Add(-time.Duration(step) * 5 * time.Second) // каждое следующее — раньше (шаги NTP)
	}
	for i := range 5 {
		w.Log(context.Background(), core.AuditEntry{Action: fmt.Sprintf("page.entry_%d", i)})
	}
	stopWriter(t, w)

	var got []string
	var before *int64
	for range 10 {
		page, err := List(ctxT(t), pool, Filter{Limit: 2, BeforeID: before, Action: "page."})
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			got = append(got, e.Action)
		}
		id := int64(page[len(page)-1].Id)
		before = &id
	}
	want := []string{"page.entry_4", "page.entry_3", "page.entry_2", "page.entry_1", "page.entry_0"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("пагинация: %v, want %v", got, want)
	}
}

// Stop во время параллельных Log: каждая запись либо сохранена, либо учтена как отброшенная.
func TestWriterStopDuringConcurrentLog(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	w := NewWriter(pool, discardLog())
	w.Start(context.Background())
	const goroutines, per = 8, 150
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := range per {
				w.Log(context.Background(), core.AuditEntry{Action: fmt.Sprintf("load.g%d_%d", g, i)})
			}
		}()
	}
	close(start)
	time.Sleep(2 * time.Millisecond)
	stopWriter(t, w)
	wg.Wait()
	written := countAudit(t, pool)
	if total := written + int(w.dropped.Load()); total != goroutines*per {
		t.Fatalf("written %d + dropped %d != %d", written, w.dropped.Load(), goroutines*per)
	}
}

// БД недоступна: батч пробуется дважды и отбрасывается с учётом в dropped — писатель не
// зависает и не теряет записи молча.
func TestWriterDropsBatchOnDBError(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	pool.Close()
	w := NewWriter(pool, discardLog())
	w.Start(context.Background())
	for i := range 3 {
		w.Log(context.Background(), core.AuditEntry{Action: fmt.Sprintf("lost.%d", i)})
	}
	stopWriter(t, w)
	if got := w.dropped.Load(); got != 3 {
		t.Fatalf("dropped = %d, want 3", got)
	}
}
