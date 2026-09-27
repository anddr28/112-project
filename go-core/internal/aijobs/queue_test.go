package aijobs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/settings"
)

func TestEnqueueValidation(t *testing.T) {
	t.Parallel()
	s := newTestService(t, nil, "") // до БД не доходит
	id := ids.New()
	other := ids.New()
	cases := []struct {
		name string
		job  core.NewJob
		want string
	}{
		{"no id", core.NewJob{Type: core.JobTTS, Payload: rawPayload(id)}, "ID is required"},
		{"unknown type", core.NewJob{ID: id, Type: "translate", Payload: rawPayload(id)}, "unknown job type"},
		{"no payload", core.NewJob{ID: id, Type: core.JobTTS}, "Payload is required"},
		{"raw payload not JSON", core.NewJob{ID: id, Type: core.JobTTS, Payload: json.RawMessage(`{"x":`)}, "not valid JSON"},
		{"bytes payload not JSON", core.NewJob{ID: id, Type: core.JobTTS, Payload: []byte(`[1,`)}, "not valid JSON"},
		{"typed payload with foreign request_id", core.NewJob{ID: id, Type: core.JobTTS,
			Payload: &aiservice.TtsJobRequest{RequestId: other, SchemaVersion: components.N1}}, "request_id"},
		{"typed value payload with foreign request_id", core.NewJob{ID: id, Type: core.JobEvaluateGrammar,
			Payload: aiservice.GrammarJobRequest{RequestId: other}}, "request_id"},
		{"unmarshalable payload", core.NewJob{ID: id, Type: core.JobTTS, Payload: make(chan int)}, "marshal payload"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := s.Enqueue(context.Background(), nil, c.job)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want containing %q", err, c.want)
			}
		})
	}
}

func TestPayloadRequestID(t *testing.T) {
	t.Parallel()
	id := ids.New()
	typed := []any{
		&aiservice.GrammarJobRequest{RequestId: id}, aiservice.GrammarJobRequest{RequestId: id},
		&aiservice.SemanticJobRequest{RequestId: id}, aiservice.SemanticJobRequest{RequestId: id},
		&aiservice.DialogueJobRequest{RequestId: id}, aiservice.DialogueJobRequest{RequestId: id},
		&aiservice.GenerateJobRequest{RequestId: id}, aiservice.GenerateJobRequest{RequestId: id},
		&aiservice.TtsJobRequest{RequestId: id}, aiservice.TtsJobRequest{RequestId: id},
	}
	for _, p := range typed {
		if got, ok := payloadRequestID(p); !ok || got != id {
			t.Errorf("%T: %s,%v", p, got, ok)
		}
	}
	for _, p := range []any{(*aiservice.TtsJobRequest)(nil), json.RawMessage(`{}`), map[string]any{"request_id": id}} {
		if _, ok := payloadRequestID(p); ok {
			t.Errorf("%T must not be recognised", p)
		}
	}
}

func TestEnqueueInsert(t *testing.T) {
	t.Parallel()
	s, pool := newDBService(t, "")
	ref := ids.New()
	runAfter := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	id := ids.New()
	payload := &aiservice.TtsJobRequest{SchemaVersion: components.N1, RequestId: id, Text: "Ожидайте", TextHash: "abc"}
	got, created, err := s.Enqueue(context.Background(), pool, core.NewJob{
		ID: id, Type: core.JobTTS, Priority: core.PriorityTTS, DedupKey: "tts:abc",
		RefType: core.RefScenario, RefID: ref, Payload: payload, MaxTries: 5, RunAfter: runAfter,
	})
	if err != nil || !created || got != id {
		t.Fatalf("enqueue: %s %v %v", got, created, err)
	}
	var (
		typ, status, dedup, refType string
		refID                       uuid.UUID
		prio, tries, maxTries       int
		ra                          time.Time
		p                           map[string]any
	)
	if err := pool.QueryRow(context.Background(), `SELECT type, status, dedup_key, ref_type, ref_id, priority, try_count,
	        max_tries, run_after, payload FROM ai_jobs WHERE id = $1`, id).Scan(&typ, &status, &dedup, &refType, &refID,
		&prio, &tries, &maxTries, &ra, &p); err != nil {
		t.Fatal(err)
	}
	if typ != "tts" || status != "queued" || dedup != "tts:abc" || refType != core.RefScenario || refID != ref ||
		prio != core.PriorityTTS || tries != 0 || maxTries != 5 || !ra.Equal(runAfter) {
		t.Errorf("row: %s %s %s %s %s prio=%d tries=%d max=%d run_after=%v", typ, status, dedup, refType, refID, prio, tries, maxTries, ra)
	}
	if p["request_id"] != id.String() || p["text"] != "Ожидайте" {
		t.Errorf("payload %v", p)
	}
}

