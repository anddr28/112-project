# Кодогенерация из contracts/openapi — источника истины контрактов.
# Требования: go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0
#             pip install "datamodel-code-generator[http]"
#             npx openapi-typescript (ставится на лету), npx @redocly/cli (линт)

OPENAPI := contracts/openapi
SPECS   := $(OPENAPI)/_components.yaml $(OPENAPI)/ai-service.v1.yaml $(OPENAPI)/go-internal.v1.yaml $(OPENAPI)/frontend.v1.yaml

.PHONY: generate generate-go generate-python generate-ts contracts-lint contracts-check \
	go-build go-vet go-test go-test-race go-run fakeai-run db-up db-down migrate up up-fake down e2e

generate: generate-go generate-python generate-ts

generate-go:
	# конфиги oapi-codegen лежат рядом с кодом (import-mapping на _components.yaml, skip-prune)
	cd go-core && oapi-codegen -config internal/gen/components.cfg.yaml ../$(OPENAPI)/_components.yaml
	cd go-core && oapi-codegen -config internal/gen/aiservice.cfg.yaml ../$(OPENAPI)/ai-service.v1.yaml
	cd go-core && oapi-codegen -config internal/gen/callbacks.cfg.yaml ../$(OPENAPI)/go-internal.v1.yaml
	cd go-core && oapi-codegen -config internal/gen/public.cfg.yaml ../$(OPENAPI)/frontend.v1.yaml

generate-python:
	mkdir -p ai-service/ai_service/gen
	datamodel-codegen --input $(OPENAPI)/ai-service.v1.yaml --input-file-type openapi \
		--output ai-service/ai_service/gen/api_models.py --output-model-type pydantic_v2.BaseModel \
		--use-standard-collections --use-union-operator --target-python-version 3.11
	datamodel-codegen --input $(OPENAPI)/go-internal.v1.yaml --input-file-type openapi \
		--output ai-service/ai_service/gen/callback_models.py --output-model-type pydantic_v2.BaseModel \
		--use-standard-collections --use-union-operator --target-python-version 3.11

generate-ts:
	mkdir -p frontend/src/shared/api/gen
	npx --yes openapi-typescript $(OPENAPI)/frontend.v1.yaml -o frontend/src/shared/api/gen/frontend.v1.d.ts

contracts-lint:
	npx --yes @redocly/cli lint $(SPECS)

contracts-check: contracts-lint generate
	git diff --exit-code -- go-core/internal/gen ai-service/ai_service/gen frontend/src/shared/api/gen

# ------------------------------------------------------------------ go-core
DB_URL ?= postgres://lct:lct@localhost:55432/lct?sslmode=disable

go-build:
	cd go-core && go build -o bin/gocore ./cmd/gocore && go build -o bin/fakeai ./cmd/fakeai

go-vet:
	cd go-core && go vet ./...

go-test:
	cd go-core && DATABASE_URL="$(DB_URL)" go test ./...

# локальный запуск ядра против PostgreSQL из db-up и имитатора ai-service (fakeai-run)
go-run:
	cd go-core && DATABASE_URL="$(DB_URL)" go run ./cmd/gocore serve

fakeai-run:
	cd go-core && GOCORE_CALLBACK_URL=http://localhost:8080/internal/ai/v1/results go run ./cmd/fakeai

# PostgreSQL для локальной разработки (порт 55432, как в DATABASE_URL по умолчанию)
db-up:
	docker run -d --name lct-pg -e POSTGRES_USER=lct -e POSTGRES_PASSWORD=lct -e POSTGRES_DB=lct \
		-p 55432:5432 postgres:16-alpine -c max_connections=300 || docker start lct-pg

db-down:
	docker rm -f lct-pg

migrate:
	cd go-core && DATABASE_URL="$(DB_URL)" go run ./cmd/gocore migrate up

# весь контур в docker: с имитатором ai-service / с реальным (нужен ai-service/compose.ai.yaml)
up-fake:
	docker compose --profile fake up -d --build

up:
	docker compose -f docker-compose.yml -f ai-service/compose.ai.yaml up -d --build

down:
	docker compose --profile fake down

# сквозная проверка публичного API (нужен запущенный go-core с демо-сидами и ai-service/fakeai)
E2E_BASE ?= https://localhost:8443
e2e:
	python3 go-core/tools/e2e/e2e.py --base $(E2E_BASE)

# тесты с гонками; интеграционные — на свежих БД из шаблона (internal/platform/pgtest, нужен make db-up)
go-test-race:
	cd go-core && go test -race -count=1 -p 4 ./...
