# ai-service — ИИ-модуль тренажёра ДДС (Python)

Сервис без собственного состояния: генерирует сценарии, играет заявителя и оценивает ответы
обучающегося. Вызывается только из go-core, наружу не публикуется.

- Контракт: [`contracts/openapi/ai-service.v1.yaml`](../contracts/openapi/ai-service.v1.yaml) — здесь
  ai-service выступает сервером; [`go-internal.v1.yaml`](../contracts/openapi/go-internal.v1.yaml) —
  здесь он клиент, отправляющий обратные вызовы (callback) в go-core.
- Контекст: [`docs/contracts.md`](../docs/contracts.md), [`docs/voice-mode.md`](../docs/voice-mode.md),
  задачи и принятые решения — [`docs/tasks/python-ai-service.md`](../docs/tasks/python-ai-service.md).
- Основа промпта заявителя — прототип ai-service из ветки `ai-mode` (коммит `02d4fdb`): учебная
  рамка, сбивчивая речь, «стоять на своих фактах». В этой версии промпт расширен таблицей фактов
  с правилами раскрытия и JSON-выходом.

## Что делает

| Эндпоинт | Режим | Что происходит |
|---|---|---|
| `POST /v1/jobs/generate` | асинхронно: 202, затем callback | Сценарий вызова: легенда, бриф заявителя, эталонная карточка по кодам классификатора и служб, обязательные факты, ожидаемые действия, чек-лист разговора |
| `POST /v1/jobs/semantic` | асинхронно | Свободный текст против обязательных фактов: LLM выносит суждение по каждому факту, балл считает Python (`tasks/scoring.py`, `rules_version`) |
| `POST /v1/jobs/grammar` | асинхронно | LanguageTool ru-RU: замечания с позициями, вариантами исправления и серьёзностью, балл по плотности ошибок |
| `POST /v1/jobs/dialogue` | асинхронно | Чек-лист протокола разговора с репликами-доказательствами, запрещённые фразы, метрики речи (темп, слова-паразиты, время ответа) |
| `POST /v1/jobs/tts` | асинхронно | Озвучка реплик заявителя в общий volume `tts_cache` |
| `POST /v1/dialog/turn` | синхронно | Ход LLM-заявителя: реплика, раскрытые факты (`revealed_fact_ids`), признак конца разговора |
| `POST /v1/stt/transcribe` | синхронно | Распознавание реплики оператора (faster-whisper, ru), аудио не сохраняется |
| `POST /v1/tts/sync` | синхронно | Предпрослушивание реплики для преподавателя |
| `GET /v1/health`, `GET /v1/queue` | — | Состояние движков и очереди; go-core показывает их на экране «Состояние контура» |

Все вызовы требуют заголовок `X-Internal-Token` (общий секрет с go-core).

**Принцип оценки.** LLM отвечает только на узкие вопросы («факт отражён / частично / нет»,
«пункт чек-листа выполнен»). Итоговый балл считает Python по формуле с версией. Поэтому оценка
воспроизводима и объяснима.

**Очередь.** Полоса LLM одна (семафор = 1), приоритеты: диалог > оценка > генерация. При
перегрузке сервис отвечает 503 с заголовком `Retry-After`. Лимиты очередей и дедлайн хода
диалога задаются в `.env`.

## Структура

```
ai_service/
  api/        HTTP-слой: jobs, voice (синхронные), health
  core/       очередь с приоритетами, callback с повторами, ошибки, логирование
  engines/    llm/ollama, grammar/languagetool, stt/faster_whisper (+ ITN), tts/silero (+ нормализация текста)
  tasks/      generate, semantic, grammar, dialogue_eval, dialog_turn, scoring, speech_metrics
  gen/        Pydantic-модели из OpenAPI (make generate-python; руками не править)
config/       models.yaml (профили моделей), словари LanguageTool, слова-паразиты, сокращения для TTS
prompts/      промпты с версией в имени файла + few-shot по категориям происшествий
tests/        unit (формулы, разбор ответов LLM, очередь, текст) и contract (ответы и callback'и против OpenAPI)
tools/        smoke.py (прогон на реальных движках), callback_sink.py, models_pull.py
```

## Запуск в составе контура

Из корня репозитория:

```bash
docker compose -f docker-compose.yml -f ai-service/compose.ai.yaml up -d --build
# один раз, нужен интернет (или перенос volume ollama_models):
docker compose -f docker-compose.yml -f ai-service/compose.ai.yaml --profile pull run --rm ollama-pull
```

Запуск поднимает `ai-service`, `ollama` (Qwen2.5-7B-Instruct) и `languagetool`. Профиль `fake`
вместе с этим контуром не поднимайте: `fakeai` отвечает по тому же имени `ai-service`.

- Лёгкий образ без голоса: `AI_WITH_VOICE=0`. Тогда `/v1/health` отдаёт `tts=false, stt=false`,
  синхронные голосовые вызовы — 503, а go-core переходит на текстовый режим.
- Ollama уже запущена на хосте (Mac с Metal, общий кэш моделей):

  ```bash
  LLM_MODEL=gemma4:12b LLM_THINK=false docker compose -f docker-compose.yml \
    -f ai-service/compose.ai.yaml -f ai-service/compose.host-ollama.yaml up -d --build
  ```

## Локальная разработка

```bash
cd ai-service
uv sync                 # зависимости (голос: uv sync --extra voice + torch CPU)
make test               # pytest: 102 теста
make lint               # ruff
make sink               # приёмник callback'ов на :18081
make run                # сервис на :8000 против Ollama/LanguageTool на хосте
make smoke              # сквозной прогон на реальных движках с таблицей длительностей
```

Переменные окружения с пояснениями — в [`.env.example`](.env.example). Профили моделей — в
[`config/models.yaml`](config/models.yaml). Переменная `LLM_MODEL` задаёт одну модель сразу для
всех LLM-профилей.

## Ограничения

- На CPU ход диалога с моделью 7B q4 укладывается в дедлайн `DIALOG_DEADLINE_SEC` (18 с).
  Генерация сценария занимает десятки секунд, поэтому она асинхронная.
- Модели нужно скачать один раз заранее: в рабочем контуре ничего не загружается. Веса Silero и
  whisper запекаются в образ при `WITH_VOICE=1`, веса LLM переносятся через volume `ollama_models`.
- Режим разговора полудуплексный (push-to-talk), аудио оператора не сохраняется.
