package attempts

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/reaction"
	"lct/gocore/internal/settings"
	"lct/gocore/internal/store"
)

// ---------------------------------------------------------------- checkDraft (регрессия: PUT принимал любой объект)

// frontCard — карточка так, как её шлёт фронт (emptyCard() + правки оператора).
const frontCard = `{
  "phones": {"aon": "+79991234567", "provided": "", "foreign": false},
  "applicant": {"name": "Мария Петровна", "status": "очевидец"},
  "address": {"raw": "Москва, Тверская ул., д. 5", "country": "", "region": "Москва", "settlement": "", "object": "",
              "okrug": "", "district": "", "street": "Тверская", "house": "5", "building": "", "structure": "",
              "apartment": "", "entrance": "2", "floor": "5", "code": "", "descriptive": "", "lat": 55.76, "lon": 37.6,
              "source": "manual"},
  "incidentTypeIds": ["fire-flat"],
  "attributes": {"where": "квартира", "floorCount": 9, "smoke": true, "x": null},
  "description": "Горит квартира, внутри может быть человек",
  "actionsTaken": "",
  "flags": {"victimsPresent": true, "victimsCount": 1, "ambulanceRefusal": false, "blocked": false, "noContact": false, "callDropped": false},
  "services": [{"serviceId": "svc-101", "code": "101", "name": "Пожарная охрана", "shortName": "01", "isPrimary": true,
                "source": "auto", "reason": "пожар", "currentStatus": "Получена службой", "currentStatusAt": "2026-09-24T10:00:00Z",
                "history": [{"status": "Добавлена", "at": "2026-09-24T10:00:00Z", "operator": "система"},
                            {"status": "Получена службой", "at": "2026-09-24T10:00:00Z", "operator": "система"}],
                "allowedNext": [], "editable": true}],
  "unknownExtra": {"kept": true}
}`

