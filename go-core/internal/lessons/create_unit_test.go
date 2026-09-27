package lessons

import (
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/settings"
)

var (
	scA = uuid.MustParse("0190a000-0000-7000-8000-00000000000a")
	scB = uuid.MustParse("0190a000-0000-7000-8000-00000000000b")
	stA = uuid.MustParse("0190b000-0000-7000-8000-00000000000a")
	stB = uuid.MustParse("0190b000-0000-7000-8000-00000000000b")
)

func validBody() createBody {
	return createBody{
		Title:          ptr("Практическое занятие: пожары"),
		Mode:           ptr(core.ModeCards),
		Perspective:    ptr(model.PerspectiveOperator112),
		TimeLimitSec:   ptr(30),
		ScenarioIDs:    []string{scA.String(), scB.String()},
		ParticipantIDs: []string{stA.String(), stB.String()},
		PassThreshold:  ptr(70.0),
		AllowReplay:    ptr(true),
	}
}

func defaultsSnap() *settings.Snapshot {
	d := settings.Defaults()
	return &d
}

func sumW(w settings.Weights) float64 { return w.Sum() }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-4 }

func TestParseCreate_Valid(t *testing.T) {
	t.Parallel()
	b := validBody()
	b.Title = ptr("   Занятие с пробелами   ")
	b.Difficulty = ptr(2)
	b.ScenarioIDs = []string{scB.String(), " " + scA.String() + " ", scB.String()} // дубль и пробелы
	d, fields := parseCreate(&b, defaultsSnap())
	if len(fields) != 0 {
		t.Fatalf("fields: %v", fields)
	}
	if d.Title != "Занятие с пробелами" {
		t.Errorf("title %q: пробелы по краям обрезаются", d.Title)
	}
	if d.Mode != core.ModeCards || d.TimeLimitSec != 30 || d.Difficulty == nil || *d.Difficulty != 2 {
		t.Errorf("draft %+v", d)
	}
	if len(d.ScenarioIDs) != 2 || d.ScenarioIDs[0] != scB || d.ScenarioIDs[1] != scA {
		t.Errorf("пул %v: порядок входа сохраняется, дубли выброшены", d.ScenarioIDs)
	}
	ls := d.Settings
	if ls.PassThreshold != 70 || !ls.AllowReplay || ls.Perspective != model.PerspectiveOperator112 {
		t.Errorf("settings %+v", ls)
	}
	if ls.Voice.Enabled || ls.Weights.Dialogue != 0 || !near(sumW(ls.Weights), 1) {
		t.Errorf("без голоса: разговор 0, сумма 1: %+v", ls.Weights)
	}
	if ls.CardsPerStudent != settings.Defaults().CardsPerStudent {
		t.Errorf("cardsPerStudent %d — из настроек платформы", ls.CardsPerStudent)
	}
}

func TestParseCreate_Defaults(t *testing.T) {
	t.Parallel()
	// Старый клиент: только обязательное по смыслу; остальное — из настроек платформы.
	snap := defaultsSnap()
	snap.PassThreshold = 55
	snap.AllowReplay = false
	snap.TimeLimitSec = 45
	b := createBody{
		Title: ptr("Минимальное занятие"), Mode: ptr(core.ModeCardActions),
		ScenarioIDs: []string{scA.String()}, ParticipantIDs: []string{stA.String()},
	}
	d, fields := parseCreate(&b, snap)
	if len(fields) != 0 {
		t.Fatalf("fields: %v", fields)
	}
	if d.Settings.PassThreshold != 55 || d.Settings.AllowReplay || d.Settings.Perspective != model.PerspectiveOperator112 {
		t.Errorf("settings %+v", d.Settings)
	}
	// Регрессия: settings.time_limit_sec (админка) раньше ни на что не влиял — без
	// timeLimitSec создание падало 400.
	if d.TimeLimitSec != 45 {
		t.Errorf("timeLimitSec %d, want 45 (settings.time_limit_sec)", d.TimeLimitSec)
	}
	if d.Difficulty != nil {
		t.Errorf("difficulty %v: не передана — NULL (любая)", *d.Difficulty)
	}
}

