package scenarios

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/callbacks"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
)

// generateAccepted — 202 POST /scenarios/generate.
type generateAccepted struct {
	JobID      uuid.UUID `json:"jobId"`
	ScenarioID uuid.UUID `json:"scenarioId"`
	EstWaitSec int       `json:"estWaitSec"`
}

// handleGenerate — POST /scenarios/generate: заглушка сценария (draft, source=generated)
// и задача generate_scenario в одной транзакции. Фронт опрашивает GET /ai-jobs/{jobId},
// затем открывает сценарий. Breaker открыт — 503 сразу: задача всё равно не уйдёт.
func (s *Service) handleGenerate(w http.ResponseWriter, r *http.Request) error {
	var in public.GenerateScenarioInput
	if err := httpx.ReadJSON(r, &in); err != nil {
		return err
	}
	fields := map[string]string{}
	info, catID, ok := s.resolveCategory(in.CategoryId)
	if !ok {
		fields["categoryId"] = "Неизвестная категория классификатора"
	}
	if !validDifficulty(int(in.Difficulty)) {
		fields["difficulty"] = "Сложность — 1, 2 или 3"
	}
	if !validMode(string(in.Mode)) {
		fields["mode"] = "Режим — cards, card_actions или both"
	}
	comment := strings.TrimSpace(deref(in.TeacherComment))
	if runes(comment) > commentMax {
		fields["teacherComment"] = "Комментарий — не длиннее 2000 символов"
	}
	if len(fields) > 0 {
		return httpx.Validation("Проверьте параметры генерации", fields)
	}
	if s.queue == nil || !s.queue.Available() {
		return httpx.AIUnavailable("Сервис генерации сценариев временно недоступен. Повторите позже.")
	}

	ctx := r.Context()
	p := core.PrincipalFrom(ctx)
	withDialogue := in.WithDialogue == nil || *in.WithDialogue

	avoid, err := s.avoidTitles(ctx, catID)
	if err != nil {
		return err
	}
	scID, jobID := ids.New(), ids.New()
	req := buildGenerateRequest(jobID, info, int(in.Difficulty), string(in.Mode), comment, avoid)
	meta := model.GenerationMeta{JobID: jobID.String(), WithDialogue: &withDialogue}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	cs := model.CallScript{}
	cs.Normalize()
	csJSON, err := json.Marshal(&cs)
	if err != nil {
		return err
	}
	// Заголовок-заглушка как у мока фронта: виден в списке, пока идёт генерация.
	title := info.Name + " — генерируется…"
	now := time.Now().UTC().Truncate(time.Microsecond)

	err = pg.WithTx(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, sqlInsertScenario, scID, title, catID, int(in.Difficulty), string(in.Mode),
			"generated", core.ScenarioDraft, csJSON, p.UserID, nullIfEmpty(comment), metaJSON, now); err != nil {
			return fmt.Errorf("scenarios: insert generated draft: %w", err)
		}
		id, _, err := s.queue.Enqueue(ctx, tx, core.NewJob{
			ID:       jobID,
			Type:     core.JobGenerateScenario,
			Priority: core.PriorityGenerate,
			DedupKey: "generate:" + scID.String(),
			RefType:  core.RefScenario,
			RefID:    scID,
			Payload:  req,
		})
		jobID = id
		return err
	})
	if err != nil {
		return err
	}
	s.audit(ctx, "scenario.generate", "scenario", scID, nil, map[string]any{
		"jobId": jobID, "categoryId": catID, "categoryCode": info.Code, "difficulty": int(in.Difficulty),
		"mode": string(in.Mode), "withDialogue": withDialogue,
	})
	httpx.WriteJSON(w, http.StatusAccepted, generateAccepted{
		JobID: jobID, ScenarioID: scID, EstWaitSec: s.queue.EstWaitSec(core.JobGenerateScenario),
	})
	return nil
}

