package scoring

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
)

// etalonFire — эталон в духе демо-сценария «Пожар в квартире».
func etalonFire() *public.IncidentCardDraft {
	d := &public.IncidentCardDraft{}
	d.Applicant.Name = ptr("Мария Иванова")
	d.Applicant.Status = ptr(public.Очевидец)
	d.Address.Raw = "Москва, Тверская улица, 12"
	d.Address.Settlement = ptr("Москва")
	d.Address.Street = ptr("Тверская")
	d.Address.House = ptr("12")
	d.Address.Entrance = ptr("3")
	d.Address.Floor = ptr("5")
	d.Address.Lat = ptr(float32(55.7652))
	d.IncidentTypeIds = []string{"t-fire"}
	d.Description = "Горит квартира на пятом этаже, дым в подъезде."
	d.Flags.VictimsPresent = true
	d.Flags.VictimsCount = ptr(1)
	d.Flags.Blocked = true
	d.Phones.Aon = ptr("+7 (903) 511-33-67")
	d.Attributes = map[string]any{
		"where":          "house",
		"fire_sign":      "flame",
		"floors":         float64(5),
		"indoor_objects": []any{"apartment", "gas_column"},
		"gasified":       true,
		"no_gas":         false,
		"people_threat":  "yes",
	}
	d.Services = []public.AssignedService{{ServiceId: "s-101", Code: "101", ShortName: "Служба 101", Name: "Пожарная охрана"}}
	return d
}

// studentFire — ответ студента, совпадающий с эталоном после нормализации.
func studentFire() *public.IncidentCardDraft {
	d := &public.IncidentCardDraft{}
	d.Applicant.Name = ptr("  иванова МАРИЯ ")
	d.Applicant.Status = ptr(public.Очевидец)
	d.Address.Raw = "Россия, г. Москва, ул. Тверская, д. 12, подъезд 3, этаж 5"
	d.Address.Street = ptr("ул. Тверская")
	d.Address.House = ptr("д. 12")
	d.Address.Entrance = ptr("подъезд 3")
	d.Address.Floor = ptr("05")
	d.Address.Lat = ptr(float32(55.76521))
	d.IncidentTypeIds = []string{"t-fire"}
	d.Description = "Пожар в квартире"
	d.Flags.VictimsPresent = true
	d.Flags.VictimsCount = ptr(1)
	d.Flags.Blocked = true
	d.Phones.Aon = ptr("8 903 511 33 67")
	d.Attributes = map[string]any{
		"where":          "house",
		"fire_sign":      "Flame",
		"floors":         "5",
		"indoor_objects": []string{"gas_column", "apartment"},
		"gasified":       true,
		"people_threat":  "yes",
	}
	d.Services = []public.AssignedService{{ServiceId: "s-101", Code: "101", ShortName: "Служба 101"}}
	return d
}

var allFirePaths = []string{
	"applicant.name", "applicant.status", "address.raw", "address.street", "address.house",
	"address.entrance", "address.floor", "address.lat", "incidentTypeIds", "description",
	"flags.victimsPresent", "flags.victimsCount", "flags.blocked", "flags.noContact", "phones.aon",
	"attributes.where", "attributes.fire_sign", "attributes.floors", "attributes.indoor_objects",
	"attributes.gasified", "attributes.no_gas", "attributes.people_threat", "services",
}

func TestFieldErrorsPerfectCard(t *testing.T) {
	t.Parallel()
	spec := FieldSpec{Required: allFirePaths}
	score, errs := EvaluateFields(studentFire(), etalonFire(), spec, testCatalog())
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %s", mustJSON(t, errs))
	}
	if score != 100 {
		t.Fatalf("score = %v, want 100", score)
	}
	if js := mustJSON(t, errs); js != "[]" {
		t.Fatalf("JSON = %s, want []", js)
	}
}

func TestFieldErrorsNilAndEmpty(t *testing.T) {
	t.Parallel()
	// Без обязательных полей — пустой (не nil) список и 100 баллов.
	errs := FieldErrors(nil, nil, FieldSpec{}, nil)
	if errs == nil || len(errs) != 0 {
		t.Fatalf("FieldErrors(nil...) = %#v", errs)
	}
	if s := FieldsScore(errs, FieldSpec{}); s != 100 {
		t.Fatalf("FieldsScore(empty spec) = %v", s)
	}

	// Пустая карточка против пустого эталона: обязательное поле всё равно «не заполнено».
	spec := FieldSpec{Required: defaultRequired()}
	score, errs := EvaluateFields(nil, nil, spec, nil)
	if len(errs) != len(spec.Required) {
		t.Fatalf("errors = %s", mustJSON(t, errs))
	}
	for i, e := range errs {
		if e.Kind != public.FieldErrorKindMissing || e.Field != spec.Required[i] || e.Weight != 1 {
			t.Errorf("errs[%d] = %+v", i, e)
		}
		if e.Label == "" || strings.Contains(e.Label, ".") {
			t.Errorf("errs[%d] label = %q", i, e.Label)
		}
		if e.Expected != nil || e.Actual != nil {
			t.Errorf("errs[%d]: expected/actual should be omitted, got %v / %v", i, e.Expected, e.Actual)
		}
	}
	if score != 0 {
		t.Fatalf("score = %v, want 0", score)
	}
	// missing без значений: ключи expected/actual опущены в JSON.
	js := mustJSON(t, errs[0])
	if strings.Contains(js, "expected") || strings.Contains(js, "actual") {
		t.Fatalf("JSON = %s", js)
	}
}