func TestParseCreate_TimeLimitDefaultClamped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ snap, want int }{{0, minTimeLimitSec}, {5, minTimeLimitSec}, {99999, maxTimeLimitSec}, {600, 600}} {
		snap := defaultsSnap()
		snap.TimeLimitSec = tc.snap
		b := validBody()
		b.TimeLimitSec = nil
		d, fields := parseCreate(&b, snap)
		if len(fields) != 0 || d.TimeLimitSec != tc.want {
			t.Errorf("snap %d: got %d (%v), want %d", tc.snap, d.TimeLimitSec, fields, tc.want)
		}
	}
}

func TestParseCreate_Errors(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("ж", maxTitleRunes+1)
	tooMany := make([]string, maxScenarios+1)
	for i := range tooMany {
		tooMany[i] = uuid.NewString()
	}
	cases := []struct {
		name  string
		mut   func(b *createBody)
		field string
	}{
		{"нет названия", func(b *createBody) { b.Title = nil }, "title"},
		{"короткое название (руны, не байты)", func(b *createBody) { b.Title = ptr("Пж") }, "title"},
		{"название из пробелов", func(b *createBody) { b.Title = ptr("    ") }, "title"},
		{"длинное название", func(b *createBody) { b.Title = ptr(long) }, "title"},
		{"нет режима", func(b *createBody) { b.Mode = nil }, "mode"},
		{"неизвестный режим", func(b *createBody) { b.Mode = ptr("both") }, "mode"},
		{"ракурс", func(b *createBody) { b.Perspective = ptr("dispatcher") }, "perspective"},
		{"сложность 0", func(b *createBody) { b.Difficulty = ptr(0) }, "difficulty"},
		{"сложность 4", func(b *createBody) { b.Difficulty = ptr(4) }, "difficulty"},
		{"норматив 9", func(b *createBody) { b.TimeLimitSec = ptr(9) }, "timeLimitSec"},
		{"норматив 3601", func(b *createBody) { b.TimeLimitSec = ptr(3601) }, "timeLimitSec"},
		{"пустой пул", func(b *createBody) { b.ScenarioIDs = []string{} }, "scenarioIds"},
		{"пул nil", func(b *createBody) { b.ScenarioIDs = nil }, "scenarioIds"},
		{"пул слишком длинный", func(b *createBody) { b.ScenarioIDs = tooMany }, "scenarioIds"},
		{"кривой UUID сценария", func(b *createBody) { b.ScenarioIDs = []string{scA.String(), "пожар"} }, "scenarioIds[1]"},
		{"нет участников", func(b *createBody) { b.ParticipantIDs = nil }, "participantIds"},
		{"кривой UUID участника", func(b *createBody) { b.ParticipantIDs = []string{""} }, "participantIds[0]"},
		{"порог -1", func(b *createBody) { b.PassThreshold = ptr(-1.0) }, "passThreshold"},
		{"порог 100.5", func(b *createBody) { b.PassThreshold = ptr(100.5) }, "passThreshold"},
		{"порог NaN", func(b *createBody) { b.PassThreshold = ptr(math.NaN()) }, "passThreshold"},
		{"карточек 0", func(b *createBody) { b.CardsPerStudent = ptr(0) }, "cardsPerStudent"},
		{"карточек 51", func(b *createBody) { b.CardsPerStudent = ptr(51) }, "cardsPerStudent"},
		{"голос: input", func(b *createBody) { b.Voice = &voiceBody{Input: ptr("telepathy")} }, "voice.input"},
		{"голос: реплик 1", func(b *createBody) { b.Voice = &voiceBody{MaxTurns: ptr(1)} }, "voice.maxTurns"},
		{"голос: реплик 41", func(b *createBody) { b.Voice = &voiceBody{MaxTurns: ptr(41)} }, "voice.maxTurns"},
		{"вес отрицательный", func(b *createBody) { b.Weights = map[string]*float64{"fields": ptr(-0.1)} }, "weights.fields"},
		{"вес Inf", func(b *createBody) { b.Weights = map[string]*float64{"grammar": ptr(math.Inf(1))} }, "weights.grammar"},
		{"неизвестный слой", func(b *createBody) { b.Weights = map[string]*float64{"мимика": ptr(0.1)} }, "weights.мимика"},
		{"сумма весов 0", func(b *createBody) {
			z := ptr(0.0)
			b.Weights = map[string]*float64{"fields": z, "semantic": z, "grammar": z, "timing": z, "dialogue": ptr(1.0)}
		}, "weights"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := validBody()
			tc.mut(&b)
			_, fields := parseCreate(&b, defaultsSnap())
			if fields[tc.field] == "" {
				t.Fatalf("нет ошибки по %q: %v", tc.field, fields)
			}
			for k, msg := range fields {
				if msg == "" || !strings.ContainsFunc(msg, func(r rune) bool { return r >= 'А' && r <= 'я' }) {
					t.Errorf("сообщение по %q не по-русски: %q", k, msg)
				}
			}
		})
	}
}

