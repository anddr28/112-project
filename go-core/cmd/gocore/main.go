// Command gocore — ядро тренажёра ДДС.
//
//	gocore serve                        HTTP(S)-сервер: API /api/v1, WebSocket, callback ai-service, SPA
//	gocore migrate [up|down|status]     миграции goose из GOCORE_MIGRATIONS_DIR
//	gocore seed [--demo]                справочники (+ демо-данные) — идемпотентно
//	gocore gencert                      CA контура и серверный сертификат (для установки CA в браузеры)
//	gocore import-classifier <file>     импорт классификатора происшествий (XLSX организаторов)
//	gocore backup                       резервная копия БД сейчас (pg_dump)
//	gocore hash-password                argon2id-хэш пароля из stdin (ручное восстановление доступа админа)
//	gocore version
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"lct/gocore/internal/app"
	"lct/gocore/internal/config"
)

// version проставляется при сборке: -ldflags "-X main.version=…".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gocore:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	if cmd == "version" || cmd == "--version" || cmd == "-v" {
		fmt.Println(version)
		return nil
	}
	if cmd == "help" || cmd == "--help" || cmd == "-h" {
		fmt.Print(usage)
		return nil
	}
	if cmd == "hash-password" { // конфигурация и БД не нужны
		return hashPassword(args, os.Stdin, os.Stdout, os.Stderr)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg.Version = version
	log := app.NewLogger(cfg)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "serve":
		return app.Serve(ctx, cfg, log)
	case "migrate":
		dir := "up"
		if len(args) > 0 {
			dir = args[0]
		}
		return app.Migrate(ctx, cfg, log, dir)
	case "seed":
		demo := cfg.DemoMode
		for _, a := range args {
			if a == "--demo" {
				demo = true
			}
		}
		return app.Seed(ctx, cfg, log, demo)
	case "gencert":
		return app.GenCert(cfg, log)
	case "import-classifier":
		if len(args) < 1 {
			return errors.New("usage: gocore import-classifier <file.xlsx>")
		}
		return app.ImportClassifier(ctx, cfg, log, args[0])
	case "backup":
		return app.Backup(ctx, cfg, log)
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

const usage = `gocore — ядро тренажёра ДДС

  gocore serve                        HTTP(S)-сервер (по умолчанию)
  gocore migrate [up|down|status]     миграции БД
  gocore seed [--demo]                справочники и демо-данные
  gocore gencert                      сертификаты контура (CA + сервер)
  gocore import-classifier <file>     импорт классификатора из XLSX
  gocore backup                       резервная копия БД
  gocore hash-password                argon2id-хэш пароля (пароль — из stdin, не аргументом)
  gocore version

Конфигурация — переменные окружения (см. go-core/README.md, .env.example).
`

// hashPassword — `gocore hash-password`. Пароль читается из stdin (с терминала — без эха,
// если доступен stty), а не из аргумента: аргумент остаётся в истории shell, в ps
// (/proc/<pid>/cmdline) и в журнале `docker exec`.
func hashPassword(args []string, stdin *os.File, stdout, stderr io.Writer) error {
	if len(args) > 0 {
		return errors.New("пароль аргументом не принимается (он останется в истории shell и в ps): " +
			"запустите `gocore hash-password` и введите пароль, или `gocore hash-password < file`")
	}
	tty := isTerminal(stdin)
	if tty {
		fmt.Fprint(stderr, "Пароль: ")
		if restore := disableEcho(stdin); restore != nil {
			// Ctrl+C во время ввода не должен оставить терминал без эха
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			go func() {
				if _, ok := <-sig; ok {
					restore()
					fmt.Fprintln(stderr)
					os.Exit(130)
				}
			}()
			defer func() {
				signal.Stop(sig)
				close(sig)
				restore()
				fmt.Fprintln(stderr)
			}()
		}
	}
	pw, err := readPassword(stdin)
	if err != nil {
		return err
	}
	h, err := app.HashPassword(pw)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, h)
	return nil
}

// readPassword — первая строка входа без перевода строки (\n, \r\n). Пустой — ошибка.
func readPassword(r io.Reader) (string, error) {
	line, err := bufio.NewReaderSize(r, 1024).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("чтение пароля: %w", err)
	}
	pw := strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(pw) == "" {
		return "", errors.New("пустой пароль: введите пароль или передайте его через stdin")
	}
	return pw, nil
}

func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// disableEcho — выключить эхо терминала через stty (без новых зависимостей); nil — не вышло
// (нет stty, например в минимальном образе): пароль будет виден на экране, но не в истории.
func disableEcho(tty *os.File) (restore func()) {
	off := exec.Command("stty", "-echo")
	off.Stdin = tty
	if off.Run() != nil {
		return nil
	}
	return func() {
		on := exec.Command("stty", "echo")
		on.Stdin = tty
		_ = on.Run()
	}
}
