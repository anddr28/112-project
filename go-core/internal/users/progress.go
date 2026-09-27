package users

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/pg"
)

// GET /users/{userId}/progress — витрины student_progress / student_category_stats /
// student_field_errors + levels + recommendations + xp_ledger. Доступ проверяется одним
// запросом, данные читаются одним батчем (один round-trip на 6 выборок).

// Элементы анонимных структур public.StudentProgress (псевдонимы идентичных типов:
// если кодоген изменит схему, сборка упадёт здесь, а не молча разойдётся с контрактом).
type (
	progressLevel = struct {
		Badge     *string `json:"badge,omitempty"`
		NextTitle *string `json:"nextTitle,omitempty"`

		// NextXpRequired отсутствует на максимальном уровне
		NextXpRequired *int   `json:"nextXpRequired,omitempty"`
		No             int    `json:"no"`
		Title          string `json:"title"`
		XpRequired     *int   `json:"xpRequired,omitempty"`
	}
	progressCategory = struct {
		AttemptsDone  int        `json:"attemptsDone"`
		AvgScore      *float32   `json:"avgScore,omitempty"`
		AvgTimeMs     *int       `json:"avgTimeMs,omitempty"`
		CategoryId    string     `json:"categoryId"`
		CategoryName  string     `json:"categoryName"`
		LastAttemptAt *time.Time `json:"lastAttemptAt,omitempty"`
		PassRatePct   *float32   `json:"passRatePct,omitempty"`
	}
	progressFieldError = struct {
		Count int                                   `json:"count"`
		Field string                                `json:"field"`
		Kind  public.StudentProgressFieldErrorsKind `json:"kind"`
		Label string                                `json:"label"`
	}
	progressXP = struct {
		At        time.Time                         `json:"at"`
		AttemptId *uuid.UUID                        `json:"attemptId,omitempty"`
		Delta     int                               `json:"delta"`
		LessonId  *uuid.UUID                        `json:"lessonId,omitempty"`
		Reason    public.StudentProgressXpLogReason `json:"reason"`
	}
)

// progressAccessSQL — существует ли обучающийся и видит ли его преподаватель $2
// (свои группы, в т.ч. архивные — история, или участие в его занятиях). $2 NULL — админ/сам.
const progressAccessSQL = `
SELECT u.role, u.last_name, u.first_name, COALESCE(u.middle_name, ''),
       ($2::uuid IS NULL
        OR EXISTS (SELECT 1 FROM group_members gm JOIN groups g ON g.id = gm.group_id
                    WHERE gm.user_id = u.id AND g.teacher_id = $2)
        OR EXISTS (SELECT 1 FROM lesson_participants lp JOIN lessons l ON l.id = lp.lesson_id
                    WHERE lp.user_id = u.id AND l.teacher_id = $2))
  FROM users u
 WHERE u.id = $1 AND u.deleted_at IS NULL`

const (
	progressRowSQL = `
SELECT attempts_done, avg_score::float8, avg_score_30d::float8, pass_rate_pct::float8, avg_time_ms::float8,
       within_norm_count, xp, last_activity_at
  FROM student_progress
 WHERE user_id = $1`

	levelsSQL = `SELECT no, title, xp_required, COALESCE(badge, '') FROM levels ORDER BY xp_required`

	categoriesSQL = `
SELECT c.category_id, COALESCE(cc.name, ''), c.attempts_done, c.avg_score::float8, c.pass_rate_pct::float8,
       c.avg_time_ms::float8, c.last_attempt_at
  FROM student_category_stats c
  LEFT JOIN classifier_categories cc ON cc.id = c.category_id
 WHERE c.user_id = $1 AND c.attempts_done > 0
 ORDER BY c.last_attempt_at DESC NULLS LAST, c.category_id`

	// Ошибки по полям — суммарно по всем категориям, топ-20 по частоте.
	fieldErrorsSQL = `
SELECT field, kind, sum(cnt)::int AS n
  FROM student_field_errors
 WHERE user_id = $1 AND field IS NOT NULL AND kind IN ('missing', 'wrong', 'extra')
 GROUP BY field, kind
 ORDER BY n DESC, field, kind
 LIMIT 20`

	storedRecsSQL = `
SELECT id, kind, body,
       CASE WHEN jsonb_typeof(evidence -> 'items') = 'array'
            THEN ARRAY(SELECT jsonb_array_elements_text(evidence -> 'items')) END
  FROM recommendations
 WHERE scope = 'student' AND user_id = $1 AND dismissed_at IS NULL
 ORDER BY created_at DESC
 LIMIT 20`

	xpLogSQL = `
SELECT delta, reason, created_at, attempt_id, lesson_id
  FROM xp_ledger
 WHERE user_id = $1
 ORDER BY created_at DESC, id DESC
 LIMIT 50`
)

