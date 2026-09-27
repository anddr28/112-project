# go-core — устройство ядра и правила разработки

Ядро тренажёра ДДС на Go: модульный монолит, PostgreSQL (pgx v5), публичный API для SPA
(`contracts/openapi/frontend.v1.yaml`, camelCase, `/api/v1`), внутренний контракт с
ai-service (`ai-service.v1.yaml` клиент, `go-internal.v1.yaml` callback, snake_case).
Источник истины — контракты и `docs/*.md`; схема БД — `db/migrations/00001_init.sql` +
`00002_go_core_voice.sql`; поведение, которого ждёт фронт, — моки фронта
(`frontend/src/shared/mocks/*`, `frontend/src/shared/api/httpApi.ts`).

> Обязательное чтение до кода: `docs/contracts.md`, `docs/voice-mode.md`, `docs/db-design.md`,
> этот файл целиком, спека своих эндпоинтов в `frontend.v1.yaml`.

## 1. Принципы

1. **Производительность и экономия ресурсов** — главный нефункциональный приоритет:
   - горячие пути (черновик, события, авторизация) — один round-trip в БД, без лишних decode/encode;
     jsonb, который не нужно разбирать, проксируется как `json.RawMessage`;
   - справочники (классификатор, службы, подписи) — в памяти, ответы предкодированы (`[]byte` + ETag);
   - события попытки — group commit (батч 50 мс одним `INSERT … SELECT unnest(...)`), `synchronous_commit=off`;
   - сессии — кэш в памяти с коротким TTL и явной инвалидацией; argon2id под семафором (память ограничена);
   - никаких горутин на запрос «на всякий случай», никаких неограниченных очередей/буферов;
   - `SELECT` только нужных колонок, индексы из схемы; N+1 запрещён (списки — одним запросом с JOIN/агрегатами).
2. **Корректность данных важнее скорости**: транзакции там, где меняется больше одной строки;
   побочные эффекты наружу (WebSocket, будильник диспетчера) — строго после COMMIT (`pg.OnCommit`).
3. **Идемпотентность** всех операций, которые клиент может повторить после обрыва (accept-call,
   submit, события по `clientSeq`, ход диалога по `turnNo`, callback по `request_id`).
4. **Контракт — закон**: имена полей, коды ошибок, статусы, обязательность полей — как в спеке.
   Обязательные массивы в JSON — всегда `[]`, никогда `null`. Время — RFC3339 UTC.
5. **Сообщения об ошибках — по-русски**, человекочитаемо (фронт показывает `message` как есть),
   решения фронт принимает по `code`.
6. Доменные пакеты **не импортируют друг друга** — только `core`, `platform/*`, `settings`,
   `gen/*`, `store`/`convert`/`model`, `scoring`, `reaction`, `dds`. Связи между доменами — порты `core/ports.go`,
   связывает `internal/app`.

## 2. Карта пакетов и владельцы

