package main

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTokensAndCountWords(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ,.!", nil},
		{"Кто-нибудь, помогите!", []string{"Кто-нибудь", "помогите"}},
		{"5 этаж, кв.12", []string{"5", "этаж", "кв", "12"}},
		{"-дефис- в начале и конце-", []string{"дефис", "в", "начале", "и", "конце"}},
		{"слово--слово", []string{"слово--слово"}},
		{"ёлка Ёж", []string{"ёлка", "Ёж"}},
		{"🔥горит🔥", []string{"горит"}},
		{"tab\tи\nперевод", []string{"tab", "и", "перевод"}},
	}
	for _, c := range cases {
		if got := tokens(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("tokens(%q) = %q, ждали %q", c.in, got, c.want)
		}
		if got := countWords(c.in); got != len(c.want) {
			t.Errorf("countWords(%q) = %d, ждали %d", c.in, got, len(c.want))
		}
	}
}

func TestStemAndNorm(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"квартире": "кварти", "квартира": "кварти", "этаже": "этаж", "этаж": "этаж", "дом": "дом", "": "",
		"пострадавших": "пострадавш", "пятом": "пято",
	} {
		if got := stem(in); got != want {
			t.Errorf("stem(%q) = %q, ждали %q", in, got, want)
		}
	}
	if norm("Ёлка ЁЖ ещё") != "елка еж еще" {
		t.Errorf("norm: %q", norm("Ёлка ЁЖ ещё"))
	}
}

func TestIsNumber(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]bool{"5": true, "112": true, "": false, "5а": false, "пять": false, "-1": false, "٣": false} {
		if got := isNumber(in); got != want {
			t.Errorf("isNumber(%q) = %v", in, got)
		}
	}
}

func TestTruncateWords(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in    string
		limit int
		want  string
	}{
		{"Раз два три четыре", 2, "Раз два…"},
		{"Раз, два, три", 2, "Раз, два…"},
		{"Раз — два — три", 2, "Раз — два…"},
		{"Раз два", 2, "Раз два"},
		{"Раз два", 0, "Раз два"},
		{"Раз два", -1, "Раз два"},
		{"", 3, ""},
		{"Кто-нибудь придёт? Да", 1, "Кто-нибудь…"},
	}
	for _, c := range cases {
		if got := truncateWords(c.in, c.limit); got != c.want {
			t.Errorf("truncateWords(%q, %d) = %q, ждали %q", c.in, c.limit, got, c.want)
		}
		if c.limit > 0 && countWords(truncateWords(c.in, c.limit)) > c.limit {
			t.Errorf("truncateWords(%q, %d): слов больше лимита", c.in, c.limit)
		}
	}
}

func TestSentenceHelpers(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"дом": "Дом", "Дом": "Дом", "": "", "ёж": "Ёж", "123": "123", "a": "A"} {
		if got := capitalize(in); got != want {
			t.Errorf("capitalize(%q) = %q", in, got)
		}
	}
	for in, want := range map[string]string{
		"Горит": "Горит.", "Горит!": "Горит!", "Горит,": "Горит.", "  Горит  ": "Горит.", "": "", "Горит…": "Горит…",
		"Горит —": "Горит.", "Кто?": "Кто?",
	} {
		if got := endSentence(in); got != want {
			t.Errorf("endSentence(%q) = %q", in, got)
		}
	}
	long := strings.Repeat("я", 100)
	if got := snippet(long, 60); utf8.RuneCountInString(got) != 61 || !strings.HasSuffix(got, "…") {
		t.Errorf("snippet: %q", got)
	}
	if got := snippet("  коротко  ", 60); got != "коротко" {
		t.Errorf("snippet: %q", got)
	}
	if got := snippet("ровно пять", 10); got != "ровно пять" {
		t.Errorf("snippet на границе: %q", got)
	}
}

func TestListHelpers(t *testing.T) {
	t.Parallel()
	if got := normList([]string{" Подъезд ", "", "  ", "ЁЛКА"}); !reflect.DeepEqual(got, []string{"подъезд", "елка"}) {
		t.Errorf("normList: %q", got)
	}
	if got := normList(nil); got == nil || len(got) != 0 {
		t.Errorf("normList(nil): %#v", got)
	}
	if s, ok := containsAny("какой подъезд", []string{"", "этаж", "подъезд"}); !ok || s != "подъезд" {
		t.Errorf("containsAny: %q %v", s, ok)
	}
	if _, ok := containsAny("что угодно", []string{""}); ok {
		t.Error("пустая подстрока не должна совпадать")
	}
	// Служебные слова и короткие слова не участвуют.
	if got := meaningfulStems("Уточнил контактный телефон и адрес"); !reflect.DeepEqual(got, []string{"контактн", "телеф", "адре"}) {
		t.Errorf("meaningfulStems: %q", got)
	}
	if got := dedupeNonEmpty([]string{"a", " ", "a", "b", ""}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("dedupeNonEmpty: %q", got)
	}
}

func TestPtrDeref(t *testing.T) {
	t.Parallel()
	if *ptr(5) != 5 || deref[int](nil) != 0 || deref(ptr("x")) != "x" || trimmed(nil) != "" || trimmed(ptr("  a ")) != "a" {
		t.Fatal("ptr/deref/trimmed")
	}
}
