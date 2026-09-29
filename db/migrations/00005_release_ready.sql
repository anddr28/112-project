-- +goose Up
-- ============================================================================
-- 00005 — закрытие ТЗ к сдаче (контракт v1.4). Обоснование — docs/db-design.md, Р28–Р31.
--
--  * materials — справочная база и методматериалы: метаданные + текст (Markdown) + файл
--    (bytea ≤ 20 МБ) в одной строке; системные материалы из поставки (slug, is_system).
--    Прежняя таблица materials из 00001 (путь к файлу на диске, audience) кодом не
--    использовалась и данных не содержит — пересоздаётся под контракт v1.4.
--  * scenarios.source_attempt_id — сценарий, сделанный из карточки обучающегося
--    (POST /attempts/{id}/to-scenario): связь + идемпотентность (частичный уникальный индекс).
--  * attempts_evaluated_idx — окно по времени для аналитики группы (GET /analytics/overview).
-- ============================================================================

DROP TABLE materials;

CREATE TABLE materials (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  slug        text,                                            -- ключ системного материала (сид идемпотентен)
  title       text NOT NULL CHECK (char_length(title) BETWEEN 1 AND 200),
  description text NOT NULL DEFAULT '',
  category    text NOT NULL DEFAULT '',                        -- раздел: «Памятка АРМ-112», «Регламенты», …
  content     text,                                            -- Markdown (GET /materials/{id})
  file_name   text,
  mime_type   text,
  size_bytes  bigint,
  file_data   bytea,                                           -- сам файл; список его не читает (TOAST)
  is_system   boolean NOT NULL DEFAULT false,                  -- из поставки: не удаляется (409)
  author_id   uuid REFERENCES users (id),                      -- NULL только у системных
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  CHECK (file_data IS NULL OR octet_length(file_data) <= 20971520),          -- 20 МБ (контракт)
  CHECK ((file_data IS NULL) = (file_name IS NULL) AND (file_data IS NULL) = (mime_type IS NULL)),
  CHECK (content IS NOT NULL OR file_data IS NOT NULL),        -- пустой материал бессмыслен
  CHECK (is_system OR author_id IS NOT NULL),
  CHECK (NOT is_system OR slug IS NOT NULL)
);
-- PDF/MP3/DOCX уже сжаты: EXTERNAL — хранить вне строки без попытки pglz-сжатия
-- (экономия CPU на записи, а чтение диапазона байт не распаковывает весь файл).
ALTER TABLE materials ALTER COLUMN file_data SET STORAGE EXTERNAL;
CREATE UNIQUE INDEX materials_slug_idx ON materials (slug) WHERE slug IS NOT NULL;
CREATE INDEX materials_category_idx ON materials (category, created_at DESC);
CREATE TRIGGER materials_updated_at BEFORE UPDATE ON materials FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE scenarios ADD COLUMN source_attempt_id uuid REFERENCES attempts (id);
CREATE UNIQUE INDEX scenarios_source_attempt_idx ON scenarios (source_attempt_id)
  WHERE source_attempt_id IS NOT NULL;

-- Аналитика считается только по оценённым попыткам в окне «последние N дней»: частичный
-- индекс мал (только evaluated) и не трогает горячий путь черновика (attempt_drafts).
CREATE INDEX attempts_evaluated_idx ON attempts (submitted_at) WHERE status = 'evaluated';

-- +goose Down
DROP INDEX IF EXISTS attempts_evaluated_idx;
DROP INDEX IF EXISTS scenarios_source_attempt_idx;
ALTER TABLE scenarios DROP COLUMN IF EXISTS source_attempt_id;

DROP TABLE IF EXISTS materials;
CREATE TABLE materials (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  title       text NOT NULL,
  kind        text NOT NULL CHECK (kind IN ('pdf', 'docx', 'xml', 'audio', 'link', 'other')),
  file_path   text,
  url         text,
  audience    text NOT NULL DEFAULT 'all' CHECK (audience IN ('all', 'teachers', 'group')),
  group_id    uuid REFERENCES groups (id),
  uploaded_by uuid NOT NULL REFERENCES users (id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  deleted_at  timestamptz,
  CHECK (audience <> 'group' OR group_id IS NOT NULL)
);
