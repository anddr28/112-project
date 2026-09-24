package evaluation

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/access"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/scoring"
	"lct/gocore/internal/store"
)

// Лимиты текстов преподавателя (защита БД и отчётов от «простыней»; UI столько не пишет).
const (
	maxReasonRunes         = 1000
	maxCommentRunes        = 4000
	maxRecommendationRunes = 2000
	maxFieldRunes          = 200
)

// ---------------------------------------------------------------- GET /attempts/{id}/evaluation

// handleGet — результат проверки (partial/done). Студент — своя попытка, преподаватель —
// попытка своего занятия, админ — любая. Оценки ещё нет — 404 (фронт трактует как null).
// Один запрос: доступ, оценка, running из ai_jobs, XP.
func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "attemptId")
	if err != nil {
		return err
	}
	row, err := loadRow(r.Context(), s.pool, sqlViewByAttempt, id)
	if err != nil {
		if pg.IsNoRows(err) {
			return httpx.NotFound("Попытка не найдена")
		}
		return err
	}
	if err := access.ViewAttempt(core.PrincipalFrom(r.Context()), row.accessRow()); err != nil {
		return err
	}
	if !row.Exists {
		return httpx.NotFound("Оценки ещё нет")
	}
	ev := s.buildView(row)
	httpx.WriteJSON(w, http.StatusOK, &ev)
	return nil
}

// ---------------------------------------------------------------- POST .../evaluation/override

type overrideBody struct {
	Score  *float64 `json:"score"`
	Reason *string  `json:"reason"`
}

// overrideAudit — компактный снимок для аудита evaluation.override (before/after).
// xpEarned — корректировка готовой оценки пересчитывает pass_bonus (syncPassBonus).
type overrideAudit struct {
	AttemptID       uuid.UUID `json:"attemptId"`
	TotalScore      float64   `json:"totalScore"`
	FinalScore      float64   `json:"finalScore"`
	Verdict         string    `json:"verdict"`
	OverrideScore   *float64  `json:"overrideScore,omitempty"`
	OverrideVerdict *string   `json:"overrideVerdict,omitempty"`
	OverrideReason  *string   `json:"overrideReason,omitempty"`
	XPEarned        int       `json:"xpEarned"`
}

func overrideSnapshot(r *evalRow) overrideAudit {
	return overrideAudit{
		AttemptID:       r.AttemptID,
		TotalScore:      r.TotalScore,
		FinalScore:      r.finalScore(),
		Verdict:         r.verdictShown(),
		OverrideScore:   r.OverrideScore,
		OverrideVerdict: r.OverrideVerdict,
		OverrideReason:  r.OverrideReason,
		XPEarned:        r.XPEarned,
	}
}

// handleOverride — ручная корректировка балла преподавателем занятия (или админом): балл
// хранится с точностью колонки (сотые, numeric(5,2)) и вердикт считается по нему же —
// 59.5 при пороге 60 остаётся «не зачтено»; обязательна причина; аудит before/after
// и evaluationUpdated обоим каналам после COMMIT. Корректировка возможна и пока AI-слои
// доезжают (как в моке): при финализации итог и XP учтут её. Корректировка уже готовой
// оценки пересчитывает pass_bonus сразу (syncPassBonus) — XP не зависит от того, что
// случилось раньше: корректировка или доезд последнего слоя.
func (s *Service) handleOverride(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "attemptId")
	if err != nil {
		return err
	}
	var body overrideBody
	if err := httpx.ReadJSON(r, &body); err != nil {
		return err
	}
	fields := map[string]string{}
	switch {
	case body.Score == nil:
		fields["score"] = "Укажите балл от 0 до 100"
	case math.IsNaN(*body.Score) || *body.Score < 0 || *body.Score > 100:
		fields["score"] = "Балл — от 0 до 100"
	}
	reason := strings.TrimSpace(deref(body.Reason))
	switch n := utf8.RuneCountInString(reason); {
	case n < 3:
		fields["reason"] = "Укажите причину корректировки (не короче 3 символов)"
	case n > maxReasonRunes:
		fields["reason"] = "Причина корректировки — не длиннее 1000 символов"
	}
	if len(fields) > 0 {
		return httpx.Validation("Проверьте балл и причину корректировки", fields)
	}

	p := core.PrincipalFrom(r.Context())
	var ev public.Evaluation
	err = pg.WithTx(r.Context(), s.pool, func(ctx context.Context, tx pgx.Tx) error {
		row, err := loadRow(ctx, tx, sqlLockEvaluation, id)
		if err != nil {
			if pg.IsNoRows(err) {
				return s.diagnoseNoEvaluation(ctx, tx, p, id)
			}
			return err
		}
		if err := access.ManageAttempt(p, row.accessRow()); err != nil {
			return err
		}
		before := overrideSnapshot(row)

		score := scoring.Round2(*body.Score)
		verdict := scoring.Verdict(score, row.Settings.PassThreshold)
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, sqlOverride, row.ID, score, verdict, reason, p.UserID, now); err != nil {
			return err
		}
		row.OverrideScore, row.OverrideVerdict, row.OverrideReason = &score, &verdict, &reason
		row.OverriddenAt, row.OverriderName = &now, p.ShortName()
		if row.Status == core.EvalDone {
			if err := s.syncPassBonus(ctx, tx, row); err != nil {
				return err
			}
		}

		ev = s.buildView(row)
		after := overrideSnapshot(row)
		pushed := ev
		pg.OnCommit(ctx, func() {
			s.audit(ctx, core.AuditEntry{
				Action: "evaluation.override", EntityType: "evaluation", EntityID: row.ID,
				LessonID: row.LessonID, Before: before, After: after,
			})
			s.publish(row.LessonID, row.AttemptID, &pushed)
		})
		return nil
	})
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, &ev)
	return nil
}

