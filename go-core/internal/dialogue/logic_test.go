package dialogue

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/settings"
	"lct/gocore/internal/store"
)

// ---------------------------------------------------------------- время реплики оператора

// Регрессия (ревью: «at_ms оператора смешивает часы браузера и сервера»): clientRecordedAt
// принимается только внутри окна, которое сервер знает сам (после реплики заявителя, на
// которую отвечают, и до «приход запроса − длительность записи»), иначе — серверная оценка.
func TestOperatorTime(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC) // приход запроса
	now := start.Add(4 * time.Second)                      // после ai-service
	prevCaller := start.Add(-10 * time.Second)             // ответ заявителя, на который отвечают
	at := func(d time.Duration) *time.Time { v := start.Add(d); return &v }

	cases := []struct {
		name       string
		recordedAt *time.Time
		spokenMs   int
		notBefore  time.Time
		want       time.Time
	}{
		{"текст: приход запроса", nil, 0, prevCaller, start},
		{"аудио без clientRecordedAt: приход минус длительность", nil, 3000, prevCaller, start.Add(-3 * time.Second)},
		{"часы синхронны: берём клиентское время", at(-5 * time.Second), 3000, prevCaller, start.Add(-5 * time.Second)},
		{"проверка расшифровки перед отправкой: клиентское время раньше оценки", at(-9 * time.Second), 3000, prevCaller, start.Add(-9 * time.Second)},
		// Часы студента спешат на 5 с: запись реально началась в start-5s, браузер говорит start.
		// Старое правило (≤ now+2s) приняло бы start — пауза завышена на 5 с.
		{"часы спешат: вне окна — серверная оценка", at(0), 3000, prevCaller, start.Add(-3 * time.Second)},
		{"часы спешат сильно (в будущем)", at(time.Minute), 3000, prevCaller, start.Add(-3 * time.Second)},
		// Часы отстают на 30 с: браузер говорит start-35s — раньше реплики заявителя.
		// Старое правило (не старше 2 мин, не раньше принятия вызова) приняло бы — пауза отрицательная.
		{"часы отстают: раньше реплики заявителя — серверная оценка", at(-35 * time.Second), 3000, prevCaller, start.Add(-3 * time.Second)},
		{"в пределах допуска раньше реплики заявителя — прижимаем к ней", at(-11 * time.Second), 3000, prevCaller, prevCaller},
		{"в пределах допуска позже оценки — берём клиентское", at(-2 * time.Second), 3000, prevCaller, start.Add(-2 * time.Second)},
		// Повтор после «заявитель занят» спустя 2.5 минуты: запись старая, но честная.
		{"повтор после паузы > 2 мин", at(-150 * time.Second), 3000, start.Add(-160 * time.Second), start.Add(-150 * time.Second)},
		{"длительность больше, чем прошло с ответа — не раньше ответа", nil, 60000, prevCaller, prevCaller},
		{"отрицательная длительность игнорируется", nil, -500, prevCaller, start},
		{"без нижней границы", nil, 1000, time.Time{}, start.Add(-time.Second)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := operatorTime(c.recordedAt, start, c.notBefore, c.spokenMs, now)
			if !got.Equal(c.want) {
				t.Fatalf("operatorTime = %s, want %s", got.Format(time.RFC3339Nano), c.want.Format(time.RFC3339Nano))
			}
			if got.Location() != time.UTC {
				t.Fatalf("not UTC: %v", got.Location())
			}
			if got.After(now) {
				t.Fatalf("after now")
			}
		})
	}

	t.Run("не позже now и микросекунды", func(t *testing.T) {
		t.Parallel()
		future := start.Add(10 * time.Second)
		got := operatorTime(nil, future, time.Time{}, 0, now)
		if !got.Equal(now) {
			t.Fatalf("got %s, want now", got)
		}
		odd := start.Add(123456789 * time.Nanosecond)
		got = operatorTime(nil, odd, time.Time{}, 0, now)
		if got.Nanosecond()%1000 != 0 {
			t.Fatalf("not truncated to µs: %d", got.Nanosecond())
		}
	})
}

func TestTranscriptAnsweredAt(t *testing.T) {
	t.Parallel()
	acc := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	rows := []store.DialogueTurnRow{
		{TurnNo: 0, Speaker: core.SpeakerCaller, At: acc},
		{TurnNo: 1, Speaker: core.SpeakerOperator, At: acc.Add(5 * time.Second)},
		{TurnNo: 1, Speaker: core.SpeakerCaller, At: acc.Add(8 * time.Second)},
	}
	tr := summarize(rows)
	if got := tr.answeredAt(1, &acc); !got.Equal(acc) {
		t.Fatalf("turn 1: %s", got)
	}
	if got := tr.answeredAt(2, &acc); !got.Equal(acc.Add(8 * time.Second)) {
		t.Fatalf("turn 2: %s", got)
	}
	// обмена 2 нет (гонка) — не раньше принятия вызова
	if got := tr.answeredAt(3, &acc); !got.Equal(acc) {
		t.Fatalf("turn 3: %s", got)
	}
	if got := tr.answeredAt(1, nil); !got.Equal(acc) {
		t.Fatalf("no acceptedAt: %s", got)
	}
	empty := summarize(nil)
	if got := empty.answeredAt(1, nil); !got.IsZero() {
		t.Fatalf("empty: %s", got)
	}
}