// defaultRequired — копия дефолтного списка (не держим ссылку на общий срез).
func defaultRequired() []string {
	return []string{"applicant.name", "applicant.status", "address.raw", "incidentTypeIds", "description"}
}

type fieldCase struct {
	name    string
	path    string
	student func(d *public.IncidentCardDraft)
	etalon  func(d *public.IncidentCardDraft)
	spec    func(s *FieldSpec)
	kind    public.FieldErrorKind // "" — ошибки нет
	exp     any                   // ожидаемый FieldError.Expected (JSON-сравнение); nil — не проверять
	act     any                   // ожидаемый FieldError.Actual; nil — не проверять
	omitAct bool                  // Actual должен отсутствовать
}

func runFieldCases(t *testing.T, cases []fieldCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			st, et := studentFire(), etalonFire()
			if c.student != nil {
				c.student(st)
			}
			if c.etalon != nil {
				c.etalon(et)
			}
			spec := FieldSpec{Required: []string{c.path}}
			if c.spec != nil {
				c.spec(&spec)
			}
			errs := FieldErrors(st, et, spec, testCatalog())
			if c.kind == "" {
				if len(errs) != 0 {
					t.Fatalf("unexpected errors: %s", mustJSON(t, errs))
				}
				return
			}
			if len(errs) != 1 {
				t.Fatalf("want 1 error of kind %q, got %s", c.kind, mustJSON(t, errs))
			}
			e := errs[0]
			if e.Kind != c.kind {
				t.Fatalf("kind = %q, want %q (%s)", e.Kind, c.kind, mustJSON(t, e))
			}
			if e.Field != CanonicalPath(c.path) {
				t.Fatalf("field = %q, want %q", e.Field, CanonicalPath(c.path))
			}
			if c.exp != nil {
				if got, want := mustJSON(t, e.Expected), mustJSON(t, c.exp); got != want {
					t.Errorf("expected = %s, want %s", got, want)
				}
			}
			if c.act != nil {
				if got, want := mustJSON(t, e.Actual), mustJSON(t, c.act); got != want {
					t.Errorf("actual = %s, want %s", got, want)
				}
			}
			if c.omitAct && e.Actual != nil {
				t.Errorf("actual = %#v, want omitted", e.Actual)
			}
		})
	}
}

func TestFieldErrorsName(t *testing.T) {
	t.Parallel()
	runFieldCases(t, []fieldCase{
		{name: "match ignoring order/case", path: "applicant.name"},
		{name: "nil → missing", path: "applicant.name", student: func(d *public.IncidentCardDraft) { d.Applicant.Name = nil },
			kind: public.FieldErrorKindMissing, exp: "Мария Иванова", omitAct: true},
		{name: "blank → missing", path: "applicant.name", student: func(d *public.IncidentCardDraft) { d.Applicant.Name = ptr("  ,  ") },
			kind: public.FieldErrorKindMissing},
		{name: "different surname → wrong", path: "applicant.name",
			student: func(d *public.IncidentCardDraft) { d.Applicant.Name = ptr("Мария Петрова") },
			kind:    public.FieldErrorKindWrong, exp: "Мария Иванова", act: "Мария Петрова"},
		{name: "partial name → wrong", path: "applicant.name",
			student: func(d *public.IncidentCardDraft) { d.Applicant.Name = ptr("Мария") },
			kind:    public.FieldErrorKindWrong},
		{name: "ё = е", path: "applicant.name",
			etalon:  func(d *public.IncidentCardDraft) { d.Applicant.Name = ptr("Пётр Семёнов") },
			student: func(d *public.IncidentCardDraft) { d.Applicant.Name = ptr("семенов петр") }},
		// Регрессия: типографский дефис LLM-эталона против обычного дефиса ввода.
		{name: "typographic hyphen in double surname", path: "applicant.name",
			etalon: func(d *public.IncidentCardDraft) {
				d.Applicant.Name = ptr("Мария Римская‑Корсакова")
			},
			student: func(d *public.IncidentCardDraft) {
				d.Applicant.Name = ptr("Римская-Корсакова Мария")
			}},
		// Регрессия: « - » между словами — разделитель, а не слово.
		{name: "spaced hyphen separator", path: "applicant.name",
			student: func(d *public.IncidentCardDraft) { d.Applicant.Name = ptr("Иванова - Мария") }},
		{name: "etalon empty, answer present → ok", path: "applicant.name",
			etalon: func(d *public.IncidentCardDraft) { d.Applicant.Name = nil }},
		{name: "both empty → missing (field is required)", path: "applicant.name",
			etalon:  func(d *public.IncidentCardDraft) { d.Applicant.Name = nil },
			student: func(d *public.IncidentCardDraft) { d.Applicant.Name = nil },
			kind:    public.FieldErrorKindMissing, omitAct: true},
	})
}

