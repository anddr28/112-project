package reports

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
)

var msk = time.FixedZone("MSK", 3*3600)

func fp(v float64) *float64 { return &v }
func ip(v int) *int         { return &v }
func bp(v bool) *bool       { return &v }
func tp(t time.Time) *time.Time {
	return &t
}

func TestCSVText(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":                         "",
		"Иванов Иван":              "Иванов Иван",
		"=HYPERLINK(\"http://x\")": "'=HYPERLINK(\"http://x\")",
		"+7 999 000-00-00":         "'+7 999 000-00-00",
		"-1":                       "'-1",
		"@SUM(A1)":                 "'@SUM(A1)",
		"строка\r\nвторая\tтретья": "строка  вторая третья",
		"\n=1+1":    " =1+1", // перевод строки в начале — пробел, формулой уже не станет
		"Сумма = 5": "Сумма = 5",
		"причина; с разделителем": "причина; с разделителем",
	}
	for in, want := range cases {
		if got := csvText(in); got != want {
			t.Errorf("csvText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCSVValue(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 24, 7, 0, 5, 0, time.UTC)
	cases := []struct {
		kind colKind
		v    any
		want string
	}{
		{kText, nil, ""},
		{kText, "=cmd", "'=cmd"},
		{kInt, 7, "7"},
		{kInt, int64(36814852), "36814852"},
		{kScore, 87.0, "87"},
		{kScore, 86.6, "87"},
		{kSeconds, 12.345, "12,3"},
		{kSeconds, 0.0, "0,0"},
		{kBool, true, "да"},
		{kBool, false, "нет"},
		{kTime, at, "24.09.2026 10:00:05"},
		{kText, struct{}{}, ""},
	}
	for _, tc := range cases {
		if got := csvValue(tc.kind, tc.v, msk); got != tc.want {
			t.Errorf("csvValue(%v, %#v) = %q, want %q", tc.kind, tc.v, got, tc.want)
		}
	}
}

func TestNumberFormatting(t *testing.T) {
	t.Parallel()
	if got := fmtDecimal(1234.56, 1); got != "1234,6" {
		t.Errorf("fmtDecimal = %q", got)
	}
	if got := fmtDecimal(-0.25, 2); got != "-0,25" {
		t.Errorf("fmtDecimal = %q", got)
	}
	if got := fmtScore(nil); got != "—" {
		t.Errorf("fmtScore(nil) = %q", got)
	}
	if got := fmtScore(fp(70)); got != "70" {
		t.Errorf("fmtScore = %q", got)
	}
	if got := fmtPercent(fp(66)); got != "66 %" {
		t.Errorf("fmtPercent = %q", got)
	}
	if got := fmtPercent(nil); got != "—" {
		t.Errorf("fmtPercent(nil) = %q", got)
	}
	at := time.Date(2026, 1, 2, 21, 30, 0, 0, time.UTC)
	if got := fmtTime(&at, msk, layoutShort); got != "03.01.2026 00:30" {
		t.Errorf("fmtTime = %q (переход через полночь в зоне отчёта)", got)
	}
	if got := fmtTime(nil, msk, layoutShort); got != "—" {
		t.Errorf("fmtTime(nil) = %q", got)
	}
}

func TestZoneLabel(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		loc  *time.Location
		want string
	}{
		{msk, "MSK, UTC+03:00"},
		{time.UTC, "UTC"},
		{time.FixedZone("NST", -(3*3600 + 30*60)), "NST, UTC−03:30"},
		{time.FixedZone("IST", 5*3600+30*60), "IST, UTC+05:30"},
		{time.FixedZone("LINT", 14*3600), "LINT, UTC+14:00"},
	}
	for _, tc := range cases {
		if got := zoneLabel(at, tc.loc); got != tc.want {
			t.Errorf("zoneLabel(%v) = %q, want %q", tc.loc, got, tc.want)
		}
	}
}

func TestWallClock(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 24, 22, 15, 30, 999_000_000, time.UTC)
	got := wallClock(at, msk)
	want := time.Date(2026, 9, 25, 1, 15, 30, 0, time.UTC)
	if !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("wallClock = %v, want %v", got, want)
	}
}

