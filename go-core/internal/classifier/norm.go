package classifier

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// normalize — нормализация для поиска (Инструкция п.4.2 и фронтовые searchTypes/suggestAddresses):
// регистр не важен, «ё» = «е», знаки препинания можно опускать (любой не буквенно-цифровой
// символ — разделитель), пробелы схлопнуты. Надмножество фронтового правила ([-,.] -> пробел):
// на данных фикстур результаты совпадают, а «пожар: мусор» из XLSX ищется по «пожар мусор».
func normalize(s string) string {
	// Быстрый путь не нужен: строки короткие (запрос, название), а один проход без regexp
	// и с заранее известной ёмкостью — уже одна аллокация.
	var b strings.Builder
	b.Grow(len(s))
	space := true // подавляет ведущий и повторные пробелы
	for _, r := range s {
		switch {
		case r == 'ё' || r == 'Ё':
			r = 'е'
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			r = unicode.ToLower(r)
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
	out := b.String()
	if n := len(out); n > 0 && out[n-1] == ' ' {
		out = out[:n-1]
	}
	return out
}

// tokens — слова нормализованной строки.
func tokens(s string) []string {
	n := normalize(s)
	if n == "" {
		return nil
	}
	return strings.Split(n, " ")
}

// anyPrefix — слово w является префиксом хотя бы одного токена.
func anyPrefix(toks []string, w string) bool {
	for _, t := range toks {
		if strings.HasPrefix(t, w) {
			return true
		}
	}
	return false
}

// hasDigitPrefix — токен начинается с цифры («12», «32а», «1905»).
func hasDigitPrefix(t string) bool {
	if t == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(t)
	return r >= '0' && r <= '9'
}

// upperFirst — первая буква заглавная (подписи узлов из XLSX пишутся как попало).
func upperFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError || unicode.IsUpper(r) {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}
