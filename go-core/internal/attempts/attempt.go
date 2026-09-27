package attempts

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/access"
	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/dds"
	"lct/gocore/internal/eventlog"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/reaction"
	"lct/gocore/internal/store"
)

// Лимиты сдачи карточки.
const (
	maxDescriptionRunes = 1999  // контракт: IncidentCardDraft.description maxLength
	maxActionTextRunes  = 10000 // контракт лимита не задаёт; защита БД и LLM-промпта
)

// metaFromRow — метаданные для кэша из полной строки попытки.
func metaFromRow(r *store.AttemptRow) attemptMeta {
	return attemptMeta{
		UserID:     r.UserID,
		LessonID:   r.LessonID,
		OwnerID:    r.LessonOwnerID(),
		Status:     r.Status,
		FirstInput: r.FirstInputAt != nil,
	}
}

// ---------------------------------------------------------------- GET /attempts/{id}

func (s *Service) getAttempt(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "attemptId")
	if err != nil {
		return err
	}
	ctx := r.Context()
	row, err := store.GetAttempt(ctx, s.pool, id)
	if err != nil {
		if pg.IsNoRows(err) {
			return errNotFound()
		}
		return fmt.Errorf("attempts: get: %w", err)
	}
	if err := access.ViewAttempt(core.PrincipalFrom(ctx), row); err != nil {
		return err
	}
	now := s.now()
	// Попутно освежаем кэш: следующий автосейв/батч событий этой попытки не пойдёт в БД за метаданными.
	s.meta.put(id, metaFromRow(row), now)
	httpx.WriteJSON(w, http.StatusOK, store.AttemptToPublic(row, now))
	return nil
}

// ---------------------------------------------------------------- POST accept-call

type acceptResponse struct {
	Attempt public.Attempt           `json:"attempt"`
	Opening *public.DialogueTurnView `json:"opening,omitempty"`
}

// acceptCall — «Принять вызов» (Инструкция п.3, п.4.1). Идемпотентно: повтор возвращает ту же
// попытку и ту же вступительную реплику. Всё — одной транзакцией под замком попытки:
// статус, АОН в черновике, события, участник, реплика 0 (DialogueHooks), чтение ответа.
func (s *Service) acceptCall(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "attemptId")
	if err != nil {
		return err
	}
	ctx := r.Context()
	p := core.PrincipalFrom(ctx)

	var out acceptResponse
	var row *store.AttemptRow
	err = pg.WithTx(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		var (
			m     attemptMeta
			phone string
			role  ddsRole
		)
		lessonStatus, err := lockLessonThenAttempt(ctx, tx, id, sqlLockAccept, func(locked pgx.Row) error {
			return locked.Scan(&m.UserID, &m.LessonID, &m.OwnerID, &m.Status, &m.FirstInput, &phone, &role.DDS, &role.Service)
		})
		if err != nil {
			return err
		}
		if err := access.OwnAttempt(p, m.accessRow()); err != nil {
			return err
		}

		now := s.now()
		accepted := false
		var (
			events      []public.AttemptEvent
			participant *public.LessonParticipant
		)
		switch m.Status {
		case core.AttemptInProgress:
			// Повтор после обрыва: состояние не меняем, отдаём то же.
		case core.AttemptIssued:
			if lessonStatus != core.LessonRunning {
				return httpx.Conflict("Занятие не идёт — принять вызов нельзя")
			}
			if participant, err = s.markAccepted(ctx, tx, id, &m, role, phone, now); err != nil {
				return err
			}
			for _, typ := range [...]string{core.EventCallAccepted, core.EventOpenCard} {
				ev, err := eventlog.InsertServer(ctx, tx, id, typ, nil, now)
				if err != nil {
					return err
				}
				events = append(events, ev)
			}
			accepted = true
		default:
			return errClosed("принять вызов нельзя")
		}

		// Вступительная реплика (голосовой режим; nil — голос выключен). Вызывается и на повторе:
		// хук идемпотентен и вернёт ту же реплику 0. В ракурсе dds звонка нет вовсе — даже если
		// голос у занятия включили в обход создания.
		var opening *public.DialogueTurnView
		if !role.DDS {
			if opening, err = s.dialogue.OnCallAccepted(ctx, tx, id); err != nil {
				return fmt.Errorf("attempts: opening turn: %w", err)
			}
		}
		out.Opening = opening

		if row, err = store.GetAttempt(ctx, tx, id); err != nil {
			return fmt.Errorf("attempts: reload after accept: %w", err)
		}
		if !accepted {
			return nil
		}
		lessonID := m.LessonID
		after := map[string]any{"status": core.AttemptInProgress, "callAcceptedAt": now}
		if role.DDS {
			after["perspective"], after["service"] = "dds", role.Service
		}
		pg.OnCommit(ctx, func() {
			s.meta.setStatus(id, core.AttemptInProgress, s.now())
			for i := range events {
				s.publishEvent(lessonID, id, events[i])
			}
			s.publishParticipant(lessonID, participant)
			if opening != nil {
				s.publishTurn(lessonID, id, opening)
			}
			s.aud.Log(ctx, core.AuditEntry{Action: "attempt.accept", EntityType: "attempt", EntityID: id, LessonID: lessonID,
				Before: map[string]any{"status": core.AttemptIssued},
				After:  after})
		})
		return nil
	})
	if err != nil {
		return err
	}
	out.Attempt = store.AttemptToPublic(row, s.now())
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

