package convert

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
)

// Проекции карточки (DESIGN §4): студент заполняет IncidentCardDraft (форма АРМ-112);
// ai-service видит контрактный IncidentCard (DraftToCard); эталон, сгенерированный LLM,
// приходит как IncidentCard и превращается в форму АРМ для сверки (CardToDraft).

// EtalonServiceTime — метка currentStatusAt у служб эталонной карточки: время в эталоне
// не сравнивается, но поле обязательное; фиксированное значение (а не zero time
// "0001-01-01") безопасно для форматирования дат на фронте.
var EtalonServiceTime = time.Unix(0, 0).UTC()

// Статусы реагирования, которые проставляет система.
const (
	ReactionAdded    = "Добавлена"
	ReactionReceived = "Получена службой"
)

// EmptyDraft — пустая карточка: зеркало emptyCard() фронта (frontend/src/shared/utils/card.ts):
// все строковые поля адреса = "", флаги false, массивы [], карты {}.
func EmptyDraft() public.IncidentCardDraft {
	// 15 строковых полей адреса — одним массивом: одна аллокация, указатели разные.
	s := new([15]string)
	var d public.IncidentCardDraft
	d.Address = public.AddressDraft{
		Raw:         "",
		Country:     &s[0],
		Region:      &s[1],
		Settlement:  &s[2],
		Object:      &s[3],
		Okrug:       &s[4],
		District:    &s[5],
		Street:      &s[6],
		House:       &s[7],
		Building:    &s[8],
		Structure:   &s[9],
		Apartment:   &s[10],
		Entrance:    &s[11],
		Floor:       &s[12],
		Code:        &s[13],
		Descriptive: &s[14],
	}
	d.IncidentTypeIds = []string{}
	d.Attributes = map[string]any{}
	d.Services = []public.AssignedService{}
	return d
}

// NormalizeDraft — обязательные массивы/карты не nil (карточка из jsonb или от клиента).
func NormalizeDraft(d *public.IncidentCardDraft) {
	if d == nil {
		return
	}
	if d.IncidentTypeIds == nil {
		d.IncidentTypeIds = []string{}
	}
	if d.Attributes == nil {
		d.Attributes = map[string]any{}
	}
	if d.Services == nil {
		d.Services = []public.AssignedService{}
	}
	for i := range d.Services {
		if d.Services[i].History == nil {
			d.Services[i].History = []public.ReactionStatusEntry{}
		}
		if d.Services[i].AllowedNext == nil {
			d.Services[i].AllowedNext = []public.AllowedTransition{}
		}
	}
}

// DraftToCard — форма АРМ -> контрактный IncidentCard (для ai-service и etalons.card).
//   - category_code — код первого распознаваемого incidentTypeIds (cat.TypeByID);
//   - address: raw, city=settlement, street, house(+building как "14к2"), entrance, floor,
//     apartment, landmark=descriptive;
//   - applicant: name, phone = phones.provided, иначе phones.aon;
//   - casualties.injured = victimsCount при victimsPresent (без числа — «хотя бы один»);
//   - services_to_notify — коды служб карточки; description, actions_taken, attributes как есть.
//
// Пустые значения не передаются (nil), чтобы LLM не видела «пустых фактов».
func DraftToCard(d *public.IncidentCardDraft, cat core.Catalog) components.IncidentCard {
	var out components.IncidentCard
	if d == nil {
		return out
	}
	if cat != nil {
		for _, id := range d.IncidentTypeIds {
			if t, ok := cat.TypeByID(id); ok && t.Code != "" {
				code := t.Code
				out.CategoryCode = &code
				break
			}
		}
	}

	a := &d.Address
	addr := components.Address{
		Raw:       NonEmpty(strings.TrimSpace(a.Raw)),
		City:      trimPtr(a.Settlement),
		Street:    trimPtr(a.Street),
		House:     NonEmpty(JoinHouse(Str(a.House), Str(a.Building))),
		Entrance:  trimPtr(a.Entrance),
		Floor:     trimPtr(a.Floor),
		Apartment: trimPtr(a.Apartment),
		Landmark:  trimPtr(a.Descriptive),
	}
	if !addressEmpty(&addr) {
		out.Address = &addr
	}

	name := trimPtr(d.Applicant.Name)
	phone := trimPtr(d.Phones.Provided)
	if phone == nil {
		phone = trimPtr(d.Phones.Aon)
	}
	if name != nil || phone != nil {
		out.Applicant = &struct {
			Name  *string `json:"name,omitempty"`
			Phone *string `json:"phone,omitempty"`
		}{Name: name, Phone: phone}
	}

	if d.Flags.VictimsPresent {
		n := 1
		if d.Flags.VictimsCount != nil && *d.Flags.VictimsCount > 0 {
			n = *d.Flags.VictimsCount
		}
		out.Casualties = &struct {
			Dead    *int `json:"dead,omitempty"`
			Injured *int `json:"injured,omitempty"`
			Trapped *int `json:"trapped,omitempty"`
		}{Injured: &n}
	}

	if len(d.Services) > 0 {
		codes := make([]string, 0, len(d.Services))
		for i := range d.Services {
			if c := strings.TrimSpace(d.Services[i].Code); c != "" {
				codes = append(codes, c)
			}
		}
		out.ServicesToNotify = SlicePtr(codes)
	}
	out.Description = NonEmpty(strings.TrimSpace(d.Description))
	out.ActionsTaken = NonEmpty(strings.TrimSpace(d.ActionsTaken))
	if len(d.Attributes) > 0 {
		attrs := d.Attributes
		out.Attributes = &attrs
	}
	return out
}

