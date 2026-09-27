package scenarios

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/store"
)

// etalonData — содержимое версии эталона (все jsonb-колонки etalons).
type etalonData struct {
	Card     components.IncidentCard  // etalons.card (snake) — уходит в ai-service
	Draft    public.IncidentCardDraft // etalons.card_draft (camelCase) — сверка слоя 1
	Scoring  model.Scoring
	Actions  []model.ExpectedAction
	Dialogue *model.ExpectedDialogue // nil — чек-листа нет ('{}')
}

// etalonJSON — закодированные колонки (для записи и для сравнения «изменилось ли»).
type etalonJSON struct {
	card, draft, scoring, actions, dialogue []byte
}

func (e *etalonData) encode() (etalonJSON, error) {
	var (
		out etalonJSON
		err error
	)
	if out.card, err = json.Marshal(&e.Card); err != nil {
		return out, fmt.Errorf("scenarios: encode etalon card: %w", err)
	}
	convert.NormalizeDraft(&e.Draft)
	if out.draft, err = json.Marshal(&e.Draft); err != nil {
		return out, fmt.Errorf("scenarios: encode etalon card_draft: %w", err)
	}
	if out.scoring, err = json.Marshal(&e.Scoring); err != nil {
		return out, fmt.Errorf("scenarios: encode etalon scoring: %w", err)
	}
	if e.Actions == nil {
		e.Actions = []model.ExpectedAction{}
	}
	if out.actions, err = json.Marshal(e.Actions); err != nil {
		return out, fmt.Errorf("scenarios: encode expected_actions: %w", err)
	}
	if e.Dialogue == nil {
		out.dialogue = []byte("{}")
	} else {
		e.Dialogue.Normalize()
		if out.dialogue, err = json.Marshal(e.Dialogue); err != nil {
			return out, fmt.Errorf("scenarios: encode expected_dialogue: %w", err)
		}
	}
	return out, nil
}

func (a etalonJSON) equal(b etalonJSON) bool {
	return bytes.Equal(a.card, b.card) && bytes.Equal(a.draft, b.draft) && bytes.Equal(a.scoring, b.scoring) &&
		bytes.Equal(a.actions, b.actions) && bytes.Equal(a.dialogue, b.dialogue)
}

// fromRow — данные текущей версии (nil — эталона ещё нет: пустая форма АРМ).
func fromRow(e *store.EtalonRow) etalonData {
	if e == nil {
		return etalonData{Draft: convert.EmptyDraft(), Actions: []model.ExpectedAction{}}
	}
	return etalonData{
		Card:     e.Card,
		Draft:    e.CardDraft,
		Scoring:  e.Scoring,
		Actions:  e.ExpectedActions,
		Dialogue: e.ExpectedDialogue,
	}
}

// insertEtalonVersion — новая текущая версия эталона: снять is_current со старой и
// вставить max+1 одним batch (один round-trip). Строка сценария должна быть заблокирована.
func insertEtalonVersion(ctx context.Context, tx pgx.Tx, scenarioID uuid.UUID, enc etalonJSON, by *uuid.UUID) (uuid.UUID, int, error) {
	id := ids.New()
	b := &pgx.Batch{}
	b.Queue(sqlFlipEtalon, scenarioID)
	b.Queue(sqlInsertEtalonVersion, id, scenarioID, enc.card, enc.draft, enc.scoring, enc.actions, enc.dialogue, by)
	br := tx.SendBatch(ctx, b)
	if _, err := br.Exec(); err != nil {
		_ = br.Close()
		return uuid.Nil, 0, fmt.Errorf("scenarios: flip current etalon: %w", err)
	}
	var version int
	if err := br.QueryRow().Scan(&version); err != nil {
		_ = br.Close()
		return uuid.Nil, 0, fmt.Errorf("scenarios: insert etalon version: %w", err)
	}
	if err := br.Close(); err != nil {
		return uuid.Nil, 0, fmt.Errorf("scenarios: etalon batch: %w", err)
	}
	return id, version, nil
}

// ---------------------------------------------------------------- правка эталона

// hasEtalonPatch — в PATCH есть хоть одно эталонное поле.
func hasEtalonPatch(in *public.ScenarioPatch) bool {
	return in.EtalonCard != nil || in.EtalonDraft != nil || in.Scoring != nil || in.RequiredFields != nil ||
		in.ExpectedActions != nil || in.ExpectedDialogue != nil
}

