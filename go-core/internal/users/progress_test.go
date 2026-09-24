package users

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/gen/public"
)

func fp(v float64) *float64 { return &v }

func TestPlural(t *testing.T) {
	t.Parallel()
	for n, want := range map[int]string{
		0: "попыток", 1: "попытка", 2: "попытки", 4: "попытки", 5: "попыток", 11: "попыток", 12: "попыток",
		14: "попыток", 21: "попытка", 22: "попытки", 25: "попыток", 101: "попытка", 111: "попыток", 112: "попыток", 1004: "попытки",
	} {
		if got := plural(n, "попытка", "попытки", "попыток"); got != want {
			t.Errorf("plural(%d)=%q, want %q", n, got, want)
		}
	}
}

func TestFmtNumAndDuration(t *testing.T) {
	t.Parallel()
	for v, want := range map[float64]string{70: "70", 54.56: "54,6", 54.54: "54,5", 0: "0", 99.94: "99,9", 100: "100", 69.96: "70"} {
		if got := fmtNum(v); got != want {
			t.Errorf("fmtNum(%v)=%q, want %q", v, got, want)
		}
	}
	for ms, want := range map[int]string{0: "0 с", 499: "0 с", 42000: "42 с", 59499: "59 с", 59500: "1 мин 00 с", 65000: "1 мин 05 с", 3_600_000: "60 мин 00 с"} {
		if got := fmtDuration(ms); got != want {
			t.Errorf("fmtDuration(%d)=%q, want %q", ms, got, want)
		}
	}
}

func TestLevelFor(t *testing.T) {
	t.Parallel()
	levels := []levelRow{{1, 0, "Новичок", "novice"}, {2, 100, "Стажёр", ""}, {3, 300, "Диспетчер", "dispatcher"}}
	for _, c := range []struct {
		xp        int
		no        int
		title     string
		next      string
		nextReq   int
		wantBadge string
	}{
		{0, 1, "Новичок", "Стажёр", 100, "novice"},
		{99, 1, "Новичок", "Стажёр", 100, "novice"},
		{100, 2, "Стажёр", "Диспетчер", 300, ""},
		{5000, 3, "Диспетчер", "", 0, "dispatcher"},
	} {
		lv := levelFor(levels, c.xp)
		if lv.No != c.no || lv.Title != c.title || lv.XpRequired == nil {
			t.Fatalf("xp=%d: %+v", c.xp, lv)
		}
		if c.next == "" {
			if lv.NextTitle != nil || lv.NextXpRequired != nil {
				t.Fatalf("xp=%d: на максимальном уровне есть следующий: %+v", c.xp, lv)
			}
		} else if lv.NextTitle == nil || *lv.NextTitle != c.next || *lv.NextXpRequired != c.nextReq {
			t.Fatalf("xp=%d: следующий уровень %+v", c.xp, lv)
		}
		if (lv.Badge == nil) != (c.wantBadge == "") || lv.Badge != nil && *lv.Badge != c.wantBadge {
			t.Fatalf("xp=%d: badge %v", c.xp, lv.Badge)
		}
	}
	// пустой справочник / справочник не с нуля
	if lv := levelFor(nil, 10); lv.No != 0 || lv.Title != "Без уровня" || lv.NextTitle != nil {
		t.Fatalf("пустой справочник: %+v", lv)
	}
	lv := levelFor([]levelRow{{1, 50, "Первый", ""}}, 10)
	if lv.No != 0 || lv.XpRequired != nil || lv.NextTitle == nil || *lv.NextTitle != "Первый" {
		t.Fatalf("до первого уровня: %+v", lv)
	}
}

