package reaction

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
)

var allStatuses = []public.ReactionStatus{
	StatusAdded, StatusReceived, StatusAccepted, StatusNotAccepted, StatusStarted,
	StatusArrived, StatusWorking, StatusDone, StatusRefused,
}

func statuses(l []public.AllowedTransition) []public.ReactionStatus {
	out := make([]public.ReactionStatus, 0, len(l))
	for _, t := range l {
		out = append(out, t.Status)
	}
	return out
}

func TestConstantsMatchContract(t *testing.T) {
	t.Parallel()
	want := map[public.ReactionStatus]string{
		StatusAdded:       "Добавлена",
		StatusReceived:    "Получена службой",
		StatusAccepted:    "Принята",
		StatusNotAccepted: "Не принята",
		StatusStarted:     "Начало реагирования",
		StatusArrived:     "Прибытие",
		StatusWorking:     "Проведение работ",
		StatusDone:        "Работы завершены",
		StatusRefused:     "Отказ от выполнения работ",
	}
	for s, w := range want {
		if string(s) != w {
			t.Errorf("status %q, want %q", s, w)
		}
		if !s.Valid() || !ValidStatus(w) {
			t.Errorf("%q must be a valid contract status", w)
		}
	}
	if SourceAuto != "auto" || SourceManual != "manual" || SourceVIS != "vis" {
		t.Errorf("sources: %q %q %q", SourceAuto, SourceManual, SourceVIS)
	}
	if OperatorSystem != "система" {
		t.Errorf("OperatorSystem = %q", OperatorSystem)
	}
	// Каждый статус контракта присутствует в графе (иначе AllowedNext молча вернёт []).
	for _, s := range allStatuses {
		if _, ok := transitions[s]; !ok {
			t.Errorf("status %q missing in transitions", s)
		}
	}
}

func TestValidStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"Принята", true},
		{"Работы завершены", true},
		{"принята", false}, // регистр важен: это значение enum
		{"Принята ", false},
		{"", false},
		{"Accepted", false},
		{"Работы завершены: Завершение работ без бригады", false}, // подпись, а не статус
	} {
		if got := ValidStatus(tc.in); got != tc.want {
			t.Errorf("ValidStatus(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestSystemAndTerminal(t *testing.T) {
	t.Parallel()
	for _, s := range allStatuses {
		wantSystem := s == StatusAdded || s == StatusReceived
		wantTerminal := s == StatusDone || s == StatusRefused
		if System(s) != wantSystem {
			t.Errorf("System(%q) = %v", s, !wantSystem)
		}
		if IsTerminal(s) != wantTerminal {
			t.Errorf("IsTerminal(%q) = %v", s, !wantTerminal)
		}
	}
	if System("") || IsTerminal("") {
		t.Error("empty status is neither system nor terminal")
	}
}

func TestAllowedNext(t *testing.T) {
	t.Parallel()
	type want = []public.ReactionStatus
	tests := []struct {
		current public.ReactionStatus
		code    string
		want    want
	}{
		// «Добавлена» -> только системный «Получена службой», в список диспетчера не попадает
		{StatusAdded, "101", want{}},
		{StatusReceived, "101", want{StatusAccepted, StatusNotAccepted}},
		{StatusReceived, "103", want{StatusAccepted}}, // 103 не ставит «Не принята»
		{StatusAccepted, "101", want{StatusStarted, StatusArrived, StatusWorking, StatusDone, StatusRefused}},
		{StatusAccepted, "103", want{StatusStarted, StatusArrived, StatusWorking, StatusDone}},
		{StatusNotAccepted, "101", want{StatusAccepted}},
		{StatusStarted, "102", want{StatusArrived, StatusWorking, StatusDone, StatusRefused}},
		{StatusArrived, "zhkh", want{StatusWorking, StatusDone, StatusRefused}},
		{StatusWorking, "104", want{StatusDone, StatusRefused}},
		{StatusWorking, "103", want{StatusDone}},
		{StatusDone, "101", want{}},
		{StatusRefused, "101", want{}},
		{"неизвестный", "101", want{}},
		{"", "", want{}},
		{StatusReceived, "", want{StatusAccepted, StatusNotAccepted}}, // пустой код — обычная служба
	}
	for _, tc := range tests {
		got := AllowedNext(tc.current, tc.code)
		if got == nil {
			t.Errorf("AllowedNext(%q, %q) = nil, want non-nil", tc.current, tc.code)
			continue
		}
		if !slices.Equal(statuses(got), tc.want) {
			t.Errorf("AllowedNext(%q, %q) = %v, want %v", tc.current, tc.code, statuses(got), tc.want)
		}
		for _, tr := range got {
			if System(tr.Status) {
				t.Errorf("system status %q offered to the dispatcher", tr.Status)
			}
			wantComment := tr.Status == StatusNotAccepted || tr.Status == StatusRefused || tr.Status == StatusDone
			if tr.CommentRequired != wantComment {
				t.Errorf("%q commentRequired = %v", tr.Status, tr.CommentRequired)
			}
			if tr.SquadNumberRequired {
				t.Errorf("%q squadNumberRequired must be false in the training contour", tr.Status)
			}
			wantLabel := string(tr.Status)
			if tc.code == "103" && tr.Status == StatusDone {
				wantLabel = "Работы завершены: Завершение работ без бригады"
			}
			if tr.Label != wantLabel {
				t.Errorf("AllowedNext(%q, %q): label %q, want %q", tc.current, tc.code, tr.Label, wantLabel)
			}
		}
	}
}

func TestAllowedNextReturnsCopy(t *testing.T) {
	t.Parallel()
	a := AllowedNext(StatusReceived, "101")
	a[0].Status = StatusRefused
	a[0].Label = "испорчено"
	b := AllowedNext(StatusReceived, "101")
	if b[0].Status != StatusAccepted || b[0].Label != "Принята" {
		t.Fatalf("shared table was mutated through the returned slice: %+v", b[0])
	}
}

func TestAllowedNextJSONIsArray(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(AllowedNext(StatusDone, "101"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "[]" {
		t.Fatalf("terminal allowedNext JSON = %s, want []", b)
	}
}

func TestCanRemove(t *testing.T) {
	t.Parallel()
	if CanRemove(nil) {
		t.Error("CanRemove(nil) = true")
	}
	for _, s := range allStatuses {
		as := &public.AssignedService{CurrentStatus: s}
		want := s == StatusAdded || s == StatusReceived
		if CanRemove(as) != want {
			t.Errorf("CanRemove(%q) = %v, want %v", s, !want, want)
		}
	}
}

func svc(code string) core.ServiceInfo {
	return core.ServiceInfo{ID: "id-" + code, Code: code, Name: "Служба " + code + " полностью", ShortName: "Служба " + code, Kind: "emergency"}
}

func TestNewAssigned(t *testing.T) {
	t.Parallel()
	msk := time.FixedZone("MSK", 3*3600)
	now := time.Date(2026, 9, 24, 15, 4, 5, 0, msk)

	as := NewAssigned(svc("101"), SourceAuto, true, "тип происшествия: пожар", now)
	if as.ServiceId != "id-101" || as.Code != "101" || as.Name != "Служба 101 полностью" || as.ShortName != "Служба 101" {
		t.Errorf("service fields: %+v", as)
	}
	if !as.IsPrimary || as.Source != public.AssignedServiceSourceAuto {
		t.Errorf("isPrimary/source: %v %q", as.IsPrimary, as.Source)
	}
	if as.Reason == nil || *as.Reason != "тип происшествия: пожар" {
		t.Errorf("reason = %v", as.Reason)
	}
	if as.CurrentStatus != StatusReceived || !as.Editable {
		t.Errorf("current %q editable %v", as.CurrentStatus, as.Editable)
	}
	if !as.CurrentStatusAt.Equal(now) || as.CurrentStatusAt.Location() != time.UTC {
		t.Errorf("currentStatusAt %v must be now in UTC", as.CurrentStatusAt)
	}
	if len(as.History) != 2 {
		t.Fatalf("history len %d, want 2", len(as.History))
	}
	for i, st := range []public.ReactionStatus{StatusAdded, StatusReceived} {
		h := as.History[i]
		if h.Status != st || h.Operator != OperatorSystem || !h.At.Equal(now) || h.Comment != nil || h.SquadNumber != nil {
			t.Errorf("history[%d] = %+v", i, h)
		}
	}
	if got := statuses(as.AllowedNext); !slices.Equal(got, []public.ReactionStatus{StatusAccepted, StatusNotAccepted}) {
		t.Errorf("allowedNext %v", got)
	}

	// 103: без «Не принята»; пустая причина -> reason отсутствует в JSON
	as103 := NewAssigned(svc("103"), SourceManual, false, "", now)
	if as103.Reason != nil {
		t.Errorf("empty reason must be nil, got %q", *as103.Reason)
	}
	if got := statuses(as103.AllowedNext); !slices.Equal(got, []public.ReactionStatus{StatusAccepted}) {
		t.Errorf("103 allowedNext %v", got)
	}
	b, err := json.Marshal(as103)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"serviceId", "code", "name", "shortName", "isPrimary", "source", "currentStatus",
		"currentStatusAt", "history", "allowedNext", "editable"} {
		if _, ok := m[k]; !ok {
			t.Errorf("JSON misses required field %q: %s", k, b)
		}
	}
	if _, ok := m["reason"]; ok {
		t.Errorf("reason must be omitted when empty: %s", b)
	}
	if m["source"] != "manual" {
		t.Errorf("source = %v", m["source"])
	}
}

func TestTransitionHappyPath(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	as := NewAssigned(svc("101"), SourceAuto, true, "", t0)

	steps := []struct {
		to      public.ReactionStatus
		squad   string
		comment string
	}{
		{StatusAccepted, "", ""},
		{StatusStarted, "  12-А ", ""},
		{StatusArrived, "", "  на месте  "},
		{StatusWorking, "", ""},
		{StatusDone, "", "Пожар ликвидирован"},
	}
	for i, st := range steps {
		now := t0.Add(time.Duration(i+1) * time.Minute)
		if err := Transition(&as, st.to, st.squad, st.comment, "оп. 227", now); err != nil {
			t.Fatalf("step %d -> %q: %v", i, st.to, err)
		}
		if as.CurrentStatus != st.to || !as.CurrentStatusAt.Equal(now) {
			t.Fatalf("step %d: current %q at %v", i, as.CurrentStatus, as.CurrentStatusAt)
		}
		last := as.History[len(as.History)-1]
		if last.Status != st.to || last.Operator != "оп. 227" || !last.At.Equal(now) {
			t.Fatalf("step %d: history entry %+v", i, last)
		}
		if sq := strings.TrimSpace(st.squad); sq != "" {
			if last.SquadNumber == nil || *last.SquadNumber != sq {
				t.Errorf("step %d: squad %v, want trimmed %q", i, last.SquadNumber, sq)
			}
		} else if last.SquadNumber != nil {
			t.Errorf("step %d: squad must be nil, got %q", i, *last.SquadNumber)
		}
		if c := strings.TrimSpace(st.comment); c != "" {
			if last.Comment == nil || *last.Comment != c {
				t.Errorf("step %d: comment %v, want trimmed %q", i, last.Comment, c)
			}
		} else if last.Comment != nil {
			t.Errorf("step %d: comment must be nil, got %q", i, *last.Comment)
		}
	}
	if len(as.History) != 2+len(steps) {
		t.Errorf("history len %d", len(as.History))
	}
	if as.Editable || len(as.AllowedNext) != 0 || as.AllowedNext == nil {
		t.Errorf("terminal: editable %v allowedNext %#v", as.Editable, as.AllowedNext)
	}
	// из терминального — никуда
	err := Transition(&as, StatusRefused, "", "поздно", "оп. 227", t0.Add(time.Hour))
	if !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("transition from terminal: %v", err)
	}
}

func TestTransitionErrors(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		code    string
		from    public.ReactionStatus
		to      public.ReactionStatus
		comment string
		kind    error
		msg     string
	}{
		{"skip accept", "101", StatusReceived, StatusStarted, "", ErrNotAllowed,
			"Переход в статус «Начало реагирования» сейчас недопустим"},
		{"system status", "101", StatusAdded, StatusReceived, "", ErrNotAllowed,
			"Переход в статус «Получена службой» сейчас недопустим"},
		{"backwards", "101", StatusArrived, StatusStarted, "", ErrNotAllowed,
			"Переход в статус «Начало реагирования» сейчас недопустим"},
		{"same status", "101", StatusAccepted, StatusAccepted, "", ErrNotAllowed,
			"Переход в статус «Принята» сейчас недопустим"},
		{"103 not accepted", "103", StatusReceived, StatusNotAccepted, "причина", ErrNotAllowed,
			"Переход в статус «Не принята» сейчас недопустим"},
		{"103 refused", "103", StatusWorking, StatusRefused, "причина", ErrNotAllowed,
			"Переход в статус «Отказ от выполнения работ» сейчас недопустим"},
		{"unknown target", "101", StatusReceived, "Уехали", "", ErrNotAllowed,
			"Переход в статус «Уехали» сейчас недопустим"},
		{"not accepted needs comment", "101", StatusReceived, StatusNotAccepted, "", ErrCommentRequired,
			"Для статуса «Не принята» комментарий обязателен"},
		{"whitespace comment", "102", StatusAccepted, StatusRefused, " \t\n ", ErrCommentRequired,
			"Для статуса «Отказ от выполнения работ» комментарий обязателен"},
		{"done needs comment", "103", StatusAccepted, StatusDone, "", ErrCommentRequired,
			"Для статуса «Работы завершены» комментарий обязателен"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			as := public.AssignedService{Code: tc.code, CurrentStatus: tc.from, CurrentStatusAt: now.Add(-time.Hour),
				History:  []public.ReactionStatusEntry{{Status: tc.from, At: now.Add(-time.Hour), Operator: "система"}},
				Editable: true}
			before, _ := json.Marshal(as)
			err := Transition(&as, tc.to, "", tc.comment, "оп. 1", now)
			if err == nil {
				t.Fatal("expected error")
			}
			if !errors.Is(err, tc.kind) {
				t.Errorf("errors.Is(%v, %v) = false", err, tc.kind)
			}
			var te *TransitionError
			if !errors.As(err, &te) {
				t.Fatalf("error %T is not *TransitionError", err)
			}
			if te.Status != tc.to || te.Message != tc.msg || err.Error() != tc.msg {
				t.Errorf("TransitionError %+v, want message %q", te, tc.msg)
			}
			after, _ := json.Marshal(as)
			if string(before) != string(after) {
				t.Errorf("service changed on a rejected transition:\n%s\n%s", before, after)
			}
		})
	}
}

