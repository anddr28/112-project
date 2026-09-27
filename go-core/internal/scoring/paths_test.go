package scoring

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"lct/gocore/internal/model"
)

func TestCanonicalPath(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"applicant.name", "applicant.name"},
		{" applicant.name ", "applicant.name"},
		{"incidentTypeIds", "incidentTypeIds"},
		{"incident_type_ids", "incidentTypeIds"},
		{"category_code", "incidentTypeIds"},
		{"categoryCode", "incidentTypeIds"},
		{"category", "incidentTypeIds"},
		{"services_to_notify", "services"},
		{"servicesToNotify", "services"},
		{"actions_taken", "actionsTaken"},
		{"applicant.foreign_language", "applicant.foreignLanguage"},
		{"phones.on_site", "phones.onSite"},
		{"flags.victims_count", "flags.victimsCount"},
		{"flags.ambulance_refusal", "flags.ambulanceRefusal"},
		{"address.city", "address.settlement"},
		{"applicant.phone", "phones.aon"},
		{"phone", "phones.aon"},
		// Регрессия: остальные пути контрактного IncidentCard (как их раскладывает CardToDraft).
		{"address.landmark", "address.descriptive"},
		{"casualties", "flags.victimsPresent"},
		{"casualties.injured", "flags.victimsCount"},
		{"casualties.dead", "flags.victimsCount"},
		{"casualties.trapped", "flags.victimsCount"},
		// Коды признаков — коды классификатора, их не трогаем.
		{"attributes.fire_sign", "attributes.fire_sign"},
		{" attributes.people_threat", "attributes.people_threat"},
		{"_x", "x"},
		{"a._b", "a.b"},
		{"a__b", "aB"},
	}
	for _, c := range cases {
		if got := CanonicalPath(c.in); got != c.want {
			t.Errorf("CanonicalPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Каждый путь контрактного IncidentCard (_components.yaml) должен приводиться к полю формы
// АРМ — иначе обязательное поле из сгенерированного эталона было бы незаполнимым.
func TestContractCardPathsAreKnown(t *testing.T) {
	t.Parallel()
	for _, p := range []string{
		"category_code", "address.raw", "address.city", "address.street", "address.house",
		"address.entrance", "address.floor", "address.apartment", "address.landmark",
		"applicant.name", "applicant.phone", "casualties", "casualties.injured", "casualties.dead",
		"casualties.trapped", "services_to_notify", "description", "actions_taken", "attributes.where",
	} {
		if c := CanonicalPath(p); classOf(c) == clsUnknown {
			t.Errorf("contract path %q → %q is not a form field", p, c)
		}
	}
}

func TestDefaultRequiredFieldsAreKnown(t *testing.T) {
	t.Parallel()
	for _, p := range model.DefaultRequiredFields() {
		if CanonicalPath(p) != p {
			t.Errorf("default required field %q is not canonical", p)
		}
		if classOf(p) == clsUnknown {
			t.Errorf("default required field %q has no comparison class", p)
		}
		if _, ok := defaultLabels[p]; !ok {
			t.Errorf("default required field %q has no built-in label", p)
		}
	}
}

// Встроенные подписи и классы сравнения описывают один и тот же набор полей формы.
func TestDefaultLabelsMatchClasses(t *testing.T) {
	t.Parallel()
	for p := range defaultLabels {
		if classOf(p) == clsUnknown {
			t.Errorf("labelled field %q has no comparison class", p)
		}
		if CanonicalPath(p) != p {
			t.Errorf("labelled field %q is not canonical", p)
		}
	}
}

// Подписи справочника (classifier/data/field_labels.json) — только для известных полей формы.
func TestClassifierFieldLabelsAreKnown(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("../classifier/data/field_labels.json")
	if err != nil {
		t.Skipf("field_labels.json: %v", err)
	}
	var labels map[string]string
	if err := json.Unmarshal(b, &labels); err != nil {
		t.Fatal(err)
	}
	for p, l := range labels {
		if classOf(p) == clsUnknown {
			t.Errorf("classifier label for %q: no comparison class", p)
		}
		if d, ok := defaultLabels[p]; ok && d != l {
			t.Errorf("label mismatch for %q: classifier %q, built-in %q", p, l, d)
		}
	}
}

func TestClassOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want valClass
	}{
		{"description", clsFree},
		{"actionsTaken", clsFree},
		{"address.descriptive", clsFree},
		{"applicant.name", clsName},
		{"applicant.status", clsText},
		{"phones.channel", clsText},
		{"flags.victimsPresent", clsBool},
		{"applicant.foreignLanguage", clsBool},
		{"phones.aon", clsPhone},
		{"flags.victimsCount", clsInt},
		{"address.lat", clsCoord},
		{"address.raw", clsAddrRaw},
		{"address.house", clsAddrPart},
		{"incidentTypeIds", clsTypes},
		{"services", clsServices},
		{"attributes.where", clsAttr},
		{"attributes.", clsUnknown},
		{"attributes", clsUnknown},
		{"foo.bar", clsUnknown},
		{"", clsUnknown},
	}
	for _, c := range cases {
		if got := classOf(c.path); got != c.want {
			t.Errorf("classOf(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestFieldLabel(t *testing.T) {
	t.Parallel()
	cat := testCatalog()
	custom := testCatalog()
	custom.fields["applicant.name"] = "Заявитель (ФИО)"
	custom.fields["attributes.orphan"] = "Сирота-признак"
	cases := []struct {
		name string
		cat  *fakeCatalog
		path string
		want string
	}{
		{"built-in without catalog", nil, "applicant.name", "ФИО заявителя"},
		{"built-in address part", nil, "address.structure", "Строение"},
		{"unknown path without catalog", nil, "foo.bar", "Поле карточки"},
		{"attribute without catalog", nil, "attributes.where", "Признак опросной карты"},
		{"catalog attribute label", cat, "attributes.where", "Где горит"},
		{"catalog unknown attribute", cat, "attributes.nope", "Признак опросной карты"},
		{"catalog field label overrides built-in", custom, "applicant.name", "Заявитель (ФИО)"},
		{"catalog fallback → built-in", cat, "flags.callDropped", "Срыв звонка"},
		{"catalog field label for attribute path", custom, "attributes.orphan", "Сирота-признак"},
		{"catalog fallback for unknown", cat, "foo.bar", "Поле карточки"},
	}
	for _, c := range cases {
		var got string
		if c.cat == nil {
			got = FieldLabel(nil, c.path)
		} else {
			got = FieldLabel(c.cat, c.path)
		}
		if got != c.want {
			t.Errorf("%s: FieldLabel(%q) = %q, want %q", c.name, c.path, got, c.want)
		}
		if strings.Contains(got, ".") && !strings.Contains(c.want, ".") {
			t.Errorf("%s: technical path leaked into label %q", c.name, got)
		}
	}
}
