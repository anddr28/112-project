package convert

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- нумерация реплик

// SeqToExchange — сквозной номер реплики транскрипта ai-service (1..n) -> номер обмена
// фронта (DESIGN §5 «Разговор»): 1 -> 0 (вступление), чётный s -> s/2 (оператор k),
// нечётный s -> (s−1)/2 (ответ заявителя на k-ю реплику). s < 1 -> 0.
func SeqToExchange(seq int) int {
	if seq <= 1 {
		return 0
	}
	if seq%2 == 0 {
		return seq / 2
	}
	return (seq - 1) / 2
}

// ExchangeToSeq — номер обмена + говорящий -> сквозной номер для ai-service:
// заявитель k -> 2k+1 (вступление 0 -> 1), оператор k -> 2k.
func ExchangeToSeq(exchange int, speaker string) int {
	if exchange < 0 {
		exchange = 0
	}
	if speaker == "operator" {
		return 2 * exchange
	}
	return 2*exchange + 1
}

// ---------------------------------------------------------------- медиа

// MediaPrefix — публичный префикс озвучки (GET /api/v1/media/tts/{path}).
const MediaPrefix = "/api/v1/media/tts/"

// MediaURL — URL файла озвучки для <audio>: каждый сегмент относительного пути
// экранируется (url.PathEscape), пустые сегменты и ведущие "/" отбрасываются.
// "" для пустого пути.
func MediaURL(p string) string {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(MediaPrefix) + len(p) + 8)
	b.WriteString(MediaPrefix)
	first := true
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" {
			continue
		}
		if !first {
			b.WriteByte('/')
		}
		first = false
		b.WriteString(url.PathEscape(seg))
	}
	return b.String()
}

// AudioMime — MIME по расширению файла озвучки ("" — неизвестно; контракт: default audio/wav).
func AudioMime(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".wav":
		return "audio/wav"
	case ".ogg", ".opus", ".oga":
		return "audio/ogg"
	case ".mp3":
		return "audio/mpeg"
	case ".webm":
		return "audio/webm"
	}
	return ""
}

// TTSHash — КАНОНИЧЕСКИЙ ключ озвучки: hex(sha256(text + "|" + voice + "|" + rate)),
// rate — с двумя знаками ("1.00"). Считает go-core (approve, tts-preview, fallback);
// ai-service использует как ключ файла; tts_cache.text_hash — он же.
func TTSHash(text, voice string, rate float64) string {
	// Один буфер + Sum256 без интерфейса hash.Hash: одна аллокация на вызов.
	b := make([]byte, 0, len(text)+len(voice)+16)
	b = append(b, text...)
	b = append(b, '|')
	b = append(b, voice...)
	b = append(b, '|')
	b = strconv.AppendFloat(b, rate, 'f', 2, 64)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
