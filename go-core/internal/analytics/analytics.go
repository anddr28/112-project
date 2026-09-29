// Package analytics — аналитика группы для преподавателя (контракт v1.4, GET
// /analytics/overview; ТЗ: «аналитические рекомендации», «тепловые карты ошибок», «инсайты
// по типичным ошибкам группы»).
//
// Считается по оценённым попыткам (attempts.status = evaluated) занятий преподавателя
// (админ — всех занятий), с фильтрами занятие / обучающийся / категория / период.
//
// Производительность (DESIGN §1, §6): все разделы — агрегаты в PostgreSQL, один запрос на
// раздел, все запросы одним pgx.Batch (один round-trip). Тяжёлые jsonb (field_errors,
// semantic, grammar_remarks, dialogue) разворачиваются только в своих разделах, в Go едут
// уже сгруппированные строки (десятки, не тысячи). Окно по времени — частичный индекс
// attempts_evaluated_idx (миграция 00005), занятия преподавателя — lessons_teacher_idx.
//
// Инсайты — детерминированные правила над агрегатами (insights.go): одинаковые данные —
// одинаковые выводы, каждый вывод объясним (метрика, доля, список обучающихся) и несёт
// конкретную рекомендацию преподавателю. LLM здесь не нужен и не используется.
package analytics

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/core"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/settings"
)

// Deps — зависимости пакета (связывает internal/app).
type Deps struct {
	Pool     *pgxpool.Pool
	Settings *settings.Store // порог зачёта платформы (слабые категории)
	Catalog  core.Catalog    // категория по коду, подписи полей
	Log      *slog.Logger
}

// Handlers — GET /analytics/overview.
type Handlers struct {
	pool *pgxpool.Pool
	st   *settings.Store
	cat  core.Catalog
	log  *slog.Logger
	now  func() time.Time
}

func New(d Deps) *Handlers {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	return &Handlers{pool: d.Pool, st: d.Settings, cat: d.Catalog, log: log.With("component", "analytics"), now: time.Now}
}

func (h *Handlers) Register(r *httpx.Router) {
	r.Handle("GET /analytics/overview", httpx.Roles(core.RoleTeacher, core.RoleAdmin), h.overview)
}

// Период по умолчанию и границы (контракт: days 1..365, по умолчанию 30).
const (
	defaultDays = 30
	maxDays     = 365
)

// scope — проверенные фильтры запроса.
type scope struct {
	teacher    *uuid.UUID // nil — админ (все занятия)
	lesson     *uuid.UUID
	student    *uuid.UUID
	category   *uuid.UUID
	categoryIn string // как прислали (для scope.categoryId в ответе)
	days       int
	since      *time.Time // nil — без окна (выбрано занятие, days не задан)
}

func (h *Handlers) overview(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.PrincipalFrom(ctx)
	sc, err := h.parseScope(ctx, r, p)
	if err != nil {
		return err
	}
	if err := h.checkAccess(ctx, p, sc); err != nil {
		return err
	}
	data, err := load(ctx, h.pool, sc)
	if err != nil {
		return err
	}
	threshold := 70.0
	if h.st != nil {
		threshold = h.st.Get(ctx).PassThreshold
	}
	httpx.WriteJSON(w, http.StatusOK, build(data, sc, threshold, h.label, h.now().UTC()))
	return nil
}

// parseScope — фильтры из query. Некорректные значения — 400 по полям; неизвестная
// категория — 404 (как «не найдено»).
func (h *Handlers) parseScope(ctx context.Context, r *http.Request, p *core.Principal) (*scope, error) {
	q := r.URL.Query()
	sc := &scope{days: defaultDays}
	if p.Role != core.RoleAdmin {
		id := p.UserID
		sc.teacher = &id
	}
	fields := map[string]string{}
	parseID := func(name string) *uuid.UUID {
		s := strings.TrimSpace(q.Get(name))
		if s == "" {
			return nil
		}
		id, err := uuid.Parse(s)
		if err != nil {
			fields[name] = "Ожидается UUID"
			return nil
		}
		return &id
	}
	sc.lesson = parseID("lessonId")
	sc.student = parseID("studentId")
	daysSet := false
	if s := strings.TrimSpace(q.Get("days")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxDays {
			fields["days"] = "Целое число от 1 до 365"
		} else {
			sc.days, daysSet = n, true
		}
	}
	if len(fields) > 0 {
		return nil, httpx.Validation("Некорректные параметры аналитики", fields)
	}
	if key := strings.TrimSpace(q.Get("categoryId")); key != "" {
		if len(key) > 100 {
			return nil, httpx.Validation("Некорректные параметры аналитики", map[string]string{"categoryId": "Слишком длинное значение"})
		}
		id, ok, err := h.resolveCategory(ctx, key)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, httpx.NotFound("Категория классификатора не найдена")
		}
		sc.category, sc.categoryIn = &id, key
	}
	// Выбрано занятие и период не задан явно — всё занятие: иначе старое занятие дало бы
	// пустую аналитику «за последние 30 дней».
	if sc.lesson == nil || daysSet {
		since := h.now().UTC().AddDate(0, 0, -sc.days)
		sc.since = &since
	}
	return sc, nil
}

