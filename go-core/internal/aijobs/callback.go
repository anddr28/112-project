package aijobs

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/callbacks"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
)

// Приём результатов: POST /internal/ai/v1/results (go-internal.v1.yaml). Монтируется
// приложением на внутренний mux (не /api/v1: там сессии, CSRF и RBAC студентов/преподавателей).
//
// Ответы: 401 — неверный токен; 400 — мусор/нарушение инвариантов конверта; 200 — принято
// (в т.ч. дубль и неизвестная задача: ai-service не должен ретраить зря); 500 — не смогли
// применить (БД/обработчик) — ai-service повторит через 1/5/30 с.

const maxCallbackBody = 4 << 20

var (
	respAccepted  = []byte(`{"accepted":true,"duplicate":false}`)
	respDuplicate = []byte(`{"accepted":true,"duplicate":true}`)
)

var bodyPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// CallbackHandler — http.Handler для "POST /internal/ai/v1/results".
func (s *Service) CallbackHandler() http.Handler { return http.HandlerFunc(s.serveCallback) }

func (s *Service) serveCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		httpx.WriteError(w, &httpx.Error{Status: http.StatusMethodNotAllowed, Code: httpx.CodeValidation, Message: "Разрешён только POST"})
		return
	}
	if !s.validToken(r.Header.Get("X-Internal-Token")) {
		httpx.WriteError(w, &httpx.Error{Status: http.StatusUnauthorized, Code: httpx.CodeUnauthorized, Message: "Неверный внутренний токен (X-Internal-Token)"})
		return
	}

	buf := bodyPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		if buf.Cap() <= 1<<20 { // гигантские буферы (сценарии) не держим в пуле
			bodyPool.Put(buf)
		}
	}()
	if _, err := buf.ReadFrom(http.MaxBytesReader(w, r.Body, maxCallbackBody)); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			httpx.WriteError(w, &httpx.Error{Status: http.StatusRequestEntityTooLarge, Code: httpx.CodeValidation, Message: "Слишком большое тело callback'а (больше 4 МБ)"})
			return
		}
		httpx.WriteError(w, httpx.BadRequest("Не удалось прочитать тело запроса"))
		return
	}
	raw := bytes.TrimSpace(buf.Bytes())

	var res callbacks.AiResult
	if err := json.Unmarshal(raw, &res); err != nil {
		s.log.Warn("ai callback: bad JSON", "err", err)
		httpx.WriteError(w, httpx.BadRequest("Некорректный JSON: "+err.Error()))
		return
	}
	typ, err := validateEnvelope(&res)
	if err != nil {
		s.log.Warn("ai callback: envelope violation", "request_id", res.RequestId, "type", res.Type, "err", err)
		httpx.WriteError(w, httpx.BadRequest(err.Error()))
		return
	}

	s.st.callbacks.Add(1)
	dup, err := s.applyCallback(r.Context(), typ, &res, raw)
	if err != nil {
		var tm *typeMismatchError
		if errors.As(err, &tm) {
			s.log.Warn("ai callback rejected: type mismatch", "request_id", res.RequestId, "type", res.Type, "job_type", tm.jobType)
			httpx.WriteError(w, httpx.BadRequest(tm.Error()))
			return
		}
		if r.Context().Err() != nil {
			return // ai-service оборвал соединение — он повторит сам
		}
		s.log.Error("ai callback: apply failed", "request_id", res.RequestId, "type", res.Type, "err", err)
		httpx.WriteError(w, httpx.Internal(err))
		return
	}
	if dup {
		s.st.callbackDups.Add(1)
		httpx.WriteRawJSON(w, http.StatusOK, respDuplicate)
		return
	}
	httpx.WriteRawJSON(w, http.StatusOK, respAccepted)
}

// validToken — сравнение за постоянное время (длина токена всё равно не секрет).
func (s *Service) validToken(got string) bool {
	if len(s.token) == 0 || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), s.token) == 1
}

// validateEnvelope — инварианты конверта AiResult (go-internal.v1.yaml): версия схемы,
// request_id, известный тип; ok → ровно одно поле результата, и это поле своего типа;
// failed → поле error с кодом. Нарушение — 400 (ai-service прислал мусор).
func validateEnvelope(r *callbacks.AiResult) (core.JobType, error) {
	if r.SchemaVersion != components.N1 {
		return "", fmt.Errorf("Неподдерживаемая schema_version %q (ожидается \"1\")", r.SchemaVersion)
	}
	if r.RequestId == uuid.Nil {
		return "", errors.New("Не задан request_id")
	}
	typ := core.JobType(r.Type)
	if !r.Type.Valid() || typeIdx(typ) < 0 {
		return "", fmt.Errorf("Неизвестный тип задачи %q", r.Type)
	}
	switch r.Status {
	case callbacks.Ok:
		present := 0
		var matches bool
		if r.Grammar != nil {
			present++
			matches = typ == core.JobEvaluateGrammar
		}
		if r.Semantic != nil {
			present++
			matches = typ == core.JobEvaluateSemantic
		}
		if r.Dialogue != nil {
			present++
			matches = typ == core.JobEvaluateDialogue
		}
		if r.Scenario != nil {
			present++
			matches = typ == core.JobGenerateScenario
		}
		if r.Tts != nil {
			present++
			matches = typ == core.JobTTS
		}
		switch {
		case present == 0:
			return "", fmt.Errorf("status=ok без поля результата для типа %s", typ)
		case present > 1:
			return "", errors.New("status=ok: должно быть ровно одно поле результата")
		case !matches:
			return "", fmt.Errorf("status=ok: поле результата не соответствует типу %s", typ)
		}
	case callbacks.Failed:
		if r.Error == nil || r.Error.Code == "" {
			return "", errors.New("status=failed без поля error")
		}
	default:
		return "", fmt.Errorf("Неизвестный status %q (ожидается ok|failed)", r.Status)
	}
	return typ, nil
}

