// Package config — конфигурация go-core из переменных окружения (12-factor).
//
// Всё, что меняет администратор во время работы (веса, пороги, голос), живёт
// в таблице settings (пакет settings). Здесь — только то, что нужно до старта:
// адреса, секреты, пути к volume'ам.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// HTTP. HTTPAddr — открытый HTTP внутри docker-сети (callback ai-service, healthcheck,
	// /metrics). HTTPSAddr — публичный вход для браузеров (микрофон требует secure context).
	HTTPAddr  string // GOCORE_HTTP_ADDR, по умолчанию :8080
	HTTPSAddr string // GOCORE_HTTPS_ADDR, по умолчанию :8443 (пусто — HTTPS выключен)
	// HTTPServeAPI — отдавать API и SPA ещё и по открытому HTTP (GOCORE_HTTP_API). Только за
	// reverse-proxy с TLS-терминацией: иначе пароль и cookie сессии идут по сети открытым
	// текстом (ТЗ: TLS внутри контура). По умолчанию выключено.
	HTTPServeAPI bool
	TLSCertFile  string // GOCORE_TLS_CERT (пусто + HTTPS включён — самоподписанный сертификат в TLSDir)
	TLSKeyFile   string // GOCORE_TLS_KEY
	TLSDir       string // GOCORE_TLS_DIR — где лежит/создаётся CA контура и серверный сертификат
	TLSHosts     []string

	DatabaseURL   string // DATABASE_URL
	DBMaxConns    int32  // GOCORE_DB_MAX_CONNS
	MigrationsDir string // GOCORE_MIGRATIONS_DIR (goose); пусто — миграции не применяются на старте
	AutoMigrate   bool   // GOCORE_AUTO_MIGRATE

	// Внутренний контракт с ai-service
	AIServiceURL     string        // AI_SERVICE_URL, http://ai-service:8000
	InternalAPIToken string        // INTERNAL_API_TOKEN — X-Internal-Token в обе стороны
	AIJobTimeout     time.Duration // таймаут POST /v1/jobs/* (ответ 202 должен быть быстрым)
	AISyncTimeout    time.Duration // таймаут синхронных вызовов (диалог) — контракт: 20 с

	TTSDir     string // TTS_DIR — shared volume tts_cache (ai-service пишет, go-core раздаёт /media/tts)
	StaticDir  string // GOCORE_STATIC_DIR — собранный SPA (frontend/dist); пусто — не раздаём
	BackupDir  string // GOCORE_BACKUP_DIR — куда pg_dump кладёт копии
	PgDumpPath string // GOCORE_PG_DUMP — путь к pg_dump

	// WSOrigins — допустимые Origin для WebSocket помимо своего хоста (vite dev-прокси: localhost:5173).
	// Cookie SameSite=Strict и так закрывает CSWSH; список — для dev-стендов.
	WSOrigins []string // GOCORE_WS_ORIGINS

	DemoMode     bool   // GOCORE_DEMO_MODE — демо-учётки в /auth/demo-accounts и сид демо-данных
	SeedOnStart  bool   // GOCORE_SEED — справочники (+демо при DemoMode) при старте, идемпотентно
	CookieSecure bool   // GOCORE_COOKIE_SECURE — Secure-флаг cookie (выключать только для http-отладки)
	LogLevel     string // GOCORE_LOG_LEVEL: debug|info|warn|error
	LogFormat    string // GOCORE_LOG_FORMAT: json|text
	InstanceID   string // GOCORE_INSTANCE_ID — ai_jobs.locked_by; по умолчанию hostname
	Version      string // заполняется из ldflags (main.version)
}

