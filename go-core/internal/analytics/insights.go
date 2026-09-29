package analytics

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"lct/gocore/internal/gen/public"
)

// Инсайты — детерминированные правила над агрегатами. Каждое правило срабатывает только при
// достаточной выборке (не «один студент один раз ошибся») и выдаёт: что происходит (с долей
// группы и числами), кого касается и что конкретно сделать преподавателю. Пороги подобраны
// под учебную группу 10–30 человек; id вывода стабилен (фронт может помнить скрытые).

const (
	minAttemptsForInsights = 3  // меньше — выводы преждевременны
	minCount               = 2  // ошибка/факт встретились хотя бы дважды
	fieldSharePct          = 25 // доля карточек с ошибкой поля — предупреждение
	criticalSharePct       = 50 // … — критично
	factSharePct           = 30
	dialogueSharePct       = 30
	grammarMinCount        = 3
	slowSharePct           = 40 // доля карточек сверх норматива в категории
	slowCriticalPct        = 70
	weakLayerScore         = 60
	criticalLayerScore     = 40
	riskStreak             = 3 // незачётов подряд в последних попытках
	maxInsights            = 12
	maxFieldInsights       = 3
	maxFactInsights        = 2
	maxCategoryInsights    = 2
)

const (
	sevInfo     = public.InsightSeverityInfo
	sevWarning  = public.InsightSeverityWarning
	sevCritical = public.InsightSeverityCritical
)

type insightInput struct {
	data                     *overviewData
	threshold                float64
	names                    map[uuid.UUID]string
	catNames                 map[uuid.UUID]string
	label                    func(field, fromData string) string
	semAttempts, dlgAttempts int
}

type insightList []public.Insight

func (l *insightList) add(id string, sev public.InsightSeverity, kind, title, body string, metric float64,
	students []string, evidence []string) {
	in := public.Insight{Id: id, Severity: sev, Kind: kind, Title: title, Body: body}
	m := float32(math.Round(metric*10) / 10)
	in.Metric = &m
	if len(students) > 0 {
		s := students
		in.AffectedStudents = &s
	}
	if len(evidence) > 0 {
		e := evidence
		in.Evidence = &e
	}
	*l = append(*l, in)
}

// buildInsights — все правила; критичные сверху, затем предупреждения, затем информация.
func buildInsights(in insightInput) []public.Insight {
	d := in.data
	s := &d.summary
	out := insightList{}
	if s.attempts == 0 {
		out.add("general:no-data", sevInfo, "general", "Нет оценённых карточек за выбранный период",
			"Выводы появятся, когда обучающиеся сдадут карточки и оценка завершится. Расширьте период или снимите фильтры.",
			0, nil, nil)
		return out
	}
	if s.attempts < minAttemptsForInsights {
		out.add("general:few-data", sevInfo, "general", "Пока мало данных для выводов",
			fmt.Sprintf("Оценено %d %s — выводы о типичных ошибках появятся с %d карточек. Смотрите разбор отдельных попыток.",
				s.attempts, plural(s.attempts, "карточка", "карточки", "карточек"), minAttemptsForInsights),
			float64(s.attempts), nil, nil)
		return out
	}

	studentsAtRisk(&out, in)
	weakFields(&out, in)
	missingFacts(&out, in)
	slowCategories(&out, in)
	weakCategories(&out, in)
	weakLayer(&out, in)
	dialogueGaps(&out, in)
	grammarPatterns(&out, in)
	general(&out, in)

	rank := map[public.InsightSeverity]int{sevCritical: 0, sevWarning: 1, sevInfo: 2}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Severity] < rank[out[j].Severity] })
	if len(out) > maxInsights {
		out = out[:maxInsights]
	}
	return out
}

// student_at_risk — незачёт во всех трёх последних попытках.
func studentsAtRisk(out *insightList, in insightInput) {
	var names []string
	for _, st := range in.data.students {
		if len(st.lastVerdicts) < riskStreak {
			continue
		}
		fail := true
		for _, v := range st.lastVerdicts[:riskStreak] {
			fail = fail && v == "fail"
		}
		if fail {
			names = append(names, in.names[st.id])
		}
	}
	if len(names) == 0 {
		return
	}
	sort.Strings(names)
	n := len(names)
	out.add("student_at_risk:last3", sevCritical, "student_at_risk",
		fmt.Sprintf("%d %s ниже порога зачёта в %d последних попытках", n,
			plural(n, "обучающийся", "обучающихся", "обучающихся"), riskStreak),
		"Незачёт подряд — признак системного пробела, а не случайной ошибки. Рекомендация: индивидуальный разбор "+
			"последних карточек (раздел «Разбор попытки»: ошибки полей, пропущенные факты), комментарии к полям с "+
			"ошибками и отдельное практическое занятие со сценариями низкой сложности до устойчивого зачёта.",
		float64(n), names, nil)
}

