package classifier

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"

	"lct/gocore/internal/gen/public"
)

// Синтетический лист в раскладке реального классификатора (см. комментарий importxlsx.go):
// строка блоков, строка заголовков, строка условий, затем разделы и типы.
// Колонки: A Г, B п1, C п2, D п3, E Номер, F Группа, G Признак.1, H Признак.2, I Признак.3,
// J Доп. признаки, K Итоговый тип, L ЕКП, M Главная служба, N..P МЧС, Q..S МВД, T СМП, U Мослифт, V новая служба.
func sampleSheet() [][]any {
	return [][]any{
		{"Генерация номера", "", "", "", "", "", "Блок, как происшествие отображается у оператора", "", "", "", "", "",
			"Главная служба", "Классификатор МЧС", "", "", "Классификатор МВД", "", "", "Классификатор СМП", "Мослифт", "Служба Новая"},
		{"Г", "п1", "п2", "п3", "Номер", "Группа происшествий (раздел)", "112 - Признак.1 (тип происшествия)",
			"112-Признак.2", "112-Признак.3", "Дополнительные признаки (опросная карта)", "Итоговый тип происшествия",
			"ТИП происшествия ЕКП 35", "", "Служба 101", "", "ОДС ПСЦ",
			"признак Правонарушение или Пострадавшие не выбран", "выбран признак Правонарушение", "выбран признак Пострадавшие"},
		{"", "", "", "", "", "", "", "", "", "", "", "", "", "признак не выбран", "(выбран признак НД - НЕТ ДОСТУПА)",
			"угроза людям"},
		// раздел 1
		{"", "", "", "", 1, "Пожары и задымления"},
		// категория п1 = 1
		{1, 1, 0, 0, 1010000, "", "на улице"},
		// тип: базовые колонки 101/103, условия 101 (нет доступа), 102 (правонарушение, пострадавшие)
		{1, 1, 1, 1, 1010101, "", "Пожар", "мусор", "", "", "Пожар: мусор", "Пожар", "MCHS", "карточка-112", "да",
			"нет реагирования", "", "Правонарушение", "Пострадавшие", "Пожар мусор"},
		// скрытый от оператора тип
		{1, 1, 1, 2, 1010102, "", "Не отображается оператору 112", "", "", "", "Задымление: мусор", "", "", "да"},
		// тип раньше строки своей категории, «Номер» пуст — код вычисляется; две главные службы
		{1, 2, 1, 1, "", "", "Транспорт", "автобус", "", "", "Пожар: автобус", "", "MCHS, POLICE", "да", "", "", "", "", "",
			"", "", "да"},
		// строка категории после её типа — заменяет синтетический узел
		{1, 2, 0, 0, 1020000, "", "Транспорт", "", "", "", "Транспорт (категория)"},
		// раздел без номера — номер из «Г» следующей строки
		{"", "", "", "", "", "БПЛА"},
		{24, 1, 1, 1, 24010101, "", "БПЛА", "падение", "", "", "Падение БПЛА", "", "", "да"},
		// повтор кода
		{1, 1, 1, 1, 1010101, "", "Пожар", "", "", "", "Повтор"},
		// «Г/п1» не числа
		{"x", 1, 1, 1, 1, "", "мусор"},
		// строка без «Г» и раздела
		{"", "", "", "", "abc"},
		// совсем пустая строка
		{},
	}
}

func sheetStrings(rows [][]any) [][]string {
	out := make([][]string, len(rows))
	for i, r := range rows {
		out[i] = make([]string, len(r))
		for j, v := range r {
			out[i][j] = fmt.Sprint(v)
		}
	}
	return out
}