```
cmd/gocore            main: serve | migrate | seed | gencert | import-classifier | hash-password   (wave 3)
cmd/fakeai            имитатор ai-service по контракту (dev/QA/E2E без Python)                      (W1 fakeai)
internal/config       env-конфиг                                                                    (готово)
internal/core         роли, статусы, типы задач/событий, Principal, порты                           (готово)
internal/settings     типизированные настройки (кэш таблицы settings)                               (готово)
internal/platform/pg      пул, WithTx/OnCommit, ошибки                                              (готово)
internal/platform/httpx   ApiError, JSON I/O, Router (RBAC, CSRF), параметры                        (готово)
internal/platform/ids     UUIDv7                                                                    (готово)
internal/gen/*        oapi-codegen: components, aiservice (типы+клиент), callbacks, public          (готово, руками не править)

internal/model        формы jsonb в БД (snake_case) — CallScript, Etalon*, LessonSettings, ...      (W1 store)
internal/convert      snake<->camel, карточка АРМ <-> контрактный IncidentCard, результаты AI      (W1 store)
internal/store        общие чтения + сборка public-представлений: Scenario, Lesson, Attempt, DialogueTurn (W1 store)
internal/scoring      слой 1 (поля), слой 4 (тайминг), веса/итог/вердикт, рекомендации, XP        (W1 scoring)
internal/reaction     граф статусов реагирования служб, AssignedService                            (W1 classifier)
internal/dds          ракурс «Диспетчер ДДС»: выбор службы, карточка 112, оценка протокола (чистый)  (v1.3)
internal/classifier   Catalog (core.Catalog), поиск, опросные карты, подписи, службы, адреса,
                      resolve-services, сид справочников, импорт XLSX + хендлеры                    (W1 classifier)
internal/aijobs       ai_jobs: Queue, диспетчер, reaper, breaker, sync-клиент, callback-приёмник,
                      роутер результатов, GET /ai-jobs/{id}                                          (W1 aijobs)
internal/realtime     WS-хаб (core.Publisher) + /ws/lessons/{id}/monitor, /ws/attempts/{id}           (W1 realtime)
internal/eventlog     group-commit писатель attempt_events + серверные события в транзакции          (W1 realtime)
internal/audit        батч-писатель аудита (core.Auditor), партиции audit_log, выборка для админки  (W1 ops)
internal/ops          планировщик, бэкапы pg_dump, чистки, TLS (CA контура), SPA-статика, health    (W1 ops)
internal/platform/metrics  лёгкие метрики Prometheus-формата                                         (W1 ops)
internal/auth         сессии, пароли, rate limit, lockout, login/logout/me/demo, демо-учётки         (W1 auth)
internal/users        /users*, /users/{id}/progress; users.Get — единственная сборка public.User       (W1 auth)

internal/scenarios    сценарии, версии, генерация, approve+TTS, tts-preview, результаты generate/tts, демо-сценарии (W2)
internal/lessons      занятия, выдача попыток (core.AttemptIssuer), мониторинг (LessonMonitor, AttemptPresence), демо-занятия (W2)
internal/attempts     попытка: get/accept-call/call-script/draft/events/services/replay/submit       (W2)
internal/dialogue     разговор: ход, состояние, завершение, /media/tts, core.DialogueHooks          (W2)
internal/evaluation   core.Evaluator, применение AI-результатов, финализация, XP, get/override/feedback (W2)
internal/admin        /admin/health, /admin/settings*, /admin/audit, /admin/backups                  (W2)
internal/reports      /lessons/{id}/report CSV/XLSX/PDF                                              (W2)
internal/app          сборка, маршруты, фоновые воркеры, graceful shutdown                          (wave 3)
```

Каждый доменный пакет экспортирует `New…(deps)` и `(h *Handlers) Register(r *httpx.Router)`,
регистрирующий **свои** маршруты (паттерны ровно как в спеке, относительно `/api/v1`).

## 3. Соглашения кода

- Go 1.26, stdlib `net/http` (паттерны `"POST /attempts/{attemptId}/submit"`), `log/slog`.
- Хендлер: `func(w http.ResponseWriter, r *http.Request) error`; ошибки — `httpx.*` конструкторы
  (`httpx.NotFound("Попытка не найдена")`, `httpx.Conflict(...)`, `httpx.Validation(msg, fields)`).
  Неизвестная ошибка = 500 `internal` + лог. Субъект: `core.PrincipalFrom(r.Context())` (не nil на защищённых маршрутах).
- Доступ задаётся при регистрации: `r.Handle("GET /lessons", httpx.Roles(core.RoleTeacher, core.RoleAdmin), h.list)`.
  Проверки «владелец ли» (студент — своя попытка; преподаватель — своё занятие) — в хендлере/сервисе:
  чужое для студента → **403** `forbidden`; несуществующее → 404. Админ видит всё (кроме действий «от студента»).
