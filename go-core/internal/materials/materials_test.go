package materials

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/pgtest"
)

// ---------------------------------------------------------------- юнит

func ooxml(t *testing.T, part string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(part)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("<x/>"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDetectType(t *testing.T) {
	t.Parallel()
	docx, xlsx := ooxml(t, "word/document.xml"), ooxml(t, "xl/workbook.xml")
	for _, tc := range []struct {
		name string
		data []byte
		mime string // "" — отклонить
	}{
		{"памятка.pdf", []byte("%PDF-1.7 ..."), "application/pdf"},
		{"x.PDF", []byte("%PDF-1.4"), "application/pdf"},
		{"x.pdf", []byte("<html><script>alert(1)</script>"), ""},
		{"регламент.docx", docx, "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
		{"x.docx", xlsx, ""},
		{"x.xlsx", xlsx, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
		{"call.mp3", []byte("ID3\x04\x00..."), "audio/mpeg"},
		{"call.mp3", []byte{0xFF, 0xFB, 0x90, 0x00}, "audio/mpeg"},
		{"call.wav", []byte("RIFF\x24\x00\x00\x00WAVEfmt "), "audio/wav"},
		{"call.wav", []byte("RIFF\x24\x00\x00\x00AVI "), ""},
		{"s.png", []byte("\x89PNG\r\n\x1a\n...."), "image/png"},
		{"s.jpeg", []byte("\xff\xd8\xff\xe0"), "image/jpeg"},
		{"a.txt", []byte("\xef\xbb\xbfТекст\nс переводом"), "text/plain; charset=utf-8"},
		{"a.txt", []byte("bin\x00ary"), ""},
		{"a.xml", []byte("  <?xml version=\"1.0\"?><a/>"), "application/xml"},
		{"a.json", []byte(`{"a": [1]}`), "application/json"},
		{"a.json", []byte(`{"a": `), ""},
		{"a.exe", []byte("MZ"), ""},
		{"a.svg", []byte("<svg/>"), ""},
		{"a.html", []byte("<html/>"), ""},
		{"пусто.pdf", nil, ""},
	} {
		ft, ok := detectType(tc.name, tc.data)
		if tc.mime == "" {
			if ok {
				t.Errorf("%s: принят как %s", tc.name, ft.mime)
			}
			continue
		}
		if !ok || ft.mime != tc.mime {
			t.Errorf("%s: %v %q, want %q", tc.name, ok, ft.mime, tc.mime)
		}
	}
}

func TestCleanFileNameAndDisposition(t *testing.T) {
	t.Parallel()
	if got := cleanFileName(`C:\Users\x\..\Памятка "ДДС".pdf`); got != "Памятка ДДС.pdf" {
		t.Errorf("cleanFileName = %q", got)
	}
	long := strings.Repeat("я", 300) + ".pdf"
	if got := cleanFileName(long); len([]rune(got)) != maxFileNameRunes || !strings.HasSuffix(got, ".pdf") {
		t.Errorf("длинное имя: %d %q", len([]rune(got)), got[len(got)-8:])
	}
	cd := contentDisposition("inline", "Памятка 112.pdf")
	if !strings.HasPrefix(cd, `inline; filename="_______ 112.pdf"; filename*=UTF-8''%D0%9F`) {
		t.Errorf("disposition = %s", cd)
	}
	if !inlineMIME("audio/mpeg") || !inlineMIME("application/pdf") || inlineMIME("application/json") {
		t.Error("inlineMIME")
	}
}

func TestSystemMaterials(t *testing.T) {
	t.Parallel()
	ms, err := System()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) < 5 || len(ms) > 8 {
		t.Fatalf("системных материалов %d, ожидается 5–8", len(ms))
	}
	seen := map[string]bool{}
	for _, m := range ms {
		if seen[m.Slug] {
			t.Errorf("повтор slug %s", m.Slug)
		}
		seen[m.Slug] = true
		if len([]rune(m.Content)) < 2000 || m.Description == "" || !strings.HasPrefix(m.Content, "# ") {
			t.Errorf("%s: материал слишком короткий или без заголовка (%d символов)", m.Slug, len([]rune(m.Content)))
		}
	}
	if _, err := parseMaterial("без заголовка"); err == nil {
		t.Error("битый материал принят")
	}
}

// ---------------------------------------------------------------- HTTP + БД

type stubAuth struct{}

func (stubAuth) Authenticate(r *http.Request) (*core.Principal, error) {
	role := r.Header.Get("X-Test-Role")
	if role == "" {
		return nil, core.ErrUnauthenticated
	}
	id, err := uuid.Parse(r.Header.Get("X-Test-User"))
	if err != nil {
		id = uuid.New()
	}
	return &core.Principal{UserID: id, Role: core.Role(role), LastName: "Учителев", FirstName: "Иван"}, nil
}

type recAuditor struct {
	mu sync.Mutex
	e  []core.AuditEntry
}

func (a *recAuditor) Log(_ context.Context, e core.AuditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.e = append(a.e, e)
}

func (a *recAuditor) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, e := range a.e {
		out = append(out, e.Action)
	}
	return out
}

type env struct {
	pool                     *pgxpool.Pool
	srv                      *httptest.Server
	aud                      *recAuditor
	teacher, teacher2, admin uuid.UUID
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := pgtest.New(t)
	e := &env{pool: pool, aud: &recAuditor{}}
	add := func(role string) uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(context.Background(), `INSERT INTO users (id, login, password_hash, role, last_name, first_name)
			VALUES ($1, $2, 'x', $3, 'Учителев', 'Иван')`, id, "u"+id.String()[:8], role); err != nil {
			t.Fatal(err)
		}
		return id
	}
	e.teacher, e.teacher2, e.admin = add("teacher"), add("teacher"), add("admin")
	if err := SeedSystem(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	h := New(Deps{Pool: pool, Auditor: e.aud, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	mux := http.NewServeMux()
	r := httpx.NewRouter(mux, "/api/v1", stubAuth{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	h.Register(r)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

type resp struct {
	status int
	header http.Header
	body   []byte
}

func (e *env) do(t *testing.T, method, path, role string, user uuid.UUID, body io.Reader, ctype string, hdr ...string) resp {
	t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+"/api/v1"+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if role != "" {
		req.Header.Set("X-Test-Role", role)
		req.Header.Set("X-Test-User", user.String())
	}
	req.Header.Set("X-Requested-With", "fetch")
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, res.Header, b}
}

// form — multipart с текстовыми полями и (необязательно) файлом.
func form(t *testing.T, fields map[string]string, fileName string, data []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	if fileName != "" {
		w, err := mw.CreateFormFile("file", fileName)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(data)
	}
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func decode[T any](t *testing.T, r resp, status int) T {
	t.Helper()
	if r.status != status {
		t.Fatalf("status %d, want %d: %s", r.status, status, r.body)
	}
	var v T
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
	return v
}

func errCode(t *testing.T, r resp, status int) (string, map[string]any) {
	t.Helper()
	if r.status != status {
		t.Fatalf("status %d, want %d: %s", r.status, status, r.body)
	}
	var e struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	}
	if err := json.Unmarshal(r.body, &e); err != nil || e.Message == "" {
		t.Fatalf("ApiError %s: %v", r.body, err)
	}
	return e.Code, e.Details
}

func TestMaterialsCRUD_DB(t *testing.T) {
	t.Parallel()
	e := newEnv(t)

	// системные материалы видны всем, без content и файлов
	list := decode[[]public.Material](t, e.do(t, "GET", "/materials", "student", uuid.New(), nil, ""), 200)
	sys, _ := System()
	if len(list) != len(sys) || !list[0].System || list[0].Content != nil || list[0].HasFile {
		t.Fatalf("список системных: %d %+v", len(list), list[0])
	}
	if list[0].Title != sys[0].Title {
		t.Errorf("порядок поставки: %q", list[0].Title)
	}
	one := decode[public.Material](t, e.do(t, "GET", "/materials/"+list[0].Id.String(), "student", uuid.New(), nil, ""), 200)
	if one.Content == nil || !strings.Contains(*one.Content, "АРМ-112") {
		t.Errorf("материал без текста: %+v", one)
	}
	// студенту добавлять нельзя
	body, ct := form(t, map[string]string{"title": "Моё"}, "", nil)
	if code, _ := errCode(t, e.do(t, "POST", "/materials", "student", uuid.New(), body, ct), 403); code != "forbidden" {
		t.Error(code)
	}

	// преподаватель: текст + PDF
	pdf := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("0123456789"), 300000)...) // ~3 МБ — несколько кусков
	body, ct = form(t, map[string]string{"title": "Регламент смены", "category": "Регламенты", "description": "Для 3 смены",
		"content": "# Смена\nТекст"}, "Регламент смены.pdf", pdf)
	m := decode[public.Material](t, e.do(t, "POST", "/materials", "teacher", e.teacher, body, ct), 201)
	if !m.HasFile || m.MimeType == nil || *m.MimeType != "application/pdf" || m.SizeBytes == nil || *m.SizeBytes != int64(len(pdf)) ||
		m.System || m.AuthorId == nil || *m.AuthorId != e.teacher || m.AuthorName == nil || m.Category != "Регламенты" {
		t.Fatalf("создан: %+v", m)
	}
	// файл целиком (несколько кусков по 1 МБ) и диапазоном
	r := e.do(t, "GET", "/materials/"+m.Id.String()+"/file", "student", uuid.New(), nil, "")
	if r.status != 200 || !bytes.Equal(r.body, pdf) || r.header.Get("Content-Type") != "application/pdf" ||
		!strings.HasPrefix(r.header.Get("Content-Disposition"), "inline;") || r.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("файл: %d len=%d %v", r.status, len(r.body), r.header)
	}
	off := (1 << 20) - 5
	r = e.do(t, "GET", "/materials/"+m.Id.String()+"/file", "student", uuid.New(), nil, "", "Range", fmt.Sprintf("bytes=%d-%d", off, off+9))
	if r.status != http.StatusPartialContent || !bytes.Equal(r.body, pdf[off:off+10]) {
		t.Fatalf("range через границу куска: %d %q", r.status, r.body)
	}
	etag := r.header.Get("ETag")
	if r = e.do(t, "GET", "/materials/"+m.Id.String()+"/file", "student", uuid.New(), nil, "", "If-None-Match", etag); r.status != http.StatusNotModified {
		t.Errorf("If-None-Match: %d", r.status)
	}

	// только текст, JSON-файл — attachment
	body, ct = form(t, map[string]string{"title": "Шаблон карточки"}, "card.json", []byte(`{"a": 1}`))
	j := decode[public.Material](t, e.do(t, "POST", "/materials", "teacher", e.teacher2, body, ct), 201)
	if j.Category != defaultCategory {
		t.Errorf("раздел по умолчанию: %q", j.Category)
	}
	r = e.do(t, "GET", "/materials/"+j.Id.String()+"/file", "teacher", e.teacher, nil, "")
	if !strings.HasPrefix(r.header.Get("Content-Disposition"), "attachment;") {
		t.Errorf("json не attachment: %s", r.header.Get("Content-Disposition"))
	}
	// у системного файла нет
	if code, _ := errCode(t, e.do(t, "GET", "/materials/"+list[0].Id.String()+"/file", "teacher", e.teacher, nil, ""), 404); code != "not_found" {
		t.Error(code)
	}

	// фильтры: раздел и поиск (без учёта регистра, % — буквально)
	got := decode[[]public.Material](t, e.do(t, "GET", "/materials?category=Регламенты&q=смен", "student", uuid.New(), nil, ""), 200)
	if len(got) != 1 || got[0].Id != m.Id {
		t.Errorf("фильтр: %+v", got)
	}
	got = decode[[]public.Material](t, e.do(t, "GET", "/materials?q=%25", "student", uuid.New(), nil, ""), 200)
	if len(got) != 0 {
		t.Errorf("%% как шаблон: %d", len(got))
	}

	// удаление: чужой преподаватель — 403, системный — 409, автор — 204, повтор — 404
	if code, _ := errCode(t, e.do(t, "DELETE", "/materials/"+m.Id.String(), "teacher", e.teacher2, nil, ""), 403); code != "forbidden" {
		t.Error(code)
	}
	if code, _ := errCode(t, e.do(t, "DELETE", "/materials/"+list[0].Id.String(), "admin", e.admin, nil, ""), 409); code != "conflict" {
		t.Error(code)
	}
	if r := e.do(t, "DELETE", "/materials/"+m.Id.String(), "teacher", e.teacher, nil, ""); r.status != 204 {
		t.Fatalf("удаление автором: %d %s", r.status, r.body)
	}
	errCode(t, e.do(t, "DELETE", "/materials/"+m.Id.String(), "teacher", e.teacher, nil, ""), 404)
	if r := e.do(t, "DELETE", "/materials/"+j.Id.String(), "admin", e.admin, nil, ""); r.status != 204 {
		t.Fatalf("удаление админом: %d", r.status)
	}
	if a := strings.Join(e.aud.actions(), ","); a != "material.create,material.create,material.delete,material.delete" {
		t.Errorf("аудит: %s", a)
	}
}

func TestMaterialsValidation_DB(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	post := func(fields map[string]string, name string, data []byte) resp {
		body, ct := form(t, fields, name, data)
		return e.do(t, "POST", "/materials", "admin", e.admin, body, ct)
	}
	_, d := errCode(t, post(map[string]string{"title": "ok"}, "", nil), 400)
	if f, _ := d["fields"].(map[string]any); f["title"] == nil || f["content"] == nil {
		t.Errorf("короткое название и пустой материал: %v", d)
	}
	_, d = errCode(t, post(map[string]string{"title": "Файл"}, "virus.pdf", []byte("MZ\x90\x00")), 400)
	if f, _ := d["fields"].(map[string]any); f["file"] == nil {
		t.Errorf("подмена расширения: %v", d)
	}
	errCode(t, post(map[string]string{"title": "Файл"}, "a.exe", []byte("MZ")), 400)
	// больше 20 МБ — 413
	big := append([]byte("%PDF-"), make([]byte, MaxFileBytes)...)
	if code, _ := errCode(t, post(map[string]string{"title": "Большой"}, "big.pdf", big), 413); code != "validation" {
		t.Error(code)
	}
	// не multipart — 415
	errCode(t, e.do(t, "POST", "/materials", "admin", e.admin, strings.NewReader(`{"title":"x"}`), "application/json"), 415)
	// ровно 20 МБ — можно
	exact := append([]byte("%PDF-"), make([]byte, MaxFileBytes-5)...)
	m := decode[public.Material](t, post(map[string]string{"title": "Ровно 20 МБ"}, "exact.pdf", exact), 201)
	if *m.SizeBytes != MaxFileBytes {
		t.Errorf("size = %d", *m.SizeBytes)
	}
	// несуществующий/кривой id
	errCode(t, e.do(t, "GET", "/materials/"+uuid.New().String(), "student", uuid.New(), nil, ""), 404)
	errCode(t, e.do(t, "GET", "/materials/not-a-uuid", "student", uuid.New(), nil, ""), 404)
}

// Сид идемпотентен и обновляет изменившийся текст поставки.
func TestSeedSystemIdempotent_DB(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	ctx := context.Background()
	for range 2 {
		if err := SeedSystem(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM materials WHERE is_system`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	sys, _ := System()
	if n != len(sys) {
		t.Fatalf("после двух сидов %d, want %d", n, len(sys))
	}
	if _, err := pool.Exec(ctx, `UPDATE materials SET content = 'старое' WHERE slug = $1`, sys[0].Slug); err != nil {
		t.Fatal(err)
	}
	if err := SeedSystem(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var c string
	if err := pool.QueryRow(ctx, `SELECT content FROM materials WHERE slug = $1`, sys[0].Slug).Scan(&c); err != nil || c != sys[0].Content {
		t.Errorf("текст не обновлён: %v", err)
	}
}
