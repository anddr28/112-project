# Кодогенерация из contracts/openapi — источника истины контрактов.
# Требования: go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest
#             pip install "datamodel-code-generator[http]"

OPENAPI := contracts/openapi

.PHONY: generate generate-go generate-python contracts-check

generate: generate-go generate-python

generate-go:
	mkdir -p go-core/internal/gen/aiservice go-core/internal/gen/callbacks
	oapi-codegen -generate types,client -package aiservice \
		-o go-core/internal/gen/aiservice/client.gen.go $(OPENAPI)/ai-service.v1.yaml
	oapi-codegen -generate types,std-http-server -package callbacks \
		-o go-core/internal/gen/callbacks/server.gen.go $(OPENAPI)/go-internal.v1.yaml

generate-python:
	mkdir -p ai-service/ai_service/gen
	datamodel-codegen --input $(OPENAPI)/ai-service.v1.yaml --input-file-type openapi \
		--output ai-service/ai_service/gen/api_models.py --output-model-type pydantic_v2.BaseModel \
		--use-standard-collections --use-union-operator --target-python-version 3.11
	datamodel-codegen --input $(OPENAPI)/go-internal.v1.yaml --input-file-type openapi \
		--output ai-service/ai_service/gen/callback_models.py --output-model-type pydantic_v2.BaseModel \
		--use-standard-collections --use-union-operator --target-python-version 3.11

contracts-check: generate
	git diff --exit-code -- go-core/internal/gen ai-service/ai_service/gen
