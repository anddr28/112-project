package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/gen/callbacks"
	"lct/gocore/internal/gen/components"
)

const testToken = "test-internal-token"

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// ------------------------------------------------------------------ callback sink

// sink — фейковый go-core /internal/ai/v1/results: копит тела callback'ов.
type sink struct {
	srv   *httptest.Server
	ch    chan []byte
	calls atomic.Int64
	fail  atomic.Int64 // столько ближайших запросов ответить 503
}

func newSink(t *testing.T) *sink {
	t.Helper()
	s := &sink{ch: make(chan []byte, 256)}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Method != http.MethodPost:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		case r.Header.Get("X-Internal-Token") != testToken:
			w.WriteHeader(http.StatusUnauthorized)
			return
		case r.Header.Get("Content-Type") != "application/json":
			w.WriteHeader(http.StatusUnsupportedMediaType)
			return
		case s.fail.Add(-1) >= 0:
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		s.ch <- body
		_, _ = w.Write([]byte(`{"accepted":true}`))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// next — следующий callback или провал теста по таймауту.
func (s *sink) next(t *testing.T, timeout time.Duration) []byte {
	t.Helper()
	select {
	case b := <-s.ch:
		return b
	case <-time.After(timeout):
		t.Fatalf("callback не пришёл за %v", timeout)
		return nil
	}
}

// none — за d не пришло ни одного callback'а.
func (s *sink) none(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case b := <-s.ch:
		t.Fatalf("неожиданный callback: %s", b)
	case <-time.After(d):
	}
}

// ------------------------------------------------------------------ harness

type harness struct {
	cfg  config
	q    *queue
	cb   *deliverer
	srv  *server
	http *httptest.Server
	sink *sink
}

func testConfig(t *testing.T) config {
	return config{
		addr: "127.0.0.1:0", token: testToken, ttsDir: t.TempDir(), speed: 0,
		queueMax: 50, llmWorkers: 2, ltWorkers: 2, ttsWorkers: 2, doneTTL: time.Hour, logLevel: "error",
	}
}

// newHarness — имитатор целиком (HTTP + очередь + доставка callback'ов в sink).
// workers=false — воркеры полос не запускаются: задачи остаются в очереди (queued).
func newHarness(t *testing.T, workers bool, mut func(*config)) *harness {
	t.Helper()
	sk := newSink(t)
	cfg := testConfig(t)
	cfg.callbackURL = sk.srv.URL + "/internal/ai/v1/results"
	if mut != nil {
		mut(&cfg)
	}
	log := discardLog()
	store := &ttsStore{root: cfg.ttsDir}
	cb := newDeliverer(cfg, log)
	q := newQueue(cfg, log, cb, store)
	srv := newServer(cfg, log, q, store)
	hs := httptest.NewServer(srv.routes())
	t.Cleanup(hs.Close)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	cb.start(ctx, &wg)
	if workers {
		q.start(ctx, &wg)
	}
	t.Cleanup(func() { cancel(); wg.Wait() })
	return &harness{cfg: cfg, q: q, cb: cb, srv: srv, http: hs, sink: sk}
}

type resp struct {
	status int
	header http.Header
	body   []byte
}

func (r resp) apiErr(t *testing.T) components.ApiError {
	t.Helper()
	var e components.ApiError
	if err := json.Unmarshal(r.body, &e); err != nil {
		t.Fatalf("тело ошибки не ApiError: %v (%s)", err, r.body)
	}
	if e.Code == "" || e.Message == "" {
		t.Fatalf("ApiError без code/message: %s", r.body)
	}
	return e
}

