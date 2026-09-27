package classifier

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode"

	"lct/gocore/internal/gen/public"
)

// Подсказки адреса (Инструкция п.4.3): оператор вводит адрес одной строкой с домом
// включительно («Тверская 12», «ленинский пр-т 32а», «профсоюзная 45 к2»), система
// предлагает варианты и заполняет поля. В контуре нет интернета — варианты строятся
// по встроенному справочнику улиц Москвы (data/streets.json) плюс точные адреса из
// фронтовых фикстур (data/streets.json: known) с их источниками (yandex_map / yandex_org /
// fias) — тренировка правила «ФИАС не подставляет службы» остаётся возможной.

// Лимиты подсказок (frontend.v1.yaml: limit default 10, maximum 30; q minLength 2).
const (
	SuggestDefaultLimit = 10
	SuggestMaxLimit     = 30
	SuggestMinQuery     = 2
)

type streetsFile struct {
	Streets [][]json.RawMessage `json:"streets"` // [name, okrug, district, lat, lon]
	Known   [][]json.RawMessage `json:"known"`   // [label, source, street, okrug, district, house, building, structure, lat, lon]
}

type street struct {
	full     string // «Тверская улица», «Ленинский проспект», «Охотный Ряд»
	field    string // значение AddressDraft.street: «Тверская» / «Ленинский проспект» (как во фикстурах)
	kind     string // тип улицы (нормализованный: «улица», «проспект», …; "" — без типа)
	okrug    string
	district string
	lat, lon float32
	tok      []string // слова названия без типа, нормализованные
	typeTok  []string // слова-типы из названия в полной форме («улица», «проспект»)
}

type knownAddr struct {
	street    *street
	label     string
	source    public.AddressSource
	okrug     string
	district  string
	house     string
	houseNorm string
	building  string
	structure string
	lat, lon  float32
}

type streetIndex struct {
	streets []*street
	known   []*knownAddr
}

// streetTypeWords — слова-типы улиц (и сокращения) в нормализованном виде. В названии
// они не участвуют в сопоставлении, в запросе — игнорируются (но дают бонус к рангу).
var streetTypeWords = map[string]string{
	"улица": "улица", "ул": "улица",
	"проспект": "проспект", "просп": "проспект", "пр-т": "проспект", "пр-кт": "проспект",
	"шоссе": "шоссе", "ш": "шоссе",
	"переулок": "переулок", "пер": "переулок",
	"бульвар": "бульвар", "бул": "бульвар", "б-р": "бульвар",
	"набережная": "набережная", "наб": "набережная",
	"площадь": "площадь", "пл": "площадь",
	"проезд": "проезд", "пр-д": "проезд",
	"тупик": "тупик", "туп": "тупик",
	"аллея": "аллея",
}

// noiseWords — слова запроса, которые не относятся к улице («Москва, ул. …, д. 5»).
var noiseWords = map[string]struct{}{
	"москва": {}, "г": {}, "город": {}, "россия": {}, "рф": {}, "д": {}, "дом": {},
}

func buildStreetIndex(f *streetsFile) *streetIndex {
	ix := &streetIndex{streets: make([]*street, 0, len(f.Streets))}
	byName := make(map[string]*street, len(f.Streets))
	for _, row := range f.Streets {
		if len(row) < 5 {
			continue
		}
		st := &street{
			full:     rawString(row[0]),
			okrug:    rawString(row[1]),
			district: rawString(row[2]),
			lat:      rawFloat(row[3]),
			lon:      rawFloat(row[4]),
		}
		st.field, st.kind = streetField(st.full)
		for _, t := range tokens(st.full) {
			if k, isType := streetTypeWords[t]; isType {
				st.typeTok = append(st.typeTok, k)
				continue
			}
			st.tok = append(st.tok, t)
		}
		ix.streets = append(ix.streets, st)
		if _, ok := byName[st.full]; !ok {
			byName[st.full] = st
		}
	}
	for _, row := range f.Known {
		if len(row) < 10 {
			continue
		}
		st := byName[rawString(row[2])]
		if st == nil {
			continue
		}
		k := &knownAddr{
			street:    st,
			label:     rawString(row[0]),
			source:    public.AddressSource(rawString(row[1])),
			okrug:     rawString(row[3]),
			district:  rawString(row[4]),
			house:     rawString(row[5]),
			building:  rawString(row[6]),
			structure: rawString(row[7]),
			lat:       rawFloat(row[8]),
			lon:       rawFloat(row[9]),
		}
		k.houseNorm = normalize(k.house)
		ix.known = append(ix.known, k)
	}
	return ix
}

