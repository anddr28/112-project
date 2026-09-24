package classifier

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"lct/gocore/internal/gen/public"
)

// mockSearch — фронтовой searchTypes (fixtures/classifier.ts) один в один: toLowerCase,
// ё -> е, [-,.] -> пробел, каждое слово — префикс какого-то слова name/code/synonyms.
func mockSearch(q string) []string {
	norm := func(s string) string {
		s = strings.ReplaceAll(strings.ToLower(s), "ё", "е")
		return strings.NewReplacer("-", " ", ",", " ", ".", " ").Replace(s)
	}
	nq := strings.TrimSpace(norm(q))
	if nq == "" {
		return nil
	}
	words := strings.Fields(nq)
	var out []string
	for _, ty := range data().classifier.Types {
		hay := strings.Fields(norm(strings.Join(append([]string{ty.Name, ty.Code}, ty.Synonyms...), " ")))
		all := true
		for _, w := range words {
			if !anyPrefix(hay, w) {
				all = false
				break
			}
		}
		if all {
			out = append(out, ty.Code)
		}
	}
	return out
}

// Сервер обещает те же результаты, что фронтовой мок, на данных фикстур (порядок — свой, по рангу).
func TestSearchMatchesFrontendMock(t *testing.T) {
	t.Parallel()
	c := fixtureCatalog(t)
	queries := map[string]bool{"Пожар": true, "ЗАПАХ газа": true, "газа запах": true, "ё": true, "дтп наезд": true,
		"справка-101": true, "справка 101": true, "101": true, "10": true, "it": true, "вызов 03": true,
		"происш": true, "происшествие 104 утечка": true, "авар": true, "на ин": true, "не туда": true, "xyz": true}
	for _, ty := range data().classifier.Types {
		for _, s := range append([]string{ty.Name, ty.Code}, ty.Synonyms...) {
			queries[s] = true
			for _, w := range strings.Fields(s) {
				r := []rune(w)
				queries[string(r[:min(len(r), 3)])] = true
				queries[strings.ToUpper(w)] = true
			}
		}
	}
	for q := range queries {
		got := codesOf(c.Search(q, SearchMaxLimit))
		want := mockSearch(q)
		gs, ws := slices.Clone(got), slices.Clone(want)
		slices.Sort(gs)
		slices.Sort(ws)
		if !slices.Equal(gs, ws) {
			t.Errorf("Search(%q) = %v, mock %v", q, got, want)
		}
	}
}

func TestSearchRanking(t *testing.T) {
	t.Parallel()
	c := fixtureCatalog(t)
	for _, tc := range []struct{ q, first string }{
		{"газ", "104"},         // синоним целиком
		{"пожар", "101"},       // синоним целиком
		{"дтп", "dtp"},         // название = код
		{"Взрыв", "explosion"}, // название целиком
		{"лифт", "elevator"},   // синоним
		{"застр", "elevator"},  // префикс названия
		{"ЗАСТРЕВАНИЕ", "elevator"},
		{"справка", "consult"}, // синоним у двух типов — порядок классификатора
		{"благодар", "thanks"},
	} {
		got := codesOf(c.Search(tc.q, 5))
		if len(got) == 0 || got[0] != tc.first {
			t.Errorf("Search(%q) = %v, want first %q", tc.q, got, tc.first)
		}
	}
	// ранги: точное совпадение > начало названия > все слова в названии > код/синонимы
	got := codesOf(c.Search("авария", 10))
	if len(got) < 2 || !slices.Contains(got, "dtp") || !slices.Contains(got, "gorhoz") {
		t.Errorf("«авария» (synonym of dtp and gorhoz): %v", got)
	}
}

// Крупный классификатор (импорт XLSX ~1400 типов): учебные типы не теряются за лимитом.
func TestSearchTiersWithLargeClassifier(t *testing.T) {
	t.Parallel()
	types, services := fixtureRaw(t)
	root := uuid.New()
	types = append(types, rawType{id: root, code: "1", name: "Пожары и задымления", depth: 1, active: true, sortOrder: 1000, rulesRank: -1})
	for i := range 200 {
		types = append(types, rawType{id: uuid.New(), code: fmt.Sprintf("10101%02d", i), parentID: &root,
			name: fmt.Sprintf("Пожар: объект %d", i), depth: 3, active: i%7 != 0, sortOrder: 1001 + i, rulesRank: -1,
			synonyms: []string{"газ " + fmt.Sprint(i)}})
	}
	types = append(types, rawType{id: uuid.New(), code: "9000000", name: "Горит склад", depth: 3, active: true,
		sortOrder: 5000, rulesRank: -1, synonyms: []string{"пожар на складе"}})
	c := catalogFrom(types, services)

	got := codesOf(c.Search("пожар", 5))
	if len(got) != 5 || got[0] != "101" {
		t.Fatalf("«пожар» limit 5: %v", got)
	}
	// после точного совпадения — типы, чьё название начинается с запроса
	for _, code := range got[1:] {
		info, _ := c.TypeByCode(code)
		if !strings.HasPrefix(normalize(info.Name), "пожар") {
			t.Errorf("tier 1 expected, got %q", info.Name)
		}
	}
	all := codesOf(c.Search("пожар", SearchMaxLimit))
	if len(all) != SearchMaxLimit {
		t.Errorf("limit %d: got %d", SearchMaxLimit, len(all))
	}
	if got := codesOf(c.Search("газ", 3)); got[0] != "104" {
		t.Errorf("«газ» first: %v", got)
	}
	// «склад пожар»: слова в любом порядке, «пожар» есть только в синониме -> ранг 3
	if got := codesOf(c.Search("склад пожар", 10)); !slices.Equal(got, []string{"9000000"}) {
		t.Errorf("«склад пожар»: %v", got)
	}
	// выключенные типы не ищутся
	for _, code := range codesOf(c.Search("объект", SearchMaxLimit)) {
		var n int
		if _, err := fmt.Sscanf(code, "10101%d", &n); err == nil && n%7 == 0 {
			t.Errorf("inactive type %s found", code)
		}
	}
}

func TestSearchLimitsAndEmpty(t *testing.T) {
	t.Parallel()
	c := fixtureCatalog(t)
	if got := c.s().searchIdx("", 10); got != nil {
		t.Errorf("empty query: %v", got)
	}
	if got := c.s().searchIdx(" -.,: ", 10); got != nil {
		t.Errorf("punctuation-only query: %v", got)
	}
	if got := c.Search("о", 0); len(got) != min(SearchDefaultLimit, len(mockSearch("о"))) {
		t.Errorf("limit 0 -> default: %d", len(got))
	}
	if got := c.Search("о", 2); len(got) != 2 {
		t.Errorf("limit 2: %d", len(got))
	}
	if got := c.Search("о", 1_000_000); len(got) != len(mockSearch("о")) {
		t.Errorf("huge limit: %d", len(got))
	}
	// Search и searchJSON отдают одно и то же
	it := decode[[]public.IncidentType](t, c.searchJSON("газ", 5))
	if !slices.EqualFunc(it, c.Search("газ", 5), func(a, b public.IncidentType) bool {
		return a.Id == b.Id && a.Name == b.Name && a.HasReference == b.HasReference && slices.Equal(a.Synonyms, b.Synonyms)
	}) {
		t.Error("searchJSON differs from Search")
	}
	if string(c.searchJSON("нетакогослова", 5)) != "[]" {
		t.Errorf("no results JSON: %s", c.searchJSON("нетакогослова", 5))
	}
}
