package evaluation

import (
	"bytes"
	"encoding/json"
	"fmt"

	"lct/gocore/internal/model"
)

// SQL пакета. Правила (DESIGN §3): константный текст (pgx кэширует prepared statements),
// явные колонки, один round-trip на операцию, где это возможно.

// ---------------------------------------------------------------- старт оценки

// sqlStart — всё, что нужно для слоёв fields/timing и payload'ов AI-задач, одним запросом:
// попытка + настройки занятия + легенда и категория сценария + зафиксированная версия эталона.
// EXISTS по уникальному индексу evaluations(attempt_id) — идемпотентность повторного submit.
const sqlStart = `
SELECT a.id, a.lesson_id, a.user_id, a.scenario_id, a.etalon_id, a.mode, a.status, a.time_limit_sec,
       a.call_accepted_at, a.first_input_at, a.submitted_at, a.time_spent_ms, a.card, a.action_text,
       a.call_ended_at,
       l.status, l.settings,
       s.category_id, s.difficulty, s.call_script, c.code, c.name,
       et.card, et.card_draft, et.scoring, et.expected_actions, et.expected_dialogue,
       EXISTS (SELECT 1 FROM evaluations ev WHERE ev.attempt_id = a.id)
  FROM attempts a
  JOIN lessons l ON l.id = a.lesson_id
  JOIN scenarios s ON s.id = a.scenario_id
  JOIN classifier_categories c ON c.id = s.category_id
  JOIN etalons et ON et.id = a.etalon_id
 WHERE a.id = $1`

// sqlInsertEvaluation — ON CONFLICT DO NOTHING: повторный StartEvaluation (ретрай submit,
// гонка двух запросов) не создаёт вторую оценку и не ставит задачи повторно.
const sqlInsertEvaluation = `
INSERT INTO evaluations (id, attempt_id, etalon_id, status, fields_score, grammar_score, semantic_score,
                         timing_score, dialogue_score, total_score, verdict, field_errors, grammar_remarks,
                         grammar_stats, semantic, dialogue, timing, layers, weights, engine,
                         needs_review, ai_unavailable, evaluated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)
ON CONFLICT (attempt_id) DO NOTHING
RETURNING id`

// ---------------------------------------------------------------- чтение оценки

// evalSelect — общий список колонок для представления оценки: попытка и занятие (доступ,
// каналы WS, порог), категория (рекомендации), сама оценка, автор корректировки,
// чек-лист эталона (тексты пунктов разговора), XP по попытке и «running» из ai_jobs.
//
// running считается только для незавершённой оценки: CASE вычисляет подзапрос лениво,
// у готовой оценки (типичное чтение результата) обращения к ai_jobs нет вовсе.
const evalSelect = `
SELECT a.id, a.lesson_id, a.user_id, a.status, l.teacher_id, l.created_by, l.status, l.settings, c.name,
       e.id, e.etalon_id, e.status, e.fields_score, e.grammar_score, e.semantic_score, e.timing_score,
       e.dialogue_score, e.total_score, e.verdict, e.field_errors, e.grammar_remarks, e.grammar_stats,
       e.semantic, e.dialogue, e.timing, e.layers, e.weights, e.engine, e.needs_review, e.ai_unavailable,
       e.evaluated_at, e.override_score, e.override_verdict, e.override_reason, e.overridden_at,
       ou.last_name, ou.first_name, ou.middle_name,
       et.expected_dialogue,
       (SELECT COALESCE(sum(x.delta), 0)::int FROM xp_ledger x WHERE x.attempt_id = a.id),
       CASE WHEN e.status = 'partial' OR e.status = 'pending'
            THEN ARRAY(SELECT j.type FROM ai_jobs j
                        WHERE j.ref_type = 'attempt' AND j.ref_id = a.id AND j.status = 'running')
       END`

