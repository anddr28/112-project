# go-core — ядро тренажёра ДДС (Go)

Модульный монолит на Go: публичный API для SPA (`/api/v1`, WebSocket), оркестрация AI-задач
(ai-service), детерминированная оценка карточек, голосовой разговор с ИИ-заявителем, аудит,
отчёты, бэкапы, TLS контура. Устройство и правила разработки — [`DESIGN.md`](DESIGN.md).
Контракты — [`contracts/openapi/`](../contracts/openapi) (источник истины), решения по БД —
[`docs/db-design.md`](../docs/db-design.md).

## Быстрый старт

### Весь контур в Docker (без Python — с имитатором ai-service)

```bash
cp .env.example .env
docker compose --profile fake up -d --build
```

Открыть `https://localhost:8443` (сертификат контура самоподписанный: принять в браузере или
установить CA — `docker compose cp go-core:/data/tls/ca.crt .`). Демо-учётки:
`teacher/teacher`, `student/student`, `student2/student2`, `admin/admin`
(только при `GOCORE_DEMO_MODE=true`).

С реальным ai-service (Ollama, LanguageTool, Silero — `ai-service/compose.ai.yaml`):

```bash
docker compose -f docker-compose.yml -f ai-service/compose.ai.yaml up -d --build
```

### Локальная разработка

```bash
make db-up          # PostgreSQL 16 в docker на :55432
make fakeai-run     # имитатор ai-service на :8000 (отдельный терминал)
make go-run         # go-core: миграции + сиды + API на https://localhost:8443 (:8080 — только callback/health/metrics)
```

Фронт с реальным API: в `frontend/.env` — `VITE_USE_MOCKS=false`, `npm run dev` (vite проксирует
`/api` и WebSocket на `https://localhost:8443`).

## Команды

| Команда | Что делает |
|---|---|
| `gocore serve` | сервер (по умолчанию); при `GOCORE_AUTO_MIGRATE=true` применяет миграции, при `GOCORE_SEED=true` — сиды |
| `gocore migrate up\|down\|status` | миграции goose из `GOCORE_MIGRATIONS_DIR` |
| `gocore seed [--demo]` | справочники (классификатор, службы) и демо-данные — идемпотентно |
| `gocore gencert` | CA контура + серверный сертификат в `GOCORE_TLS_DIR` |
| `gocore import-classifier <file.xlsx>` | импорт классификатора происшествий организаторов |
| `gocore backup` | резервная копия БД сейчас (`pg_dump -Fc`) |
| `gocore hash-password` | argon2id-хэш пароля из stdin (не аргументом — он остался бы в истории shell и в `ps`): `docker compose exec go-core gocore hash-password` |

## Переменные окружения

