package store

import "strings"

// ServiceCodesSQL — SQL-выражение: службы списка оповещения эталона (алиас таблицы etalons —
// e), известные справочнику и активные, строкой кодов через запятую; основная служба —
// первой (dds.SplitCodes разбирает строку). Источник — как у слоя полей (evaluation.fieldSpec):
// службы формы АРМ эталона (card_draft.services), а если форма их не показывала (фикстуры,
// старые эталоны) — services_to_notify контрактной карточки. Коды служб запятых не содержат
// (справочник: '101'..'104', 'zhkh', 'gormost', …). Выражение не падает на любой форме jsonb.
//
// Строка, а не text[]: выражение стоит внутри ARRAY(...) по пулу сценариев, а массив массивов
// разной длины PostgreSQL не строит.
func ServiceCodesSQL(e string) string {
	return strings.ReplaceAll(serviceCodesTemplate, "{E}", e)
}

const serviceCodesTemplate = `
(SELECT COALESCE(string_agg(sv.code, ',' ORDER BY c.prim DESC, c.ord), '')
   FROM (SELECT x.v->>'code' AS code, COALESCE(x.v->'isPrimary' = 'true'::jsonb, false) AS prim, x.ord
           FROM jsonb_array_elements(CASE WHEN jsonb_typeof({E}.card_draft->'services') = 'array'
                                          THEN {E}.card_draft->'services' ELSE '[]'::jsonb END)
                WITH ORDINALITY AS x(v, ord)
          WHERE jsonb_typeof(x.v) = 'object'
         UNION ALL
         SELECT y.v #>> '{}', false, y.ord
           FROM jsonb_array_elements(CASE WHEN COALESCE(jsonb_array_length(CASE WHEN jsonb_typeof({E}.card_draft->'services') = 'array'
                                                                                THEN {E}.card_draft->'services' END), 0) = 0
                                           AND jsonb_typeof({E}.card->'services_to_notify') = 'array'
                                          THEN {E}.card->'services_to_notify' ELSE '[]'::jsonb END)
                WITH ORDINALITY AS y(v, ord)
          WHERE jsonb_typeof(y.v) = 'string') c
   JOIN services sv ON lower(sv.code) = lower(c.code) AND sv.is_active)`
