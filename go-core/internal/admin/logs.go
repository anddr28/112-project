package admin

import (
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/logring"
)

// LogSource — кольцо системного журнала (реализация: *logring.Ring, logring.Default).
type LogSource interface {
	Snapshot(q logring.Query) []logring.Entry
}

// Границы GET /admin/logs (контракт v1.4).
const (
	logsDefaultLimit = 200
	logsMaxLimit     = 1000
	logsMaxQuery     = 100
)

// GET /admin/logs?level&q&limit — последние записи журнала go-core, новые сверху.
// Некорректные параметры — 400 с разбором по полям (как у аудита).
func (h *Handlers) listLogs(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	errs := map[string]string{}
	lvl, ok := logring.ParseLevel(q.Get("level"))
	if !ok {
		errs["level"] = "ожидается debug, info, warn или error"
	}
	needle := strings.TrimSpace(q.Get("q"))
	if utf8.RuneCountInString(needle) > logsMaxQuery {
		errs["q"] = "не длиннее 100 символов"
	}
	limit := logsDefaultLimit
	if s := strings.TrimSpace(q.Get("limit")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			errs["limit"] = "целое число от 1 до 1000"
		} else {
			limit = min(n, logsMaxLimit)
		}
	}
	if len(errs) > 0 {
		return httpx.Validation("Некорректные параметры журнала", errs)
	}

	out := []public.LogEntry{}
	if h.logs != nil {
		entries := h.logs.Snapshot(logring.Query{MinLevel: lvl, Contains: needle, Limit: limit})
		out = make([]public.LogEntry, 0, len(entries))
		for i := range entries {
			e := &entries[i]
			le := public.LogEntry{At: e.At, Level: public.LogEntryLevel(logring.LevelName(e.Level)), Message: e.Message}
			if m := e.AttrMap(); m != nil {
				le.Attrs = &m
			}
			out = append(out, le)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}
