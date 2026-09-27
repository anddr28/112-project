package scoring

import (
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
)

// Слой 1 — формализованные поля карточки против эталона (etalons.card_draft).

// FieldSpec — что и с каким весом проверять (etalons.scoring).
type FieldSpec struct {
	// Required — пути IncidentCardDraft (etalons.scoring.required_fields). Порядок сохраняется
	// в FieldErrors; дубли (в т.ч. после приведения к канону) игнорируются.
	Required []string
	// Weights — веса полей (etalons.scoring.field_weights); нет ключа / отрицательный /
	// не число → 1. Ключ может быть как в Required, так и каноническим путём.
	Weights map[string]float64
	// ServiceCodes — эталонные коды служб, если в card_draft эталона служб нет
	// (например, есть только etalons.card.services_to_notify). Пусто — берём из card_draft.
	ServiceCodes []string
}

type specEntry struct {
	path string // канонический
	w    float64
}

// entries — канонические пути без дублей с весами. Путь, которого нет в форме АРМ
// (опечатка, чужая схема), пропускается: такое поле заполнить невозможно, и оно было бы
// вечным «не заполнено» с подписью «Поле карточки» в каждой попытке.
func (s FieldSpec) entries() []specEntry {
	out := make([]specEntry, 0, len(s.Required))
next:
	for _, p := range s.Required {
		c := CanonicalPath(p)
		if c == "" || classOf(c) == clsUnknown {
			continue
		}
		for i := range out {
			if out[i].path == c {
				continue next
			}
		}
		out = append(out, specEntry{path: c, w: s.weight(p, c)})
	}
	return out
}

func (s FieldSpec) weight(orig, canon string) float64 {
	if s.Weights != nil {
		if w, ok := s.Weights[orig]; ok && validWeight(w) {
			return w
		}
		if canon != orig {
			if w, ok := s.Weights[canon]; ok && validWeight(w) {
				return w
			}
		}
	}
	return 1
}

func validWeight(w float64) bool { return w >= 0 && !math.IsInf(w, 0) && !math.IsNaN(w) }

// FieldErrors — ошибки заполнения обязательных полей (порядок — как в spec.Required).
// Никогда не nil. card/etalon nil — пустая карточка; cat nil — встроенные подписи и
// сырые коды вместо названий. Пути вне формы АРМ не проверяются (см. entries).
//
// Правила:
//   - пусто в ответе → missing (даже если в эталоне тоже пусто: поле обязательное);
//   - свободный текст (description, actionsTaken) — только наличие, смысл — слой semantic;
//   - флажки: false — тоже ответ; missing — если эталон «да», а отмечено «нет»;
//     эталон не задан (nil) — любой ответ верен;
//   - остальное — сравнение нормализованных множеств значений; в эталоне пусто → ok;
//   - службы: не хватает эталонных → wrong (пусто → missing); все эталонные + лишние → extra.
func FieldErrors(card, etalon *public.IncidentCardDraft, spec FieldSpec, cat core.Catalog) []public.FieldError {
	out := []public.FieldError{}
	entries := spec.entries()
	if len(entries) == 0 {
		return out
	}
	var empty public.IncidentCardDraft
	if card == nil {
		card = &empty
	}
	if etalon == nil {
		etalon = &empty
	}
	for _, e := range entries {
		cls := classOf(e.path)
		act := valueAt(card, e.path, cls, cat, nil)
		if cls == clsBool && act.empty {
			act = boolVal(false) // флажок, которого не касались, — «нет», а не «не заполнено»
		}
		exp := valueAt(etalon, e.path, cls, cat, spec.ServiceCodes)
		var street []string
		if cls == clsAddrRaw {
			street = addrTokens(strPtr(etalon.Address.Street), false)
		}
		kind, ok := compare(cls, exp, act, street)
		if ok {
			continue
		}
		fe := public.FieldError{
			Field:  e.path,
			Label:  FieldLabel(cat, e.path),
			Kind:   kind,
			Weight: float32(e.w),
		}
		if cls == clsServices {
			// Имена служб — из общего словаря обеих сторон: одна служба подписана одинаково
			// в expected и actual (на этом строится список лишних служб в рекомендациях).
			names := serviceNameIndex(exp.raw, act.raw, cat)
			fe.Expected, fe.Actual = serviceNames(exp.raw, names, cat), serviceNames(act.raw, names, cat)
		} else {
			fe.Expected, fe.Actual = readable(cls, e.path, exp, cat), readable(cls, e.path, act, cat)
		}
		out = append(out, fe)
	}
	return out
}

