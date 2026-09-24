package classifier

import (
	"sort"
	"strconv"
	"time"

	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/reaction"
)

// ResolveServices — определение служб по типам, признакам и адресу (GAP-21).
// Зеркало resolveServices из mockApi.ts + fixtures/classifier.ts:
//   - нет типов или адрес не заполнен -> служб нет;
//   - адрес из ФИАС -> службы не подставляются (Инструкция п.4.3), suppressedByAddressSource
//     = addressFilled && source == fias (как в моке);
//   - правила выбранных типов: без attr — всегда, с attr — если значение признака входит в any_of;
//     первичность «залипает» (хоть одно правило назвало службу основной), причина — от первого;
//   - слияние с current: уже назначенная служба сохраняет статус и историю (первичность
//     «залипает», причина прежняя), новые — reaction.NewAssigned(source=auto), ручные
//     остаются как есть.
//
// Уточнение сервера (в моке нет): служба, которая уже приступила к реагированию
// (статус дальше «Получена службой»), не исчезает при смене типа/признаков — снять её
// нельзя и вручную (409), иначе потерялась бы история статусов.
func (c *Catalog) ResolveServices(req public.ResolveServicesRequest) public.ResolvedServicesResult {
	return c.resolveAt(req, time.Now())
}

type resolvedRef struct {
	code      string
	isPrimary bool
	reason    string
}

func (c *Catalog) resolveAt(req public.ResolveServicesRequest, now time.Time) public.ResolvedServicesResult {
	s := c.s()
	fias := req.AddressSource != nil && *req.AddressSource == public.AddressSourceFias
	res := public.ResolvedServicesResult{
		Services:                  make([]public.AssignedService, 0, 8),
		SuppressedByAddressSource: req.AddressFilled && fias,
	}

	var resolved []resolvedRef
	if len(req.TypeIds) > 0 && req.AddressFilled && !fias {
		resolved = s.resolveRules(req.TypeIds, req.Attributes)
	}

	// current: первое вхождение по коду (дубликаты от клиента игнорируем)
	byCode := make(map[string]int, len(req.Current))
	for i := range req.Current {
		if _, ok := byCode[req.Current[i].Code]; !ok {
			byCode[req.Current[i].Code] = i
		}
	}
	taken := make(map[string]struct{}, len(resolved)+len(req.Current))

	for _, r := range resolved {
		if i, ok := byCode[r.code]; ok {
			as := req.Current[i]
			as.IsPrimary = as.IsPrimary || r.isPrimary
			if as.Reason == nil && r.reason != "" {
				reason := r.reason
				as.Reason = &reason
			}
			s.refreshService(&as)
			reaction.Normalize(&as)
			res.Services = append(res.Services, as)
			taken[r.code] = struct{}{}
			continue
		}
		svc := s.svcByCode[r.code]
		if svc == nil || !svc.active {
			continue // правило ссылается на неизвестную/выключенную службу — не назначаем
		}
		res.Services = append(res.Services, reaction.NewAssigned(svc.info, reaction.SourceAuto, r.isPrimary, r.reason, now))
		taken[r.code] = struct{}{}
	}

	// Ручные службы оператор добавил осознанно — автоопределение их не трогает;
	// службы в реагировании не снимаются (см. выше).
	for i := range req.Current {
		as := req.Current[i]
		if _, ok := taken[as.Code]; ok {
			continue
		}
		if string(as.Source) != reaction.SourceManual && reaction.CanRemove(&as) {
			continue
		}
		s.refreshService(&as)
		reaction.Normalize(&as)
		res.Services = append(res.Services, as)
		taken[as.Code] = struct{}{}
	}
	return res
}

// refreshService — справочные поля службы берём из каталога, а не из тела запроса.
func (s *snapshot) refreshService(as *public.AssignedService) {
	if svc := s.svcByCode[as.Code]; svc != nil {
		as.ServiceId = svc.info.ID
		as.Name = svc.info.Name
		as.ShortName = svc.info.ShortName
	}
}

// resolveRules — службы по правилам выбранных типов. Типы обходятся в порядке правил
// фикстуры (rules_rank), затем в порядке классификатора — так порядок служб и причины
// совпадают с фронтовым RULES.
func (s *snapshot) resolveRules(typeIDs []string, attrs map[string]any) []resolvedRef {
	sel := make([]*typeEntry, 0, len(typeIDs))
	seen := make(map[*typeEntry]struct{}, len(typeIDs))
	for _, id := range typeIDs {
		e := s.lookupType(id)
		if e == nil {
			continue
		}
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		sel = append(sel, e)
	}
	sort.SliceStable(sel, func(i, j int) bool {
		a, b := sel[i], sel[j]
		ra, rb := a.rulesRank, b.rulesRank
		if (ra < 0) != (rb < 0) {
			return ra >= 0 // типы с правилами фикстуры — первыми
		}
		if ra != rb {
			return ra < rb
		}
		return a.sortOrder < b.sortOrder
	})

	out := make([]resolvedRef, 0, 8)
	idx := make(map[string]int, 8)
	for _, e := range sel {
		for _, rule := range e.rules {
			if rule.Attr != "" && !attrMatches(attrs[rule.Attr], rule.AnyOf) {
				continue
			}
			for _, sv := range rule.Services {
				if i, ok := idx[sv.Code]; ok {
					if sv.Primary {
						out[i].isPrimary = true
					}
					continue
				}
				idx[sv.Code] = len(out)
				out = append(out, resolvedRef{code: sv.Code, isPrimary: sv.Primary, reason: sv.Reason})
			}
		}
	}
	return out
}

// attrMatches — значение признака (строка, массив, bool, число) содержит один из кодов.
// Как во фронте: массив — как есть, null — пусто, остальное — String(value).
func attrMatches(v any, anyOf []string) bool {
	if v == nil || len(anyOf) == 0 {
		return false
	}
	has := func(x string) bool {
		for _, c := range anyOf {
			if c == x {
				return true
			}
		}
		return false
	}
	switch t := v.(type) {
	case string:
		return has(t)
	case []any:
		for _, x := range t {
			if xs, ok := x.(string); ok && has(xs) {
				return true
			}
		}
		return false
	case []string:
		for _, x := range t {
			if has(x) {
				return true
			}
		}
		return false
	case bool:
		return has(strconv.FormatBool(t))
	case float64:
		return has(strconv.FormatFloat(t, 'f', -1, 64))
	default:
		return false
	}
}