func TestParseCreate_Weights(t *testing.T) {
	t.Parallel()
	w := func(f, s, g, tm float64, dlg *float64) map[string]*float64 {
		m := map[string]*float64{"fields": ptr(f), "semantic": ptr(s), "grammar": ptr(g), "timing": ptr(tm)}
		if dlg != nil {
			m["dialogue"] = dlg
		}
		return m
	}
	on := &voiceBody{Enabled: ptr(true)}
	off := &voiceBody{Enabled: ptr(false)}
	cases := []struct {
		name    string
		weights map[string]*float64
		voice   *voiceBody
		want    settings.Weights
	}{
		{
			// Регрессия (finding: POST /lessons replaces teacher-supplied weights): преподаватель
			// явно выставил разговору 0% при включённом голосе — сервер не подмешивает 25%.
			name: "голос вкл, явный dialogue=0 — уважаем", weights: w(0.5, 0.2, 0.1, 0.2, ptr(0.0)), voice: on,
			want: settings.Weights{Fields: 0.5, Semantic: 0.2, Grammar: 0.1, Timing: 0.2, Dialogue: 0},
		},
		{
			name: "голос вкл, явные веса формы", weights: w(0.35, 0.2, 0.1, 0.1, ptr(0.25)), voice: on,
			want: settings.Weights{Fields: 0.35, Semantic: 0.2, Grammar: 0.1, Timing: 0.1, Dialogue: 0.25},
		},
		{
			name: "голос вкл, dialogue не передан — доля по умолчанию", weights: w(0.5, 0.25, 0.1, 0.15, nil), voice: on,
			want: settings.Weights{Fields: 0.375, Semantic: 0.1875, Grammar: 0.075, Timing: 0.1125, Dialogue: 0.25},
		},
		{
			name: "голос вкл, dialogue: null — как не передан", voice: on,
			weights: map[string]*float64{"fields": ptr(0.5), "semantic": ptr(0.25), "grammar": ptr(0.1), "timing": ptr(0.15), "dialogue": nil},
			want:    settings.Weights{Fields: 0.375, Semantic: 0.1875, Grammar: 0.075, Timing: 0.1125, Dialogue: 0.25},
		},
		{
			name: "голос вкл, веса не переданы — доля по умолчанию", weights: nil, voice: on,
			want: settings.Weights{Fields: 0.375, Semantic: 0.1875, Grammar: 0.075, Timing: 0.1125, Dialogue: 0.25},
		},
		{
			name: "голос выкл, dialogue>0 — обнуляется, остальные перенормируются", weights: w(0.35, 0.2, 0.1, 0.1, ptr(0.25)), voice: off,
			want: settings.Weights{Fields: 0.4667, Semantic: 0.2667, Grammar: 0.1333, Timing: 0.1333},
		},
		{
			name: "голос выкл, веса в процентах — нормировка к 1", weights: w(50, 25, 10, 15, ptr(0.0)), voice: off,
			want: settings.Weights{Fields: 0.5, Semantic: 0.25, Grammar: 0.1, Timing: 0.15},
		},
		{
			name: "частичные веса поверх платформенных", weights: map[string]*float64{"grammar": ptr(0.35)}, voice: nil,
			want: settings.Weights{Fields: 0.4, Semantic: 0.2, Grammar: 0.28, Timing: 0.12},
		},
		{
			name: "голос вкл, только разговор", weights: w(0, 0, 0, 0, ptr(1.0)), voice: on,
			want: settings.Weights{Dialogue: 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := validBody()
			b.Weights, b.Voice = tc.weights, tc.voice
			d, fields := parseCreate(&b, defaultsSnap())
			if len(fields) != 0 {
				t.Fatalf("fields: %v", fields)
			}
			got := d.Settings.Weights
			if !near(got.Fields, tc.want.Fields) || !near(got.Semantic, tc.want.Semantic) || !near(got.Grammar, tc.want.Grammar) ||
				!near(got.Timing, tc.want.Timing) || !near(got.Dialogue, tc.want.Dialogue) {
				t.Fatalf("weights %+v, want %+v", got, tc.want)
			}
			if !near(sumW(got), 1) {
				t.Errorf("сумма %v, want 1", sumW(got))
			}
		})
	}
}