// FieldsScore — балл слоя fields 0..100: 100·Σw(верных)/Σw(обязательных); extra — половина
// веса. Нет обязательных полей (или все веса 0) → 100. Учитываются только ошибки по
// полям из spec.Required (каждое поле — один раз).
func FieldsScore(errs []public.FieldError, spec FieldSpec) float64 {
	entries := spec.entries()
	total := 0.0
	for _, e := range entries {
		total += e.w
	}
	if len(entries) == 0 || total <= 0 {
		return 100
	}
	var buf [32]bool
	counted := buf[:]
	if len(entries) > len(buf) {
		counted = make([]bool, len(entries))
	}
	lost := 0.0
	for _, fe := range errs {
		p := CanonicalPath(fe.Field)
		for i := range entries {
			if entries[i].path != p {
				continue
			}
			if counted[i] {
				break
			}
			counted[i] = true
			k := 1.0
			if fe.Kind == public.FieldErrorKindExtra {
				k = 0.5
			}
			lost += entries[i].w * k
			break
		}
	}
	return Round2(clamp100(100 * (total - lost) / total))
}

// EvaluateFields — FieldErrors + FieldsScore одним вызовом (spec разбирается один раз).
func EvaluateFields(card, etalon *public.IncidentCardDraft, spec FieldSpec, cat core.Catalog) (float64, []public.FieldError) {
	errs := FieldErrors(card, etalon, spec, cat)
	return FieldsScore(errs, spec), errs
}

// ---------------------------------------------------------------- значения

// fval — значение поля, подготовленное к сравнению.
type fval struct {
	empty  bool     // нет ответа (для флажка — не задан вовсе)
	isBool bool     // значение — флажок
	b      bool     // значение флажка
	set    []string // нормализованное множество (отсортировано, без повторов)
	raw    any      // исходное значение — для читаемого expected/actual
}

var emptyVal = fval{empty: true}

func strPtr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// valueAt — значение поля карточки по каноническому пути (типизированный доступ без
// JSON-круга и рефлексии). Неизвестный путь — пусто.
func valueAt(d *public.IncidentCardDraft, path string, cls valClass, cat core.Catalog, serviceCodes []string) fval {
	switch cls {
	case clsFree:
		var s string
		switch path {
		case "description":
			s = d.Description
		case "actionsTaken":
			s = d.ActionsTaken
		case "address.descriptive":
			s = strPtr(d.Address.Descriptive)
		}
		if s = strings.TrimSpace(s); s == "" {
			return emptyVal
		}
		return fval{raw: s}
	case clsName:
		s := strings.TrimSpace(strPtr(d.Applicant.Name))
		set := wordTokens(s)
		if len(set) == 0 {
			return emptyVal
		}
		return fval{set: set, raw: s}
	case clsText:
		var s string
		switch path {
		case "applicant.status":
			if d.Applicant.Status != nil {
				s = string(*d.Applicant.Status)
			}
		case "phones.channel":
			s = strPtr(d.Phones.Channel)
		case "address.source":
			if d.Address.Source != nil {
				s = string(*d.Address.Source)
			}
		case "address.code":
			s = strPtr(d.Address.Code)
		}
		return scalarVal(s)
	case clsPhone:
		var s string
		switch path {
		case "phones.aon":
			s = strPtr(d.Phones.Aon)
		case "phones.provided":
			s = strPtr(d.Phones.Provided)
		case "phones.onSite":
			s = strPtr(d.Phones.OnSite)
		}
		n := NormalizePhone(s)
		if n == "" {
			return emptyVal
		}
		return fval{set: []string{n}, raw: strings.TrimSpace(s)}
	case clsBool:
		switch path {
		case "applicant.foreignLanguage":
			return boolPtrVal(d.Applicant.ForeignLanguage)
		case "phones.foreign":
			return boolPtrVal(d.Phones.Foreign)
		case "flags.victimsPresent":
			return boolVal(d.Flags.VictimsPresent)
		case "flags.ambulanceRefusal":
			return boolVal(d.Flags.AmbulanceRefusal)
		case "flags.blocked":
			return boolVal(d.Flags.Blocked)
		case "flags.noContact":
			return boolVal(d.Flags.NoContact)
		case "flags.callDropped":
			return boolVal(d.Flags.CallDropped)
		}
	case clsInt:
		if d.Flags.VictimsCount == nil {
			return emptyVal
		}
		n := *d.Flags.VictimsCount
		return fval{set: []string{strconv.Itoa(n)}, raw: n}
	case clsCoord:
		p := d.Address.Lat
		if path == "address.lon" {
			p = d.Address.Lon
		}
		if p == nil || math.IsNaN(float64(*p)) || math.IsInf(float64(*p), 0) {
			return emptyVal
		}
		return fval{set: []string{strconv.FormatFloat(float64(*p), 'f', 4, 64)}, raw: *p}
	case clsAddrRaw:
		s := strings.TrimSpace(d.Address.Raw)
		if s == "" {
			s = composeAddress(&d.Address)
		}
		set := sortUnique(addrTokens(s, true))
		if len(set) == 0 {
			return emptyVal
		}
		return fval{set: set, raw: s}
	case clsAddrPart:
		p := addrPart(&d.Address, path[len("address."):])
		s := strings.TrimSpace(strPtr(p))
		n := NormalizeAddressPart(s)
		if n == "" {
			return emptyVal
		}
		return fval{set: []string{n}, raw: s}
	case clsTypes:
		return typesVal(d.IncidentTypeIds, cat)
	case clsServices:
		return servicesVal(d.Services, serviceCodes, cat)
	case clsAttr:
		if d.Attributes == nil {
			return emptyVal
		}
		return attrVal(d.Attributes[path[len(attrPrefix):]])
	}
	return emptyVal
}

