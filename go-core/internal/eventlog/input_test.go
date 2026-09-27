package eventlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
)

var testNow = time.Date(2026, 9, 24, 12, 0, 0, 123456789, time.UTC)

func obj(kv ...any) *map[string]any {
	m := make(map[string]any, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return &m
}

func evIn(seq int, typ public.AttemptEventType, payload *map[string]any, at time.Time) public.AttemptEventInput {
	return public.AttemptEventInput{ClientSeq: seq, Type: typ, Payload: payload, At: at}
}

func TestParseInputs_BatchSize(t *testing.T) {
	t.Parallel()
	one := evIn(1, public.AttemptEventTypeSave, nil, testNow)
	tooMany := make([]public.AttemptEventInput, MaxBatch+1)
	for i := range tooMany {
		tooMany[i] = evIn(i+1, public.AttemptEventTypeSave, nil, testNow)
	}
	exact := tooMany[:MaxBatch]

	cases := []struct {
		name string
		body []public.AttemptEventInput
		ok   bool
	}{
		{"nil", nil, false},
		{"empty", []public.AttemptEventInput{}, false},
		{"one", []public.AttemptEventInput{one}, true},
		{"max", exact, true},
		{"max+1", tooMany, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := ParseInputs(tc.body, testNow)
			if tc.ok {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if len(out) != len(tc.body) {
					t.Fatalf("len = %d, want %d", len(out), len(tc.body))
				}
				return
			}
			he := mustValidation(t, err)
			fields := he.Details["fields"].(map[string]string)
			if _, ok := fields["events"]; !ok {
				t.Fatalf("details.fields.events missing: %#v", he.Details)
			}
		})
	}
}

func mustValidation(t *testing.T, err error) *httpx.Error {
	t.Helper()
	var he *httpx.Error
	if !errors.As(err, &he) {
		t.Fatalf("want *httpx.Error, got %T %v", err, err)
	}
	if he.Status != http.StatusBadRequest || he.Code != httpx.CodeValidation {
		t.Fatalf("status/code = %d/%s, want 400/validation", he.Status, he.Code)
	}
	if strings.TrimSpace(he.Message) == "" {
		t.Fatal("empty message")
	}
	return he
}

func TestParseInputs_FieldErrors(t *testing.T) {
	t.Parallel()
	body := []public.AttemptEventInput{
		evIn(1, public.AttemptEventTypeSave, nil, testNow),
		evIn(0, public.AttemptEventTypeSave, nil, testNow),                 // clientSeq < 1
		evIn(-5, public.AttemptEventTypeSave, nil, testNow),                // clientSeq < 1
		evIn(4, public.AttemptEventType("bogus"), nil, testNow),            // unknown type
		evIn(5, public.AttemptEventType(""), nil, testNow),                 // empty type
		evIn(6, public.AttemptEventTypeFieldChanged, obj("f", 1), testNow), // ok
	}
	_, err := ParseInputs(body, testNow)
	he := mustValidation(t, err)
	fields := he.Details["fields"].(map[string]string)
	want := []string{"events[1].clientSeq", "events[2].clientSeq", "events[3].type", "events[4].type"}
	for _, k := range want {
		if fields[k] == "" {
			t.Errorf("missing field error %q in %v", k, fields)
		}
	}
	if len(fields) != len(want) {
		t.Errorf("fields = %v, want exactly %v", fields, want)
	}
}

func TestParseInputs_FieldErrorsCapped(t *testing.T) {
	t.Parallel()
	body := make([]public.AttemptEventInput, 50)
	for i := range body {
		body[i] = evIn(0, public.AttemptEventTypeSave, nil, testNow)
	}
	_, err := ParseInputs(body, testNow)
	he := mustValidation(t, err)
	if n := len(he.Details["fields"].(map[string]string)); n != maxFieldErrors {
		t.Fatalf("field errors = %d, want capped at %d", n, maxFieldErrors)
	}
}