// resolveCategory — категория по uuid, коду или id фикстуры: сначала справочник в памяти,
// затем БД (справочник мог ещё не перечитаться после импорта).
func (h *Handlers) resolveCategory(ctx context.Context, key string) (uuid.UUID, bool, error) {
	if h.cat != nil {
		info, ok := h.cat.TypeByID(key)
		if !ok {
			info, ok = h.cat.TypeByCode(key)
		}
		if ok {
			id, err := uuid.Parse(info.ID)
			return id, err == nil, nil
		}
	}
	var id uuid.UUID
	err := h.pool.QueryRow(ctx, sqlCategoryByKey, key).Scan(&id)
	switch {
	case err == nil:
		return id, true, nil
	case pg.IsNoRows(err):
		return uuid.Nil, false, nil
	}
	return uuid.Nil, false, fmt.Errorf("analytics: category: %w", err)
}

const sqlCategoryByKey = `
SELECT id FROM classifier_categories
 WHERE id::text = $1 OR code = $1 OR extra->>'fixture_id' = $1
 LIMIT 1`

const (
	sqlLessonOwner = `SELECT teacher_id, created_by FROM lessons WHERE id = $1`
	sqlStudentRole = `SELECT role FROM users WHERE id = $1 AND deleted_at IS NULL`
)

// checkAccess — занятие существует и принадлежит преподавателю; обучающийся существует.
// Данные в любом случае режутся по занятиям преподавателя (scope.teacher) — чужих попыток
// преподаватель не увидит и без этих проверок; они дают честные 404/403 вместо пустоты.
func (h *Handlers) checkAccess(ctx context.Context, p *core.Principal, sc *scope) error {
	if sc.lesson != nil {
		var teacher, createdBy *uuid.UUID
		if err := h.pool.QueryRow(ctx, sqlLessonOwner, *sc.lesson).Scan(&teacher, &createdBy); err != nil {
			if pg.IsNoRows(err) {
				return httpx.NotFound("Занятие не найдено")
			}
			return fmt.Errorf("analytics: lesson: %w", err)
		}
		owner := createdBy
		if teacher != nil {
			owner = teacher
		}
		if p.Role != core.RoleAdmin && (owner == nil || *owner != p.UserID) {
			return httpx.Forbidden("Аналитика доступна по своим занятиям")
		}
	}
	if sc.student != nil {
		var role string
		if err := h.pool.QueryRow(ctx, sqlStudentRole, *sc.student).Scan(&role); err != nil || role != string(core.RoleStudent) {
			if err == nil || pg.IsNoRows(err) {
				return httpx.NotFound("Обучающийся не найден")
			}
			return fmt.Errorf("analytics: student: %w", err)
		}
	}
	return nil
}

// label — подпись поля карточки: из самой ошибки (FieldError.label), иначе справочник.
func (h *Handlers) label(field, fromData string) string {
	if l, ok := ddsLabels[field]; ok {
		return l
	}
	if strings.TrimSpace(fromData) != "" {
		return fromData
	}
	if h.cat != nil {
		return h.cat.FieldLabel(field)
	}
	return field
}

// ddsLabels — поля протокола реагирования (ракурс ДДС): у statuses подпись в каждой ошибке
// своя («Статус «Прибытие»»), в разрезе группы нужна общая.
var ddsLabels = map[string]string{
	"reaction.decision":     "Решение по карточке",
	"reaction.decisionTime": "Норматив решения",
	"reaction.refusal":      "Необоснованный отказ",
	"reaction.statuses":     "Статусы хода работ",
}
