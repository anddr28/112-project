package reports

import (
	"bytes"
	"encoding/csv"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"

	"lct/gocore/internal/core"
	"lct/gocore/internal/model"
)

var reportAt = time.Date(2026, 9, 24, 12, 30, 0, 0, time.UTC)

func sampleMeta() *lessonMeta {
	started := time.Date(2026, 9, 24, 7, 0, 0, 0, time.UTC)
	finished := started.Add(45 * time.Minute)
	s := model.DefaultLessonSettings(nil)
	s.PassThreshold = 60
	s.Voice.Enabled = true
	return &lessonMeta{ID: uuid.New(), Kind: core.LessonKindClass, Title: "Пожары & ДТП: итоговое занятие",
		Mode: core.ModeCards, TimeLimitSec: 30, Settings: s, Status: core.LessonFinished,
		StartedAt: &started, FinishedAt: &finished, CreatedAt: started.Add(-time.Hour),
		Teacher: "Учителев И. П.", Scenarios: []string{"Пожар в квартире", "ДТП (версия 2)"}, Participants: 3}
}

func sampleRows() []reportRow {
	u1, u2, u3 := uuid.New(), uuid.New(), uuid.New()
	acc := time.Date(2026, 9, 24, 7, 1, 0, 0, time.UTC)
	first := acc.Add(2500 * time.Millisecond)
	sub := acc.Add(25 * time.Second)
	return []reportRow{
		{UserID: u1, FullName: "Андреев Андрей Андреевич", Short: "Андреев А. А.", Attempt: true, AttemptID: uuid.New(),
			SeqNo: 1, IncidentNo: 36814852, Status: core.AttemptEvaluated, TimeLimitSec: 30,
			CallAcceptedAt: &acc, FirstInputAt: &first, SubmittedAt: &sub, SpentMs: ip(25_060), ReplayCount: 1,
			Scenario: "Пожар в квартире", Category: "Пожар", EvalStatus: core.EvalDone,
			Fields: fp(80), Grammar: fp(90), Semantic: fp(70), Timing: fp(100), Total: fp(82), Final: fp(82),
			Verdict: core.VerdictPass, WithinNorm: bp(true),
			Errors: []string{"Адрес (не заполнено)", "=Телефон (неверно)"}},
		{UserID: u2, FullName: "Борисов Борис", Short: "Борисов Б.", Attempt: true, AttemptID: uuid.New(),
			SeqNo: 1, IncidentNo: 36814853, Status: core.AttemptEvaluated, TimeLimitSec: 30,
			Scenario: "ДТП (версия 2)", EvalStatus: core.EvalDone, Total: fp(40), Final: fp(75),
			Verdict: core.VerdictPass, Overridden: true, OverrideRsn: "Учтён шум на линии 🚒\x01", NeedsReview: true,
			WithinNorm: bp(false)},
		{UserID: u3, FullName: "Васильев Василий", Short: "Васильев В."},
	}
}

func sampleReport(rows []reportRow) *report {
	return newReport(sampleMeta(), rows, false, reportAt, msk)
}

func TestNewReport(t *testing.T) {
	t.Parallel()
	rep := newReport(sampleMeta(), sampleRows(), true, reportAt.In(msk), nil)
	if rep.Loc != time.UTC || rep.GeneratedAt.Location() != time.UTC || !rep.Truncated || rep.Summary.Participants != 3 {
		t.Fatalf("report = %+v", rep)
	}
}

// ---------------------------------------------------------------- CSV

