package classifier

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"lct/gocore/internal/gen/public"
)

func TestParseAddrQuery(t *testing.T) {
	t.Parallel()
	tests := []struct {
		q    string
		want addrQuery
	}{
		{"Тверская 12", addrQuery{words: []string{"тверская"}, house: "12"}},
		{"ленинский пр-т 32а", addrQuery{words: []string{"ленинский"}, kind: "проспект", house: "32А"}},
		{"Ленинский пр-кт 32", addrQuery{words: []string{"ленинский"}, kind: "проспект", house: "32"}},
		{"Ленинский пр. 32", addrQuery{words: []string{"ленинский", "пр"}, house: "32"}},
		{"профсоюзная 45 к2", addrQuery{words: []string{"профсоюзная"}, house: "45", building: "2"}},
		{"Профсоюзная 45к2", addrQuery{words: []string{"профсоюзная"}, house: "45", building: "2"}},
		{"Профсоюзная 45 корп 2", addrQuery{words: []string{"профсоюзная"}, house: "45", building: "2"}},
		{"Тверская 12 стр 1", addrQuery{words: []string{"тверская"}, house: "12", structure: "1"}},
		{"Тверская 12с1", addrQuery{words: []string{"тверская"}, house: "12", structure: "1"}},
		{"Тверская, д. 12, стр1", addrQuery{words: []string{"тверская"}, house: "12", structure: "1"}},
		{"Москва, ул. Тверская, д. 5", addrQuery{words: []string{"тверская"}, kind: "улица", house: "5"}},
		{"1-я Тверская-Ямская 5", addrQuery{words: []string{"1", "тверская", "ямская"}, house: "5"}},
		{"26 Бакинских Комиссаров 3", addrQuery{words: []string{"26", "бакинских", "комиссаров"}, house: "3"}},
		{"Тверская 12 а", addrQuery{words: []string{"тверская"}, house: "12А"}},
		{"Тверская 12 кв 5 подъезд 2", addrQuery{words: []string{"тверская"}, house: "12"}},
		{"проспект Мира 5", addrQuery{words: []string{"мира"}, kind: "проспект", house: "5"}},
		{"ЁЛОЧНАЯ", addrQuery{words: []string{"елочная"}}},
		{"пер", addrQuery{words: []string{"пер"}}}, // одно слово-тип — оператор ещё печатает название
		{"ул", addrQuery{words: []string{"ул"}}},
		{"Москва", addrQuery{}},
		{"Москва, д. 5", addrQuery{words: []string{"5"}}},
		// регрессия: дробь номера дома терялась («Тверская 12/1» -> дом 12)
		{"Тверская 12/1", addrQuery{words: []string{"тверская"}, house: "12/1"}},
		{"Тверская 12/1к2", addrQuery{words: []string{"тверская"}, house: "12/1", building: "2"}},
		{"Тверская 12/1 а", addrQuery{words: []string{"тверская"}, house: "12/1А"}},
	}
	for _, tc := range tests {
		got := parseAddrQuery(tc.q)
		if !slices.Equal(got.words, tc.want.words) || got.kind != tc.want.kind || got.house != tc.want.house ||
			got.building != tc.want.building || got.structure != tc.want.structure {
			t.Errorf("parseAddrQuery(%q) = %+v, want %+v", tc.q, got, tc.want)
		}
	}
}

func TestSplitHouse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, house, building, structure string }{
		{"12", "12", "", ""},
		{"12к2", "12", "2", ""},
		{"12корп3", "12", "3", ""},
		{"12корпус3", "12", "3", ""},
		{"12с1", "12", "", "1"},
		{"12стр4", "12", "", "4"},
		{"32а", "32А", "", ""},
		{"12к", "12К", "", ""},
		{"12/1", "12/1", "", ""},
		{"12/1к2", "12/1", "2", ""},
		{"12/1с3", "12/1", "", "3"},
		{"12/1а", "12/1А", "", ""},
	} {
		h, b, s := splitHouse(tc.in)
		if h != tc.house || b != tc.building || s != tc.structure {
			t.Errorf("splitHouse(%q) = (%q, %q, %q), want (%q, %q, %q)", tc.in, h, b, s, tc.house, tc.building, tc.structure)
		}
	}
}

