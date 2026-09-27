package classifier

import (
	"strings"

	"lct/gocore/internal/gen/public"
)

// Лимиты поиска типов (frontend.v1.yaml: limit default 20, maximum 100).
const (
	SearchDefaultLimit = 20
	SearchMaxLimit     = 100
)

// searchIdx — поиск типов по Инструкции п.4.2 (зеркало searchTypes из фикстур):
// каждое слово запроса — префикс какого-то слова из названия, кода или синонимов,
// порядок слов любой, регистр/«ё»/знаки препинания не важны.
// Ранжирование: запрос целиком совпал с названием/кодом/синонимом («газ» -> «Происшествие 104»:
// среди 1300+ типов импортированного классификатора учебные типы не должны теряться за лимитом) >
// название начинается с запроса > все слова нашлись в названии > совпадение через код/синонимы;
// внутри ранга — порядок классификатора.
func (s *snapshot) searchIdx(q string, limit int) []*typeEntry {
	nq := normalize(q)
	if nq == "" {
		return nil
	}
	if limit <= 0 {
		limit = SearchDefaultLimit
	}
	if limit > SearchMaxLimit {
		limit = SearchMaxLimit
	}
	words := strings.Split(nq, " ")

	var tier [4][]*typeEntry
	for _, e := range s.active {
		inName, all := true, true
		for _, w := range words {
			if anyPrefix(e.nameTok, w) {
				continue
			}
			inName = false
			if !anyPrefix(e.otherTok, w) {
				all = false
				break
			}
		}
		if !all {
			continue
		}
		switch {
		case e.exactMatch(nq):
			tier[0] = append(tier[0], e)
		case strings.HasPrefix(e.nameNorm, nq):
			tier[1] = append(tier[1], e)
		case inName:
			tier[2] = append(tier[2], e)
		default:
			tier[3] = append(tier[3], e)
		}
		// Первого ранга набрано на весь лимит — остальные ранги уже не попадут в ответ.
		if len(tier[0]) >= limit {
			break
		}
	}
	out := make([]*typeEntry, 0, min(limit, len(tier[0])+len(tier[1])+len(tier[2])+len(tier[3])))
	for _, t := range tier {
		for _, e := range t {
			if len(out) == limit {
				return out
			}
			out = append(out, e)
		}
	}
	return out
}

// Search — найденные типы (public.IncidentType).
func (c *Catalog) Search(q string, limit int) []public.IncidentType {
	found := c.s().searchIdx(q, limit)
	out := make([]public.IncidentType, 0, len(found))
	for _, e := range found {
		it := public.IncidentType{
			Id: e.info.ID, Code: e.info.Code, Name: e.info.Name, Depth: e.info.Depth,
			Synonyms: e.synonyms, HasReference: e.reference != "",
		}
		if e.info.ParentID != "" {
			pid := e.info.ParentID
			it.ParentId = &pid
		}
		out = append(out, it)
	}
	return out
}

// searchJSON — ответ поиска из предкодированных типов (горячий путь: набор текста в карточке).
func (c *Catalog) searchJSON(q string, limit int) []byte {
	return joinTypes(c.s().searchIdx(q, limit))
}

// exactMatch — нормализованный запрос равен названию, коду или одному из синонимов.
func (e *typeEntry) exactMatch(nq string) bool {
	for _, x := range e.exact {
		if x == nq {
			return true
		}
	}
	return false
}
