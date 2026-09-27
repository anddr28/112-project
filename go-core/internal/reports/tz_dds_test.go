package reports

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
)

// ---------------------------------------------------------------- часовой пояс отчёта

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

func TestReportLocation(t *testing.T) {
	t.Parallel()
	for _, tz := range []string{"America/Sao_Paulo", "Asia/Kolkata", "Pacific/Chatham", "  Africa/Nairobi  ", "UTC"} {
		loc, err := reportLocation(tz)
		if err != nil || loc.String() != strings.TrimSpace(tz) {
			t.Errorf("%q: loc=%v err=%v", tz, loc, err)
		}
	}
	if loc, err := reportLocation(""); err != nil || loc != time.UTC {
		t.Errorf("без tz: loc=%v err=%v, want UTC", loc, err)
	}
	for _, bad := range []string{"Mars/Olympus_Mons", "Local", "../../etc/passwd", "/etc/localtime", "utc+3",
		strings.Repeat("A", maxTZLen+1)} {
		if _, err := reportLocation(bad); err == nil {
			t.Errorf("%q: ожидалась ошибка валидации", bad)
		}
	}
}

// Один и тот же момент UTC — в разных поясах IANA; смещение и летнее время — по tzdata для даты.
func TestZoneLabelIANAAndDST(t *testing.T) {
	t.Parallel()
	jan := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	jul := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		tz       string
		jan, jul string
	}{
		{"America/New_York", "America/New_York, UTC−05:00", "America/New_York, UTC−04:00"},
		{"Australia/Adelaide", "Australia/Adelaide, UTC+10:30", "Australia/Adelaide, UTC+09:30"},
		{"Europe/London", "Europe/London, UTC+00:00", "Europe/London, UTC+01:00"},
		{"Asia/Kolkata", "Asia/Kolkata, UTC+05:30", "Asia/Kolkata, UTC+05:30"},
		{"America/Sao_Paulo", "America/Sao_Paulo, UTC−03:00", "America/Sao_Paulo, UTC−03:00"},
	}
	for _, tc := range cases {
		loc := mustLoc(t, tc.tz)
		if got := zoneLabel(jan, loc); got != tc.jan {
			t.Errorf("%s январь: %q, want %q", tc.tz, got, tc.jan)
		}
		if got := zoneLabel(jul, loc); got != tc.jul {
			t.Errorf("%s июль: %q, want %q", tc.tz, got, tc.jul)
		}
	}

	// Переход на летнее время в Нью-Йорке 08.03.2026 в 02:00: за час до и через час после.
	ny := mustLoc(t, "America/New_York")
	before, after := time.Date(2026, 3, 8, 6, 30, 0, 0, time.UTC), time.Date(2026, 3, 8, 7, 30, 0, 0, time.UTC)
	if got := fmtTime(&before, ny, layoutDateTime); got != "08.03.2026 01:30:00" {
		t.Errorf("до перехода: %s", got)
	}
	if got := fmtTime(&after, ny, layoutDateTime); got != "08.03.2026 03:30:00" {
		t.Errorf("после перехода: %s", got)
	}
	if zoneLabel(before, ny) == zoneLabel(after, ny) {
		t.Error("смещение не сменилось на переходе DST")
	}

	// Тот же момент в разных поясах: разные часы и даже даты.
	at := time.Date(2026, 9, 24, 22, 30, 0, 0, time.UTC)
	for tz, want := range map[string]string{
		"UTC": "24.09.2026 22:30:00", "Asia/Tokyo": "25.09.2026 07:30:00",
		"America/Los_Angeles": "24.09.2026 15:30:00", "Asia/Kathmandu": "25.09.2026 04:15:00",
	} {
		if got := fmtTime(&at, mustLoc(t, tz), layoutDateTime); got != want {
			t.Errorf("%s: %s, want %s", tz, got, want)
		}
	}
}

