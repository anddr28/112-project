package attempts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/access"
	"lct/gocore/internal/core"
	"lct/gocore/internal/eventlog"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/reaction"
)

// Службы карточки живут в attempt_drafts.data.services (DESIGN §5): read-modify-write под
// замком (попытка FOR NO KEY UPDATE → черновик FOR UPDATE), меняется только ключ services
// (jsonb_set) — остальная карточка и её неизвестные поля не трогаются и не разбираются.
// Переходы статусов считает только сервер (internal/reaction), клиентскому allowedNext
// не доверяем. Каждое действие — событие попытки (хронология и «время первого ввода»).

const manualReason = "добавлена оператором вручную"

// serviceList — службы черновика. Элементы, которые не разбираются как AssignedService
// (мусор от клиента через PUT черновика), сохраняются как есть и в поиске не участвуют.
type serviceList struct {
	raw   []json.RawMessage
	items []*public.AssignedService // nil — элемент не разобрался
}

func parseServices(b []byte) *serviceList {
	l := &serviceList{}
	var raws []json.RawMessage
	if len(b) == 0 || json.Unmarshal(b, &raws) != nil {
		return l // нет ключа / не массив — список пуст (перезапишется корректным)
	}
	l.raw = raws
	l.items = make([]*public.AssignedService, len(raws))
	for i, r := range raws {
		as := new(public.AssignedService)
		if json.Unmarshal(r, as) == nil && as.Code != "" {
			l.items[i] = as
		}
	}
	return l
}

// find — индекс службы по serviceId из пути (фронт шлёт AssignedService.serviceId),
// запасной вариант — по коду. -1 — не назначена.
func (l *serviceList) find(key string) int {
	key = strings.TrimSpace(key)
	if key == "" {
		return -1
	}
	for i, as := range l.items {
		if as != nil && as.ServiceId == key {
			return i
		}
	}
	return l.findCode(key)
}

func (l *serviceList) findCode(code string) int {
	for i, as := range l.items {
		if as != nil && strings.EqualFold(as.Code, code) {
			return i
		}
	}
	return -1
}

func (l *serviceList) add(as public.AssignedService) {
	l.raw = append(l.raw, nil)
	l.items = append(l.items, &as)
}

func (l *serviceList) remove(i int) {
	l.raw = append(l.raw[:i], l.raw[i+1:]...)
	l.items = append(l.items[:i], l.items[i+1:]...)
}

// encode — массив для jsonb_set. Разобранные службы кодируются заново после Normalize
// (allowedNext/editable всегда пересчитаны сервером), неразобранные — байт в байт.
func (l *serviceList) encode() (json.RawMessage, error) {
	out := make([]json.RawMessage, len(l.items))
	for i, as := range l.items {
		if as == nil {
			out[i] = l.raw[i]
			continue
		}
		reaction.Normalize(as)
		b, err := json.Marshal(as)
		if err != nil {
			return nil, err
		}
		out[i] = b
	}
	return json.Marshal(out)
}

// serviceLabel — подпись службы в событии (как в моке: краткое имя).
func serviceLabel(as *public.AssignedService) string {
	switch {
	case as.ShortName != "":
		return as.ShortName
	case as.Name != "":
		return as.Name
	}
	return as.Code
}

// servicesChange — что записать в хронологию после изменения списка.
type servicesChange struct {
	eventType string
	payload   map[string]any
}

// mutateServices — общий каркас: замок, проверки, изменение fn, запись ключа services,
// событие, время первого ввода; мониторинг — после COMMIT.
func (s *Service) mutateServices(ctx context.Context, id uuid.UUID, fn func(l *serviceList, now time.Time) (servicesChange, error)) error {
	p := core.PrincipalFrom(ctx)
	return pg.WithTx(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		m, raw, missing, err := lockServices(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := access.OwnAttempt(p, m.accessRow()); err != nil {
			return err
		}
		if core.AttemptTerminal(m.Status) {
			return errClosed("службы менять нельзя")
		}
		if m.Status == core.AttemptIssued {
			return errNotAccepted()
		}
		if missing {
			// Черновика нет (попытка создана в обход IssueNext) — заводим пустую карточку и берём её под замок.
			if _, err := tx.Exec(ctx, sqlEnsureDraft, id, emptyDraftJSON); err != nil {
				return fmt.Errorf("attempts: ensure draft: %w", err)
			}
			if err := tx.QueryRow(ctx, sqlLockDraftServices, id).Scan(&raw); err != nil {
				return fmt.Errorf("attempts: lock new draft: %w", err)
			}
		}

		list := parseServices(raw)
		now := s.now()
		change, err := fn(list, now)
		if err != nil {
			return err
		}
		enc, err := list.encode()
		if err != nil {
			return fmt.Errorf("attempts: encode services: %w", err)
		}
		if _, err := tx.Exec(ctx, sqlSetServices, id, enc, now); err != nil {
			return fmt.Errorf("attempts: write services: %w", err)
		}
		ev, err := eventlog.InsertServer(ctx, tx, id, change.eventType, change.payload, now)
		if err != nil {
			return err
		}
		// Действие со службами — содержательный ввод (core.UserInputEvent): время реакции.
		if !m.FirstInput {
			if _, err := tx.Exec(ctx, sqlFirstInputTx, id, now); err != nil {
				return fmt.Errorf("attempts: first input: %w", err)
			}
		}
		lessonID, firstInput := m.LessonID, m.FirstInput
		pg.OnCommit(ctx, func() {
			if !firstInput {
				s.meta.setFirstInput(id)
			}
			s.publishEvent(lessonID, id, ev)
		})
		return nil
	})
}