func TestTransitionIgnoresClientAllowedNext(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	// Клиент подсунул allowedNext с недопустимым переходом — сервер считает граф сам.
	as := public.AssignedService{Code: "101", CurrentStatus: StatusReceived,
		AllowedNext: []public.AllowedTransition{{Status: StatusDone, Label: "Работы завершены"}}}
	if err := Transition(&as, StatusDone, "", "готово", "оп. 1", now); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("client allowedNext was trusted: %v", err)
	}
	// Статус 103 решает код службы, а не подпись в запросе.
	as = public.AssignedService{Code: "103", CurrentStatus: StatusReceived}
	if err := Transition(&as, StatusNotAccepted, "", "нет бригад", "оп. 1", now); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("103 «Не принята»: %v", err)
	}
	// nil history — переход всё равно записывается
	as = public.AssignedService{Code: "101", CurrentStatus: StatusNotAccepted}
	if err := Transition(&as, StatusAccepted, "", "", "Иванов И. И.", now); err != nil {
		t.Fatal(err)
	}
	if len(as.History) != 1 || as.History[0].Operator != "Иванов И. И." {
		t.Fatalf("history %+v", as.History)
	}
	if got := statuses(as.AllowedNext); !slices.Equal(got, []public.ReactionStatus{StatusStarted, StatusArrived, StatusWorking, StatusDone, StatusRefused}) {
		t.Errorf("allowedNext after «Принята»: %v", got)
	}
}