func TestAddrTokens(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"Тверская 12/1", []string{"тверская", "12/1"}},
		{"Тверская 12 / 1", []string{"тверская", "12", "1"}}, // дробь только слитно
		{"Тверская/Ямская", []string{"тверская", "ямская"}},
		{"/12/", []string{"12"}},
		{"12//1", []string{"12", "1"}},
		{"Ёлочная, 5", []string{"елочная", "5"}},
	} {
		got := addrTokens(tc.in)
		if !slices.Equal(got, tc.want) {
			t.Errorf("addrTokens(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
		// без дробей — то же, что tokens
		if !strings.Contains(tc.in, "/") && !slices.Equal(got, tokens(tc.in)) {
			t.Errorf("addrTokens(%q) differs from tokens: %#v vs %#v", tc.in, got, tokens(tc.in))
		}
	}
}

func TestStreetField(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ full, field, kind string }{
		{"Тверская улица", "Тверская", "улица"},
		{"улица Ильинка", "Ильинка", "улица"},
		{"Ленинский проспект", "Ленинский проспект", "проспект"},
		{"проспект Мира", "проспект Мира", "проспект"},
		{"Каширское шоссе", "Каширское шоссе", "шоссе"},
		{"Садовая-Кудринская улица", "Садовая-Кудринская", "улица"},
		{"Охотный Ряд", "Охотный Ряд", ""},
		{"улица", "улица", "улица"},
	} {
		field, kind := streetField(tc.full)
		if field != tc.field || kind != tc.kind {
			t.Errorf("streetField(%q) = (%q, %q), want (%q, %q)", tc.full, field, kind, tc.field, tc.kind)
		}
	}
}

func labels(l []public.AddressSuggestion) []string {
	out := make([]string, 0, len(l))
	for _, s := range l {
		out = append(out, s.Label+" |"+string(s.Source))
	}
	return out
}

func checkSuggestionInvariants(t *testing.T, q string, l []public.AddressSuggestion) {
	t.Helper()
	seenKey := map[string]bool{}
	seenID := map[string]bool{}
	for _, s := range l {
		key := s.Label + "|" + string(s.Source)
		if seenKey[key] {
			t.Errorf("%q: duplicate suggestion %s", q, key)
		}
		seenKey[key] = true
		if s.Id == "" || seenID[s.Id] {
			t.Errorf("%q: empty or duplicate id %q", q, s.Id)
		}
		seenID[s.Id] = true
		if !s.Source.Valid() {
			t.Errorf("%q: invalid source %q", q, s.Source)
		}
		v := s.Value
		if v.Raw != s.Label {
			t.Errorf("%q: value.raw %q != label %q", q, v.Raw, s.Label)
		}
		if v.Source == nil || *v.Source != s.Source {
			t.Errorf("%q: value.source %v != %q", q, v.Source, s.Source)
		}
		// фронт подставляет value целиком и читает строки без проверок на undefined
		for name, p := range map[string]*string{"country": v.Country, "region": v.Region, "settlement": v.Settlement,
			"object": v.Object, "okrug": v.Okrug, "district": v.District, "street": v.Street, "house": v.House,
			"building": v.Building, "structure": v.Structure, "apartment": v.Apartment, "entrance": v.Entrance,
			"floor": v.Floor, "code": v.Code, "descriptive": v.Descriptive} {
			if p == nil {
				t.Errorf("%q: value.%s is nil", q, name)
			}
		}
		if v.Lat == nil || v.Lon == nil || *v.Lat < 55 || *v.Lat > 56.5 || *v.Lon < 36.5 || *v.Lon > 38.5 {
			t.Errorf("%q: coordinates outside Moscow: %v %v", q, v.Lat, v.Lon)
		}
		if *v.Country != "Россия" || *v.Settlement != "Москва" || *v.Region != "Москва" {
			t.Errorf("%q: country/region/settlement %q %q %q", q, *v.Country, *v.Region, *v.Settlement)
		}
		if !strings.HasPrefix(s.Label, "Москва, ") {
			t.Errorf("%q: label %q", q, s.Label)
		}
	}
}

func TestSuggestShortAndNoise(t *testing.T) {
	t.Parallel()
	for _, q := range []string{"", " ", "Т", "  т  ", "Москва", "г. Москва, д.", ",,"} {
		got := Suggest(q, 10)
		if got == nil || len(got) != 0 {
			t.Errorf("Suggest(%q) = %v, want [] (non-nil)", q, labels(got))
		}
		b, _ := json.Marshal(got)
		if string(b) != "[]" {
			t.Errorf("Suggest(%q) JSON = %s", q, b)
		}
	}
}

