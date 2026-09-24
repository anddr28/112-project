package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/callbacks"
	"lct/gocore/internal/gen/components"
)

const cbWait = 5 * time.Second

// deleteKey — значение для mutate: удалить ключ, а не записать null.
var deleteKey = &struct{}{}

// mutate — копия payload'а с изменённым полем по пути "a.b.0.c".
func mutate(t *testing.T, o obj, path string, v any) obj {
	t.Helper()
	o = clone(t, o)
	keys := strings.Split(path, ".")
	var cur any = o
	for i, k := range keys {
		last := i == len(keys)-1
		switch c := cur.(type) {
		case map[string]any:
			if last {
				if v == deleteKey {
					delete(c, k)
				} else {
					c[k] = v
				}
				return o
			}
			cur = c[k]
		case []any:
			idx, err := strconv.Atoi(k)
			if err != nil || idx >= len(c) {
				t.Fatalf("mutate %s: плохой индекс %q", path, k)
			}
			if last {
				c[idx] = v
				return o
			}
			cur = c[idx]
		default:
			t.Fatalf("mutate %s: %q не контейнер", path, k)
		}
	}
	return o
}

// ------------------------------------------------------------------ health, auth, routing

func TestHealthIsPublicAndComplete(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	for _, tok := range []string{"", "wrong", testToken} {
		r := h.request(t, http.MethodGet, "/v1/health", "", nil, tok)
		if r.status != http.StatusOK {
			t.Fatalf("health (token %q): %d", tok, r.status)
		}
		var hl aiservice.Health
		if err := json.Unmarshal(r.body, &hl); err != nil {
			t.Fatal(err)
		}
		if hl.Status != aiservice.Ok || !deref(hl.Ollama) || !deref(hl.Languagetool) || !deref(hl.Tts) || !deref(hl.Stt) {
			t.Fatalf("health: %s", r.body)
		}
		if hl.Profiles == nil || len(*hl.Profiles) != 7 {
			t.Fatalf("profiles: %v", hl.Profiles)
		}
		for _, p := range []components.Profile{components.EvalFast, components.EvalThorough, components.EvalDialogue,
			components.Generate, components.DialogFast, components.TtsDefault, components.SttDefault} {
			if (*hl.Profiles)[string(p)] == "" {
				t.Fatalf("нет профиля %s", p)
			}
		}
		if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("Content-Type %q", ct)
		}
	}
}

func TestProtectedRoutesRequireToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	routes := []struct{ method, path string }{
		{http.MethodGet, "/v1/queue"},
		{http.MethodPost, "/v1/jobs/grammar"},
		{http.MethodPost, "/v1/jobs/semantic"},
		{http.MethodPost, "/v1/jobs/dialogue"},
		{http.MethodPost, "/v1/jobs/generate"},
		{http.MethodPost, "/v1/jobs/tts"},
		{http.MethodPost, "/v1/dialog/turn"},
		{http.MethodPost, "/v1/stt/transcribe"},
		{http.MethodPost, "/v1/tts/sync"},
	}
	for _, rt := range routes {
		for _, tok := range []string{"", "wrong-token", testToken + "x"} {
			r := h.request(t, rt.method, rt.path, "application/json", mustJSON(t, grammarPayload()), tok)
			if r.status != http.StatusUnauthorized {
				t.Fatalf("%s %s token=%q: %d, ждали 401", rt.method, rt.path, tok, r.status)
			}
			if e := r.apiErr(t); e.Code != "unauthorized" {
				t.Fatalf("code %q", e.Code)
			}
		}
	}
	h.sink.none(t, 50*time.Millisecond)
}

func TestUnknownRoutesAre404(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/v1/jobs/unknown"},
		{http.MethodPost, "/v1/jobs/"},
		{http.MethodGet, "/nope"},
		{http.MethodPost, "/v2/jobs/grammar"},
	} {
		r := h.request(t, c.method, c.path, "application/json", mustJSON(t, grammarPayload()), testToken)
		if r.status != http.StatusNotFound {
			t.Fatalf("%s %s: %d", c.method, c.path, r.status)
		}
		if e := r.apiErr(t); e.Code != "not_found" {
			t.Fatalf("code %q", e.Code)
		}
	}
}

// ------------------------------------------------------------------ валидация задач