func TestCheckDraft_Valid(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"пустая карточка сервера": string(emptyDraftJSON),
		"карточка фронта":         frontCard,
		"пустой phones/applicant": `{"phones":{},"applicant":{},"address":{"raw":""},"incidentTypeIds":[],"attributes":{},
			"description":"","actionsTaken":"","flags":{"victimsPresent":false,"ambulanceRefusal":false,"blocked":false,"noContact":false,"callDropped":false},"services":[]}`,
	} {
		if err := checkDraft([]byte(body)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func mutateCard(t *testing.T, fn func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(frontCard), &m); err != nil {
		t.Fatal(err)
	}
	fn(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCheckDraft_Invalid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		body  []byte
		field string // ожидаемый ключ details.fields ("" — без полей)
	}{
		{"чужой объект", []byte(`{"foo":1}`), "address"},
		{"нет description", mutateCard(t, func(m map[string]any) { delete(m, "description") }), "description"},
		{"description null", mutateCard(t, func(m map[string]any) { m["description"] = nil }), "description"},
		{"description числом", mutateCard(t, func(m map[string]any) { m["description"] = 5 }), "description"},
		{"ключ в другом регистре", mutateCard(t, func(m map[string]any) {
			m["Description"] = m["description"]
			delete(m, "description")
		}), "description"},
		{"services объектом", mutateCard(t, func(m map[string]any) { m["services"] = map[string]any{} }), "services"},
		{"incidentTypeIds строкой", mutateCard(t, func(m map[string]any) { m["incidentTypeIds"] = "fire" }), "incidentTypeIds"},
		{"address строкой", mutateCard(t, func(m map[string]any) { m["address"] = "Тверская" }), "address"},
		{"address без raw", mutateCard(t, func(m map[string]any) { delete(m["address"].(map[string]any), "raw") }), "address.raw"},
		{"address.raw null", mutateCard(t, func(m map[string]any) { m["address"].(map[string]any)["raw"] = nil }), "address.raw"},
		{"flags null", mutateCard(t, func(m map[string]any) { m["flags"] = nil }), "flags"},
		{"applicant.name числом", mutateCard(t, func(m map[string]any) { m["applicant"].(map[string]any)["name"] = 42 }), "applicant.name"},
		{"street числом", mutateCard(t, func(m map[string]any) { m["address"].(map[string]any)["street"] = 1 }), "address.street"},
		{"victimsCount дробное", mutateCard(t, func(m map[string]any) { m["flags"].(map[string]any)["victimsCount"] = 2.5 }), "flags.victimsCount"},
		{"служба: история не массив", mutateCard(t, func(m map[string]any) {
			m["services"].([]any)[0].(map[string]any)["history"] = "x"
		}), "services.history"},
		{"служба: время не RFC3339", mutateCard(t, func(m map[string]any) {
			m["services"].([]any)[0].(map[string]any)["currentStatusAt"] = "вчера"
		}), ""},
	}
	for _, c := range cases {
		err := checkDraft(c.body)
		var he *httpx.Error
		if !errors.As(err, &he) || he.Status != http.StatusBadRequest || he.Code != httpx.CodeValidation {
			t.Errorf("%s: ожидался 400 validation, получено %v", c.name, err)
			continue
		}
		if c.field == "" {
			continue
		}
		fields, _ := he.Details["fields"].(map[string]string)
		if _, ok := fields[c.field]; !ok {
			t.Errorf("%s: нет поля %q в %v", c.name, c.field, he.Details)
		}
	}
}

// ---------------------------------------------------------------- NUL

func TestStripNULEscapes(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{`{"a":"Пожар"}`, `{"a":"Пожар"}`},
		{`{"a":"По\u0000жар"}`, `{"a":"Пожар"}`},
		{`{"a":"\u0000\u0000x\u0000"}`, `{"a":"x"}`},
		{`{"a":"\\u0000"}`, `{"a":"\\u0000"}`},     // экранированная косая черта + текст — не NUL
		{`{"a":"\\\u0000"}`, `{"a":"\\"}`},         // косая черта, затем NUL
		{`{"a":"\"\u0000\""}`, `{"a":"\"\""}`},     // кавычки внутри строки
		{`{"k\u0000ey":"\u00001"}`, `{"key":"1"}`}, // NUL в ключе; \u00001 = NUL + «1»
		{`{"a":"\u0001\né"}`, `{"a":"\u0001\né"}`}, // другие escape'ы не трогаем
	}
	for _, c := range cases {
		got := stripNULEscapes([]byte(c.in))
		if string(got) != c.want {
			t.Errorf("stripNULEscapes(%s) = %s, want %s", c.in, got, c.want)
		}
		if !json.Valid(got) {
			t.Errorf("stripNULEscapes(%s): невалидный JSON %s", c.in, got)
		}
	}
	// без NUL — тот же срез, без копирования (горячий путь)
	in := []byte(`{"a":"b"}`)
	if out := stripNULEscapes(in); &out[0] != &in[0] {
		t.Error("без NUL ожидался тот же срез")
	}
	// после json.Marshal строки с NUL
	b, _ := json.Marshal(map[string]string{"d": "дым\x00 из окна"})
	var back map[string]string
	if err := json.Unmarshal(stripNULEscapes(b), &back); err != nil || back["d"] != "дым из окна" {
		t.Errorf("marshal+strip: %v %q", err, back["d"])
	}
}

