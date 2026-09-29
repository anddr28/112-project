"""GET /v1/health (без токена: docker healthcheck и проба breaker'а) и GET /v1/queue."""

from __future__ import annotations

from fastapi import APIRouter, Request
from fastapi.responses import JSONResponse

from ai_service.api.common import check_token, ctx_of
from ai_service.gen import api_models as am

router = APIRouter()


@router.get("/v1/health", summary="Health-check")
async def health(request: Request) -> JSONResponse:
    ctx = ctx_of(request)
    h = await ctx.probe()
    tts_ok, stt_ok = ctx.tts is not None, ctx.stt is not None
    profiles = {name: p.model for name, p in ctx.settings.profiles.llm.items()}
    profiles["stt_default"] = ctx.stt.model_name if stt_ok else "unavailable"
    profiles["tts_default"] = ctx.tts.version if tts_ok else "unavailable"
    # degraded — не «мёртв»: сервис отвечает, недоступные движки видны по полям,
    # соответствующие задачи вернут failed(retryable) или 503.
    status = "ok" if (h.ollama and h.languagetool and tts_ok and stt_ok) else "degraded"
    body = am.Health.model_validate({
        "status": status, "ollama": h.ollama, "languagetool": h.languagetool, "tts": tts_ok, "stt": stt_ok,
        "models_available": h.models, "profiles": profiles,
    })
    return JSONResponse(body.model_dump(mode="json", exclude_none=True))


@router.get("/v1/queue", summary="Состояние очереди")
async def queue(request: Request) -> JSONResponse:
    check_token(request)
    ctx = ctx_of(request)
    h = await ctx.probe()
    jobs = ctx.jobs
    model = ctx.llm.last_model or ctx.settings.profiles.llm["dialog_fast"].model
    body = am.QueueStatus.model_validate({
        "pending": jobs.pending(),
        "running": jobs.running(),
        "current_model": model,
        "llm_loaded": model in h.loaded,
        "est_wait_sec": jobs.est_wait("evaluate_semantic"),
        "dialog_waiting": jobs.dialog_waiting(),
        "dialog_avg_ms": int(jobs.dialog_ms.value),
        "stt_running": jobs.lanes["stt"].running,
    })
    return JSONResponse(body.model_dump(mode="json", exclude_none=True))
