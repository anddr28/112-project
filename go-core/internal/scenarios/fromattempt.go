package scenarios

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/store"
)

// Сценарий из карточки обучающегося (контракт v1.4, ТЗ режим 2: «сформированные
// обучающимися карточки»). Карточка оценённой попытки режима cards становится эталоном v1
// нового сценария source=student; легенда, категория и сложность — из исходного сценария,
// критерии оценки (обязательные поля, факты, действия, чек-лист) — из версии эталона, по
// которой работал обучающийся. Связь хранится в scenarios.source_attempt_id (db-design Р30):
// частичный уникальный индекс даёт идемпотентность и под гонкой двух запросов.

// sqlAttemptForScenario — попытка, её занятие, исходный сценарий и версия эталона одним запросом;
// последним столбцом — уже созданный из этой попытки сценарий (идемпотентность).
const sqlAttemptForScenario = `
SELECT a.status, a.mode, a.service_id IS NOT NULL, a.card, l.teacher_id, l.created_by,
       s.id, s.title, s.category_id, s.difficulty, s.call_script,
       et.scoring, et.expected_actions, et.expected_dialogue,
       e.final_score,
       (SELECT sc.id FROM scenarios sc WHERE sc.source_attempt_id = a.id)
  FROM attempts a
  JOIN lessons l ON l.id = a.lesson_id
  JOIN scenarios s ON s.id = a.scenario_id
  JOIN etalons et ON et.id = a.etalon_id
  LEFT JOIN evaluations e ON e.attempt_id = a.id
 WHERE a.id = $1`

const sqlScenarioBySourceAttempt = `SELECT id FROM scenarios WHERE source_attempt_id = $1`

// sqlInsertStudentScenario — как sqlInsertScenarioWithEtalon, плюс source_attempt_id.
const sqlInsertStudentScenario = `
WITH s AS (
  INSERT INTO scenarios (id, title, category_id, difficulty, mode, source, status, call_script, author_id,
                         generation_meta, created_at, updated_at, version, source_attempt_id)
  VALUES ($1, $2, $3, $4, 'card_actions', 'student', 'draft', $5, $6, $7, $8, $8, 1, $9)
  RETURNING id
)
INSERT INTO etalons (id, scenario_id, version, is_current, card, card_draft, scoring, expected_actions,
                     expected_dialogue, created_by, created_at)
SELECT $10, s.id, 1, true, $11, $12, $13, $14, $15, $6, $8 FROM s`

type toScenarioBody struct {
	Title *string `json:"title"`
}

// fromAttempt — строка sqlAttemptForScenario.
type fromAttempt struct {
	status, mode         string
	dds                  bool
	card                 []byte
	teacherID, createdBy *uuid.UUID
	srcID                uuid.UUID
	srcTitle             string
	categoryID           uuid.UUID
	difficulty           int
	callScript           []byte
	scoring, actions     []byte
	dialogue             []byte
	finalScore           *float64
	existing             *uuid.UUID
}

func (f *fromAttempt) ownerID() uuid.UUID {
	if f.teacherID != nil {
		return *f.teacherID
	}
	if f.createdBy != nil {
		return *f.createdBy
	}
	return uuid.Nil
}

