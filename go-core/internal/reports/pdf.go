package reports

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"github.com/go-pdf/fpdf"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"

	"lct/gocore/internal/scoring"
)

// Геометрия A4 альбомной ориентации, мм.
const (
	pdfFont      = "gofont"
	pageW        = 297.0
	pageH        = 210.0
	marginL      = 10.0
	marginR      = 10.0
	marginT      = 10.0
	marginB      = 14.0 // под колонтитул
	contentW     = pageW - marginL - marginR
	tableFont    = 7.5
	headFont     = 6.5
	tableLineH   = 3.4
	tablePadY    = 0.9
	maxErrShown  = 3 // ошибок карточки в ячейке (полный список — в CSV/XLSX)
	maxCellLines = 4
)

type rgb struct{ r, g, b int }

var (
	cText   = rgb{31, 41, 51}
	cMuted  = rgb{82, 96, 109}
	cHead   = rgb{228, 234, 242}
	cZebra  = rgb{247, 249, 251}
	cBorder = rgb{195, 203, 213}
	cPass   = rgb{30, 123, 52}
	cFail   = rgb{180, 35, 24}
)

// pdfCol — колонка PDF-таблицы (сокращённый состав: лист A4 не вмещает все колонки XLSX).
type pdfCol struct {
	title string
	w     float64
	align string
	text  func(i int, r *reportRow) string
}

// Ширины: 238 мм фиксированных колонок + «Ошибки» на остаток ширины (39 мм). Подписи шапки
// короткие: узкие колонки не должны переноситься посреди слова.
func pdfColumns() []pdfCol {
	score := func(get func(r *reportRow) *float64) func(int, *reportRow) string {
		return func(_ int, r *reportRow) string {
			if !r.Attempt {
				return ""
			}
			return dashIfNil(get(r))
		}
	}
	return []pdfCol{
		{"№", 8, "C", func(i int, _ *reportRow) string { return strconv.Itoa(i + 1) }},
		{"Обучающийся", 38, "L", func(_ int, r *reportRow) string { return r.FullName }},
		{"Карт.", 10, "C", func(_ int, r *reportRow) string { return ifAttempt(r, strconv.Itoa(r.SeqNo)) }},
		{"Сценарий", 46, "L", func(_ int, r *reportRow) string { return r.Scenario }},
		{"Статус", 22, "L", func(_ int, r *reportRow) string { return statusText(r) }},
		{"Затрачено, с", 17, "C", func(_ int, r *reportRow) string {
			if r.SpentMs == nil {
				return ifAttempt(r, "—")
			}
			return fmtDecimal(float64(*r.SpentMs)/1000, 1)
		}},
		{"В норме", 13, "C", func(_ int, r *reportRow) string {
			if r.WithinNorm == nil {
				return ifAttempt(r, "—")
			}
			if *r.WithinNorm {
				return "да"
			}
			return "нет"
		}},
		{"Поля", 11, "C", score(func(r *reportRow) *float64 { return r.Fields })},
		{"Сем.", 11, "C", score(func(r *reportRow) *float64 { return r.Semantic })},
		{"Грам.", 11, "C", score(func(r *reportRow) *float64 { return r.Grammar })},
		{"Время", 11, "C", score(func(r *reportRow) *float64 { return r.Timing })},
		{"Разг.", 11, "C", score(func(r *reportRow) *float64 { return r.Dialogue })},
		{"Итог", 12, "C", func(_ int, r *reportRow) string {
			if !r.Attempt {
				return ""
			}
			if !r.Evaluated() {
				return "—"
			}
			s := fmtScore(r.Final)
			if r.Overridden {
				s += "*"
			}
			return s
		}},
		{"Вердикт", 17, "C", func(_ int, r *reportRow) string { return verdictText(r) }},
		{"Ошибки в карточке", contentW - 238, "L", func(_ int, r *reportRow) string {
			if len(r.Errors) <= maxErrShown {
				return strings.Join(r.Errors, "; ")
			}
			return strings.Join(r.Errors[:maxErrShown], "; ") + "; и ещё " + strconv.Itoa(len(r.Errors)-maxErrShown)
		}},
	}
}