func TestSuggestKnownAddresses(t *testing.T) {
	t.Parallel()
	got := Suggest("Тверская 12", 10)
	checkSuggestionInvariants(t, "Тверская 12", got)
	if len(got) < 2 {
		t.Fatalf("too few suggestions: %v", labels(got))
	}
	// точные адреса справочника — первыми, со своими источниками
	if got[0].Label != "Москва, Тверская улица, 12" || got[0].Source != public.AddressSourceYandexMap {
		t.Errorf("first = %s", labels(got)[0])
	}
	if got[1].Label != "Москва, Тверская улица, 12, строение 2" || got[1].Source != public.AddressSourceYandexOrg {
		t.Errorf("second = %s", labels(got)[1])
	}
	v := got[0].Value
	if *v.Street != "Тверская" || *v.House != "12" || *v.Okrug != "ЦАО" || *v.District != "Тверской" ||
		*v.Building != "" || *v.Structure != "" {
		t.Errorf("value of the first: %+v", v)
	}
	if *got[1].Value.Structure != "2" || *got[1].Value.Building != "" {
		t.Errorf("строение 2 goes to structure: building %q structure %q", *got[1].Value.Building, *got[1].Value.Structure)
	}
	// дом 21 той же улицы не подходит к «12»
	for _, l := range labels(got) {
		if strings.Contains(l, ", 21") {
			t.Errorf("house 21 suggested for «12»: %s", l)
		}
	}
	// регистр и лишние пробелы не важны
	if other := Suggest("  ТВЕРСКАЯ   12 ", 10); !slices.Equal(labels(other), labels(got)) {
		t.Errorf("case-insensitive: %v vs %v", labels(other), labels(got))
	}
}

func TestSuggestFIASAndBuildings(t *testing.T) {
	t.Parallel()
	got := Suggest("Профсоюзная 45", 10)
	checkSuggestionInvariants(t, "Профсоюзная 45", got)
	l := labels(got)
	for _, want := range []string{
		"Москва, Профсоюзная улица, 45, корпус 2 |yandex_map",
		"Москва, Профсоюзная улица, 45 |fias", // правило «ФИАС не подставляет службы» можно отработать
	} {
		if !slices.Contains(l, want) {
			t.Errorf("missing %q in %v", want, l)
		}
	}

	// корпус в запросе отсекает адреса без корпуса; построенный вариант совпадает с точным -> один
	got = Suggest("профсоюзная 45 к2", 10)
	checkSuggestionInvariants(t, "профсоюзная 45 к2", got)
	if l := labels(got); len(l) != 1 || l[0] != "Москва, Профсоюзная улица, 45, корпус 2 |yandex_map" {
		t.Errorf("with building: %v", l)
	}
	if *got[0].Value.Building != "2" || *got[0].Value.House != "45" {
		t.Errorf("building value: %+v", got[0].Value)
	}

	// построенный вариант: дом, корпус, строение в подписи и полях
	got = Suggest("Тверская 7 к3 с1", 10)
	checkSuggestionInvariants(t, "Тверская 7 к3 с1", got)
	if len(got) == 0 || got[0].Label != "Москва, Тверская улица, 7, корпус 3, строение 1" {
		t.Fatalf("built: %v", labels(got))
	}
	v := got[0].Value
	if *v.House != "7" || *v.Building != "3" || *v.Structure != "1" || *v.Street != "Тверская" || got[0].Source != public.AddressSourceYandexMap {
		t.Errorf("built value: %+v", v)
	}
}

func TestSuggestRankingAndDedup(t *testing.T) {
	t.Parallel()
	got := Suggest("Тверская", 30)
	checkSuggestionInvariants(t, "Тверская", got)
	if len(got) == 0 || !strings.HasPrefix(got[0].Label, "Москва, Тверская улица") {
		t.Fatalf("«Тверская улица» must rank first: %v", labels(got))
	}
	// Ленинский проспект лежит в справочнике двумя участками — вариант без дома один
	got = Suggest("Ленинский проспект", 30)
	checkSuggestionInvariants(t, "Ленинский проспект", got)
	n := 0
	for _, s := range got {
		if s.Label == "Москва, Ленинский проспект" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("street without house suggested %d times: %v", n, labels(got))
	}
	// улица без типа в названии
	got = Suggest("Охотный ряд 2", 10)
	checkSuggestionInvariants(t, "Охотный ряд 2", got)
	if len(got) == 0 || got[0].Label != "Москва, Охотный Ряд, 2" || got[0].Source != public.AddressSourceYandexOrg {
		t.Errorf("Охотный Ряд: %v", labels(got))
	}
	// «ё» в запросе и в названии
	a, b := Suggest("Кремлевская наб", 5), Suggest("Кремлёвская набережная", 5)
	if len(a) == 0 || !slices.Equal(labels(a), labels(b)) {
		t.Errorf("ё-insensitive: %v vs %v", labels(a), labels(b))
	}
}

func TestSuggestLimit(t *testing.T) {
	t.Parallel()
	if got := Suggest("ул", 3); len(got) != 3 {
		t.Errorf("limit 3: %d", len(got))
	}
	if got := Suggest("ул", 0); len(got) != SuggestDefaultLimit {
		t.Errorf("limit 0 -> default: %d", len(got))
	}
	if got := Suggest("ул", -5); len(got) != SuggestDefaultLimit {
		t.Errorf("negative limit -> default: %d", len(got))
	}
	if got := Suggest("ул", 1000); len(got) != SuggestMaxLimit {
		t.Errorf("limit clamped to %d: %d", SuggestMaxLimit, len(got))
	}
}

