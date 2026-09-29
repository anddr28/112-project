package materials

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"lct/gocore/internal/platform/httpx"
)

// upload — проверенная форма создания материала.
type upload struct {
	title, description, category, content string
	file                                  *uploadFile
}

type uploadFile struct {
	name, mime string
	data       []byte
}

// maxFormOverhead — текстовые поля и заголовки частей поверх файла (content ≤ 200 000
// символов кириллицы ≈ 400 КБ UTF-8).
const maxFormOverhead = 1 << 20

func runes(s string) int { return utf8.RuneCountInString(s) }

func errTooLarge() *httpx.Error {
	return &httpx.Error{Status: http.StatusRequestEntityTooLarge, Code: httpx.CodeValidation,
		Message: "Файл больше 20 МБ — уменьшите его или разделите на части"}
}

// readUpload — разбор multipart потоком (без временных файлов ParseMultipartForm): текстовые
// поля с лимитами, файл — в память не больше 20 МБ + 1 байт (признак превышения).
func readUpload(w http.ResponseWriter, r *http.Request) (*upload, error) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" {
		return nil, &httpx.Error{Status: http.StatusUnsupportedMediaType, Code: httpx.CodeValidation,
			Message: "Ожидается multipart/form-data (поля title, description, category, content, file)"}
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxFileBytes+maxFormOverhead)
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, httpx.BadRequest("Некорректное тело multipart")
	}
	in := &upload{}
	seen := map[string]bool{}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if isTooLarge(err) {
				return nil, errTooLarge()
			}
			return nil, httpx.BadRequest("Некорректное тело multipart")
		}
		name := part.FormName()
		if seen[name] {
			_ = part.Close()
			return nil, httpx.Validation("Поле формы передано дважды", map[string]string{name: "Передайте один раз"})
		}
		seen[name] = true
		switch name {
		case "title", "description", "category", "content":
			v, err := readField(part, name)
			_ = part.Close()
			if err != nil {
				return nil, err
			}
			switch name {
			case "title":
				in.title = v
			case "description":
				in.description = v
			case "category":
				in.category = v
			case "content":
				in.content = v
			}
		case "file":
			f, err := readFile(part)
			_ = part.Close()
			if err != nil {
				return nil, err
			}
			in.file = f
		default:
			// Лишние поля не ломают загрузку (фронт может прислать больше), но и не читаются в память.
			if _, err := io.Copy(io.Discard, part); err != nil && isTooLarge(err) {
				return nil, errTooLarge()
			}
			_ = part.Close()
		}
	}
	if fields := in.validate(); len(fields) > 0 {
		return nil, httpx.Validation("Проверьте поля материала", fields)
	}
	return in, nil
}

// fieldLimits — байтовые потолки чтения текстовых полей (руны проверяет validate).
var fieldLimits = map[string]int64{
	"title":       maxTitleRunes * 4,
	"description": maxDescRunes * 4,
	"category":    maxCategoryRunes * 4,
	"content":     maxContentRunes * 4,
}

func readField(part io.Reader, name string) (string, error) {
	limit := fieldLimits[name]
	b, err := io.ReadAll(io.LimitReader(part, limit+1))
	if err != nil {
		if isTooLarge(err) {
			return "", errTooLarge()
		}
		return "", httpx.BadRequest("Не удалось прочитать поле " + name)
	}
	if int64(len(b)) > limit {
		return "", httpx.Validation("Слишком длинное поле", map[string]string{name: "Превышена допустимая длина"})
	}
	if !utf8.Valid(b) {
		return "", httpx.Validation("Поле не в кодировке UTF-8", map[string]string{name: "Ожидается текст UTF-8"})
	}
	s := strings.ReplaceAll(string(b), "\x00", "")
	if name == "content" {
		return strings.TrimRight(s, " \t\r\n"), nil
	}
	return strings.TrimSpace(s), nil
}

func readFile(part interface {
	io.Reader
	FileName() string
}) (*uploadFile, error) {
	name := cleanFileName(part.FileName())
	data, err := io.ReadAll(io.LimitReader(part, MaxFileBytes+1))
	if err != nil {
		if isTooLarge(err) {
			return nil, errTooLarge()
		}
		return nil, httpx.BadRequest("Не удалось прочитать файл")
	}
	if len(data) > MaxFileBytes {
		return nil, errTooLarge()
	}
	if name == "" {
		return nil, httpx.Validation("У файла нет имени", map[string]string{"file": "Укажите имя файла"})
	}
	if len(data) == 0 {
		return nil, httpx.Validation("Файл пустой", map[string]string{"file": "Пустой файл"})
	}
	ft, ok := detectType(name, data)
	if !ok {
		return nil, httpx.Validation("Тип файла не поддерживается или содержимое не совпадает с расширением. Допустимо: "+allowedList,
			map[string]string{"file": "Допустимо: " + allowedList})
	}
	return &uploadFile{name: name, mime: ft.mime, data: data}, nil
}

func (in *upload) validate() map[string]string {
	fields := map[string]string{}
	switch n := runes(in.title); {
	case n < minTitleRunes:
		fields["title"] = "Название — не короче 3 символов"
	case n > maxTitleRunes:
		fields["title"] = "Название — не длиннее 200 символов"
	}
	if runes(in.description) > maxDescRunes {
		fields["description"] = "Описание — не длиннее 2000 символов"
	}
	if runes(in.category) > maxCategoryRunes {
		fields["category"] = "Раздел — не длиннее 100 символов"
	}
	if in.category == "" {
		in.category = defaultCategory
	}
	if runes(in.content) > maxContentRunes {
		fields["content"] = "Текст — не длиннее 200 000 символов"
	}
	if strings.TrimSpace(in.content) == "" && in.file == nil {
		fields["content"] = "Добавьте текст материала или файл"
	}
	return fields
}
