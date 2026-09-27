package scoring

import (
	"slices"
	"strings"
	"testing"
)

func TestNormalizeText(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"", ""},
		{" \t\n ", ""},
		{"  Ёлка   Зелёная. ", "елка зеленая"},
		{"ПОЖАР!!!", "пожар"},
		{"многоточие…", "многоточие"},
		{"итог: да ;", "итог: да"},
		{"Иванов—Петров", "иванов-петров"},
		{"Римский‑Корсаков", "римский-корсаков"}, // U+2011
		{"минус −5", "минус -5"},
		{"Hello World", "hello world"},
		{"a , b", "a , b"}, // внутренняя пунктуация не трогается
		{"5.", "5"},
		{"строка\nдве", "строка две"},
		{"ÀÉÎ", "àéî"},
	}
	for _, c := range cases {
		if got := NormalizeText(c.in); got != c.want {
			t.Errorf("NormalizeText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizePhone(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"+7 (903) 511-33-67", "9035113367"},
		{"8 903 5113367", "9035113367"},
		{"89035113367", "9035113367"},
		{"9035113367", "9035113367"},
		{"112", "112"},
		{"01", "01"},
		{"", ""},
		{"нет номера", ""},
		{"1234567890123456789012345678", "9012345678"}, // длиннее внутреннего буфера
		{"+7 ９０３", "7"},                                // только ASCII-цифры
	}
	for _, c := range cases {
		if got := NormalizePhone(c.in); got != c.want {
			t.Errorf("NormalizePhone(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeAddressPart(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"ул.", ""},
		{"д. 12 А", "12а"},
		{"12А", "12а"},
		{"12-а", "12а"},
		{"Тверская улица", "тверская"},
		{"ул. Тверская", "тверская"},
		{"этаж 05", "5"},
		{"5 этаж", "5"},
		{"кв. 012", "12"},
		{"корпус 2", "2"},
		{"12к2", "12 2"},
		{"12стр1", "12 1"},
		{"12/2", "12/2"},
		{"12-14", "12-14"},
		{"3-я Парковая", "3я парковая"},
		{"Б. Никитская", "большая никитская"},
		{"Большая Никитская", "большая никитская"},
		{"Черёмушки", "черемушки"},
		{"г. Москва", "москва"},
		{"Россия", "россия"},
		{"РФ", "россия"},
		{"р-н Тверской", "тверской"},
		{"0", "0"},
		{"00", "0"},
		{"—12—", "12"},
	}
	for _, c := range cases {
		if got := NormalizeAddressPart(c.in); got != c.want {
			t.Errorf("NormalizeAddressPart(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormScalar(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"", ""},
		{"05", "5"},
		{"5,0", "5"},
		{"5.50", "5.5"},
		{"0,25", "0.25"},
		{".5", "0.5"},
		{"-0", "0"},
		{"+5", "5"},
		{"-12", "-12"},
		{" 7 ", "7"},
		{"0", "0"},
		{"00", "0"},
		{"1e5", "1e5"}, // экспонента — не число формы
		{"inf", "inf"},
		{"NaN", "nan"},
		{"0x10", "0x10"},
		{"1.2.3", "1.2.3"},
		{"-", "-"},
		{"5-", "5-"},
		{"12 000", "12 000"},
		{"Да", "да"},
	}
	for _, c := range cases {
		if got := normScalar(c.in); got != c.want {
			t.Errorf("normScalar(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSortUnique(t *testing.T) {
	t.Parallel()
	if got := sortUnique([]string{"b", "", "a", "b", ""}); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("sortUnique = %v", got)
	}
	if got := sortUnique(nil); len(got) != 0 {
		t.Errorf("sortUnique(nil) = %v", got)
	}
	if got := sortUnique([]string{""}); len(got) != 0 {
		t.Errorf("sortUnique([\"\"]) = %v", got)
	}
}

func TestWordTokens(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"-", nil},
		{"Иванов Пётр", []string{"иванов", "петр"}},
		{"пётр  ИВАНОВ.", []string{"иванов", "петр"}},
		{"Иванов, Пётр Сергеевич", []string{"иванов", "петр", "сергеевич"}},
		{"Римский-Корсаков Николай", []string{"николай", "римский-корсаков"}},
		// Регрессия: типографский дефис в двойной фамилии — та же фамилия, а не два слова.
		{"Римский‑Корсаков Николай", []string{"николай", "римский-корсаков"}},
		{"Римский–Корсаков Николай", []string{"николай", "римский-корсаков"}},
		// Регрессия: дефис-разделитель с пробелами — не слово «-».
		{"Иванов - Пётр", []string{"иванов", "петр"}},
		{"Иванов — Пётр", []string{"иванов", "петр"}},
		{"-Иванов-", []string{"иванов"}},
		{"Мария Мария", []string{"мария"}},
	}
	for _, c := range cases {
		got := wordTokens(c.in)
		if !slices.Equal(got, c.want) {
			t.Errorf("wordTokens(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func rawSet(s string) []string { return sortUnique(addrTokens(s, true)) }

func TestAddrTokensRaw(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"Москва, Тверская улица, 12", []string{"12", "москва", "тверская"}},
		// страна и детали внутри здания (с номерами) не участвуют в сверке единой строки
		{"Россия, г. Москва, ул. Тверская, д. 12, подъезд 3, этаж 5, кв. 45", []string{"12", "москва", "тверская"}},
		{"Москва, Тверская 12, 3 подъезд", []string{"12", "москва", "тверская"}},
		{"Москва, Тверская, д. 12 под. 3 эт. 5 кв. 45", []string{"12", "москва", "тверская"}},
		{"Москва, Профсоюзная улица, 45, корпус 2", []string{"2", "45", "москва", "профсоюзная"}},
		{"Профсоюзная 45к2", []string{"2", "45", "профсоюзная"}},
		{"Б. Серпуховская ул., 12 Б", []string{"12б", "большая", "серпуховская"}},
		{"ул. 8 Марта, д. 012", []string{"12", "8", "марта"}},
	}
	for _, c := range cases {
		if got := rawSet(c.in); !slices.Equal(got, c.want) {
			t.Errorf("addrTokens(%q, raw) = %q, want %q", c.in, got, c.want)
		}
	}
	// Не-raw режим детали и страну сохраняет (это отдельные поля карточки).
	if got := addrTokens("Россия, кв. 12", false); !slices.Equal(got, []string{"россия", "12"}) {
		t.Errorf("addrTokens(non-raw) = %q", got)
	}
}

func TestAddressRawMatch(t *testing.T) {
	t.Parallel()
	const etalon = "Москва, Тверская улица, 12"
	street := addrTokens("Тверская", false)
	cases := []struct {
		act    string
		street []string
		want   bool
	}{
		{"г. Москва, ул. Тверская, д. 12", street, true},
		{"Россия, Москва, Тверская ул., 12, подъезд 3, этаж 5, кв. 45", street, true},
		{"тверская 12", street, true}, // город можно не указывать
		{"Москва, Тверская, 14", street, false},
		{"Москва, Тверская, 12к2", street, false},
		{"Москва, Новый Арбат, 12", street, false},
		{"Москва, 12", street, false}, // без улицы эталона не совпадает
		{"Москва, 12", nil, true},     // ...а без структурной улицы — только слова/номера
		{"12", nil, false},            // в ответе нет ни одного слова
	}
	for _, c := range cases {
		if got := addressRawMatch(rawSet(etalon), rawSet(c.act), c.street); got != c.want {
			t.Errorf("addressRawMatch(%q, %q, street=%v) = %v, want %v", etalon, c.act, c.street, got, c.want)
		}
	}

	// Корпус: «45, корпус 2» = «45к2» = «д. 45 к. 2».
	exp := rawSet("Москва, Профсоюзная улица, 45, корпус 2")
	for _, a := range []string{"Профсоюзная 45к2", "Профсоюзная ул., д. 45 к. 2", "Москва, ул. Профсоюзная, 45 корп. 2"} {
		if !addressRawMatch(exp, rawSet(a), addrTokens("Профсоюзная", false)) {
			t.Errorf("building: %q should match", a)
		}
	}
	// Литера и сокращение «Б.» = «Большая».
	exp = rawSet("Москва, Б. Серпуховская ул., 12 Б")
	if !addressRawMatch(exp, rawSet("Большая Серпуховская улица, д. 12б"), addrTokens("Б. Серпуховская", false)) {
		t.Error("literal/synonym should match")
	}
	// Номер в названии улицы сверяется как число, а не как слово улицы.
	exp = rawSet("Москва, ул. 8 Марта, 5")
	if !addressRawMatch(exp, rawSet("8 Марта 5"), addrTokens("ул. 8 Марта", false)) {
		t.Error("numbered street should match")
	}
}

func TestSubset(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b []string
		want bool
	}{
		{nil, nil, true},
		{nil, []string{"a"}, true},
		{[]string{"a"}, nil, false},
		{[]string{"a", "c"}, []string{"a", "b", "c"}, true},
		{[]string{"a", "d"}, []string{"a", "b", "c"}, false},
		{[]string{"a", "b", "c"}, []string{"a", "b"}, false},
	}
	for _, c := range cases {
		if got := subset(c.a, c.b); got != c.want {
			t.Errorf("subset(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestFoldString(t *testing.T) {
	t.Parallel()
	if got := foldString("already lower 12"); got != "already lower 12" {
		t.Errorf("foldString fast path = %q", got)
	}
	if got := foldString("ЁЖИК – Ёж"); got != "ежик - еж" {
		t.Errorf("foldString = %q", got)
	}
	if strings.ContainsRune(foldString("a‒b―c"), '‒') {
		t.Error("figure dash not folded")
	}
}
