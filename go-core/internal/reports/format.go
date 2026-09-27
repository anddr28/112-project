package reports

import (
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"lct/gocore/internal/core"
)

// report — всё, что нужно рендерам (CSV/XLSX/PDF — чистые функции от него).
type report struct {
	Meta        *lessonMeta
	Rows        []reportRow
	Truncated   bool
	Summary     summary
	Students    []studentSummary
	GeneratedAt time.Time      // UTC
	Loc         *time.Location // часовой пояс пользователя (параметр tz), без него — UTC
}

func newReport(meta *lessonMeta, rows []reportRow, truncated bool, now time.Time, loc *time.Location) *report {
	if loc == nil {
		loc = time.UTC
	}
	s, st := summarize(rows)
	return &report{Meta: meta, Rows: rows, Truncated: truncated, Summary: s, Students: st,
		GeneratedAt: now.UTC(), Loc: loc}
}

// ddsView — отчёт ракурса «Диспетчер ДДС» (lessons.settings.perspective = dds).
func (rep *report) ddsView() bool { return rep.Meta != nil && rep.Meta.Settings.IsDDS() }

// ddsServices — службы обучающихся в занятии ДДС (attempts.service_id) без повторов, по порядку строк.
func (rep *report) ddsServices() []string {
	var out []string
	seen := map[string]bool{}
	for i := range rep.Rows {
		if p := rep.Rows[i].DDS; p != nil && p.ServiceName != "" && !seen[p.ServiceName] {
			seen[p.ServiceName] = true
			out = append(out, p.ServiceName)
		}
	}
	return out
}

// ---------------------------------------------------------------- подписи (как в UI)

var attemptStatusLabel = map[string]string{
	core.AttemptIssued:     "Поступил вызов",
	core.AttemptInProgress: "В работе",
	core.AttemptSubmitted:  "Сдана",
	core.AttemptEvaluating: "Проверяется",
	core.AttemptEvaluated:  "Оценена",
	core.AttemptExpired:    "Не выполнена",
	core.AttemptAborted:    "Прервана",
}

var lessonStatusLabel = map[string]string{
	core.LessonDraft:     "Черновик",
	core.LessonScheduled: "Запланировано",
	core.LessonRunning:   "Идёт",
	core.LessonFinished:  "Завершено",
	core.LessonCancelled: "Отменено",
}

// layerTitles — подписи слоёв как в UI преподавателя (порядок = layerKeys).
var layerTitles = [len(layerKeys)]string{"Поля карточки", "Семантика", "Грамматика", "Время", "Разговор"}

func statusText(r *reportRow) string {
	if !r.Attempt {
		return "Карточка не выдана"
	}
	if s, ok := attemptStatusLabel[r.Status]; ok {
		return s
	}
	return r.Status
}

func lessonStatusText(s string) string {
	if v, ok := lessonStatusLabel[s]; ok {
		return v
	}
	return s
}

func modeText(m string) string {
	if m == core.ModeCardActions {
		return "Действия с карточками"
	}
	return "Карточки"
}

func kindText(k string) string {
	if k == core.LessonKindPractice {
		return "Самостоятельная практика"
	}
	return "Занятие"
}

// verdictText — вердикт только для окончательного итога; пока слои доезжают — «ожидается».
func verdictText(r *reportRow) string {
	if !r.Attempt {
		return ""
	}
	if !r.Evaluated() {
		if r.EvalStatus != "" {
			return "ожидается"
		}
		return ""
	}
	switch r.Verdict {
	case core.VerdictPass:
		return "Зачёт"
	case core.VerdictFail:
		return "Незачёт"
	}
	return "ожидается"
}

func errorText(label, kind string) string {
	switch kind {
	case "missing":
		return label + " (не заполнено)"
	case "wrong":
		return label + " (неверно)"
	case "extra":
		return label + " (лишнее)"
	}
	return label
}

func withVersion(title string, v int) string {
	if v > 1 {
		return title + " (версия " + strconv.Itoa(v) + ")"
	}
	return title
}

func fullName(last, first, middle string) string {
	parts := make([]string, 0, 3)
	for _, p := range [...]string{last, first, middle} {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " ")
}

// ---------------------------------------------------------------- колонки CSV/XLSX

type colKind uint8

