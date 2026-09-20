# Задачи: frontend (SPA, клон АРМ-112)

Владелец: фронтендер. Срок: freeze **29.09**. Контракт: `contracts/openapi/frontend.v1.yaml`
(**новый**, camelCase, зеркалит `shared/types`), сгенерированные типы —
`frontend/src/shared/api/gen/frontend.v1.d.ts` (`make generate-ts`, не править руками).
Контекст: `docs/voice-mode.md`. Что уже есть: все три кабинета, АРМ-112, карточка,
статусы реагирования, результат и отчёт — на моках. Голоса нет вообще.

Принципы, которые не меняем: UI знает только интерфейс `shared/api/types.ts`;
моки остаются рабочими всегда (демо на них — план Б); студенту не приходят
эталон/бриф/факты — это гарантирует сервер, но фронт тоже не хранит их в state студента.

## 0. Сначала прочитать в спеке

- `§dialogue`: `POST /attempts/{id}/dialogue/turns` (multipart `audio` | JSON `text`),
  `GET /attempts/{id}/dialogue`, `POST …/dialogue/end`, `GET /media/tts/{path}`.
- `POST /attempts/{id}/accept-call` теперь возвращает `{attempt, opening}` — вступительная
  реплика с `audio.audioUrl`, чтобы начать воспроизведение по жесту «Принять».
- `LessonSettings.voice` и `weights.dialogue`; `Attempt.voice`; `StudentCallScript.voice`.
- `Evaluation`: `dialogueScore`, `dialogue`, `layers` (состояние AI-слоёв), `weights`.
- `Scenario`: `callScript.dialogue` (бриф), `expectedDialogue` (чек-лист), `generationMeta`, `ttsReady`.
- `AttemptEventType`: + `mic_check`, `ptt_start`, `ptt_stop`, `dialogue_operator`,
  `dialogue_caller`, `dialogue_ended`.
- WS: `/ws/lessons/{id}/monitor` (`MonitorMessage`), `/ws/attempts/{id}` (`StudentMessage`),
  reconnect с `?since=`.
- Ошибки: `ApiError.code` — `caller_busy` (503), `audio_too_long` (413),
  `audio_unsupported` (415), `unauthorized`, `forbidden`, `conflict`.

## 1. Слой API

**FE-01 — `httpApi` по `frontend.v1.yaml`.** *(1.5 дня; каркас к M0, полностью к M2)*

- `shared/api/http/client.ts`: `fetch` с `credentials: 'include'`, заголовок
  `X-Requested-With: fetch` на мутациях, парсинг `ApiError` в `class ApiHttpError
  {status, code, message, details}`; 401 → сброс сессии и редирект на `/login`
  (кроме самого `/auth/me`); таймауты (`AbortController`, 15 с; для `dialogue/turns` — 25 с).
- `shared/api/http/httpApi.ts`: реализация `Api` метод-в-метод. Типы запросов/ответов —
  из `gen/frontend.v1.d.ts` (`paths['/attempts/{attemptId}/dialogue/turns']['post']…`),
  доменные типы — как раньше из `shared/types`; там, где они расходятся (быть не должно),
  править `domain.ts` вслед за спекой, а не наоборот.
- `shared/api/index.ts`: `VITE_USE_MOCKS !== 'false' ? mockApi : httpApi` (как и задумано).
- `evaluation.get`: 404 → `null`.
- Обновить `Api` в `types.ts`: добавить `dialogue: {get, turn, end}`, `scenarios.ttsPreview`,
  `aiJobs.get`, `admin.health/settings`, `users.create/update`; `lessons.create` принимает
  `voice` и `weights`.
- Dev-прокси в `vite.config.ts`: `/api` → `https://localhost:8443` (go-core), WS тоже.

Приёмка: все страницы работают на `httpApi` против go-core лида (M2); переключение
флагом без правки компонентов; ошибки показываются текстом `message` с сервера.

**FE-12 — моки голосового режима.** *(0.5 дня, M0 — раньше всего остального)*

- `mockApi.dialogue.turn()`: ответ заявителя по ключевым словам реплики оператора
  (`подъезд/этаж` → факт адреса, `люди/кто-нибудь` → «там дедушка…», `телефон` →
  номер, `помощь направлена` → `callEnded`), иначе — паническая реплика по кругу;
  задержка 1.5–3 с для реалистичных loading-состояний; каждые ~6 ходов — `callEnded`.
  Раз в 8 запросов — `503 caller_busy` (проверить UX ошибки). Аудио — статические
  файлы `public/mock-audio/caller_*.wav` (10–15 коротких фраз, запишет QA — задача QA-04-б).
