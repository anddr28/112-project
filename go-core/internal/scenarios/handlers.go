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
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/store"
)

// ---------------------------------------------------------------- чтение

// handleList — GET /scenarios?status&categoryId&mode. Архив скрыт, если не запрошен явно.
func (s *Service) handleList(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	f := store.ScenarioFilter{Status: strings.TrimSpace(q.Get("status")), Mode: strings.TrimSpace(q.Get("mode"))}
	fields := map[string]string{}
	if f.Status != "" && !validStatus(f.Status) {
		fields["status"] = "Неизвестный статус сценария"
	}
	if f.Mode != "" && !validMode(f.Mode) {
		fields["mode"] = "Режим — cards, card_actions или both"
	}
	if len(fields) > 0 {
		return httpx.Validation("Некорректный фильтр списка сценариев", fields)
	}
	if key := strings.TrimSpace(q.Get("categoryId")); key != "" {
		id, err := uuid.Parse(key)
		if err != nil {
			var ok bool
			if _, id, ok = s.resolveCategory(key); !ok {
				// Неизвестная категория — пустой список, а не ошибка: фильтр ничего не нашёл.
				httpx.WriteRawJSON(w, http.StatusOK, []byte("[]"))
				return nil
			}
		}
		f.CategoryID = &id
	}
	// Постранично (keyset, без OFFSET): строка сценария тяжёлая — легенда и эталон целиком.
	page, err := httpx.ParsePage(r, scenariosPageDefault, scenariosPageMax)
	if err != nil {
		return err
	}
	if page.Cursor != nil {
		k, ok := store.ParseTimeKey(page.Cursor)
		if !ok {
			return httpx.BadCursor()
		}
		f.After = k
	}
	f.Limit = page.Limit + 1 // +1 строка — есть ли следующая страница, без count(*)
	rows, err := store.ListScenarios(r.Context(), s.pool, f)
	if err != nil {
		return err
	}
	if len(rows) > page.Limit {
		rows = rows[:page.Limit]
		httpx.SetNextCursor(w, rows[len(rows)-1].Key().Parts()...)
	}
	httpx.WriteJSON(w, http.StatusOK, store.ScenariosToPublic(rows))
	return nil
}

// Размеры страниц GET /scenarios (контракт v1.2: ?limit).
const (
	scenariosPageDefault = 100
	scenariosPageMax     = 200
)

// handleGet — GET /scenarios/{id}: сценарий целиком (эталон, бриф, чек-лист).
func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "scenarioId")
	if err != nil {
		return errNotFound()
	}
	row, err := store.GetScenario(r.Context(), s.pool, id)
	if err != nil {
		if pg.IsNoRows(err) {
			return errNotFound()
		}
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, store.ScenarioToPublic(row))
	return nil
}

// ---------------------------------------------------------------- создание вручную

// handleCreate — POST /scenarios: source=manual, status=draft, пустая легенда и эталон v1
// (пустая форма АРМ с типом происшествия = категория, обязательные поля по умолчанию —
// как в моке фронта). Один оператор INSERT … WITH: атомарно и за один round-trip;
// ответ собирается в памяти (всё, что в нём есть, известно до записи).
func (s *Service) handleCreate(w http.ResponseWriter, r *http.Request) error {
	var in public.CreateScenarioInput
	if err := httpx.ReadJSON(r, &in); err != nil {
		return err
	}
	title := strings.TrimSpace(in.Title)
	fields := map[string]string{}
	if msg := checkTitle(title); msg != "" {
		fields["title"] = msg
	}
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
	if len(fields) > 0 {
		return httpx.Validation("Проверьте поля сценария", fields)
	}
	ctx := r.Context()
	p := core.PrincipalFrom(ctx)

	cs := model.CallScript{}
	cs.Normalize()
	draft := convert.EmptyDraft()
	draft.IncidentTypeIds = append(draft.IncidentTypeIds, info.ID)
	e := etalonData{
		Card:    convert.DraftToCard(&draft, s.cat),
		Draft:   draft,
		Scoring: model.Scoring{RequiredFields: model.DefaultRequiredFields()},
		Actions: []model.ExpectedAction{},
	}
	enc, err := e.encode()
	if err != nil {
		return err
	}
	csJSON, err := json.Marshal(&cs)
	if err != nil {
		return err
	}
	id, eid := ids.New(), ids.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	author := p.UserID
	if _, err := s.pool.Exec(ctx, sqlInsertScenarioWithEtalon,
		id, title, catID, int(in.Difficulty), string(in.Mode), "manual", core.ScenarioDraft, csJSON, author,
		nil, nil, nil, []byte("{}"), now,
		eid, enc.card, enc.draft, enc.scoring, enc.actions, enc.dialogue); err != nil {
		return fmt.Errorf("scenarios: create: %w", err)
	}
	s.audit(ctx, "scenario.create", "scenario", id, nil, map[string]any{
		"title": title, "categoryId": catID, "categoryCode": info.Code, "difficulty": int(in.Difficulty), "mode": string(in.Mode),
	})

	row := store.ScenarioRow{
		ID: id, Title: title, CategoryID: catID, CategoryName: info.Name, CategoryCode: info.Code,
		Difficulty: int(in.Difficulty), Mode: string(in.Mode), Source: "manual", Status: core.ScenarioDraft,
		CallScript: cs, AuthorID: &author, CreatedAt: now, UpdatedAt: now, Version: 1,
		Etalon: &store.EtalonRow{
			ID: eid, ScenarioID: id, Version: 1, IsCurrent: true, Card: e.Card, CardDraft: e.Draft,
			CardDraftRaw: enc.draft, Scoring: e.Scoring, ExpectedActions: e.Actions, CreatedBy: &author, CreatedAt: now,
		},
	}
	httpx.WriteJSON(w, http.StatusCreated, store.ScenarioToPublic(&row))
	return nil
}

