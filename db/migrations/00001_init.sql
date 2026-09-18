-- +goose Up
-- ============================================================================
-- Тренажёр оператора ДДС — схема БД v2 (PostgreSQL 12+)
--
-- Обоснование каждого решения: docs/db-design.md. Кратко о принципах:
--   * PK — uuid; значения генерирует приложение (UUIDv7 — монотонные по времени,
--     btree-индексы не пухнут от случайных вставок). DEFAULT gen_random_uuid() — страховка.
--   * журналы (audit_log, attempt_events, xp_ledger) — bigint identity: дешевле и быстрее uuid
--   * timestamptz везде; created_at / updated_at на изменяемых таблицах
--   * справочные значения — text + CHECK (менять дешевле, чем ENUM)
--   * формы карточек / эталонов / результатов — jsonb по JSON-схемам из docs/contracts/;
--     реляционно — только то, по чему ищем, фильтруем и считаем статистику
--   * ничего не удаляем физически: archived_at / deleted_at (ТЗ: аудит, сохранность результатов)
--   * горячий путь записи (1000 ops/s с запасом): события батчами, черновики в отдельной
--     таблице с fillfactor 50, идемпотентность по client_seq, BRIN для журналов
--   * audit_log партиционирован помесячно: ретеншн ≥6 мес (ТЗ) — это DROP PARTITION, не DELETE
-- ============================================================================

CREATE EXTENSION IF NOT EXISTS pgcrypto;   -- gen_random_uuid() в PG12 (в PG13+ встроено)
CREATE EXTENSION IF NOT EXISTS citext;     -- регистронезависимый логин

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
  NEW.updated_at = now();
  RETURN NEW;
END
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- ----------------------------------------------------------------------------
-- 1. Справочник служб (ДДС и экстренные оперативные службы)
--    Нужен дважды: профиль обучающегося ("профильные события", ТЗ стр. 14)
--    и список оповещения в карточке происшествия.
-- ----------------------------------------------------------------------------

CREATE TABLE services (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  code       text NOT NULL UNIQUE,                             -- '01'..'04' — экстренные; остальные — городские ДДС
  name       text NOT NULL,
  kind       text NOT NULL DEFAULT 'city' CHECK (kind IN ('emergency', 'city')),
  is_active  boolean NOT NULL DEFAULT true,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- ----------------------------------------------------------------------------
-- 2. Пользователи, аутентификация, группы
-- ----------------------------------------------------------------------------

CREATE TABLE users (
  id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  login                citext NOT NULL,                        -- уникальность — частичным индексом ниже (soft delete)
  password_hash        text NOT NULL,                          -- argon2id; никогда не plaintext
  auth_source          text NOT NULL DEFAULT 'local' CHECK (auth_source IN ('local', 'ldap')),
                                                               -- ТЗ: интеграция с локальной системой управления доступом
  role                 text NOT NULL CHECK (role IN ('admin', 'teacher', 'student')),
  last_name            text NOT NULL,
  first_name           text NOT NULL,
  middle_name          text,                                   -- отчество может отсутствовать
  service_id           uuid REFERENCES services (id),          -- профиль ДДС обучающегося; NULL — универсальный / не студент
  status               text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'blocked')),
  blocked_reason       text,
  must_change_password boolean NOT NULL DEFAULT true,          -- первый вход по временному паролю
  failed_login_count   int NOT NULL DEFAULT 0,                 -- защита от перебора
  locked_until         timestamptz,
  password_changed_at  timestamptz,
  last_login_at        timestamptz,
  consent_given_at     timestamptz,                            -- согласие на обработку ПДн (ТЗ: защита ПДн)
  created_by           uuid REFERENCES users (id),
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now(),
  deleted_at           timestamptz                             -- soft delete: результаты обучения остаются
);
-- логин уникален среди живых: удалённый Иванов не блокирует логин новому Иванову
CREATE UNIQUE INDEX users_login_active_idx ON users (login) WHERE deleted_at IS NULL;
CREATE INDEX users_role_idx ON users (role) WHERE deleted_at IS NULL;
CREATE TRIGGER users_updated_at BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Сессии (refresh-токены). Access-токен — короткоживущий JWT, в БД не хранится.
-- Нужны для "аутентификация при каждом входе", отзыва при блокировке и восстановления WS-сессии.
CREATE TABLE auth_sessions (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id            uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  refresh_token_hash text NOT NULL UNIQUE,
  ip                 inet,
  user_agent         text,
  created_at         timestamptz NOT NULL DEFAULT now(),
  expires_at         timestamptz NOT NULL,
  revoked_at         timestamptz
);
CREATE INDEX auth_sessions_user_idx ON auth_sessions (user_id) WHERE revoked_at IS NULL;

