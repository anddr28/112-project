package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
)

const (
	maxAudioBytes  = 2 << 20 // STT_MAX_BYTES имитатора: больше — 413
	minSpeechBytes = 200     // меньше — «тишина» (no_speech)
	maxJSONPart    = 1 << 20 // часть request/options в multipart
)

// audioInfo — что имитатору нужно знать об аудио: размер и длительность. Само аудио
// не хранится и даже не держится в памяти целиком (контракт: аудио оператора не
// сохраняется) — читаем заголовок и считаем байты.
type audioInfo struct {
	size       int64
	durationMs int
	format     string // webm|ogg|wav|mpeg
}

// audioFormat — формат по Content-Type части. octet-stream и пустой тип — по
// сигнатуре (лояльно: браузеры и прокси не всегда ставят тип).
func audioFormat(contentType string) (string, bool) {
	if contentType == "" {
		return "", true
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", false
	}
	switch mt {
	case "application/octet-stream":
		return "", true
	case "audio/webm", "video/webm":
		return "webm", true
	case "audio/ogg", "application/ogg", "audio/opus":
		return "ogg", true
	case "audio/wav", "audio/x-wav", "audio/wave", "audio/vnd.wave":
		return "wav", true
	case "audio/mpeg", "audio/mp3":
		return "mpeg", true
	}
	return "", false
}

func sniffFormat(head []byte) string {
	switch {
	case len(head) >= 12 && string(head[0:4]) == "RIFF" && string(head[8:12]) == "WAVE":
		return "wav"
	case len(head) >= 4 && string(head[0:4]) == "OggS":
		return "ogg"
	case len(head) >= 3 && (string(head[0:3]) == "ID3" || head[0] == 0xFF && head[1]&0xE0 == 0xE0):
		return "mpeg"
	default:
		return "webm"
	}
}

// readAudioPart — проверка типа (415), размера (413) и оценка длительности.
func readAudioPart(p *multipart.Part) (audioInfo, error) {
	format, ok := audioFormat(p.Header.Get("Content-Type"))
	if !ok {
		return audioInfo{}, &apiError{status: http.StatusUnsupportedMediaType, code: "unsupported_media",
			msg: "Формат аудио не поддерживается: ожидается webm/opus, ogg/opus, wav или mpeg"}
	}
	var head [64]byte
	n, err := io.ReadFull(p, head[:])
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return audioInfo{}, readErr(err)
	}
	rest, err := io.Copy(io.Discard, io.LimitReader(p, maxAudioBytes+1-int64(n)))
	if err != nil {
		return audioInfo{}, readErr(err)
	}
	size := int64(n) + rest
	if size > maxAudioBytes {
		return audioInfo{}, &apiError{status: http.StatusRequestEntityTooLarge, code: "payload_too_large",
			msg: "Аудио больше 2 МБ"}
	}
	if format == "" {
		format = sniffFormat(head[:n])
	}
	return audioInfo{size: size, format: format, durationMs: estimateDuration(format, head[:n], size)}, nil
}

// estimateDuration — WAV точно по заголовку; сжатые форматы — по среднему битрейту
// (opus ~64 кбит/с, mp3 ~128 кбит/с). Нужна только для метрик и транскрипта.
func estimateDuration(format string, head []byte, size int64) int {
	var ms int64
	switch format {
	case "wav":
		if len(head) >= 32 && bytes.Equal(head[0:4], []byte("RIFF")) {
			if br := int64(binary.LittleEndian.Uint32(head[28:32])); br > 0 {
				ms = (size - wavHeaderSize) * 1000 / br
			}
		}
		if ms == 0 {
			ms = size * 1000 / (wavRate * 2)
		}
	case "mpeg":
		ms = size / 16
	default:
		ms = size / 8
	}
	return int(min(max(ms, 300), 60_000))
}

// readMultipart обходит части формы: JSON-части отдаёт onJSON, аудио — readAudioPart.
// Порядок частей любой (клиенты кладут аудио и первым, и последним).
func readMultipart(r *http.Request, jsonParts map[string]*[]byte) (*audioInfo, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, badPayload("Ожидается multipart/form-data: %v", err)
	}
	var audio *audioInfo
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, readErr(err)
		}
		name := p.FormName()
		switch {
		case name == "audio":
			a, err := readAudioPart(p)
			if err != nil {
				return nil, err
			}
			audio = &a
		case jsonParts[name] != nil:
			b, err := io.ReadAll(io.LimitReader(p, maxJSONPart+1))
			if err != nil {
				return nil, readErr(err)
			}
			if len(b) > maxJSONPart {
				return nil, badPayload("Часть %q больше 1 МБ", name)
			}
			*jsonParts[name] = b
		default:
			// Лишние части не ломают запрос (совместимость), но и не читаются в память.
			if _, err := io.Copy(io.Discard, p); err != nil {
				return nil, readErr(err)
			}
		}
		_ = p.Close()
	}
	return audio, nil
}

// readErr — ошибка чтения тела: превышение MaxBytesReader — 413, остальное — 400.
func readErr(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return &apiError{status: http.StatusRequestEntityTooLarge, code: "payload_too_large", msg: "Тело запроса слишком большое"}
	}
	return badPayload("Не удалось прочитать тело запроса: %v", err)
}

func isMultipart(r *http.Request) bool {
	return strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data")
}