func rawString(m json.RawMessage) string {
	var s string
	_ = json.Unmarshal(m, &s)
	return s
}

func rawFloat(m json.RawMessage) float32 {
	var f float64
	_ = json.Unmarshal(m, &f)
	return float32(f)
}

// streetField — значение поля «Улица» как во фронтовых фикстурах: у «улиц» тип
// опускается («Тверская улица» -> «Тверская»), у остальных — часть названия
// («Ленинский проспект», «Ярославское шоссе»).
func streetField(full string) (field, kind string) {
	words := strings.Fields(full)
	for i, w := range words {
		lw := strings.ToLower(w)
		if k, ok := streetTypeWords[lw]; ok && (i == 0 || i == len(words)-1) {
			kind = k
			if k == "улица" && len(words) > 1 {
				rest := append(append([]string{}, words[:i]...), words[i+1:]...)
				return strings.Join(rest, " "), kind
			}
			return full, kind
		}
	}
	return full, ""
}

// ---------------------------------------------------------------- разбор запроса

type addrQuery struct {
	words     []string // слова улицы (нормализованные, без типов и «шума»)
	kind      string   // тип улицы из запроса ("" — не указан)
	house     string   // как ввёл оператор, нормализовано к верхнему регистру буквы: «32А»
	building  string   // корпус
	structure string   // строение
}

// parseAddrQuery — «улица [дом][к/корп N][стр/с N]». Числа до первого слова улицы
// относятся к названию («1-я Тверская-Ямская», «26 Бакинских Комиссаров»), первое число
// после слова улицы — дом. Слитные формы «12к2», «12с1», «к2», «стр1» тоже понимаются.
func parseAddrQuery(q string) addrQuery {
	var aq addrQuery
	// Сокращения с дефисом — до нормализации (иначе «пр-т» распадётся на «пр» и «т»).
	lq := strings.ToLower(q)
	for _, abbr := range [...]string{"пр-кт", "пр-т", "б-р", "пр-д"} {
		if strings.Contains(lq, abbr) {
			lq = strings.ReplaceAll(lq, abbr, " "+streetTypeWords[abbr]+" ")
		}
	}
	toks := addrTokens(lq)
	sawStreetWord := false
	kindTok := "" // первое слово-тип: «пер» может оказаться началом «Перовская»
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if k, ok := streetTypeWords[t]; ok {
			aq.kind = k
			if kindTok == "" {
				kindTok = t
			}
			continue
		}
		if _, ok := noiseWords[t]; ok {
			continue
		}
		// корпус / строение отдельным словом: «к 2», «корп 2», «стр 1», «с 1»
		if aq.house != "" {
			switch t {
			case "к", "корп", "корпус":
				if i+1 < len(toks) && hasDigitPrefix(toks[i+1]) {
					aq.building = strings.ToUpper(toks[i+1])
					i++
				}
				continue
			case "с", "стр", "строение":
				if i+1 < len(toks) && hasDigitPrefix(toks[i+1]) {
					aq.structure = strings.ToUpper(toks[i+1])
					i++
				}
				continue
			}
			if b, ok := cutNumberSuffix(t, "корпус", "корп", "к"); ok {
				aq.building = b
				continue
			}
			if s, ok := cutNumberSuffix(t, "строение", "стр", "с"); ok {
				aq.structure = s
				continue
			}
		}
		if hasDigitPrefix(t) {
			if sawStreetWord && aq.house == "" {
				aq.house, aq.building, aq.structure = splitHouse(t)
				continue
			}
			if !sawStreetWord {
				aq.words = append(aq.words, t) // номер в названии улицы
			}
			continue
		}
		// одиночная буква после номера дома («12 а») — литера дома
		if aq.house != "" && len([]rune(t)) == 1 && aq.building == "" && aq.structure == "" {
			aq.house += strings.ToUpper(t)
			continue
		}
		if aq.house != "" {
			continue // слова после дома (подъезд, квартира) — не про улицу
		}
		// Однобуквенные слова («я» из «1-я») — окончания порядковых, фильтром не служат.
		if len([]rune(t)) == 1 {
			continue
		}
		aq.words = append(aq.words, t)
		sawStreetWord = true
	}
	// Запрос из одного «типа» («пер», «пл») — оператор ещё набирает название улицы.
	if len(aq.words) == 0 && kindTok != "" {
		aq.words = append(aq.words, kindTok)
		aq.kind = ""
	}
	return aq
}

// cutNumberSuffix — «к2» -> «2» для префиксов корпуса/строения.
func cutNumberSuffix(t string, prefixes ...string) (string, bool) {
	for _, p := range prefixes {
		if rest, ok := strings.CutPrefix(t, p); ok && hasDigitPrefix(rest) {
			return strings.ToUpper(rest), true
		}
	}
	return "", false
}