func TestWriteCSV(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := writeCSV(&buf, sampleReport(sampleRows())); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	if !bytes.HasPrefix(raw, []byte(utf8BOM)) {
		t.Fatal("нет UTF-8 BOM")
	}
	if !bytes.Contains(raw, []byte("\r\n")) {
		t.Fatal("строки не CRLF")
	}
	r := csv.NewReader(bytes.NewReader(raw[len(utf8BOM):]))
	r.Comma = ';'
	recs, err := r.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 4 {
		t.Fatalf("строк %d, want шапка + 3", len(recs))
	}
	col := map[string]int{}
	for i, h := range recs[0] {
		if h != columns[i].Title {
			t.Errorf("заголовок %d = %q, want %q", i, h, columns[i].Title)
		}
		col[h] = i
	}
	a := recs[1]
	checks := map[string]string{
		"№": "1", "Обучающийся": "Андреев Андрей Андреевич", "Карточка": "1", "№ происшествия": "36814852",
		"Статус": "Оценена", "Вызов принят": "24.09.2026 10:01:00", "Сдано": "24.09.2026 10:01:25",
		"Затрачено, с": "25,1", "Норматив, с": "30", "В нормативе": "да", "Реакция, с": "2,5", "Переспросов": "1",
		"Поля карточки": "80", "Итог (авто)": "82", "Итог": "82", "Вердикт": "Зачёт", "Требует ревью": "нет",
		"Ошибки в карточке": "Адрес (не заполнено); =Телефон (неверно)", "Корректировка преподавателя": "",
		"Разговор": "", "Тип происшествия": "Пожар",
	}
	for name, want := range checks {
		if got := a[col[name]]; got != want {
			t.Errorf("Андреев %q = %q, want %q", name, got, want)
		}
	}
	b := recs[2]
	if b[col["Корректировка преподавателя"]] != "Учтён шум на линии 🚒\x01" || b[col["Требует ревью"]] != "да" ||
		b[col["Итог"]] != "75" || b[col["Итог (авто)"]] != "40" || b[col["Вызов принят"]] != "" {
		t.Errorf("Борисов = %q", b)
	}
	v := recs[3]
	for i, cell := range v {
		switch columns[i].Title {
		case "№":
			if cell != "3" {
				t.Errorf("№ = %q", cell)
			}
		case "Обучающийся", "Статус":
		default:
			if cell != "" {
				t.Errorf("участник без карточки: %q = %q", columns[i].Title, cell)
			}
		}
	}
}

func TestWriteCSVFormulaInjection(t *testing.T) {
	t.Parallel()
	rows := sampleRows()[:1]
	rows[0].FullName = "=cmd|' /C calc'!A0"
	rows[0].Scenario = "+SUM(1;2)"
	var buf bytes.Buffer
	if err := writeCSV(&buf, sampleReport(rows)); err != nil {
		t.Fatal(err)
	}
	s := buf.String()
	if !strings.Contains(s, `'=cmd|' /C calc'!A0`) || !strings.Contains(s, `"'+SUM(1;2)"`) {
		t.Fatalf("формулы не обезврежены: %s", s)
	}
}

func TestWriteCSVEmpty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := writeCSV(&buf, sampleReport(nil)); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(strings.TrimPrefix(buf.String(), utf8BOM), "\r\n"), "\r\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "№;Обучающийся;") {
		t.Fatalf("пустой CSV = %q", buf.String())
	}
}

type failWriter struct{ after int }

func (w *failWriter) Write(p []byte) (int, error) {
	if w.after <= 0 {
		return 0, errors.New("клиент закрыл соединение")
	}
	w.after -= len(p)
	return len(p), nil
}

func TestWriteCSVWriterError(t *testing.T) {
	t.Parallel()
	rows := make([]reportRow, 0, 2000)
	for i := 0; i < 2000; i++ {
		rows = append(rows, sampleRows()[0])
	}
	if err := writeCSV(&failWriter{after: 0}, sampleReport(rows)); err == nil {
		t.Fatal("ошибка записи потеряна")
	}
	if err := writeCSV(&failWriter{after: 100 << 10}, sampleReport(rows)); err == nil {
		t.Fatal("ошибка записи посреди файла потеряна")
	}
}

// ---------------------------------------------------------------- XLSX

