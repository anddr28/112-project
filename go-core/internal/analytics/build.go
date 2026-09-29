package analytics

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
)

// Элементы анонимных структур public.AnalyticsOverview (псевдонимы идентичных типов:
// если кодоген изменит схему, сборка упадёт здесь, а не молча разойдётся с контрактом).
type (
	layerItem = struct {
		Attempts int                                 `json:"attempts"`
		AvgScore float32                             `json:"avgScore"`
		Layer    public.AnalyticsOverviewLayersLayer `json:"layer"`
	}
	heatRowItem = struct {
		Attempts int    `json:"attempts"`
		Id       string `json:"id"`
		Label    string `json:"label"`
	}
	heatColItem = struct {
		Id    string `json:"id"`
		Label string `json:"label"`
	}
	trendItem = struct {
		Attempts    int                `json:"attempts"`
		AvgScore    float32            `json:"avgScore"`
		AvgTimeMs   *int               `json:"avgTimeMs,omitempty"`
		Date        openapi_types.Date `json:"date"`
		PassRatePct float32            `json:"passRatePct"`
	}
	fieldErrItem = struct {
		Count    int                                        `json:"count"`
		Field    string                                     `json:"field"`
		Kind     public.AnalyticsOverviewTopFieldErrorsKind `json:"kind"`
		Label    string                                     `json:"label"`
		SharePct float32                                    `json:"sharePct"`
	}
	grammarItem = struct {
		Count   int     `json:"count"`
		Example *string `json:"example,omitempty"`
		Message string  `json:"message"`
		Rule    string  `json:"rule"`
	}
	factItem = struct {
		Count    int     `json:"count"`
		Fact     string  `json:"fact"`
		SharePct float32 `json:"sharePct"`
	}
	studentItem = struct {
		Attempts     int                `json:"attempts"`
		AvgScore     float32            `json:"avgScore"`
		AvgTimeMs    *int               `json:"avgTimeMs,omitempty"`
		Name         string             `json:"name"`
		PassRatePct  float32            `json:"passRatePct"`
		UserId       openapi_types.UUID `json:"userId"`
		WeakestLayer *string            `json:"weakestLayer,omitempty"`
	}
	categoryItem = struct {
		Attempts     int     `json:"attempts"`
		AvgScore     float32 `json:"avgScore"`
		AvgTimeMs    *int    `json:"avgTimeMs,omitempty"`
		CategoryId   string  `json:"categoryId"`
		CategoryName string  `json:"categoryName"`
		PassRatePct  float32 `json:"passRatePct"`
	}
)

// layerKeys — порядок слоёв (как summaryRow.layers / studentRow.layers).
var layerKeys = [5]string{core.LayerFields, core.LayerSemantic, core.LayerGrammar, core.LayerTiming, core.LayerDialogue}

// Границы тепловой карты: полей больше 15 и категорий больше 20 на экране не прочитать.
const (
	heatMaxCols = 15
	heatMaxRows = 20
	topErrors   = 10
)

// build — ответ контракта из агрегатов (чистая функция: тестируется без БД).
func build(d *overviewData, sc *scope, threshold float64, label func(field, fromData string) string, now time.Time) public.AnalyticsOverview {
	var out public.AnalyticsOverview
	out.GeneratedAt = now
	out.Scope.Days = sc.days
	out.Scope.LessonId, out.Scope.StudentId = sc.lesson, sc.student
	if sc.categoryIn != "" {
		c := sc.categoryIn
		out.Scope.CategoryId = &c
	}

	s := &d.summary
	out.Summary.Attempts, out.Summary.Students, out.Summary.NeedsReview = s.attempts, s.students, s.needsReview
	out.Summary.AvgScore, out.Summary.PassRatePct = r1(s.avgScore), r1(s.passPct)
	out.Summary.AvgTimeMs = int(math.Round(s.avgTimeMs))
	out.Summary.AvgReactionMs = roundInt(s.avgReactionMs)
	out.Summary.WithinNormPct = r1(s.withinPct)

	out.Layers = make([]layerItem, 0, len(layerKeys))
	for i, k := range layerKeys {
		if l := s.layers[i]; l.n > 0 && l.avg != nil {
			out.Layers = append(out.Layers, layerItem{Layer: public.AnalyticsOverviewLayersLayer(k), AvgScore: r1(*l.avg), Attempts: l.n})
		}
	}

	names := make(map[uuid.UUID]string, len(d.students))
	for _, st := range d.students {
		names[st.id] = fullName(st.last, st.first, st.middle)
	}
	catNames := make(map[uuid.UUID]string, len(d.categories))
	for _, c := range d.categories {
		catNames[c.id] = orText(c.name, "Категория без названия")
	}

	out.Heatmap.Rows, out.Heatmap.Cols, out.Heatmap.Cells = buildHeatmap(d, catNames, label)

	out.Trend = make([]trendItem, 0, len(d.trend))
	for _, t := range d.trend {
		out.Trend = append(out.Trend, trendItem{Date: openapi_types.Date{Time: t.day}, Attempts: t.attempts,
			AvgScore: r1(t.avgScore), PassRatePct: r1(t.passPct), AvgTimeMs: roundInt(t.avgTimeMs)})
	}

	out.TopFieldErrors = make([]fieldErrItem, 0, min(len(d.fieldErrs), topErrors))
	for _, fe := range d.fieldErrs {
		if len(out.TopFieldErrors) == topErrors {
			break
		}
		out.TopFieldErrors = append(out.TopFieldErrors, fieldErrItem{Field: fe.field, Label: label(fe.field, fe.label),
			Kind: public.AnalyticsOverviewTopFieldErrorsKind(fe.kind), Count: fe.count, SharePct: pct(fe.attempts, s.attempts)})
	}

	semN, dlgN := s.layers[1].n, s.layers[4].n
	out.TopMissingFacts = make([]factItem, 0, len(d.facts))
	for _, f := range d.facts {
		out.TopMissingFacts = append(out.TopMissingFacts, factItem{Fact: f.text, Count: f.count, SharePct: pct(f.count, semN)})
	}
	out.TopGrammarRules = make([]grammarItem, 0, len(d.grammar))
	for _, g := range d.grammar {
		it := grammarItem{Rule: g.rule, Message: g.message, Count: g.count}
		if g.suggestion != nil && strings.TrimSpace(*g.suggestion) != "" {
			ex := "Вариант исправления: «" + strings.TrimSpace(*g.suggestion) + "»"
			it.Example = &ex
		}
		out.TopGrammarRules = append(out.TopGrammarRules, it)
	}

	out.Students = make([]studentItem, 0, len(d.students))
	for _, st := range d.students {
		it := studentItem{UserId: st.id, Name: names[st.id], Attempts: st.attempts, AvgScore: r1(st.avgScore),
			PassRatePct: r1(st.passPct), AvgTimeMs: roundInt(st.avgTimeMs)}
		if w := weakestLayer(st.layers); w != "" {
			it.WeakestLayer = &w
		}
		out.Students = append(out.Students, it)
	}
	out.Categories = make([]categoryItem, 0, len(d.categories))
	for _, c := range d.categories {
		out.Categories = append(out.Categories, categoryItem{CategoryId: c.id.String(), CategoryName: catNames[c.id],
			Attempts: c.attempts, AvgScore: r1(c.avgScore), PassRatePct: r1(c.passPct), AvgTimeMs: roundInt(c.avgTimeMs)})
	}

	out.Insights = buildInsights(insightInput{data: d, threshold: threshold, names: names, catNames: catNames,
		label: label, semAttempts: semN, dlgAttempts: dlgN})
	return out
}

