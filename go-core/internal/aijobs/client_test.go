package aijobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/settings"
)

func dialogReq() *aiservice.DialogTurnRequest {
	text := "Алло, служба 112, что случилось?"
	return &aiservice.DialogTurnRequest{AttemptId: ids.New(), RequestId: ids.New(), TurnNo: 1, OperatorText: &text}
}

const dialogReply = `{"schema_version":"1","request_id":"018f6b2a-7c1e-7f00-a000-000000000001",
 "attempt_id":"018f6b2a-7c1e-7f00-a000-000000000002","turn_no":1,
 "operator":{"text":"Алло"},"caller":{"text":"У нас пожар на кухне!"},"engine":{"duration_ms":4200}}`

func aiKind(t *testing.T, err error) *core.AIError {
	t.Helper()
	var ae *core.AIError
	if !errors.As(err, &ae) {
		t.Fatalf("error %v (%T) is not *core.AIError", err, err)
	}
	return ae
}

func TestDialogTurnStatusMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		status        int
		headers       map[string]string
		body          string
		kind          core.AIErrorKind
		retryAfter    int
		breakerFailed bool
	}{
		{"503 busy with Retry-After", 503, map[string]string{"Retry-After": "7"}, `{}`, core.AIBusy, 7, false},
		{"429 busy without Retry-After", 429, nil, `{}`, core.AIBusy, 0, false},
		{"413 audio too long", 413, nil, `{}`, core.AIAudioTooLong, 0, false},
		{"415 unsupported audio", 415, nil, `{}`, core.AIAudioUnsupported, 0, false},
		{"400 bad request", 400, nil, `{"code":"validation"}`, core.AIBadRequest, 0, false},
		{"422 bad request", 422, nil, `{"detail":[]}`, core.AIBadRequest, 0, false},
		{"401 token", 401, nil, `{}`, core.AIUnavailable, 0, false},
		{"500 server error", 500, nil, `boom`, core.AIUnavailable, 0, true},
		{"418 unexpected", 418, nil, `{}`, core.AIUnavailable, 0, false},
		{"200 not JSON", 200, nil, `<html>`, core.AIUnavailable, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeAI(t, replyStatus(c.status, c.headers, c.body))
			s := newTestService(t, nil, f.URL)
			setAI(s, func(ai *settings.AI) { ai.BreakerFailures = 1 })
			_, err := s.DialogTurn(context.Background(), dialogReq(), nil)
			ae := aiKind(t, err)
			if ae.Kind != c.kind || ae.RetryAfter != c.retryAfter {
				t.Errorf("kind=%d retryAfter=%d, want %d, %d (%v)", ae.Kind, ae.RetryAfter, c.kind, c.retryAfter, err)
			}
			if got := s.BreakerState() == BreakerOpen; got != c.breakerFailed {
				t.Errorf("breaker open = %v, want %v", got, c.breakerFailed)
			}
		})
	}
}

func TestDialogTurnJSON(t *testing.T) {
	t.Parallel()
	f := newFakeAI(t, replyStatus(200, nil, dialogReply))
	s := newTestService(t, nil, f.URL)
	req := dialogReq()
	res, err := s.DialogTurn(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Caller.Text != "У нас пожар на кухне!" || res.TurnNo != 1 || res.Engine.DurationMs != 4200 {
		t.Errorf("decoded %+v", res)
	}
	seen := f.requests()
	if len(seen) != 1 {
		t.Fatalf("requests = %d", len(seen))
	}
	r := seen[0]
	if r.Method != http.MethodPost || r.Path != "/v1/dialog/turn" || r.Token != testToken || r.ContentType != "application/json" {
		t.Errorf("request %+v", r)
	}
	var sent aiservice.DialogTurnRequest
	if err := json.Unmarshal(r.Body, &sent); err != nil || sent.RequestId != req.RequestId || *sent.OperatorText != *req.OperatorText {
		t.Errorf("sent body %s (%v)", r.Body, err)
	}
	if st := s.Stats(); st.SyncCalls != 1 || st.SyncFailures != 0 {
		t.Errorf("stats %+v", st)
	}
}

func TestDialogTurnMultipartStreamsAudio(t *testing.T) {
	t.Parallel()
	audio := bytes.Repeat([]byte("OggS\x00opus"), 20000) // ~200 КБ
	var gotAudio []byte
	var gotReq aiservice.DialogTurnRequest
	var gotName, gotCT string
	f := newFakeAI(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			data, _ := io.ReadAll(p)
			switch p.FormName() {
			case "request":
				_ = json.Unmarshal(data, &gotReq)
			case "audio":
				gotAudio, gotName, gotCT = data, p.FileName(), p.Header.Get("Content-Type")
			}
		}
		replyStatus(200, nil, dialogReply)(w, r, body)
	})
	s := newTestService(t, nil, f.URL)
	req := dialogReq()
	req.OperatorText = nil
	_, err := s.DialogTurn(context.Background(), req, &core.AudioInput{
		Reader: bytes.NewReader(audio), Filename: `реплика "1".webm`, ContentType: "audio/webm",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotAudio, audio) {
		t.Errorf("audio: got %d bytes, want %d", len(gotAudio), len(audio))
	}
	if gotReq.RequestId != req.RequestId {
		t.Errorf("request part: %+v", gotReq)
	}
	if gotName != `реплика "1".webm` || gotCT != "audio/webm" {
		t.Errorf("audio part name=%q type=%q", gotName, gotCT)
	}
	if !strings.HasPrefix(f.requests()[0].ContentType, "multipart/form-data; boundary=") {
		t.Errorf("content type %q", f.requests()[0].ContentType)
	}
}

