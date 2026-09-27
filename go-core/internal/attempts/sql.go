package attempts

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/reaction"
	"lct/gocore/internal/store"
)

// SQL — константный текст (pgx кэширует prepared statements по тексту), колонки явно.
//
// Блокировки:
//   - попытку пакет блокирует FOR NO KEY UPDATE, а не FOR UPDATE (как store.LockAttempt).
//     Причина — group commit событий: INSERT в attempt_events проверяет FK на attempts через
//     FOR KEY SHARE, который конфликтует только с FOR UPDATE. Со слабой блокировкой пачка
//     событий всех студентов не ждёт коммита чужой транзакции submit/accept/служб;
//   - порядок захвата везде один: занятие → попытка → черновик/участник. accept и submit
//     сначала берут занятие FOR SHARE (как IssueNext): завершение занятия (lessons.finish:
//     занятие → попытки → участники) иначе взаимно блокировалось бы со сдачей карточки
//     (попытка → занятие в IssueNext → участник) ровно в момент «время вышло, все сдают».
const (
	// Занятие попытки под FOR SHARE (первым оператором пакета, до замка попытки).
	sqlLockLessonOfAttempt = `
SELECT l.status FROM lessons l
 WHERE l.id = (SELECT a.lesson_id FROM attempts a WHERE a.id = $1)
   FOR SHARE`

	// Метаданные для горячих путей (cache.go).
	sqlMeta = `
SELECT a.user_id, a.lesson_id, COALESCE(l.teacher_id, l.created_by), a.status, a.first_input_at IS NOT NULL
  FROM attempts a
  JOIN lessons l ON l.id = a.lesson_id
 WHERE a.id = $1`

	// Черновик на чтение + право доступа одним запросом.
	sqlGetDraft = `
SELECT a.user_id, COALESCE(l.teacher_id, l.created_by), d.data
  FROM attempts a
  JOIN lessons l ON l.id = a.lesson_id
  LEFT JOIN attempt_drafts d ON d.attempt_id = a.id
 WHERE a.id = $1`

	// Легенда для студента: доступ, статус попытки, настройки занятия, call_script и файлы
	// озвучки его реплик (tts_cache по PK) — одним запросом. jsonb_path_query в lax-режиме не
	// падает на кривой форме turns (объект, null) — просто ничего не находит. Именно
	// "= ANY(ARRAY(...))", а не "IN (SELECT ...)": у функции оценка 1000 строк, и с IN
	// планировщик уходит в hash join с seq scan по всему tts_cache (проверено EXPLAIN на
	// 5000 строк); с ANY — PK index scan.
	sqlCallScript = `
SELECT a.user_id, COALESCE(l.teacher_id, l.created_by), a.status, l.settings, s.call_script,
       t.hashes, t.paths, t.durations, a.service_id IS NOT NULL
  FROM attempts a
  JOIN lessons l ON l.id = a.lesson_id
  JOIN scenarios s ON s.id = a.scenario_id
  CROSS JOIN LATERAL (
        SELECT array_agg(c.text_hash) AS hashes,
               array_agg(c.file_path) AS paths,
               array_agg(COALESCE(c.duration_ms, 0)) AS durations
          FROM tts_cache c
         WHERE c.text_hash = ANY (ARRAY(SELECT h #>> '{}' FROM jsonb_path_query(s.call_script, '$.turns[*].tts_hash') AS h))
       ) t
 WHERE a.id = $1`

	// ---- accept-call (после sqlLockLessonOfAttempt)
	sqlLockAccept = `
SELECT a.user_id, a.lesson_id, COALESCE(l.teacher_id, l.created_by), a.status, a.first_input_at IS NOT NULL,
       COALESCE(s.call_script->'caller'->>'phone', ''),
       sv.id IS NOT NULL, COALESCE(sv.code, '')
  FROM attempts a
  JOIN lessons l ON l.id = a.lesson_id
  JOIN scenarios s ON s.id = a.scenario_id
  LEFT JOIN services sv ON sv.id = a.service_id
 WHERE a.id = $1
   FOR NO KEY UPDATE OF a`

	sqlAccept = `
UPDATE attempts SET status = 'in_progress', call_accepted_at = $2
 WHERE id = $1 AND status = 'issued'`

	// Инструкция п.4.1: при приёме вызова поле АОН заполняется автоматически. Черновик обычно
	// уже есть (пустая карточка от IssueNext) — дописываем phones.aon, не трогая остальное;
	// отсутствующие ключи верхнего уровня добиваются пустой карточкой ($2), чтобы фронт
	// никогда не получил карточку без address/flags/services.
	sqlAcceptDraft = `
INSERT INTO attempt_drafts AS d (attempt_id, data, updated_at)
VALUES ($1, $2::jsonb, $4)
ON CONFLICT (attempt_id) DO UPDATE
   SET data = jsonb_set(
                CASE WHEN jsonb_typeof(d.data) = 'object' THEN $2::jsonb || d.data ELSE $2::jsonb END,
                '{phones}',
                CASE WHEN jsonb_typeof(d.data->'phones') = 'object' THEN d.data->'phones' ELSE '{}'::jsonb END
                  || jsonb_build_object('aon', $3::text)),
       updated_at = EXCLUDED.updated_at`

	// ---- submit (после sqlLockLessonOfAttempt)
	sqlLockSubmit = `
SELECT a.user_id, a.lesson_id, a.status, a.mode, a.call_accepted_at, a.service_id IS NOT NULL
  FROM attempts a
 WHERE a.id = $1
   FOR NO KEY UPDATE`

	// Ракурс dds: сдаётся карточка с сервера (поля 112 и статусы служб — только серверные).
	sqlLockDraftData = `SELECT d.data FROM attempt_drafts d WHERE d.attempt_id = $1 FOR UPDATE`

	sqlSubmit = `
UPDATE attempts
   SET card = $2::jsonb, action_text = $3, submitted_at = $4, time_spent_ms = $5, status = 'evaluating'
 WHERE id = $1 AND status = 'in_progress'`

	sqlUpsertDraft = `
INSERT INTO attempt_drafts (attempt_id, data, updated_at) VALUES ($1, $2::jsonb, $3)
ON CONFLICT (attempt_id) DO UPDATE SET data = EXCLUDED.data, updated_at = EXCLUDED.updated_at`

	// ---- replay: владелец, статус и разрешение занятия — в самом UPDATE. Отсутствие ключа
	// allow_replay = разрешено (дефолт settings.allow_replay = true, как у store).
	sqlReplay = `
UPDATE attempts a SET replay_count = a.replay_count + 1
  FROM lessons l
 WHERE a.id = $1 AND a.user_id = $2 AND a.status = 'in_progress' AND l.id = a.lesson_id
   AND l.settings->'allow_replay' IS DISTINCT FROM 'false'::jsonb
   AND a.service_id IS NULL
RETURNING a.replay_count, a.lesson_id`

	sqlReplayDiagnose = `
SELECT a.user_id, COALESCE(l.teacher_id, l.created_by), a.status,
       l.settings->'allow_replay' IS DISTINCT FROM 'false'::jsonb,
       a.service_id IS NOT NULL
  FROM attempts a
  JOIN lessons l ON l.id = a.lesson_id
 WHERE a.id = $1`

	// ---- время первого ввода (время реакции). CTE: UPDATE только при NULL, и в том же
	// запросе — «уже проставлено раньше?» (снимок до UPDATE), чтобы кэш узнал истину за один
	// round-trip и не повторял пустой UPDATE на каждом батче. Время не раньше приёма вызова:
	// часы клиента могут отставать от серверных.
	sqlFirstInput = `
WITH u AS (
  UPDATE attempts SET first_input_at = GREATEST($2::timestamptz, call_accepted_at)
   WHERE id = $1 AND first_input_at IS NULL AND call_accepted_at IS NOT NULL
  RETURNING 1)
SELECT EXISTS (SELECT 1 FROM u)
    OR EXISTS (SELECT 1 FROM attempts WHERE id = $1 AND first_input_at IS NOT NULL)`

	// В транзакции служб (строка попытки уже заблокирована, first_input_at известен как NULL).
	// call_accepted_at IS NOT NULL — как в sqlFirstInput: GREATEST пропускает NULL, и ввод до
	// приёма вызова дал бы first_input_at раньше старта таймера (время реакции 0 навсегда).
	sqlFirstInputTx = `
UPDATE attempts SET first_input_at = GREATEST($2::timestamptz, call_accepted_at)
 WHERE id = $1 AND first_input_at IS NULL AND call_accepted_at IS NOT NULL`

	// ---- службы карточки (attempt_drafts.data.services): попытка, затем черновик — под замком.
	sqlLockServices = `
SELECT a.user_id, a.lesson_id, a.status, a.first_input_at IS NOT NULL,
       sv.id IS NOT NULL, COALESCE(sv.code, '')
  FROM attempts a
  LEFT JOIN services sv ON sv.id = a.service_id
 WHERE a.id = $1
   FOR NO KEY UPDATE OF a`

	sqlLockDraftServices = `
SELECT d.data->'services' FROM attempt_drafts d WHERE d.attempt_id = $1 FOR UPDATE`

	sqlEnsureDraft = `
INSERT INTO attempt_drafts (attempt_id, data) VALUES ($1, $2::jsonb) ON CONFLICT (attempt_id) DO NOTHING`

	// Меняется только ключ services: остальная карточка (и неизвестные поля) — как была.
	sqlSetServices = `
UPDATE attempt_drafts SET data = jsonb_set(data, '{services}', $2::jsonb), updated_at = $3
 WHERE attempt_id = $1`

	// ---- участник занятия (+ ФИО и последняя попытка — для WS participantStatus).
	participantReturning = `
RETURNING p.user_id, u.last_name, u.first_name, COALESCE(u.middle_name, ''), p.status,
          p.joined_at, p.finished_at, p.mic_ready,
          (SELECT a.id FROM attempts a WHERE a.lesson_id = p.lesson_id AND a.user_id = p.user_id
            ORDER BY a.seq_no DESC LIMIT 1)`

	sqlParticipantActive = `
UPDATE lesson_participants p
   SET status = 'active', joined_at = COALESCE(p.joined_at, $3)
  FROM users u
 WHERE p.lesson_id = $1 AND p.user_id = $2 AND p.status <> 'finished' AND u.id = p.user_id` + participantReturning

	sqlParticipantFinished = `
UPDATE lesson_participants p
   SET status = 'finished', finished_at = COALESCE(p.finished_at, $3)
  FROM users u
 WHERE p.lesson_id = $1 AND p.user_id = $2 AND p.status <> 'finished' AND u.id = p.user_id` + participantReturning

	sqlParticipantMic = `
UPDATE lesson_participants p
   SET mic_ready = true
  FROM users u
 WHERE p.lesson_id = $1 AND p.user_id = $2 AND NOT p.mic_ready AND u.id = p.user_id` + participantReturning
)

