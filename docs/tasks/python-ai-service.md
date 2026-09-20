# Задачи: ai-service (Python)

Владелец: питонист. Срок: freeze **29.09**. Контракт: `contracts/openapi/ai-service.v1.yaml`
(сервер — ты), `go-internal.v1.yaml` (клиент — ты), общие схемы `_components.yaml`.
Контекст: `docs/voice-mode.md`, `docs/contracts.md`. Выбор моделей — `ml-research.md`
(делается параллельно с QA, результаты кладутся в `config/models.yaml`).

Принцип, который держим весь проект: **сервис stateless, всё детерминированное —
в коде, LLM отвечает на вопросы «да/нет/частично», а балл считает Python по
формуле с версией**. Так оценка воспроизводима (ТЗ требует QA модели) и не плавает
от запуска к запуску.

## 0. Что зафиксировано и не обсуждается

- FastAPI + uvicorn, Python 3.11, Pydantic v2 (модели генерируются: `make generate-python`).
- Ollama как LLM-рантайм (одна модель в памяти, семафор=1 на полосу LLM).
- LanguageTool self-hosted (HTTP API, официальный docker-образ).
- Silero TTS v4 ru. STT — faster-whisper по умолчанию, движок сменяемый (см. ML-01).
- Fire-and-forget + callback для всех задач, кроме трёх синхронных голосовых.
- Никаких облачных API, ничего не качается в рантайме: модели в образе/volume.
- Отклонено ранее: Celery, Redis, RabbitMQ, несколько LLM параллельно. Не поднимать.

## 1. Структура репозитория `ai-service/`

```
ai-service/
  pyproject.toml            # ruff, pytest, deps; uv.lock (или requirements.txt)
  Dockerfile                # multi-stage, CPU torch, модели запечены (см. §2)
  compose.ai.yaml           # фрагмент для корневого docker-compose: ai-service, ollama, languagetool
  .env.example
  README.md                 # PY-13
  config/
    models.yaml             # профили: dialog_fast, eval_fast, eval_thorough, eval_dialogue, generate, stt_default, tts_default
    fillers_ru.txt          # слова-паразиты для SpeechMetrics
    tts_abbrev_ru.yaml      # раскрытие сокращений для TTS
  prompts/                  # ТОЛЬКО текст промптов, версия в имени файла
    caller/v1.md  caller/v2.md
    semantic/v1.md
    dialogue_eval/v1.md
    generate/v1.md
    _fewshot/…              # примеры для генерации по категориям (собирает QA)
  ai_service/
    main.py                 # app factory, lifespan (прогрев моделей), роуты
    config.py               # pydantic-settings из env + models.yaml
    gen/                    # СГЕНЕРИРОВАНО, не править
    api/                    # роуты: jobs.py, stt.py, dialog.py, tts.py, health.py
    core/
      queue.py              # полосы, приоритеты, реестр задач, идемпотентность
      callback.py           # POST в go-core с ретраями
      registry.py           # реестр промптов (имя+версия -> текст, схема выхода)
      errors.py
    engines/
      llm/ollama.py         # клиент Ollama: chat+json, ретрай починки JSON, метрики
      stt/base.py, faster_whisper.py, (gigaam.py, vosk.py — после bench)
      stt/itn_ru.py         # числительные -> цифры
      tts/silero.py, tts/textnorm.py
      grammar/languagetool.py
    tasks/
      grammar.py            # слой 2
      semantic.py           # слой 3
      dialogue_eval.py      # слой dialogue (LLM-часть + метрики)
      speech_metrics.py     # детерминированные метрики речи
      dialog_turn.py        # оркестрация хода: STT -> LLM-заявитель -> TTS
      generate.py           # сценарий + эталон + бриф + чек-лист
      scoring.py            # ВСЕ формулы баллов, с версиями (rules_version)
  bench/                    # ml-research.md: датасеты, скрипты, отчёты
  tests/
    unit/  contract/  fixtures/
  tools/
    callback_sink.py        # заглушка go-core для локальной разработки
    smoke.py                # сквозной прогон всех эндпоинтов для QA
    models_pull.py          # скачать веса в models/ (один раз, с интернетом)
```

## 2. Docker и офлайн

**PY-01 — скелет, конфиг, health, Dockerfile, compose.** *(0.5 дня, до M0 22.09)*