func TestParseInputs_Normalization(t *testing.T) {
	t.Parallel()
	msk := time.FixedZone("MSK", 3*3600)
	clientAt := testNow.Add(-3 * time.Second).In(msk)
	body := []public.AttemptEventInput{
		evIn(1, public.AttemptEventTypeFieldChanged, obj("field", "applicant.name", "value", "Иванов Иван Иванович"), clientAt),
		evIn(2, public.AttemptEventTypeSave, nil, testNow),
		evIn(3, public.AttemptEventTypeSave, obj(), testNow), // пустой объект — как отсутствие
	}
	out, err := ParseInputs(body, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].ClientSeq != 1 || out[0].Type != "field_changed" {
		t.Fatalf("bad input: %+v", out[0])
	}
	if out[0].At.Location() != time.UTC || !out[0].At.Equal(clientAt.Truncate(time.Microsecond)) {
		t.Fatalf("at = %v, want UTC %v", out[0].At, clientAt)
	}
	if out[0].At.Nanosecond()%1000 != 0 {
		t.Fatalf("at not truncated to µs: %v", out[0].At)
	}
	var p map[string]any
	if err := json.Unmarshal(out[0].Payload, &p); err != nil || p["value"] != "Иванов Иван Иванович" {
		t.Fatalf("payload = %s (%v)", out[0].Payload, err)
	}
	if !bytes.Contains(out[0].Payload, []byte("Иванов")) {
		t.Fatalf("payload must keep Cyrillic as is: %s", out[0].Payload)
	}
	for _, i := range []int{1, 2} {
		if string(out[i].Payload) != "{}" {
			t.Errorf("event %d payload = %s, want {}", i, out[i].Payload)
		}
		if out[i].payloadMap() != nil {
			t.Errorf("event %d payloadMap must be nil for empty payload", i)
		}
	}
	if pm := out[0].payloadMap(); pm == nil || (*pm)["field"] != "applicant.name" {
		t.Fatalf("payloadMap = %v", pm)
	}
}

func TestClampAt(t *testing.T) {
	t.Parallel()
	now := testNow
	nowUS := now.Truncate(time.Microsecond)
	cases := []struct {
		name string
		at   time.Time
		want time.Time
	}{
		{"zero -> now", time.Time{}, nowUS},
		{"recent kept", now.Add(-10 * time.Second), now.Add(-10 * time.Second).Truncate(time.Microsecond)},
		{"slightly future kept", now.Add(4 * time.Minute), now.Add(4 * time.Minute).Truncate(time.Microsecond)},
		{"far future -> now", now.Add(6 * time.Minute), nowUS},
		{"too old -> now", now.Add(-25 * time.Hour), nowUS},
		{"old but within day kept", now.Add(-23 * time.Hour), now.Add(-23 * time.Hour).Truncate(time.Microsecond)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := clampAt(tc.at, now)
			if !got.Equal(tc.want) || got.Location() != time.UTC {
				t.Fatalf("clampAt = %v, want %v UTC", got, tc.want)
			}
		})
	}
}

func TestEncodeClientPayload_Truncation(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("я", MaxPayloadBytes) // 2 байта на символ — заведомо больше лимита
	raw, o, err := encodeClientPayload(obj("field", "incident.description", "value", big))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > MaxPayloadBytes {
		t.Fatalf("stub too big: %d", len(raw))
	}
	var stub map[string]any
	if err := json.Unmarshal(raw, &stub); err != nil {
		t.Fatal(err)
	}
	if stub["truncated"] != true || stub["field"] != "incident.description" || stub["bytes"].(float64) <= MaxPayloadBytes {
		t.Fatalf("stub = %v", stub)
	}
	if o == nil || (*o)["truncated"] != true {
		t.Fatalf("obj must be the stub, got %v", o)
	}

	// Имя поля не строка или слишком длинное — в заглушку не попадает.
	raw, _, err = encodeClientPayload(obj("field", strings.Repeat("x", 300), "value", big))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"field"`)) {
		t.Fatalf("long field name must be dropped: %s", raw)
	}
	raw, _, err = encodeClientPayload(obj("field", 42, "value", big))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"field"`)) {
		t.Fatalf("non-string field must be dropped: %s", raw)
	}
}

func TestEncodeClientPayload_Unmarshalable(t *testing.T) {
	t.Parallel()
	if _, _, err := encodeClientPayload(obj("bad", func() {})); err == nil {
		t.Fatal("want marshal error")
	}
}

// esc — JSON-escape \uXXXX, собранный из частей (в исходнике таблицы остаются читаемыми).
func esc(hex string) string { return `\u` + hex }