func TestFieldErrorsScalarsAndPhones(t *testing.T) {
	t.Parallel()
	runFieldCases(t, []fieldCase{
		{name: "status nil → missing", path: "applicant.status",
			student: func(d *public.IncidentCardDraft) { d.Applicant.Status = nil },
			kind:    public.FieldErrorKindMissing, exp: "очевидец"},
		{name: "status differs → wrong", path: "applicant.status",
			student: func(d *public.IncidentCardDraft) { d.Applicant.Status = ptr(public.Пострадавший) },
			kind:    public.FieldErrorKindWrong, exp: "очевидец", act: "пострадавший"},
		{name: "phone formats match", path: "phones.aon"},
		{name: "phone differs → wrong", path: "phones.aon",
			student: func(d *public.IncidentCardDraft) { d.Phones.Aon = ptr("+7 903 511-33-68") },
			kind:    public.FieldErrorKindWrong, exp: "+7 (903) 511-33-67", act: "+7 903 511-33-68"},
		{name: "phone without digits → missing", path: "phones.aon",
			student: func(d *public.IncidentCardDraft) { d.Phones.Aon = ptr("не сообщил") },
			kind:    public.FieldErrorKindMissing, omitAct: true},
		{name: "phone via contract alias", path: "applicant.phone"},
		{name: "channel compared as text", path: "phones.channel",
			etalon:  func(d *public.IncidentCardDraft) { d.Phones.Channel = ptr("Мобильная связь") },
			student: func(d *public.IncidentCardDraft) { d.Phones.Channel = ptr("мобильная  связь.") }},
		{name: "address source", path: "address.source",
			etalon:  func(d *public.IncidentCardDraft) { d.Address.Source = ptr(public.AddressSourceYandexMap) },
			student: func(d *public.IncidentCardDraft) { d.Address.Source = ptr(public.AddressSourceManual) },
			kind:    public.FieldErrorKindWrong, exp: "yandex_map", act: "manual"},
		{name: "victims count equal", path: "flags.victimsCount"},
		{name: "victims count differs", path: "flags.victimsCount",
			student: func(d *public.IncidentCardDraft) { d.Flags.VictimsCount = ptr(2) },
			kind:    public.FieldErrorKindWrong, exp: 1, act: 2},
		{name: "victims count nil → missing", path: "flags.victimsCount",
			student: func(d *public.IncidentCardDraft) { d.Flags.VictimsCount = nil },
			kind:    public.FieldErrorKindMissing, exp: 1, omitAct: true},
		{name: "victims count: etalon empty, zero answered → ok", path: "flags.victimsCount",
			etalon:  func(d *public.IncidentCardDraft) { d.Flags.VictimsCount = nil },
			student: func(d *public.IncidentCardDraft) { d.Flags.VictimsCount = ptr(0) }},
		{name: "coordinates equal to 4 digits", path: "address.lat"},
		{name: "coordinates differ", path: "address.lat",
			student: func(d *public.IncidentCardDraft) { d.Address.Lat = ptr(float32(55.7700)) },
			kind:    public.FieldErrorKindWrong},
		{name: "coordinates absent → missing", path: "address.lon",
			etalon: func(d *public.IncidentCardDraft) { d.Address.Lon = ptr(float32(37.6)) },
			kind:   public.FieldErrorKindMissing},
	})
}

func TestFieldErrorsFreeText(t *testing.T) {
	t.Parallel()
	runFieldCases(t, []fieldCase{
		// Смысл описания — слой semantic; здесь только наличие.
		{name: "description differs from etalon → ok", path: "description"},
		{name: "description blank → missing", path: "description",
			student: func(d *public.IncidentCardDraft) { d.Description = " \n " },
			kind:    public.FieldErrorKindMissing, exp: "Горит квартира на пятом этаже, дым в подъезде.", omitAct: true},
		{name: "actionsTaken missing even when etalon empty", path: "actions_taken",
			kind: public.FieldErrorKindMissing, omitAct: true},
		{name: "actionsTaken present", path: "actionsTaken",
			student: func(d *public.IncidentCardDraft) { d.ActionsTaken = "Передал в 101" }},
		// Регрессия: контрактный путь address.landmark → описательный адрес формы.
		{name: "landmark alias present", path: "address.landmark",
			etalon:  func(d *public.IncidentCardDraft) { d.Address.Descriptive = ptr("напротив школы №14") },
			student: func(d *public.IncidentCardDraft) { d.Address.Descriptive = ptr("у школы") }},
		{name: "landmark alias missing", path: "address.landmark",
			etalon: func(d *public.IncidentCardDraft) { d.Address.Descriptive = ptr("напротив школы №14") },
			kind:   public.FieldErrorKindMissing, exp: "напротив школы №14"},
	})
}

func TestFieldErrorsFlags(t *testing.T) {
	t.Parallel()
	runFieldCases(t, []fieldCase{
		{name: "both true", path: "flags.victimsPresent"},
		{name: "etalon yes, answered no → missing", path: "flags.victimsPresent",
			student: func(d *public.IncidentCardDraft) { d.Flags.VictimsPresent = false },
			kind:    public.FieldErrorKindMissing, exp: true, act: false},
		{name: "etalon no, answered yes → wrong", path: "flags.noContact",
			student: func(d *public.IncidentCardDraft) { d.Flags.NoContact = true },
			kind:    public.FieldErrorKindWrong, exp: false, act: true},
		{name: "both no → ok (no is an answer)", path: "flags.callDropped"},
		{name: "contract casualties → victimsPresent", path: "casualties",
			student: func(d *public.IncidentCardDraft) { d.Flags.VictimsPresent = false },
			kind:    public.FieldErrorKindMissing},
		{name: "contract casualties.injured → victimsCount", path: "casualties.injured",
			student: func(d *public.IncidentCardDraft) { d.Flags.VictimsCount = ptr(3) },
			kind:    public.FieldErrorKindWrong, exp: 1, act: 3},
		// Флажок-указатель: эталон не задан — любой ответ верен, даже «не трогал».
		{name: "pointer flag, etalon unset", path: "applicant.foreignLanguage"},
		{name: "pointer flag, etalon unset, answered yes", path: "applicant.foreignLanguage",
			student: func(d *public.IncidentCardDraft) { d.Applicant.ForeignLanguage = ptr(true) }},
		{name: "pointer flag, etalon yes, untouched → missing", path: "applicant.foreignLanguage",
			etalon: func(d *public.IncidentCardDraft) { d.Applicant.ForeignLanguage = ptr(true) },
			kind:   public.FieldErrorKindMissing, exp: true, act: false},
		{name: "pointer flag, etalon no, untouched → ok", path: "phones.foreign",
			etalon: func(d *public.IncidentCardDraft) { d.Phones.Foreign = ptr(false) }},
		{name: "snake path", path: "applicant.foreign_language",
			etalon:  func(d *public.IncidentCardDraft) { d.Applicant.ForeignLanguage = ptr(false) },
			student: func(d *public.IncidentCardDraft) { d.Applicant.ForeignLanguage = ptr(true) },
			kind:    public.FieldErrorKindWrong},
	})
}