// handleFromAttempt — POST /attempts/{attemptId}/to-scenario (преподаватель занятия).
func (s *Service) handleFromAttempt(w http.ResponseWriter, r *http.Request) error {
	attemptID, err := httpx.PathUUID(r, "attemptId")
	if err != nil {
		return httpx.NotFound("Попытка не найдена")
	}
	body, err := readOptionalJSON[toScenarioBody](r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	p := core.PrincipalFrom(ctx)

	var f fromAttempt
	if err := s.pool.QueryRow(ctx, sqlAttemptForScenario, attemptID).Scan(&f.status, &f.mode, &f.dds, &f.card,
		&f.teacherID, &f.createdBy, &f.srcID, &f.srcTitle, &f.categoryID, &f.difficulty, &f.callScript,
		&f.scoring, &f.actions, &f.dialogue, &f.finalScore, &f.existing); err != nil {
		if pg.IsNoRows(err) {
			return httpx.NotFound("Попытка не найдена")
		}
		return fmt.Errorf("scenarios: to-scenario: load: %w", err)
	}
	if p.Role != core.RoleTeacher || f.ownerID() != p.UserID {
		return httpx.Forbidden("Сделать сценарий из карточки может преподаватель занятия")
	}
	if f.existing != nil {
		return errAlreadyCreated(*f.existing)
	}
	switch {
	case f.status != core.AttemptEvaluated:
		return httpx.Conflict("Карточка ещё не оценена — сценарий делается из оценённой карточки").
			WithDetails(map[string]any{"attemptStatus": f.status})
	case f.mode != core.ModeCards || f.dds:
		return httpx.Conflict("Сценарий делается только из карточки режима «Карточки» (ракурс оператора 112)")
	case len(bytes.TrimSpace(f.card)) == 0 || bytes.Equal(bytes.TrimSpace(f.card), []byte("null")):
		return httpx.Conflict("В попытке нет сданной карточки")
	}

	title := "Карточка обучающегося: " + f.srcTitle
	if body.Title != nil && strings.TrimSpace(*body.Title) != "" {
		title = strings.TrimSpace(*body.Title)
	}
	title = truncRunes(title, titleMax)
	if msg := checkTitle(title); msg != "" {
		return httpx.Validation("Проверьте название сценария", map[string]string{"title": msg})
	}

	e, err := f.etalon(s)
	if err != nil {
		return err
	}
	enc, err := e.encode()
	if err != nil {
		return err
	}
	meta := model.GenerationMeta{
		NotesForTeacher: "Эталон v1 — карточка обучающегося (итог " + scoreText(f.finalScore) +
			"). Проверьте поля эталона перед подтверждением: карточка могла содержать ошибки.",
		Extra: map[string]any{"source_attempt_id": attemptID.String(), "source_scenario_id": f.srcID.String()},
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	id := ids.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := s.pool.Exec(ctx, sqlInsertStudentScenario, id, title, f.categoryID, f.difficulty, f.callScript,
		p.UserID, metaJSON, now, attemptID,
		ids.New(), enc.card, enc.draft, enc.scoring, enc.actions, enc.dialogue); err != nil {
		if pg.IsUniqueViolation(err) {
			// Параллельный запрос успел первым — отвечаем, как на повтор.
			var existing uuid.UUID
			if qerr := s.pool.QueryRow(ctx, sqlScenarioBySourceAttempt, attemptID).Scan(&existing); qerr == nil {
				return errAlreadyCreated(existing)
			}
		}
		return fmt.Errorf("scenarios: to-scenario: insert: %w", err)
	}
	s.audit(ctx, "scenario.create", "scenario", id, nil, map[string]any{
		"title": title, "source": "student", "sourceAttemptId": attemptID, "sourceScenarioId": f.srcID,
	})
	out, err := store.GetScenario(ctx, s.pool, id)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusCreated, store.ScenarioToPublic(out))
	return nil
}

func errAlreadyCreated(id uuid.UUID) *httpx.Error {
	return httpx.Conflict("Сценарий из этой карточки уже создан").WithDetails(map[string]any{"scenarioId": id.String()})
}

// etalon — эталон v1: форма АРМ = карточка обучающегося (без истории статусов служб),
// контрактная карточка — её проекция; критерии оценки — из версии эталона попытки.
func (f *fromAttempt) etalon(s *Service) (etalonData, error) {
	var d public.IncidentCardDraft
	if err := json.Unmarshal(f.card, &d); err != nil {
		return etalonData{}, fmt.Errorf("scenarios: to-scenario: card: %w", err)
	}
	convert.NormalizeDraft(&d)
	d.ActionsTaken = strings.TrimSpace(d.ActionsTaken)
	for i := range d.Services {
		d.Services[i].History, d.Services[i].AllowedNext = []public.ReactionStatusEntry{}, []public.AllowedTransition{}
	}
	e := etalonData{Draft: d, Card: convert.DraftToCard(&d, s.cat), Actions: []model.ExpectedAction{}}
	if err := unmarshalOptional(f.scoring, &e.Scoring); err != nil {
		return e, fmt.Errorf("scenarios: to-scenario: scoring: %w", err)
	}
	if len(e.Scoring.RequiredFields) == 0 {
		e.Scoring.RequiredFields = model.DefaultRequiredFields()
	}
	if err := unmarshalOptional(f.actions, &e.Actions); err != nil {
		return e, fmt.Errorf("scenarios: to-scenario: expected_actions: %w", err)
	}
	if e.Actions == nil {
		e.Actions = []model.ExpectedAction{}
	}
	var dlg model.ExpectedDialogue
	if err := unmarshalOptional(f.dialogue, &dlg); err != nil {
		return e, fmt.Errorf("scenarios: to-scenario: expected_dialogue: %w", err)
	}
	if len(dlg.Checklist) > 0 {
		e.Dialogue = &dlg
	}
	return e, nil
}

// unmarshalOptional — jsonb, где '{}', '[]' и NULL значат «нет».
func unmarshalOptional(raw []byte, dst any) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" {
		return nil
	}
	return json.Unmarshal(raw, dst)
}

// readOptionalJSON — необязательное JSON-тело (requestBody.required = false): пусто — нули.
func readOptionalJSON[T any](r *http.Request) (T, error) {
	var v T
	raw, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, httpx.MaxJSONBody))
	if err != nil {
		return v, httpx.BadRequest("Не удалось прочитать тело запроса")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return v, nil
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, httpx.BadRequest("Некорректный JSON: " + err.Error())
	}
	return v, nil
}

func scoreText(p *float64) string {
	if p == nil {
		return "—"
	}
	return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.1f", *p), "0"), ".")
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
