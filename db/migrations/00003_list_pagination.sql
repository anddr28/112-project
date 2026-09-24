-- +goose Up
-- ============================================================================
-- 00003 — постраничные списки (контракт v1.2: ?limit=&cursor=, X-Next-Cursor).
-- Обоснование — docs/db-design.md, Р26.
--
-- Страница — keyset: «строки после курсора» в порядке сортировки + LIMIT. Чтобы это был
-- индексный диапазон (читается ровно страница, сколько бы строк ни накопилось), нужен
-- индекс в порядке сортировки списка. Без него PostgreSQL сортирует всю выборку ради
-- первых 100 строк — именно это и «роняет» базу на больших списках.
-- ============================================================================

-- GET /lessons (администратор): новые сверху по всем занятиям
CREATE INDEX lessons_created_idx ON lessons (created_at DESC, id DESC);

-- GET /lessons (преподаватель): его занятия, новые сверху. Старый lessons_teacher_idx
-- (teacher_id, status) остаётся — им пользуются проверки видимости обучающихся.
CREATE INDEX lessons_teacher_created_idx ON lessons (teacher_id, created_at DESC, id DESC)
  WHERE teacher_id IS NOT NULL;

-- GET /scenarios: библиотека сценариев растёт генерацией, строка тяжёлая (легенда + эталон)
CREATE INDEX scenarios_created_idx ON scenarios (created_at DESC, id DESC);

-- GET /users: по ФИО (удалённые не показываются никогда)
CREATE INDEX users_name_idx ON users (last_name, first_name, id) WHERE deleted_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS users_name_idx;
DROP INDEX IF EXISTS scenarios_created_idx;
DROP INDEX IF EXISTS lessons_teacher_created_idx;
DROP INDEX IF EXISTS lessons_created_idx;