func TestFieldErrorsAddress(t *testing.T) {
	t.Parallel()
	runFieldCases(t, []fieldCase{
		{name: "raw with details and country matches", path: "address.raw"},
		{name: "raw other house → wrong", path: "address.raw",
			student: func(d *public.IncidentCardDraft) { d.Address.Raw = "Москва, Тверская, 14" },
			kind:    public.FieldErrorKindWrong, exp: "Москва, Тверская улица, 12", act: "Москва, Тверская, 14"},
		{name: "raw without street of etalon → wrong", path: "address.raw",
			student: func(d *public.IncidentCardDraft) { d.Address.Raw = "Москва, 12" },
			kind:    public.FieldErrorKindWrong},
		{name: "raw empty, composed from parts", path: "address.raw",
			student: func(d *public.IncidentCardDraft) {
				d.Address.Raw = ""
				d.Address.Settlement = ptr("Москва")
			}},
		{name: "raw and parts empty → missing", path: "address.raw",
			student: func(d *public.IncidentCardDraft) {
				d.Address = public.AddressDraft{}
			},
			kind: public.FieldErrorKindMissing, exp: "Москва, Тверская улица, 12", omitAct: true},
		{name: "raw of etalon composed from parts", path: "address.raw",
			etalon: func(d *public.IncidentCardDraft) { d.Address.Raw = "" }},
		{name: "street with type words", path: "address.street"},
		{name: "street differs", path: "address.street",
			student: func(d *public.IncidentCardDraft) { d.Address.Street = ptr("Тверская-Ямская") },
			kind:    public.FieldErrorKindWrong, exp: "Тверская", act: "Тверская-Ямская"},
		{name: "house with prefix", path: "address.house"},
		{name: "house literal", path: "address.house",
			etalon:  func(d *public.IncidentCardDraft) { d.Address.House = ptr("12А") },
			student: func(d *public.IncidentCardDraft) { d.Address.House = ptr("д. 12 а") }},
		{name: "entrance with word", path: "address.entrance"},
		{name: "floor with leading zero", path: "address.floor"},
		{name: "city alias → settlement", path: "address.city",
			student: func(d *public.IncidentCardDraft) { d.Address.Settlement = ptr("г. Москва") }},
		{name: "apartment missing", path: "address.apartment",
			etalon: func(d *public.IncidentCardDraft) { d.Address.Apartment = ptr("45") },
			kind:   public.FieldErrorKindMissing, exp: "45"},
	})
}

func TestFieldErrorsTypes(t *testing.T) {
	t.Parallel()
	runFieldCases(t, []fieldCase{
		{name: "same type id", path: "incidentTypeIds"},
		{name: "etalon refers to type code", path: "incidentTypeIds",
			etalon: func(d *public.IncidentCardDraft) { d.IncidentTypeIds = []string{"101"} }},
		{name: "contract alias category_code", path: "category_code"},
		{name: "no type → missing with readable expected", path: "incidentTypeIds",
			student: func(d *public.IncidentCardDraft) { d.IncidentTypeIds = []string{" "} },
			kind:    public.FieldErrorKindMissing, exp: []string{"Пожар"}, omitAct: true},
		{name: "other type → wrong with names", path: "incidentTypeIds",
			student: func(d *public.IncidentCardDraft) { d.IncidentTypeIds = []string{"t-gas"} },
			kind:    public.FieldErrorKindWrong, exp: []string{"Пожар"}, act: []string{"Запах газа"}},
		{name: "extra type → wrong", path: "incidentTypeIds",
			student: func(d *public.IncidentCardDraft) { d.IncidentTypeIds = []string{"t-fire", "t-gas"} },
			kind:    public.FieldErrorKindWrong, act: []string{"Пожар", "Запах газа"}},
		{name: "unknown id shown as is", path: "incidentTypeIds",
			student: func(d *public.IncidentCardDraft) { d.IncidentTypeIds = []string{"t-unknown"} },
			kind:    public.FieldErrorKindWrong, act: []string{"t-unknown"}},
		{name: "duplicates ignored", path: "incidentTypeIds",
			student: func(d *public.IncidentCardDraft) { d.IncidentTypeIds = []string{"t-fire", "101", "t-fire"} }},
	})
}

