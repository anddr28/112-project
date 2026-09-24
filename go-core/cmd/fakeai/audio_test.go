package main

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestAudioFormat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ct     string
		format string
		ok     bool
	}{
		{"", "", true},
		{"application/octet-stream", "", true},
		{"audio/webm;codecs=opus", "webm", true},
		{"AUDIO/WEBM", "webm", true},
		{"video/webm", "webm", true},
		{"audio/ogg; codecs=opus", "ogg", true},
		{"application/ogg", "ogg", true},
		{"audio/opus", "ogg", true},
		{"audio/wav", "wav", true},
		{"audio/x-wav", "wav", true},
		{"audio/wave", "wav", true},
		{"audio/vnd.wave", "wav", true},
		{"audio/mpeg", "mpeg", true},
		{"audio/mp3", "mpeg", true},
		{"audio/flac", "", false},
		{"audio/mp4", "", false},
		{"text/plain", "", false},
		{"not a type", "", false},
		{"audio/", "", false},
	}
	for _, c := range cases {
		f, ok := audioFormat(c.ct)
		if f != c.format || ok != c.ok {
			t.Errorf("audioFormat(%q) = %q %v, ждали %q %v", c.ct, f, ok, c.format, c.ok)
		}
	}
}

func TestSniffFormat(t *testing.T) {
	t.Parallel()
	wav := make([]byte, 44)
	copy(wav, "RIFF")
	copy(wav[8:], "WAVE")
	for _, c := range []struct {
		head []byte
		want string
	}{
		{wav, "wav"},
		{[]byte("RIFF\x00\x00\x00\x00AVI "), "webm"},
		{[]byte("OggS\x00\x02"), "ogg"},
		{[]byte("ID3\x04"), "mpeg"},
		{[]byte{0xFF, 0xFB, 0x90}, "mpeg"},
		{[]byte{0x1A, 0x45, 0xDF, 0xA3}, "webm"},
		{nil, "webm"},
		{[]byte{0xFF}, "webm"},
	} {
		if got := sniffFormat(c.head); got != c.want {
			t.Errorf("sniffFormat(%q) = %q, ждали %q", c.head, got, c.want)
		}
	}
}

func TestEstimateDuration(t *testing.T) {
	t.Parallel()
	var hdr bytes.Buffer
	if err := writeWAV(&hdr, "x", 2000); err != nil {
		t.Fatal(err)
	}
	head := hdr.Bytes()[:64]
	custom := append([]byte(nil), head...)
	binary.LittleEndian.PutUint32(custom[28:32], 8000) // 8 кГц × 8 бит

	cases := []struct {
		name   string
		format string
		head   []byte
		size   int64
		want   int
	}{
		{"wav по заголовку", "wav", head, 44 + 64000, 2000},
		{"wav со своим byte rate", "wav", custom, 44 + 8000, 1000},
		{"wav без заголовка — 16 кГц 16 бит", "wav", []byte("xxxx"), 64000, 2000},
		{"wav короче заголовка — минимум", "wav", head, 10, 300},
		{"opus ~64 кбит/с", "webm", nil, 8000, 1000},
		{"ogg", "ogg", nil, 16000, 2000},
		{"mp3 ~128 кбит/с", "mpeg", nil, 16000, 1000},
		{"минимум 300 мс", "webm", nil, 1, 300},
		{"максимум 60 с", "webm", nil, 10 << 20, 60_000},
	}
	for _, c := range cases {
		if got := estimateDuration(c.format, c.head, c.size); got != c.want {
			t.Errorf("%s: %d, ждали %d", c.name, got, c.want)
		}
	}
}