func TestSubmitJobValidation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, nil)

	type tc struct {
		name string
		kind string
		body any // obj или сырые байты
	}
	var cases []tc
	add := func(kind string, base obj, name, path string, v any) {
		cases = append(cases, tc{kind + "/" + name, kind, mutate(t, base, path, v)})
	}
	bases := map[string]obj{
		"grammar": grammarPayload(), "semantic": semanticPayload(), "dialogue": dialoguePayload(),
		"generate": generatePayload(), "tts": ttsPayload(),
	}
	for kind, base := range bases {
		add(kind, base, "no schema_version", "schema_version", deleteKey)
		add(kind, base, "schema_version 2", "schema_version", "2")
		add(kind, base, "schema_version number", "schema_version", 1)
		add(kind, base, "schema_version null", "schema_version", nil)
		add(kind, base, "no request_id", "request_id", deleteKey)
		add(kind, base, "request_id nil uuid", "request_id", uuid.Nil.String())
		add(kind, base, "request_id not uuid", "request_id", "abc")
		add(kind, base, "priority 0", "priority", 0)
		add(kind, base, "priority 10", "priority", 10)
		for _, raw := range []string{`[]`, `null`, `"x"`, `{`, ``, `42`} {
			cases = append(cases, tc{kind + "/raw " + raw, kind, []byte(raw)})
		}
	}
	g := bases["grammar"]
	add("grammar", g, "texts null", "texts", nil)
	add("grammar", g, "texts missing", "texts", deleteKey)
	add("grammar", g, "texts empty", "texts", []any{})
	add("grammar", g, "texts not array", "texts", "text")
	add("grammar", g, "empty field", "texts.0.field", " ")
	add("grammar", g, "attempt_id null", "attempt_id", nil)
	add("grammar", g, "attempt_id nil uuid", "attempt_id", uuid.Nil.String())

	s := bases["semantic"]
	add("semantic", s, "free_text_fields null", "free_text_fields", nil)
	add("semantic", s, "free_text_fields missing", "free_text_fields", deleteKey)
	add("semantic", s, "etalon null", "etalon", nil)
	add("semantic", s, "etalon.card null", "etalon.card", nil)
	add("semantic", s, "etalon not object", "etalon", []any{})
	add("semantic", s, "answer null", "answer", nil)
	add("semantic", s, "answer not object", "answer", "x")
	add("semantic", s, "mode bogus", "mode", "both")
	add("semantic", s, "mode null", "mode", nil)
	add("semantic", s, "profile bogus", "profile", "gpt-5")
	add("semantic", s, "call_script turns empty", "call_script.turns", []any{})
	add("semantic", s, "call_script turns null", "call_script.turns", nil)
	add("semantic", s, "call_script caller null", "call_script.caller", nil)
	add("semantic", s, "attempt_id nil uuid", "attempt_id", uuid.Nil.String())

	d := bases["dialogue"]
	add("dialogue", d, "transcript null", "transcript", nil)
	add("dialogue", d, "transcript empty", "transcript", []any{})
	add("dialogue", d, "transcript speaker bogus", "transcript.1.speaker", "dispatcher")
	add("dialogue", d, "transcript turn_no 0", "transcript.1.turn_no", 0)
	add("dialogue", d, "transcript source bogus", "transcript.0.source", "tts")
	add("dialogue", d, "call_script null", "call_script", nil)
	add("dialogue", d, "call_script.address null", "call_script.address", nil)
	add("dialogue", d, "etalon.expected_dialogue null", "etalon.expected_dialogue", nil)
	add("dialogue", d, "checklist empty", "etalon.expected_dialogue.checklist", []any{})
	add("dialogue", d, "checklist null", "etalon.expected_dialogue.checklist", nil)
	add("dialogue", d, "checklist kind bogus", "etalon.expected_dialogue.checklist.0.kind", "quiz")
	add("dialogue", d, "checklist id empty", "etalon.expected_dialogue.checklist.0.id", "")
	add("dialogue", d, "profile bogus", "profile", "x")
	add("dialogue", d, "attempt_id missing", "attempt_id", deleteKey)

	gen := bases["generate"]
	add("generate", gen, "spec null", "spec", nil)
	add("generate", gen, "spec.category null", "spec.category", nil)
	add("generate", gen, "spec.category.code empty", "spec.category.code", " ")
	add("generate", gen, "spec.category.name empty", "spec.category.name", "")
	add("generate", gen, "difficulty 0", "spec.difficulty", 0)
	add("generate", gen, "difficulty 4", "spec.difficulty", 4)
	add("generate", gen, "difficulty null", "spec.difficulty", nil)
	add("generate", gen, "mode bogus", "spec.mode", "voice")
	add("generate", gen, "profile bogus", "profile", "x")

	tt := bases["tts"]
	add("tts", tt, "text null", "text", nil)
	add("tts", tt, "text blank", "text", "  \n")
	add("tts", tt, "text_hash missing", "text_hash", deleteKey)
	add("tts", tt, "text_hash traversal", "text_hash", "../../../../etc/passwd")
	add("tts", tt, "text_hash slash", "text_hash", "abcdefgh/ijk")
	add("tts", tt, "text_hash short", "text_hash", "abc")
	add("tts", tt, "text_hash long", "text_hash", strings.Repeat("a", 129))

	for _, c := range cases {
		r := h.postJSON(t, "/v1/jobs/"+c.kind, c.body)
		if r.status != http.StatusBadRequest {
			t.Errorf("%s: %d %s, ждали 400", c.name, r.status, r.body)
			continue
		}
		if e := r.apiErr(t); e.Code != "bad_payload" {
			t.Errorf("%s: code %q", c.name, e.Code)
		}
	}
	// Отклонённые задачи не попадают в очередь и не порождают callback'ов.
	h.sink.none(t, 100*time.Millisecond)
	if pending, _, _ := h.q.snapshot(); sum(pending) != 0 {
		t.Fatalf("в очереди остались задачи: %v", pending)
	}

	// Базовые payload'ы при этом валидны.
	for kind, base := range bases {
		if r := h.postJSON(t, "/v1/jobs/"+kind, base); r.status != http.StatusAccepted {
			t.Fatalf("%s: валидный payload → %d %s", kind, r.status, r.body)
		}
	}
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func TestSubmitJobBodyTooLarge(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	p := grammarPayload()
	p["texts"] = []any{obj{"field": "description", "text": strings.Repeat("а", maxJobBody/2+1)}}
	r := h.postJSON(t, "/v1/jobs/grammar", p)
	if r.status != http.StatusBadRequest || r.apiErr(t).Code != "bad_payload" {
		t.Fatalf("%d %s", r.status, r.body)
	}
}

// ------------------------------------------------------------------ happy path и конверт callback'а