CREATE TABLE groups (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name        text NOT NULL,
  description text,
  teacher_id  uuid NOT NULL REFERENCES users (id),             -- владелец группы
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  archived_at timestamptz
);
CREATE TRIGGER groups_updated_at BEFORE UPDATE ON groups FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE group_members (
  group_id  uuid NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
  user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  joined_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (group_id, user_id)
);
CREATE INDEX group_members_user_idx ON group_members (user_id);

-- ----------------------------------------------------------------------------
-- 3. Классификатор происшествий (импорт из XLSX организаторов)
-- ----------------------------------------------------------------------------

CREATE TABLE classifier_categories (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  code       text NOT NULL UNIQUE,                             -- код из классификатора
  parent_id  uuid REFERENCES classifier_categories (id),
  name       text NOT NULL,
  depth      smallint NOT NULL DEFAULT 1,                      -- 1 = раздел, 2 = категория, 3 = подкатегория
                                                               -- (не "level": уровни геймификации — таблица levels)
  services   jsonb NOT NULL DEFAULT '[]',                      -- коды services.code: кого оповещать ["01","03","gormost"]
  attributes jsonb NOT NULL DEFAULT '[]',                      -- признаки опросной карты, характерные для категории
  is_active  boolean NOT NULL DEFAULT true,
  extra      jsonb NOT NULL DEFAULT '{}',                      -- остальные колонки XLSX как есть
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX classifier_parent_idx ON classifier_categories (parent_id);
CREATE TRIGGER classifier_updated_at BEFORE UPDATE ON classifier_categories FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ----------------------------------------------------------------------------
-- 4. Сценарии, эталоны, кэш озвучки
-- ----------------------------------------------------------------------------

-- Сценарий = "звонок заявителя" + служебные атрибуты. Эталон вынесен отдельно и версионируется:
-- преподаватель правит эталон, а выставленные оценки ссылаются на версию, по которой считались.
CREATE TABLE scenarios (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  title            text NOT NULL,
  category_id      uuid NOT NULL REFERENCES classifier_categories (id),
  difficulty       smallint NOT NULL DEFAULT 1 CHECK (difficulty BETWEEN 1 AND 3),   -- ТЗ: классификация по сложности
  mode             text NOT NULL DEFAULT 'both' CHECK (mode IN ('cards', 'card_actions', 'both')),
  source           text NOT NULL CHECK (source IN ('generated', 'manual', 'ticket', 'student')),
                                                               -- generated: нейросеть; ticket: билеты организаторов;
                                                               -- student: карточка, сформированная учеником (режим 2, ТЗ стр. 15)
  status           text NOT NULL DEFAULT 'draft'
                   CHECK (status IN ('draft', 'generated', 'validated', 'rejected', 'archived')),
  call_script      jsonb NOT NULL DEFAULT '{}',                -- по scenario.schema.json: заявитель, адрес, реплики[]
                                                               -- (реплика может ссылаться на tts_cache.text_hash)
  author_id        uuid REFERENCES users (id),
  validated_by     uuid REFERENCES users (id),
  validated_at     timestamptz,
  teacher_comment  text,                                       -- "контекстное поле" для коррекции генерации (ТЗ стр. 14)
  generation_meta  jsonb NOT NULL DEFAULT '{}',                -- {model, prompt_version, temperature, duration_ms, job_id}
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  archived_at      timestamptz
);
CREATE INDEX scenarios_category_idx ON scenarios (category_id);
CREATE INDEX scenarios_status_idx ON scenarios (status) WHERE archived_at IS NULL;
CREATE TRIGGER scenarios_updated_at BEFORE UPDATE ON scenarios FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Эталоны иммутабельны: правка = новая версия. Оценки всегда объяснимы задним числом.
CREATE TABLE etalons (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  scenario_id      uuid NOT NULL REFERENCES scenarios (id) ON DELETE CASCADE,
  version          int NOT NULL DEFAULT 1,
  is_current       boolean NOT NULL DEFAULT true,
  card             jsonb NOT NULL,                             -- эталонная карточка по etalon.schema.json (режим "карточки")
  expected_actions jsonb NOT NULL DEFAULT '[]',                -- эталон режима "действия с карточками":
                                                               -- [{action_text, required_facts[], forbidden_facts[]}]
  scoring          jsonb NOT NULL DEFAULT '{}',                -- переопределение весов / обязательных полей для сценария
  created_by       uuid REFERENCES users (id),
  created_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (scenario_id, version)
);
CREATE UNIQUE INDEX etalons_current_idx ON etalons (scenario_id) WHERE is_current;

-- Глобальный кэш озвучки (Silero): дедупликация по содержимому, без привязки к сценарию.
-- Одинаковая реплика в двух сценариях ("Алло, вы меня слышите?") озвучивается один раз.
CREATE TABLE tts_cache (
  text_hash    text PRIMARY KEY,                               -- sha256(text | voice | rate)
  voice        text NOT NULL,
  rate         numeric(3,2) NOT NULL DEFAULT 1.0,
  file_path    text NOT NULL,                                  -- относительный путь в volume tts_cache (wav/ogg)
  duration_ms  int,
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz NOT NULL DEFAULT now()              -- обновляется лениво (батчем), для LRU-очистки диска
);

-- ----------------------------------------------------------------------------
-- 5. Уровни геймификации
-- ----------------------------------------------------------------------------

-- Справочник уровней по накопленному XP. Текущий XP — sum(xp_ledger.delta),
-- уровень — max(no) where xp_required <= xp. Редактируется администратором.
CREATE TABLE levels (
  no          smallint PRIMARY KEY CHECK (no > 0),
  title       text NOT NULL,
  xp_required int NOT NULL UNIQUE CHECK (xp_required >= 0),
  badge       text,                                            -- имя иконки для фронта
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER levels_updated_at BEFORE UPDATE ON levels FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ----------------------------------------------------------------------------
-- 6. Занятия
-- ----------------------------------------------------------------------------

-- Занятие — единственный контекст выдачи карточек. kind='class' — занятие с преподавателем,
-- kind='practice' — самостоятельная тренировка (создаётся студентом, преподавателя нет).
-- Одна модель попыток на оба случая; никакого nullable lesson_id в attempts.
CREATE TABLE lessons (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  kind            text NOT NULL DEFAULT 'class' CHECK (kind IN ('class', 'practice')),
  title           text NOT NULL,
  teacher_id      uuid REFERENCES users (id),                  -- NULL только для practice
  created_by      uuid NOT NULL REFERENCES users (id),         -- кто создал (для practice — сам студент)
  group_id        uuid REFERENCES groups (id),                 -- NULL, если участники назначены поимённо
  mode            text NOT NULL CHECK (mode IN ('cards', 'card_actions')),
  scenario_source text NOT NULL DEFAULT 'generated'
                  CHECK (scenario_source IN ('generated', 'student', 'mixed')),   -- ТЗ стр. 15, режим 2
  difficulty      smallint CHECK (difficulty BETWEEN 1 AND 3), -- NULL = любая
  time_limit_sec  int NOT NULL DEFAULT 30,                     -- норматив (ТЗ: 30 с по умолчанию, задаёт преподаватель)
  settings        jsonb NOT NULL DEFAULT '{}',                 -- {pass_threshold, weights{fields,semantic,grammar,timing},
                                                               --  grammar_strict, cards_per_student, allow_replay}
  status          text NOT NULL DEFAULT 'draft'
                  CHECK (status IN ('draft', 'scheduled', 'running', 'finished', 'cancelled')),
  scheduled_at    timestamptz,
  started_at      timestamptz,
  finished_at     timestamptz,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  CHECK (kind = 'practice' OR teacher_id IS NOT NULL)
);
CREATE INDEX lessons_teacher_idx ON lessons (teacher_id, status) WHERE teacher_id IS NOT NULL;
CREATE INDEX lessons_group_idx ON lessons (group_id);
CREATE TRIGGER lessons_updated_at BEFORE UPDATE ON lessons FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Выбранные преподавателем категории (множественный выбор, ТЗ стр. 14)
CREATE TABLE lesson_categories (
  lesson_id   uuid NOT NULL REFERENCES lessons (id) ON DELETE CASCADE,
  category_id uuid NOT NULL REFERENCES classifier_categories (id),
  PRIMARY KEY (lesson_id, category_id)
);

-- Пул сценариев занятия (из него ученикам выдаются случайные карточки)
CREATE TABLE lesson_scenarios (
  lesson_id   uuid NOT NULL REFERENCES lessons (id) ON DELETE CASCADE,
  scenario_id uuid NOT NULL REFERENCES scenarios (id),
  sort_order  int NOT NULL DEFAULT 0,
  PRIMARY KEY (lesson_id, scenario_id)
);

-- Участники занятия и их live-состояние (для мониторинга преподавателем)
CREATE TABLE lesson_participants (
  lesson_id    uuid NOT NULL REFERENCES lessons (id) ON DELETE CASCADE,
  user_id      uuid NOT NULL REFERENCES users (id),
  status       text NOT NULL DEFAULT 'assigned'
               CHECK (status IN ('assigned', 'joined', 'active', 'disconnected', 'finished')),
  joined_at    timestamptz,
  finished_at  timestamptz,
  last_seen_at timestamptz,                                    -- последний WS-пинг; для "student.disconnected"
  PRIMARY KEY (lesson_id, user_id)
);
CREATE INDEX lesson_participants_user_idx ON lesson_participants (user_id);

-- ----------------------------------------------------------------------------
-- 7. Попытки: карточка, выданная ученику, и что он с ней сделал
-- ----------------------------------------------------------------------------

-- fillfactor 85: строка обновляется на каждом переходе статуса — оставляем место под HOT-update,
-- чтобы не трогать 3 индекса таблицы на каждый UPDATE.
CREATE TABLE attempts (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  lesson_id        uuid NOT NULL REFERENCES lessons (id),
  user_id          uuid NOT NULL REFERENCES users (id),
  scenario_id      uuid NOT NULL REFERENCES scenarios (id),
  etalon_id        uuid NOT NULL REFERENCES etalons (id),      -- версия эталона зафиксирована на момент выдачи
  mode             text NOT NULL CHECK (mode IN ('cards', 'card_actions')),
  seq_no           int NOT NULL,                               -- порядковый номер карточки ученика в занятии
  status           text NOT NULL DEFAULT 'issued'
                   CHECK (status IN ('issued', 'in_progress', 'submitted', 'evaluating', 'evaluated', 'expired', 'aborted')),
  time_limit_sec   int NOT NULL,                               -- копия норматива занятия на момент выдачи
  -- тайминг: не одна длительность, а метки, из которых считается всё остальное
  issued_at        timestamptz NOT NULL DEFAULT now(),         -- карточка выдана / звонок "пошёл"
  call_accepted_at timestamptz,                                -- ученик снял трубку → старт таймера
  first_input_at   timestamptz,                                -- первое изменение поля (время реакции)
  submitted_at     timestamptz,
  time_spent_ms    int CHECK (time_spent_ms IS NULL OR time_spent_ms >= 0),
                                                               -- submitted_at - call_accepted_at, денормализовано для отчётов
  replay_count     int NOT NULL DEFAULT 0,                     -- сколько раз переспрашивал заявителя
  card             jsonb,                                      -- итоговая карточка по incident_card.schema.json (режим 1)
  action_text      text,                                       -- текст действия с карточкой (режим 2)
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (lesson_id, user_id, seq_no)
) WITH (fillfactor = 85);
CREATE INDEX attempts_user_idx ON attempts (user_id, submitted_at DESC);
CREATE INDEX attempts_lesson_idx ON attempts (lesson_id, status);
CREATE INDEX attempts_scenario_idx ON attempts (scenario_id);
CREATE TRIGGER attempts_updated_at BEFORE UPDATE ON attempts FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Черновик (автосохранение, ТЗ стр. 12) — отдельно от attempts: самый частый UPDATE в системе
-- не должен раздувать таблицу с 3 индексами. Здесь индексов нет, fillfactor 50 → почти все
-- обновления HOT, vacuum успевает. Пишется приложением с ON CONFLICT (upsert), без триггера.
CREATE TABLE attempt_drafts (
  attempt_id uuid PRIMARY KEY REFERENCES attempts (id) ON DELETE CASCADE,
  data       jsonb NOT NULL DEFAULT '{}',
  updated_at timestamptz NOT NULL DEFAULT now()
) WITH (fillfactor = 50);

-- Поток событий попытки — источник истины для live-мониторинга, восстановления после обрыва
-- и аналитики "где ученик тормозит". Go-ядро пишет батчами (multi-values INSERT раз в 50–100 мс).
CREATE TABLE attempt_events (
  id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  attempt_id uuid NOT NULL REFERENCES attempts (id) ON DELETE CASCADE,
  client_seq bigint,                                           -- сквозной номер события от клиента:
                                                               -- идемпотентность при reconnect ≤30 c (ТЗ) — без дублей
  type       text NOT NULL,                                    -- issued | call_accepted | field_changed | replay |
                                                               -- submitted | timer_expired | disconnected | reconnected
  payload    jsonb NOT NULL DEFAULT '{}',                      -- {field, value} для field_changed и т.п.
  at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX attempt_events_attempt_idx ON attempt_events (attempt_id, at);
CREATE UNIQUE INDEX attempt_events_dedup_idx ON attempt_events (attempt_id, client_seq) WHERE client_seq IS NOT NULL;
CREATE INDEX attempt_events_at_brin ON attempt_events USING brin (at);  -- ретеншн/выборки по времени без btree-цены

-- ----------------------------------------------------------------------------
-- 8. Результаты проверки, ревью, рекомендации, прогресс
-- ----------------------------------------------------------------------------

CREATE TABLE evaluations (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  attempt_id      uuid NOT NULL UNIQUE REFERENCES attempts (id) ON DELETE CASCADE,
  etalon_id       uuid NOT NULL REFERENCES etalons (id),       -- по какой версии реально считали; при переоценке
                                                               -- может отличаться от attempts.etalon_id — это осознанно
  status          text NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending', 'partial', 'done', 'failed')),   -- partial: поля посчитаны, AI-слои ещё нет
  -- слои (0..100 каждый) и итог по весам занятия
  fields_score    numeric(5,2),
  grammar_score   numeric(5,2),
  semantic_score  numeric(5,2),
  timing_score    numeric(5,2),
  total_score     numeric(5,2) NOT NULL DEFAULT 0,
  verdict         text NOT NULL DEFAULT 'pending' CHECK (verdict IN ('pending', 'pass', 'fail')),
  -- детализация по evaluation_result.schema.json
  field_errors    jsonb NOT NULL DEFAULT '[]',                 -- [{field, expected, actual, kind: missing|wrong|extra, weight}]
  grammar_remarks jsonb NOT NULL DEFAULT '[]',                 -- [{offset, length, message, rule, suggestions[]}]
  semantic        jsonb NOT NULL DEFAULT '{}',                 -- {score, remarks[], missing_facts[], extra_facts[]}
  timing          jsonb NOT NULL DEFAULT '{}',                 -- {spent_ms, limit_sec, delta_ms, within_norm, reaction_ms}
  engine          jsonb NOT NULL DEFAULT '{}',                 -- {rules_version, llm_model, prompt_version, lt_version} —
                                                               -- воспроизводимость оценки для QA модели
  evaluated_at    timestamptz,
  -- ручная корректировка преподавателем: только с причиной и записью в audit_log (ТЗ стр. 11)
  override_score  numeric(5,2),
  override_verdict text CHECK (override_verdict IN ('pass', 'fail')),
  override_reason text,
  overridden_by   uuid REFERENCES users (id),
  overridden_at   timestamptz,
  final_score     numeric(5,2) GENERATED ALWAYS AS (COALESCE(override_score, total_score)) STORED,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  CHECK (override_score IS NULL OR (overridden_by IS NOT NULL AND override_reason IS NOT NULL))
);
CREATE INDEX evaluations_status_idx ON evaluations (status) WHERE status <> 'done';
CREATE TRIGGER evaluations_updated_at BEFORE UPDATE ON evaluations FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Ревью: комментарии преподавателя к попытке (ТЗ: "обратная связь через интерфейс")
CREATE TABLE teacher_feedback (
  id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  attempt_id            uuid NOT NULL REFERENCES attempts (id) ON DELETE CASCADE,
  teacher_id            uuid NOT NULL REFERENCES users (id),
  field                 text,                                  -- NULL = ко всей попытке; иначе путь поля карточки
                                                               -- ("address", "attributes.injured") — фронт подсвечивает
  comment               text NOT NULL,
  recommendation        text,                                  -- "что подтянуть"
  is_visible_to_student boolean NOT NULL DEFAULT true,
  acknowledged_at       timestamptz,                           -- студент отметил "прочитано"
  created_at            timestamptz NOT NULL DEFAULT now(),
  updated_at            timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX teacher_feedback_attempt_idx ON teacher_feedback (attempt_id);
CREATE TRIGGER teacher_feedback_updated_at BEFORE UPDATE ON teacher_feedback FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Рекомендации системы и преподавателя (ТЗ стр. 11–12: "аналитические рекомендации от системы",
-- "видеть рекомендации системы по улучшению навыков"). evidence — объяснимость: из чего вывод.
CREATE TABLE recommendations (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  scope       text NOT NULL CHECK (scope IN ('student', 'group')),
  user_id     uuid REFERENCES users (id),
  group_id    uuid REFERENCES groups (id),
  source      text NOT NULL CHECK (source IN ('system', 'teacher')),
  kind        text NOT NULL CHECK (kind IN ('weak_category', 'weak_field', 'slow_timing', 'grammar_pattern', 'general')),
  category_id uuid REFERENCES classifier_categories (id),
  field       text,
  body        text NOT NULL,
  evidence    jsonb NOT NULL DEFAULT '{}',                     -- {attempt_ids[], error_rate, avg_time_ms, sample_errors[]}
  created_by  uuid REFERENCES users (id),                      -- NULL = system
  created_at  timestamptz NOT NULL DEFAULT now(),
  dismissed_at timestamptz,
  CHECK ((scope = 'student' AND user_id IS NOT NULL AND group_id IS NULL)
      OR (scope = 'group'   AND group_id IS NOT NULL AND user_id IS NULL))
);
CREATE INDEX recommendations_user_idx  ON recommendations (user_id)  WHERE dismissed_at IS NULL AND user_id IS NOT NULL;
CREATE INDEX recommendations_group_idx ON recommendations (group_id) WHERE dismissed_at IS NULL AND group_id IS NOT NULL;

-- Опыт (XP): append-only журнал начислений. Текущий XP = sum(delta); уровень — по levels.
-- Не счётчик в users: журнал можно пересчитать, объяснить ученику, и он не расходится с фактами.
CREATE TABLE xp_ledger (
  id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id    uuid NOT NULL REFERENCES users (id),              -- без CASCADE: журнал не удаляем вслед за кем-либо
  delta      int NOT NULL,
  reason     text NOT NULL CHECK (reason IN ('attempt_evaluated', 'within_norm', 'lesson_completed', 'streak', 'manual')),
  attempt_id uuid REFERENCES attempts (id) ON DELETE SET NULL,
  lesson_id  uuid REFERENCES lessons (id) ON DELETE SET NULL,
  created_by uuid REFERENCES users (id),                       -- для manual
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX xp_ledger_user_idx ON xp_ledger (user_id);
CREATE UNIQUE INDEX xp_ledger_once_per_attempt ON xp_ledger (attempt_id, reason) WHERE attempt_id IS NOT NULL;

-- ----------------------------------------------------------------------------
-- 9. Витрины прогресса (VIEW: на целевых объёмах считаются за миллисекунды,
--    граница пересмотра в docs/db-design.md, Р15)
-- ----------------------------------------------------------------------------

CREATE VIEW student_progress AS
WITH xp AS (
  SELECT user_id, sum(delta)::int AS xp FROM xp_ledger GROUP BY user_id
)
SELECT
  u.id                                                                   AS user_id,
  count(a.id) FILTER (WHERE a.status = 'evaluated')                      AS attempts_done,
  round(avg(e.final_score), 1)                                           AS avg_score,
  round(avg(e.final_score) FILTER (WHERE e.evaluated_at > now() - interval '30 days'), 1)
                                                                         AS avg_score_30d,
  round(100.0 * count(e.id) FILTER (WHERE COALESCE(e.override_verdict, e.verdict) = 'pass')
        / NULLIF(count(e.id), 0), 1)                                     AS pass_rate_pct,
  round(avg(a.time_spent_ms) FILTER (WHERE a.status = 'evaluated'))      AS avg_time_ms,
  count(e.id) FILTER (WHERE (e.timing ->> 'within_norm')::boolean)       AS within_norm_count,
  COALESCE(x.xp, 0)                                                      AS xp,
  (SELECT l.no FROM levels l WHERE l.xp_required <= COALESCE(x.xp, 0)
   ORDER BY l.xp_required DESC LIMIT 1)                                  AS level_no,
  max(a.submitted_at)                                                    AS last_activity_at
FROM users u
LEFT JOIN xp x          ON x.user_id = u.id
LEFT JOIN attempts a    ON a.user_id = u.id
LEFT JOIN evaluations e ON e.attempt_id = a.id AND e.status = 'done'
WHERE u.role = 'student' AND u.deleted_at IS NULL
GROUP BY u.id, x.xp;

-- Успеваемость по категориям происшествий — "в каких типах слаб" (тепловая карта, рекомендации)
CREATE VIEW student_category_stats AS
SELECT
  a.user_id,
  s.category_id,
  count(a.id) FILTER (WHERE a.status = 'evaluated')                      AS attempts_done,
  round(avg(e.final_score), 1)                                           AS avg_score,
  round(100.0 * count(e.id) FILTER (WHERE COALESCE(e.override_verdict, e.verdict) = 'pass')
        / NULLIF(count(e.id), 0), 1)                                     AS pass_rate_pct,
  round(avg(a.time_spent_ms) FILTER (WHERE a.status = 'evaluated'))      AS avg_time_ms,
  max(a.submitted_at)                                                    AS last_attempt_at
FROM attempts a
JOIN scenarios s        ON s.id = a.scenario_id
LEFT JOIN evaluations e ON e.attempt_id = a.id AND e.status = 'done'
GROUP BY a.user_id, s.category_id;

-- Ошибки по полям в разрезе ученика — для "истории ошибок" и тепловых карт
CREATE VIEW student_field_errors AS
SELECT
  a.user_id,
  s.category_id,
  fe ->> 'field' AS field,
  fe ->> 'kind'  AS kind,
  count(*)       AS cnt
FROM evaluations e
JOIN attempts a  ON a.id = e.attempt_id
JOIN scenarios s ON s.id = a.scenario_id
CROSS JOIN LATERAL jsonb_array_elements(e.field_errors) fe
WHERE e.status = 'done'
GROUP BY a.user_id, s.category_id, fe ->> 'field', fe ->> 'kind';

-- ----------------------------------------------------------------------------
-- 10. Служебные: очередь AI, импорт, отчёты, материалы, настройки, бэкапы
-- ----------------------------------------------------------------------------

-- Очередь запросов к ai-service в самой PG (SKIP LOCKED): генерация и AI-слои проверки
-- не блокируют HTTP-запрос и переживают рестарт. Отдельный брокер в изолированном контуре — лишний компонент.
CREATE TABLE ai_jobs (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  type         text NOT NULL CHECK (type IN ('generate_scenario', 'evaluate_semantic', 'evaluate_grammar', 'tts')),
  status       text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'done', 'failed', 'cancelled')),
  priority     smallint NOT NULL DEFAULT 5,                    -- проверка на занятии важнее фоновой генерации
  run_after    timestamptz NOT NULL DEFAULT now(),             -- экспоненциальный backoff при ретраях:
                                                               -- не долбим упавший Ollama без пауз
  dedup_key    text UNIQUE,                                    -- защита от дублей (напр. tts:{text_hash})
  ref_type     text,                                           -- scenario | attempt | evaluation
  ref_id       uuid,
  payload      jsonb NOT NULL,
  result       jsonb,
  error        text,
  try_count    int NOT NULL DEFAULT 0,
  max_tries    int NOT NULL DEFAULT 3,
  locked_by    text,                                           -- инстанс go-core; зависшие (locked_at старее N мин) — в requeue
  locked_at    timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now(),
  started_at   timestamptz,
  finished_at  timestamptz
);
CREATE INDEX ai_jobs_queue_idx ON ai_jobs (priority, run_after) WHERE status = 'queued';
CREATE INDEX ai_jobs_ref_idx ON ai_jobs (ref_type, ref_id);

-- Пакетный импорт (ТЗ, опциональные: "механизм импорта обновлений в ручном режиме"):
-- классификатор XLSX, билеты PDF, материалы. История запусков + статистика для отчёта админу.
CREATE TABLE import_batches (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  kind        text NOT NULL CHECK (kind IN ('classifier', 'tickets', 'scenarios', 'materials')),
  file_name   text NOT NULL,
  status      text NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'done', 'failed')),
  stats       jsonb NOT NULL DEFAULT '{}',                     -- {rows_total, inserted, updated, skipped, errors[]}
  error       text,
  created_by  uuid NOT NULL REFERENCES users (id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz
);

CREATE TABLE reports (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  type         text NOT NULL CHECK (type IN ('lesson', 'student', 'group', 'period')),
  format       text NOT NULL CHECK (format IN ('csv', 'xlsx', 'pdf')),   -- ТЗ: выгрузка в Excel и PDF
  lesson_id    uuid REFERENCES lessons (id),
  user_id      uuid REFERENCES users (id),                     -- субъект отчёта (ученик)
  group_id     uuid REFERENCES groups (id),
  params       jsonb NOT NULL DEFAULT '{}',                    -- период, фильтры
  status       text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'done', 'failed')),
  file_path    text,
  generated_by uuid NOT NULL REFERENCES users (id),
  created_at   timestamptz NOT NULL DEFAULT now(),
  finished_at  timestamptz
);
CREATE INDEX reports_lesson_idx ON reports (lesson_id);

