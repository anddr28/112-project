package scoring

import (
	"strings"

	"lct/gocore/internal/core"
)

// Пути полей IncidentCardDraft (etalons.scoring.required_fields, FieldError.field).
// Канон — camelCase-пути формы АРМ ("applicant.name", "address.raw", "incidentTypeIds",
// "attributes.where"). Эталон, сгенерированный LLM, может прийти со snake_case или с путями
// контрактного IncidentCard ("category_code", "services_to_notify") — приводим к канону,
// чтобы обязательное поле не превращалось в вечное «не заполнено».

const attrPrefix = "attributes."

var pathAliases = map[string]string{
	"categoryCode":     "incidentTypeIds",
	"category":         "incidentTypeIds",
	"incidentType":     "incidentTypeIds",
	"incidentTypes":    "incidentTypeIds",
	"incidentTypeId":   "incidentTypeIds",
	"servicesToNotify": "services",
	"address.city":     "address.settlement",
	"applicant.phone":  "phones.aon",
	"phone":            "phones.aon",
	// Остальные пути контрактного IncidentCard — туда же, куда их кладёт convert.CardToDraft
	// (landmark → descriptive; casualties → флажок и число пострадавших).
	"address.landmark":   "address.descriptive",
	"casualties":         "flags.victimsPresent",
	"casualties.injured": "flags.victimsCount",
	"casualties.dead":    "flags.victimsCount",
	"casualties.trapped": "flags.victimsCount",
}

// CanonicalPath — путь поля в каноническом виде (camelCase, алиасы контрактной карточки).
// Коды признаков ("attributes.fire_sign") не трогаются: это коды классификатора.
func CanonicalPath(p string) string {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, attrPrefix) {
		return p
	}
	if a, ok := pathAliases[p]; ok {
		return a
	}
	if strings.IndexByte(p, '_') < 0 {
		return p
	}
	c := snakeToCamel(p)
	if a, ok := pathAliases[c]; ok {
		return a
	}
	return c
}

func snakeToCamel(p string) string {
	b := make([]byte, 0, len(p))
	up := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '_':
			up = len(b) > 0 && b[len(b)-1] != '.'
		case up && 'a' <= c && c <= 'z':
			b = append(b, c-('a'-'A'))
			up = false
		default:
			b = append(b, c)
			up = false
		}
	}
	return string(b)
}

// valClass — как сравнивается поле.
type valClass uint8

const (
	clsUnknown  valClass = iota
	clsFree              // свободный текст: только наличие (смысл — слой semantic)
	clsText              // короткий текст/перечисление: нормализованная строка целиком
	clsName              // ФИО: множество слов (порядок не важен)
	clsPhone             // последние 10 цифр
	clsBool              // флажок
	clsInt               // число
	clsCoord             // координата (4 знака ≈ 11 м)
	clsAddrRaw           // единая строка адреса: сверка до уровня здания
	clsAddrPart          // часть адреса без слов-типов
	clsTypes             // типы происшествия: множество id
	clsServices          // службы: множество кодов, лишние — kind=extra
	clsAttr              // признак опросной карты: тип по значению
)

func classOf(path string) valClass {
	switch path {
	case "description", "actionsTaken", "address.descriptive":
		return clsFree
	case "applicant.name":
		return clsName
	case "applicant.status", "phones.channel", "address.source", "address.code":
		return clsText
	case "applicant.foreignLanguage", "phones.foreign", "flags.victimsPresent", "flags.ambulanceRefusal",
		"flags.blocked", "flags.noContact", "flags.callDropped":
		return clsBool
	case "phones.aon", "phones.provided", "phones.onSite":
		return clsPhone
	case "flags.victimsCount":
		return clsInt
	case "address.lat", "address.lon":
		return clsCoord
	case "address.raw":
		return clsAddrRaw
	case "address.apartment", "address.building", "address.country", "address.district", "address.entrance",
		"address.floor", "address.house", "address.object", "address.okrug", "address.region",
		"address.settlement", "address.street", "address.structure":
		return clsAddrPart
	case "incidentTypeIds":
		return clsTypes
	case "services":
		return clsServices
	}
	if len(path) > len(attrPrefix) && strings.HasPrefix(path, attrPrefix) {
		return clsAttr
	}
	return clsUnknown
}

// Подписи полей — запасной вариант, если каталог не дал своей (совпадают с
// frontend/src/shared/utils/card.ts FIELD_LABELS + адресные поля формы АРМ).
const (
	labelFallback     = "Поле карточки"
	attrLabelFallback = "Признак опросной карты"
)

var defaultLabels = map[string]string{
	"applicant.name":            "ФИО заявителя",
	"applicant.status":          "Статус заявителя",
	"applicant.foreignLanguage": "Вызов на иностранном языке",
	"phones.aon":                "Телефон АОН",
	"phones.provided":           "Предоставленный номер",
	"phones.onSite":             "Телефон на место",
	"phones.foreign":            "Иностранный номер",
	"phones.channel":            "Канал связи",
	"address.raw":               "Адрес",
	"address.country":           "Страна",
	"address.region":            "Регион",
	"address.settlement":        "Населённый пункт",
	"address.object":            "Объект",
	"address.okrug":             "Округ",
	"address.district":          "Район",
	"address.street":            "Улица",
	"address.house":             "Дом",
	"address.building":          "Корпус",
	"address.structure":         "Строение",
	"address.apartment":         "Квартира",
	"address.entrance":          "Подъезд",
	"address.floor":             "Этаж",
	"address.code":              "Код",
	"address.descriptive":       "Описательный адрес",
	"address.lat":               "Широта",
	"address.lon":               "Долгота",
	"address.source":            "Источник адреса",
	"incidentTypeIds":           "Тип происшествия",
	"description":               "Описание со слов заявителя",
	"actionsTaken":              "Действия оператора",
	"flags.victimsPresent":      "Пострадавшие",
	"flags.victimsCount":        "Количество пострадавших",
	"flags.ambulanceRefusal":    "Нет на месте / отказ от скорой",
	"flags.blocked":             "Нет доступа / заблокированные",
	"flags.noContact":           "Нет контакта с заявителем",
	"flags.callDropped":         "Срыв звонка",
	"services":                  "Службы",
}

// FieldLabel — подпись поля для отчёта: каталог (подписи из БД/классификатора), иначе
// встроенная таблица; технический путь на экран не выводится никогда.
func FieldLabel(cat core.Catalog, path string) string {
	if strings.HasPrefix(path, attrPrefix) {
		code := path[len(attrPrefix):]
		if cat != nil {
			if l, ok := cat.AttributeLabel(code); ok && l != "" {
				return l
			}
			if l := cat.FieldLabel(path); l != "" && l != labelFallback {
				return l
			}
		}
		return attrLabelFallback
	}
	if cat != nil {
		if l := cat.FieldLabel(path); l != "" && l != labelFallback {
			return l
		}
	}
	if l, ok := defaultLabels[path]; ok {
		return l
	}
	return labelFallback
}
