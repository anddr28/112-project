package dialogue

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"lct/gocore/internal/platform/httpx"
)

// Лимиты входа хода (frontend.v1.yaml: audio ≤ 2 МБ / 60 с, text ≤ 1000 символов).
const (
	maxAudioBytes = 2 << 20
	maxTextRunes  = 1000
	maxTextBytes  = maxTextRunes * 4 // UTF-8: не больше 4 байт на символ
	maxFieldBytes = 64               // turnNo, clientRecordedAt
	maxSkipParts  = 8                // сколько «хвостовых» частей multipart смотрим после аудио
)

// turnInput — разобранная реплика оператора. Аудио НЕ буферизуется: часть multipart
// потоком уходит в ai-service (core.AudioInput.Reader), тело читается по мере отправки.
type turnInput struct {
	turnNo     int
	text       string     // текстовая реплика (JSON или multipart-поле text без аудио)
	recordedAt *time.Time // clientRecordedAt (только multipart)
	audio      *audioIn
	mr         *multipart.Reader // остаток формы (clientRecordedAt после аудио) — дочитывается после хода
}

func (in *turnInput) isAudio() bool { return in.audio != nil }

// audioIn — аудио реплики: нормализованный тип + «охраняемый» поток части формы.
type audioIn struct {
	contentType string // audio/webm | audio/ogg | audio/wav
	filename    string
	src         *guardReader
}

// close — больше из тела запроса не читаем (идемпотентно).
func (in *turnInput) close() {
	if in.audio != nil {
		in.audio.src.stop()
	}
}

// readTurnInput — JSON {turnNo, text} или multipart {turnNo, audio, clientRecordedAt}.
// Для multipart читаются поля до части audio включительно (заголовок и сигнатура);
// само аудио остаётся в теле запроса до вызова ai-service.
func readTurnInput(w http.ResponseWriter, r *http.Request) (*turnInput, error) {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt == "multipart/form-data" {
		return readMultipartInput(w, r)
	}
	return readJSONInput(r)
}

func readJSONInput(r *http.Request) (*turnInput, error) {
	var body struct {
		TurnNo *int    `json:"turnNo"`
		Text   *string `json:"text"`
	}
	if err := httpx.ReadJSONLimit(r, &body, 16<<10); err != nil {
		return nil, err
	}
	if body.TurnNo == nil {
		return nil, httpx.Validation("Не указан номер реплики", map[string]string{"turnNo": "обязательно"})
	}
	in := &turnInput{turnNo: *body.TurnNo}
	if in.turnNo < 1 {
		return nil, httpx.Validation("Номер реплики должен быть не меньше 1", map[string]string{"turnNo": "≥ 1"})
	}
	text, err := checkText(body.Text)
	if err != nil {
		return nil, err
	}
	in.text = text
	return in, nil
}

// checkText — текст реплики: не пустой, ≤ 1000 символов, без NUL (text в PG его не примет).
func checkText(p *string) (string, error) {
	if p == nil {
		return "", httpx.Validation("Пустая реплика", map[string]string{"text": "обязательно"})
	}
	t := cleanText(*p)
	if t == "" {
		return "", httpx.Validation("Пустая реплика", map[string]string{"text": "обязательно"})
	}
	if utf8.RuneCountInString(t) > maxTextRunes {
		return "", httpx.Validation("Реплика длиннее 1000 символов", map[string]string{"text": "не больше 1000 символов"})
	}
	return t, nil
}

// cleanText — валидный UTF-8 без NUL, обрезанный по краям.
func cleanText(s string) string {
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	return strings.TrimSpace(s)
}

func readMultipartInput(w http.ResponseWriter, r *http.Request) (*turnInput, error) {
	// Лимит всего тела (аудио ≤ 2 МБ + поля); сработавший лимит закрывает соединение.
	r.Body = http.MaxBytesReader(w, r.Body, httpx.MaxAudioBody)
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, httpx.BadRequest("Ожидается multipart/form-data с полями turnNo и audio")
	}
	in := &turnInput{mr: mr}
	var textPart *string
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			in.mr = nil // форма прочитана целиком
			break
		}
		if err != nil {
			return nil, bodyError(err)
		}
		switch part.FormName() {
		case "turnNo":
			v, err := readField(part, maxFieldBytes)
			if err != nil {
				return nil, err
			}
			n, perr := strconv.Atoi(strings.TrimSpace(v))
			if perr != nil || n < 1 {
				return nil, httpx.Validation("Номер реплики должен быть целым числом не меньше 1", map[string]string{"turnNo": "≥ 1"})
			}
			in.turnNo = n
		case "clientRecordedAt":
			v, err := readField(part, maxFieldBytes)
			if err != nil {
				return nil, err
			}
			if t, perr := time.Parse(time.RFC3339Nano, strings.TrimSpace(v)); perr == nil {
				in.recordedAt = &t
			}
		case "text":
			v, err := readField(part, maxTextBytes+16)
			if err != nil {
				return nil, err
			}
			textPart = &v
		case "audio":
			if in.audio != nil {
				continue // вторая запись — лишняя, NextPart её пропустит
			}
			a, err := openAudio(part)
			if err != nil {
				return nil, err
			}
			in.audio = a
			if in.turnNo > 0 {
				// Номер уже известен (фронт шлёт turnNo первым) — аудио пойдёт потоком
				// прямо в ai-service; clientRecordedAt дочитаем после хода.
				return in, nil
			}
			// turnNo идёт после аудио (чужой клиент) — придётся буферизовать (≤ 2 МБ).
			if err := a.buffer(); err != nil {
				return nil, err
			}
		}
	}
	if in.turnNo < 1 {
		return nil, httpx.Validation("Не указан номер реплики", map[string]string{"turnNo": "обязательно"})
	}
	if in.audio == nil {
		if textPart == nil {
			return nil, httpx.Validation("Нужна запись реплики (audio) или текст", map[string]string{"audio": "обязательно"})
		}
		t, err := checkText(textPart)
		if err != nil {
			return nil, err
		}
		in.text = t
	}
	return in, nil
}