// sqlViewByAttempt — от попытки (LEFT JOIN оценки): одним запросом и проверка доступа
// (попытки нет → 404, чужая → 403), и «оценки ещё нет» (e.id IS NULL → 404).
const sqlViewByAttempt = evalSelect + `
  FROM attempts a
  JOIN lessons l ON l.id = a.lesson_id
  JOIN scenarios s ON s.id = a.scenario_id
  JOIN classifier_categories c ON c.id = s.category_id
  LEFT JOIN evaluations e ON e.attempt_id = a.id
  LEFT JOIN etalons et ON et.id = e.etalon_id
  LEFT JOIN users ou ON ou.id = e.overridden_by
 WHERE a.id = $1`

// sqlLockEvaluation — та же форма, но от оценки и под FOR UPDATE OF e: строка оценки —
// точка сериализации доезжающих слоёв (grammar и semantic могут прийти одновременно)
// и ручной корректировки. Блокируется только evaluations.
const sqlLockEvaluation = evalSelect + `
  FROM evaluations e
  JOIN attempts a ON a.id = e.attempt_id
  JOIN lessons l ON l.id = a.lesson_id
  JOIN scenarios s ON s.id = a.scenario_id
  JOIN classifier_categories c ON c.id = s.category_id
  JOIN etalons et ON et.id = e.etalon_id
  LEFT JOIN users ou ON ou.id = e.overridden_by
 WHERE e.attempt_id = $1
   FOR UPDATE OF e`

// ---------------------------------------------------------------- изменение оценки

// sqlSaveEvaluation — всё, что меняется при доезде AI-слоя/его провале и финализации.
// field_errors/timing/weights после старта не меняются — не переписываются.
const sqlSaveEvaluation = `
UPDATE evaluations
   SET status = $2, grammar_score = $3, semantic_score = $4, dialogue_score = $5, total_score = $6,
       verdict = $7, grammar_remarks = $8, grammar_stats = $9, semantic = $10, dialogue = $11,
       layers = $12, engine = $13, needs_review = $14, ai_unavailable = $15, evaluated_at = $16
 WHERE id = $1`

// sqlOverride — ручная корректировка (CHECK в схеме требует автора и причину).
const sqlOverride = `
UPDATE evaluations
   SET override_score = $2, override_verdict = $3, override_reason = $4, overridden_by = $5, overridden_at = $6
 WHERE id = $1`

// ---------------------------------------------------------------- финализация

// sqlFinalizeAttempt — попытка → evaluated и XP за неё одним оператором. Идемпотентность XP —
// частичный уникальный индекс xp_ledger_once_per_attempt (attempt_id, reason); RETURNING
// отдаёт только реально добавленные строки (для xpEarned без повторного чтения).
const sqlFinalizeAttempt = `
WITH att AS (
  UPDATE attempts SET status = 'evaluated'
   WHERE id = $1 AND (status = 'submitted' OR status = 'evaluating')
  RETURNING id
), xp AS (
  INSERT INTO xp_ledger (user_id, delta, reason, attempt_id, lesson_id)
  SELECT $2::uuid, t.d, t.r, $1::uuid, $3::uuid FROM unnest($4::int[], $5::text[]) AS t(d, r)
  ON CONFLICT (attempt_id, reason) WHERE attempt_id IS NOT NULL DO NOTHING
  RETURNING delta
)
SELECT (SELECT count(*) FROM att)::int, COALESCE((SELECT sum(delta) FROM xp), 0)::int`

// sqlLockParticipant — сериализация «последней карточки» студента в занятии: две оценки
// одного студента, финализируемые параллельно, иначе обе увидели бы друг друга
// «ещё в проверке», и lesson_completed не начислился бы никому. Следующий оператор
// (sqlLessonCompleted) в READ COMMITTED берёт свежий снимок уже после получения блокировки.
const sqlLockParticipant = `
SELECT 1 FROM lesson_participants WHERE lesson_id = $1 AND user_id = $2 FOR UPDATE`

