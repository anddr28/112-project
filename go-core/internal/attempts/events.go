package attempts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/access"
	"lct/gocore/internal/core"
	"lct/gocore/internal/eventlog"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
)

// followUpTimeout — сколько даём «хвосту» POST /events (время первого ввода, микрофон) после
// того, как события уже записаны: клиент мог уйти, но эти факты терять нельзя.
const followUpTimeout = 5 * time.Second

// ---------------------------------------------------------------- GET /events

func (s *Service) listEvents(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "attemptId")
	if err != nil {
		return err
	}
	ctx := r.Context()
	m, err := s.metaFor(ctx, id)
	if err != nil {
		return err
	}
	if err := access.ViewAttempt(core.PrincipalFrom(ctx), m.accessRow()); err != nil {
		return err
	}
	list, err := eventlog.List(ctx, s.pool, id)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, list)
	return nil
}

// ---------------------------------------------------------------- POST /events

type eventsBody struct {
	Events []public.AttemptEventInput `json:"events"`
}

type eventsAccepted struct {
	Accepted int   `json:"accepted"`
	LastSeq  int64 `json:"lastSeq"`
}

// postEvents — батч клиентских событий. Путь: валидация → владелец (кэш метаданных) →
// group commit (eventlog.Writer: Append ждёт коммита своей пачки) → время первого ввода
// (один раз на попытку) → микрофон → мониторинг. Принимается в любом статусе попытки:
// outbox фронта досылает хвост и после сдачи, дубли по clientSeq отбрасываются молча.
func (s *Service) postEvents(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "attemptId")
	if err != nil {
		return err
	}
	var body eventsBody
	if err := httpx.ReadJSON(r, &body); err != nil {
		return err
	}
	now := s.now()
	in, err := eventlog.ParseInputs(body.Events, now)
	if err != nil {
		return err
	}
	ctx := r.Context()
	m, err := s.metaFor(ctx, id)
	if err != nil {
		return err
	}
	if err := access.OwnAttempt(core.PrincipalFrom(ctx), m.accessRow()); err != nil {
		return err
	}
	res, err := s.events.Append(ctx, id, in)
	if err != nil {
		return fmt.Errorf("attempts: append events: %w", err)
	}

	// События уже в БД: остальное — best effort, но и при уходе клиента доводим до конца.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), followUpTimeout)
	defer cancel()
	if !m.FirstInput {
		s.markFirstInput(fctx, id, in)
	}
	s.markMicReady(fctx, id, m, in)
	s.publishInserted(m.LessonID, id, res.Inserted, now)

	httpx.WriteJSON(w, http.StatusOK, eventsAccepted{Accepted: res.Accepted, LastSeq: res.LastSeq})
	return nil
}

// markFirstInput — attempts.first_input_at = самое раннее содержательное действие пачки
// (core.UserInputEvent; автосохранение не в счёт). Считается по ВСЕЙ пачке, а не только по
// новым событиям: если ответ на первую отправку потерялся, повтор той же пачки (где всё —
// дубли) всё равно проставит время.
func (s *Service) markFirstInput(ctx context.Context, id uuid.UUID, in []eventlog.Input) {
	var first time.Time
	for i := range in {
		if core.UserInputEvent(in[i].Type) && (first.IsZero() || in[i].At.Before(first)) {
			first = in[i].At
		}
	}
	if first.IsZero() {
		return
	}
	var set bool
	if err := s.pool.QueryRow(ctx, sqlFirstInput, id, first).Scan(&set); err != nil {
		s.log.Warn("attempts: first_input_at", "attempt", id, "err", err)
		return
	}
	if set {
		s.meta.setFirstInput(id)
	}
}

// markMicReady — событие mic_check с ok≠false → lesson_participants.mic_ready (один раз) и
// participantStatus в мониторинг (преподаватель видит, у кого гарнитура готова).
func (s *Service) markMicReady(ctx context.Context, id uuid.UUID, m attemptMeta, in []eventlog.Input) {
	ok := false
	for i := range in {
		if in[i].Type == core.EventMicCheck && micOK(in[i].Payload) {
			ok = true
			break
		}
	}
	if !ok {
		return
	}
	p, err := scanParticipant(s.pool.QueryRow(ctx, sqlParticipantMic, m.LessonID, m.UserID))
	if err != nil {
		s.log.Warn("attempts: mic_ready", "attempt", id, "err", err)
		return
	}
	s.publishParticipant(m.LessonID, p)
}

// micOK — payload.ok проверки микрофона (фронт шлёт {ok}); поля нет — считаем «готов».
// Событие редкое (раз на попытку), разбор крошечного объекта ничего не стоит.
func micOK(payload []byte) bool {
	var p struct {
		OK *bool `json:"ok"`
	}
	if json.Unmarshal(payload, &p) != nil || p.OK == nil {
		return true
	}
	return *p.OK
}

// ---------------------------------------------------------------- мониторинг

// liveTelemetry — частые события ввода: в мониторинг идёт не больше одного на попытку за
// liveEvery (последнее из пачки). Остальные типы (службы, сохранение, разговор, связь,
// микрофон) редкие и содержательные — публикуются все.
func liveTelemetry(t public.AttemptEventType) bool {
	return t == core.EventFieldChanged || t == core.EventChooseValue
}

// publishInserted — новые события пачки в канал мониторинга занятия (после коммита — Append
// возвращается только после COMMIT своей пачки).
func (s *Service) publishInserted(lessonID, attemptID uuid.UUID, inserted []public.AttemptEvent, now time.Time) {
	last := -1
	for i := range inserted {
		if liveTelemetry(inserted[i].Type) {
			last = i
			continue
		}
		s.publishEvent(lessonID, attemptID, inserted[i])
	}
	if last >= 0 && s.meta.allowLive(attemptID, now) {
		s.publishEvent(lessonID, attemptID, inserted[last])
	}
}

func (s *Service) publishEvent(lessonID, attemptID uuid.UUID, ev public.AttemptEvent) {
	aid := attemptID
	s.pub.Monitor(lessonID, public.MonitorMessage{
		Type:      public.MonitorMessageTypeAttemptEvent,
		AttemptId: &aid,
		Event:     &ev,
	})
}

func (s *Service) publishParticipant(lessonID uuid.UUID, p *public.LessonParticipant) {
	if p == nil {
		return
	}
	s.pub.Monitor(lessonID, public.MonitorMessage{
		Type:        public.MonitorMessageTypeParticipantStatus,
		AttemptId:   p.AttemptId,
		Participant: p,
	})
}

// publishTurn — вступительная реплика в живой транскрипт преподавателя.
func (s *Service) publishTurn(lessonID, attemptID uuid.UUID, t *public.DialogueTurnView) {
	aid := attemptID
	turn := *t
	s.pub.Monitor(lessonID, public.MonitorMessage{
		Type:      public.MonitorMessageTypeDialogueTurn,
		AttemptId: &aid,
		Turn:      &turn,
	})
}