func TestNormalize(t *testing.T) {
	t.Parallel()
	as := public.AssignedService{Code: "103", CurrentStatus: StatusReceived, Editable: false,
		AllowedNext: []public.AllowedTransition{{Status: StatusNotAccepted}, {Status: StatusDone}}}
	Normalize(&as)
	if as.History == nil || len(as.History) != 0 {
		t.Errorf("history must become [] not nil: %#v", as.History)
	}
	if !as.Editable {
		t.Error("non-terminal service must be editable")
	}
	if got := statuses(as.AllowedNext); !slices.Equal(got, []public.ReactionStatus{StatusAccepted}) {
		t.Errorf("allowedNext %v", got)
	}

	done := public.AssignedService{Code: "101", CurrentStatus: StatusRefused, Editable: true,
		History:     []public.ReactionStatusEntry{{Status: StatusRefused}},
		AllowedNext: []public.AllowedTransition{{Status: StatusAccepted}}}
	Normalize(&done)
	if done.Editable || done.AllowedNext == nil || len(done.AllowedNext) != 0 || len(done.History) != 1 {
		t.Errorf("terminal normalize: %+v", done)
	}
}

// TestMirrorsFrontendGraph — граф и правила службы 103 совпадают с фронтовым
// reactionTransitions.ts (пакет объявлен его точным зеркалом). Нет файла — пропуск.
func TestMirrorsFrontendGraph(t *testing.T) {
	t.Parallel()
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "..", "..", "..", "frontend", "src", "shared", "mocks", "reactionTransitions.ts")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("frontend fixture not available: %v", err)
	}
	s := string(src)
	start := strings.Index(s, "const TRANSITIONS")
	if start < 0 {
		t.Fatal("TRANSITIONS not found in reactionTransitions.ts")
	}
	end := strings.Index(s[start:], "};")
	block := s[start : start+end]
	entry := regexp.MustCompile(`'([^']+)':\s*\[([^\]]*)\]`)
	item := regexp.MustCompile(`'([^']+)'`)
	matches := entry.FindAllStringSubmatch(block, -1)
	if len(matches) != len(allStatuses) {
		t.Fatalf("parsed %d TRANSITIONS entries, want %d", len(matches), len(allStatuses))
	}
	for _, m := range matches {
		from := public.ReactionStatus(m[1])
		var want []public.ReactionStatus
		for _, it := range item.FindAllStringSubmatch(m[2], -1) {
			want = append(want, public.ReactionStatus(it[1]))
		}
		got, ok := transitions[from]
		if !ok {
			t.Errorf("status %q from the frontend is missing", from)
			continue
		}
		if !slices.Equal(got, want) && !(len(got) == 0 && len(want) == 0) {
			t.Errorf("transitions[%q] = %v, frontend %v", from, got, want)
		}
	}
	if !strings.Contains(s, `'103': ['Не принята', 'Отказ от выполнения работ']`) {
		t.Error("frontend FORBIDDEN_BY_SERVICE for 103 changed — update reaction.forbiddenFor")
	}
	if !strings.Contains(s, "Работы завершены: Завершение работ без бригады") {
		t.Error("frontend 103 label changed — update reaction.labelFor")
	}
}
