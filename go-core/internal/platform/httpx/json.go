package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Лимиты тела запроса.
const (
	MaxJSONBody  = 1 << 20 // 1 МБ — карточка, сценарий, батч событий (200 шт.)
	MaxAudioBody = 3 << 20 // 3 МБ multipart (аудио ≤ 2 МБ по контракту + поля)
)

var bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// WriteJSON сериализует v в пуловый буфер и пишет одним Write (Content-Length известен —
// без chunked). HTML не экранируется: в ответах русский текст и "<>" из карточек.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		if buf.Cap() <= 1<<20 { // гигантские буферы не держим в пуле
			bufPool.Put(buf)
		}
	}()
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		// ApiError с тем же Content-Type, что у остальных ответов (http.Error ставит text/plain)
		WriteRawJSON(w, http.StatusInternalServerError, []byte(`{"code":"internal","message":"Ошибка сериализации ответа"}`+"\n"))
		return
	}
	WriteRawJSON(w, status, buf.Bytes())
}

// WriteRawJSON — готовые JSON-байты (кэш справочников, jsonb из БД как есть).
func WriteRawJSON(w http.ResponseWriter, status int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// NoContent — 204.
func NoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// ReadJSON читает тело (лимит MaxJSONBody) в dst. Ошибки — 400 validation по-русски.
// Неизвестные поля допускаются (фронт может слать больше, чем нужно конкретной ручке).
func ReadJSON(r *http.Request, dst any) error {
	return ReadJSONLimit(r, dst, MaxJSONBody)
}

func ReadJSONLimit(r *http.Request, dst any, limit int64) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || (mt != "application/json" && !strings.HasSuffix(mt, "+json")) {
			return &Error{Status: http.StatusUnsupportedMediaType, Code: CodeValidation, Message: "Ожидается тело application/json"}
		}
	}
	body := http.MaxBytesReader(nil, r.Body, limit)
	dec := json.NewDecoder(body)
	if err := dec.Decode(dst); err != nil {
		var mbe *http.MaxBytesError
		switch {
		case errors.As(err, &mbe):
			return &Error{Status: http.StatusRequestEntityTooLarge, Code: CodeValidation, Message: "Слишком большой запрос"}
		case errors.Is(err, io.EOF):
			return BadRequest("Пустое тело запроса")
		default:
			return BadRequest("Некорректный JSON: " + err.Error())
		}
	}
	return nil
}

// ReadRawJSON — тело как есть (проверяется только, что это валидный JSON-объект).
// Для горячего пути (черновик карточки): без decode/encode в Go-структуры.
func ReadRawJSON(r *http.Request, limit int64) (json.RawMessage, error) {
	body := http.MaxBytesReader(nil, r.Body, limit)
	buf, err := io.ReadAll(body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, &Error{Status: http.StatusRequestEntityTooLarge, Code: CodeValidation, Message: "Слишком большой запрос"}
		}
		return nil, BadRequest("Не удалось прочитать тело запроса")
	}
	trimmed := bytes.TrimSpace(buf)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil, BadRequest("Ожидается JSON-объект")
	}
	return trimmed, nil
}

// ---------------------------------------------------------------- параметры

// PathUUID — UUID из параметра пути ({attemptId}); 404, если не UUID (как «не найдено»).
func PathUUID(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		return uuid.Nil, NotFound("")
	}
	return id, nil
}

// QueryInt — целое из query с дефолтом и границами.
func QueryInt(r *http.Request, name string, def, min, max int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return def
	}
	if v < min {
		return min
	}
	if max > 0 && v > max {
		return max
	}
	return v
}

// QueryUUID — необязательный UUID из query (ok=false — нет/мусор).
func QueryUUID(r *http.Request, name string) (uuid.UUID, bool) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(s)
	return id, err == nil
}

// QueryTime — необязательное время RFC3339 из query.
func QueryTime(r *http.Request, name string) (time.Time, bool) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

// ClientIP — адрес клиента (X-Forwarded-For учитывается только от reverse-proxy контура;
// go-core обычно стоит на входе сам, поэтому по умолчанию — RemoteAddr).
func ClientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}
