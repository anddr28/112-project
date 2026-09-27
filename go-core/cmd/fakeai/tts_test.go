package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestValidHashAndPath(t *testing.T) {
	t.Parallel()
	for h, want := range map[string]bool{
		"abcdefgh": true, "ABCDEF_-09": true, strings.Repeat("f", 64): true, strings.Repeat("a", 128): true,
		"abcdefg": false, strings.Repeat("a", 129): false, "../../etc": false, "abc/defgh": false, "abc.defgh": false,
		"abcdefgh\x00": false, "абвгдежз": false, "": false, "abcd efgh": false,
	} {
		if got := validHash(h); got != want {
			t.Errorf("validHash(%q) = %v", h, got)
		}
	}
	if got := hashPath("0123456789"); got != "tts/01/0123456789.wav" {
		t.Errorf("hashPath: %q", got)
	}
}

func TestTTSDurationAndHash(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		text string
		want int
	}{
		{"", ttsMinMs},
		{"Алло", ttsMinMs},                             // 4 символа × 55 < 700
		{strings.Repeat("я", 100), 5500},               // по символам, не по байтам
		{strings.Repeat("я", 10_000), ttsMaxMs},        // потолок
		{"Служба 112, слушаю вас.", 23 * ttsMsPerChar}, // 23 руны
	} {
		if got := ttsDuration(c.text); got != c.want {
			t.Errorf("ttsDuration(%d рун) = %d, ждали %d", len([]rune(c.text)), got, c.want)
		}
	}
	h := ttsHash("Алло", "baya", 1)
	if len(h) != 64 || !validHash(h) || h != ttsHash("Алло", "baya", 1) {
		t.Fatalf("ttsHash %q", h)
	}
	if h == ttsHash("Алло", "xenia", 1) || h == ttsHash("Алло", "baya", 1.25) || h == ttsHash("Алло!", "baya", 1) {
		t.Fatal("ttsHash не зависит от голоса/темпа/текста")
	}
}

func TestWriteWAV(t *testing.T) {
	t.Parallel()
	for _, durMs := range []int{ttsMinMs, 1234, 5000} {
		var buf bytes.Buffer
		if err := writeWAV(&buf, "Горит квартира на пятом этаже", durMs); err != nil {
			t.Fatal(err)
		}
		w := parseWAV(t, buf.Bytes())
		if w.rate != wavRate || w.channels != 1 || w.bits != 16 || w.dataSize != wavRate*durMs/1000*2 {
			t.Fatalf("%d мс: %+v", durMs, w)
		}
		samples := buf.Bytes()[wavHeaderSize:]
		peak, nonZero := 0, 0
		for i := 0; i+1 < len(samples); i += 2 {
			v := int(int16(binary.LittleEndian.Uint16(samples[i:])))
			if v < 0 {
				v = -v
			}
			peak = max(peak, v)
			if v != 0 {
				nonZero++
			}
		}
		if peak == 0 || peak > 1400 || nonZero == 0 {
			t.Fatalf("%d мс: peak %d, ненулевых %d", durMs, peak, nonZero)
		}
		// Последние 150 мс — тишина.
		tail := samples[len(samples)-wavRate*150/1000*2:]
		if !bytes.Equal(tail, make([]byte, len(tail))) {
			t.Fatalf("%d мс: хвост не тихий", durMs)
		}
	}
	// Детерминизм и зависимость от текста.
	a, b, c := wavBytes(t, "Алло", 1000), wavBytes(t, "Алло", 1000), wavBytes(t, "Пожар на пятом этаже", 1000)
	if !bytes.Equal(a, b) || bytes.Equal(a, c) {
		t.Fatal("WAV недетерминирован или не зависит от текста")
	}
}

type failWriter struct{ n int }

func (f *failWriter) Write(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, os.ErrClosed
	}
	f.n -= len(p)
	return len(p), nil
}

func TestWriteWAVPropagatesErrors(t *testing.T) {
	t.Parallel()
	if err := writeWAV(&failWriter{}, "Алло", 5000); err == nil {
		t.Fatal("ошибка записи потеряна")
	}
}

func TestTTSStoreWrite(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	st := &ttsStore{root: root}

	rel := hashPath("abcdef0123")
	dur, err := st.write(rel, "Алло, пожар!", true)
	if err != nil || dur != ttsDuration("Алло, пожар!") {
		t.Fatalf("write: %d %v", dur, err)
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	fi, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("права %v — go-core под другим uid не прочитает", fi.Mode().Perm())
	}
	readWAVFile(t, root, rel)

	// keep=true: файл адресуется содержимым и не переписывается.
	sentinel := append(make([]byte, wavHeaderSize), []byte("sentinel")...)
	if err := os.WriteFile(full, sentinel, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.write(rel, "Алло, пожар!", true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(full); !bytes.Equal(b, sentinel) {
		t.Fatal("keep=true переписал существующий файл")
	}
	// keep=false (реплики диалога): переписывается.
	if _, err := st.write(rel, "Алло, пожар!", false); err != nil {
		t.Fatal(err)
	}
	readWAVFile(t, root, rel)
	// keep=true поверх битого (≤ заголовка) файла — перезапись.
	if err := os.WriteFile(full, []byte("RIFF"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.write(rel, "Алло, пожар!", true); err != nil {
		t.Fatal(err)
	}
	readWAVFile(t, root, rel)

	// Временных файлов не остаётся.
	entries, _ := os.ReadDir(filepath.Dir(full))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".wav-") {
			t.Fatalf("остался временный файл %s", e.Name())
		}
	}
}

func TestTTSStoreConcurrentWrites(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	st := &ttsStore{root: root}
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := range 32 {
		wg.Go(func() {
			if _, err := st.write("dialog/a/1.wav", strings.Repeat("слово ", 1+i%3), false); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	readWAVFile(t, root, "dialog/a/1.wav") // атомарная замена: файл всегда целый
}

func TestTTSStoreWriteError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	blocker := filepath.Join(root, "tts")
	if err := os.WriteFile(blocker, []byte("файл вместо каталога"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (&ttsStore{root: root}).write(hashPath("abcdef0123"), "Алло", true); err == nil {
		t.Fatal("ошибка создания каталога потеряна")
	}
}
