package scenarios

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
	"lct/gocore/internal/model"
	"lct/gocore/internal/store"
)

// Границы ввода. Контракт задаёт только минимум названия; верхние пределы — защита
// jsonb и LLM-промптов от мусора (тело запроса и так ограничено 1 МБ).
const (
	titleMin       = 3
	titleMax       = 300
	commentMax     = 2000
	reasonMin      = 3
	reasonMax      = 2000
	previewTextMax = 500
	briefTurnsMin  = 2
	briefTurnsMax  = 40
	maxListItems   = 200 // обязательных полей, фактов, пунктов чек-листа, реплик
)

func validMode(m string) bool {
	return m == core.ModeCards || m == core.ModeCardActions || m == core.ModeBoth
}

func validStatus(s string) bool {
	switch s {
	case core.ScenarioDraft, core.ScenarioGenerated, core.ScenarioValidated, core.ScenarioRejected, core.ScenarioArchived:
		return true
	}
	return false
}

func validDifficulty(d int) bool { return d >= 1 && d <= 3 }

func runes(s string) int { return utf8.RuneCountInString(s) }

// checkTitle — сообщение об ошибке названия или "".
func checkTitle(t string) string {
	switch n := runes(t); {
	case n < titleMin:
		return "Название — не короче 3 символов"
	case n > titleMax:
		return "Название — не длиннее 300 символов"
	}
	return ""
}

// resolveCategory — тип классификатора по uuid, id фикстуры ("it-101") или коду:
// фронт шлёт uuid, демо-данные и ручные вызовы — что угодно из этого.
func (s *Service) resolveCategory(key string) (core.IncidentTypeInfo, uuid.UUID, bool) {
	key = strings.TrimSpace(key)
	if key == "" || s.cat == nil {
		return core.IncidentTypeInfo{}, uuid.Nil, false
	}
	info, ok := s.cat.TypeByID(key)
	if !ok {
		info, ok = s.cat.TypeByCode(key)
	}
	if !ok {
		return core.IncidentTypeInfo{}, uuid.Nil, false
	}
	id, err := uuid.Parse(info.ID)
	if err != nil {
		return core.IncidentTypeInfo{}, uuid.Nil, false
	}
	return info, id, true
}

// ---------------------------------------------------------------- подтверждение

// approveGaps — чего не хватает сценарию для выдачи обучающимся (пусто — можно).
// Как scenarioGaps мока фронта: эталон, по которому сверяется карточка, и легенда, по
// которой её заполняют. Уточнения: ФИО/статус/описание эталона требуются, только если
// эти поля обязательны в сценарии (иначе их нечем и не с чем сверять), реплика нужна
// именно заявителя (вступление голосового режима), а при брифе разговора — факты
// брифа и чек-лист протокола (DESIGN §5). Названия — по-русски: UI показывает как есть.
func approveGaps(r *store.ScenarioRow) []string {
	miss := make([]string, 0, 4)
	if strings.TrimSpace(r.Title) == "" {
		miss = append(miss, "название")
	}
	e := r.Etalon
	if e == nil {
		miss = append(miss, "эталон карточки")
	} else {
		d := &e.CardDraft
		required := make(map[string]bool, len(e.Scoring.RequiredFields))
		for _, f := range e.Scoring.RequiredFields {
			required[f] = true
		}
		if !hasNonEmpty(d.IncidentTypeIds) {
			miss = append(miss, "тип происшествия в эталоне")
		}
		if strings.TrimSpace(d.Address.Raw) == "" {
			miss = append(miss, "адрес в эталоне")
		}
		if required["applicant.name"] && strings.TrimSpace(deref(d.Applicant.Name)) == "" {
			miss = append(miss, "ФИО заявителя в эталоне")
		}
		if required["applicant.status"] && (d.Applicant.Status == nil || strings.TrimSpace(string(*d.Applicant.Status)) == "") {
			miss = append(miss, "статус заявителя в эталоне")
		}
		if required["description"] && strings.TrimSpace(d.Description) == "" {
			miss = append(miss, "описание со слов заявителя в эталоне")
		}
		if len(e.Scoring.RequiredFields) == 0 {
			miss = append(miss, "обязательные поля")
		}
	}
	if !r.CallScript.HasCallerTurn() {
		miss = append(miss, "реплики заявителя")
	}
	if b := r.CallScript.Dialogue; b != nil {
		facts := 0
		for i := range b.Facts {
			if strings.TrimSpace(b.Facts[i].Text) != "" {
				facts++
			}
		}
		if facts == 0 {
			miss = append(miss, "факты в брифе заявителя")
		}
		if e == nil || e.ExpectedDialogue == nil || len(e.ExpectedDialogue.Checklist) == 0 {
			miss = append(miss, "чек-лист разговора")
		}
	}
	return miss
}

func hasNonEmpty(s []string) bool {
	for _, v := range s {
		if strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- очистка брифа и чек-листа

// sanitizeBrief — факты без текста отбрасываются; пустые и повторяющиеся id получают
// уникальные (редактор фронта нумерует новые факты по длине списка — после удаления
// из середины id повторяются, а ai-service раскрывает факты именно по id).
func sanitizeBrief(b *model.DialogueBrief) {
	if b == nil {
		return
	}
	out := b.Facts[:0]
	seen := make(map[string]bool, len(b.Facts))
	for _, f := range b.Facts {
		if strings.TrimSpace(f.Text) == "" {
			continue
		}
		f.ID = uniqueID(strings.TrimSpace(f.ID), "fact", len(out)+1, seen)
		out = append(out, f)
	}
	if out == nil {
		out = []model.DialogueFact{}
	}
	b.Facts = out
}

// sanitizeChecklist — повторяющиеся id пунктов чек-листа делаются уникальными
// (результат оценки разговора ссылается на пункты по id).
func sanitizeChecklist(d *model.ExpectedDialogue) {
	if d == nil {
		return
	}
	seen := make(map[string]bool, len(d.Checklist))
	for i := range d.Checklist {
		d.Checklist[i].ID = uniqueID(d.Checklist[i].ID, "chk", i+1, seen)
	}
	d.Normalize()
}

func uniqueID(id, prefix string, n int, seen map[string]bool) string {
	if id == "" {
		id = prefix + "-" + strconv.Itoa(n)
	}
	base, k := id, 2
	for seen[id] {
		id = fmt.Sprintf("%s-%d", base, k)
		k++
	}
	seen[id] = true
	return id
}

// cleanStrings — без пустых, обрезанные, без повторов (пути обязательных полей).
func cleanStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func deref[T any](p *T) T {
	if p == nil {
		var z T
		return z
	}
	return *p
}
