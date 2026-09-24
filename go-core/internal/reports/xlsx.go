package reports

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"

	"lct/gocore/internal/scoring"
)

const (
	sheetSummary = "Сводка"
	sheetResults = "Результаты"
	docCreator   = "Тренажёр ДДС-112"
)

// xlsxStyles — идентификаторы стилей книги (создаются один раз на файл).
type xlsxStyles struct {
	header, text, wrap, integer, score, seconds, datetime, center int
	pass, fail                                                    int
	title, subtitle, section, label                               int
}

func newXLSXStyles(f *excelize.File) (xlsxStyles, error) {
	var (
		st  xlsxStyles
		err error
	)
	border := []excelize.Border{
		{Type: "left", Color: "C3CBD5", Style: 1}, {Type: "right", Color: "C3CBD5", Style: 1},
		{Type: "top", Color: "C3CBD5", Style: 1}, {Type: "bottom", Color: "C3CBD5", Style: 1},
	}
	top := &excelize.Alignment{Vertical: "top"}
	topCenter := &excelize.Alignment{Vertical: "top", Horizontal: "center"}
	secFmt, dtFmt := "0.0", "dd.mm.yyyy hh:mm:ss"
	defs := []struct {
		dst *int
		s   *excelize.Style
	}{
		{&st.header, &excelize.Style{
			Font:      &excelize.Font{Bold: true, Color: "1F2933"},
			Fill:      excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"E4EAF2"}},
			Border:    border,
			Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center", WrapText: true},
		}},
		{&st.text, &excelize.Style{Border: border, Alignment: top}},
		{&st.wrap, &excelize.Style{Border: border, Alignment: &excelize.Alignment{Vertical: "top", WrapText: true}}},
		{&st.integer, &excelize.Style{Border: border, Alignment: topCenter, NumFmt: 1}},
		{&st.score, &excelize.Style{Border: border, Alignment: topCenter, NumFmt: 1}},
		{&st.seconds, &excelize.Style{Border: border, Alignment: topCenter, CustomNumFmt: &secFmt}},
		{&st.datetime, &excelize.Style{Border: border, Alignment: topCenter, CustomNumFmt: &dtFmt}},
		{&st.center, &excelize.Style{Border: border, Alignment: topCenter}},
		{&st.pass, &excelize.Style{Border: border, Alignment: topCenter, Font: &excelize.Font{Bold: true, Color: "1E7B34"}}},
		{&st.fail, &excelize.Style{Border: border, Alignment: topCenter, Font: &excelize.Font{Bold: true, Color: "B42318"}}},
		{&st.title, &excelize.Style{Font: &excelize.Font{Bold: true, Size: 14}}},
		{&st.subtitle, &excelize.Style{Font: &excelize.Font{Bold: true, Size: 12}}},
		{&st.section, &excelize.Style{Font: &excelize.Font{Bold: true, Size: 11}, Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"E4EAF2"}}}},
		{&st.label, &excelize.Style{Font: &excelize.Font{Color: "52606D"}, Alignment: top}},
	}
	for _, d := range defs {
		if *d.dst, err = f.NewStyle(d.s); err != nil {
			return st, fmt.Errorf("reports: xlsx style: %w", err)
		}
	}
	return st, nil
}

// buildXLSX — книга из двух листов: «Сводка» (шапка занятия, итоги, средние по слоям, итоги по
// обучающимся) и «Результаты» (таблица попыток: стилизованная шапка, закреплённые шапка и ФИО,
// фильтры, ширины, числовые форматы). Лист результатов пишется StreamWriter'ом — без DOM на
// каждую ячейку. excelize собирает zip в памяти целиком, поэтому результат — готовый буфер:
// ошибки сборки видны до отправки заголовков, а Content-Length известен.
func buildXLSX(rep *report) (*bytes.Buffer, error) {
	f := excelize.NewFile()
	defer func() { _ = f.Close() }() // временные файлы StreamWriter, если он сбрасывал на диск

	if err := f.SetSheetName("Sheet1", sheetSummary); err != nil {
		return nil, fmt.Errorf("reports: xlsx: %w", err)
	}
	if _, err := f.NewSheet(sheetResults); err != nil {
		return nil, fmt.Errorf("reports: xlsx: %w", err)
	}
	st, err := newXLSXStyles(f)
	if err != nil {
		return nil, err
	}
	if err := writeResultsSheet(f, rep, &st); err != nil {
		return nil, fmt.Errorf("reports: xlsx results: %w", err)
	}
	if err := writeSummarySheet(f, rep, &st); err != nil {
		return nil, fmt.Errorf("reports: xlsx summary: %w", err)
	}
	f.SetActiveSheet(0)
	_ = f.SetDocProps(&excelize.DocProperties{
		Title:    "Отчёт по занятию «" + rep.Meta.Title + "»",
		Creator:  docCreator,
		Created:  rep.GeneratedAt.Format(time.RFC3339),
		Modified: rep.GeneratedAt.Format(time.RFC3339),
		Language: "ru-RU",
	})
	buf, err := f.WriteToBuffer()
	if err != nil {
		return nil, fmt.Errorf("reports: xlsx write: %w", err)
	}
	return buf, nil
}