const (
	kText colKind = iota
	kInt
	kScore
	kSeconds
	kTime
	kBool
)

// column — одна колонка таблицы результатов. value возвращает nil (пусто), string, int,
// float64 (баллы — целые, секунды — с десятыми), time.Time или bool — рендер форматирует по kind.
type column struct {
	Title string
	Kind  colKind
	Width float64 // ширина в XLSX, символов
	value func(i int, r *reportRow) any
}

func scoreVal(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func secondsVal(ms *int) any {
	if ms == nil {
		return nil
	}
	return float64(*ms) / 1000
}

func timeVal(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

func attemptOnly(f func(r *reportRow) any) func(int, *reportRow) any {
	return func(_ int, r *reportRow) any {
		if !r.Attempt {
			return nil
		}
		return f(r)
	}
}

// columns — состав таблицы результатов (одинаковый в CSV и XLSX).
var columns = []column{
	{"№", kInt, 5, func(i int, _ *reportRow) any { return i + 1 }},
	{"Обучающийся", kText, 30, func(_ int, r *reportRow) any { return r.FullName }},
	{"Карточка", kInt, 9, attemptOnly(func(r *reportRow) any { return r.SeqNo })},
	{"№ происшествия", kInt, 14, attemptOnly(func(r *reportRow) any { return r.IncidentNo })},
	{"Сценарий", kText, 34, attemptOnly(func(r *reportRow) any { return r.Scenario })},
	{"Тип происшествия", kText, 24, attemptOnly(func(r *reportRow) any { return nilIfEmpty(r.Category) })},
	{"Статус", kText, 16, func(_ int, r *reportRow) any { return statusText(r) }},
	{"Вызов принят", kTime, 19, attemptOnly(func(r *reportRow) any { return timeVal(r.CallAcceptedAt) })},
	{"Сдано", kTime, 19, attemptOnly(func(r *reportRow) any { return timeVal(r.SubmittedAt) })},
	{"Затрачено, с", kSeconds, 12, attemptOnly(func(r *reportRow) any { return secondsVal(r.SpentMs) })},
	{"Норматив, с", kInt, 11, attemptOnly(func(r *reportRow) any { return r.TimeLimitSec })},
	{"В нормативе", kBool, 11, attemptOnly(func(r *reportRow) any {
		if r.WithinNorm == nil {
			return nil
		}
		return *r.WithinNorm
	})},
	{"Реакция, с", kSeconds, 10, attemptOnly(func(r *reportRow) any { return secondsVal(r.ReactionMs()) })},
	{"Переспросов", kInt, 11, attemptOnly(func(r *reportRow) any { return r.ReplayCount })},
	{"Поля карточки", kScore, 10, attemptOnly(func(r *reportRow) any { return scoreVal(r.Fields) })},
	{"Семантика", kScore, 10, attemptOnly(func(r *reportRow) any { return scoreVal(r.Semantic) })},
	{"Грамматика", kScore, 10, attemptOnly(func(r *reportRow) any { return scoreVal(r.Grammar) })},
	{"Время", kScore, 8, attemptOnly(func(r *reportRow) any { return scoreVal(r.Timing) })},
	{"Разговор", kScore, 9, attemptOnly(func(r *reportRow) any { return scoreVal(r.Dialogue) })},
	{"Итог (авто)", kScore, 10, attemptOnly(func(r *reportRow) any { return scoreVal(r.Total) })},
	{"Итог", kScore, 8, attemptOnly(func(r *reportRow) any {
		if !r.Evaluated() {
			return nil
		}
		return scoreVal(r.Final)
	})},
	{"Вердикт", kText, 11, func(_ int, r *reportRow) any { return nilIfEmpty(verdictText(r)) }},
	{"Требует ревью", kBool, 12, attemptOnly(func(r *reportRow) any {
		if r.EvalStatus == "" {
			return nil
		}
		return r.NeedsReview
	})},
	{"Ошибки в карточке", kText, 60, attemptOnly(func(r *reportRow) any { return nilIfEmpty(strings.Join(r.Errors, "; ")) })},
	{"Корректировка преподавателя", kText, 40, attemptOnly(func(r *reportRow) any {
		if !r.Overridden {
			return nil
		}
		return nilIfEmpty(r.OverrideRsn)
	})},
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---------------------------------------------------------------- числа и даты по-русски

// fmtDecimal — число с десятичной запятой (Excel/LibreOffice в русской локали).
func fmtDecimal(x float64, prec int) string {
	return strings.Replace(strconv.FormatFloat(x, 'f', prec, 64), ".", ",", 1)
}

func fmtScore(p *float64) string {
	if p == nil {
		return "—"
	}
	return strconv.FormatFloat(*p, 'f', 0, 64)
}

func fmtPercent(p *float64) string {
	if p == nil {
		return "—"
	}
	return strconv.FormatFloat(*p, 'f', 0, 64) + " %"
}

const (
	layoutDateTime = "02.01.2006 15:04:05"
	layoutShort    = "02.01.2006 15:04"
)

func fmtTime(t *time.Time, loc *time.Location, layout string) string {
	if t == nil {
		return "—"
	}
	return t.In(loc).Format(layout)
}

// zoneLabel — «America/Sao_Paulo, UTC−03:00» для подписи «время …»: имя пояса IANA и смещение
// на момент t (летнее время — по правилам tzdata для этой даты). Пояс не передан — «UTC».
func zoneLabel(t time.Time, loc *time.Location) string {
	if name := loc.String(); name != "" && name != "UTC" {
		return name + ", " + utcOffset(t, loc)
	}
	return "UTC"
}

// utcOffset — «UTC+05:30» / «UTC−03:00» / «UTC+00:00» на момент t в поясе loc.
func utcOffset(t time.Time, loc *time.Location) string {
	_, off := t.In(loc).Zone()
	sign := '+'
	if off < 0 {
		sign, off = '−', -off
	}
	return "UTC" + string(sign) + twoDigits(off/3600) + ":" + twoDigits(off%3600/60)
}

func twoDigits(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// wallClock — момент как «настенное» время зоны отчёта, записанное в UTC. Excel хранит
// дату-время без зоны; excelize переводит time.Time по абсолютному значению (UTC), поэтому
// в ячейку уходит уже сдвинутое время — в файле то же, что в CSV/PDF.
func wallClock(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	// до секунды: формат ячейки секундный, а «хвост» микросекунд мешал бы сравнению/фильтрам в Excel
	return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), l.Minute(), l.Second(), 0, time.UTC)
}

// ---------------------------------------------------------------- имя файла

// fileNames — ASCII-имя (параметр filename) и человеческое имя (параметр filename* в UTF-8,
// RFC 5987/6266).
func fileNames(meta *lessonMeta, format string, at time.Time, loc *time.Location) (ascii, utf string) {
	date := at.In(loc).Format("2006-01-02")
	ascii = "lesson-report-" + date + "." + format
	title := safeFileTitle(meta.Title)
	if title == "" {
		return ascii, ascii
	}
	return ascii, "Отчёт — " + title + " — " + date + "." + format
}

// safeFileTitle — название занятия без символов, недопустимых в именах файлов ОС, и не длиннее
// 80 символов (длинные имена режут браузеры и файловые системы).
func safeFileTitle(s string) string {
	var b strings.Builder
	n := 0
	space := false
	for _, r := range s {
		if n >= 80 {
			break
		}
		switch {
		case r == utf8.RuneError, unicode.IsControl(r), strings.ContainsRune(`\/:*?"<>|`, r):
			r = ' '
		}
		if unicode.IsSpace(r) {
			if space || b.Len() == 0 {
				continue
			}
			space = true
			b.WriteByte(' ')
			n++
			continue
		}
		space = false
		b.WriteRune(r)
		n++
	}
	return strings.TrimRight(b.String(), " .")
}

// contentDisposition — attachment с обоими именами: filename (ASCII, для старых клиентов)
// и filename* (UTF-8, percent-encoding по RFC 5987 — пробел как %20, не «+»).
func contentDisposition(ascii, utf string) string {
	return `attachment; filename="` + ascii + `"; filename*=UTF-8''` + rfc5987(utf)
}

func rfc5987(s string) string {
	const (
		attrChar = "!#$&+-.^_`|~"
		hex      = "0123456789ABCDEF"
	)
	var b strings.Builder
	b.Grow(len(s) * 3)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte(attrChar, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		// каждый байт UTF-8 — отдельный «%XX»
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0F])
	}
	return b.String()
}