// ---------------------------------------------------------------- причина завершения

// Регрессия (ревью: «лимит брифа записывается как caller_hung_up»): ai-service закрывает
// разговор по лимиту брифа через should_end + end_reason="max_turns".
func TestEndReasonFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		caller   callerPart
		turnNo   int
		maxTurns int
		want     string
	}{
		{"разговор продолжается", callerPart{}, 3, 12, ""},
		{"заявитель положил трубку (end_conditions)", callerPart{shouldEnd: true, endReason: "оператор сообщил, что помощь направлена"}, 3, 12, core.EndCallerHungUp},
		{"should_end без причины", callerPart{shouldEnd: true}, 3, 12, core.EndCallerHungUp},
		{"лимит брифа от ai-service", callerPart{shouldEnd: true, endReason: "max_turns"}, 10, 10, core.EndMaxTurns},
		{"лимит брифа от ai-service раньше лимита занятия", callerPart{shouldEnd: true, endReason: "max_turns"}, 8, 12, core.EndMaxTurns},
		{"лимит занятия без should_end", callerPart{}, 12, 12, core.EndMaxTurns},
		{"лимит занятия превышен", callerPart{}, 13, 12, core.EndMaxTurns},
		{"заявитель сам положил трубку на последнем ходу", callerPart{shouldEnd: true, endReason: "помощь направлена"}, 12, 12, core.EndCallerHungUp},
		{"end_reason max_turns без should_end — не конец до лимита", callerPart{endReason: "max_turns"}, 3, 12, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := endReasonFor(c.caller, c.turnNo, c.maxTurns); got != c.want {
				t.Fatalf("endReasonFor = %q, want %q", got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------- транскрипт и лимиты

func TestSummarizeAndRevealed(t *testing.T) {
	t.Parallel()
	rows := []store.DialogueTurnRow{
		{TurnNo: 0, Speaker: core.SpeakerCaller, RevealedFactIDs: []string{"f_smoke"}},
		{TurnNo: 1, Speaker: core.SpeakerOperator, RevealedFactIDs: []string{"ignored"}},
		{TurnNo: 1, Speaker: core.SpeakerCaller, RevealedFactIDs: []string{"f_entrance", "", "f_smoke"}},
		{TurnNo: 2, Speaker: core.SpeakerOperator},
		{TurnNo: 2, Speaker: core.SpeakerCaller, RevealedFactIDs: []string{"f_floor"}},
	}
	tr := summarize(rows)
	if tr.operatorTurns != 2 || tr.maxOperator != 2 || tr.next() != 3 {
		t.Fatalf("summary: %+v next=%d", tr, tr.next())
	}
	if got, want := tr.revealed(), []string{"f_smoke", "f_entrance", "f_floor"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("revealed = %v, want %v", got, want)
	}
	op, caller := tr.exchange(2)
	if op == nil || caller == nil || op.Speaker != core.SpeakerOperator || caller.Speaker != core.SpeakerCaller {
		t.Fatalf("exchange(2) = %v %v", op, caller)
	}
	if op, caller := tr.exchange(7); op != nil || caller != nil {
		t.Fatal("exchange(7) must be empty")
	}

	empty := summarize(nil)
	if empty.next() != 1 {
		t.Fatalf("next on empty = %d", empty.next())
	}
	rv := empty.revealed()
	if rv == nil || len(rv) != 0 {
		t.Fatalf("revealed on empty must be [] not nil: %#v", rv)
	}
	b, _ := json.Marshal(rv)
	if string(b) != "[]" {
		t.Fatalf("json = %s", b)
	}
}

func TestMaxTurns(t *testing.T) {
	t.Parallel()
	cases := []struct{ lesson, brief, want int }{
		{12, 0, 12},
		{12, 10, 10},
		{8, 10, 8},
		{0, 0, 12},
		{0, 5, 5},
		{-3, 0, 12},
	}
	for _, c := range cases {
		a := &attemptCtx{BriefMaxTurns: c.brief}
		a.Settings.Voice.MaxTurns = c.lesson
		if got := a.maxTurns(); got != c.want {
			t.Errorf("maxTurns(lesson=%d, brief=%d) = %d, want %d", c.lesson, c.brief, got, c.want)
		}
	}
}

// ---------------------------------------------------------------- текст, пути, аудио

func TestCleanAndCheckText(t *testing.T) {
	t.Parallel()
	if got := cleanText("  Алло,\x00 вы меня слышите?\n "); got != "Алло, вы меня слышите?" {
		t.Fatalf("cleanText = %q", got)
	}
	if got := cleanText("Пожар\xff\xfe на кухне"); got != "Пожар на кухне" {
		t.Fatalf("invalid utf8: %q", got)
	}
	if got := cleanText("ёлка Ёж"); got != "ёлка Ёж" {
		t.Fatalf("ё: %q", got)
	}

	long := strings.Repeat("ж", maxTextRunes)
	cases := []struct {
		name string
		in   *string
		ok   bool
		want string
	}{
		{"nil", nil, false, ""},
		{"пусто", ptr(""), false, ""},
		{"пробелы и NUL", ptr(" \x00\t "), false, ""},
		{"обычная реплика", ptr("  Назовите адрес.  "), true, "Назовите адрес."},
		{"ровно 1000 символов кириллицей", &long, true, long},
		{"1001 символ", ptr(long + "ы"), false, ""},
	}
	for _, c := range cases {
		got, err := checkText(c.in)
		if c.ok != (err == nil) {
			t.Fatalf("%s: err=%v", c.name, err)
		}
		if err != nil {
			he := httpx.AsError(err)
			if he.Status != http.StatusBadRequest || he.Code != httpx.CodeValidation {
				t.Fatalf("%s: %+v", c.name, he)
			}
			continue
		}
		if got != c.want {
			t.Fatalf("%s: got %q", c.name, got)
		}
	}
}

func TestCleanMediaPath(t *testing.T) {
	t.Parallel()
	good := []string{"dialog/018f/3.wav", "ab/cdef.ogg", "x.wav", "дом/реплика.wav"}
	for _, p := range good {
		if got, ok := cleanMediaPath(p); !ok || got != p {
			t.Errorf("cleanMediaPath(%q) = %q, %v", p, got, ok)
		}
	}
	bad := []string{"", "/etc/passwd", "../x.wav", "a/../b.wav", "a/./b.wav", "a//b.wav", "a/", `a\b.wav`, "a\x00.wav", "..", ".", strings.Repeat("a", maxMediaPath+1)}
	for _, p := range bad {
		if _, ok := cleanMediaPath(p); ok {
			t.Errorf("cleanMediaPath(%q) must reject", p)
		}
	}
}

func TestAudioContentType(t *testing.T) {
	t.Parallel()
	wav := []byte("RIFF\x00\x00\x00\x00WAVEfmt ")
	ogg := []byte("OggS\x00\x02")
	ebml := []byte{0x1A, 0x45, 0xDF, 0xA3, 0x01}
	cases := []struct {
		header string
		head   []byte
		want   string
	}{
		{"audio/webm;codecs=opus", nil, "audio/webm"},
		{"AUDIO/WEBM", nil, "audio/webm"},
		{"video/webm", nil, "audio/webm"},
		{"audio/ogg; codecs=opus", nil, "audio/ogg"},
		{"application/ogg", nil, "audio/ogg"},
		{"audio/opus", nil, "audio/ogg"},
		{"audio/wav", nil, "audio/wav"},
		{"audio/x-wav", nil, "audio/wav"},
		{"audio/wave", nil, "audio/wav"},
		{"audio/mp4", ebml, ""},
		{"audio/mpeg", nil, ""},
		{"text/plain", wav, ""},
		{";;;", wav, ""},
		{"", wav, "audio/wav"},
		{"", ogg, "audio/ogg"},
		{"", ebml, "audio/webm"},
		{"application/octet-stream", ebml, "audio/webm"},
		{"", []byte("ID3\x04"), ""},
		{"", []byte("RIFF"), ""},
		{"", nil, ""},
	}
	for _, c := range cases {
		if got := audioContentType(c.header, c.head); got != c.want {
			t.Errorf("audioContentType(%q, %q) = %q, want %q", c.header, c.head, got, c.want)
		}
	}
	for ct, want := range map[string]string{"audio/ogg": "turn.ogg", "audio/wav": "turn.wav", "audio/webm": "turn.webm", "": "turn.webm"} {
		if got := audioFilename(ct); got != want {
			t.Errorf("audioFilename(%q) = %q", ct, got)
		}
	}
}

// ---------------------------------------------------------------- подсказки и факты

func TestSTTHints(t *testing.T) {
	t.Parallel()
	cs := demoScript(0)
	cs.Address.Landmark = ptr("напротив школы №14")
	cs.Dialogue.Facts = append(cs.Dialogue.Facts, model.DialogueFact{ID: "dup", Hints: []string{"ПОДЪЕЗД", "7", "x", " "}})
	hints := sttHints(&cs)
	want := []string{"улица Ленина", "Москва", "напротив школы №14", "Иванова Мария Петровна", "дым", "этаж", "подъезд"}
	if !reflect.DeepEqual(hints, want) {
		t.Fatalf("hints = %q, want %q", hints, want)
	}

	// лимит 30 и длина подсказки
	many := model.CallScript{Dialogue: &model.DialogueBrief{}}
	for i := range 50 {
		many.Dialogue.Facts = append(many.Dialogue.Facts, model.DialogueFact{ID: fmt.Sprint(i), Hints: []string{fmt.Sprintf("слово%d", i)}})
	}
	many.Dialogue.Facts[0].Hints = []string{strings.Repeat("я", maxHintRunes+1)}
	got := sttHints(&many)
	if len(got) != maxSTTHints {
		t.Fatalf("len = %d", len(got))
	}
	for _, h := range got {
		if len([]rune(h)) > maxHintRunes {
			t.Fatalf("too long hint %q", h)
		}
	}

	// без брифа и адреса — пусто, не nil
	empty := sttHints(&model.CallScript{})
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty = %#v", empty)
	}
}

func TestAllowedFacts(t *testing.T) {
	t.Parallel()
	cs := demoScript(0)
	cases := []struct {
		name string
		ids  []string
		want []string
	}{
		{"nil", nil, []string{}},
		{"разрешённые по порядку без дублей", []string{"f_entrance", " f_smoke ", "f_entrance"}, []string{"f_entrance", "f_smoke"}},
		{"never вычёркивается", []string{"f_secret", "f_smoke"}, []string{"f_smoke"}},
		{"выдуманный id", []string{"f_made_up", ""}, []string{}},
	}
	for _, c := range cases {
		got := allowedFacts(&cs, c.ids)
		if got == nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: allowedFacts = %#v, want %#v", c.name, got, c.want)
		}
	}
	noBrief := &model.CallScript{}
	if got := allowedFacts(noBrief, []string{"f_smoke"}); got == nil || len(got) != 0 {
		t.Fatalf("no brief: %#v", got)
	}
}

func TestSmallHelpers(t *testing.T) {
	t.Parallel()
	if clamp01(nil) != nil {
		t.Fatal("nil")
	}
	nan := float32(math.NaN())
	if clamp01(&nan) != nil {
		t.Fatal("NaN must be dropped")
	}
	for in, want := range map[float32]float32{-0.5: 0, 0: 0, 0.42: 0.42, 1: 1, 7: 1} {
		v := in
		if got := clamp01(&v); got == nil || *got != want {
			t.Errorf("clamp01(%v) = %v", in, got)
		}
	}
	if speechDurationMs("") != 1200 || speechDurationMs("Да") != 1200 {
		t.Fatal("minimum 1200")
	}
	if got := speechDurationMs(strings.Repeat("ы", 100)); got != 7000 {
		t.Fatalf("100 runes = %d", got)
	}
	acc := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if msSince(nil, acc) != 0 || msSince(&acc, acc.Add(-time.Second)) != 0 || msSince(&acc, acc.Add(1500*time.Millisecond)) != 1500 {
		t.Fatal("msSince")
	}
	ev := eventTime(time.Date(2026, 1, 1, 3, 0, 0, 123456789, time.FixedZone("MSK", 3*3600)))
	if ev.Location() != time.UTC || ev.Nanosecond() != 123456000 || ev.Hour() != 0 {
		t.Fatalf("eventTime = %v", ev)
	}
}

// ---------------------------------------------------------------- состояние и сохранённый ответ

func voiceAttempt() *attemptCtx {
	acc := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	a := &attemptCtx{ID: uuid.New(), UserID: uuid.New(), LessonID: uuid.New(), Status: core.AttemptInProgress, CallAcceptedAt: &acc}
	a.Settings = model.DefaultLessonSettings(nil)
	a.Settings.Voice.Enabled = true
	a.Settings.Voice.Input = model.VoiceInputBoth
	a.Settings.Voice.MaxTurns = 5
	a.Settings.Voice.TTSEnabled = false // localReply без пула
	cs := demoScript(0)
	a.Script = &cs
	return a
}

func TestBuildState(t *testing.T) {
	t.Parallel()
	a := voiceAttempt()
	tr := summarize(nil)
	st := buildState(a, &tr)
	b, _ := json.Marshal(st)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"attemptId", "turns", "callEnded", "input", "nextTurnNo", "turnsLeft"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing %q in %s", k, b)
		}
	}
	if !strings.Contains(string(b), `"turns":[]`) {
		t.Fatalf("turns must be []: %s", b)
	}
	if _, ok := m["endReason"]; ok {
		t.Fatalf("endReason only for ended call: %s", b)
	}
	if *st.NextTurnNo != 1 || *st.TurnsLeft != 5 || st.Input != "both" || st.CallEnded {
		t.Fatalf("state: %+v", st)
	}

	rows := []store.DialogueTurnRow{{TurnNo: 0, Speaker: "caller"}}
	for k := 1; k <= 6; k++ {
		rows = append(rows, store.DialogueTurnRow{TurnNo: k, Speaker: "operator"}, store.DialogueTurnRow{TurnNo: k, Speaker: "caller"})
	}
	tr = summarize(rows)
	ended := time.Now()
	reason := core.EndMaxTurns
	a.CallEndedAt, a.CallEndReason = &ended, &reason
	st = buildState(a, &tr)
	if *st.TurnsLeft != 0 || *st.NextTurnNo != 7 || !st.CallEnded || st.EndReason == nil || string(*st.EndReason) != "max_turns" || len(st.Turns) != 13 {
		t.Fatalf("ended state: %+v", st)
	}
}

