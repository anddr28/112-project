# Контракты: go-core ↔ ai-service, frontend ↔ go-core

Источник истины — OpenAPI-спеки в `contracts/openapi/`. Этот документ — канон
разграничения ответственности; при конфликте с кодом прав контракт.

| Спека | Сервер | Клиент | Формат |
|---|---|---|---|
| `ai-service.v1.yaml` | ai-service | go-core | snake_case |
| `go-internal.v1.yaml` | go-core (callback) | ai-service | snake_case |
| `frontend.v1.yaml` | go-core (`/api/v1`, WS) | SPA | **camelCase** (зеркало `frontend/src/shared/types`) |
| `_components.yaml` | — | — | общие схемы для двух внутренних |

v1.1 (2026-09-20) — голосовой режим; концепция в `docs/voice-mode.md`.

## Схема взаимодействия

```
студент ──submit──▶ go-core ──┐ мгновенно: поля + тайминг, evaluations(partial)
                              │ INSERT ai_jobs(queued)
                              ▼
                 диспетчер (SKIP LOCKED, группировка по типу/профилю)
                              │ POST ai-service /v1/jobs/{grammar|semantic|dialogue|generate|tts}
                              │   202 → running   |   429 → ждём Retry-After
                              │   refused/timeout/5xx → circuit breaker (open 30s → half-open)
                              ▼
                        ai-service (stateless, asyncio)
                    полоса LLM (семафор=1, приоритет: dialog_turn > evaluate > generate)
                    полоса LanguageTool | полоса STT | полоса TTS
                    синхронно (голос): /v1/dialog/turn, /v1/stt/transcribe, /v1/tts/sync
                              │
                              │ POST go-core /internal/ai/v1/results (retry 1/5/30 c)
                              ▼
        go-core: UPDATE evaluations → пересчёт total_score → done → XP
                 (дубликат callback'а → 200, игнор)

reaper go-core: running старше N мин → queued (переотправка; идемпотентность по request_id)
poison-pill:    try_count ≥ max_tries → failed → "требуется ревью преподавателя"
```

## Кто что считает

**Принцип: go-core считает всё детерминированное; ai-service — всё, что требует
NLP/LLM; ai-service никогда не видит весов занятия и не считает итоговые баллы.**

| Вычисление | Где | Вход → Выход |
|---|---|---|
| Слой 1: формализованные поля (enum, чекбоксы, нормализация адреса, числа) | **go-core**, мгновенно | card + etalon.card + scoring → `fields_score`, `field_errors[]` |
| Слой 2: грамматика | **ai-service** (LT, ~0.3 с) | только free-text поля → `grammar.score` + `remarks[]` |
| Слой 3: семантика | **ai-service** (LLM, профили) | etalon + answer + call_script → `semantic.score`, факты, `confidence` |
| Слой 4: тайминг | **go-core**, мгновенно | метки attempts → `timing_score`, `timing{}` |
| Итог: веса, verdict, XP | **go-core** | 4 слоя × lessons.settings.weights → `total_score`; пересчёт при доезде слоёв и при смене весов — без повторного LLM |
| Генерация сценариев/эталонов (+ бриф заявителя, чек-лист разговора) | **ai-service** | spec категории → черновик → валидация преподавателем |
| TTS | **ai-service** (Silero) | text → файл в shared volume + duration |
| **Слой 5: разговор (голос)** — чек-лист протокола, тон | **ai-service** (LLM `eval_dialogue`) | транскрипт + `expected_dialogue` → `dialogue.checklist[]`, `tone`, `confidence` |
| **Слой 5: метрики речи** (темп, паразиты, паузы) | **ai-service**, детерминированно | транскрипт → `dialogue.speech` — считаются всегда, даже без LLM |
| STT реплики оператора | **ai-service** (синхронно) | аудио → `SttResult` (+ ITN чисел) |
| Реплика ИИ-заявителя | **ai-service** (LLM `dialog_fast`, синхронно) | бриф + история → `CallerReply` с `revealed_fact_ids` |
| Состояние диалога: нумерация ходов, транскрипт, раскрытые факты, завершение | **go-core** | `attempt_dialogue_turns`; студенту раскрытые факты не отдаются |
| Аналитика, рекомендации, отчёты | **go-core** | evaluations/attempts → recommendations, reports |

