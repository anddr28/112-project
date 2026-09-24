-- +goose Up
-- ============================================================================
-- 00002 — голосовой режим (docs/voice-mode.md §6) + то, что нужно go-core поверх схемы v2.
-- Обоснования — docs/db-design.md, раздел «Миграция 00002».
--
--   * ai_jobs.type += evaluate_dialogue; индекс для reaper'а (running по locked_at)
--   * evaluations: слой dialogue, флаги ревью/недоступности AI, состояние слоёв, веса итога,
--     статистика грамматики
--   * attempt_dialogue_turns — транскрипт разговора (аудио оператора НЕ хранится)
--   * attempts: номер происшествия (sequence), завершение разговора
--   * etalons: эталонная карточка в форме АРМ (card_draft) и чек-лист разговора
--   * scenarios: версии сценария (parent_id, version) — POST /scenarios/{id}/versions
--   * classifier_categories: синонимы, справка, «частые/значимые», правила служб
--   * services: краткое имя и реальные коды АРМ-112 (101..104) — GAP-08
--   * users: номер АРМ и операторский номер; lesson_participants.mic_ready
--   * xp_ledger: причина pass_bonus, идемпотентность lesson_completed
--   * настройки голосового режима и эксплуатации
-- ============================================================================

-- ---------------------------------------------------------------- очередь AI
ALTER TABLE ai_jobs DROP CONSTRAINT ai_jobs_type_check;
ALTER TABLE ai_jobs ADD CONSTRAINT ai_jobs_type_check
  CHECK (type IN ('generate_scenario', 'evaluate_semantic', 'evaluate_grammar', 'evaluate_dialogue', 'tts'));
-- reaper: running старше N минут -> queued; таблица маленькая, индекс частичный и крошечный
CREATE INDEX ai_jobs_running_idx ON ai_jobs (locked_at) WHERE status = 'running';

-- ---------------------------------------------------------------- оценка
ALTER TABLE evaluations
  ADD COLUMN dialogue_score numeric(5,2),
  ADD COLUMN dialogue       jsonb   NOT NULL DEFAULT '{}',   -- DialogueResult (snake_case, как пришёл от ai-service)
  ADD COLUMN grammar_stats  jsonb   NOT NULL DEFAULT '{}',   -- GrammarResult.stats: {words_checked, errors_by_severity}
  ADD COLUMN layers         jsonb   NOT NULL DEFAULT '{}',   -- {"grammar":"queued|running|done|failed|skipped", ...}
  ADD COLUMN weights        jsonb   NOT NULL DEFAULT '{}',   -- веса, по которым посчитан total_score (UI их показывает)
  ADD COLUMN needs_review   boolean NOT NULL DEFAULT false,  -- confidence < порога или AI-слой не доехал
  ADD COLUMN ai_unavailable boolean NOT NULL DEFAULT false;  -- хотя бы один AI-слой окончательно failed
CREATE INDEX evaluations_review_idx ON evaluations (updated_at) WHERE needs_review;

-- ---------------------------------------------------------------- попытки
CREATE SEQUENCE incident_no_seq START 36814852;             -- номер происшествия в шапке карточки АРМ
ALTER TABLE attempts
  ADD COLUMN incident_no     bigint NOT NULL DEFAULT nextval('incident_no_seq'),
  ADD COLUMN call_ended_at   timestamptz,                    -- разговор завершён (голосовой режим)
  ADD COLUMN call_end_reason text CHECK (call_end_reason IN
             ('operator_hung_up', 'caller_hung_up', 'max_turns', 'submitted', 'timeout')),
  ADD COLUMN dialogue_turns  int NOT NULL DEFAULT 0;         -- денормализованный счётчик реплик для Attempt.dialogue
ALTER SEQUENCE incident_no_seq OWNED BY attempts.incident_no;

-- Транскрипт разговора. turn_no — номер ОБМЕНА, как его видит фронт (DialogueTurnView.turnNo):
-- 0 — вступительная реплика заявителя; k >= 1 — k-я реплика оператора и ответ заявителя на неё.
-- Поэтому PK включает speaker. Повтор хода после обрыва (тот же turnNo) упирается в PK —
-- идемпотентность без отдельного ключа. Реплики "нет речи" (no_speech) не сохраняются.
CREATE TABLE attempt_dialogue_turns (
  attempt_id        uuid NOT NULL REFERENCES attempts (id) ON DELETE CASCADE,
  turn_no           int  NOT NULL CHECK (turn_no >= 0),
  speaker           text NOT NULL CHECK (speaker IN ('operator', 'caller')),
  text              text NOT NULL,
  source            text NOT NULL CHECK (source IN ('stt', 'text', 'script', 'llm')),
  confidence        real,                                    -- уверенность STT (operator)
  audio_path        text,                                    -- только caller: путь в volume tts_cache
  duration_ms       int,
  at                timestamptz NOT NULL DEFAULT now(),
  at_ms             int NOT NULL DEFAULT 0,                  -- смещение от attempts.call_accepted_at
  emotional_state   text,
  revealed_fact_ids text[] NOT NULL DEFAULT '{}',            -- caller: какие DialogueFact.id раскрыты этой репликой
  meta              jsonb NOT NULL DEFAULT '{}',             -- engine, latency_ms, fallback, text_raw, is_unknown_answer
  PRIMARY KEY (attempt_id, turn_no, speaker)
);