// ---------------------------------------------------------------- POST /services

type addServiceBody struct {
	ServiceCode string `json:"serviceCode"`
}

// addService — служба вручную: сразу «Получена службой» с историей «Добавлена» →
// «Получена службой» от имени системы (reaction.NewAssigned, как buildService мока).
func (s *Service) addService(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "attemptId")
	if err != nil {
		return err
	}
	var body addServiceBody
	if err := httpx.ReadJSON(r, &body); err != nil {
		return err
	}
	code := strings.TrimSpace(body.ServiceCode)
	if code == "" {
		return httpx.Validation("Не указана служба", map[string]string{"serviceCode": "обязательно"})
	}
	svc, ok := s.lookupService(code)
	if !ok {
		return httpx.Validation("Неизвестная служба: "+code, map[string]string{"serviceCode": "нет в справочнике служб"})
	}

	var out public.AssignedService
	err = s.mutateServices(r.Context(), id, func(l *serviceList, now time.Time) (servicesChange, error) {
		if l.findCode(svc.Code) >= 0 {
			return servicesChange{}, httpx.Conflict("Служба уже назначена")
		}
		out = reaction.NewAssigned(svc, reaction.SourceManual, false, manualReason, now)
		l.add(out)
		return servicesChange{
			eventType: core.EventServiceAssigned,
			payload:   map[string]any{"service": serviceLabel(&out), "source": reaction.SourceManual},
		}, nil
	})
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

// lookupService — служба справочника по коду (контракт: serviceCode), запасной вариант — id.
func (s *Service) lookupService(key string) (core.ServiceInfo, bool) {
	if s.cat == nil {
		return core.ServiceInfo{}, false
	}
	if svc, ok := s.cat.ServiceByCode(key); ok {
		return svc, true
	}
	return s.cat.ServiceByID(key)
}

// ---------------------------------------------------------------- DELETE /services/{serviceId}

// removeService — снять службу можно, только пока она не приступила к реагированию
// (статус «Добавлена» или «Получена службой»); снятие — нештатное действие, оно в хронологии.
func (s *Service) removeService(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "attemptId")
	if err != nil {
		return err
	}
	key := r.PathValue("serviceId")
	err = s.mutateServices(r.Context(), id, func(l *serviceList, _ time.Time) (servicesChange, error) {
		i := l.find(key)
		if i < 0 {
			return servicesChange{}, httpx.NotFound("Служба не назначена на карточку")
		}
		as := l.items[i]
		if !reaction.CanRemove(as) {
			return servicesChange{}, httpx.Conflict("Служба уже приступила к реагированию — снять её нельзя")
		}
		l.remove(i)
		return servicesChange{
			eventType: core.EventServiceRemoved,
			payload:   map[string]any{"service": serviceLabel(as)},
		}, nil
	})
	if err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

// ---------------------------------------------------------------- POST /services/{serviceId}/status

// changeServiceStatus — переход статуса реагирования по графу reaction (B-07): недопустимый
// переход — 409, нет обязательного комментария/номера наряда — 400 с полем.
func (s *Service) changeServiceStatus(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "attemptId")
	if err != nil {
		return err
	}
	var in public.ChangeStatusInput
	if err := httpx.ReadJSON(r, &in); err != nil {
		return err
	}
	if !in.Status.Valid() {
		return httpx.Validation("Неизвестный статус реагирования", map[string]string{"status": "недопустимое значение"})
	}
	key := r.PathValue("serviceId")
	operator := core.PrincipalFrom(r.Context()).OperatorLabel()
	squad, comment := deref(in.SquadNumber), deref(in.Comment)

	var out public.AssignedService
	err = s.mutateServices(r.Context(), id, func(l *serviceList, now time.Time) (servicesChange, error) {
		i := l.find(key)
		if i < 0 {
			return servicesChange{}, httpx.NotFound("Служба не назначена на карточку")
		}
		as := l.items[i]
		if err := reaction.Transition(as, in.Status, squad, comment, operator, now); err != nil {
			return servicesChange{}, transitionError(err)
		}
		reaction.Normalize(as)
		out = *as
		return servicesChange{
			eventType: core.EventServiceStatusChanged,
			payload: map[string]any{
				"service":    serviceLabel(as),
				"status":     string(in.Status),
				"hasComment": strings.TrimSpace(comment) != "",
			},
		}, nil
	})
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

// transitionError — ошибки reaction → ApiError (сообщение reaction уже по-русски).
func transitionError(err error) error {
	switch {
	case errors.Is(err, reaction.ErrCommentRequired):
		return httpx.Validation(err.Error(), map[string]string{"comment": "обязателен для этого статуса"})
	case errors.Is(err, reaction.ErrSquadRequired):
		return httpx.Validation(err.Error(), map[string]string{"squadNumber": "обязателен для этого статуса"})
	case errors.Is(err, reaction.ErrNotAllowed):
		return httpx.Conflict(err.Error())
	}
	return err
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