func TestStripNUL(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"": "", "абв": "абв", "а\x00б\x00": "аб", "\x00": ""} {
		if got := stripNUL(in); got != want {
			t.Errorf("stripNUL(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------- ошибки PostgreSQL → 400

func TestBadTextError(t *testing.T) {
	t.Parallel()
	if badTextError(errors.New("x")) != nil || badTextError(nil) != nil {
		t.Fatal("не-PG ошибка — не 400")
	}
}

// ---------------------------------------------------------------- decodeServices

func TestDecodeServices(t *testing.T) {
	t.Parallel()
	svc := reaction.NewAssigned(catalogServices["101"], reaction.SourceManual, false, "", time.Now())
	good, _ := json.Marshal(svc)
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"nil", "", []string{}},
		{"не массив", `{"a":1}`, []string{}},
		{"пустой", `[]`, []string{}},
		{"мусор отброшен", `[` + string(good) + `, 5, {"code":""}, {"code":"102","history":"x"},
			{"code":"103","source":"auto","currentStatus":"Непонятно","currentStatusAt":"2026-09-24T10:00:00Z","history":[],"allowedNext":[],"editable":true}]`,
			[]string{"101"}},
	}
	for _, c := range cases {
		got := decodeServices([]byte(c.in))
		if got == nil || strings.Join(codes(got), ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: %v, want %v", c.name, codes(got), c.want)
		}
	}
}

func TestServicesMergeSQL_Placeholders(t *testing.T) {
	t.Parallel()
	for _, q := range []string{sqlPutDraft, sqlSubmitServices} {
		if strings.Contains(q, "{ARR_") || strings.Contains(q, "{PINNED_") {
			t.Fatalf("незаменённый плейсхолдер: %s", q)
		}
	}
	if !strings.Contains(sqlPutDraft, "a.status = 'in_progress'") {
		t.Fatal("PUT черновика должен требовать in_progress")
	}
	if !strings.Contains(sqlFirstInputTx, "call_accepted_at IS NOT NULL") {
		t.Fatal("время первого ввода — только после приёма вызова")
	}
}

// ---------------------------------------------------------------- validateCard / resolveActionText

func TestValidateCard(t *testing.T) {
	t.Parallel()
	base := func() *public.IncidentCardDraft {
		d := convert.EmptyDraft()
		return &d
	}
	s := func(v string) *string { return &v }
	cases := []struct {
		name   string
		mut    func(c *public.IncidentCardDraft)
		action *string
		fields []string
	}{
		{"пустая", func(*public.IncidentCardDraft) {}, nil, nil},
		{"описание 1999 рун", func(c *public.IncidentCardDraft) { c.Description = strings.Repeat("ж", 1999) }, nil, nil},
		{"описание 2000 рун", func(c *public.IncidentCardDraft) { c.Description = strings.Repeat("ж", 2000) }, nil, []string{"card.description"}},
		{"пустой статус заявителя — не выбран", func(c *public.IncidentCardDraft) {
			st := public.ApplicantStatus("")
			c.Applicant.Status = &st
		}, nil, nil},
		{"чужой статус заявителя", func(c *public.IncidentCardDraft) {
			st := public.ApplicantStatus("прохожий")
			c.Applicant.Status = &st
		}, nil, []string{"card.applicant.status"}},
		{"отрицательное число пострадавших", func(c *public.IncidentCardDraft) {
			n := -1
			c.Flags.VictimsCount = &n
		}, nil, []string{"card.flags.victimsCount"}},
		{"источник адреса", func(c *public.IncidentCardDraft) {
			src := public.AddressSource("gps")
			c.Address.Source = &src
		}, nil, []string{"card.address.source"}},
		{"служба без кода и статуса", func(c *public.IncidentCardDraft) {
			c.Services = []public.AssignedService{{Source: "auto", CurrentStatus: "Горит"}}
		}, nil, []string{"card.services[0].currentStatus", "card.services[0].code"}},
		{"actionText длинный", func(*public.IncidentCardDraft) {}, s(strings.Repeat("д", maxActionTextRunes+1)), []string{"actionText"}},
	}
	for _, c := range cases {
		card := base()
		c.mut(card)
		got := validateCard(card, c.action)
		if len(got) != len(c.fields) {
			t.Errorf("%s: %v, want %v", c.name, got, c.fields)
			continue
		}
		for _, f := range c.fields {
			if _, ok := got[f]; !ok {
				t.Errorf("%s: нет %q в %v", c.name, f, got)
			}
		}
	}
}

func TestResolveActionText(t *testing.T) {
	t.Parallel()
	s := func(v string) *string { return &v }
	card := func(actions string) *public.IncidentCardDraft {
		d := convert.EmptyDraft()
		d.ActionsTaken = actions
		return &d
	}
	cases := []struct {
		name   string
		mode   string
		action *string
		card   *public.IncidentCardDraft
		want   *string
	}{
		{"явный текст", core.ModeCards, s("  Направил пожарных  "), card(""), s("Направил пожарных")},
		{"cards без текста", core.ModeCards, nil, card("Направил"), nil},
		{"card_actions из карточки", core.ModeCardActions, nil, card(" Направил пожарных "), s("Направил пожарных")},
		{"пробелы — нет текста", core.ModeCardActions, s("   "), card("  "), nil},
		{"NUL вырезается", core.ModeCardActions, s("На\x00правил"), card(""), s("Направил")},
		{"NUL в карточке", core.ModeCardActions, nil, card("\x00Направил\x00"), s("Направил")},
		{"только NUL", core.ModeCardActions, s("\x00"), card("\x00"), nil},
	}
	for _, c := range cases {
		got := resolveActionText(c.mode, c.action, c.card)
		switch {
		case (got == nil) != (c.want == nil):
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		case got != nil && *got != *c.want:
			t.Errorf("%s: %q, want %q", c.name, *got, *c.want)
		}
	}
}

// ---------------------------------------------------------------- легенда студента

func testScript() *model.CallScript {
	var cs model.CallScript
	if err := json.Unmarshal([]byte(testCallScript), &cs); err != nil {
		panic(err)
	}
	cs.Normalize()
	return &cs
}

func TestStudentCallScript_Text(t *testing.T) {
	t.Parallel()
	files := ttsIndex{hashes: []string{"h-open"}, paths: []string{"ab/h-open.ogg"}, durations: []int32{3100}}
	ls := model.LessonSettings{AllowReplay: true}
	out := studentCallScript(testScript(), ls, files)

	if !out.AllowReplay || out.Voice.Enabled {
		t.Fatalf("allowReplay/voice: %+v", out)
	}
	if out.Caller.Phone == nil || *out.Caller.Phone != "+79991234567" || out.Caller.Name == nil || *out.Caller.Name != "Мария Петровна" {
		t.Fatalf("caller: %+v", out.Caller)
	}
	// пустая реплика (индекс 2) пропущена, индексы — позиции в сценарии
	if len(out.Turns) != 3 || out.Turns[0].Index != 0 || out.Turns[1].Index != 1 || out.Turns[2].Index != 3 {
		t.Fatalf("turns: %+v", out.Turns)
	}
	if out.Turns[1].Speaker != public.CallTurnViewSpeakerOperatorHint || out.Turns[0].Speaker != public.CallTurnViewSpeakerCaller {
		t.Fatal("speaker")
	}
	if out.Turns[0].AudioUrl == nil || !strings.HasSuffix(*out.Turns[0].AudioUrl, "ab/h-open.ogg") || *out.Turns[0].DurationMs != 3100 {
		t.Fatalf("озвученная реплика: %+v", out.Turns[0])
	}
	// нет файла — без аудио, длительность оценкой max(2200, руны × 70)
	if out.Turns[2].AudioUrl != nil || *out.Turns[2].DurationMs != max(2200, len([]rune(out.Turns[2].Text))*70) {
		t.Fatalf("неозвученная реплика: %+v", out.Turns[2])
	}
	b, _ := json.Marshal(out)
	for _, leak := range []string{"СЕКРЕТНЫЙ", "key_facts", "keyFacts", "emotional", "dialogue", "baya"} {
		if bytes.Contains(b, []byte(leak)) {
			t.Errorf("утечка %q в легенде студента: %s", leak, b)
		}
	}
}

func TestStudentCallScript_Voice(t *testing.T) {
	t.Parallel()
	ls := model.LessonSettings{Voice: settings.Voice{Enabled: true, Input: "voice"}}
	out := studentCallScript(testScript(), ls, ttsIndex{})
	if !out.Voice.Enabled || len(out.Turns) != 1 || out.Turns[0].Index != 0 {
		t.Fatalf("голосовой режим — только вступление: %+v", out.Turns)
	}
	// нет реплик заявителя — пустой массив, не null
	empty := &model.CallScript{}
	empty.Normalize()
	out = studentCallScript(empty, ls, ttsIndex{})
	if out.Turns == nil || len(out.Turns) != 0 {
		t.Fatalf("turns = %#v", out.Turns)
	}
	b, _ := json.Marshal(out)
	if !bytes.Contains(b, []byte(`"turns":[]`)) || !bytes.Contains(b, []byte(`"caller":{}`)) {
		t.Fatalf("форма контракта: %s", b)
	}
}

func TestTTSIndexLookup(t *testing.T) {
	t.Parallel()
	idx := ttsIndex{hashes: []string{"a", "b", "c"}, paths: []string{"a.ogg", "b.ogg"}, durations: []int32{100}}
	if p, d, ok := idx.lookup("a"); !ok || p != "a.ogg" || d != 100 {
		t.Fatal("a")
	}
	if p, d, ok := idx.lookup("b"); !ok || p != "b.ogg" || d != 0 {
		t.Fatal("b: нет длительности — 0")
	}
	for _, h := range []string{"c", "", "zzz"} {
		if _, _, ok := idx.lookup(h); ok {
			t.Fatalf("%q найден", h)
		}
	}
}

// ---------------------------------------------------------------- службы: список черновика

func TestServiceList(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	a := reaction.NewAssigned(catalogServices["101"], reaction.SourceAuto, true, "пожар", now)
	b := reaction.NewAssigned(catalogServices["103"], reaction.SourceManual, false, manualReason, now)
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	raw := `[` + string(ab) + `, {"garbage":true}, ` + string(bb) + `]`

	l := parseServices([]byte(raw))
	if len(l.items) != 3 || l.items[1] != nil {
		t.Fatalf("items: %v", l.items)
	}
	if l.find("svc-103") != 2 || l.find(" 101 ") != 0 || l.find("") != -1 || l.find("102") != -1 {
		t.Fatal("find по serviceId/коду")
	}
	if l.findCode("101") != 0 {
		t.Fatal("findCode")
	}
	l.remove(0)
	l.add(reaction.NewAssigned(catalogServices["102"], reaction.SourceManual, false, manualReason, now))
	enc, err := l.encode()
	if err != nil {
		t.Fatal(err)
	}
	var back []json.RawMessage
	if err := json.Unmarshal(enc, &back); err != nil || len(back) != 3 {
		t.Fatalf("encode: %s %v", enc, err)
	}
	if string(back[0]) != `{"garbage":true}` {
		t.Fatalf("неразобранный элемент должен сохраниться байт в байт: %s", back[0])
	}
	var s public.AssignedService
	if err := json.Unmarshal(back[2], &s); err != nil || s.Code != "102" || s.AllowedNext == nil || len(s.History) != 2 {
		t.Fatalf("новая служба: %s", back[2])
	}

	// нет ключа / не массив — пустой список
	for _, in := range []string{"", "null", `{"a":1}`, `"x"`} {
		if l := parseServices([]byte(in)); len(l.items) != 0 {
			t.Errorf("%q: %d", in, len(l.items))
		}
	}
	if enc, _ := parseServices(nil).encode(); string(enc) != "[]" {
		t.Fatalf("пустой список кодируется как [] (не null): %s", enc)
	}
}

func TestServiceLabel(t *testing.T) {
	t.Parallel()
	if serviceLabel(&public.AssignedService{Code: "101", Name: "Пожарная", ShortName: "01"}) != "01" ||
		serviceLabel(&public.AssignedService{Code: "101", Name: "Пожарная"}) != "Пожарная" ||
		serviceLabel(&public.AssignedService{Code: "101"}) != "101" {
		t.Fatal("serviceLabel")
	}
}

func TestTransitionError(t *testing.T) {
	t.Parallel()
	as := reaction.NewAssigned(catalogServices["101"], reaction.SourceManual, false, "", time.Now())
	cases := []struct {
		to     public.ReactionStatus
		status int
		field  string
	}{
		{reaction.StatusDone, http.StatusConflict, ""},
		{reaction.StatusNotAccepted, http.StatusBadRequest, "comment"},
	}
	for _, c := range cases {
		cp := as
		err := transitionError(reaction.Transition(&cp, c.to, "", "", "оп. 1", time.Now()))
		var he *httpx.Error
		if !errors.As(err, &he) || he.Status != c.status {
			t.Errorf("%s: %v", c.to, err)
			continue
		}
		if c.field != "" {
			if f, _ := he.Details["fields"].(map[string]string); f[c.field] == "" {
				t.Errorf("%s: нет поля %s", c.to, c.field)
			}
		}
	}
	plain := errors.New("x")
	if transitionError(plain) != plain {
		t.Fatal("чужая ошибка — как есть")
	}
}

func TestDeref(t *testing.T) {
	t.Parallel()
	v := "a"
	if deref(nil) != "" || deref(&v) != "a" {
		t.Fatal("deref")
	}
}

// ---------------------------------------------------------------- события

func TestMicOK(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]bool{
		``: true, `{}`: true, `{"ok":true}`: true, `{"ok":false}`: false, `{"ok":"false"}`: true, `garbage`: true,
	} {
		if micOK([]byte(in)) != want {
			t.Errorf("micOK(%s) != %v", in, want)
		}
	}
}