// CardToDraft — контрактный IncidentCard -> форма АРМ (эталон для сверки слоя 1).
//   - incidentTypeIds — по category_code (cat.TypeByCode);
//   - адрес: settlement=city, house/building — из "14к2" / "14 корп. 2";
//   - applicant.name; phones.provided = applicant.phone;
//   - flags.victimsPresent/victimsCount — сумма injured+trapped+dead (> 0);
//   - services — минимальные AssignedService эталона из справочника (неизвестные коды
//     пропускаются): первая — основная, source=auto, «Получена службой», история пуста;
//   - description, actionsTaken, attributes.
func CardToDraft(c *components.IncidentCard, cat core.Catalog) public.IncidentCardDraft {
	d := EmptyDraft()
	if c == nil {
		return d
	}
	if c.CategoryCode != nil && cat != nil {
		if t, ok := cat.TypeByCode(strings.TrimSpace(*c.CategoryCode)); ok && t.ID != "" {
			d.IncidentTypeIds = append(d.IncidentTypeIds, t.ID)
		}
	}
	if a := c.Address; a != nil {
		d.Address.Raw = Str(a.Raw)
		*d.Address.Settlement = Str(a.City)
		*d.Address.Street = Str(a.Street)
		house, building := SplitHouse(Str(a.House))
		*d.Address.House = house
		*d.Address.Building = building
		*d.Address.Entrance = Str(a.Entrance)
		*d.Address.Floor = Str(a.Floor)
		*d.Address.Apartment = Str(a.Apartment)
		*d.Address.Descriptive = Str(a.Landmark)
	}
	if ap := c.Applicant; ap != nil {
		d.Applicant.Name = trimPtr(ap.Name)
		d.Phones.Provided = trimPtr(ap.Phone)
	}
	if cs := c.Casualties; cs != nil {
		n := max(Deref(cs.Injured), 0) + max(Deref(cs.Trapped), 0) + max(Deref(cs.Dead), 0)
		if n > 0 {
			d.Flags.VictimsPresent = true
			d.Flags.VictimsCount = &n
		}
	}
	if c.ServicesToNotify != nil && cat != nil {
		for _, code := range *c.ServicesToNotify {
			info, ok := cat.ServiceByCode(strings.TrimSpace(code))
			if !ok || serviceListed(d.Services, info.Code) {
				continue
			}
			d.Services = append(d.Services, public.AssignedService{
				ServiceId:       info.ID,
				Code:            info.Code,
				Name:            info.Name,
				ShortName:       info.ShortName,
				IsPrimary:       len(d.Services) == 0,
				Source:          public.AssignedServiceSourceAuto,
				CurrentStatus:   public.ReactionStatus(ReactionReceived),
				CurrentStatusAt: EtalonServiceTime,
				History:         []public.ReactionStatusEntry{},
				AllowedNext:     []public.AllowedTransition{},
				Editable:        false,
			})
		}
	}
	d.Description = Str(c.Description)
	d.ActionsTaken = Str(c.ActionsTaken)
	if c.Attributes != nil && len(*c.Attributes) > 0 {
		m := make(map[string]any, len(*c.Attributes))
		for k, v := range *c.Attributes {
			m[k] = v
		}
		d.Attributes = m
	}
	return d
}