// readTrailing дочитывает форму после хода: clientRecordedAt у фронта идёт после аудио.
// Ошибки не фатальны — время реплики тогда берётся по приходу запроса.
func (in *turnInput) readTrailing() {
	if in.mr == nil || in.recordedAt != nil {
		return
	}
	defer func() { in.mr = nil }()
	for i := 0; i < maxSkipParts; i++ {
		part, err := in.mr.NextPart()
		if err != nil {
			return
		}
		if part.FormName() != "clientRecordedAt" {
			continue
		}
		v, err := readField(part, maxFieldBytes)
		if err != nil {
			return
		}
		if t, perr := time.Parse(time.RFC3339Nano, strings.TrimSpace(v)); perr == nil {
			in.recordedAt = &t
		}
		return
	}
}

// readField — короткое текстовое поле формы.
func readField(p *multipart.Part, limit int64) (string, error) {
	b, err := io.ReadAll(io.LimitReader(p, limit+1))
	if err != nil {
		return "", bodyError(err)
	}
	if int64(len(b)) > limit {
		return "", httpx.Validation("Поле формы слишком длинное", map[string]string{p.FormName(): "слишком длинное"})
	}
	return string(b), nil
}

// bodyError — ошибка чтения тела: сработал лимит — 413, иначе запрос битый/оборван — 400.
func bodyError(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return httpx.AudioTooLong()
	}
	return httpx.BadRequest("Не удалось прочитать запрос — повторите реплику")
}

// openAudio — тип аудио по заголовку части (webm/ogg/wav; "audio/webm;codecs=opus" —
// тоже webm), octet-stream/пусто — по сигнатуре. Пустая запись — 400, чужой формат — 415.
func openAudio(p *multipart.Part) (*audioIn, error) {
	br := bufio.NewReaderSize(p, 64) // только для Peek сигнатуры: крупные чтения идут мимо буфера
	head, err := br.Peek(12)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return nil, bodyError(err)
	}
	if len(head) == 0 {
		return nil, httpx.Validation("Запись реплики пустая — повторите", map[string]string{"audio": "пустая запись"})
	}
	ct := audioContentType(p.Header.Get("Content-Type"), head)
	if ct == "" {
		return nil, httpx.AudioUnsupported()
	}
	a := &audioIn{contentType: ct, filename: audioFilename(ct)}
	// Лимит 2 МБ именно на аудио: http.MaxBytesError AIClient переводит в AIAudioTooLong.
	a.src = &guardReader{r: http.MaxBytesReader(nil, io.NopCloser(br), maxAudioBytes)}
	return a, nil
}

// buffer — прочитать аудио в память (редкий путь: turnNo после аудио).
func (a *audioIn) buffer() error {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(a.src.r); err != nil {
		return bodyError(err)
	}
	a.src = &guardReader{r: bytes.NewReader(buf.Bytes())}
	return nil
}

// audioContentType — нормализованный тип для ai-service ("" — не поддерживается).
func audioContentType(header string, head []byte) string {
	mt := ""
	if header != "" {
		v, _, err := mime.ParseMediaType(header)
		if err != nil {
			return ""
		}
		mt = strings.ToLower(v)
	}
	switch mt {
	case "audio/webm", "video/webm":
		return "audio/webm"
	case "audio/ogg", "application/ogg", "audio/opus":
		return "audio/ogg"
	case "audio/wav", "audio/x-wav", "audio/wave", "audio/vnd.wave":
		return "audio/wav"
	case "", "application/octet-stream":
		return sniffAudio(head)
	}
	return ""
}

// sniffAudio — по сигнатуре: RIFF…WAVE, OggS, EBML (webm/matroska).
func sniffAudio(head []byte) string {
	switch {
	case len(head) >= 12 && string(head[0:4]) == "RIFF" && string(head[8:12]) == "WAVE":
		return "audio/wav"
	case len(head) >= 4 && string(head[0:4]) == "OggS":
		return "audio/ogg"
	case len(head) >= 4 && head[0] == 0x1A && head[1] == 0x45 && head[2] == 0xDF && head[3] == 0xA3:
		return "audio/webm"
	}
	return ""
}

// audioFilename — имя файла для ai-service: своё, по типу (имя от клиента не пересылаем).
func audioFilename(ct string) string {
	switch ct {
	case "audio/ogg":
		return "turn.ogg"
	case "audio/wav":
		return "turn.wav"
	}
	return "turn.webm"
}

// guardReader — поток аудио, который AIClient читает в своей горутине (io.Pipe в multipart).
// stop() дожидается текущего Read и запрещает следующие: после хода хендлер безопасно
// дочитывает форму (multipart.Reader не потокобезопасен), а горутина клиента не трогает
// тело запроса после возврата хендлера. err — первая ошибка источника (обрыв загрузки
// у студента, лимит), чтобы не спутать её с отказом ai-service.
type guardReader struct {
	mu      sync.Mutex
	r       io.Reader
	stopped bool
	err     error
}

var errAudioStopped = errors.New("dialogue: audio stream closed")

func (g *guardReader) Read(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped {
		return 0, errAudioStopped
	}
	n, err := g.r.Read(p)
	if err != nil && err != io.EOF && g.err == nil {
		g.err = err
	}
	return n, err
}

// stop — больше не читать; возвращает ошибку источника, если она была.
func (g *guardReader) stop() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopped = true
	return g.err
}