func TestParseCreate_Voice(t *testing.T) {
	t.Parallel()
	b := validBody()
	b.Voice = &voiceBody{Enabled: ptr(true), Input: ptr(model.VoiceInputBoth), PushToTalk: ptr(false), MaxTurns: ptr(6), TtsEnabled: ptr(false)}
	b.CardsPerStudent = ptr(3)
	d, fields := parseCreate(&b, defaultsSnap())
	if len(fields) != 0 {
		t.Fatalf("fields: %v", fields)
	}
	v := d.Settings.Voice
	if !v.Enabled || v.Input != model.VoiceInputBoth || v.PushToTalk || v.MaxTurns != 6 || v.TTSEnabled {
		t.Errorf("voice %+v", v)
	}
	if d.Settings.Perspective != model.PerspectiveOperator112 || d.Settings.CardsPerStudent != 3 {
		t.Errorf("settings %+v", d.Settings)
	}
	// Частичный voice — поверх голоса платформы по умолчанию.
	b = validBody()
	b.Voice = &voiceBody{Enabled: ptr(true)}
	d, _ = parseCreate(&b, defaultsSnap())
	def := settings.Defaults().Voice
	if !d.Settings.Voice.Enabled || d.Settings.Voice.MaxTurns != def.MaxTurns || d.Settings.Voice.Input != def.Input {
		t.Errorf("частичный voice %+v", d.Settings.Voice)
	}
}

func TestParseIDs(t *testing.T) {
	t.Parallel()
	fields := map[string]string{}
	got := parseIDs("x", []string{" " + scA.String(), scA.String(), scB.String()}, 3, "пусто", fields)
	if len(fields) != 0 || len(got) != 2 || got[0] != scA || got[1] != scB {
		t.Fatalf("got %v fields %v", got, fields)
	}
	fields = map[string]string{}
	if got := parseIDs("x", []string{scA.String(), scB.String()}, 1, "пусто", fields); got != nil || fields["x"] == "" {
		t.Fatalf("лимит: got %v fields %v", got, fields)
	}
	fields = map[string]string{}
	parseIDs("x", nil, 1, "Выберите хотя бы один сценарий", fields)
	if fields["x"] != "Выберите хотя бы один сценарий" {
		t.Fatalf("пустой список: %v", fields)
	}
}

