package attempts

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
)

// ---------------------------------------------------------------- форма черновика (PUT /draft)

// draftKeys — обязательные ключи IncidentCardDraft (frontend.v1.yaml: required) и вид
// их JSON-значения: '{' объект, '[' массив, '"' строка. null и пропуск — ошибка: GET /draft
// отдаёт сохранённое как есть, а фронт обращается к этим полям без проверок
// (card.address.raw.trim(), card.services.map и т. п.).
var draftKeys = [...]struct {
	key  string
	kind byte
}{
	{"phones", '{'}, {"applicant", '{'}, {"address", '{'}, {"incidentTypeIds", '['},
	{"attributes", '{'}, {"description", '"'}, {"actionsTaken", '"'}, {"flags", '{'}, {"services", '['},
}

func kindName(kind byte) string {
	switch kind {
	case '{':
		return "ожидается объект"
	case '[':
		return "ожидается массив"
	}
	return "ожидается строка"
}

// checkDraft — тело автосохранения — это IncidentCardDraft: обязательные ключи на месте и
// нужного вида (точное имя ключа — фронт регистр не сворачивает), address.raw — строка,
// все поля контрактных типов (как при сдаче карточки). Бизнес-ограничения (длина описания,
// перечисления) — при сдаче: черновик — рабочая копия. Цена — два разбора тела в несколько КБ
// (десятки микросекунд) против round-trip в БД.
func checkDraft(body []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return httpx.BadRequest("Ожидается JSON-объект")
	}
	var fields map[string]string
	add := func(k, v string) {
		if fields == nil {
			fields = make(map[string]string, 2)
		}
		fields[k] = v
	}
	for _, k := range draftKeys {
		v, ok := top[k.key]
		switch {
		case !ok:
			add(k.key, "обязательно")
		case len(v) == 0 || v[0] != k.kind:
			add(k.key, kindName(k.kind))
		}
	}
	if fields == nil {
		var addr map[string]json.RawMessage
		if json.Unmarshal(top["address"], &addr) != nil {
			add("address", "ожидается объект")
		} else if raw := addr["raw"]; len(raw) == 0 || raw[0] != '"' {
			add("address.raw", "обязательно, строка")
		}
	}
	if fields != nil {
		return httpx.Validation("Черновик карточки неполный или некорректный", fields)
	}
	var d public.IncidentCardDraft
	if err := json.Unmarshal(body, &d); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) && te.Field != "" {
			return httpx.Validation("Черновик карточки содержит значение неверного типа",
				map[string]string{te.Field: "неверный тип значения"})
		}
		return httpx.BadRequest("Черновик карточки некорректен: " + err.Error())
	}
	return nil
}

// ---------------------------------------------------------------- NUL в тексте

// PostgreSQL не принимает U+0000 ни в jsonb (\u0000 → SQLSTATE 22P05), ни в text (22021).
// Такой символ приходит вставкой из PDF/другой программы и невидим — вырезаем его, а не
// отвергаем всю карточку (иначе студент не может её ни сохранить, ни сдать и не видит почему).

var nulEscape = []byte(`\u0000`)

// stripNULEscapes — валидный JSON без экранированных NUL. Обратная косая черта в валидном
// JSON бывает только внутри строк и всегда начинает escape-последовательность, поэтому
// достаточно пропускать escape'ы целиком («\\u0000» — это «\» и текст «u0000», не NUL).
// Без NUL возвращает тот же срез (без копирования).
func stripNULEscapes(b []byte) []byte {
	if !bytes.Contains(b, nulEscape) {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c != '\\' || i+1 >= len(b) {
			out = append(out, c)
			continue
		}
		if b[i+1] == 'u' && i+6 <= len(b) && string(b[i+2:i+6]) == "0000" {
			i += 5
			continue
		}
		out = append(out, c, b[i+1])
		i++
	}
	return out
}

// stripNUL — строка без U+0000 (текстовые колонки, например action_text).
func stripNUL(s string) string {
	if strings.IndexByte(s, 0) < 0 {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}

// ---------------------------------------------------------------- службы после слияния

// decodeServices — массив служб из SQL-слияния; элементы, которые не разбираются как
// AssignedService (мусор, сохранённый до проверки формы черновика), отбрасываются — сдача
// карточки из-за них не падает.
func decodeServices(b []byte) []public.AssignedService {
	var raws []json.RawMessage
	if len(b) == 0 || json.Unmarshal(b, &raws) != nil {
		return []public.AssignedService{}
	}
	out := make([]public.AssignedService, 0, len(raws))
	for _, r := range raws {
		var as public.AssignedService
		if json.Unmarshal(r, &as) == nil && strings.TrimSpace(as.Code) != "" && as.CurrentStatus.Valid() && as.Source.Valid() {
			out = append(out, as)
		}
	}
	return out
}
