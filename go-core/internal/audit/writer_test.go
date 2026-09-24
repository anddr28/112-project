package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// decodeMap — JSON-объект из EncodeValue (числа — json.Number, как в redact).
func decodeMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("не объект JSON: %q: %v", b, err)
	}
	return m
}

func TestEncodeValue(t *testing.T) {
	t.Parallel()
	type card struct {
		Address string `json:"address"`
		Floor   int    `json:"floor"`
	}
	tests := []struct {
		name string
		in   any
		want string // точный JSON; "" — nil
	}{
		{"nil", nil, ""},
		{"raw null", json.RawMessage("null"), ""},
		{"raw spaces", json.RawMessage("  \n "), ""},
		{"raw null padded", json.RawMessage(" null "), ""},
		{"bytes empty", []byte{}, ""},
		{"map", map[string]any{"a": 1}, `{"a":1}`},
		{"struct russian", card{Address: "ул. Ленина, д. 14 <кв. 5>", Floor: 3}, `{"address":"ул. Ленина, д. 14 <кв. 5>","floor":3}`},
		{"string wrapped", "строка", `{"value":"строка"}`},
		{"number wrapped", 42, `{"value":42}`},
		{"slice wrapped", []int{1, 2}, `{"value":[1,2]}`},
		{"raw array wrapped", json.RawMessage(`[1, 2]`), `{"value":[1, 2]}`},
		{"raw object trimmed", json.RawMessage("  {\"a\":true}\n"), `{"a":true}`},
		{"bytes not json", []byte("просто текст"), `{"value":"просто текст"}`},
		{"bytes json", []byte(`{"k":"v"}`), `{"k":"v"}`},
		{"NUL escape replaced", json.RawMessage(`{"a":"x\u0000y"}`), `{"a":"x\ufffdy"}`},
		{"escaped backslash kept", json.RawMessage(`{"a":"\\u0000"}`), `{"a":"\\u0000"}`},
		{"marshal NUL replaced", map[string]string{"a": "x\x00y"}, `{"a":"x\ufffdy"}`},
		{"surrogate pair normalized", json.RawMessage(`{"a":"\ud83d\ude00"}`), `{"a":"😀"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := EncodeValue(tt.in)
			if err != nil {
				t.Fatalf("EncodeValue: %v", err)
			}
			if tt.want == "" {
				if got != nil {
					t.Fatalf("want nil, got %q", got)
				}
				return
			}
			if string(got) != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestEncodeValueRedactsSecrets(t *testing.T) {
	t.Parallel()
	in := map[string]any{
		"login":          "ivanov",
		"password":       "p@ss",
		"passwordHash":   "$argon2id$...",
		"pass_threshold": 70,
		"passBonus":      1,
		"sessionId":      "abc",
		"id":             int64(9007199254740993), // > 2^53: точность не теряется при разборе
		"nested": map[string]any{
			"refresh_token_hash": "h",
			"list":               []any{map[string]any{"apiKey": "k", "name": "Иван"}},
		},
		"user_session": "s",
	}
	b, err := EncodeValue(in)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b)
	for _, k := range []string{"password", "passwordHash", "user_session"} {
		if m[k] != Redacted {
			t.Errorf("%s = %v, want redacted", k, m[k])
		}
	}
	for k, want := range map[string]any{"login": "ivanov", "sessionId": "abc"} {
		if m[k] != want {
			t.Errorf("%s = %v, want %v", k, m[k], want)
		}
	}
	if n, ok := m["pass_threshold"].(json.Number); !ok || n.String() != "70" {
		t.Errorf("pass_threshold = %v", m["pass_threshold"])
	}
	if n, ok := m["id"].(json.Number); !ok || n.String() != "9007199254740993" {
		t.Errorf("id потерял точность: %v", m["id"])
	}
	nested := m["nested"].(map[string]any)
	if nested["refresh_token_hash"] != Redacted {
		t.Errorf("nested token = %v", nested["refresh_token_hash"])
	}
	item := nested["list"].([]any)[0].(map[string]any)
	if item["apiKey"] != Redacted || item["name"] != "Иван" {
		t.Errorf("list item = %v", item)
	}
	if strings.Contains(string(b), "p@ss") || strings.Contains(string(b), "argon2") {
		t.Fatalf("секрет попал в журнал: %s", b)
	}
}

func TestEncodeValueRawSecretsRedacted(t *testing.T) {
	t.Parallel()
	b, err := EncodeValue(json.RawMessage(`{"token":"t","value":{"Authorization":"Bearer x"}}`))
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b)
	if m["token"] != Redacted || m["value"].(map[string]any)["Authorization"] != Redacted {
		t.Fatalf("got %s", b)
	}
}

// Регрессия: сырые байты с невалидным UTF-8 или одиночным суррогатом раньше уходили в COPY
// как есть — PostgreSQL отвергает такой jsonb, и падал весь батч (до 256 чужих записей).
func TestEncodeValueNormalizesInvalidJSONText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   any
		want string // значение поля "a" после нормализации
	}{
		{"invalid utf8 raw", json.RawMessage("{\"a\":\"x\xffy\"}"), "x�y"},
		{"invalid utf8 bytes", []byte("{\"a\":\"\xc3\"}"), "�"},
		{"lone high surrogate", json.RawMessage(`{"a":"\ud800"}`), "�"},
		{"lone low surrogate upper", json.RawMessage(`{"a":"z\uDC00"}`), "z�"},
		{"nested raw in struct", struct {
			A json.RawMessage `json:"a"`
		}{json.RawMessage("\"\xff\"")}, "�"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, err := EncodeValue(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if !utf8.Valid(b) || hasSurrogateEscape(b) {
				t.Fatalf("не нормализовано: %q", b)
			}
			if got := decodeMap(t, b)["a"]; got != tt.want {
				t.Fatalf("a = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEncodeValueTruncatesHuge(t *testing.T) {
	t.Parallel()
	b, err := EncodeValue(map[string]string{"text": strings.Repeat("я", MaxValueBytes)})
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMap(t, b)
	if m["truncated"] != true {
		t.Fatalf("got %s", b)
	}
	if n, _ := m["bytes"].(json.Number).Int64(); n <= MaxValueBytes {
		t.Fatalf("bytes = %d", n)
	}
}

func TestEncodeValueUnmarshalable(t *testing.T) {
	t.Parallel()
	if _, err := EncodeValue(map[string]any{"ch": make(chan int)}); err == nil {
		t.Fatal("want error")
	}
}

func TestSensitiveKey(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		"password": true, "Password": true, "new_password": true, "passwordHash": true, "PASSWD": true,
		"passphrase": true, "client_secret": true, "refresh_token_hash": true, "accessToken": true,
		"Cookie": true, "text_hash": true, "credentials": true, "api_key": true, "apiKey": true,
		"private-key": true, "Authorization": true,
		"pass": true, "newPass": true, "USER_PASS": true, "pwd": true, "user_pwd": true,
		"session": true, "user_session": true, "userSession": true,
		"pass_threshold": false, "passBonus": false, "sessionId": false, "session_id": false,
		"login": false, "author": false, "key": false, "value": false, "status": false, "": false,
		"passport": false, "компания": false,
	}
	for k, want := range tests {
		if got := SensitiveKey(k); got != want {
			t.Errorf("SensitiveKey(%q) = %v, want %v", k, got, want)
		}
	}
}

func TestLastWord(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"pass": "pass", "newPass": "pass", "new_pass": "pass", "new-pass": "pass", "USER_PASS": "pass",
		"PASS": "pass", "passThreshold": "threshold", "sessionId": "id", "user_session_": "session",
		"HTTPSession": "session", "x": "x", "": "", "__": "", "a1": "a1",
	}
	for in, want := range tests {
		if got := lastWord(in); got != want {
			t.Errorf("lastWord(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanText(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("ж", 150) // 300 байт
	tests := []struct {
		name, in string
		max      int
		want     string
	}{
		{"empty", "", 10, ""},
		{"trim", "  user.login \n", 200, "user.login"},
		{"nul removed", "a\x00b", 200, "ab"},
		{"invalid utf8", "a\xffb", 200, "a�b"},
		{"cut on rune boundary", long, 201, strings.Repeat("ж", 100)},
		{"cut exact", "abcdef", 3, "abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := cleanText(tt.in, tt.max)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
			if !utf8.ValidString(got) || len(got) > tt.max {
				t.Fatalf("invalid result %q", got)
			}
		})
	}
}

func TestReplaceNULEscapes(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		`"\u0000"`:            `"\ufffd"`,
		`"\\u0000"`:           `"\\u0000"`,
		`"\\\u0000"`:          `"\\\ufffd"`,
		`"a\u00001\u0000"`:    `"a\ufffd1\ufffd"`,
		`"\u00001"`:           `"\ufffd1"`,
		`"\u000"`:             `"\u000"`,
		`{"k":"\n\u0000\t"}`:  `{"k":"\n\ufffd\t"}`,
		`"нет escape вообще"`: `"нет escape вообще"`,
	}
	for in, want := range tests {
		b := []byte(in)
		replaceNULEscapes(b)
		if string(b) != want {
			t.Errorf("replaceNULEscapes(%s) = %s, want %s", in, b, want)
		}
	}
}

func TestHasSurrogateEscape(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		`"\ud800"`: true, `"\uDBFF"`: true, `"\udc00"`: true, `"\uDfFf"`: true,
		`"\ud7ff"`: false, `"\ue000"`: false, `"\\ud800"`: false, `"\\\ud800"`: true,
		`"ud800"`: false, `""`: false, `"\u00e9"`: false,
	}
	for in, want := range tests {
		if got := hasSurrogateEscape([]byte(in)); got != want {
			t.Errorf("hasSurrogateEscape(%s) = %v, want %v", in, got, want)
		}
	}
}

func TestBuildRow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.FixedZone("MSK", 3*3600))
	user := uuid.MustParse("01920000-0000-7000-8000-000000000001")
	other := uuid.MustParse("01920000-0000-7000-8000-000000000002")
	reqID := uuid.MustParse("01920000-0000-7000-8000-0000000000ff")
	teacher := &core.Principal{UserID: user, Role: core.RoleTeacher}
	meta := core.RequestMeta{RequestID: reqID, IP: " 10.1.2.3 ", UserAgent: "Mozilla/5.0"}
	withMeta := core.WithRequestMeta(context.Background(), meta)

	tests := []struct {
		name     string
		ctx      context.Context
		e        core.AuditEntry
		actor    uuid.UUID
		role     string
		ip       string
		hasReqID bool
	}{
		{"system flag", core.WithPrincipal(withMeta, teacher), core.AuditEntry{Action: "backup.run", ActorSystem: true},
			uuid.Nil, RoleSystem, "10.1.2.3", true},
		{"explicit actor with role", context.Background(), core.AuditEntry{Action: "user.login", ActorID: &other, ActorRole: core.RoleStudent},
			other, "student", "", false},
		{"explicit actor role from principal", core.WithPrincipal(withMeta, teacher), core.AuditEntry{Action: "x.y", ActorID: &user},
			user, "teacher", "10.1.2.3", true},
		{"explicit actor other than principal", core.WithPrincipal(withMeta, teacher), core.AuditEntry{Action: "x.y", ActorID: &other},
			other, "", "10.1.2.3", true},
		{"nil explicit actor falls to principal", core.WithPrincipal(withMeta, teacher), core.AuditEntry{Action: "x.y", ActorID: &uuid.Nil},
			user, "teacher", "10.1.2.3", true},
		{"principal", core.WithPrincipal(context.Background(), teacher), core.AuditEntry{Action: "x.y"},
			user, "teacher", "", false},
		{"background is system", context.Background(), core.AuditEntry{Action: "x.y"},
			uuid.Nil, RoleSystem, "", false},
		{"http without session", withMeta, core.AuditEntry{Action: "user.login_failed", ActorRole: core.RoleAdmin},
			uuid.Nil, "admin", "10.1.2.3", true},
		{"http without session no role", withMeta, core.AuditEntry{Action: "user.login_failed"},
			uuid.Nil, "", "10.1.2.3", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := buildRow(tt.ctx, tt.e, now)
			if !r.at.Equal(now) || r.at.Location() != time.UTC {
				t.Errorf("at = %v", r.at)
			}
			if r.actorID != tt.actor || r.actorRole != tt.role {
				t.Errorf("actor = %v/%q, want %v/%q", r.actorID, r.actorRole, tt.actor, tt.role)
			}
			gotIP := ""
			if r.ip.IsValid() {
				gotIP = r.ip.String()
			}
			if gotIP != tt.ip {
				t.Errorf("ip = %q, want %q", gotIP, tt.ip)
			}
			if (r.requestID == reqID) != tt.hasReqID {
				t.Errorf("requestID = %v", r.requestID)
			}
		})
	}
}

func TestBuildRowCleansInput(t *testing.T) {
	t.Parallel()
	ua := strings.Repeat("Яндекс.Браузер ", 60) // > 512 байт, кириллица
	ctx := core.WithRequestMeta(context.Background(), core.RequestMeta{IP: "fe80::1%en0", UserAgent: ua})
	r := buildRow(ctx, core.AuditEntry{
		Action:     "  user.\x00login" + strings.Repeat("x", 300),
		EntityType: "user\xff",
	}, time.Now())
	if r.ip != netip.MustParseAddr("fe80::1") {
		t.Errorf("ip = %v (зона должна быть снята)", r.ip)
	}
	if len(r.userAgent) > maxUA || !utf8.ValidString(r.userAgent) || !strings.HasPrefix(ua, r.userAgent) {
		t.Errorf("user agent = %q", r.userAgent)
	}
	if len(r.action) != maxText || !strings.HasPrefix(r.action, "user.login") {
		t.Errorf("action = %q", r.action)
	}
	if r.entityType != "user�" {
		t.Errorf("entityType = %q", r.entityType)
	}

	bad := buildRow(core.WithRequestMeta(context.Background(), core.RequestMeta{IP: "not-an-ip"}), core.AuditEntry{Action: "a"}, time.Now())
	if bad.ip.IsValid() {
		t.Errorf("ip = %v, want invalid", bad.ip)
	}
}

func TestRowSourceNulls(t *testing.T) {
	t.Parallel()
	actor := uuid.New()
	rows := []*row{
		{at: time.Unix(1, 0).UTC(), action: "a"},
		{at: time.Unix(2, 0).UTC(), action: "b", actorID: actor, actorRole: "teacher", entityType: "user",
			entityID: actor, lessonID: actor, before: []byte(`{"x":1}`), after: []byte(`{}`),
			ip: netip.MustParseAddr("10.0.0.1"), userAgent: "ua", requestID: actor},
	}
	src := &rowSource{rows: rows, i: -1}
	if !src.Next() {
		t.Fatal("Next")
	}
	v, _ := src.Values()
	for i, x := range v {
		if i == 0 || i == 3 {
			continue
		}
		if x != nil {
			t.Errorf("col %d (%s) = %v, want nil", i, auditColumns[i], x)
		}
	}
	if !src.Next() {
		t.Fatal("Next 2")
	}
	v, _ = src.Values()
	for i, x := range v {
		if x == nil {
			t.Errorf("col %d (%s) = nil", i, auditColumns[i])
		}
	}
	if src.Next() || src.Err() != nil {
		t.Fatal("источник должен закончиться")
	}
}

func TestLogQueueing(t *testing.T) {
	t.Parallel()
	w := NewWriter(nil, discardLog()) // без Start: записи копятся в очереди
	w.Log(context.Background(), core.AuditEntry{Action: ""})
	if len(w.ch) != 0 {
		t.Fatal("пустой action не должен попадать в очередь")
	}
	w.Log(context.Background(), core.AuditEntry{Action: "user.create", After: map[string]any{"password": "x"}})
	if len(w.ch) != 1 {
		t.Fatalf("queue = %d", len(w.ch))
	}
	r := <-w.ch
	if strings.Contains(string(r.after), `"x"`) {
		t.Fatalf("секрет в очереди: %s", r.after)
	}

	// JSON, который не сериализуется, не роняет Log: before = NULL
	w.Log(context.Background(), core.AuditEntry{Action: "x.y", Before: make(chan int)})
	r = <-w.ch
	if r.before != nil {
		t.Fatalf("before = %s", r.before)
	}
}

func TestLogQueueFullDrops(t *testing.T) {
	t.Parallel()
	w := NewWriter(nil, discardLog())
	for range queueCap + 5 {
		w.Log(context.Background(), core.AuditEntry{Action: "x.y"})
	}
	if len(w.ch) != queueCap {
		t.Fatalf("queue = %d", len(w.ch))
	}
	if got := w.dropped.Load(); got != 5 {
		t.Fatalf("dropped = %d, want 5", got)
	}
}

// Регрессия: at проставляется под тем же мьютексом, что и постановка в очередь, и не
// убывает при шаге часов назад — иначе порядок at расходится с порядком id и курсор
// beforeId в GET /admin/audit пропускает записи.
func TestLogAtMonotonicInQueueOrder(t *testing.T) {
	t.Parallel()
	w := NewWriter(nil, discardLog())
	base := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	ticks := []struct{ clock, want time.Time }{
		{base, base},
		{base.Add(-time.Second), base},                 // шаг назад — at не убывает
		{base.Add(-maxAtClamp), base},                  // на границе допуска — тоже
		{base.Add(time.Second), base.Add(time.Second)}, // часы пошли вперёд
		{base.Add(time.Millisecond), base.Add(time.Second)},
		// часы стояли в будущем и их исправили на час: принимаем, а не «замораживаем» время
		{base.Add(-time.Hour), base.Add(-time.Hour)},
		{base.Add(-time.Hour + time.Millisecond), base.Add(-time.Hour + time.Millisecond)},
	}
	i := 0
	w.now = func() time.Time { c := ticks[i].clock; i++; return c }
	for range ticks {
		w.Log(context.Background(), core.AuditEntry{Action: "x.y"})
	}
	for k, tk := range ticks {
		r := <-w.ch
		if !r.at.Equal(tk.want) {
			t.Fatalf("запись %d: at = %v, want %v", k, r.at, tk.want)
		}
	}
}

func TestLogAfterStopIsDropped(t *testing.T) {
	t.Parallel()
	w := NewWriter(nil, discardLog())
	w.stopping.Store(true)
	w.Log(context.Background(), core.AuditEntry{Action: "x.y"})
	if len(w.ch) != 0 || w.dropped.Load() != 1 {
		t.Fatalf("queue=%d dropped=%d", len(w.ch), w.dropped.Load())
	}
}

func TestDecodeObject(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want string // JSON результата; "" — nil
	}{
		{"", ""},
		{"null", ""},
		{"  ", ""},
		{`{"a":1}`, `{"a":1}`},
		{`[1,2]`, `{"value":[1,2]}`},
		{`"строка"`, `{"value":"строка"}`},
		{`{broken`, ""},
		{`[broken`, ""},
		{`{}`, `{}`},
	}
	for _, tt := range tests {
		got := decodeObject([]byte(tt.in))
		if tt.want == "" {
			if got != nil {
				t.Errorf("decodeObject(%q) = %v, want nil", tt.in, *got)
			}
			continue
		}
		if got == nil {
			t.Errorf("decodeObject(%q) = nil", tt.in)
			continue
		}
		b, _ := json.Marshal(*got)
		if string(b) != tt.want {
			t.Errorf("decodeObject(%q) = %s, want %s", tt.in, b, tt.want)
		}
	}
}

func TestPartitionUpper(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want time.Time
		ok   bool
	}{
		{"audit_log_2026_09", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), true},
		{"audit_log_2026_12", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), true},
		{"audit_log_2026_13", time.Time{}, false},
		{"audit_log_2026_00", time.Time{}, false},
		{"audit_log_default", time.Time{}, false},
		{"audit_log_2026_9", time.Time{}, false},
		{"xaudit_log_2026_09", time.Time{}, false},
	}
	for _, tt := range tests {
		got, ok := partitionUpper(tt.name)
		if ok != tt.ok || !got.Equal(tt.want) {
			t.Errorf("partitionUpper(%q) = %v,%v want %v,%v", tt.name, got, ok, tt.want, tt.ok)
		}
	}
}

func TestMonthsFrom(t *testing.T) {
	t.Parallel()
	// 1 ноября 01:00 MSK — в UTC ещё 31 октября: месяц считается по UTC
	now := time.Date(2026, 11, 1, 1, 0, 0, 0, time.FixedZone("MSK", 3*3600))
	got := monthsFrom(now, 3)
	want := []string{"audit_log_2026_10", "audit_log_2026_11", "audit_log_2026_12", "audit_log_2027_01"}
	if len(got) != len(want) {
		t.Fatalf("got %d months", len(got))
	}
	for i, m := range got {
		if m.name != want[i] {
			t.Errorf("month %d = %s, want %s", i, m.name, want[i])
		}
		if m.lo.Day() != 1 || m.lo.Hour() != 0 || m.lo.Location() != time.UTC || !m.hi.Equal(m.lo.AddDate(0, 1, 0)) {
			t.Errorf("bounds %v .. %v", m.lo, m.hi)
		}
		if i > 0 && !m.lo.Equal(got[i-1].hi) {
			t.Errorf("месяцы не стыкуются: %v != %v", m.lo, got[i-1].hi)
		}
	}
}

func TestSchemeOffset(t *testing.T) {
	t.Parallel()
	msk := time.FixedZone("MSK", 3*3600)
	part := func(name string, y int, m time.Month, loc *time.Location) partInfo {
		lo := time.Date(y, m, 1, 0, 0, 0, 0, loc)
		return partInfo{name: name, lo: lo.UTC(), hi: lo.AddDate(0, 1, 0).UTC()}
	}
	tests := []struct {
		name  string
		parts []partInfo
		want  time.Duration
	}{
		{"none", nil, 0},
		{"default only", []partInfo{{name: "audit_log_default"}}, 0},
		{"utc", []partInfo{part("audit_log_2026_09", 2026, 9, time.UTC)}, 0},
		{"msk latest wins", []partInfo{
			part("audit_log_2026_09", 2026, 9, time.UTC),
			part("audit_log_2026_10", 2026, 10, msk),
			{name: "audit_log_default"},
		}, -3 * time.Hour},
		{"west", []partInfo{part("audit_log_2026_09", 2026, 9, time.FixedZone("EST", -5*3600))}, 5 * time.Hour},
		{"nonsense ignored", []partInfo{{name: "audit_log_2026_09", lo: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), hi: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}}, 0},
	}
	for _, tt := range tests {
		if got := schemeOffset(tt.parts); got != tt.want {
			t.Errorf("%s: schemeOffset = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestBoundLiteral(t *testing.T) {
	t.Parallel()
	msk := time.FixedZone("MSK", 3*3600)
	tests := map[time.Time]string{
		time.Date(2026, 10, 1, 0, 0, 0, 0, msk):           "'2026-09-30 21:00:00+00'",
		time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC):      "'2026-10-01 00:00:00+00'",
		time.Date(2026, 10, 1, 0, 0, 0, 1500, time.UTC):   "'2026-10-01 00:00:00.000001+00'",
		time.Date(2026, 10, 1, 0, 0, 0, 250000, time.UTC): "'2026-10-01 00:00:00.00025+00'",
	}
	for in, want := range tests {
		if got := boundLiteral(in); got != want {
			t.Errorf("boundLiteral(%v) = %s, want %s", in, got, want)
		}
	}
}
