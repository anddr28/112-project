package aijobs

// Весь SQL пакета — константы: текст не меняется от вызова к вызову, pgx кэширует
// prepared statements (QueryExecModeCacheStatement). Статусы в фильтрах записаны через OR,
// а не IN: так планировщик берёт BitmapOr по двум частичным индексам
// (ai_jobs_queue_idx WHERE status='queued', ai_jobs_running_idx WHERE status='running'),
// а IN ('queued','running') на большой таблице уходит в seq scan (проверено EXPLAIN).

// jobCols — строка задачи для обработчиков результатов (scanJob). Два последних столбца:
// request_id текущей отправки (после ретрая по failed-callback'у он новый, см. sqlRetryJob) и
// код ошибки последнего failed-callback'а (result хранит его до следующего исхода).
const jobCols = `id, type, status, ref_type, ref_id, try_count, max_tries, payload, created_at, locked_at,
       COALESCE(payload ->> 'request_id', ''), COALESCE(result #>> '{error,code}', '')`

const (
	// $1 id, $2 type, $3 priority, $4 run_after (NULL — сейчас), $5 dedup_key, $6 ref_type,
	// $7 ref_id, $8 payload, $9 max_tries. Набор параметров общий с sqlReviveJob.
	sqlInsertJob = `
INSERT INTO ai_jobs (id, type, status, priority, run_after, dedup_key, ref_type, ref_id, payload, max_tries)
VALUES ($1, $2, 'queued', $3, COALESCE($4, now()), $5, $6, $7, $8, $9)
ON CONFLICT (dedup_key) DO NOTHING
RETURNING id`

	// Оживление провалившейся/отменённой задачи с тем же dedup_key. id меняется на новый:
	// request_id в payload уже новый, а ai-service по старому request_id мог запомнить
	// «выполнено» и прислать старый результат вместо пересчёта. Внешних ключей на ai_jobs нет.
	sqlReviveJob = `
UPDATE ai_jobs
   SET id = $1, type = $2, status = 'queued', priority = $3, run_after = COALESCE($4, now()),
       ref_type = $6, ref_id = $7, payload = $8, max_tries = $9, try_count = 0,
       result = NULL, error = NULL, locked_by = NULL, locked_at = NULL,
       created_at = now(), started_at = NULL, finished_at = NULL
 WHERE dedup_key = $5 AND status IN ('failed', 'cancelled')
RETURNING id`

	sqlJobByDedup = `SELECT id FROM ai_jobs WHERE dedup_key = $1`

	// Захват пачки одним запросом. ARRAY(...) превращает подзапрос в InitPlan, и внешний
	// UPDATE идёт по PK (= ANY) при любой статистике — без hash join по всей таблице.
	// $1 instance, $2 limit, $3 типы на паузе (429) — всегда не-NULL массив.
	// Последний столбец — request_id отправки (Idempotency-Key).
	sqlClaim = `
UPDATE ai_jobs j
   SET status = 'running', locked_by = $1, locked_at = now(), started_at = COALESCE(j.started_at, now())
 WHERE j.id = ANY (ARRAY(
       SELECT q.id FROM ai_jobs q
        WHERE q.status = 'queued' AND q.run_after <= now() AND q.type <> ALL ($3)
        ORDER BY q.priority, q.type, q.run_after
        LIMIT $2
        FOR UPDATE SKIP LOCKED))
RETURNING j.id, j.type, j.try_count, j.max_tries, j.locked_at, j.payload,
          COALESCE(j.payload ->> 'request_id', j.id::text)`

	// Вернуть отправленную задачу в очередь. Условие по (locked_by, locked_at) — ровно наш
	// захват: если за это время пришёл callback или reaper уже вернул задачу, не трогаем.
	// $4 задержка в секундах, $5 прирост try_count (0|1), $6 текст ошибки (NULL — оставить).
	sqlRequeueSent = `
UPDATE ai_jobs
   SET status = 'queued', locked_by = NULL, locked_at = NULL,
       run_after = now() + make_interval(secs => $4),
       try_count = try_count + $5, error = COALESCE($6, error)
 WHERE id = $1 AND status = 'running' AND locked_by = $2 AND locked_at = $3`

	sqlLockSent = `SELECT ` + jobCols + ` FROM ai_jobs
 WHERE id = $1 AND status = 'running' AND locked_by = $2 AND locked_at = $3
   FOR UPDATE`

	sqlLockJob = `SELECT ` + jobCols + ` FROM ai_jobs WHERE id = $1 FOR UPDATE`

	// Callback по request_id ретрая (≠ ai_jobs.id): ищем только среди незавершённых задач —
	// это частичные индексы queued/running, без seq scan по всей таблице. $1 — request_id текстом.
	sqlLockByRequest = `SELECT ` + jobCols + ` FROM ai_jobs
 WHERE (status = 'queued' OR status = 'running') AND payload ->> 'request_id' = $1
 LIMIT 1
   FOR UPDATE`

	// $2 прирост try_count, $3 текст ошибки, $4 полный callback (NULL — оставить как было).
	sqlMarkFailed = `
UPDATE ai_jobs
   SET status = 'failed', try_count = try_count + $2, error = $3, result = COALESCE($4, result),
       finished_at = now(), locked_by = NULL, locked_at = NULL
 WHERE id = $1`

	sqlMarkDone = `
UPDATE ai_jobs
   SET status = 'done', result = $2, error = NULL, finished_at = now(), locked_by = NULL, locked_at = NULL
 WHERE id = $1`

	// Ретрай по callback'у status=failed, retryable=true. $2 задержка в секундах, $3 ошибка,
	// $4 полный failed-callback (его error.code читает следующий callback — shouldRetry),
	// $5 новый request_id. Повтор уходит под НОВЫМ request_id: ai-service идемпотентен по
	// request_id и на повтор завершённой задачи (в т.ч. отказом) вправе ответить 202 done и
	// переслать тот же отказ из кэша, не пересчитывая. id задачи не меняется — его опрашивает
	// фронт (GET /ai-jobs/{id}). started_at сбрасывается: срок удержания reaper'а — на отправку.
	sqlRetryJob = `
UPDATE ai_jobs
   SET status = 'queued', try_count = try_count + 1, run_after = now() + make_interval(secs => $2),
       error = $3, result = $4,
       payload = CASE WHEN jsonb_typeof(payload) = 'object'
                      THEN jsonb_set(payload, '{request_id}', to_jsonb($5::text)) ELSE payload END,
       started_at = NULL, locked_by = NULL, locked_at = NULL
 WHERE id = $1`

	// ---------------------------------------------------------------- reaper

	// Переотправка задачи без результата дольше $1 секунд. Попытка НЕ тратится: повтор
	// идемпотентен по request_id, а долгое ожидание в очереди ai-service (полоса LLM занята
	// ходами диалога) — не отказ задачи. Предел — $2 секунд с первой отправки (started_at),
	// дальше задача считается зависшей (sqlReaperExhausted). $3 текст для ai_jobs.error.
	// Индекс ai_jobs_running_idx (locked_at).
	sqlReaperRequeue = `
UPDATE ai_jobs
   SET status = 'queued', run_after = now(), locked_by = NULL, locked_at = NULL, error = $3
 WHERE status = 'running' AND locked_at < now() - make_interval(secs => $1)
   AND COALESCE(started_at, locked_at) >= now() - make_interval(secs => $2)`

	sqlReaperExhausted = `
SELECT id FROM ai_jobs
 WHERE status = 'running' AND locked_at < now() - make_interval(secs => $1)
   AND COALESCE(started_at, locked_at) < now() - make_interval(secs => $2)
 ORDER BY locked_at
 LIMIT 50`

	// ai-service недоступен дольше порога: оценочные задачи, ждущие в очереди go-core дольше
	// $2 секунд. $1 — типы evaluate_*.
	sqlUnavailableIDs = `
SELECT id FROM ai_jobs
 WHERE status = 'queued' AND type = ANY ($1) AND created_at < now() - make_interval(secs => $2)
 ORDER BY created_at
 LIMIT 50`

	sqlLockQueued = `SELECT ` + jobCols + ` FROM ai_jobs WHERE id = $1 AND status = 'queued' FOR UPDATE SKIP LOCKED`

	sqlReaperLock = `SELECT ` + jobCols + ` FROM ai_jobs
 WHERE id = $1 AND status = 'running' AND locked_at < now() - make_interval(secs => $2)
   FOR UPDATE SKIP LOCKED`

	// Старт инстанса: свои running-задачи отправляются заново (см. requeueOwn).
	sqlRequeueOwn = `
UPDATE ai_jobs SET status = 'queued', locked_by = NULL, locked_at = NULL, run_after = now()
 WHERE status = 'running' AND locked_by = $1`

	// ---------------------------------------------------------------- оценки ожидания и счётчики

	sqlLoad = `
SELECT type, status, priority, count(*)::int
  FROM ai_jobs
 WHERE status = 'queued' OR status = 'running'
 GROUP BY type, status, priority`

	// Средняя длительность по последним выполненным задачам (engine.duration_ms из callback'а):
	// оценки ожидания переживают рестарт. $1 — нижняя граница UUIDv7 (PK-диапазон, без seq scan).
	sqlSeedAvg = `
SELECT type, avg((result -> 'engine' ->> 'duration_ms')::float8)::float8
  FROM (SELECT type, result FROM ai_jobs
         WHERE id >= $1 AND status = 'done'
         ORDER BY id DESC
         LIMIT 500) t
 WHERE jsonb_typeof(result -> 'engine' -> 'duration_ms') = 'number'
 GROUP BY type`

	// done/failed/cancelled — только за сутки: id >= UUIDv7(now-24h) даёт диапазон по PK.
	sqlCounts = `
SELECT status, count(*)::int FROM ai_jobs
 WHERE status = 'queued' OR status = 'running'
 GROUP BY status
UNION ALL
SELECT status, count(*)::int FROM ai_jobs
 WHERE id >= $1 AND status IN ('done', 'failed', 'cancelled') AND finished_at > now() - interval '24 hours'
 GROUP BY status`

	sqlJobView = `
SELECT id, type, status, priority, run_after, ref_type, ref_id, try_count, error, created_at, locked_at, finished_at
  FROM ai_jobs
 WHERE id = $1`

	// Кто впереди задачи в той же полосе ai-service. $1 типы полосы, $2 priority, $3 run_after,
	// $4 id задачи, $5 locked_at (NULL для queued — тогда running-сравнение ложно).
	sqlAhead = `
SELECT type,
       count(*) FILTER (WHERE status = 'queued' AND (priority, run_after, id) < ($2, $3, $4))::int,
       count(*) FILTER (WHERE status = 'running' AND (priority < $2 OR (priority = $2 AND locked_at < $5)))::int,
       count(*) FILTER (WHERE status = 'running')::int
  FROM ai_jobs
 WHERE (status = 'queued' OR status = 'running') AND type = ANY ($1) AND id <> $4
 GROUP BY type`
)