func TestSystemRecommendations(t *testing.T) {
	t.Parallel()
	t.Run("нет данных — пусто, не nil", func(t *testing.T) {
		t.Parallel()
		out := systemRecommendations(recInput{threshold: 70})
		if out == nil || len(out) != 0 {
			t.Fatalf("%v", out)
		}
	})
	t.Run("слабые категории", func(t *testing.T) {
		t.Parallel()
		out := systemRecommendations(recInput{threshold: 70, categories: []categoryRow{
			{name: "Пожар", done: 3, avg: fp(54.5)},
			{name: "ДТП", done: 2, avg: fp(40)},
			{name: "Одна попытка", done: 1, avg: fp(10)}, // мало попыток
			{name: "Без оценки", done: 5, avg: nil},      // нет балла
			{name: "Сдано", done: 4, avg: fp(70)},        // ровно порог — не слабая
			{name: "", done: 21, avg: fp(69.96)},         // без названия
		}})
		if len(out) != 1 || out[0].Id != recWeakCategoryID || out[0].Kind != public.WeakCategory {
			t.Fatalf("%+v", out)
		}
		if !strings.Contains(out[0].Body, "(70)") {
			t.Fatalf("порог в тексте: %q", out[0].Body)
		}
		want := []string{"«ДТП» — средний балл 40, 2 попытки", "«Пожар» — средний балл 54,5, 3 попытки",
			"«Категория без названия» — средний балл 70, 21 попытка"}
		if got := *out[0].Items; strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("items:\n%q\nwant\n%q", got, want)
		}
	})
	t.Run("повторяющиеся ошибки полей — топ-3 с count>=2", func(t *testing.T) {
		t.Parallel()
		out := systemRecommendations(recInput{threshold: 70, fieldErrors: []progressFieldError{
			{Field: "address.raw", Label: "Адрес", Kind: "missing", Count: 21},
			{Field: "applicant.name", Label: "ФИО заявителя", Kind: "wrong", Count: 3},
			{Field: "x", Label: "Лишнее", Kind: "extra", Count: 2},
			{Field: "y", Label: "Четвёртое", Kind: "wrong", Count: 2},
			{Field: "z", Label: "Разовое", Kind: "wrong", Count: 1},
		}})
		if len(out) != 1 || out[0].Id != recWeakFieldID || out[0].Kind != public.WeakField {
			t.Fatalf("%+v", out)
		}
		want := []string{"Адрес — не заполнено (21 раз)", "ФИО заявителя — не совпадает с эталоном (3 раза)",
			"Лишнее — лишнее значение (2 раза)"}
		if got := *out[0].Items; strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("items: %q", got)
		}
		if out := systemRecommendations(recInput{fieldErrors: []progressFieldError{{Label: "a", Count: 1}}}); len(out) != 0 {
			t.Fatalf("разовая ошибка дала рекомендацию: %+v", out)
		}
	})
	t.Run("медленная работа", func(t *testing.T) {
		t.Parallel()
		avg := 65000
		out := systemRecommendations(recInput{attemptsDone: 4, withinNorm: 1, avgTimeMs: &avg})
		if len(out) != 1 || out[0].Id != recSlowTimingID || out[0].Kind != public.SlowTiming || out[0].Items != nil {
			t.Fatalf("%+v", out)
		}
		if !strings.Contains(out[0].Body, "1 из 4 карточек") || !strings.Contains(out[0].Body, "1 мин 05 с") {
			t.Fatalf("body: %q", out[0].Body)
		}
		if out := systemRecommendations(recInput{attemptsDone: 4, withinNorm: 2}); len(out) != 0 {
			t.Fatal("половина в нормативе — не медленно")
		}
		if out := systemRecommendations(recInput{attemptsDone: 1, withinNorm: 0}); len(out) != 0 {
			t.Fatal("одна попытка — рано судить")
		}
		zero := 0
		out = systemRecommendations(recInput{attemptsDone: 2, withinNorm: 0, avgTimeMs: &zero})
		if len(out) != 1 || strings.Contains(out[0].Body, "Среднее время") || !strings.Contains(out[0].Body, "0 из 2 карточек") {
			t.Fatalf("без времени: %+v", out)
		}
	})
}

func TestKindText(t *testing.T) {
	t.Parallel()
	for k, want := range map[string]string{"missing": "не заполнено", "wrong": "не совпадает с эталоном", "extra": "лишнее значение", "other": "other"} {
		if got := kindText(k); got != want {
			t.Errorf("kindText(%q)=%q", k, got)
		}
	}
}