// markAccepted — issued → in_progress одним round-trip (pgx.Batch): статус и старт таймера,
// АОН в черновике, участник → active (+joined_at). Возвращает участника для мониторинга.
// Ракурс dds: черновик целиком становится карточкой, поступившей от оператора 112
// (incomingCard) — диспетчер берёт её в работу, таймер решения стартует сейчас.
func (s *Service) markAccepted(ctx context.Context, tx pgx.Tx, id uuid.UUID, m *attemptMeta, role ddsRole, phone string, now time.Time) (p *public.LessonParticipant, err error) {
	b := &pgx.Batch{}
	b.Queue(sqlAccept, id, now)
	if role.DDS {
		card, err := s.incomingCard(ctx, tx, id, role.Service, phone, now)
		if err != nil {
			return nil, err
		}
		b.Queue(sqlUpsertDraft, id, card, now)
	} else {
		draft, err := draftWithAON(phone)
		if err != nil {
			return nil, fmt.Errorf("attempts: encode draft: %w", err)
		}
		b.Queue(sqlAcceptDraft, id, draft, phone, now)
	}
	b.Queue(sqlParticipantActive, m.LessonID, m.UserID, now)
	br := tx.SendBatch(ctx, b)
	defer func() {
		if cerr := br.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("attempts: accept batch: %w", cerr)
		}
	}()
	tag, err := br.Exec()
	if err != nil {
		return nil, fmt.Errorf("attempts: accept: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return nil, fmt.Errorf("attempts: accept: попытка %s не в статусе issued под замком", id)
	}
	if _, err = br.Exec(); err != nil {
		return nil, fmt.Errorf("attempts: accept draft: %w", err)
	}
	return scanParticipant(br.QueryRow())
}

// Эталон выданной версии (карточка 112) и службы его списка оповещения — для ракурса dds.
var sqlIncomingCard = `
SELECT e.card_draft, COALESCE(` + store.ServiceCodesSQL("e") + `, '')
  FROM attempts a
  JOIN etalons e ON e.id = a.etalon_id
 WHERE a.id = $1`

// incomingCard — карточка, поступившая диспетчеру ДДС (dds.IncomingCard) по эталону версии,
// зафиксированной при выдаче, в jsonb черновика.
func (s *Service) incomingCard(ctx context.Context, tx pgx.Tx, id uuid.UUID, acting, phone string, now time.Time) (json.RawMessage, error) {
	var (
		raw   []byte
		codes string
	)
	if err := tx.QueryRow(ctx, sqlIncomingCard, id).Scan(&raw, &codes); err != nil {
		return nil, fmt.Errorf("attempts: incoming card: %w", err)
	}
	var etalon *public.IncidentCardDraft
	if isDraftObject(raw) {
		etalon = new(public.IncidentCardDraft)
		if err := json.Unmarshal(raw, etalon); err != nil {
			return nil, fmt.Errorf("attempts: incoming card: etalons.card_draft: %w", err)
		}
		convert.NormalizeDraft(etalon)
	}
	var lookup dds.ServiceLookup
	if s.cat != nil {
		lookup = s.cat.ServiceByCode
	}
	card := dds.IncomingCard(etalon, dds.SplitCodes(codes), acting, phone, lookup, now)
	b, err := json.Marshal(card)
	if err != nil {
		return nil, fmt.Errorf("attempts: encode incoming card: %w", err)
	}
	return stripNULEscapes(b), nil
}

// ---------------------------------------------------------------- POST replay

type replayResponse struct {
	ReplayCount int `json:"replayCount"`
}

