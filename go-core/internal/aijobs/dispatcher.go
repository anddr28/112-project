package aijobs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/core"
	"lct/gocore/internal/platform/pg"
)

// claimedJob — задача, захваченная диспетчером (строка sqlClaim).
type claimedJob struct {
	id        uuid.UUID
	typ       int
	tryCount  int
	maxTries  int
	lockedAt  time.Time
	payload   []byte
	requestID string // request_id этой отправки: = id, после ретрая по failed-callback'у — новый
}

// dispatchLoop — единственная горутина, захватывающая задачи. Будится kick'ом после COMMIT
// Enqueue и тикером (отложенные run_after, задачи других инстансов, истёкшие паузы 429).
func (s *Service) dispatchLoop(ctx context.Context) {
	defer s.loops.Done()
	t := time.NewTicker(dispatchTick)
	defer t.Stop()
	for {
		now := time.Now()
		if now.Sub(s.aiAt) >= settingsEvery {
			s.refreshSettings(ctx)
		}
		if s.est.loadStale(now) {
			s.refreshLoad(ctx, now)
		}
		s.dispatchRound(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.kickCh:
		case <-t.C:
		}
	}
}

// dispatchRound захватывает столько задач, сколько свободных слотов отправки: захваченная, но
// не отправленная задача числилась бы running и сбивала reaper и оценки ожидания.
func (s *Service) dispatchRound(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	free := dispatchConcurrency - int(s.inflight.Load())
	if free <= 0 {
		return
	}
	ok, probe := s.br.Allow()
	if !ok {
		return
	}
	if probe && !s.probeHealth(ctx) {
		return
	}
	jobs, err := s.claim(ctx, free)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("ai jobs: claim failed", "err", err)
		}
		return
	}
	s.lastClaimFull.Store(len(jobs) == free)
	for i := range jobs {
		s.inflight.Add(1)
		s.sends.Add(1)
		go s.send(jobs[i])
	}
}

// probeHealth — проба half_open: GET /v1/health (задачу пробой не рискуем). true — breaker закрыт.
func (s *Service) probeHealth(ctx context.Context) bool {
	pctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, s.base+"/v1/health", nil)
	if err != nil {
		s.br.Release(true)
		return false
	}
	s.setHeaders(req, "")
	resp, err := s.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			s.br.Release(true)
			return false
		}
		s.br.Failure()
		return false
	}
	drainClose(resp.Body)
	if resp.StatusCode >= 500 && resp.StatusCode != http.StatusServiceUnavailable {
		s.br.Failure()
		return false
	}
	s.br.Success()
	return true
}

// pausedTypes — типы, по которым ai-service недавно ответил 429 (параметр $3 sqlClaim).
// Всегда не-nil: NULL в "type <> ALL($3)" отфильтровал бы всё.
func (s *Service) pausedTypes(now time.Time) []string {
	out := []string{}
	n := now.UnixNano()
	for i := range s.pausedUntil {
		if s.pausedUntil[i].Load() > n {
			out = append(out, string(jobTypes[i]))
		}
	}
	return out
}

func (s *Service) pauseType(i int, d time.Duration) {
	until := time.Now().Add(d).UnixNano()
	for {
		cur := s.pausedUntil[i].Load()
		if cur >= until || s.pausedUntil[i].CompareAndSwap(cur, until) {
			return
		}
	}
}