// syncPassBonus — pass_bonus готовой оценки после корректировки: начисление следует итогу
// с корректировкой по тем же правилам, что и финализация (xpInput), поэтому XP одинаков при
// любом порядке «корректировка ↔ доезд последнего AI-слоя». fail → pass добавляет бонус,
// смена балла его пересчитывает, pass → fail снимает. attempt_evaluated/within_norm от
// корректировки не зависят. Изменение видно в аудите evaluation.override (xpEarned).
// Строка оценки уже под FOR UPDATE: финализация и другие корректировки ждут.
func (s *Service) syncPassBonus(ctx context.Context, tx pgx.Tx, r *evalRow) error {
	bonus := passBonus(r.xpInput(), s.st.Get(ctx).XPRules)
	if err := tx.QueryRow(ctx, sqlSyncPassBonus, r.AttemptID, r.UserID, r.LessonID, bonus).Scan(&r.XPEarned); err != nil {
		return fmt.Errorf("evaluation: sync pass_bonus %s: %w", r.AttemptID, err)
	}
	return nil
}

// diagnoseNoEvaluation — почему оценку не удалось заблокировать: попытки нет (404), она
// чужая (403) или её ещё не оценивали (404 «Оценки ещё нет»).
func (s *Service) diagnoseNoEvaluation(ctx context.Context, q pg.Querier, p *core.Principal, id uuid.UUID) error {
	row, err := loadRow(ctx, q, sqlViewByAttempt, id)
	if err != nil {
		if pg.IsNoRows(err) {
			return httpx.NotFound("Попытка не найдена")
		}
		return err
	}
	if err := access.ManageAttempt(p, row.accessRow()); err != nil {
		return err
	}
	if !row.Exists {
		return httpx.NotFound("Оценки ещё нет")
	}
	// Оценка появилась между двумя чтениями (submit в эту же миллисекунду) — пусть повторят.
	return httpx.Conflict("Оценка только что сформирована — повторите корректировку")
}

// ---------------------------------------------------------------- GET /attempts/{id}/feedback