// Пояс процесса go-core (time.Local) на отчёт не влияет: ни при переданном tz, ни без него.
// Тест не параллельный — подменяет глобальный time.Local и возвращает его.
func TestReportIgnoresProcessTimeZone(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("ProcessZone", 11*3600+45*60)
	defer func() { time.Local = saved }()

	at := time.Date(2026, 9, 24, 22, 30, 0, 0, time.UTC)
	rows := []reportRow{{UserID: uuid.New(), FullName: "Иванова Ирина", Attempt: true, SeqNo: 1,
		Status: core.AttemptSubmitted, CallAcceptedAt: &at, SubmittedAt: &at}}

	sp := mustLoc(t, "America/Sao_Paulo")
	rep := newReport(sampleMeta(), rows, false, at, sp)
	var buf bytes.Buffer
	if err := writeCSV(&buf, rep); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "24.09.2026 19:30:00") || strings.Contains(buf.String(), "25.09.2026 10:15") {
		t.Errorf("CSV не в поясе пользователя:\n%s", buf.String())
	}
	if head := strings.Join(pdfHeaderLines(rep), "\n"); !strings.Contains(head, "Отчёт сформирован: 24.09.2026 19:30 (время America/Sao_Paulo, UTC−03:00)") {
		t.Errorf("PDF: %s", head)
	}
	f := openXLSX(t, rep)
	summary, _ := f.GetRows(sheetSummary)
	if text := flatten(summary); !strings.Contains(text, "24.09.2026 19:30:00 (время America/Sao_Paulo, UTC−03:00)") {
		t.Errorf("XLSX: %s", text)
	}
	cell, _ := f.GetCellValue(sheetResults, "H2")
	if !strings.Contains(cell, "24.09.2026 19:30") && !strings.Contains(cell, "09-24-26 19:30") {
		t.Errorf("XLSX «Вызов принят» = %q", cell)
	}

	// без tz — UTC, а не пояс процесса
	utc := newReport(sampleMeta(), rows, false, at, nil)
	if utc.Loc != time.UTC || !strings.Contains(strings.Join(pdfHeaderLines(utc), "\n"), "24.09.2026 22:30 (время UTC)") {
		t.Errorf("без tz: loc=%v %v", utc.Loc, pdfHeaderLines(utc))
	}
	if loc, err := reportLocation(""); err != nil || loc != time.UTC {
		t.Errorf("reportLocation(\"\") = %v %v", loc, err)
	}
}

// HTTP: tz применяется к CSV/XLSX/имени файла; неверный — 400; без tz — UTC.
func TestReportTimeZoneParam(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	base := "/lessons/" + e.lesson.String() + "/report?format="

	for _, bad := range []string{"Mars%2FOlympus_Mons", "Local", "..%2F..%2Fetc%2Fpasswd", strings.Repeat("A", maxTZLen+1)} {
		d := expectAPIError(t, e.get(t, base+"csv&tz="+bad, "teacher", e.teacher), http.StatusBadRequest, "validation")
		if fields, _ := d["fields"].(map[string]any); fields["tz"] == nil {
			t.Errorf("%q: нет details.fields.tz: %v", bad, d)
		}
	}

	// Андреев принял вызов 24.09.2026 07:01 UTC.
	for _, tc := range []struct{ tz, row, date string }{
		{"", "24.09.2026 07:01:00", "2026-09-24"},
		{"Asia%2FTokyo", "24.09.2026 16:01:00", "2026-09-25"},
		{"America%2FLos_Angeles", "24.09.2026 00:01:00", "2026-09-24"},
	} {
		q := base + "csv"
		if tc.tz != "" {
			q += "&tz=" + tc.tz
		}
		r := e.get(t, q, "teacher", e.teacher)
		if r.status != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.tz, r.status, r.body)
		}
		if !bytes.Contains(r.body, []byte("Андреев Андрей Андреевич;1;100;Пожар в квартире;Пожар;Оценена;"+tc.row)) {
			t.Errorf("tz=%q: время строки не %s:\n%s", tc.tz, tc.row, r.body)
		}
		if cd := r.header.Get("Content-Disposition"); !strings.Contains(cd, "lesson-report-"+tc.date+".csv") {
			t.Errorf("tz=%q: имя файла %q", tc.tz, cd)
		}
	}

	r := e.get(t, base+"xlsx&tz=Asia%2FTokyo", "teacher", e.teacher)
	f, err := excelize.OpenReader(bytes.NewReader(r.body))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	summary, _ := f.GetRows(sheetSummary)
	if text := flatten(summary); !strings.Contains(text, "25.09.2026 07:30:00 (время Asia/Tokyo, UTC+09:00)") {
		t.Errorf("XLSX «Отчёт сформирован»: %s", text)
	}
	if r := e.get(t, base+"pdf&tz=Asia%2FTokyo", "teacher", e.teacher); r.status != http.StatusOK || !bytes.HasPrefix(r.body, []byte("%PDF-")) {
		t.Errorf("pdf: %d", r.status)
	}
}