func (s *Service) claim(ctx context.Context, n int) ([]claimedJob, error) {
	rows, err := s.pool.Query(ctx, sqlClaim, s.instance, n, s.pausedTypes(time.Now()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := make([]claimedJob, 0, n)
	for rows.Next() {
		var (
			j   claimedJob
			typ string
		)
		if err := rows.Scan(&j.id, &typ, &j.tryCount, &j.maxTries, &j.lockedAt, &j.payload, &j.requestID); err != nil {
			return nil, err
		}
		j.typ = typeIdx(core.JobType(typ))
		if j.typ < 0 { // CHECK в БД не пускает — но не падаем, если схема разъехалась
			s.log.Error("ai jobs: unknown job type in queue", "job", j.id, "type", typ)
			continue
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// sendOutcome — итог одной отправки.
type sendOutcome struct {
	class      sendClass
	status     int
	retryAfter string
	detail     string // обрывок тела ответа / текст сетевой ошибки — в лог и ai_jobs.error
}

// send — POST {AI_SERVICE_URL}/v1/jobs/{type} с payload как есть и разбор ответа.
// Работает на собственном контексте: остановка сервиса не обрывает запрос посередине
// (иначе результат приёма задачи ai-service'ом неизвестен), Stop дожидается итога.
func (s *Service) send(j claimedJob) {
	defer func() {
		s.inflight.Add(-1)
		s.sends.Done()
		if s.lastClaimFull.Load() {
			s.kick() // слот освободился, а в очереди, вероятно, есть ещё
		}
	}()
	s.st.dispatched.Add(1)
	out := s.post(j)
	ctx, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
	defer cancel()
	s.applyOutcome(ctx, j, out)
}

func (s *Service) post(j claimedJob) sendOutcome {
	ctx, cancel := context.WithTimeout(context.Background(), s.jobTimeout())
	defer cancel()
	typ := jobTypes[j.typ]
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+typ.Path(), bytes.NewReader(j.payload))
	if err != nil {
		return sendOutcome{class: sendNetErr, detail: err.Error()}
	}
	s.setHeaders(req, "application/json")
	// ai-service идемпотентен по request_id: с этим заголовком транспорт может повторить POST
	// на свежем соединении, если keep-alive соединение закрылось на стороне сервера.
	req.Header.Set("Idempotency-Key", j.requestID)
	resp, err := s.hc.Do(req)
	if err != nil {
		return sendOutcome{class: sendNetErr, detail: err.Error()}
	}
	out := sendOutcome{class: classifyJobStatus(resp.StatusCode), status: resp.StatusCode}
	if out.class != sendAccepted {
		out.retryAfter = resp.Header.Get("Retry-After")
		out.detail = readSnippet(resp.Body)
	}
	drainClose(resp.Body)
	return out
}

func (s *Service) jobTimeout() time.Duration {
	if s.cfg.AIJobTimeout > 0 {
		return s.cfg.AIJobTimeout
	}
	return 5 * time.Second
}

// applyOutcome — запись итога отправки в ai_jobs (контракт ai-service.v1.yaml, «Гарантии и правила»).
func (s *Service) applyOutcome(ctx context.Context, j claimedJob, out sendOutcome) {
	typ := jobTypes[j.typ]
	switch out.class {
	case sendAccepted:
		// 202: задача остаётся running до callback'а (или reaper'а).
		s.br.Success()
		s.st.accepted.Add(1)
		s.markReachable()

	case sendBusy:
		// Очередь ai-service полна: не ошибка задачи и не отказ сервиса. Ждём Retry-After
		// и не шлём задачи этого типа до конца паузы.
		s.br.Success()
		s.st.busy.Add(1)
		s.markReachable()
		d := busyDelay(out.retryAfter, time.Now())
		s.pauseType(j.typ, d)
		s.requeue(ctx, j, d, 0, "busy: ai-service queue full (HTTP "+strconv.Itoa(out.status)+")")

	case sendAuth:
		// Токен не принят: задачи не уходят, пока админ не поправит конфигурацию, — для
		// оценок это такая же недоступность ai-service, как и сетевая (failUnavailable).
		s.br.Success()
		s.st.rejected.Add(1)
		s.markUnreachable()
		s.log.Error("ai jobs: ai-service rejected X-Internal-Token — check INTERNAL_API_TOKEN on both sides",
			"job", j.id, "type", typ, "status", out.status)
		s.pauseType(j.typ, authRetryDelay)
		s.requeue(ctx, j, authRetryDelay, 0, "auth: HTTP "+strconv.Itoa(out.status))

	case sendRejected:
		// Payload не прошёл валидацию ai-service — ошибка go-core, ретраи бессмысленны.
		s.br.Success()
		s.st.rejected.Add(1)
		s.log.Error("ai jobs: ai-service rejected payload", "job", j.id, "type", typ, "status", out.status, "body", logSnippet(out.detail))
		s.failSent(ctx, j, CodeBadPayload, fmt.Sprintf("HTTP %d: %s", out.status, out.detail))

	case sendClientErr, sendServerErr:
		if out.class == sendServerErr {
			s.br.Failure()
			s.st.failures.Add(1)
		} else {
			s.br.Success()
			s.st.rejected.Add(1)
		}
		msg := fmt.Sprintf("HTTP %d: %s", out.status, out.detail)
		s.log.Warn("ai jobs: dispatch failed", "job", j.id, "type", typ, "status", out.status, "try", j.tryCount+1, "max_tries", j.maxTries)
		if j.tryCount+1 >= j.maxTries {
			s.failSent(ctx, j, CodeDispatchFailed, msg)
			return
		}
		s.requeue(ctx, j, backoff(j.tryCount), 1, errorText(CodeDispatchFailed, msg))

	case sendNetErr:
		// ai-service недоступен: задача не виновата — try_count не растёт; частоту повторов
		// ограничивает breaker (после N отказов диспетчер перестаёт захватывать задачи).
		s.br.Failure()
		s.st.failures.Add(1)
		s.log.Warn("ai jobs: ai-service unreachable", "job", j.id, "type", typ, "err", out.detail)
		s.requeue(ctx, j, backoff(j.tryCount), 0, errorText("unreachable", out.detail))
	}
}

// requeue возвращает отправленную задачу в очередь (только если она всё ещё наша).
func (s *Service) requeue(ctx context.Context, j claimedJob, delay time.Duration, incTry int, errText string) {
	var e *string
	if errText != "" {
		e = &errText
	}
	tag, err := s.pool.Exec(ctx, sqlRequeueSent, j.id, s.instance, j.lockedAt, delay.Seconds(), incTry, e)
	if err != nil {
		// Задача останется running — её вернёт reaper; перезапуск инстанса — requeueOwn.
		s.log.Error("ai jobs: requeue failed", "job", j.id, "err", err)
		return
	}
	if tag.RowsAffected() > 0 {
		s.st.requeued.Add(1)
	}
}

// failSent — окончательный провал отправленной задачи: обработчик домена (ApplyFailure)
// и status=failed — в одной транзакции.
func (s *Service) failSent(ctx context.Context, j claimedJob, code, msg string) {
	err := pg.WithTx(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		rec, err := scanJob(tx.QueryRow(ctx, sqlLockSent, j.id, s.instance, j.lockedAt))
		if err != nil {
			if pg.IsNoRows(err) {
				return nil // уже не наша: пришёл callback или reaper вернул в очередь
			}
			return err
		}
		return s.finishFailed(ctx, tx, rec, 1, code, msg, nil)
	})
	if err != nil {
		s.log.Error("ai jobs: mark failed", "job", j.id, "code", code, "err", err)
	}
}

// finishFailed — ApplyFailure обработчика типа + ai_jobs.status=failed (в транзакции tx,
// строка уже под FOR UPDATE). raw — полный callback (nil — не менять result).
func (s *Service) finishFailed(ctx context.Context, tx pgx.Tx, rec jobRow, incTry int, code, msg string, raw []byte) error {
	if h := s.handler(rec.typ); h != nil {
		if err := h.ApplyFailure(ctx, tx, rec.record(), code, msg); err != nil {
			return fmt.Errorf("apply failure %s: %w", rec.ID, err)
		}
	} else {
		s.log.Error("ai jobs: no result handler registered", "type", rec.Type, "job", rec.ID)
	}
	var result any
	if raw != nil {
		result = raw
	}
	if _, err := tx.Exec(ctx, sqlMarkFailed, rec.ID, incTry, errorText(code, msg), result); err != nil {
		return err
	}
	s.st.failed.Add(1)
	return nil
}

// ---------------------------------------------------------------- строка задачи

// jobRow — строка jobCols: core.JobRecord + служебное (индекс типа, locked_at, request_id
// текущей отправки, код ошибки последнего failed-callback'а).
type jobRow struct {
	core.JobRecord
	typ       int
	lockedAt  *time.Time
	requestID string
	prevCode  string
}

func (r *jobRow) record() core.JobRecord { return r.JobRecord }

func scanJob(row pgx.Row) (jobRow, error) {
	var (
		r       jobRow
		typ     string
		refType *string
		refID   *uuid.UUID
		payload []byte
	)
	err := row.Scan(&r.ID, &typ, &r.Status, &refType, &refID, &r.TryCount, &r.MaxTries, &payload, &r.CreatedAt, &r.lockedAt,
		&r.requestID, &r.prevCode)
	if err != nil {
		return jobRow{}, err
	}
	r.Type = core.JobType(typ)
	r.typ = typeIdx(r.Type)
	if r.typ < 0 {
		return jobRow{}, fmt.Errorf("aijobs: unknown job type %q in ai_jobs", typ)
	}
	if refType != nil {
		r.RefType = *refType
	}
	if refID != nil {
		r.RefID = *refID
	}
	r.Payload = payload
	r.CreatedAt = r.CreatedAt.UTC()
	return r, nil
}

// ---------------------------------------------------------------- снимок очереди, статистика

// refreshLoad — снимок queued/running по (type, status, priority) для EstWaitSec.
func (s *Service) refreshLoad(ctx context.Context, now time.Time) {
	qctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	rows, err := s.pool.Query(qctx, sqlLoad)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("ai jobs: load snapshot failed", "err", err)
		}
		s.est.setLoad(nil, now) // не долбим упавшую БД каждую итерацию
		return
	}
	defer rows.Close()
	out := make([]loadRow, 0, 8)
	for rows.Next() {
		var (
			typ, status string
			r           loadRow
		)
		if err := rows.Scan(&typ, &status, &r.prio, &r.n); err != nil {
			s.log.Warn("ai jobs: load snapshot scan", "err", err)
			return
		}
		if r.typ = typeIdx(core.JobType(typ)); r.typ < 0 {
			continue
		}
		r.running = status == core.JobRunning
		out = append(out, r)
	}
	if rows.Err() == nil {
		s.est.setLoad(out, now)
	}
}

// seedAverages — начальные средние длительности из последних выполненных задач (≤ 7 суток).
func (s *Service) seedAverages(ctx context.Context) {
	qctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	rows, err := s.pool.Query(qctx, sqlSeedAvg, uuidV7Floor(time.Now().Add(-7*24*time.Hour)))
	if err != nil {
		s.log.Warn("ai jobs: seed averages failed", "err", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var (
			typ string
			ms  float64
		)
		if err := rows.Scan(&typ, &ms); err != nil {
			return
		}
		s.est.seed(typeIdx(core.JobType(typ)), ms)
	}
}

// requeueOwn — при старте свои running-задачи уходят в очередь без try_count++.
// Почему: пока go-core был выключен, ai-service мог выполнить задачу и исчерпать ретраи
// callback'а (1/5/30 с) — результат потерян, и reaper заметил бы это только через
// reaper_after_sec (15 мин). Повторная отправка безопасна: ai-service идемпотентен по
// request_id (в очереди/в работе → 202 без дубля; уже выполнена → 202 + callback повторно).
// Чужие running-задачи (другой инстанс / старое имя хоста) — забота reaper'а; поэтому
// GOCORE_INSTANCE_ID в развёртывании должен быть стабильным.
func (s *Service) requeueOwn(ctx context.Context) {
	qctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	tag, err := s.pool.Exec(qctx, sqlRequeueOwn, s.instance)
	if err != nil {
		s.log.Warn("ai jobs: requeue own running jobs failed", "err", err)
		return
	}
	if n := tag.RowsAffected(); n > 0 {
		s.log.Info("ai jobs: re-dispatching own running jobs after restart", "count", n, "instance", s.instance)
		s.st.requeued.Add(uint64(n))
	}
}

// uuidV7Floor — наименьший UUIDv7 с моментом t: PK ai_jobs (UUIDv7, ids.New) упорядочен по
// времени создания, поэтому "id >= floor" — дешёвый диапазон по индексу вместо seq scan.
func uuidV7Floor(t time.Time) uuid.UUID {
	var u uuid.UUID
	ms := uint64(max(t.UnixMilli(), 0))
	u[0] = byte(ms >> 40)
	u[1] = byte(ms >> 32)
	u[2] = byte(ms >> 24)
	u[3] = byte(ms >> 16)
	u[4] = byte(ms >> 8)
	u[5] = byte(ms)
	u[6] = 0x70 // версия 7
	u[8] = 0x80 // вариант RFC 4122
	return u
}

// ---------------------------------------------------------------- HTTP-мелочи

func (s *Service) setHeaders(req *http.Request, contentType string) {
	h := req.Header
	h.Set("X-Internal-Token", string(s.token))
	h.Set("Accept", "application/json")
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
}

const (
	snippetLimit = 1 << 10  // обрывок тела ошибки — в лог
	drainLimit   = 64 << 10 // дочитываем хвост, чтобы соединение вернулось в keep-alive пул
)

// logSnippet — короткий обрывок для лога: 422/400 FastAPI эхом возвращает входные значения
// (тексты карточек), а в логах ПДн не место. Полный обрывок остаётся в ai_jobs.error.
func logSnippet(s string) string { return truncateUTF8(s, 200) }

func readSnippet(body io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(body, snippetLimit))
	return string(bytes.TrimSpace(b))
}

func drainClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, drainLimit))
	_ = body.Close()
}