func TestStoredResponseAndDuplicate(t *testing.T) {
	t.Parallel()
	a := voiceAttempt()
	meta, _ := json.Marshal(model.TurnMeta{LatencyMs: 1234, Fallback: true})
	rows := []store.DialogueTurnRow{
		{TurnNo: 0, Speaker: "caller", Text: "Алло!", Source: "script"},
		{TurnNo: 1, Speaker: "operator", Text: "Адрес?", Source: "text"},
		{TurnNo: 1, Speaker: "caller", Text: "Ленина 12", Source: "script", Meta: meta},
	}
	tr := summarize(rows)
	resp := storedResponse(a, &tr, 1)
	if resp == nil || resp.TurnNo != 1 || resp.NextTurnNo != 2 || resp.Operator == nil || resp.Operator.Text != "Адрес?" ||
		resp.Caller.Text != "Ленина 12" || !resp.Fallback || resp.LatencyMs == nil || *resp.LatencyMs != 1234 || resp.CallEnded {
		t.Fatalf("stored: %+v", resp)
	}
	if storedResponse(a, &tr, 5) != nil {
		t.Fatal("unknown turn must be nil")
	}
	// разговор закрыт на последнем ходе — ответ это отражает
	ended := time.Now()
	reason := core.EndCallerHungUp
	a.CallEndedAt, a.CallEndReason = &ended, &reason
	resp = storedResponse(a, &tr, 1)
	if !resp.CallEnded || resp.EndReason == nil || *resp.EndReason != reason {
		t.Fatalf("ended stored: %+v", resp)
	}

	he := httpx.AsError(duplicateConflict(a, &tr, 1))
	if he.Status != http.StatusConflict || he.Code != httpx.CodeConflict || he.Details["nextTurnNo"] != 2 || he.Details["response"] == nil {
		t.Fatalf("duplicate: %+v", he)
	}
}

