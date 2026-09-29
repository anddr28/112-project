"""Точка входа ai-service: app factory, lifespan (загрузка движков, прогрев), роуты, ошибки."""

from __future__ import annotations

import asyncio
import logging
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager, suppress

import httpx
from fastapi import FastAPI, Request
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse
from starlette.exceptions import HTTPException as StarletteHTTPException

from ai_service.api import health, jobs, voice
from ai_service.api.common import error_response
from ai_service.config import Settings
from ai_service.context import AppContext, build_context, load_voice
from ai_service.core.errors import HttpError
from ai_service.core.logging import log_fields, setup_logging
from ai_service.engines.tts.store import NO_SPEECH_PATH, NO_SPEECH_TEXT
from ai_service.tasks.voice import TtsUnavailable, synthesize_to

log = logging.getLogger("ai_service")


async def warmup(ctx: AppContext) -> None:
    """Прогрев в фоне (сервис уже принимает запросы): веса LLM в память, служебная фраза
    «Алло? Вы меня слышите?» — в кэш. Первая реплика на занятии не ждёт загрузки модели."""
    if ctx.tts is not None:
        try:
            await synthesize_to(ctx, NO_SPEECH_PATH, NO_SPEECH_TEXT, None, 1.0, reuse=True)
        except TtsUnavailable as e:
            log.warning("служебная фраза не озвучена", extra=log_fields(error=str(e)))
    if ctx.settings.warmup:
        model = ctx.settings.profiles.llm["dialog_fast"].model
        try:
            await ctx.llm.warmup("dialog_fast")
            log.info("LLM прогрета", extra=log_fields(model=model))
        except httpx.HTTPError as e:
            log.warning("прогрев LLM не удался (Ollama недоступна или модель не скачана)",
                        extra=log_fields(model=model, error=f"{type(e).__name__}: {e}"))


def create_app(settings: Settings | None = None, http_client: httpx.AsyncClient | None = None,
               with_voice: bool = True) -> FastAPI:
    settings = settings or Settings()

    @asynccontextmanager
    async def lifespan(app: FastAPI) -> AsyncIterator[None]:
        setup_logging(settings.log_level, settings.log_format)
        ctx = build_context(settings, http_client)
        settings.tts_dir.mkdir(parents=True, exist_ok=True)
        if with_voice:
            await asyncio.to_thread(load_voice, ctx)
        app.state.ctx = ctx
        log.info("ai-service запущен", extra=log_fields(
            ollama=settings.ollama_url, languagetool=settings.languagetool_url, callback=settings.callback_endpoint,
            llm_model=settings.profiles.llm["dialog_fast"].model, tts=ctx.tts is not None, stt=ctx.stt is not None))
        warm = asyncio.create_task(warmup(ctx)) if with_voice or settings.warmup else None
        try:
            yield
        finally:
            if warm and not warm.done():
                warm.cancel()
                with suppress(asyncio.CancelledError):
                    await warm
            await ctx.jobs.shutdown(grace_sec=30.0)
            await ctx.jobs.deliverer.drain(timeout=5.0)
            if http_client is None:
                await ctx.llm.aclose()
                await ctx.lt.aclose()
            log.info("ai-service остановлен")

    app = FastAPI(title="LCT AI Service", version="1.1", lifespan=lifespan,
                  docs_url="/docs", redoc_url=None, openapi_url="/openapi.json")
    app.include_router(health.router)
    app.include_router(jobs.router)
    app.include_router(voice.router)

    @app.exception_handler(HttpError)
    async def _http_error(_: Request, e: HttpError) -> JSONResponse:
        return error_response(e)

    @app.exception_handler(RequestValidationError)
    async def _validation(_: Request, e: RequestValidationError) -> JSONResponse:
        return error_response(HttpError(400, "bad_payload", f"Невалидный запрос: {str(e)[:300]}"))

    @app.exception_handler(StarletteHTTPException)
    async def _starlette(_: Request, e: StarletteHTTPException) -> JSONResponse:
        code = {404: "not_found", 405: "method_not_allowed"}.get(e.status_code, "http_error")
        return error_response(HttpError(e.status_code, code, str(e.detail)))

    @app.exception_handler(Exception)
    async def _internal(_: Request, e: Exception) -> JSONResponse:
        log.exception("необработанная ошибка")
        return error_response(HttpError(500, "internal", f"Внутренняя ошибка ai-service: {type(e).__name__}"))

    return app


app = create_app()
