package classifier

import (
	"slices"
	"testing"
)

func TestNormalize(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"---", ""},
		{"Пожар", "пожар"},
		{"ПОЖАР: мусор", "пожар мусор"},
		{"Ёлка ёжик", "елка ежик"},
		{"ЁЛКА", "елка"},
		{"  Справка-101  ", "справка 101"},
		{"пожар,,,  мусор..", "пожар мусор"},
		{"Ленинский пр-т, д.32а", "ленинский пр т д 32а"},
		{"Tverskaya 12", "tverskaya 12"},
		{"«Мосгаз»/ДУ", "мосгаз ду"},
		{"a\tb\nc", "a b c"},
		{"12/1", "12 1"},
	} {
		if got := normalize(tc.in); got != tc.want {
			t.Errorf("normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTokens(t *testing.T) {
	t.Parallel()
	if got := tokens(""); got != nil {
		t.Errorf("tokens(\"\") = %#v, want nil", got)
	}
	if got := tokens(" ,.- "); got != nil {
		t.Errorf("tokens(punct) = %#v, want nil", got)
	}
	if got := tokens("Запах  газа, подъезд"); !slices.Equal(got, []string{"запах", "газа", "подъезд"}) {
		t.Errorf("tokens = %#v", got)
	}
}

func TestAnyPrefixAndDigits(t *testing.T) {
	t.Parallel()
	toks := []string{"пожар", "мусор"}
	if !anyPrefix(toks, "пож") || !anyPrefix(toks, "мусор") || anyPrefix(toks, "ожар") || anyPrefix(nil, "а") {
		t.Error("anyPrefix")
	}
	if !anyPrefix(toks, "") {
		t.Error("empty word is a prefix of every token")
	}
	for _, tc := range []struct {
		in   string
		want bool
	}{{"", false}, {"12", true}, {"32а", true}, {"а32", false}, {"٣", false}, {"к2", false}} {
		if got := hasDigitPrefix(tc.in); got != tc.want {
			t.Errorf("hasDigitPrefix(%q) = %v", tc.in, got)
		}
	}
}

func TestUpperFirst(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"пожар: мусор", "Пожар: мусор"},
		{"Пожар", "Пожар"},
		{"ёлка", "Ёлка"},
		{"12 этаж", "12 этаж"},
		{"bpla", "Bpla"},
	} {
		if got := upperFirst(tc.in); got != tc.want {
			t.Errorf("upperFirst(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
