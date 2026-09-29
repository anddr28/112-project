// Package materials — справочная база и методматериалы (контракт v1.4, ТЗ: «загружать
// дополнительные ресурсы», «справочная информация»): список, материал с текстом (Markdown),
// загрузка файла (multipart, до 20 МБ), удаление, отдача файла. Системные материалы из
// поставки (памятка АРМ-112, порядок обработки вызова, статусы реагирования, методика
// оценки, руководство обучающегося) — встроенные Markdown-файлы data/*.md, сид при старте.
//
// Хранение (db-design Р28): файл — bytea в той же строке (STORAGE EXTERNAL, вне строки
// таблицы): бэкап pg_dump один на всё, файловых сирот нет. Список читает только метаданные;
// отдача файла идёт кусками substring(...) — в памяти не больше куска, Range (перемотка
// аудио) работает через http.ServeContent.
package materials

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
)

// Лимиты (контракт v1.4).
const (
	MaxFileBytes     = 20 << 20 // 20 МБ
	maxTitleRunes    = 200
	minTitleRunes    = 3
	maxDescRunes     = 2000
	maxCategoryRunes = 100
	maxContentRunes  = 200000
	maxQueryRunes    = 100
	maxFileNameRunes = 200
	// listCap — потолок строк списка (DESIGN §6: список без потолка в ядро не добавлять).
	// Справочная база учебного класса — десятки–сотни материалов; постраничность не нужна.
	listCap = 500
	// defaultCategory — раздел материала, если автор его не указал.
	defaultCategory = "Прочее"
)

// Deps — зависимости пакета (связывает internal/app).
type Deps struct {
	Pool    *pgxpool.Pool
	Auditor core.Auditor
	Log     *slog.Logger
	// MaxUploads — одновременных загрузок (файл целиком в памяти до INSERT; 0 — 2).
	MaxUploads int
}

// Handlers — ручки /materials*.
type Handlers struct {
	pool    *pgxpool.Pool
	aud     core.Auditor
	log     *slog.Logger
	uploads chan struct{}
}

func New(d Deps) *Handlers {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	n := d.MaxUploads
	if n <= 0 {
		n = 2
	}
	return &Handlers{pool: d.Pool, aud: d.Auditor, log: log.With("component", "materials"), uploads: make(chan struct{}, n)}
}

// Register — маршруты справочной базы. Читают все роли; добавляют и удаляют преподаватель
// (своё) и администратор (любое, кроме системного).
func (h *Handlers) Register(r *httpx.Router) {
	staff := httpx.Roles(core.RoleTeacher, core.RoleAdmin)
	r.Handle("GET /materials", httpx.Authenticated, h.list)
	r.Handle("POST /materials", staff, h.create)
	r.Handle("GET /materials/{materialId}", httpx.Authenticated, h.get)
	r.Handle("DELETE /materials/{materialId}", staff, h.remove)
	r.Handle("GET /materials/{materialId}/file", httpx.Authenticated, h.file)
}

func (h *Handlers) audit(ctx context.Context, action string, id uuid.UUID, before, after any) {
	if h.aud == nil {
		return
	}
	h.aud.Log(ctx, core.AuditEntry{Action: action, EntityType: "material", EntityID: id, Before: before, After: after})
}

func errNotFound() *httpx.Error { return httpx.NotFound("Материал не найден") }

// ---------------------------------------------------------------- чтение

// Метаданные без content и file_data: список не тянет тексты и файлы (TOAST не читается).
const materialCols = `
SELECT m.id, m.title, m.description, m.category, m.file_name, m.mime_type, m.size_bytes, m.is_system,
       m.author_id, COALESCE(u.last_name, ''), COALESCE(u.first_name, ''), COALESCE(u.middle_name, ''), m.created_at`

// Системные — сверху в порядке поставки (slug с номером), затем новые сверху.
const sqlList = materialCols + `
  FROM materials m
  LEFT JOIN users u ON u.id = m.author_id
 WHERE ($1::text IS NULL OR m.category = $1)
   AND ($2::text IS NULL OR m.title ILIKE $2 OR m.description ILIKE $2)
 ORDER BY m.is_system DESC, CASE WHEN m.is_system THEN m.slug END, m.created_at DESC, m.id
 LIMIT $3`

const sqlGet = materialCols + `, m.content
  FROM materials m
  LEFT JOIN users u ON u.id = m.author_id
 WHERE m.id = $1`

