package admin

import (
	"errors"
	"log/slog"
	"net/http"
	"testing"

	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/logring"
)

// GET /admin/logs: новые сверху, фильтры уровня/подстроки/лимита, ошибки — по полям.
func TestListLogs(t *testing.T) {
	t.Parallel()
	ring := logring.New(100)
	log := slog.New(logring.NewHandler(slog.NewTextHandler(discardWriter{}, nil), ring))
	log.Info("listening", "addr", ":8443")
	log.Warn("ai-service circuit breaker", "state", "open")
	log.Error("handler error", "route", "POST /lessons", "err", errors.New("boom"))

	srv, _ := newServer(t, New(Deps{Logs: ring, Log: discardLog()}))
	opt := reqOpt{role: "admin"}

	all := decode[[]public.LogEntry](t, do(t, srv, http.MethodGet, "/admin/logs", "", opt))
	if len(all) != 3 || all[0].Message != "handler error" || all[0].Level != "error" || all[2].Level != "info" {
		t.Fatalf("журнал = %+v", all)
	}
	if all[0].Attrs == nil || (*all[0].Attrs)["err"] != "boom" || (*all[0].Attrs)["route"] != "POST /lessons" {
		t.Errorf("attrs = %+v", all[0].Attrs)
	}
	errs := decode[[]public.LogEntry](t, do(t, srv, http.MethodGet, "/admin/logs?level=error", "", opt))
	if len(errs) != 1 || errs[0].Message != "handler error" {
		t.Errorf("level=error: %+v", errs)
	}
	warn := decode[[]public.LogEntry](t, do(t, srv, http.MethodGet, "/admin/logs?level=warn&limit=1", "", opt))
	if len(warn) != 1 || warn[0].Level != "error" {
		t.Errorf("level=warn&limit=1: %+v", warn)
	}
	q := decode[[]public.LogEntry](t, do(t, srv, http.MethodGet, "/admin/logs?q=BREAKER", "", opt))
	if len(q) != 1 || q[0].Level != "warn" {
		t.Errorf("q: %+v", q)
	}
	expectError(t, do(t, srv, http.MethodGet, "/admin/logs?level=fatal&limit=0", "", opt), http.StatusBadRequest, "validation", "level", "limit")

	// без кольца — пустой массив, а не null
	srv2, _ := newServer(t, New(Deps{Log: discardLog()}))
	if r := do(t, srv2, http.MethodGet, "/admin/logs", "", opt); r.status != 200 || string(r.body) != "[]\n" {
		t.Errorf("пустой журнал: %d %q", r.status, r.body)
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
