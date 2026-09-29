package reports

import (
	"bytes"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// GET /users/{id}/certificate: доступ как у прогресса, 409 без оценённых попыток, PDF с
// кириллицей и журнал выгрузок.
func TestCertificate(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	path := func(id uuid.UUID) string { return "/users/" + id.String() + "/certificate?tz=Europe/Moscow" }

	for _, tc := range []struct {
		role   string
		user   uuid.UUID
		target uuid.UUID
		status int
		code   string
	}{
		{"student", e.andreev, e.andreev, 200, ""},
		{"teacher", e.teacher, e.andreev, 200, ""},
		{"admin", e.admin, e.andreev, 200, ""},
		{"student", e.borisov, e.andreev, 403, "forbidden"},
		{"teacher", e.other, e.andreev, 403, "forbidden"},
		{"teacher", e.teacher, e.borisov, 409, "conflict"},  // оценка ещё не готова
		{"teacher", e.teacher, e.vasiliev, 409, "conflict"}, // попыток нет
		{"admin", e.admin, e.teacher, 404, "not_found"},     // не обучающийся
		{"admin", e.admin, uuid.New(), 404, "not_found"},
		{"", uuid.Nil, e.andreev, 401, "unauthorized"},
	} {
		r := e.get(t, path(tc.target), tc.role, tc.user)
		if tc.status != 200 {
			expectAPIError(t, r, tc.status, tc.code)
			continue
		}
		if r.status != 200 || !bytes.HasPrefix(r.body, []byte("%PDF")) || r.header.Get("Content-Type") != "application/pdf" {
			t.Fatalf("%s: %d %q", tc.role, r.status, r.header)
		}
		if cd := r.header.Get("Content-Disposition"); !strings.HasPrefix(cd, `attachment; filename="certificate-2026-09-25.pdf"`) ||
			!strings.Contains(cd, "%D0%A1%D0%B5%D1%80") { // «Сер…» в filename*
			t.Errorf("Content-Disposition = %s", cd)
		}
		if len(r.body) < 5000 {
			t.Errorf("подозрительно маленький PDF: %d байт", len(r.body))
		}
		if tc.role == "admin" && os.Getenv("LCT_KEEP_PDF") != "" {
			_ = os.WriteFile(os.Getenv("LCT_KEEP_PDF"), r.body, 0o644)
		}
	}
	if st := e.get(t, "/users/"+e.andreev.String()+"/certificate?tz=Mars/Base", "admin", e.admin); st.status != http.StatusBadRequest {
		t.Errorf("кривой tz: %d", st.status)
	}
	var n int
	if err := e.pool.QueryRow(t.Context(), `SELECT count(*) FROM reports WHERE type = 'student' AND format = 'pdf' AND user_id = $1 AND status = 'done'`,
		e.andreev).Scan(&n); err != nil || n != 3 {
		t.Errorf("журнал выгрузок: %d %v", n, err)
	}
	certs := 0
	for _, a := range e.aud.all() {
		if a.Action == "certificate.export" && a.EntityID == e.andreev {
			certs++
		}
	}
	if certs != 3 {
		t.Errorf("аудит certificate.export: %d", certs)
	}
}

func TestCertificateTexts(t *testing.T) {
	t.Parallel()
	first := time.Date(2026, 9, 1, 21, 30, 0, 0, time.UTC) // в Москве уже 2 сентября
	last := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	msk, _ := time.LoadLocation("Europe/Moscow")
	d := &certData{Attempts: 21, Lessons: 3, First_: &first, LastAt: &last}
	s := certSentence(d, msk)
	if !strings.Contains(s, "с 02.09.2026 по 24.09.2026") || !strings.Contains(s, "21 карточку в 3 занятиях") {
		t.Errorf("sentence = %s", s)
	}
	id := uuid.MustParse("018f6b2a-0000-7000-8000-000000000001")
	if n := certNumber(id, last, msk); n != "С-20260924-018F6B2A" {
		t.Errorf("номер = %s", n)
	}
	if pluralRu(12, "a", "b", "c") != "c" || pluralRu(22, "a", "b", "c") != "b" {
		t.Error("plural")
	}
}
