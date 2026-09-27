package dialogue

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/access"
	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/store"
)

// transcript — сводка транскрипта для нумерации и контекста ai-service.
type transcript struct {
	rows          []store.DialogueTurnRow
	maxOperator   int // последний номер обмена с репликой оператора (0 — не было)
	operatorTurns int
}

func summarize(rows []store.DialogueTurnRow) transcript {
	t := transcript{rows: rows}
	for i := range rows {
		if rows[i].Speaker == core.SpeakerOperator {
			t.operatorTurns++
			if rows[i].TurnNo > t.maxOperator {
				t.maxOperator = rows[i].TurnNo
			}
		}
	}
	return t
}

// next — какой turnNo слать следующим: последний обработанный + 1 (после вступления — 1).
func (t *transcript) next() int { return t.maxOperator + 1 }

// revealed — факты, уже раскрытые заявителем (агрегат по его репликам, порядок появления).
// Студенту не отдаётся — только в ai-service.
func (t *transcript) revealed() []string {
	var out []string
	var seen map[string]struct{}
	for i := range t.rows {
		r := &t.rows[i]
		if r.Speaker != core.SpeakerCaller {
			continue
		}
		for _, id := range r.RevealedFactIDs {
			if id == "" {
				continue
			}
			if seen == nil {
				seen = make(map[string]struct{}, 8)
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// exchange — строки оператора и заявителя обмена turnNo (nil — нет).
func (t *transcript) exchange(turnNo int) (op, caller *store.DialogueTurnRow) {
	for i := range t.rows {
		r := &t.rows[i]
		if r.TurnNo != turnNo {
			continue
		}
		switch r.Speaker {
		case core.SpeakerOperator:
			op = r
		case core.SpeakerCaller:
			caller = r
		}
	}
	return op, caller
}

// answeredAt — когда появилась реплика заявителя, на которую отвечает реплика оператора
// turnNo (ответ заявителя в обмене turnNo−1; для первой — вступление), не раньше принятия
// вызова. Нижняя граница времени реплики оператора (operatorTime).
func (t *transcript) answeredAt(turnNo int, acceptedAt *time.Time) time.Time {
	var at time.Time
	if acceptedAt != nil {
		at = *acceptedAt
	}
	if _, prev := t.exchange(turnNo - 1); prev != nil && prev.At.After(at) {
		at = prev.At
	}
	return at
}

// buildState — DialogueState для фронта (turns всегда [], не null).
func buildState(a *attemptCtx, tr *transcript) public.DialogueState {
	vp := convert.VoiceToPublic(a.Settings.Voice)
	next := tr.next()
	left := max(0, a.maxTurns()-tr.operatorTurns)
	st := public.DialogueState{
		AttemptId:   a.ID,
		Turns:       store.DialogueTurnsToView(tr.rows),
		CallEnded:   a.CallEndedAt != nil,
		CallEndedAt: convert.UTC(a.CallEndedAt),
		Input:       public.DialogueStateInput(vp.Input),
		NextTurnNo:  &next,
		TurnsLeft:   &left,
	}
	if a.CallEndedAt != nil && a.CallEndReason != nil && *a.CallEndReason != "" {
		r := public.DialogueStateEndReason(*a.CallEndReason)
		st.EndReason = &r
	}
	return st
}

// storedResponse — сохранённый ответ на уже обработанный ход (для 409 при повторе
// после обрыва): те же реплики, fallback/latency — из meta реплики заявителя.
// callEnded — текущее состояние разговора, если это последний ход (иначе false).
func storedResponse(a *attemptCtx, tr *transcript, turnNo int) *public.DialogueTurnResponse {
	opRow, callerRow := tr.exchange(turnNo)
	if callerRow == nil {
		return nil
	}
	resp := &public.DialogueTurnResponse{
		TurnNo:     turnNo,
		Caller:     store.DialogueTurnToView(callerRow),
		NextTurnNo: turnNo + 1,
	}
	if opRow != nil {
		v := store.DialogueTurnToView(opRow)
		resp.Operator = &v
	}
	var meta model.TurnMeta
	if len(callerRow.Meta) > 0 && json.Unmarshal(callerRow.Meta, &meta) == nil {
		resp.Fallback = meta.Fallback
		if meta.LatencyMs > 0 {
			l := meta.LatencyMs
			resp.LatencyMs = &l
		}
	}
	if turnNo == tr.maxOperator && a.CallEndedAt != nil {
		resp.CallEnded = true
		resp.EndReason = a.CallEndReason
	}
	return resp
}

// duplicateConflict — 409 на повтор обработанного хода: details.nextTurnNo и сохранённый ответ.
func duplicateConflict(a *attemptCtx, tr *transcript, turnNo int) error {
	details := map[string]any{"nextTurnNo": tr.next()}
	if resp := storedResponse(a, tr, turnNo); resp != nil {
		details["response"] = resp
	}
	return httpx.Conflict("Эта реплика уже обработана").WithDetails(details)
}

// ---------------------------------------------------------------- GET /dialogue

func (s *Service) getDialogue(w http.ResponseWriter, r *http.Request) error {
	attemptID, err := httpx.PathUUID(r, "attemptId")
	if err != nil {
		return err
	}
	a, rows, err := load(r.Context(), s.pool, attemptID, false)
	if err != nil {
		return err
	}
	if err := access.ViewAttempt(core.PrincipalFrom(r.Context()), a.accessRow()); err != nil {
		return err
	}
	tr := summarize(rows)
	httpx.WriteJSON(w, http.StatusOK, buildState(a, &tr))
	return nil
}

// ---------------------------------------------------------------- POST /dialogue/end

// endDialogue — «положить трубку». Идемпотентно: разговор уже завершён (кем угодно) — 200
// с текущим состоянием. Карточку после этого можно дозаполнить и сдать.
func (s *Service) endDialogue(w http.ResponseWriter, r *http.Request) error {
	attemptID, err := httpx.PathUUID(r, "attemptId")
	if err != nil {
		return err
	}
	p := core.PrincipalFrom(r.Context())
	var state public.DialogueState
	err = pg.WithTx(r.Context(), s.pool, func(ctx context.Context, tx pgx.Tx) error {
		a := &attemptCtx{ID: attemptID}
		if err := scanStateContext(tx.QueryRow(ctx, sqlStateContextLock, attemptID), a); err != nil {
			if pg.IsNoRows(err) {
				return httpx.NotFound("Попытка не найдена")
			}
			return fmt.Errorf("dialogue: lock attempt: %w", err)
		}
		if err := access.OwnAttempt(p, a.accessRow()); err != nil {
			return err
		}

		b := &pgx.Batch{}
		var evs []pendingEvent
		if a.CallEndedAt == nil {
			if err := endable(a); err != nil {
				return err
			}
			now := eventTime(time.Now())
			reason := core.EndOperatorHungUp
			evs = []pendingEvent{{typ: core.EventDialogueEnded, payload: map[string]any{"reason": reason}, at: now}}
			b.Queue(sqlEndCall, attemptID, now, reason)
			if err := queueEvents(b, attemptID, evs); err != nil {
				return err
			}
			a.CallEndedAt, a.CallEndReason = &now, &reason
		}
		b.Queue(sqlTurns, attemptID)

		br := tx.SendBatch(ctx, b)
		var published []public.AttemptEvent
		if len(evs) > 0 {
			if _, err := br.Exec(); err != nil {
				br.Close()
				return fmt.Errorf("dialogue: end call: %w", err)
			}
			if published, err = insertEvents(br, evs); err != nil {
				br.Close()
				return err
			}
		}
		rows, err := br.Query()
		if err != nil {
			br.Close()
			return fmt.Errorf("dialogue: list turns: %w", err)
		}
		turns, err := scanTurns(rows, attemptID)
		if cerr := br.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("dialogue: end batch: %w", cerr)
		}
		if err != nil {
			return err
		}
		tr := summarize(turns)
		state = buildState(a, &tr)
		if len(published) > 0 {
			lessonID := a.LessonID
			pg.OnCommit(ctx, func() { s.publishEvents(lessonID, attemptID, published) })
		}
		return nil
	})
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, state)
	return nil
}

// endable — можно ли положить трубку в открытом разговоре.
func endable(a *attemptCtx) error {
	if !a.Settings.Voice.Enabled {
		return httpx.Conflict("В этом занятии нет разговора с заявителем")
	}
	switch a.Status {
	case core.AttemptInProgress:
		return nil
	case core.AttemptIssued:
		return httpx.Conflict("Вызов ещё не принят")
	}
	return httpx.Conflict("Попытка уже завершена — разговор недоступен")
}