// ---------------------------------------------------------------- правка

// handlePatch — PATCH /scenarios/{id}. Частичная правка; сценарий в занятиях — 409 с
// details.lessonsCount (правка только новой версией). Эталонные поля создают новую
// версию etalons — но только если содержимое действительно изменилось: редактор фронта
// присылает etalonDraft и requiredFields при каждом сохранении, даже при смене названия.
func (s *Service) handlePatch(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "scenarioId")
	if err != nil {
		return errNotFound()
	}
	var in public.ScenarioPatch
	if err := httpx.ReadJSON(r, &in); err != nil {
		return err
	}
	if fields := validatePatch(&in); len(fields) > 0 {
		return httpx.Validation("Проверьте поля сценария", fields)
	}
	ctx := r.Context()
	p := core.PrincipalFrom(ctx)
	snap := s.snap(ctx)

	var (
		out         *store.ScenarioRow
		changed     []string
		before      map[string]any
		newEtalonID uuid.UUID
		newVersion  int
		oldVersion  int
		ttsJobs     int
	)
	err = pg.WithTx(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		cur, err := lockAndLoad(ctx, tx, id)
		if err != nil {
			return err
		}
		if cur.LessonsCount > 0 {
			return httpx.Conflict(fmt.Sprintf("Сценарий используется в занятиях (%d), изменить нельзя — создайте новую версию", cur.LessonsCount)).
				WithDetails(map[string]any{"lessonsCount": cur.LessonsCount})
		}
		if archived(cur) {
			return httpx.Conflict("Сценарий в архиве — изменить нельзя")
		}
		before = map[string]any{"title": cur.Title, "difficulty": cur.Difficulty, "mode": cur.Mode, "status": cur.Status}

		// Простые поля.
		var (
			title, mode *string
			difficulty  *int
			setComment  bool
			comment     string
			csJSON      []byte
		)
		if in.Title != nil {
			if t := strings.TrimSpace(*in.Title); t != cur.Title {
				title = &t
				changed = append(changed, "title")
			}
		}
		if in.Difficulty != nil && int(*in.Difficulty) != cur.Difficulty {
			d := int(*in.Difficulty)
			difficulty = &d
			changed = append(changed, "difficulty")
		}
		if in.Mode != nil && string(*in.Mode) != cur.Mode {
			m := string(*in.Mode)
			mode = &m
			changed = append(changed, "mode")
		}
		if in.TeacherComment != nil {
			comment = strings.TrimSpace(*in.TeacherComment)
			if comment != strings.TrimSpace(deref(cur.TeacherComment)) {
				setComment = true
				changed = append(changed, "teacherComment")
			}
		}

		// Легенда: tts_hash неизменённых реплик сохраняется; у подтверждённого сценария
		// хэши пересчитываются сразу и недостающая озвучка ставится в очередь.
		if in.CallScript != nil {
			cs := mergeCallScript(&cur.CallScript, in.CallScript)
			var plan ttsPlan
			if cur.Status == core.ScenarioValidated {
				plan, _ = assignHashes(&cs, snap)
			}
			newJSON, err := json.Marshal(&cs)
			if err != nil {
				return err
			}
			oldJSON, err := json.Marshal(&cur.CallScript)
			if err != nil {
				return err
			}
			if string(newJSON) != string(oldJSON) {
				csJSON = newJSON
				changed = append(changed, "callScript")
			}
			if cur.Status == core.ScenarioValidated {
				if ttsJobs, err = s.enqueueMissing(ctx, tx, id, plan, core.PriorityTTS); err != nil {
					return err
				}
			}
		}

		// Эталон: новая версия только при фактическом изменении.
		if hasEtalonPatch(&in) {
			old := fromRow(cur.Etalon)
			oldEnc, err := old.encode()
			if err != nil {
				return err
			}
			next := s.applyEtalonPatch(fromRow(cur.Etalon), &in)
			nextEnc, err := next.encode()
			if err != nil {
				return err
			}
			if cur.Etalon == nil || !nextEnc.equal(oldEnc) {
				if cur.Etalon != nil {
					oldVersion = cur.Etalon.Version
				}
				by := p.UserID
				if newEtalonID, newVersion, err = insertEtalonVersion(ctx, tx, id, nextEnc, &by); err != nil {
					return err
				}
				changed = append(changed, "etalon")
			}
		}

		if title != nil || difficulty != nil || mode != nil || setComment || csJSON != nil || newEtalonID != uuid.Nil {
			if _, err := tx.Exec(ctx, sqlUpdateScenario, id, title, difficulty, mode, setComment, comment, csJSON); err != nil {
				return fmt.Errorf("scenarios: update: %w", err)
			}
		}
		if out, err = store.GetScenario(ctx, tx, id); err != nil {
			return err
		}
		// Подтверждённый (но ещё не выданный) сценарий правится на месте — и должен остаться
		// пригодным для выдачи: иначе занятие получило бы сценарий без эталона или реплик.
		if cur.Status == core.ScenarioValidated {
			if gaps := approveGaps(out); len(gaps) > 0 {
				return httpx.BadRequest("Подтверждённый сценарий должен оставаться полным: не заполнено — " + strings.Join(gaps, ", ")).
					WithDetails(map[string]any{"fields": gaps})
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(changed) > 0 {
		s.audit(ctx, "scenario.update", "scenario", id, before, map[string]any{
			"changed": changed, "title": out.Title, "difficulty": out.Difficulty, "mode": out.Mode, "ttsJobs": ttsJobs,
		})
	}
	if newEtalonID != uuid.Nil {
		s.audit(ctx, "etalon.version", "etalon", newEtalonID,
			map[string]any{"version": oldVersion},
			map[string]any{"scenarioId": id, "version": newVersion})
	}
	httpx.WriteJSON(w, http.StatusOK, store.ScenarioToPublic(out))
	return nil
}

// validatePatch — ошибки по полям (пусто — всё в порядке).
func validatePatch(in *public.ScenarioPatch) map[string]string {
	fields := map[string]string{}
	if in.Title != nil {
		if msg := checkTitle(strings.TrimSpace(*in.Title)); msg != "" {
			fields["title"] = msg
		}
	}
	if in.Difficulty != nil && !validDifficulty(int(*in.Difficulty)) {
		fields["difficulty"] = "Сложность — 1, 2 или 3"
	}
	if in.Mode != nil && !validMode(string(*in.Mode)) {
		fields["mode"] = "Режим — cards, card_actions или both"
	}
	if in.TeacherComment != nil && runes(strings.TrimSpace(*in.TeacherComment)) > commentMax {
		fields["teacherComment"] = "Комментарий — не длиннее 2000 символов"
	}
	if cs := in.CallScript; cs != nil {
		if len(cs.Turns) > maxListItems {
			fields["callScript.turns"] = "Слишком много реплик"
		}
		if b := cs.Dialogue; b != nil {
			if b.MaxTurns != nil && (*b.MaxTurns < briefTurnsMin || *b.MaxTurns > briefTurnsMax) {
				fields["callScript.dialogue.maxTurns"] = "Число реплик оператора — от 2 до 40"
			}
			if len(b.Facts) > maxListItems {
				fields["callScript.dialogue.facts"] = "Слишком много фактов"
			}
		}
	}
	if in.RequiredFields != nil && len(*in.RequiredFields) > maxListItems {
		fields["requiredFields"] = "Слишком много обязательных полей"
	}
	if in.ExpectedDialogue != nil && len(in.ExpectedDialogue.Checklist) > maxListItems {
		fields["expectedDialogue.checklist"] = "Слишком много пунктов чек-листа"
	}
	return fields
}

// mergeCallScript — легенда из правки поверх текущей:
//   - caller.voice, не присланный вовсе, сохраняется (редактор фронта голос не показывает
//     и пересобирает caller без него — иначе каждое сохранение сбрасывало бы голос);
//   - пустые реплики отбрасываются, бриф чистится (факты без текста, повторы id);
//   - tts_hash сохраняется у реплик заявителя с тем же текстом при том же голосе,
//     у изменённых — сбрасывается (approve посчитает заново).
func mergeCallScript(old *model.CallScript, p *public.CallScript) model.CallScript {
	cs := convert.CallScriptFromPublic(p)
	if p.Caller.Voice == nil {
		cs.Caller.Voice = old.Caller.Voice
	}
	sanitizeBrief(cs.Dialogue)

	keep := map[string]string{}
	if cs.Caller.Voice == old.Caller.Voice {
		for _, t := range old.Turns {
			if t.Speaker == model.SpeakerCaller && t.TTSHash != "" {
				keep[t.Text] = t.TTSHash
			}
		}
	}
	turns := cs.Turns[:0]
	for _, t := range cs.Turns {
		if strings.TrimSpace(t.Text) == "" {
			continue
		}
		t.TTSHash = ""
		if t.Speaker == model.SpeakerCaller {
			t.TTSHash = keep[t.Text]
		}
		turns = append(turns, t)
	}
	cs.Turns = turns
	cs.Normalize()
	return cs
}

// ---------------------------------------------------------------- подтверждение / отклонение

// handleApprove — POST /scenarios/{id}/approve: проверка полноты (422 validation с
// details.fields — русские названия недостающего), status=validated, tts_hash репликам
// заявителя и TTS-задачи на неозвученные. Повтор для подтверждённого — 200 без смены
// статуса (заодно доозвучивает то, что не успело или провалилось).
func (s *Service) handleApprove(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "scenarioId")
	if err != nil {
		return errNotFound()
	}
	ctx := r.Context()
	p := core.PrincipalFrom(ctx)
	snap := s.snap(ctx)

	var (
		out     *store.ScenarioRow
		prev    string
		ttsJobs int
	)
	err = pg.WithTx(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		cur, err := lockAndLoad(ctx, tx, id)
		if err != nil {
			return err
		}
		if archived(cur) {
			return httpx.Conflict("Сценарий в архиве — подтвердить нельзя")
		}
		prev = cur.Status
		if cur.Status != core.ScenarioValidated {
			if gaps := approveGaps(cur); len(gaps) > 0 {
				return httpx.Unprocessable("Сценарий нельзя подтвердить: не заполнено — " + strings.Join(gaps, ", ")).
					WithDetails(map[string]any{"fields": gaps})
			}
		}
		cs := cur.CallScript
		cs.Turns = append([]model.Turn(nil), cur.CallScript.Turns...)
		plan, changed := assignHashes(&cs, snap)
		csJSON, err := json.Marshal(&cs)
		if err != nil {
			return err
		}
		switch {
		case cur.Status != core.ScenarioValidated:
			if _, err := tx.Exec(ctx, sqlApprove, id, p.UserID, csJSON); err != nil {
				return fmt.Errorf("scenarios: approve: %w", err)
			}
		case changed:
			if _, err := tx.Exec(ctx, sqlUpdateCallScript, id, csJSON); err != nil {
				return fmt.Errorf("scenarios: approve: update call script: %w", err)
			}
		}
		if ttsJobs, err = s.enqueueMissing(ctx, tx, id, plan, core.PriorityTTS); err != nil {
			return err
		}
		out, err = store.GetScenario(ctx, tx, id)
		return err
	})
	if err != nil {
		return err
	}
	if prev != core.ScenarioValidated {
		s.audit(ctx, "scenario.approve", "scenario", id,
			map[string]any{"status": prev},
			map[string]any{"status": core.ScenarioValidated, "ttsJobs": ttsJobs})
	}
	httpx.WriteJSON(w, http.StatusOK, store.ScenarioToPublic(out))
	return nil
}

