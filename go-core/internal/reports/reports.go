// Package reports — выгрузка результатов занятия (ТЗ: отчёты в Excel/PDF; frontend.v1.yaml
// v1.2, GET /lessons/{lessonId}/report?format=csv|xlsx|pdf[&tz=<IANA>]). Время в отчёте — в
// часовом поясе пользователя из tz (без него — UTC), в БД и API — абсолютные моменты UTC.
//
// Данные — один round-trip (шапка + все строки одним SQL в pgx.Batch), затем соединение
// отпускается, и отчёт собирается в памяти из компактных строк. CSV пишется потоково;
// XLSX/PDF библиотеки собирают целиком (zip/документ), поэтому ошибки сборки видны до первого
// байта ответа. Одновременных сборок — не больше MaxConcurrent: большой PDF/XLSX — это десятки
// мегабайт памяти, всплеск выгрузок не должен выдавить живые занятия.
package reports

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/access"
	"lct/gocore/internal/core"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/store"
)

// Deps — зависимости пакета (связывает internal/app).
type Deps struct {
	Pool    *pgxpool.Pool
	Catalog core.Catalog // зарезервировано: подписи уже лежат в field_errors, справочник не нужен
	Auditor core.Auditor
	Log     *slog.Logger

	// MaxConcurrent — одновременных сборок отчётов (0 — 2).
	MaxConcurrent int
}

// Handlers — GET /lessons/{lessonId}/report и GET /users/{userId}/certificate.
type Handlers struct {
	pool *pgxpool.Pool
	aud  core.Auditor
	log  *slog.Logger
	sem  chan struct{}
	now  func() time.Time
}

func New(d Deps) *Handlers {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	n := d.MaxConcurrent
	if n <= 0 {
		n = 2
	}
	return &Handlers{pool: d.Pool, aud: d.Auditor, log: log.With("component", "reports"),
		sem: make(chan struct{}, n), now: time.Now}
}

// Register — маршрут отчёта. Роль — преподаватель/админ; «своё ли занятие» — access.ManageLesson.
func (h *Handlers) Register(r *httpx.Router) {
	r.Handle("GET /lessons/{lessonId}/report", httpx.Roles(core.RoleTeacher, core.RoleAdmin), h.report)
	// v1.4: сертификат обучающегося (сам, преподаватель его групп/занятий, админ).
	r.Handle("GET /users/{userId}/certificate", httpx.Authenticated, h.certificate)
}

// Форматы и их MIME (контракт lessonReport).
var mimeTypes = map[string]string{
	"csv":  "text/csv; charset=utf-8",
	"xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"pdf":  "application/pdf",
}

func (h *Handlers) report(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	lessonID, err := httpx.PathUUID(r, "lessonId")
	if err != nil {
		return err
	}
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	mime, ok := mimeTypes[format]
	if !ok {
		return httpx.Validation("Укажите формат отчёта: csv, xlsx или pdf",
			map[string]string{"format": "ожидается csv, xlsx или pdf"})
	}
	loc, err := reportLocation(r.URL.Query().Get("tz"))
	if err != nil {
		return err
	}
	p := core.PrincipalFrom(ctx)

	// Очередь на сборку — до запроса к БД: ждущий не держит соединение пула.
	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	meta, rows, truncated, err := loadReport(ctx, h.pool, lessonID, func(m *lessonMeta) error {
		return access.ManageLesson(p, &store.LessonRow{ID: m.ID, TeacherID: m.TeacherID, CreatedBy: m.CreatedBy})
	})
	switch {
	case errors.Is(err, errLessonNotFound):
		return httpx.NotFound("Занятие не найдено")
	case err != nil:
		return err
	}
	now := h.now()
	rep := newReport(meta, rows, truncated, now, loc)

	// XLSX/PDF собираются до заголовков: ошибка сборки — обычный ApiError 500, а не битый файл.
	var (
		xlsx *bytes.Buffer
		doc  *fpdf.Fpdf
	)
	switch format {
	case "xlsx":
		if xlsx, err = buildXLSX(rep); err != nil {
			return err
		}
	case "pdf":
		if doc, err = buildPDF(rep); err != nil {
			return err
		}
	}

	ascii, utf := fileNames(meta, format, now, loc)
	hdr := w.Header()
	hdr.Set("Content-Type", mime)
	hdr.Set("Content-Disposition", contentDisposition(ascii, utf))
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Content-Type-Options", "nosniff")
	if xlsx != nil {
		hdr.Set("Content-Length", strconv.Itoa(xlsx.Len()))
	}
	w.WriteHeader(http.StatusOK)
	switch format {
	case "csv":
		err = writeCSV(w, rep) // потоково, буфер 32 КБ
	case "xlsx":
		_, err = xlsx.WriteTo(w)
	case "pdf":
		err = writePDF(w, doc)
	}
	if err != nil {
		// Заголовки уже ушли — ApiError не отправить; чаще всего клиент просто закрыл вкладку.
		h.log.Warn("отчёт не дописан", "lesson", lessonID, "format", format, "err", err)
	}
	h.record(ctx, p, lessonID, format, len(rows), truncated, err == nil)
	return nil
}

// maxTZLen — потолок длины параметра tz: самые длинные имена IANA — около 30 символов.
const maxTZLen = 64

// reportLocation — зона времени отчёта из параметра tz: имя IANA, которое браузер обучающегося
// или преподавателя определил сам (Intl.DateTimeFormat().resolvedOptions().timeZone), с правилами
// летнего времени из tzdata. Нет параметра — UTC: зона процесса go-core (time.Local) с
// пользователем не связана и в отчёт не попадает. «Local» — не зона IANA, а та же зона процесса.
func reportLocation(tz string) (*time.Location, error) {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		return time.UTC, nil
	}
	bad := httpx.Validation("Неизвестный часовой пояс отчёта",
		map[string]string{"tz": "ожидается имя часового пояса IANA"})
	if len(tz) > maxTZLen || tz == "Local" {
		return nil, bad
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, bad
	}
	return loc, nil
}

// record — журнал выгрузок: аудит report.export и строка reports (история отчётов занятия,
// db-design: «Отчёты CSV/Excel/PDF — reports»). Файл не хранится — отчёт собирается по запросу,
// поэтому file_path пуст. Ошибка записи в журнал отчёт не ломает (он уже отдан).
func (h *Handlers) record(ctx context.Context, p *core.Principal, lessonID uuid.UUID, format string,
	rows int, truncated, delivered bool) {
	after := map[string]any{"format": format, "rows": rows, "truncated": truncated, "delivered": delivered}
	if h.aud != nil {
		h.aud.Log(ctx, core.AuditEntry{
			Action:     "report.export",
			EntityType: "lesson",
			EntityID:   lessonID,
			LessonID:   lessonID,
			After:      after,
		})
	}
	status := "done"
	if !delivered {
		status = "failed"
	}
	// Клиент мог уже уйти — запись журнала не должна отменяться вместе с запросом.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if _, err := h.pool.Exec(wctx, `
		INSERT INTO reports (id, type, format, lesson_id, params, status, generated_by, finished_at)
		VALUES ($1, 'lesson', $2, $3, $4, $5, $6, now())`,
		ids.New(), format, lessonID, after, status, p.UserID); err != nil {
		h.log.Warn("журнал отчётов: запись не удалась", "lesson", lessonID, "err", err)
	}
}
