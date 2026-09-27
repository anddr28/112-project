package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParsePage(t *testing.T) {
	t.Parallel()
	req := func(q string) *http.Request { return httptest.NewRequest(http.MethodGet, "/x?"+q, nil) }

	p, err := ParsePage(req(""), 100, 200)
	if err != nil || p.Limit != 100 || p.Cursor != nil {
		t.Fatalf("defaults: %+v %v", p, err)
	}
	if p, _ = ParsePage(req("limit=5000"), 100, 200); p.Limit != 200 {
		t.Fatalf("limit clamped to max: %d", p.Limit)
	}
	if p, _ = ParsePage(req("limit=7"), 100, 200); p.Limit != 7 {
		t.Fatalf("limit: %d", p.Limit)
	}
	for _, bad := range []string{"limit=0", "limit=-3", "limit=abc", "limit=1.5"} {
		if _, err := ParsePage(req(bad), 100, 200); err == nil || AsError(err).Status != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %v", bad, err)
		}
	}
	c := EncodeCursor("2026-09-24T12:00:00.000001Z", "01a0d35b-3eb1-78ed-8dcd-7dab0dfb0098")
	p, err = ParsePage(req("cursor="+c), 100, 200)
	if err != nil || len(p.Cursor) != 2 || p.Cursor[0] != "2026-09-24T12:00:00.000001Z" {
		t.Fatalf("cursor: %+v %v", p, err)
	}
	for _, bad := range []string{"cursor=%21%21", "cursor=bm90LWpzb24", "cursor=W10", "cursor=" + strings.Repeat("A", 600)} {
		_, err := ParsePage(req(bad), 100, 200)
		he := AsError(err)
		if err == nil || he.Status != http.StatusBadRequest || he.Details["fields"] == nil {
			t.Fatalf("%s: want 400 with details.fields, got %v", bad, err)
		}
	}
}

func TestSetNextCursor(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	SetNextCursor(w, "a", "b")
	got := w.Header().Get(HeaderNextCursor)
	parts, ok := DecodeCursor(got)
	if !ok || len(parts) != 2 || parts[0] != "a" || parts[1] != "b" {
		t.Fatalf("X-Next-Cursor %q -> %v %v", got, parts, ok)
	}
	if strings.ContainsAny(got, "+/=") {
		t.Fatalf("cursor must be URL-safe without padding: %q", got)
	}
}
