package config

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// Все переменные, которые читает Load (чистое окружение для теста).
var allVars = []string{
	"GOCORE_HTTP_ADDR", "GOCORE_HTTPS_ADDR", "GOCORE_HTTP_API", "GOCORE_TLS_CERT", "GOCORE_TLS_KEY", "GOCORE_TLS_DIR",
	"GOCORE_TLS_HOSTS", "DATABASE_URL", "GOCORE_DB_MAX_CONNS", "GOCORE_MIGRATIONS_DIR", "GOCORE_AUTO_MIGRATE",
	"AI_SERVICE_URL", "INTERNAL_API_TOKEN", "GOCORE_AI_JOB_TIMEOUT", "GOCORE_AI_SYNC_TIMEOUT", "TTS_DIR",
	"GOCORE_STATIC_DIR", "GOCORE_BACKUP_DIR", "GOCORE_PG_DUMP", "GOCORE_WS_ORIGINS", "GOCORE_DEMO_MODE",
	"GOCORE_SEED", "GOCORE_COOKIE_SECURE", "GOCORE_LOG_LEVEL", "GOCORE_LOG_FORMAT", "GOCORE_INSTANCE_ID",
}

// cleanEnv — снять все переменные (t.Setenv восстановит исходные значения после теста).
// t.Setenv несовместим с t.Parallel — тесты пакета идут последовательно.
func cleanEnv(t *testing.T) {
	t.Helper()
	for _, k := range allVars {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func TestLoadDefaults(t *testing.T) {
	cleanEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	checks := []struct {
		name string
		ok   bool
	}{
		{"HTTPAddr", c.HTTPAddr == ":8080"},
		{"HTTPSAddr", c.HTTPSAddr == ":8443"},
		{"HTTPServeAPI выключен", !c.HTTPServeAPI},
		{"TLSHosts", slices.Equal(c.TLSHosts, []string{"localhost", "127.0.0.1", "go-core"})},
		{"DBMaxConns", c.DBMaxConns == 24},
		{"AutoMigrate", c.AutoMigrate},
		{"AIServiceURL", c.AIServiceURL == "http://localhost:8000"},
		{"InternalAPIToken", c.InternalAPIToken == DevInternalToken},
		{"AIJobTimeout", c.AIJobTimeout == 5*time.Second},
		{"AISyncTimeout", c.AISyncTimeout == 20*time.Second},
		{"StaticDir", c.StaticDir == ""},
		{"WSOrigins", slices.Equal(c.WSOrigins, []string{"localhost:*", "127.0.0.1:*"})},
		{"DemoMode", c.DemoMode},
		{"CookieSecure", c.CookieSecure},
		{"InstanceID", c.InstanceID == host},
		{"DatabaseURL", strings.HasPrefix(c.DatabaseURL, "postgres://")},
	}
	for _, ch := range checks {
		if !ch.ok {
			t.Errorf("по умолчанию: %s (%+v)", ch.name, c)
		}
	}
}

func TestLoadOverrides(t *testing.T) {
	cleanEnv(t)
	t.Setenv("GOCORE_HTTP_ADDR", "  127.0.0.1:9090 ")
	t.Setenv("GOCORE_HTTPS_ADDR", "")
	t.Setenv("GOCORE_HTTP_API", "true")
	t.Setenv("GOCORE_TLS_HOSTS", " a.local , ,10.0.0.5,")
	t.Setenv("GOCORE_DB_MAX_CONNS", "8")
	t.Setenv("AI_SERVICE_URL", "http://ai:8000///")
	t.Setenv("GOCORE_AI_SYNC_TIMEOUT", "1m30s")
	t.Setenv("GOCORE_AI_JOB_TIMEOUT", "пять секунд") // мусор — значение по умолчанию
	t.Setenv("GOCORE_DEMO_MODE", "0")
	t.Setenv("GOCORE_COOKIE_SECURE", "FALSE")
	t.Setenv("GOCORE_AUTO_MIGRATE", "maybe") // мусор — по умолчанию true
	t.Setenv("INTERNAL_API_TOKEN", "s3cr3t-token-of-enough-length")
	t.Setenv("GOCORE_WS_ORIGINS", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != "127.0.0.1:9090" || c.HTTPSAddr != "" || !c.HTTPServeAPI {
		t.Errorf("адреса: %q %q %v", c.HTTPAddr, c.HTTPSAddr, c.HTTPServeAPI)
	}
	if !slices.Equal(c.TLSHosts, []string{"a.local", "10.0.0.5"}) {
		t.Errorf("TLSHosts: %q", c.TLSHosts)
	}
	if c.DBMaxConns != 8 || c.AIServiceURL != "http://ai:8000" || c.AISyncTimeout != 90*time.Second || c.AIJobTimeout != 5*time.Second {
		t.Errorf("числа/URL: %+v", c)
	}
	if c.DemoMode || c.CookieSecure || !c.AutoMigrate {
		t.Errorf("bool: demo=%v secure=%v migrate=%v", c.DemoMode, c.CookieSecure, c.AutoMigrate)
	}
	if c.WSOrigins != nil {
		t.Errorf("пустой список: %q", c.WSOrigins)
	}
	if err := c.CheckServe(); err != nil {
		t.Errorf("боевой конфиг с собственным токеном: %v", err)
	}
}

func TestLoadErrors(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"пустой DATABASE_URL":  {"DATABASE_URL": ""},
		"пустой токен":         {"INTERNAL_API_TOKEN": "   "},
		"сертификат без ключа": {"GOCORE_TLS_CERT": "/c.pem"},
		"ключ без сертификата": {"GOCORE_TLS_KEY": "/k.pem"},
	} {
		t.Run(name, func(t *testing.T) {
			cleanEnv(t)
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil {
				t.Fatal("ожидалась ошибка")
			}
		})
	}
	cleanEnv(t)
	t.Setenv("GOCORE_TLS_CERT", "/c.pem")
	t.Setenv("GOCORE_TLS_KEY", "/k.pem")
	if _, err := Load(); err != nil {
		t.Fatalf("пара сертификат+ключ: %v", err)
	}
}

// Регрессия (ревью: insecure-defaults). Вне демо-режима сервер не стартует с общеизвестным
// или коротким внутренним токеном; небезопасные, но допустимые настройки — предупреждения.
func TestCheckServeAndWarnings(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		ok      bool
		warns   []string // подстроки ожидаемых предупреждений
		noWarns []string
	}{
		{"демо-стенд по умолчанию", Config{DemoMode: true, InternalAPIToken: DevInternalToken, CookieSecure: true, HTTPAddr: ":8080"},
			true, []string{"GOCORE_DEMO_MODE", "INTERNAL_API_TOKEN"}, []string{"GOCORE_HTTP_API", "COOKIE_SECURE"}},
		{"бой с токеном по умолчанию", Config{InternalAPIToken: DevInternalToken, CookieSecure: true},
			false, []string{"INTERNAL_API_TOKEN"}, []string{"GOCORE_DEMO_MODE"}},
		{"бой с коротким токеном", Config{InternalAPIToken: "short", CookieSecure: true}, false, nil, nil},
		{"бой как надо", Config{InternalAPIToken: "0123456789abcdef-long", CookieSecure: true, HTTPAddr: ":8080"},
			true, nil, []string{"GOCORE_DEMO_MODE", "INTERNAL_API_TOKEN", "GOCORE_HTTP_API", "COOKIE_SECURE"}},
		{"API по http и cookie без Secure", Config{InternalAPIToken: "0123456789abcdef-long", HTTPServeAPI: true, HTTPAddr: ":8080"},
			true, []string{"GOCORE_HTTP_API", "GOCORE_COOKIE_SECURE"}, nil},
		{"API по http без http-слушателя", Config{InternalAPIToken: "0123456789abcdef-long", HTTPServeAPI: true, CookieSecure: true},
			true, nil, []string{"GOCORE_HTTP_API"}},
	}
	for _, c := range cases {
		err := c.cfg.CheckServe()
		if (err == nil) != c.ok {
			t.Errorf("%s: CheckServe = %v", c.name, err)
		}
		if err != nil && !strings.Contains(err.Error(), "INTERNAL_API_TOKEN") {
			t.Errorf("%s: ошибка не называет переменную: %v", c.name, err)
		}
		all := strings.Join(c.cfg.SecurityWarnings(), "\n")
		for _, w := range c.warns {
			if !strings.Contains(all, w) {
				t.Errorf("%s: нет предупреждения про %s: %q", c.name, w, all)
			}
		}
		for _, w := range c.noWarns {
			if strings.Contains(all, w) {
				t.Errorf("%s: лишнее предупреждение про %s", c.name, w)
			}
		}
	}
}

func TestSplitList(t *testing.T) {
	for in, want := range map[string][]string{
		"":            nil,
		" , ,":        nil,
		"a":           {"a"},
		" a , b ,, c": {"a", "b", "c"},
		"Москва,Тула": {"Москва", "Тула"},
	} {
		if got := splitList(in); !slices.Equal(got, want) {
			t.Errorf("splitList(%q) = %q, want %q", in, got, want)
		}
	}
}
