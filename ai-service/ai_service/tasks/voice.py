"""Голосовые движки в полосах: TTS (файл в shared volume) и STT (аудио -> SttResult + ITN).

Синтез и распознавание — CPU-bound и блокирующие: выполняются в пуле потоков под слотом
своей полосы (одна модель — один поток), event loop остаётся свободным для HTTP и callback'ов.
"""

from __future__ import annotations

import asyncio
import logging
import time
from typing import TYPE_CHECKING, Any

from ai_service.core.errors import HttpError, JobError
from ai_service.core.queue import CLASS_DIALOG, CLASS_EVALUATE, JobOutcome
from ai_service.engines.stt.base import audio_format, decode_audio
from ai_service.engines.stt.itn_ru import itn
from ai_service.engines.tts.store import cache_path, valid_hash
from ai_service.gen import api_models as am

if TYPE_CHECKING:
    from ai_service.context import AppContext

log = logging.getLogger("ai_service.voice")


class TtsUnavailable(Exception):
    """TTS-движок не загружен (образ без голоса) или упал на тексте."""


async def synthesize_to(ctx: AppContext, rel: str, text: str, voice: str | None, rate: float, *,
                        reuse: bool, cls: int = CLASS_EVALUATE, lane: bool = True) -> tuple[int, str]:
    """Озвучить text в файл rel (относительно TTS_DIR). reuse — файл адресуется хэшем
    содержимого: если он уже есть, не синтезируем повторно. lane=False — вызывающий уже
    держит слот полосы TTS (фоновая задача tts). -> (длительность мс, голос)."""
    if ctx.tts is None:
        raise TtsUnavailable("TTS не загружен (образ собран без голоса: WITH_VOICE=0)")
    v = ctx.tts.voice(voice)
    if reuse and (dur := ctx.store.duration_ms(rel)) is not None:
        return dur, v
    if not lane:
        return await _synthesize(ctx, rel, text, v, rate), v
    async with ctx.jobs.lanes["tts"].slot(cls):
        return await _synthesize(ctx, rel, text, v, rate), v


async def _synthesize(ctx: AppContext, rel: str, text: str, voice: str, rate: float) -> int:
    try:
        pcm = await asyncio.to_thread(ctx.tts.synthesize, text, voice, rate)
        return await asyncio.to_thread(ctx.store.write, rel, pcm, ctx.tts.sample_rate)
    except Exception as e:
        log.exception("TTS упал на тексте")
        raise TtsUnavailable(f"{type(e).__name__}: {e}") from e


def _rate(r: float | None) -> float:
    return r if r and r > 0 else 1.0


async def run_tts_job(ctx: AppContext, req: am.TtsJobRequest) -> JobOutcome:
    """POST /v1/jobs/tts — файл cache/<hash[:2]>/<hash>.wav, go-core пишет строку tts_cache."""
    rate = _rate(req.rate)
    rel = cache_path(req.text_hash)
    engine: dict[str, Any] = {"tts_version": ctx.tts.version if ctx.tts else "none"}
    try:
        dur, voice = await synthesize_to(ctx, rel, req.text, req.voice, rate, reuse=True, lane=False)
    except TtsUnavailable as e:
        err = JobError("tts_failed", str(e))
        err.engine = engine
        raise err from e
    return JobOutcome(value={"text_hash": req.text_hash, "file_path": rel, "duration_ms": dur, "voice": voice,
                             "rate": rate}, engine=engine)


async def tts_sync(ctx: AppContext, req: am.TtsSyncRequest) -> dict[str, Any]:
    """POST /v1/tts/sync — тот же код, что у задачи; недоступность — 503 (контракт)."""
    if not req.text.strip():
        raise HttpError(400, "bad_payload", "text пуст")
    if not valid_hash(req.text_hash):
        raise HttpError(400, "bad_payload", "text_hash: ожидается 8..128 символов [0-9A-Za-z_-]")
    rate = _rate(req.rate)
    rel = cache_path(req.text_hash)
    try:
        dur, voice = await synthesize_to(ctx, rel, req.text, req.voice, rate, reuse=True, cls=CLASS_DIALOG)
    except TtsUnavailable as e:
        raise HttpError(503, "tts_unavailable", f"Озвучка недоступна: {e}", retry_after=30) from e
    return {"text_hash": req.text_hash, "file_path": rel, "duration_ms": dur, "voice": voice, "rate": rate}


async def transcribe(ctx: AppContext, data: bytes, content_type: str | None,
                     opts: am.SttOptions | None) -> tuple[dict[str, Any], dict[str, Any]]:
    """Аудио реплики -> (SttResult, engine). 413/415/503 — HttpError по контракту."""
    if len(data) > ctx.settings.stt_max_bytes:
        raise HttpError(413, "payload_too_large", f"Аудио больше {ctx.settings.stt_max_bytes // 1024} КБ")
    if not data:
        raise HttpError(400, "bad_payload", "Пустая часть audio")
    audio_format(content_type, data[:64])
    if ctx.stt is None:
        raise HttpError(503, "stt_unavailable", "Распознавание речи недоступно (образ без голоса) — используйте текст",
                        retry_after=30)
    opts = opts or am.SttOptions()
    lang = (opts.lang or "ru").split("-")[0]
    t0 = time.monotonic()
    pcm, dur_ms = await asyncio.to_thread(decode_audio, data, ctx.settings.stt_max_audio_sec)
    async with ctx.jobs.lanes["stt"].slot(CLASS_DIALOG) as waited:
        try:
            out = await asyncio.to_thread(ctx.stt.transcribe, pcm, opts.hints, lang)
        except Exception as e:
            log.exception("STT упал")
            raise HttpError(503, "stt_failed", f"Распознавание не удалось: {type(e).__name__}", retry_after=3) from e
    text = itn(out.text_raw) if (opts.itn is None or opts.itn) and out.text_raw else out.text_raw
    result: dict[str, Any] = {
        "text": text, "text_raw": out.text_raw, "language": out.language, "confidence": out.confidence,
        "audio_duration_ms": dur_ms, "segments": out.segments, "no_speech": out.no_speech,
    }
    engine = {"stt_model": ctx.stt.model_name, "stt_version": ctx.stt.version,
              "duration_ms": int((time.monotonic() - t0) * 1000), "queue_wait_ms": int(waited)}
    return result, engine