// buildPDF — отчёт для печати/архива: шапка занятия, плитки итогов, средние по слоям, таблица
// попыток с переносом страниц и повтором шапки, примечания (корректировки преподавателя).
// Шрифты Go (goregular/gobold, WGL4 — кириллица) встраиваются подмножеством. fpdf держит
// документ в памяти, поэтому ошибки сборки проверяются здесь, до первого байта ответа;
// writePDF только выводит готовый документ.
func buildPDF(rep *report) (*fpdf.Fpdf, error) {
	pdf := fpdf.New("L", "mm", "A4", "")
	// AliasNbPages — до добавления UTF-8 шрифтов: fpdf включает цифры в подмножество шрифта,
	// только если алиас уже задан.
	pdf.AliasNbPages("{nb}")
	pdf.AddUTF8FontFromBytes(pdfFont, "", goregular.TTF)
	pdf.AddUTF8FontFromBytes(pdfFont, "B", gobold.TTF)
	pdf.SetMargins(marginL, marginT, marginR)
	pdf.SetAutoPageBreak(true, marginB)
	pdf.SetTitle(pdfText("Отчёт по занятию «"+rep.Meta.Title+"»"), true)
	pdf.SetCreator(docCreator, true)
	pdf.SetCreationDate(rep.GeneratedAt)
	pdf.SetModificationDate(rep.GeneratedAt)

	footerLeft := pdfText(docCreator + " · Отчёт по занятию «" + truncRunes(rep.Meta.Title, 70) + "»")
	pdf.SetFooterFunc(func() {
		pdf.SetY(-(marginB - 4))
		setColor(pdf, cMuted)
		pdf.SetFont(pdfFont, "", 7)
		pdf.CellFormat(contentW/2, 4, footerLeft, "", 0, "L", false, 0, "")
		pdf.CellFormat(contentW/2, 4, "Стр. "+strconv.Itoa(pdf.PageNo())+" из {nb}", "", 0, "R", false, 0, "")
	})

	pdf.AddPage()
	pdfHeader(pdf, rep)
	pdfSummary(pdf, rep)
	pdfTable(pdf, rep)
	pdfNotes(pdf, rep)

	if err := pdf.Error(); err != nil {
		return nil, fmt.Errorf("reports: pdf: %w", err)
	}
	return pdf, nil
}

func writePDF(w io.Writer, pdf *fpdf.Fpdf) error {
	return pdf.Output(w)
}

func pdfHeader(pdf *fpdf.Fpdf, rep *report) {
	m := rep.Meta
	setColor(pdf, cText)
	pdf.SetFont(pdfFont, "B", 15)
	pdf.CellFormat(contentW, 7, "Отчёт по занятию", "", 1, "L", false, 0, "")
	pdf.SetFont(pdfFont, "B", 12)
	pdf.MultiCell(contentW, 5.5, pdfText(m.Title), "", "L", false)
	pdf.Ln(1)

	voice := "выключен"
	if m.Settings.Voice.Enabled {
		voice = "включён"
	}
	lines := []string{
		strings.Join([]string{
			kindText(m.Kind),
			"Преподаватель: " + orDash(m.Teacher),
			"Статус: " + lessonStatusText(m.Status),
			"Режим: " + modeText(m.Mode),
			"Норматив: " + strconv.Itoa(m.TimeLimitSec) + " с",
			"Порог зачёта: " + fmtScore(ptr(roundScore(m.Settings.PassThreshold))) + " баллов",
			"Голосовой режим: " + voice,
		}, "   ·   "),
		strings.Join([]string{
			"Начато: " + fmtTime(m.StartedAt, rep.Loc, layoutShort),
			"Завершено: " + fmtTime(m.FinishedAt, rep.Loc, layoutShort),
			"Отчёт сформирован: " + rep.GeneratedAt.In(rep.Loc).Format(layoutShort) +
				" (время " + zoneLabel(rep.GeneratedAt, rep.Loc) + ")",
		}, "   ·   "),
		"Сценарии: " + orDash(strings.Join(m.Scenarios, "; ")),
	}
	pdf.SetFont(pdfFont, "", 8.5)
	setColor(pdf, cMuted)
	for _, l := range lines {
		pdf.MultiCell(contentW, 4.2, pdfText(l), "", "L", false)
	}
	pdf.Ln(2)
}