// weak_field — поле, в котором ошибается заметная доля группы.
func weakFields(out *insightList, in insightInput) {
	s := &in.data.summary
	added := 0
	for _, fe := range in.data.fieldErrs {
		if added == maxFieldInsights {
			break
		}
		share := 100 * float64(fe.attempts) / float64(s.attempts)
		if fe.count < minCount || share < fieldSharePct {
			continue
		}
		sev := sevWarning
		if share >= criticalSharePct {
			sev = sevCritical
		}
		groupShare := 100 * float64(fe.students) / float64(max(s.students, 1))
		lbl := in.label(fe.field, fe.label)
		title := fmt.Sprintf("%s группы %s «%s»", pctText(groupShare), kindVerb(fe.kind), lbl)
		body := fmt.Sprintf("Ошибка встречается в %d из %d %s (%s) у %d из %d %s. %s",
			fe.attempts, s.attempts, ofCards(s.attempts), pctText(share), fe.students, s.students, ofStudents(s.students),
			fieldAdvice(fe.field, fe.kind))
		ev := []string{fmt.Sprintf("%s — %d %s", kindText(fe.kind), fe.count, plural(fe.count, "раз", "раза", "раз"))}
		if cats := topCategoriesFor(in, fe.field); cats != "" {
			ev = append(ev, "Чаще всего в категориях: "+cats)
		}
		out.add("weak_field:"+fe.field+":"+fe.kind, sev, "weak_field", title, body, groupShare, namesOf(in, fe.users), ev)
		added++
	}
}

// missing_fact — факт, который часто не попадает в карточку.
func missingFacts(out *insightList, in insightInput) {
	if in.semAttempts < minAttemptsForInsights {
		return
	}
	added := 0
	for _, f := range in.data.facts {
		if added == maxFactInsights {
			break
		}
		share := 100 * float64(f.count) / float64(in.semAttempts)
		if f.count < minCount || share < factSharePct {
			continue
		}
		sev := sevWarning
		if share >= criticalSharePct {
			sev = sevCritical
		}
		out.add("missing_fact:"+shortKey(f.text), sev, "missing_fact",
			fmt.Sprintf("Факт «%s» не отражён в %s карточек", truncRunes(f.text, 80), pctText(share)),
			fmt.Sprintf("Пропущен в %d из %d проверенных по смыслу %s у %d %s. Рекомендация: при опросе заявителя "+
				"уточняйте эту деталь и фиксируйте её в описании; разберите на занятии эталонное описание сценария и "+
				"потренируйте пересказ обращения без пропусков и домыслов.",
				f.count, in.semAttempts, ofCards(in.semAttempts), f.students, ofStudents(f.students)),
			share, namesOf(in, f.users), nil)
		added++
	}
}