func TestSanitizeNUL(t *testing.T) {
	t.Parallel()
	nul, rep := esc("0000"), esc("fffd")
	cases := []struct{ in, want string }{
		{`{}`, `{}`},
		{`{"a":"b"}`, `{"a":"b"}`},
		{`{"a":"x` + nul + `y"}`, `{"a":"x` + rep + `y"}`},
		{`{"a":"` + nul + nul + `"}`, `{"a":"` + rep + rep + `"}`},
		{`{"` + nul + `":"v"}`, `{"` + rep + `":"v"}`},
		{`{"a":"\\` + `u0000"}`, `{"a":"\\` + `u0000"}`},   // экранированная обратная косая — не NUL
		{`{"a":"\\` + nul + `"}`, `{"a":"\\` + rep + `"}`}, // \\ затем настоящий NUL
		{`{"a":"\"` + nul + `"}`, `{"a":"\"` + rep + `"}`}, // \" затем NUL
		{`{"a":"` + nul + `1"}`, `{"a":"` + rep + `1"}`},   // цифра после escape не съедается
		{`{"a":"Пожар` + nul + `"}`, `{"a":"Пожар` + rep + `"}`},
		{`{"a":"` + esc("0001") + `"}`, `{"a":"` + esc("0001") + `"}`}, // другие управляющие не трогаем
		{`{"a":"end` + `\` + `u000"}`, `{"a":"end` + `\` + `u000"}`},   // обрезанный escape — без паники
		{`{"a":"end` + `\` + `u00`, `{"a":"end` + `\` + `u00`},         // обрезанный JSON
	}
	for _, tc := range cases {
		got := string(sanitizeNUL([]byte(tc.in)))
		if got != tc.want {
			t.Errorf("sanitizeNUL(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestSanitizeNUL_FromMarshal(t *testing.T) {
	t.Parallel()
	raw, o, err := encodeClientPayload(obj("value", "до\x00после"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, nulEscape) {
		t.Fatalf("NUL escape survived: %s", raw)
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil || m["value"] != "до�после" {
		t.Fatalf("decoded = %v (%v)", m, err)
	}
	if o == nil {
		t.Fatal("obj must be set")
	}
}

func TestDecodeObject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		nil_ bool
	}{
		{"", true},
		{"  ", true},
		{"{}", true},
		{" {} ", true},
		{"null", true},
		{"[1]", true},
		{"{bad", true},
		{`{"a":1}`, false},
		{`{"город":"Москва"}`, false},
	}
	for _, tc := range cases {
		got := decodeObject([]byte(tc.in))
		if (got == nil) != tc.nil_ {
			t.Errorf("decodeObject(%q) = %v, want nil=%v", tc.in, got, tc.nil_)
		}
	}
}

type serverPayload struct {
	ScenarioID string `json:"scenarioId"`
	Note       string `json:"note,omitempty"`
}

func TestEncodeServerPayload(t *testing.T) {
	t.Parallel()
	var nilMap *map[string]any
	emptyMap := map[string]any{}
	cases := []struct {
		name    string
		in      any
		wantRaw string // "" — ошибка
		wantObj bool
	}{
		{"nil", nil, "{}", false},
		{"empty map", map[string]any{}, "{}", false},
		{"map", map[string]any{"reason": "Абонент положил трубку"}, `{"reason":"Абонент положил трубку"}`, true},
		{"nil *map", nilMap, "{}", false},
		{"empty *map", &emptyMap, "{}", false},
		{"*map", obj("turnNo", 2), `{"turnNo":2}`, true},
		{"raw", json.RawMessage(` {"a":1} `), `{"a":1}`, true},
		{"raw empty obj", json.RawMessage(`{}`), "{}", false},
		{"bytes", []byte(`{"b":"в"}`), `{"b":"в"}`, true},
		{"struct", serverPayload{ScenarioID: "s1"}, `{"scenarioId":"s1"}`, true},
		{"struct ptr", &serverPayload{ScenarioID: "s2", Note: "<b>"}, `{"scenarioId":"s2","note":"` + esc("003c") + "b" + esc("003e") + `"}`, true},
		{"raw array", json.RawMessage(`[1,2]`), "", false},
		{"raw null", json.RawMessage(`null`), "", false},
		{"raw garbage", json.RawMessage(`{oops`), "", false},
		{"raw trailing", json.RawMessage(`{"a":1} x`), "", false},
		{"string", "text", "", false},
		{"number", 42, "", false},
		{"slice", []int{1}, "", false},
		{"unmarshalable", map[string]any{"f": func() {}}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, o, err := encodeServerPayload(tc.in)
			if tc.wantRaw == "" {
				if err == nil {
					t.Fatalf("want error, got %s", raw)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != tc.wantRaw {
				t.Fatalf("raw = %s, want %s", raw, tc.wantRaw)
			}
			if (o != nil) != tc.wantObj {
				t.Fatalf("obj = %v, want present=%v", o, tc.wantObj)
			}
		})
	}
}

func TestEncodeServerPayload_NULDoesNotMutateCaller(t *testing.T) {
	t.Parallel()
	in := json.RawMessage(`{"a":"x\u0000"}`)
	orig := string(in)
	raw, o, err := encodeServerPayload(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(in) != orig {
		t.Fatalf("caller's slice mutated: %s", in)
	}
	if bytes.Contains(raw, nulEscape) || o == nil {
		t.Fatalf("raw = %s obj = %v", raw, o)
	}
	raw, _, err = encodeServerPayload(map[string]any{"a": "x\x00"})
	if err != nil || bytes.Contains(raw, nulEscape) {
		t.Fatalf("map NUL not sanitized: %s %v", raw, err)
	}
}

func TestRequestResult(t *testing.T) {
	t.Parallel()
	at := testNow.Truncate(time.Microsecond)
	r := &request{
		events: []Input{
			{ClientSeq: 7, Type: "save", Payload: emptyObject, At: at},
			{ClientSeq: 9, Type: "field_changed", Payload: json.RawMessage(`{"field":"a"}`), At: at},
			{ClientSeq: 8, Type: "save", Payload: emptyObject, At: at},
		},
		ids:     []int64{101, 0, 102}, // второе — дубль
		lastSeq: 4,                    // в БД меньше, чем в батче (невозможно, но результат — не ниже батча)
	}
	res := r.result()
	if res.Accepted != 2 || len(res.Inserted) != 2 {
		t.Fatalf("accepted = %d inserted = %d", res.Accepted, len(res.Inserted))
	}
	if res.LastSeq != 9 {
		t.Fatalf("LastSeq = %d, want 9 (max of batch)", res.LastSeq)
	}
	if res.Inserted[0].Id != 101 || res.Inserted[0].ClientSeq != 7 || res.Inserted[1].Id != 102 || res.Inserted[1].ClientSeq != 8 {
		t.Fatalf("inserted = %+v", res.Inserted)
	}
	if res.Inserted[0].Payload != nil {
		t.Fatal("empty payload must be omitted")
	}

	r.lastSeq = 42 // в БД есть события попытки новее батча
	if got := r.result().LastSeq; got != 42 {
		t.Fatalf("LastSeq = %d, want attempt max 42", got)
	}

	// Все дубли: Inserted — пустой срез, не nil (для WS/JSON).
	r.ids = []int64{0, 0, 0}
	res = r.result()
	if res.Inserted == nil || len(res.Inserted) != 0 || res.Accepted != 0 {
		t.Fatalf("all-dup result = %+v", res)
	}
}

// NUL в имени поля попадает и в заглушку слишком большого payload: без санитайзинга jsonb
// отверг бы весь групповой INSERT, а клиент повторял бы батч бесконечно.
func TestEncodeClientPayload_NULInTruncatedStub(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("я", MaxPayloadBytes)
	raw, o, err := encodeClientPayload(obj("field", "поле\x00", "value", big))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, nulEscape) {
		t.Fatalf("NUL escape survived in stub: %s", raw)
	}
	if o == nil || (*o)["field"] != "поле\uFFFD" || (*o)["truncated"] != true {
		t.Fatalf("obj = %v", o)
	}
}

// WS-копия события (obj) совпадает с тем, что ляжет в jsonb: NUL заменён и там, и там.
func TestPayloadObjMatchesStoredJSON(t *testing.T) {
	t.Parallel()
	cases := []*map[string]any{
		obj("value", "a\x00b"),
		obj("вложенный", map[string]any{"\x00ключ": []any{"x\x00"}}),
		obj("value", "обычный текст"),
	}
	for i, p := range cases {
		raw, o, err := encodeClientPayload(p)
		if err != nil {
			t.Fatal(err)
		}
		fromObj, _ := json.Marshal(*o)
		var stored map[string]any
		if err := json.Unmarshal(raw, &stored); err != nil {
			t.Fatal(err)
		}
		fromRaw, _ := json.Marshal(stored)
		if string(fromObj) != string(fromRaw) {
			t.Errorf("case %d: ws=%s stored=%s", i, fromObj, fromRaw)
		}

		sraw, sobj, err := encodeServerPayload(*p)
		if err != nil {
			t.Fatal(err)
		}
		sFromObj, _ := json.Marshal(*sobj)
		if string(sFromObj) != string(fromRaw) || string(sraw) != string(raw) {
			t.Errorf("server case %d: obj=%s raw=%s want %s", i, sFromObj, sraw, fromRaw)
		}
	}
}