- `Dockerfile`: `python:3.11-slim`, `ffmpeg` (декод webm/opus для STT), torch CPU
  (`--index-url https://download.pytorch.org/whl/cpu`), `faster-whisper`, `silero`
  (файл `v4_ru.pt`), `num2words`, `httpx`, `pydantic-settings`, `av`. Модели скачиваются
  на этапе сборки `tools/models_pull.py` (whisper small int8 → `models/whisper/`,
  silero → `models/silero/`) и **запекаются в образ** — на стенде интернета нет.
  Размер ~2.5 ГБ — норм. Тег: `lct/ai-service:<git-sha>` и `:m0/:m1/…` для QA.
- `compose.ai.yaml`:
  ```yaml
  ai-service:   build ./ai-service; env_file .env; volumes: tts_cache:/data/tts;
                cpuset "2-7"; mem_limit 4g; healthcheck GET /v1/health; depends_on ollama, languagetool
  ollama:       image ollama/ollama; volumes: ollama_models:/root/.ollama; cpuset "2-7"; mem_limit 9g;
                environment OLLAMA_NUM_PARALLEL=1 (поднимаем по bench), OLLAMA_KEEP_ALIVE=30m, OLLAMA_MAX_LOADED_MODELS=1
  ollama-init:  one-shot: ollama pull для моделей из config/models.yaml (нужен интернет один раз;
                для передачи на стенд — tar volume `ollama_models`)
  languagetool: image erikvl87/languagetool; environment langtool_languageModel=… (n-gram опционально); mem_limit 1g
  ```
  Общий volume `tts_cache` монтируется и в go-core (он раздаёт `/media/tts/…`).
- `config.py`: все настройки из env (таблица в §6) + `config/models.yaml`.
- `GET /v1/health`: `ollama` (ping `/api/tags`), `languagetool` (`/v2/languages`),
  `tts` (модель загружена), `stt` (модель загружена), `models_available`, `profiles`.
  `status=degraded`, если что-то из этого false — сервис всё равно живёт, соответствующие
  задачи возвращают `failed` с `retryable=true`.
- `lifespan`: прогрев — загрузить STT и TTS в память, один тестовый `chat` в Ollama
  с моделью `dialog_fast` (чтобы первая реплика на занятии не ждала загрузки весов).
- Аутентификация: middleware `X-Internal-Token` для всего, кроме `/v1/health`.

Приёмка: `docker compose up` на чистой машине без интернета (после `ollama-init`) →
`/v1/health` = `ok`; образ передан QA (`docker save` или registry).

**PY-02 — очередь, идемпотентность, callback.** *(1 день, до M0)*

- `core/queue.py`:
  - Полосы: `llm` (семафор 1, **PriorityQueue** с ключом `(class, priority, seq)`,
    class: 0 = dialog_turn, 1 = evaluate_*, 2 = generate), `lt` (семафор 4),
    `stt` (семафор 1), `tts` (семафор 1). Фоновая LLM-задача берётся **только если
    очередь диалогов пуста**.
  - Реестр задач `request_id → {status, result, submitted_at}` (LRU ~2000, TTL 1 ч):
    повтор в очереди/работе → 202 без дубля; повтор done → 202 + повторный callback.
  - Лимит очереди по типу (`QUEUE_MAX_<TYPE>`) → 429 + `Retry-After` (не ошибка).
  - `GET /v1/queue`: `pending` по типам, `running`, `current_model`, `llm_loaded`,
    `est_wait_sec` (скользящее среднее по 20 последним), `dialog_waiting`, `dialog_avg_ms`.
- `core/callback.py`: POST `/internal/ai/v1/results`, ретраи 1/5/30 с, после — лог
  `callback_dropped` с `request_id`; всегда ровно одно поле результата по `type`;
  `engine` заполняется всегда (в т.ч. при `failed`).
- Ошибки движков → `AiJobError` с корректным `retryable` (таблица в `go-internal.v1.yaml`).
- Graceful shutdown: SIGTERM → перестать принимать, дать 30 с текущей задаче, выйти.

Приёмка: `tools/callback_sink.py` принимает результаты; тест «дважды POST одной задачи»
→ один результат; тест «переполнение» → 429; тест «диалог обгоняет генерацию».

## 3. Слои оценки и движки

**PY-03 — грамматика (LanguageTool).** *(0.5 дня, M1)*