- `mockApi.attempts.acceptCall()` возвращает `opening`; `mockApi.evaluation.get()`
  отдаёт `dialogue` с чек-листом и метриками; `mockApi.scenarios.generate()` — с брифом
  и чек-листом; `lessons.defaultSettings()` — с `voice` и `weights.dialogue`.
- Фикстуры: `shared/mocks/fixtures/dialogue.ts` (бриф, чек-лист, два готовых транскрипта).

## 2. Голос в рабочем месте студента

**FE-02 — запись: `features/voice/`.** *(1 день, M1)*

- `useRecorder.ts` — конечный автомат `idle → requesting → ready → recording → sending
  → waitingCaller → playingCaller → ready`, плюс `denied` и `unsupported`. Никаких
  переходов «вбок»: пока `playingCaller`, запись невозможна (полудуплекс).
  `getUserMedia({audio: {echoCancellation: true, noiseSuppression: true, autoGainControl: true,
  channelCount: 1}})`, `MediaRecorder` с первым поддерживаемым из
  `['audio/webm;codecs=opus', 'audio/ogg;codecs=opus', 'audio/webm']` (Chrome/Яндекс — webm,
  Firefox — webm или ogg). `timeslice` не нужен: один Blob на реплику.
  Максимум 60 с — автостоп с уведомлением. Минимум 300 мс — короче не отправляем.
- `usePushToTalk.ts`: удержание кнопки (pointerdown/up, с `pointercancel`) и горячая
  клавиша. **Не `Space`** — студент печатает в карточке. Дефолт: `Ctrl` (удержание) —
  не конфликтует с вводом; альтернативно режим «клик — старт / клик — стоп»
  (`settings.voice.pushToTalk=false`). Показывать подсказку клавиши на кнопке.
- `LevelMeter.tsx`: индикатор уровня через `AnalyserNode` (видно, что микрофон живой).
- `useAudioQueue.ts`: очередь воспроизведения `<audio>`; `play(url) → Promise<void>`,
  `stop()`; после `ended` — переход `playingCaller → ready`. Ошибка загрузки аудио
  (404 файла) — не блокировать: показать текст реплики и перейти в `ready`.
- События в журнал попытки: `mic_check {ok, mime}`, `ptt_start`, `ptt_stop {durationMs}`.

Приёмка: работает в Chrome, Firefox, Яндекс.Браузере на HTTPS; отказ в доступе к
микрофону → состояние `denied` с понятным текстом и кнопкой «Перейти на текстовый ввод».

**FE-03 — панель разговора v2: `features/call-panel/`.** *(1.5 дня, M1 на моках)*

Переписать `CallPanel` под диалог (текстовый режим сценарных реплик оставить как ветку
для `voice.enabled=false`):
- Транскрипт: пузыри `operator` / `caller`, у реплик оператора — маленький бейдж
  уверенности STT, если `< 0.6` («распознано неуверенно»); у заявителя — индикатор
  `emotionalState`; автоскролл; воспроизвести реплику заявителя повторно (кнопка
  «прослушать ещё раз» → `attempts.replay()` + `replay` в журнал, счётчик учитывается).
- Кнопка PTT с состояниями: «Удерживайте Ctrl и говорите» / «Запись… 00:04» /
  «Отправка» / «Заявитель говорит…». Пока `waitingCaller` — «печатает» индикатор
  и (P1) заполняющая реплика из `public/…` или `opening`-кэша.
- Текстовый ввод реплики: виден при `voice.input = text|both` и при `denied` —
  тот же `dialogue.turn({text})`. `Enter` — отправить.
- «Положить трубку» → `dialogue.end()`; `callEnded` → панель read-only, карточка
  остаётся редактируемой (норматив продолжает идти до «Сохранить»).
- Ошибки: `caller_busy` → тост «Заявитель не отвечает, попробуйте ещё раз» и возврат в
  `ready` (реплика сохранена в буфере — повтор той же `turnNo` одной кнопкой);
  `audio_too_long` → «Реплика длиннее 60 с»; сетевая ошибка → повтор с той же `turnNo`
  (сервер идемпотентен); `noSpeech` → «Речь не распознана, повторите» без пузыря.
- `nextTurnNo` из ответа — единственный источник номера следующего хода; при
  перезагрузке страницы — `dialogue.get()` восстанавливает транскрипт и `nextTurnNo`.
- Dev-панель (только `import.meta.env.DEV`): `latencyMs` последнего хода, `fallback`.