| Переменная | По умолчанию | Назначение |
|---|---|---|
| `DATABASE_URL` | `postgres://lct:lct@localhost:55432/lct?sslmode=disable` | PostgreSQL 12+ (стенд — 16) |
| `GOCORE_DB_MAX_CONNS` | `24` | размер пула pgx |
| `GOCORE_HTTP_ADDR` | `:8080` | HTTP внутри docker-сети: callback ai-service, `/healthz`, `/readyz`, `/metrics` |
| `GOCORE_HTTP_API` | `false` | отдавать API и SPA ещё и по открытому HTTP — только за reverse-proxy с TLS (иначе пароль и cookie идут открытым текстом) |
| `GOCORE_HTTPS_ADDR` | `:8443` | HTTPS для браузеров (SPA + API + WS); пусто — выключен |
| `GOCORE_TLS_CERT` / `GOCORE_TLS_KEY` | — | свой сертификат; иначе создаётся CA контура в `GOCORE_TLS_DIR` |
| `GOCORE_TLS_DIR` | `./data/tls` | CA и серверный сертификат |
| `GOCORE_TLS_HOSTS` | `localhost,127.0.0.1,go-core` | SAN сертификата (+ IP интерфейсов автоматически) |
| `AI_SERVICE_URL` | `http://localhost:8000` | ai-service (или `cmd/fakeai`) |
| `INTERNAL_API_TOKEN` | `dev-internal-token` | `X-Internal-Token` в обе стороны — **сменить на стенде** |
| `GOCORE_AI_JOB_TIMEOUT` | `5s` | таймаут постановки AI-задачи (ответ 202) |
| `GOCORE_AI_SYNC_TIMEOUT` | `20s` | таймаут синхронного хода диалога (контракт) |
| `TTS_DIR` | `./data/tts_cache` | общий с ai-service volume озвучки (go-core раздаёт `/api/v1/media/tts/…`) |
| `GOCORE_STATIC_DIR` | — | собранный SPA (`frontend/dist`); в образе — `/app/web` |
| `GOCORE_BACKUP_DIR` | `./data/backups` | резервные копии |
| `GOCORE_PG_DUMP` | `pg_dump` | путь к pg_dump (мажорная версия = серверу) |
| `GOCORE_MIGRATIONS_DIR` | `../db/migrations` | миграции goose |
| `GOCORE_AUTO_MIGRATE` | `true` | миграции при старте |
| `GOCORE_SEED` | `true` | сиды справочников при старте |
| `GOCORE_DEMO_MODE` | `true` | демо-учётки и демо-занятия; в боевом контуре — `false` |
| `GOCORE_COOKIE_SECURE` | `true` | флаг `Secure` у cookie сессии |
| `GOCORE_WS_ORIGINS` | `localhost:*,127.0.0.1:*` | допустимые Origin WebSocket (dev-прокси) |
| `GOCORE_LOG_LEVEL` / `GOCORE_LOG_FORMAT` | `info` / `text` | журналирование (`json` в контейнере) |

Сессии PostgreSQL получают страховочные таймауты: `statement_timeout` 60 с, `lock_timeout` 15 с,
`idle_in_transaction_session_timeout` 60 с (переопределяются параметрами `DATABASE_URL`, например
`?statement_timeout=300000`; `0` — выключить). Обычный запрос API ограничен 60 с (WebSocket — нет),
превышение — 503 с `Retry-After`.

Всё, что администратор меняет во время работы (веса слоёв, порог, норматив, голос, XP, бэкапы,
срок хранения аудита), — таблица `settings`, экран «Настройки» (`/admin/settings`).

## Проверка

```bash
make go-test-race   # юнит- и интеграционные тесты с -race (БД — свежие копии шаблона, нужен make db-up)
make e2e            # сквозной сценарий через публичный API (запущенный go-core + ai-service/fakeai)
go run ./tools/wscheck -lesson <id>   # живой мониторинг занятия по WebSocket
```

## Списки и пагинация

`GET /lessons`, `/scenarios`, `/users`, `/lessons/assigned` отдают страницу: `?limit=` (по умолчанию
100 / 100 / 500 / 50) и `?cursor=` — значение заголовка `X-Next-Cursor` предыдущего ответа; заголовка
нет — страница последняя. Тело — массив, как раньше. Курсор keyset (без OFFSET): стоимость страницы
не растёт с глубиной листания.

## Эксплуатация

- **Здоровье:** `GET /healthz` (жив), `GET /readyz` (БД доступна), `GET /api/v1/admin/health` (весь контур:
  PostgreSQL, ai-service и модели, очередь AI, breaker, WebSocket, сессии).
- **Метрики:** `GET /metrics` (Prometheus): HTTP по маршрутам, пул БД, очередь AI, runtime Go.
- **Бэкапы:** ежедневно в час `settings.backup.hour`, хранится `settings.backup.keep` копий;
  вручную — `POST /api/v1/admin/backups` или `gocore backup`. Восстановление:
  `pg_restore --clean --if-exists -d "$DATABASE_URL" lct-YYYYMMDD-HHMMSS.dump`.
- **Аудит:** `audit_log` партиционирован помесячно; партиции создаются заранее, старше
  `settings.audit_retention_days` (≥180, ТЗ) удаляются целиком.
- **Восстановление после сбоя:** все фоновые задачи AI — в `ai_jobs` (переживают рестарт, зависшие
  переотправляются), события попыток идемпотентны по `clientSeq`, черновик — upsert; клиент
  переподключает WebSocket с `since` и дочитывает пропущенное.