// slow_timing — выход за норматив в категории.
func slowCategories(out *insightList, in insightInput) {
	type slow struct {
		c     categoryRow
		share float64
	}
	var list []slow
	for _, c := range in.data.categories {
		if c.timed < minCount || c.overLimit < minCount || c.avgOverMs == nil {
			continue
		}
		share := 100 * float64(c.overLimit) / float64(c.timed)
		if share >= slowSharePct {
			list = append(list, slow{c, share})
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return *list[i].c.avgOverMs > *list[j].c.avgOverMs })
	for i, x := range list {
		if i == maxCategoryInsights {
			break
		}
		sev := sevWarning
		if x.share >= slowCriticalPct {
			sev = sevCritical
		}
		sec := *x.c.avgOverMs / 1000
		name := in.catNames[x.c.id]
		out.add("slow_timing:"+x.c.id.String(), sev, "slow_timing",
			fmt.Sprintf("Средний выход за норматив %s с в категории «%s»", numText(sec), name),
			fmt.Sprintf("За норматив вышли %d из %d %s категории (%s). Рекомендация: отработайте параллельный ввод — "+
				"адрес и тип происшествия заполняются во время разговора, а не после; начните с коротких сценариев этой "+
				"категории и проверьте, реалистичен ли норматив для её опросной карты.",
				x.c.overLimit, x.c.timed, ofCards(x.c.timed), pctText(x.share)),
			sec, nil, nil)
	}
	if len(list) == 0 && in.data.summary.withinPct < 50 {
		s := &in.data.summary
		out.add("slow_timing:all", sevWarning, "slow_timing",
			fmt.Sprintf("В норматив укладываются только %s карточек", pctText(s.withinPct)),
			"Время обработки — системная проблема группы, а не отдельной категории. Рекомендация: тренировки на скорость "+
				"со сценариями низкой сложности; разберите порядок действий — сначала адрес и характер происшествия, "+
				"детали — по ходу разговора.",
			s.withinPct, nil, nil)
	}
}

// weak_category — средний балл категории ниже порога зачёта.
func weakCategories(out *insightList, in insightInput) {
	var list []categoryRow
	for _, c := range in.data.categories {
		if c.attempts >= minCount && c.avgScore < in.threshold {
			list = append(list, c)
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].avgScore < list[j].avgScore })
	for i, c := range list {
		if i == maxCategoryInsights {
			break
		}
		sev := sevWarning
		if c.avgScore < in.threshold-20 {
			sev = sevCritical
		}
		name := in.catNames[c.id]
		out.add("weak_category:"+c.id.String(), sev, "weak_category",
			fmt.Sprintf("Слабая категория «%s»: средний балл %s", name, numText(c.avgScore)),
			fmt.Sprintf("Ниже порога зачёта (%s) по %d %s, зачёт в %s. Рекомендация: разберите опросную карту категории и "+
				"эталонные карточки, затем дайте 2–3 сценария этой категории, начиная с низкой сложности.",
				numText(in.threshold), c.attempts, plural(c.attempts, "карточке", "карточкам", "карточкам"), pctText(c.passPct)),
			c.avgScore, nil, nil)
	}
}

// weak_layer — самый слабый слой оценки у группы.
func weakLayer(out *insightList, in insightInput) {
	s := &in.data.summary
	best, idx := math.Inf(1), -1
	for i, l := range s.layers {
		if l.n >= minAttemptsForInsights && l.avg != nil && *l.avg < best {
			best, idx = *l.avg, i
		}
	}
	if idx < 0 || best >= weakLayerScore {
		return
	}
	sev := sevWarning
	if best < criticalLayerScore {
		sev = sevCritical
	}
	key := layerKeys[idx]
	out.add("weak_layer:"+key, sev, "weak_layer",
		fmt.Sprintf("Слабее всего у группы слой «%s» — средний балл %s", layerTitle(key), numText(best)),
		fmt.Sprintf("Оценено %d %s. %s", s.layers[idx].n, plural(s.layers[idx].n, "карточка", "карточки", "карточек"), layerAdvice(key)),
		best, nil, nil)
}

// dialogue_pattern — что чаще всего не уточняют в разговоре.
func dialogueGaps(out *insightList, in insightInput) {
	if in.dlgAttempts < minAttemptsForInsights || len(in.data.dialogue) == 0 {
		return
	}
	q := in.data.dialogue[0]
	share := 100 * float64(q.count) / float64(in.dlgAttempts)
	if q.count < minCount || share < dialogueSharePct {
		return
	}
	out.add("dialogue_pattern:"+shortKey(q.text), sevWarning, "dialogue_pattern",
		fmt.Sprintf("В разговоре не уточняют: «%s»", truncRunes(q.text, 80)),
		fmt.Sprintf("Пропущено в %d из %d %s (%s). Рекомендация: разберите протокол разговора оператора 112 "+
			"(материал «Порядок обработки вызова»): адрес → что случилось → пострадавшие → заявитель → инструкции; "+
			"потренируйте вопрос в парах.", q.count, in.dlgAttempts, plural(in.dlgAttempts, "разговора", "разговоров", "разговоров"), pctText(share)),
		share, namesOf(in, q.users), nil)
}

