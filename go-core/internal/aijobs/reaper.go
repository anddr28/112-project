package aijobs

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/platform/pg"
)

// reaperLoop — раз в минуту: running-задачи без результата дольше settings.ai.reaper_after_sec.
// ai-service stateless: его перезапуск теряет очередь в памяти, а callback, который не удалось
// доставить за 3 попытки, отбрасывается. Такие задачи отправляются повторно с тем же request_id
// (ai-service идемпотентен: держит — 202 без дубля, выполнил — перешлёт callback, потерял —
// посчитает заново); попытка на это не тратится — задача, долго ждущая в очереди ai-service,
// не виновата. Без результата дольше reaperMaxResends × reaper_after_sec с первой отправки —
// failed + ApplyFailure("reaper_timeout") (poison pill не крутится вечно).
// Тем же проходом — failUnavailable: оценочные задачи при долгой недоступности ai-service.
func (s *Service) reaperLoop(ctx context.Context) {
	defer s.loops.Done()
	t := time.NewTicker(reaperEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.reap(ctx)
		s.failUnavailable(ctx)
	}
}

func (s *Service) reap(ctx context.Context) {
	ai := s.aiSettings()
	after := float64(ai.ReaperAfterSec)
	hold := reaperHoldSec(ai)

	qctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	tag, err := s.pool.Exec(qctx, sqlReaperRequeue, after, hold,
		errorText(codeResent, "нет результата от ai-service дольше reaper_after_sec"))
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("ai jobs reaper: requeue failed", "err", err)
		}
		return
	}
	if n := tag.RowsAffected(); n > 0 {
		s.log.Warn("ai jobs reaper: re-dispatching stuck jobs", "count", n, "after_sec", after)
		s.st.reaped.Add(uint64(n))
		s.st.requeued.Add(uint64(n))
		s.kick()
	}

	// Исчерпавшие попытки — по одной транзакции на задачу: ошибка обработчика одной задачи
	// не блокирует остальные (и повторится на следующем проходе).
	ids, err := s.selectIDs(ctx, sqlReaperExhausted, after, hold)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("ai jobs reaper: select exhausted failed", "err", err)
		}
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		s.reapOne(ctx, id, after)
	}
}

func (s *Service) selectIDs(ctx context.Context, sql string, args ...any) ([]uuid.UUID, error) {
	qctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	rows, err := s.pool.Query(qctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Service) reapOne(ctx context.Context, id uuid.UUID, after float64) {
	tctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	failed := false
	err := pg.WithTx(tctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		rec, err := scanJob(tx.QueryRow(ctx, sqlReaperLock, id, after))
		if err != nil {
			if pg.IsNoRows(err) {
				return nil // успел прийти callback или строку держит другой обработчик
			}
			return err
		}
		failed = true
		return s.finishFailed(ctx, tx, rec, 1, CodeReaperTimeout, "нет результата от ai-service, переотправки исчерпаны", nil)
	})
	if err != nil {
		s.log.Error("ai jobs reaper: fail exhausted job", "job", id, "err", err)
		return
	}
	if !failed {
		return
	}
	s.st.reaped.Add(1)
	s.log.Warn("ai jobs reaper: job failed, no result after max resends", "job", id)
}

// failUnavailable — ai-service не принимает задачи (breaker open / токен отвергнут) дольше
// settings.ai.reaper_after_sec: оценочные задачи, ждущие в очереди go-core столько же,
// проваливаются с ai_unavailable. Иначе на всё время аварии оценки висели бы «частичными»:
// без вердикта, XP и флагов aiUnavailable/needsReview (DESIGN §5: AI-слой окончательно
// failed → итог по доступным слоям + ревью преподавателя). Задачи, которые ai-service принял
// (running), сюда не попадают: их сначала вернёт в очередь reaper, пока авария продолжается.
// generate_scenario и tts не трогаем: фоновые, доедут, когда ai-service вернётся.
// Признак недоступности — breaker этого инстанса (развёртывание — один go-core).
func (s *Service) failUnavailable(ctx context.Context) {
	after := float64(s.aiSettings().ReaperAfterSec)
	down := s.unavailableFor(time.Now())
	if down == 0 || down.Seconds() < after {
		return
	}
	ids, err := s.selectIDs(ctx, sqlUnavailableIDs, evaluateTypeNames, after)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("ai jobs: select jobs to fail while ai-service unavailable", "err", err)
		}
		return
	}
	msg := fmt.Sprintf("ai-service недоступен дольше %d с", int(after))
	n := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		tctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
		failed := false
		err := pg.WithTx(tctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
			rec, err := scanJob(tx.QueryRow(ctx, sqlLockQueued, id))
			if err != nil {
				if pg.IsNoRows(err) {
					return nil // успели отправить или закрыть
				}
				return err
			}
			failed = true
			return s.finishFailed(ctx, tx, rec, 0, CodeAIUnavailable, msg, nil)
		})
		cancel()
		if err != nil {
			s.log.Error("ai jobs: fail job while ai-service unavailable", "job", id, "err", err)
			continue
		}
		if failed {
			n++
		}
	}
	if n > 0 {
		s.log.Warn("ai jobs: evaluation jobs failed, ai-service unavailable", "count", n,
			"down_sec", int(down.Seconds()))
	}
}