// JoinHouse — "14" + "2" -> "14к2" (компактная запись корпуса, как в адресах Москвы).
func JoinHouse(house, building string) string {
	house, building = strings.TrimSpace(house), strings.TrimSpace(building)
	switch {
	case building == "":
		return house
	case house == "":
		return "к" + building
	}
	return house + "к" + building
}

// SplitHouse — обратное JoinHouse: "14к2", "14 к. 2", "14, корп. 2", "14 корпус 2" ->
// ("14", "2"), "32Ак3" -> ("32А", "3"). Литеры дома ("32А") не трогает: корпус отделяется
// только после номера дома.
func SplitHouse(h string) (house, building string) {
	h = strings.TrimSpace(h)
	if h == "" {
		return "", ""
	}
	lower := strings.ToLower(h)
	if len(lower) != len(h) { // экзотика, меняющая длину в байтах, — индексы бы разъехались
		lower = h
	}
	for _, marker := range [...]string{"корпус", "корп.", "корп", "к."} {
		i := strings.LastIndex(lower, marker)
		if i <= 0 {
			continue
		}
		// Маркер без точки — целое слово: "14 корпус" не даёт корпус "ус" (через "корп"),
		// "14 корпуса" — корпус "а".
		if !strings.HasSuffix(marker, ".") {
			if r, _ := utf8.DecodeRuneInString(h[i+len(marker):]); unicode.IsLetter(r) {
				continue
			}
		}
		if hh, bb, ok := splitAt(h, i, len(marker)); ok {
			return hh, bb
		}
	}
	// "14к2", "32Ак3" (так их склеивает JoinHouse): «к» сразу после номера дома, за ней —
	// непустое продолжение.
	if i := strings.LastIndex(lower, "к"); i > 0 && isBuildingK(h[:i], h[i+len("к"):]) {
		if hh, bb, ok := splitAt(h, i, len("к")); ok {
			return hh, bb
		}
	}
	return h, ""
}

// isBuildingK — «к» между head и tail отделяет корпус: head оканчивается номером дома —
// цифрой или литерой сразу после цифры ("32А", тогда корпус обязан начинаться с цифры),
// а tail — не слово ("14корпус" — не корпус «орпус»; однобуквенный корпус "14кА" — да).
func isBuildingK(head, tail string) bool {
	tail = strings.TrimLeft(tail, " .")
	next, n := utf8.DecodeRuneInString(tail)
	if after, _ := utf8.DecodeRuneInString(tail[n:]); unicode.IsLetter(next) && unicode.IsLetter(after) {
		return false
	}
	r, n := utf8.DecodeLastRuneInString(head)
	if unicode.IsDigit(r) {
		return true
	}
	if !unicode.IsLetter(r) {
		return false
	}
	prev, _ := utf8.DecodeLastRuneInString(head[:len(head)-n])
	return unicode.IsDigit(prev) && unicode.IsDigit(next)
}

// splitAt — h[:i] / h[i+n:] с очисткой разделителей; ok=false, если часть пуста или
// перед маркером нет цифры (значит это не корпус, а часть названия).
func splitAt(h string, i, n int) (house, building string, ok bool) {
	house = strings.TrimRight(strings.TrimSpace(h[:i]), " ,")
	building = strings.TrimLeft(strings.TrimSpace(h[i+n:]), " .")
	if house == "" || building == "" {
		return "", "", false
	}
	if r, _ := utf8.DecodeLastRuneInString(house); !unicode.IsDigit(r) && !unicode.IsLetter(r) {
		return "", "", false
	}
	if r, _ := utf8.DecodeRuneInString(house); !unicode.IsDigit(r) {
		return "", "", false
	}
	return house, building, true
}

func serviceListed(s []public.AssignedService, code string) bool {
	for i := range s {
		if s[i].Code == code {
			return true
		}
	}
	return false
}

func trimPtr(p *string) *string {
	if p == nil {
		return nil
	}
	return NonEmpty(strings.TrimSpace(*p))
}

func addressEmpty(a *components.Address) bool {
	return a.Raw == nil && a.City == nil && a.Street == nil && a.House == nil &&
		a.Entrance == nil && a.Floor == nil && a.Apartment == nil && a.Landmark == nil
}