-- Справочная база / методические материалы (ТЗ: "загружать дополнительные ресурсы")
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

-- Настройки платформы: норматив по умолчанию, пороги, веса, голос TTS, правила XP
CREATE TABLE settings (
  key         text PRIMARY KEY,
  value       jsonb NOT NULL,
  description text,
  updated_by  uuid REFERENCES users (id),
  updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE backups (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  started_at   timestamptz NOT NULL DEFAULT now(),
  finished_at  timestamptz,
  status       text NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'done', 'failed')),
  file_path    text,
  size_bytes   bigint,
  sha256       text,
  error        text,
  triggered_by uuid REFERENCES users (id)                      -- NULL = по расписанию
);

-- ----------------------------------------------------------------------------
-- 11. Журнал аудита — партиционирован помесячно.
--     Ретеншн ≥6 мес (ТЗ) = DROP PARTITION (мгновенно, без vacuum-долга), а не DELETE.
--     Новые партиции создаёт планировщик go-core (ежемесячный job); DEFAULT-партиция — страховка,
--     чтобы запись не потерялась, даже если job не отработал.
--     ВАЖНО (контракт приложения): before/after редактируются ДО записи —
--     password_hash и токены в журнал попадать не должны.
-- ----------------------------------------------------------------------------

CREATE TABLE audit_log (
  id          bigint GENERATED ALWAYS AS IDENTITY,
  at          timestamptz NOT NULL DEFAULT now(),
  actor_id    uuid,                                            -- NULL = система (планировщик, очередь); без FK —
                                                               -- журнал не должен зависеть от жизни строк users
  actor_role  text,
  action      text NOT NULL,                                   -- user.login | user.block | scenario.validate |
                                                               -- evaluation.override | lesson.start | ...
  entity_type text,
  entity_id   uuid,
  lesson_id   uuid,                                            -- для выборки "что происходило на занятии"
  before      jsonb,
  after       jsonb,
  ip          inet,
  user_agent  text,
  request_id  uuid,
  PRIMARY KEY (at, id)                                         -- включает ключ партиционирования; даёт и индекс по at
) PARTITION BY RANGE (at);
CREATE INDEX audit_log_actor_idx  ON audit_log (actor_id, at);
CREATE INDEX audit_log_entity_idx ON audit_log (entity_type, entity_id);

