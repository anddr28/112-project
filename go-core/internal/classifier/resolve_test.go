package classifier

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/reaction"
)

// mockResolve — фронтовой resolveServices (fixtures/classifier.ts) один в один: правила в
// глобальном порядке RULES, первичность «залипает», причина — от первого правила.
func mockResolve(typeCodes []string, attrs map[string]any) []resolvedRef {
	sel := map[string]bool{}
	for _, c := range typeCodes {
		sel[c] = true
	}
	var out []resolvedRef
	idx := map[string]int{}
	for _, r := range data().classifier.Rules {
		if !sel[r.Type] {
			continue
		}
		if r.Attr != "" {
			var selected []string
			switch v := attrs[r.Attr].(type) {
			case nil:
			case []any:
				for _, x := range v {
					if s, ok := x.(string); ok {
						selected = append(selected, s)
					}
				}
			default:
				selected = []string{fmt.Sprint(v)}
			}
			matched := false
			for _, code := range r.AnyOf {
				if slices.Contains(selected, code) {
					matched = true
				}
			}
			if !matched {
				continue
			}
		}
		for _, s := range r.Services {
			if i, ok := idx[s.Code]; ok {
				if s.Primary {
					out[i].isPrimary = true
				}
				continue
			}
			idx[s.Code] = len(out)
			out = append(out, resolvedRef{code: s.Code, isPrimary: s.Primary, reason: s.Reason})
		}
	}
	return out
}

func fixtureAlias(code string) string {
	for _, t := range data().classifier.Types {
		if t.Code == code {
			return t.FixtureID
		}
	}
	return ""
}

func refsOf(l []public.AssignedService) []resolvedRef {
	out := make([]resolvedRef, 0, len(l))
	for _, s := range l {
		r := resolvedRef{code: s.Code, isPrimary: s.IsPrimary}
		if s.Reason != nil {
			r.reason = *s.Reason
		}
		out = append(out, r)
	}
	return out
}

var t0 = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

// Порядок служб, первичность и причины совпадают с фронтовым моком на любых комбинациях
// типов и признаков фикстур.
func TestResolveMatchesFrontendMock(t *testing.T) {
	t.Parallel()
	c := fixtureCatalog(t)
	emb := data()
	rng := rand.New(rand.NewPCG(1, 2))
	allCodes := make([]string, 0, len(emb.classifier.Types))
	optionsOf := map[string][]string{}
	for _, ty := range emb.classifier.Types {
		allCodes = append(allCodes, ty.Code)
		for _, g := range ty.Attributes {
			if g.Options != nil {
				for _, o := range *g.Options {
					optionsOf[g.Code] = append(optionsOf[g.Code], o.Code)
				}
			}
		}
	}
	for i := range 500 {
		n := 1 + rng.IntN(4)
		var codes []string
		for range n {
			codes = append(codes, allCodes[rng.IntN(len(allCodes))])
		}
		attrs := map[string]any{}
		for attr, opts := range optionsOf {
			switch rng.IntN(4) {
			case 0: // не выбран
			case 1:
				attrs[attr] = opts[rng.IntN(len(opts))]
			case 2:
				attrs[attr] = []any{opts[rng.IntN(len(opts))], opts[rng.IntN(len(opts))]}
			case 3:
				attrs[attr] = nil
			}
		}
		ids := make([]string, 0, len(codes))
		for j, code := range codes { // ключи типов — вперемешку uuid / код / id фикстуры
			switch j % 3 {
			case 0:
				ids = append(ids, fixtureAlias(code))
			case 1:
				ids = append(ids, typeUUID(code).String())
			default:
				ids = append(ids, code)
			}
		}
		res := c.resolveAt(public.ResolveServicesRequest{TypeIds: ids, Attributes: attrs, AddressFilled: true,
			AddressSource: ptr(public.AddressSourceYandexMap), Current: []public.AssignedService{}}, t0)
		want := mockResolve(codes, attrs)
		if got := refsOf(res.Services); !slices.Equal(got, want) {
			t.Fatalf("case %d types %v attrs %v:\n got %+v\nwant %+v", i, codes, attrs, got, want)
		}
		if res.SuppressedByAddressSource {
			t.Fatal("suppressed without fias")
		}
		for _, s := range res.Services {
			if s.Source != public.AssignedServiceSourceAuto || s.CurrentStatus != reaction.StatusReceived ||
				len(s.History) != 2 || s.AllowedNext == nil || !s.Editable || s.ServiceId != serviceUUID(s.Code).String() {
				t.Fatalf("new service shape: %+v", s)
			}
		}
	}
}