func TestEnqueueDefaults(t *testing.T) {
	t.Parallel()
	s, pool := newDBService(t, "")
	setAI(s, func(ai *settings.AI) { ai.MaxTries = 4 })
	for _, prio := range []int{0, -1, 10} {
		id := enqueue(t, s, core.JobEvaluateSemantic, withPriority(prio))
		j := readJob(t, pool, id)
		if j.Priority != 5 || j.MaxTries != 4 || j.RunAfter.After(time.Now().Add(time.Second)) {
			t.Errorf("prio %d: stored priority=%d max_tries=%d run_after=%v", prio, j.Priority, j.MaxTries, j.RunAfter)
		}
	}
	var refType *string
	var refID *uuid.UUID
	id := enqueue(t, s, core.JobEvaluateSemantic)
	if err := pool.QueryRow(context.Background(), `SELECT ref_type, ref_id FROM ai_jobs WHERE id = $1`, id).Scan(&refType, &refID); err != nil {
		t.Fatal(err)
	}
	if refType != nil || refID != nil {
		t.Errorf("empty ref stored as %v / %v, want NULLs", refType, refID)
	}
}

func TestEnqueueDedupAndRevive(t *testing.T) {
	t.Parallel()
	s, pool := newDBService(t, "")
	first := enqueue(t, s, core.JobTTS, withDedup("tts:h1"))

	// Дубль, пока задача жива: тот же id, created=false.
	dupID := ids.New()
	got, created, err := s.Enqueue(context.Background(), pool, core.NewJob{ID: dupID, Type: core.JobTTS,
		DedupKey: "tts:h1", Payload: rawPayload(dupID)})
	if err != nil || created || got != first {
		t.Fatalf("duplicate: %s %v %v, want %s false", got, created, err, first)
	}

	// Провалившаяся — оживает с новым id, payload и счётчиками.
	exec(t, pool, `UPDATE ai_jobs SET status = 'failed', try_count = 3, error = 'tts_failed: x', result = '{"a":1}',
	        finished_at = now(), started_at = now() WHERE id = $1`, first)
	newID := ids.New()
	got, created, err = s.Enqueue(context.Background(), pool, core.NewJob{ID: newID, Type: core.JobTTS, Priority: 2,
		DedupKey: "tts:h1", Payload: rawPayload(newID)})
	if err != nil || !created || got != newID {
		t.Fatalf("revive: %s %v %v, want %s true", got, created, err, newID)
	}
	j := readJob(t, pool, newID)
	if j.Status != "queued" || j.TryCount != 0 || j.Error != nil || j.Result != nil || j.StartedAt != nil ||
		j.FinishedAt != nil || j.RequestID != newID.String() || j.Priority != 2 {
		t.Errorf("revived row %+v", j)
	}
	var n int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM ai_jobs WHERE dedup_key = 'tts:h1'`).Scan(&n)
	if n != 1 {
		t.Errorf("%d rows with the dedup key", n)
	}

	// Отменённая — тоже оживает; выполненная — нет.
	exec(t, pool, `UPDATE ai_jobs SET status = 'done' WHERE id = $1`, newID)
	thirdID := ids.New()
	got, created, err = s.Enqueue(context.Background(), pool, core.NewJob{ID: thirdID, Type: core.JobTTS,
		DedupKey: "tts:h1", Payload: rawPayload(thirdID)})
	if err != nil || created || got != newID {
		t.Fatalf("done job must not be revived: %s %v %v", got, created, err)
	}
}

// Диспетчер будится только после COMMIT бизнес-транзакции.
func TestEnqueueKicksAfterCommit(t *testing.T) {
	t.Parallel()
	s, pool := newDBService(t, "")
	drain := func() bool {
		select {
		case <-s.kickCh:
			return true
		default:
			return false
		}
	}
	err := pg.WithTx(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		id := ids.New()
		if _, _, err := s.Enqueue(ctx, tx, core.NewJob{ID: id, Type: core.JobTTS, Payload: rawPayload(id)}); err != nil {
			return err
		}
		if drain() {
			t.Error("dispatcher kicked before commit")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !drain() {
		t.Error("dispatcher not kicked after commit")
	}

	// Откат — задачи нет, и будить нечего.
	id := ids.New()
	_ = pg.WithTx(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		if _, _, err := s.Enqueue(ctx, tx, core.NewJob{ID: id, Type: core.JobTTS, Payload: rawPayload(id)}); err != nil {
			return err
		}
		return context.Canceled
	})
	if drain() {
		t.Error("kicked after rollback")
	}
	var n int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM ai_jobs WHERE id = $1`, id).Scan(&n)
	if n != 0 {
		t.Error("rolled back job persisted")
	}
}

func TestEstWaitSecAndAvailable(t *testing.T) {
	t.Parallel()
	s := newTestService(t, nil, "")
	if got := s.EstWaitSec("translate"); got != 0 {
		t.Errorf("unknown type: %d", got)
	}
	if got := s.EstWaitSec(core.JobEvaluateSemantic); got != int(defaultAvgSec[typeIdx(core.JobEvaluateSemantic)]) {
		t.Errorf("empty queue semantic: %d", got)
	}
	if !s.Available() {
		t.Error("fresh service must be available")
	}
}