func TestPrecheck(t *testing.T) {
	t.Parallel()
	rows := []store.DialogueTurnRow{
		{TurnNo: 0, Speaker: "caller"},
		{TurnNo: 1, Speaker: "operator"}, {TurnNo: 1, Speaker: "caller"},
	}
	ended := time.Now()
	reason := core.EndOperatorHungUp
	cases := []struct {
		name    string
		mut     func(a *attemptCtx)
		in      turnInput
		status  int
		details string // ключ details, который обязан быть
	}{
		{"ок", nil, turnInput{turnNo: 2, text: "Адрес?"}, 0, ""},
		{"повтор обработанного хода", nil, turnInput{turnNo: 1}, 409, "response"},
		{"повтор даже в завершённой попытке", func(a *attemptCtx) { a.Status = core.AttemptSubmitted }, turnInput{turnNo: 1}, 409, "response"},
		{"вызов не принят", func(a *attemptCtx) { a.Status = core.AttemptIssued }, turnInput{turnNo: 2}, 409, ""},
		{"попытка сдана", func(a *attemptCtx) { a.Status = core.AttemptSubmitted }, turnInput{turnNo: 2}, 409, ""},
		{"нет call_accepted_at", func(a *attemptCtx) { a.CallAcceptedAt = nil }, turnInput{turnNo: 2}, 409, ""},
		{"голос выключен", func(a *attemptCtx) { a.Settings.Voice.Enabled = false }, turnInput{turnNo: 2}, 409, ""},
		{"разговор завершён", func(a *attemptCtx) { a.CallEndedAt, a.CallEndReason = &ended, &reason }, turnInput{turnNo: 2}, 409, "endReason"},
		{"номер из будущего", nil, turnInput{turnNo: 4}, 409, "nextTurnNo"},
		{"лимит исчерпан", func(a *attemptCtx) { a.Settings.Voice.MaxTurns = 1 }, turnInput{turnNo: 2}, 409, "nextTurnNo"},
		{"аудио в текстовом занятии", func(a *attemptCtx) { a.Settings.Voice.Input = model.VoiceInputText }, turnInput{turnNo: 2, audio: &audioIn{}}, 400, "fields"},
		{"текст в голосовом занятии разрешён", func(a *attemptCtx) { a.Settings.Voice.Input = model.VoiceInputVoice }, turnInput{turnNo: 2, text: "Адрес?"}, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			a := voiceAttempt()
			if c.mut != nil {
				c.mut(a)
			}
			tr := summarize(rows)
			in := c.in
			err := precheck(a, &tr, &in)
			if c.status == 0 {
				if err != nil {
					t.Fatalf("unexpected %v", err)
				}
				return
			}
			he := httpx.AsError(err)
			if he.Status != c.status {
				t.Fatalf("status %d, want %d (%v)", he.Status, c.status, err)
			}
			if c.details != "" {
				if _, ok := he.Details[c.details]; !ok {
					t.Fatalf("details without %q: %+v", c.details, he.Details)
				}
			}
		})
	}
}