// grammar_pattern — повторяющееся правило грамотности.
func grammarPatterns(out *insightList, in insightInput) {
	if len(in.data.grammar) == 0 {
		return
	}
	g := in.data.grammar[0]
	if g.count < grammarMinCount || g.students < minCount {
		return
	}
	sev := sevInfo
	if s := in.data.summary.students; s > 0 && 100*g.students >= criticalSharePct*s {
		sev = sevWarning
	}
	body := fmt.Sprintf("Встречается %d %s у %d %s. Рекомендация: пятиминутный разбор правила на занятии на примерах из "+
		"карточек группы; напомните проверять текст описания перед сдачей.", g.count, plural(g.count, "раз", "раза", "раз"),
		g.students, ofStudents(g.students))
	if g.suggestion != nil && strings.TrimSpace(*g.suggestion) != "" {
		body += " Пример исправления: «" + strings.TrimSpace(*g.suggestion) + "»."
	}
	out.add("grammar_pattern:"+shortKey(g.rule), sev, "grammar_pattern",
		"Повторяющаяся ошибка грамотности: "+truncRunes(g.message, 90), body, float64(g.count), nil, []string{"Правило: " + g.rule})
}

// general — ревью и «группа готова к усложнению».
func general(out *insightList, in insightInput) {
	s := &in.data.summary
	if s.needsReview > 0 {
		out.add("general:needs-review", sevInfo, "general",
			fmt.Sprintf("%d %s ждут проверки преподавателем", s.needsReview, plural(s.needsReview, "оценка", "оценки", "оценок")),
			"Низкая уверенность ИИ или недоступный ИИ-слой. Рекомендация: откройте разбор этих попыток, проверьте "+
				"смысл и разговор и при необходимости скорректируйте итог с указанием причины.",
			float64(s.needsReview), nil, nil)
	}
	if s.attempts >= 5 && s.passPct >= 85 && s.avgScore >= in.threshold+10 {
		out.add("general:ready-for-harder", sevInfo, "general", "Группа уверенно проходит карточки",
			fmt.Sprintf("Зачёт в %s карточек, средний балл %s. Рекомендация: повысьте сложность сценариев, сократите "+
				"норматив или включите голосовой режим с разговором.", pctText(s.passPct), numText(s.avgScore)),
			s.passPct, nil, nil)
	}
}

// ---------------------------------------------------------------- тексты

func kindVerb(kind string) string {
	switch kind {
	case "missing":
		return "не заполняют поле"
	case "wrong":
		return "заполняют с ошибкой поле"
	case "extra":
		return "указывают лишнее в поле"
	}
	return "ошибаются в поле"
}

func kindText(kind string) string {
	switch kind {
	case "missing":
		return "не заполнено"
	case "wrong":
		return "не совпадает с эталоном"
	case "extra":
		return "лишнее значение"
	}
	return kind
}

// fieldAdvice — что сделать преподавателю по полю (по префиксу пути IncidentCardDraft).
func fieldAdvice(field, kind string) string {
	var a string
	switch {
	case field == "reaction.decision":
		a = "Разберите по памятке ДДС критерии «Принята» / «Не принята»: зона ответственности, профиль происшествия, дубли."
	case field == "reaction.decisionTime":
		a = "Потренируйте решение в пределах 30 секунд: сначала первичный статус, детали — после."
	case field == "reaction.refusal":
		a = "Разберите примеры неправомерного отказа от профильных происшествий (материал «Разбор типичных ошибок ДДС»)."
	case field == "reaction.statuses":
		a = "Напомните последовательность статусов хода работ и обязательные комментарии к ним."
	case strings.HasPrefix(field, "address"):
		a = "Разберите порядок уточнения адреса: населённый пункт → улица → дом → подъезд и этаж, ориентиры; " +
			"потренируйте ввод через подсказку адреса во время разговора."
	case field == "applicant.name":
		a = "Напомните, что ФИО заявителя спрашивают после адреса и характера происшествия, а отказ назвать — тоже отмечают."
	case field == "applicant.status":
		a = "Повторите закрытый список статусов заявителя (очевидец, пострадавший, родственник, ребёнок, участник, знакомый)."
	case strings.HasPrefix(field, "phones"):
		a = "Покажите, где в карточке АОН и контактный телефон; проверка номера — последний шаг перед сохранением."
	case field == "incidentTypeIds":
		a = "Отработайте поиск типа происшествия по ключевым словам классификатора: тип определяет список служб."
	case field == "description":
		a = "Разберите структуру описания: что случилось, где, кто пострадал, особые обстоятельства — коротко и только со слов заявителя."
	case field == "actionsTaken":
		a = "Разберите, что пишется в действиях: кому передано, какие инструкции даны заявителю."
	case field == "services":
		a = "Разберите, как формируется список оповещения по типу происшествия и когда службу добавляют вручную."
	case strings.HasPrefix(field, "flags.victims"):
		a = "Отработайте вопрос о пострадавших и их числе — от него зависит оповещение скорой помощи."
	case strings.HasPrefix(field, "attributes."):
		a = "Пройдите с группой опросную карту категории: какие вопросы обязательны и почему."
	default:
		a = "Разберите поле на примере эталонной карточки и добавьте в занятие сценарии, где оно ключевое."
	}
	r := []rune(a)
	r[0] = unicode.ToLower(r[0])
	a = "Рекомендация: " + string(r)
	if kind == "wrong" {
		a += " Сравните ответы с эталоном: проблема не в пропуске, а в значении."
	}
	return a
}