// writeXLSX — лист в файл (числа — числами, как в реальном файле; блоки служб объединены).
func writeXLSX(t *testing.T, rows [][]any) string {
	t.Helper()
	f := excelize.NewFile()
	defer f.Close()
	const sh = "Лист1"
	if err := f.SetSheetName("Sheet1", sh); err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		for j, v := range r {
			if s, ok := v.(string); ok && s == "" {
				continue
			}
			cell, _ := excelize.CoordinatesToCellName(j+1, i+1)
			if err := f.SetCellValue(sh, cell, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, m := range [][2]string{{"N1", "P1"}, {"Q1", "S1"}} {
		if err := f.MergeCell(sh, m[0], m[1]); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "Классификатор_тест.xlsx")
	if err := f.SaveAs(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func nodeByCode(p *xlsxParsed, code string) *xlsxNode {
	for i := range p.nodes {
		if p.nodes[i].code == code {
			return &p.nodes[i]
		}
	}
	return nil
}

func TestDetectColumns(t *testing.T) {
	t.Parallel()
	rows := sheetStrings(sampleSheet())
	h := findHeaderRow(rows)
	if h != 1 {
		t.Fatalf("header row %d", h)
	}
	c, err := detectColumns(rows, h)
	if err != nil {
		t.Fatal(err)
	}
	if c.g != 0 || c.p1 != 1 || c.p2 != 2 || c.p3 != 3 || c.num != 4 || c.group != 5 || c.f1 != 6 || c.f2 != 7 ||
		c.f3 != 8 || c.extraSigns != 9 || c.final != 10 || c.ekp != 11 || c.main != 12 {
		t.Errorf("columns %+v", c)
	}
	type col struct{ code, cond string }
	var got []col
	for _, sc := range c.svc {
		cond := ""
		if sc.cond != nil {
			cond = sc.cond.attr + "=" + sc.cond.value
		}
		got = append(got, col{sc.code, cond})
	}
	want := []col{
		{"101", ""}, {"101", "access=no_access"}, {"101", "people_threat=yes"},
		{"102", ""}, {"102", "offense=yes"}, {"102", "victims=yes"},
		{"103", ""}, {"moslift", ""}, {"xls_v", ""},
	}
	if !slices.Equal(got, want) {
		t.Errorf("service columns\n got %v\nwant %v", got, want)
	}
	if c.svc[1].title != "Классификатор МЧС / Служба 101 / (выбран признак НД - НЕТ ДОСТУПА)" {
		t.Errorf("title %q", c.svc[1].title)
	}

	if _, err := detectColumns([][]string{{"Номер", "Г"}}, 0); err == nil {
		t.Error("missing «п1» must fail")
	}
	if findHeaderRow([][]string{{"a"}, {"Номер"}}) != -1 {
		t.Error("header needs both «Г» and «Номер»")
	}
	if _, err := parseClassifierSheet([][]string{{"что-то"}}, &ImportStats{}); err == nil {
		t.Error("sheet without header must fail")
	}
}

func TestParseClassifierSheet(t *testing.T) {
	t.Parallel()
	stats := &ImportStats{Errors: []string{}}
	p, err := parseClassifierSheet(sheetStrings(sampleSheet()), stats)
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	for _, n := range p.nodes {
		codes = append(codes, n.code)
	}
	wantCodes := []string{"1", "1010000", "1010101", "1010102", "1020000", "1020101", "24", "24010000", "24010101"}
	if !slices.Equal(codes, wantCodes) {
		t.Fatalf("nodes %v, want %v", codes, wantCodes)
	}
	// родители — раньше детей, sort_order растёт
	pos := map[string]int{}
	for i, n := range p.nodes {
		pos[n.code] = i
		if n.parent != "" {
			if pi, ok := pos[n.parent]; !ok || pi >= i {
				t.Errorf("parent %s of %s is not emitted before it", n.parent, n.code)
			}
		}
		if i > 0 && n.sortOrder <= p.nodes[i-1].sortOrder && n.code != "1020000" {
			t.Errorf("sort order of %s: %d", n.code, n.sortOrder)
		}
	}
	type exp struct {
		parent, name string
		depth        int16
		active       bool
	}
	for code, e := range map[string]exp{
		"1":        {"", "Пожары и задымления", 1, true},
		"1010000":  {"1", "На улице", 2, true},
		"1010101":  {"1010000", "Пожар: мусор", 3, true},
		"1010102":  {"1010000", "Задымление: мусор", 3, false},
		"1020000":  {"1", "Транспорт (категория)", 2, true},
		"1020101":  {"1020000", "Пожар: автобус", 3, true},
		"24":       {"", "БПЛА", 1, true},
		"24010000": {"24", "БПЛА", 2, true},
		"24010101": {"24010000", "Падение БПЛА", 3, true},
	} {
		n := nodeByCode(p, code)
		if n.parent != e.parent || n.name != e.name || n.depth != e.depth || n.active != e.active {
			t.Errorf("%s = {parent %q name %q depth %d active %v}, want %+v", code, n.parent, n.name, n.depth, n.active, e)
		}
		for name, js := range map[string][]byte{"services": n.servicesJSON, "attrs": n.attrsJSON, "rules": n.rulesJSON, "extra": n.extraJSON} {
			if !json.Valid(js) {
				t.Errorf("%s: %s is not JSON: %s", code, name, js)
			}
		}
		if n.synonyms == nil {
			t.Errorf("%s: synonyms nil", code)
		}
	}
	// заменённая синтетическая категория — не synthetic; созданная и не заменённая — synthetic
	var extra map[string]any
	_ = json.Unmarshal(nodeByCode(p, "1020000").extraJSON, &extra)
	if extra["synthetic"] != nil || extra["kind"] != "category" || extra["source"] != "xlsx" {
		t.Errorf("1020000 extra: %v", extra)
	}
	_ = json.Unmarshal(nodeByCode(p, "24010000").extraJSON, &extra)
	if extra["synthetic"] != true {
		t.Errorf("24010000 extra: %v", extra)
	}

	// правила и опросная карта типа
	n := nodeByCode(p, "1010101")
	var rules []ruleJSON
	if err := json.Unmarshal(n.rulesJSON, &rules); err != nil {
		t.Fatal(err)
	}
	if len(rules) != 4 {
		t.Fatalf("rules: %s", n.rulesJSON)
	}
	if base := rules[0]; base.Attr != "" || len(base.Services) != 2 || base.Services[0] != (ruleServiceJSON{"101", true, "тип происшествия: пожар: мусор"}) ||
		base.Services[1].Code != "103" || base.Services[1].Primary {
		t.Errorf("base rule: %+v", base)
	}
	for i, want := range []struct{ attr, value, code string }{{"access", "no_access", "101"}, {"offense", "yes", "102"}, {"victims", "yes", "102"}} {
		r := rules[i+1]
		if r.Attr != want.attr || !slices.Equal(r.AnyOf, []string{want.value}) || len(r.Services) != 1 || r.Services[0].Code != want.code ||
			!strings.HasPrefix(r.Services[0].Reason, "признак: ") {
			t.Errorf("rule %d: %+v", i+1, r)
		}
	}
	var svcCodes []string
	_ = json.Unmarshal(n.servicesJSON, &svcCodes)
	if !slices.Equal(svcCodes, []string{"101", "103", "102"}) {
		t.Errorf("services %v", svcCodes)
	}
	var groups []public.AttributeGroup
	_ = json.Unmarshal(n.attrsJSON, &groups)
	if len(groups) != 3 || groups[0].Code != "access" || groups[0].Widget != public.AttributeGroupWidgetChipsMulti ||
		groups[1].Code != "offense" || groups[1].Widget != public.AttributeGroupWidgetBool || len(*groups[1].Options) != 2 ||
		groups[2].Order != 3 {
		t.Errorf("attributes: %s", n.attrsJSON)
	}
	if !slices.Contains(n.synonyms, "мусор") || slices.Contains(n.synonyms, "пожар: мусор") {
		t.Errorf("synonyms: %v", n.synonyms)
	}
	_ = json.Unmarshal(n.extraJSON, &extra)
	notify, _ := extra["notify"].(map[string]any)
	if notify["Классификатор МЧС / ОДС ПСЦ / угроза людям"] != "нет реагирования" || extra["main_service"] != "MCHS" {
		t.Errorf("extra: %v", extra)
	}

	// две главные службы: обе основные и в базовом правиле; новая служба из колонки V
	n = nodeByCode(p, "1020101")
	_ = json.Unmarshal(n.rulesJSON, &rules)
	if len(rules) != 1 {
		t.Fatalf("1020101 rules: %s", n.rulesJSON)
	}
	var got []string
	for _, s := range rules[0].Services {
		got = append(got, fmt.Sprintf("%s:%v", s.Code, s.Primary))
	}
	if !slices.Equal(got, []string{"101:true", "xls_v:false", "102:true"}) {
		t.Errorf("1020101 services %v", got)
	}

	// службы для вставки: все колонки + главные, без повторов
	var svc []string
	for _, s := range p.services {
		svc = append(svc, s.code)
		if s.name == "" || s.short == "" || len([]rune(s.short)) > 40 {
			t.Errorf("service %+v", s)
		}
	}
	if !slices.Equal(svc, []string{"101", "102", "103", "moslift", "xls_v"}) {
		t.Errorf("services %v", svc)
	}

	// статистика разбора
	if stats.RowsTotal != 11 { // строка условий и пустая строка не считаются
		t.Errorf("rows_total %d", stats.RowsTotal)
	}
	if stats.Skipped != 3 || len(stats.Errors) != 3 {
		t.Errorf("skipped %d errors %v", stats.Skipped, stats.Errors)
	}
	for _, want := range []string{"код 1010101 повторяется", "строка 13: «Г/п1/п2/п3» не числа", "строка 14: не распознана"} {
		if !slices.ContainsFunc(stats.Errors, func(e string) bool { return strings.Contains(e, want) }) {
			t.Errorf("errors %v miss %q", stats.Errors, want)
		}
	}
}

func TestParseSkipsFixtureCodesAndCapsErrors(t *testing.T) {
	t.Parallel()
	rows := sheetStrings(sampleSheet()[:3])
	rows = append(rows, []string{"9", "9", "9", "9", "101", "", "Происшествие"}) // код типа из фикстур
	for i := range maxImportErrors + 20 {
		rows = append(rows, []string{"y", fmt.Sprint(i)})
	}
	stats := &ImportStats{Errors: []string{}}
	p, err := parseClassifierSheet(rows, stats)
	if err != nil {
		t.Fatal(err)
	}
	if nodeByCode(p, "101") != nil {
		t.Error("fixture code imported")
	}
	if !strings.Contains(stats.Errors[0], "совпадает с типом из встроенного справочника") {
		t.Errorf("first error %q", stats.Errors[0])
	}
	if len(stats.Errors) != maxImportErrors || stats.Skipped != maxImportErrors+21 {
		t.Errorf("errors capped at %d: %d, skipped %d", maxImportErrors, len(stats.Errors), stats.Skipped)
	}
}

func TestServiceCodeFor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"Классификатор МЧС", "101"},
		{"Служба 101 (признак НД - НЕТ ДОСТУПА не выбран)", "101"},
		{"ОДС ПСЦ", "101"},
		{"МГПСС", "mgpss"},
		{"Классификатор МВД", "102"},
		{"Классификатор СМП", "103"},
		{"Классификатор МОСГАЗ", "104"},
		{"Классификатор Мособлгаз", "mosoblgaz"},
		{"Автомобильные дороги АО г.Москвы", "avtodor_ao"},
		{"Автомобильные дороги", "avtodor"},
		{"МОЭСК (ПАО \"Россети Московский регион\")", "moesk"},
		{"МОЭК", "moek"},
		{"ОЭК", "oek"},
		{"ЦУКБ.БПЛА Министерство обороны", "mo_bpla"},
		{"ЦУКБ Министерство обороны", "mo_cukb"},
		{"Департамент РБиПК (ГКУ МОСБЕЗ)", "mosbez"},
		{"Территориальные ОИВ ТиНАО", "oiv_tinao"},
		{"Территориальные ОИВ", "oiv"},
		{"Депортамент гражданского строительства", "dgs"},
		{"Совсем новая служба", ""},
		{"метрополитен", ""}, // целые слова, а не подстроки
	} {
		if got := serviceCodeFor(tc.in); got != tc.want {
			t.Errorf("serviceCodeFor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// у каждой службы из таблицы сопоставления есть название для INSERT
	for _, nd := range serviceNeedles {
		if _, ok := serviceNames[nd[1]]; !ok {
			t.Errorf("serviceNames lacks %q", nd[1])
		}
	}
	for _, code := range mainServiceCodes {
		if _, ok := serviceNames[code]; !ok {
			t.Errorf("serviceNames lacks main service %q", code)
		}
	}
}

func TestConditionFor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"признак не выбран", ""},
		{"Служба 101 (признак НД - НЕТ ДОСТУПА не выбран)", ""},
		{"МГТС реагирование всегда", ""},
		{"(выбран признак НД - НЕТ ДОСТУПА)", "access=no_access"},
		{"угроза людям", "people_threat=yes"},
		{"пострадавшие/погибшие", "victims=yes"},
		{"постр / погибшие", "victims=yes"},
		{"выбран признак Пострадавшие не на месте", "victims=absent"},
		{"выбран признак Правонарушение", "offense=yes"},
		{"газификация", "gasified=yes"},
		{"мед. помощь", "medical_needed=yes"},
		{"треб. Эвакуация", "evacuation=yes"},
		{">5 чел / ОД", "mass_casualties=yes"},
		{"перекрытие движение", "traffic_block=yes"},
		{"тоннель", "object_kind=tunnel"},
		{"пеш", "object_kind=pedestrian_bridge"},
		{"ав", "object_kind=road_bridge"},
		{"на объектах связи", "object_kind=comm_object"},
		{"стройка", "object_kind=construction"},
		{"объект из перечня", "object_kind=listed_object"},
		{"что-то непонятное", ""},
	} {
		got := ""
		if c := conditionFor(tc.in); c != nil {
			got = c.attr + "=" + c.value
			if c.attrLabel == "" || c.valueLabel == "" || !public.AttributeGroupWidget(c.widget).Valid() {
				t.Errorf("conditionFor(%q) = %+v", tc.in, c)
			}
		}
		if got != tc.want {
			t.Errorf("conditionFor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSynonymsFor(t *testing.T) {
	t.Parallel()
	got := synonymsFor("Пожар: мусор", "Пожар", "МУСОР", "", "Пожар: мусор", "Не отображается оператору 112", "нет в ЕКП", "мусор")
	if !slices.Equal(got, []string{"пожар", "мусор"}) {
		t.Errorf("synonymsFor = %v", got)
	}
	if got := synonymsFor("X"); got == nil || len(got) != 0 {
		t.Errorf("no synonyms: %#v", got)
	}
}

func TestImportStatsJSON(t *testing.T) {
	t.Parallel()
	b, _ := json.Marshal(ImportStats{Errors: []string{}})
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"rows_total", "nodes", "inserted", "updated", "skipped", "deactivated", "services_added", "errors"} {
		if _, ok := m[k]; !ok {
			t.Errorf("stats JSON misses %q: %s", k, b)
		}
	}
	if _, ok := m["BatchID"]; ok {
		t.Error("BatchID must not be serialized")
	}
}

func TestReadSheetFromFile(t *testing.T) {
	t.Parallel()
	path := writeXLSX(t, sampleSheet())
	rows, err := readSheet(path)
	if err != nil {
		t.Fatal(err)
	}
	// сырые значения: числа без форматирования, объединённые ячейки — только левая верхняя
	if rows[5][4] != "1010101" || rows[0][13] != "Классификатор МЧС" || cell(rows[0], 14) != "" {
		t.Errorf("rows[5] = %v, rows[0] = %v", rows[5], rows[0])
	}
	p, err := parseClassifierSheet(rows, &ImportStats{Errors: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.nodes) != 9 {
		t.Errorf("nodes from file: %d", len(p.nodes))
	}
	if _, err := readSheet(filepath.Join(t.TempDir(), "нет.xlsx")); err == nil {
		t.Error("missing file")
	}
	// лист без заголовков
	f := excelize.NewFile()
	_ = f.SetCellValue("Sheet1", "A1", "просто текст")
	empty := filepath.Join(t.TempDir(), "empty.xlsx")
	if err := f.SaveAs(empty); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := readSheet(empty); err == nil || !strings.Contains(err.Error(), "не найден лист") {
		t.Errorf("sheet without headers: %v", err)
	}
}
