package dialogue

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/config"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
)

// ---------------------------------------------------------------- разбор реплики

func turnRequest(t *testing.T, body io.Reader, ctype string) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/attempts/x/dialogue/turns", body)
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	return httptest.NewRecorder(), r
}

func TestReadJSONInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		body   string
		status int
		turnNo int
		text   string
	}{
		{"ок", `{"turnNo":2,"text":"  Назовите адрес  "}`, 0, 2, "Назовите адрес"},
		{"текст \\u-эскейпами", `{"turnNo":1,"text":"\u0410\u043b\u043b\u043e"}`, 0, 1, "Алло"},
		{"нет turnNo", `{"text":"Алло"}`, 400, 0, ""},
		{"turnNo 0", `{"turnNo":0,"text":"Алло"}`, 400, 0, ""},
		{"turnNo отрицательный", `{"turnNo":-1,"text":"Алло"}`, 400, 0, ""},
		{"нет текста", `{"turnNo":1}`, 400, 0, ""},
		{"пустой текст", `{"turnNo":1,"text":"   "}`, 400, 0, ""},
		{"слишком длинный текст", fmt.Sprintf(`{"turnNo":1,"text":%q}`, strings.Repeat("ю", 1001)), 400, 0, ""},
		{"битый JSON", `{"turnNo":`, 400, 0, ""},
		{"turnNo строкой", `{"turnNo":"1","text":"Алло"}`, 400, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w, r := turnRequest(t, strings.NewReader(c.body), "application/json")
			in, err := readTurnInput(w, r)
			if c.status != 0 {
				he := httpx.AsError(err)
				if err == nil || he.Status != c.status || he.Code != httpx.CodeValidation {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if in.turnNo != c.turnNo || in.text != c.text || in.isAudio() || in.recordedAt != nil {
				t.Fatalf("in = %+v", in)
			}
		})
	}
}