func writeResultsSheet(f *excelize.File, rep *report, st *xlsxStyles) error {
	// Параметры печати — ДО StreamWriter: он забирает sheetPr при создании листа.
	fit, landscape, one, zero := true, "landscape", 1, 0
	if err := f.SetSheetProps(sheetResults, &excelize.SheetPropsOptions{FitToPage: &fit}); err != nil {
		return err
	}
	if err := f.SetPageLayout(sheetResults, &excelize.PageLayoutOptions{
		Orientation: &landscape, FitToWidth: &one, FitToHeight: &zero,
	}); err != nil {
		return err
	}
	if err := f.SetHeaderFooter(sheetResults, &excelize.HeaderFooterOptions{
		OddFooter: "&L" + xlsxHF(rep.Meta.Title) + "&RСтр. &P из &N",
	}); err != nil {
		return err
	}
	// шапка таблицы повторяется на каждой печатной странице
	if err := f.SetDefinedName(&excelize.DefinedName{
		Name: "_xlnm.Print_Titles", RefersTo: "'" + sheetResults + "'!$1:$1", Scope: sheetResults,
	}); err != nil {
		return err
	}

	sw, err := f.NewStreamWriter(sheetResults)
	if err != nil {
		return err
	}
	for i, c := range columns {
		if err := sw.SetColWidth(i+1, i+1, c.Width); err != nil {
			return err
		}
	}
	// закреплены строка шапки и колонки «№», «Обучающийся»
	if err := sw.SetPanes(&excelize.Panes{Freeze: true, XSplit: 2, YSplit: 1, TopLeftCell: "C2", ActivePane: "bottomRight",
		Selection: []excelize.Selection{{SQRef: "C2", ActiveCell: "C2", Pane: "bottomRight"}}}); err != nil {
		return err
	}

	cells := make([]any, len(columns))
	for i, c := range columns {
		cells[i] = excelize.Cell{StyleID: st.header, Value: c.Title}
	}
	if err := sw.SetRow("A1", cells, excelize.RowOpts{Height: 32}); err != nil {
		return err
	}

	colStyle := make([]int, len(columns))
	for i, c := range columns {
		switch c.Kind {
		case kInt:
			colStyle[i] = st.integer
		case kScore:
			colStyle[i] = st.score
		case kSeconds:
			colStyle[i] = st.seconds
		case kTime:
			colStyle[i] = st.datetime
		case kBool:
			colStyle[i] = st.center
		default:
			colStyle[i] = st.text
			if c.Width >= 30 {
				colStyle[i] = st.wrap
			}
		}
	}
	for i := range rep.Rows {
		r := &rep.Rows[i]
		for j, c := range columns {
			style := colStyle[j]
			v := c.value(i, r)
			switch x := v.(type) {
			case time.Time:
				v = wallClock(x, rep.Loc)
			case bool:
				if x {
					v = "да"
				} else {
					v = "нет"
				}
			case string:
				if c.Title == "Вердикт" {
					switch x {
					case "Зачёт":
						style = st.pass
					case "Незачёт":
						style = st.fail
					}
				}
			}
			// пустые ячейки тоже со стилем — сетка таблицы не рвётся
			cells[j] = excelize.Cell{StyleID: style, Value: v}
		}
		if err := sw.SetRow("A"+strconv.Itoa(i+2), cells); err != nil {
			return err
		}
	}
	if len(rep.Rows) > 0 {
		// Таблица Excel — кнопки фильтра в шапке и полосы строк (явные стили ячеек сильнее).
		last, _ := excelize.CoordinatesToCellName(len(columns), len(rep.Rows)+1)
		noStripes := false
		if err := sw.AddTable(&excelize.Table{Range: "A1:" + last, Name: "Results",
			StyleName: "TableStyleLight1", ShowRowStripes: &noStripes}); err != nil {
			return err
		}
	}
	return sw.Flush()
}

