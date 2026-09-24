package scoring

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Нормализация значений карточки перед сравнением с эталоном (слой 1).
// Всё детерминированно и без регулярных выражений: вызывается на каждом submit
// для десятка полей, аллокации — только на результирующие строки.

// NormalizeText — регистр, ё→е, пробелы схлопнуты, по краям обрезано, хвостовая
// пунктуация (". , ; : ! ? …") снята, типографские тире приведены к «-».
func NormalizeText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(foldRune(r))
	}
	return strings.TrimRightFunc(b.String(), isTrailPunct)
}

// NormalizePhone — только цифры, последние 10: «+7 (903) 511-33-67» и «8 903 5113367»
// дают одно и то же. Короткие номера (112, 01) остаются как есть.
func NormalizePhone(s string) string {
	var buf [24]byte
	d := buf[:0]
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= '0' && c <= '9' {
			d = append(d, c)
		}
	}
	if len(d) > 10 {
		d = d[len(d)-10:]
	}
	return string(d)
}

// NormalizeAddressPart — часть адреса без слов-типов («ул.», «улица», «д.», «кв.», «подъезд»…),
// без пунктуации, с числами без ведущих нулей и литерой, прижатой к номеру дома
// («д. 12 А» → «12а», «Тверская улица» → «тверская», «этаж 05» → «5»).
func NormalizeAddressPart(s string) string {
	toks := addrTokens(s, false)
	switch len(toks) {
	case 0:
		return ""
	case 1:
		return toks[0]
	}
	return strings.Join(toks, " ")
}

// foldRune — нижний регистр, ё→е, все виды тире/минуса → '-'.
func foldRune(r rune) rune {
	if r < 0x80 {
		if 'A' <= r && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}
	switch r {
	case 'ё', 'Ё':
		return 'е'
	case '‐', '‑', '‒', '–', '—', '―', '−':
		return '-'
	}
	return unicode.ToLower(r)
}

func isTrailPunct(r rune) bool {
	switch r {
	case '.', ',', ';', ':', '!', '?', '…':
		return true
	}
	return unicode.IsSpace(r)
}

// normScalar — нормализованное скалярное значение: текст, а если это число — каноническая
// запись числа («05» = «5», «5,0» = «5»), чтобы «5» из поля ввода совпадало с 5 из эталона.
func normScalar(s string) string {
	t := NormalizeText(s)
	if n, ok := canonNumber(t); ok {
		return n
	}
	return t
}

// canonNumber — каноническая запись десятичного числа. Принимает только цифры, один
// разделитель (. или ,) и знак в начале — «inf», «1e5», «0x10» числами не считаются.
func canonNumber(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	digits, sep, simple := 0, false, true
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			digits++
		case (c == '.' || c == ',') && !sep:
			sep, simple = true, false
		case (c == '-' || c == '+') && i == 0:
			simple = false
		default:
			return "", false
		}
	}
	if digits == 0 {
		return "", false
	}
	// Быстрый путь: целое без знака и ведущих нулей уже канонично — без аллокаций.
	if simple && (s[0] != '0' || len(s) == 1) {
		return s, true
	}
	f, err := strconv.ParseFloat(strings.Replace(s, ",", ".", 1), 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return "", false
	}
	return formatNum(f), true
}