- Клиент `POST /v2/check` (`language=ru-RU`, `enabledOnly=false`, `disabledRules`
  из `options.exclude_rules` + дефолтный список шумных правил, который соберёт QA:
  капитализация после сокращений, пробелы в номерах, `MORFOLOGIK_RULE_RU_RU` для
  фамилий и т.п.).
- Маппинг `matches[]` → `GrammarRemark {field, offset, length, severity, rule, message, suggestions[:3]}`;
  severity: `rule.issueType` misspelling/grammar → `error`, typographical/punctuation → `warning`,
  style → `style`.
- `scoring.grammar_v1(words, E, W, S, strict)`:
  `score = clamp(100 − (12·E + 5·W + (2·S если strict)) · 100 / max(words, 30), 0, 100)`,
  `rules_version = "gr-v1"`. Формула тюнится по bench (ML-05), версия меняется.
- Словарь домена (адреса, «ДДС», «АРМ», названия служб) — `config/lt_dictionary_ru.txt`,
  подключается через `--languageModel`/`userDictionary`, чтобы LT не подчёркивал каждую улицу.

Приёмка: 10 фикстур из `tests/fixtures/grammar/` (текст → ожидаемые E/W) проходят;
адрес «ул. Ленина, д. 14, кв. 7» даёт 0 ошибок.

**PY-04 — клиент Ollama и реестр промптов.** *(0.5 дня, M1)*

- `engines/llm/ollama.py`: `chat(profile, messages, json_schema) -> (obj, Engine)`.
  `POST /api/chat` с `format` = JSON-схема выхода (Ollama ≥ 0.5 поддерживает schema;
  fallback `format: json`), `options {temperature, num_predict, num_ctx, seed}` из
  профиля, `keep_alive` из конфига, `stream: false`. Таймаут из профиля
  (dialog_fast 12 с, eval 90 с, generate 240 с). Токены — из `prompt_eval_count/eval_count`
  → `Engine.tokens_in/out`, `duration_ms` из `eval_duration`.
- Невалидный JSON → одна попытка «почини свой JSON» с тем же контекстом → если
  снова мимо — `llm_invalid_output`.
- `core/registry.py`: промпт = файл `prompts/<task>/v<N>.md` с фронт-маттером
  (`version`, `output_schema`, `profile_default`). В `Engine.prompt_version` всегда
  идёт `"<task>-v<N>"`. **Промпты не хардкодятся в .py** — QA правит текст без кода.
- `config/models.yaml` — профили (значения по умолчанию, ML-bench уточнит):
  ```yaml
  profiles:
    dialog_fast:   {model: qwen2.5:3b-instruct-q4_K_M, temperature: 0.4, num_predict: 90,  num_ctx: 4096, timeout_s: 12}
    eval_fast:     {model: qwen2.5:3b-instruct-q4_K_M, temperature: 0.0, num_predict: 400, num_ctx: 6144, timeout_s: 90}
    eval_dialogue: {model: qwen2.5:7b-instruct-q4_K_M, temperature: 0.0, num_predict: 600, num_ctx: 8192, timeout_s: 120}
    eval_thorough: {model: qwen2.5:7b-instruct-q4_K_M, temperature: 0.0, num_predict: 600, num_ctx: 8192, timeout_s: 180}
    generate:      {model: qwen2.5:7b-instruct-q4_K_M, temperature: 0.7, num_predict: 1500, num_ctx: 8192, timeout_s: 240}
    stt_default:   {engine: faster_whisper, model: small, compute_type: int8, beam_size: 2, cpu_threads: 4}
    tts_default:   {engine: silero, model: v4_ru, voice: baya, sample_rate: 24000}
  ```
  Важно: два разных LLM в памяти на 16 ГБ не живут; если `eval_dialogue` = 7b, а
  `dialog_fast` = 3b — Ollama будет свапать. **Решение по bench (ML-02/03)**: либо
  всё на одной модели, либо 7b-оценка только ночью/после занятия. До bench — всё на 3b.

**PY-05 — семантика (слой 3).** *(1 день, M1)*

