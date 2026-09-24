package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestWriteJSON(t *testing.T) {
	t.Parallel()
	rr := httptest.NewRecorder()
	WriteJSON(rr, http.StatusCreated, map[string]any{"text": "Пожар <этаж 3> & дым", "list": []string{}})
	if rr.Code != 201 || rr.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("%d %q", rr.Code, rr.Header().Get("Content-Type"))
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Пожар <этаж 3> & дым") {
		t.Fatalf("HTML/кириллица экранированы: %s", body)
	}
	if !strings.Contains(body, `"list":[]`) {
		t.Fatalf("пустой срез: %s", body)
	}
	if rr.Header().Get("Content-Length") != fmt.Sprint(len(body)) {
		t.Fatalf("Content-Length %s vs %d", rr.Header().Get("Content-Length"), len(body))
	}
}

// Регрессия: несериализуемое значение — ApiError с application/json (раньше http.Error
// отдавал JSON-тело с Content-Type text/plain).
func TestWriteJSONEncodeError(t *testing.T) {
	t.Parallel()
	for _, v := range []any{math.NaN(), map[string]any{"ch": make(chan int)}} {
		rr := httptest.NewRecorder()
		WriteJSON(rr, http.StatusOK, v)
		if rr.Code != 500 || rr.Header().Get("Content-Type") != "application/json; charset=utf-8" {
			t.Fatalf("%v: %d %q", v, rr.Code, rr.Header().Get("Content-Type"))
		}
		var e apiErr
		if err := json.Unmarshal(rr.Body.Bytes(), &e); err != nil || e.Code != CodeInternal || e.Message == "" {
			t.Fatalf("тело: %s", rr.Body)
		}
	}
}

func TestNoContentAndRaw(t *testing.T) {
	t.Parallel()
	rr := httptest.NewRecorder()
	NoContent(rr)
	if rr.Code != 204 || rr.Body.Len() != 0 {
		t.Fatalf("204: %d %q", rr.Code, rr.Body)
	}
	rr = httptest.NewRecorder()
	WriteRawJSON(rr, 200, []byte(`[]`))
	if rr.Body.String() != "[]" || rr.Header().Get("Content-Length") != "2" {
		t.Fatalf("raw: %q", rr.Body)
	}
}

func req(method, body, ct string) *http.Request {
	r := httptest.NewRequest(method, "/x", strings.NewReader(body))
	if ct != "" {
		r.Header.Set("Content-Type", ct)
	}
	return r
}

