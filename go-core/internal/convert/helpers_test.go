package convert

import (
	"encoding/json"
	"testing"
	"time"

	"lct/gocore/internal/core"
)

// fakeCatalog — справочник в памяти: типы и службы по id/коду.
type fakeCatalog struct {
	types    []core.IncidentTypeInfo
	services []core.ServiceInfo
}

var _ core.Catalog = (*fakeCatalog)(nil)

func (c *fakeCatalog) TypeByID(id string) (core.IncidentTypeInfo, bool) {
	for _, t := range c.types {
		if t.ID == id {
			return t, true
		}
	}
	return core.IncidentTypeInfo{}, false
}

func (c *fakeCatalog) TypeByCode(code string) (core.IncidentTypeInfo, bool) {
	for _, t := range c.types {
		if t.Code == code {
			return t, true
		}
	}
	return core.IncidentTypeInfo{}, false
}

func (c *fakeCatalog) ServiceByID(id string) (core.ServiceInfo, bool) {
	for _, s := range c.services {
		if s.ID == id {
			return s, true
		}
	}
	return core.ServiceInfo{}, false
}

func (c *fakeCatalog) ServiceByCode(code string) (core.ServiceInfo, bool) {
	for _, s := range c.services {
		if s.Code == code {
			return s, true
		}
	}
	return core.ServiceInfo{}, false
}

func (c *fakeCatalog) FieldLabel(string) string                          { return "Поле карточки" }
func (c *fakeCatalog) AttributeLabel(string) (string, bool)              { return "", false }
func (c *fakeCatalog) AttributeValueLabel(string, string) (string, bool) { return "", false }

func testCatalog() *fakeCatalog {
	return &fakeCatalog{
		types: []core.IncidentTypeInfo{
			{ID: "t-fire", Code: "101", Name: "Пожар"},
			{ID: "t-gas", Code: "104", Name: "Запах газа"},
			{ID: "t-nocode", Code: "", Name: "Без кода"},
		},
		services: []core.ServiceInfo{
			{ID: "s-101", Code: "101", Name: "Пожарная охрана", ShortName: "Служба 101"},
			{ID: "s-103", Code: "103", Name: "Скорая помощь", ShortName: "Служба 103"},
		},
	}
}

func TestPtrDeref(t *testing.T) {
	t.Parallel()

	v := 5
	p := Ptr(v)
	v = 6
	if *p != 5 {
		t.Fatal("Ptr должен копировать значение")
	}
	if Deref(p) != 5 || Deref[int](nil) != 0 || Deref[string](nil) != "" {
		t.Fatal("Deref")
	}
	if StrOr(nil, "def") != "def" || StrOr(Ptr(""), "def") != "" || StrOr(Ptr("Иван"), "def") != "Иван" {
		t.Fatal("StrOr")
	}
}

func TestNonEmpty(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"", " ", "\t\n", " "} {
		if NonEmpty(s) != nil {
			t.Errorf("NonEmpty(%q) != nil", s)
		}
	}
	p := NonEmpty(" Москва ")
	if p == nil || *p != " Москва " {
		t.Fatalf("NonEmpty не обрезает значение: %v", p)
	}

	if NonEmptyPtr(nil) != nil || NonEmptyPtr(Ptr("  ")) != nil {
		t.Fatal("NonEmptyPtr пустых")
	}
	in := Ptr("ул. Ленина")
	if NonEmptyPtr(in) != in {
		t.Fatal("NonEmptyPtr должен вернуть тот же указатель")
	}
	if Str(nil) != "" || Str(Ptr("  Пётр  ")) != "Пётр" {
		t.Fatal("Str")
	}
}

func TestSlices(t *testing.T) {
	t.Parallel()

	if s := Slice[string](nil); s == nil || len(s) != 0 {
		t.Fatal("Slice(nil) -> []")
	}
	b, _ := json.Marshal(Slice[int](nil))
	if string(b) != "[]" {
		t.Fatalf("json: %s", b)
	}
	in := []string{"a"}
	if got := Slice(in); &got[0] != &in[0] {
		t.Fatal("Slice не должен копировать непустой срез")
	}

	if SlicePtr[string](nil) != nil || SlicePtr([]string{}) != nil {
		t.Fatal("SlicePtr пустого -> nil")
	}
	if p := SlicePtr([]string{"x"}); p == nil || (*p)[0] != "x" {
		t.Fatal("SlicePtr")
	}
	if SliceFromPtr[string](nil) != nil {
		t.Fatal("SliceFromPtr(nil)")
	}
	if got := SliceFromPtr(&[]int{1, 2}); len(got) != 2 {
		t.Fatal("SliceFromPtr")
	}
	if p := NonNilSlicePtr[string](nil); p == nil || *p == nil || len(*p) != 0 {
		t.Fatal("NonNilSlicePtr(nil) -> &[]")
	}
	if p := NonNilSlicePtr([]string{"q"}); len(*p) != 1 {
		t.Fatal("NonNilSlicePtr")
	}
}

func TestUTCAndIntPtrIf(t *testing.T) {
	t.Parallel()

	if UTC(nil) != nil {
		t.Fatal("UTC(nil)")
	}
	msk := time.FixedZone("MSK", 3*3600)
	in := time.Date(2026, 9, 24, 15, 0, 0, 0, msk)
	got := UTC(&in)
	if got.Location() != time.UTC || !got.Equal(in) || got.Hour() != 12 {
		t.Fatalf("UTC = %v", got)
	}
	if got == &in {
		t.Fatal("UTC должен возвращать новый указатель")
	}
	if IntPtrIf(0) != nil || *IntPtrIf(7) != 7 || *IntPtrIf(-1) != -1 {
		t.Fatal("IntPtrIf")
	}
}

func TestCopyStrings(t *testing.T) {
	t.Parallel()

	if copyStrings(nil) != nil {
		t.Fatal("nil сохраняется")
	}
	in := []string{"a", "b"}
	out := copyStrings(in)
	out[0] = "z"
	if in[0] != "a" {
		t.Fatal("copyStrings должен копировать")
	}
	if e := copyStrings([]string{}); e == nil || len(e) != 0 {
		t.Fatal("пустой -> пустой не nil")
	}
}