func TestFieldErrorsAttributes(t *testing.T) {
	t.Parallel()
	runFieldCases(t, []fieldCase{
		{name: "code match, case-insensitive", path: "attributes.fire_sign"},
		{name: "other value → labels", path: "attributes.where",
			student: func(d *public.IncidentCardDraft) { d.Attributes["where"] = "flat" },
			kind:    public.FieldErrorKindWrong, exp: "Жилой дом", act: "Квартира"},
		{name: "absent → missing", path: "attributes.where",
			student: func(d *public.IncidentCardDraft) { delete(d.Attributes, "where") },
			kind:    public.FieldErrorKindMissing, exp: "Жилой дом", omitAct: true},
		{name: "nil attributes map → missing", path: "attributes.where",
			student: func(d *public.IncidentCardDraft) { d.Attributes = nil },
			kind:    public.FieldErrorKindMissing},
		{name: "multi-select order-insensitive", path: "attributes.indoor_objects"},
		{name: "multi-select subset → wrong", path: "attributes.indoor_objects",
			student: func(d *public.IncidentCardDraft) { d.Attributes["indoor_objects"] = []any{"apartment"} },
			kind:    public.FieldErrorKindWrong, exp: []string{"Квартира", "Газовая колонка"}, act: []string{"Квартира"}},
		{name: "multi-select empty list → missing", path: "attributes.indoor_objects",
			student: func(d *public.IncidentCardDraft) { d.Attributes["indoor_objects"] = []any{} },
			kind:    public.FieldErrorKindMissing},
		{name: "number: string answer", path: "attributes.floors"},
		{name: "number: int answer", path: "attributes.floors",
			student: func(d *public.IncidentCardDraft) { d.Attributes["floors"] = 5 }},
		{name: "number: json.Number answer", path: "attributes.floors",
			student: func(d *public.IncidentCardDraft) { d.Attributes["floors"] = json.Number("5.0") }},
		{name: "number: leading zero", path: "attributes.floors",
			student: func(d *public.IncidentCardDraft) { d.Attributes["floors"] = "05" }},
		{name: "number differs", path: "attributes.floors",
			student: func(d *public.IncidentCardDraft) { d.Attributes["floors"] = float64(9) },
			kind:    public.FieldErrorKindWrong, exp: 5, act: 9},
		{name: "bool yes", path: "attributes.gasified"},
		{name: "bool: etalon yes, absent → missing", path: "attributes.gasified",
			student: func(d *public.IncidentCardDraft) { delete(d.Attributes, "gasified") },
			kind:    public.FieldErrorKindMissing, exp: true, omitAct: true},
		{name: "bool: etalon no, absent → ok", path: "attributes.no_gas"},
		{name: "bool: etalon no, answered yes → wrong", path: "attributes.no_gas",
			student: func(d *public.IncidentCardDraft) { d.Attributes["no_gas"] = true },
			kind:    public.FieldErrorKindWrong, exp: false, act: true},
		{name: "bool vs string → wrong", path: "attributes.gasified",
			student: func(d *public.IncidentCardDraft) { d.Attributes["gasified"] = "yes" },
			kind:    public.FieldErrorKindWrong},
		{name: "etalon empty, any answer ok", path: "attributes.extra_note",
			student: func(d *public.IncidentCardDraft) { d.Attributes["extra_note"] = "что угодно" }},
		{name: "object value is not an answer", path: "attributes.where",
			student: func(d *public.IncidentCardDraft) { d.Attributes["where"] = map[string]any{"x": 1} },
			kind:    public.FieldErrorKindMissing},
		{name: "unknown code shown as is", path: "attributes.people_threat",
			student: func(d *public.IncidentCardDraft) { d.Attributes["people_threat"] = "no" },
			kind:    public.FieldErrorKindWrong, exp: "yes", act: "no"},
	})
}

func TestFieldErrorsServices(t *testing.T) {
	t.Parallel()
	svc := func(code string) public.AssignedService { return public.AssignedService{Code: code} }
	runFieldCases(t, []fieldCase{
		{name: "same services", path: "services"},
		{name: "contract alias", path: "services_to_notify"},
		{name: "none → missing", path: "services",
			student: func(d *public.IncidentCardDraft) { d.Services = nil },
			kind:    public.FieldErrorKindMissing, exp: []string{"Служба 101"}, omitAct: true},
		{name: "other service → wrong", path: "services",
			student: func(d *public.IncidentCardDraft) { d.Services = []public.AssignedService{svc("103")} },
			kind:    public.FieldErrorKindWrong, exp: []string{"Служба 101"}, act: []string{"Служба 103"}},
		{name: "extra service → extra", path: "services",
			student: func(d *public.IncidentCardDraft) {
				d.Services = append(d.Services, svc("103"))
			},
			kind: public.FieldErrorKindExtra, exp: []string{"Служба 101"}, act: []string{"Служба 101", "Служба 103"}},
		{name: "code case-insensitive", path: "services",
			etalon:  func(d *public.IncidentCardDraft) { d.Services = []public.AssignedService{svc("gormost")} },
			student: func(d *public.IncidentCardDraft) { d.Services = []public.AssignedService{svc("GORMOST")} }},
		{name: "code resolved by serviceId", path: "services",
			student: func(d *public.IncidentCardDraft) {
				d.Services = []public.AssignedService{{ServiceId: "s-101"}}
			}},
		{name: "etalon codes from spec when card has none", path: "services",
			etalon: func(d *public.IncidentCardDraft) { d.Services = nil },
			spec:   func(s *FieldSpec) { s.ServiceCodes = []string{" 101 ", ""} }},
		{name: "spec codes: missing shows catalog name", path: "services",
			etalon:  func(d *public.IncidentCardDraft) { d.Services = nil },
			student: func(d *public.IncidentCardDraft) { d.Services = nil },
			spec:    func(s *FieldSpec) { s.ServiceCodes = []string{"101", "103"} },
			kind:    public.FieldErrorKindMissing, exp: []string{"Служба 101", "Служба 103"}},
		{name: "spec codes ignored when etalon has services", path: "services",
			spec: func(s *FieldSpec) { s.ServiceCodes = []string{"104"} }},
		{name: "unknown code: name from card, then code", path: "services",
			etalon: func(d *public.IncidentCardDraft) { d.Services = nil },
			student: func(d *public.IncidentCardDraft) {
				d.Services = []public.AssignedService{{Code: "x-1", ShortName: "Своя служба"}}
			},
			spec: func(s *FieldSpec) { s.ServiceCodes = []string{"x-2"} },
			kind: public.FieldErrorKindWrong, exp: []string{"x-2"}, act: []string{"Своя служба"}},
		{name: "catalog without short name uses full name", path: "services",
			student: func(d *public.IncidentCardDraft) { d.Services = []public.AssignedService{svc("noname")} },
			kind:    public.FieldErrorKindWrong, act: []string{"Служба без краткого имени"}},
		{name: "etalon without services, answer present → ok", path: "services",
			etalon: func(d *public.IncidentCardDraft) { d.Services = nil }},
	})
}