// pdfSummary — плитки итогов и строка средних по слоям.
func pdfSummary(pdf *fpdf.Fpdf, rep *report) {
	s := &rep.Summary
	tiles := []struct{ label, value string }{
		{"Участников", strconv.Itoa(s.Participants)},
		{"Выдано карточек", strconv.Itoa(s.Attempts)},
		{"Сдано", strconv.Itoa(s.Submitted)},
		{"Оценено", strconv.Itoa(s.Evaluated)},
		{"Средний итог", fmtScore(s.AvgFinal)},
		{"Зачёт", ratioShort(s.PassRate, s.Passed, s.Evaluated)},
		{"В нормативе", ratioShort(s.WithinRate, s.WithinNorm, s.TimingKnown)},
		{"Требуют ревью", strconv.Itoa(s.NeedsReview)},
	}
	const (
		gap   = 2.0
		tileH = 13.0
	)
	tw := (contentW - gap*float64(len(tiles)-1)) / float64(len(tiles))
	x0, y0 := pdf.GetX(), pdf.GetY()
	for i, t := range tiles {
		x := x0 + float64(i)*(tw+gap)
		setFill(pdf, cZebra)
		setDraw(pdf, cBorder)
		pdf.Rect(x, y0, tw, tileH, "FD")
		pdf.SetXY(x+2, y0+1.6)
		pdf.SetFont(pdfFont, "", 7)
		setColor(pdf, cMuted)
		pdf.CellFormat(tw-4, 3.5, t.label, "", 0, "L", false, 0, "")
		pdf.SetXY(x+2, y0+5.6)
		pdf.SetFont(pdfFont, "B", 12)
		setColor(pdf, cText)
		pdf.CellFormat(tw-4, 6, t.value, "", 0, "L", false, 0, "")
	}
	pdf.SetXY(x0, y0+tileH+2.5)

	parts := make([]string, len(layerTitles))
	for i, t := range layerTitles {
		parts[i] = t + " " + fmtScore(s.AvgLayers[i])
	}
	line := "Средние баллы по слоям: " + strings.Join(parts, "   ·   ")
	if s.AvgSpentMs != nil {
		line += "      Среднее время обработки: " + scoring.FormatDuration(int(*s.AvgSpentMs+0.5))
	}
	pdf.SetFont(pdfFont, "", 8.5)
	setColor(pdf, cMuted)
	pdf.MultiCell(contentW, 4.2, pdfText(line), "", "L", false)
	pdf.Ln(2.5)
}

// pdfTable — таблица с переносом строк в ячейках, ручным разрывом страниц (строка целиком
// на одной странице) и повтором шапки на каждой новой странице.
func pdfTable(pdf *fpdf.Fpdf, rep *report) {
	cols := pdfColumns()
	limit := pageH - marginB

	header := func() {
		pdf.SetFont(pdfFont, "B", headFont)
		titles := make([][]string, len(cols))
		n := 1
		for i, c := range cols {
			titles[i] = pdf.SplitText(c.title, c.w)
			n = max(n, len(titles[i]))
		}
		h := float64(n)*tableLineH + 2*tablePadY
		// Шапка вместе хотя бы с одной строкой — на одной странице. Иначе (длинный список
		// сценариев сдвинул таблицу к низу листа) прямоугольники шапки остались бы на этой
		// странице, а каждая строка текста CellFormat (drawRow ставит Y сам) — автопереносом
		// fpdf на свою новую страницу: полтора десятка почти пустых листов.
		if pdf.GetY()+h+tableLineH+2*tablePadY > limit {
			pdf.AddPage()
		}
		drawRow(pdf, cols, titles, h, &cHead, nil)
	}
	header()

	if len(rep.Rows) == 0 {
		pdf.SetFont(pdfFont, "", 8.5)
		setColor(pdf, cMuted)
		pdf.Ln(2)
		pdf.CellFormat(contentW, 5, "В занятии нет участников и попыток.", "", 1, "L", false, 0, "")
		return
	}

	cells := make([][]string, len(cols))
	for i := range rep.Rows {
		r := &rep.Rows[i]
		pdf.SetFont(pdfFont, "", tableFont)
		n := 1
		for j, c := range cols {
			cells[j] = splitCell(pdf, pdfText(c.text(i, r)), c.w)
			n = max(n, len(cells[j]))
		}
		h := float64(n)*tableLineH + 2*tablePadY
		if pdf.GetY()+h > limit {
			pdf.AddPage()
			header()
			pdf.SetFont(pdfFont, "", tableFont)
		}
		var fill *rgb
		if i%2 == 1 {
			fill = &cZebra
		}
		var verdictColor *rgb
		switch verdictText(r) {
		case "Зачёт":
			verdictColor = &cPass
		case "Незачёт":
			verdictColor = &cFail
		}
		drawRow(pdf, cols, cells, h, fill, verdictColor)
	}
}

