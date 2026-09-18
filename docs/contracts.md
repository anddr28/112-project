# Контракт go-core ↔ ai-service

Источник истины — OpenAPI-спеки в `contracts/openapi/`. Этот документ — канон
разграничения ответственности; при конфликте с кодом прав контракт.

## Схема взаимодействия

```
студент ──submit──▶ go-core ──┐ мгновенно: поля + тайминг, evaluations(partial)
                              │ INSERT ai_jobs(queued)
                              ▼
                 диспетчер (SKIP LOCKED, группировка по типу/профилю)
                              │ POST ai-service /v1/jobs/{grammar|semantic|generate|tts}
                              │   202 → running   |   429 → ждём Retry-After
                              │   refused/timeout/5xx → circuit breaker (open 30s → half-open)
                              ▼
                        ai-service (stateless, asyncio)
                    полоса LLM (семафор=1)  |  полоса LanguageTool
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
| Генерация сценариев/эталонов | **ai-service** | spec категории → черновик → валидация преподавателем |
| TTS | **ai-service** (Silero) | text → файл в shared volume + duration |
| Аналитика, рекомендации, отчёты | **go-core** | evaluations/attempts → recommendations, reports |

Исключение (осознанное): нормировку `grammar.score` из ошибок LT делает ai-service —
формула привязана к NLP-специфике и тюнится AI-разработчиком; её версия едет в
`engine.rules_version` для воспроизводимости.

## Профили моделей

| Профиль | Задача | Модель (дефолт) | Когда |
|---|---|---|---|
| `eval_fast` | семантика | 3b q4, короткий промпт, num_predict с потолком | на занятии |
| `eval_thorough` | переоценка | 7b/14b, полный промпт | фон/ночь: confidence < порога |
| `generate` | сценарии | 7b/14b | заранее, priority низкий |

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
  attempt_id и содержимое ответа.

## Кодогенерация

```
make generate          # Go-типы/клиент/сервер + Pydantic-модели
make contracts-check   # CI: generate && git diff --exit-code
```

* Go: `oapi-codegen` → `internal/gen/aiservice` (клиент), `internal/gen/callbacks` (сервер).
* Python: `datamodel-code-generator` → `ai_service/gen/models.py`; FastAPI-роуты
  пишутся руками против сгенерённых моделей (генерим модели, не сервер — осознанно).
* Спека меняется только PR'ом; руками сгенерённые файлы не правятся (CI-замок).
* Формы опросных карт по категориям в спеку НЕ зашиваются: `IncidentCard.attributes` —
  объект, валидируемый данными `classifier_categories.attributes` на границе go-core.
