package reports

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"

	"lct/gocore/internal/core"
	"lct/gocore/internal/platform/httpx"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// stubAuth — роль из X-Test-Role (пусто — нет сессии, blocked — заблокирован), id — из X-Test-User.
type stubAuth struct{}

func (stubAuth) Authenticate(r *http.Request) (*core.Principal, error) {
	switch role := r.Header.Get("X-Test-Role"); role {
	case "":
		return nil, core.ErrUnauthenticated
	case "blocked":
		return nil, core.ErrUserBlocked
	default:
		id, err := uuid.Parse(r.Header.Get("X-Test-User"))
		if err != nil {
			id = uuid.New()
		}
		return &core.Principal{UserID: id, Role: core.Role(role), Login: role}, nil
	}
}

type recAuditor struct {
	mu      sync.Mutex
	entries []core.AuditEntry
}

func (a *recAuditor) Log(_ context.Context, e core.AuditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
}

func (a *recAuditor) all() []core.AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]core.AuditEntry(nil), a.entries...)
}

type env struct {
	*fixture
	h   *Handlers
	aud *recAuditor
	srv *httptest.Server
}

// fixedNow — 24.09.2026 22:30 UTC: в поясе UTC+3 уже 25-е (дата в имени файла — в зоне отчёта).
var fixedNow = time.Date(2026, 9, 24, 22, 30, 0, 0, time.UTC)

func newEnv(t *testing.T) *env {
	t.Helper()
	f := newFixture(t)
	aud := &recAuditor{}
	h := New(Deps{Pool: f.pool, Auditor: aud, Log: discardLog()})
	h.now = func() time.Time { return fixedNow }
	mux := http.NewServeMux()
	r := httpx.NewRouter(mux, "/api/v1", stubAuth{}, discardLog(), nil)
	h.Register(r)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &env{fixture: f, h: h, aud: aud, srv: srv}
}

type resp struct {
	status int
	header http.Header
	body   []byte
}

func (e *env) get(t *testing.T, path, role string, user uuid.UUID) resp {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if role != "" {
		req.Header.Set("X-Test-Role", role)
	}
	if user != uuid.Nil {
		req.Header.Set("X-Test-User", user.String())
	}
	res, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp{status: res.StatusCode, header: res.Header, body: b}
}

func expectAPIError(t *testing.T, r resp, status int, code string) map[string]any {
	t.Helper()
	if r.status != status {
		t.Fatalf("status %d, want %d: %s", r.status, status, r.body)
	}
	var e struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	}
	if err := json.Unmarshal(r.body, &e); err != nil {
		t.Fatalf("ApiError %s: %v", r.body, err)
	}
	if e.Code != code || e.Message == "" {
		t.Fatalf("ApiError = %+v, want code %s", e, code)
	}
	return e.Details
}

func TestReportRoute(t *testing.T) {
	t.Parallel()
	h := New(Deps{})
	mux := http.NewServeMux()
	r := httpx.NewRouter(mux, "/api/v1", stubAuth{}, discardLog(), nil)
	h.Register(r)
	if got := r.Routes(); len(got) != 1 || got[0] != "GET /lessons/{lessonId}/report" {
		t.Fatalf("routes = %v", got)
	}
	if cap(h.sem) != 2 || h.log == nil || h.now == nil {
		t.Fatalf("defaults: %+v", h)
	}
}