func scalarVal(s string) fval {
	s = strings.TrimSpace(s)
	if s == "" {
		return emptyVal
	}
	n := normScalar(s)
	if n == "" {
		return emptyVal
	}
	return fval{set: []string{n}, raw: s}
}

func boolVal(b bool) fval { return fval{isBool: true, b: b, raw: b} }

func boolPtrVal(p *bool) fval {
	if p == nil {
		return emptyVal
	}
	return boolVal(*p)
}

func addrPart(a *public.AddressDraft, name string) *string {
	switch name {
	case "apartment":
		return a.Apartment
	case "building":
		return a.Building
	case "country":
		return a.Country
	case "district":
		return a.District
	case "entrance":
		return a.Entrance
	case "floor":
		return a.Floor
	case "house":
		return a.House
	case "object":
		return a.Object
	case "okrug":
		return a.Okrug
	case "region":
		return a.Region
	case "settlement":
		return a.Settlement
	case "street":
		return a.Street
	case "structure":
		return a.Structure
	}
	return nil
}

// composeAddress — единая строка из структурных полей, если raw пуст (ручной ввод по частям).
func composeAddress(a *public.AddressDraft) string {
	var b strings.Builder
	add := func(prefix string, p *string) {
		s := strings.TrimSpace(strPtr(p))
		if s == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		b.WriteString(prefix)
		b.WriteString(s)
	}
	add("", a.Settlement)
	add("", a.Street)
	add("д. ", a.House)
	add("корп. ", a.Building)
	add("стр. ", a.Structure)
	return b.String()
}

// typesVal — типы происшествия. Сравниваем канонические id каталога: эталон из LLM мог
// сослаться на код типа, а не на id.
func typesVal(ids []string, cat core.Catalog) fval {
	if len(ids) == 0 {
		return emptyVal
	}
	set := make([]string, 0, len(ids))
	raw := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		raw = append(raw, id)
		key := id
		if cat != nil {
			if t, ok := cat.TypeByID(id); ok && t.ID != "" {
				key = t.ID
			} else if t, ok := cat.TypeByCode(id); ok && t.ID != "" {
				key = t.ID
			}
		}
		set = append(set, strings.ToLower(key))
	}
	set = sortUnique(set)
	if len(set) == 0 {
		return emptyVal
	}
	return fval{set: set, raw: raw}
}