- Вход: `SemanticJobRequest` (режимы `cards` и `card_actions`).
- Промпт `semantic/v1`: экзаменатор; ему даются `required_facts`, `forbidden_facts`,
  эталонный текст поля, ответ студента, контекст легенды. **Выход — таблица суждений**,
  а не балл:
  ```json
  {"facts": [{"fact": "...", "status": "present|partial|missing", "evidence": "цитата"}],
   "forbidden_found": [{"fact": "...", "evidence": "..."}],
   "coherence": "ok|weak|contradiction",
   "per_field_comment": {"description": "..."},
   "summary_for_student": "...", "self_confidence": 0.0-1.0}
  ```
- `scoring.semantic_v1`: `score = 100 · Σ(present=1, partial=0.5) / N_required − 15·|forbidden_found|
  − (10 если coherence=weak, 30 если contradiction)`, clamp; `missing_facts` = status=missing;
  `extra_facts` = forbidden_found. `confidence = 0.5·self_confidence + 0.5·(доля фактов с непустым evidence)`;
  если ответ короче 5 слов — `confidence ≤ 0.5`.
- `card_actions`: то же, но `required_facts` берутся из `expected_actions[].required_facts`.

Приёмка: на 10 фикстурах (ответ студента ↔ ожидаемые missing/extra, составляет QA)
совпадение по missing_facts ≥ 80 %; один и тот же вход ×3 при temperature 0 даёт
один и тот же score.

**PY-06 — STT.** *(1 день, M1)*

- `engines/stt/base.py`: `class SttEngine: transcribe(audio: bytes, mime, opts) -> SttResult`.
  Выбор через `STT_ENGINE` (`faster_whisper` | `gigaam` | `vosk`) — по итогам ML-01
  подключаются дополнительные реализации; интерфейс один.
- `faster_whisper.py`: `WhisperModel(path, device="cpu", compute_type="int8", cpu_threads=N)`;
  `transcribe(language="ru", beam_size, vad_filter=True, initial_prompt=hints,
  condition_on_previous_text=False, no_speech_threshold=0.6)`. Декод webm/opus/ogg/wav
  через PyAV/ffmpeg → PCM 16 kHz mono. `confidence = mean(exp(avg_logprob))` по сегментам;
  `no_speech = (нет сегментов) или (no_speech_prob > 0.6 у всех)`.
- `hints` из `SttOptions.hints` (go-core кладёт улицу, имя заявителя, термины) →
  `initial_prompt = "Служба 112. " + ", ".join(hints)`.
- `itn_ru.py`: числительные → цифры («дом четырнадцать» → «дом 14», «сто двенадцать»
  → «112», «пятый этаж» → «5 этаж», телефон «восемь девятьсот шестнадцать…» → «8 916…»).
  Только правило-based, покрытие 0–9999 + порядковые + группы для телефонов; тесты.
  `text_raw` — до ITN, `text` — после.
- Лимиты: `STT_MAX_AUDIO_SEC=60`, `STT_MAX_BYTES=2 МБ` → 413; неизвестный контейнер → 415.
- `POST /v1/stt/transcribe` — сам по себе (используется bench и фронтом через go-core при
  `input=voice` без диалога — например, режим `card_actions` голосом, P2).

Приёмка: bench-набор QA-04 (см. `ml-research.md`) прогоняется `bench/stt_eval.py`;
RTF ≤ 0.4 на стенде; «улица Ленина дом четырнадцать подъезд три» → «улица Ленина дом 14 подъезд 3».

**PY-08 — TTS.** *(0.5 дня, M1; раньше PY-07, потому что нужен ему)*

- `engines/tts/silero.py`: загрузка `v4_ru.pt` через `torch.package` (без torch.hub —
  офлайн), `apply_tts(text, speaker, sample_rate=24000)`, запись wav 16-bit mono.
- `textnorm.py` — **обязательно**, иначе Silero читает «д. 14» как «дэ четырнадцать»:
  `num2words(lang="ru")` для чисел (с падежом там, где очевидно: «14 этаж» → «четырнадцатый»
  не пытаемся — читаем «четырнадцать этаж», это норм для заявителя в панике);
  раскрытие сокращений из `config/tts_abbrev_ru.yaml` (ул., д., кв., корп., стр., эт.,
  ДТП, ЖК, ТЦ, «112» → «сто двенадцать»); латиница → транслит; удаление markdown/кавычек.
- Пути в shared volume: диалог — `dialog/<attempt_id>/<turn_no>.wav` (transient,
  чистится go-core по завершении занятия); кэш — `cache/<hash[:2]>/<hash>.wav`.
  Служебные фразы (`common/allo_slyshite.wav`, «ой, секунду», «повторите, плохо слышно»)
  генерируются при старте, если нет.
