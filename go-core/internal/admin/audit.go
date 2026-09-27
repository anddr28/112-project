package admin

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/audit"
	"lct/gocore/internal/platform/httpx"
)

// maxFilterText — потолок строковых фильтров (action, entityType): длиннее имён не бывает,
// а мусор в сотни килобайт в SQL-параметр не нужен.
const maxFilterText = 100

// GET /admin/audit — журнал аудита, новые сверху, курсор beforeId. Некорректные параметры —
// 400 с разбором по полям (молча игнорировать фильтр опасно: админ увидит «не те» записи).
func (h *Handlers) listAudit(w http.ResponseWriter, r *http.Request) error {
	f, err := parseAuditFilter(r.URL.Query(), time.Local)
	if err != nil {
		return err
	}
	list, err := audit.List(r.Context(), h.pool, f)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, list)
	return nil
}

// parseAuditFilter — query → audit.Filter. loc — зона для дат без времени («2026-09-24»:
// админ имеет в виду местные сутки).
func parseAuditFilter(q url.Values, loc *time.Location) (audit.Filter, error) {
	var (
		f    audit.Filter
		errs = map[string]string{}
	)
	parseID := func(name string) *uuid.UUID {
		s := strings.TrimSpace(q.Get(name))
		if s == "" {
			return nil
		}
		id, err := uuid.Parse(s)
		if err != nil {
			errs[name] = "ожидается UUID"
			return nil
		}
		return &id
	}
	text := func(name string) string {
		s := strings.TrimSpace(q.Get(name))
		if len(s) > maxFilterText {
			errs[name] = "слишком длинное значение"
			return ""
		}
		return s
	}

	f.ActorID = parseID("actorId")
	f.EntityID = parseID("entityId")
	f.Action = text("action")
	f.EntityType = text("entityType")

	if s := strings.TrimSpace(q.Get("from")); s != "" {
		if t, ok := parseTime(s, loc, false); ok {
			f.From = &t
		} else {
			errs["from"] = "ожидается дата-время RFC 3339 или дата ГГГГ-ММ-ДД"
		}
	}
	if s := strings.TrimSpace(q.Get("to")); s != "" {
		if t, ok := parseTime(s, loc, true); ok {
			f.To = &t
		} else {
			errs["to"] = "ожидается дата-время RFC 3339 или дата ГГГГ-ММ-ДД"
		}
	}
	if f.From != nil && f.To != nil && f.From.After(*f.To) {
		errs["from"] = "начало периода позже конца"
	}

	if s := strings.TrimSpace(q.Get("beforeId")); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil || id <= 0 {
			errs["beforeId"] = "ожидается положительное целое"
		} else {
			f.BeforeID = &id
		}
	}
	if s := strings.TrimSpace(q.Get("limit")); s != "" {
		n, err := strconv.Atoi(s)
		switch {
		case err != nil || n < 1:
			errs["limit"] = "ожидается целое от 1 до 500"
		case n > audit.MaxLimit:
			f.Limit = audit.MaxLimit // контракт: maximum 500 — больший лимит ужимаем, а не отвергаем
		default:
			f.Limit = n
		}
	}

	if len(errs) > 0 {
		return audit.Filter{}, httpx.Validation("Некорректные параметры фильтра журнала аудита", errs)
	}
	return f, nil
}

// parseTime — RFC 3339 (с дробной частью или без) либо дата. Дата как конец периода —
// последняя микросекунда суток (сравнение в audit.List включительное, точность timestamptz — мкс).
func parseTime(s string, loc *time.Location, endOfDay bool) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), true
	}
	// «+03:00» без URL-кодирования приходит как « 03:00» (плюс в query — пробел)
	if i := strings.LastIndexByte(s, ' '); i > 0 {
		if t, err := time.Parse(time.RFC3339Nano, s[:i]+"+"+s[i+1:]); err == nil {
			return t.UTC(), true
		}
	}
	if loc == nil {
		loc = time.UTC
	}
	d, err := time.ParseInLocation(time.DateOnly, s, loc)
	if err != nil {
		return time.Time{}, false
	}
	if endOfDay {
		d = d.AddDate(0, 0, 1).Add(-time.Microsecond)
	}
	return d.UTC(), true
}