// Регрессия: путь вне формы АРМ раньше давал вечное «не заполнено» и снижал балл.
func TestFieldErrorsUnknownPathSkipped(t *testing.T) {
	t.Parallel()
	spec := FieldSpec{Required: []string{"foo.bar", "applicant.name", "applicantName", "attributes."}}
	score, errs := EvaluateFields(studentFire(), etalonFire(), spec, testCatalog())
	if len(errs) != 0 || score != 100 {
		t.Fatalf("score %v, errors %s", score, mustJSON(t, errs))
	}
	// Все пути неизвестны → как будто обязательных нет.
	spec = FieldSpec{Required: []string{"foo", "bar.baz"}}
	score, errs = EvaluateFields(nil, nil, spec, nil)
	if len(errs) != 0 || score != 100 {
		t.Fatalf("all unknown: score %v, errors %s", score, mustJSON(t, errs))
	}
}

func TestFieldErrorsOrderAndDedup(t *testing.T) {
	t.Parallel()
	st := studentFire()
	st.Applicant.Name = nil
	st.IncidentTypeIds = nil
	st.Description = ""
	spec := FieldSpec{Required: []string{
		"description", "category_code", "incidentTypeIds", " incidentTypeIds ", "applicant.name", "services_to_notify", "services",
	}}
	errs := FieldErrors(st, etalonFire(), spec, testCatalog())
	var got []string
	for _, e := range errs {
		got = append(got, e.Field)
	}
	want := []string{"description", "incidentTypeIds", "applicant.name"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fields = %v, want %v", got, want)
	}
	// Подписи — из каталога, иначе встроенные.
	labels := []string{errs[0].Label, errs[1].Label, errs[2].Label}
	if !reflect.DeepEqual(labels, []string{"Описание со слов заявителя", "Тип происшествия", "ФИО заявителя"}) {
		t.Fatalf("labels = %v", labels)
	}
}

func TestFieldsScore(t *testing.T) {
	t.Parallel()
	missing := func(f string) public.FieldError {
		return public.FieldError{Field: f, Kind: public.FieldErrorKindMissing}
	}
	extra := func(f string) public.FieldError {
		return public.FieldError{Field: f, Kind: public.FieldErrorKindExtra}
	}
	four := []string{"applicant.name", "address.raw", "incidentTypeIds", "services"}
	cases := []struct {
		name string
		spec FieldSpec
		errs []public.FieldError
		want float64
	}{
		{"no errors", FieldSpec{Required: four}, nil, 100},
		{"one of four missing", FieldSpec{Required: four}, []public.FieldError{missing("address.raw")}, 75},
		{"extra costs half", FieldSpec{Required: four}, []public.FieldError{extra("services")}, 87.5},
		{"all missing", FieldSpec{Required: four},
			[]public.FieldError{missing("applicant.name"), missing("address.raw"), missing("incidentTypeIds"), missing("services")}, 0},
		{"same field counted once", FieldSpec{Required: four},
			[]public.FieldError{missing("address.raw"), missing("address.raw"), {Field: "address.raw", Kind: public.FieldErrorKindWrong}}, 75},
		{"errors outside required ignored", FieldSpec{Required: four}, []public.FieldError{missing("flags.blocked")}, 100},
		{"snake field in error is canonicalized", FieldSpec{Required: four}, []public.FieldError{missing("category_code")}, 75},
		{"weights", FieldSpec{Required: []string{"applicant.name", "address.raw"}, Weights: map[string]float64{"applicant.name": 3}},
			[]public.FieldError{missing("applicant.name")}, 25},
		{"weight by canonical key", FieldSpec{Required: []string{"category_code", "address.raw"}, Weights: map[string]float64{"incidentTypeIds": 3}},
			[]public.FieldError{missing("incidentTypeIds")}, 25},
		{"weight by original key", FieldSpec{Required: []string{"category_code", "address.raw"}, Weights: map[string]float64{"category_code": 3}},
			[]public.FieldError{missing("incidentTypeIds")}, 25},
		{"invalid weights fall back to 1", FieldSpec{Required: []string{"applicant.name", "address.raw"},
			Weights: map[string]float64{"applicant.name": -2, "address.raw": nanValue()}},
			[]public.FieldError{missing("applicant.name")}, 50},
		{"zero weight field does not cost", FieldSpec{Required: []string{"applicant.name", "address.raw"}, Weights: map[string]float64{"applicant.name": 0}},
			[]public.FieldError{missing("applicant.name")}, 100},
		{"all weights zero → 100", FieldSpec{Required: []string{"applicant.name"}, Weights: map[string]float64{"applicant.name": 0}},
			[]public.FieldError{missing("applicant.name")}, 100},
		{"thirds are rounded", FieldSpec{Required: []string{"applicant.name", "address.raw", "incidentTypeIds"}},
			[]public.FieldError{missing("applicant.name")}, 66.67},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := FieldsScore(c.errs, c.spec); got != c.want {
				t.Errorf("FieldsScore = %v, want %v", got, c.want)
			}
		})
	}
}