-- ---------------------------------------------------------------- эталоны и сценарии
ALTER TABLE etalons
  ADD COLUMN card_draft        jsonb NOT NULL DEFAULT '{}',  -- эталон в форме карточки АРМ (IncidentCardDraft, camelCase);
                                                             -- с ним go-core сверяет карточку студента (слой 1)
  ADD COLUMN expected_dialogue jsonb NOT NULL DEFAULT '{}';  -- ExpectedDialogue (чек-лист разговора), snake_case

ALTER TABLE scenarios
  ADD COLUMN parent_id uuid REFERENCES scenarios (id),       -- предыдущая версия сценария
  ADD COLUMN version   int NOT NULL DEFAULT 1 CHECK (version >= 1);
CREATE INDEX scenarios_parent_idx ON scenarios (parent_id) WHERE parent_id IS NOT NULL;
CREATE INDEX lesson_scenarios_scenario_idx ON lesson_scenarios (scenario_id);   -- inUse / lessonsCount

-- ---------------------------------------------------------------- классификатор и службы
ALTER TABLE classifier_categories
  ADD COLUMN synonyms      text[] NOT NULL DEFAULT '{}',     -- поиск по Инструкции п.4.2
  ADD COLUMN reference     text,                             -- справочная страница (markdown); NULL — нет
  ADD COLUMN featured      text CHECK (featured IN ('frequent', 'significant')),
  ADD COLUMN sort_order    int NOT NULL DEFAULT 0,
  ADD COLUMN service_rules jsonb NOT NULL DEFAULT '[]';      -- [{attr?, any_of?[], services:[{code, primary, reason}]}]

ALTER TABLE services ADD COLUMN short_name text;
-- GAP-08: реальный АРМ-112 использует коды 101..104
UPDATE services SET code = '101', short_name = 'Служба 101' WHERE code = '01';
UPDATE services SET code = '102', short_name = 'Служба 102' WHERE code = '02';
UPDATE services SET code = '103', short_name = 'Служба 103' WHERE code = '03';
UPDATE services SET code = '104', short_name = 'Служба 104' WHERE code = '04';
UPDATE services SET short_name = name WHERE short_name IS NULL;
ALTER TABLE services ALTER COLUMN short_name SET NOT NULL;

-- ---------------------------------------------------------------- люди
ALTER TABLE users
  ADD COLUMN workstation text,                               -- номер АРМ для шапки ("АРМ 4")
  ADD COLUMN operator_no text;                               -- операторский номер ("оп. 227") — подпись в истории статусов
ALTER TABLE auth_sessions ADD COLUMN last_seen_at timestamptz;
CREATE INDEX auth_sessions_expires_idx ON auth_sessions (expires_at);

ALTER TABLE lesson_participants ADD COLUMN mic_ready boolean NOT NULL DEFAULT false;

-- ---------------------------------------------------------------- XP и рекомендации
ALTER TABLE xp_ledger DROP CONSTRAINT xp_ledger_reason_check;
ALTER TABLE xp_ledger ADD CONSTRAINT xp_ledger_reason_check
  CHECK (reason IN ('attempt_evaluated', 'within_norm', 'pass_bonus', 'lesson_completed', 'streak', 'manual'));
CREATE UNIQUE INDEX xp_ledger_once_per_lesson ON xp_ledger (user_id, lesson_id)
  WHERE reason = 'lesson_completed';

ALTER TABLE recommendations DROP CONSTRAINT recommendations_kind_check;
ALTER TABLE recommendations ADD CONSTRAINT recommendations_kind_check
  CHECK (kind IN ('weak_category', 'weak_field', 'slow_timing', 'grammar_pattern', 'dialogue_pattern', 'general'));

-- ---------------------------------------------------------------- настройки
UPDATE settings SET value = '{"fields": 0.5, "semantic": 0.25, "grammar": 0.1, "timing": 0.15, "dialogue": 0}'
 WHERE key = 'score_weights';