// ---------------------------------------------------------------- службы: слияние записи клиента

// Службы карточки пишут двое (DESIGN §5): сервер — POST/DELETE /services и смена статуса
// (под замком попытки и черновика), и клиент — целиком с автосохранением (PUT /draft) и сдачей
// (submit): автоопределённые службы (resolve-services) живут только в карточке клиента.
// Автосохранение отправляет снимок, сделанный до ответа на действие со службой, поэтому
// слепая перезапись теряла добавленную службу или смену статуса (lost update).
//
// Правило слияния (одно для PUT и submit; зеркало семантики classifier.ResolveServices):
//   - статус и история службы — только серверные: у службы, которая есть и в черновике, и в
//     теле, берутся source/currentStatus/currentStatusAt/history/allowedNext/editable черновика,
//     а isPrimary/reason (их меняет автоопределение) и справочные поля — из тела;
//   - состав «закреплённых» служб (ручные — source=manual — и приступившие к реагированию,
//     статус дальше «Получена службой») — тоже серверный: клиент не может ни убрать такую
//     службу (снятие — только DELETE /services: 409 для приступивших), ни добавить (ручные
//     добавляет только POST /services). Закреплённая служба черновика, которой нет в теле, —
//     устаревший снимок: остаётся (в конце списка); закреплённая служба тела, которой нет в
//     черновике, — устаревший снимок после DELETE (или подделка): отбрасывается;
//   - состав остальных служб (auto/vis в «Добавлена|Получена службой») — клиентский:
//     автоопределение их добавляет и убирает, берётся из тела;
//   - порядок — как в теле, дубликаты по коду (без учёта регистра) и элементы без кода
//     отбрасываются.
//
// Выражение безопасно на любой форме jsonb (не массив, не объект, null — пусто) и не падает.
const servicesMergeTemplate = `
(SELECT COALESCE(jsonb_agg(m.v ORDER BY m.k, m.ord), '[]'::jsonb)
   FROM (
     SELECT 0 AS k, b.ord,
            CASE WHEN s.v IS NULL THEN b.v
                 ELSE b.v || (s.v - '{serviceId,code,name,shortName,isPrimary,reason}'::text[]) END AS v
       FROM (SELECT DISTINCT ON (lower(e.v->>'code')) e.v, e.ord
               FROM jsonb_array_elements({ARR_BODY}) WITH ORDINALITY AS e(v, ord)
              WHERE jsonb_typeof(e.v) = 'object' AND COALESCE(e.v->>'code', '') <> ''
              ORDER BY lower(e.v->>'code'), e.ord) b
       LEFT JOIN LATERAL (
              SELECT x.v FROM jsonb_array_elements({ARR_STORED}) WITH ORDINALITY AS x(v, ord)
               WHERE jsonb_typeof(x.v) = 'object' AND lower(x.v->>'code') = lower(b.v->>'code')
               ORDER BY x.ord LIMIT 1) s ON true
      WHERE s.v IS NOT NULL OR NOT {PINNED_B}
     UNION ALL
     SELECT 1, x.ord, x.v
       FROM jsonb_array_elements({ARR_STORED}) WITH ORDINALITY AS x(v, ord)
      WHERE jsonb_typeof(x.v) = 'object' AND COALESCE(x.v->>'code', '') <> '' AND {PINNED_X}
        AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements({ARR_BODY}) AS e(v)
                         WHERE jsonb_typeof(e.v) = 'object' AND lower(e.v->>'code') = lower(x.v->>'code'))
   ) m)`