func TestReportAccess(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	path := "/lessons/" + e.lesson.String() + "/report?format=csv"

	expectAPIError(t, e.get(t, path, "", uuid.Nil), http.StatusUnauthorized, "unauthorized")
	expectAPIError(t, e.get(t, path, "blocked", uuid.Nil), http.StatusLocked, "user_blocked")
	expectAPIError(t, e.get(t, path, "student", e.andreev), http.StatusForbidden, "forbidden")
	expectAPIError(t, e.get(t, path, "teacher", e.other), http.StatusForbidden, "forbidden")
	// практика студента — не занятие преподавателя: только админ
	expectAPIError(t, e.get(t, "/lessons/"+e.practice.String()+"/report?format=csv", "teacher", e.teacher), http.StatusForbidden, "forbidden")
	expectAPIError(t, e.get(t, "/lessons/"+uuid.NewString()+"/report?format=csv", "admin", e.admin), http.StatusNotFound, "not_found")
	expectAPIError(t, e.get(t, "/lessons/not-a-uuid/report?format=csv", "admin", e.admin), http.StatusNotFound, "not_found")

	for _, q := range []string{"", "?format=", "?format=docx", "?format=csv,pdf"} {
		d := expectAPIError(t, e.get(t, "/lessons/"+e.lesson.String()+"/report"+q, "teacher", e.teacher), http.StatusBadRequest, "validation")
		if fields, _ := d["fields"].(map[string]any); fields["format"] == nil {
			t.Errorf("%q: нет details.fields.format: %v", q, d)
		}
	}
	if n := len(e.aud.all()); n != 0 {
		t.Fatalf("отказы попали в журнал выгрузок: %d", n)
	}
	var cnt int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM reports`).Scan(&cnt); err != nil || cnt != 0 {
		t.Fatalf("reports rows = %d (%v)", cnt, err)
	}

	// владелец и админ — 200
	if r := e.get(t, path, "teacher", e.teacher); r.status != http.StatusOK {
		t.Fatalf("владелец: %d %s", r.status, r.body)
	}
	if r := e.get(t, "/lessons/"+e.practice.String()+"/report?format=csv", "admin", e.admin); r.status != http.StatusOK {
		t.Fatalf("админ: %d %s", r.status, r.body)
	}
}

func TestReportFormats(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	// Пояс пользователя — параметром tz (браузер передаёт свой IANA-пояс).
	base := "/lessons/" + e.lesson.String() + "/report?tz=Europe/Moscow&format="
	const wantDate = "2026-09-25"

	cases := []struct {
		format, query, mime string
		check               func(t *testing.T, body []byte)
	}{
		{"csv", "csv", "text/csv; charset=utf-8", func(t *testing.T, body []byte) {
			if !bytes.HasPrefix(body, []byte(utf8BOM+"№;Обучающийся;")) {
				t.Errorf("csv = %.80q", body)
			}
			if n := bytes.Count(body, []byte("\r\n")); n != 6 {
				t.Errorf("строк %d, want шапка + 5", n)
			}
			if !bytes.Contains(body, []byte("Андреев Андрей Андреевич;1;100;Пожар в квартире;Пожар;Оценена;24.09.2026 10:01:00")) {
				t.Errorf("строка Андреева в зоне отчёта не найдена:\n%s", body)
			}
		}},
		{"xlsx", "XLSX", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", func(t *testing.T, body []byte) {
			f, err := excelize.OpenReader(bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			rows, err := f.GetRows(sheetResults)
			if err != nil || len(rows) != 6 {
				t.Errorf("results rows = %d (%v)", len(rows), err)
			}
		}},
		{"pdf", "%20pdf%20", "application/pdf", func(t *testing.T, body []byte) {
			if !bytes.HasPrefix(body, []byte("%PDF-")) || len(body) < 1000 {
				t.Errorf("pdf = %.20q (%d байт)", body, len(body))
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			r := e.get(t, base+tc.query, "teacher", e.teacher)
			if r.status != http.StatusOK {
				t.Fatalf("%d %s", r.status, r.body)
			}
			if ct := r.header.Get("Content-Type"); ct != tc.mime {
				t.Errorf("Content-Type = %q", ct)
			}
			cd := r.header.Get("Content-Disposition")
			if !strings.HasPrefix(cd, `attachment; filename="lesson-report-`+wantDate+"."+tc.format+`"; filename*=UTF-8''`) ||
				!strings.Contains(cd, rfc5987("Отчёт — Итоговое занятие — "+wantDate+"."+tc.format)) {
				t.Errorf("Content-Disposition = %q", cd)
			}
			if r.header.Get("Cache-Control") != "no-store" || r.header.Get("X-Content-Type-Options") != "nosniff" {
				t.Errorf("headers = %v", r.header)
			}
			if tc.format == "xlsx" {
				if cl := r.header.Get("Content-Length"); cl != strconv.Itoa(len(r.body)) {
					t.Errorf("Content-Length = %q, body %d", cl, len(r.body))
				}
			}
			tc.check(t, r.body)
		})
	}

	// журнал: аудит report.export + строка reports на каждую выгрузку
	entries := e.aud.all()
	if len(entries) != 3 {
		t.Fatalf("audit = %d", len(entries))
	}
	for _, a := range entries {
		after, _ := a.After.(map[string]any)
		if a.Action != "report.export" || a.EntityType != "lesson" || a.EntityID != e.lesson || a.LessonID != e.lesson ||
			after["rows"] != 5 || after["delivered"] != true || after["truncated"] != false {
			t.Errorf("audit = %+v", a)
		}
	}
	rows, err := e.pool.Query(context.Background(),
		`SELECT format, type, status, generated_by, lesson_id, params->>'rows', finished_at IS NOT NULL FROM reports ORDER BY format`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var formats []string
	for rows.Next() {
		var (
			format, typ, status, nrows string
			by, lesson                 uuid.UUID
			finished                   bool
		)
		if err := rows.Scan(&format, &typ, &status, &by, &lesson, &nrows, &finished); err != nil {
			t.Fatal(err)
		}
		if typ != "lesson" || status != "done" || by != e.teacher || lesson != e.lesson || nrows != "5" || !finished {
			t.Errorf("reports row = %s %s %s %v %v %s %v", format, typ, status, by, lesson, nrows, finished)
		}
		formats = append(formats, format)
	}
	if strings.Join(formats, ",") != "csv,pdf,xlsx" {
		t.Errorf("formats = %v", formats)
	}
}

// Пустое занятие: файл собирается (шапка без строк), ничего не падает.
func TestReportEmptyLesson(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, f := range []string{"csv", "xlsx", "pdf"} {
		r := e.get(t, "/lessons/"+e.empty.String()+"/report?format="+f, "admin", e.admin)
		if r.status != http.StatusOK || len(r.body) == 0 {
			t.Errorf("%s: %d %.100s", f, r.status, r.body)
		}
	}
}

// Клиент ушёл посреди файла — отчёт помечается failed (delivered=false), обработчик не паникует.
func TestReportRecordUndelivered(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := &core.Principal{UserID: e.teacher, Role: core.RoleTeacher}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // запрос уже отменён: запись журнала не должна отмениться вместе с ним
	e.h.record(ctx, p, e.lesson, "pdf", 3, true, false)
	var status, delivered, truncated string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT status, params->>'delivered', params->>'truncated' FROM reports WHERE lesson_id = $1`, e.lesson).
		Scan(&status, &delivered, &truncated); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || delivered != "false" || truncated != "true" {
		t.Fatalf("reports row = %s %s %s", status, delivered, truncated)
	}
	if a := e.aud.all(); len(a) != 1 || a[0].After.(map[string]any)["delivered"] != false {
		t.Fatalf("audit = %+v", a)
	}
}