// ---------------------------------------------------------------- PDF: 112 без изменений, ДДС

// Отчёт оператора 112: колонки, шапка и строка слоёв — те же, что до ракурса ДДС.
func TestPDF112Unchanged(t *testing.T) {
	t.Parallel()
	rep := sampleReport(sampleRows())
	if rep.ddsView() {
		t.Fatal("112 распознан как ДДС")
	}
	wantCols := []struct {
		title string
		w     float64
	}{
		{"№", 8}, {"Обучающийся", 38}, {"Карт.", 10}, {"Сценарий", 46}, {"Статус", 22}, {"Затрачено, с", 17},
		{"В норме", 13}, {"Поля", 11}, {"Сем.", 11}, {"Грам.", 11}, {"Время", 11}, {"Разг.", 11}, {"Итог", 12},
		{"Вердикт", 17}, {"Ошибки в карточке", contentW - 238},
	}
	cols := pdfColumns(rep.ddsView())
	if len(cols) != len(wantCols) {
		t.Fatalf("колонок %d, want %d", len(cols), len(wantCols))
	}
	for i, c := range cols {
		if c.title != wantCols[i].title || c.w != wantCols[i].w {
			t.Errorf("колонка %d = %q %.1f, want %q %.1f", i, c.title, c.w, wantCols[i].title, wantCols[i].w)
		}
	}
	rows := sampleRows()
	if got := cols[len(cols)-1].text(0, &rows[0]); got != "Адрес (не заполнено); =Телефон (неверно)" {
		t.Errorf("ошибки карточки = %q", got)
	}
	head := pdfHeaderLines(rep)
	if head[0] != "Занятие   ·   Преподаватель: Учителев И. П.   ·   Статус: Завершено   ·   Режим: Карточки   ·   Норматив: 30 с   ·   Порог зачёта: 60 баллов   ·   Голосовой режим: включён" {
		t.Errorf("шапка 112: %q", head[0])
	}
	if !strings.HasPrefix(pdfLayersLine(rep), "Средние баллы по слоям: Поля карточки 80   ·   Семантика 70   ·   Грамматика 90   ·   Время 100   ·   Разговор —") {
		t.Errorf("слои 112: %q", pdfLayersLine(rep))
	}
	if b := pdfProtocolBlocks(rep); len(b) != 0 {
		t.Errorf("у 112 раздел протокола: %+v", b)
	}
}

func ddsSampleReport(loc *time.Location) *report {
	meta := sampleMeta()
	meta.Mode = core.ModeCardActions
	meta.Settings.Perspective = model.PerspectiveDDS
	meta.Settings.Voice.Enabled = false
	rows := sampleRows()
	opened := time.Date(2026, 9, 24, 7, 1, 0, 0, time.UTC)
	comment := "Адрес вне зоны обслуживания"
	rows[0].DDS = &ddsProtocol{
		ServiceName: "Департамент ЖКХ / управляющие организации", ServiceShort: "Деп. ЖКХ",
		Status: "Начало реагирования",
		History: []public.ReactionStatusEntry{
			{At: opened, Status: "Получена службой", Operator: "система"},
			{At: opened.Add(5 * time.Second), Status: "Не принята", Operator: "оп. 227", Comment: &comment},
			{At: opened.Add(9 * time.Second), Status: "Принята", Operator: "оп. 227"},
		},
		ActionText: "Карточка принята в работу, аварийная бригада направлена на место.",
		Expected:   (&model.ReactionExpectation{RequiredStatuses: []string{"Начало реагирования"}}).Resolved(),
		Evaluated:  true,
		Remarks:    []ddsRemark{{Label: "Необоснованный отказ «Не принята»", Kind: "extra", Actual: "Не принята"}},
	}
	rows[1].DDS = &ddsProtocol{ServiceName: "Департамент ЖКХ / управляющие организации", ServiceShort: "Деп. ЖКХ",
		Expected: (*model.ReactionExpectation)(nil).Resolved()}
	return newReport(meta, rows, false, reportAt, loc)
}