func TestDialogTurnMultipartDefaults(t *testing.T) {
	t.Parallel()
	var name, ct string
	f := newFakeAI(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		for p, err := mr.NextPart(); err == nil; p, err = mr.NextPart() {
			if p.FormName() == "audio" {
				name, ct = p.FileName(), p.Header.Get("Content-Type")
			}
		}
		replyStatus(200, nil, dialogReply)(w, r, body)
	})
	s := newTestService(t, nil, f.URL)
	if _, err := s.DialogTurn(context.Background(), dialogReq(), &core.AudioInput{Reader: strings.NewReader("x"), Filename: "  "}); err != nil {
		t.Fatal(err)
	}
	if name != defaultAudioName || ct != "application/octet-stream" {
		t.Errorf("defaults: name=%q ct=%q", name, ct)
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

// Обрыв загрузки аудио у студента — не отказ ai-service: breaker не трогаем.
func TestDialogTurnAudioSourceErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		kind core.AIErrorKind
	}{
		{"client upload broken", errors.New("unexpected EOF"), core.AIBadRequest},
		{"body limit exceeded", &http.MaxBytesError{Limit: 10}, core.AIAudioTooLong},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeAI(t, replyStatus(200, nil, dialogReply))
			s := newTestService(t, nil, f.URL)
			setAI(s, func(ai *settings.AI) { ai.BreakerFailures = 1 })
			_, err := s.DialogTurn(context.Background(), dialogReq(), &core.AudioInput{Reader: failingReader{c.err}})
			if ae := aiKind(t, err); ae.Kind != c.kind {
				t.Errorf("kind = %d, want %d (%v)", ae.Kind, c.kind, err)
			}
			if s.BreakerState() != BreakerClosed {
				t.Errorf("breaker = %s after client-side audio error", s.BreakerState())
			}
		})
	}
}

func TestSyncCallBreakerOpenFailsFast(t *testing.T) {
	t.Parallel()
	f := newFakeAI(t, replyStatus(200, nil, dialogReply))
	s := newTestService(t, nil, f.URL)
	setAI(s, func(ai *settings.AI) { ai.BreakerFailures = 1; ai.BreakerOpenSec = 3600 })
	s.br.Failure()
	start := time.Now()
	_, err := s.DialogTurn(context.Background(), dialogReq(), nil)
	if ae := aiKind(t, err); ae.Kind != core.AIUnavailable {
		t.Fatalf("kind = %d", ae.Kind)
	}
	if time.Since(start) > time.Second || len(f.requests()) != 0 {
		t.Error("open breaker must fail without calling ai-service")
	}
	if s.Available() {
		t.Error("Available() must be false while breaker is open")
	}
}

func TestSyncCallUnreachableOpensBreaker(t *testing.T) {
	t.Parallel()
	s := newTestService(t, nil, "") // порт 1 — connection refused
	setAI(s, func(ai *settings.AI) { ai.BreakerFailures = 2 })
	var states []string
	s.OnBreakerChange(func(st string) { states = append(states, st) })
	for i := 0; i < 2; i++ {
		_, err := s.TTSSync(context.Background(), &aiservice.TtsSyncRequest{Text: "Ожидайте", TextHash: "h"})
		if ae := aiKind(t, err); ae.Kind != core.AIUnavailable {
			t.Fatalf("kind = %d", ae.Kind)
		}
	}
	if s.BreakerState() != BreakerOpen || len(states) != 1 || states[0] != BreakerOpen {
		t.Errorf("breaker %s, hook states %v", s.BreakerState(), states)
	}
	if s.Available() {
		t.Error("Available() with open breaker")
	}
	if s.unavailSince.Load() == 0 {
		t.Error("breaker opened but outage start was not recorded")
	}
	if st := s.Stats(); st.SyncFailures != 2 {
		t.Errorf("sync failures = %d", st.SyncFailures)
	}
}

