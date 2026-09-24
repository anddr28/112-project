package scoring

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"lct/gocore/internal/core"
	"lct/gocore/internal/settings"
)

// fakeCatalog — core.Catalog для тестов: подписи ведут себя как у classifier.Catalog
// (неизвестное поле — «Поле карточки», неизвестный признак — «Признак опросной карты»).
type fakeCatalog struct {
	types    []core.IncidentTypeInfo
	services []core.ServiceInfo
	fields   map[string]string
	attrs    map[string]string
	values   map[string]map[string]string
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
		if strings.EqualFold(s.Code, code) {
			return s, true
		}
	}
	return core.ServiceInfo{}, false
}

func (c *fakeCatalog) FieldLabel(path string) string {
	if l, ok := c.fields[path]; ok {
		return l
	}
	if code, ok := strings.CutPrefix(path, "attributes."); ok {
		if l, ok := c.attrs[code]; ok {
			return l
		}
		return "Признак опросной карты"
	}
	return "Поле карточки"
}

func (c *fakeCatalog) AttributeLabel(code string) (string, bool) {
	l, ok := c.attrs[code]
	return l, ok
}

func (c *fakeCatalog) AttributeValueLabel(attr, value string) (string, bool) {
	l, ok := c.values[attr][value]
	return l, ok
}

// testCatalog — небольшой справочник в духе демо-данных (типы 101/104, службы 101..104, Гормост).
func testCatalog() *fakeCatalog {
	return &fakeCatalog{
		types: []core.IncidentTypeInfo{
			{ID: "t-fire", Code: "101", Name: "Пожар"},
			{ID: "t-gas", Code: "104", Name: "Запах газа"},
		},
		services: []core.ServiceInfo{
			{ID: "s-101", Code: "101", ShortName: "Служба 101", Name: "Пожарная охрана"},
			{ID: "s-103", Code: "103", ShortName: "Служба 103", Name: "Скорая помощь"},
			{ID: "s-104", Code: "104", ShortName: "Служба 104", Name: "Мосгаз"},
			{ID: "s-gm", Code: "gormost", ShortName: "Гормост", Name: "ГБУ «Гормост»"},
			{ID: "s-noname", Code: "noname", Name: "Служба без краткого имени"},
		},
		fields: map[string]string{
			"applicant.name": "ФИО заявителя",
			"address.raw":    "Адрес",
			"services":       "Службы",
		},
		attrs: map[string]string{
			"where":          "Где горит",
			"fire_sign":      "Признак пожара",
			"floors":         "Этажность",
			"indoor_objects": "Объекты в помещении",
			"gasified":       "Газифицирован",
		},
		values: map[string]map[string]string{
			"where":          {"house": "Жилой дом", "flat": "Квартира"},
			"fire_sign":      {"flame": "Открытое пламя", "smoke": "Дым"},
			"indoor_objects": {"apartment": "Квартира", "gas_column": "Газовая колонка"},
		},
	}
}

func ptr[T any](v T) *T { return &v }

func approx(a, b, eps float64) bool { return math.Abs(a-b) <= eps }

// mustJSON — JSON-представление значения (для проверки формы ответа).
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return string(b)
}

// defaultTolerance — допуски тайминга из сидов settings (soft 20%, hard 100%).
func defaultTolerance() settings.TimingTolerance { return settings.Defaults().TimingTolerance }
