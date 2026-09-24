// Package reaction — граф статусов реагирования служб на карточке происшествия.
//
// Точное зеркало frontend/src/shared/mocks/reactionTransitions.ts и
// SYSTEM_REACTION_STATUSES (frontend/src/shared/types/domain.ts). Источник правил —
// «Работа с АРМ-112 для ДДС от ОКр_ГСИ», таблица «Статусы реагирования служб»:
// статусы выбираются только последовательно; «Добавлена» и «Получена службой»
// проставляет система, в выпадающем списке диспетчера их нет.
//
// Пакет чистый (без I/O): фронт рисует список переходов строго из
// AssignedService.allowedNext, и считаем мы его только здесь — клиентскому
// allowedNext не доверяем никогда.
package reaction

import (
	"errors"
	"strings"
	"time"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
)

// Статусы реагирования (значения — как в контракте ReactionStatus).
const (
	StatusAdded       = public.Добавлена
	StatusReceived    = public.ПолученаСлужбой
	StatusAccepted    = public.Принята
	StatusNotAccepted = public.НеПринята
	StatusStarted     = public.НачалоРеагирования
	StatusArrived     = public.Прибытие
	StatusWorking     = public.ПроведениеРабот
	StatusDone        = public.РаботыЗавершены
	StatusRefused     = public.ОтказОтВыполненияРабот
)

// OperatorSystem — подпись автоматических статусов в истории.
const OperatorSystem = "система"

// Источники назначения службы (AssignedService.source).
const (
	SourceAuto   = string(public.AssignedServiceSourceAuto)
	SourceManual = string(public.AssignedServiceSourceManual)
	SourceVIS    = string(public.AssignedServiceSourceVis)
)

// transitions — граф переходов (порядок = порядок в выпадающем списке).
var transitions = map[public.ReactionStatus][]public.ReactionStatus{
	StatusAdded:       {StatusReceived},
	StatusReceived:    {StatusAccepted, StatusNotAccepted},
	StatusAccepted:    {StatusStarted, StatusArrived, StatusWorking, StatusDone, StatusRefused},
	StatusNotAccepted: {StatusAccepted},
	StatusStarted:     {StatusArrived, StatusWorking, StatusDone, StatusRefused},
	StatusArrived:     {StatusWorking, StatusDone, StatusRefused},
	StatusWorking:     {StatusDone, StatusRefused},
	StatusDone:        {},
	StatusRefused:     {},
}

// Памятка ДДС: «Служба 103 не проставляет статусы "Не принята" и "Отказ от выполнения
// работ". Вместо них проставляется статус "Работы завершены: Завершение работ без бригады"».
const service103 = "103"

func forbiddenFor(serviceCode string, s public.ReactionStatus) bool {
	return serviceCode == service103 && (s == StatusNotAccepted || s == StatusRefused)
}

// System — статус проставляет система, а не диспетчер (в allowedNext не попадает).
func System(s public.ReactionStatus) bool { return s == StatusAdded || s == StatusReceived }

// IsTerminal — терминальный статус закрывает службу для дальнейших переходов.
func IsTerminal(s public.ReactionStatus) bool { return s == StatusDone || s == StatusRefused }

// commentRequired — «Не принята», «Отказ от выполнения работ», «Работы завершены».
func commentRequired(s public.ReactionStatus) bool {
	return s == StatusNotAccepted || s == StatusRefused || s == StatusDone
}

// ValidStatus — строка является статусом реагирования из контракта.
func ValidStatus(s string) bool { return public.ReactionStatus(s).Valid() }

func labelFor(s public.ReactionStatus, serviceCode string) string {
	if serviceCode == service103 && s == StatusDone {
		return "Работы завершены: Завершение работ без бригады"
	}
	return string(s)
}

// Таблица переходов предвычислена для двух вариантов (служба 103 и все остальные):
// AllowedNext вызывается на каждый ответ со службами — без пересчёта и лишних строк.
var (
	allowedCommon = buildTable("")
	allowed103    = buildTable(service103)
)

func buildTable(serviceCode string) map[public.ReactionStatus][]public.AllowedTransition {
	t := make(map[public.ReactionStatus][]public.AllowedTransition, len(transitions))
	for from, tos := range transitions {
		out := make([]public.AllowedTransition, 0, len(tos))
		for _, to := range tos {
			if forbiddenFor(serviceCode, to) || System(to) {
				continue
			}
			out = append(out, public.AllowedTransition{
				Status:          to,
				Label:           labelFor(to, serviceCode),
				CommentRequired: commentRequired(to),
				// Номер наряда осмыслен, когда служба реально выезжает; в учебном контуре
				// не требуется (как во фронтовой фикстуре).
				SquadNumberRequired: false,
			})
		}
		t[from] = out
	}
	return t
}