func openXLSX(t *testing.T, rep *report) *excelize.File {
	t.Helper()
	buf, err := buildXLSX(rep)
	if err != nil {
		t.Fatal(err)
	}
	f, err := excelize.OpenReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("книга не открывается: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestBuildXLSX(t *testing.T) {
	t.Parallel()
	f := openXLSX(t, sampleReport(sampleRows()))
	if got := f.GetSheetList(); len(got) != 2 || got[0] != sheetSummary || got[1] != sheetResults {
		t.Fatalf("листы = %v", got)
	}
	if f.GetActiveSheetIndex() != 0 {
		t.Errorf("активный лист = %d, want «Сводка»", f.GetActiveSheetIndex())
	}
	rows, err := f.GetRows(sheetResults)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("строк %d", len(rows))
	}
	for i, c := range columns {
		if rows[0][i] != c.Title {
			t.Errorf("шапка %d = %q", i, rows[0][i])
		}
	}
	cell := func(title string, row int) string {
		t.Helper()
		for i, c := range columns {
			if c.Title == title {
				name, _ := excelize.CoordinatesToCellName(i+1, row)
				v, err := f.GetCellValue(sheetResults, name)
				if err != nil {
					t.Fatal(err)
				}
				return v
			}
		}
		t.Fatalf("нет колонки %q", title)
		return ""
	}
	if got := cell("Вызов принят", 2); got != "24.09.2026 10:01:00" {
		t.Errorf("дата-время в зоне отчёта = %q", got)
	}
	if got := cell("Затрачено, с", 2); got != "25.1" && got != "25,1" {
		t.Errorf("секунды = %q", got)
	}
	if got := cell("Итог", 3); got != "75" {
		t.Errorf("итог Борисова = %q", got)
	}
	if got := cell("Ошибки в карточке", 2); got != "Адрес (не заполнено); =Телефон (неверно)" {
		t.Errorf("ошибки = %q (текст, не формула)", got)
	}
	if formula, _ := f.GetCellFormula(sheetResults, "X2"); formula != "" {
		t.Errorf("текст стал формулой: %q", formula)
	}
	if got := cell("Корректировка преподавателя", 3); !strings.HasPrefix(got, "Учтён шум на линии 🚒") {
		t.Errorf("причина = %q", got)
	}
	if got := cell("Статус", 4); got != "Карточка не выдана" {
		t.Errorf("статус = %q", got)
	}
	tables, err := f.GetTables(sheetResults)
	if err != nil || len(tables) != 1 || tables[0].Range != "A1:Y4" {
		t.Errorf("таблица = %+v %v", tables, err)
	}
	// числа — числами (по ним можно считать в Excel)
	if typ, _ := f.GetCellType(sheetResults, "O2"); typ != excelize.CellTypeNumber && typ != excelize.CellTypeUnset {
		t.Errorf("балл не число: %v", typ)
	}

	summary, err := f.GetRows(sheetSummary)
	if err != nil {
		t.Fatal(err)
	}
	text := flatten(summary)
	for _, want := range []string{"Отчёт по занятию", "Пожары & ДТП: итоговое занятие", "Учителев И. П.", "Завершено",
		"включён", "Пожар в квартире; ДТП (версия 2)", "24.09.2026 15:30:00 (время MSK, UTC+03:00)",
		"2 из 2 (100 %)", "1 из 2 (50 %)", "Андреев Андрей Андреевич", "Итоги по обучающимся"} {
		if !strings.Contains(text, want) {
			t.Errorf("в сводке нет %q", want)
		}
	}
	if strings.Contains(text, "Внимание") {
		t.Error("предупреждение об усечении без усечения")
	}
	props, err := f.GetDocProps()
	if err != nil || !strings.Contains(props.Title, "Пожары & ДТП") || props.Creator != docCreator {
		t.Errorf("docProps = %+v %v", props, err)
	}
}

func flatten(rows [][]string) string {
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(strings.Join(r, "|"))
		b.WriteByte('\n')
	}
	return b.String()
}

func TestBuildXLSXEmptyAndTruncated(t *testing.T) {
	t.Parallel()
	rep := sampleReport(nil)
	rep.Truncated = true
	f := openXLSX(t, rep)
	rows, err := f.GetRows(sheetResults)
	if err != nil || len(rows) != 1 {
		t.Fatalf("пустой лист результатов: %v %v", rows, err)
	}
	if tables, _ := f.GetTables(sheetResults); len(tables) != 0 {
		t.Errorf("таблица без строк: %+v", tables)
	}
	summary, _ := f.GetRows(sheetSummary)
	text := flatten(summary)
	if !strings.Contains(text, "показаны первые "+strconv.Itoa(maxRows)+" строк") {
		t.Errorf("нет предупреждения об усечении:\n%s", text)
	}
	if !strings.Contains(text, "Средний итог, баллов|—") {
		t.Errorf("пустые средние — прочерк:\n%s", text)
	}
}

// Управляющие символы из пользовательского ввода не ломают XML книги.
func TestBuildXLSXControlChars(t *testing.T) {
	t.Parallel()
	rows := sampleRows()
	rows[0].FullName = "Имя\x00с\x0bуправляющими\x1f"
	rows[0].Errors = []string{"\x02Адрес"}
	meta := sampleMeta()
	meta.Title = "Занятие\x07 & <тест>"
	f := openXLSX(t, newReport(meta, rows, false, reportAt, msk))
	if _, err := f.GetRows(sheetResults); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------- PDF

func renderPDF(t *testing.T, rep *report) ([]byte, int) {
	t.Helper()
	doc, err := buildPDF(rep)
	if err != nil {
		t.Fatal(err)
	}
	pages := doc.PageNo()
	var buf bytes.Buffer
	if err := writePDF(&buf, doc); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("%PDF-")) || !bytes.Contains(buf.Bytes(), []byte("%%EOF")) {
		t.Fatal("не PDF")
	}
	return buf.Bytes(), pages
}

func TestBuildPDF(t *testing.T) {
	t.Parallel()
	b, pages := renderPDF(t, sampleReport(sampleRows()))
	if pages != 1 {
		t.Errorf("страниц %d, want 1", pages)
	}
	if !bytes.Contains(b, []byte("/Title")) {
		t.Error("нет заголовка документа")
	}
}