Приёмка: сценарий из `docs/tasks/README.md` §DoD п.4 и п.7 проходит на моках;
после F5 в середине разговора транскрипт и номер хода восстановлены.

**FE-04 — входящий вызов и pre-flight микрофона.** *(0.5 дня, M1)*

- `IncomingCallPage`: при `voice.enabled && input !== 'text'` перед «Принять» — блок
  «Проверка гарнитуры»: запрос доступа, индикатор уровня, «скажите что-нибудь» →
  зелёная галка; «Проверить звук» — играет короткий wav. Событие `mic_check`.
  Если доступа нет — предупреждение и выбор «продолжить текстом».
- Рингтон (`public/audio/ring.ogg`, локальный) с момента открытия страницы до «Принять».
- «Принять» → `acceptCall()` → сразу `play(opening.audio.audioUrl)` (жест есть,
  autoplay разрешён) → переход в АРМ с уже играющей репликой (состояние `playingCaller`
  передаётся через store, не через query).

**FE-05 — результат и отчёт: слой «Разговор».** *(1 день, M2)*

`features/evaluation/DialogueLayer.tsx`, используется в `ResultPage` (студент) и
`AttemptReportPage` (преподаватель):
- Плитка `dialogueScore` рядом с остальными (`ScoreTile`), вес из `evaluation.weights`
  — не хардкодить; при `voice.enabled=false` плитка не показывается.
- Чек-лист протокола: пункт → статус (✓ выполнено / ◐ частично / ✗ пропущено / —),
  комментарий; клик по пункту подсвечивает `evidenceTurnNo` в транскрипте (скролл + подсветка).
- Метрики речи: слов/мин, слова-паразиты (с раскрытием каких), средняя пауза перед
  ответом, реплик; «нормы» для подсветки — из `settings` (пока константы в
  `shared/utils/score.ts` с TODO(backend)).
- Тон: три полоски `politeness / calmness / clarity` + комментарий.
- `missingQuestions`, `forbiddenHits` — списком; `summaryForStudent` — как есть.
- Транскрипт целиком (`dialogue.get()`), у реплик заявителя — кнопка воспроизведения,
  у оператора — уверенность STT. Преподаватель может добавить `feedback` с `field =
  "dialogue.turn.<n>"` прямо у реплики.
- `layers`: пока `dialogue: queued|running` — «Оценка разговора выполняется…» вместо
  плитки; `failed` → «AI-слой недоступен, требуется ревью преподавателя».
- `needsReview` — бейдж уже есть; добавить причину, если `dialogue.confidence < 0.7`.

**FE-06 — редактор сценария: бриф заявителя и чек-лист.** *(1 день, M2)*

`ScenarioDetailPage` → вкладки «Легенда» / «Заявитель (бриф)» / «Эталон карточки» /
«Чек-лист разговора» / «Реплики».
- Бриф: `persona`, `speakingStyle` (textarea), таблица фактов `id | текст | reveal
  (select) | hints (chips)` с добавлением/удалением, `unknowns`, `endConditions`, `maxTurns`.
  Валидация на клиенте: id уникальны и латиницей, ≥ 1 факт `volunteer`.
- Чек-лист: таблица `id | текст | kind | required | weight | hints`; шаблоны по
  категориям — кнопка «Добавить типовые пункты» (5 базовых: адрес, что случилось,
  пострадавшие, телефон, «помощь направлена» — из фикстуры, потом с сервера).
- Реплики: `turns[]` + кнопка «Прослушать» → `scenarios.ttsPreview(text)` (P1; на
  моках — mock-audio).
- Генерация: `scenarios.generate()` → 202 `{jobId}` → опрос `aiJobs.get(jobId)` каждые
  3 с с прогресс-строкой «в очереди N / выполняется / готово» → редирект в сценарий.
  Ошибка/`failed` → показать `error`, кнопка «Повторить».
- `generationMeta` (модель, версия промпта) — мелким текстом в шапке для QA модели.
- Approve заблокирован с подсказкой, если голосовой сценарий без брифа/чек-листа (422 с сервера — тоже обработать).

**FE-07 — настройки занятия и веса.** *(0.5 дня, M3)*

- `LessonListPage` форма создания: переключатель «Голосовой режим», `input`
  (`voice/text/both`), `pushToTalk`, `maxTurns`; веса слоёв с ползунками, сумма = 1
  показывается и подсвечивается; при выключенном голосе `dialogue` = 0 и disabled.
  Дефолты — из `lessons.defaultSettings()`.
