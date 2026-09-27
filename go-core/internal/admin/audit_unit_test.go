package admin

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/audit"
	"lct/gocore/internal/platform/httpx"
)

func TestParseAuditFilter(t *testing.T) {
	t.Parallel()
	msk := time.FixedZone("MSK", 3*3600)
	actor := uuid.MustParse("7d0f6f0e-8d3b-4c1e-9a55-5b3f1f0c2a11")
	entity := uuid.MustParse("0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0")
	utc := func(s string) time.Time {
		t.Helper()
		v, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	cases := []struct {
		name   string
		query  string // как в URL (без «?»): «+» декодируется в пробел
		check  func(t *testing.T, f audit.Filter)
		fields []string // ожидаемые ошибки по полям
	}{
		{name: "пусто — без фильтров", query: "", check: func(t *testing.T, f audit.Filter) {
			if f.ActorID != nil || f.EntityID != nil || f.Action != "" || f.EntityType != "" || f.From != nil ||
				f.To != nil || f.BeforeID != nil || f.Limit != 0 {
				t.Fatalf("filter = %+v", f)
			}
		}},
		{name: "все фильтры", query: "actorId=" + actor.String() + "&entityId=" + entity.String() +
			"&action=user.&entityType=lesson&beforeId=42&limit=50", check: func(t *testing.T, f audit.Filter) {
			if f.ActorID == nil || *f.ActorID != actor || f.EntityID == nil || *f.EntityID != entity {
				t.Fatalf("ids = %v %v", f.ActorID, f.EntityID)
			}
			if f.Action != "user." || f.EntityType != "lesson" || f.BeforeID == nil || *f.BeforeID != 42 || f.Limit != 50 {
				t.Fatalf("filter = %+v", f)
			}
		}},
		{name: "пробелы вокруг значений", query: "action=%20setting.update%20&actorId=%20" + actor.String(), check: func(t *testing.T, f audit.Filter) {
			if f.Action != "setting.update" || f.ActorID == nil || *f.ActorID != actor {
				t.Fatalf("filter = %+v", f)
			}
		}},
		{name: "дата без времени — местные сутки", query: "from=2026-09-24&to=2026-09-24", check: func(t *testing.T, f audit.Filter) {
			if !f.From.Equal(utc("2026-09-23T21:00:00Z")) {
				t.Errorf("from = %v", f.From)
			}
			if !f.To.Equal(utc("2026-09-24T20:59:59.999999Z")) {
				t.Errorf("to = %v", f.To)
			}
		}},
		{name: "RFC 3339 с зоной (закодированный плюс)", query: "from=" + url.QueryEscape("2026-09-24T10:00:00+03:00"), check: func(t *testing.T, f audit.Filter) {
			if !f.From.Equal(utc("2026-09-24T07:00:00Z")) || f.From.Location() != time.UTC {
				t.Fatalf("from = %v", f.From)
			}
		}},
		{name: "RFC 3339 с незакодированным плюсом (пробел)", query: "to=2026-09-24T10:00:00+03:00", check: func(t *testing.T, f audit.Filter) {
			if !f.To.Equal(utc("2026-09-24T07:00:00Z")) {
				t.Fatalf("to = %v", f.To)
			}
		}},
		{name: "дробные секунды", query: "from=2026-09-24T10:00:00.123456Z", check: func(t *testing.T, f audit.Filter) {
			if !f.From.Equal(utc("2026-09-24T10:00:00.123456Z")) {
				t.Fatalf("from = %v", f.From)
			}
		}},
		{name: "лимит больше максимума ужимается", query: "limit=100000", check: func(t *testing.T, f audit.Filter) {
			if f.Limit != audit.MaxLimit {
				t.Fatalf("limit = %d", f.Limit)
			}
		}},
		{name: "граница длины фильтра — 100 символов допустимо", query: "entityType=" + strings.Repeat("x", maxFilterText),
			check: func(t *testing.T, f audit.Filter) {
				if len(f.EntityType) != maxFilterText {
					t.Fatalf("entityType len = %d", len(f.EntityType))
				}
			}},
		{name: "некорректный UUID", query: "actorId=abc&entityId=123", fields: []string{"actorId", "entityId"}},
		{name: "слишком длинные строки", query: "action=" + strings.Repeat("a", maxFilterText+1) + "&entityType=" + strings.Repeat("б", 51),
			fields: []string{"action", "entityType"}},
		{name: "дата по-русски", query: "from=" + url.QueryEscape("вчера") + "&to=24.09.2026", fields: []string{"from", "to"}},
		{name: "начало позже конца", query: "from=2026-09-25&to=2026-09-24", fields: []string{"from"}},
		{name: "курсор не число", query: "beforeId=abc", fields: []string{"beforeId"}},
		{name: "курсор ноль", query: "beforeId=0", fields: []string{"beforeId"}},
		{name: "курсор отрицательный", query: "beforeId=-5", fields: []string{"beforeId"}},
		{name: "лимит ноль", query: "limit=0", fields: []string{"limit"}},
		{name: "лимит не число", query: "limit=ten", fields: []string{"limit"}},
		{name: "все ошибки сразу", query: "actorId=x&limit=-1&beforeId=x&from=x", fields: []string{"actorId", "limit", "beforeId", "from"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			f, err := parseAuditFilter(q, msk)
			if len(tc.fields) > 0 {
				var he *httpx.Error
				if !errors.As(err, &he) || he.Status != 400 || he.Code != httpx.CodeValidation {
					t.Fatalf("err = %v, want 400 validation", err)
				}
				fields, _ := he.Details["fields"].(map[string]string)
				for _, name := range tc.fields {
					if fields[name] == "" {
						t.Errorf("нет ошибки поля %s: %v", name, fields)
					}
				}
				if len(fields) != len(tc.fields) {
					t.Errorf("fields = %v, want only %v", fields, tc.fields)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tc.check(t, f)
		})
	}
}

func TestParseTime(t *testing.T) {
	t.Parallel()
	// nil-зона — UTC
	got, ok := parseTime("2026-01-02", nil, false)
	if !ok || !got.Equal(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("parseTime(date, nil) = %v %v", got, ok)
	}
	got, ok = parseTime("2026-01-02", nil, true)
	if !ok || !got.Equal(time.Date(2026, 1, 2, 23, 59, 59, 999999000, time.UTC)) {
		t.Fatalf("parseTime(date, end) = %v %v", got, ok)
	}
	// RFC 3339 с зоной не зависит от loc и от endOfDay
	vlad := time.FixedZone("VLAT", 10*3600)
	got, ok = parseTime("2026-01-02T03:04:05-05:00", vlad, true)
	if !ok || !got.Equal(time.Date(2026, 1, 2, 8, 4, 5, 0, time.UTC)) {
		t.Fatalf("parseTime(rfc3339) = %v %v", got, ok)
	}
	for _, bad := range []string{"", " ", "2026-13-01", "2026-02-30", "2026-09-24 10:00:00", "2026-09-24T10:00", "сегодня"} {
		if _, ok := parseTime(bad, vlad, false); ok {
			t.Errorf("parseTime(%q) принят", bad)
		}
	}
}