func TestPDFDDS(t *testing.T) {
	t.Parallel()
	rep := ddsSampleReport(mustLoc(t, "Asia/Kolkata"))
	if !rep.ddsView() {
		t.Fatal("ракурс ДДС не распознан")
	}
	head := strings.Join(pdfHeaderLines(rep), "\n")
	for _, want := range []string{"Ракурс: Диспетчер ДДС", "Служба: Департамент ЖКХ / управляющие организации",
		"Режим: Действия с карточками", "(время Asia/Kolkata, UTC+05:30)"} {
		if !strings.Contains(head, want) {
			t.Errorf("в шапке нет %q:\n%s", want, head)
		}
	}
	if strings.Contains(head, "Голосовой режим") {
		t.Errorf("голосовой режим в шапке ДДС:\n%s", head)
	}

	layers := pdfLayersLine(rep)
	if !strings.Contains(layers, "Протокол реагирования 80") || strings.Contains(layers, "Поля карточки") || strings.Contains(layers, "Разговор") {
		t.Errorf("слои ДДС: %q", layers)
	}

	var titles []string
	w := 0.0
	for _, c := range pdfColumns(true) {
		titles = append(titles, c.title)
		if c.w <= 0 {
			t.Errorf("%s: ширина %v", c.title, c.w)
		}
		w += c.w
	}
	joined := strings.Join(titles, "|")
	for _, want := range []string{"Служба", "Прот.", "Замечания по протоколу"} {
		if !strings.Contains(joined, want) {
			t.Errorf("нет колонки %q: %s", want, joined)
		}
	}
	for _, bad := range []string{"|Поля|", "Разг.", "Ошибки в карточке", "Переспрос"} {
		if strings.Contains("|"+joined+"|", bad) {
			t.Errorf("лишняя колонка %q: %s", bad, joined)
		}
	}
	if d := w - contentW; d > 0.01 || d < -0.01 {
		t.Errorf("сумма ширин %.2f, want %.2f", w, contentW)
	}
	cols := pdfColumns(true)
	if got := cols[2].text(0, &rep.Rows[0]); got != "Деп. ЖКХ" {
		t.Errorf("служба в строке = %q", got)
	}
	if got := cols[len(cols)-1].text(0, &rep.Rows[0]); got != "Необоснованный отказ «Не принята» (лишнее действие)" {
		t.Errorf("замечания в строке = %q", got)
	}

	blocks := pdfProtocolBlocks(rep)
	if len(blocks) != 2 {
		t.Fatalf("протоколов %d, want 2", len(blocks))
	}
	b := blocks[0].title + "\n" + strings.Join(blocks[0].lines, "\n")
	for _, want := range []string{
		"Андреев Андрей Андреевич · карточка 1 · происшествие № 36814852 · служба: Деп. ЖКХ (Департамент ЖКХ / управляющие организации)",
		"Ожидается по эталону: решение «Принята» в течение 30 с; обязательные статусы: «Начало реагирования»",
		"Статус службы: Начало реагирования",
		// 07:01:05 UTC в Калькутте (+05:30) — 12:31:05
		"24.09.2026 12:31:05 Не принята (оп. 227 — Адрес вне зоны обслуживания)",
		"Текст действия: Карточка принята в работу, аварийная бригада направлена на место.",
		"Замечания: Необоснованный отказ «Не принята» (лишнее действие) — факт: Не принята",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("в протоколе нет %q:\n%s", want, b)
		}
	}
	second := strings.Join(blocks[1].lines, "\n")
	if !strings.Contains(second, "Текст действия: не заполнен") || !strings.Contains(second, "Замечания: оценки ещё нет") ||
		!strings.Contains(second, "решение «Принята» в течение 30 с") {
		t.Errorf("протокол без оценки:\n%s", second)
	}

	if _, pages := renderPDF(t, rep); pages < 1 {
		t.Error("PDF ДДС не собран")
	}
}