-- +goose StatementBegin
DO $$
DECLARE
  m timestamptz;
BEGIN
  -- партиции на год вперёд; дальше — ежемесячный job в go-core (границы — в TZ сервера)
  FOR m IN SELECT generate_series(date '2026-09-01', date '2027-08-01', interval '1 month')
  LOOP
    EXECUTE format(
      'CREATE TABLE audit_log_%s PARTITION OF audit_log FOR VALUES FROM (%L) TO (%L)',
      to_char(m, 'YYYY_MM'), m, m + interval '1 month'
    );
  END LOOP;
END
$$;
-- +goose StatementEnd

CREATE TABLE audit_log_default PARTITION OF audit_log DEFAULT;

-- ----------------------------------------------------------------------------
-- 12. Сиды: службы, уровни, настройки по умолчанию
-- ----------------------------------------------------------------------------

INSERT INTO services (code, name, kind) VALUES
  ('01',          'Пожарно-спасательная служба',            'emergency'),
  ('02',          'Полиция',                                'emergency'),
  ('03',          'Скорая медицинская помощь',              'emergency'),
  ('04',          'Аварийная служба газовой сети',          'emergency'),
  ('zhkh',        'ДДС ЖКХ / управляющие организации',      'city'),
  ('vodokanal',   'АО «Мосводоканал»',                      'city'),
  ('mosvodostok', 'ГУП «Мосводосток»',                      'city'),
  ('moskollektor','ГУП «Москоллектор»',                     'city'),
  ('gormost',     'ГБУ «Гормост»',                          'city'),
  ('oek',         'АО «ОЭК» (электросети)',                 'city'),
  ('codd',        'ЦОДД',                                   'city');