func TestSafeFileTitle(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":           "",
		"Занятие №1": "Занятие №1",
		`Пожары: 1/2 "итог" <черновик>?*|`: "Пожары 1 2 итог черновик",
		"  много   пробелов\tи\nстрок  ":   "много пробелов и строк",
		"Итоги...":    "Итоги",
		"...":         "",
		"a\x00b\x7fc": "a b c",
		"с\\обратным": "с обратным",
		"эмодзи 🚒 ок": "эмодзи 🚒 ок",
	}
	for in, want := range cases {
		if got := safeFileTitle(in); got != want {
			t.Errorf("safeFileTitle(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("Д", 200)
	if got := safeFileTitle(long); len([]rune(got)) != 80 {
		t.Errorf("длина = %d рун, want 80", len([]rune(got)))
	}
	// невалидный UTF-8 не попадает в имя файла
	if got := safeFileTitle("a\xffb"); got != "a b" {
		t.Errorf("safeFileTitle(invalid) = %q", got)
	}
}

func TestContentDisposition(t *testing.T) {
	t.Parallel()
	if got := rfc5987("Отчёт — A b.csv"); got != "%D0%9E%D1%82%D1%87%D1%91%D1%82%20%E2%80%94%20A%20b.csv" {
		t.Errorf("rfc5987 = %q", got)
	}
	if got := rfc5987("a!#$&+-.^_`|~z"); got != "a!#$&+-.^_`|~z" {
		t.Errorf("attr-char экранированы: %q", got)
	}
	if got := rfc5987(`"';%*()`); got != "%22%27%3B%25%2A%28%29" {
		t.Errorf("спецсимволы = %q", got)
	}
	cd := contentDisposition("lesson-report-2026-09-24.pdf", "Отчёт.pdf")
	if cd != `attachment; filename="lesson-report-2026-09-24.pdf"; filename*=UTF-8''%D0%9E%D1%82%D1%87%D1%91%D1%82.pdf` {
		t.Errorf("contentDisposition = %q", cd)
	}
}

func TestFileNames(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 24, 22, 0, 0, 0, time.UTC) // в Москве уже 25-е
	ascii, utf := fileNames(&lessonMeta{Title: "Пожары: итог"}, "xlsx", at, msk)
	if ascii != "lesson-report-2026-09-25.xlsx" {
		t.Errorf("ascii = %q", ascii)
	}
	if utf != "Отчёт — Пожары итог — 2026-09-25.xlsx" {
		t.Errorf("utf = %q", utf)
	}
	ascii, utf = fileNames(&lessonMeta{Title: " /// "}, "csv", at, time.UTC)
	if ascii != "lesson-report-2026-09-24.csv" || utf != ascii {
		t.Errorf("пустое название: %q %q", ascii, utf)
	}
}

func TestLabels(t *testing.T) {
	t.Parallel()
	if errorText("Адрес", "missing") != "Адрес (не заполнено)" || errorText("Адрес", "wrong") != "Адрес (неверно)" ||
		errorText("Адрес", "extra") != "Адрес (лишнее)" || errorText("Адрес", "") != "Адрес" || errorText("Адрес", "other") != "Адрес" {
		t.Error("errorText")
	}
	if withVersion("ДТП", 1) != "ДТП" || withVersion("ДТП", 0) != "ДТП" || withVersion("ДТП", 3) != "ДТП (версия 3)" {
		t.Error("withVersion")
	}
	if fullName(" Иванов ", "Иван", "") != "Иванов Иван" || fullName("Иванов", "Иван", "Иванович") != "Иванов Иван Иванович" ||
		fullName("", "", "") != "" {
		t.Error("fullName")
	}
	if statusText(&reportRow{}) != "Карточка не выдана" || statusText(&reportRow{Attempt: true, Status: core.AttemptExpired}) != "Не выполнена" ||
		statusText(&reportRow{Attempt: true, Status: "weird"}) != "weird" {
		t.Error("statusText")
	}
	if lessonStatusText(core.LessonFinished) != "Завершено" || lessonStatusText("x") != "x" {
		t.Error("lessonStatusText")
	}
	if modeText(core.ModeCardActions) != "Действия с карточками" || modeText(core.ModeCards) != "Карточки" {
		t.Error("modeText")
	}
	if kindText(core.LessonKindPractice) != "Самостоятельная практика" || kindText(core.LessonKindClass) != "Занятие" {
		t.Error("kindText")
	}
	for s, want := range attemptStatusLabel {
		if want == "" || statusText(&reportRow{Attempt: true, Status: s}) != want {
			t.Errorf("статус %s без подписи", s)
		}
	}
}

func TestVerdictText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		r    reportRow
		want string
	}{
		{"нет карточки", reportRow{}, ""},
		{"нет оценки", reportRow{Attempt: true}, ""},
		{"оценка в процессе", reportRow{Attempt: true, EvalStatus: core.EvalPartial, Final: fp(50), Verdict: core.VerdictFail}, "ожидается"},
		{"зачёт", reportRow{Attempt: true, EvalStatus: core.EvalDone, Final: fp(80), Verdict: core.VerdictPass}, "Зачёт"},
		{"незачёт", reportRow{Attempt: true, EvalStatus: core.EvalDone, Final: fp(40), Verdict: core.VerdictFail}, "Незачёт"},
		{"оценено, вердикт pending", reportRow{Attempt: true, EvalStatus: core.EvalDone, Final: fp(40), Verdict: core.VerdictPending}, "ожидается"},
		{"корректировка до окончания слоёв", reportRow{Attempt: true, EvalStatus: core.EvalPartial, Overridden: true, Final: fp(75), Verdict: core.VerdictPass}, "Зачёт"},
	}
	for _, tc := range cases {
		if got := verdictText(&tc.r); got != tc.want {
			t.Errorf("%s: verdictText = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestReactionMs(t *testing.T) {
	t.Parallel()
	a := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	if (&reportRow{}).ReactionMs() != nil || (&reportRow{CallAcceptedAt: &a}).ReactionMs() != nil {
		t.Error("без обеих меток — nil")
	}
	b := a.Add(2500 * time.Millisecond)
	if got := (&reportRow{CallAcceptedAt: &a, FirstInputAt: &b}).ReactionMs(); got == nil || *got != 2500 {
		t.Errorf("reaction = %v", got)
	}
	early := a.Add(-time.Second) // рассинхрон часов — не отрицательное время
	if got := (&reportRow{CallAcceptedAt: &a, FirstInputAt: &early}).ReactionMs(); got == nil || *got != 0 {
		t.Errorf("reaction = %v", got)
	}
}

func TestRoundScore(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want float64 }{
		{0, 0}, {-0.4, 0}, {0.5, 1}, {2.5, 3}, {69.49, 69}, {99.5, 100}, {-1.6, -2},
		{math.NaN(), 0}, {math.Inf(1), 0}, {math.Inf(-1), 0},
	}
	for _, tc := range cases {
		got := roundScore(tc.in)
		if got != tc.want || math.Signbit(got) && got == 0 {
			t.Errorf("roundScore(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestSummarize(t *testing.T) {
	t.Parallel()
	u1, u2, u3 := uuid.New(), uuid.New(), uuid.New()
	rows := []reportRow{
		// Андреев: зачёт в нормативе + карточка в работе
		{UserID: u1, FullName: "Андреев А", Attempt: true, Status: core.AttemptEvaluated, EvalStatus: core.EvalDone,
			Final: fp(90), Verdict: core.VerdictPass, WithinNorm: bp(true), SpentMs: ip(20000),
			Fields: fp(100), Semantic: fp(80), Grammar: fp(90), Timing: fp(100)},
		{UserID: u1, FullName: "Андреев А", Attempt: true, Status: core.AttemptInProgress},
		// Борисов: незачёт вне норматива, требует ревью; вторая — скорректирована до зачёта
		{UserID: u2, FullName: "Борисов Б", Attempt: true, Status: core.AttemptEvaluated, EvalStatus: core.EvalDone,
			Final: fp(41), Verdict: core.VerdictFail, WithinNorm: bp(false), SpentMs: ip(40000), NeedsReview: true,
			Fields: fp(50), Semantic: fp(30), Dialogue: fp(60)},
		{UserID: u2, FullName: "Борисов Б", Attempt: true, Status: core.AttemptEvaluating, EvalStatus: core.EvalPartial,
			Overridden: true, Final: fp(75), Verdict: core.VerdictPass},
		// Васильев: карточка не выдана
		{UserID: u3, FullName: "Васильев В"},
		// попытка без оценки, время вышло
		{UserID: uuid.New(), FullName: "Григорьев Г", Attempt: true, Status: core.AttemptExpired},
	}
	s, st := summarize(rows)
	check := func(name string, got, want int) {
		if got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}
	check("Participants", s.Participants, 4)
	check("Attempts", s.Attempts, 5)
	check("NotIssued", s.NotIssued, 1)
	check("InWork", s.InWork, 1)
	check("Submitted", s.Submitted, 3)
	check("Expired", s.Expired, 1)
	check("Evaluated", s.Evaluated, 3)
	check("Passed", s.Passed, 2)
	check("NeedsReview", s.NeedsReview, 1)
	check("Overridden", s.Overridden, 1)
	check("WithinNorm", s.WithinNorm, 1)
	check("TimingKnown", s.TimingKnown, 2)
	if s.AvgFinal == nil || *s.AvgFinal != 69 { // (90+41+75)/3 = 68.67
		t.Errorf("AvgFinal = %v", s.AvgFinal)
	}
	if s.PassRate == nil || *s.PassRate != 67 {
		t.Errorf("PassRate = %v", s.PassRate)
	}
	if s.WithinRate == nil || *s.WithinRate != 50 {
		t.Errorf("WithinRate = %v", s.WithinRate)
	}
	if s.AvgSpentMs == nil || *s.AvgSpentMs != 30000 {
		t.Errorf("AvgSpentMs = %v", s.AvgSpentMs)
	}
	// слои: fields, semantic, grammar, timing, dialogue
	wantLayers := []*float64{fp(75), fp(55), fp(90), fp(100), fp(60)}
	for i, w := range wantLayers {
		if s.AvgLayers[i] == nil || *s.AvgLayers[i] != *w {
			t.Errorf("AvgLayers[%s] = %v, want %v", layerKeys[i], s.AvgLayers[i], *w)
		}
	}
	if len(st) != 4 {
		t.Fatalf("students = %+v", st)
	}
	if st[0].Name != "Андреев А" || st[0].Attempts != 2 || st[0].Evaluated != 1 || st[0].Passed != 1 ||
		st[0].AvgFinal == nil || *st[0].AvgFinal != 90 || st[0].WithinNorm != 1 || st[0].TimingKnown != 1 {
		t.Errorf("student[0] = %+v", st[0])
	}
	if st[1].Evaluated != 2 || st[1].Passed != 1 || st[1].AvgFinal == nil || *st[1].AvgFinal != 58 {
		t.Errorf("student[1] = %+v", st[1])
	}
	if st[2].Attempts != 0 || st[2].AvgFinal != nil {
		t.Errorf("student[2] = %+v", st[2])
	}

	empty, est := summarize(nil)
	if empty.Participants != 0 || empty.AvgFinal != nil || empty.PassRate != nil || empty.WithinRate != nil ||
		empty.AvgSpentMs != nil || est == nil || len(est) != 0 {
		t.Errorf("пустой отчёт: %+v %v", empty, est)
	}
}

func TestColumns(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, c := range columns {
		if c.Title == "" || seen[c.Title] {
			t.Errorf("колонка без имени или повтор (таблица Excel требует уникальных заголовков): %q", c.Title)
		}
		seen[c.Title] = true
		if c.Width <= 0 {
			t.Errorf("%s: ширина %v", c.Title, c.Width)
		}
	}
	// строка без карточки: заполнены только №, ФИО и статус
	r := &reportRow{FullName: "Васильев Василий"}
	for i, c := range columns {
		v := c.value(4, r)
		switch c.Title {
		case "№":
			if v != 5 {
				t.Errorf("№ = %v", v)
			}
		case "Обучающийся":
			if v != "Васильев Василий" {
				t.Errorf("ФИО = %v", v)
			}
		case "Статус":
			if v != "Карточка не выдана" {
				t.Errorf("статус = %v", v)
			}
		default:
			if v != nil {
				t.Errorf("колонка %d %q = %#v, want пусто", i, c.Title, v)
			}
		}
	}
}

func TestPDFHelpers(t *testing.T) {
	t.Parallel()
	if got := pdfText("Пожар 🔥\tв\r\nдоме\x07!"); got != "Пожар ? в  доме!" {
		t.Errorf("pdfText = %q", got)
	}
	if got := pdfText("a�b"); got != "a?b" {
		t.Errorf("pdfText(replacement) = %q", got)
	}
	if got := truncRunes("Длинное название", 7); got != "Длинное…" {
		t.Errorf("truncRunes = %q", got)
	}
	if got := truncRunes("Коротко", 7); got != "Коротко" {
		t.Errorf("truncRunes = %q", got)
	}
	if dashIfNil(nil) != "—" || dashIfNil(fp(12.0)) != "12" {
		t.Error("dashIfNil")
	}
	if ifAttempt(&reportRow{}, "x") != "" || ifAttempt(&reportRow{Attempt: true}, "x") != "x" {
		t.Error("ifAttempt")
	}
	if got := ratioShort(fp(70), 7, 10); got != "70 % (7/10)" {
		t.Errorf("ratioShort = %q", got)
	}
	if ratioShort(nil, 0, 0) != "—" || ratioShort(fp(0), 0, 0) != "—" {
		t.Error("ratioShort пусто")
	}
	if *ptr(3) != 3 {
		t.Error("ptr")
	}
}

func TestXLSXHelpers(t *testing.T) {
	t.Parallel()
	if got := xlsxHF("Пожары & ДТП"); got != "Пожары && ДТП" {
		t.Errorf("xlsxHF = %q", got)
	}
	if got := xlsxHF(strings.Repeat("я", 70)); len([]rune(got)) != 61 || !strings.HasSuffix(got, "…") {
		t.Errorf("xlsxHF длинное = %q", got)
	}
	if orDash("  ") != "—" || orDash("x") != "x" {
		t.Error("orDash")
	}
	if scoreOrDash(nil) != "—" || scoreOrDash(fp(88)) != 88 {
		t.Error("scoreOrDash")
	}
	cases := []struct {
		n, of int
		want  string
	}{{7, 10, "7 из 10 (70 %)"}, {0, 0, "—"}, {1, 3, "1 из 3 (33 %)"}, {2, 3, "2 из 3 (67 %)"}}
	for _, tc := range cases {
		if got := ratioText(tc.n, tc.of); got != tc.want {
			t.Errorf("ratioText(%d, %d) = %q, want %q", tc.n, tc.of, got, tc.want)
		}
	}
	if durationOrDash(nil) != "—" || durationOrDash(fp(83_400)) != "1 мин 23 с" || durationOrDash(fp(59_600)) != "1 мин" {
		t.Errorf("durationOrDash = %q %q", durationOrDash(fp(83_400)), durationOrDash(fp(59_600)))
	}
}
