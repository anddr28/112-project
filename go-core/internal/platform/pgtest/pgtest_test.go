package pgtest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Смоук: две независимые БД из шаблона, в каждой — схема со всеми миграциями.
func TestNewIsolated(t *testing.T) {
	t.Parallel()
	a, b := New(t), New(t)
	ctx := context.Background()
	if _, err := a.Exec(ctx, `INSERT INTO settings (key, value) VALUES ('pgtest_probe', '1')`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := b.QueryRow(ctx, `SELECT count(*) FROM settings WHERE key = 'pgtest_probe'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("БД тестов не изолированы: %d", n)
	}
	var tbl bool
	if err := a.QueryRow(ctx, `SELECT to_regclass('attempt_dialogue_turns') IS NOT NULL`).Scan(&tbl); err != nil || !tbl {
		t.Fatalf("миграции не применены: %v", err)
	}
}

func TestWithDB(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"postgres://lct:lct@localhost:55432/postgres?sslmode=disable": "postgres://lct:lct@localhost:55432/lct_x?sslmode=disable",
		"postgres://lct:lct@localhost:55432/postgres":                 "postgres://lct:lct@localhost:55432/lct_x",
		"postgres://lct:lct@localhost:55432?sslmode=disable":          "postgres://lct:lct@localhost:55432/lct_x?sslmode=disable",
		"postgres://localhost":                                        "postgres://localhost/lct_x",
		// раньше: postgresql:// без пути резал схему («postgresql:/lct_x»)
		"postgresql://u:p@db:5432?sslmode=disable":        "postgresql://u:p@db:5432/lct_x?sslmode=disable",
		"postgresql://u:p@db:5432/other":                  "postgresql://u:p@db:5432/lct_x",
		"postgres://u:p%40ss@h/db?a=1&b=2":                "postgres://u:p%40ss@h/lct_x?a=1&b=2",
		"host=localhost port=55432 user=lct dbname=other": "host=localhost port=55432 user=lct dbname=other dbname=lct_x",
	}
	for in, want := range cases {
		if got := withDB(in, "lct_x"); got != want {
			t.Errorf("withDB(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHashDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("00001_init.sql", "CREATE TABLE a();")
	write("README.md", "не миграция")
	h1, err := hashDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	write("README.md", "другое")
	if h2, _ := hashDir(dir); h2 != h1 {
		t.Fatal("не-.sql файлы влияют на хэш")
	}
	write("00001_init.sql", "CREATE TABLE b();")
	if h3, _ := hashDir(dir); h3 == h1 {
		t.Fatal("изменение миграции не меняет хэш (шаблон не пересоздастся)")
	}
	if _, err := hashDir(filepath.Join(dir, "нет")); err == nil {
		t.Fatal("нет каталога — ошибка")
	}
	if _, err := os.Stat(filepath.Join(MigrationsDir(), "00001_init.sql")); err != nil {
		t.Fatalf("MigrationsDir: %v", err)
	}
}
