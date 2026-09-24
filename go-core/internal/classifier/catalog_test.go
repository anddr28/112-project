package classifier

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
)

var _ core.Catalog = (*Catalog)(nil)

func decode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return v
}

func codesOf(l []public.IncidentType) []string {
	out := make([]string, 0, len(l))
	for _, t := range l {
		out = append(out, t.Code)
	}
	return out
}

func TestEmptyCatalogServesEmptyLists(t *testing.T) {
	t.Parallel()
	c := NewCatalog(nil, nil)
	s := c.s()
	for name, body := range map[string][]byte{"types": s.typesBody.body, "services": s.servicesBody.body} {
		if string(body) != "[]" {
			t.Errorf("%s = %s, want []", name, body)
		}
	}
	if string(s.featuredBody.body) != `{"frequent":[],"significant":[]}` {
		t.Errorf("featured = %s", s.featuredBody.body)
	}
	lb := decode[map[string]json.RawMessage](t, s.labelsBody.body)
	for _, k := range []string{"fields", "attributes", "values", "types"} {
		if v, ok := lb[k]; !ok || string(v) == "null" {
			t.Errorf("labels.%s = %s", k, v)
		}
	}
	if len(decode[map[string]string](t, lb["fields"])) == 0 {
		t.Error("field labels are embedded and must be served even before Reload")
	}
	if got := c.Services(); got == nil || len(got) != 0 {
		t.Errorf("Services() = %#v", got)
	}
	if _, ok := c.TypeByID("it-101"); ok {
		t.Error("empty catalog has no types")
	}
	if got := c.Search("пожар", 10); got == nil || len(got) != 0 {
		t.Errorf("Search on empty catalog = %#v", got)
	}
	if string(c.searchJSON("пожар", 10)) != "[]" {
		t.Errorf("searchJSON = %s", c.searchJSON("пожар", 10))
	}
	if s.etagOK() != nil {
		t.Error(s.etagOK())
	}
}

// etagOK — у каждого предкодированного ответа есть сильный ETag в кавычках.
func (s *snapshot) etagOK() error {
	for _, c := range []cached{s.typesBody, s.featuredBody, s.labelsBody, s.servicesBody, s.emptyAttrs} {
		if len(c.etag) != 18 || c.etag[0] != '"' || c.etag[17] != '"' {
			return fmt.Errorf("bad etag %q", c.etag)
		}
	}
	return nil
}

func TestFixtureTypesList(t *testing.T) {
	t.Parallel()
	c := fixtureCatalog(t)
	s := c.s()
	if err := s.etagOK(); err != nil {
		t.Fatal(err)
	}
	types := decode[[]public.IncidentType](t, s.typesBody.body)
	emb := data()
	if len(types) != len(emb.classifier.Types) {
		t.Fatalf("types %d, want %d", len(types), len(emb.classifier.Types))
	}
	for i, it := range types {
		ft := emb.classifier.Types[i] // порядок классификатора = порядок фикстуры
		if it.Code != ft.Code || it.Name != ft.Name || it.Depth != max(ft.Depth, 1) {
			t.Errorf("types[%d] = %+v, fixture %+v", i, it, ft)
		}
		if it.Id != typeUUID(ft.Code).String() {
			t.Errorf("types[%d].id = %s", i, it.Id)
		}
		if it.Synonyms == nil || !slices.Equal(it.Synonyms, ft.Synonyms) {
			t.Errorf("types[%d].synonyms = %#v", i, it.Synonyms)
		}
		if it.HasReference != (ft.Reference != "") {
			t.Errorf("types[%d].hasReference = %v", i, it.HasReference)
		}
		if it.ParentId != nil {
			t.Errorf("fixture types are flat, got parent %v", *it.ParentId)
		}
	}
	// JSON-форма: обязательные поля есть, parentId опущен, synonyms — массив
	raw := decode[[]map[string]any](t, s.typesBody.body)
	for _, m := range raw {
		for _, k := range []string{"id", "code", "name", "depth", "synonyms", "hasReference"} {
			if _, ok := m[k]; !ok {
				t.Fatalf("type JSON misses %q: %v", k, m)
			}
		}
		if _, ok := m["synonyms"].([]any); !ok {
			t.Fatalf("synonyms is not an array: %v", m)
		}
	}
}

