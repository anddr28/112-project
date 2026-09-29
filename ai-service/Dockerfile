# syntax=docker/dockerfile:1.7
# ai-service: FastAPI + движки. Два варианта образа (build-arg WITH_VOICE):
#   WITH_VOICE=1 (по умолчанию в compose.ai.yaml) — torch CPU + Silero v4 ru + faster-whisper small,
#                веса запекаются в образ (~2.5 ГБ): стенд работает без интернета;
#   WITH_VOICE=0 — лёгкий образ (~250 МБ): оценка/генерация/текстовый диалог; /v1/health
#                отдаёт tts=false, stt=false, голосовые эндпоинты — 503, go-core переходит на текст.
# LLM (Ollama) и LanguageTool — отдельные контейнеры (compose.ai.yaml).

ARG PYTHON_VERSION=3.11

# ---------------------------------------------------------------- зависимости
FROM python:${PYTHON_VERSION}-slim AS deps
ARG WITH_VOICE=0
ARG TORCH_VERSION=2.5.1
ENV PIP_NO_CACHE_DIR=1 PIP_DISABLE_PIP_VERSION_CHECK=1 UV_NO_CACHE=1
COPY --from=ghcr.io/astral-sh/uv:0.10 /uv /usr/local/bin/uv
WORKDIR /build
COPY pyproject.toml uv.lock ./
RUN python -m venv /opt/venv
ENV PATH=/opt/venv/bin:$PATH VIRTUAL_ENV=/opt/venv
# Версии — строго из uv.lock (воспроизводимая сборка); torch — CPU-колесо отдельным индексом,
# иначе pip притащит CUDA-сборку на 2+ ГБ.
RUN if [ "$WITH_VOICE" = "1" ]; then EXTRA="--extra voice"; else EXTRA=""; fi && \
    uv export --frozen --no-dev --no-emit-project --no-hashes $EXTRA -o requirements.txt && \
    uv pip install -r requirements.txt && \
    if [ "$WITH_VOICE" = "1" ]; then \
      uv pip install "torch==${TORCH_VERSION}" --index-url https://download.pytorch.org/whl/cpu; \
    fi

# ---------------------------------------------------------------- веса моделей (только с голосом)
FROM deps AS models
ARG WITH_VOICE=0
COPY tools/models_pull.py /build/models_pull.py
RUN mkdir -p /models && if [ "$WITH_VOICE" = "1" ]; then python /build/models_pull.py /models --verify; fi

# ---------------------------------------------------------------- runtime
FROM python:${PYTHON_VERSION}-slim AS runtime
ARG WITH_VOICE=0
# uid 10001 — как у go-core: общий volume tts_cache пишет ai-service, читает go-core.
RUN useradd --system --uid 10001 --home /app app && mkdir -p /data/tts /models && chown app /data/tts
COPY --from=deps /opt/venv /opt/venv
COPY --from=models /models /models
WORKDIR /app
COPY ai_service ./ai_service
COPY config ./config
COPY prompts ./prompts
COPY tools ./tools
ENV PATH=/opt/venv/bin:$PATH PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 \
    MODELS_DIR=/models TTS_DIR=/data/tts VOICE_BUILD=${WITH_VOICE} \
    HF_HUB_OFFLINE=1 TRANSFORMERS_OFFLINE=1
USER app
EXPOSE 8000
HEALTHCHECK --interval=10s --timeout=4s --start-period=60s --retries=5 \
    CMD python -c "import urllib.request,sys; urllib.request.urlopen('http://127.0.0.1:8000/v1/health', timeout=3); sys.exit(0)"
# --timeout-graceful-shutdown 30: SIGTERM -> текущая задача дорабатывает до 30 с (PY-02).
CMD ["uvicorn", "ai_service.main:app", "--host", "0.0.0.0", "--port", "8000", \
     "--timeout-graceful-shutdown", "30", "--no-access-log"]