func TestJobsHappyPathCallbacks(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, nil)
	cases := []struct {
		kind    string
		payload obj
		typ     components.JobType
		check   func(t *testing.T, res callbacks.AiResult)
	}{
		{"grammar", grammarPayload(), components.EvaluateGrammar, func(t *testing.T, res callbacks.AiResult) {
			g := res.Grammar
			// UPPERCASE_SENTENCE_START + MORFOLOGIK (ошибки) + MISSING_FINAL_PUNCT (предупреждение).
			if len(g.Remarks) != 3 || g.Stats.WordsChecked != 8 || g.Score != 100-15*2-7 {
				t.Fatalf("grammar: %+v", g)
			}
			if deref(res.Engine.LtVersion) == "" || deref(res.Engine.RulesVersion) == "" {
				t.Fatalf("engine: %+v", res.Engine)
			}
		}},
		{"semantic", semanticPayload(), components.EvaluateSemantic, func(t *testing.T, res callbacks.AiResult) {
			s := res.Semantic
			if s.Score != 100 || len(deref(s.MissingFacts)) != 0 || len(deref(s.ExtraFacts)) != 0 || s.Confidence < 0.8 {
				t.Fatalf("semantic: %+v", s)
			}
			if deref(res.Engine.LlmModel) != modelLLM || deref(res.Engine.PromptVersion) == "" {
				t.Fatalf("engine: %+v", res.Engine)
			}
		}},
		{"dialogue", dialoguePayload(), components.EvaluateDialogue, func(t *testing.T, res callbacks.AiResult) {
			d := res.Dialogue
			if len(d.Checklist) != 5 || d.Score != 64 || d.Speech.OperatorTurns != 2 {
				t.Fatalf("dialogue: score=%v checklist=%d turns=%d", d.Score, len(d.Checklist), d.Speech.OperatorTurns)
			}
		}},
		{"generate", generatePayload(), components.GenerateScenario, func(t *testing.T, res callbacks.AiResult) {
			sc := res.Scenario
			if sc.Title == "" || len(sc.CallScript.Turns) == 0 || sc.ExpectedActions == nil || sc.ExpectedDialogue == nil {
				t.Fatalf("scenario: %+v", sc)
			}
			if sc.Title == "Пожар в квартире многоквартирного дома" {
				t.Fatal("avoid_titles проигнорирован")
			}
		}},
		{"tts", ttsPayload(), components.Tts, func(t *testing.T, res callbacks.AiResult) {
			tr := res.Tts
			if tr.TextHash != "0123456789abcdef0123456789abcdef" || tr.FilePath != "tts/01/0123456789abcdef0123456789abcdef.wav" {
				t.Fatalf("tts: %+v", tr)
			}
			w, _ := readWAVFile(t, h.cfg.ttsDir, tr.FilePath)
			if w.rate != wavRate || w.channels != 1 || w.bits != 16 || w.dataSize != wavRate*tr.DurationMs/1000*2 {
				t.Fatalf("wav: %+v, duration %d", w, tr.DurationMs)
			}
			if tr.DurationMs != ttsDuration("Алло! У нас дым из квартиры идёт!") {
				t.Fatalf("duration %d", tr.DurationMs)
			}
		}},
	}
	for _, c := range cases {
		r := h.postJSON(t, "/v1/jobs/"+c.kind, c.payload)
		if r.status != http.StatusAccepted {
			t.Fatalf("%s: %d %s", c.kind, r.status, r.body)
		}
		var acc aiservice.JobAccepted
		if err := json.Unmarshal(r.body, &acc); err != nil {
			t.Fatal(err)
		}
		if acc.RequestId.String() != c.payload["request_id"] || acc.QueuePosition == nil || acc.EstWaitSec == nil {
			t.Fatalf("%s: JobAccepted %s", c.kind, r.body)
		}
		if acc.Status != aiservice.Queued && acc.Status != aiservice.Running {
			t.Fatalf("%s: status %q", c.kind, acc.Status)
		}
		body := h.sink.next(t, cbWait)
		res := checkEnvelope(t, body, c.typ)
		if res.Status != callbacks.Ok {
			t.Fatalf("%s: status %s: %s", c.kind, res.Status, body)
		}
		if res.RequestId.String() != c.payload["request_id"] {
			t.Fatalf("%s: request_id %v", c.kind, res.RequestId)
		}
		if aid, ok := c.payload["attempt_id"]; ok && res.AttemptId.String() != aid {
			t.Fatalf("%s: attempt_id %v", c.kind, res.AttemptId)
		}
		if res.Engine.QueueWaitMs == nil {
			t.Fatalf("%s: engine.queue_wait_ms не заполнен", c.kind)
		}
		c.check(t, res)
	}
}

// Массивы результатов — [] а не null: go-core и фронт не различают «пусто» и «нет данных».
func TestCallbackArraysAreNotNull(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, nil)
	g := grammarPayload()
	g["texts"] = []any{obj{"field": "description", "text": "Всё в порядке."}}
	s := semanticPayload()
	s["etalon"].(obj)["scoring"] = obj{}
	delete(s, "call_script") // фактов нет ни в эталоне, ни в легенде — missing/extra пусты
	d := dialoguePayload()
	d["etalon"].(obj)["expected_dialogue"].(obj)["forbidden"] = []any{}
	for kind, p := range map[string]obj{"grammar": g, "semantic": s, "dialogue": d} {
		if r := h.postJSON(t, "/v1/jobs/"+kind, p); r.status != http.StatusAccepted {
			t.Fatalf("%s: %d", kind, r.status)
		}
	}
	want := map[string][]string{
		"grammar":  {`"remarks":[]`},
		"semantic": {`"missing_facts":[]`, `"extra_facts":[]`},
		"dialogue": {`"forbidden_hits":[]`, `"missing_questions":[`, `"checklist":[`},
	}
	for range 3 {
		body := string(h.sink.next(t, cbWait))
		for k, subs := range want {
			if !strings.Contains(body, `"`+k+`":{`) {
				continue
			}
			for _, sub := range subs {
				if !strings.Contains(body, sub) {
					t.Errorf("%s: нет %s в %s", k, sub, body)
				}
			}
		}
		if strings.Contains(body, ":null") {
			t.Errorf("null в callback: %s", body)
		}
	}
}