INSERT INTO levels (no, title, xp_required, badge) VALUES
  (1, 'Новичок',           0,    'novice'),
  (2, 'Стажёр',            100,  'trainee'),
  (3, 'Диспетчер',         300,  'dispatcher'),
  (4, 'Оператор',          600,  'operator'),
  (5, 'Старший оператор',  1000, 'senior'),
  (6, 'Наставник',         1500, 'mentor');

INSERT INTO settings (key, value, description) VALUES
  ('time_limit_sec',   '30',   'Норматив заполнения карточки по умолчанию, секунд (ТЗ)'),
  ('pass_threshold',   '70',   'Порог зачёта попытки, баллов из 100'),
  ('score_weights',    '{"fields": 0.5, "semantic": 0.25, "grammar": 0.1, "timing": 0.15}', 'Веса слоёв проверки в итоговом балле'),
  ('timing_tolerance', '{"soft_pct": 20, "hard_pct": 100}', 'Превышение норматива: до soft — без штрафа, до hard — линейный штраф, дальше 0'),
  ('xp_rules',         '{"attempt_evaluated": 10, "within_norm": 5, "lesson_completed": 30, "pass_bonus_per_10_points": 1}', 'Начисление XP'),
  ('tts',              '{"voice": "xenia", "rate": 1.0, "sample_rate": 24000}', 'Параметры озвучки заявителя (Silero)'),
  ('audit_retention_days', '365', 'Срок хранения журнала аудита (ТЗ: не менее 180); партиции старше — DROP');

-- +goose Down
DROP VIEW IF EXISTS student_field_errors;
DROP VIEW IF EXISTS student_category_stats;
DROP VIEW IF EXISTS student_progress;
DROP TABLE IF EXISTS audit_log, backups, settings, materials, reports, import_batches, ai_jobs,
  xp_ledger, recommendations, teacher_feedback, evaluations, attempt_events, attempt_drafts, attempts,
  lesson_participants, lesson_scenarios, lesson_categories, lessons, levels,
  tts_cache, etalons, scenarios, classifier_categories,
  group_members, groups, auth_sessions, users, services CASCADE;
DROP FUNCTION IF EXISTS set_updated_at();
