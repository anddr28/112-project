package scenarios

// SQL пакета — константы (pgx кэширует prepared statements по тексту), колонки явно.
// Чтения сценария целиком — store.GetScenario/ListScenarios (один запрос с эталоном,
// числом занятий и готовностью озвучки); здесь — только записи и блокировки.

// sqlLockScenario — блокировка строки сценария перед read-modify-write (PATCH, approve,
// reject). FOR UPDATE конфликтует с FOR KEY SHARE, который берёт FK при INSERT в
// lesson_scenarios: сценарий не может «попасть в занятие» посреди правки, а последующее
// чтение (store.GetScenario — новый снимок READ COMMITTED) видит актуальное число занятий.
const sqlLockScenario = `SELECT id FROM scenarios WHERE id = $1 FOR UPDATE`

// sqlInsertScenarioWithEtalon — сценарий и эталон v1 одним оператором: атомарно без
// явной транзакции и за один round-trip (FK эталона проверяется в конце оператора).
const sqlInsertScenarioWithEtalon = `
WITH s AS (
  INSERT INTO scenarios (id, title, category_id, difficulty, mode, source, status, call_script, author_id,
                         validated_by, validated_at, teacher_comment, generation_meta, created_at, updated_at, version)
  VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $14, 1)
  RETURNING id
)
INSERT INTO etalons (id, scenario_id, version, is_current, card, card_draft, scoring, expected_actions,
                     expected_dialogue, created_by, created_at)
SELECT $15, s.id, 1, true, $16, $17, $18, $19, $20, $9, $14 FROM s`

// sqlInsertScenario — сценарий без эталона (заглушка генерации: эталон придёт с результатом).
const sqlInsertScenario = `
INSERT INTO scenarios (id, title, category_id, difficulty, mode, source, status, call_script, author_id,
                       teacher_comment, generation_meta, created_at, updated_at, version)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12, 1)`

// sqlUpdateScenario — простые поля и легенда из PATCH. NULL-параметр — поле не менялось;
// teacher_comment меняется только по флагу ($5), пустая строка очищает.
const sqlUpdateScenario = `
UPDATE scenarios
   SET title           = COALESCE($2, title),
       difficulty      = COALESCE($3, difficulty),
       mode            = COALESCE($4, mode),
       teacher_comment = CASE WHEN $5::bool THEN NULLIF($6::text, '') ELSE teacher_comment END,
       call_script     = COALESCE($7, call_script)
 WHERE id = $1`

// sqlUpdateCallScript — только легенда (проставленные tts_hash).
const sqlUpdateCallScript = `UPDATE scenarios SET call_script = $2 WHERE id = $1`

// Новая версия эталона: сначала снять is_current со старой (частичный уникальный индекс
// etalons_current_idx проверяется немедленно), затем вставить max(version)+1. Оба оператора
// идут одним pgx.Batch; гонок нет — строка сценария заблокирована вызывающим.
const sqlFlipEtalon = `UPDATE etalons SET is_current = false WHERE scenario_id = $1 AND is_current`

const sqlInsertEtalonVersion = `
INSERT INTO etalons (id, scenario_id, version, is_current, card, card_draft, scoring, expected_actions,
                     expected_dialogue, created_by)
VALUES ($1, $2, (SELECT COALESCE(max(version), 0) + 1 FROM etalons WHERE scenario_id = $2), true,
        $3, $4, $5, $6, $7, $8)
RETURNING version`

const sqlApprove = `
UPDATE scenarios
   SET status = 'validated', validated_by = $2, validated_at = now(), call_script = $3
 WHERE id = $1`

// sqlReject — причина и в teacher_comment (её видит редактор), и в generation_meta
// (история для QA модели: почему отклонили сгенерированное).
const sqlReject = `
UPDATE scenarios
   SET status = 'rejected', teacher_comment = $2, validated_by = NULL, validated_at = NULL,
       generation_meta = CASE WHEN jsonb_typeof(generation_meta) = 'object' THEN generation_meta ELSE '{}'::jsonb END
                         || jsonb_build_object('rejected_reason', $2::text)
 WHERE id = $1`

// ---------------------------------------------------------------- версии сценария

// sqlLockChainRoot — блокировка корня цепочки версий: две одновременные «новые версии»
// в одной цепочке сериализуются и не получают одинаковый номер. Глубина подъёма
// ограничена (защита от цикла parent_id, созданного руками в БД).
const sqlLockChainRoot = `
WITH RECURSIVE up (id, parent_id, depth) AS (
  SELECT id, parent_id, 0 FROM scenarios WHERE id = $1
  UNION ALL
  SELECT s.id, s.parent_id, up.depth + 1 FROM scenarios s JOIN up ON s.id = up.parent_id WHERE up.depth < 64
)
SELECT s.id FROM scenarios s
 WHERE s.id = (SELECT id FROM up ORDER BY depth DESC LIMIT 1)
   FOR UPDATE OF s`