func TestResolveFireCase(t *testing.T) {
	t.Parallel()
	c := fixtureCatalog(t)
	res := c.resolveAt(public.ResolveServicesRequest{
		TypeIds:       []string{"it-101"},
		Attributes:    map[string]any{"people_threat": "yes", "indoor_objects": []any{"gas_stove", "elevator"}, "floors": 9.0},
		AddressFilled: true,
	}, t0)
	got := refsOf(res.Services)
	var codes []string
	for _, r := range got {
		codes = append(codes, r.code)
	}
	// основные: 101 (тип), 103 (угроза людям), 104 (газовое оборудование); лифт в подъезде — Мослифт
	if want := []string{"101", "zhkh", "103", "cemp", "104", "moslift"}; !slices.Equal(codes, want) {
		t.Fatalf("codes %v, want %v", codes, want)
	}
	if want := mockResolve([]string{"101"}, map[string]any{"people_threat": "yes",
		"indoor_objects": []any{"gas_stove", "elevator"}}); !slices.Equal(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	for _, r := range got {
		if r.isPrimary != (r.code == "101" || r.code == "103" || r.code == "104") || r.reason == "" {
			t.Errorf("%+v", r)
		}
	}
	// 103 из правила: без «Не принята»
	for _, s := range res.Services {
		if s.Code == "103" && len(s.AllowedNext) != 1 {
			t.Errorf("103 allowedNext: %+v", s.AllowedNext)
		}
	}
}

func TestResolveNoServices(t *testing.T) {
	t.Parallel()
	c := fixtureCatalog(t)
	manual := reaction.NewAssigned(mustService(t, c, "oati"), reaction.SourceManual, false, "добавлена оператором вручную", t0)
	auto := reaction.NewAssigned(mustService(t, c, "101"), reaction.SourceAuto, true, "тип происшествия: пожар", t0)
	fias := public.AddressSourceFias
	tests := []struct {
		name       string
		req        public.ResolveServicesRequest
		suppressed bool
	}{
		{"no types", public.ResolveServicesRequest{AddressFilled: true}, false},
		{"address not filled", public.ResolveServicesRequest{TypeIds: []string{"it-101"}}, false},
		{"fias", public.ResolveServicesRequest{TypeIds: []string{"it-101"}, AddressFilled: true, AddressSource: &fias}, true},
		{"fias, address not filled", public.ResolveServicesRequest{TypeIds: []string{"it-101"}, AddressSource: &fias}, false},
		{"unknown type", public.ResolveServicesRequest{TypeIds: []string{"нет-такого"}, AddressFilled: true}, false},
		{"type without rules", public.ResolveServicesRequest{TypeIds: []string{"it-cancel"}, AddressFilled: true}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := c.resolveAt(tc.req, t0)
			if res.Services == nil || len(res.Services) != 0 || res.SuppressedByAddressSource != tc.suppressed {
				t.Errorf("empty current: %+v", res)
			}
			// ручная служба остаётся, авто-служба в начальном статусе снимается
			tc.req.Current = []public.AssignedService{auto, manual}
			res = c.resolveAt(tc.req, t0)
			if got := refsOf(res.Services); len(got) != 1 || got[0].code != "oati" {
				t.Errorf("with current: %+v", got)
			}
		})
	}
}

func mustService(t *testing.T, c *Catalog, code string) core.ServiceInfo {
	t.Helper()
	s, ok := c.ServiceByCode(code)
	if !ok {
		t.Fatalf("service %s", code)
	}
	return s
}

