package httpx

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// Постраничная выдача списков (контракт v1.2: ?limit=&cursor=, заголовок X-Next-Cursor).
//
// Курсор — keyset: значения полей сортировки последней строки страницы, а не OFFSET.
// Следующая страница — индексный диапазон «после курсора» + LIMIT, стоимость запроса не
// растёт с глубиной листания и не зависит от размера таблицы (OFFSET 20 000 заставил бы
// PostgreSQL прочитать и выбросить 20 000 строк). Тело ответа — по-прежнему массив:
// клиенты, не знающие о страницах, получают первую страницу.

// HeaderNextCursor — курсор следующей страницы; заголовка нет — страница последняя.
const HeaderNextCursor = "X-Next-Cursor"

// maxCursorLen — курсор несёт пару-тройку значений; длиннее — мусор.
const maxCursorLen = 512

// Page — параметры страницы из запроса. Cursor == nil — первая страница.
type Page struct {
	Limit  int
	Cursor []string
}

// ParsePage — ?limit (по умолчанию def, не больше max) и ?cursor. Некорректные значения —
// 400 validation с details.fields (молча подменять нельзя: клиент решит, что данных нет).
func ParsePage(r *http.Request, def, max int) (Page, error) {
	q := r.URL.Query()
	p := Page{Limit: def}
	if s := strings.TrimSpace(q.Get("limit")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return p, Validation("Некорректные параметры страницы", map[string]string{"limit": "Целое число от 1"})
		}
		p.Limit = min(n, max)
	}
	if s := strings.TrimSpace(q.Get("cursor")); s != "" {
		parts, ok := DecodeCursor(s)
		if !ok {
			return p, BadCursor()
		}
		p.Cursor = parts
	}
	return p, nil
}

// BadCursor — курсор не разобран (подделан, от другого списка, устарел формат).
func BadCursor() *Error {
	return Validation("Некорректный курсор страницы — начните список сначала", map[string]string{"cursor": "Некорректный курсор"})
}

// EncodeCursor — непрозрачный курсор: base64url(JSON-массив строк). Клиент его не разбирает.
func EncodeCursor(parts ...string) string {
	b, _ := json.Marshal(parts)
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor — обратное EncodeCursor.
func DecodeCursor(s string) ([]string, bool) {
	if len(s) > maxCursorLen {
		return nil, false
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	var parts []string
	if err := json.Unmarshal(b, &parts); err != nil || len(parts) == 0 {
		return nil, false
	}
	return parts, true
}

// SetNextCursor — выставить X-Next-Cursor (вызывать до записи тела).
func SetNextCursor(w http.ResponseWriter, parts ...string) {
	w.Header().Set(HeaderNextCursor, EncodeCursor(parts...))
}