// applyEtalonPatch — новая версия эталона поверх текущей (частичная правка):
//   - etalonDraft — форма АРМ как есть; контрактная карточка — DraftToCard (или etalonCard,
//     если прислан и он), с сохранением того, чего в форме АРМ нет (см. mergeCard);
//   - только etalonCard — форма АРМ строится CardToDraft;
//   - requiredFields (верхнеуровневое) важнее scoring.requiredFields; scoring без обоих
//     списков не стирает обязательные поля;
//   - expectedActions / expectedDialogue заменяются целиком.
func (s *Service) applyEtalonPatch(cur etalonData, in *public.ScenarioPatch) etalonData {
	next := cur
	switch {
	case in.EtalonDraft != nil:
		d := *in.EtalonDraft
		convert.NormalizeDraft(&d)
		next.Draft = d
		switch {
		case in.EtalonCard != nil:
			next.Card = convert.IncidentCardFromPublic(in.EtalonCard)
		case sameDraft(&d, &cur.Draft):
			// Форма АРМ не менялась (редактор присылает её при каждом сохранении, даже при
			// смене названия): карточка остаётся прежней — пересборка из формы у фикстур и
			// сгенерированных эталонов дала бы новую версию эталона на ровном месте.
		default:
			// Службы карточки сохраняются, только если форма АРМ их и не показывала
			// (фикстуры, старые эталоны): иначе пустой список — это преподаватель снял все.
			next.Card = mergeCard(convert.DraftToCard(&d, s.cat), &cur.Card, len(cur.Draft.Services) == 0)
		}
	case in.EtalonCard != nil:
		next.Card = convert.IncidentCardFromPublic(in.EtalonCard)
		next.Draft = convert.CardToDraft(&next.Card, s.cat)
	}
	switch {
	case in.Scoring != nil:
		rf := in.RequiredFields
		if rf == nil && in.Scoring.RequiredFields == nil {
			old := cur.Scoring.RequiredFields
			rf = &old
		}
		next.Scoring = convert.ScoringFromPublic(in.Scoring, rf)
		if in.Scoring.Reaction == nil {
			// Редактор, который не знает про ракурс ДДС (v1.3), присылает scoring без
			// reaction — эталон работы диспетчера при этом не должен теряться.
			next.Scoring.Reaction = cur.Scoring.Reaction
		}
	case in.RequiredFields != nil:
		sc := cur.Scoring
		sc.RequiredFields = cleanStrings(*in.RequiredFields)
		next.Scoring = sc
	}
	if in.ExpectedActions != nil {
		next.Actions = convert.ExpectedActionsFromPublic(*in.ExpectedActions)
	}
	if in.ExpectedDialogue != nil {
		d := convert.ExpectedDialogueFromPublic(in.ExpectedDialogue)
		sanitizeChecklist(&d)
		next.Dialogue = &d
	}
	return next
}

// sameDraft — формы АРМ совпадают по содержимому (сравнение закодированного JSON;
// b нормализуется на копии). Ошибка кодирования — «не совпадают».
func sameDraft(a, b *public.IncidentCardDraft) bool {
	bb := *b
	bb.Services = slices.Clone(b.Services) // NormalizeDraft правит элементы — не трогаем чужой срез
	convert.NormalizeDraft(&bb)
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(&bb)
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}

// mergeCard — карточка из формы АРМ плюс то, чего форма не выражает: коды служб
// эталона (у фикстур службы живут только в services_to_notify — по ним слой 1 сверяет
// службы; keepServices — прежняя форма АРМ служб не содержала, значит пустой список в
// правке их не снимал: редактор их просто не показывал), код категории и разбивка
// пострадавших (injured/trapped/dead), если их сумма не изменилась.
func mergeCard(next components.IncidentCard, old *components.IncidentCard, keepServices bool) components.IncidentCard {
	if old == nil {
		return next
	}
	if keepServices && next.ServicesToNotify == nil && old.ServicesToNotify != nil {
		next.ServicesToNotify = old.ServicesToNotify
	}
	if next.CategoryCode == nil {
		next.CategoryCode = old.CategoryCode
	}
	if next.Casualties != nil && old.Casualties != nil {
		o := old.Casualties
		if sum := deref(o.Injured) + deref(o.Trapped) + deref(o.Dead); sum > 0 && sum == deref(next.Casualties.Injured) {
			next.Casualties = o
		}
	}
	return next
}

// ---------------------------------------------------------------- эталон из генерации

// generatedEtalon — эталон v1 из результата LLM: карточка как пришла (код категории
// сценария подставляется, если LLM его не дала или дала неизвестный), форма АРМ —
// CardToDraft, дополненная тем, что знает только легенда (см. enrichDraft).
func (s *Service) generatedEtalon(res *components.ScenarioResult, cs *model.CallScript, info core.IncidentTypeInfo, withDialogue bool) etalonData {
	card := res.EtalonCard
	if code := strings.TrimSpace(deref(card.CategoryCode)); code == "" || !s.knownTypeCode(code) {
		if info.Code != "" {
			c := info.Code
			card.CategoryCode = &c
		}
	}
	draft := convert.CardToDraft(&card, s.cat)
	enrichDraft(&draft, cs, info)

	e := etalonData{
		Card:    card,
		Draft:   draft,
		Scoring: model.Scoring{RequiredFields: model.DefaultRequiredFields(), RequiredFacts: requiredFacts(cs)},
		Actions: convert.ExpectedActionsFromContract(deref(res.ExpectedActions)),
	}
	if withDialogue && res.ExpectedDialogue != nil {
		d := convert.ExpectedDialogueFromContract(res.ExpectedDialogue)
		sanitizeChecklist(&d)
		if len(d.Checklist) > 0 {
			e.Dialogue = &d
		}
	}
	return e
}

