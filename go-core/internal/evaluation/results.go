package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/callbacks"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/scoring"
	"lct/gocore/internal/settings"
	"lct/gocore/internal/store"
)

// Код провала слоя, когда ai-service прислал формально валидный, но непригодный результат
// (балл не число и т.п.): ретрай не поможет — слой failed, попытку проверит преподаватель.
const codeInvalidResult = "invalid_result"

// ApplyResult — core.AIResultHandler: доехал AI-слой. Вызывается aijobs в транзакции
// callback'а (строка ai_jobs уже заблокирована). Ошибка возвращается ТОЛЬКО для временных
// проблем БД (aijobs ответит 500, ai-service повторит); непригодный результат — слой failed
// и nil: повтор того же результата ничего бы не исправил.
func (s *Service) ApplyResult(ctx context.Context, tx pgx.Tx, job core.JobRecord, res *callbacks.AiResult) error {
	layer, attemptID, ok := s.jobTarget(job, res)
	if !ok {
		return nil
	}
	r, err := s.lockForLayer(ctx, tx, attemptID, layer, job)
	if err != nil || r == nil {
		return err
	}
	snap := s.st.Get(ctx)
	if problem := r.applyAI(layer, res, snap.ConfidenceThreshold); problem != "" {
		s.log.Warn("evaluation: unusable AI result, layer marked failed",
			"attempt", attemptID, "job", job.ID, "layer", layer, "problem", problem)
		r.markFailed(layer, codeInvalidResult)
	}
	return s.save(ctx, tx, r, snap)
}

// ApplyFailure — core.AIResultHandler: слой окончательно не посчитан (ретраи исчерпаны,
// 400 от ai-service, reaper). Итог — по остальным слоям, оценка помечается «ревью» и
// «AI недоступен». message в лог не пишется: ai-service может эхом вернуть текст ответа.
func (s *Service) ApplyFailure(ctx context.Context, tx pgx.Tx, job core.JobRecord, code, message string) error {
	layer, attemptID, ok := s.jobTarget(job, nil)
	if !ok {
		return nil
	}
	r, err := s.lockForLayer(ctx, tx, attemptID, layer, job)
	if err != nil || r == nil {
		return err
	}
	s.log.Warn("evaluation: AI layer failed", "attempt", attemptID, "job", job.ID, "layer", layer, "code", code)
	r.markFailed(layer, code)
	return s.save(ctx, tx, r, s.st.Get(ctx))
}

// jobTarget — слой и попытка задачи. Не оценочная задача / нет ссылки — лог и пропуск
// (не ошибка: повтор callback'а ничего не изменит).
func (s *Service) jobTarget(job core.JobRecord, res *callbacks.AiResult) (layer string, attemptID uuid.UUID, ok bool) {
	layer = job.Type.Layer()
	if job.RefType == core.RefAttempt {
		attemptID = job.RefID
	}
	if attemptID == uuid.Nil && res != nil && res.AttemptId != nil {
		attemptID = *res.AttemptId
	}
	if layer == "" || attemptID == uuid.Nil {
		s.log.Warn("evaluation: job without evaluation target ignored", "job", job.ID, "type", job.Type, "ref_type", job.RefType)
		return "", uuid.Nil, false
	}
	return layer, attemptID, true
}

// lockForLayer — оценка под FOR UPDATE, если слой ещё ждёт результата. nil, nil — применять
// нечего: оценки нет (попытку удалили) или слой уже терминален (дубль после переотправки).
func (s *Service) lockForLayer(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, layer string, job core.JobRecord) (*evalRow, error) {
	r, err := loadRow(ctx, tx, sqlLockEvaluation, attemptID)
	if err != nil {
		if pg.IsNoRows(err) {
			s.log.Warn("evaluation: AI result for attempt without evaluation ignored", "attempt", attemptID, "job", job.ID)
			return nil, nil
		}
		return nil, fmt.Errorf("evaluation: lock %s: %w", attemptID, err)
	}
	st, expected := r.Layers[layer]
	if !expected || layerTerminal(st) {
		s.log.Info("evaluation: AI result for settled layer ignored", "attempt", attemptID, "job", job.ID, "layer", layer, "state", st)
		return nil, nil
	}
	return r, nil
}

// applyAI — результат слоя в строку. "" — применено; иначе — почему результат непригоден.
func (r *evalRow) applyAI(layer string, res *callbacks.AiResult, confThreshold float64) string {
	switch layer {
	case core.LayerGrammar:
		g := res.Grammar
		if g == nil {
			return "нет результата grammar"
		}
		score, ok := score100(g.Score)
		if !ok {
			return "grammar.score не число"
		}
		remarks := g.Remarks
		if remarks == nil {
			remarks = []components.GrammarRemark{}
		}
		rr, err := json.Marshal(remarks)
		if err != nil {
			return "grammar.remarks: " + err.Error()
		}
		stats := model.GrammarStats{WordsChecked: g.Stats.WordsChecked}
		if g.Stats.ErrorsBySeverity != nil {
			stats.ErrorsBySeverity = *g.Stats.ErrorsBySeverity
		}
		sr, err := json.Marshal(stats)
		if err != nil {
			return "grammar.stats: " + err.Error()
		}
		r.GrammarScore, r.GrammarRemarks, r.GrammarStats = &score, rr, sr

	case core.LayerSemantic:
		sm := res.Semantic
		if sm == nil {
			return "нет результата semantic"
		}
		score, ok := score100(sm.Score)
		if !ok {
			return "semantic.score не число"
		}
		raw, err := json.Marshal(sm)
		if err != nil {
			return "semantic: " + err.Error()
		}
		r.SemanticScore, r.Semantic = &score, raw
		if lowConfidence(sm.Confidence, confThreshold) {
			r.NeedsReview = true
		}

	case core.LayerDialogue:
		d := res.Dialogue
		if d == nil {
			return "нет результата dialogue"
		}
		score, ok := score100(d.Score)
		if !ok {
			return "dialogue.score не число"
		}
		raw, err := json.Marshal(d)
		if err != nil {
			return "dialogue: " + err.Error()
		}
		r.DialogueScore, r.Dialogue = &score, raw
		if lowConfidence(d.Confidence, confThreshold) {
			r.NeedsReview = true
		}

	default:
		return "неизвестный слой " + layer
	}
	r.Layers[layer] = core.LayerDone
	r.Engine[layer] = res.Engine // модель/промпт/версия правил — воспроизводимость для QA
	return ""
}