// ---------------------------------------------------------------- запрос к ai-service

func TestBuildRequest(t *testing.T) {
	t.Parallel()
	snap := settings.Defaults()
	a := voiceAttempt()
	a.Settings.Voice.TTSEnabled = true

	tr := summarize(nil) // вступления нет: история всё равно [] (ai-service отвергает null)
	req := buildRequest(a, &tr, &turnInput{turnNo: 1, text: "Что случилось?"}, &snap)
	b, _ := json.Marshal(req)
	s := string(b)
	for _, want := range []string{`"history":[]`, `"revealed_fact_ids":[]`, `"operator_text":"Что случилось?"`, `"turn_no":1`,
		`"profile":"dialog_fast"`, `"lang":"ru"`, `"max_reply_words":40`, `"voice":"xenia"`, `"enabled":true`, `"schema_version":"1"`} {
		if !strings.Contains(s, want) {
			t.Errorf("request lacks %s: %s", want, s)
		}
	}
	if strings.Contains(s, `"stt"`) {
		t.Errorf("text turn must not send stt options: %s", s)
	}
	if req.AttemptId != a.ID || req.RequestId == uuid.Nil {
		t.Fatalf("ids: %+v", req)
	}

	// аудио: stt-опции с подсказками, без operator_text; голос заявителя из легенды
	a.Script.Caller.Voice = "baya"
	rows := []store.DialogueTurnRow{
		{TurnNo: 0, Speaker: "caller", Text: "Алло!", RevealedFactIDs: []string{"f_smoke"}},
		{TurnNo: 1, Speaker: "operator", Text: "Адрес?"},
		{TurnNo: 1, Speaker: "caller", Text: "Ленина 12", RevealedFactIDs: []string{}},
	}
	tr = summarize(rows)
	req = buildRequest(a, &tr, &turnInput{turnNo: 2, audio: &audioIn{}}, &snap)
	if req.OperatorText != nil {
		t.Fatal("audio turn must not send operator_text")
	}
	if req.Options == nil || req.Options.Stt == nil || req.Options.Stt.Hints == nil || len(*req.Options.Stt.Hints) == 0 {
		t.Fatalf("stt options: %+v", req.Options)
	}
	if *req.Options.Tts.Voice != "baya" {
		t.Fatalf("voice = %s", *req.Options.Tts.Voice)
	}
	if got := len(req.History); got != 3 || req.History[0].TurnNo != 1 || req.History[1].TurnNo != 2 || req.History[2].TurnNo != 3 {
		t.Fatalf("history seq numbering: %+v", req.History)
	}
	if !reflect.DeepEqual(*req.RevealedFactIds, []string{"f_smoke"}) {
		t.Fatalf("revealed: %v", *req.RevealedFactIds)
	}
}

