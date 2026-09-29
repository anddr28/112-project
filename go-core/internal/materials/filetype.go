package materials

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// fileType — разрешённый тип файла материала (контракт v1.4: PDF, DOCX, XLSX, MP3, WAV,
// PNG, JPG, TXT, XML, JSON). Тип определяется по расширению И проверяется по содержимому:
// расширение можно подделать, а отдавать с сервера «PDF», который на деле HTML со
// скриптом, нельзя. MIME в ответе — канонический для типа, а не присланный клиентом.
type fileType struct {
	mime   string
	inline bool // браузер показывает сам (PDF, аудио, изображения); остальное — скачивание
	sniff  func(b []byte) bool
}

var fileTypes = map[string]fileType{
	".pdf":  {"application/pdf", true, hasPrefix("%PDF-")},
	".docx": {"application/vnd.openxmlformats-officedocument.wordprocessingml.document", false, zipWith("word/document.xml")},
	".xlsx": {"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", false, zipWith("xl/workbook.xml")},
	".mp3":  {"audio/mpeg", true, isMP3},
	".wav":  {"audio/wav", true, isWAV},
	".png":  {"image/png", true, hasPrefix("\x89PNG\r\n\x1a\n")},
	".jpg":  {"image/jpeg", true, hasPrefix("\xff\xd8\xff")},
	".jpeg": {"image/jpeg", true, hasPrefix("\xff\xd8\xff")},
	".txt":  {"text/plain; charset=utf-8", false, isText},
	".xml":  {"application/xml", false, isXML},
	".json": {"application/json", false, func(b []byte) bool { return json.Valid(trimBOM(b)) }},
}

// allowedList — для сообщения об ошибке.
const allowedList = "PDF, DOCX, XLSX, MP3, WAV, PNG, JPG, TXT, XML, JSON"

// detectType — тип по имени файла и содержимому; ok=false — не разрешён или содержимое
// не соответствует расширению.
func detectType(name string, data []byte) (fileType, bool) {
	ft, ok := fileTypes[strings.ToLower(filepath.Ext(name))]
	if !ok || len(data) == 0 || !ft.sniff(data) {
		return fileType{}, false
	}
	return ft, true
}

// inlineMIME — отдавать ли файл с этим MIME для просмотра в браузере (Content-Disposition).
func inlineMIME(mime string) bool {
	return mime == "application/pdf" || strings.HasPrefix(mime, "audio/") || mime == "image/png" || mime == "image/jpeg"
}

func hasPrefix(p string) func([]byte) bool {
	return func(b []byte) bool { return bytes.HasPrefix(b, []byte(p)) }
}

// zipWith — OOXML-документ: zip-архив с обязательной частью (word/… или xl/…). Читается
// только центральный каталог архива, распаковки нет.
func zipWith(part string) func([]byte) bool {
	return func(b []byte) bool {
		if !bytes.HasPrefix(b, []byte("PK\x03\x04")) {
			return false
		}
		zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			return false
		}
		for _, f := range zr.File {
			if f.Name == part {
				return true
			}
		}
		return false
	}
}

// isMP3 — тег ID3 или сразу MPEG-кадр (11 бит синхронизации).
func isMP3(b []byte) bool {
	if bytes.HasPrefix(b, []byte("ID3")) {
		return true
	}
	return len(b) >= 2 && b[0] == 0xFF && b[1]&0xE0 == 0xE0
}

func isWAV(b []byte) bool {
	return len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WAVE"
}

func trimBOM(b []byte) []byte { return bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")) }

// isText — UTF-8 без NUL и управляющих символов (кроме переводов строк и табуляции).
func isText(b []byte) bool {
	b = trimBOM(b)
	if !utf8.Valid(b) {
		return false
	}
	for _, r := range string(b) {
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func isXML(b []byte) bool {
	t := bytes.TrimLeft(trimBOM(b), " \t\r\n")
	return len(t) > 0 && t[0] == '<' && isText(t)
}

// cleanFileName — имя файла без путей и управляющих символов, не длиннее 200 символов
// (с сохранением расширения).
func cleanFileName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '"' {
			return -1
		}
		return r
	}, strings.TrimSpace(name))
	if r := []rune(name); len(r) > maxFileNameRunes {
		ext := []rune(filepath.Ext(name))
		name = string(r[:maxFileNameRunes-len(ext)]) + string(ext)
	}
	return name
}
