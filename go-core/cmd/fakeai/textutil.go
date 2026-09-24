package main

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

func ptr[T any](v T) *T { return &v }

func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

// norm — нижний регистр и «ё» → «е»: подсказки брифа и факты эталона пишут люди,
// сравнение не должно зависеть от того, поставил ли кто-то точки над ё.
func norm(s string) string {
	return strings.ReplaceAll(strings.ToLower(s), "ё", "е")
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func isCyrillic(r rune) bool { return unicode.Is(unicode.Cyrillic, r) }

// tokens — слова текста: буквы/цифры, дефис внутри слова сохраняется («кто-нибудь»).
func tokens(s string) []string {
	var out []string
	start := -1
	for i, r := range s {
		if isWordRune(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if r == '-' && start >= 0 {
			continue
		}
		if start >= 0 {
			out = append(out, strings.TrimRight(s[start:i], "-"))
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, strings.TrimRight(s[start:], "-"))
	}
	return out
}

// countWords — число слов без аллокаций (тот же критерий слова, что у tokens).
func countWords(s string) int {
	n, in := 0, false
	for _, r := range s {
		switch {
		case isWordRune(r):
			if !in {
				n++
				in = true
			}
		case r == '-' && in:
		default:
			in = false
		}
	}
	return n
}

// stem — грубая «основа» слова, как в моке фронта (computeSemantic): отрезаем
// окончание, но не короче 4 букв. Для русского этого хватает, чтобы «пятом этаже»
// совпало с «5 этаж», а «квартире» — с «квартира».
func stem(w string) string {
	n := utf8.RuneCountInString(w)
	k := max(4, n-2)
	if k >= n {
		return w
	}
	i := 0
	for j := range w {
		if i == k {
			return w[:j]
		}
		i++
	}
	return w
}

func isNumber(w string) bool {
	if w == "" {
		return false
	}
	for _, r := range w {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// numberStems — как число звучит словами: «5 этаж» в эталоне и «пятом этаже»
// в ответе студента — один и тот же факт.
var numberStems = map[string][]string{
	"1": {"перв", "один", "одна", "одно"}, "2": {"втор", "двое", "двух", "два", "две"},
	"3": {"трет", "трое", "трех", "три"}, "4": {"четв", "четыр"}, "5": {"пят"}, "6": {"шест"},
	"7": {"седьм", "сем"}, "8": {"восьм", "восем"}, "9": {"девят"}, "10": {"десят"},
	"11": {"одиннадцат"}, "12": {"двенадцат"},
}

// containsAny — первая подстрока из subs, найденная в text (оба уже нормализованы).
func containsAny(text string, subs []string) (string, bool) {
	for _, s := range subs {
		if s != "" && strings.Contains(text, s) {
			return s, true
		}
	}
	return "", false
}

// normList — нормализованные непустые строки (подсказки брифа/чек-листа).
func normList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = norm(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// checklistStop — служебные слова формулировок пунктов («Уточнил адрес»): по ним
// выполнение пункта не распознать, это глагол-обёртка, а не суть вопроса.
var checklistStop = map[string]bool{
	"уточнить": true, "уточнил": true, "уточнила": true, "спросить": true, "спросил": true,
	"спросила": true, "сообщить": true, "сообщил": true, "сообщила": true, "выяснить": true,
	"выяснил": true, "выяснила": true, "задать": true, "задал": true, "нужно": true, "чтобы": true,
	"какие": true, "какой": true, "какая": true, "говорил": true, "говорила": true, "назвать": true,
	"назвал": true, "оператор": true, "заявителя": true, "заявителю": true, "заявитель": true,
}

// meaningfulStems — основы значимых слов (≥5 букв, не служебных): запасной способ
// сопоставления, когда преподаватель не задал подсказки пункту/факту.
func meaningfulStems(text string) []string {
	ws := tokens(norm(text))
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		if utf8.RuneCountInString(w) >= 5 && !checklistStop[w] {
			out = append(out, stem(w))
		}
	}
	return out
}

// truncateWords — не длиннее limit слов (options.max_reply_words): обрезаем по
// границе слова и ставим многоточие, как оборванную реплику.
func truncateWords(s string, limit int) string {
	if limit <= 0 {
		return s
	}
	cnt, in := 0, false
	for i, r := range s {
		if isWordRune(r) {
			if !in {
				cnt++
				in = true
				if cnt > limit {
					return strings.TrimRight(strings.TrimSpace(s[:i]), ",;:—–- ") + "…"
				}
			}
			continue
		}
		if r == '-' && in {
			continue
		}
		in = false
	}
	return s
}

func capitalize(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError || unicode.IsUpper(r) {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

func isTerminal(r rune) bool { return r == '.' || r == '!' || r == '?' || r == '…' }

// endSentence — гарантирует знак конца предложения (реплики склеиваются из фактов).
func endSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	r, _ := utf8.DecodeLastRuneInString(s)
	if isTerminal(r) {
		return s
	}
	return strings.TrimRight(s, ",;:—–- ") + "."
}

// snippet — короткая цитата реплики для комментария (без ПДн-рисков: это текст разговора).
func snippet(s string, limit int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	i := 0
	for j := range s {
		if i == limit {
			return strings.TrimSpace(s[:j]) + "…"
		}
		i++
	}
	return s
}