func formatNum(f float64) string {
	if f == 0 {
		return "0" // без «-0»
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// sortUnique — отсортированное множество без пустых строк (in-place).
func sortUnique(s []string) []string {
	s = slices.DeleteFunc(s, func(x string) bool { return x == "" })
	if len(s) < 2 {
		return s
	}
	slices.Sort(s)
	return slices.Compact(s)
}

// wordTokens — множество слов (для ФИО: порядок «Фамилия Имя» не важен). Сначала свёртка
// (типографский дефис «Римский‑Корсаков» = «Римский-Корсаков»), затем разбиение; одиночный
// или краевой дефис («Иванов - Пётр») словом не считается.
func wordTokens(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(foldString(s), isWordSep) {
		if f = strings.Trim(f, "-"); f != "" {
			out = append(out, NormalizeText(f))
		}
	}
	return sortUnique(out)
}

func isWordSep(r rune) bool {
	return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-')
}

// ---------------------------------------------------------------- адрес

// addrTypeWords — слова-типы адресных элементов (после снятия точек). Удаляются с обеих
// сторон сравнения, поэтому лишнее слово в списке ничего не ломает, а пропущенное —
// даёт ложное «не совпадает».
var addrTypeWords = map[string]struct{}{
	// улицы
	"ул": {}, "улица": {}, "пр": {}, "пр-т": {}, "пр-кт": {}, "просп": {}, "проспект": {},
	"пер": {}, "переулок": {}, "ш": {}, "шоссе": {}, "б-р": {}, "бул": {}, "бульвар": {},
	"пл": {}, "площадь": {}, "наб": {}, "набережная": {}, "проезд": {}, "пр-д": {},
	"туп": {}, "тупик": {}, "ал": {}, "аллея": {}, "мкр": {}, "мкрн": {}, "микрорайон": {},
	"кв-л": {}, "квартал": {},
	// населённые пункты и территории
	"г": {}, "гор": {}, "город": {}, "пос": {}, "поселок": {}, "п": {}, "пгт": {},
	"с": {}, "село": {}, "дер": {}, "деревня": {}, "обл": {}, "область": {}, "р-н": {},
	"район": {}, "респ": {}, "республика": {}, "край": {},
	// здание
	"д": {}, "дом": {}, "корп": {}, "корпус": {}, "к": {}, "стр": {}, "строение": {},
	"влд": {}, "вл": {}, "владение": {}, "лит": {}, "литера": {},
	// детали внутри здания
	"кв": {}, "квартира": {}, "под": {}, "подъезд": {}, "эт": {}, "этаж": {},
	"оф": {}, "офис": {}, "пом": {}, "помещение": {},
}

// addrDetailWords — детали внутри здания. В address.raw они вместе со своим номером
// не участвуют в сравнении: подъезд/этаж/квартира — отдельные поля карточки, а единая
// строка адреса сверяется до уровня здания.
var addrDetailWords = map[string]struct{}{
	"кв": {}, "квартира": {}, "под": {}, "подъезд": {}, "эт": {}, "этаж": {},
	"оф": {}, "офис": {}, "пом": {}, "помещение": {},
}

// addrCountryWords — страна в единой строке адреса не различает адреса учебного контура.
var addrCountryWords = map[string]struct{}{"россия": {}, "рф": {}, "российская": {}, "федерация": {}}

// addrSynonyms — сокращения, которые пишут вместо полного слова.
var addrSynonyms = map[string]string{"б": "большая", "м": "малая", "рф": "россия"}

// buildingMarks — буквы между цифрами внутри номера: «12к2» = дом 12 корпус 2.
var buildingMarks = map[string]struct{}{"к": {}, "корп": {}, "корпус": {}, "с": {}, "стр": {}, "строение": {}}

type addrTok struct {
	s   string
	seg int // номер сегмента (между запятыми) — для привязки «подъезд 3» к своему номеру
}

// addrTokens — токены адреса: нижний регистр, ё→е, разделители — всё, кроме букв, цифр,
// '-' и '/'; сегменты — по запятым/скобкам. raw=true — единая строка: убираются
// детали внутри здания с номерами и страна.
func addrTokens(s string, raw bool) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	folded := foldString(s)
	toks := make([]addrTok, 0, 8)
	seg, start := 0, -1
	flush := func(end int) {
		if start >= 0 {
			toks = appendAddrTok(toks, folded[start:end], seg)
			start = -1
		}
	}
	for i, r := range folded {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '/' {
			if start < 0 {
				start = i
			}
			continue
		}
		flush(i)
		switch r {
		case ',', ';', '(', ')', '\n':
			seg++
		}
	}
	flush(len(folded))

	// Детали внутри здания (только единая строка): «подъезд 3», «5 этаж», «кв. 12».
	if raw {
		kept := toks[:0]
		for i := 0; i < len(toks); i++ {
			t := toks[i]
			if _, ok := addrDetailWords[t.s]; !ok {
				kept = append(kept, t)
				continue
			}
			if i+1 < len(toks) && toks[i+1].seg == t.seg && digitLead(toks[i+1].s) {
				i++ // номер после слова
			} else if n := len(kept); n > 0 && kept[n-1].seg == t.seg && digitLead(kept[n-1].s) {
				kept = kept[:n-1] // номер перед словом
			}
		}
		toks = kept
	}

	out := make([]string, 0, len(toks))
	prevSeg := -1
	for _, t := range toks {
		if _, ok := addrTypeWords[t.s]; ok {
			continue
		}
		if raw {
			if _, ok := addrCountryWords[t.s]; ok {
				continue
			}
		}
		w := t.s
		// Литера отдельно от номера: «12 а» → «12а» (до синонимов: «12 Б» — литера, не «Большая»).
		if n := len(out); n > 0 && prevSeg == t.seg && isDigitsOnly(out[n-1]) && singleLetter(w) {
			out[n-1] += w
			continue
		}
		if syn, ok := addrSynonyms[w]; ok {
			w = syn
		}
		out = append(out, w)
		prevSeg = t.seg
	}
	return out
}

// appendAddrTok — чистка токена: краевые '-' и '/', «12-а» → «12а», «12к2» → «12» «2»,
// ведущие нули у чисел.
func appendAddrTok(dst []addrTok, t string, seg int) []addrTok {
	t = strings.Trim(t, "-/")
	if t == "" {
		return dst
	}
	if !digitLead(t) {
		return append(dst, addrTok{t, seg})
	}
	// «12к2», «12стр1»: буквы между цифрами — корпус/строение, делим на два номера.
	if a, b, ok := splitBuilding(t); ok {
		dst = appendAddrTok(dst, a, seg)
		return appendAddrTok(dst, b, seg)
	}
	if strings.IndexByte(t, '-') >= 0 {
		t = dropDashBeforeLetter(t)
	}
	if isDigitsOnly(t) && len(t) > 1 && t[0] == '0' {
		t = strings.TrimLeft(t, "0")
		if t == "" {
			t = "0"
		}
	}
	return append(dst, addrTok{t, seg})
}

// splitBuilding — «12к2» → («12», «2»). Только если между цифрами стоит метка корпуса/строения.
func splitBuilding(t string) (string, string, bool) {
	i := 0
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	j := i
	for j < len(t) && !(t[j] >= '0' && t[j] <= '9') {
		j++
	}
	if i == j || j == len(t) {
		return "", "", false
	}
	if _, ok := buildingMarks[t[i:j]]; !ok {
		return "", "", false
	}
	return t[:i], t[j:], true
}

// dropDashBeforeLetter — «12-а» → «12а», «1-я» → «1я»; «12-14» не трогаем.
func dropDashBeforeLetter(t string) string {
	var b strings.Builder
	b.Grow(len(t))
	rs := []rune(t)
	for i, r := range rs {
		if r == '-' && i+1 < len(rs) && unicode.IsLetter(rs[i+1]) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func foldString(s string) string {
	// Быстрый путь: ASCII в нижнем регистре — без копии.
	clean := true
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 0x80 || ('A' <= c && c <= 'Z') {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		b.WriteRune(foldRune(r))
	}
	return b.String()
}

func digitLead(s string) bool { return s != "" && s[0] >= '0' && s[0] <= '9' }

func isDigitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func singleLetter(s string) bool {
	rs := []rune(s)
	return len(rs) == 1 && unicode.IsLetter(rs[0])
}

// addressRawMatch — сверка единой строки адреса до уровня здания:
//   - номера (дом/корпус/строение) совпадают как множества;
//   - слова одной стороны — подмножество слов другой (страна/регион/округ могут быть
//     не указаны, но чужих слов быть не должно);
//   - улица из структурного адреса эталона (если есть) присутствует в ответе —
//     иначе «Москва, 12» совпала бы с «Москва, Тверская, 12».
//
// exp/act — отсортированные множества токенов (addrTokens+sortUnique).
func addressRawMatch(exp, act, expStreet []string) bool {
	expN, expW := splitNumsWords(exp)
	actN, actW := splitNumsWords(act)
	if !slices.Equal(expN, actN) {
		return false
	}
	if len(expW) > 0 && len(actW) == 0 {
		return false
	}
	if !subset(expW, actW) && !subset(actW, expW) {
		return false
	}
	for _, w := range expStreet {
		if digitLead(w) {
			continue // номер в названии улицы уже сверен как число
		}
		if _, ok := slices.BinarySearch(actW, w); !ok {
			return false
		}
	}
	return true
}

// splitNumsWords — числа и слова из отсортированного множества (порядок сохраняется).
func splitNumsWords(s []string) (nums, words []string) {
	for _, t := range s {
		if digitLead(t) {
			nums = append(nums, t)
		} else {
			words = append(words, t)
		}
	}
	return nums, words
}

// subset — a ⊆ b; оба отсортированы.
func subset(a, b []string) bool {
	if len(a) > len(b) {
		return false
	}
	for _, x := range a {
		if _, ok := slices.BinarySearch(b, x); !ok {
			return false
		}
	}
	return true
}
