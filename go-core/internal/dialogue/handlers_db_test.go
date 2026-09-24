package dialogue

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/httpx"
)

// HTTP-тесты разговора поверх настоящей БД (pgtest): маршрут -> httpx.Router (CSRF, RBAC,
// ApiError) -> сервис -> PostgreSQL; ai-service, очередь и WebSocket — фейки.

func (w *world) path(suffix string) string {
	return "/attempts/" + w.attempt.String() + "/dialogue" + suffix
}

func (hs *harness) textTurn(t testing.TB, w *world, who as, turnNo int, text string) *httptest.ResponseRecorder {
	t.Helper()
	return hs.json(t, who, http.MethodPost, w.path("/turns"), map[string]any{"turnNo": turnNo, "text": text})
}

func (hs *harness) audioTurn(t testing.TB, w *world, who as, turnNo int, audio []byte, recordedAt *time.Time) *httptest.ResponseRecorder {
	t.Helper()
	parts := []formPart{
		{name: "turnNo", data: []byte(strings.TrimSpace(jsonNum(turnNo)))},
		{name: "audio", filename: "turn.webm", ctype: "audio/webm;codecs=opus", data: audio},
	}
	if recordedAt != nil {
		parts = append(parts, formPart{name: "clientRecordedAt", data: []byte(recordedAt.Format(time.RFC3339Nano))})
	}
	body, ct := multipartBody(t, parts...)
	return hs.do(t, who, http.MethodPost, w.path("/turns"), body, ct)
}

func jsonNum(n int) string { b, _ := json.Marshal(n); return string(b) }

// requireKeys — обязательные поля контракта присутствуют в JSON.
func requireKeys(t testing.TB, raw []byte, keys ...string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("json: %v; %s", err, raw)
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing %q: %s", k, raw)
		}
	}
	return m
}

// ---------------------------------------------------------------- доступ