func TestBuildProgressEmptyShape(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	sp := buildProgress(id, "Рожкова Ольга", &progressData{}, 70, func(s string) string { return s })
	b, err := json.Marshal(sp)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"categories", "fieldErrors", "recommendations", "xpLog"} {
		if arr, ok := m[k].([]any); !ok || len(arr) != 0 {
			t.Fatalf("%s должен быть [], а не null: %s", k, b)
		}
	}
	for _, k := range []string{"userId", "name", "xp", "attemptsDone", "level"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("нет обязательного %s: %s", k, b)
		}
	}
	for _, k := range []string{"avgScore", "avgScore30d", "passRatePct", "avgTimeMs", "lastActivityAt"} {
		if _, ok := m[k]; ok {
			t.Fatalf("пустое %s должно отсутствовать: %s", k, b)
		}
	}
	if m["withinNormCount"] != float64(0) {
		t.Fatalf("withinNormCount: %s", b)
	}
}

func TestBuildProgressFull(t *testing.T) {
	t.Parallel()
	msk := time.FixedZone("MSK", 3*3600)
	last := time.Date(2026, 9, 24, 15, 0, 0, 0, msk)
	calls := map[string]int{}
	label := func(f string) string { calls[f]++; return "подпись " + f }
	stored := public.Recommendation{Id: uuid.NewString(), Kind: public.General, Body: "Совет преподавателя"}
	d := &progressData{
		attemptsDone: 4, withinNorm: 1, xp: 120,
		avgScore: fp(61.26), avgScore30d: fp(64), passRate: fp(25), avgTimeMs: fp(40500.5),
		lastActivity: &last,
		levels:       []levelRow{{1, 0, "Новичок", ""}, {2, 100, "Стажёр", ""}},
		categories:   []categoryRow{{id: "c1", name: "Пожар", done: 3, avg: fp(50), pass: fp(33.3), avgTime: fp(1000.4), last: &last}},
		fieldErrors: []fieldErrorRow{
			{field: "address.raw", kind: "missing", count: 3},
			{field: "address.raw", kind: "wrong", count: 2},
		},
		stored: []public.Recommendation{stored},
	}
	sp := buildProgress(uuid.New(), "Рожкова Ольга", d, 70, label)
	if sp.Level.No != 2 || sp.Xp != 120 || *sp.AvgTimeMs != 40501 || *sp.AvgScore != float32(61.26) {
		t.Fatalf("итоги: %+v", sp)
	}
	if sp.LastActivityAt.Location() != time.UTC || !sp.LastActivityAt.Equal(last) {
		t.Fatalf("время не в UTC: %v", sp.LastActivityAt)
	}
	if len(sp.Categories) != 1 || *sp.Categories[0].AvgTimeMs != 1000 || sp.Categories[0].LastAttemptAt.Location() != time.UTC {
		t.Fatalf("категории: %+v", sp.Categories)
	}
	if len(sp.FieldErrors) != 2 || sp.FieldErrors[0].Label != "подпись address.raw" || calls["address.raw"] != 1 {
		t.Fatalf("подписи полей (одна на поле): %+v %v", sp.FieldErrors, calls)
	}
	// сначала сохранённые, затем системные: слабая категория, ошибки поля, медленная работа
	ids := []string{}
	for _, r := range sp.Recommendations {
		ids = append(ids, r.Id)
	}
	if want := []string{stored.Id, recWeakCategoryID, recWeakFieldID, recSlowTimingID}; strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("рекомендации: %v", ids)
	}
	if sp.XpLog == nil {
		t.Fatal("xpLog nil")
	}
}

func TestNumericHelpers(t *testing.T) {
	t.Parallel()
	if f32(nil) != nil || intPtr(nil) != nil || utcPtr(nil) != nil {
		t.Fatal("nil-указатели")
	}
	if *intPtr(fp(2.5)) != 3 || *intPtr(fp(2.49)) != 2 {
		t.Fatal("округление intPtr")
	}
	if got := nonBlank("", " Иванов ", "Иван", "  "); strings.Join(got, "|") != "Иванов|Иван" {
		t.Fatalf("nonBlank: %q", got)
	}
	if got := nonBlank(); len(got) != 0 {
		t.Fatalf("nonBlank(): %v", got)
	}
}