- SQL: pgx, явные колонки, плейсхолдеры `$n`, никаких конкатенаций пользовательского ввода.
  Сканирование вручную (быстрее рефлексии). Функции репозиториев принимают `pg.Querier`.
  Ошибки: `pg.IsNoRows`, `pg.IsUniqueViolation`, `pg.ConstraintName`.
- Транзакции: `pg.WithTx(ctx, pool, func(ctx, tx) error {...})`; внутри — `pg.OnCommit(ctx, func(){ publisher... })`.
  Блокировки строк: `SELECT … FOR UPDATE` там, где read-modify-write (черновик при правке служб,
  попытка при submit/accept, задача при callback).
- ID — `ids.New()` (UUIDv7) до INSERT. `uuid.UUID` (google/uuid) сканируется/пишется pgx напрямую.
- Время — `time.Now().UTC()`; в JSON — `time.Time` (RFC3339Nano). Длительности в мс — `int`.
- Сгенерированные типы: `public.*` (фронт), `components.*`/`aiservice.*`/`callbacks.*` (ai-service).
  Опциональные поля — указатели: хелперы `ptr[T]`, пустые срезы `[]T{}` для обязательных массивов.
- Логи: `log.Info/Warn/Error("что случилось", "attempt", id, "err", err)`; без ПДн (ФИО, пароли, тексты карточек).
- Аудит (`core.Auditor.Log`) — на каждое значимое действие: `user.login`, `user.login_failed`, `user.logout`,
  `user.create`, `user.update`, `user.block`, `user.unblock`, `scenario.create|update|version|generate|approve|reject`,
  `etalon.version`, `lesson.create|start|finish`, `attempt.accept|submit`, `evaluation.override`,
  `feedback.add`, `setting.update`, `backup.run`, `classifier.import`. Before/After — компактные структуры,
  без паролей/хэшей/токенов.
- Комментарии — по-русски, объясняют «почему», как в остальном репозитории.

## 4. Данные: что где лежит

| Колонка | Формат | Кто пишет |
|---|---|---|
| `scenarios.call_script` | `model.CallScript` (snake_case = contract CallScript + `caller.voice`, `turns[].tts_hash`, `dialogue`) | scenarios |
| `scenarios.generation_meta` | `{engine…, notes_for_teacher, difficulty_estimate, job_id, rejected_reason}` | scenarios |
| `etalons.card` | contract `IncidentCard` (snake_case) — уходит в ai-service | scenarios (проекция из card_draft) |
| `etalons.card_draft` | `public.IncidentCardDraft` (camelCase) — с ним сверяется карточка студента | scenarios |
| `etalons.scoring` | contract `Scoring` (snake) + `required_fields` — пути IncidentCardDraft ("applicant.name", "address.raw", "incidentTypeIds", "attributes.where", …) | scenarios |
| `etalons.expected_actions` / `expected_dialogue` | contract `ExpectedAction[]` / `ExpectedDialogue` (snake) | scenarios |
| `lessons.settings` | `model.LessonSettings` (snake): `pass_threshold, weights{fields,semantic,grammar,timing,dialogue}, cards_per_student, allow_replay, voice{enabled,input,push_to_talk,max_turns,tts_enabled}, perspective` | lessons |
| `attempts.card`, `attempt_drafts.data` | `public.IncidentCardDraft` (camelCase, как прислал фронт) | attempts |
| `evaluations.field_errors` | `public.FieldError[]` (camelCase, как отдаём) | evaluation |
| `evaluations.timing` | `{spent_ms, limit_sec, delta_ms, within_norm, reaction_ms}` (snake; VIEW читает `within_norm`) | evaluation |
| `evaluations.semantic` / `dialogue` / `grammar_remarks`+`grammar_stats` | как пришло от ai-service (snake) | evaluation |
| `evaluations.layers` | `{"grammar":"queued|done|failed|skipped", …}` — терминальные состояния; `running` накладывается при чтении из `ai_jobs` | evaluation |
| `evaluations.weights` | фактические веса итога (после перенормировки) | evaluation |
| `attempt_dialogue_turns` | транскрипт; `turn_no` = номер обмена (0 — вступление) | dialogue |
| `classifier_categories.attributes` | `public.AttributeGroup[]` (camelCase: code,label,widget,options,required,visibleWhen,order) | classifier |
| `classifier_categories.service_rules` | `[{attr?, any_of?, services:[{code, primary, reason}]}]` | classifier |