// buildHeatmap — строки: категории (по числу попыток, не больше 20); столбцы: поля с
// ошибками (по числу ошибок, не больше 15); ячейки — число ошибок поля в категории.
func buildHeatmap(d *overviewData, catNames map[uuid.UUID]string, label func(string, string) string) ([]heatRowItem, []heatColItem, [][]int) {
	type colAgg struct {
		field, label string
		total        int
	}
	agg := map[string]*colAgg{}
	for _, h := range d.heat {
		c := agg[h.field]
		if c == nil {
			c = &colAgg{field: h.field}
			agg[h.field] = c
		}
		c.total += h.count
		if c.label == "" && h.label != "" {
			c.label = h.label
		}
	}
	cols := make([]*colAgg, 0, len(agg))
	for _, c := range agg {
		cols = append(cols, c)
	}
	sort.Slice(cols, func(i, j int) bool {
		if cols[i].total != cols[j].total {
			return cols[i].total > cols[j].total
		}
		return cols[i].field < cols[j].field
	})
	if len(cols) > heatMaxCols {
		cols = cols[:heatMaxCols]
	}
	colIdx := make(map[string]int, len(cols))
	outCols := make([]heatColItem, len(cols))
	for i, c := range cols {
		colIdx[c.field] = i
		outCols[i] = heatColItem{Id: c.field, Label: label(c.field, c.label)}
	}

	cats := d.categories // уже по убыванию числа попыток
	if len(cats) > heatMaxRows {
		cats = cats[:heatMaxRows]
	}
	rowIdx := make(map[uuid.UUID]int, len(cats))
	rows := make([]heatRowItem, len(cats))
	cells := make([][]int, len(cats))
	for i, c := range cats {
		rowIdx[c.id] = i
		rows[i] = heatRowItem{Id: c.id.String(), Label: catNames[c.id], Attempts: c.attempts}
		cells[i] = make([]int, len(cols))
	}
	for _, h := range d.heat {
		ri, okR := rowIdx[h.category]
		ci, okC := colIdx[h.field]
		if okR && okC {
			cells[ri][ci] += h.count
		}
	}
	return rows, outCols, cells
}

// weakestLayer — слой с наименьшим средним (только оценённые слои).
func weakestLayer(l [5]*float64) string {
	best, name := math.Inf(1), ""
	for i, v := range l {
		if v != nil && *v < best {
			best, name = *v, layerKeys[i]
		}
	}
	return name
}

// ---------------------------------------------------------------- helpers

func r1(v float64) float32 { return float32(math.Round(v*10) / 10) }

func roundInt(p *float64) *int {
	if p == nil {
		return nil
	}
	n := int(math.Round(*p))
	return &n
}

func pct(n, of int) float32 {
	if of <= 0 {
		return 0
	}
	return r1(100 * float64(n) / float64(of))
}

func fullName(last, first, middle string) string {
	parts := make([]string, 0, 3)
	for _, p := range []string{last, first, middle} {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " ")
}

func orText(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