// replay — «переспросить заявителя»: replay_count++ и событие replay. Владелец, статус
// in_progress и разрешение занятия проверяются в самом UPDATE; отказ разбирается отдельно.
func (s *Service) replay(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "attemptId")
	if err != nil {
		return err
	}
	ctx := r.Context()
	p := core.PrincipalFrom(ctx)

	var count int
	err = pg.WithTx(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		var lessonID uuid.UUID
		if err := tx.QueryRow(ctx, sqlReplay, id, p.UserID).Scan(&count, &lessonID); err != nil {
			if pg.IsNoRows(err) {
				return s.replayRejected(ctx, tx, p, id)
			}
			return fmt.Errorf("attempts: replay: %w", err)
		}
		now := s.now()
		ev, err := eventlog.InsertServer(ctx, tx, id, core.EventReplay, nil, now)
		if err != nil {
			return err
		}
		pg.OnCommit(ctx, func() { s.publishEvent(lessonID, id, ev) })
		return nil
	})
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, replayResponse{ReplayCount: count})
	return nil
}

func (s *Service) replayRejected(ctx context.Context, q pg.Querier, p *core.Principal, id uuid.UUID) error {
	var (
		m        attemptMeta
		allowed  bool
		perspDDS bool
	)
	if err := q.QueryRow(ctx, sqlReplayDiagnose, id).Scan(&m.UserID, &m.OwnerID, &m.Status, &allowed, &perspDDS); err != nil {
		if pg.IsNoRows(err) {
			return errNotFound()
		}
		return fmt.Errorf("attempts: replay diagnose: %w", err)
	}
	if err := access.OwnAttempt(p, m.accessRow()); err != nil {
		return err
	}
	switch {
	case m.Status == core.AttemptIssued:
		return errNotAccepted()
	case m.Status != core.AttemptInProgress:
		return errClosed("переспросить заявителя нельзя")
	case perspDDS:
		return httpx.Conflict("Карточка поступила от оператора 112 — звонка нет, переспрашивать некого")
	case !allowed:
		return httpx.Conflict("Переспрашивать заявителя в этом занятии нельзя")
	}
	return httpx.Conflict("Не удалось переспросить заявителя — повторите")
}

// ---------------------------------------------------------------- POST submit

type submitBody struct {
	Card       *public.IncidentCardDraft `json:"card"`
	ActionText *string                   `json:"actionText"`
}

