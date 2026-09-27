package aijobs

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/pg"
)

// RegisterRoutes — маршруты публичного API пакета (относительно /api/v1).
func (s *Service) RegisterRoutes(r *httpx.Router) {
	r.Handle("GET /ai-jobs/{jobId}", httpx.Roles(core.RoleTeacher, core.RoleAdmin), s.getJob)
}

func (s *Service) getJob(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "jobId")
	if err != nil {
		return httpx.NotFound("Задача не найдена")
	}
	v, err := s.JobView(r.Context(), s.pool, id)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, v)
	return nil
}

// JobView — AiJob для фронта (frontend.v1.yaml). Для queued — позиция (1 — следующая) и
// ожидание, для running — оставшееся время (если его можно оценить). Нет задачи —
// *httpx.Error 404 (errors.Is(err, pgx.ErrNoRows) тоже истинно).
func (s *Service) JobView(ctx context.Context, q pg.Querier, id uuid.UUID) (*public.AiJob, error) {
	var (
		typ, status          string
		prio, tryCount       int
		runAfter, createdAt  time.Time
		refType, errText     *string
		refID                *uuid.UUID
		lockedAt, finishedAt *time.Time
	)
	err := q.QueryRow(ctx, sqlJobView, id).Scan(&id, &typ, &status, &prio, &runAfter, &refType, &refID,
		&tryCount, &errText, &createdAt, &lockedAt, &finishedAt)
	if err != nil {
		if pg.IsNoRows(err) {
			return nil, httpx.NotFound("Задача не найдена").Wrap(pgx.ErrNoRows)
		}
		return nil, fmt.Errorf("aijobs: job view: %w", err)
	}
	ti := typeIdx(core.JobType(typ))
	v := &public.AiJob{
		Id:        id,
		Type:      public.AiJobType(typ),
		Status:    public.AiJobStatus(status),
		CreatedAt: createdAt.UTC(),
		RefType:   refType,
		RefId:     refID,
		TryCount:  &tryCount,
	}
	if finishedAt != nil {
		t := finishedAt.UTC()
		v.FinishedAt = &t
	}
	if errText != nil && *errText != "" {
		msg := humanError(*errText)
		v.Error = &msg
	}
	if ti < 0 || (status != core.JobQueued && status != core.JobRunning) {
		return v, nil
	}

	rows, err := q.Query(ctx, sqlAhead, laneTypeNames[laneOf(ti)], prio, runAfter, id, lockedAt)
	if err != nil {
		return nil, fmt.Errorf("aijobs: job queue position: %w", err)
	}
	ahead := make([]aheadRow, 0, 3)
	for rows.Next() {
		var (
			t string
			a aheadRow
		)
		if err := rows.Scan(&t, &a.queuedAhead, &a.runningAhead, &a.runningAll); err != nil {
			rows.Close()
			return nil, fmt.Errorf("aijobs: job queue position: %w", err)
		}
		if a.typ = typeIdx(core.JobType(t)); a.typ >= 0 {
			ahead = append(ahead, a)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("aijobs: job queue position: %w", err)
	}

	avg := s.est.averages()
	now := time.Now()
	if status == core.JobQueued {
		pos, wait := estimateQueued(&avg, ti, ahead, runAfter.Sub(now))
		v.QueuePosition = &pos
		v.EstWaitSec = &wait
		return v, nil
	}
	var elapsed time.Duration
	if lockedAt != nil {
		elapsed = now.Sub(*lockedAt)
	}
	if wait, ok := estimateRunning(&avg, ti, ahead, elapsed); ok {
		v.EstWaitSec = &wait
	}
	return v, nil
}

// Counts — ai_jobs по статусам для админки (SystemHealth.jobs) и метрик: queued/running —
// все, done/failed/cancelled — за последние сутки. Все пять ключей есть всегда.
func (s *Service) Counts(ctx context.Context) (map[string]int, error) {
	out := map[string]int{
		core.JobQueued: 0, core.JobRunning: 0, core.JobDone: 0, core.JobFailed: 0, core.JobCancelled: 0,
	}
	rows, err := s.pool.Query(ctx, sqlCounts, uuidV7Floor(time.Now().Add(-24*time.Hour)))
	if err != nil {
		return nil, fmt.Errorf("aijobs: counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			status string
			n      int
		)
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("aijobs: counts: %w", err)
		}
		out[status] += n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("aijobs: counts: %w", err)
	}
	return out, nil
}