func Load() (*Config, error) {
	host, _ := os.Hostname()
	c := &Config{
		HTTPAddr:         env("GOCORE_HTTP_ADDR", ":8080"),
		HTTPSAddr:        env("GOCORE_HTTPS_ADDR", ":8443"),
		HTTPServeAPI:     envBool("GOCORE_HTTP_API", false),
		TLSCertFile:      env("GOCORE_TLS_CERT", ""),
		TLSKeyFile:       env("GOCORE_TLS_KEY", ""),
		TLSDir:           env("GOCORE_TLS_DIR", "./data/tls"),
		TLSHosts:         splitList(env("GOCORE_TLS_HOSTS", "localhost,127.0.0.1,go-core")),
		DatabaseURL:      env("DATABASE_URL", "postgres://lct:lct@localhost:55432/lct?sslmode=disable"),
		DBMaxConns:       int32(envInt("GOCORE_DB_MAX_CONNS", 24)),
		MigrationsDir:    env("GOCORE_MIGRATIONS_DIR", "../db/migrations"),
		AutoMigrate:      envBool("GOCORE_AUTO_MIGRATE", true),
		AIServiceURL:     strings.TrimRight(env("AI_SERVICE_URL", "http://localhost:8000"), "/"),
		InternalAPIToken: env("INTERNAL_API_TOKEN", DevInternalToken),
		AIJobTimeout:     envDuration("GOCORE_AI_JOB_TIMEOUT", 5*time.Second),
		AISyncTimeout:    envDuration("GOCORE_AI_SYNC_TIMEOUT", 20*time.Second),
		TTSDir:           env("TTS_DIR", "./data/tts_cache"),
		StaticDir:        env("GOCORE_STATIC_DIR", ""),
		BackupDir:        env("GOCORE_BACKUP_DIR", "./data/backups"),
		PgDumpPath:       env("GOCORE_PG_DUMP", "pg_dump"),
		WSOrigins:        splitList(env("GOCORE_WS_ORIGINS", "localhost:*,127.0.0.1:*")),
		DemoMode:         envBool("GOCORE_DEMO_MODE", true),
		SeedOnStart:      envBool("GOCORE_SEED", true),
		CookieSecure:     envBool("GOCORE_COOKIE_SECURE", true),
		LogLevel:         env("GOCORE_LOG_LEVEL", "info"),
		LogFormat:        env("GOCORE_LOG_FORMAT", "text"),
		InstanceID:       env("GOCORE_INSTANCE_ID", host),
	}
	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	if c.InternalAPIToken == "" {
		return nil, fmt.Errorf("INTERNAL_API_TOKEN is required")
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return nil, fmt.Errorf("GOCORE_TLS_CERT and GOCORE_TLS_KEY must be set together")
	}
	return c, nil
}

// DevInternalToken — токен go-core <-> ai-service по умолчанию (dev-стенд, .env.example).
// Общеизвестен: в боевом контуре (без демо-режима) сервер с ним не стартует.
const DevInternalToken = "dev-internal-token"

// minInternalTokenLen — короче токен вне демо-режима не принимаем (подбор).
const minInternalTokenLen = 16

// CheckServe — проверка конфигурации перед `gocore serve`: небезопасное сочетание, с которым
// сервер не должен подниматься. Демо-режим — осознанный стенд: для него только предупреждения.
func (c *Config) CheckServe() error {
	if c.DemoMode {
		return nil
	}
	if c.InternalAPIToken == DevInternalToken {
		return fmt.Errorf("INTERNAL_API_TOKEN = %q (значение по умолчанию) при GOCORE_DEMO_MODE=false: задайте собственный секрет", DevInternalToken)
	}
	if len(c.InternalAPIToken) < minInternalTokenLen {
		return fmt.Errorf("INTERNAL_API_TOKEN короче %d символов при GOCORE_DEMO_MODE=false", minInternalTokenLen)
	}
	return nil
}

// SecurityWarnings — небезопасные, но допустимые настройки (пишутся в лог при старте).
func (c *Config) SecurityWarnings() []string {
	var out []string
	if c.DemoMode {
		out = append(out, "GOCORE_DEMO_MODE=true: демо-учётки с известными паролями (в т.ч. admin) создаются при старте и публикуются в /auth/demo-accounts — в боевом контуре выключите")
	}
	if c.InternalAPIToken == DevInternalToken {
		out = append(out, "INTERNAL_API_TOKEN по умолчанию: callback /internal/ai/v1/results принимает общеизвестный токен — задайте свой секрет")
	}
	if c.HTTPServeAPI && c.HTTPAddr != "" {
		out = append(out, "GOCORE_HTTP_API=true: API и вход в систему доступны по открытому HTTP "+c.HTTPAddr+" — только за reverse-proxy с TLS")
	}
	if !c.CookieSecure {
		out = append(out, "GOCORE_COOKIE_SECURE=false: cookie сессии без флага Secure — только для отладки по http")
	}
	return out
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return strings.TrimSpace(v)
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(env(key, "")); err == nil {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, err := strconv.ParseBool(env(key, "")); err == nil {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(env(key, "")); err == nil {
		return v
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