func (s *Service) knownTypeCode(code string) bool {
	if s.cat == nil {
		return false
	}
	_, ok := s.cat.TypeByCode(code)
	return ok
}

// enrichDraft — эталонная форма АРМ так, как её заполнил бы оператор по этому звонку:
//   - тип происшествия — категория сценария, если CardToDraft его не распознал;
//   - телефон АОН — номер заявителя из легенды (при приёме вызова АОН подставляется
//     именно он — Инструкция п.4.1; так же устроены эталоны фикстур);
//   - статус заявителя — по его роли в легенде (IncidentCard статуса не несёт, а поле
//     обязательное по умолчанию: без этого каждый сгенерированный сценарий упирался бы
//     в 422 при подтверждении). Преподаватель проверяет эталон перед approve.
func enrichDraft(d *public.IncidentCardDraft, cs *model.CallScript, info core.IncidentTypeInfo) {
	if len(d.IncidentTypeIds) == 0 && info.ID != "" {
		d.IncidentTypeIds = append(d.IncidentTypeIds, info.ID)
	}
	if strings.TrimSpace(deref(d.Phones.Aon)) == "" {
		if p := strings.TrimSpace(cs.Caller.Phone); p != "" {
			d.Phones.Aon = &p
		}
	}
	if d.Applicant.Name == nil {
		if n := strings.TrimSpace(cs.Caller.Name); n != "" && !strings.EqualFold(n, "неизвестен") {
			d.Applicant.Name = &n
		}
	}
	if d.Applicant.Status == nil {
		st := statusByRole(cs.Caller.Role)
		d.Applicant.Status = &st
	}
}

// statusByRole — статус заявителя (Инструкция п.5.3, закрытый список) по роли из легенды.
// Решает первое слово роли, по которому статус узнаётся: в русской роли главное слово
// идёт первым («жена пострадавшего» — родственник, «пострадавший водитель» —
// пострадавший, «сосед пострадавшего» — очевидец). Слова сверяются целиком или по
// основе, а не подстрокой: «мужчина» — не «муж», «медсестра» — не «сестра», «другой» —
// не «друг». По умолчанию «очевидец»: сосед/житель/прохожий в эталонах фикстур — именно он.
func statusByRole(role string) public.ApplicantStatus {
	words := strings.FieldsFunc(strings.ToLower(role), func(r rune) bool { return !unicode.IsLetter(r) })
	for _, w := range words {
		w = strings.ReplaceAll(w, "ё", "е")
		for i := range roleWords {
			rw := &roleWords[i]
			if slices.Contains(rw.exact, w) || slices.ContainsFunc(rw.stems, func(p string) bool { return strings.HasPrefix(w, p) }) {
				return rw.status
			}
		}
	}
	return public.Очевидец
}

// roleWords — слова ролей по статусам (ё уже заменена на е). Короткие слова, основа
// которых начинает посторонние слова («муж» — «мужчина», «друг» — «другой»), — целиком.
var roleWords = []struct {
	status public.ApplicantStatus
	exact  []string
	stems  []string
}{
	{public.Очевидец, []string{"жилец"}, []string{"очевид", "свидетел", "сосед", "прохож", "житель", "жильц", "пешеход"}},
	{public.Пострадавший, nil, []string{"пострадав", "потерпев", "ранен", "травмирова"}},
	{public.Родственник,
		[]string{"муж", "мужа", "мужу", "жена", "жены", "жене", "жену", "сын", "сына", "сыну", "сынок", "мать", "матери",
			"отец", "отца", "отцу", "брат", "брата", "брату", "дочь", "зять", "тесть", "теща", "тетя", "тети", "дядя", "дяди"},
		[]string{"родствен", "родител", "мам", "пап", "дочер", "дочк", "сестр", "бабушк", "дедушк", "внук", "внуч",
			"супруг", "племянни", "свекр", "невестк", "опекун"}},
	{public.Ребенок, nil, []string{"ребен", "дети", "школьн", "подрост", "мальчик", "девочк", "несовершеннолет"}},
	{public.Участник, nil, []string{"участник", "водител", "виновник"}},
	{public.Знакомый, []string{"друг", "друга", "другу"}, []string{"подруг", "знаком", "коллег", "приятел"}},
}

// requiredFacts — факты для семантической проверки: key_facts легенды, иначе проекция
// брифа без reveal=never (voice-mode §5).
func requiredFacts(cs *model.CallScript) []string {
	if len(cs.KeyFacts) > 0 {
		return append([]string(nil), cs.KeyFacts...)
	}
	if cs.Dialogue == nil {
		return nil
	}
	var out []string
	for _, f := range cs.Dialogue.Facts {
		if f.Reveal != model.RevealNever && strings.TrimSpace(f.Text) != "" {
			out = append(out, f.Text)
		}
	}
	return out
}