// lowConfidence — уверенность модели ниже порога (не число — тоже «ниже»: доверять нечему).
// Сравнение — в точности float32, в которой уверенность пришла: 0.7 из JSON в float32 —
// это 0.699999988, и против порога float64 0.7 «ровно на пороге» превращалось бы в «ниже»
// (контракт: ревью только при confidence < порога).
func lowConfidence(c float32, threshold float64) bool {
	return math.IsNaN(float64(c)) || c < float32(threshold)
}

// save — пересчёт итога, запись оценки, финализация при последнем слое, пуш после COMMIT.
func (s *Service) save(ctx context.Context, tx pgx.Tx, r *evalRow, snap *settings.Snapshot) error {
	finalized := r.recompute(time.Now().UTC())
	layers, err := json.Marshal(r.Layers)
	if err != nil {
		return fmt.Errorf("evaluation: encode layers: %w", err)
	}
	engine, err := json.Marshal(r.Engine)
	if err != nil {
		return fmt.Errorf("evaluation: encode engine: %w", err)
	}
	if _, err := tx.Exec(ctx, sqlSaveEvaluation, r.ID, r.Status, r.GrammarScore, r.SemanticScore, r.DialogueScore,
		r.TotalScore, r.Verdict, orDefault(r.GrammarRemarks, jsonEmptyArray), orDefault(r.GrammarStats, jsonEmptyObject),
		orDefault(r.Semantic, jsonEmptyObject), orDefault(r.Dialogue, jsonEmptyObject),
		json.RawMessage(layers), json.RawMessage(engine), r.NeedsReview, r.AIUnavailable, r.EvaluatedAt,
	); err != nil {
		return fmt.Errorf("evaluation: save %s: %w", r.AttemptID, err)
	}
	if finalized {
		if err := s.finalize(ctx, tx, r, snap); err != nil {
			return err
		}
	}
	ev := s.buildView(r)
	pg.OnCommit(ctx, func() { s.publish(r.LessonID, r.AttemptID, &ev) })
	return nil
}

// finalize — последствия окончательной оценки в той же транзакции, одним round-trip (batch):
// попытка → evaluated, XP за попытку, а если этой оценкой студент выполнил все карточки
// занятия (sqlLessonCompleted) — XP lesson_completed и участник finished (participantStatus
// после COMMIT).
func (s *Service) finalize(ctx context.Context, tx pgx.Tx, r *evalRow, snap *settings.Snapshot) error {
	entries := scoring.XPEntries(r.xpInput(), snap.XPRules)
	deltas := make([]int32, len(entries))
	reasons := make([]string, len(entries))
	for i, e := range entries {
		deltas[i], reasons[i] = int32(e.Delta), e.Reason
	}

	b := &pgx.Batch{}
	b.Queue(sqlFinalizeAttempt, r.AttemptID, r.UserID, r.LessonID, deltas, reasons)
	b.Queue(sqlLockParticipant, r.LessonID, r.UserID)
	b.Queue(sqlLessonCompleted, r.LessonID, r.UserID, r.AttemptID, r.Settings.CardsPerStudent, snap.XPRules.LessonCompleted)

	var (
		updated, added, lessonXP int
		done                     bool
		pUser                    *uuid.UUID
		pStatus                  *string
		pJoined, pFinished       *time.Time
		pMic                     *bool
		uLast, uFirst, uMiddle   *string
		lastAttempt              *uuid.UUID
	)
	br := tx.SendBatch(ctx, b)
	err := br.QueryRow().Scan(&updated, &added)
	if err == nil {
		_, err = br.Exec()
	}
	if err == nil {
		err = br.QueryRow().Scan(&done, &lessonXP, &pUser, &pStatus, &pJoined, &pFinished, &pMic,
			&uLast, &uFirst, &uMiddle, &lastAttempt)
	}
	err = errors.Join(err, br.Close())
	if err != nil {
		return fmt.Errorf("evaluation: finalize %s: %w", r.AttemptID, err)
	}

	r.XPEarned += added
	if updated > 0 {
		r.AttemptStatus = core.AttemptEvaluated
	}
	if pUser != nil {
		p := store.ParticipantToPublic(&store.ParticipantRow{
			UserID:     *pUser,
			LastName:   deref(uLast),
			FirstName:  deref(uFirst),
			MiddleName: deref(uMiddle),
			Status:     deref(pStatus),
			JoinedAt:   utcPtr(pJoined),
			FinishedAt: utcPtr(pFinished),
			MicReady:   deref(pMic),
			AttemptID:  lastAttempt,
		})
		lessonID := r.LessonID
		pg.OnCommit(ctx, func() { s.publishParticipant(lessonID, &p) })
	}
	return nil
}