// БД: протокол ДДС собирается из attempts.service_id, карточки, эталона и оценки; у 112 — нет.
func TestLoadReportDDS(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	pool := f.pool

	svc, code := uuid.New(), "t"+uuid.NewString()[:8]
	exec(t, pool, `INSERT INTO services (id, code, name, short_name) VALUES ($1, $2, 'Департамент ЖКХ / управляющие организации', 'Деп. ЖКХ')`, svc, code)
	scen, et := uuid.New(), uuid.New()
	exec(t, pool, `INSERT INTO scenarios (id, title, category_id, source, status)
		SELECT $1, 'Прорыв трубы', category_id, 'manual', 'validated' FROM scenarios LIMIT 1`, scen)
	exec(t, pool, `INSERT INTO etalons (id, scenario_id, card, scoring) VALUES ($1, $2, '{}',
		'{"reaction": {"decision": "accept", "decision_within_sec": 45, "required_statuses": ["Прибытие"]}}')`, et, scen)
	lesson := uuid.New()
	exec(t, pool, `INSERT INTO lessons (id, kind, title, teacher_id, created_by, mode, time_limit_sec, settings, status)
		VALUES ($1, 'class', 'Диспетчер ДДС', $2, $2, 'card_actions', 120, '{"perspective": "dds", "voice": {"enabled": false}}', 'running')`,
		lesson, f.teacher)
	exec(t, pool, `INSERT INTO lesson_participants (lesson_id, user_id) VALUES ($1, $2)`, lesson, f.andreev)
	card := `{"services": [
		{"serviceId": "x1", "code": "101", "name": "Служба 101", "shortName": "Служба 101", "currentStatus": "Получена службой",
		 "currentStatusAt": "2026-09-24T07:01:00Z", "history": [], "allowedNext": [], "editable": true, "isPrimary": true, "source": "auto"},
		{"serviceId": "x2", "code": "` + code + `", "name": "Департамент ЖКХ", "shortName": "Деп. ЖКХ", "currentStatus": "Принята",
		 "currentStatusAt": "2026-09-24T07:01:09Z", "allowedNext": [], "editable": true, "isPrimary": false, "source": "auto",
		 "history": [{"status": "Получена службой", "at": "2026-09-24T07:01:00Z", "operator": "система"},
		             {"status": "Принята", "at": "2026-09-24T07:01:09Z", "operator": "оп. 227", "comment": "Бригада выезжает"}]}],
		"actionsTaken": "из черновика"}`
	a := uuid.New()
	exec(t, pool, `INSERT INTO attempts (id, lesson_id, user_id, scenario_id, etalon_id, mode, seq_no, status, time_limit_sec,
		incident_no, card, action_text, service_id)
		VALUES ($1, $2, $3, $4, $5, 'card_actions', 1, 'evaluated', 120, 555, $6, 'Бригада направлена на место', $7)`,
		a, lesson, f.andreev, scen, et, card, svc)
	exec(t, pool, `INSERT INTO evaluations (attempt_id, etalon_id, status, fields_score, total_score, verdict, field_errors)
		VALUES ($1, $2, 'done', 75, 75, 'pass',
		  '[{"field": "reaction.statuses", "label": "Статус «Прибытие»", "kind": "missing", "expected": "Прибытие", "weight": 1}]')`, a, et)

	meta, rows, _, err := loadReport(context.Background(), pool, lesson, allow)
	if err != nil {
		t.Fatal(err)
	}
	if !meta.Settings.IsDDS() || len(rows) != 1 || rows[0].DDS == nil {
		t.Fatalf("meta dds=%v rows=%+v", meta.Settings.IsDDS(), rows)
	}
	p := rows[0].DDS
	if p.ServiceName != "Департамент ЖКХ / управляющие организации" || p.ServiceShort != "Деп. ЖКХ" || p.Status != "Принята" ||
		len(p.History) != 2 || p.ActionText != "Бригада направлена на место" {
		t.Errorf("protocol = %+v", p)
	}
	if p.Expected.DecisionWithinSec != 45 || strings.Join(p.Expected.RequiredStatuses, ",") != "Прибытие" {
		t.Errorf("expected = %+v", p.Expected)
	}
	if !p.Evaluated || len(p.Remarks) != 1 || p.Remarks[0].Label != "Статус «Прибытие»" || p.Remarks[0].Expected != "Прибытие" {
		t.Errorf("remarks = %+v", p.Remarks)
	}

	// занятие оператора 112 из той же фикстуры — без протоколов ДДС
	_, rows112, _, err := loadReport(context.Background(), pool, f.lesson, allow)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows112 {
		if r.DDS != nil {
			t.Errorf("у попытки 112 протокол ДДС: %+v", r)
		}
	}
}