// drawRow — строка таблицы высотой h: рамки/заливка прямоугольниками, текст построчно.
// verdict — цвет колонки «Вердикт» (nil — обычный).
func drawRow(pdf *fpdf.Fpdf, cols []pdfCol, cells [][]string, h float64, fill, verdict *rgb) {
	x, y := marginL, pdf.GetY()
	setDraw(pdf, cBorder)
	style := "D"
	if fill != nil {
		setFill(pdf, *fill)
		style = "FD"
	}
	for j, c := range cols {
		pdf.Rect(x, y, c.w, h, style)
		setColor(pdf, cText)
		if verdict != nil && c.title == "Вердикт" {
			setColor(pdf, *verdict)
		}
		for k, line := range cells[j] {
			pdf.SetXY(x, y+tablePadY+float64(k)*tableLineH)
			pdf.CellFormat(c.w, tableLineH, line, "", 0, c.align, false, 0, "")
		}
		x += c.w
	}
	pdf.SetXY(marginL, y+h)
}

// splitCell — перенос текста по ширине колонки; больше maxCellLines строк — хвост «…».
func splitCell(pdf *fpdf.Fpdf, s string, w float64) []string {
	if s == "" {
		return nil
	}
	lines := pdf.SplitText(s, w)
	if len(lines) > maxCellLines {
		lines = lines[:maxCellLines]
		lines[maxCellLines-1] = strings.TrimRight(lines[maxCellLines-1], " ;,") + "…"
	}
	return lines
}

// pdfNotes — примечания под таблицей: сноска о корректировках и их причины (ТЗ: изменение
// оценок фиксируется), предупреждение об усечении.
func pdfNotes(pdf *fpdf.Fpdf, rep *report) {
	var notes []string
	for i := range rep.Rows {
		r := &rep.Rows[i]
		if r.Attempt && r.Overridden {
			n := r.FullName + ", карточка " + strconv.Itoa(r.SeqNo) + ": итог " + fmtScore(r.Final) +
				" (авто " + fmtScore(r.Total) + ")"
			if r.OverrideRsn != "" {
				n += " — " + r.OverrideRsn
			}
			notes = append(notes, n)
		}
	}
	if len(notes) == 0 && !rep.Truncated {
		return
	}
	pdf.Ln(3)
	setColor(pdf, cMuted)
	if len(notes) > 0 {
		pdf.SetFont(pdfFont, "B", 8.5)
		pdf.CellFormat(contentW, 5, "* Итог скорректирован преподавателем:", "", 1, "L", false, 0, "")
		pdf.SetFont(pdfFont, "", 8)
		for _, n := range notes {
			pdf.MultiCell(contentW, 4, pdfText("— "+n), "", "L", false)
		}
	}
	if rep.Truncated {
		pdf.Ln(1)
		pdf.SetFont(pdfFont, "B", 8.5)
		setColor(pdf, cFail)
		pdf.MultiCell(contentW, 4.5, "Показаны первые "+strconv.Itoa(maxRows)+" строк — полный набор выгрузите в CSV.", "", "L", false)
	}
}

// ---------------------------------------------------------------- helpers

func setColor(pdf *fpdf.Fpdf, c rgb) { pdf.SetTextColor(c.r, c.g, c.b) }
func setFill(pdf *fpdf.Fpdf, c rgb)  { pdf.SetFillColor(c.r, c.g, c.b) }
func setDraw(pdf *fpdf.Fpdf, c rgb)  { pdf.SetDrawColor(c.r, c.g, c.b) }

// pdfText — текст, безопасный для fpdf: таблицы ширин UTF-8 шрифта в fpdf покрывают только
// BMP (руна ≥ U+10000, например эмодзи в причине корректировки, уронила бы fpdf паникой),
// управляющие символы — пробелы.
func pdfText(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case r > 0xFFFF || (r >= 0xD800 && r <= 0xDFFF) || r == unicode.ReplacementChar:
			return '?'
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, s)
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func dashIfNil(p *float64) string {
	if p == nil {
		return "—"
	}
	return fmtScore(p)
}

func ifAttempt(r *reportRow, s string) string {
	if !r.Attempt {
		return ""
	}
	return s
}

// ratioShort — «70 % (7/10)» для плитки; нечего считать — «—».
func ratioShort(pct *float64, n, of int) string {
	if pct == nil || of == 0 {
		return "—"
	}
	return fmtPercent(pct) + " (" + strconv.Itoa(n) + "/" + strconv.Itoa(of) + ")"
}

func ptr[T any](v T) *T { return &v }