// submit — «Сохранить карточку». Идемпотентно (повтор — 200 с той же попыткой).
// Одной транзакцией под замком попытки: карточка и тайминг, черновик = карточка, события
// save+submitted, закрытие разговора (DialogueHooks), старт оценки (Evaluator: слои fields+
// timing сразу, AI-слои — задачи), выдача следующей карточки (AttemptIssuer) или завершение
// участника. Ответ читается в той же транзакции — ровно то, что закоммичено.
func (s *Service) submit(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "attemptId")
	if err != nil {
		return err
	}
	ctx := r.Context()
	p := core.PrincipalFrom(ctx)

	var body submitBody
	if err := httpx.ReadJSON(r, &body); err != nil {
		return err
	}
	if body.Card == nil {
		return httpx.Validation("Карточка не передана", map[string]string{"card": "обязательно"})
	}
	card := body.Card
	if fields := validateCard(card, body.ActionText); fields != nil {
		return httpx.Validation("Карточка заполнена некорректно", fields)
	}
	// Нормализация до записи: обязательные массивы не null, allowedNext/editable служб
	// пересчитаны сервером (клиентскому allowedNext не доверяем).
	convert.NormalizeDraft(card)
	for i := range card.Services {
		reaction.Normalize(&card.Services[i])
	}
	bodyServices, err := json.Marshal(card.Services)
	if err != nil {
		return fmt.Errorf("attempts: encode services: %w", err)
	}
	bodyServices = stripNULEscapes(bodyServices)

	var row *store.AttemptRow
	err = pg.WithTx(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		var (
			userID, lessonID uuid.UUID
			status, mode     string
			callAcceptedAt   *time.Time
			perspDDS         bool
		)
		if _, err := lockLessonThenAttempt(ctx, tx, id, sqlLockSubmit, func(locked pgx.Row) error {
			return locked.Scan(&userID, &lessonID, &status, &mode, &callAcceptedAt, &perspDDS)
		}); err != nil {
			return err
		}
		if err := access.OwnAttempt(p, &store.AttemptRow{UserID: userID}); err != nil {
			return err
		}
		switch status {
		case core.AttemptInProgress:
		case core.AttemptIssued:
			return errNotAccepted()
		case core.AttemptSubmitted, core.AttemptEvaluating, core.AttemptEvaluated:
			// Повтор сдачи после обрыва: карточка уже принята — отдаём попытку как есть.
			var err error
			if row, err = store.GetAttempt(ctx, tx, id); err != nil {
				return fmt.Errorf("attempts: reload: %w", err)
			}
			return nil
		default: // expired | aborted — например, преподаватель завершил занятие, пока карточка была открыта
			return httpx.Conflict("Попытка уже закрыта — карточку сохранить нельзя")
		}

		now := s.now()
		spentMs := 0
		if callAcceptedAt != nil {
			spentMs = spentMillis(now.Sub(*callAcceptedAt))
		}
		if perspDDS {
			// Ракурс dds: сдаётся карточка 112 с сервера (поля и статусы служб диспетчер не
			// правит), от клиента — только текст действия.
			var err error
			if card, err = ddsSubmittedCard(ctx, tx, id, card.ActionsTaken); err != nil {
				return err
			}
		} else {
			// Службы — слиянием с черновиком (servicesMergeSQL): карточка в теле — снимок клиента,
			// сделанный, возможно, до ответа на последнее действие со службой.
			var merged []byte
			if err := tx.QueryRow(ctx, sqlSubmitServices, id, json.RawMessage(bodyServices)).Scan(&merged); err != nil {
				if bad := badTextError(err); bad != nil {
					return bad
				}
				return fmt.Errorf("attempts: merge services: %w", err)
			}
			card.Services = decodeServices(merged)
		}
		for i := range card.Services {
			reaction.Normalize(&card.Services[i])
		}
		// Кодируем один раз — те же байты уходят в attempts.card и в черновик.
		cardJSON, err := json.Marshal(card)
		if err != nil {
			return fmt.Errorf("attempts: encode card: %w", err)
		}
		cardJSON = stripNULEscapes(cardJSON)
		actionText := resolveActionText(mode, body.ActionText, card)
		if err := s.writeSubmission(ctx, tx, id, cardJSON, actionText, now, spentMs); err != nil {
			if bad := badTextError(err); bad != nil {
				return bad
			}
			return err
		}
		var events []public.AttemptEvent
		for _, typ := range [...]string{core.EventSave, core.EventSubmitted} {
			ev, err := eventlog.InsertServer(ctx, tx, id, typ, nil, now)
			if err != nil {
				return err
			}
			events = append(events, ev)
		}

		// Разговор закрывается до старта оценки: задаче evaluate_dialogue нужен call_ended_at.
		if err := s.dialogue.CloseOnSubmit(ctx, tx, id, now); err != nil {
			return fmt.Errorf("attempts: close dialogue: %w", err)
		}
		if err := s.eval.StartEvaluation(ctx, tx, id); err != nil {
			return fmt.Errorf("attempts: start evaluation: %w", err)
		}
		nextID, issued, err := s.issuer.IssueNext(ctx, tx, lessonID, userID)
		if err != nil {
			return fmt.Errorf("attempts: issue next: %w", err)
		}
		// Карточек больше нет — участник закончил занятие (следующую выдачу публикует lessons).
		var participant *public.LessonParticipant
		if !issued {
			if participant, err = scanParticipant(tx.QueryRow(ctx, sqlParticipantFinished, lessonID, userID, now)); err != nil {
				return err
			}
		}

		if row, err = store.GetAttempt(ctx, tx, id); err != nil {
			return fmt.Errorf("attempts: reload after submit: %w", err)
		}
		finalStatus := row.Status
		var next any
		if issued {
			next = nextID
		}
		pg.OnCommit(ctx, func() {
			s.meta.setStatus(id, finalStatus, s.now())
			for i := range events {
				s.publishEvent(lessonID, id, events[i])
			}
			s.publishParticipant(lessonID, participant)
			s.aud.Log(ctx, core.AuditEntry{Action: "attempt.submit", EntityType: "attempt", EntityID: id, LessonID: lessonID,
				Before: map[string]any{"status": core.AttemptInProgress},
				After: map[string]any{"status": finalStatus, "submittedAt": now, "timeSpentMs": spentMs,
					"mode": mode, "nextAttemptId": next}})
		})
		return nil
	})
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, store.AttemptToPublic(row, s.now()))
	return nil
}

