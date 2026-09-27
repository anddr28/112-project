package users

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
)

// Чистая логика валидации входа /users — без I/O (легко тестировать).

const (
	minLoginLen    = 3
	maxLoginLen    = 64
	minPasswordLen = 8
	maxPasswordLen = 256 // = auth.MaxPasswordLen (длиннее не хэшируем)
	maxNameLen     = 100
	maxGroups      = 100

	msgValidation = "Проверьте заполнение полей"
)

// createInput — тело POST /users (CreateUserInput). Свои типы вместо public.CreateUserInput:
// кривой UUID в groupIds должен дать понятную ошибку поля, а не «Некорректный JSON».
type createInput struct {
	Login      string   `json:"login"`
	Password   string   `json:"password"`
	Role       string   `json:"role"`
	LastName   string   `json:"lastName"`
	FirstName  string   `json:"firstName"`
	MiddleName *string  `json:"middleName"`
	ServiceID  *string  `json:"serviceId"`
	GroupIDs   []string `json:"groupIds"`
}

// patchInput — тело PATCH /users/{id} (UpdateUserInput): отсутствующее поле — не менять;
// serviceId: null — снять профиль службы (поэтому optString, а не *string).
type patchInput struct {
	Role       *string   `json:"role"`
	LastName   *string   `json:"lastName"`
	FirstName  *string   `json:"firstName"`
	MiddleName *string   `json:"middleName"`
	ServiceID  optString `json:"serviceId"`
	Password   *string   `json:"password"`
	GroupIDs   *[]string `json:"groupIds"`
}

// empty — в теле нет ни одного поля (PATCH ничего не меняет).
func (in *patchInput) empty() bool {
	return in.Role == nil && in.LastName == nil && in.FirstName == nil && in.MiddleName == nil &&
		!in.ServiceID.Set && in.Password == nil && in.GroupIDs == nil
}

// optString различает «поля нет», «null» и значение.
type optString struct {
	Set   bool
	Null  bool
	Value string
}

func (o *optString) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Null = true
		return nil
	}
	return json.Unmarshal(b, &o.Value)
}

// checkLogin — 3..64 символа [A-Za-z0-9._-].
func checkLogin(s string) string {
	if len(s) < minLoginLen {
		return "Логин — не короче 3 символов"
	}
	if len(s) > maxLoginLen {
		return "Логин — не длиннее 64 символов"
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return "Логин может содержать только латинские буквы, цифры, точку, дефис и подчёркивание"
		}
	}
	return ""
}

func checkPassword(s string) string {
	n := utf8.RuneCountInString(s)
	if n < minPasswordLen {
		return "Пароль — не короче 8 символов"
	}
	if len(s) > maxPasswordLen {
		return "Пароль слишком длинный"
	}
	if strings.TrimSpace(s) == "" {
		return "Пароль не может состоять из пробелов"
	}
	return ""
}

func checkRole(s string) string {
	if !core.Role(s).Valid() {
		return "Недопустимая роль (admin, teacher или student)"
	}
	return ""
}

// checkName — обязательная часть ФИО (уже обрезанная).
func checkName(s, what string) string {
	if s == "" {
		return "Укажите " + what
	}
	return checkMiddle(s)
}

// checkMiddle — необязательная часть ФИО (уже обрезанная; "" — нет).
func checkMiddle(s string) string {
	if utf8.RuneCountInString(s) > maxNameLen {
		return "Слишком длинное значение"
	}
	if strings.ContainsRune(s, 0) {
		// PostgreSQL не принимает NUL в тексте: без проверки запись упала бы 500.
		return "Недопустимые символы"
	}
	return ""
}

// parseGroupIDs — разбор и дедупликация id групп (порядок сохраняется).
func parseGroupIDs(in []string) ([]uuid.UUID, string) {
	out := make([]uuid.UUID, 0, len(in))
	if len(in) > maxGroups {
		return nil, "Слишком много групп"
	}
	seen := make(map[uuid.UUID]struct{}, len(in))
	for _, s := range in {
		id, err := uuid.Parse(strings.TrimSpace(s))
		if err != nil {
			return nil, "Некорректный идентификатор группы"
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, ""
}

// trimPtr — обрезанная строка из необязательного поля ("" — пусто/нет).
func trimPtr(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}
