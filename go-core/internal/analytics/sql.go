package analytics

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/platform/pg"
)

// baseCTE — оценённые попытки в рамках фильтров. Параметры: $1 since (NULL — без окна),
// $2 преподаватель (NULL — админ), $3 занятие, $4 обучающийся, $5 категория.
// CTE, на который ссылаются один раз, PostgreSQL встраивает в запрос (12+): лишние колонки
// не читаются, фильтры проталкиваются к индексам.
//
// Слой «оценён» — есть балл и состояние не skipped/failed (у fields/timing состояния нет —
// они считаются всегда).
const baseCTE = `
WITH base AS (
  SELECT a.id, a.user_id, a.submitted_at, a.time_spent_ms, a.time_limit_sec,
         (EXTRACT(EPOCH FROM (a.first_input_at - a.call_accepted_at)) * 1000)::float8 AS reaction_ms,
         s.category_id, e.final_score::float8 AS final_score,
         COALESCE(e.override_verdict, e.verdict) AS verdict,
         e.needs_review, e.timing, e.field_errors, e.semantic, e.grammar_remarks, e.dialogue,
         e.fields_score::float8 AS fields_score,
         CASE WHEN COALESCE(e.layers->>'semantic', 'done') IN ('skipped', 'failed') THEN NULL
              ELSE e.semantic_score::float8 END AS semantic_score,
         CASE WHEN COALESCE(e.layers->>'grammar', 'done') IN ('skipped', 'failed') THEN NULL
              ELSE e.grammar_score::float8 END AS grammar_score,
         e.timing_score::float8 AS timing_score,
         CASE WHEN COALESCE(e.layers->>'dialogue', 'done') IN ('skipped', 'failed') THEN NULL
              ELSE e.dialogue_score::float8 END AS dialogue_score
    FROM attempts a
    JOIN lessons l ON l.id = a.lesson_id
    JOIN scenarios s ON s.id = a.scenario_id
    JOIN evaluations e ON e.attempt_id = a.id
   WHERE a.status = 'evaluated'
     AND ($1::timestamptz IS NULL OR a.submitted_at >= $1)
     AND ($2::uuid IS NULL OR l.teacher_id = $2)
     AND ($3::uuid IS NULL OR a.lesson_id = $3)
     AND ($4::uuid IS NULL OR a.user_id = $4)
     AND ($5::uuid IS NULL OR s.category_id = $5)
)`

// passPct — доля зачётов в процентах (0 при пустой группе).
const passPct = `COALESCE(100.0 * count(*) FILTER (WHERE verdict = 'pass') / NULLIF(count(*), 0), 0)::float8`

// fieldErrorsLateral / … — развёртка jsonb-массивов с защитой от «не массива».
const (
	fieldErrorsLateral = `CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(b.field_errors) = 'array'
                                                  THEN b.field_errors ELSE '[]'::jsonb END) fe`
	fieldErrorWhere = `jsonb_typeof(fe) = 'object' AND COALESCE(fe->>'field', '') <> ''`
)

// Раздел 1: сводка и средние по слоям — одна строка.
const sqlSummary = baseCTE + `
SELECT count(*)::int, count(DISTINCT user_id)::int,
       COALESCE(avg(final_score), 0)::float8, ` + passPct + `,
       COALESCE(avg(time_spent_ms), 0)::float8,
       avg(reaction_ms) FILTER (WHERE reaction_ms >= 0)::float8,
       COALESCE(100.0 * count(*) FILTER (WHERE timing->'within_norm' = 'true'::jsonb)
                / NULLIF(count(*) FILTER (WHERE jsonb_typeof(timing->'within_norm') = 'boolean'), 0), 0)::float8,
       count(*) FILTER (WHERE needs_review)::int,
       avg(fields_score)::float8, count(fields_score)::int,
       avg(semantic_score)::float8, count(semantic_score)::int,
       avg(grammar_score)::float8, count(grammar_score)::int,
       avg(timing_score)::float8, count(timing_score)::int,
       avg(dialogue_score)::float8, count(dialogue_score)::int
  FROM base`

// Раздел 2: тепловая карта — ошибки по (категория, поле).
const sqlHeatmap = baseCTE + `
SELECT b.category_id, fe->>'field', COALESCE(max(NULLIF(fe->>'label', '')), ''), count(*)::int
  FROM base b ` + fieldErrorsLateral + `
 WHERE ` + fieldErrorWhere + `
 GROUP BY 1, 2`

// Раздел 3: частые ошибки полей — (поле, вид), доля попыток и обучающиеся с ошибкой.
const sqlFieldErrors = baseCTE + `
SELECT fe->>'field', fe->>'kind', COALESCE(max(NULLIF(fe->>'label', '')), ''), count(*)::int,
       count(DISTINCT b.id)::int, count(DISTINCT b.user_id)::int,
       (array_agg(DISTINCT b.user_id))[1:50]
  FROM base b ` + fieldErrorsLateral + `
 WHERE ` + fieldErrorWhere + ` AND fe->>'kind' IN ('missing', 'wrong', 'extra')
 GROUP BY 1, 2
 ORDER BY 5 DESC, 1, 2
 LIMIT 20`