**Проекции карточки** (`convert`): студент заполняет `IncidentCardDraft`; для ai-service
go-core строит contract `IncidentCard` (`DraftToCard`); сгенерированный LLM эталон приходит
как `IncidentCard` → `CardToDraft` даёт форму АРМ для сверки. Правила маппинга — в `convert`.

## 5. Семантика, согласованная с фронтом (моки = ожидания UI)

### Попытка
- Выдача при `POST /lessons/{id}/start`: каждому участнику `seq_no=1`, сценарий — по кругу из пула
  (`lesson_scenarios.sort_order`), `etalon_id` = текущая версия, `time_limit_sec` = норматив занятия,
  `incident_no` = sequence, пустой черновик, событие `issued`. Следующая карточка (если
  `cardsPerStudent > seq_no`) — сразу после submit (`core.AttemptIssuer.IssueNext`).
- `accept-call` (идемпотентно): `issued → in_progress`, `call_accepted_at = now`, `phones.aon` черновика =
  `call_script.caller.phone` (Инструкция п.4.1), события `call_accepted`, `open_card`, участник → `active`,
  `joined_at`. В голосовом режиме ответ несёт `opening` (turn 0, аудио из tts_cache).
- `first_input_at` — время первого события из `core.UserInputEvent` (не `save`).
- `submit` (идемпотентно, повтор → 200 та же попытка): `card`, `action_text`, `submitted_at`,
  `time_spent_ms = submitted_at − call_accepted_at`, статус `evaluating`, события `save`+`submitted`,
  закрыть разговор (`CloseOnSubmit`), `Evaluator.StartEvaluation`, выдать следующую карточку.
  Из статуса `issued` submit → 409 (вызов не принят).
- `finish` занятия: попытки `issued|in_progress` → `expired`, участники → `finished`, WS `lessonStatus`
  (монитор) и `lessonFinished` (студентам).
- Службы карточки живут в `attempt_drafts.data.services` (read-modify-write под `FOR UPDATE`):
  добавленная служба сразу в «Получена службой» с историей `Добавлена`,`Получена службой` (оператор «система»);
  переходы — `reaction`; снять можно только в `Добавлена|Получена службой` (иначе 409);
  подпись оператора в истории — `Principal.OperatorLabel()`.

### Ракурс «Диспетчер ДДС» (v1.3, `lessons.settings.perspective = dds`)
ТЗ «действия с карточками» + Памятка АРМ-112 для ДДС: карточку создал оператор 112, обучающийся —
диспетчер **своей** службы. Признак ракурса у попытки — `attempts.service_id` (миграция 00004),
а не настройки занятия: попытки, выданные раньше, работают по-старому.
- Создание: только `mode = card_actions`; голос выключается (вес разговора 0, перенормировка);
  режим сценария не проверяется (нужна только карточка 112 — эталон). 422, если у сценария нет
  списка оповещения или у участника с профилем ДДС (`users.service_id`) в пуле нет профильной
  карточки (`details.scenarioIds` / `participantIds`).
- Список оповещения сценария — `store.ServiceCodesSQL`: службы `card_draft.services` эталона (иначе
  `card.services_to_notify`), только известные справочнику, основная — первой.
- Выдача: `dds.Pick` — профиль есть → только сценарии, где его служба в списке оповещения; профиля
  нет → основная служба карточки. Служба пишется в `attempts.service_id`. Профильных карточек нет
  (профиль сменили) — участнику не выдаётся ничего, занятие не падает.