// addrTokens — слова адресной строки (как tokens), но дробь в номере дома («12/1») остаётся
// одним словом: normalize считает «/» разделителем, и «Тверская 12/1» превращалась в дом 12.
func addrTokens(s string) []string {
	rs := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	space := true // подавляет ведущий и повторные пробелы
	for i, r := range rs {
		switch {
		case r == 'ё' || r == 'Ё':
			r = 'е'
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			r = unicode.ToLower(r)
		case r == '/' && !space && i > 0 && isASCIIDigit(rs[i-1]) && i+1 < len(rs) && isASCIIDigit(rs[i+1]):
			// дробь номера дома — часть слова
		default:
			if !space {
				b.WriteByte(' ')
				space = true
			}
			continue
		}
		b.WriteRune(r)
		space = false
	}
	return strings.Fields(b.String())
}

func isASCIIDigit(r rune) bool { return r >= '0' && r <= '9' }

// splitHouse — «12к2» -> (12, 2, ""), «12с1» -> (12, "", 1), «32а» -> (32А, "", ""),
// «12/1» -> (12/1, "", "").
func splitHouse(t string) (house, building, structure string) {
	// номер дома — цифры, затем необязательная дробь и литера («12/1», «12а», «12/1а»)
	i := 0
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	if i < len(t) && t[i] == '/' {
		j := i + 1
		for j < len(t) && t[j] >= '0' && t[j] <= '9' {
			j++
		}
		if j > i+1 {
			i = j
		}
	}
	rest := t[i:]
	house = t[:i]
	if rest == "" {
		return house, "", ""
	}
	if b, ok := cutNumberSuffix(rest, "корпус", "корп", "к"); ok {
		return house, b, ""
	}
	if s, ok := cutNumberSuffix(rest, "строение", "стр", "с"); ok {
		return house, "", s
	}
	return strings.ToUpper(t), "", ""
}

// ---------------------------------------------------------------- подсказки

// Suggest — варианты адреса по строке оператора. q короче SuggestMinQuery символов -> [].
// limit <= 0 — SuggestDefaultLimit, больше SuggestMaxLimit — обрезается.
func Suggest(q string, limit int) []public.AddressSuggestion {
	if limit <= 0 {
		limit = SuggestDefaultLimit
	}
	if limit > SuggestMaxLimit {
		limit = SuggestMaxLimit
	}
	out := make([]public.AddressSuggestion, 0, limit)
	if len([]rune(strings.TrimSpace(q))) < SuggestMinQuery {
		return out
	}
	aq := parseAddrQuery(q)
	if len(aq.words) == 0 {
		return out
	}
	ix := data().streets

	type cand struct {
		st    *street
		score int
		pos   int
	}
	cands := make([]cand, 0, 16)
	for pos, st := range ix.streets {
		score, ok := matchStreet(st, &aq)
		if ok {
			cands = append(cands, cand{st, score, pos})
		}
	}
	if len(cands) == 0 {
		return out
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].pos < cands[j].pos
	})

	// Дедупликация по «подпись + источник»: длинная улица лежит в справочнике несколькими
	// участками (районами), но оператор должен видеть один вариант «Ленинский проспект, 32»
	// от источника; точный адрес из справочника (добавлен первым) побеждает построенный.
	seen := make(map[string]struct{}, limit)
	add := func(sg public.AddressSuggestion) bool {
		key := sg.Label + "|" + string(sg.Source)
		if _, dup := seen[key]; dup {
			return len(out) < limit
		}
		seen[key] = struct{}{}
		out = append(out, sg)
		return len(out) < limit
	}

	// 1) точные адреса из справочника (фикстуры) на найденных улицах — с их источником
	for _, c := range cands {
		for _, k := range ix.known {
			if k.street.full != c.st.full {
				continue
			}
			if !knownMatches(k, &aq) {
				continue
			}
			if !add(knownSuggestion(k)) {
				return out
			}
		}
	}
	// 2) построенные варианты: улица + введённый дом (или улица без дома, если дом не введён)
	for _, c := range cands {
		if !add(builtSuggestion(c.st, &aq)) {
			return out
		}
	}
	return out
}