// Раздел 4: категории — успеваемость и выход за норматив (time_spent > норматива).
const sqlCategories = baseCTE + `
SELECT b.category_id, COALESCE(c.name, ''), count(*)::int, COALESCE(avg(final_score), 0)::float8, ` + passPct + `,
       avg(time_spent_ms)::float8,
       count(*) FILTER (WHERE time_spent_ms > time_limit_sec * 1000)::int,
       avg(time_spent_ms - time_limit_sec * 1000) FILTER (WHERE time_spent_ms > time_limit_sec * 1000)::float8,
       count(*) FILTER (WHERE time_spent_ms IS NOT NULL)::int
  FROM base b
  LEFT JOIN classifier_categories c ON c.id = b.category_id
 GROUP BY 1, 2
 ORDER BY 3 DESC, 2
 LIMIT 100`

// Раздел 5: динамика по дням (UTC), только дни с попытками.
const sqlTrend = baseCTE + `
SELECT (submitted_at AT TIME ZONE 'UTC')::date, count(*)::int, COALESCE(avg(final_score), 0)::float8, ` + passPct + `,
       avg(time_spent_ms)::float8
  FROM base
 GROUP BY 1
 ORDER BY 1`

// Раздел 6: обучающиеся — успеваемость, средние по слоям, вердикты трёх последних попыток.
const sqlStudents = baseCTE + `
SELECT b.user_id, u.last_name, u.first_name, COALESCE(u.middle_name, ''),
       count(*)::int, COALESCE(avg(final_score), 0)::float8, ` + passPct + `, avg(time_spent_ms)::float8,
       avg(fields_score)::float8, avg(semantic_score)::float8, avg(grammar_score)::float8,
       avg(timing_score)::float8, avg(dialogue_score)::float8,
       (array_agg(verdict ORDER BY submitted_at DESC))[1:3]
  FROM base b
  JOIN users u ON u.id = b.user_id
 GROUP BY 1, 2, 3, 4
 ORDER BY 2, 3, 1
 LIMIT 500`

// Раздел 7: факты, не отражённые в карточке (semantic.missing_facts).
const sqlMissingFacts = baseCTE + `
SELECT f, count(*)::int, count(DISTINCT b.user_id)::int, (array_agg(DISTINCT b.user_id))[1:50]
  FROM base b
  CROSS JOIN LATERAL jsonb_array_elements_text(CASE WHEN jsonb_typeof(b.semantic->'missing_facts') = 'array'
                                                    THEN b.semantic->'missing_facts' ELSE '[]'::jsonb END) f
 WHERE b.semantic_score IS NOT NULL AND btrim(f) <> ''
 GROUP BY 1
 ORDER BY 2 DESC, 1
 LIMIT 10`

// Раздел 8: правила грамотности (LanguageTool rule; нет правила — по сообщению).
const sqlGrammar = baseCTE + `
SELECT COALESCE(NULLIF(r->>'rule', ''), r->>'message'), COALESCE(max(r->>'message'), ''), count(*)::int,
       count(DISTINCT b.user_id)::int, max(r->'suggestions'->>0)
  FROM base b
  CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(b.grammar_remarks) = 'array'
                                               THEN b.grammar_remarks ELSE '[]'::jsonb END) r
 WHERE jsonb_typeof(r) = 'object' AND COALESCE(NULLIF(r->>'rule', ''), r->>'message', '') <> ''
 GROUP BY 1
 ORDER BY 3 DESC, 1
 LIMIT 10`

// Раздел 9: что не уточняют в разговоре (dialogue.missing_questions).
const sqlDialogueGaps = baseCTE + `
SELECT q, count(*)::int, count(DISTINCT b.user_id)::int, (array_agg(DISTINCT b.user_id))[1:50]
  FROM base b
  CROSS JOIN LATERAL jsonb_array_elements_text(CASE WHEN jsonb_typeof(b.dialogue->'missing_questions') = 'array'
                                                    THEN b.dialogue->'missing_questions' ELSE '[]'::jsonb END) q
 WHERE b.dialogue_score IS NOT NULL AND btrim(q) <> ''
 GROUP BY 1
 ORDER BY 2 DESC, 1
 LIMIT 5`

// ---------------------------------------------------------------- строки разделов

type layerAgg struct {
	avg *float64
	n   int
}

type summaryRow struct {
	attempts, students, needsReview int
	avgScore, passPct, avgTimeMs    float64
	avgReactionMs                   *float64
	withinPct                       float64
	layers                          [5]layerAgg // fields, semantic, grammar, timing, dialogue
}

type heatRow struct {
	category     uuid.UUID
	field, label string
	count        int
}

type fieldErrRow struct {
	field, kind, label        string
	count, attempts, students int
	users                     []uuid.UUID
}

