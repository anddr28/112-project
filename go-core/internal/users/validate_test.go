package users

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestCheckLogin(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in string
		ok bool
	}{
		{"abc", true},
		{"student2", true},
		{"ivan.petrov-01_x", true},
		{"ABC", true},
		{strings.Repeat("a", 64), true},
		{"ab", false},
		{"", false},
		{strings.Repeat("a", 65), false},
		{"иванов", false}, // кириллица в логине не допускается
		{"with space", false},
		{"a@b.c", false},
		{"nul\x00x", false},
	} {
		if got := checkLogin(c.in); (got == "") != c.ok {
			t.Errorf("checkLogin(%q) = %q, want ok=%v", c.in, got, c.ok)
		}
	}
}

func TestCheckPassword(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, in string
		want     string // "" — годен; иначе подстрока сообщения
	}{
		{"7 символов", "1234567", "не короче 8"},
		{"8 кириллических (16 байт)", "пароль12", ""},
		{"ровно 256 байт", strings.Repeat("x", 256), ""},
		{"257 байт", strings.Repeat("x", 257), "слишком длинный"},
		{"128 кириллических = 256 байт", strings.Repeat("я", 128), ""},
		{"129 кириллических = 258 байт", strings.Repeat("я", 129), "слишком длинный"},
		{"только пробелы", "        ", "пробелов"},
		{"пробелы внутри", "  pass word  ", ""},
		{"пусто", "", "не короче 8"},
	} {
		got := checkPassword(c.in)
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(strings.ToLower(got), strings.ToLower(c.want)) {
			t.Errorf("%s: checkPassword = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCheckRoleAndNames(t *testing.T) {
	t.Parallel()
	for _, r := range []string{"admin", "teacher", "student"} {
		if checkRole(r) != "" {
			t.Errorf("роль %s отвергнута", r)
		}
	}
	for _, r := range []string{"", "Admin", "root", " student"} {
		if checkRole(r) == "" {
			t.Errorf("роль %q принята", r)
		}
	}

	if got := checkName("", "фамилию"); got != "Укажите фамилию" {
		t.Errorf("пустая фамилия: %q", got)
	}
	if got := checkName(strings.Repeat("Ё", 100), "имя"); got != "" {
		t.Errorf("100 рун: %q", got)
	}
	if got := checkName(strings.Repeat("Ё", 101), "имя"); got == "" {
		t.Error("101 руна принята")
	}
	if got := checkName("Ива\x00нов", "фамилию"); got == "" {
		t.Error("NUL в фамилии принят (PostgreSQL отверг бы запись с 500)")
	}
	if checkMiddle("") != "" || checkMiddle("Сергеевна") != "" {
		t.Error("отчество отвергнуто")
	}
	if checkMiddle("a\x00") == "" || checkMiddle(strings.Repeat("ж", 101)) == "" {
		t.Error("плохое отчество принято")
	}
}

func TestParseGroupIDs(t *testing.T) {
	t.Parallel()
	a, b := uuid.New(), uuid.New()
	got, msg := parseGroupIDs(nil)
	if msg != "" || got == nil || len(got) != 0 {
		t.Fatalf("nil: %v %q", got, msg)
	}
	got, msg = parseGroupIDs([]string{a.String(), " " + b.String() + " ", a.String(), strings.ToUpper(b.String())})
	if msg != "" || len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("дедупликация/порядок: %v %q", got, msg)
	}
	if _, msg = parseGroupIDs([]string{a.String(), "не-uuid"}); msg == "" {
		t.Fatal("кривой uuid принят")
	}
	many := make([]string, maxGroups+1)
	for i := range many {
		many[i] = uuid.NewString()
	}
	if _, msg = parseGroupIDs(many); msg == "" {
		t.Fatal("слишком много групп принято")
	}
}

func TestOptStringAndPatchEmpty(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		body              string
		set, null         bool
		value             string
		empty, wantDecErr bool
	}{
		{`{}`, false, false, "", true, false},
		{`{"serviceId": null}`, true, true, "", false, false},
		{`{"serviceId": ""}`, true, false, "", false, false},
		{`{"serviceId": "abc"}`, true, false, "abc", false, false},
		{`{"serviceId": 5}`, false, false, "", false, true},
		{`{"groupIds": []}`, false, false, "", false, false},
		{`{"middleName": null}`, false, false, "", true, false}, // null без значения = «не менять»
		{`{"password": "x"}`, false, false, "", false, false},
		{`{"unknownField": 1}`, false, false, "", true, false},
	} {
		var in patchInput
		err := json.Unmarshal([]byte(c.body), &in)
		if (err != nil) != c.wantDecErr {
			t.Errorf("%s: err=%v", c.body, err)
			continue
		}
		if err != nil {
			continue
		}
		if in.ServiceID.Set != c.set || in.ServiceID.Null != c.null || in.ServiceID.Value != c.value {
			t.Errorf("%s: serviceId=%+v", c.body, in.ServiceID)
		}
		if in.empty() != c.empty {
			t.Errorf("%s: empty=%v", c.body, in.empty())
		}
	}
}

func TestEscapeLikeAndHelpers(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"plain":      "plain",
		"Рожкова":    "Рожкова",
		"100%":       `100\%`,
		"a_b":        `a\_b`,
		`back\slash`: `back\\slash`,
		`%_\`:        `\%\_\\`,
		"":           "",
	} {
		if got := escapeLike(in); got != want {
			t.Errorf("escapeLike(%q)=%q, want %q", in, got, want)
		}
	}
	s, blank := "x", "  "
	if nonEmpty(nil) != nil || nonEmpty(&blank) != nil || nonEmpty(&s) != &s {
		t.Error("nonEmpty")
	}
	if trimPtr(nil) != "" || trimPtr(&blank) != "" || trimPtr(&s) != "x" {
		t.Error("trimPtr")
	}
	if nilIfEmpty("") != nil || *nilIfEmpty("a") != "a" {
		t.Error("nilIfEmpty")
	}
}