- `POST /v1/jobs/tts` (уже в контракте) и `POST /v1/tts/sync` — один код.
- Эмоция (P1): Silero принимает SSML `<prosody rate="..." pitch="...">` — маппинг
  `emotional_state` → rate/pitch в `config/models.yaml`.

Приёмка: 20 фраз из `tests/fixtures/tts/` (адреса, телефоны, сокращения) — на слух
без «дэ», «ка-вэ» и латиницы (проверяет QA, ML-04); RTF ≤ 0.3.

**PY-07 — ход диалога (главная задача).** *(1.5 дня, M2)*

- `POST /v1/dialog/turn` (multipart `request` + `audio`, либо JSON с `operator_text`).
- `tasks/dialog_turn.py`:
  1. Если `operator_text` — пропустить STT. Иначе STT (полоса `stt`); `no_speech` →
     ответ-переспрос из служебных фраз, **без LLM**, `operator.no_speech=true`.
  2. Собрать промпт `caller/v1`:
     - system: persona, speaking_style, эмоциональное состояние; **таблица фактов**
       `id | text | reveal | hints` с правилами: `volunteer` — можно сказать сам,
       `on_request` — только если оператор спросил об этом (hints — ориентир),
       `never` — никогда; `unknowns` — на это отвечать «не знаю / не вижу»;
       уже раскрытые `revealed_fact_ids` — не повторять, если не переспросили;
       ограничения: ≤ `max_reply_words`, разговорный русский, без списков и без
       упоминания id, одна-две реплики, не задавать оператору более одного вопроса.
     - history: последние `DIALOG_HISTORY_TURNS=8` реплик как chat-сообщения
       (оператор = user, заявитель = assistant).
     - user: реплика оператора.
     - Выход (JSON-схема): `{reply, revealed_fact_ids[], is_unknown_answer, should_end, end_reason, emotional_state}`.
  3. Пост-проверки в коде (не доверяем модели):
     - `revealed_fact_ids ⊆ facts.id`; факты с `reveal=never` — вычеркнуть из списка и
       убедиться, что ключевые слова их текста не встречаются в `reply` (нормализованный
       substring по леммам-заглушкам: нижний регистр, без пунктуации, 2 самых длинных слова факта);
       нарушение → один повтор с добавленной строгой инструкцией → потом fallback;
     - `reply` обрезать по границе предложения до `max_reply_words`;
     - `should_end=true` разрешить только если `turn_no ≥ 3` или оператор явно
       попрощался (ключевые фразы) — иначе модели свойственно «прощаться» рано;
     - `turn_no ≥ dialogue.max_turns` → `should_end=true, end_reason="max_turns"`.
  4. LLM недоступна / таймаут / дважды невалидный JSON → **fallback**: следующая
     сценарная реплика `call_script.turns[k]` по кругу (k = число caller-реплик в history),
     `fallback=true`. Занятие не срывается.
  5. TTS ответа (если `options.tts.enabled`), путь `dialog/<attempt>/<turn>.wav`.
  6. `Engine`: `stt_model`, `llm_model`, `prompt_version`, `tts_version`, `duration_ms`
     (сумма этапов), `queue_wait_ms` (ожидание полосы LLM), токены.
  7. Backpressure: если `dialog_waiting ≥ DIALOG_MAX_WAITING` → 503 + `Retry-After: 3`.
- Логи по ходу: `request_id, attempt_id, turn_no, stt_ms, llm_ms, tts_ms, wait_ms,
  fallback, revealed` — QA и bench их читают.

Приёмка (сценарий QA-05 текстом, детерминированно при `temperature 0`, `seed`):
- оператор спрашивает «какой подъезд?» → в `revealed_fact_ids` появляется факт подъезда;
- оператор не спрашивал про человека в квартире → факт `on_request` не звучит 5 ходов подряд;
- факт `never` не появляется ни в одном из 20 прогонов;
- «повторите адрес» → адрес повторён без новых фактов;
- «помощь направлена, оставайтесь на связи» → `should_end=true` в ≤ 2 хода;
- Ollama остановлен → ответ приходит из `turns[]`, `fallback=true`, 200.

**PY-09 — оценка разговора (слой dialogue).** *(1 день, M3)*

