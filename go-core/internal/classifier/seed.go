package classifier

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/platform/pg"
)

// SeedReference — идемпотентный сид справочников из встроенных данных (фронтовые фикстуры):
// службы (по code: имя/краткое имя/вид обновляются, недостающие добавляются, лишние в БД
// остаются) и типы классификатора (по code: название, глубина, родитель, синонимы, справка,
// «частые/значимые», порядок, опросная карта, правила служб). id сохраняются между сидами
// (ON CONFLICT по code), поэтому ссылки сценариев/занятий не ломаются. Одна транзакция,
// один round-trip на таблицу (pgx.Batch). После сида вызовите Catalog.Reload.
func SeedReference(ctx context.Context, pool *pgxpool.Pool) error {
	emb := data()
	types, err := seedRows(emb)
	if err != nil {
		return err
	}
	return pg.WithTx(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		return seedTx(ctx, tx, emb, types)
	})
}

// seedTx — запись сида в открытой транзакции (два батча: службы, затем типы).
func seedTx(ctx context.Context, q pg.Querier, emb *embedded, types []seedTypeRow) error {
	b := &pgx.Batch{}
	for _, s := range emb.services {
		b.Queue(qSeedService, s.Code, s.Name, s.ShortName, s.Kind)
	}
	if err := q.SendBatch(ctx, b).Close(); err != nil {
		return fmt.Errorf("classifier: seed services: %w", err)
	}

	b = &pgx.Batch{}
	for i := range types {
		t := &types[i]
		b.Queue(qSeedType, t.code, t.parent, t.name, t.depth, t.services, t.attributes,
			t.synonyms, t.reference, t.featured, t.sortOrder, t.rules, t.extra)
	}
	if err := q.SendBatch(ctx, b).Close(); err != nil {
		return fmt.Errorf("classifier: seed types: %w", err)
	}
	return nil
}

// qSeedService — службы: справочные поля из фикстуры, is_active не трогаем (решение админа).
const qSeedService = `
INSERT INTO services (code, name, short_name, kind)
VALUES ($1, $2, $3, $4)
ON CONFLICT (code) DO UPDATE
   SET name = EXCLUDED.name, short_name = EXCLUDED.short_name, kind = EXCLUDED.kind
 WHERE (services.name, services.short_name, services.kind)
       IS DISTINCT FROM (EXCLUDED.name, EXCLUDED.short_name, EXCLUDED.kind)`

// qSeedType — тип классификатора. Родитель — по коду (строки идут родителями вперёд).
// extra сливается (ключи импорта сохраняются). Неизменённая строка не переписывается —
// updated_at (и отпечаток каталога) не дёргается на каждом старте.
const qSeedType = `
INSERT INTO classifier_categories
       (code, parent_id, name, depth, services, attributes, synonyms, reference, featured, sort_order,
        service_rules, extra, is_active)
VALUES ($1, (SELECT id FROM classifier_categories WHERE code = NULLIF($2, '')), $3, $4, $5, $6, $7, NULLIF($8, ''),
        NULLIF($9, ''), $10, $11, $12, true)
ON CONFLICT (code) DO UPDATE
   SET parent_id = EXCLUDED.parent_id, name = EXCLUDED.name, depth = EXCLUDED.depth,
       services = EXCLUDED.services, attributes = EXCLUDED.attributes, synonyms = EXCLUDED.synonyms,
       reference = EXCLUDED.reference, featured = EXCLUDED.featured, sort_order = EXCLUDED.sort_order,
       service_rules = EXCLUDED.service_rules, extra = classifier_categories.extra || EXCLUDED.extra,
       is_active = true
 WHERE (classifier_categories.parent_id, classifier_categories.name, classifier_categories.depth,
        classifier_categories.services, classifier_categories.attributes, classifier_categories.synonyms,
        classifier_categories.reference, classifier_categories.featured, classifier_categories.sort_order,
        classifier_categories.service_rules, classifier_categories.extra || EXCLUDED.extra,
        classifier_categories.is_active)
       IS DISTINCT FROM
       (EXCLUDED.parent_id, EXCLUDED.name, EXCLUDED.depth, EXCLUDED.services, EXCLUDED.attributes,
        EXCLUDED.synonyms, EXCLUDED.reference, EXCLUDED.featured, EXCLUDED.sort_order,
        EXCLUDED.service_rules, classifier_categories.extra, true)`