// Ожидание очереди сборок ограничено контекстом запроса и не держит соединение пула.
func TestReportConcurrencyLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	h := New(Deps{Pool: e.pool, Log: discardLog(), MaxConcurrent: 1})
	h.sem <- struct{}{} // единственный слот занят

	req := httptest.NewRequest(http.MethodGet, "/api/v1/lessons/"+e.lesson.String()+"/report?format=csv", nil)
	req.SetPathValue("lessonId", e.lesson.String())
	ctx, cancel := context.WithTimeout(core.WithPrincipal(context.Background(), &core.Principal{UserID: e.teacher, Role: core.RoleTeacher}), 100*time.Millisecond)
	defer cancel()
	w := httptest.NewRecorder()
	err := h.report(w, req.WithContext(ctx))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("ответ записан: %s", w.Body.String())
	}
	if st := e.pool.Stat(); st.AcquiredConns() != 0 {
		t.Fatalf("соединений занято: %d", st.AcquiredConns())
	}

	<-h.sem // слот освободился — запрос проходит
	w = httptest.NewRecorder()
	ctx2 := core.WithPrincipal(context.Background(), &core.Principal{UserID: e.teacher, Role: core.RoleTeacher})
	if err := h.report(w, req.WithContext(ctx2)); err != nil || w.Code != http.StatusOK {
		t.Fatalf("err = %v code = %d", err, w.Code)
	}
	if len(h.sem) != 0 {
		t.Fatalf("слот не возвращён: %d", len(h.sem))
	}
}