// sqlChainMaxVersion — максимальный номер версии по всей цепочке (корень и все потомки,
// индекс scenarios_parent_idx). Идёт в том же batch после блокировки корня — отдельный
// оператор, поэтому видит всё, что закоммитили до получения блокировки.
const sqlChainMaxVersion = `
WITH RECURSIVE up (id, parent_id, depth) AS (
  SELECT id, parent_id, 0 FROM scenarios WHERE id = $1
  UNION ALL
  SELECT s.id, s.parent_id, up.depth + 1 FROM scenarios s JOIN up ON s.id = up.parent_id WHERE up.depth < 64
),
down (id, version) AS (
  SELECT id, version FROM scenarios WHERE id = (SELECT id FROM up ORDER BY depth DESC LIMIT 1)
  UNION
  SELECT s.id, s.version FROM scenarios s JOIN down d ON s.parent_id = d.id
)
SELECT COALESCE(max(version), 1)::int FROM down`

// Копия сценария и текущего эталона на стороне БД: jsonb не гоняется через Go.
const sqlCopyScenario = `
INSERT INTO scenarios (id, title, category_id, difficulty, mode, source, status, call_script, author_id,
                       teacher_comment, generation_meta, parent_id, version)
SELECT $2, title, category_id, difficulty, mode, source, 'draft', call_script, $3, teacher_comment,
       CASE WHEN jsonb_typeof(generation_meta) = 'object' THEN generation_meta ELSE '{}'::jsonb END
       || jsonb_build_object('parent_id', id::text),
       id, $4
  FROM scenarios
 WHERE id = $1`

const sqlCopyEtalon = `
INSERT INTO etalons (id, scenario_id, version, is_current, card, card_draft, scoring, expected_actions,
                     expected_dialogue, created_by)
SELECT $2, $3, 1, true, card, card_draft, scoring, expected_actions, expected_dialogue, $4
  FROM etalons
 WHERE scenario_id = $1 AND is_current`

// ---------------------------------------------------------------- генерация

// sqlAvoidTitles — заголовки категории, которые LLM не должна повторять (без архива и
// без заглушек генерации в полёте). Индекс scenarios_category_idx.
const sqlAvoidTitles = `
SELECT title FROM scenarios
 WHERE category_id = $1 AND archived_at IS NULL AND status <> 'archived'
   AND NOT (source = 'generated' AND status = 'draft')
 ORDER BY created_at DESC
 LIMIT 20`

// sqlLockGenerated — сценарий, ждущий результат генерации (под блокировкой: преподаватель
// мог в этот момент отклонить заглушку).
const sqlLockGenerated = `
SELECT status, archived_at IS NOT NULL, category_id, generation_meta
  FROM scenarios
 WHERE id = $1
   FOR UPDATE`

const sqlApplyGenerated = `
UPDATE scenarios
   SET title = $2, call_script = $3, status = 'generated', generation_meta = $4
 WHERE id = $1`

// sqlGenerateFailed — генерация окончательно провалилась: заглушка уходит в архив (из
// списка по умолчанию пропадает; причину UI берёт из статуса задачи), ошибка — в
// generation_meta. Только для заглушки, которую преподаватель ещё не трогал.
const sqlGenerateFailed = `
UPDATE scenarios
   SET status = 'archived', archived_at = now(),
       generation_meta = CASE WHEN jsonb_typeof(generation_meta) = 'object' THEN generation_meta ELSE '{}'::jsonb END
                         || jsonb_build_object('error', $2::text, 'error_code', $3::text)
 WHERE id = $1 AND status = 'draft' AND archived_at IS NULL`

// ---------------------------------------------------------------- озвучка

// sqlUpsertTTS — файл озвучки в глобальном кэше (ключ — sha256(text|voice|rate)).
// Повтор (переотправка результата, повторное прослушивание) обновляет путь и LRU-метку.
const sqlUpsertTTS = `
INSERT INTO tts_cache (text_hash, voice, rate, file_path, duration_ms)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (text_hash) DO UPDATE
   SET file_path = EXCLUDED.file_path, duration_ms = EXCLUDED.duration_ms, last_used_at = now()`

// sqlCallerVoice — голос заявителя сценария (для прослушивания реплики в редакторе).
const sqlCallerVoice = `SELECT COALESCE(call_script->'caller'->>'voice', '') FROM scenarios WHERE id = $1`

// ---------------------------------------------------------------- сид

// sqlSeedLock — сид демо-сценариев не должен задвоиться (у scenarios.title нет
// уникальности; так же устроен сид демо-занятий).
const sqlSeedLock = `SELECT pg_advisory_xact_lock(hashtext('lct:seed:demo-scenarios'))`

const sqlSeedTeacher = `SELECT id FROM users WHERE login = $1 AND deleted_at IS NULL`

// sqlSeedCategories — типы классификатора по кодам и id фронтовых фикстур (extra.fixture_id
// пишет classifier.SeedReference): сид не зависит от того, перечитан ли уже Catalog.
const sqlSeedCategories = `
SELECT id, code, COALESCE(extra->>'fixture_id', '')
  FROM classifier_categories
 WHERE code = ANY($1::text[]) OR extra->>'fixture_id' = ANY($2::text[])`

const sqlSeedExisting = `SELECT title FROM scenarios WHERE title = ANY($1::text[])`
