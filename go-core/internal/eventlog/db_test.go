package eventlog

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/pgtest"
)

// seedAttempt — минимальная цепочка FK для строки attempts: преподаватель, обучающийся,
// категория, сценарий, эталон, занятие, попытка.
func seedAttempt(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	const q = `
WITH t AS (
  INSERT INTO users (login, password_hash, role, last_name, first_name)
  VALUES ('t_' || gen_random_uuid(), 'x', 'teacher', 'Петров', 'Пётр') RETURNING id
), s AS (
  INSERT INTO users (login, password_hash, role, last_name, first_name)
  VALUES ('s_' || gen_random_uuid(), 'x', 'student', 'Иванов', 'Иван') RETURNING id
), c AS (
  INSERT INTO classifier_categories (code, name) VALUES ('c_' || gen_random_uuid(), 'Пожар') RETURNING id
), sc AS (
  INSERT INTO scenarios (title, category_id, source) SELECT 'Пожар в квартире', c.id, 'manual' FROM c RETURNING id
), e AS (
  INSERT INTO etalons (scenario_id, card) SELECT sc.id, '{}' FROM sc RETURNING id, scenario_id
), l AS (
  INSERT INTO lessons (title, teacher_id, created_by, mode) SELECT 'Занятие', t.id, t.id, 'cards' FROM t RETURNING id
)
INSERT INTO attempts (lesson_id, user_id, scenario_id, etalon_id, mode, seq_no, time_limit_sec)
SELECT l.id, s.id, e.scenario_id, e.id, 'cards', 1, 30 FROM l, s, e
RETURNING id`
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), q).Scan(&id); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}
	return id
}

func dbWriter(t *testing.T, pool *pgxpool.Pool) *Writer {
	t.Helper()
	w := NewWriter(pool, quietLog())
	startWriter(t, w)
	return w
}