func newUnitService() *Service {
	return New(Deps{Log: discardLog()})
}

func TestFromAI(t *testing.T) {
	t.Parallel()
	s := newUnitService()
	snap := settings.Defaults()
	a := voiceAttempt()
	a.Settings.Voice.TTSEnabled = true
	req := &aiservice.DialogTurnRequest{TurnNo: 2}

	t.Run("аудио: распознанный текст и заявитель", func(t *testing.T) {
		t.Parallel()
		res := aiReply(req, "  Третий подъезд, быстрее!  ")
		res.Operator = components.SttResult{Text: " Какой подъезд? ", TextRaw: ptr("какой подъезд"), Confidence: ptr(float32(1.7)), AudioDurationMs: 2100}
		res.Caller.RevealedFactIds = &[]string{"f_entrance", "f_secret", "f_fake"}
		res.Caller.ShouldEnd = ptr(true)
		res.Caller.EndReason = ptr(" max_turns ")
		res.Caller.IsUnknownAnswer = ptr(true)
		res.Caller.Tts = &components.TtsResult{FilePath: "dialog/x/2.wav", DurationMs: 3300}
		op, caller := s.fromAI(t.Context(), res, a, &turnInput{turnNo: 2, audio: &audioIn{}}, &snap)
		if op.text != "Какой подъезд?" || op.source != core.TurnSourceSTT || op.noSpeech || op.textRaw != "какой подъезд" ||
			op.confidence == nil || *op.confidence != 1 || op.durationMs == nil || *op.durationMs != 2100 {
			t.Fatalf("op: %+v", op)
		}
		if caller.text != "Третий подъезд, быстрее!" || caller.source != core.TurnSourceLLM || caller.fallback ||
			!caller.shouldEnd || caller.endReason != "max_turns" || !caller.unknown ||
			caller.audioPath == nil || *caller.audioPath != "dialog/x/2.wav" || *caller.durationMs != 3300 ||
			!reflect.DeepEqual(caller.revealed, []string{"f_entrance"}) || caller.emotional == nil || *caller.emotional != "паника" {
			t.Fatalf("caller: %+v", caller)
		}
	})

	t.Run("аудио: тишина — переспрос, факты не раскрываются", func(t *testing.T) {
		t.Parallel()
		res := aiReply(req, "")
		res.Operator = components.SttResult{Text: "", NoSpeech: ptr(true)}
		res.Caller.RevealedFactIds = &[]string{"f_smoke"}
		op, caller := s.fromAI(t.Context(), res, a, &turnInput{turnNo: 2, audio: &audioIn{}}, &snap)
		if !op.noSpeech || caller.text != reaskText || caller.source != core.TurnSourceScript || len(caller.revealed) != 0 {
			t.Fatalf("op=%+v caller=%+v", op, caller)
		}
	})

	t.Run("текст распознан пустым без флага — тоже тишина", func(t *testing.T) {
		t.Parallel()
		res := aiReply(req, "Что?")
		res.Operator = components.SttResult{Text: " \x00 "}
		op, caller := s.fromAI(t.Context(), res, a, &turnInput{turnNo: 2, audio: &audioIn{}}, &snap)
		if !op.noSpeech || caller.text != "Что?" {
			t.Fatalf("op=%+v caller=%+v", op, caller)
		}
	})

	t.Run("текст: реплика оператора из запроса, fallback ai-service", func(t *testing.T) {
		t.Parallel()
		res := aiReply(req, "Приезжайте!")
		res.Operator = components.SttResult{Text: "не то"}
		res.Fallback = ptr(true)
		res.Caller.Tts = &components.TtsResult{FilePath: "../../etc/passwd"}
		op, caller := s.fromAI(t.Context(), res, a, &turnInput{turnNo: 2, text: "Адрес?"}, &snap)
		if op.text != "Адрес?" || op.source != core.TurnSourceText || op.noSpeech {
			t.Fatalf("op: %+v", op)
		}
		if caller.source != core.TurnSourceScript || !caller.fallback || caller.audioPath != nil || caller.durationMs == nil || *caller.durationMs != 1200 {
			t.Fatalf("caller: %+v", caller)
		}
	})

	t.Run("пустой ответ заявителя — локальная реплика", func(t *testing.T) {
		t.Parallel()
		b := voiceAttempt() // TTS выключен — пул не нужен
		res := aiReply(req, "   ")
		op, caller := s.fromAI(t.Context(), res, b, &turnInput{turnNo: 1, text: "Адрес?"}, &snap)
		if op.text != "Адрес?" || !caller.fallback || caller.source != core.TurnSourceScript || caller.text != "Улица Ленина, дом двенадцать!" {
			t.Fatalf("op=%+v caller=%+v", op, caller)
		}
	})

	t.Run("TTS выключен в занятии — путь от ai-service не берём", func(t *testing.T) {
		t.Parallel()
		b := voiceAttempt()
		res := aiReply(req, "Скорее!")
		res.Caller.Tts = &components.TtsResult{FilePath: "dialog/x/2.wav", DurationMs: 999}
		_, caller := s.fromAI(t.Context(), res, b, &turnInput{turnNo: 2, text: "Адрес?"}, &snap)
		if caller.audioPath != nil || *caller.durationMs == 999 {
			t.Fatalf("caller: %+v", caller)
		}
	})
}