func TestLiveTelemetry(t *testing.T) {
	t.Parallel()
	if !liveTelemetry(core.EventFieldChanged) || !liveTelemetry(core.EventChooseValue) ||
		liveTelemetry(core.EventServiceAssigned) || liveTelemetry(core.EventSave) {
		t.Fatal("liveTelemetry")
	}
}

// publishInserted: частые события ввода — не больше одного на попытку за liveEvery, остальные все.
func TestPublishInserted_Throttle(t *testing.T) {
	t.Parallel()
	pub := &recPublisher{}
	s := New(Deps{Publisher: pub, Log: quietLog()})
	lesson, att := uuid.New(), uuid.New()
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	s.meta.put(att, attemptMeta{LessonID: lesson, Status: core.AttemptInProgress}, now)

	batch := []public.AttemptEvent{
		{Id: 1, Type: core.EventFieldChanged}, {Id: 2, Type: core.EventChooseValue},
		{Id: 3, Type: core.EventServiceAssigned}, {Id: 4, Type: core.EventFieldChanged},
	}
	s.publishInserted(lesson, att, batch, now)
	if n := pub.count(public.MonitorMessageTypeAttemptEvent, ""); n != 2 { // service_assigned + последний ввод
		t.Fatalf("первая пачка: %d сообщений", n)
	}
	s.publishInserted(lesson, att, batch[:1], now.Add(liveEvery/2))
	if n := pub.count(public.MonitorMessageTypeAttemptEvent, ""); n != 2 {
		t.Fatalf("ввод в пределах liveEvery не публикуется: %d", n)
	}
	s.publishInserted(lesson, att, batch[:1], now.Add(liveEvery))
	if n := pub.count(public.MonitorMessageTypeAttemptEvent, ""); n != 3 {
		t.Fatalf("после liveEvery — снова: %d", n)
	}
	pub.mu.Lock()
	last := pub.monitor[len(pub.monitor)-1]
	pub.mu.Unlock()
	if last.lesson != lesson || last.msg.AttemptId == nil || *last.msg.AttemptId != att || last.msg.Event.Id != 1 {
		t.Fatalf("сообщение: %+v", last)
	}
}