// sqlLessonCompleted — студент выполнил занятие: открытых карточек нет (кроме текущей,
// она уже evaluated в этой транзакции) и оценены все cards_per_student карточек. Тогда —
// XP lesson_completed (идемпотентно: xp_ledger_once_per_lesson) и участник finished.
// Возвращает строку участника, если его статус изменился (для participantStatus).
//
// Условие не зависит от того, когда доехала оценка: карточки, погашенные завершением
// занятия (expired), выполненными не считаются ни до, ни после finish. Прежнее «занятие
// не идёт» давало +XP, только если AI-слой доезжал после finish (карточка 2 уже expired),
// и не давало, если раньше (карточка 2 ещё открыта, а потом её гасил finish).
const sqlLessonCompleted = `
WITH cond AS (
  SELECT (NOT EXISTS (SELECT 1 FROM attempts o
                       WHERE o.lesson_id = $1 AND o.user_id = $2 AND o.id <> $3
                         AND o.status IN ('issued', 'in_progress', 'submitted', 'evaluating'))
          AND (SELECT count(*) FROM attempts o
                WHERE o.lesson_id = $1 AND o.user_id = $2
                  AND (o.id = $3 OR o.status = 'evaluated')) >= $4::bigint) AS done
), xp AS (
  INSERT INTO xp_ledger (user_id, delta, reason, lesson_id)
  SELECT $2::uuid, $5::int, 'lesson_completed', $1::uuid FROM cond WHERE cond.done AND $5::int > 0
  ON CONFLICT (user_id, lesson_id) WHERE reason = 'lesson_completed' DO NOTHING
  RETURNING delta
), part AS (
  UPDATE lesson_participants lp
     SET status = 'finished', finished_at = COALESCE(lp.finished_at, now())
    FROM cond
   WHERE cond.done AND lp.lesson_id = $1 AND lp.user_id = $2 AND lp.status <> 'finished'
  RETURNING lp.user_id, lp.status, lp.joined_at, lp.finished_at, lp.mic_ready
)
SELECT COALESCE((SELECT done FROM cond), false), COALESCE((SELECT sum(delta) FROM xp), 0)::int,
       p.user_id, p.status, p.joined_at, p.finished_at, p.mic_ready,
       u.last_name, u.first_name, u.middle_name,
       (SELECT o.id FROM attempts o WHERE o.lesson_id = $1 AND o.user_id = $2 ORDER BY o.seq_no DESC LIMIT 1)
  FROM (SELECT 1) AS one
  LEFT JOIN part p ON true
  LEFT JOIN users u ON u.id = p.user_id`

// sqlSyncPassBonus — pass_bonus готовой оценки после ручной корректировки ($4 — положенный
// бонус; 0 — не положен): строка добавляется, пересчитывается или снимается. Возвращает
// новую сумму XP по попытке: остальные начисления читаются снимком этого оператора (он
// начат уже под блокировкой оценки — видит всё, что добавила финализация), бонус — $4.
const sqlSyncPassBonus = `
WITH del AS (
  DELETE FROM xp_ledger WHERE attempt_id = $1 AND reason = 'pass_bonus' AND $4::int <= 0
), ins AS (
  INSERT INTO xp_ledger (user_id, delta, reason, attempt_id, lesson_id)
  SELECT $2::uuid, $4::int, 'pass_bonus', $1::uuid, $3::uuid WHERE $4::int > 0
  ON CONFLICT (attempt_id, reason) WHERE attempt_id IS NOT NULL DO UPDATE SET delta = EXCLUDED.delta
)
SELECT (COALESCE((SELECT sum(x.delta) FROM xp_ledger x WHERE x.attempt_id = $1 AND x.reason <> 'pass_bonus'), 0)
        + GREATEST($4::int, 0))::int`

// ---------------------------------------------------------------- комментарии преподавателя

// sqlFeedbackByAttempt — доступ и список одним запросом: строка попытки есть всегда
// (LEFT JOIN комментариев), комментариев может не быть (f.id IS NULL). $2 = показывать
// скрытые от студента (преподаватель/админ).
const sqlFeedbackByAttempt = `
SELECT a.user_id, l.teacher_id, l.created_by,
       f.id, f.field, f.comment, f.recommendation, f.created_at,
       u.last_name, u.first_name, u.middle_name
  FROM attempts a
  JOIN lessons l ON l.id = a.lesson_id
  LEFT JOIN teacher_feedback f ON f.attempt_id = a.id AND ($2::bool OR f.is_visible_to_student)
  LEFT JOIN users u ON u.id = f.teacher_id
 WHERE a.id = $1
 ORDER BY f.created_at, f.id`