func nanValue() float64 {
	var z float64
	return z / z
}

// Больше 32 обязательных полей — счётчик уходит из буфера на стеке в кучу.
func TestFieldsScoreManyFields(t *testing.T) {
	t.Parallel()
	var req []string
	et := &public.IncidentCardDraft{Attributes: map[string]any{}}
	st := &public.IncidentCardDraft{Attributes: map[string]any{}}
	for i := range 40 {
		code := fmt.Sprintf("a%02d", i)
		req = append(req, "attributes."+code)
		et.Attributes[code] = "v"
		if i%2 == 0 {
			st.Attributes[code] = "v"
		}
	}
	score, errs := EvaluateFields(st, et, FieldSpec{Required: req}, nil)
	if len(errs) != 20 || score != 50 {
		t.Fatalf("score %v, %d errors", score, len(errs))
	}
	if errs[0].Label != "Признак опросной карты" {
		t.Fatalf("label = %q", errs[0].Label)
	}
}

func TestFieldErrorWeightReported(t *testing.T) {
	t.Parallel()
	st := studentFire()
	st.Applicant.Name = nil
	spec := FieldSpec{Required: []string{"applicant.name"}, Weights: map[string]float64{"applicant.name": 2.5}}
	errs := FieldErrors(st, etalonFire(), spec, nil)
	if len(errs) != 1 || errs[0].Weight != 2.5 {
		t.Fatalf("errs = %s", mustJSON(t, errs))
	}
}

// Оценка не меняет карточки (черновик и эталон читаются из БД и пишутся обратно другими).
func TestFieldErrorsDoesNotMutateInputs(t *testing.T) {
	t.Parallel()
	st, et := studentFire(), etalonFire()
	st.IncidentTypeIds = []string{"t-gas", "t-fire"}
	st.Services = append(st.Services, public.AssignedService{Code: "103"})
	before := mustJSON(t, st) + mustJSON(t, et)
	spec := FieldSpec{Required: allFirePaths, ServiceCodes: []string{"104", "101"}}
	_ = FieldErrors(st, et, spec, testCatalog())
	if after := mustJSON(t, st) + mustJSON(t, et); after != before {
		t.Fatalf("inputs mutated:\nbefore %s\nafter  %s", before, after)
	}
	if spec.ServiceCodes[0] != "104" {
		t.Fatalf("spec mutated: %v", spec.ServiceCodes)
	}
}

func TestFieldErrorsDeterministic(t *testing.T) {
	t.Parallel()
	st := studentFire()
	st.Applicant.Name = ptr("Петрова")
	st.IncidentTypeIds = []string{"t-gas"}
	st.Attributes["indoor_objects"] = []any{"gas_column"}
	st.Services = append(st.Services, public.AssignedService{Code: "103"}, public.AssignedService{Code: "104"})
	spec := FieldSpec{Required: allFirePaths}
	first := mustJSON(t, FieldErrors(st, etalonFire(), spec, testCatalog()))
	for range 20 {
		if again := mustJSON(t, FieldErrors(st, etalonFire(), spec, testCatalog())); again != first {
			t.Fatalf("non-deterministic:\n%s\n%s", first, again)
		}
	}
}

// Эталон, пришедший из LLM как контрактный IncidentCard, после convert.CardToDraft сверяется
// с формой АРМ и по каноническим, и по контрактным путям.
func TestFieldErrorsContractEtalon(t *testing.T) {
	t.Parallel()
	cat := testCatalog()
	card := components.IncidentCard{
		CategoryCode:     ptr("101"),
		Address:          &components.Address{Raw: ptr("Москва, Тверская улица, 12"), City: ptr("Москва"), Street: ptr("Тверская"), House: ptr("12"), Landmark: ptr("у школы")},
		ServicesToNotify: &[]string{"101", "103"},
		Description:      ptr("Горит квартира"),
	}
	card.Applicant = &struct {
		Name  *string `json:"name,omitempty"`
		Phone *string `json:"phone,omitempty"`
	}{Name: ptr("Мария Иванова")}
	inj := 2
	card.Casualties = &struct {
		Dead    *int `json:"dead,omitempty"`
		Injured *int `json:"injured,omitempty"`
		Trapped *int `json:"trapped,omitempty"`
	}{Injured: &inj}
	etalon := convert.CardToDraft(&card, cat)

	st := studentFire()
	st.Address.Settlement = ptr("г. Москва")
	st.Address.Descriptive = ptr("возле школы")
	st.Flags.VictimsCount = ptr(2)
	st.Services = append(st.Services, public.AssignedService{Code: "103"})

	spec := FieldSpec{Required: []string{
		"category_code", "address.raw", "address.city", "address.landmark", "applicant.name",
		"casualties", "casualties.injured", "services_to_notify", "description",
	}}
	score, errs := EvaluateFields(st, &etalon, spec, cat)
	if len(errs) != 0 || score != 100 {
		t.Fatalf("score %v, errors %s", score, mustJSON(t, errs))
	}
}