func (h *harness) request(t *testing.T, method, path, contentType string, body []byte, token string) resp {
	t.Helper()
	req, err := http.NewRequest(method, h.http.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("X-Internal-Token", token)
	}
	res, err := h.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp{status: res.StatusCode, header: res.Header, body: b}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (h *harness) postJSON(t *testing.T, path string, v any) resp {
	t.Helper()
	var body []byte
	switch x := v.(type) {
	case []byte:
		body = x
	case string:
		body = []byte(x)
	default:
		body = mustJSON(t, v)
	}
	return h.request(t, http.MethodPost, path, "application/json", body, testToken)
}

// ------------------------------------------------------------------ multipart

type part struct {
	name, filename, contentType string
	body                        []byte
}

func multipartBody(t *testing.T, parts ...part) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		hdr := textproto.MIMEHeader{}
		cd := `form-data; name="` + p.name + `"`
		if p.filename != "" {
			cd += `; filename="` + p.filename + `"`
		}
		hdr.Set("Content-Disposition", cd)
		if p.contentType != "" {
			hdr.Set("Content-Type", p.contentType)
		}
		w, err := mw.CreatePart(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(p.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return mw.FormDataContentType(), buf.Bytes()
}

// fakeWebm — «аудио» нужного размера (сигнатура EBML, содержимое не важно).
func fakeWebm(size int) []byte {
	b := bytes.Repeat([]byte{0x42}, size)
	if size >= 4 {
		copy(b, []byte{0x1A, 0x45, 0xDF, 0xA3})
	}
	return b
}

// ------------------------------------------------------------------ payloads

type obj = map[string]any

func callScript() obj {
	return obj{
		"caller":    obj{"name": "Мария", "role": "соседка", "emotional_state": "паника"},
		"address":   obj{"raw": "Ленина 14, подъезд 3, 5 этаж"},
		"key_facts": []any{"дым из квартиры на 5 этаже", "подъезд 3"},
		"dialogue": obj{
			"persona": "женщина ~60 лет, соседка, напугана",
			"facts": []any{
				obj{"id": "smoke", "text": "дым из квартиры на 5 этаже", "reveal": "volunteer"},
				obj{"id": "entrance", "text": "подъезд 3", "reveal": "on_request", "hints": []any{"подъезд"}},
				obj{"id": "person_inside", "text": "в квартире может быть пожилой сосед", "reveal": "on_request",
					"hints": []any{"люди", "кто-то внутри", "в квартире"}},
				obj{"id": "secret", "text": "сосед курит в постели", "reveal": "never", "hints": []any{"курит"}},
			},
			"unknowns":       []any{"номер квартиры соседа"},
			"end_conditions": []any{"оператор сказал, что помощь направлена"},
			"max_turns":      8,
		},
		"turns": []any{obj{"speaker": "caller", "text": "Алло! У нас дым из квартиры идёт, Ленина 14!"}},
	}
}

func grammarPayload() obj {
	return obj{
		"schema_version": "1", "request_id": uuid.NewString(), "attempt_id": uuid.NewString(), "priority": 2,
		"texts": []any{obj{"field": "description", "text": "горит квартира на пятом этаже в подьезде 3"}},
	}
}

func semanticPayload() obj {
	return obj{
		"schema_version": "1", "request_id": uuid.NewString(), "attempt_id": uuid.NewString(), "priority": 3,
		"profile": "eval_fast", "mode": "cards",
		"etalon": obj{
			"card": obj{"description": "Задымление в квартире на 5 этаже, внутри может быть человек."},
			"scoring": obj{
				"required_facts":  []any{"задымление в квартире", "5 этаж", "в квартире может находиться человек"},
				"forbidden_facts": []any{"открытое пламя"},
			},
		},
		"answer": obj{"card": obj{
			"description":   "Задымление в квартире на пятом этаже, в квартире может находиться пожилой человек.",
			"actions_taken": "Направлены пожарные и скорая помощь.",
		}},
		"free_text_fields": []any{"description", "actions_taken"},
		"call_script":      callScript(),
	}
}

func dialoguePayload() obj {
	return obj{
		"schema_version": "1", "request_id": uuid.NewString(), "attempt_id": uuid.NewString(), "priority": 3,
		"profile": "eval_dialogue", "call_script": callScript(),
		"etalon": obj{"expected_dialogue": obj{
			"checklist": []any{
				obj{"id": "ask_address", "text": "Уточнил адрес: дом, подъезд, этаж", "kind": "question", "required": true,
					"hints": []any{"подъезд", "этаж", "какой дом"}},
				obj{"id": "ask_people", "text": "Спросил, есть ли люди в квартире", "kind": "question", "required": true,
					"hints": []any{"люди", "кто-нибудь внутри"}},
				obj{"id": "ask_phone", "text": "Уточнил контактный телефон", "kind": "question", "required": false},
				obj{"id": "say_dispatched", "text": "Сообщил, что помощь направлена", "kind": "phrase", "required": true,
					"hints": []any{"направлена", "выехали", "едут"}},
				obj{"id": "calm", "text": "Говорил спокойно, не перебивал", "kind": "behavior", "required": true},
			},
			"forbidden": []any{"перезвоните позже", "ждите"},
		}},
		"transcript": []any{
			obj{"turn_no": 1, "speaker": "caller", "text": "Алло! У нас дым из квартиры идёт, Ленина 14!", "at_ms": 0, "source": "script"},
			obj{"turn_no": 2, "speaker": "operator", "text": "Служба 112, слушаю. Какой подъезд и этаж?", "at_ms": 4100,
				"confidence": 0.91, "audio_duration_ms": 2900, "source": "stt"},
			obj{"turn_no": 3, "speaker": "caller", "text": "Третий подъезд, пятый этаж, ой, дым такой!", "at_ms": 9800,
				"source": "llm", "revealed_fact_ids": []any{"entrance"}},
			obj{"turn_no": 4, "speaker": "operator", "text": "Понял, помощь направлена, оставайтесь на связи.", "at_ms": 15200,
				"confidence": 0.88, "audio_duration_ms": 3100, "source": "stt"},
		},
		"timing":  obj{"call_accepted_at_ms": 0, "submitted_at_ms": 41000},
		"options": obj{"lang": "ru"},
	}
}

func generatePayload() obj {
	return obj{
		"schema_version": "1", "request_id": uuid.NewString(), "priority": 7, "profile": "generate",
		"spec": obj{
			"category":        obj{"code": "101", "name": "Пожар в жилом доме", "path": []any{"Пожары"}},
			"difficulty":      2,
			"mode":            "both",
			"teacher_comment": "сделай панику",
			"avoid_titles":    []any{"Пожар в квартире многоквартирного дома"},
		},
	}
}

func ttsPayload() obj {
	return obj{
		"schema_version": "1", "request_id": uuid.NewString(), "priority": 5,
		"text": "Алло! У нас дым из квартиры идёт!", "text_hash": "0123456789abcdef0123456789abcdef",
		"voice": "baya", "rate": 1.0,
	}
}

func dialogTurnPayload() obj {
	return obj{
		"schema_version": "1", "request_id": uuid.NewString(), "attempt_id": uuid.NewString(), "turn_no": 1,
		"profile": "dialog_fast", "call_script": callScript(),
		"history":       []any{obj{"turn_no": 1, "speaker": "caller", "text": "Алло! У нас дым из квартиры идёт, Ленина 14!"}},
		"operator_text": "Служба 112. Какой подъезд?",
		"options":       obj{"tts": obj{"enabled": true, "voice": "baya", "rate": 1.0}, "max_reply_words": 40},
	}
}

// clone — глубокая копия payload'а через JSON (подтесты портят поля независимо).
func clone(t *testing.T, o obj) obj {
	t.Helper()
	var out obj
	if err := json.Unmarshal(mustJSON(t, o), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// ------------------------------------------------------------------ callback envelope

// checkEnvelope — инварианты go-internal.v1.yaml: schema_version "1", request_id,
// engine.duration_ms; status=ok — ровно одно поле результата, соответствующее type;
// status=failed — только error; attempt_id — у evaluate_* и нет у generate/tts.
func checkEnvelope(t *testing.T, body []byte, wantType components.JobType) callbacks.AiResult {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("callback не JSON-объект: %v", err)
	}
	for _, k := range []string{"schema_version", "request_id", "type", "status", "engine"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("в callback нет обязательного поля %q: %s", k, body)
		}
	}
	var eng map[string]json.RawMessage
	if err := json.Unmarshal(raw["engine"], &eng); err != nil || eng["duration_ms"] == nil {
		t.Fatalf("engine без duration_ms: %s", raw["engine"])
	}
	var res callbacks.AiResult
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("callback не AiResult: %v", err)
	}
	if res.SchemaVersion != components.N1 || res.Type != wantType || res.RequestId == uuid.Nil {
		t.Fatalf("конверт: schema=%q type=%q (ждали %q) request_id=%v", res.SchemaVersion, res.Type, wantType, res.RequestId)
	}
	resultKeys := map[components.JobType]string{
		components.EvaluateGrammar: "grammar", components.EvaluateSemantic: "semantic",
		components.EvaluateDialogue: "dialogue", components.GenerateScenario: "scenario", components.Tts: "tts",
	}
	present := []string{}
	for _, k := range []string{"grammar", "semantic", "dialogue", "scenario", "tts"} {
		if v, ok := raw[k]; ok && !isNull(v) {
			present = append(present, k)
		}
	}
	switch res.Status {
	case callbacks.Ok:
		if len(present) != 1 || present[0] != resultKeys[wantType] {
			t.Fatalf("status=ok: поля результата %v, ждали ровно [%s]", present, resultKeys[wantType])
		}
		if _, ok := raw["error"]; ok {
			t.Fatalf("status=ok с полем error: %s", body)
		}
	case callbacks.Failed:
		if len(present) != 0 {
			t.Fatalf("status=failed с полями результата %v", present)
		}
		if res.Error == nil || res.Error.Code == "" || res.Error.Message == "" {
			t.Fatalf("status=failed без error: %s", body)
		}
	default:
		t.Fatalf("неизвестный status %q", res.Status)
	}
	evaluate := wantType == components.EvaluateGrammar || wantType == components.EvaluateSemantic ||
		wantType == components.EvaluateDialogue
	if evaluate != (res.AttemptId != nil) {
		t.Fatalf("attempt_id: evaluate=%v, есть=%v", evaluate, res.AttemptId != nil)
	}
	return res
}

// ------------------------------------------------------------------ WAV

type wavInfo struct {
	channels, bits  int
	rate, byteRate  int
	dataSize, total int
}

// parseWAV — проверка канонического PCM WAV: RIFF/WAVE, fmt PCM, размер data = файл − 44.
func parseWAV(t *testing.T, b []byte) wavInfo {
	t.Helper()
	if len(b) < wavHeaderSize {
		t.Fatalf("WAV короче заголовка: %d байт", len(b))
	}
	le := binary.LittleEndian
	switch {
	case string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" || string(b[12:16]) != "fmt " || string(b[36:40]) != "data":
		t.Fatalf("не RIFF/WAVE: %q", b[:40])
	case int(le.Uint32(b[4:8])) != len(b)-8:
		t.Fatalf("RIFF size %d, файл %d", le.Uint32(b[4:8]), len(b))
	case le.Uint32(b[16:20]) != 16 || le.Uint16(b[20:22]) != 1:
		t.Fatalf("fmt не PCM")
	case int(le.Uint32(b[40:44])) != len(b)-wavHeaderSize:
		t.Fatalf("data size %d, фактически %d", le.Uint32(b[40:44]), len(b)-wavHeaderSize)
	}
	w := wavInfo{
		channels: int(le.Uint16(b[22:24])), rate: int(le.Uint32(b[24:28])), byteRate: int(le.Uint32(b[28:32])),
		bits: int(le.Uint16(b[34:36])), dataSize: int(le.Uint32(b[40:44])), total: len(b),
	}
	if w.byteRate != w.rate*w.channels*w.bits/8 || int(le.Uint16(b[32:34])) != w.channels*w.bits/8 {
		t.Fatalf("несогласованный fmt: %+v", w)
	}
	return w
}

func readWAVFile(t *testing.T, root, rel string) (wavInfo, []byte) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("файл озвучки %s: %v", rel, err)
	}
	return parseWAV(t, b), b
}

// wavBytes — настоящий WAV имитатора в памяти (для STT-тестов).
func wavBytes(t *testing.T, text string, durMs int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := writeWAV(&buf, text, durMs); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