// ------------------------------------------------------------------ идемпотентность

func TestIdempotencyWhileQueued(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil) // воркеров нет — задачи остаются в очереди
	p := semanticPayload()
	other := semanticPayload()
	other["priority"] = 1

	for i := range 3 {
		r := h.postJSON(t, "/v1/jobs/semantic", p)
		var acc aiservice.JobAccepted
		if err := json.Unmarshal(r.body, &acc); err != nil || r.status != http.StatusAccepted {
			t.Fatalf("submit %d: %d %s", i, r.status, r.body)
		}
		if acc.Status != aiservice.Queued || deref(acc.QueuePosition) != 0 {
			t.Fatalf("submit %d: %s", i, r.body)
		}
	}
	// Более приоритетная задача встаёт впереди — позиция первой растёт, дубля нет.
	if r := h.postJSON(t, "/v1/jobs/semantic", other); r.status != http.StatusAccepted {
		t.Fatal(r.status)
	}
	r := h.postJSON(t, "/v1/jobs/semantic", p)
	var acc aiservice.JobAccepted
	_ = json.Unmarshal(r.body, &acc)
	if acc.Status != aiservice.Queued || deref(acc.QueuePosition) != 1 {
		t.Fatalf("после приоритетной: %s", r.body)
	}
	pending, _, _ := h.q.snapshot()
	if pending[string(components.EvaluateSemantic)] != 2 || sum(pending) != 2 {
		t.Fatalf("pending %v, ждали 2 (без дублей)", pending)
	}
	h.sink.none(t, 50*time.Millisecond)
}

func TestIdempotencyDoneResendsSameCallback(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, nil)
	for _, c := range []struct {
		kind string
		p    obj
		typ  components.JobType
	}{
		{"grammar", grammarPayload(), components.EvaluateGrammar},
		{"generate", generatePayload(), components.GenerateScenario},
		{"tts", ttsPayload(), components.Tts},
	} {
		if r := h.postJSON(t, "/v1/jobs/"+c.kind, c.p); r.status != http.StatusAccepted {
			t.Fatal(r.status)
		}
		first := h.sink.next(t, cbWait)
		checkEnvelope(t, first, c.typ)

		// Повтор уже выполненной задачи: 202 done + тот же результат повторно (не пересчёт).
		r := h.postJSON(t, "/v1/jobs/"+c.kind, c.p)
		var acc aiservice.JobAccepted
		if err := json.Unmarshal(r.body, &acc); err != nil || r.status != http.StatusAccepted || acc.Status != aiservice.Done {
			t.Fatalf("%s: повтор done → %d %s", c.kind, r.status, r.body)
		}
		second := h.sink.next(t, cbWait)
		if !bytes.Equal(first, second) {
			t.Fatalf("%s: повторный callback отличается:\n%s\n%s", c.kind, first, second)
		}
	}
	pending, running, _ := h.q.snapshot()
	if sum(pending) != 0 || running != 0 {
		t.Fatalf("pending %v running %d", pending, running)
	}
}

// ------------------------------------------------------------------ 429, отказы

func TestQueueFull429(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, func(c *config) { c.queueMax = 2; c.speed = 1 })
	first := grammarPayload()
	if r := h.postJSON(t, "/v1/jobs/grammar", first); r.status != http.StatusAccepted {
		t.Fatalf("%d %s", r.status, r.body)
	}
	if r := h.postJSON(t, "/v1/jobs/semantic", semanticPayload()); r.status != http.StatusAccepted {
		t.Fatalf("%d %s", r.status, r.body)
	}
	r := h.postJSON(t, "/v1/jobs/tts", ttsPayload())
	if r.status != http.StatusTooManyRequests {
		t.Fatalf("третья задача: %d, ждали 429", r.status)
	}
	if e := r.apiErr(t); e.Code != "queue_full" {
		t.Fatalf("code %q", e.Code)
	}
	ra, err := strconv.Atoi(r.header.Get("Retry-After"))
	if err != nil || ra < 1 || ra > 30 {
		t.Fatalf("Retry-After %q", r.header.Get("Retry-After"))
	}
	// Повтор уже принятой задачи при полной очереди — всё равно 202 (идемпотентность важнее лимита).
	if r := h.postJSON(t, "/v1/jobs/grammar", first); r.status != http.StatusAccepted {
		t.Fatalf("дубль при полной очереди: %d", r.status)
	}
}