type rejectInput struct {
	Reason string `json:"reason"`
}

// handleReject — POST /scenarios/{id}/reject {reason}: status=rejected, причина —
// в teacher_comment (как у мока) и generation_meta.rejected_reason. Сценарий, по
// которому идут занятия, отклонить нельзя (409): занятия выдают карточки из него.
func (s *Service) handleReject(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "scenarioId")
	if err != nil {
		return errNotFound()
	}
	var in rejectInput
	if err := httpx.ReadJSON(r, &in); err != nil {
		return err
	}
	reason := strings.TrimSpace(in.Reason)
	switch n := runes(reason); {
	case n < reasonMin:
		return httpx.Validation("Укажите причину отклонения", map[string]string{"reason": "Не короче 3 символов"})
	case n > reasonMax:
		return httpx.Validation("Причина слишком длинная", map[string]string{"reason": "Не длиннее 2000 символов"})
	}
	ctx := r.Context()

	var (
		out  *store.ScenarioRow
		prev string
	)
	err = pg.WithTx(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		cur, err := lockAndLoad(ctx, tx, id)
		if err != nil {
			return err
		}
		if archived(cur) {
			return httpx.Conflict("Сценарий в архиве — отклонить нельзя")
		}
		if cur.LessonsCount > 0 {
			return httpx.Conflict(fmt.Sprintf("Сценарий используется в занятиях (%d) — отклонить нельзя", cur.LessonsCount)).
				WithDetails(map[string]any{"lessonsCount": cur.LessonsCount})
		}
		prev = cur.Status
		if _, err := tx.Exec(ctx, sqlReject, id, reason); err != nil {
			return fmt.Errorf("scenarios: reject: %w", err)
		}
		out, err = store.GetScenario(ctx, tx, id)
		return err
	})
	if err != nil {
		return err
	}
	s.audit(ctx, "scenario.reject", "scenario", id,
		map[string]any{"status": prev},
		map[string]any{"status": core.ScenarioRejected, "reason": reason})
	httpx.WriteJSON(w, http.StatusOK, store.ScenarioToPublic(out))
	return nil
}