// servicesVal — службы карточки как множество кодов (без учёта регистра). codes — эталонные
// коды вместо списка из карточки (spec.ServiceCodes).
func servicesVal(list []public.AssignedService, codes []string, cat core.Catalog) fval {
	if len(list) == 0 && len(codes) == 0 {
		return emptyVal
	}
	var set []string
	var raw any
	if len(list) > 0 {
		set = make([]string, 0, len(list))
		for i := range list {
			set = append(set, strings.ToLower(serviceCode(&list[i], cat)))
		}
		raw = list
	} else {
		set = make([]string, 0, len(codes))
		orig := make([]string, 0, len(codes))
		for _, c := range codes {
			if c = strings.TrimSpace(c); c != "" {
				set = append(set, strings.ToLower(c))
				orig = append(orig, c)
			}
		}
		raw = orig // коды; readable развернёт в краткие имена
	}
	set = sortUnique(set)
	if len(set) == 0 {
		return emptyVal
	}
	return fval{set: set, raw: raw}
}

// serviceCode — код службы как записан (для каталога); пустой код — по serviceId.
func serviceCode(s *public.AssignedService, cat core.Catalog) string {
	c := strings.TrimSpace(s.Code)
	if c == "" && s.ServiceId != "" {
		if cat != nil {
			if info, ok := cat.ServiceByID(s.ServiceId); ok {
				c = strings.TrimSpace(info.Code)
			}
		}
		if c == "" {
			c = strings.TrimSpace(s.ServiceId)
		}
	}
	return c
}

// attrVal — значение признака опросной карты: строка, число, флажок или список.
func attrVal(v any) fval {
	switch x := v.(type) {
	case nil:
		return emptyVal
	case string:
		return scalarVal(x)
	case bool:
		return boolVal(x)
	case float64:
		return numVal(x)
	case float32:
		return numVal(float64(x))
	case int:
		return numVal(float64(x))
	case int64:
		return numVal(float64(x))
	case int32:
		return numVal(float64(x))
	case json.Number:
		return scalarVal(x.String())
	case []string:
		set := make([]string, 0, len(x))
		for _, s := range x {
			set = append(set, normScalar(s))
		}
		if set = sortUnique(set); len(set) == 0 {
			return emptyVal
		}
		return fval{set: set, raw: x}
	case []any:
		set := make([]string, 0, len(x))
		for _, e := range x {
			if n, ok := elemNorm(e); ok {
				set = append(set, n)
			}
		}
		if set = sortUnique(set); len(set) == 0 {
			return emptyVal
		}
		return fval{set: set, raw: x}
	}
	// Объект или неизвестный тип — не значение признака.
	return emptyVal
}

func numVal(f float64) fval {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return emptyVal
	}
	return fval{set: []string{formatNum(f)}, raw: f}
}

func elemNorm(e any) (string, bool) {
	switch x := e.(type) {
	case string:
		n := normScalar(x)
		return n, n != ""
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return "", false
		}
		return formatNum(x), true
	case bool:
		if x {
			return "true", true
		}
		return "false", true
	case json.Number:
		n := normScalar(x.String())
		return n, n != ""
	case int:
		return strconv.Itoa(x), true
	}
	return "", false
}

// ---------------------------------------------------------------- сравнение

// compare — вердикт по полю: ok=true — ошибки нет.
func compare(cls valClass, exp, act fval, expStreet []string) (public.FieldErrorKind, bool) {
	if cls == clsFree {
		if act.empty {
			return public.FieldErrorKindMissing, false
		}
		return "", true
	}
	// Флажки: «нет» — тоже ответ. Семантика флажка — если обе стороны флажки или не заданы.
	if (exp.isBool || act.isBool) && (exp.isBool || exp.empty) && (act.isBool || act.empty) {
		if !exp.isBool {
			return "", true // эталон не задан — любой ответ верен
		}
		ab := act.isBool && act.b
		switch {
		case exp.b == ab:
			return "", true
		case exp.b:
			return public.FieldErrorKindMissing, false
		default:
			return public.FieldErrorKindWrong, false
		}
	}
	if act.empty {
		return public.FieldErrorKindMissing, false
	}
	if exp.empty {
		return "", true
	}
	switch cls {
	case clsServices:
		if !subset(exp.set, act.set) {
			return public.FieldErrorKindWrong, false
		}
		if len(act.set) > len(exp.set) {
			return public.FieldErrorKindExtra, false
		}
		return "", true
	case clsAddrRaw:
		if addressRawMatch(exp.set, act.set, expStreet) {
			return "", true
		}
		return public.FieldErrorKindWrong, false
	}
	if equalSets(exp.set, act.set) {
		return "", true
	}
	return public.FieldErrorKindWrong, false
}

func equalSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- читаемые значения