- `accept-call` = взять карточку в работу: без `opening` (хук разговора не зовётся), черновик —
  `dds.IncomingCard` (поля эталона, АОН, службы оповещения «Получена службой», `actionsTaken` пуст).
  `call-script` — `turns: []`; `replay` — 409.
- Службы: добавить/снять — 409 (состав задал 112); статус чужой службы — 403; своей — граф `reaction`.
- `PUT /draft`: из тела только `actionsTaken`; `submit`: карточка из черновика сервера, из тела —
  `actionText` (или `card.actionsTaken`).
- Оценка: слой `fields` = `dds.Evaluate` (`etalons.scoring.reaction`: решение 3, норматив решения
  от `callAcceptedAt` 2, лишний отказ 1, обязательные статусы по 1; нет эталона — «Принята» за 30 с),
  `engine.fields.source = dds_reaction`; грамматика — `actionText` + комментарии к статусам своей
  службы (`reactionComments`); рекомендации — тексты диспетчера.
- Семантика ДДС — `actionText` против **только** `etalons.expected_actions[].required_facts` (действия
  диспетчера: принята в работу, направлена бригада, принятые меры). Факты первоначального звонка
  (`scoring.required_facts`, `call_script.key_facts`) — это то, что видел заявитель, а не работа
  службы: по ним ДДС не оценивается и в задачу ai-service они не передаются. Нет `expected_actions` —
  слой `semantic` = `skipped` (`engine.semantic.reason = dds_no_expected_actions`), в итог не входит,
  веса остальных слоёв перенормируются. Оператор 112 — без изменений: факты из `scoring.required_facts`
  → `expected_actions` → `call_script.key_facts`. Демо-сценарии с действиями ДДС фиксируют факты
  звонка в `scoring.required_facts` явно, чтобы запасной путь 112 не дошёл до действий ДДС.

### Оценка (слои, веса, итог)
- `fields` и `timing` — мгновенно в submit (`status=partial`); AI-слои — задачи `evaluate_grammar`
  (есть свободный текст), `evaluate_semantic` (всегда для card_actions; для cards — если есть
  description/actionsTaken, иначе semantic=0 без LLM с пояснением), `evaluate_dialogue`
  (если голос включён). Нет текста для грамматики → слой `skipped`, в итог не входит.
- Веса: `lessons.settings.weights`; голос выключен → `dialogue=0`, остальные перенормируются к 1;
  голос включён и `dialogue=0` → `dialogue_weight_default` (0.25), остальные пропорционально ужимаются
  (как `withDialogueWeight`/`normalizeWeights` в `frontend/src/shared/mocks/db.ts`).
- Итог = Σ(score·w) / Σ(w по слоям, у которых есть балл) — пока слои доезжают, итог «частичный».
  Все ожидаемые слои на месте (done/failed/skipped) → `status=done`, `verdict = final ≥ passThreshold`,
  XP, попытка `evaluated`, участник `finished` (если карточек больше нет). AI-слой окончательно failed →
  `aiUnavailable=true`, `needsReview=true`, итог по доступным слоям.
- `confidence < confidence_threshold` → `needsReview=true`.
- `timing`: `withinNorm = spent ≤ limit·(1+soft%)`; балл 100 до soft, линейно до 0 к hard
  (`settings.timing_tolerance`), `reactionMs = first_input_at − call_accepted_at`.
- Слой 1: по `required_fields` эталона (веса `field_weights`, по умолчанию 1): пусто → `missing`;
  свободный текст (`description`, `actionsTaken`) — только наличие; остальное — сравнение
  нормализованных множеств значений (регистр, ё/е, пробелы, телефоны по последним 10 цифрам,
  «ул./д./кв.» в адресе); коды → подписи в `expected/actual` (типы — названия, признаки — подписи).