func (h *Handlers) progress(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	id, err := httpx.PathUUID(r, "userId")
	if err != nil {
		return httpx.NotFound("Обучающийся не найден")
	}
	var teacher *uuid.UUID
	switch p.Role {
	case core.RoleStudent:
		if id != p.UserID {
			return httpx.Forbidden("Можно смотреть только свой прогресс")
		}
	case core.RoleTeacher:
		teacher = &p.UserID
	}

	var (
		role, last, first, middle string
		allowed                   bool
	)
	if err := h.pool.QueryRow(ctx, progressAccessSQL, id, teacher).Scan(&role, &last, &first, &middle, &allowed); err != nil {
		if pg.IsNoRows(err) {
			return httpx.NotFound("Обучающийся не найден")
		}
		return httpx.Internal(fmt.Errorf("users: progress access: %w", err))
	}
	if role != string(core.RoleStudent) {
		return httpx.NotFound("Обучающийся не найден")
	}
	if !allowed {
		return httpx.Forbidden("Обучающийся не состоит в ваших группах и не участвовал в ваших занятиях")
	}

	data, err := loadProgress(ctx, h.pool, id)
	if err != nil {
		return httpx.Internal(err)
	}
	threshold := 70.0
	if h.settings != nil {
		threshold = h.settings.Get(ctx).PassThreshold
	}
	label := func(field string) string { return field }
	if h.catalog != nil {
		label = h.catalog.FieldLabel
	}
	name := strings.Join(nonBlank(last, first, middle), " ")
	httpx.WriteJSON(w, http.StatusOK, buildProgress(id, name, data, threshold, label))
	return nil
}

// ---------------------------------------------------------------- чтение

type progressData struct {
	attemptsDone, withinNorm, xp int
	avgScore, avgScore30d        *float64
	passRate, avgTimeMs          *float64
	lastActivity                 *time.Time
	levels                       []levelRow
	categories                   []categoryRow
	fieldErrors                  []fieldErrorRow
	stored                       []public.Recommendation
	xpLog                        []progressXP
}

type levelRow struct {
	no, xpRequired int
	title, badge   string
}

type categoryRow struct {
	id, name           string
	done               int
	avg, pass, avgTime *float64
	last               *time.Time
}

type fieldErrorRow struct {
	field, kind string
	count       int
}