// Уход вызывающего (студент закрыл вкладку) — не отказ ai-service.
func TestSyncCallCallerCancelDoesNotTripBreaker(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	f := newFakeAI(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	s := newTestService(t, nil, f.URL)
	setAI(s, func(ai *settings.AI) { ai.BreakerFailures = 1 })
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := s.DialogTurn(ctx, dialogReq(), nil)
	if ae := aiKind(t, err); ae.Kind != core.AIUnavailable || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if s.BreakerState() != BreakerClosed {
		t.Errorf("breaker = %s after caller cancel", s.BreakerState())
	}
}

func TestSyncCallTimeoutTripsBreaker(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	f := newFakeAI(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	s := newTestService(t, nil, f.URL)
	setAI(s, func(ai *settings.AI) { ai.BreakerFailures = 1; ai.DialogTimeoutSec = 0 })
	s.cfg.AISyncTimeout = 100 * time.Millisecond
	_, err := s.DialogTurn(context.Background(), dialogReq(), nil)
	ae := aiKind(t, err)
	if ae.Kind != core.AIUnavailable || !strings.Contains(ae.Message, "вовремя") {
		t.Fatalf("err = %v", err)
	}
	if s.BreakerState() != BreakerOpen {
		t.Errorf("breaker = %s after ai-service timeout", s.BreakerState())
	}
}

func TestDialogTimeoutSource(t *testing.T) {
	t.Parallel()
	s := newTestService(t, nil, "")
	setAI(s, func(ai *settings.AI) { ai.DialogTimeoutSec = 7 })
	if d := s.dialogTimeout(); d != 7*time.Second {
		t.Errorf("settings: %v", d)
	}
	setAI(s, func(ai *settings.AI) { ai.DialogTimeoutSec = 0 })
	s.cfg.AISyncTimeout = 3 * time.Second
	if d := s.dialogTimeout(); d != 3*time.Second {
		t.Errorf("config: %v", d)
	}
	s.cfg.AISyncTimeout = 0
	if d := s.dialogTimeout(); d != 20*time.Second {
		t.Errorf("contract default: %v", d)
	}
}

func TestNilSyncRequests(t *testing.T) {
	t.Parallel()
	s := newTestService(t, nil, "")
	if _, err := s.DialogTurn(context.Background(), nil, nil); aiKind(t, err).Kind != core.AIBadRequest {
		t.Error("nil dialog request")
	}
	if _, err := s.TTSSync(context.Background(), nil); aiKind(t, err).Kind != core.AIBadRequest {
		t.Error("nil tts request")
	}
}

func TestTTSSync(t *testing.T) {
	t.Parallel()
	f := newFakeAI(t, replyStatus(200, nil, `{"file_path":"ab/cd.wav","duration_ms":900,"text_hash":"h1"}`))
	s := newTestService(t, nil, f.URL)
	res, err := s.TTSSync(context.Background(), &aiservice.TtsSyncRequest{Text: "Ожидайте на линии", TextHash: "h1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.FilePath != "ab/cd.wav" || res.DurationMs != 900 {
		t.Errorf("result %+v", res)
	}
	if p := f.requests()[0].Path; p != "/v1/tts/sync" {
		t.Errorf("path %s", p)
	}
}

func TestHealthAndQueueCached(t *testing.T) {
	t.Parallel()
	f := newFakeAI(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		switch r.URL.Path {
		case "/v1/health":
			replyStatus(200, nil, `{"status":"ok","ollama":true}`)(w, r, body)
		case "/v1/queue":
			replyStatus(200, nil, `{"running":2}`)(w, r, body) // pending не прислан
		default:
			http.NotFound(w, r)
		}
	})
	s := newTestService(t, nil, f.URL)
	for i := 0; i < 3; i++ {
		h, err := s.Health(context.Background())
		if err != nil || h.Status != "ok" {
			t.Fatalf("health: %+v %v", h, err)
		}
		q, err := s.Queue(context.Background())
		if err != nil || q.Running != 2 || q.Pending == nil {
			t.Fatalf("queue: %+v %v", q, err)
		}
	}
	if n := len(f.requests()); n != 2 {
		t.Errorf("ai-service called %d times, want 2 (cache)", n)
	}
}

func TestHealthErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status int
		kind   core.AIErrorKind
	}{
		{503, core.AIBusy}, {500, core.AIUnavailable}, {401, core.AIUnavailable}, {200, core.AIUnavailable},
	}
	for _, c := range cases {
		f := newFakeAI(t, replyStatus(c.status, nil, `not json`))
		s := newTestService(t, nil, f.URL)
		_, err := s.Health(context.Background())
		if ae := aiKind(t, err); ae.Kind != c.kind {
			t.Errorf("HTTP %d: kind %d, want %d", c.status, ae.Kind, c.kind)
		}
	}
	// Отменённый контекст вызывающего — ошибка сразу, без ожидания общего запроса.
	s := newTestService(t, nil, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Queue(ctx); aiKind(t, err).Kind != core.AIUnavailable {
		t.Error("cancelled queue call")
	}
}
