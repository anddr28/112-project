package aijobs

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/callbacks"
	"lct/gocore/internal/platform/ids"
)

func decodeResult(t *testing.T, body string) callbacks.AiResult {
	t.Helper()
	var r callbacks.AiResult
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return r
}

func TestValidateEnvelope(t *testing.T) {
	t.Parallel()
	id := ids.New()
	cases := []struct {
		name    string
		body    string
		wantTyp core.JobType
		wantErr string
	}{
		{"ok semantic", okCallback(id, core.JobEvaluateSemantic), core.JobEvaluateSemantic, ""},
		{"ok grammar", okCallback(id, core.JobEvaluateGrammar), core.JobEvaluateGrammar, ""},
		{"ok dialogue", okCallback(id, core.JobEvaluateDialogue), core.JobEvaluateDialogue, ""},
		{"ok scenario", okCallback(id, core.JobGenerateScenario), core.JobGenerateScenario, ""},
		{"ok tts", okCallback(id, core.JobTTS), core.JobTTS, ""},
		{"failed with error", failedCallback(id, core.JobTTS, "tts_failed", true), core.JobTTS, ""},
		{"wrong schema version",
			`{"schema_version":"2","request_id":"` + id.String() + `","type":"tts","status":"ok","engine":{},"tts":{}}`, "", "schema_version"},
		{"no request id", `{"schema_version":"1","type":"tts","status":"ok","engine":{},"tts":{}}`, "", "request_id"},
		{"unknown type",
			`{"schema_version":"1","request_id":"` + id.String() + `","type":"translate","status":"ok","engine":{}}`, "", "тип задачи"},
		{"ok without result",
			`{"schema_version":"1","request_id":"` + id.String() + `","type":"tts","status":"ok","engine":{}}`, "", "без поля результата"},
		{"ok with two results",
			`{"schema_version":"1","request_id":"` + id.String() + `","type":"tts","status":"ok","engine":{},"tts":{},"grammar":{}}`, "", "ровно одно"},
		{"ok with foreign result",
			`{"schema_version":"1","request_id":"` + id.String() + `","type":"tts","status":"ok","engine":{},"grammar":{}}`, "", "не соответствует"},
		{"failed without error",
			`{"schema_version":"1","request_id":"` + id.String() + `","type":"tts","status":"failed","engine":{}}`, "", "без поля error"},
		{"failed with empty code",
			`{"schema_version":"1","request_id":"` + id.String() + `","type":"tts","status":"failed","engine":{},"error":{"code":"","message":"","retryable":true}}`, "", "без поля error"},
		{"unknown status",
			`{"schema_version":"1","request_id":"` + id.String() + `","type":"tts","status":"partial","engine":{},"tts":{}}`, "", "status"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := decodeResult(t, c.body)
			typ, err := validateEnvelope(&r)
			if c.wantErr == "" {
				if err != nil || typ != c.wantTyp {
					t.Fatalf("got %q, %v; want %q, nil", typ, err, c.wantTyp)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, c.wantErr)
			}
		})
	}
}

func TestSuperseded(t *testing.T) {
	t.Parallel()
	a, b := ids.New(), ids.New()
	cases := []struct {
		cur  string
		got  uuid.UUID
		want bool
	}{
		{a.String(), a, false},
		{strings.ToUpper(a.String()), a, false}, // регистр UUID не делает отправку «чужой»
		{b.String(), a, true},
		{"", a, false},
		{"не uuid", a, false},
	}
	for _, c := range cases {
		if got := superseded(c.cur, c.got); got != c.want {
			t.Errorf("superseded(%q, %s) = %v, want %v", c.cur, c.got, got, c.want)
		}
	}
}

// Отказы до обращения к БД: метод, токен, тело, конверт. Пул не нужен.
func TestCallbackHandlerRejects(t *testing.T) {
	t.Parallel()
	s := newTestService(t, nil, "")
	id := ids.New()
	valid := okCallback(id, core.JobEvaluateSemantic)
	cases := []struct {
		name   string
		method string
		token  string
		body   string
		status int
		code   string
	}{
		{"GET not allowed", http.MethodGet, testToken, "", http.StatusMethodNotAllowed, "validation"},
		{"no token", http.MethodPost, "", valid, http.StatusUnauthorized, "unauthorized"},
		{"wrong token", http.MethodPost, "test-internal-tokeN", valid, http.StatusUnauthorized, "unauthorized"},
		{"garbage JSON", http.MethodPost, testToken, `{"schema_version":`, http.StatusBadRequest, "validation"},
		{"empty body", http.MethodPost, testToken, ``, http.StatusBadRequest, "validation"},
		{"envelope violation", http.MethodPost, testToken,
			`{"schema_version":"1","request_id":"` + id.String() + `","type":"tts","status":"ok","engine":{}}`, http.StatusBadRequest, "validation"},
		{"too large", http.MethodPost, testToken,
			`{"pad":"` + strings.Repeat("я", maxCallbackBody/2+1) + `"}`, http.StatusRequestEntityTooLarge, "validation"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, "/internal/ai/v1/results", strings.NewReader(c.body))
			if c.token != "" {
				req.Header.Set("X-Internal-Token", c.token)
			}
			rec := httptest.NewRecorder()
			s.CallbackHandler().ServeHTTP(rec, req)
			if rec.Code != c.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, c.status, rec.Body)
			}
			var e struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Code != c.code || e.Message == "" {
				t.Errorf("error body = %s (code %q, want %q)", rec.Body, e.Code, c.code)
			}
			if c.status == http.StatusMethodNotAllowed && rec.Header().Get("Allow") != http.MethodPost {
				t.Errorf("Allow = %q", rec.Header().Get("Allow"))
			}
		})
	}
	if st := s.Stats(); st.Callbacks != 0 {
		t.Errorf("rejected callbacks counted as accepted: %d", st.Callbacks)
	}
}

func TestValidTokenEmptyConfig(t *testing.T) {
	t.Parallel()
	s := newTestService(t, nil, "")
	s.token = nil // INTERNAL_API_TOKEN не задан — никто не проходит, даже с пустым заголовком
	if s.validToken("") || s.validToken("anything") {
		t.Error("empty configured token must reject everything")
	}
}
