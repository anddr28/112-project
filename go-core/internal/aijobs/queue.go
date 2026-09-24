package aijobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
)

// Enqueue ставит задачу в ai_jobs в транзакции q (core.JobQueue).
//
// Дубль по dedup_key: если существующая задача failed/cancelled — она оживает с новым id,
// payload и счётчиками (created=true: для вызывающего это новая задача); иначе возвращается
// id существующей и created=false. Диспетчер будится после COMMIT (pg.OnCommit).
func (s *Service) Enqueue(ctx context.Context, q pg.Querier, j core.NewJob) (uuid.UUID, bool, error) {
	if j.ID == uuid.Nil {
		return uuid.Nil, false, errors.New("aijobs: NewJob.ID is required (ids.New() before building payload)")
	}
	if typeIdx(j.Type) < 0 {
		return uuid.Nil, false, fmt.Errorf("aijobs: unknown job type %q", j.Type)
	}
	payload, err := marshalPayload(j)
	if err != nil {
		return uuid.Nil, false, err
	}
	prio := j.Priority
	if prio < 1 || prio > 9 {
		prio = 5 // JobBase.priority default
	}
	maxTries := j.MaxTries
	if maxTries <= 0 {
		maxTries = s.aiSettings().MaxTries
	}
	var runAfter *time.Time
	if !j.RunAfter.IsZero() {
		t := j.RunAfter.UTC()
		runAfter = &t
	}
	var dedup, refType *string
	if j.DedupKey != "" {
		dedup = &j.DedupKey
	}
	if j.RefType != "" {
		refType = &j.RefType
	}
	args := [...]any{j.ID, string(j.Type), prio, runAfter, dedup, refType, ids.OrNil(j.RefID), payload, maxTries}

	var id uuid.UUID
	err = q.QueryRow(ctx, sqlInsertJob, args[:]...).Scan(&id)
	switch {
	case err == nil:
		pg.OnCommit(ctx, s.kick)
		return id, true, nil
	case !pg.IsNoRows(err):
		return uuid.Nil, false, fmt.Errorf("aijobs: insert job: %w", err)
	}

	// Конфликт dedup_key (без dedup_key конфликта не бывает). Сначала оживить провалившуюся.
	err = q.QueryRow(ctx, sqlReviveJob, args[:]...).Scan(&id)
	switch {
	case err == nil:
		pg.OnCommit(ctx, s.kick)
		return id, true, nil
	case !pg.IsNoRows(err):
		return uuid.Nil, false, fmt.Errorf("aijobs: revive job: %w", err)
	}
	if err = q.QueryRow(ctx, sqlJobByDedup, j.DedupKey).Scan(&id); err != nil {
		return uuid.Nil, false, fmt.Errorf("aijobs: existing job by dedup key: %w", err)
	}
	return id, false, nil
}

// marshalPayload — тело запроса к ai-service. Для известных типов запросов проверяется, что
// request_id = ID задачи: иначе callback не найдёт свою строку ai_jobs (тихая потеря результата).
func marshalPayload(j core.NewJob) ([]byte, error) {
	if j.Payload == nil {
		return nil, errors.New("aijobs: NewJob.Payload is required")
	}
	if rid, ok := payloadRequestID(j.Payload); ok && rid != j.ID {
		return nil, fmt.Errorf("aijobs: payload request_id %s != job id %s", rid, j.ID)
	}
	switch p := j.Payload.(type) {
	case json.RawMessage:
		if !json.Valid(p) {
			return nil, errors.New("aijobs: payload is not valid JSON")
		}
		return p, nil
	case []byte:
		if !json.Valid(p) {
			return nil, errors.New("aijobs: payload is not valid JSON")
		}
		return p, nil
	}
	b, err := json.Marshal(j.Payload)
	if err != nil {
		return nil, fmt.Errorf("aijobs: marshal payload: %w", err)
	}
	return b, nil
}

func payloadRequestID(p any) (uuid.UUID, bool) {
	switch v := p.(type) {
	case *aiservice.GrammarJobRequest:
		if v != nil {
			return v.RequestId, true
		}
	case aiservice.GrammarJobRequest:
		return v.RequestId, true
	case *aiservice.SemanticJobRequest:
		if v != nil {
			return v.RequestId, true
		}
	case aiservice.SemanticJobRequest:
		return v.RequestId, true
	case *aiservice.DialogueJobRequest:
		if v != nil {
			return v.RequestId, true
		}
	case aiservice.DialogueJobRequest:
		return v.RequestId, true
	case *aiservice.GenerateJobRequest:
		if v != nil {
			return v.RequestId, true
		}
	case aiservice.GenerateJobRequest:
		return v.RequestId, true
	case *aiservice.TtsJobRequest:
		if v != nil {
			return v.RequestId, true
		}
	case aiservice.TtsJobRequest:
		return v.RequestId, true
	}
	return uuid.Nil, false
}

// Available — ai-service принимает задачи: breaker не open (half_open — да, проба пройдёт).
func (s *Service) Available() bool { return !s.br.Open() }

// EstWaitSec — оценка ожидания новой задачи типа t (сек, ≥ 1): средняя длительность ×
// очередь её полосы. Без обращения к БД (снимок очереди обновляет диспетчер).
func (s *Service) EstWaitSec(t core.JobType) int {
	i := typeIdx(t)
	if i < 0 {
		return 0
	}
	return s.est.estimateNew(i)
}