// servicesMergeSQL — выражение «службы после записи клиента»: stored — службы черновика
// (SQL-выражение jsonb, может быть NULL), body — службы из тела запроса.
func servicesMergeSQL(stored, body string) string {
	arr := func(x string) string {
		return "CASE WHEN jsonb_typeof(" + x + ") = 'array' THEN " + x + " ELSE '[]'::jsonb END"
	}
	// «Закреплённая» служба: ручная или уже приступила к реагированию (reaction.CanRemove = false).
	pinned := func(v string) string {
		return "COALESCE(" + v + "->>'source' = 'manual' OR " + v + "->>'currentStatus' NOT IN ('" +
			string(reaction.StatusAdded) + "', '" + string(reaction.StatusReceived) + "'), false)"
	}
	return strings.NewReplacer(
		"{ARR_BODY}", arr(body),
		"{ARR_STORED}", arr(stored),
		"{PINNED_B}", pinned("b.v"),
		"{PINNED_X}", pinned("x.v"),
	).Replace(servicesMergeTemplate)
}

var (
	// Черновик: ОДИН оператор на самом частом запросе системы. Владелец и статус in_progress
	// (до приёма вызова карточку заполнять нельзя — иначе таймер обходится) проверяются в том
	// же запросе (строка attempts по PK). Тело уходит в jsonb без разбора в Go; ключ services
	// сливается с серверными службами черновика (servicesMergeSQL) — строка черновика при этом
	// под замком ON CONFLICT DO UPDATE, как и у действий со службами (FOR UPDATE), так что
	// слияние видит последнюю закоммиченную версию. Нет строки в RETURNING — не наша/нет/не в
	// работе (диагностика отдельным запросом только на этом редком пути).
	// Ракурс dds (у попытки есть служба обучающегося, attempts.service_id): карточку заполнил
	// оператор 112, диспетчер её не правит — из тела берётся только actionsTaken (черновик
	// текста действия, восстановление после обрыва), остальное (поля 112 и статусы служб)
	// остаётся серверным.
	sqlPutDraft = `
WITH a AS (
  SELECT a.id, a.service_id IS NOT NULL AS dds
    FROM attempts a
   WHERE a.id = $1 AND a.user_id = $2 AND a.status = 'in_progress')
INSERT INTO attempt_drafts AS d (attempt_id, data, updated_at)
SELECT a.id, jsonb_set($3::jsonb, '{services}', ` + servicesMergeSQL("NULL::jsonb", "$3::jsonb->'services'") + `), now()
  FROM a
ON CONFLICT (attempt_id) DO UPDATE
   SET data = CASE WHEN (SELECT a.dds FROM a)
                   THEN jsonb_set(d.data, '{actionsTaken}',
                          CASE WHEN jsonb_typeof($3::jsonb->'actionsTaken') = 'string'
                               THEN $3::jsonb->'actionsTaken' ELSE '""'::jsonb END)
                   ELSE jsonb_set($3::jsonb, '{services}', ` + servicesMergeSQL("d.data->'services'", "$3::jsonb->'services'") + `)
              END,
       updated_at = EXCLUDED.updated_at
RETURNING d.updated_at`

	// Службы сдаваемой карточки ($2 — из тела) после слияния с черновиком. Вызывается под
	// замком попытки (submit): действия со службами берут тот же замок первым, поэтому
	// службы черновика между чтением и записью карточки не меняются.
	sqlSubmitServices = `
SELECT ` + servicesMergeSQL("(SELECT d.data->'services' FROM attempt_drafts d WHERE d.attempt_id = $1)", "$2::jsonb")
)

