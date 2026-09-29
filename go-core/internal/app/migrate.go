package app

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"lct/gocore/internal/config"
	"lct/gocore/internal/platform/logring"
)

// NewLogger — slog по конфигу (json в контейнере, text локально).
func NewLogger(cfg *config.Config) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if strings.ToLower(cfg.LogFormat) == "json" {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	// Тройник в кольцо системного журнала (GET /admin/logs): info+ видны администратору
	// из UI независимо от уровня stdout.
	return slog.New(logring.NewHandler(h, logring.Default)).With("svc", "go-core")
}

// Migrate — goose по каталогу cfg.MigrationsDir. dir: up | down | status | version.
// Отдельное соединение database/sql (goose работает через него), пул приложения не трогаем.
func Migrate(ctx context.Context, cfg *config.Config, log *slog.Logger, dir string) error {
	if cfg.MigrationsDir == "" {
		return fmt.Errorf("GOCORE_MIGRATIONS_DIR is empty")
	}
	if _, err := os.Stat(cfg.MigrationsDir); err != nil {
		return fmt.Errorf("migrations dir %q: %w", cfg.MigrationsDir, err)
	}
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("migrate: open: %w", err)
	}
	defer db.Close()
	pingCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// БД в compose может подниматься дольше go-core — ждём её, а не падаем.
	for {
		if err = db.PingContext(pingCtx); err == nil {
			break
		}
		select {
		case <-pingCtx.Done():
			return fmt.Errorf("migrate: database not reachable: %w", err)
		case <-time.After(time.Second):
		}
	}

	p, err := goose.NewProvider(goose.DialectPostgres, db, os.DirFS(cfg.MigrationsDir))
	if err != nil {
		return fmt.Errorf("migrate: provider: %w", err)
	}
	switch dir {
	case "up":
		res, err := p.Up(ctx)
		for _, r := range res {
			log.Info("migration applied", "file", r.Source.Path, "dur_ms", r.Duration.Milliseconds())
		}
		if err != nil {
			return fmt.Errorf("migrate up: %w", err)
		}
		v, _ := p.GetDBVersion(ctx)
		log.Info("database schema up to date", "version", v)
	case "down":
		r, err := p.Down(ctx)
		if err != nil {
			return fmt.Errorf("migrate down: %w", err)
		}
		if r != nil {
			log.Info("migration rolled back", "file", r.Source.Path)
		}
	case "status", "version":
		st, err := p.Status(ctx)
		if err != nil {
			return fmt.Errorf("migrate status: %w", err)
		}
		for _, s := range st {
			applied := "pending"
			if s.State == goose.StateApplied {
				applied = "applied " + s.AppliedAt.Format(time.RFC3339)
			}
			fmt.Printf("%-40s %s\n", s.Source.Path, applied)
		}
	default:
		return fmt.Errorf("migrate: unknown direction %q (up|down|status)", dir)
	}
	return nil
}

// регистрирует драйвер "pgx" для database/sql (нужен goose)
var _ = stdlib.GetDefaultDriver