- XP (`settings.xp_rules`, идемпотентно по `(attempt_id, reason)`): `attempt_evaluated`, `within_norm`,
  `pass_bonus` = `pass_bonus_per_10_points · floor(final/10)` при pass; `lesson_completed` — когда
  у студента не осталось незавершённых карточек занятия. `xpEarned` = сумма по попытке.
- Override (преподаватель): `override_score`, `override_verdict = score ≥ порога`, причина, автор, время;
  аудит `evaluation.override` с before/after; WS обоим каналам.

### Разговор (голос)
- Нумерация: `turnNo` обмена — 0 вступление заявителя; k ≥ 1 — k-я реплика оператора и ответ на неё
  (обе строки с одним `turn_no`). `nextTurnNo` = последний обработанный + 1 (после вступления — 1).
  `noSpeech` — ничего не сохраняется, `nextTurnNo` не меняется (повтор тем же номером).
- Повтор `turnNo`, который уже обработан → **409** `conflict`, `details: {nextTurnNo, response}` (сохранённый
  `DialogueTurnResponse`); одновременный дубль в полёте ждёт первый (singleflight) и получает тот же 200.
  `turnNo > nextTurnNo` → 409 с `details.nextTurnNo`.
- Для ai-service транскрипт нумеруется сквозным `turn_no` 1..n: вступление = 1, оператор k = 2k,
  заявитель k = 2k+1. Ссылки `evidence_turn_no`/`forbidden_hits.turn_no` из результата оценки
  переводятся обратно в номер обмена: `s=1 → 0` (вступление), чётный `s → s/2` (оператор),
  нечётный `s → (s−1)/2` (заявитель) — `convert.SeqToExchange` / `convert.ExchangeToSeq`.
- Завершение: `should_end` → `caller_hung_up`; операторских реплик ≥ `maxTurns` → `max_turns`;
  `POST /dialogue/end` → `operator_hung_up`; submit → `submitted`. Состояние — `attempts.call_ended_at/
  call_end_reason`, событие `dialogue_ended`, WS студенту `callerHungUp` (если повесил заявитель).
- ai-service недоступен/breaker open: для **текстовой** реплики go-core отвечает сам по сценарию
  (`fallback=true`, `source=script`, реплики `turns[1:]` по кругу, факты не раскрываются); для
  аудио — 503 `caller_busy`. 503/429 ai-service → 503 `caller_busy` + `Retry-After`. Попытку не ломаем.
- Аудио оператора не хранится нигде. Аудио заявителя — URL `/api/v1/media/tts/{file_path}`.

### Сценарии
- Правка (`PATCH`) запрещена, если сценарий стоит хотя бы в одном занятии: 409, `details.lessonsCount`.
  Изменение эталонных полей (`etalonCard`, `etalonDraft`, `scoring`, `requiredFields`, `expectedActions`,
  `expectedDialogue`) создаёт новую версию `etalons` (старая `is_current=false`).
- `POST /scenarios/{id}/versions` — копия (status `draft`, `parent_id`, `version = max(цепочки)+1`, эталон v1).
- `approve`: нужен эталон (непустой card_draft и `required_fields`), легенда с репликой заявителя; если есть
  `callScript.dialogue`, чек-лист `expectedDialogue` не пуст — иначе 422 `validation` с `details.fields`.
  Проставить `tts_hash = sha256(text|voice|rate)` репликам заявителя и поставить TTS-задачи (dedup `tts:{hash}`).
- `generate`: сценарий `draft/generated` c заглушкой-заголовком + задача `generate_scenario`; breaker open → 503.
  Callback → легенда, эталон v1 (card, card_draft=CardToDraft, scoring с required_fields по умолчанию),
  чек-лист, `status=generated`, `generation_meta`.
- `inUse/lessonsCount` — число занятий, где сценарий в `lesson_scenarios`; `ttsReady` — у всех реплик
  заявителя есть строка в `tts_cache`.