func parse(t *testing.T, body ...public.AttemptEventInput) []Input {
	t.Helper()
	in, err := ParseInputs(body, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func save(seq int) public.AttemptEventInput {
	return public.AttemptEventInput{ClientSeq: seq, Type: public.AttemptEventTypeSave, At: time.Now()}
}

// Регрессия (review): lastSeq = максимальный сохранённый clientSeq попытки. Воспроизведение
// ревьюера: [5] -> 5; [3] -> accepted 1, lastSeq 5 (было 3); [3] ещё раз -> accepted 0, lastSeq 5.
func TestDB_LastSeqIsAttemptMax(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	a := seedAttempt(t, pool)
	w := dbWriter(t, pool)
	ctx := appendCtx(t)

	steps := []struct {
		seqs     []int
		accepted int
		lastSeq  int64
	}{
		{[]int{5}, 1, 5},
		{[]int{3}, 1, 5},
		{[]int{3}, 0, 5},
		{[]int{1, 2, 2}, 2, 5}, // дубль внутри батча
		{[]int{9, 6}, 2, 9},
	}
	for i, st := range steps {
		body := make([]public.AttemptEventInput, len(st.seqs))
		for j, s := range st.seqs {
			body[j] = save(s)
		}
		res, err := w.Append(ctx, a, parse(t, body...))
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if res.Accepted != st.accepted || res.LastSeq != st.lastSeq || len(res.Inserted) != st.accepted {
			t.Fatalf("step %d %v: accepted=%d lastSeq=%d, want %d/%d", i, st.seqs, res.Accepted, res.LastSeq, st.accepted, st.lastSeq)
		}
	}

	list, err := List(ctx, pool, a)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 6 { // 5,3,1,2,9,6
		t.Fatalf("stored = %d events, want 6", len(list))
	}
}

// gatedDBWriter — писатель с настоящей БД, у которого можно «придержать» следующую транзакцию:
// пока она висит, запросы копятся и уходят следующей группой (детерминированный group commit).
type gatedDBWriter struct {
	*Writer
	armed   atomic.Bool
	gate    chan struct{}
	entered chan struct{}
}

func newGatedDBWriter(t *testing.T, pool *pgxpool.Pool) *gatedDBWriter {
	g := &gatedDBWriter{Writer: NewWriter(pool, quietLog())}
	g.exec = func(ctx context.Context, reqs []*request) error {
		if g.armed.CompareAndSwap(true, false) {
			close(g.entered)
			<-g.gate
		}
		return g.write(ctx, reqs)
	}
	startWriter(t, g.Writer)
	return g
}

// hold — следующая транзакция будет ждать release; возвращает канал «транзакция началась».
func (g *gatedDBWriter) hold() <-chan struct{} {
	g.gate, g.entered = make(chan struct{}), make(chan struct{})
	g.armed.Store(true)
	return g.entered
}

func (g *gatedDBWriter) release() { close(g.gate) }

// Группа из нескольких попыток в одной транзакции: у каждой своя граница lastSeq (запрос
// после вставки видит строки соседей по батчу), дубль соседа по батчу не засчитывается.
func TestDB_GroupCommitMultiAttempt(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	a, b := seedAttempt(t, pool), seedAttempt(t, pool)
	w := newGatedDBWriter(t, pool)
	ctx := appendCtx(t)

	if _, err := w.Append(ctx, b, parse(t, save(40))); err != nil { // у b уже есть 40
		t.Fatal(err)
	}

	entered := w.hold()
	warm := appendAsync(ctx, w.Writer, a, parse(t, save(1)))
	<-entered
	// Порядок в очереди фиксируем: при дубле внутри группы вставляется первое вхождение.
	chA := appendAsync(ctx, w.Writer, a, parse(t, save(2), save(3)))
	waitQueued(t, w.Writer, 2)
	chB := appendAsync(ctx, w.Writer, b, parse(t, save(7)))
	waitQueued(t, w.Writer, 3)
	chA2 := appendAsync(ctx, w.Writer, a, parse(t, save(3), save(4))) // 3 — дубль соседа по батчу
	waitQueued(t, w.Writer, 5)
	w.release()

	if r := <-warm; r.err != nil || r.res.LastSeq != 1 {
		t.Fatalf("warm: %+v", r)
	}
	ra, rb, ra2 := <-chA, <-chB, <-chA2
	if ra.err != nil || ra.res.Accepted != 2 || ra.res.LastSeq != 4 {
		t.Fatalf("a: %+v", ra)
	}
	if ra2.err != nil || ra2.res.Accepted != 1 || ra2.res.Inserted[0].ClientSeq != 4 || ra2.res.LastSeq != 4 {
		t.Fatalf("a2: %+v", ra2)
	}
	if rb.err != nil || rb.res.Accepted != 1 || rb.res.LastSeq != 40 {
		t.Fatalf("b: %+v", rb)
	}
}

// Ошибка FK одной попытки (её нет) в групповой транзакции: батч переписывается по запросам,
// события соседей записаны, ошибка — только у виновника.
func TestDB_GroupFailureFallsBackPerRequest(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	a, b := seedAttempt(t, pool), seedAttempt(t, pool)
	w := newGatedDBWriter(t, pool)
	ctx := appendCtx(t)

	entered := w.hold()
	warm := appendAsync(ctx, w.Writer, a, parse(t, save(1)))
	<-entered
	chX := appendAsync(ctx, w.Writer, uuid.New(), parse(t, save(1)))
	chB := appendAsync(ctx, w.Writer, b, parse(t, save(5), save(6)))
	waitQueued(t, w.Writer, 3)
	w.release()

	if r := <-warm; r.err != nil {
		t.Fatal(r.err)
	}
	if r := <-chX; r.err == nil {
		t.Fatal("missing attempt must fail")
	}
	if r := <-chB; r.err != nil || r.res.Accepted != 2 || r.res.LastSeq != 6 {
		t.Fatalf("b: %+v", r)
	}
	list, err := List(ctx, pool, b)
	if err != nil || len(list) != 2 {
		t.Fatalf("b events = %v %v", list, err)
	}
}

func TestDB_ConcurrentClientsNoLossNoDup(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	w := dbWriter(t, pool)
	ctx := appendCtx(t)
	const students, batches, per = 8, 5, 20
	ids := make([]uuid.UUID, students)
	for i := range ids {
		ids[i] = seedAttempt(t, pool)
	}
	var wg sync.WaitGroup
	for _, a := range ids {
		wg.Add(1)
		go func(a uuid.UUID) {
			defer wg.Done()
			for b := 0; b < batches; b++ {
				body := make([]public.AttemptEventInput, per)
				for i := range body {
					body[i] = public.AttemptEventInput{ClientSeq: b*per + i + 1, Type: public.AttemptEventTypeFieldChanged,
						Payload: obj("field", "applicant.phone", "value", "+7 999"), At: time.Now()}
				}
				in, err := ParseInputs(body, time.Now())
				if err != nil {
					t.Error(err)
					return
				}
				for try := 0; try < 2; try++ { // повтор после «обрыва» — дубли
					res, err := w.Append(ctx, a, in)
					if err != nil {
						t.Error(err)
						return
					}
					want := per
					if try == 1 {
						want = 0
					}
					if res.Accepted != want || res.LastSeq != int64((b+1)*per) {
						t.Errorf("attempt %s batch %d try %d: %+v", a, b, try, res)
					}
				}
			}
		}(a)
	}
	wg.Wait()
	for _, a := range ids {
		list, err := List(ctx, pool, a)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != batches*per {
			t.Fatalf("attempt %s: %d events, want %d", a, len(list), batches*per)
		}
	}
}

func TestDB_PayloadEdgeCases(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	a := seedAttempt(t, pool)
	w := dbWriter(t, pool)
	ctx := appendCtx(t)

	big := strings.Repeat("Ж", MaxPayloadBytes)
	in := parse(t,
		public.AttemptEventInput{ClientSeq: 1, Type: public.AttemptEventTypeFieldChanged,
			Payload: obj("field", "incident.description", "value", "дым\x00из окна"), At: time.Now()},
		public.AttemptEventInput{ClientSeq: 2, Type: public.AttemptEventTypeFieldChanged,
			Payload: obj("field", "incident.description", "value", big), At: time.Now()},
		public.AttemptEventInput{ClientSeq: 3, Type: public.AttemptEventTypeMicCheck, Payload: obj("ok", true), At: time.Now()},
	)
	res, err := w.Append(ctx, a, in)
	if err != nil {
		t.Fatalf("NUL/oversized payload must not fail the batch: %v", err)
	}
	if res.Accepted != 3 {
		t.Fatalf("accepted = %d", res.Accepted)
	}
	list, err := List(ctx, pool, a)
	if err != nil {
		t.Fatal(err)
	}
	byseq := map[int]public.AttemptEvent{}
	for _, e := range list {
		byseq[e.ClientSeq] = e
	}
	if v := (*byseq[1].Payload)["value"]; v != "дым�из окна" {
		t.Fatalf("NUL payload stored as %q", v)
	}
	if p := *byseq[2].Payload; p["truncated"] != true || p["field"] != "incident.description" {
		t.Fatalf("oversized payload stored as %v", p)
	}
	// WS-копия (Inserted) совпадает с тем, что отдаёт GET /events.
	for _, ins := range res.Inserted {
		got := byseq[ins.ClientSeq]
		gj, _ := json.Marshal(got)
		ij, _ := json.Marshal(ins)
		if string(gj) != string(ij) {
			t.Fatalf("WS copy differs from stored:\n ws=%s\ndb=%s", ij, gj)
		}
	}
}

func TestDB_InsertServerAndList(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	a := seedAttempt(t, pool)
	w := dbWriter(t, pool)
	ctx := appendCtx(t)

	t0 := time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)
	issued, err := InsertServer(ctx, pool, a, "issued", map[string]any{"scenarioId": "s-1"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if issued.ClientSeq != 0 || issued.Id == 0 || issued.Payload == nil || !issued.At.Equal(t0) {
		t.Fatalf("issued = %+v", issued)
	}
	accepted, err := InsertServer(ctx, pool, a, "call_accepted", nil, t0.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Payload != nil {
		t.Fatalf("empty payload must be omitted: %+v", accepted)
	}
	// Одинаковое время — порядок записи (id).
	tie1, err := InsertServer(ctx, pool, a, "dialogue_operator", json.RawMessage(`{"turnNo":1}`), t0.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	tie2, err := InsertServer(ctx, pool, a, "dialogue_caller", serverPayload{ScenarioID: "реплика"}, t0.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// Нулевое время — сейчас.
	now, err := InsertServer(ctx, pool, a, "submitted", nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(now.At) > time.Minute || now.At.Location() != time.UTC {
		t.Fatalf("zero at -> %v", now.At)
	}
	// Клиентское событие между серверными.
	cl := []Input{{ClientSeq: 1, Type: "open_card", Payload: emptyObject, At: t0.Add(1500 * time.Millisecond)}}
	if _, err := w.Append(ctx, a, cl); err != nil {
		t.Fatal(err)
	}
	if _, err := InsertServer(ctx, pool, a, "issued", json.RawMessage(`[1]`), time.Time{}); err == nil {
		t.Fatal("non-object payload must fail")
	}
	if _, err := InsertServer(ctx, pool, uuid.New(), "issued", nil, time.Time{}); err == nil {
		t.Fatal("unknown attempt must fail (FK)")
	}

	list, err := List(ctx, pool, a)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, e := range list {
		types = append(types, string(e.Type))
		if e.At.Location() != time.UTC {
			t.Fatalf("at not UTC: %v", e.At)
		}
	}
	want := "issued,call_accepted,open_card,dialogue_operator,dialogue_caller,submitted"
	if got := strings.Join(types, ","); got != want {
		t.Fatalf("order = %s\nwant    %s", got, want)
	}
	if list[3].Id != tie1.Id || list[4].Id != tie2.Id {
		t.Fatal("tie on at must be ordered by id")
	}
	if list[2].ClientSeq != 1 || list[0].ClientSeq != 0 {
		t.Fatalf("clientSeq: client=%d server=%d", list[2].ClientSeq, list[0].ClientSeq)
	}
	if (*list[4].Payload)["scenarioId"] != "реплика" {
		t.Fatalf("payload = %v", *list[4].Payload)
	}

	// Контракт: массив, не null — и для попытки без событий.
	empty, err := List(ctx, pool, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(empty); string(b) != "[]" {
		t.Fatalf("empty list JSON = %s", b)
	}
}

// Серверное событие в транзакции вызывающего откатывается вместе с ней.
func TestDB_InsertServerInTxRollback(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	a := seedAttempt(t, pool)
	ctx := appendCtx(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InsertServer(ctx, tx, a, "replay", nil, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := List(ctx, pool, a)
	if err != nil || len(list) != 0 {
		t.Fatalf("list after rollback = %v %v", list, err)
	}
}

func TestDB_StopFlushesAndRejects(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	a := seedAttempt(t, pool)
	w := NewWriter(pool, quietLog())
	w.Start(context.Background())
	ctx := appendCtx(t)
	if _, err := w.Append(ctx, a, parse(t, save(1))); err != nil {
		t.Fatal(err)
	}
	if err := w.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(ctx, a, parse(t, save(2))); !errors.Is(err, ErrStopped) {
		t.Fatalf("err = %v", err)
	}
}