- `POST /v1/jobs/dialogue` → задача `evaluate_dialogue` (полоса LLM, class 1).
- `tasks/speech_metrics.py` — **без LLM, всегда**: `operator_turns/words/talk_ms`,
  `words_per_min = words / (talk_ms/60000)`, `fillers` по `config/fillers_ru.txt`
  (по словам и биграммам: «как бы», «это самое»), `avg/max_response_ms` = пауза между
  `caller.at_ms + audio_duration_ms` и следующим `operator.at_ms`, `low_confidence_turns`
  (confidence < `stt_confidence_floor`).
- Промпт `dialogue_eval/v1`: транскрипт с номерами реплик + чек-лист `expected_dialogue`
  + `forbidden`. Выход — по пункту: `{id, status: done|partial|missed|not_applicable,
  evidence_turn_no, comment}`, `forbidden_hits[{phrase, turn_no}]`, `tone {politeness,
  calmness, clarity, comment}`, `summary_for_student`, `self_confidence`.
  Инструкция модели: реплики оператора с низким confidence STT трактовать в его пользу.
- `scoring.dialogue_v1`: `base = 100 · Σ w·(done=1, partial=0.5, missed=0) / Σ w`
  по `required` пунктам (необязательные — только бонус до +5); `− 10·|forbidden_hits|`
  (cap 30); `− 5`, если `fillers/100 слов > 5`; `− 5`, если `avg_response_ms > 4000`;
  clamp 0..100; `rules_version="dlg-v1"`. `confidence` = `self_confidence`, понижается
  на 0.2, если `low_confidence_turns / operator_turns > 0.3`.
- Если LLM упала — результат `failed retryable`, **но** `speech` метрики всё равно
  логируются (go-core получит их со следующей попытки).

Приёмка: golden set QA-06 (≥ 30 транскриптов с ручной разметкой чек-листа):
точность статусов пунктов ≥ 80 %, Spearman по score ≥ 0.7 (ML-03).

**PY-10 — генерация сценария с брифом и чек-листом.** *(1 день, M2)*

- Промпт `generate/v1` по `spec` (категория, признаки опросной карты, службы,
  сложность, `teacher_comment`, `avoid_titles`). Выход — `ScenarioResult` целиком:
  `title`, `call_script` (caller, address, key_facts, **dialogue** {persona,
  speaking_style, facts с reveal, unknowns, end_conditions}, turns — минимум
  вступительная реплика + 3–5 резервных), `etalon_card`, `expected_actions` (для
  `card_actions`/`both`), **`expected_dialogue`** (чек-лист 5–8 пунктов),
  `difficulty_estimate`, `notes_for_teacher`.
- Few-shot по категориям из `prompts/_fewshot/` — базовые чек-листы протокола
  (пожар / ДТП / медицина / газ / преступление) пишет QA с питонистом по памятке АРМ-112
  и билетам; модель адаптирует их под легенду, а не выдумывает с нуля.
- Валидация выхода Pydantic + инварианты: `facts[].id` уникальны; `key_facts` =
  `facts[].text`; адрес легенды = адрес эталона; `services_to_notify ⊆ spec.services`
  (иначе предупреждение в `notes_for_teacher`); чек-лист содержит `ask_address`,
  `say_help_dispatched`. Нарушение → один повтор с указанием ошибок → потом `llm_invalid_output`.
- Сложность 1/2/3 влияет на: число `on_request`-фактов, `unknowns`, эмоциональность,
  наличие `never`-фактов (ловушки).

Приёмка: 5 генераций на категорию × 4 категории — все проходят валидацию; QA
оценивает по рубрике ML-06 (реализм, согласованность карточки и легенды, полезность чек-листа).

## 4. Качество, наблюдаемость, тесты

**PY-11 — наблюдаемость.** *(0.5 дня, M3)*
- Структурные JSON-логи (`structlog`/`logging` + JSON formatter) с `request_id`,
  `attempt_id`, `type`, `profile`, стадии и длительности. Уровень из `LOG_LEVEL`.
- `/v1/queue` уже покрывает панель преподавателя; `/metrics` Prometheus — опционально (P1).
- Счётчики: задачи по типу/статусу, p50/p95 длительности по типам, `fallback` диалогов,
  `callback_dropped`.