func TestAIFailure(t *testing.T) {
	t.Parallel()
	s := newUnitService()
	a := voiceAttempt()
	text := &turnInput{turnNo: 1, text: "Адрес?"}
	audio := &turnInput{turnNo: 1, audio: &audioIn{}}
	aiErr := func(k core.AIErrorKind, retry int) error {
		return &core.AIError{Kind: k, RetryAfter: retry, Message: "x"}
	}

	cases := []struct {
		name   string
		err    error
		src    error
		in     *turnInput
		status int // 0 — nil (локальная реплика)
		code   string
		retry  int
		leader bool
	}{
		{"занят", aiErr(core.AIBusy, 7), nil, text, 503, httpx.CodeCallerBusy, 7, false},
		{"недоступен, текст — локальный ответ", aiErr(core.AIUnavailable, 0), nil, text, 0, "", 0, false},
		{"недоступен, аудио — caller_busy", aiErr(core.AIUnavailable, 0), errAudioStopped, audio, 503, httpx.CodeCallerBusy, 5, false},
		{"аудио длинное", aiErr(core.AIAudioTooLong, 0), nil, audio, 413, httpx.CodeAudioTooLong, 0, false},
		{"аудио не того формата", aiErr(core.AIAudioUnsupported, 0), nil, audio, 415, httpx.CodeAudioUnsupported, 0, false},
		{"ai-service отверг запрос", aiErr(core.AIBadRequest, 0), nil, text, 500, httpx.CodeInternal, 0, false},
		{"чужая ошибка", errors.New("boom"), nil, text, 500, httpx.CodeInternal, 0, false},
		{"загрузка упёрлась в лимит", aiErr(core.AIUnavailable, 0), &http.MaxBytesError{Limit: 1}, audio, 413, httpx.CodeAudioTooLong, 0, false},
		{"загрузка оборвалась у студента", aiErr(core.AIUnavailable, 0), errors.New("unexpected EOF"), audio, 400, httpx.CodeValidation, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := s.aiFailure(c.err, c.src, c.in, a)
			if c.status == 0 {
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
				return
			}
			var lo *leaderOnlyError
			if got := errors.As(err, &lo); got != c.leader {
				t.Fatalf("leaderOnly = %v", got)
			}
			he := httpx.AsError(err)
			if he.Status != c.status || he.Code != c.code || he.RetryAfter != c.retry {
				t.Fatalf("got %d %s retry=%d, want %d %s retry=%d", he.Status, he.Code, he.RetryAfter, c.status, c.code, c.retry)
			}
		})
	}
}