func TestResolveMergesWithCurrent(t *testing.T) {
	t.Parallel()
	c := fixtureCatalog(t)
	earlier := t0.Add(-5 * time.Minute)

	// 101 уже назначена и принята: статус и история сохраняются, причина прежняя
	fire := reaction.NewAssigned(mustService(t, c, "101"), reaction.SourceAuto, false, "", earlier)
	if err := reaction.Transition(&fire, reaction.StatusAccepted, "", "", "оп. 1", earlier.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	fire.Name, fire.ShortName, fire.ServiceId = "подделано клиентом", "x", "не тот id"
	fire.AllowedNext = []public.AllowedTransition{} // клиент прислал мусор — сервер пересчитает
	// zhkh — вручную, со своей причиной
	zhkh := reaction.NewAssigned(mustService(t, c, "zhkh"), reaction.SourceManual, false, "добавлена оператором вручную", earlier)
	// codd — авто, в реагировании, но правило больше не срабатывает: не снимается
	codd := reaction.NewAssigned(mustService(t, c, "codd"), reaction.SourceAuto, false, "признак: перекрытие движения", earlier)
	if err := reaction.Transition(&codd, reaction.StatusAccepted, "", "", "оп. 1", earlier); err != nil {
		t.Fatal(err)
	}
	// oek — авто в начальном статусе, правило не срабатывает: снимается
	oek := reaction.NewAssigned(mustService(t, c, "oek"), reaction.SourceAuto, false, "признак: электрооборудование", earlier)
	// неизвестная службе справочника ручная служба остаётся как есть
	alien := public.AssignedService{Code: "alien", Name: "Чужая", ShortName: "Ч", Source: public.AssignedServiceSourceManual,
		CurrentStatus: reaction.StatusReceived, CurrentStatusAt: earlier}

	res := c.resolveAt(public.ResolveServicesRequest{
		TypeIds:       []string{"it-101", "it-101"}, // дубликат типа
		Attributes:    map[string]any{},
		AddressFilled: true,
		Current:       []public.AssignedService{fire, zhkh, codd, oek, alien, fire /* дубликат */},
	}, t0)
	got := refsOf(res.Services)
	wantCodes := []string{"101", "zhkh", "codd", "alien"}
	var codes []string
	for _, r := range got {
		codes = append(codes, r.code)
	}
	if !slices.Equal(codes, wantCodes) {
		t.Fatalf("codes %v, want %v", codes, wantCodes)
	}
	f := res.Services[0]
	if f.CurrentStatus != reaction.StatusAccepted || len(f.History) != 3 || !f.IsPrimary {
		t.Errorf("101 must keep status/history and become primary: %+v", f)
	}
	if f.Reason == nil || *f.Reason != "тип происшествия: пожар" {
		t.Errorf("101 reason from the rule when none was set: %v", f.Reason)
	}
	if f.Name == "подделано клиентом" || f.ServiceId != serviceUUID("101").String() || f.ShortName != "Служба 101" {
		t.Errorf("reference fields must come from the catalog: %+v", f)
	}
	if len(f.AllowedNext) != 5 {
		t.Errorf("allowedNext recomputed from «Принята»: %+v", f.AllowedNext)
	}
	z := res.Services[1]
	if z.Source != public.AssignedServiceSourceManual || z.Reason == nil || *z.Reason != "добавлена оператором вручную" || z.IsPrimary {
		t.Errorf("zhkh (rule: not primary) keeps manual source and reason: %+v", z)
	}
	if res.Services[2].CurrentStatus != reaction.StatusAccepted {
		t.Errorf("codd in reaction kept as is: %+v", res.Services[2])
	}
	a := res.Services[3]
	if a.History == nil || a.AllowedNext == nil || !a.Editable || a.Name != "Чужая" {
		t.Errorf("alien service normalized: %+v", a)
	}
}

func TestResolveSkipsUnknownAndInactiveServices(t *testing.T) {
	t.Parallel()
	types, services := fixtureRaw(t)
	for i := range services {
		if services[i].code == "zhkh" {
			services[i].active = false
		}
	}
	// правило ссылается на службу, которой нет в справочнике
	types = append(types, rawType{id: typeUUID("x"), code: "x", name: "X", depth: 1, active: true, rulesRank: -1,
		sortOrder: 999, serviceRules: []byte(`[{"services":[{"code":"ghost","primary":true,"reason":"r"},{"code":"102","primary":false,"reason":"тип X"}]}]`)})
	c := catalogFrom(types, services)
	res := c.resolveAt(public.ResolveServicesRequest{TypeIds: []string{"x", "it-101"}, AddressFilled: true}, t0)
	var codes []string
	for _, s := range res.Services {
		codes = append(codes, s.Code)
	}
	// типы с правилами фикстуры — раньше импортированных; выключенная и неизвестная службы — нет
	if !slices.Equal(codes, []string{"101", "102"}) {
		t.Errorf("codes %v", codes)
	}
}

func TestAttrMatches(t *testing.T) {
	t.Parallel()
	anyOf := []string{"yes", "gas_stove", "true", "5"}
	for _, tc := range []struct {
		v    any
		want bool
	}{
		{nil, false},
		{"yes", true},
		{"no", false},
		{"", false},
		{[]any{"no", "gas_stove"}, true},
		{[]any{"no", 5.0, true}, false}, // в массиве сравниваются только строки (как includes во фронте)
		{[]any{}, false},
		{[]string{"x", "yes"}, true},
		{[]string{}, false},
		{true, true},
		{false, false},
		{5.0, true},
		{5.5, false},
		{map[string]any{"yes": true}, false},
		{42, false}, // не из JSON
	} {
		if got := attrMatches(tc.v, anyOf); got != tc.want {
			t.Errorf("attrMatches(%#v) = %v, want %v", tc.v, got, tc.want)
		}
	}
	if attrMatches("yes", nil) {
		t.Error("empty anyOf never matches")
	}
}