// defaultLessonSettings — чем добиваются отсутствующие ключи lessons.settings (как в store).
var defaultLessonSettings = model.DefaultLessonSettings(nil)

// emptyDraftJSON — пустая карточка (зеркало emptyCard() фронта), закодированная один раз.
var emptyDraftJSON = func() json.RawMessage {
	b, err := json.Marshal(convert.EmptyDraft())
	if err != nil {
		panic("attempts: encode empty draft: " + err.Error())
	}
	return b
}()

// draftWithAON — пустая карточка с phones.aon (вставка черновика при приёме вызова).
func draftWithAON(phone string) (json.RawMessage, error) {
	d := convert.EmptyDraft()
	d.Phones.Aon = &phone
	return json.Marshal(d)
}

// loadMeta — метаданные попытки из БД (pgx.ErrNoRows — нет попытки).
func loadMeta(ctx context.Context, q pg.Querier, id uuid.UUID) (attemptMeta, error) {
	var m attemptMeta
	err := q.QueryRow(ctx, sqlMeta, id).Scan(&m.UserID, &m.LessonID, &m.OwnerID, &m.Status, &m.FirstInput)
	return m, err
}

// metaFor — метаданные из кэша, иначе из БД (с записью в кэш). Нет попытки — 404.
func (s *Service) metaFor(ctx context.Context, id uuid.UUID) (attemptMeta, error) {
	now := s.now()
	if m, ok := s.meta.get(id, now); ok {
		return m, nil
	}
	return s.refreshMeta(ctx, id)
}