func TestWholePercents(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   settings.Weights
		want [5]int
	}{
		{settings.Weights{Fields: 0.5, Semantic: 0.25, Grammar: 0.1, Timing: 0.15}, [5]int{50, 25, 10, 15, 0}},
		// ужатие под голос: 37.5/18.75/7.5/11.25/25 — наибольшие остатки, сумма ровно 100
		{settings.Weights{Fields: 0.375, Semantic: 0.1875, Grammar: 0.075, Timing: 0.1125, Dialogue: 0.25}, [5]int{38, 19, 7, 11, 25}},
		// 0.29·100 = 28.999999999999996 — это 29, а не 28
		{settings.Weights{Fields: 0.29, Semantic: 0.29, Grammar: 0.14, Timing: 0.14, Dialogue: 0.14}, [5]int{29, 29, 14, 14, 14}},
		{settings.Weights{Fields: 1.0 / 3, Semantic: 1.0 / 3, Grammar: 1.0 / 3}, [5]int{34, 33, 33, 0, 0}},
		// вход не нормирован (сумма 0.5) — не «дотягиваем» до 100 из воздуха
		{settings.Weights{Fields: 0.25, Semantic: 0.25}, [5]int{25, 25, 0, 0, 0}},
		{settings.Weights{}, [5]int{}},
	}
	for _, tc := range cases {
		got := wholePercents(tc.in)
		pct := [5]int{
			int(math.Round(got.Fields * 100)), int(math.Round(got.Semantic * 100)), int(math.Round(got.Grammar * 100)),
			int(math.Round(got.Timing * 100)), int(math.Round(got.Dialogue * 100)),
		}
		if pct != tc.want {
			t.Errorf("wholePercents(%+v) = %v, want %v", tc.in, pct, tc.want)
		}
	}
}

func TestNewLessonSettings(t *testing.T) {
	t.Parallel()
	snap := defaultsSnap()
	ls := newLessonSettings(snap)
	if ls.Voice.Enabled || ls.Weights.Dialogue != 0 || !near(ls.Weights.Sum(), 1) {
		t.Errorf("без голоса по умолчанию: %+v", ls)
	}
	// Голос по умолчанию включён — форма сразу показывает долю разговора и 100% ровно.
	snap.Voice.Enabled = true
	ls = newLessonSettings(snap)
	if !ls.Voice.Enabled || !near(ls.Weights.Dialogue, 0.25) || !near(ls.Weights.Sum(), 1) {
		t.Errorf("с голосом: %+v", ls.Weights)
	}
	for _, v := range []float64{ls.Weights.Fields, ls.Weights.Semantic, ls.Weights.Grammar, ls.Weights.Timing, ls.Weights.Dialogue} {
		if p := v * 100; math.Abs(p-math.Round(p)) > 1e-9 {
			t.Errorf("вес %v — не целый процент", v)
		}
	}
}

// ---------------------------------------------------------------- checkRefs

func goodScenario(id uuid.UUID, title string) scenarioCheck {
	return scenarioCheck{ID: id, Title: title, Status: core.ScenarioValidated, Mode: core.ModeBoth, HasEtalon: true,
		HasBrief: true, Facts: 3, Checklist: 2}
}

func goodStudent(id uuid.UUID, name string) userCheck {
	return userCheck{ID: id, Role: string(core.RoleStudent), Status: "active", Name: name, Visible: true}
}