// readable — expected/actual для FieldError: коды → подписи (типы — названия, признаки —
// подписи значений, службы — краткие имена); пусто → nil (поле опускается в JSON).
func readable(cls valClass, path string, v fval, cat core.Catalog) any {
	if v.raw == nil {
		return nil
	}
	switch cls {
	case clsTypes:
		ids, _ := v.raw.([]string)
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			out = append(out, typeName(id, cat))
		}
		return out
	case clsServices:
		return serviceNames(v.raw, serviceNameIndex(v.raw, nil, cat), cat)
	case clsAttr:
		return attrReadable(path[len(attrPrefix):], v.raw, cat)
	}
	return v.raw
}

func typeName(id string, cat core.Catalog) string {
	if cat != nil {
		if t, ok := cat.TypeByID(id); ok && t.Name != "" {
			return t.Name
		}
		if t, ok := cat.TypeByCode(id); ok && t.Name != "" {
			return t.Name
		}
	}
	return id
}

// serviceNameIndex — краткое имя по коду (без учёта регистра): каталог, затем краткое/полное
// имя из любой из карточек, затем сам код.
func serviceNameIndex(a, b any, cat core.Catalog) map[string]string {
	idx := make(map[string]string, 8)
	fill := func(raw any) {
		switch x := raw.(type) {
		case []public.AssignedService:
			for i := range x {
				code := serviceCode(&x[i], cat)
				key := strings.ToLower(code)
				if cur, ok := idx[key]; ok && cur != code {
					continue // уже есть имя лучше кода
				}
				idx[key] = serviceName(code, x[i].ShortName, x[i].Name, cat)
			}
		case []string:
			for _, code := range x {
				key := strings.ToLower(code)
				if _, ok := idx[key]; !ok {
					idx[key] = serviceName(code, "", "", cat)
				}
			}
		}
	}
	fill(a)
	fill(b)
	return idx
}

// serviceNames — краткие имена служб без повторов, в порядке карточки.
func serviceNames(raw any, idx map[string]string, cat core.Catalog) any {
	var codes []string
	switch x := raw.(type) {
	case []public.AssignedService:
		codes = make([]string, 0, len(x))
		for i := range x {
			codes = append(codes, serviceCode(&x[i], cat))
		}
	case []string:
		codes = x
	default:
		return nil
	}
	out := make([]string, 0, len(codes))
	seen := make([]string, 0, len(codes))
	for _, code := range codes {
		key := strings.ToLower(code)
		if key == "" || slices.Contains(seen, key) {
			continue
		}
		seen = append(seen, key)
		if n, ok := idx[key]; ok && n != "" {
			out = append(out, n)
		} else {
			out = append(out, code)
		}
	}
	return out
}

// serviceName — краткое имя службы: каталог, затем то, что записано в карточке, затем код.
func serviceName(code, short, name string, cat core.Catalog) string {
	if cat != nil && code != "" {
		if s, ok := cat.ServiceByCode(code); ok {
			if s.ShortName != "" {
				return s.ShortName
			}
			if s.Name != "" {
				return s.Name
			}
		}
	}
	if s := strings.TrimSpace(short); s != "" {
		return s
	}
	if s := strings.TrimSpace(name); s != "" {
		return s
	}
	return code
}

func attrReadable(attr string, raw any, cat core.Catalog) any {
	switch x := raw.(type) {
	case string:
		return attrValueLabel(attr, x, cat)
	case []string:
		out := make([]string, 0, len(x))
		for _, s := range x {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, attrValueLabel(attr, s, cat))
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			switch v := e.(type) {
			case string:
				if v = strings.TrimSpace(v); v != "" {
					out = append(out, attrValueLabel(attr, v, cat))
				}
			case float64:
				out = append(out, formatNum(v))
			case bool:
				if v {
					out = append(out, "да")
				} else {
					out = append(out, "нет")
				}
			case json.Number:
				out = append(out, v.String())
			}
		}
		return out
	}
	return raw // число / флажок — как есть (UI рисует «да/нет»)
}

func attrValueLabel(attr, code string, cat core.Catalog) string {
	if cat != nil {
		if l, ok := cat.AttributeValueLabel(attr, code); ok && l != "" {
			return l
		}
	}
	return code
}

func clamp100(x float64) float64 {
	switch {
	case math.IsNaN(x):
		return 0
	case x < 0:
		return 0
	case x > 100:
		return 100
	}
	return x
}