Исключение (осознанное): нормировку `grammar.score` из ошибок LT делает ai-service —
формула привязана к NLP-специфике и тюнится AI-разработчиком; её версия едет в
`engine.rules_version` для воспроизводимости.

## Синхронные вызовы (только голос)

Три эндпоинта ai-service отвечают телом, а не 202: `/v1/stt/transcribe`,
`/v1/dialog/turn`, `/v1/tts/sync`. Причина — студент ждёт на линии. Правила:
* таймаут go-core 20 с; ответ 503 + `Retry-After` = «полоса занята» — go-core отдаёт
  фронту `caller_busy`, попытка не страдает, breaker **не** открывается;
* полоса LLM — приоритетная очередь: `dialog_turn` > `evaluate_*` > `generate`;
  фоновые LLM-задачи стартуют только при пустой очереди диалогов;
* ai-service не хранит состояние диалога и не сохраняет аудио оператора;
* LLM недоступна → `fallback=true`, реплика из сценарных `turns[]` — занятие продолжается.

## Профили моделей

| Профиль | Задача | Модель (дефолт) | Когда |
|---|---|---|---|
| `dialog_fast` | ИИ-заявитель | 3b q4, JSON-выход, num_predict ≤ 90 | на занятии, синхронно |
| `eval_fast` | семантика | 3b q4, короткий промпт, num_predict с потолком | на занятии |
| `eval_dialogue` | оценка разговора | 3b или 7b — по bench (RAM: одна модель в памяти) | после submit |
| `eval_thorough` | переоценка | 7b/14b, полный промпт | фон/ночь: confidence < порога |
| `generate` | сценарии + бриф + чек-лист | 7b/14b | заранее, priority низкий |
| `stt_default` | распознавание | faster-whisper small int8 (движок сменяемый, см. model-selection) | синхронно |
| `tts_default` | озвучка | Silero v4 ru | синхронно и job |

Маппинг профиль → модель живёт в конфиге ai-service + settings (меняется админом
без правки контракта). Диспетчер группирует задачи по типу (`ORDER BY priority,
type, run_after`) — модели не свапаются вперемешку (RAM позволяет одну LLM).

## Правила эксплуатации контракта

* `request_id = ai_jobs.id` — ключ идемпотентности на обоих концах.
* 429 ≠ ошибка: не инкрементит try_count, не открывает breaker.
* `confidence < порога` (settings, дефолт 0.7) → evaluations "требует ревью" +
  кандидат на eval_thorough.
* Оба API — только docker-сеть + `X-Internal-Token` (env INTERNAL_API_TOKEN).
* ПДн в ai-service не уезжают: ни user_id, ни ФИО, ни lesson_id — только
  attempt_id и содержимое ответа. Аудио оператора в ai-service не сохраняется.
* Студенту через `frontend.v1.yaml` никогда не отдаются: эталон, `keyFacts`,
  `dialogue`-бриф, `expectedDialogue`, `revealedFactIds` — отдельные `Student*`-схемы.

## Кодогенерация

```
make contracts-lint    # redocly по всем четырём спекам
make generate          # Go-типы/клиент/сервер + Pydantic-модели + TS-типы фронта
make contracts-check   # CI: lint && generate && git diff --exit-code
```

* Go: `oapi-codegen` → `internal/gen/aiservice` (клиент), `internal/gen/callbacks` (сервер),
  `internal/gen/public` (сервер публичного API).
* TS: `openapi-typescript` → `frontend/src/shared/api/gen/frontend.v1.d.ts`; `httpApi`
  типизируется этим файлом, доменные типы фронта остаются в `shared/types`.
* Python: `datamodel-code-generator` → `ai_service/gen/models.py`; FastAPI-роуты
  пишутся руками против сгенерённых моделей (генерим модели, не сервер — осознанно).
* Спека меняется только PR'ом; руками сгенерённые файлы не правятся (CI-замок).
* Формы опросных карт по категориям в спеку НЕ зашиваются: `IncidentCard.attributes` —
  объект, валидируемый данными `classifier_categories.attributes` на границе go-core.