INSERT INTO settings (key, value, description) VALUES
  ('dialogue_weight_default', '0.25', 'Вес слоя разговора, если голос включён, а вес не задан (остальные слои ужимаются пропорционально)'),
  ('voice', '{"enabled": false, "input": "voice", "push_to_talk": true, "max_turns": 12, "tts_enabled": true}',
            'Голосовой режим занятия по умолчанию (lessons.settings.voice)'),
  ('stt', '{"confidence_floor": 0.6}', 'Реплики с уверенностью STT ниже порога не считаются ошибками оператора'),
  ('confidence_threshold', '0.7', 'Ниже — оценка AI помечается «требует ревью преподавателя»'),
  ('cards_per_student', '1', 'Карточек на обучающегося в занятии по умолчанию'),
  ('allow_replay', 'true', 'Разрешить «переспросить заявителя» по умолчанию'),
  ('ai', '{"reaper_after_sec": 900, "max_tries": 3, "dialog_timeout_sec": 20, "breaker_open_sec": 30, "breaker_failures": 3}',
         'Эксплуатация очереди AI: переотправка зависших, попытки, таймауты, circuit breaker'),
  ('login', '{"max_failed": 5, "lock_minutes": 15, "session_ttl_hours": 12}', 'Защита входа и срок сессии'),
  ('backup', '{"enabled": true, "hour": 3, "keep": 14}', 'Резервное копирование: час запуска (время сервера) и сколько копий хранить'),
  ('events_retention_days', '365', 'Срок хранения событий попыток (attempt_events), дней')
ON CONFLICT (key) DO NOTHING;

-- +goose Down
DELETE FROM settings WHERE key IN ('dialogue_weight_default', 'voice', 'stt', 'confidence_threshold',
  'cards_per_student', 'allow_replay', 'ai', 'login', 'backup', 'events_retention_days');
UPDATE settings SET value = '{"fields": 0.5, "semantic": 0.25, "grammar": 0.1, "timing": 0.15}' WHERE key = 'score_weights';

ALTER TABLE recommendations DROP CONSTRAINT recommendations_kind_check;
ALTER TABLE recommendations ADD CONSTRAINT recommendations_kind_check
  CHECK (kind IN ('weak_category', 'weak_field', 'slow_timing', 'grammar_pattern', 'general'));
DROP INDEX IF EXISTS xp_ledger_once_per_lesson;
ALTER TABLE xp_ledger DROP CONSTRAINT xp_ledger_reason_check;
ALTER TABLE xp_ledger ADD CONSTRAINT xp_ledger_reason_check
  CHECK (reason IN ('attempt_evaluated', 'within_norm', 'lesson_completed', 'streak', 'manual'));

ALTER TABLE lesson_participants DROP COLUMN mic_ready;
DROP INDEX IF EXISTS auth_sessions_expires_idx;
ALTER TABLE auth_sessions DROP COLUMN last_seen_at;
ALTER TABLE users DROP COLUMN workstation, DROP COLUMN operator_no;

UPDATE services SET code = '01' WHERE code = '101';
UPDATE services SET code = '02' WHERE code = '102';
UPDATE services SET code = '03' WHERE code = '103';
UPDATE services SET code = '04' WHERE code = '104';
ALTER TABLE services DROP COLUMN short_name;

ALTER TABLE classifier_categories DROP COLUMN synonyms, DROP COLUMN reference, DROP COLUMN featured,
  DROP COLUMN sort_order, DROP COLUMN service_rules;

DROP INDEX IF EXISTS lesson_scenarios_scenario_idx;
DROP INDEX IF EXISTS scenarios_parent_idx;
ALTER TABLE scenarios DROP COLUMN parent_id, DROP COLUMN version;
ALTER TABLE etalons DROP COLUMN card_draft, DROP COLUMN expected_dialogue;

DROP TABLE IF EXISTS attempt_dialogue_turns;
ALTER TABLE attempts DROP COLUMN incident_no, DROP COLUMN call_ended_at, DROP COLUMN call_end_reason,
  DROP COLUMN dialogue_turns;
DROP SEQUENCE IF EXISTS incident_no_seq;

DROP INDEX IF EXISTS evaluations_review_idx;
ALTER TABLE evaluations DROP COLUMN dialogue_score, DROP COLUMN dialogue, DROP COLUMN grammar_stats,
  DROP COLUMN layers, DROP COLUMN weights, DROP COLUMN needs_review, DROP COLUMN ai_unavailable;

DROP INDEX IF EXISTS ai_jobs_running_idx;
ALTER TABLE ai_jobs DROP CONSTRAINT ai_jobs_type_check;
ALTER TABLE ai_jobs ADD CONSTRAINT ai_jobs_type_check
  CHECK (type IN ('generate_scenario', 'evaluate_semantic', 'evaluate_grammar', 'tts'));