func layerTitle(key string) string {
	switch key {
	case "fields":
		return "Поля карточки"
	case "semantic":
		return "Смысл"
	case "grammar":
		return "Грамотность"
	case "timing":
		return "Время"
	case "dialogue":
		return "Разговор"
	}
	return key
}

func layerAdvice(key string) string {
	switch key {
	case "fields":
		return "Рекомендация: сделайте акцент на полноте карточки — чек-лист обязательных полей перед сохранением, разбор типовых пропусков."
	case "semantic":
		return "Рекомендация: разберите, какие факты обязательно попадают в описание, и потренируйте пересказ обращения своими словами без домыслов."
	case "grammar":
		return "Рекомендация: короткие диктанты по типовым формулировкам карточек; напомните перечитывать описание перед сдачей."
	case "timing":
		return "Рекомендация: тренировки на скорость — сценарии низкой сложности с сокращённым нормативом, ввод данных во время разговора."
	case "dialogue":
		return "Рекомендация: разберите протокол разговора — представление, адрес, пострадавшие, инструкции безопасности, «помощь направлена»."
	}
	return ""
}

// topCategoriesFor — до двух категорий, где ошибок в поле больше всего (из тепловой карты).
func topCategoriesFor(in insightInput, field string) string {
	type cc struct {
		name string
		n    int
	}
	var list []cc
	for _, h := range in.data.heat {
		if h.field == field {
			list = append(list, cc{in.catNames[h.category], h.count})
		}
	}
	if len(list) < 2 {
		return ""
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].n > list[j].n })
	parts := []string{}
	for i := 0; i < len(list) && i < 2; i++ {
		if list[i].name != "" {
			parts = append(parts, fmt.Sprintf("«%s» (%d)", list[i].name, list[i].n))
		}
	}
	return strings.Join(parts, ", ")
}

func namesOf(in insightInput, ids []uuid.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if n := in.names[id]; n != "" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// ofCards / ofStudents — родительный падеж после «из N» / «у N»: из 1 карточки, из 5 карточек.
func ofCards(n int) string {
	return plural(n, "карточки", "карточек", "карточек")
}
func ofStudents(n int) string {
	return plural(n, "обучающегося", "обучающихся", "обучающихся")
}

// pctText — «42%» (целые проценты: в заголовке точнее не нужно).
func pctText(v float64) string { return strconv.Itoa(int(math.Round(v))) + "%" }

// numText — число с одним знаком после запятой по-русски («54,5», «18»).
func numText(v float64) string {
	v = math.Round(v*10) / 10
	return strings.Replace(strconv.FormatFloat(v, 'f', -1, 64), ".", ",", 1)
}

// plural — 1 карточка, 2 карточки, 5 карточек.
func plural(n int, one, few, many string) string {
	n %= 100
	if n >= 11 && n <= 14 {
		return many
	}
	switch n % 10 {
	case 1:
		return one
	case 2, 3, 4:
		return few
	}
	return many
}

func truncRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}

// shortKey — стабильная часть id вывода из текста (первые 40 символов без пробелов по краям).
func shortKey(s string) string { return truncRunes(strings.ToLower(s), 40) }