func TestFailEveryProducesFailedEnvelopeAndRecomputes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, func(c *config) { c.failEvery = 2 })
	a, b := grammarPayload(), grammarPayload()

	if r := h.postJSON(t, "/v1/jobs/grammar", a); r.status != http.StatusAccepted {
		t.Fatal(r.status)
	}
	if res := checkEnvelope(t, h.sink.next(t, cbWait), components.EvaluateGrammar); res.Status != callbacks.Ok {
		t.Fatalf("первая задача: %s", res.Status)
	}
	if r := h.postJSON(t, "/v1/jobs/grammar", b); r.status != http.StatusAccepted {
		t.Fatal(r.status)
	}
	res := checkEnvelope(t, h.sink.next(t, cbWait), components.EvaluateGrammar)
	if res.Status != callbacks.Failed || res.Error.Code != callbacks.LtUnavailable || !res.Error.Retryable {
		t.Fatalf("вторая задача: %+v", res.Error)
	}
	// Отказ retryable не кэшируется: повтор с тем же request_id считается заново.
	r := h.postJSON(t, "/v1/jobs/grammar", b)
	var acc aiservice.JobAccepted
	_ = json.Unmarshal(r.body, &acc)
	if r.status != http.StatusAccepted || acc.Status == aiservice.Done {
		t.Fatalf("повтор после отказа: %d %s", r.status, r.body)
	}
	if res := checkEnvelope(t, h.sink.next(t, cbWait), components.EvaluateGrammar); res.Status != callbacks.Ok {
		t.Fatalf("пересчёт: %s", res.Status)
	}
}

func TestFailCodesByKind(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, func(c *config) { c.failEvery = 1 })
	for _, c := range []struct {
		kind string
		p    obj
		typ  components.JobType
		code callbacks.AiJobErrorCode
	}{
		{"semantic", semanticPayload(), components.EvaluateSemantic, callbacks.LlmTimeout},
		{"dialogue", dialoguePayload(), components.EvaluateDialogue, callbacks.LlmTimeout},
		{"generate", generatePayload(), components.GenerateScenario, callbacks.LlmTimeout},
		{"tts", ttsPayload(), components.Tts, callbacks.TtsFailed},
	} {
		if r := h.postJSON(t, "/v1/jobs/"+c.kind, c.p); r.status != http.StatusAccepted {
			t.Fatal(r.status)
		}
		res := checkEnvelope(t, h.sink.next(t, cbWait), c.typ)
		if res.Status != callbacks.Failed || res.Error.Code != c.code || !res.Error.Retryable {
			t.Fatalf("%s: %+v", c.kind, res.Error)
		}
	}
}

// ------------------------------------------------------------------ очередь

func TestQueueStatusShape(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, func(c *config) { c.speed = 1 })
	_ = h.postJSON(t, "/v1/jobs/semantic", semanticPayload())
	_ = h.postJSON(t, "/v1/jobs/generate", generatePayload())
	r := h.request(t, http.MethodGet, "/v1/queue", "", nil, testToken)
	if r.status != http.StatusOK {
		t.Fatal(r.status)
	}
	var qs aiservice.QueueStatus
	if err := json.Unmarshal(r.body, &qs); err != nil {
		t.Fatal(err)
	}
	for _, typ := range kindTypes {
		if _, ok := qs.Pending[string(typ)]; !ok {
			t.Fatalf("pending без %s: %v", typ, qs.Pending)
		}
	}
	if qs.Pending[string(components.EvaluateSemantic)] != 1 || qs.Pending[string(components.GenerateScenario)] != 1 {
		t.Fatalf("pending %v", qs.Pending)
	}
	if qs.Running != 0 || deref(qs.CurrentModel) != modelLLM || !deref(qs.LlmLoaded) || qs.DialogAvgMs == nil {
		t.Fatalf("queue: %s", r.body)
	}
	// Оценка для новой semantic: 2 задачи LLM впереди на 2 воркерах + своя = 2×1.5 с.
	if deref(qs.EstWaitSec) != 3 {
		t.Fatalf("est_wait_sec %d", deref(qs.EstWaitSec))
	}
}

// ------------------------------------------------------------------ диалог: ход

func decodeTurn(t *testing.T, r resp) aiservice.DialogTurnResult {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("dialog/turn: %d %s", r.status, r.body)
	}
	var res aiservice.DialogTurnResult
	if err := json.Unmarshal(r.body, &res); err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(r.body, &raw)
	for _, k := range []string{"schema_version", "request_id", "attempt_id", "turn_no", "operator", "caller", "engine"} {
		if isNull(raw[k]) {
			t.Fatalf("нет поля %s: %s", k, r.body)
		}
	}
	if res.SchemaVersion != components.N1 || res.Caller.RevealedFactIds == nil || res.Caller.ShouldEnd == nil {
		t.Fatalf("turn: %s", r.body)
	}
	return res
}

func TestDialogTurnJSONText(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	p := dialogTurnPayload()
	res := decodeTurn(t, h.postJSON(t, "/v1/dialog/turn", p))

	if res.RequestId.String() != p["request_id"] || res.AttemptId.String() != p["attempt_id"] || res.TurnNo != 1 {
		t.Fatalf("эхо идентификаторов: %+v", res)
	}
	if res.Operator.Text != "Служба 112. Какой подъезд?" || deref(res.Operator.Confidence) != 1 || deref(res.Operator.NoSpeech) {
		t.Fatalf("operator: %+v", res.Operator)
	}
	if got := *res.Caller.RevealedFactIds; len(got) != 1 || got[0] != "entrance" {
		t.Fatalf("revealed %v", got)
	}
	if res.Caller.Text != "Господи! Подъезд 3. Приезжайте скорее!" || deref(res.Caller.ShouldEnd) {
		t.Fatalf("caller: %q", res.Caller.Text)
	}
	if deref(res.Fallback) || deref(res.Engine.LlmModel) != modelLLM || res.Engine.SttModel != nil || deref(res.Engine.TtsVersion) == "" {
		t.Fatalf("engine: %+v", res.Engine)
	}
	tr := res.Caller.Tts
	want := "dialog/" + p["attempt_id"].(string) + "/1.wav"
	if tr == nil || tr.FilePath != want || tr.DurationMs != ttsDuration(res.Caller.Text) || len(tr.TextHash) != 64 {
		t.Fatalf("tts: %+v", tr)
	}
	if tr.TextHash != ttsHash(res.Caller.Text, "baya", 1) {
		t.Fatalf("text_hash не sha256(text|voice|rate)")
	}
	readWAVFile(t, h.cfg.ttsDir, tr.FilePath)

	// Детерминизм: тот же ход — тот же ответ.
	again := decodeTurn(t, h.postJSON(t, "/v1/dialog/turn", p))
	if again.Caller.Text != res.Caller.Text || again.Caller.Tts.TextHash != tr.TextHash {
		t.Fatalf("недетерминированный ответ заявителя")
	}
}