// refreshMeta — свежие метаданные из БД мимо кэша (диагностика отказа, после чужих изменений).
func (s *Service) refreshMeta(ctx context.Context, id uuid.UUID) (attemptMeta, error) {
	m, err := loadMeta(ctx, s.pool, id)
	if err != nil {
		if pg.IsNoRows(err) {
			return m, errNotFound()
		}
		return m, fmt.Errorf("attempts: meta: %w", err)
	}
	s.meta.put(id, m, s.now())
	return m, nil
}

// ---------------------------------------------------------------- сканеры jsonb

// settingsScan — lessons.settings прямо из буфера драйвера с добивкой дефолтами (кривое
// значение не роняет чтение: ParseLessonSettings всегда даёт пригодный результат).
type settingsScan struct{ dst *model.LessonSettings }

func (s *settingsScan) ScanBytes(b []byte) error {
	*s.dst, _ = model.ParseLessonSettings(b, defaultLessonSettings)
	return nil
}

// callScriptScan — scenarios.call_script прямо из буфера драйвера.
type callScriptScan struct{ dst *model.CallScript }

func (s *callScriptScan) ScanBytes(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, s.dst); err != nil {
		return fmt.Errorf("attempts: scenarios.call_script: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- участник

// scanParticipant — результат одного из sqlParticipant* (RETURNING participantReturning)
// как участник для WS participantStatus. nil без ошибки — строка не изменилась (уже в нужном
// состоянии или участника нет). Принимает pgx.Row — работает и с QueryRow, и с pgx.Batch.
func scanParticipant(row pgx.Row) (*public.LessonParticipant, error) {
	var p store.ParticipantRow
	err := row.Scan(&p.UserID, &p.LastName, &p.FirstName, &p.MiddleName, &p.Status,
		&p.JoinedAt, &p.FinishedAt, &p.MicReady, &p.AttemptID)
	if err != nil {
		if pg.IsNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("attempts: participant: %w", err)
	}
	lp := store.ParticipantToPublic(&p) // ФИО → «Фамилия И. О.», времена → UTC
	return &lp, nil
}

// ---------------------------------------------------------------- замки accept/submit

// lockLessonThenAttempt — занятие попытки FOR SHARE, затем строка попытки (lockSQL с $1 = id)
// одним round-trip: pgx.Batch в транзакции выполняет операторы строго по порядку, так что
// порядок захвата «занятие → попытка» гарантирован. scan читает строку lockSQL.
// Нет попытки — 404.
func lockLessonThenAttempt(ctx context.Context, tx pgx.Tx, id uuid.UUID, lockSQL string, scan func(pgx.Row) error) (lessonStatus string, err error) {
	b := &pgx.Batch{}
	b.Queue(sqlLockLessonOfAttempt, id)
	b.Queue(lockSQL, id)
	br := tx.SendBatch(ctx, b)
	defer func() {
		if cerr := br.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("attempts: lock: %w", cerr)
		}
	}()
	if err = br.QueryRow().Scan(&lessonStatus); err != nil {
		if pg.IsNoRows(err) {
			return "", errNotFound()
		}
		return "", fmt.Errorf("attempts: lock lesson: %w", err)
	}
	if err = scan(br.QueryRow()); err != nil {
		if pg.IsNoRows(err) {
			return "", errNotFound()
		}
		return "", fmt.Errorf("attempts: lock attempt: %w", err)
	}
	return lessonStatus, nil
}

// ---------------------------------------------------------------- пакетный захват служб

// lockServices — попытка и её черновик под замком одним round-trip (pgx.Batch в
// транзакции: операторы выполняются по порядку — сначала attempts, затем attempt_drafts).
// servicesRaw — data->'services' (nil — ключа нет). draftMissing — строки черновика нет.
func lockServices(ctx context.Context, tx pgx.Tx, id uuid.UUID) (m attemptMeta, role ddsRole, servicesRaw []byte, draftMissing bool, err error) {
	b := &pgx.Batch{}
	b.Queue(sqlLockServices, id)
	b.Queue(sqlLockDraftServices, id)
	br := tx.SendBatch(ctx, b)
	defer func() {
		if cerr := br.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("attempts: lock services: %w", cerr)
		}
	}()
	if err = br.QueryRow().Scan(&m.UserID, &m.LessonID, &m.Status, &m.FirstInput, &role.DDS, &role.Service); err != nil {
		if pg.IsNoRows(err) {
			return m, role, nil, false, errNotFound()
		}
		return m, role, nil, false, fmt.Errorf("attempts: lock attempt: %w", err)
	}
	if err = br.QueryRow().Scan(&servicesRaw); err != nil {
		if pg.IsNoRows(err) {
			return m, role, nil, true, nil
		}
		return m, role, nil, false, fmt.Errorf("attempts: lock draft: %w", err)
	}
	return m, role, servicesRaw, false, nil
}

// ddsRole — ракурс попытки: DDS — «Диспетчер ДДС» (v1.3), Service — код службы, за
// диспетчера которой работает обучающийся (статусы — только у неё). Признак ракурса —
// служба у попытки (attempts.service_id, фиксируется при выдаче), а не настройки занятия:
// попытки, выданные до миграции 00004, работают по-старому.
type ddsRole struct {
	DDS     bool
	Service string
}