func loadProgress(ctx context.Context, q pg.Querier, userID uuid.UUID) (*progressData, error) {
	b := &pgx.Batch{}
	b.Queue(progressRowSQL, userID)
	b.Queue(levelsSQL)
	b.Queue(categoriesSQL, userID)
	b.Queue(fieldErrorsSQL, userID)
	b.Queue(storedRecsSQL, userID)
	b.Queue(xpLogSQL, userID)
	br := q.SendBatch(ctx, b)
	defer br.Close()

	d := &progressData{}
	var done, within int64
	var xp int32
	err := br.QueryRow().Scan(&done, &d.avgScore, &d.avgScore30d, &d.passRate, &d.avgTimeMs, &within, &xp, &d.lastActivity)
	switch {
	case err == nil:
		d.attemptsDone, d.withinNorm, d.xp = int(done), int(within), int(xp)
	case pg.IsNoRows(err):
		// Строки нет (пользователь только что удалён/сменил роль) — нули.
	default:
		return nil, fmt.Errorf("users: progress row: %w", err)
	}

	if err := scanRows(br, func(rows pgx.Rows) error {
		var l levelRow
		var no int16
		var req int32
		if err := rows.Scan(&no, &l.title, &req, &l.badge); err != nil {
			return err
		}
		l.no, l.xpRequired = int(no), int(req)
		d.levels = append(d.levels, l)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("users: levels: %w", err)
	}

	if err := scanRows(br, func(rows pgx.Rows) error {
		var c categoryRow
		var cid uuid.UUID
		var n int64
		if err := rows.Scan(&cid, &c.name, &n, &c.avg, &c.pass, &c.avgTime, &c.last); err != nil {
			return err
		}
		c.id, c.done = cid.String(), int(n)
		d.categories = append(d.categories, c)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("users: categories: %w", err)
	}

	if err := scanRows(br, func(rows pgx.Rows) error {
		var f fieldErrorRow
		var n int32
		if err := rows.Scan(&f.field, &f.kind, &n); err != nil {
			return err
		}
		f.count = int(n)
		d.fieldErrors = append(d.fieldErrors, f)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("users: field errors: %w", err)
	}

	if err := scanRows(br, func(rows pgx.Rows) error {
		var (
			rid   uuid.UUID
			kind  string
			rec   public.Recommendation
			items []string
		)
		if err := rows.Scan(&rid, &kind, &rec.Body, &items); err != nil {
			return err
		}
		rec.Id, rec.Kind = rid.String(), public.RecommendationKind(kind)
		if len(items) > 0 {
			rec.Items = &items
		}
		d.stored = append(d.stored, rec)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("users: recommendations: %w", err)
	}

	d.xpLog = make([]progressXP, 0, 16)
	if err := scanRows(br, func(rows pgx.Rows) error {
		var (
			x      progressXP
			delta  int32
			reason string
		)
		if err := rows.Scan(&delta, &reason, &x.At, &x.AttemptId, &x.LessonId); err != nil {
			return err
		}
		x.Delta, x.Reason, x.At = int(delta), public.StudentProgressXpLogReason(reason), x.At.UTC()
		d.xpLog = append(d.xpLog, x)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("users: xp log: %w", err)
	}
	return d, nil
}

// scanRows — очередной результат батча построчно.
func scanRows(br pgx.BatchResults, fn func(pgx.Rows) error) error {
	rows, err := br.Query()
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ---------------------------------------------------------------- сборка (чистая логика)

func buildProgress(id uuid.UUID, name string, d *progressData, threshold float64, label func(string) string) public.StudentProgress {
	sp := public.StudentProgress{
		UserId:          id,
		Name:            name,
		Xp:              d.xp,
		AttemptsDone:    d.attemptsDone,
		AvgScore:        f32(d.avgScore),
		AvgScore30d:     f32(d.avgScore30d),
		PassRatePct:     f32(d.passRate),
		AvgTimeMs:       intPtr(d.avgTimeMs),
		WithinNormCount: &d.withinNorm,
		LastActivityAt:  utcPtr(d.lastActivity),
		Level:           levelFor(d.levels, d.xp),
		Categories:      make([]progressCategory, 0, len(d.categories)),
		FieldErrors:     make([]progressFieldError, 0, len(d.fieldErrors)),
		XpLog:           d.xpLog,
	}
	if sp.XpLog == nil {
		sp.XpLog = []progressXP{}
	}
	for _, c := range d.categories {
		sp.Categories = append(sp.Categories, progressCategory{
			CategoryId:    c.id,
			CategoryName:  c.name,
			AttemptsDone:  c.done,
			AvgScore:      f32(c.avg),
			PassRatePct:   f32(c.pass),
			AvgTimeMs:     intPtr(c.avgTime),
			LastAttemptAt: utcPtr(c.last),
		})
	}
	labels := make(map[string]string, len(d.fieldErrors))
	for _, f := range d.fieldErrors {
		l, ok := labels[f.field]
		if !ok {
			l = label(f.field)
			labels[f.field] = l
		}
		sp.FieldErrors = append(sp.FieldErrors, progressFieldError{
			Field: f.field, Label: l, Kind: public.StudentProgressFieldErrorsKind(f.kind), Count: f.count,
		})
	}

	sys := systemRecommendations(recInput{
		threshold:    threshold,
		categories:   d.categories,
		fieldErrors:  sp.FieldErrors,
		attemptsDone: d.attemptsDone,
		withinNorm:   d.withinNorm,
		avgTimeMs:    sp.AvgTimeMs,
	})
	sp.Recommendations = make([]public.Recommendation, 0, len(d.stored)+len(sys))
	sp.Recommendations = append(sp.Recommendations, d.stored...)
	sp.Recommendations = append(sp.Recommendations, sys...)
	return sp
}

// levelFor — текущий уровень (max xp_required ≤ xp) и следующий. levels отсортированы по xp_required.
func levelFor(levels []levelRow, xp int) progressLevel {
	cur, next := -1, -1
	for i, l := range levels {
		if l.xpRequired <= xp {
			cur = i
		} else {
			next = i
			break
		}
	}
	var lv progressLevel
	if cur >= 0 {
		c := levels[cur]
		req := c.xpRequired
		lv.No, lv.Title, lv.XpRequired = c.no, c.title, &req
		if c.badge != "" {
			b := c.badge
			lv.Badge = &b
		}
	} else {
		// Справочник пуст или начинается не с нуля — «до первого уровня».
		lv.No, lv.Title = 0, "Без уровня"
	}
	if next >= 0 {
		n := levels[next]
		t, req := n.title, n.xpRequired
		lv.NextTitle, lv.NextXpRequired = &t, &req
	}
	return lv
}

type recInput struct {
	threshold    float64
	categories   []categoryRow
	fieldErrors  []progressFieldError // отсортированы по убыванию count
	attemptsDone int
	withinNorm   int
	avgTimeMs    *int
}

// Детерминированные id системных рекомендаций: фронт может помнить «скрытые» и не мигать списком.
const (
	recWeakCategoryID = "sys-weak-category"
	recWeakFieldID    = "sys-weak-field"
	recSlowTimingID   = "sys-slow-timing"

	minAttemptsForRec = 2
	topWeakFields     = 3
)

// systemRecommendations — рекомендации, вычисленные по витринам (не хранятся в БД):
// слабые категории (средний балл ниже порога при ≥2 попытках), повторяющиеся ошибки полей
// (топ-3, встречались ≥2 раз), медленная работа (в норматив уложились меньше чем в половине).
func systemRecommendations(in recInput) []public.Recommendation {
	out := make([]public.Recommendation, 0, 3)

	type weak struct {
		name string
		avg  float64
		n    int
	}
	var weakCats []weak
	for _, c := range in.categories {
		if c.done >= minAttemptsForRec && c.avg != nil && *c.avg < in.threshold {
			name := c.name
			if name == "" {
				name = "Категория без названия"
			}
			weakCats = append(weakCats, weak{name: name, avg: *c.avg, n: c.done})
		}
	}
	if len(weakCats) > 0 {
		sort.SliceStable(weakCats, func(i, j int) bool { return weakCats[i].avg < weakCats[j].avg })
		items := make([]string, 0, len(weakCats))
		for _, w := range weakCats {
			items = append(items, fmt.Sprintf("«%s» — средний балл %s, %d %s", w.name, fmtNum(w.avg), w.n,
				plural(w.n, "попытка", "попытки", "попыток")))
		}
		out = append(out, public.Recommendation{
			Id:   recWeakCategoryID,
			Kind: public.WeakCategory,
			Body: fmt.Sprintf("Средний балл ниже порога зачёта (%s) в категориях — повторите опросные карты и эталонные действия:",
				fmtNum(in.threshold)),
			Items: &items,
		})
	}

	items := make([]string, 0, topWeakFields)
	for _, f := range in.fieldErrors {
		if len(items) == topWeakFields {
			break
		}
		if f.Count < minAttemptsForRec {
			continue
		}
		items = append(items, fmt.Sprintf("%s — %s (%d %s)", f.Label, kindText(string(f.Kind)), f.Count,
			plural(f.Count, "раз", "раза", "раз")))
	}
	if len(items) > 0 {
		out = append(out, public.Recommendation{
			Id:    recWeakFieldID,
			Kind:  public.WeakField,
			Body:  "Повторяющиеся ошибки в карточке — проверяйте эти поля перед сохранением:",
			Items: &items,
		})
	}

	if in.attemptsDone >= minAttemptsForRec && in.withinNorm*2 < in.attemptsDone {
		body := fmt.Sprintf("В норматив уложились %d из %d %s.", in.withinNorm, in.attemptsDone,
			plural(in.attemptsDone, "карточки", "карточек", "карточек"))
		if in.avgTimeMs != nil && *in.avgTimeMs > 0 {
			body += " Среднее время заполнения — " + fmtDuration(*in.avgTimeMs) + "."
		}
		body += " Начинайте вводить адрес и тип происшествия одновременно с разговором, не дожидаясь конца обращения."
		out = append(out, public.Recommendation{Id: recSlowTimingID, Kind: public.SlowTiming, Body: body})
	}
	return out
}

func kindText(kind string) string {
	switch kind {
	case "missing":
		return "не заполнено"
	case "wrong":
		return "не совпадает с эталоном"
	case "extra":
		return "лишнее значение"
	}
	return kind
}

// plural — русское склонение по числу: 1 попытка, 2 попытки, 5 попыток.
func plural(n int, one, few, many string) string {
	n = n % 100
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

// fmtNum — число с одним знаком после запятой по-русски ("54,5", "70").
func fmtNum(v float64) string {
	v = math.Round(v*10) / 10
	return strings.Replace(strconv.FormatFloat(v, 'f', -1, 64), ".", ",", 1)
}

// fmtDuration — «42 с» / «1 мин 05 с».
func fmtDuration(ms int) string {
	sec := int(math.Round(float64(ms) / 1000))
	if sec < 60 {
		return strconv.Itoa(sec) + " с"
	}
	return fmt.Sprintf("%d мин %02d с", sec/60, sec%60)
}

func f32(v *float64) *float32 {
	if v == nil {
		return nil
	}
	f := float32(*v)
	return &f
}

func intPtr(v *float64) *int {
	if v == nil {
		return nil
	}
	n := int(math.Round(*v))
	return &n
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func nonBlank(parts ...string) []string {
	out := parts[:0:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