func TestDialogTurnTTSDisabled(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	p := mutate(t, dialogTurnPayload(), "options.tts.enabled", false)
	res := decodeTurn(t, h.postJSON(t, "/v1/dialog/turn", p))
	if res.Caller.Tts != nil || res.Engine.TtsVersion != nil {
		t.Fatalf("tts при enabled=false: %+v", res.Caller.Tts)
	}
	if entries, _ := os.ReadDir(h.cfg.ttsDir); len(entries) != 0 {
		t.Fatalf("файлы записаны при enabled=false: %v", entries)
	}
}

func TestDialogTurnMultipartAudio(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	p := mutate(t, dialogTurnPayload(), "operator_text", deleteKey)
	ct, body := multipartBody(t,
		part{name: "audio", filename: "turn.webm", contentType: "audio/webm;codecs=opus", body: fakeWebm(8000)},
		part{name: "request", contentType: "application/json", body: mustJSON(t, p)},
	)
	res := decodeTurn(t, h.request(t, http.MethodPost, "/v1/dialog/turn", ct, body, testToken))
	if res.Operator.Text != sttPhrases[0] || deref(res.Operator.Confidence) != 0.5 || res.Operator.AudioDurationMs != 1000 {
		t.Fatalf("operator: %+v", res.Operator)
	}
	if deref(res.Engine.SttModel) != modelSTT {
		t.Fatalf("engine без stt_model: %+v", res.Engine)
	}
	if got := *res.Caller.RevealedFactIds; len(got) != 1 || got[0] != "smoke" {
		t.Fatalf("revealed %v (ждали volunteer-факт smoke)", got)
	}

	// Текст оператора важнее аудио: STT не вызывается.
	p2 := dialogTurnPayload()
	ct, body = multipartBody(t,
		part{name: "request", contentType: "application/json", body: mustJSON(t, p2)},
		part{name: "extra", body: []byte("ignored")},
		part{name: "audio", filename: "turn.webm", contentType: "audio/webm", body: fakeWebm(8000)},
	)
	res = decodeTurn(t, h.request(t, http.MethodPost, "/v1/dialog/turn", ct, body, testToken))
	if res.Operator.Text != "Служба 112. Какой подъезд?" || res.Engine.SttModel != nil {
		t.Fatalf("operator_text должен победить аудио: %+v", res.Operator)
	}
}

func TestDialogTurnNoSpeech(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	p := mutate(t, dialogTurnPayload(), "operator_text", deleteKey)
	ct, body := multipartBody(t,
		part{name: "request", contentType: "application/json", body: mustJSON(t, p)},
		part{name: "audio", filename: "turn.webm", contentType: "application/octet-stream", body: fakeWebm(100)},
	)
	for range 2 { // служебная фраза пишется один раз и переиспользуется
		res := decodeTurn(t, h.request(t, http.MethodPost, "/v1/dialog/turn", ct, body, testToken))
		if !deref(res.Operator.NoSpeech) || res.Operator.Text != "" {
			t.Fatalf("operator: %+v", res.Operator)
		}
		if res.Caller.Text != noSpeechText || len(*res.Caller.RevealedFactIds) != 0 || deref(res.Caller.ShouldEnd) {
			t.Fatalf("caller: %+v", res.Caller)
		}
		if res.Caller.Tts == nil || res.Caller.Tts.FilePath != noSpeechPath {
			t.Fatalf("tts: %+v", res.Caller.Tts)
		}
		readWAVFile(t, h.cfg.ttsDir, noSpeechPath)
	}
}

