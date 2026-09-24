package main

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf16"

	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/components"
)

// Правила «LanguageTool» имитатора. Идентификаторы — как у настоящего LT там, где
// аналог есть (UPPERCASE_SENTENCE_START, MORFOLOGIK_RULE_RU_RU), чтобы фронт и отчёты
// не зависели от того, кто проверял.
const (
	ruleUpperStart = "UPPERCASE_SENTENCE_START"
	ruleAbbrev     = "ABBREVIATION"
	ruleFinalPunct = "MISSING_FINAL_PUNCT"
	ruleDoubleSp   = "DOUBLE_SPACE"
	ruleSpelling   = "MORFOLOGIK_RULE_RU_RU"
)

// abbrevExpansion — сокращения, которые в описании «со слов заявителя» раскрывают.
var abbrevExpansion = map[string]string{"кв": "квартира", "д": "дом", "ул": "улица", "эт": "этаж"}

// abbrevDot — слова, после точки за которыми предложение НЕ заканчивается («ул. Ленина»).
var abbrevDot = map[string]bool{
	"кв": true, "д": true, "ул": true, "эт": true, "г": true, "пр": true, "пер": true, "корп": true,
	"стр": true, "т": true, "им": true, "пос": true, "мкр": true, "просп": true, "ш": true, "наб": true,
	"пл": true, "обл": true, "р": true, "б": true, "св": true, "тел": true, "п": true,
}

// misspellings — частые ошибки операторов: неверная основа → верная. Поиск по
// префиксу слова покрывает словоформы («подьезде» → «подъезде»).
var misspellings = []struct{ bad, good string }{
	{"подьезд", "подъезд"}, {"подезд", "подъезд"}, {"обьект", "объект"}, {"обьясн", "объясн"},
	{"обьявл", "объявл"}, {"извени", "извини"}, {"пожалуста", "пожалуйста"}, {"вообщем", "в общем"},
	{"впринципе", "в принципе"}, {"зделал", "сделал"}, {"помошь", "помощь"}, {"помоши", "помощи"},
	{"помошн", "помощн"}, {"обезательн", "обязательн"}, {"адресс", "адрес"}, {"аддрес", "адрес"},
	{"калидор", "коридор"}, {"колидор", "коридор"}, {"лесниц", "лестниц"}, {"сдесь", "здесь"},
	{"агонь", "огонь"}, {"пострадавщ", "пострадавш"}, {"тоесть", "то есть"}, {"незнаю", "не знаю"},
	{"немогу", "не могу"}, {"задымленни", "задымлени"}, {"евакуац", "эвакуац"}, {"расположеный", "расположенный"},
}

func misspelled(low string) (bad, good string, ok bool) {
	for _, m := range misspellings {
		if strings.HasPrefix(low, m.bad) {
			return m.bad, m.good, true
		}
	}
	return "", "", false
}

// checkGrammar — слой 2 (как computeGrammar в моке фронта, но по всем полям и
// предложениям). Смещения — в UTF-16 единицах, как у LanguageTool: фронт подсвечивает
// по JS-строке, для кириллицы это совпадает с номером символа.
func checkGrammar(req *aiservice.GrammarJobRequest) components.GrammarResult {
	var exclude map[string]bool
	strict := false
	if o := req.Options; o != nil {
		strict = deref(o.Strict)
		if o.ExcludeRules != nil {
			exclude = make(map[string]bool, len(*o.ExcludeRules))
			for _, r := range *o.ExcludeRules {
				exclude[r] = true
			}
		}
	}
	remarks := make([]components.GrammarRemark, 0, 8)
	words := 0
	for _, t := range req.Texts {
		words += countWords(t.Text)
		remarks = checkText(t.Field, t.Text, exclude, remarks)
	}

	var errs, warns, styles int
	for _, r := range remarks {
		switch r.Severity {
		case components.Error:
			errs++
		case components.Warning:
			warns++
		default:
			styles++
		}
	}
	// Формула fake-rules-1 (как в моке фронта). Стиль снижает балл только при
	// options.strict=true (контракт: strict, default false — «считать style-замечания
	// в score»); сами style-замечания возвращаются всегда.
	penalty := 15*errs + 7*warns
	if strict {
		penalty += 3 * styles
	}
	score := max(0, 100-penalty)

	res := components.GrammarResult{Remarks: remarks, Score: float32(score)}
	res.Stats.WordsChecked = words
	res.Stats.ErrorsBySeverity = &map[string]int{"error": errs, "warning": warns, "style": styles}
	return res
}