type categoryRow struct {
	id                uuid.UUID
	name              string
	attempts          int
	avgScore, passPct float64
	avgTimeMs         *float64
	overLimit, timed  int
	avgOverMs         *float64
}

type trendRow struct {
	day               time.Time
	attempts          int
	avgScore, passPct float64
	avgTimeMs         *float64
}

type studentRow struct {
	id                  uuid.UUID
	last, first, middle string
	attempts            int
	avgScore, passPct   float64
	avgTimeMs           *float64
	layers              [5]*float64
	lastVerdicts        []string
}

type factRow struct {
	text            string
	count, students int
	users           []uuid.UUID
}

type grammarRow struct {
	rule, message   string
	count, students int
	suggestion      *string
}

type overviewData struct {
	summary    summaryRow
	heat       []heatRow
	fieldErrs  []fieldErrRow
	categories []categoryRow
	trend      []trendRow
	students   []studentRow
	facts      []factRow
	grammar    []grammarRow
	dialogue   []factRow
}

// load — все разделы одним batch (один round-trip).
func load(ctx context.Context, q pg.Querier, sc *scope) (*overviewData, error) {
	args := []any{sc.since, sc.teacher, sc.lesson, sc.student, sc.category}
	b := &pgx.Batch{}
	for _, sql := range []string{sqlSummary, sqlHeatmap, sqlFieldErrors, sqlCategories, sqlTrend, sqlStudents,
		sqlMissingFacts, sqlGrammar, sqlDialogueGaps} {
		b.Queue(sql, args...)
	}
	br := q.SendBatch(ctx, b)
	defer br.Close()

	d := &overviewData{}
	s := &d.summary
	dst := []any{&s.attempts, &s.students, &s.avgScore, &s.passPct, &s.avgTimeMs, &s.avgReactionMs, &s.withinPct, &s.needsReview}
	for i := range s.layers {
		dst = append(dst, &s.layers[i].avg, &s.layers[i].n)
	}
	if err := br.QueryRow().Scan(dst...); err != nil {
		return nil, fmt.Errorf("analytics: summary: %w", err)
	}

	steps := []struct {
		name string
		scan func(pgx.Rows) error
	}{
		{"heatmap", func(r pgx.Rows) error {
			var x heatRow
			if err := r.Scan(&x.category, &x.field, &x.label, &x.count); err != nil {
				return err
			}
			d.heat = append(d.heat, x)
			return nil
		}},
		{"field errors", func(r pgx.Rows) error {
			var x fieldErrRow
			if err := r.Scan(&x.field, &x.kind, &x.label, &x.count, &x.attempts, &x.students, &x.users); err != nil {
				return err
			}
			d.fieldErrs = append(d.fieldErrs, x)
			return nil
		}},
		{"categories", func(r pgx.Rows) error {
			var x categoryRow
			if err := r.Scan(&x.id, &x.name, &x.attempts, &x.avgScore, &x.passPct, &x.avgTimeMs, &x.overLimit,
				&x.avgOverMs, &x.timed); err != nil {
				return err
			}
			d.categories = append(d.categories, x)
			return nil
		}},
		{"trend", func(r pgx.Rows) error {
			var x trendRow
			if err := r.Scan(&x.day, &x.attempts, &x.avgScore, &x.passPct, &x.avgTimeMs); err != nil {
				return err
			}
			d.trend = append(d.trend, x)
			return nil
		}},
		{"students", func(r pgx.Rows) error {
			var x studentRow
			if err := r.Scan(&x.id, &x.last, &x.first, &x.middle, &x.attempts, &x.avgScore, &x.passPct, &x.avgTimeMs,
				&x.layers[0], &x.layers[1], &x.layers[2], &x.layers[3], &x.layers[4], &x.lastVerdicts); err != nil {
				return err
			}
			d.students = append(d.students, x)
			return nil
		}},
		{"missing facts", func(r pgx.Rows) error {
			var x factRow
			if err := r.Scan(&x.text, &x.count, &x.students, &x.users); err != nil {
				return err
			}
			d.facts = append(d.facts, x)
			return nil
		}},
		{"grammar", func(r pgx.Rows) error {
			var x grammarRow
			if err := r.Scan(&x.rule, &x.message, &x.count, &x.students, &x.suggestion); err != nil {
				return err
			}
			d.grammar = append(d.grammar, x)
			return nil
		}},
		{"dialogue", func(r pgx.Rows) error {
			var x factRow
			if err := r.Scan(&x.text, &x.count, &x.students, &x.users); err != nil {
				return err
			}
			d.dialogue = append(d.dialogue, x)
			return nil
		}},
	}
	for _, st := range steps {
		rows, err := br.Query()
		if err != nil {
			return nil, fmt.Errorf("analytics: %s: %w", st.name, err)
		}
		for rows.Next() {
			if err := st.scan(rows); err != nil {
				rows.Close()
				return nil, fmt.Errorf("analytics: %s: %w", st.name, err)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("analytics: %s: %w", st.name, err)
		}
	}
	return d, nil
}