func TestDialogTurnErrors(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	base := dialogTurnPayload()
	noText := mutate(t, base, "operator_text", deleteKey)

	for _, c := range []struct {
		name string
		body obj
	}{
		{"history null", mutate(t, base, "history", nil)},
		{"history missing", mutate(t, base, "history", deleteKey)},
		{"call_script null", mutate(t, base, "call_script", nil)},
		{"turns empty", mutate(t, base, "call_script.turns", []any{})},
		{"schema 2", mutate(t, base, "schema_version", "2")},
		{"schema missing", mutate(t, base, "schema_version", deleteKey)},
		{"turn_no 0", mutate(t, base, "turn_no", 0)},
		{"turn_no missing", mutate(t, base, "turn_no", deleteKey)},
		{"attempt nil", mutate(t, base, "attempt_id", uuid.Nil.String())},
		{"speaker bogus", mutate(t, base, "history.0.speaker", "robot")},
		{"reveal bogus", mutate(t, base, "call_script.dialogue.facts.0.reveal", "sometimes")},
		{"profile bogus", mutate(t, base, "profile", "x")},
		{"no text no audio", noText},
		{"blank text no audio", mutate(t, base, "operator_text", "   ")},
	} {
		r := h.postJSON(t, "/v1/dialog/turn", c.body)
		if r.status != http.StatusBadRequest || r.apiErr(t).Code != "bad_payload" {
			t.Errorf("%s: %d %s", c.name, r.status, r.body)
		}
	}

	// multipart без части request.
	ct, body := multipartBody(t, part{name: "audio", contentType: "audio/webm", body: fakeWebm(4000)})
	if r := h.request(t, http.MethodPost, "/v1/dialog/turn", ct, body, testToken); r.status != http.StatusBadRequest {
		t.Errorf("без request: %d", r.status)
	}
	// Слишком большая JSON-часть.
	ct, body = multipartBody(t, part{name: "request", body: bytes.Repeat([]byte("x"), maxJSONPart+1)})
	if r := h.request(t, http.MethodPost, "/v1/dialog/turn", ct, body, testToken); r.status != http.StatusBadRequest {
		t.Errorf("request > 1 МБ: %d", r.status)
	}
	// 413 — аудио больше 2 МБ.
	ct, body = multipartBody(t,
		part{name: "request", body: mustJSON(t, noText)},
		part{name: "audio", contentType: "audio/webm", body: fakeWebm(maxAudioBytes + 1)},
	)
	if r := h.request(t, http.MethodPost, "/v1/dialog/turn", ct, body, testToken); r.status != http.StatusRequestEntityTooLarge ||
		r.apiErr(t).Code != "payload_too_large" {
		t.Errorf("аудио > 2 МБ: %d %s", r.status, r.body)
	}
	// Ровно 2 МБ — ещё можно.
	ct, body = multipartBody(t,
		part{name: "request", body: mustJSON(t, noText)},
		part{name: "audio", contentType: "audio/webm", body: fakeWebm(maxAudioBytes)},
	)
	if r := h.request(t, http.MethodPost, "/v1/dialog/turn", ct, body, testToken); r.status != http.StatusOK {
		t.Errorf("аудио = 2 МБ: %d %s", r.status, r.body)
	}
	// 415 — неподдерживаемый формат аудио.
	for _, typ := range []string{"audio/flac", "audio/mp4", "text/plain", "bad;;type"} {
		ct, body = multipartBody(t,
			part{name: "request", body: mustJSON(t, noText)},
			part{name: "audio", contentType: typ, body: fakeWebm(4000)},
		)
		r := h.request(t, http.MethodPost, "/v1/dialog/turn", ct, body, testToken)
		if r.status != http.StatusUnsupportedMediaType || r.apiErr(t).Code != "unsupported_media" {
			t.Errorf("аудио %s: %d %s", typ, r.status, r.body)
		}
	}
	// Битый multipart (нет boundary в теле).
	if r := h.request(t, http.MethodPost, "/v1/dialog/turn", "multipart/form-data; boundary=zzz", []byte("garbage"), testToken); r.status != http.StatusBadRequest {
		t.Errorf("битый multipart: %d", r.status)
	}
}

func TestDialogTurnBusy503(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, func(c *config) { c.busyEvery = 2 })
	p := dialogTurnPayload()
	// Невалидные ходы не считаются в busy-счётчик.
	if r := h.postJSON(t, "/v1/dialog/turn", mutate(t, p, "history", nil)); r.status != http.StatusBadRequest {
		t.Fatal(r.status)
	}
	for i := 1; i <= 4; i++ {
		r := h.postJSON(t, "/v1/dialog/turn", p)
		if i%2 == 1 {
			if r.status != http.StatusOK {
				t.Fatalf("ход %d: %d", i, r.status)
			}
			continue
		}
		if r.status != http.StatusServiceUnavailable || r.apiErr(t).Code != "service_busy" {
			t.Fatalf("ход %d: %d %s, ждали 503", i, r.status, r.body)
		}
		if r.header.Get("Retry-After") != strconv.Itoa(busyRetryAfter) {
			t.Fatalf("Retry-After %q", r.header.Get("Retry-After"))
		}
	}
}

// ------------------------------------------------------------------ STT

func TestTranscribe(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	ct, body := multipartBody(t,
		part{name: "options", contentType: "application/json", body: []byte(`{"lang":"ru","profile":"stt_default","hints":["подъезд"]}`)},
		part{name: "audio", filename: "a.wav", contentType: "audio/wav", body: wavBytes(t, "Алло", 1000)},
	)
	r := h.request(t, http.MethodPost, "/v1/stt/transcribe", ct, body, testToken)
	if r.status != http.StatusOK {
		t.Fatalf("%d %s", r.status, r.body)
	}
	var res aiservice.SttResponse
	if err := json.Unmarshal(r.body, &res); err != nil {
		t.Fatal(err)
	}
	if res.Text != sttPhrases[0] || res.AudioDurationMs != 1000 || deref(res.NoSpeech) || deref(res.Language) != "ru" {
		t.Fatalf("stt: %s", r.body)
	}
	if deref(res.Engine.SttModel) != modelSTT || res.Segments == nil || len(*res.Segments) != 1 || deref(res.TextRaw) == "" {
		t.Fatalf("stt: %s", r.body)
	}
	// Следующий вызов — следующая фраза протокола.
	r = h.request(t, http.MethodPost, "/v1/stt/transcribe", ct, body, testToken)
	_ = json.Unmarshal(r.body, &res)
	if res.Text != sttPhrases[1] {
		t.Fatalf("вторая фраза: %q", res.Text)
	}

	// Без options, тишина.
	ct, body = multipartBody(t, part{name: "audio", body: fakeWebm(50)})
	r = h.request(t, http.MethodPost, "/v1/stt/transcribe", ct, body, testToken)
	var silent aiservice.SttResponse
	if err := json.Unmarshal(r.body, &silent); err != nil || r.status != http.StatusOK || !deref(silent.NoSpeech) || silent.Text != "" {
		t.Fatalf("тишина: %d %s", r.status, r.body)
	}
	if silent.Segments == nil || len(*silent.Segments) != 0 || silent.AudioDurationMs != 300 {
		t.Fatalf("тишина: %s", r.body)
	}
}