// ---------------------------------------------------------------- новая версия

// handleCreateVersion — POST /scenarios/{id}/versions: копия (draft, parent_id=id,
// version = max по всей цепочке + 1, автор — текущий пользователь, отметки валидации
// сброшены, легенда с хэшами озвучки как есть) и эталон v1 = текущий эталон родителя.
// Родитель и занятия на нём не меняются. Копирование — INSERT … SELECT на стороне БД.
func (s *Service) handleCreateVersion(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "scenarioId")
	if err != nil {
		return errNotFound()
	}
	ctx := r.Context()
	p := core.PrincipalFrom(ctx)
	newID := ids.New()

	var (
		out     *store.ScenarioRow
		version int
	)
	err = pg.WithTx(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		// Блокировка корня цепочки и максимум версии — один batch (один round-trip).
		b := &pgx.Batch{}
		b.Queue(sqlLockChainRoot, id)
		b.Queue(sqlChainMaxVersion, id)
		br := tx.SendBatch(ctx, b)
		var root uuid.UUID
		if err := br.QueryRow().Scan(&root); err != nil {
			_ = br.Close()
			if pg.IsNoRows(err) {
				return errNotFound()
			}
			return fmt.Errorf("scenarios: lock version chain: %w", err)
		}
		var maxVersion int
		if err := br.QueryRow().Scan(&maxVersion); err != nil {
			_ = br.Close()
			return fmt.Errorf("scenarios: chain max version: %w", err)
		}
		if err := br.Close(); err != nil {
			return err
		}

		src, err := store.GetScenario(ctx, tx, id)
		if err != nil {
			return err
		}
		if archived(src) {
			return httpx.Conflict("Сценарий в архиве — новую версию создать нельзя")
		}
		version = maxVersion + 1

		b = &pgx.Batch{}
		b.Queue(sqlCopyScenario, id, newID, p.UserID, version)
		b.Queue(sqlCopyEtalon, id, ids.New(), newID, p.UserID)
		if err := tx.SendBatch(ctx, b).Close(); err != nil {
			return fmt.Errorf("scenarios: copy version: %w", err)
		}
		out, err = store.GetScenario(ctx, tx, newID)
		return err
	})
	if err != nil {
		return err
	}
	s.audit(ctx, "scenario.version", "scenario", newID, nil, map[string]any{"parentId": id, "version": version})
	httpx.WriteJSON(w, http.StatusCreated, store.ScenarioToPublic(out))
	return nil
}