func TestCheckRefs(t *testing.T) {
	t.Parallel()
	base := func() (lessonDraft, map[uuid.UUID]scenarioCheck, map[uuid.UUID]userCheck) {
		d := lessonDraft{Mode: core.ModeCards, ScenarioIDs: []uuid.UUID{scA, scB}, ParticipantIDs: []uuid.UUID{stA, stB}}
		d.Settings = model.DefaultLessonSettings(nil)
		scen := map[uuid.UUID]scenarioCheck{scA: goodScenario(scA, "Пожар"), scB: goodScenario(scB, "Газ")}
		users := map[uuid.UUID]userCheck{stA: goodStudent(stA, "Иванов И. И."), stB: goodStudent(stB, "Петров П. П.")}
		return d, scen, users
	}
	cases := []struct {
		name     string
		mut      func(d *lessonDraft, s map[uuid.UUID]scenarioCheck, u map[uuid.UUID]userCheck)
		status   int
		msgHas   string
		msgNot   string
		detailID uuid.UUID
	}{
		{name: "всё хорошо", mut: func(*lessonDraft, map[uuid.UUID]scenarioCheck, map[uuid.UUID]userCheck) {}},
		{name: "режим сценария = режим занятия", mut: func(_ *lessonDraft, s map[uuid.UUID]scenarioCheck, _ map[uuid.UUID]userCheck) {
			c := s[scA]
			c.Mode = core.ModeCards
			s[scA] = c
		}},
		{name: "сценарий не найден", status: 400, detailID: scB,
			mut: func(_ *lessonDraft, s map[uuid.UUID]scenarioCheck, _ map[uuid.UUID]userCheck) { delete(s, scB) }},
		{name: "сценарий не подтверждён", status: 400, msgHas: "«Газ»", detailID: scB,
			mut: func(_ *lessonDraft, s map[uuid.UUID]scenarioCheck, _ map[uuid.UUID]userCheck) {
				c := s[scB]
				c.Status = "generated"
				s[scB] = c
			}},
		{name: "сценарий в архиве", status: 400, msgHas: "«Пожар»", detailID: scA,
			mut: func(_ *lessonDraft, s map[uuid.UUID]scenarioCheck, _ map[uuid.UUID]userCheck) {
				c := s[scA]
				c.Archived = true
				s[scA] = c
			}},
		{name: "без эталона", status: 400, detailID: scA,
			mut: func(_ *lessonDraft, s map[uuid.UUID]scenarioCheck, _ map[uuid.UUID]userCheck) {
				c := s[scA]
				c.HasEtalon = false
				s[scA] = c
			}},
		{name: "режим не совпадает", status: 400, msgHas: "режима", detailID: scB,
			mut: func(_ *lessonDraft, s map[uuid.UUID]scenarioCheck, _ map[uuid.UUID]userCheck) {
				c := s[scB]
				c.Mode = core.ModeCardActions
				s[scB] = c
			}},
		{name: "голос: нет чек-листа — 422", status: 422, msgHas: "Газ", detailID: scB,
			mut: func(d *lessonDraft, s map[uuid.UUID]scenarioCheck, _ map[uuid.UUID]userCheck) {
				d.Settings.Voice.Enabled = true
				c := s[scB]
				c.Checklist = 0
				s[scB] = c
			}},
		{name: "голос выкл — бриф не нужен", mut: func(_ *lessonDraft, s map[uuid.UUID]scenarioCheck, _ map[uuid.UUID]userCheck) {
			c := s[scB]
			c.HasBrief, c.Facts, c.Checklist = false, 0, 0
			s[scB] = c
		}},
		{name: "участник не найден", status: 400, detailID: stB,
			mut: func(_ *lessonDraft, _ map[uuid.UUID]scenarioCheck, u map[uuid.UUID]userCheck) { delete(u, stB) }},
		{
			// Регрессия (finding: teacher can add any student): чужой обучающийся — как
			// несуществующий, и его ФИО/статус в ответ не попадают.
			name: "участник не виден преподавателю", status: 400, msgNot: "Петров", detailID: stB,
			mut: func(_ *lessonDraft, _ map[uuid.UUID]scenarioCheck, u map[uuid.UUID]userCheck) {
				c := u[stB]
				c.Visible, c.Status = false, "blocked"
				u[stB] = c
			},
		},
		{name: "участник не обучающийся", status: 400, detailID: stA,
			mut: func(_ *lessonDraft, _ map[uuid.UUID]scenarioCheck, u map[uuid.UUID]userCheck) {
				c := u[stA]
				c.Role = string(core.RoleTeacher)
				u[stA] = c
			}},
		{name: "заблокированный (свой) — с ФИО", status: 400, msgHas: "Петров П. П.",
			mut: func(_ *lessonDraft, _ map[uuid.UUID]scenarioCheck, u map[uuid.UUID]userCheck) {
				c := u[stB]
				c.Status = "blocked"
				u[stB] = c
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, s, u := base()
			tc.mut(&d, s, u)
			herr := checkRefs(&d, s, u)
			if tc.status == 0 {
				if herr != nil {
					t.Fatalf("unexpected %v", herr)
				}
				return
			}
			if herr == nil {
				t.Fatalf("want %d", tc.status)
			}
			if herr.Status != tc.status || herr.Code != httpx.CodeValidation {
				t.Fatalf("got %d %s, want %d validation", herr.Status, herr.Code, tc.status)
			}
			all := herr.Message + " " + strings.Join(fieldMsgs(herr), " ")
			if tc.msgHas != "" && !strings.Contains(all, tc.msgHas) {
				t.Errorf("сообщение %q без %q", all, tc.msgHas)
			}
			if tc.msgNot != "" && strings.Contains(all, tc.msgNot) {
				t.Errorf("сообщение %q раскрывает %q", all, tc.msgNot)
			}
			if tc.detailID != uuid.Nil && !detailsHave(herr, tc.detailID.String()) {
				t.Errorf("details %v без %s", herr.Details, tc.detailID)
			}
		})
	}
}