// ---------------------------------------------------------------- кэш метаданных

func TestMetaCache(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	c := newMetaCache(16)
	id := uuid.New()
	if _, ok := c.get(id, now); ok {
		t.Fatal("пустой кэш")
	}
	c.put(id, attemptMeta{Status: core.AttemptIssued}, now)
	if m, ok := c.get(id, now.Add(metaTTL)); !ok || m.Status != core.AttemptIssued {
		t.Fatal("в пределах TTL")
	}
	if _, ok := c.get(id, now.Add(metaTTL+time.Nanosecond)); ok {
		t.Fatal("после TTL запись протухла")
	}
	c.setStatus(id, core.AttemptEvaluating, now)
	if _, ok := c.get(id, now.Add(metaTTL+time.Second)); !ok {
		t.Fatal("терминальный статус живёт дольше")
	}
	c.setFirstInput(id)
	if m, _ := c.get(id, now); !m.FirstInput {
		t.Fatal("setFirstInput")
	}
	// своей записи нет — setStatus/setFirstInput ничего не создают
	other := uuid.New()
	c.setStatus(other, core.AttemptInProgress, now)
	c.setFirstInput(other)
	if _, ok := c.get(other, now); ok {
		t.Fatal("запись не должна появиться")
	}
	// allowLive: без записи — можно всегда; с записью — не чаще liveEvery
	if !c.allowLive(other, now) || !c.allowLive(other, now) {
		t.Fatal("allowLive без записи")
	}
	if !c.allowLive(id, now) || c.allowLive(id, now.Add(liveEvery-1)) || !c.allowLive(id, now.Add(liveEvery)) {
		t.Fatal("allowLive троттлинг")
	}
	// put сохраняет отметку троттлинга
	c.put(id, attemptMeta{Status: core.AttemptInProgress}, now.Add(liveEvery))
	if c.allowLive(id, now.Add(liveEvery+1)) {
		t.Fatal("put сбросил livePub")
	}
}