// Регрессия: слово-тип, набранное не до конца или сокращением «пр», обнуляло подсказки
// (фронт запрашивает подсказки на каждый ввод, и список пропадал посреди набора «улица»).
func TestSuggestPartialStreetType(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ q, first string }{
		{"Тверская ули", "Москва, Тверская улица"},
		{"Тверская улиц", "Москва, Тверская улица"},
		{"Ленинский пр 32", "Москва, Ленинский проспект, 32"},
		{"Ленинский пр. 32", "Москва, Ленинский проспект, 32"},
		{"Ленинский проспе 32", "Москва, Ленинский проспект, 32"},
		{"Ленинский пр-кт 32", "Москва, Ленинский проспект, 32"},
		{"пр Мира 5", "Москва, проспект Мира, 5"},
		{"Каширское шос 26", "Москва, Каширское шоссе, 26"},
	} {
		got := Suggest(tc.q, 10)
		checkSuggestionInvariants(t, tc.q, got)
		if len(got) == 0 || !strings.HasPrefix(got[0].Label, tc.first) {
			t.Errorf("Suggest(%q) = %v, want first %q…", tc.q, labels(got), tc.first)
		}
	}
	// недописанный тип фильтрует: площадь не подходит к «ули»
	for _, l := range labels(Suggest("Тверская ули", 30)) {
		if strings.Contains(l, "площадь") || strings.Contains(l, "бульвар") {
			t.Errorf("«Тверская ули» suggested %s", l)
		}
	}
	// одно сокращение — совпадения по названию раньше совпадений по типу
	got := Suggest("пер", 30)
	if len(got) == 0 {
		t.Fatal("«пер» must suggest something")
	}
	if l := normalize(got[0].Label); !strings.Contains(l, " пер") {
		t.Errorf("«пер»: first %q", got[0].Label)
	}
}

// Регрессия: дробь в номере дома отбрасывалась — в карточку уходил дом «12» вместо «12/1».
func TestSuggestFractionHouse(t *testing.T) {
	t.Parallel()
	got := Suggest("Тверская 12/1", 10)
	checkSuggestionInvariants(t, "Тверская 12/1", got)
	if len(got) == 0 || got[0].Label != "Москва, Тверская улица, 12/1" || *got[0].Value.House != "12/1" {
		t.Fatalf("Suggest(«Тверская 12/1») = %v", labels(got))
	}
	for _, s := range got {
		if *s.Value.House == "12" {
			t.Errorf("house 12 suggested for 12/1: %s", s.Label)
		}
	}
}

func TestStreetIndexLoaded(t *testing.T) {
	t.Parallel()
	ix := data().streets
	if len(ix.streets) < 500 {
		t.Errorf("streets: %d", len(ix.streets))
	}
	if len(ix.known) != 14 {
		t.Errorf("known addresses (frontend SEEDS): %d, want 14", len(ix.known))
	}
	for _, st := range ix.streets {
		if len(st.tok) == 0 {
			t.Errorf("street %q has no name tokens", st.full)
		}
		if st.okrug == "" || st.district == "" || st.lat == 0 || st.lon == 0 {
			t.Errorf("street %q lacks okrug/district/coords", st.full)
		}
	}
	for _, k := range ix.known {
		if !k.source.Valid() || k.house == "" {
			t.Errorf("known %q: source %q house %q", k.label, k.source, k.house)
		}
	}
}

func TestBuildStreetIndexSkipsBrokenRows(t *testing.T) {
	t.Parallel()
	var f streetsFile
	src := `{"streets":[["Тверская улица","ЦАО","Тверской",55.7,37.6],["короткая"],["Охотный Ряд","ЦАО","Тверской","нечисло",37.6]],
	         "known":[["Москва, Тверская улица, 1","yandex_map","Тверская улица","ЦАО","Тверской","1","","",55.7,37.6],
	                  ["Москва, Нет улицы, 1","yandex_map","Нет улицы","ЦАО","Тверской","1","","",55.7,37.6],
	                  ["мало полей"]]}`
	if err := json.Unmarshal([]byte(src), &f); err != nil {
		t.Fatal(err)
	}
	ix := buildStreetIndex(&f)
	if len(ix.streets) != 2 {
		t.Errorf("streets %d, want 2 (short row skipped)", len(ix.streets))
	}
	if ix.streets[1].lat != 0 {
		t.Errorf("broken number must become 0, got %v", ix.streets[1].lat)
	}
	if len(ix.known) != 1 || ix.known[0].street != ix.streets[0] {
		t.Errorf("known: %+v", ix.known)
	}
}