func TestDialogueAccess(t *testing.T) {
	t.Parallel()
	hs, w := dbWorld(t, seedOpts{})

	t.Run("GET", func(t *testing.T) {
		expectError(t, hs.do(t, as{}, http.MethodGet, w.path(""), nil, ""), http.StatusUnauthorized, httpx.CodeUnauthorized)
		expectError(t, hs.do(t, w.asStudent2(), http.MethodGet, w.path(""), nil, ""), http.StatusForbidden, httpx.CodeForbidden)
		expectError(t, hs.do(t, w.asOtherT(), http.MethodGet, w.path(""), nil, ""), http.StatusForbidden, httpx.CodeForbidden)
		expectError(t, hs.do(t, w.asStudent(), http.MethodGet, "/attempts/"+uuid.NewString()+"/dialogue", nil, ""), http.StatusNotFound, httpx.CodeNotFound)
		expectError(t, hs.do(t, w.asStudent(), http.MethodGet, "/attempts/not-a-uuid/dialogue", nil, ""), http.StatusNotFound, httpx.CodeNotFound)
		for _, who := range []as{w.asStudent(), w.asTeacher(), w.asAdmin()} {
			rec := hs.do(t, who, http.MethodGet, w.path(""), nil, "")
			expectStatus(t, rec, http.StatusOK)
			m := requireKeys(t, rec.Body.Bytes(), "attemptId", "turns", "callEnded", "input", "nextTurnNo", "turnsLeft")
			if m["attemptId"] != w.attempt.String() || m["input"] != "both" || m["nextTurnNo"] != float64(1) || m["turnsLeft"] != float64(12) || m["callEnded"] != false {
				t.Fatalf("%s: %s", who.role, rec.Body.String())
			}
			turns := m["turns"].([]any)
			if len(turns) != 1 {
				t.Fatalf("opening turn expected: %s", rec.Body.String())
			}
			t0 := turns[0].(map[string]any)
			if t0["turnNo"] != float64(0) || t0["speaker"] != "caller" || t0["source"] != "script" || t0["atMs"] != float64(0) ||
				t0["text"] != "Алло! Пожар! У соседей дым валит!" || t0["emotionalState"] != "паника" {
				t.Fatalf("opening: %v", t0)
			}
		}
	})

	t.Run("POST turns", func(t *testing.T) {
		body := map[string]any{"turnNo": 1, "text": "Что случилось?"}
		expectError(t, hs.json(t, as{}, http.MethodPost, w.path("/turns"), body), http.StatusUnauthorized, httpx.CodeUnauthorized)
		expectError(t, hs.json(t, w.asTeacher(), http.MethodPost, w.path("/turns"), body), http.StatusForbidden, httpx.CodeForbidden)
		expectError(t, hs.json(t, w.asAdmin(), http.MethodPost, w.path("/turns"), body), http.StatusForbidden, httpx.CodeForbidden)
		expectError(t, hs.json(t, w.asStudent2(), http.MethodPost, w.path("/turns"), body), http.StatusForbidden, httpx.CodeForbidden)
		expectError(t, hs.json(t, w.asStudent(), http.MethodPost, "/attempts/"+uuid.NewString()+"/dialogue/turns", body), http.StatusNotFound, httpx.CodeNotFound)

		// без X-Requested-With (CSRF) — 403 до хендлера
		req := httptest.NewRequest(http.MethodPost, "/api/v1"+w.path("/turns"), strings.NewReader(`{"turnNo":1,"text":"Алло"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-Role", "student")
		req.Header.Set("X-Test-User", w.student.String())
		rec := httptest.NewRecorder()
		hs.h.ServeHTTP(rec, req)
		expectError(t, rec, http.StatusForbidden, httpx.CodeForbidden)

		// не JSON и не multipart
		expectStatus(t, hs.do(t, w.asStudent(), http.MethodPost, w.path("/turns"), strings.NewReader("turnNo=1"), "text/plain"), http.StatusUnsupportedMediaType)

		if n := hs.ai.callCount(); n != 0 {
			t.Fatalf("ai-service must not be called: %d", n)
		}
		if n := countTurns(t, w.pool, w.attempt); n != 1 {
			t.Fatalf("turns stored: %d", n)
		}
	})

	t.Run("POST end", func(t *testing.T) {
		expectError(t, hs.json(t, as{}, http.MethodPost, w.path("/end"), nil), http.StatusUnauthorized, httpx.CodeUnauthorized)
		expectError(t, hs.json(t, w.asTeacher(), http.MethodPost, w.path("/end"), nil), http.StatusForbidden, httpx.CodeForbidden)
		expectError(t, hs.json(t, w.asStudent2(), http.MethodPost, w.path("/end"), nil), http.StatusForbidden, httpx.CodeForbidden)
		expectError(t, hs.json(t, w.asStudent(), http.MethodPost, "/attempts/"+uuid.NewString()+"/dialogue/end", nil), http.StatusNotFound, httpx.CodeNotFound)
		if r := readAttempt(t, w.pool, w.attempt); r.endedAt != nil {
			t.Fatal("call must stay open")
		}
	})
}

// ---------------------------------------------------------------- ход текстом

func TestTextTurn(t *testing.T) {
	t.Parallel()
	hs, w := dbWorld(t, seedOpts{})
	hs.ai.respond(func(req *aiservice.DialogTurnRequest, _ []byte) (*aiservice.DialogTurnResult, error) {
		res := aiReply(req, "Третий подъезд! Дым из окна!")
		res.Caller.RevealedFactIds = &[]string{"f_entrance", "f_secret", "f_made_up"}
		res.Caller.EmotionalState = ptr("паника")
		return res, nil
	})

	before := time.Now()
	rec := hs.textTurn(t, w, w.asStudent(), 1, "  Какой подъезд?  ")
	after := time.Now()
	expectStatus(t, rec, http.StatusOK)
	requireKeys(t, rec.Body.Bytes(), "turnNo", "caller", "callEnded", "noSpeech", "fallback", "nextTurnNo", "operator", "latencyMs")
	resp := decode[public.DialogueTurnResponse](t, rec)
	if resp.TurnNo != 1 || resp.NextTurnNo != 2 || resp.CallEnded || resp.NoSpeech || resp.Fallback || resp.EndReason != nil || resp.LatencyMs == nil {
		t.Fatalf("resp: %s", rec.Body.String())
	}
	if resp.Operator == nil || resp.Operator.Text != "Какой подъезд?" || resp.Operator.Source != "text" || resp.Operator.Speaker != "operator" || resp.Operator.TurnNo != 1 {
		t.Fatalf("operator: %+v", resp.Operator)
	}
	if resp.Caller.Text != "Третий подъезд! Дым из окна!" || resp.Caller.Source != "llm" || resp.Caller.Speaker != "caller" ||
		resp.Caller.EmotionalState == nil || resp.Caller.Audio != nil || resp.Caller.DurationMs == nil {
		t.Fatalf("caller: %+v", resp.Caller)
	}
	// время реплики оператора — приход запроса (сервер), смещение от принятия вызова
	opAt := *resp.Operator.At
	if opAt.Before(before.Add(-time.Millisecond)) || opAt.After(after) {
		t.Fatalf("operator at %s not in [%s, %s]", opAt, before, after)
	}
	if want := int(opAt.Sub(w.acceptedAt).Milliseconds()); resp.Operator.AtMs != want || resp.Caller.AtMs < resp.Operator.AtMs {
		t.Fatalf("atMs op=%d (want %d) caller=%d", resp.Operator.AtMs, want, resp.Caller.AtMs)
	}

	// запрос в ai-service: легенда, история со вступлением, текст оператора
	call := hs.ai.lastCall()
	if call.req.TurnNo != 1 || call.req.OperatorText == nil || *call.req.OperatorText != "Какой подъезд?" || call.audio != nil ||
		len(call.req.History) != 1 || call.req.History[0].TurnNo != 1 || call.req.History[0].Speaker != "caller" ||
		call.req.CallScript.Dialogue == nil || call.req.AttemptId != w.attempt {
		t.Fatalf("ai request: %+v", call.req)
	}

	// БД: обмен, счётчик, события; факты — только разрешённые брифом
	if r := readAttempt(t, w.pool, w.attempt); r.turns != 3 || r.endedAt != nil {
		t.Fatalf("attempt: %+v", r)
	}
	if got := eventTypes(t, w.pool, w.attempt); !reflect.DeepEqual(got, []string{"dialogue_operator", "dialogue_caller"}) {
		t.Fatalf("events: %v", got)
	}
	var revealed []string
	var meta []byte
	if err := w.pool.QueryRow(context.Background(), `SELECT revealed_fact_ids, meta FROM attempt_dialogue_turns
	     WHERE attempt_id = $1 AND turn_no = 1 AND speaker = 'caller'`, w.attempt).Scan(&revealed, &meta); err != nil {
		t.Fatal(err)
	}
	var m model.TurnMeta
	if err := json.Unmarshal(meta, &m); err != nil || !reflect.DeepEqual(revealed, []string{"f_entrance"}) ||
		m.Engine["llm_model"] != "qwen2.5:3b" || m.LatencyMs < 0 || m.Fallback {
		t.Fatalf("stored caller: %v %s", revealed, meta)
	}

	// WS: вступление (accept) + две реплики + два события — преподавателю; студенту ничего
	if got, want := hs.pub.monitorTypes(), []string{"dialogueTurn", "dialogueTurn", "dialogueTurn", "attemptEvent:dialogue_operator", "attemptEvent:dialogue_caller"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("monitor: %v", got)
	}
	if got := hs.pub.studentTypes(); len(got) != 0 {
		t.Fatalf("student ws: %v", got)
	}

	// второй ход: ai-service получает раскрытые факты и сквозную нумерацию
	expectStatus(t, hs.textTurn(t, w, w.asStudent(), 2, "Сколько человек в квартире?"), http.StatusOK)
	call = hs.ai.lastCall()
	if !reflect.DeepEqual(*call.req.RevealedFactIds, []string{"f_entrance"}) || len(call.req.History) != 3 || call.req.History[2].TurnNo != 3 {
		t.Fatalf("second request: revealed=%v history=%+v", *call.req.RevealedFactIds, call.req.History)
	}

	// состояние после двух ходов
	rec = hs.do(t, w.asTeacher(), http.MethodGet, w.path(""), nil, "")
	st := decode[public.DialogueState](t, rec)
	if len(st.Turns) != 5 || *st.NextTurnNo != 3 || *st.TurnsLeft != 10 || st.CallEnded {
		t.Fatalf("state: %s", rec.Body.String())
	}
	for i, want := range []struct {
		no      int
		speaker string
	}{{0, "caller"}, {1, "operator"}, {1, "caller"}, {2, "operator"}, {2, "caller"}} {
		if st.Turns[i].TurnNo != want.no || string(st.Turns[i].Speaker) != want.speaker {
			t.Fatalf("order at %d: %+v", i, st.Turns[i])
		}
	}
}

func TestTurnDuplicateAndOrdering(t *testing.T) {
	t.Parallel()
	hs, w := dbWorld(t, seedOpts{})
	first := hs.textTurn(t, w, w.asStudent(), 1, "Адрес?")
	expectStatus(t, first, http.StatusOK)
	stored := decode[public.DialogueTurnResponse](t, first)

	// повтор после обрыва: 409 с сохранённым ответом, ai-service не вызывается
	calls := hs.ai.callCount()
	e := expectError(t, hs.textTurn(t, w, w.asStudent(), 1, "Адрес?"), http.StatusConflict, httpx.CodeConflict)
	if e.Details["nextTurnNo"] != float64(2) {
		t.Fatalf("details: %v", e.Details)
	}
	b, _ := json.Marshal(e.Details["response"])
	var dup public.DialogueTurnResponse
	if err := json.Unmarshal(b, &dup); err != nil {
		t.Fatal(err)
	}
	if dup.TurnNo != 1 || dup.NextTurnNo != 2 || dup.Caller.Text != stored.Caller.Text || dup.Operator == nil || dup.Operator.Text != "Адрес?" ||
		!dup.Operator.At.Equal(*stored.Operator.At) || dup.Caller.AtMs != stored.Caller.AtMs {
		t.Fatalf("stored response: %s vs %+v", b, stored)
	}
	if hs.ai.callCount() != calls {
		t.Fatal("duplicate must not call ai-service")
	}

	// номер из будущего
	e = expectError(t, hs.textTurn(t, w, w.asStudent(), 5, "Алло"), http.StatusConflict, httpx.CodeConflict)
	if e.Details["nextTurnNo"] != float64(2) {
		t.Fatalf("details: %v", e.Details)
	}
	if n := countTurns(t, w.pool, w.attempt); n != 3 {
		t.Fatalf("turns: %d", n)
	}
}

// ---------------------------------------------------------------- ход голосом

func TestAudioTurn(t *testing.T) {
	t.Parallel()
	hs, w := dbWorld(t, seedOpts{tts: true})
	hs.ai.respond(func(req *aiservice.DialogTurnRequest, audio []byte) (*aiservice.DialogTurnResult, error) {
		res := aiReply(req, "Подъезд третий!")
		res.Operator = components.SttResult{Text: "Какой подъезд?", TextRaw: ptr("какой подъезд"), Confidence: ptr(float32(0.8)), AudioDurationMs: 2000}
		res.Caller.Tts = &components.TtsResult{FilePath: "dialog/" + req.AttemptId.String() + "/" + jsonNum(req.TurnNo) + ".wav", DurationMs: 1700, TextHash: "h"}
		return res, nil
	})

	audio := webm(4096)
	recorded := time.Now().Add(-5 * time.Second).UTC().Truncate(time.Microsecond)
	rec := hs.audioTurn(t, w, w.asStudent(), 1, audio, &recorded)
	expectStatus(t, rec, http.StatusOK)
	resp := decode[public.DialogueTurnResponse](t, rec)
	op := resp.Operator
	if op == nil || op.Text != "Какой подъезд?" || op.Source != "stt" || op.Confidence == nil || *op.Confidence != float32(0.8) ||
		op.DurationMs == nil || *op.DurationMs != 2000 || op.Audio != nil {
		t.Fatalf("operator: %s", rec.Body.String())
	}
	// часы браузера в окне сервера — время реплики клиентское
	if !op.At.Equal(recorded) || op.AtMs != int(recorded.Sub(w.acceptedAt).Milliseconds()) {
		t.Fatalf("operator at %s (atMs %d), want %s", op.At, op.AtMs, recorded)
	}
	c := resp.Caller
	wantURL := "/api/v1/media/tts/dialog/" + w.attempt.String() + "/1.wav"
	if c.Audio == nil || c.Audio.AudioUrl != wantURL || c.Audio.DurationMs != 1700 || c.Audio.Mime == nil || *c.Audio.Mime != "audio/wav" || *c.DurationMs != 1700 {
		t.Fatalf("caller audio: %s", rec.Body.String())
	}
	call := hs.ai.lastCall()
	if string(call.audio) != string(audio) || call.ctype != "audio/webm" || call.fname != "turn.webm" || call.req.OperatorText != nil ||
		call.req.Options.Stt == nil || call.req.Options.Tts == nil || !*call.req.Options.Tts.Enabled {
		t.Fatalf("ai call: ctype=%s fname=%s len=%d opts=%+v", call.ctype, call.fname, len(call.audio), call.req.Options)
	}
	var textRaw string
	if err := w.pool.QueryRow(context.Background(), `SELECT meta->>'text_raw' FROM attempt_dialogue_turns
	     WHERE attempt_id = $1 AND turn_no = 1 AND speaker = 'operator'`, w.attempt).Scan(&textRaw); err != nil || textRaw != "какой подъезд" {
		t.Fatalf("text_raw: %q %v", textRaw, err)
	}

	// Регрессия (часы браузера спешат на 30 с): время реплики — серверная оценка
	// «приход запроса − длительность записи», а не будущее по часам студента.
	// Ответ заявителя «прозвучал» 10 с назад: оператор его выслушал и записал реплику.
	mustExec(t, w.pool, `UPDATE attempt_dialogue_turns SET at = at - interval '10 seconds' WHERE attempt_id = $1 AND turn_no = 1`, w.attempt)
	ahead := time.Now().Add(30 * time.Second)
	before := time.Now()
	rec = hs.audioTurn(t, w, w.asStudent(), 2, audio, &ahead)
	after := time.Now()
	expectStatus(t, rec, http.StatusOK)
	resp = decode[public.DialogueTurnResponse](t, rec)
	opAt := *resp.Operator.At
	if opAt.Before(before.Add(-2*time.Second-time.Millisecond)) || opAt.After(after.Add(-2*time.Second)) {
		t.Fatalf("skewed client clock: operator at %s, want ≈ arrival−2s in [%s, %s]", opAt, before.Add(-2*time.Second), after.Add(-2*time.Second))
	}
	if resp.Caller.AtMs < resp.Operator.AtMs {
		t.Fatalf("caller before operator: %d < %d", resp.Caller.AtMs, resp.Operator.AtMs)
	}

	// Часы отстают на 10 минут — раньше ответа заявителя; тоже серверная оценка.
	behind := time.Now().Add(-10 * time.Minute)
	prevCaller := *resp.Caller.At
	rec = hs.audioTurn(t, w, w.asStudent(), 3, audio, &behind)
	expectStatus(t, rec, http.StatusOK)
	resp = decode[public.DialogueTurnResponse](t, rec)
	if resp.Operator.At.Before(prevCaller) {
		t.Fatalf("operator %s before previous caller reply %s", resp.Operator.At, prevCaller)
	}
}

func TestAudioTurnRejected(t *testing.T) {
	t.Parallel()
	hs, w := dbWorld(t, seedOpts{input: "text"})
	e := expectError(t, hs.audioTurn(t, w, w.asStudent(), 1, webm(100), nil), http.StatusBadRequest, httpx.CodeValidation)
	if e.Details["fields"] == nil {
		t.Fatalf("details: %v", e.Details)
	}
	if hs.ai.callCount() != 0 || countTurns(t, w.pool, w.attempt) != 1 {
		t.Fatal("nothing must happen")
	}
	// текстом в текстовом занятии — можно
	expectStatus(t, hs.textTurn(t, w, w.asStudent(), 1, "Адрес?"), http.StatusOK)
}

func TestNoSpeech(t *testing.T) {
	t.Parallel()
	hs, w := dbWorld(t, seedOpts{})
	hs.ai.respond(func(req *aiservice.DialogTurnRequest, _ []byte) (*aiservice.DialogTurnResult, error) {
		res := aiReply(req, "Алло? Не слышу вас!")
		res.Operator = components.SttResult{Text: "", NoSpeech: ptr(true), AudioDurationMs: 900}
		res.Caller.RevealedFactIds = &[]string{"f_smoke"}
		return res, nil
	})
	monitorBefore := len(hs.pub.monitorTypes())
	rec := hs.audioTurn(t, w, w.asStudent(), 1, webm(200), nil)
	expectStatus(t, rec, http.StatusOK)
	resp := decode[public.DialogueTurnResponse](t, rec)
	if !resp.NoSpeech || resp.NextTurnNo != 1 || resp.TurnNo != 1 || resp.Operator != nil || resp.CallEnded || resp.Fallback ||
		resp.Caller.Text != "Алло? Не слышу вас!" || resp.Caller.Source != "script" {
		t.Fatalf("no speech: %s", rec.Body.String())
	}
	if countTurns(t, w.pool, w.attempt) != 1 || len(eventTypes(t, w.pool, w.attempt)) != 0 || len(hs.pub.monitorTypes()) != monitorBefore {
		t.Fatal("no speech must not be stored or published")
	}
	// повтор тем же номером проходит
	hs.ai.respond(nil)
	expectStatus(t, hs.textTurn(t, w, w.asStudent(), 1, "Алло, говорите громче"), http.StatusOK)
}

// ---------------------------------------------------------------- ai-service недоступен

func TestAIUnavailable(t *testing.T) {
	t.Parallel()

	t.Run("занят — 503 caller_busy с Retry-After, попытка не страдает", func(t *testing.T) {
		t.Parallel()
		hs, w := dbWorld(t, seedOpts{})
		hs.ai.respond(func(*aiservice.DialogTurnRequest, []byte) (*aiservice.DialogTurnResult, error) {
			return nil, &core.AIError{Kind: core.AIBusy, RetryAfter: 4, Message: "busy"}
		})
		rec := hs.textTurn(t, w, w.asStudent(), 1, "Адрес?")
		expectError(t, rec, http.StatusServiceUnavailable, httpx.CodeCallerBusy)
		if rec.Header().Get("Retry-After") != "4" {
			t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
		}
		if countTurns(t, w.pool, w.attempt) != 1 {
			t.Fatal("nothing stored")
		}
		// повтор тем же номером после «занят»
		hs.ai.respond(nil)
		expectStatus(t, hs.textTurn(t, w, w.asStudent(), 1, "Адрес?"), http.StatusOK)
	})

	t.Run("недоступен, текст — ответ по сценарию", func(t *testing.T) {
		t.Parallel()
		hs, w := dbWorld(t, seedOpts{})
		hs.ai.respond(func(*aiservice.DialogTurnRequest, []byte) (*aiservice.DialogTurnResult, error) {
			return nil, &core.AIError{Kind: core.AIUnavailable, Message: "breaker open"}
		})
		rec := hs.textTurn(t, w, w.asStudent(), 1, "Адрес?")
		expectStatus(t, rec, http.StatusOK)
		resp := decode[public.DialogueTurnResponse](t, rec)
		if !resp.Fallback || resp.Caller.Source != "script" || resp.Caller.Text != "Улица Ленина, дом двенадцать!" || resp.Operator.Source != "text" || resp.CallEnded {
			t.Fatalf("fallback: %s", rec.Body.String())
		}
		// сохранённый ответ помнит fallback
		e := expectError(t, hs.textTurn(t, w, w.asStudent(), 1, "Адрес?"), http.StatusConflict, httpx.CodeConflict)
		if r, _ := e.Details["response"].(map[string]any); r == nil || r["fallback"] != true {
			t.Fatalf("stored fallback: %v", e.Details)
		}
	})

	t.Run("недоступен, аудио — 503 caller_busy", func(t *testing.T) {
		t.Parallel()
		hs, w := dbWorld(t, seedOpts{})
		hs.ai.respond(func(*aiservice.DialogTurnRequest, []byte) (*aiservice.DialogTurnResult, error) {
			return nil, &core.AIError{Kind: core.AIUnavailable, Message: "refused"}
		})
		rec := hs.audioTurn(t, w, w.asStudent(), 1, webm(100), nil)
		expectError(t, rec, http.StatusServiceUnavailable, httpx.CodeCallerBusy)
		if rec.Header().Get("Retry-After") != "5" {
			t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
		}
	})

	t.Run("аудио не принято ai-service — 415/413", func(t *testing.T) {
		t.Parallel()
		hs, w := dbWorld(t, seedOpts{})
		hs.ai.respond(func(*aiservice.DialogTurnRequest, []byte) (*aiservice.DialogTurnResult, error) {
			return nil, &core.AIError{Kind: core.AIAudioUnsupported, Message: "codec"}
		})
		expectError(t, hs.audioTurn(t, w, w.asStudent(), 1, webm(100), nil), http.StatusUnsupportedMediaType, httpx.CodeAudioUnsupported)
		hs.ai.respond(func(*aiservice.DialogTurnRequest, []byte) (*aiservice.DialogTurnResult, error) {
			return nil, &core.AIError{Kind: core.AIAudioTooLong, Message: "60s"}
		})
		expectError(t, hs.audioTurn(t, w, w.asStudent(), 1, webm(100), nil), http.StatusRequestEntityTooLarge, httpx.CodeAudioTooLong)
	})
}

// ---------------------------------------------------------------- завершение разговора

// fakeaiLimit — поведение ai-service на лимите брифа (python-ai-service.md, cmd/fakeai):
// turn_no ≥ max_turns → should_end=true, end_reason="max_turns".
func fakeaiLimit(req *aiservice.DialogTurnRequest, _ []byte) (*aiservice.DialogTurnResult, error) {
	res := aiReply(req, "Хорошо, жду.")
	if d := req.CallScript.Dialogue; d != nil && d.MaxTurns != nil && req.TurnNo >= *d.MaxTurns {
		res.Caller.Text = "Всё, я больше не могу говорить! Приезжайте скорее!"
		res.Caller.ShouldEnd = ptr(true)
		res.Caller.EndReason = ptr("max_turns")
	}
	return res, nil
}

// Регрессия (ревью: «лимит брифа записывается как caller_hung_up»).
func TestBriefMaxTurnsEndsWithMaxTurns(t *testing.T) {
	t.Parallel()
	hs, w := dbWorld(t, seedOpts{briefMax: 2, maxTurns: 12})
	hs.ai.respond(fakeaiLimit)

	rec := hs.textTurn(t, w, w.asStudent(), 1, "Адрес?")
	expectStatus(t, rec, http.StatusOK)
	if r := decode[public.DialogueTurnResponse](t, rec); r.CallEnded {
		t.Fatalf("turn 1 must continue: %s", rec.Body.String())
	}
	rec = hs.textTurn(t, w, w.asStudent(), 2, "Кто пострадал?")
	expectStatus(t, rec, http.StatusOK)
	resp := decode[public.DialogueTurnResponse](t, rec)
	if !resp.CallEnded || resp.EndReason == nil || *resp.EndReason != core.EndMaxTurns || resp.NextTurnNo != 3 {
		t.Fatalf("turn 2: %s", rec.Body.String())
	}
	if r := readAttempt(t, w.pool, w.attempt); r.endedAt == nil || r.endReason == nil || *r.endReason != core.EndMaxTurns || r.turns != 5 {
		t.Fatalf("attempt: %+v", r)
	}
	var payload string
	if err := w.pool.QueryRow(context.Background(), `SELECT payload->>'reason' FROM attempt_events WHERE attempt_id = $1 AND type = 'dialogue_ended'`,
		w.attempt).Scan(&payload); err != nil || payload != core.EndMaxTurns {
		t.Fatalf("dialogue_ended payload: %q %v", payload, err)
	}
	if got := hs.pub.studentTypes(); len(got) != 0 {
		t.Fatalf("no callerHungUp on max_turns: %v", got)
	}
	st := decode[public.DialogueState](t, hs.do(t, w.asStudent(), http.MethodGet, w.path(""), nil, ""))
	if !st.CallEnded || st.EndReason == nil || string(*st.EndReason) != core.EndMaxTurns || *st.TurnsLeft != 0 || st.CallEndedAt == nil {
		t.Fatalf("state: %+v", st)
	}
	e := expectError(t, hs.textTurn(t, w, w.asStudent(), 3, "Алло?"), http.StatusConflict, httpx.CodeConflict)
	if e.Details["endReason"] != core.EndMaxTurns || e.Details["nextTurnNo"] != float64(3) {
		t.Fatalf("details: %v", e.Details)
	}
	// повтор последнего хода отдаёт сохранённый ответ с завершением
	e = expectError(t, hs.textTurn(t, w, w.asStudent(), 2, "Кто пострадал?"), http.StatusConflict, httpx.CodeConflict)
	if r, _ := e.Details["response"].(map[string]any); r == nil || r["callEnded"] != true || r["endReason"] != core.EndMaxTurns {
		t.Fatalf("stored last turn: %v", e.Details)
	}
}

func TestLessonMaxTurns(t *testing.T) {
	t.Parallel()
	hs, w := dbWorld(t, seedOpts{maxTurns: 2})
	expectStatus(t, hs.textTurn(t, w, w.asStudent(), 1, "Адрес?"), http.StatusOK)
	rec := hs.textTurn(t, w, w.asStudent(), 2, "Этаж?")
	resp := decode[public.DialogueTurnResponse](t, rec)
	if !resp.CallEnded || resp.EndReason == nil || *resp.EndReason != core.EndMaxTurns {
		t.Fatalf("resp: %s", rec.Body.String())
	}
	if got := hs.pub.studentTypes(); len(got) != 0 {
		t.Fatalf("student ws: %v", got)
	}
}

func TestCallerHangsUp(t *testing.T) {
	t.Parallel()
	hs, w := dbWorld(t, seedOpts{})
	hs.ai.respond(func(req *aiservice.DialogTurnRequest, _ []byte) (*aiservice.DialogTurnResult, error) {
		res := aiReply(req, "Спасибо, жду пожарных!")
		res.Caller.ShouldEnd = ptr(true)
		res.Caller.EndReason = ptr("оператор сообщил, что помощь направлена")
		return res, nil
	})
	rec := hs.textTurn(t, w, w.asStudent(), 1, "Помощь направлена, оставайтесь на связи")
	resp := decode[public.DialogueTurnResponse](t, rec)
	if !resp.CallEnded || resp.EndReason == nil || *resp.EndReason != core.EndCallerHungUp {
		t.Fatalf("resp: %s", rec.Body.String())
	}
	if got := hs.pub.studentTypes(); !reflect.DeepEqual(got, []string{"callerHungUp"}) {
		t.Fatalf("student ws: %v", got)
	}
	hs.pub.mu.Lock()
	hung := hs.pub.student[0]
	hs.pub.mu.Unlock()
	if hung.Turn == nil || hung.Turn.Text != "Спасибо, жду пожарных!" {
		t.Fatalf("callerHungUp turn: %+v", hung.Turn)
	}
	if got := hs.pub.monitorTypes(); got[len(got)-1] != "attemptEvent:dialogue_ended" {
		t.Fatalf("monitor: %v", got)
	}
}

func TestEndDialogue(t *testing.T) {
	t.Parallel()
	hs, w := dbWorld(t, seedOpts{})
	expectStatus(t, hs.textTurn(t, w, w.asStudent(), 1, "Адрес?"), http.StatusOK)

	rec := hs.json(t, w.asStudent(), http.MethodPost, w.path("/end"), nil)
	expectStatus(t, rec, http.StatusOK)
	requireKeys(t, rec.Body.Bytes(), "attemptId", "turns", "callEnded", "input", "callEndedAt", "endReason")
	st := decode[public.DialogueState](t, rec)
	if !st.CallEnded || string(*st.EndReason) != core.EndOperatorHungUp || len(st.Turns) != 3 || *st.NextTurnNo != 2 {
		t.Fatalf("state: %s", rec.Body.String())
	}
	endedAt := *st.CallEndedAt
	if got := eventTypes(t, w.pool, w.attempt); got[len(got)-1] != "dialogue_ended" {
		t.Fatalf("events: %v", got)
	}
	if got := hs.pub.monitorTypes(); got[len(got)-1] != "attemptEvent:dialogue_ended" {
		t.Fatalf("monitor: %v", got)
	}

	// идемпотентно: второй раз — тот же ответ, без нового события
	nEvents := len(eventTypes(t, w.pool, w.attempt))
	rec = hs.json(t, w.asStudent(), http.MethodPost, w.path("/end"), nil)
	expectStatus(t, rec, http.StatusOK)
	st = decode[public.DialogueState](t, rec)
	if !st.CallEndedAt.Equal(endedAt) || string(*st.EndReason) != core.EndOperatorHungUp || len(eventTypes(t, w.pool, w.attempt)) != nEvents {
		t.Fatalf("repeat: %s", rec.Body.String())
	}
	// после трубки ход не принимается
	e := expectError(t, hs.textTurn(t, w, w.asStudent(), 2, "Алло?"), http.StatusConflict, httpx.CodeConflict)
	if e.Details["endReason"] != core.EndOperatorHungUp {
		t.Fatalf("details: %v", e.Details)
	}
}

func TestTurnAndEndStateConflicts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opts seedOpts
	}{
		{"вызов не принят", seedOpts{status: "issued", notAccepted: true}},
		{"попытка сдана", seedOpts{status: "submitted"}},
		{"голос выключен", seedOpts{voiceOff: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			hs, w := dbWorld(t, c.opts)
			expectError(t, hs.textTurn(t, w, w.asStudent(), 1, "Адрес?"), http.StatusConflict, httpx.CodeConflict)
			expectError(t, hs.json(t, w.asStudent(), http.MethodPost, w.path("/end"), nil), http.StatusConflict, httpx.CodeConflict)
			if hs.ai.callCount() != 0 {
				t.Fatal("ai-service must not be called")
			}
			// состояние читается всегда (turns — [], не null)
			rec := hs.do(t, w.asStudent(), http.MethodGet, w.path(""), nil, "")
			expectStatus(t, rec, http.StatusOK)
			if c.opts.voiceOff && !strings.Contains(rec.Body.String(), `"turns":[]`) {
				t.Fatalf("turns must be []: %s", rec.Body.String())
			}
		})
	}
}

// ---------------------------------------------------------------- один ход в полёте (HTTP)

func TestConcurrentSameTurn(t *testing.T) {
	t.Parallel()
	hs, w := dbWorld(t, seedOpts{})
	hs.ai.block = make(chan struct{})
	hs.ai.entered = make(chan struct{}, 1)

	var wg sync.WaitGroup
	recs := make([]*httptest.ResponseRecorder, 2)
	wg.Add(1)
	go func() { defer wg.Done(); recs[0] = hs.textTurn(t, w, w.asStudent(), 1, "Адрес?") }()
	<-hs.ai.entered

	// другой номер хода, пока первый в полёте, — 409 сразу
	e := expectError(t, hs.textTurn(t, w, w.asStudent(), 2, "Этаж?"), http.StatusConflict, httpx.CodeConflict)
	if e.Details != nil && e.Details["response"] != nil {
		t.Fatalf("busy must not carry a response: %v", e.Details)
	}

	wg.Add(1)
	go func() { defer wg.Done(); recs[1] = hs.textTurn(t, w, w.asStudent(), 1, "Адрес?") }()
	time.Sleep(150 * time.Millisecond) // дубль встаёт в ожидание первого
	close(hs.ai.block)
	wg.Wait()

	expectStatus(t, recs[0], http.StatusOK)
	first := decode[public.DialogueTurnResponse](t, recs[0])
	switch recs[1].Code {
	case http.StatusOK: // склеился с первым
		if dup := decode[public.DialogueTurnResponse](t, recs[1]); dup.Caller.Text != first.Caller.Text || !dup.Caller.At.Equal(*first.Caller.At) {
			t.Fatalf("dup differs: %s", recs[1].Body.String())
		}
	case http.StatusConflict: // опоздал — сохранённый ответ
		if !strings.Contains(recs[1].Body.String(), `"response"`) {
			t.Fatalf("dup 409 without response: %s", recs[1].Body.String())
		}
	default:
		t.Fatalf("dup: %d %s", recs[1].Code, recs[1].Body.String())
	}
	if n := countTurns(t, w.pool, w.attempt); n != 3 {
		t.Fatalf("exactly one exchange must be stored: %d rows", n)
	}
	if r := readAttempt(t, w.pool, w.attempt); r.turns != 3 {
		t.Fatalf("counter: %d", r.turns)
	}
}

// ---------------------------------------------------------------- перепроверка при записи

// Пока ai-service думал, состояние попытки поменялось: запись хода перепроверяет его под
// замком попытки (другой инстанс записал ход, попытку сдали, разговор закрыли).
func TestPersistRechecksUnderLock(t *testing.T) {
	t.Parallel()
	during := func(hs *harness, sql string, args ...any) {
		hs.ai.respond(func(req *aiservice.DialogTurnRequest, _ []byte) (*aiservice.DialogTurnResult, error) {
			if _, err := hs.pool.Exec(context.Background(), sql, args...); err != nil {
				return nil, err
			}
			return aiReply(req, "Жду!"), nil
		})
	}

	t.Run("ход записал другой инстанс — 409 с его ответом", func(t *testing.T) {
		t.Parallel()
		hs, w := dbWorld(t, seedOpts{})
		during(hs, `INSERT INTO attempt_dialogue_turns (attempt_id, turn_no, speaker, text, source, at, at_ms)
		            VALUES ($1, 1, 'operator', 'Адрес?', 'text', now(), 1000), ($1, 1, 'caller', 'С другого инстанса', 'llm', now(), 1500)`, w.attempt)
		e := expectError(t, hs.textTurn(t, w, w.asStudent(), 1, "Адрес?"), http.StatusConflict, httpx.CodeConflict)
		r, _ := e.Details["response"].(map[string]any)
		if r == nil || r["caller"].(map[string]any)["text"] != "С другого инстанса" || e.Details["nextTurnNo"] != float64(2) {
			t.Fatalf("details: %v", e.Details)
		}
		if got := eventTypes(t, w.pool, w.attempt); len(got) != 0 {
			t.Fatalf("events of the lost turn: %v", got)
		}
	})

	t.Run("попытку сдали во время хода", func(t *testing.T) {
		t.Parallel()
		hs, w := dbWorld(t, seedOpts{})
		during(hs, `UPDATE attempts SET status = 'submitted' WHERE id = $1`, w.attempt)
		expectError(t, hs.textTurn(t, w, w.asStudent(), 1, "Адрес?"), http.StatusConflict, httpx.CodeConflict)
		if countTurns(t, w.pool, w.attempt) != 1 || len(hs.pub.monitorTypes()) != 1 {
			t.Fatal("nothing must be stored or published")
		}
	})

	t.Run("разговор закрыли во время хода", func(t *testing.T) {
		t.Parallel()
		hs, w := dbWorld(t, seedOpts{})
		during(hs, `UPDATE attempts SET call_ended_at = now(), call_end_reason = 'timeout' WHERE id = $1`, w.attempt)
		expectError(t, hs.textTurn(t, w, w.asStudent(), 1, "Адрес?"), http.StatusConflict, httpx.CodeConflict)
		if r := readAttempt(t, w.pool, w.attempt); r.turns != 1 || *r.endReason != "timeout" {
			t.Fatalf("attempt: %+v", r)
		}
	})

	t.Run("попытку удалили во время хода — 404", func(t *testing.T) {
		t.Parallel()
		hs, w := dbWorld(t, seedOpts{})
		during(hs, `DELETE FROM attempts WHERE id = $1`, w.attempt)
		expectError(t, hs.textTurn(t, w, w.asStudent(), 1, "Адрес?"), http.StatusNotFound, httpx.CodeNotFound)
	})
}

func TestFallbackReplyWithCachedTTS(t *testing.T) {
	t.Parallel()
	hs, w := dbWorld(t, seedOpts{tts: true})
	hash := convert.TTSHash("Улица Ленина, дом двенадцать!", "xenia", 1)
	mustExec(t, w.pool, `INSERT INTO tts_cache (text_hash, voice, file_path, duration_ms) VALUES ($1, 'xenia', 'ef/reply.wav', 1900)`, hash)
	hs.ai.respond(func(*aiservice.DialogTurnRequest, []byte) (*aiservice.DialogTurnResult, error) {
		return nil, &core.AIError{Kind: core.AIUnavailable, Message: "down"}
	})
	rec := hs.textTurn(t, w, w.asStudent(), 1, "Адрес?")
	expectStatus(t, rec, http.StatusOK)
	resp := decode[public.DialogueTurnResponse](t, rec)
	if !resp.Fallback || resp.Caller.Audio == nil || resp.Caller.Audio.AudioUrl != "/api/v1/media/tts/ef/reply.wav" || *resp.Caller.DurationMs != 1900 {
		t.Fatalf("fallback with tts: %s", rec.Body.String())
	}
}
