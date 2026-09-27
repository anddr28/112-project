package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"lct/gocore/internal/auth"
)

func TestReadPassword(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"Секрет-123\n", "Секрет-123", true},
		{"with spaces inside \r\n", "with spaces inside ", true},
		{"no-newline", "no-newline", true},
		{"first\nsecond\n", "first", true},
		{"", "", false},
		{"\n", "", false},
		{"   \r\n", "", false},
	}
	for _, c := range cases {
		got, err := readPassword(strings.NewReader(c.in))
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("readPassword(%q) = %q, %v", c.in, got, err)
		}
	}
}

// Регрессия (ревью: secrets-handling). Пароль аргументом командной строки не принимается
// (история shell, ps), читается из stdin; хэш проверяется тем же auth.
func TestHashPasswordFromStdin(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	if err := hashPassword([]string{"NewAdminPass1"}, nil, &out, &errOut); err == nil || out.Len() != 0 {
		t.Fatalf("пароль из аргумента принят: %v %q", err, out.String())
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("Пароль Админа 1\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	defer r.Close()
	if isTerminal(r) {
		t.Fatal("pipe — не терминал")
	}
	if err := hashPassword(nil, r, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	h := strings.TrimSpace(out.String())
	if !strings.HasPrefix(h, "$argon2id$") || errOut.Len() != 0 {
		t.Fatalf("hash %q, stderr %q (без терминала — без приглашения)", h, errOut.String())
	}
	if ok, err := auth.VerifyPassword(h, "Пароль Админа 1"); err != nil || !ok {
		t.Fatalf("хэш не проверяется: %v %v", ok, err)
	}
	if isTerminal(nil) {
		t.Fatal("isTerminal(nil)")
	}
}

func TestRunNoConfigCommands(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"version"}, {"--version"}, {"help"}, {"-h"}} {
		if err := run(args); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
}
