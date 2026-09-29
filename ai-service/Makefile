# ai-service: локальные цели (корневой Makefile — кодогенерация и весь контур).
.PHONY: sync gen run sink test lint smoke image image-novoice models

sync:
	uv sync

# Pydantic-модели из contracts/openapi (та же цель, что make generate-python в корне)
gen:
	$(MAKE) -C .. generate-python

# Локальный запуск против Ollama/LanguageTool на хосте; callback'и — в tools/callback_sink.py
run:
	CALLBACK_URL=http://localhost:18081/internal/ai/v1/results OLLAMA_URL=http://localhost:11434 \
	LANGUAGETOOL_URL=http://localhost:8010 TTS_DIR=./data/tts MODELS_DIR=./models LOG_FORMAT=text \
	uv run uvicorn ai_service.main:app --port 8000 --reload

sink:
	uv run python tools/callback_sink.py --port 18081

test:
	uv run pytest -q

lint:
	uv run ruff check .

smoke:
	uv run python tools/smoke.py --base http://localhost:8000 --sink sink

image:
	docker build -t lct112/ai-service:dev --build-arg WITH_VOICE=1 .

image-novoice:
	docker build -t lct112/ai-service:novoice --build-arg WITH_VOICE=0 .

# веса голоса для локального запуска (нужен uv sync --extra voice + torch)
models:
	uv run python tools/models_pull.py ./models