func TestMetaCache_Evict(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	c := newMetaCache(16)
	for i := 0; i < 8; i++ {
		c.put(uuid.New(), attemptMeta{Status: core.AttemptIssued}, now.Add(-time.Hour)) // протухшие
	}
	for i := 0; i < 8; i++ {
		c.put(uuid.New(), attemptMeta{Status: core.AttemptIssued}, now)
	}
	c.put(uuid.New(), attemptMeta{}, now) // переполнение: выбрасываются протухшие
	if n := len(c.m); n != 9 {
		t.Fatalf("после вытеснения протухших: %d", n)
	}
	for i := 0; i < 20; i++ {
		c.put(uuid.New(), attemptMeta{}, now)
	}
	if n := len(c.m); n > 16 {
		t.Fatalf("размер ограничен: %d", n)
	}
}

func TestMetaFromRowAndAccessRow(t *testing.T) {
	t.Parallel()
	teacher, student, lesson := uuid.New(), uuid.New(), uuid.New()
	now := time.Now()
	r := &store.AttemptRow{UserID: student, LessonID: lesson, Status: core.AttemptInProgress, FirstInputAt: &now, LessonCreatedBy: teacher}
	m := metaFromRow(r)
	if m.UserID != student || m.OwnerID != teacher || !m.FirstInput || m.Status != core.AttemptInProgress {
		t.Fatalf("%+v", m)
	}
	ar := m.accessRow()
	if ar.UserID != student || ar.LessonOwnerID() != teacher {
		t.Fatalf("accessRow: %+v", ar)
	}
}

func TestSpentMillis(t *testing.T) {
	t.Parallel()
	cases := []struct {
		d    time.Duration
		want int
	}{
		{0, 0},
		{-5 * time.Second, 0}, // часы сервера «назад» — не отрицательное
		{1500*time.Millisecond + 999*time.Microsecond, 1500},
		{24 * time.Hour, 86_400_000},
		{30 * 24 * time.Hour, 2147483647}, // предел int4 колонки time_spent_ms
	}
	for _, c := range cases {
		if got := spentMillis(c.d); got != c.want {
			t.Errorf("spentMillis(%v) = %d, want %d", c.d, got, c.want)
		}
	}
}
