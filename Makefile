# Кодогенерация из contracts/openapi — источника истины контрактов.
# Требования: go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest
#             pip install "datamodel-code-generator[http]"
#             npx openapi-typescript (ставится на лету), npx @redocly/cli (линт)

OPENAPI := contracts/openapi
SPECS   := $(OPENAPI)/_components.yaml $(OPENAPI)/ai-service.v1.yaml $(OPENAPI)/go-internal.v1.yaml $(OPENAPI)/frontend.v1.yaml

.PHONY: generate generate-go generate-python generate-ts contracts-lint contracts-check

generate: generate-go generate-python generate-ts

generate-go:
	mkdir -p go-core/internal/gen/aiservice go-core/internal/gen/callbacks go-core/internal/gen/public
	oapi-codegen -generate types,client -package aiservice \
		-o go-core/internal/gen/aiservice/client.gen.go $(OPENAPI)/ai-service.v1.yaml
	oapi-codegen -generate types,std-http-server -package callbacks \
		-o go-core/internal/gen/callbacks/server.gen.go $(OPENAPI)/go-internal.v1.yaml
	oapi-codegen -generate types,std-http-server -package public \
		-o go-core/internal/gen/public/server.gen.go $(OPENAPI)/frontend.v1.yaml

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