// avoidTitles — до 20 заголовков категории: LLM не должна повторять готовые сценарии.
func (s *Service) avoidTitles(ctx context.Context, catID uuid.UUID) ([]string, error) {
	rows, err := s.pool.Query(ctx, sqlAvoidTitles, catID)
	if err != nil {
		return nil, fmt.Errorf("scenarios: avoid titles: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0, 20)
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// buildGenerateRequest — тело POST /v1/jobs/generate (request_id = id задачи).
func buildGenerateRequest(jobID uuid.UUID, info core.IncidentTypeInfo, difficulty int, mode, comment string, avoid []string) *aiservice.GenerateJobRequest {
	prio := core.PriorityGenerate
	profile := components.Generate
	lang := "ru"
	req := &aiservice.GenerateJobRequest{
		SchemaVersion: components.N1,
		RequestId:     jobID,
		Priority:      &prio,
		Profile:       &profile,
	}
	req.Options = &struct {
		Lang *string `json:"lang,omitempty"`
	}{Lang: &lang}
	req.Spec.Category.Code = info.Code
	req.Spec.Category.Name = info.Name
	req.Spec.Category.Path = convert.SlicePtr(info.Path)
	req.Spec.Category.Attributes = convert.SlicePtr(info.Attributes)
	req.Spec.Category.Services = convert.SlicePtr(info.Services)
	req.Spec.Difficulty = difficulty
	req.Spec.Mode = aiservice.GenerateJobRequestSpecMode(mode)
	req.Spec.TeacherComment = convert.NonEmpty(comment)
	req.Spec.AvoidTitles = convert.SlicePtr(avoid)
	return req
}

// ---------------------------------------------------------------- результат генерации

// generateHandler — core.AIResultHandler для generate_scenario.
type generateHandler struct{ s *Service }

// ApplyResult — легенда, эталон v1 (или следующая версия), чек-лист разговора,
// status=generated, engine генерации в generation_meta. Применяется только к заглушке,
// которую преподаватель не успел отклонить (status=draft, не в архиве); иначе результат
// тихо отбрасывается — задача всё равно закрывается (ошибка вызвала бы ретраи callback'а).
func (h generateHandler) ApplyResult(ctx context.Context, tx pgx.Tx, job core.JobRecord, res *callbacks.AiResult) error {
	s := h.s
	var (
		status   string
		isArch   bool
		catID    uuid.UUID
		metaJSON []byte
	)
	err := tx.QueryRow(ctx, sqlLockGenerated, job.RefID).Scan(&status, &isArch, &catID, &metaJSON)
	if err != nil {
		if pg.IsNoRows(err) {
			s.log.Warn("generate result for missing scenario", "job", job.ID, "scenario", job.RefID)
			return nil
		}
		return err
	}
	if isArch || status != core.ScenarioDraft {
		s.log.Info("generate result ignored: scenario is no longer awaiting generation",
			"job", job.ID, "scenario", job.RefID, "status", status)
		return nil
	}
	if res.Scenario == nil {
		return h.ApplyFailure(ctx, tx, job, "bad_payload", "пустой результат генерации")
	}

	var meta model.GenerationMeta
	if len(metaJSON) > 0 {
		_ = json.Unmarshal(metaJSON, &meta) // битые метаданные не мешают применить результат
	}
	withDialogue := meta.WithDialogue == nil || *meta.WithDialogue
	info, _ := s.catalogType(catID)
	sc := res.Scenario

	cs := convert.CallScriptFromContract(&sc.CallScript)
	if withDialogue {
		sanitizeBrief(cs.Dialogue)
	} else {
		cs.Dialogue = nil
	}
	cs.Normalize()

	title := strings.TrimSpace(sc.Title)
	if title == "" {
		title = info.Name + " — учебный сценарий"
	}
	if r := []rune(title); len(r) > titleMax {
		title = string(r[:titleMax])
	}

	e := s.generatedEtalon(sc, &cs, info, withDialogue)
	enc, err := e.encode()
	if err != nil {
		return err
	}
	meta.SetEngine(&res.Engine)
	meta.NotesForTeacher = strings.TrimSpace(deref(sc.NotesForTeacher))
	meta.DifficultyEstimate = deref(sc.DifficultyEstimate)
	if meta.JobID == "" {
		meta.JobID = job.ID.String()
	}
	newMeta, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	csJSON, err := json.Marshal(&cs)
	if err != nil {
		return err
	}

	if _, _, err := insertEtalonVersion(ctx, tx, job.RefID, enc, nil); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, sqlApplyGenerated, job.RefID, title, csJSON, newMeta); err != nil {
		return fmt.Errorf("scenarios: apply generated scenario: %w", err)
	}
	return nil
}

// ApplyFailure — генерация окончательно провалилась: заглушка в архив, причина —
// в generation_meta (UI узнаёт о провале из статуса задачи и предлагает повторить).
func (h generateHandler) ApplyFailure(ctx context.Context, tx pgx.Tx, job core.JobRecord, code, message string) error {
	h.s.log.Warn("scenario generation failed", "job", job.ID, "scenario", job.RefID, "code", code)
	if _, err := tx.Exec(ctx, sqlGenerateFailed, job.RefID, truncate(message, 1000), code); err != nil {
		return fmt.Errorf("scenarios: mark generation failed: %w", err)
	}
	return nil
}

// catalogType — тип классификатора по uuid категории сценария.
func (s *Service) catalogType(id uuid.UUID) (core.IncidentTypeInfo, bool) {
	if s.cat == nil {
		return core.IncidentTypeInfo{}, false
	}
	return s.cat.TypeByID(id.String())
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