func TestFeatured(t *testing.T) {
	t.Parallel()
	c := fixtureCatalog(t)
	type featured struct {
		Frequent    []public.IncidentType `json:"frequent"`
		Significant []public.IncidentType `json:"significant"`
	}
	f := decode[featured](t, c.s().featuredBody.body)
	emb := data()
	if got := codesOf(f.Frequent); !slices.Equal(got, emb.classifier.Frequent) {
		t.Errorf("frequent %v, want fixture order %v", got, emb.classifier.Frequent)
	}
	if got := codesOf(f.Significant); !slices.Equal(got, emb.classifier.Significant) {
		t.Errorf("significant %v, want fixture order %v", got, emb.classifier.Significant)
	}
}

func TestLabels(t *testing.T) {
	t.Parallel()
	c := fixtureCatalog(t)
	lb := decode[public.ClassifierLabels](t, c.s().labelsBody.body)
	if !maps.Equal(lb.Fields, FieldLabels()) {
		t.Errorf("fields differ from embedded FIELD_LABELS")
	}
	if lb.Fields["address.raw"] != "Адрес" || lb.Fields["incidentTypeIds"] != "Тип происшествия" {
		t.Errorf("fields: %v", lb.Fields)
	}
	if lb.Attributes["where"] == "" || lb.Attributes["people_threat"] == "" || lb.Attributes["water_where"] == "" {
		t.Errorf("attributes: %v", lb.Attributes)
	}
	if lb.Values["where"]["house"] == "" || lb.Values["gasified"]["unknown"] == "" {
		t.Errorf("values: %v", lb.Values)
	}
	if _, ok := lb.Values["floors"]; ok {
		t.Error("attribute without options must not appear in values")
	}
	if len(lb.Types) != len(data().classifier.Types) || lb.Types[typeUUID("dtp").String()] != "ДТП" {
		t.Errorf("types: %v", lb.Types)
	}

	// core.Catalog-подписи
	if l, ok := c.AttributeLabel("where"); !ok || l != lb.Attributes["where"] {
		t.Errorf("AttributeLabel(where) = %q %v", l, ok)
	}
	if _, ok := c.AttributeLabel("нет-такого"); ok {
		t.Error("unknown attribute label")
	}
	if l, ok := c.AttributeValueLabel("where", "house"); !ok || l != lb.Values["where"]["house"] {
		t.Errorf("AttributeValueLabel = %q %v", l, ok)
	}
	for _, tc := range [][2]string{{"where", "нет"}, {"нет", "house"}, {"floors", "5"}} {
		if _, ok := c.AttributeValueLabel(tc[0], tc[1]); ok {
			t.Errorf("AttributeValueLabel(%q, %q) must be unknown", tc[0], tc[1])
		}
	}
	for _, tc := range []struct{ path, want string }{
		{"applicant.name", "ФИО заявителя"},
		{"services", "Службы"},
		{"attributes.where", lb.Attributes["where"]},
		{"attributes.нет", "Признак опросной карты"},
		{"что-то.другое", "Поле карточки"},
		{"", "Поле карточки"},
	} {
		if got := c.FieldLabel(tc.path); got != tc.want {
			t.Errorf("FieldLabel(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestFieldLabelsIsCopy(t *testing.T) {
	t.Parallel()
	a := FieldLabels()
	a["address.raw"] = "испорчено"
	if FieldLabels()["address.raw"] != "Адрес" {
		t.Fatal("FieldLabels returned the shared map")
	}
}

func TestLookups(t *testing.T) {
	t.Parallel()
	c := fixtureCatalog(t)
	id := typeUUID("101").String()
	for _, key := range []string{id, "101", "it-101"} {
		info, ok := c.ResolveType(key)
		if !ok || info.ID != id || info.Code != "101" || info.Name != "Происшествие 101" {
			t.Errorf("ResolveType(%q) = %+v %v", key, info, ok)
		}
	}
	if _, ok := c.ResolveType("нет"); ok {
		t.Error("ResolveType(unknown)")
	}
	// TypeByID — uuid и id фикстуры, но не код
	if _, ok := c.TypeByID("it-dtp"); !ok {
		t.Error("TypeByID(fixture id)")
	}
	if _, ok := c.TypeByID(id); !ok {
		t.Error("TypeByID(uuid)")
	}
	if _, ok := c.TypeByID("dtp"); ok {
		t.Error("TypeByID must not accept a code")
	}
	if info, ok := c.TypeByCode("dtp"); !ok || info.Name != "ДТП" {
		t.Errorf("TypeByCode = %+v", info)
	}
	if _, ok := c.TypeByCode("it-dtp"); ok {
		t.Error("TypeByCode must not accept a fixture id")
	}

	info, _ := c.TypeByCode("101")
	if !slices.Equal(info.Path, []string{"Происшествие 101"}) || info.Depth != 1 || info.ParentID != "" {
		t.Errorf("101 info: %+v", info)
	}
	// службы типа — из правил, в порядке правил, без повторов
	if want := []string{"101", "zhkh", "103", "cemp", "104", "moslift", "oek", "codd"}; !slices.Equal(info.Services, want) {
		t.Errorf("101 services %v, want %v", info.Services, want)
	}
	if want := []string{"where", "fire_sign", "access", "house_kind", "floors", "people_threat", "indoor_objects",
		"traffic_block", "gasified"}; !slices.Equal(info.Attributes, want) {
		t.Errorf("101 attributes %v", info.Attributes)
	}
	if info, _ := c.TypeByCode("cancel"); info.Services == nil || len(info.Services) != 0 || info.Attributes == nil {
		t.Errorf("type without rules: services %#v attributes %#v", info.Services, info.Attributes)
	}

	svc, ok := c.ServiceByCode("103")
	if !ok || svc.ID != serviceUUID("103").String() || svc.Kind != "emergency" || svc.ShortName != "Служба 103" {
		t.Errorf("ServiceByCode(103) = %+v", svc)
	}
	if got, ok := c.ServiceByID(svc.ID); !ok || got != svc {
		t.Errorf("ServiceByID = %+v", got)
	}
	if _, ok := c.ServiceByCode("нет"); ok {
		t.Error("unknown service")
	}
	if _, ok := c.ServiceByID(uuid.NewString()); ok {
		t.Error("unknown service id")
	}
}

func TestServicesOrderAndInactive(t *testing.T) {
	t.Parallel()
	types, services := fixtureRaw(t)
	// службы из БД в «случайном» порядке + служба не из фикстуры + выключенная
	slices.Reverse(services)
	services = append(services,
		rawService{id: serviceUUID("aaa"), code: "aaa", name: "Городская А", shortName: "А", kind: "city", active: true},
		rawService{id: serviceUUID("zzz-em"), code: "zzz-em", name: "Экстренная Я", shortName: "Я", kind: "emergency", active: true},
	)
	for i := range services {
		if services[i].code == "oati" {
			services[i].active = false
		}
	}
	c := catalogFrom(types, services)

	var codes []string
	for _, s := range c.Services() {
		codes = append(codes, s.Code)
	}
	var want []string
	for _, s := range data().services {
		if s.Code != "oati" {
			want = append(want, s.Code)
		}
	}
	want = append(want, "zzz-em", "aaa") // вне фикстуры: экстренные раньше, затем по коду
	if !slices.Equal(codes, want) {
		t.Errorf("Services() = %v, want %v", codes, want)
	}
	refs := decode[[]public.ServiceRef](t, c.s().servicesBody.body)
	if len(refs) != len(want) {
		t.Fatalf("services body: %d", len(refs))
	}
	for i, r := range refs {
		if r.Code != want[i] || !r.Kind.Valid() || r.Id == "" || r.Name == "" || r.ShortName == "" {
			t.Errorf("services[%d] = %+v", i, r)
		}
	}
	// выключенная служба не в списке, но находится по коду (история карточек)
	if _, ok := c.ServiceByCode("oati"); !ok {
		t.Error("inactive service lookup")
	}
}

func TestInactiveTypesAndHierarchy(t *testing.T) {
	t.Parallel()
	root, cat, leaf, hidden := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	a, b := uuid.New(), uuid.New() // цикл parent_id — путь не должен зациклиться
	types := []rawType{
		{id: root, code: "1", name: "Пожары", depth: 1, active: true, sortOrder: 1, rulesRank: -1},
		{id: cat, code: "1010000", parentID: &root, name: "На улице", depth: 2, active: true, sortOrder: 2, rulesRank: -1},
		{id: leaf, code: "1010101", parentID: &cat, name: "Пожар: мусор", depth: 3, active: true, sortOrder: 3, rulesRank: -1,
			synonyms: []string{"мусор"}},
		{id: hidden, code: "1010102", parentID: &cat, name: "Пожар: скрытый", depth: 3, active: false, sortOrder: 4, rulesRank: -1},
		{id: a, code: "a", parentID: &b, name: "А", depth: 2, active: true, sortOrder: 5, rulesRank: -1},
		{id: b, code: "b", parentID: &a, name: "Б", depth: 2, active: true, sortOrder: 6, rulesRank: -1},
		// битый jsonb: тип без опросной карты и правил, справочник не падает
		{id: uuid.New(), code: "broken", name: "Битый", depth: 1, active: true, sortOrder: 7, rulesRank: -1,
			attributes: []byte(`{не json`), serviceRules: []byte(`[{"services":`), services: []byte(`"x"`)},
	}
	c := catalogFrom(types, nil)

	info, ok := c.TypeByCode("1010101")
	if !ok || !slices.Equal(info.Path, []string{"Пожары", "На улице", "Пожар: мусор"}) || info.ParentID != cat.String() {
		t.Errorf("leaf: %+v", info)
	}
	if info, _ := c.TypeByCode("a"); len(info.Path) != 16 {
		t.Errorf("cyclic path must be cut at 16, got %d", len(info.Path))
	}
	listed := codesOf(decode[[]public.IncidentType](t, c.s().typesBody.body))
	if slices.Contains(listed, "1010102") || len(listed) != 6 {
		t.Errorf("inactive type listed: %v", listed)
	}
	if found := codesOf(c.Search("пожар", 100)); slices.Contains(found, "1010102") || !slices.Contains(found, "1010101") {
		t.Errorf("search: %v", found)
	}
	// выключенный тип остаётся доступен по id (сценарии и отчёты на него ссылаются) и в labels.types
	if _, ok := c.TypeByID(hidden.String()); !ok {
		t.Error("inactive type lookup")
	}
	if lb := decode[public.ClassifierLabels](t, c.s().labelsBody.body); lb.Types[hidden.String()] != "Пожар: скрытый" {
		t.Error("inactive type must stay in labels.types")
	}
	// parentId в JSON
	for _, it := range decode[[]public.IncidentType](t, c.s().typesBody.body) {
		if it.Code == "1010000" && (it.ParentId == nil || *it.ParentId != root.String()) {
			t.Errorf("parentId: %v", it.ParentId)
		}
	}
	br, _ := c.TypeByCode("broken")
	if len(br.Services) != 0 || len(br.Attributes) != 0 {
		t.Errorf("broken jsonb: %+v", br)
	}
	if groups, ok := c.Attributes("broken"); !ok || len(groups) != 0 {
		t.Errorf("broken attributes: %v %v", groups, ok)
	}
}

func TestAttributesCopyAndOrder(t *testing.T) {
	t.Parallel()
	attrs := []public.AttributeGroup{
		{Code: "second", Label: "Второй", Widget: public.AttributeGroupWidgetBool, Order: 2},
		{Code: "first", Label: "Первый", Widget: public.AttributeGroupWidgetText, Order: 1},
	}
	b, _ := json.Marshal(attrs)
	c := catalogFrom([]rawType{{id: uuid.New(), code: "x", name: "X", depth: 1, active: true, attributes: b, rulesRank: -1}}, nil)
	got, ok := c.Attributes("x")
	if !ok || len(got) != 2 || got[0].Code != "first" || got[1].Code != "second" {
		t.Fatalf("Attributes = %+v", got)
	}
	got[0].Code = "испорчено"
	again, _ := c.Attributes("x")
	if again[0].Code != "first" {
		t.Fatal("Attributes returned the snapshot slice")
	}
	body := decode[[]public.AttributeGroup](t, c.s().byCode["x"].attrsBody.body)
	if body[0].Code != "first" {
		t.Errorf("attrs body order: %+v", body)
	}
	if _, ok := c.Attributes("нет"); ok {
		t.Error("unknown type attributes")
	}
}

// TestSnapshotSwapIsRaceFree — читатели не блокируются и не видят «полуснимок» (go test -race).
func TestSnapshotSwapIsRaceFree(t *testing.T) {
	t.Parallel()
	types, services := fixtureRaw(t)
	c := catalogFrom(types, services)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, ok := c.ResolveType("it-101"); !ok {
					t.Error("type vanished during swap")
					return
				}
				_ = c.Search("пож", 5)
				_ = c.Services()
				_ = c.FieldLabel("attributes.where")
			}
		}()
	}
	for range 20 {
		c.snap.Store(buildSnapshot(types, services))
	}
	close(stop)
	wg.Wait()
}
