// Package dds — ракурс «Диспетчер ДДС» (lessons.settings.perspective = "dds", контракт v1.3).
//
// ТЗ («действия с карточками») и Памятка АРМ-112 для ДДС: карточку происшествия создаёт
// оператор 112 и направляет службам списка оповещения; диспетчер службы в течение 30 с
// подтверждает приём («Принята» / «Не принята» с причиной в комментарии), затем
// проставляет статусы реагирования с комментариями. Пакет чистый (без I/O):
//   - Pick — за какую службу обучающийся получит карточку (профиль ДДС или основная служба);
//   - IncomingCard — карточка, которая «поступает» диспетчеру при взятии в работу;
//   - Evaluate — слой fields в ракурсе dds: протокол реагирования своей службы против
//     эталона etalons.scoring.reaction (решение, норматив решения, обязательные статусы);
//   - Comments — комментарии диспетчера к статусам (свободный текст для грамматики).
package dds

import (
	"math"
	"strconv"
	"strings"
	"time"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/reaction"
)

// SplitCodes — коды служб из store.ServiceCodesSQL (строка через запятую) в срез.
func SplitCodes(s string) []string {
	if s = strings.TrimSpace(s); s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Pick — служба, за диспетчера которой обучающийся работает с карточкой сценария.
// codes — службы списка оповещения сценария (известные справочнику, основная — первой).
// profile — код службы профиля обучающегося ("" — профиля нет):
//   - профиль есть — карточка профильная, только если его служба в списке оповещения
//     (ТЗ: «в ленту попадают только профильные события»);
//   - профиля нет — обучающийся работает за основную службу карточки.
//
// ok=false — сценарий этому обучающемуся не выдаётся.
func Pick(codes []string, profile string) (string, bool) {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		if len(codes) == 0 {
			return "", false
		}
		return codes[0], true
	}
	for _, c := range codes {
		if strings.EqualFold(c, profile) {
			return c, true
		}
	}
	return "", false
}

// ServiceLookup — служба справочника по коду (core.Catalog.ServiceByCode).
type ServiceLookup func(code string) (core.ServiceInfo, bool)

// IncomingCard — карточка, поступившая диспетчеру ДДС от оператора 112: поля эталонной
// карточки сценария (её заполнил «оператор 112»), АОН — номер заявителя, службы списка
// оповещения в статусе «Получена службой» (доставка в учебном контуре мгновенная),
// actionsTaken пуст — текст действия пишет сам диспетчер. codes — службы оповещения
// (основная первой), acting — служба обучающегося: она в списке всегда, даже если эталон
// правили после выдачи. Коды, которых нет в справочнике, пропускаются.
func IncomingCard(etalon *public.IncidentCardDraft, codes []string, acting, callerPhone string, lookup ServiceLookup, now time.Time) public.IncidentCardDraft {
	var card public.IncidentCardDraft
	if etalon != nil {
		card = *etalon
		// Срезы и карты эталона не разделяем с вызывающим: карточка живёт своей жизнью.
		card.IncidentTypeIds = append([]string(nil), etalon.IncidentTypeIds...)
		card.Attributes = make(map[string]any, len(etalon.Attributes))
		for k, v := range etalon.Attributes {
			card.Attributes[k] = v
		}
	} else {
		card = convert.EmptyDraft()
	}
	card.ActionsTaken = ""
	if phone := strings.TrimSpace(callerPhone); phone != "" && strings.TrimSpace(convert.Deref(card.Phones.Aon)) == "" {
		card.Phones.Aon = &phone
	}

	// Пометки основной службы и причины назначения — из эталона, если он их знает.
	type meta struct {
		primary bool
		reason  string
	}
	known := make(map[string]meta)
	if etalon != nil {
		for i := range etalon.Services {
			sv := &etalon.Services[i]
			known[strings.ToLower(sv.Code)] = meta{primary: sv.IsPrimary, reason: convert.Deref(sv.Reason)}
		}
	}
	anyPrimary := false
	for _, m := range known {
		anyPrimary = anyPrimary || m.primary
	}

	list := codes
	if acting != "" && !containsFold(codes, acting) {
		list = append(append([]string(nil), codes...), acting)
	}
	card.Services = make([]public.AssignedService, 0, len(list))
	seen := make(map[string]bool, len(list))
	for i, code := range list {
		key := strings.ToLower(code)
		if seen[key] || lookup == nil {
			continue
		}
		svc, ok := lookup(code)
		if !ok {
			continue
		}
		seen[key] = true
		m := known[key]
		primary := m.primary || (!anyPrimary && i == 0)
		card.Services = append(card.Services, reaction.NewAssigned(svc, reaction.SourceAuto, primary, m.reason, now))
	}
	convert.NormalizeDraft(&card)
	return card
}

// FindService — служба карточки по коду (без учёта регистра); nil — нет.
func FindService(card *public.IncidentCardDraft, code string) *public.AssignedService {
	if card == nil || code == "" {
		return nil
	}
	for i := range card.Services {
		if strings.EqualFold(card.Services[i].Code, code) {
			return &card.Services[i]
		}
	}
	return nil
}