type row struct {
	m                   public.Material
	last, first, middle string
}

func scanMaterial(dst *row, extra ...any) []any {
	return append([]any{&dst.m.Id, &dst.m.Title, &dst.m.Description, &dst.m.Category, &dst.m.FileName, &dst.m.MimeType,
		&dst.m.SizeBytes, &dst.m.System, &dst.m.AuthorId, &dst.last, &dst.first, &dst.middle, &dst.m.CreatedAt}, extra...)
}

// finish — вычисляемые поля: hasFile, authorName, UTC; пустое описание не отдаём.
func (r *row) finish() public.Material {
	m := r.m
	m.HasFile = m.FileName != nil
	m.CreatedAt = m.CreatedAt.UTC()
	if m.Description != nil && strings.TrimSpace(*m.Description) == "" {
		m.Description = nil
	}
	if m.AuthorId != nil {
		if n := core.ShortName(r.last, r.first, r.middle); n != "" {
			m.AuthorName = &n
		}
	}
	return m
}

// likePattern — подстрока для ILIKE с экранированием % _ \ (поиск по тексту, а не шаблону).
func likePattern(q string) *string {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil
	}
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
	p := "%" + r + "%"
	return &p
}

func nullIfEmpty(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

// GET /materials?category&q — метаданные (без content).
func (h *Handlers) list(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	needle := q.Get("q")
	if runes(strings.TrimSpace(needle)) > maxQueryRunes {
		return httpx.Validation("Слишком длинный поисковый запрос", map[string]string{"q": "Не длиннее 100 символов"})
	}
	rows, err := h.pool.Query(r.Context(), sqlList, nullIfEmpty(q.Get("category")), likePattern(needle), listCap)
	if err != nil {
		return fmt.Errorf("materials: list: %w", err)
	}
	defer rows.Close()
	out := make([]public.Material, 0, 16)
	for rows.Next() {
		var m row
		if err := rows.Scan(scanMaterial(&m)...); err != nil {
			return fmt.Errorf("materials: list: %w", err)
		}
		out = append(out, m.finish())
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("materials: list: %w", err)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

// GET /materials/{id} — материал с текстом.
func (h *Handlers) get(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "materialId")
	if err != nil {
		return errNotFound()
	}
	var m row
	var content *string
	if err := h.pool.QueryRow(r.Context(), sqlGet, id).Scan(scanMaterial(&m, &content)...); err != nil {
		if pg.IsNoRows(err) {
			return errNotFound()
		}
		return fmt.Errorf("materials: get: %w", err)
	}
	out := m.finish()
	out.Content = content
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

// ---------------------------------------------------------------- удаление

// sqlDelete — удаление с проверками в том же операторе (гонка «проверил → удалил» исключена):
// не системный и (админ, $2 NULL) либо автор. Не удалилось — причину выясняет sqlDiagnose.
const sqlDelete = `
DELETE FROM materials
 WHERE id = $1 AND NOT is_system AND ($2::uuid IS NULL OR author_id = $2)
RETURNING title, category, file_name, size_bytes`

const sqlDiagnose = `SELECT is_system, author_id FROM materials WHERE id = $1`

// DELETE /materials/{id} — автор-преподаватель или админ; системные — 409. Удаление
// физическое: материал — не результат обучения (Р14 про результаты), а 20 МБ файла в
// «мягко удалённой» строке жили бы вечно. След — в аудите material.delete.
func (h *Handlers) remove(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "materialId")
	if err != nil {
		return errNotFound()
	}
	ctx := r.Context()
	p := core.PrincipalFrom(ctx)
	var author *uuid.UUID
	if p.Role != core.RoleAdmin {
		author = &p.UserID
	}
	var (
		title, category string
		fileName        *string
		size            *int64
	)
	err = h.pool.QueryRow(ctx, sqlDelete, id, author).Scan(&title, &category, &fileName, &size)
	if pg.IsNoRows(err) {
		var (
			system bool
			owner  *uuid.UUID
		)
		switch derr := h.pool.QueryRow(ctx, sqlDiagnose, id).Scan(&system, &owner); {
		case pg.IsNoRows(derr):
			return errNotFound()
		case derr != nil:
			return fmt.Errorf("materials: delete diagnose: %w", derr)
		case system:
			return httpx.Conflict("Системный материал из поставки удалить нельзя")
		default:
			return httpx.Forbidden("Удалить материал может его автор или администратор")
		}
	}
	if err != nil {
		return fmt.Errorf("materials: delete: %w", err)
	}
	h.audit(ctx, "material.delete", id, map[string]any{"title": title, "category": category, "fileName": fileName, "sizeBytes": size}, nil)
	httpx.NoContent(w)
	return nil
}

// ---------------------------------------------------------------- создание

const sqlInsert = `
INSERT INTO materials (id, title, description, category, content, file_name, mime_type, size_bytes, file_data,
                       author_id, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11)`

// POST /materials (multipart/form-data): title, description, category, content, file.
func (h *Handlers) create(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	// Очередь на загрузку — до чтения тела: файл до 20 МБ держится в памяти до INSERT.
	select {
	case h.uploads <- struct{}{}:
		defer func() { <-h.uploads }()
	case <-ctx.Done():
		return ctx.Err()
	}
	in, err := readUpload(w, r)
	if err != nil {
		return err
	}
	p := core.PrincipalFrom(ctx)
	id := ids.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	m := public.Material{
		Id: id, Title: in.title, Category: in.category, HasFile: in.file != nil, AuthorId: &p.UserID, CreatedAt: now,
	}
	if in.description != "" {
		m.Description = &in.description
	}
	if n := p.ShortName(); n != "" {
		m.AuthorName = &n
	}
	var (
		content        *string
		fileName, mime *string
		size           *int64
		data           []byte
	)
	if in.content != "" {
		content = &in.content
	}
	if in.file != nil {
		fileName, mime, data = &in.file.name, &in.file.mime, in.file.data
		n := int64(len(data))
		size = &n
		m.FileName, m.MimeType, m.SizeBytes = fileName, mime, size
	}
	if _, err := h.pool.Exec(ctx, sqlInsert, id, in.title, in.description, in.category, content, fileName, mime, size,
		data, p.UserID, now); err != nil {
		return fmt.Errorf("materials: insert: %w", err)
	}
	h.audit(ctx, "material.create", id, nil, map[string]any{
		"title": in.title, "category": in.category, "fileName": fileName, "mimeType": mime, "sizeBytes": size,
		"hasContent": content != nil,
	})
	httpx.WriteJSON(w, http.StatusCreated, m)
	return nil
}

// ---------------------------------------------------------------- файл

const sqlFileMeta = `SELECT file_name, mime_type, size_bytes, updated_at FROM materials WHERE id = $1`

// GET /materials/{id}/file — файл материала. Content-Disposition: inline для PDF, аудио и
// изображений (просмотр в браузере), attachment для остального. Range поддерживается
// (перемотка аудио); тело читается из БД кусками по chunkSize.
func (h *Handlers) file(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "materialId")
	if err != nil {
		return errNotFound()
	}
	ctx := r.Context()
	var (
		name, mime *string
		size       *int64
		updated    time.Time
	)
	if err := h.pool.QueryRow(ctx, sqlFileMeta, id).Scan(&name, &mime, &size, &updated); err != nil {
		if pg.IsNoRows(err) {
			return errNotFound()
		}
		return fmt.Errorf("materials: file meta: %w", err)
	}
	if name == nil || mime == nil || size == nil {
		return httpx.NotFound("У материала нет файла")
	}
	hdr := w.Header()
	hdr.Set("Content-Type", *mime)
	disp := "attachment"
	if inlineMIME(*mime) {
		disp = "inline"
	}
	hdr.Set("Content-Disposition", contentDisposition(disp, *name))
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Cache-Control", "private, max-age=3600")
	hdr.Set("ETag", fmt.Sprintf(`"%s-%d"`, id, updated.UnixMicro()))
	http.ServeContent(w, r, "", updated, &dbReader{ctx: ctx, pool: h.pool, id: id, size: *size})
	return nil
}

// ---------------------------------------------------------------- helpers

// contentDisposition — inline|attachment с ASCII-именем и UTF-8 (RFC 5987) для кириллицы.
func contentDisposition(disp, name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' || r == ';' {
			return '_'
		}
		return r
	}, name)
	return disp + `; filename="` + ascii + `"; filename*=UTF-8''` + rfc5987(name)
}

func rfc5987(s string) string {
	const attrChar = "!#$&+-.^_`|~"
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte(attrChar, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0F])
	}
	return b.String()
}

// isTooLarge — тело запроса превысило http.MaxBytesReader.
func isTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}