func TestReadJSON(t *testing.T) {
	t.Parallel()
	type dst struct {
		Login string `json:"login"`
		N     int    `json:"n"`
	}
	cases := []struct {
		name, body, ct string
		limit          int64
		status         int // 0 — успех
	}{
		{"ok", `{"login":"иванов","n":1,"extra":true}`, "application/json", MaxJSONBody, 0},
		{"charset", `{"login":"a"}`, "application/json; charset=utf-8", MaxJSONBody, 0},
		{"+json", `{"login":"a"}`, "application/merge-patch+json", MaxJSONBody, 0},
		{"без Content-Type", `{"login":"a"}`, "", MaxJSONBody, 0},
		{"чужой тип", `{"login":"a"}`, "text/plain", MaxJSONBody, 415},
		{"мусорный тип", `{}`, ";;;", MaxJSONBody, 415},
		{"пустое тело", ``, "application/json", MaxJSONBody, 400},
		{"битый JSON", `{"login":`, "application/json", MaxJSONBody, 400},
		{"не тот тип поля", `{"n":"x"}`, "application/json", MaxJSONBody, 400},
		{"слишком большое", `{"login":"` + strings.Repeat("я", 100) + `"}`, "application/json", 64, 413},
	}
	for _, c := range cases {
		var d dst
		err := ReadJSONLimit(req("POST", c.body, c.ct), &d, c.limit)
		if c.status == 0 {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		var he *Error
		if !errors.As(err, &he) || he.Status != c.status || he.Code != CodeValidation || he.Message == "" {
			t.Errorf("%s: %v (want %d)", c.name, err, c.status)
		}
	}
	var d dst
	if err := ReadJSON(req("POST", `{"login":"Пётр"}`, "application/json"), &d); err != nil || d.Login != "Пётр" {
		t.Fatalf("ReadJSON: %v %+v", err, d)
	}
}

func TestReadRawJSON(t *testing.T) {
	t.Parallel()
	got, err := ReadRawJSON(req("PUT", "  {\"a\": [1, 2]}\n", "application/json"), 1024)
	if err != nil || string(got) != `{"a": [1, 2]}` {
		t.Fatalf("%q %v", got, err)
	}
	for _, body := range []string{``, `   `, `[1]`, `"x"`, `{"a":`, `{} {}`} {
		if _, err := ReadRawJSON(req("PUT", body, ""), 1024); err == nil {
			t.Errorf("%q принят", body)
		}
	}
	_, err = ReadRawJSON(req("PUT", `{"a":"`+strings.Repeat("x", 100)+`"}`, ""), 16)
	var he *Error
	if !errors.As(err, &he) || he.Status != 413 {
		t.Fatalf("лимит: %v", err)
	}
}

func TestPathAndQuery(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	r := httptest.NewRequest("GET", "/x?limit=500&neg=-3&junk=abc&u="+id.String()+"&bad=zz&t=2026-09-24T10:00:00Z&tb=вчера", nil)
	r.SetPathValue("attemptId", id.String())
	r.SetPathValue("bad", "не-uuid")
	if got, err := PathUUID(r, "attemptId"); err != nil || got != id {
		t.Fatalf("PathUUID: %v %v", got, err)
	}
	_, err := PathUUID(r, "bad")
	var he *Error
	if !errors.As(err, &he) || he.Status != 404 || he.Code != CodeNotFound {
		t.Fatalf("PathUUID мусор: %v", err)
	}
	for _, c := range []struct {
		name          string
		def, min, max int
		want          int
	}{
		{"limit", 50, 1, 200, 200},
		{"limit", 50, 1, 0, 500}, // max 0 — без потолка
		{"neg", 50, 0, 200, 0},
		{"junk", 50, 1, 200, 50},
		{"missing", 7, 1, 200, 7},
	} {
		if got := QueryInt(r, c.name, c.def, c.min, c.max); got != c.want {
			t.Errorf("QueryInt(%s,%d,%d,%d) = %d, want %d", c.name, c.def, c.min, c.max, got, c.want)
		}
	}
	if got, ok := QueryUUID(r, "u"); !ok || got != id {
		t.Fatal("QueryUUID")
	}
	if _, ok := QueryUUID(r, "bad"); ok {
		t.Fatal("QueryUUID мусор")
	}
	if _, ok := QueryUUID(r, "missing"); ok {
		t.Fatal("QueryUUID нет")
	}
	if got, ok := QueryTime(r, "t"); !ok || !got.Equal(time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("QueryTime: %v", got)
	}
	if _, ok := QueryTime(r, "tb"); ok {
		t.Fatal("QueryTime мусор")
	}
}

func TestClientIP(t *testing.T) {
	t.Parallel()
	for addr, want := range map[string]string{
		"192.0.2.1:1234":     "192.0.2.1",
		"[::1]:8443":         "::1",
		"[fe80::1%en0]:5555": "fe80::1%en0",
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = addr
		r.Header.Set("X-Forwarded-For", "10.0.0.1")
		if got := ClientIP(r); got != want {
			t.Errorf("%s -> %q, want %q", addr, got, want)
		}
	}
}

func TestErrorConstructors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		e      *Error
		status int
		code   string
	}{
		{BadRequest("x"), 400, CodeValidation},
		{Validation("x", nil), 400, CodeValidation},
		{Unauthorized(), 401, CodeUnauthorized},
		{Forbidden(""), 403, CodeForbidden},
		{NotFound(""), 404, CodeNotFound},
		{Conflict("x"), 409, CodeConflict},
		{Unprocessable("x"), 422, CodeValidation},
		{UserBlocked(), 423, CodeUserBlocked},
		{TooManyRequests("", 5), 429, CodeRateLimited},
		{AIUnavailable(""), 503, CodeAIUnavailable},
		{CallerBusy(0), 503, CodeCallerBusy},
		{AudioTooLong(), 413, CodeAudioTooLong},
		{AudioUnsupported(), 415, CodeAudioUnsupported},
		{Internal(errors.New("x")), 500, CodeInternal},
		{RequestTimeout(context.DeadlineExceeded), 503, CodeInternal},
	}
	for _, c := range cases {
		if c.e.Status != c.status || c.e.Code != c.code || c.e.Message == "" {
			t.Errorf("%+v: want %d %s", c.e, c.status, c.code)
		}
	}
	if CallerBusy(0).RetryAfter != 3 || CallerBusy(9).RetryAfter != 9 {
		t.Error("CallerBusy RetryAfter")
	}
	if Validation("x", nil).Details != nil {
		t.Error("Validation без полей — без details")
	}
	e := Validation("x", map[string]string{"a": "b"}).WithDetails(map[string]any{"n": 1})
	if e.Details["n"] != 1 || e.Details["fields"] == nil {
		t.Errorf("WithDetails: %v", e.Details)
	}
	if d := NotFound("x").WithDetails(map[string]any{"k": "v"}).Details; d["k"] != "v" {
		t.Errorf("WithDetails на пустых: %v", d)
	}
	cause := errors.New("pg down")
	w := Conflict("x").Wrap(cause)
	if !errors.Is(w, cause) || !strings.Contains(w.Error(), "pg down") || !strings.Contains(Conflict("y").Error(), "409 conflict: y") {
		t.Errorf("Wrap/Error: %v", w)
	}
	if got := AsError(fmt.Errorf("слой: %w", Forbidden("z"))); got.Status != 403 {
		t.Errorf("AsError wrapped: %+v", got)
	}
	if got := AsError(cause); got.Status != 500 || !errors.Is(got, cause) {
		t.Errorf("AsError plain: %+v", got)
	}
}

func TestWriteError(t *testing.T) {
	t.Parallel()
	rr := httptest.NewRecorder()
	WriteError(rr, TooManyRequests("Подождите", 12))
	if rr.Code != 429 || rr.Header().Get("Retry-After") != "12" {
		t.Fatalf("%d %q", rr.Code, rr.Header().Get("Retry-After"))
	}
	if rr.Body.String() != "{\"code\":\"rate_limited\",\"message\":\"Подождите\"}\n" {
		t.Fatalf("тело: %q", rr.Body)
	}
	rr = httptest.NewRecorder()
	WriteError(rr, Internal(errors.New("секрет подключения postgres://lct:lct@")))
	if strings.Contains(rr.Body.String(), "postgres") {
		t.Fatalf("внутренняя причина в ответе: %s", rr.Body)
	}
}