func writeSummarySheet(f *excelize.File, rep *report, st *xlsxStyles) error {
	const sh = sheetSummary
	m, s := rep.Meta, &rep.Summary
	for col, w := range map[string]float64{"A": 34, "B": 46, "C": 12, "D": 12, "E": 14, "F": 14} {
		if err := f.SetColWidth(sh, col, col, w); err != nil {
			return err
		}
	}
	row := 1
	set := func(cell string, v any, style int) error {
		if err := f.SetCellValue(sh, cell, v); err != nil {
			return err
		}
		if style != 0 {
			return f.SetCellStyle(sh, cell, cell, style)
		}
		return nil
	}
	pair := func(label string, v any, style int) error {
		r := strconv.Itoa(row)
		row++
		if err := set("A"+r, label, st.label); err != nil {
			return err
		}
		return set("B"+r, v, style)
	}
	section := func(title string) error {
		row++ // пустая строка перед разделом
		r := strconv.Itoa(row)
		row++
		if err := set("A"+r, title, st.section); err != nil {
			return err
		}
		return f.SetCellStyle(sh, "B"+r, "F"+r, st.section)
	}

	if err := set("A1", "Отчёт по занятию", st.title); err != nil {
		return err
	}
	if err := set("A2", m.Title, st.subtitle); err != nil {
		return err
	}
	row = 4
	voice := "выключен"
	if m.Settings.Voice.Enabled {
		voice = "включён"
	}
	meta := []struct {
		l string
		v any
	}{
		{"Вид", kindText(m.Kind)},
		{"Преподаватель", orDash(m.Teacher)},
		{"Статус занятия", lessonStatusText(m.Status)},
		{"Режим", modeText(m.Mode)},
		{"Норматив, с", m.TimeLimitSec},
		{"Порог зачёта, баллов", roundScore(m.Settings.PassThreshold)},
		{"Карточек на обучающегося", m.Settings.CardsPerStudent},
		{"Голосовой режим", voice},
		{"Начато", fmtTime(m.StartedAt, rep.Loc, layoutDateTime)},
		{"Завершено", fmtTime(m.FinishedAt, rep.Loc, layoutDateTime)},
		{"Сценарии", orDash(strings.Join(m.Scenarios, "; "))},
		{"Отчёт сформирован", rep.GeneratedAt.In(rep.Loc).Format(layoutDateTime) + " (время " + zoneLabel(rep.GeneratedAt, rep.Loc) + ")"},
	}
	for _, p := range meta {
		if err := pair(p.l, p.v, 0); err != nil {
			return err
		}
	}
	if rep.Truncated {
		if err := pair("Внимание", "показаны первые "+strconv.Itoa(maxRows)+" строк", st.fail); err != nil {
			return err
		}
	}

	if err := section("Итоги"); err != nil {
		return err
	}
	totals := []struct {
		l string
		v any
	}{
		{"Участников", s.Participants},
		{"Выдано карточек", s.Attempts},
		{"Участников без карточки", s.NotIssued},
		{"В работе", s.InWork},
		{"Сдано", s.Submitted},
		{"Оценено окончательно", s.Evaluated},
		{"Не выполнено (время занятия вышло)", s.Expired},
		{"Средний итог, баллов", scoreOrDash(s.AvgFinal)},
		{"Зачёт", ratioText(s.Passed, s.Evaluated)},
		{"В нормативе времени", ratioText(s.WithinNorm, s.TimingKnown)},
		{"Требуют ревью преподавателя", s.NeedsReview},
		{"Оценка скорректирована преподавателем", s.Overridden},
		{"Среднее время обработки", durationOrDash(s.AvgSpentMs)},
	}
	for _, p := range totals {
		if err := pair(p.l, p.v, 0); err != nil {
			return err
		}
	}

	if err := section("Средние баллы по слоям оценки"); err != nil {
		return err
	}
	for i, t := range layerTitles {
		if err := pair(t, scoreOrDash(s.AvgLayers[i]), 0); err != nil {
			return err
		}
	}

	if err := section("Итоги по обучающимся"); err != nil {
		return err
	}
	for i, h := range []string{"Обучающийся", "Карточек", "Оценено", "Зачётов", "Средний итог", "В нормативе"} {
		cell, _ := excelize.CoordinatesToCellName(i+1, row)
		if err := set(cell, h, st.header); err != nil {
			return err
		}
	}
	row++
	for i := range rep.Students {
		p := &rep.Students[i]
		vals := []any{p.Name, p.Attempts, p.Evaluated, p.Passed, scoreOrDash(p.AvgFinal), ratioText(p.WithinNorm, p.TimingKnown)}
		for j, v := range vals {
			cell, _ := excelize.CoordinatesToCellName(j+1, row)
			style := st.center
			if j == 0 {
				style = st.text
			}
			if err := set(cell, v, style); err != nil {
				return err
			}
		}
		row++
	}
	return nil
}

// xlsxHF — текст для колонтитула Excel: «&» — управляющий символ, длина ограничена.
func xlsxHF(s string) string {
	s = strings.ReplaceAll(s, "&", "&&")
	if r := []rune(s); len(r) > 60 {
		s = string(r[:60]) + "…"
	}
	return s
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// scoreOrDash — целый балл числом (в Excel по нему можно считать) или «—».
func scoreOrDash(p *float64) any {
	if p == nil {
		return "—"
	}
	return int(*p)
}

// ratioText — «7 из 10 (70 %)»; знаменатель 0 — «—».
func ratioText(n, of int) string {
	if of == 0 {
		return "—"
	}
	pct := roundScore(100 * float64(n) / float64(of))
	return strconv.Itoa(n) + " из " + strconv.Itoa(of) + " (" + strconv.FormatFloat(pct, 'f', 0, 64) + " %)"
}

func durationOrDash(ms *float64) string {
	if ms == nil {
		return "—"
	}
	return scoring.FormatDuration(int(*ms + 0.5))
}