### WebSocket
- `/ws/lessons/{id}/monitor?since=seq` (преподаватель-владелец/админ): первое — `snapshot`, далее
  `participantStatus | attemptEvent | dialogueTurn | evaluationUpdated | lessonStatus | aiHealth`; `seq`
  монотонен в канале, буфер 5 мин для `since`; ping каждые 20 с.
- `/ws/attempts/{id}` (владелец): `evaluationUpdated | lessonFinished | timerExpired | callerHungUp`.
  Подключение/отключение = участник `active/disconnected` (+ `participantStatus` в монитор).

## 6. Нагрузка и лимиты (ТЗ ×10)

- 200 одновременных студентов, ~1000 событий/с → group commit; черновик — один upsert с проверкой
  владельца в том же запросе; ответ API ≤ 2 с при 100 пользователях — держим p99 < 100 мс на
  не-AI ручках.
- Пул БД 24 соединения; HTTP-сервер: `ReadHeaderTimeout 10s`, `IdleTimeout 120s`, `MaxHeaderBytes 64K`.
- Синхронный ход диалога: таймаут 20 с к ai-service; одновременно в полёте ≤ 1 ход на попытку.
- Все фоновые циклы останавливаются по `ctx` (graceful shutdown ≤ 15 с, батчеры дописывают хвост).
- Потолки: обычный запрос API — 60 с (`httpx.DefaultRequestTimeout`, WS без потолка; превышение —
  503 + Retry-After); сессии PG — `statement_timeout` 60 с, `lock_timeout` 15 с,
  `idle_in_transaction_session_timeout` 60 с (переопределяются параметрами DATABASE_URL).
- Открытые WebSocket раз в 10 с перепроверяют сессию: выход/блокировка закрывают сокет (1008).
- Ошибки данных PostgreSQL (класс 22: NUL-символ, кодировка, переполнение) — 400 validation, не 500.
- Растущие списки (`/lessons`, `/scenarios`, `/users`, `/lessons/assigned`) — только постранично:
  keyset-курсор (`httpx.ParsePage` / `SetNextCursor`, ключи `store.TimeKey`/`AssignedKey`,
  `users.NameKey`), `LIMIT n+1`, индекс в порядке сортировки (миграция 00003, db-design Р26).
  Новый список без потолка строк в ядро не добавлять.

## 7. Уточнения после ревью (поведение, закреплённое тестами)

- `call-script` до accept-call отдаёт `turns: []` (легенду не читают до снятия трубки); черновик и
  службы до accept-call — 409 «Сначала примите вызов».
- Службы в черновике: статус, история и «закреплённые» службы — у сервера (эндпоинты служб);
  состав auto/vis — у клиента (автосохранение). Правило слияния — комментарий к
  `servicesMergeTemplate` в `internal/attempts/sql.go`.
- Серверные события хронологии — `clientSeq = 0` (контракт v1.2: `AttemptEvent.clientSeq ≥ 0`).
- `should_end` с `end_reason = max_turns` от ai-service — завершение `max_turns` (без callerHungUp).
- `lesson_completed` XP — когда открытых карточек нет И оценено `cards_per_student` карточек
  (истёкшие при завершении занятия не считаются).
- XP после ручной корректировки пересчитывается: бонус за зачёт следует итоговому баллу
  преподавателя (незачёт — снимается, зачёт — начисляется по баллу), порядок «корректировка /
  доезд последнего AI-слоя» не влияет (`evaluation.syncPassBonus`).
- Веса итога: занятие хранит эффективные веса (при создании добавляется доля разговора по
  умолчанию, если голос включён и вес не задан); оценка их только нормирует.
- AI-задачи: повторная отправка после failed-результата идёт под новым `request_id` (id отправки,
  `payload.request_id`), `ai_jobs.id` не меняется; ожидание в очереди ai-service не расходует
  попытки (reaper переотправляет бесплатно, сдаётся после 12 × `reaper_after_sec`);
  `evaluate_*` при недоступном ai-service дольше `reaper_after_sec` завершаются `ai_unavailable`
  (оценка по доступным слоям, needsReview).
