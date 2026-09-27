package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"unicode/utf8"
)

// Параметры «озвучки»: 16 кГц моно 16 бит — как у Silero, чтобы плеер фронта и
// раздача /media/tts вели себя так же, как с настоящими файлами.
const (
	wavRate       = 16000
	wavHeaderSize = 44
	ttsMinMs      = 700
	ttsMsPerChar  = 55
	ttsMaxMs      = 120_000
	defaultVoice  = "baya"
	noSpeechPath  = "common/allo_slyshite.wav"
	noSpeechText  = "Алло? Вы меня слышите?"
)

// ttsStore пишет WAV-файлы в shared volume TTS_DIR. Пути в результатах —
// относительные, со слешами (go-core отдаёт их как /api/v1/media/tts/{file_path}).
type ttsStore struct{ root string }

// ttsDuration — длительность «речи» по длине текста: ≈55 мс на символ, не короче 0.7 с.
func ttsDuration(text string) int {
	return min(max(ttsMinMs, ttsMsPerChar*utf8.RuneCountInString(text)), ttsMaxMs)
}

// ttsHash — sha256(text|voice|rate), формат ключа tts_cache (для реплик диалога,
// у которых ключа от go-core нет).
func ttsHash(text, voice string, rate float32) string {
	sum := sha256.Sum256([]byte(text + "|" + voice + "|" + strconv.FormatFloat(float64(rate), 'f', 2, 32)))
	return hex.EncodeToString(sum[:])
}

// validHash — text_hash попадает в путь файла: только [0-9A-Za-z_-], 8..128 символов,
// иначе это попытка выйти из каталога.
func validHash(h string) bool {
	if len(h) < 8 || len(h) > 128 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// hashPath — tts/<hash[0:2]>/<hash>.wav: веер каталогов, чтобы не держать
// десятки тысяч файлов в одном.
func hashPath(hash string) string {
	return path.Join("tts", hash[:2], hash+".wav")
}

// write создаёт файл rel с «речью» для text и возвращает её длительность.
// keep=true — файл адресуется содержимым (хэш): если он уже есть, не переписываем.
// Запись атомарная (temp + rename): go-core может раздавать файл в ту же секунду.
func (s *ttsStore) write(rel, text string, keep bool) (int, error) {
	dur := ttsDuration(text)
	full := filepath.Join(s.root, filepath.FromSlash(rel))
	if keep {
		if st, err := os.Stat(full); err == nil && st.Size() > wavHeaderSize {
			return dur, nil
		}
	}
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	f, err := os.CreateTemp(dir, ".wav-*")
	if err != nil {
		return 0, err
	}
	tmp := f.Name()
	werr := writeWAV(f, text, dur)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("запись wav: %w", err)
	}
	// CreateTemp создаёт 0600 — файл читает другой процесс (go-core), возможно под другим uid.
	if err := os.Chmod(tmp, 0o644); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	if err := os.Rename(tmp, full); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	return dur, nil
}

// writeWAV — PCM 16 кГц моно 16 бит: тихое «бормотание» слогами (тон 170–230 Гц
// с мягкой огибающей) и пауза в конце. Потоково, через буфер 32 КБ — без аллокации
// всего сигнала.
func writeWAV(w io.Writer, text string, durMs int) error {
	n := wavRate * durMs / 1000
	dataSize := n * 2
	bw := bufio.NewWriterSize(w, 32<<10)

	var h [wavHeaderSize]byte
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+dataSize))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)        // размер fmt-чанка
	binary.LittleEndian.PutUint16(h[20:], 1)         // PCM
	binary.LittleEndian.PutUint16(h[22:], 1)         // моно
	binary.LittleEndian.PutUint32(h[24:], wavRate)   // частота
	binary.LittleEndian.PutUint32(h[28:], wavRate*2) // байт в секунду
	binary.LittleEndian.PutUint16(h[32:], 2)         // выравнивание блока
	binary.LittleEndian.PutUint16(h[34:], 16)        // бит на отсчёт
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(dataSize))
	if _, err := bw.Write(h[:]); err != nil {
		return err
	}

	// Высота «слогов» зависит от текста — разные реплики звучат по-разному, но
	// детерминированно.
	var seed uint32 = 2166136261
	for i := 0; i < len(text); i++ {
		seed = (seed ^ uint32(text[i])) * 16777619
	}
	const (
		sylLen  = wavRate * 180 / 1000 // слог 180 мс
		toneLen = wavRate * 120 / 1000 // из них звучит 120 мс
		amp     = 1400.0               // ≈ −27 dBFS: тихо, не режет слух в наушниках
	)
	tail := wavRate * 150 / 1000 // последние 150 мс — тишина
	var buf [2]byte
	for i := 0; i < n; i++ {
		var v int16
		if pos := i % sylLen; pos < toneLen && i < n-tail {
			syl := uint32(i / sylLen)
			f := 170 + float64((seed>>(syl%24)+syl*7)%60)
			env := math.Sin(math.Pi * float64(pos) / toneLen)
			v = int16(amp * env * math.Sin(2*math.Pi*f*float64(i)/wavRate))
		}
		binary.LittleEndian.PutUint16(buf[:], uint16(v))
		if _, err := bw.Write(buf[:]); err != nil {
			return err
		}
	}
	return bw.Flush()
}
