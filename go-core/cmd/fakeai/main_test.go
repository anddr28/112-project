package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var configEnv = []string{
	"FAKEAI_ADDR", "INTERNAL_API_TOKEN", "GOCORE_CALLBACK_URL", "TTS_DIR", "FAKEAI_SPEED", "FAKEAI_BUSY_EVERY",
	"FAKEAI_FAIL_EVERY", "FAKEAI_QUEUE_MAX", "FAKEAI_LLM_WORKERS", "FAKEAI_LT_WORKERS", "FAKEAI_TTS_WORKERS",
	"FAKEAI_DONE_TTL", "FAKEAI_LOG_LEVEL",
}

// clearEnv — переменные конфига не заданы (t.Setenv восстановит исходные значения).
func clearEnv(t *testing.T) {
	for _, k := range configEnv {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

// Не t.Parallel: t.Setenv.
func TestLoadConfigDefaults(t *testing.T) {
	clearEnv(t)
	c := loadConfig()
	want := config{
		addr: ":8000", token: "dev-internal-token", callbackURL: "http://localhost:8080/internal/ai/v1/results",
		ttsDir: "./data/tts_cache", speed: 1, queueMax: 200, llmWorkers: 2, ltWorkers: 2, ttsWorkers: 2,
		doneTTL: 30 * time.Minute, logLevel: "info",
	}
	if c != want {
		t.Fatalf("defaults:\n%+v\n%+v", c, want)
	}
}

func TestLoadConfigOverridesAndClamps(t *testing.T) {
	clearEnv(t)
	for k, v := range map[string]string{
		"FAKEAI_ADDR": " :9000 ", "INTERNAL_API_TOKEN": "tok", "FAKEAI_SPEED": "-2", "FAKEAI_BUSY_EVERY": "-1",
		"FAKEAI_FAIL_EVERY": "3", "FAKEAI_QUEUE_MAX": "0", "FAKEAI_LLM_WORKERS": "0", "FAKEAI_LT_WORKERS": "abc",
		"FAKEAI_TTS_WORKERS": " 4 ", "FAKEAI_DONE_TTL": "-5s", "FAKEAI_LOG_LEVEL": "DEBUG",
	} {
		t.Setenv(k, v)
	}
	c := loadConfig()
	if c.addr != ":9000" || c.token != "tok" || c.speed != 0 || c.busyEvery != 0 || c.failEvery != 3 || c.queueMax != 1 ||
		c.llmWorkers != 1 || c.ltWorkers != 2 || c.ttsWorkers != 4 || c.doneTTL != 30*time.Minute || c.logLevel != "DEBUG" {
		t.Fatalf("config %+v", c)
	}
	t.Setenv("FAKEAI_SPEED", "0.25")
	t.Setenv("FAKEAI_DONE_TTL", "90s")
	if c = loadConfig(); c.speed != 0.25 || c.doneTTL != 90*time.Second {
		t.Fatalf("config %+v", c)
	}
}

func TestNewLogger(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for lvl, want := range map[string]slog.Level{
		"debug": slog.LevelDebug, "DEBUG": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn,
		"error": slog.LevelError, "": slog.LevelInfo, "verbose": slog.LevelInfo,
	} {
		l := newLogger(lvl)
		if !l.Enabled(ctx, want) || (want > slog.LevelDebug && l.Enabled(ctx, want-4)) {
			t.Errorf("newLogger(%q): уровень не %v", lvl, want)
		}
	}
}

// Ошибки старта не зависают: воркеры гасятся, run возвращает ошибку.
func TestRunFailsFast(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.addr = "127.0.0.1:-1"
	done := make(chan error, 1)
	go func() { done <- run(cfg, discardLog()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("run с плохим адресом вернул nil")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run завис")
	}

	cfg = testConfig(t)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.ttsDir = filepath.Join(file, "tts")
	if err := run(cfg, discardLog()); err == nil {
		t.Fatal("run с недоступным TTS_DIR вернул nil")
	}
}