// handleListFeedback — комментарии преподавателя к попытке, по времени. Студент видит
// только отмеченные видимыми и только к своей попытке. Доступ и список — один запрос.
func (s *Service) handleListFeedback(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "attemptId")
	if err != nil {
		return err
	}
	p := core.PrincipalFrom(r.Context())
	rows, err := s.pool.Query(r.Context(), sqlFeedbackByAttempt, id, p.Role != core.RoleStudent)
	if err != nil {
		return err
	}
	defer rows.Close()

	out := []public.TeacherFeedback{}
	found := false
	for rows.Next() {
		var (
			acc                 store.AttemptRow
			fid                 *uuid.UUID
			field, comment, rec *string
			createdAt           *time.Time
			last, first, middle *string
		)
		if err := rows.Scan(&acc.UserID, &acc.LessonTeacherID, &acc.LessonCreatedBy,
			&fid, &field, &comment, &rec, &createdAt, &last, &first, &middle); err != nil {
			return err
		}
		if !found {
			found = true
			if err := access.ViewAttempt(p, &acc); err != nil {
				return err
			}
		}
		if fid == nil {
			continue // комментариев нет — строка только с попыткой
		}
		fb := public.TeacherFeedback{
			Id:             *fid,
			AttemptId:      id,
			TeacherName:    core.ShortName(deref(last), deref(first), deref(middle)),
			Field:          nonBlank(field),
			Comment:        deref(comment),
			Recommendation: nonBlank(rec),
		}
		if createdAt != nil {
			fb.CreatedAt = createdAt.UTC()
		}
		out = append(out, fb)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !found {
		return httpx.NotFound("Попытка не найдена")
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

// ---------------------------------------------------------------- POST /attempts/{id}/feedback

type feedbackBody struct {
	Field          *string `json:"field"`
	Comment        *string `json:"comment"`
	Recommendation *string `json:"recommendation"`
}

// handleAddFeedback — комментарий/рекомендация преподавателя занятия (или админа). Вставка с
// проверкой владельца — одним оператором; отказ разбирается вторым запросом (404/403).
func (s *Service) handleAddFeedback(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "attemptId")
	if err != nil {
		return err
	}
	var body feedbackBody
	if err := httpx.ReadJSON(r, &body); err != nil {
		return err
	}
	comment := strings.TrimSpace(deref(body.Comment))
	field := strings.TrimSpace(deref(body.Field))
	rec := strings.TrimSpace(deref(body.Recommendation))
	fields := map[string]string{}
	switch n := utf8.RuneCountInString(comment); {
	case n == 0:
		fields["comment"] = "Комментарий не может быть пустым"
	case n > maxCommentRunes:
		fields["comment"] = "Комментарий — не длиннее 4000 символов"
	}
	if utf8.RuneCountInString(field) > maxFieldRunes {
		fields["field"] = "Слишком длинный путь поля"
	}
	if utf8.RuneCountInString(rec) > maxRecommendationRunes {
		fields["recommendation"] = "Рекомендация — не длиннее 2000 символов"
	}
	if len(fields) > 0 {
		return httpx.Validation("Проверьте комментарий", fields)
	}

	ctx := r.Context()
	p := core.PrincipalFrom(ctx)
	fid := ids.New()
	var (
		lessonID  uuid.UUID
		createdAt time.Time
	)
	err = s.pool.QueryRow(ctx, sqlAddFeedback, fid, id, p.UserID, nullIfEmpty(field), comment, nullIfEmpty(rec),
		p.Role == core.RoleAdmin).Scan(&lessonID, &createdAt)
	if err != nil {
		if pg.IsNoRows(err) {
			return s.diagnoseFeedbackDenied(ctx, p, id)
		}
		return err
	}
	s.audit(ctx, core.AuditEntry{
		Action: "feedback.add", EntityType: "attempt", EntityID: id, LessonID: lessonID,
		After: map[string]any{"feedbackId": fid, "field": field, "hasRecommendation": rec != ""},
	})
	out := public.TeacherFeedback{
		Id:             fid,
		AttemptId:      id,
		TeacherName:    p.ShortName(),
		Field:          nullIfEmpty(field),
		Comment:        comment,
		Recommendation: nullIfEmpty(rec),
		CreatedAt:      createdAt.UTC(),
	}
	httpx.WriteJSON(w, http.StatusCreated, &out)
	return nil
}

// diagnoseFeedbackDenied — вставка не прошла: попытки нет (404) или занятие чужое (403).
func (s *Service) diagnoseFeedbackDenied(ctx context.Context, p *core.Principal, id uuid.UUID) error {
	var acc store.AttemptRow
	err := s.pool.QueryRow(ctx, sqlAttemptAccess, id).Scan(&acc.UserID, &acc.LessonTeacherID, &acc.LessonCreatedBy)
	if err != nil {
		if pg.IsNoRows(err) {
			return httpx.NotFound("Попытка не найдена")
		}
		return err
	}
	if err := access.ManageAttempt(p, &acc); err != nil {
		return err
	}
	return httpx.Forbidden("Действие доступно преподавателю занятия")
}

// ---------------------------------------------------------------- мелочи

// nonBlank — nil для nil/пустой строки (опциональные поля ответа).
func nonBlank(p *string) *string {
	if p == nil || strings.TrimSpace(*p) == "" {
		return nil
	}
	return p
}

// nullIfEmpty — "" -> NULL (nullable text колонки).
func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