func TestReadMultipartInput(t *testing.T) {
	t.Parallel()
	rec := "2026-09-24T10:00:00.5+03:00"

	t.Run("фронт: turnNo, audio, clientRecordedAt — аудио потоком, время дочитывается после хода", func(t *testing.T) {
		t.Parallel()
		audio := webm(5000)
		body, ct := multipartBody(t,
			formPart{name: "turnNo", data: []byte("3")},
			formPart{name: "audio", filename: "turn.webm", ctype: "audio/webm;codecs=opus", data: audio},
			formPart{name: "clientRecordedAt", data: []byte(rec)})
		w, r := turnRequest(t, body, ct)
		in, err := readTurnInput(w, r)
		if err != nil {
			t.Fatal(err)
		}
		defer in.close()
		if in.turnNo != 3 || !in.isAudio() || in.audio.contentType != "audio/webm" || in.audio.filename != "turn.webm" {
			t.Fatalf("in = %+v audio=%+v", in, in.audio)
		}
		if in.recordedAt != nil || in.mr == nil {
			t.Fatal("clientRecordedAt идёт после аудио — ещё не прочитан")
		}
		got, err := io.ReadAll(in.audio.src)
		if err != nil || !bytes.Equal(got, audio) {
			t.Fatalf("audio: %d bytes, err=%v", len(got), err)
		}
		if err := in.audio.src.stop(); err != nil {
			t.Fatalf("stop: %v", err)
		}
		in.readTrailing()
		if in.recordedAt == nil || !in.recordedAt.Equal(time.Date(2026, 9, 24, 7, 0, 0, 5e8, time.UTC)) || in.mr != nil {
			t.Fatalf("recordedAt = %v", in.recordedAt)
		}
	})

	t.Run("чужой клиент: turnNo после аудио — аудио буферизуется", func(t *testing.T) {
		t.Parallel()
		audio := []byte("RIFF\x10\x00\x00\x00WAVEfmt data")
		body, ct := multipartBody(t,
			formPart{name: "clientRecordedAt", data: []byte(rec)},
			formPart{name: "audio", filename: "x.bin", ctype: "application/octet-stream", data: audio},
			formPart{name: "audio", filename: "second.webm", ctype: "audio/webm", data: webm(10)},
			formPart{name: "turnNo", data: []byte(" 7 ")})
		w, r := turnRequest(t, body, ct)
		in, err := readTurnInput(w, r)
		if err != nil {
			t.Fatal(err)
		}
		if in.turnNo != 7 || in.audio.contentType != "audio/wav" || in.audio.filename != "turn.wav" || in.recordedAt == nil || in.mr != nil {
			t.Fatalf("in = %+v", in)
		}
		got, _ := io.ReadAll(in.audio.src)
		if !bytes.Equal(got, audio) {
			t.Fatalf("buffered audio = %q", got)
		}
		in.readTrailing() // форма уже прочитана — не паникует
	})

	t.Run("текст формой без аудио", func(t *testing.T) {
		t.Parallel()
		body, ct := multipartBody(t, formPart{name: "turnNo", data: []byte("1")}, formPart{name: "text", data: []byte(" Где горит? ")})
		w, r := turnRequest(t, body, ct)
		in, err := readTurnInput(w, r)
		if err != nil || in.text != "Где горит?" || in.isAudio() {
			t.Fatalf("in=%+v err=%v", in, err)
		}
	})

	t.Run("кривой clientRecordedAt игнорируется", func(t *testing.T) {
		t.Parallel()
		body, ct := multipartBody(t, formPart{name: "clientRecordedAt", data: []byte("вчера")},
			formPart{name: "turnNo", data: []byte("1")}, formPart{name: "text", data: []byte("Алло")})
		w, r := turnRequest(t, body, ct)
		in, err := readTurnInput(w, r)
		if err != nil || in.recordedAt != nil {
			t.Fatalf("in=%+v err=%v", in, err)
		}
	})

	errCases := []struct {
		name   string
		parts  []formPart
		status int
		code   string
	}{
		{"нет turnNo", []formPart{{name: "audio", filename: "a.webm", ctype: "audio/webm", data: webm(20)}}, 400, httpx.CodeValidation},
		{"turnNo не число", []formPart{{name: "turnNo", data: []byte("два")}}, 400, httpx.CodeValidation},
		{"turnNo 0", []formPart{{name: "turnNo", data: []byte("0")}}, 400, httpx.CodeValidation},
		{"turnNo длинный", []formPart{{name: "turnNo", data: []byte(strings.Repeat("1", maxFieldBytes+1))}}, 400, httpx.CodeValidation},
		{"ни аудио, ни текста", []formPart{{name: "turnNo", data: []byte("1")}}, 400, httpx.CodeValidation},
		{"пустой текст", []formPart{{name: "turnNo", data: []byte("1")}, {name: "text", data: []byte("  ")}}, 400, httpx.CodeValidation},
		{"пустая запись", []formPart{{name: "turnNo", data: []byte("1")}, {name: "audio", filename: "a.webm", ctype: "audio/webm"}}, 400, httpx.CodeValidation},
		{"mp4 из Safari", []formPart{{name: "turnNo", data: []byte("1")}, {name: "audio", filename: "a.mp4", ctype: "audio/mp4", data: webm(20)}}, 415, httpx.CodeAudioUnsupported},
		{"неизвестная сигнатура", []formPart{{name: "turnNo", data: []byte("1")}, {name: "audio", filename: "a", data: []byte("ID3\x04\x00mp3")}}, 415, httpx.CodeAudioUnsupported},
		{"аудио > 2 МБ при буферизации", []formPart{{name: "audio", filename: "a.webm", ctype: "audio/webm", data: webm(maxAudioBytes + 10)}, {name: "turnNo", data: []byte("1")}}, 413, httpx.CodeAudioTooLong},
	}
	for _, c := range errCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			body, ct := multipartBody(t, c.parts...)
			w, r := turnRequest(t, body, ct)
			in, err := readTurnInput(w, r)
			if err == nil {
				in.close()
				t.Fatalf("want error, got %+v", in)
			}
			he := httpx.AsError(err)
			if he.Status != c.status || he.Code != c.code {
				t.Fatalf("got %d %s (%v), want %d %s", he.Status, he.Code, err, c.status, c.code)
			}
		})
	}

	t.Run("не multipart, а заявлен multipart", func(t *testing.T) {
		t.Parallel()
		w, r := turnRequest(t, strings.NewReader("garbage"), "multipart/form-data")
		_, err := readTurnInput(w, r)
		if he := httpx.AsError(err); he.Status != 400 {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("аудио > 2 МБ потоком — MaxBytesError у читателя", func(t *testing.T) {
		t.Parallel()
		body, ct := multipartBody(t, formPart{name: "turnNo", data: []byte("1")},
			formPart{name: "audio", filename: "a.webm", ctype: "audio/webm", data: webm(maxAudioBytes + 1)})
		w, r := turnRequest(t, body, ct)
		in, err := readTurnInput(w, r)
		if err != nil {
			t.Fatal(err)
		}
		_, rerr := io.ReadAll(in.audio.src)
		var mbe *http.MaxBytesError
		if !errors.As(rerr, &mbe) {
			t.Fatalf("read err = %v", rerr)
		}
		srcErr := in.audio.src.stop()
		if !errors.As(srcErr, &mbe) {
			t.Fatalf("stop err = %v", srcErr)
		}
	})
}

func TestGuardReader(t *testing.T) {
	t.Parallel()
	g := &guardReader{r: strings.NewReader("абв")}
	buf := make([]byte, 2)
	if n, err := g.Read(buf); n != 2 || err != nil {
		t.Fatalf("read: %d %v", n, err)
	}
	if err := g.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := g.Read(buf); !errors.Is(err, errAudioStopped) {
		t.Fatalf("after stop: %v", err)
	}
	if err := g.stop(); err != nil { // идемпотентно
		t.Fatal(err)
	}

	boom := errors.New("connection reset")
	g2 := &guardReader{r: io.MultiReader(strings.NewReader("x"), errReader{boom})}
	_, err := io.ReadAll(g2)
	if !errors.Is(err, boom) || !errors.Is(g2.stop(), boom) {
		t.Fatalf("source error must be remembered: %v", err)
	}

	// EOF — не ошибка источника
	g3 := &guardReader{r: strings.NewReader("")}
	_, _ = io.ReadAll(g3)
	if err := g3.stop(); err != nil {
		t.Fatalf("EOF: %v", err)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// ---------------------------------------------------------------- один ход в полёте

func TestRunFlight(t *testing.T) {
	t.Parallel()

	t.Run("дубль того же хода ждёт первый и получает его ответ; другой ход — 409", func(t *testing.T) {
		t.Parallel()
		s := newUnitService()
		id := uuid.New()
		release := make(chan struct{})
		started := make(chan struct{})
		var runs atomic.Int32
		leader := func() (*public.DialogueTurnResponse, error) {
			runs.Add(1)
			close(started)
			<-release
			return &public.DialogueTurnResponse{TurnNo: 2, NextTurnNo: 3}, nil
		}
		var wg sync.WaitGroup
		results := make([]*public.DialogueTurnResponse, 2)
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[0], _ = s.runFlight(id, 2, leader)
		}()
		<-started

		// другой номер хода, пока идёт ход, — 409 сразу
		_, err := s.runFlight(id, 3, func() (*public.DialogueTurnResponse, error) {
			t.Error("must not run")
			return nil, nil
		})
		if he := httpx.AsError(err); he.Status != http.StatusConflict {
			t.Fatalf("other turn: %v", err)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			results[1], _ = s.runFlight(id, 2, func() (*public.DialogueTurnResponse, error) {
				runs.Add(1)
				return &public.DialogueTurnResponse{TurnNo: 2, NextTurnNo: 99}, nil
			})
		}()
		time.Sleep(20 * time.Millisecond) // дубль встаёт в singleflight
		close(release)
		wg.Wait()
		if results[0] == nil || results[0].NextTurnNo != 3 {
			t.Fatalf("leader: %+v", results[0])
		}
		// дубль либо склеился с лидером, либо (если опоздал) выполнился сам — но не оба сразу
		if results[1] == nil {
			t.Fatal("duplicate got nothing")
		}
		if results[1].NextTurnNo == 3 && runs.Load() != 1 {
			t.Fatalf("shared result, but %d runs", runs.Load())
		}
		s.mu.Lock()
		n := len(s.inflight)
		s.mu.Unlock()
		if n != 0 {
			t.Fatalf("inflight not cleaned: %d", n)
		}
	})

	t.Run("обрыв загрузки у лидера — дубль пробует со своим аудио", func(t *testing.T) {
		t.Parallel()
		s := newUnitService()
		id := uuid.New()
		started := make(chan struct{})
		release := make(chan struct{})
		var leaderErr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, leaderErr = s.runFlight(id, 1, func() (*public.DialogueTurnResponse, error) {
				close(started)
				<-release
				return nil, &leaderOnlyError{err: httpx.BadRequest("оборвалась")}
			})
		}()
		<-started
		dupDone := make(chan *public.DialogueTurnResponse)
		go func() {
			r, _ := s.runFlight(id, 1, func() (*public.DialogueTurnResponse, error) {
				return &public.DialogueTurnResponse{TurnNo: 1, NextTurnNo: 2}, nil
			})
			dupDone <- r
		}()
		time.Sleep(20 * time.Millisecond)
		close(release)
		<-done
		if he := httpx.AsError(leaderErr); he.Status != http.StatusBadRequest {
			t.Fatalf("leader must get unwrapped 400: %v", leaderErr)
		}
		if r := <-dupDone; r == nil || r.NextTurnNo != 2 {
			t.Fatalf("duplicate must retry with its own audio: %+v", r)
		}
	})

	t.Run("паника хода — 500, карта очищена", func(t *testing.T) {
		t.Parallel()
		s := newUnitService()
		id := uuid.New()
		_, err := s.runFlight(id, 1, func() (*public.DialogueTurnResponse, error) { panic("boom") })
		if he := httpx.AsError(err); he.Status != http.StatusInternalServerError {
			t.Fatalf("panic: %v", err)
		}
		r, err := s.runFlight(id, 1, func() (*public.DialogueTurnResponse, error) {
			return &public.DialogueTurnResponse{TurnNo: 1}, nil
		})
		if err != nil || r == nil {
			t.Fatalf("after panic: %v", err)
		}
	})

	t.Run("разные попытки не мешают друг другу", func(t *testing.T) {
		t.Parallel()
		s := newUnitService()
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r, err := s.runFlight(uuid.New(), 1, func() (*public.DialogueTurnResponse, error) {
					time.Sleep(5 * time.Millisecond)
					return &public.DialogueTurnResponse{TurnNo: i}, nil
				})
				if err != nil || r.TurnNo != i {
					t.Errorf("attempt %d: %v %+v", i, err, r)
				}
			}()
		}
		wg.Wait()
	})
}

