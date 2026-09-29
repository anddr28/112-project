package reports

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"

	"lct/gocore/internal/core"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
)

// PDF-сертификат о прохождении подготовки (контракт v1.4, GET /users/{id}/certificate; ТЗ:
// «PDF для сертификатов»). Те же шрифты и помощники, что у отчёта по занятию (кириллица —
// goregular/gobold). Данные — витрины прогресса (student_progress, student_category_stats) и
// levels одним batch; сертификат выдаётся после хотя бы одной оценённой карточки (иначе 409).

// certAccessSQL — обучающийся существует и виден преподавателю $2 (его группы или участие в
// его занятиях); $2 NULL — админ или сам обучающийся. Тот же предикат, что у прогресса.
const certAccessSQL = `
SELECT u.role, u.last_name, u.first_name, COALESCE(u.middle_name, ''),
       ($2::uuid IS NULL
        OR EXISTS (SELECT 1 FROM group_members gm JOIN groups g ON g.id = gm.group_id
                    WHERE gm.user_id = u.id AND g.teacher_id = $2)
        OR EXISTS (SELECT 1 FROM lesson_participants lp JOIN lessons l ON l.id = lp.lesson_id
                    WHERE lp.user_id = u.id AND l.teacher_id = $2))
  FROM users u
 WHERE u.id = $1 AND u.deleted_at IS NULL`

const certProgressSQL = `
SELECT p.attempts_done::int, p.avg_score::float8, p.pass_rate_pct::float8, p.avg_time_ms::float8,
       p.within_norm_count::int, p.xp,
       (SELECT min(a.submitted_at) FROM attempts a WHERE a.user_id = p.user_id AND a.status = 'evaluated'),
       (SELECT max(a.submitted_at) FROM attempts a WHERE a.user_id = p.user_id AND a.status = 'evaluated'),
       (SELECT count(DISTINCT a.lesson_id)::int FROM attempts a WHERE a.user_id = p.user_id AND a.status = 'evaluated')
  FROM student_progress p
 WHERE p.user_id = $1`

// certLevelSQL — текущий уровень по XP обучающегося (нет строки — уровня нет).
const certLevelSQL = `
SELECT l.title, l.no::int
  FROM levels l
 WHERE l.xp_required <= COALESCE((SELECT xp FROM student_progress WHERE user_id = $1), 0)
 ORDER BY l.xp_required DESC
 LIMIT 1`

const certCategoriesSQL = `
SELECT COALESCE(cc.name, ''), c.attempts_done::int, c.avg_score::float8, c.pass_rate_pct::float8
  FROM student_category_stats c
  LEFT JOIN classifier_categories cc ON cc.id = c.category_id
 WHERE c.user_id = $1 AND c.attempts_done > 0
 ORDER BY c.attempts_done DESC, cc.name
 LIMIT 12`

// certData — всё, что печатается в сертификате.
type certData struct {
	UserID             uuid.UUID
	Last, First, Mid   string
	Attempts           int
	AvgScore, PassRate *float64
	AvgTimeMs          *float64
	WithinNorm, XP     int
	Lessons            int
	First_, LastAt     *time.Time
	LevelTitle         *string
	LevelNo            *int
	Categories         []certCategory
}

type certCategory struct {
	Name          string
	Attempts      int
	Avg, PassRate *float64
}

func (d *certData) fullName() string {
	return strings.TrimSpace(strings.Join(nonEmpty(d.Last, d.First, d.Mid), " "))
}

func nonEmpty(parts ...string) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// certificate — GET /users/{userId}/certificate?tz=…
func (h *Handlers) certificate(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	id, err := httpx.PathUUID(r, "userId")
	if err != nil {
		return httpx.NotFound("Обучающийся не найден")
	}
	loc, err := reportLocation(r.URL.Query().Get("tz"))
	if err != nil {
		return err
	}
	p := core.PrincipalFrom(ctx)
	var teacher *uuid.UUID
	switch p.Role {
	case core.RoleStudent:
		if id != p.UserID {
			return httpx.Forbidden("Сертификат можно получить только свой")
		}
	case core.RoleTeacher:
		teacher = &p.UserID
	}

	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	d, err := loadCertificate(ctx, h.pool, id, teacher)
	if err != nil {
		return err
	}
	now := h.now()
	doc, err := buildCertificate(d, now, loc)
	if err != nil {
		return err
	}
	date := now.In(loc).Format("2006-01-02")
	ascii := "certificate-" + date + ".pdf"
	utf := "Сертификат — " + safeFileTitle(d.fullName()) + " — " + date + ".pdf"
	hdr := w.Header()
	hdr.Set("Content-Type", "application/pdf")
	hdr.Set("Content-Disposition", contentDisposition(ascii, utf))
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	werr := writePDF(w, doc)
	if werr != nil {
		h.log.Warn("сертификат не дописан", "user", id, "err", werr)
	}
	h.recordCertificate(ctx, p, id, d, werr == nil)
	return nil
}