// sqlAddFeedback — вставка только для владельца занятия (или админа: $7) одним оператором;
// нет строки — попытки нет или чужая (разбор — sqlAttemptAccess).
const sqlAddFeedback = `
WITH src AS (
  SELECT a.id, a.lesson_id
    FROM attempts a
    JOIN lessons l ON l.id = a.lesson_id
   WHERE a.id = $2 AND ($7::bool OR COALESCE(l.teacher_id, l.created_by) = $3::uuid)
), ins AS (
  INSERT INTO teacher_feedback (id, attempt_id, teacher_id, field, comment, recommendation)
  SELECT $1::uuid, src.id, $3::uuid, $4::text, $5::text, $6::text FROM src
  RETURNING created_at
)
SELECT src.lesson_id, ins.created_at FROM src, ins`

// sqlAttemptAccess — минимум для проверки доступа (разбор отказа 404/403).
const sqlAttemptAccess = `
SELECT a.user_id, l.teacher_id, l.created_by
  FROM attempts a
  JOIN lessons l ON l.id = a.lesson_id
 WHERE a.id = $1`

// ---------------------------------------------------------------- jsonb

// defaultLessonSettings — чем добиваются отсутствующие ключи lessons.settings (как store:
// дефолты миграций, а не живой снимок — go-core всегда пишет настройки занятия целиком).
var defaultLessonSettings = model.DefaultLessonSettings(nil)

// jsonInto — jsonb прямо из буфера драйвера в dst (без промежуточной копии []byte).
// NULL оставляет dst нетронутым: вызывающий заранее кладёт туда значение по умолчанию.
type jsonInto struct {
	dst any
	col string
}

func (s *jsonInto) ScanBytes(b []byte) error {
	if b == nil {
		return nil
	}
	if err := json.Unmarshal(b, s.dst); err != nil {
		return fmt.Errorf("evaluation: jsonb %s: %w", s.col, err)
	}
	return nil
}

// rawCopy — копия jsonb (буфер драйвера переиспользуется). NULL -> nil.
type rawCopy struct{ dst *json.RawMessage }

func (s *rawCopy) ScanBytes(b []byte) error {
	if b == nil {
		*s.dst = nil
		return nil
	}
	*s.dst = append(json.RawMessage(nil), b...)
	return nil
}

// settingsInto — lessons.settings с добивкой дефолтами; кривое значение не роняет чтение
// (ParseLessonSettings всегда возвращает пригодный результат).
type settingsInto struct{ dst *model.LessonSettings }

func (s *settingsInto) ScanBytes(b []byte) error {
	*s.dst, _ = model.ParseLessonSettings(b, defaultLessonSettings)
	return nil
}

// expectedDialogueInto — etalons.expected_dialogue: '{}' / NULL -> nil (чек-листа нет).
type expectedDialogueInto struct{ dst **model.ExpectedDialogue }

func (s *expectedDialogueInto) ScanBytes(b []byte) error {
	if isEmptyJSON(b) {
		*s.dst = nil
		return nil
	}
	d := new(model.ExpectedDialogue)
	if err := json.Unmarshal(b, d); err != nil {
		return fmt.Errorf("evaluation: jsonb etalons.expected_dialogue: %w", err)
	}
	d.Normalize()
	*s.dst = d
	return nil
}

// isEmptyJSON — NULL, null, {} или [] (дефолты колонок '{}' / '[]').
func isEmptyJSON(b []byte) bool {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return true
	}
	switch string(b) {
	case "null", "{}", "[]":
		return true
	}
	return false
}

// Значения по умолчанию jsonb-колонок evaluations (NOT NULL DEFAULT ...).
var (
	jsonEmptyObject = json.RawMessage(`{}`)
	jsonEmptyArray  = json.RawMessage(`[]`)
)

// orDefault — raw или значение по умолчанию колонки (в NOT NULL jsonb нельзя писать NULL).
func orDefault(raw, def json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return def
	}
	return raw
}