type seedTypeRow struct {
	code       string
	parent     string
	name       string
	depth      int16
	services   []byte // jsonb: коды служб
	attributes []byte // jsonb: []public.AttributeGroup (camelCase)
	synonyms   []string
	reference  string
	featured   string
	sortOrder  int32
	rules      []byte // jsonb: []ruleJSON (snake_case)
	extra      []byte // jsonb: {fixture_id, featured_rank?, rules_rank?}
}

// seedRows — чистая часть сида: встроенные фикстуры -> строки БД (коды вместо id фикстур).
func seedRows(emb *embedded) ([]seedTypeRow, error) {
	c := &emb.classifier
	known := make(map[string]int, len(c.Types))
	for i, t := range c.Types {
		known[t.Code] = i
	}

	rulesByType := make(map[string][]ruleJSON, len(c.Types))
	rulesRank := make(map[string]int, len(c.Types))
	for i, r := range c.Rules {
		if _, ok := known[r.Type]; !ok {
			return nil, fmt.Errorf("classifier: seed rule %d: неизвестный тип %q", i, r.Type)
		}
		if _, ok := rulesRank[r.Type]; !ok {
			rulesRank[r.Type] = i
		}
		rulesByType[r.Type] = append(rulesByType[r.Type], ruleJSON{Attr: r.Attr, AnyOf: r.AnyOf, Services: r.Services})
	}
	featured := make(map[string]string, len(c.Frequent)+len(c.Significant))
	featuredRank := make(map[string]int, len(featured))
	for i, code := range c.Frequent {
		featured[code], featuredRank[code] = "frequent", i
	}
	for i, code := range c.Significant {
		featured[code], featuredRank[code] = "significant", i
	}

	// родители — раньше детей (подзапрос parent_id видит строки, вставленные ранее в батче)
	order := make([]int, 0, len(c.Types))
	placed := make(map[string]bool, len(c.Types))
	var place func(i, depth int) error
	place = func(i, depth int) error {
		t := c.Types[i]
		if placed[t.Code] {
			return nil
		}
		if depth > 16 {
			return fmt.Errorf("classifier: seed: цикл родителей у %q", t.Code)
		}
		if t.Parent != "" {
			pi, ok := known[t.Parent]
			if !ok {
				return fmt.Errorf("classifier: seed: у %q неизвестный родитель %q", t.Code, t.Parent)
			}
			if err := place(pi, depth+1); err != nil {
				return err
			}
		}
		placed[t.Code] = true
		order = append(order, i)
		return nil
	}
	for i := range c.Types {
		if err := place(i, 0); err != nil {
			return nil, err
		}
	}

	rows := make([]seedTypeRow, 0, len(c.Types))
	for _, i := range order {
		t := c.Types[i]
		rules := rulesByType[t.Code]
		if rules == nil {
			rules = []ruleJSON{}
		}
		attrs := t.Attributes
		syn := t.Synonyms
		if syn == nil {
			syn = []string{}
		}
		extra := map[string]any{"fixture_id": t.FixtureID, "source": "fixture"}
		if r, ok := featuredRank[t.Code]; ok {
			extra["featured_rank"] = r
		}
		if r, ok := rulesRank[t.Code]; ok {
			extra["rules_rank"] = r
		}
		attrsJSON := []byte("[]")
		if len(attrs) > 0 {
			attrsJSON = mustMarshal(attrs)
		}
		servicesJSON, err := json.Marshal(codesFromRules(rules))
		if err != nil {
			return nil, err
		}
		rows = append(rows, seedTypeRow{
			code:       t.Code,
			parent:     t.Parent,
			name:       t.Name,
			depth:      int16(max(t.Depth, 1)),
			services:   servicesJSON,
			attributes: attrsJSON,
			synonyms:   syn,
			reference:  t.Reference,
			featured:   featured[t.Code],
			sortOrder:  int32(i),
			rules:      mustMarshal(rules),
			extra:      mustMarshal(extra),
		})
	}
	return rows, nil
}