// matchStreet — каждое слово запроса — префикс слова названия (без типа) или, без бонуса
// к рангу, префикс типа улицы: оператор допечатывает «Тверская ули…», пишет «Ленинский пр. 32»
// (сокращения «пр» нет в streetTypeWords — оно неоднозначно: проспект или проезд).
// Ранг: точные совпадения слов, совпадение первого слова, указанный тип улицы, короче название.
func matchStreet(st *street, aq *addrQuery) (int, bool) {
	score := 0
	for i, w := range aq.words {
		found := false
		for j, t := range st.tok {
			if !strings.HasPrefix(t, w) {
				continue
			}
			found = true
			if t == w {
				score += 10
			}
			if i == 0 && j == 0 {
				score += 5
			}
			break
		}
		if !found {
			for _, t := range st.typeTok {
				if strings.HasPrefix(t, w) {
					found = true
					break
				}
			}
		}
		if !found {
			return 0, false
		}
	}
	if aq.kind != "" && aq.kind == st.kind {
		score += 8
	}
	score -= len(st.tok) // при прочих равных — короче название («Тверская» раньше «1-я Тверская-Ямская»)
	return score, true
}

// knownMatches — точный адрес подходит к запросу: дом не введён, или совпадает начало номера,
// корпус/строение (если введены) совпадают.
func knownMatches(k *knownAddr, aq *addrQuery) bool {
	if aq.house == "" {
		return true
	}
	if !strings.HasPrefix(k.houseNorm, normalize(aq.house)) {
		return false
	}
	if aq.building != "" && !strings.EqualFold(k.building, aq.building) {
		return false
	}
	if aq.structure != "" && !strings.EqualFold(k.structure, aq.structure) {
		return false
	}
	return true
}

func knownSuggestion(k *knownAddr) public.AddressSuggestion {
	v := addressValue(k.label, k.street.field, k.okrug, k.district, k.house, k.building, k.structure, k.lat, k.lon, k.source)
	return public.AddressSuggestion{
		Id:     suggestionID(k.street, k.house, k.building, k.structure, k.source),
		Label:  k.label,
		Source: k.source,
		Value:  v,
	}
}

// builtSuggestion — вариант из справочника улиц. Подпись — как во фикстурах:
// «Москва, Тверская улица, 12, корпус 2, строение 1»; источник — yandex_map
// (основной источник списка подсказок в АРМ).
func builtSuggestion(st *street, aq *addrQuery) public.AddressSuggestion {
	var b strings.Builder
	b.Grow(64)
	b.WriteString("Москва, ")
	b.WriteString(st.full)
	if aq.house != "" {
		b.WriteString(", ")
		b.WriteString(aq.house)
		if aq.building != "" {
			b.WriteString(", корпус ")
			b.WriteString(aq.building)
		}
		if aq.structure != "" {
			b.WriteString(", строение ")
			b.WriteString(aq.structure)
		}
	}
	label := b.String()
	src := public.AddressSourceYandexMap
	return public.AddressSuggestion{
		Id:     suggestionID(st, aq.house, aq.building, aq.structure, src),
		Label:  label,
		Source: src,
		Value:  addressValue(label, st.field, st.okrug, st.district, aq.house, aq.building, aq.structure, st.lat, st.lon, src),
	}
}

func suggestionID(st *street, house, building, structure string, src public.AddressSource) string {
	var b strings.Builder
	b.Grow(64)
	b.WriteString("addr-")
	b.WriteString(normalize(st.full))
	b.WriteByte('-')
	b.WriteString(st.district)
	b.WriteByte('-')
	b.WriteString(house)
	if building != "" {
		b.WriteString("-к")
		b.WriteString(building)
	}
	if structure != "" {
		b.WriteString("-с")
		b.WriteString(structure)
	}
	b.WriteByte('-')
	b.WriteString(string(src))
	return strings.ReplaceAll(b.String(), " ", "_")
}

// addressValue — AddressDraft со всеми строковыми полями (фронт подставляет value
// целиком вместо адреса карточки и читает поля без проверок на undefined).
// raw = подпись варианта (как во фикстурах: с ней сверяется эталонный address.raw).
func addressValue(raw, streetName, okrug, district, house, building, structure string, lat, lon float32, src public.AddressSource) public.AddressDraft {
	str := func(s string) *string { return &s }
	v := public.AddressDraft{
		Raw:         raw,
		Country:     str("Россия"),
		Region:      str("Москва"),
		Settlement:  str("Москва"),
		Object:      str(""),
		Okrug:       str(okrug),
		District:    str(district),
		Street:      str(streetName),
		House:       str(house),
		Building:    str(building),
		Structure:   str(structure),
		Apartment:   str(""),
		Entrance:    str(""),
		Floor:       str(""),
		Code:        str(""),
		Descriptive: str(""),
		Source:      &src,
	}
	if lat != 0 || lon != 0 {
		v.Lat, v.Lon = &lat, &lon
	}
	return v
}