// ---------------------------------------------------------------- /media/tts

func TestMedia(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "dialog", "a1"), 0o755))
	must(os.WriteFile(filepath.Join(root, "dialog", "a1", "3.wav"), []byte("RIFF....WAVEdata0123456789"), 0o644))
	must(os.WriteFile(filepath.Join(root, "ab.ogg"), []byte("OggS"), 0o644))
	must(os.WriteFile(filepath.Join(root, "notes.txt"), []byte("secret"), 0o644))
	outside := t.TempDir()
	must(os.WriteFile(filepath.Join(outside, "leak.wav"), []byte("RIFFleak"), 0o644))
	symlinkOK := os.Symlink(filepath.Join(outside, "leak.wav"), filepath.Join(root, "link.wav")) == nil
	must(os.MkdirAll(filepath.Join(root, "dir.wav"), 0o755))

	s := New(Deps{Config: &config.Config{TTSDir: root}, Log: discardLog()})
	mux := http.NewServeMux()
	r := httpx.NewRouter(mux, "/api/v1", stubAuth{}, discardLog(), nil)
	s.Register(r)
	get := func(path string, role string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/media/tts/"+path, nil)
		if role != "" {
			req.Header.Set("X-Test-Role", role)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	rec := get("dialog/a1/3.wav", "student", nil)
	expectStatus(t, rec, http.StatusOK)
	if rec.Header().Get("Content-Type") != "audio/wav" || rec.Header().Get("Cache-Control") != "private, max-age=86400" ||
		rec.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(rec.Body.String(), "RIFF") {
		t.Fatalf("headers %v body %q", rec.Header(), rec.Body.String())
	}
	if rec := get("ab.ogg", "teacher", nil); rec.Code != 200 || rec.Header().Get("Content-Type") != "audio/ogg" {
		t.Fatalf("ogg: %d %v", rec.Code, rec.Header())
	}
	// перемотка в <audio>
	rec = get("dialog/a1/3.wav", "admin", map[string]string{"Range": "bytes=0-3"})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "RIFF" {
		t.Fatalf("range: %d %q", rec.Code, rec.Body.String())
	}

	expectError(t, get("dialog/a1/3.wav", "", nil), http.StatusUnauthorized, httpx.CodeUnauthorized)
	for _, p := range []string{"notes.txt", "missing.wav", "dir.wav", "..%2Fleak.wav", "dialog%2F..%2Fab.ogg", "dialog%2F%2Fa1%2F3.wav", "dialog/a1/3.WAV.txt"} {
		if rec := get(p, "student", nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d", p, rec.Code)
		}
	}
	// неканонический путь ServeMux перенаправляет на очищенный (до хендлера) — файл не отдаётся
	for _, p := range []string{"dialog/../ab.ogg", "dialog//a1/3.wav"} {
		if rec := get(p, "student", nil); rec.Code == http.StatusOK {
			t.Errorf("%s: served", p)
		}
	}
	if symlinkOK {
		if rec := get("link.wav", "student", nil); rec.Code != http.StatusNotFound {
			t.Errorf("symlink outside root must be 404, got %d", rec.Code)
		}
	}

	// без каталога озвучки — 404, не паника
	s2 := New(Deps{Log: discardLog()})
	mux2 := http.NewServeMux()
	s2.Register(httpx.NewRouter(mux2, "/api/v1", stubAuth{}, discardLog(), nil))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/media/tts/ab.ogg", nil)
	req.Header.Set("X-Test-Role", "student")
	rec = httptest.NewRecorder()
	mux2.ServeHTTP(rec, req)
	expectError(t, rec, http.StatusNotFound, httpx.CodeNotFound)
}