- Карточка занятия у студента: бейдж «Голосовое», подсказка про гарнитуру.
- Предупреждение при выборе сценария без `ttsReady` в голосовое занятие.

**FE-08 — живой мониторинг по WS.** *(1 день, M3)*

- `shared/ws/useLessonMonitor.ts`: подключение к `/ws/lessons/{id}/monitor?since=`,
  экспоненциальный reconnect (1 → 2 → 4 → … 15 с), `since` = последний `seq`,
  `snapshot` при (пере)подключении заменяет state целиком; fallback на текущий polling
  (3 с) если WS недоступен 30 с.
- `LessonDetailPage`: по участнику — статус, текущее действие (из последних
  `attemptEvent`: «слушает заявителя», «говорит», «заполняет адрес», «оповещает
  службы»), число ходов, последняя реплика (одна строка), время в попытке; клик →
  боковая панель с live-транскриптом и черновиком карточки (`getDraft` для
  преподавателя — уже в спеке).
- Бейдж здоровья AI (`aiHealth`): «AI: ок / очередь диалогов N / недоступен».
- `evaluationUpdated` → обновить строку без перезагрузки.
- Студент: `useAttemptChannel` на `/ws/attempts/{id}` — `evaluationUpdated` →
  результат обновляется сам (сейчас polling), `timerExpired`, `lessonFinished`.

## 3. Инфраструктура, админ, полировка

**FE-09 — HTTPS в dev и матрица браузеров.** *(0.25 дня, M1 — нужно для микрофона!)*
- `vite.config.ts`: `@vitejs/plugin-basic-ssl` (или mkcert-сертификат из `certs/`);
  `server.https`, прокси `/api` и `/api/v1/ws` на go-core. README фронта: как принять
  self-signed в Chrome/Firefox/Яндекс, как разрешить микрофон, чек-лист «нет звука».
- `getUserMedia` недоступен на http://IP — это главный источник «у меня не работает».

**FE-10 — панель администратора: AI и настройки.** *(0.5 дня, M4)*
- `AdminDashboard`: карточки из `admin.health()` — Ollama/LT/STT/TTS, модели по
  профилям, очередь (pending по типам, `dialogWaiting`, `dialogAvgMs`), breaker,
  `jobs` по статусам, последний бэкап. Обновление раз в 5 с.
- `admin.settings`: таблица key/value с JSON-редактором и сохранением (`PUT`).
- Пользователи: создание и правка (`users.create/update`) — сейчас только блокировка.

**FE-11 — полировка под демо.** *(0.5 дня, M4)*
- Адаптив планшета (мобильная в локальной сети — ТЗ): панель разговора и карточка
  в две вкладки на ширине < 1024.
- Все тексты по-русски, единая типографика ошибок, фокус-ловушки в модалках.
- `npm run build` без warning'ов, `oxlint` чистый; размер бандла — без лишних зависимостей
  (аудио — нативные API, никаких wavesurfer'ов).

**FE-13 — тесты.** *(в ходе задач)*
- `vitest`: автомат `useRecorder` (таблица переходов, запрет записи в `playingCaller`),
  редьюсер транскрипта (восстановление после F5, дедуп по `turnNo`), клиент API
  (маппинг ошибок `ApiError`), WS reconnect с `since`.
- Playwright smoke (P1): логин → занятие → входящий → текстовый ход → сохранить → результат — на моках, в CI.

## 4. Порядок

```
M0 22.09: FE-12 (моки голоса) → FE-01 каркас клиента → FE-09 (HTTPS)
M1 24.09: FE-02 (запись) → FE-03 (панель) → FE-04 (входящий, pre-flight)
M2 26.09: FE-01 добить на живом go-core → FE-05 (результат/отчёт) → FE-06 (редактор)
M3 28.09: FE-07 (настройки) → FE-08 (WS-мониторинг)
M4 29.09: FE-10 (админ) → FE-11 (полировка) → демо-прогон с QA
```

Если не успеваем, режем: FE-10 → FE-08 (остаётся polling) → вкладка «Реплики» с
предпрослушиванием → dev-панель задержек. **Не режем**: FE-02/03/04/05, текстовый fallback.

## 5. Definition of Done

- Сценарий DoD из `docs/tasks/README.md` проходит на моках и на живом ядре в трёх браузерах.
- Отказ микрофона, `caller_busy`, обрыв сети посреди хода, F5 — не ломают попытку.
- Студент не получает и не хранит эталон, бриф, факты, чек-лист (проверка по Network и по state).
- `make generate-ts` не даёт diff (типы синхронны со спекой), `npm run build` зелёный.
