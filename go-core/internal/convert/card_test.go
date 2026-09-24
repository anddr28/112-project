package convert

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
)

func TestEmptyDraftMirrorsFrontend(t *testing.T) {
	t.Parallel()

	d := EmptyDraft()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	// emptyCard() фронта: 16 строковых полей адреса = "", массивы [], карты {}
	addr := m["address"].(map[string]any)
	for _, k := range []string{"raw", "country", "region", "settlement", "object", "okrug", "district", "street",
		"house", "building", "structure", "apartment", "entrance", "floor", "code", "descriptive"} {
		if v, ok := addr[k]; !ok || v != "" {
			t.Errorf("address.%s = %v (есть: %v), want \"\"", k, v, ok)
		}
	}
	if len(addr) != 16 {
		t.Errorf("address: %d ключей, want 16 (без lat/lon/source)", len(addr))
	}
	for k, want := range map[string]string{"incidentTypeIds": "[]", "services": "[]", "attributes": "{}"} {
		got, _ := json.Marshal(m[k])
		if string(got) != want {
			t.Errorf("%s = %s, want %s", k, got, want)
		}
	}
	if m["description"] != "" || m["actionsTaken"] != "" {
		t.Error("текстовые поля — пустые строки")
	}
	flags := m["flags"].(map[string]any)
	for _, k := range []string{"victimsPresent", "ambulanceRefusal", "blocked", "noContact", "callDropped"} {
		if flags[k] != false {
			t.Errorf("flags.%s = %v", k, flags[k])
		}
	}
	if s, _ := json.Marshal(m["phones"]); string(s) != "{}" {
		t.Errorf("phones = %s", s)
	}
	if s, _ := json.Marshal(m["applicant"]); string(s) != "{}" {
		t.Errorf("applicant = %s", s)
	}

	// указатели адреса независимы: запись в одно поле не трогает другие
	*d.Address.Street = "Тверская"
	if *d.Address.House != "" || *d.Address.Settlement != "" {
		t.Fatal("поля адреса делят память")
	}
	// каждая пустая карточка — своя
	d2 := EmptyDraft()
	if *d2.Address.Street != "" {
		t.Fatal("EmptyDraft делит память между вызовами")
	}
}