**PY-12 — тесты и инструменты для QA.** *(1 день, размазать по всем задачам)*
- `pytest` unit: `itn_ru`, `textnorm`, `scoring_*` (таблицы кейсов), `speech_metrics`,
  пост-проверки диалога (утечки `never`), реестр задач (идемпотентность, 429).
- Контрактные: `schemathesis run contracts/openapi/ai-service.v1.yaml --base-url …`
  на запущенном сервисе с моками движков (`ENGINES=mock` — все движки отдают
  фиксированные ответы за 0 мс; этот же режим — для CI и для фронта, если нужен
  «быстрый Python»).
- `tools/callback_sink.py` — HTTP-приёмник callback'ов, печатает и складывает в
  `./sink/<request_id>.json`.
- `tools/smoke.py` — сквозной прогон: health → tts sync → stt (wav из fixtures) →
  dialog turn текстом → dialog turn аудио → jobs grammar/semantic/dialogue → ждёт
  callback'и в sink → выводит таблицу длительностей. **Это главный инструмент QA.**
- `make` цели в `ai-service/Makefile`: `run`, `test`, `lint` (ruff), `smoke`, `bench-stt`, `bench-llm`, `image`.

**PY-13 — документация.** *(0.5 дня, M4)*
- `ai-service/README.md`: запуск, env, профили, как поменять модель, как добавить
  STT-движок, лимиты и ожидаемые задержки на CPU, известные ограничения.
- Model card в `docs/model-selection.md` (совместно, ML-07): выбранные модели, метрики,
  датасеты, версии промптов, формулы баллов и их версии.
- ТЗ требует «описание методов и ограничений» — это оно.

## 5. Порядок и приоритеты

```
M0 22.09: PY-01, PY-02                                   (образ у QA)
M1 24.09: PY-08 → PY-06 → PY-04 → PY-03 → PY-05           (движки живые)
M2 26.09: PY-07 (диалог) → PY-10 (генерация)              (сквозной ход через go-core)
M3 28.09: PY-09 (оценка диалога) → PY-11, PY-12 добить
M4 29.09: PY-13, образ :release, ollama_models tar для стенда
```

Если не успеваем — режем в таком порядке: эмоции TTS (P1) → `eval_dialogue` на 7b
(всё на 3b) → `tone` в оценке диалога (только чек-лист + метрики) → генерация
`expected_actions` (режим `card_actions` без LLM-эталона). **Не режем**: диалог с
дисциплиной фактов, STT, текстовый fallback, чек-лист.

## 6. Переменные окружения

| Переменная | Дефолт | Назначение |
|---|---|---|
| `INTERNAL_API_TOKEN` | — | общий секрет с go-core |
| `GO_CORE_URL` | `http://go-core:8080` | callback |
| `OLLAMA_URL` | `http://ollama:11434` | |
| `LANGUAGETOOL_URL` | `http://languagetool:8010` | |
| `MODELS_DIR` | `/models` | whisper, silero |
| `TTS_DIR` | `/data/tts` | shared volume с go-core |
| `STT_ENGINE` | `faster_whisper` | `gigaam` / `vosk` после bench |
| `STT_MAX_AUDIO_SEC` / `STT_MAX_BYTES` | `60` / `2097152` | 413 |
| `DIALOG_MAX_WAITING` | `8` | 503 порог |
| `DIALOG_HISTORY_TURNS` | `8` | окно контекста |
| `QUEUE_MAX_SEMANTIC` / `_GRAMMAR` / `_GENERATE` / `_TTS` / `_DIALOGUE` | `200/500/20/200/200` | 429 |
| `CALLBACK_RETRIES` | `1,5,30` | секунды |
| `ENGINES` | `real` | `mock` — все движки-заглушки (CI, фронт) |
| `LOG_LEVEL` | `info` | |
| `CPU_THREADS` | `4` | whisper/torch |

## 7. Definition of Done

- Все эндпоинты `ai-service.v1.yaml` реализованы; `schemathesis` зелёный в режиме `mock`.
- `tools/smoke.py` проходит на реальных движках на стенде 8 ядер / 16 ГБ; ход диалога
  по тексту ≤ 5 с p50, по аудио ≤ 7 с p50.
- Образ работает офлайн; `docker compose up` из README воспроизводится QA без вопросов.
- Все формулы баллов — в `scoring.py`, с версиями, покрыты тестами.
- `docs/model-selection.md` заполнен, промпты версионированы.