// ---------------------------------------------------------------- запасные реплики

func TestLocalReply(t *testing.T) {
	t.Parallel()
	s := newUnitService()
	snap := settings.Defaults()
	a := voiceAttempt() // TTS выключен
	want := []string{"Улица Ленина, дом двенадцать!", "Приезжайте быстрее, пожалуйста!", "Улица Ленина, дом двенадцать!"}
	for i, w := range want {
		c := s.localReply(t.Context(), a, i+1, &snap)
		if c.text != w || !c.fallback || c.source != core.TurnSourceScript || c.shouldEnd || c.revealed == nil || len(c.revealed) != 0 ||
			c.durationMs == nil || *c.durationMs != speechDurationMs(w) || c.emotional == nil || *c.emotional != "паника" {
			t.Fatalf("turn %d: %+v", i+1, c)
		}
	}

	// в легенде только вступление — запасные фразы по кругу
	only := voiceAttempt()
	only.Script = &model.CallScript{Turns: []model.Turn{{Speaker: model.SpeakerCaller, Text: "Алло!"}, {Speaker: model.SpeakerCaller, Text: "  "}}}
	for k := 1; k <= len(panicLines)+1; k++ {
		c := s.localReply(t.Context(), only, k, &snap)
		if c.text != panicLines[(k-1)%len(panicLines)] || c.emotional != nil {
			t.Fatalf("turn %d: %q", k, c.text)
		}
	}
	// turnNo 0 (не бывает, но не паникуем)
	if c := s.localReply(t.Context(), only, 0, &snap); c.text != panicLines[0] {
		t.Fatalf("turn 0: %q", c.text)
	}
}

func TestOpeningHash(t *testing.T) {
	t.Parallel()
	cs := demoScript(0)
	h, text, voice := openingHash(&cs, "xenia", 1)
	if text != "Алло! Пожар! У соседей дым валит!" || voice != "xenia" || len(h) != 64 {
		t.Fatalf("opening: %q %q %q", h, text, voice)
	}
	cs.Turns[0].TTSHash = " abc "
	cs.Caller.Voice = "baya"
	h, _, voice = openingHash(&cs, "xenia", 1)
	if h != "abc" || voice != "baya" {
		t.Fatalf("tts_hash from approve: %q %q", h, voice)
	}
	empty := model.CallScript{}
	h2, text, _ := openingHash(&empty, "xenia", 1)
	if text != openingFallback || h2 == "" {
		t.Fatalf("fallback: %q %q", h2, text)
	}
}

func TestTurnMetaBytes(t *testing.T) {
	t.Parallel()
	if got := string(turnMeta{}.bytes()); got != "{}" {
		t.Fatalf("empty meta = %s", got)
	}
	b := turnMeta{LatencyMs: 10, Fallback: true, TextRaw: "сырой", IsUnknownAnswer: true, Engine: &components.Engine{DurationMs: 5}}.bytes()
	var m model.TurnMeta
	if err := json.Unmarshal(b, &m); err != nil || m.LatencyMs != 10 || !m.Fallback || m.TextRaw != "сырой" || !m.IsUnknownAnswer || m.Engine["duration_ms"] != float64(5) {
		t.Fatalf("meta: %s -> %+v (%v)", b, m, err)
	}
}

func TestTimeouts(t *testing.T) {
	t.Parallel()
	s := newUnitService()
	if got := s.dialogTimeout(nil); got != 20*time.Second {
		t.Fatalf("default = %s", got)
	}
	snap := settings.Defaults()
	snap.AI.DialogTimeoutSec = 7
	if got := s.dialogTimeout(&snap); got != 7*time.Second {
		t.Fatalf("settings = %s", got)
	}
	if got := s.turnTimeout(&snap); got != 17*time.Second {
		t.Fatalf("turn = %s", got)
	}
	if s.snapshot(t.Context()) == nil {
		t.Fatal("snapshot without store must be defaults")
	}
}