func checkText(field, text string, exclude map[string]bool, out []components.GrammarRemark) []components.GrammarRemark {
	if strings.TrimSpace(text) == "" {
		return out
	}
	rs := []rune(text)
	u16 := make([]int, len(rs)+1)
	for i, r := range rs {
		n := utf16.RuneLen(r)
		if n < 1 {
			n = 1
		}
		u16[i+1] = u16[i] + n
	}
	start := len(out)
	add := func(from, to int, sev components.GrammarRemarkSeverity, rule, msg string, sugg ...string) {
		if exclude[rule] || to <= from {
			return
		}
		if sugg == nil {
			sugg = []string{}
		}
		out = append(out, components.GrammarRemark{
			Field: field, Offset: u16[from], Length: u16[to] - u16[from],
			Severity: sev, Rule: ptr(rule), Message: msg, Suggestions: &sugg,
		})
	}

	sentenceStart := true
	prevWord := ""
	for i := 0; i < len(rs); {
		r := rs[i]
		if isWordRune(r) {
			j := i + 1
			for j < len(rs) && (isWordRune(rs[j]) || (rs[j] == '-' && j+1 < len(rs) && isWordRune(rs[j+1]))) {
				j++
			}
			word := string(rs[i:j])
			low := strings.ToLower(word)
			if sentenceStart && unicode.IsLower(r) && isCyrillic(r) {
				add(i, i+1, components.Error, ruleUpperStart,
					"Предложение следует начинать с заглавной буквы.", string(unicode.ToUpper(r)))
			}
			sentenceStart = false
			if exp, ok := abbrevExpansion[low]; ok {
				end := j
				if end < len(rs) && rs[end] == '.' {
					end++
				}
				add(i, end, components.Style, ruleAbbrev,
					"В описании со слов заявителя сокращения лучше раскрывать полностью.", exp)
			} else if bad, good, ok := misspelled(low); ok {
				sugg := good + low[len(bad):]
				if unicode.IsUpper(r) {
					sugg = capitalize(sugg)
				}
				add(i, j, components.Error, ruleSpelling, fmt.Sprintf("Возможно, опечатка: «%s».", word), sugg)
			}
			prevWord = low
			i = j
			continue
		}
		switch {
		case isTerminal(r):
			// «ул. Ленина» — точка сокращения, а не конец предложения.
			if r != '.' || !abbrevDot[prevWord] {
				sentenceStart = true
			}
			prevWord = ""
		case r == ' ':
			j := i
			for j < len(rs) && rs[j] == ' ' {
				j++
			}
			if j-i >= 2 && i > 0 && j < len(rs) && !unicode.IsSpace(rs[i-1]) && !unicode.IsSpace(rs[j]) {
				add(i, j, components.Style, ruleDoubleSp, "Лишние пробелы между словами.", " ")
			}
			i = j
			continue
		case r == '\n':
			// Новая строка в свободном тексте — как правило, новое предложение.
			sentenceStart = true
		}
		i++
	}

	// Знак в конце: пропускаем закрывающие кавычки/скобки («…квартира».).
	k := len(rs) - 1
	for k >= 0 && (unicode.IsSpace(rs[k]) || rs[k] == ')' || rs[k] == '»' || rs[k] == '"') {
		k--
	}
	if k >= 0 && !isTerminal(rs[k]) {
		add(k, k+1, components.Warning, ruleFinalPunct, "В конце предложения отсутствует знак препинания.", ".")
	}

	slices.SortStableFunc(out[start:], func(a, b components.GrammarRemark) int { return a.Offset - b.Offset })
	return out
}