func TestBuildPDFManyRowsAndOddText(t *testing.T) {
	t.Parallel()
	base := sampleRows()
	rows := make([]reportRow, 0, 300)
	for i := 0; i < 300; i++ {
		r := base[i%3]
		r.UserID = uuid.New()
		r.FullName = strings.Repeat("Константинопольский ", 1+i%4) + "🚒"
		r.Errors = []string{"Адрес (не заполнено)", "Телефон (неверно)", "Кол-во пострадавших (неверно)", "Этаж (лишнее)", "Подъезд (лишнее)"}
		rows = append(rows, r)
	}
	meta := sampleMeta()
	meta.Title = strings.Repeat("Очень длинное название занятия ", 20) + "\U0001F525"
	rep := newReport(meta, rows, true, reportAt, msk)
	_, pages := renderPDF(t, rep)
	if pages < 5 {
		t.Errorf("300 строк на %d страницах", pages)
	}
}

func TestBuildPDFEmpty(t *testing.T) {
	t.Parallel()
	if _, pages := renderPDF(t, sampleReport(nil)); pages != 1 {
		t.Errorf("страниц %d", pages)
	}
}

func testPDF() *fpdf.Fpdf {
	pdf := fpdf.New("L", "mm", "A4", "")
	pdf.AddUTF8FontFromBytes(pdfFont, "", goregular.TTF)
	pdf.AddUTF8FontFromBytes(pdfFont, "B", gobold.TTF)
	pdf.SetMargins(marginL, marginT, marginR)
	pdf.SetAutoPageBreak(true, marginB)
	pdf.AddPage()
	return pdf
}

// Регрессия: таблица, начавшаяся у нижнего края листа (длинный список сценариев в шапке),
// переносится целиком — шапка не разрывается между страницами.
func TestPDFTableHeaderNotSplitAcrossPages(t *testing.T) {
	t.Parallel()
	limit := pageH - marginB

	pdf := testPDF()
	pdf.SetY(limit - 2)
	pdfTable(pdf, sampleReport(nil))
	if err := pdf.Error(); err != nil {
		t.Fatal(err)
	}
	if pdf.PageNo() != 2 {
		t.Fatalf("страниц %d, want 2: шапка разорвана автопереносом", pdf.PageNo())
	}

	// места хватает на шапку, но не на строку под ней — переносится вместе со строкой
	pdf = testPDF()
	pdf.SetY(limit - 9)
	pdfTable(pdf, sampleReport(sampleRows()[:1]))
	if pdf.PageNo() != 2 {
		t.Fatalf("страниц %d, want 2", pdf.PageNo())
	}
	if y := pdf.GetY(); y > marginT+40 {
		t.Fatalf("после шапки и строки y = %.1f: шапка осталась на первой странице", y)
	}

	// места достаточно — таблица остаётся на странице
	pdf = testPDF()
	pdfTable(pdf, sampleReport(sampleRows()))
	if pdf.PageNo() != 1 {
		t.Fatalf("страниц %d, want 1", pdf.PageNo())
	}
}

func TestSplitCell(t *testing.T) {
	t.Parallel()
	pdf := testPDF()
	pdf.SetFont(pdfFont, "", tableFont)
	if got := splitCell(pdf, "", 20); got != nil {
		t.Errorf("пусто = %v", got)
	}
	lines := splitCell(pdf, strings.Repeat("длинный текст; ", 40), 20)
	if len(lines) != maxCellLines || !strings.HasSuffix(lines[maxCellLines-1], "…") {
		t.Errorf("lines = %q", lines)
	}
}

func TestPDFColumnsFitPage(t *testing.T) {
	t.Parallel()
	var w float64
	for _, c := range pdfColumns(false) {
		if c.w <= 0 {
			t.Errorf("%s: ширина %v", c.title, c.w)
		}
		w += c.w
	}
	if d := w - contentW; d > 0.01 || d < -0.01 {
		t.Fatalf("сумма ширин %.2f, want %.2f", w, contentW)
	}
	// колонка «Итог» помечает корректировку звёздочкой, до окончания оценки — прочерк
	var total pdfCol
	for _, c := range pdfColumns(false) {
		if c.title == "Итог" {
			total = c
		}
	}
	rows := sampleRows()
	if got := total.text(0, &rows[1]); got != "75*" {
		t.Errorf("итог с корректировкой = %q", got)
	}
	pending := rows[0]
	pending.EvalStatus = core.EvalPartial
	if got := total.text(0, &pending); got != "—" {
		t.Errorf("итог до окончания оценки = %q", got)
	}
	if got := total.text(0, &rows[2]); got != "" {
		t.Errorf("итог без карточки = %q", got)
	}
}