func TestNormalizeDraft(t *testing.T) {
	t.Parallel()

	NormalizeDraft(nil) // не паникует

	var d public.IncidentCardDraft
	d.Services = []public.AssignedService{{Code: "101"}}
	NormalizeDraft(&d)
	if d.IncidentTypeIds == nil || d.Attributes == nil || d.Services == nil {
		t.Fatalf("обязательные массивы/карты: %+v", d)
	}
	if d.Services[0].History == nil || d.Services[0].AllowedNext == nil {
		t.Fatal("history/allowedNext служб не nil")
	}
	b, _ := json.Marshal(d)
	for _, want := range []string{`"incidentTypeIds":[]`, `"attributes":{}`, `"history":[]`, `"allowedNext":[]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("json без %s: %s", want, b)
		}
	}

	// существующие значения не трогаются
	d = public.IncidentCardDraft{IncidentTypeIds: []string{"t-fire"}, Attributes: map[string]any{"where": "house"}}
	NormalizeDraft(&d)
	if len(d.IncidentTypeIds) != 1 || d.Attributes["where"] != "house" {
		t.Fatal("NormalizeDraft изменил данные")
	}
}

func fullDraft() public.IncidentCardDraft {
	d := EmptyDraft()
	d.IncidentTypeIds = []string{"unknown-id", "t-nocode", "t-fire", "t-gas"}
	d.Address.Raw = "  Москва, Тверская ул., д. 14 к. 2  "
	*d.Address.Settlement = "Москва"
	*d.Address.Street = " Тверская "
	*d.Address.House = "14"
	*d.Address.Building = "2"
	*d.Address.Entrance = "3"
	*d.Address.Floor = ""
	*d.Address.Apartment = "45"
	*d.Address.Descriptive = "у аптеки"
	d.Applicant.Name = Ptr("  Иванова Мария  ")
	d.Phones.Aon = Ptr("+7 (903) 511-33-67")
	n := 2
	d.Flags.VictimsPresent = true
	d.Flags.VictimsCount = &n
	d.Services = []public.AssignedService{{Code: "101"}, {Code: "  "}, {Code: "103"}}
	d.Description = "  Горит квартира на пятом этаже  "
	d.ActionsTaken = " "
	d.Attributes = map[string]any{"where": "house", "people_threat": "yes"}
	return d
}

func TestDraftToCard(t *testing.T) {
	t.Parallel()

	cat := testCatalog()

	t.Run("nil", func(t *testing.T) {
		t.Parallel()
		if got := DraftToCard(nil, cat); !reflect.DeepEqual(got, components.IncidentCard{}) {
			t.Fatalf("nil -> %+v", got)
		}
	})

	t.Run("пустая карточка — без пустых фактов", func(t *testing.T) {
		t.Parallel()
		d := EmptyDraft()
		b, _ := json.Marshal(DraftToCard(&d, cat))
		if string(b) != "{}" {
			t.Fatalf("пустая карточка -> %s, want {}", b)
		}
	})

	t.Run("полная", func(t *testing.T) {
		t.Parallel()
		d := fullDraft()
		c := DraftToCard(&d, cat)
		if Deref(c.CategoryCode) != "101" {
			t.Errorf("category_code = %v (первый распознаваемый тип с кодом)", c.CategoryCode)
		}
		a := c.Address
		if a == nil {
			t.Fatal("address nil")
		}
		checks := map[string][2]*string{
			"raw":       {a.Raw, Ptr("Москва, Тверская ул., д. 14 к. 2")},
			"city":      {a.City, Ptr("Москва")},
			"street":    {a.Street, Ptr("Тверская")},
			"house":     {a.House, Ptr("14к2")},
			"entrance":  {a.Entrance, Ptr("3")},
			"floor":     {a.Floor, nil},
			"apartment": {a.Apartment, Ptr("45")},
			"landmark":  {a.Landmark, Ptr("у аптеки")},
		}
		for k, v := range checks {
			if !reflect.DeepEqual(v[0], v[1]) {
				t.Errorf("address.%s = %v, want %v", k, strOrNil(v[0]), strOrNil(v[1]))
			}
		}
		if c.Applicant == nil || Deref(c.Applicant.Name) != "Иванова Мария" || Deref(c.Applicant.Phone) != "+7 (903) 511-33-67" {
			t.Errorf("applicant = %+v (телефон — АОН, если нет сообщённого)", c.Applicant)
		}
		if c.Casualties == nil || Deref(c.Casualties.Injured) != 2 || c.Casualties.Dead != nil {
			t.Errorf("casualties = %+v", c.Casualties)
		}
		if c.ServicesToNotify == nil || !reflect.DeepEqual(*c.ServicesToNotify, []string{"101", "103"}) {
			t.Errorf("services_to_notify = %v", c.ServicesToNotify)
		}
		if Deref(c.Description) != "Горит квартира на пятом этаже" {
			t.Errorf("description = %q", Deref(c.Description))
		}
		if c.ActionsTaken != nil {
			t.Errorf("пустые actions_taken должны быть nil")
		}
		if c.Attributes == nil || (*c.Attributes)["people_threat"] != "yes" {
			t.Errorf("attributes = %v", c.Attributes)
		}
	})

	t.Run("сообщённый телефон важнее АОН, пострадавшие без числа", func(t *testing.T) {
		t.Parallel()
		d := EmptyDraft()
		d.Phones.Aon = Ptr("111")
		d.Phones.Provided = Ptr(" 222 ")
		d.Flags.VictimsPresent = true
		zero := 0
		d.Flags.VictimsCount = &zero
		c := DraftToCard(&d, cat)
		if c.Applicant == nil || Deref(c.Applicant.Phone) != "222" || c.Applicant.Name != nil {
			t.Fatalf("applicant = %+v", c.Applicant)
		}
		if Deref(c.Casualties.Injured) != 1 {
			t.Fatalf("victimsPresent без числа -> хотя бы один, got %v", c.Casualties.Injured)
		}
	})

	t.Run("без справочника и без кодов служб", func(t *testing.T) {
		t.Parallel()
		d := EmptyDraft()
		d.IncidentTypeIds = []string{"t-fire"}
		d.Services = []public.AssignedService{{Code: " "}}
		c := DraftToCard(&d, nil)
		if c.CategoryCode != nil {
			t.Fatal("без справочника категория не определяется")
		}
		if c.ServicesToNotify != nil {
			t.Fatal("службы без кодов -> nil, не []")
		}
		d.IncidentTypeIds = []string{"unknown"}
		if DraftToCard(&d, cat).CategoryCode != nil {
			t.Fatal("неизвестный тип")
		}
	})

	t.Run("только корпус", func(t *testing.T) {
		t.Parallel()
		d := EmptyDraft()
		*d.Address.Building = "5"
		c := DraftToCard(&d, cat)
		if c.Address == nil || Deref(c.Address.House) != "к5" {
			t.Fatalf("house = %v", c.Address)
		}
	})
}

func strOrNil(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestCardToDraft(t *testing.T) {
	t.Parallel()

	cat := testCatalog()

	t.Run("nil -> пустая форма", func(t *testing.T) {
		t.Parallel()
		if got := CardToDraft(nil, cat); !reflect.DeepEqual(got, EmptyDraft()) {
			t.Fatalf("nil -> %+v", got)
		}
	})

	t.Run("полная", func(t *testing.T) {
		t.Parallel()
		inj, trapped, dead := 2, 1, -3
		attrs := map[string]any{"where": "house"}
		svc := []string{"101", "999", " 103 ", "101"}
		c := components.IncidentCard{
			CategoryCode: Ptr(" 104 "),
			Address: &components.Address{
				Raw: Ptr(" Москва, ул. Ленина, 14 корп. 2 "), City: Ptr("Москва"), Street: Ptr("Ленина"),
				House: Ptr("14 корп. 2"), Entrance: Ptr("1"), Floor: Ptr("5"), Apartment: Ptr("12"), Landmark: Ptr("напротив школы"),
			},
			ServicesToNotify: &svc,
			Description:      Ptr(" Запах газа в подъезде "),
			ActionsTaken:     Ptr("Рекомендовано покинуть помещение"),
			Attributes:       &attrs,
		}
		c.Applicant = &struct {
			Name  *string `json:"name,omitempty"`
			Phone *string `json:"phone,omitempty"`
		}{Name: Ptr(" Петров "), Phone: Ptr("+79035113367")}
		c.Casualties = &struct {
			Dead    *int `json:"dead,omitempty"`
			Injured *int `json:"injured,omitempty"`
			Trapped *int `json:"trapped,omitempty"`
		}{Dead: &dead, Injured: &inj, Trapped: &trapped}

		d := CardToDraft(&c, cat)
		if !reflect.DeepEqual(d.IncidentTypeIds, []string{"t-gas"}) {
			t.Errorf("incidentTypeIds = %v", d.IncidentTypeIds)
		}
		if d.Address.Raw != "Москва, ул. Ленина, 14 корп. 2" || *d.Address.Settlement != "Москва" ||
			*d.Address.Street != "Ленина" || *d.Address.House != "14" || *d.Address.Building != "2" ||
			*d.Address.Entrance != "1" || *d.Address.Floor != "5" || *d.Address.Apartment != "12" ||
			*d.Address.Descriptive != "напротив школы" {
			b, _ := json.Marshal(d.Address)
			t.Errorf("address = %s", b)
		}
		if *d.Address.Country != "" || *d.Address.Okrug != "" {
			t.Error("неизвестные поля адреса — пустые строки")
		}
		if Deref(d.Applicant.Name) != "Петров" || Deref(d.Phones.Provided) != "+79035113367" || d.Phones.Aon != nil {
			t.Errorf("applicant/phones = %+v %+v", d.Applicant, d.Phones)
		}
		if !d.Flags.VictimsPresent || Deref(d.Flags.VictimsCount) != 3 {
			t.Errorf("victims = %v %v (отрицательные не считаются)", d.Flags.VictimsPresent, d.Flags.VictimsCount)
		}
		if len(d.Services) != 2 || d.Services[0].Code != "101" || d.Services[1].Code != "103" {
			t.Fatalf("services = %+v (неизвестные и повторы пропускаются)", d.Services)
		}
		s0, s1 := d.Services[0], d.Services[1]
		if !s0.IsPrimary || s1.IsPrimary || s0.ServiceId != "s-101" || s0.ShortName != "Служба 101" || s0.Name != "Пожарная охрана" {
			t.Errorf("service[0] = %+v", s0)
		}
		if s0.Source != public.AssignedServiceSourceAuto || string(s0.CurrentStatus) != ReactionReceived ||
			!s0.CurrentStatusAt.Equal(EtalonServiceTime) || s0.Editable || s0.History == nil || s0.AllowedNext == nil {
			t.Errorf("service[0] служебные поля = %+v", s0)
		}
		if d.Description != "Запах газа в подъезде" || d.ActionsTaken != "Рекомендовано покинуть помещение" {
			t.Errorf("тексты: %q / %q", d.Description, d.ActionsTaken)
		}
		// атрибуты скопированы: правка формы не меняет контрактную карточку
		d.Attributes["where"] = "street"
		if attrs["where"] != "house" {
			t.Error("attributes должны копироваться")
		}
	})

	t.Run("нулевые пострадавшие и пустые службы", func(t *testing.T) {
		t.Parallel()
		zero := 0
		empty := []string{}
		c := components.IncidentCard{ServicesToNotify: &empty}
		c.Casualties = &struct {
			Dead    *int `json:"dead,omitempty"`
			Injured *int `json:"injured,omitempty"`
			Trapped *int `json:"trapped,omitempty"`
		}{Injured: &zero}
		d := CardToDraft(&c, cat)
		if d.Flags.VictimsPresent || d.Flags.VictimsCount != nil {
			t.Error("0 пострадавших — флаг не ставится")
		}
		if d.Services == nil || len(d.Services) != 0 {
			t.Error("services — [] (не nil)")
		}
		if d.Attributes == nil || d.IncidentTypeIds == nil {
			t.Error("обязательные массивы/карты")
		}
	})

	t.Run("без справочника", func(t *testing.T) {
		t.Parallel()
		svc := []string{"101"}
		c := components.IncidentCard{CategoryCode: Ptr("101"), ServicesToNotify: &svc}
		d := CardToDraft(&c, nil)
		if len(d.IncidentTypeIds) != 0 || len(d.Services) != 0 {
			t.Fatalf("без справочника коды не разрешаются: %+v", d)
		}
	})
}

func TestDraftCardRoundTrip(t *testing.T) {
	t.Parallel()

	cat := testCatalog()
	d := EmptyDraft()
	d.IncidentTypeIds = []string{"t-fire"}
	d.Address.Raw = "Москва, Тверская, 12"
	*d.Address.Settlement = "Москва"
	*d.Address.Street = "Тверская"
	*d.Address.House = "12"
	*d.Address.Building = "3"
	d.Applicant.Name = Ptr("Мария")
	d.Phones.Provided = Ptr("+79001234567")
	d.Description = "Пожар"
	d.Services = []public.AssignedService{{Code: "101"}}

	c := DraftToCard(&d, cat)
	back := CardToDraft(&c, cat)
	if !reflect.DeepEqual(back.IncidentTypeIds, d.IncidentTypeIds) || back.Address.Raw != d.Address.Raw ||
		*back.Address.House != "12" || *back.Address.Building != "3" || *back.Address.Street != "Тверская" ||
		Deref(back.Applicant.Name) != "Мария" || Deref(back.Phones.Provided) != "+79001234567" ||
		back.Description != "Пожар" || len(back.Services) != 1 || back.Services[0].Code != "101" {
		b, _ := json.Marshal(back)
		t.Fatalf("round trip: %s", b)
	}
}

func TestJoinHouse(t *testing.T) {
	t.Parallel()

	tests := []struct{ house, building, want string }{
		{"", "", ""},
		{"14", "", "14"},
		{" 14 ", " ", "14"},
		{"14", "2", "14к2"},
		{" 32А ", " 1 ", "32Ак1"},
		{"", "5", "к5"},
	}
	for _, tt := range tests {
		if got := JoinHouse(tt.house, tt.building); got != tt.want {
			t.Errorf("JoinHouse(%q, %q) = %q, want %q", tt.house, tt.building, got, tt.want)
		}
	}
}

func TestSplitHouse(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, house, building string }{
		{"", "", ""},
		{"   ", "", ""},
		{"14", "14", ""},
		{"14к2", "14", "2"},
		{"14К2", "14", "2"},
		{"14 к. 2", "14", "2"},
		{"14к.2", "14", "2"},
		{"14, корп. 2", "14", "2"},
		{"14 корп 2", "14", "2"},
		{"14 корпус 2", "14", "2"},
		{"14 КОРПУС 2", "14", "2"},
		{"14корп2", "14", "2"},
		{"14 корпус А", "14", "А"},
		{"14 корпус", "14 корпус", ""}, // было ("14", "ус") через маркер "корп"
		{"14корпус", "14корпус", ""},
		{"14 корпуса", "14 корпуса", ""}, // было ("14", "а")
		{"14 корп.", "14 корп.", ""},
		{"14кА", "14", "А"}, // однобуквенный корпус
		{"14кв", "14", "в"},
		{"14кВ2", "14", "В2"},
		{"14к 2", "14", "2"},
		{"32А", "32А", ""},       // литера — не корпус
		{"32А к. 1", "32А", "1"}, // литера + корпус
		{"32Ак3", "32А", "3"},    // так склеивает JoinHouse
		{"32ак 3", "32а", "3"},
		{"32Ак", "32Ак", ""},   // корпуса нет
		{"32Акв", "32Акв", ""}, // после литеры корпус — только с цифры
		{"Ак3", "Ак3", ""},     // нет номера дома
		{"5к", "5к", ""},       // «к» без продолжения
		{"к5", "к5", ""},       // нет номера дома
		{"корпус 2", "корпус 2", ""},
		{"д. 14к2", "д. 14к2", ""}, // дом должен начинаться с цифры
		{"Строение 14к2", "Строение 14к2", ""},
		{"14 к. ", "14 к.", ""},
		{"  7к1  ", "7", "1"},
	}
	for _, tt := range tests {
		h, b := SplitHouse(tt.in)
		if h != tt.house || b != tt.building {
			t.Errorf("SplitHouse(%q) = (%q, %q), want (%q, %q)", tt.in, h, b, tt.house, tt.building)
		}
	}
	// обратимость JoinHouse -> SplitHouse для номеров домов
	for _, hb := range [][2]string{{"14", "2"}, {"1", "10"}, {"32А", "3"}, {"7", ""}} {
		h, b := SplitHouse(JoinHouse(hb[0], hb[1]))
		if h != hb[0] || b != hb[1] {
			t.Errorf("round trip %v -> (%q, %q)", hb, h, b)
		}
	}
}

func FuzzSplitHouse(f *testing.F) {
	for _, s := range []string{"14к2", "14 корп. 2", "32А", "İİк1", "к", "1к", "ǅ14к2"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		h, b := SplitHouse(s) // не паникует на любых байтах
		if b != "" && h == "" {
			t.Fatalf("корпус без дома: %q -> (%q, %q)", s, h, b)
		}
	})
}