// Comments — комментарии диспетчера к статусам своей службы (без системных записей),
// по строке на комментарий: свободный текст, который проверяется грамматикой.
func Comments(card *public.IncidentCardDraft, acting string) string {
	sv := FindService(card, acting)
	if sv == nil {
		return ""
	}
	var b strings.Builder
	for i := range sv.History {
		e := &sv.History[i]
		if e.Operator == reaction.OperatorSystem {
			continue
		}
		if c := strings.TrimSpace(convert.Deref(e.Comment)); c != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(c)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------- оценка

// Веса проверок протокола. Решение по карточке — главное (служба выезжает или нет),
// норматив решения — следом (Памятка: без статуса за 30 с карточка «Не оповещено»),
// остальное — по единице.
const (
	weightDecision     = 3
	weightDecisionTime = 2
	weightRefusal      = 1
	weightStatus       = 1
)

// Поля FieldError слоя fields в ракурсе dds (UI показывает label, по field группируются
// типичные ошибки в прогрессе обучающегося).
const (
	FieldDecision     = "reaction.decision"
	FieldDecisionTime = "reaction.decisionTime"
	FieldRefusal      = "reaction.refusal"
	FieldStatuses     = "reaction.statuses"
)

// Evaluate — протокол реагирования службы acting в сданной карточке против эталона
// expectation (nil — значения по умолчанию: «Принята» за 30 с).
// openedAt — когда карточка взята в работу (старт норматива решения; nil — норматив не
// проверить, проверка считается проваленной). Балл — доля пройденных проверок по весам,
// 0..100; ошибки — FieldError (как у слоя полей карточки), никогда не nil.
func Evaluate(card *public.IncidentCardDraft, acting string, expectation *model.ReactionExpectation, openedAt *time.Time) (float64, []public.FieldError) {
	exp := expectation.Resolved()
	expected := decisionStatus(exp.Decision)
	errs := []public.FieldError{}
	total, lost := 0.0, 0.0
	fail := func(e public.FieldError) {
		lost += float64(e.Weight)
		errs = append(errs, e)
	}

	var history []public.ReactionStatusEntry
	if sv := FindService(card, acting); sv != nil {
		history = sv.History
	}
	first, final := decisions(history)

	// 1. Решение по карточке: итоговое («Не принята» → «Принята» — итог «Принята»).
	total += weightDecision
	switch {
	case final == nil:
		fail(public.FieldError{Field: FieldDecision, Label: "Решение по карточке («Принята» / «Не принята»)",
			Kind: public.FieldErrorKindMissing, Expected: string(expected), Weight: weightDecision})
	case final.Status != expected:
		fail(public.FieldError{Field: FieldDecision, Label: "Решение по карточке («Принята» / «Не принята»)",
			Kind: public.FieldErrorKindWrong, Expected: string(expected), Actual: string(final.Status), Weight: weightDecision})
	}

	// 2. Норматив решения: первое решение — не позже decisionWithinSec от взятия в работу.
	total += weightDecisionTime
	limit := "не позже " + strconv.Itoa(exp.DecisionWithinSec) + " с"
	switch {
	case first == nil:
		fail(public.FieldError{Field: FieldDecisionTime, Label: "Норматив решения по карточке",
			Kind: public.FieldErrorKindMissing, Expected: limit, Weight: weightDecisionTime})
	case openedAt == nil:
		fail(public.FieldError{Field: FieldDecisionTime, Label: "Норматив решения по карточке",
			Kind: public.FieldErrorKindWrong, Expected: limit, Actual: "время взятия в работу неизвестно", Weight: weightDecisionTime})
	default:
		if spent := first.At.Sub(*openedAt); spent > time.Duration(exp.DecisionWithinSec)*time.Second {
			fail(public.FieldError{Field: FieldDecisionTime, Label: "Норматив решения по карточке",
				Kind: public.FieldErrorKindWrong, Expected: limit, Actual: seconds(spent), Weight: weightDecisionTime})
		}
	}

	// 3. Лишний отказ: карточку надо было принять, а диспетчер сначала отказал (служба
	// не выехала вовремя, даже если потом «Принята»).
	if exp.Decision == model.DecisionAccept {
		total += weightRefusal
		if first != nil && first.Status == reaction.StatusNotAccepted {
			fail(public.FieldError{Field: FieldRefusal, Label: "Необоснованный отказ «Не принята»",
				Kind: public.FieldErrorKindExtra, Actual: string(reaction.StatusNotAccepted), Weight: weightRefusal})
		}
	}

	// 4. Обязательные статусы после решения.
	for _, st := range exp.RequiredStatuses {
		total += weightStatus
		if !hasStatus(history, public.ReactionStatus(st)) {
			fail(public.FieldError{Field: FieldStatuses, Label: "Статус «" + st + "»",
				Kind: public.FieldErrorKindMissing, Expected: st, Weight: weightStatus})
		}
	}

	if total <= 0 {
		return 100, errs
	}
	return math.Round(100 * (total - lost) / total), errs
}

func decisionStatus(decision string) public.ReactionStatus {
	if decision == model.DecisionReject {
		return reaction.StatusNotAccepted
	}
	return reaction.StatusAccepted
}

// decisions — первое решение диспетчера и итоговое: «Принята» после «Не принята»
// перекрывает отказ (граф статусов назад из «Принята» не пускает).
func decisions(history []public.ReactionStatusEntry) (first, final *public.ReactionStatusEntry) {
	for i := range history {
		e := &history[i]
		if e.Status != reaction.StatusAccepted && e.Status != reaction.StatusNotAccepted {
			continue
		}
		if first == nil {
			first = e
		}
		if final == nil || e.Status == reaction.StatusAccepted {
			final = e
		}
	}
	return first, final
}

func hasStatus(history []public.ReactionStatusEntry, st public.ReactionStatus) bool {
	for i := range history {
		if history[i].Status == st {
			return true
		}
	}
	return false
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// seconds — длительность для отчёта: «42 с» (с десятыми, если меньше минуты и не целое).
func seconds(d time.Duration) string {
	s := d.Seconds()
	if s < 60 && s != math.Trunc(s) {
		return strconv.FormatFloat(math.Round(s*10)/10, 'f', -1, 64) + " с"
	}
	return strconv.Itoa(int(math.Round(s))) + " с"
}