func TestTranscribeErrors(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	check := func(name string, r resp, status int, code string) {
		t.Helper()
		if r.status != status || r.apiErr(t).Code != code {
			t.Errorf("%s: %d %s, ждали %d %s", name, r.status, r.body, status, code)
		}
	}
	check("json вместо multipart", h.postJSON(t, "/v1/stt/transcribe", `{}`), http.StatusUnsupportedMediaType, "unsupported_media")

	ct, body := multipartBody(t, part{name: "options", body: []byte(`{}`)})
	check("нет audio", h.request(t, http.MethodPost, "/v1/stt/transcribe", ct, body, testToken), http.StatusBadRequest, "bad_payload")

	ct, body = multipartBody(t, part{name: "options", body: []byte(`{"profile":"bogus"}`)}, part{name: "audio", body: fakeWebm(4000)})
	check("плохой profile", h.request(t, http.MethodPost, "/v1/stt/transcribe", ct, body, testToken), http.StatusBadRequest, "bad_payload")

	ct, body = multipartBody(t, part{name: "options", body: []byte(`{"lang":`)}, part{name: "audio", body: fakeWebm(4000)})
	check("битые options", h.request(t, http.MethodPost, "/v1/stt/transcribe", ct, body, testToken), http.StatusBadRequest, "bad_payload")

	ct, body = multipartBody(t, part{name: "audio", contentType: "audio/aac", body: fakeWebm(4000)})
	check("aac", h.request(t, http.MethodPost, "/v1/stt/transcribe", ct, body, testToken), http.StatusUnsupportedMediaType, "unsupported_media")

	ct, body = multipartBody(t, part{name: "audio", contentType: "audio/ogg", body: fakeWebm(maxAudioBytes + 10)})
	check("> 2 МБ", h.request(t, http.MethodPost, "/v1/stt/transcribe", ct, body, testToken), http.StatusRequestEntityTooLarge, "payload_too_large")
}

// ------------------------------------------------------------------ TTS sync

func TestTTSSync(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	hash := "Abc_def-0123456789"
	r := h.postJSON(t, "/v1/tts/sync", obj{"text": "Служба 112, слушаю вас.", "text_hash": hash, "voice": "xenia", "rate": 1.25})
	if r.status != http.StatusOK {
		t.Fatalf("%d %s", r.status, r.body)
	}
	var tr components.TtsResult
	if err := json.Unmarshal(r.body, &tr); err != nil {
		t.Fatal(err)
	}
	if tr.TextHash != hash || tr.FilePath != "tts/Ab/"+hash+".wav" || deref(tr.Voice) != "xenia" || deref(tr.Rate) != 1.25 {
		t.Fatalf("tts: %s", r.body)
	}
	if tr.DurationMs != ttsDuration("Служба 112, слушаю вас.") {
		t.Fatalf("duration %d", tr.DurationMs)
	}
	w, _ := readWAVFile(t, h.cfg.ttsDir, tr.FilePath)
	if w.dataSize != wavRate*tr.DurationMs/1000*2 {
		t.Fatalf("wav %+v", w)
	}
	// Значения по умолчанию.
	r = h.postJSON(t, "/v1/tts/sync", obj{"text": "Алло", "text_hash": "ffffffff"})
	_ = json.Unmarshal(r.body, &tr)
	if r.status != http.StatusOK || deref(tr.Voice) != defaultVoice || deref(tr.Rate) != 1 || tr.DurationMs != ttsMinMs {
		t.Fatalf("defaults: %d %s", r.status, r.body)
	}

	for _, c := range []struct {
		name string
		body any
	}{
		{"no text_hash", obj{"text": "Алло"}},
		{"no text", obj{"text_hash": "ffffffff"}},
		{"blank text", obj{"text": " ", "text_hash": "ffffffff"}},
		{"traversal", obj{"text": "Алло", "text_hash": "../../../../tmp/x"}},
		{"dots", obj{"text": "Алло", "text_hash": "........"}},
		{"not object", `["Алло"]`},
		{"text number", obj{"text": 5, "text_hash": "ffffffff"}},
	} {
		r := h.postJSON(t, "/v1/tts/sync", c.body)
		if r.status != http.StatusBadRequest || r.apiErr(t).Code != "bad_payload" {
			t.Errorf("%s: %d %s", c.name, r.status, r.body)
		}
	}
	// Ничего не записано за пределами TTS_DIR/tts.
	entries, _ := os.ReadDir(h.cfg.ttsDir)
	for _, e := range entries {
		if e.Name() != "tts" {
			t.Fatalf("лишний файл в TTS_DIR: %s", e.Name())
		}
	}
}

// Ответы-ошибки всегда JSON ApiError с Content-Type application/json.
func TestErrorResponsesAreJSON(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	for _, r := range []resp{
		h.request(t, http.MethodGet, "/v1/queue", "", nil, ""),
		h.postJSON(t, "/v1/jobs/grammar", `{}`),
		h.request(t, http.MethodGet, "/missing", "", nil, testToken),
	} {
		if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("Content-Type %q", ct)
		}
		r.apiErr(t)
	}
}