func TestAttrVal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		v     any
		empty bool
		set   []string
		isB   bool
	}{
		{"nil", nil, true, nil, false},
		{"blank string", "  ", true, nil, false},
		{"string", " Flame ", false, []string{"flame"}, false},
		{"bool", false, false, nil, true},
		{"float64", 5.0, false, []string{"5"}, false},
		{"float32", float32(2.5), false, []string{"2.5"}, false},
		{"int", 7, false, []string{"7"}, false},
		{"int64", int64(-3), false, []string{"-3"}, false},
		{"int32", int32(0), false, []string{"0"}, false},
		{"NaN", nanValue(), true, nil, false},
		{"json.Number", json.Number("05"), false, []string{"5"}, false},
		{"[]string", []string{"b", " A ", "", "b"}, false, []string{"a", "b"}, false},
		{"[]string blanks", []string{"", " "}, true, nil, false},
		{"[]any mixed", []any{"Yes", 2.0, true, json.Number("3,0"), 4, nanValue(), map[string]any{}, "", false},
			false, []string{"2", "3", "4", "false", "true", "yes"}, false},
		{"[]any junk only", []any{nil, map[string]any{}, ""}, true, nil, false},
		{"object", map[string]any{"a": 1}, true, nil, false},
		{"unsupported", struct{}{}, true, nil, false},
	}
	for _, c := range cases {
		got := attrVal(c.v)
		if got.empty != c.empty || got.isBool != c.isB || !reflect.DeepEqual(got.set, c.set) {
			t.Errorf("%s: attrVal(%#v) = %+v, want empty=%v isBool=%v set=%v", c.name, c.v, got, c.empty, c.isB, c.set)
		}
	}
}

func TestAttrReadable(t *testing.T) {
	t.Parallel()
	cat := testCatalog()
	cases := []struct {
		name string
		raw  any
		want string // JSON
	}{
		{"code → label", "house", `"Жилой дом"`},
		{"unknown code as is", "tent", `"tent"`},
		{"[]string labels", []string{"house", " ", "flat"}, `["Жилой дом","Квартира"]`},
		{"[]any mixed", []any{"flat", 3.5, true, false, json.Number("7"), ""}, `["Квартира","3.5","да","нет","7"]`},
		{"number as is", 5.0, `5`},
		{"bool as is", true, `true`},
	}
	for _, c := range cases {
		if got := mustJSON(t, attrReadable("where", c.raw, cat)); got != c.want {
			t.Errorf("%s: attrReadable = %s, want %s", c.name, got, c.want)
		}
	}
	// Без каталога — коды как есть.
	if got := mustJSON(t, attrReadable("where", []string{"house"}, nil)); got != `["house"]` {
		t.Errorf("nil catalog: %s", got)
	}
}

// Каждая часть структурного адреса читается из своего поля формы.
func TestFieldErrorsAllAddressParts(t *testing.T) {
	t.Parallel()
	parts := map[string]func(a *public.AddressDraft) **string{
		"apartment":  func(a *public.AddressDraft) **string { return &a.Apartment },
		"building":   func(a *public.AddressDraft) **string { return &a.Building },
		"country":    func(a *public.AddressDraft) **string { return &a.Country },
		"district":   func(a *public.AddressDraft) **string { return &a.District },
		"entrance":   func(a *public.AddressDraft) **string { return &a.Entrance },
		"floor":      func(a *public.AddressDraft) **string { return &a.Floor },
		"house":      func(a *public.AddressDraft) **string { return &a.House },
		"object":     func(a *public.AddressDraft) **string { return &a.Object },
		"okrug":      func(a *public.AddressDraft) **string { return &a.Okrug },
		"region":     func(a *public.AddressDraft) **string { return &a.Region },
		"settlement": func(a *public.AddressDraft) **string { return &a.Settlement },
		"street":     func(a *public.AddressDraft) **string { return &a.Street },
		"structure":  func(a *public.AddressDraft) **string { return &a.Structure },
	}
	for name, field := range parts {
		path := "address." + name
		et, st := &public.IncidentCardDraft{}, &public.IncidentCardDraft{}
		*field(&et.Address) = ptr("Значение 7")
		*field(&st.Address) = ptr("значение 07")
		spec := FieldSpec{Required: []string{path}}
		if errs := FieldErrors(st, et, spec, nil); len(errs) != 0 {
			t.Errorf("%s: equal values reported: %s", path, mustJSON(t, errs))
		}
		*field(&st.Address) = ptr("Другое 8")
		errs := FieldErrors(st, et, spec, nil)
		if len(errs) != 1 || errs[0].Kind != public.FieldErrorKindWrong || errs[0].Label == labelFallback {
			t.Errorf("%s: different values: %s", path, mustJSON(t, errs))
		}
		*field(&st.Address) = nil
		if errs := FieldErrors(st, et, spec, nil); len(errs) != 1 || errs[0].Kind != public.FieldErrorKindMissing {
			t.Errorf("%s: nil answer: %s", path, mustJSON(t, errs))
		}
	}
}

func TestClamp100(t *testing.T) {
	t.Parallel()
	for in, want := range map[float64]float64{-1: 0, 0: 0, 55.5: 55.5, 100: 100, 101: 100} {
		if got := clamp100(in); got != want {
			t.Errorf("clamp100(%v) = %v", in, got)
		}
	}
	if clamp100(nanValue()) != 0 {
		t.Error("clamp100(NaN) != 0")
	}
}