// AllowedNext — допустимые переходы из current для службы serviceCode. Никогда не nil
// (терминальный или неизвестный статус -> []). Возвращается копия: вызывающий может
// хранить её в AssignedService и сериализовать, не боясь гонок на общей таблице.
func AllowedNext(current public.ReactionStatus, serviceCode string) []public.AllowedTransition {
	table := allowedCommon
	if serviceCode == service103 {
		table = allowed103
	}
	src := table[current]
	out := make([]public.AllowedTransition, len(src))
	copy(out, src)
	return out
}

// CanRemove — службу можно снять с карточки, только пока она не приступила к
// реагированию: текущий статус «Добавлена» или «Получена службой» (иначе 409).
func CanRemove(as *public.AssignedService) bool {
	return as != nil && System(as.CurrentStatus)
}

// NewAssigned — служба, только что назначенная на карточку (зеркало buildService в mockApi.ts).
// Памятка ДДС: «Добавлена» и «Получена службой» проставляет система — при направлении
// карточки и при её поступлении на сервер службы. В учебном контуре доставка мгновенная,
// поэтому служба сразу «Получена службой», и диспетчеру доступны «Принята» / «Не принята».
// source — auto | manual | vis; reason "" — без пояснения.
func NewAssigned(svc core.ServiceInfo, source string, isPrimary bool, reason string, now time.Time) public.AssignedService {
	now = now.UTC()
	var rs *string
	if reason != "" {
		r := reason
		rs = &r
	}
	return public.AssignedService{
		ServiceId: svc.ID,
		Code:      svc.Code,
		Name:      svc.Name,
		ShortName: svc.ShortName,
		IsPrimary: isPrimary,
		Source:    public.AssignedServiceSource(source),
		Reason:    rs,
		History: []public.ReactionStatusEntry{
			{Status: StatusAdded, At: now, Operator: OperatorSystem},
			{Status: StatusReceived, At: now, Operator: OperatorSystem},
		},
		CurrentStatus:   StatusReceived,
		CurrentStatusAt: now,
		AllowedNext:     AllowedNext(StatusReceived, svc.Code),
		Editable:        true,
	}
}

// Normalize приводит службу, пришедшую от клиента, к инвариантам сервера:
// allowedNext пересчитан из текущего статуса, editable = !terminal, history не nil.
func Normalize(as *public.AssignedService) {
	if as.History == nil {
		as.History = []public.ReactionStatusEntry{}
	}
	as.AllowedNext = AllowedNext(as.CurrentStatus, as.Code)
	as.Editable = !IsTerminal(as.CurrentStatus)
}

// ---------------------------------------------------------------- переходы

// Ошибки перехода. Error() у *TransitionError — готовое сообщение пользователю по-русски
// (как fail(...) в mockApi.ts); вид ошибки — через errors.Is с этими значениями.
var (
	ErrNotAllowed      = errors.New("переход статуса недопустим")
	ErrCommentRequired = errors.New("для статуса обязателен комментарий")
	ErrSquadRequired   = errors.New("для статуса обязателен номер наряда")
)

// TransitionError — отказ в переходе с человекочитаемым сообщением.
type TransitionError struct {
	Kind    error // ErrNotAllowed | ErrCommentRequired | ErrSquadRequired
	Status  public.ReactionStatus
	Message string
}

func (e *TransitionError) Error() string { return e.Message }
func (e *TransitionError) Unwrap() error { return e.Kind }

// Transition переводит службу в статус to. Допустимость проверяется по графу от текущего
// статуса (клиентскому allowedNext не доверяем). При успехе дописывает историю и обновляет
// currentStatus/currentStatusAt/allowedNext/editable. operatorLabel — Principal.OperatorLabel().
func Transition(as *public.AssignedService, to public.ReactionStatus, squad, comment, operatorLabel string, now time.Time) error {
	var tr *public.AllowedTransition
	allowed := AllowedNext(as.CurrentStatus, as.Code)
	for i := range allowed {
		if allowed[i].Status == to {
			tr = &allowed[i]
			break
		}
	}
	if tr == nil {
		return &TransitionError{Kind: ErrNotAllowed, Status: to,
			Message: "Переход в статус «" + string(to) + "» сейчас недопустим"}
	}
	comment = strings.TrimSpace(comment)
	squad = strings.TrimSpace(squad)
	if tr.CommentRequired && comment == "" {
		return &TransitionError{Kind: ErrCommentRequired, Status: to,
			Message: "Для статуса «" + string(to) + "» комментарий обязателен"}
	}
	if tr.SquadNumberRequired && squad == "" {
		return &TransitionError{Kind: ErrSquadRequired, Status: to,
			Message: "Для статуса «" + string(to) + "» нужен номер наряда"}
	}

	now = now.UTC()
	e := public.ReactionStatusEntry{Status: to, At: now, Operator: operatorLabel}
	if squad != "" {
		e.SquadNumber = &squad
	}
	if comment != "" {
		e.Comment = &comment
	}
	if as.History == nil {
		as.History = make([]public.ReactionStatusEntry, 0, 4)
	}
	as.History = append(as.History, e)
	as.CurrentStatus = to
	as.CurrentStatusAt = now
	as.AllowedNext = AllowedNext(to, as.Code)
	as.Editable = !IsTerminal(to)
	return nil
}