// typeMismatchError — тип в callback'е не совпадает с типом задачи: единственный случай,
// когда по известной задаче отвечаем 400 (ошибки обработчиков — 500, чтобы ai-service повторил).
type typeMismatchError struct{ got, jobType core.JobType }

func (e *typeMismatchError) Error() string {
	return fmt.Sprintf("Тип результата %s не совпадает с типом задачи %s", e.got, e.jobType)
}

// applyCallback — применение результата в транзакции под FOR UPDATE строки задачи.
// dup=true — задача уже закрыта (повтор после переотправки) или callback относится к
// отправке, которую уже сменил ретрай.
//
// request_id callback'а — это request_id отправки: первая = ai_jobs.id, ретрай по
// failed-callback'у уходит под новым (sqlRetryJob), и его результат ищется по payload.
func (s *Service) applyCallback(ctx context.Context, typ core.JobType, res *callbacks.AiResult, raw []byte) (dup bool, err error) {
	var (
		requeued bool
		observe  float64
		obsType  = typeIdx(typ)
	)
	err = pg.WithTx(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		rec, err := scanJob(tx.QueryRow(ctx, sqlLockJob, res.RequestId))
		if pg.IsNoRows(err) {
			rec, err = scanJob(tx.QueryRow(ctx, sqlLockByRequest, res.RequestId.String()))
		}
		if err != nil {
			if pg.IsNoRows(err) {
				// Задача удалена/оживлена с новым id, уже закрыта после ретрая или callback
				// чужого стенда — принять и забыть.
				s.log.Warn("ai callback for unknown job", "request_id", res.RequestId, "type", typ)
				return nil
			}
			return err
		}
		if rec.Type != typ {
			return &typeMismatchError{got: typ, jobType: rec.Type}
		}
		switch rec.Status {
		case core.JobDone, core.JobFailed, core.JobCancelled:
			dup = true
			return nil
		}
		if res.Status == callbacks.Failed && superseded(rec.requestID, res.RequestId) {
			// Отказ прежней отправки (повтор доставки уже обработанного failed-callback'а):
			// задача ушла ретраем под новым request_id и ждёт его результата. Годный
			// результат (ok) от любой отправки задачи принимается — ниже.
			dup = true
			return nil
		}

		if res.Status == callbacks.Ok {
			if h := s.handler(rec.typ); h != nil {
				if err := h.ApplyResult(ctx, tx, rec.record(), res); err != nil {
					return fmt.Errorf("apply result: %w", err)
				}
			} else {
				s.log.Error("ai callback: no result handler registered, marking done", "type", typ, "job", rec.ID)
			}
			if _, err := tx.Exec(ctx, sqlMarkDone, rec.ID, raw); err != nil {
				return err
			}
			observe = serviceTimeMs(res, rec.lockedAt)
			return nil
		}

		// status=failed
		e := res.Error
		code := string(e.Code)
		if !e.Code.Valid() {
			s.log.Warn("ai callback: unknown error code, treated as internal", "code", code, "job", rec.ID)
			code = string(callbacks.Internal)
		}
		if shouldRetry(code, e.Retryable, rec.TryCount, rec.MaxTries, rec.prevCode) {
			delay := backoff(rec.TryCount)
			next := ids.New()
			if _, err := tx.Exec(ctx, sqlRetryJob, rec.ID, delay.Seconds(), errorText(code, e.Message), raw, next.String()); err != nil {
				return err
			}
			requeued = true
			s.log.Warn("ai job failed, retrying", "job", rec.ID, "type", typ, "code", code,
				"try", rec.TryCount+1, "max_tries", rec.MaxTries, "delay_sec", int(delay.Seconds()),
				"request_id", next)
			return nil
		}
		s.log.Warn("ai job failed permanently", "job", rec.ID, "type", typ, "code", code, "retryable", e.Retryable, "try", rec.TryCount+1)
		return s.finishFailed(ctx, tx, rec, 1, code, e.Message, raw)
	})
	if err != nil {
		return false, err
	}
	if requeued {
		s.st.requeued.Add(1)
		s.kick()
	}
	if observe > 0 {
		s.est.observe(obsType, observe)
	}
	return dup, nil
}

// superseded — callback пришёл по отправке, которую сменил ретрай: request_id текущей
// отправки (payload) — другой. Нечитаемый request_id в payload — не повод отбросить результат.
func superseded(current string, got uuid.UUID) bool {
	cur, err := uuid.Parse(current)
	return err == nil && cur != got
}

// serviceTimeMs — длительность выполнения для оценок ожидания: чистое время вычисления из
// engine.duration_ms; нет его — от последней отправки до результата.
func serviceTimeMs(res *callbacks.AiResult, lockedAt *time.Time) float64 {
	if res.Engine.DurationMs > 0 {
		return float64(res.Engine.DurationMs)
	}
	if lockedAt != nil {
		return float64(time.Since(*lockedAt).Milliseconds())
	}
	return 0
}