func fieldMsgs(e *httpx.Error) []string {
	var out []string
	if f, ok := e.Details["fields"].(map[string]string); ok {
		for _, v := range f {
			out = append(out, v)
		}
	}
	return out
}

func detailsHave(e *httpx.Error, id string) bool {
	for _, v := range e.Details {
		if ids, ok := v.([]string); ok {
			for _, x := range ids {
				if x == id {
					return true
				}
			}
		}
	}
	return false
}

func TestVoiceReady(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		c    scenarioCheck
		want bool
	}{
		{scenarioCheck{HasBrief: true, Facts: 1, Checklist: 1}, true},
		{scenarioCheck{HasBrief: false, Facts: 1, Checklist: 1}, false},
		{scenarioCheck{HasBrief: true, Facts: 0, Checklist: 1}, false},
		{scenarioCheck{HasBrief: true, Facts: 1, Checklist: 0}, false},
	} {
		if got := voiceReady(&tc.c); got != tc.want {
			t.Errorf("voiceReady(%+v) = %v", tc.c, got)
		}
	}
}

func TestDeref(t *testing.T) {
	t.Parallel()
	if deref(nil) != "" || deref(ptr("  cards ")) != "cards" {
		t.Fatal("deref")
	}
}

func TestErrLessonNotFound(t *testing.T) {
	t.Parallel()
	if e := errLessonNotFound(); e.Status != http.StatusNotFound || e.Code != httpx.CodeNotFound || e.Message == "" {
		t.Fatalf("%+v", e)
	}
}

// Ракурс «Диспетчер ДДС» (v1.3): только «действия с карточками», голос выключается всегда
// (вес разговора уходит в 0, остальные слои перенормированы к 1).
func TestParseCreate_DDS(t *testing.T) {
	t.Parallel()
	b := validBody()
	b.Perspective = ptr(model.PerspectiveDDS)
	if _, fields := parseCreate(&b, defaultsSnap()); fields["mode"] == "" {
		t.Errorf("dds + cards должно быть ошибкой по mode: %v", fields)
	}

	b.Mode = ptr(core.ModeCardActions)
	b.Voice = &voiceBody{Enabled: ptr(true)}
	d, fields := parseCreate(&b, defaultsSnap())
	if len(fields) != 0 {
		t.Fatalf("fields: %v", fields)
	}
	if !d.Settings.IsDDS() || d.Settings.Voice.Enabled {
		t.Errorf("settings %+v", d.Settings)
	}
	w := d.Settings.Weights
	if w.Dialogue != 0 {
		t.Errorf("вес разговора в ракурсе dds: %+v", w)
	}
	if sum := w.Fields + w.Semantic + w.Grammar + w.Timing; sum < 0.999 || sum > 1.001 {
		t.Errorf("веса не нормированы: %+v (сумма %v)", w, sum)
	}
}