// loadCertificate — доступ и данные одним batch (один round-trip). Ошибки — контрактные.
func loadCertificate(ctx context.Context, q pg.Querier, id uuid.UUID, teacher *uuid.UUID) (*certData, error) {
	b := &pgx.Batch{}
	b.Queue(certAccessSQL, id, teacher)
	b.Queue(certProgressSQL, id)
	b.Queue(certCategoriesSQL, id)
	b.Queue(certLevelSQL, id)
	br := q.SendBatch(ctx, b)
	defer br.Close()

	d := &certData{UserID: id}
	var (
		role    string
		allowed bool
	)
	if err := br.QueryRow().Scan(&role, &d.Last, &d.First, &d.Mid, &allowed); err != nil {
		if pg.IsNoRows(err) {
			return nil, httpx.NotFound("Обучающийся не найден")
		}
		return nil, fmt.Errorf("reports: certificate access: %w", err)
	}
	if role != string(core.RoleStudent) {
		return nil, httpx.NotFound("Обучающийся не найден")
	}
	if !allowed {
		return nil, httpx.Forbidden("Обучающийся не состоит в ваших группах и не участвовал в ваших занятиях")
	}
	err := br.QueryRow().Scan(&d.Attempts, &d.AvgScore, &d.PassRate, &d.AvgTimeMs, &d.WithinNorm, &d.XP,
		&d.First_, &d.LastAt, &d.Lessons)
	if err != nil && !pg.IsNoRows(err) {
		return nil, fmt.Errorf("reports: certificate progress: %w", err)
	}
	if d.Attempts == 0 {
		return nil, httpx.Conflict("Сертификат выдаётся после первой оценённой карточки — оценённых попыток пока нет")
	}
	rows, err := br.Query()
	if err != nil {
		return nil, fmt.Errorf("reports: certificate categories: %w", err)
	}
	for rows.Next() {
		var c certCategory
		if err := rows.Scan(&c.Name, &c.Attempts, &c.Avg, &c.PassRate); err != nil {
			rows.Close()
			return nil, fmt.Errorf("reports: certificate categories: %w", err)
		}
		d.Categories = append(d.Categories, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reports: certificate categories: %w", err)
	}
	if err := br.QueryRow().Scan(&d.LevelTitle, &d.LevelNo); err != nil && !pg.IsNoRows(err) {
		return nil, fmt.Errorf("reports: certificate level: %w", err)
	}
	return d, nil
}

// recordCertificate — аудит certificate.export и строка reports (type student, pdf).
func (h *Handlers) recordCertificate(ctx context.Context, p *core.Principal, userID uuid.UUID, d *certData, delivered bool) {
	after := map[string]any{"attempts": d.Attempts, "delivered": delivered}
	if h.aud != nil {
		h.aud.Log(ctx, core.AuditEntry{Action: "certificate.export", EntityType: "user", EntityID: userID, After: after})
	}
	status := "done"
	if !delivered {
		status = "failed"
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if _, err := h.pool.Exec(wctx, `
		INSERT INTO reports (id, type, format, user_id, params, status, generated_by, finished_at)
		VALUES ($1, 'student', 'pdf', $2, $3, $4, $5, now())`,
		ids.New(), userID, after, status, p.UserID); err != nil {
		h.log.Warn("журнал отчётов: сертификат не записан", "user", userID, "err", err)
	}
}

// ---------------------------------------------------------------- PDF

// Лист A4 альбомный; двойная рамка, центрированный текст, плитки показателей, таблица категорий.
const (
	certMargin = 12.0
	certInner  = 4.0
)

var (
	cAccent = rgb{24, 64, 120}
	cGold   = rgb{176, 141, 60}
)

// buildCertificate — документ целиком в памяти (ошибки сборки — до первого байта ответа).
func buildCertificate(d *certData, now time.Time, loc *time.Location) (*fpdf.Fpdf, error) {
	pdf := fpdf.New("L", "mm", "A4", "")
	pdf.AddUTF8FontFromBytes(pdfFont, "", goregular.TTF)
	pdf.AddUTF8FontFromBytes(pdfFont, "B", gobold.TTF)
	pdf.SetMargins(certMargin+certInner+6, certMargin+certInner+4, certMargin+certInner+6)
	pdf.SetAutoPageBreak(false, 0)
	pdf.SetTitle(pdfText("Сертификат — "+d.fullName()), true)
	pdf.SetCreator(docCreator, true)
	pdf.SetCreationDate(now)
	pdf.SetModificationDate(now)
	pdf.AddPage()

	// рамка
	setDraw(pdf, cAccent)
	pdf.SetLineWidth(1.2)
	pdf.Rect(certMargin, certMargin, pageW-2*certMargin, pageH-2*certMargin, "D")
	setDraw(pdf, cGold)
	pdf.SetLineWidth(0.4)
	pdf.Rect(certMargin+certInner, certMargin+certInner, pageW-2*(certMargin+certInner), pageH-2*(certMargin+certInner), "D")
	pdf.SetLineWidth(0.2)

	w := pageW - 2*(certMargin+certInner+6)
	x0 := certMargin + certInner + 6
	pdf.SetXY(x0, certMargin+certInner+8)

	setColor(pdf, cMuted)
	pdf.SetFont(pdfFont, "", 9)
	pdf.CellFormat(w, 5, "Тренажёр операторов системы-112 и дежурно-диспетчерских служб (АРМ-112)", "", 1, "C", false, 0, "")
	pdf.Ln(4)
	setColor(pdf, cAccent)
	pdf.SetFont(pdfFont, "B", 34)
	pdf.CellFormat(w, 14, "СЕРТИФИКАТ", "", 1, "C", false, 0, "")
	setColor(pdf, cText)
	pdf.SetFont(pdfFont, "", 12)
	pdf.CellFormat(w, 7, "о прохождении подготовки на тренажёре", "", 1, "C", false, 0, "")
	pdf.Ln(5)
	pdf.SetFont(pdfFont, "", 11)
	setColor(pdf, cMuted)
	pdf.CellFormat(w, 6, "Настоящим подтверждается, что", "", 1, "C", false, 0, "")
	pdf.Ln(1)
	setColor(pdf, cText)
	pdf.SetFont(pdfFont, "B", 22)
	pdf.CellFormat(w, 11, pdfText(truncRunes(d.fullName(), 60)), "", 1, "C", false, 0, "")
	setDraw(pdf, cGold)
	pdf.Line(x0+w/2-60, pdf.GetY()+1, x0+w/2+60, pdf.GetY()+1)
	pdf.Ln(4)
	pdf.SetFont(pdfFont, "", 11)
	setColor(pdf, cText)
	pdf.MultiCell(w, 5.5, pdfText(certSentence(d, loc)), "", "C", false)
	pdf.Ln(4)

	certTiles(pdf, d, x0, w)
	certCategoriesTable(pdf, d, x0, w)

	// подпись и дата
	y := pageH - certMargin - certInner - 22
	setColor(pdf, cText)
	pdf.SetFont(pdfFont, "", 10)
	pdf.SetXY(x0, y)
	pdf.CellFormat(w/2, 5, "Дата выдачи: "+now.In(loc).Format("02.01.2006"), "", 0, "L", false, 0, "")
	pdf.CellFormat(w/2, 5, "Преподаватель: ______________________", "", 1, "R", false, 0, "")
	pdf.SetXY(x0, y+7)
	setColor(pdf, cMuted)
	pdf.SetFont(pdfFont, "", 7.5)
	pdf.CellFormat(w, 4, pdfText("№ "+certNumber(d.UserID, now, loc)+" · документ сформирован автоматически по результатам "+
		"оценки тренажёра · "+docCreator), "", 1, "L", false, 0, "")

	if err := pdf.Error(); err != nil {
		return nil, fmt.Errorf("reports: certificate pdf: %w", err)
	}
	return pdf, nil
}

// certSentence — «прошёл(-ла) подготовку … в период с … по …, выполнив N карточек в M занятиях».
func certSentence(d *certData, loc *time.Location) string {
	s := "прошёл(-ла) подготовку по приёму и обработке вызовов по единому номеру «112» и работе с карточками " +
		"происшествий на АРМ-112"
	if d.First_ != nil && d.LastAt != nil {
		from, to := d.First_.In(loc).Format("02.01.2006"), d.LastAt.In(loc).Format("02.01.2006")
		if from == to {
			s += " (" + from + ")"
		} else {
			s += " в период с " + from + " по " + to
		}
	}
	s += fmt.Sprintf(", выполнив и сдав на оценку %d %s", d.Attempts, pluralRu(d.Attempts, "карточку", "карточки", "карточек"))
	if d.Lessons > 0 {
		s += fmt.Sprintf(" в %d %s", d.Lessons, pluralRu(d.Lessons, "занятии", "занятиях", "занятиях"))
	}
	return s + "."
}

// certTiles — пять плиток показателей.
func certTiles(pdf *fpdf.Fpdf, d *certData, x0, w float64) {
	level := "—"
	if d.LevelTitle != nil {
		level = *d.LevelTitle
		if d.LevelNo != nil {
			level = strconv.Itoa(*d.LevelNo) + " · " + level
		}
	}
	within := "—"
	if d.Attempts > 0 {
		within = strconv.Itoa(int(math.Round(100*float64(d.WithinNorm)/float64(d.Attempts)))) + " %"
	}
	tiles := []struct{ label, value string }{
		{"Уровень", level},
		{"Опыт (XP)", strconv.Itoa(d.XP)},
		{"Оценено карточек", strconv.Itoa(d.Attempts)},
		{"Средний балл", fmtScore(d.AvgScore)},
		{"Доля зачётов", fmtPercent(d.PassRate)},
		{"В нормативе времени", within},
	}
	const gap, th = 3.0, 15.0
	tw := (w - gap*float64(len(tiles)-1)) / float64(len(tiles))
	y := pdf.GetY()
	for i, t := range tiles {
		x := x0 + float64(i)*(tw+gap)
		setFill(pdf, cZebra)
		setDraw(pdf, cBorder)
		pdf.Rect(x, y, tw, th, "FD")
		pdf.SetXY(x, y+2)
		pdf.SetFont(pdfFont, "", 7.5)
		setColor(pdf, cMuted)
		pdf.CellFormat(tw, 4, t.label, "", 0, "C", false, 0, "")
		pdf.SetXY(x, y+7)
		pdf.SetFont(pdfFont, "B", 11)
		setColor(pdf, cText)
		pdf.CellFormat(tw, 6, pdfText(truncRunes(t.value, 24)), "", 0, "C", false, 0, "")
	}
	pdf.SetXY(x0, y+th+5)
}

// certCategoriesTable — результаты по категориям происшествий (до 6 строк — лист один).
func certCategoriesTable(pdf *fpdf.Fpdf, d *certData, x0, w float64) {
	if len(d.Categories) == 0 {
		return
	}
	cats := d.Categories
	if len(cats) > 6 {
		cats = cats[:6]
	}
	cols := []struct {
		title string
		w     float64
		align string
	}{{"Категория происшествий", w * 0.55, "L"}, {"Карточек", w * 0.15, "C"}, {"Средний балл", w * 0.15, "C"}, {"Зачёт", w * 0.15, "C"}}
	const rh = 5.6
	pdf.SetFont(pdfFont, "B", 8.5)
	setFill(pdf, cHead)
	setDraw(pdf, cBorder)
	setColor(pdf, cText)
	pdf.SetX(x0)
	for _, c := range cols {
		pdf.CellFormat(c.w, rh, c.title, "1", 0, c.align, true, 0, "")
	}
	pdf.Ln(rh)
	pdf.SetFont(pdfFont, "", 8.5)
	for i, c := range cats {
		fill := i%2 == 1
		setFill(pdf, cZebra)
		pdf.SetX(x0)
		vals := []string{pdfText(truncRunes(orText(c.Name, "Без названия"), 70)), strconv.Itoa(c.Attempts), fmtScore(c.Avg), fmtPercent(c.PassRate)}
		for j, col := range cols {
			pdf.CellFormat(col.w, rh, vals[j], "1", 0, col.align, fill, 0, "")
		}
		pdf.Ln(rh)
	}
}

// certNumber — номер сертификата: дата выдачи и начало id обучающегося (проверяемо по журналу отчётов).
func certNumber(id uuid.UUID, now time.Time, loc *time.Location) string {
	return "С-" + now.In(loc).Format("20060102") + "-" + strings.ToUpper(strings.ReplaceAll(id.String(), "-", "")[:8])
}

func pluralRu(n int, one, few, many string) string {
	n %= 100
	if n >= 11 && n <= 14 {
		return many
	}
	switch n % 10 {
	case 1:
		return one
	case 2, 3, 4:
		return few
	}
	return many
}