// ddsSubmittedCard — сдаваемая карточка ракурса dds: черновик сервера (карточка 112 и
// статусы служб) под замком, actionsTaken — из тела, если прислан (иначе — автосохранённый).
func ddsSubmittedCard(ctx context.Context, tx pgx.Tx, id uuid.UUID, actionsTaken string) (*public.IncidentCardDraft, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, sqlLockDraftData, id).Scan(&raw); err != nil && !pg.IsNoRows(err) {
		return nil, fmt.Errorf("attempts: dds draft: %w", err)
	}
	card := convert.EmptyDraft()
	if isDraftObject(raw) {
		if err := json.Unmarshal(raw, &card); err != nil {
			// Черновик пишет только сервер (accept-call, службы, actionsTaken) — битым он
			// быть не должен; если всё же так, сдаём пустую карточку, а не 500.
			card = convert.EmptyDraft()
		}
	}
	convert.NormalizeDraft(&card)
	if t := stripNUL(actionsTaken); strings.TrimSpace(t) != "" {
		card.ActionsTaken = t
	}
	return &card, nil
}

// writeSubmission — карточка/тайминг/статус попытки и черновик = карточка, одним round-trip.
func (s *Service) writeSubmission(ctx context.Context, tx pgx.Tx, id uuid.UUID, cardJSON []byte, actionText *string, now time.Time, spentMs int) (err error) {
	b := &pgx.Batch{}
	b.Queue(sqlSubmit, id, json.RawMessage(cardJSON), actionText, now, spentMs)
	b.Queue(sqlUpsertDraft, id, json.RawMessage(cardJSON), now)
	br := tx.SendBatch(ctx, b)
	defer func() {
		if cerr := br.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("attempts: submit batch: %w", cerr)
		}
	}()
	tag, err := br.Exec()
	if err != nil {
		return fmt.Errorf("attempts: submit: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("attempts: submit: попытка %s не в статусе in_progress под замком", id)
	}
	if _, err = br.Exec(); err != nil {
		return fmt.Errorf("attempts: submit draft: %w", err)
	}
	return nil
}

// spentMillis — time_spent_ms: не меньше 0 и не больше предела колонки int4 (попытку
// практики могут открыть и сдать через месяц — «integer out of range» уронил бы сдачу).
func spentMillis(d time.Duration) int {
	return int(min(max(0, d.Milliseconds()), math.MaxInt32))
}

// resolveActionText — текст действий (режим card_actions). Фронт отдельного actionText не
// шлёт (submit(id, card)) — действия оператор пишет в card.actionsTaken, поэтому он и есть
// запасной источник. Пустой текст не блокирует сдачу: оценка поставит semantic=0 с пояснением.
// NUL вырезается: колонка text его не принимает (SQLSTATE 22021).
func resolveActionText(mode string, actionText *string, card *public.IncidentCardDraft) *string {
	if actionText != nil {
		if t := strings.TrimSpace(stripNUL(*actionText)); t != "" {
			return &t
		}
	}
	if mode == core.ModeCardActions {
		if t := strings.TrimSpace(stripNUL(card.ActionsTaken)); t != "" {
			return &t
		}
	}
	return nil
}

// validateCard — проверки контракта, которые не делает декодер (длины, enum'ы).
// nil — всё в порядке. Пустые строки в необязательных enum'ах трактуются как «не выбрано».
func validateCard(c *public.IncidentCardDraft, actionText *string) map[string]string {
	var fields map[string]string
	add := func(k, v string) {
		if fields == nil {
			fields = make(map[string]string, 2)
		}
		fields[k] = v
	}
	if utf8.RuneCountInString(c.Description) > maxDescriptionRunes {
		add("card.description", "не длиннее 1999 символов")
	}
	if st := c.Applicant.Status; st != nil {
		if *st == "" {
			c.Applicant.Status = nil
		} else if !st.Valid() {
			add("card.applicant.status", "недопустимый статус заявителя")
		}
	}
	if n := c.Flags.VictimsCount; n != nil && *n < 0 {
		add("card.flags.victimsCount", "не может быть отрицательным")
	}
	if src := c.Address.Source; src != nil {
		if *src == "" {
			c.Address.Source = nil
		} else if !src.Valid() {
			add("card.address.source", "недопустимый источник адреса")
		}
	}
	for i := range c.Services {
		sv := &c.Services[i]
		if !sv.CurrentStatus.Valid() {
			add(fmt.Sprintf("card.services[%d].currentStatus", i), "неизвестный статус реагирования")
		}
		if !sv.Source.Valid() {
			add(fmt.Sprintf("card.services[%d].source", i), "неизвестный источник назначения")
		}
		if strings.TrimSpace(sv.Code) == "" {
			add(fmt.Sprintf("card.services[%d].code", i), "обязательно")
		}
	}
	if actionText != nil && utf8.RuneCountInString(*actionText) > maxActionTextRunes {
		add("actionText", "не длиннее 10000 символов")
	}
	return fields
}
