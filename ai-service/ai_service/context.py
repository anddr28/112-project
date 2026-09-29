"""Контекст приложения: настройки, движки, реестр промптов, очередь. Создаётся в lifespan."""

from __future__ import annotations

import asyncio
import logging
import time
from dataclasses import dataclass, field
from typing import Any

import httpx

from ai_service.config import Settings, load_word_list
from ai_service.core.callback import CallbackDeliverer
from ai_service.core.logging import log_fields
from ai_service.core.queue import JobManager
from ai_service.core.registry import PromptRegistry
from ai_service.engines.grammar.languagetool import LanguageToolClient
from ai_service.engines.llm.ollama import OllamaClient
from ai_service.engines.tts.store import WavStore
from ai_service.engines.tts.textnorm import TextNormalizer

log = logging.getLogger("ai_service")


@dataclass
class HealthState:
    """Кэш проверок внешних движков: /v1/health дёргают часто (docker, breaker, админка)."""

    at: float = 0.0
    ollama: bool = False
    languagetool: bool = False
    models: list[str] = field(default_factory=list)
    loaded: list[str] = field(default_factory=list)
    lock: asyncio.Lock = field(default_factory=asyncio.Lock)


@dataclass
class AppContext:
    settings: Settings
    llm: OllamaClient
    lt: LanguageToolClient
    prompts: PromptRegistry
    jobs: JobManager
    store: WavStore
    normalizer: TextNormalizer
    tts: Any = None  # SileroTts | None — None в образе без голоса
    stt: Any = None  # FasterWhisperStt | None
    fillers: list[str] = field(default_factory=list)
    lt_dictionary: set[str] = field(default_factory=set)
    lt_disabled_rules: list[str] = field(default_factory=list)
    health: HealthState = field(default_factory=HealthState)
    voice_error: str | None = None

    async def probe(self, max_age: float = 5.0) -> HealthState:
        h = self.health
        if time.monotonic() - h.at < max_age:
            return h
        async with h.lock:
            if time.monotonic() - h.at < max_age:
                return h
            ollama_t = asyncio.create_task(self._probe_ollama())
            lt_t = asyncio.create_task(self._probe_lt())
            (h.ollama, h.models, h.loaded), h.languagetool = await ollama_t, await lt_t
            h.at = time.monotonic()
        return h

    async def _probe_ollama(self) -> tuple[bool, list[str], list[str]]:
        try:
            models = await self.llm.tags()
        except (httpx.HTTPError, ValueError):
            return False, [], []
        try:
            loaded = await self.llm.loaded()
        except (httpx.HTTPError, ValueError):
            loaded = []
        return True, models, loaded

    async def _probe_lt(self) -> bool:
        try:
            return await self.lt.ping()
        except httpx.HTTPError:
            return False


def build_context(settings: Settings, http_client: httpx.AsyncClient | None = None) -> AppContext:
    """Сборка без загрузки тяжёлых моделей (их грузит load_voice в lifespan)."""
    profiles = settings.profiles
    deliverer = CallbackDeliverer(settings.callback_endpoint, settings.internal_api_token, settings.callback_backoff,
                                  settings.callback_timeout_sec, client=http_client)
    return AppContext(
        settings=settings,
        llm=OllamaClient(settings.ollama_url, profiles.llm, settings.ollama_keep_alive, client=http_client),
        lt=LanguageToolClient(settings.languagetool_url, client=http_client),
        prompts=PromptRegistry(settings.prompts_dir),
        jobs=JobManager(settings, deliverer),
        store=WavStore(settings.tts_dir),
        normalizer=TextNormalizer(settings.config_dir / "tts_abbrev_ru.yaml"),
        fillers=load_word_list(settings.config_dir / "fillers_ru.txt"),
        lt_dictionary={w.lower().replace("ё", "е")
                       for w in load_word_list(settings.config_dir / "lt_dictionary_ru.txt")},
        lt_disabled_rules=load_word_list(settings.config_dir / "lt_disabled_rules.txt"),
    )


def load_voice(ctx: AppContext) -> None:
    """Загрузка Silero и whisper, если образ собран с голосом (WITH_VOICE=1) и веса на месте.
    Нет пакетов/весов — сервис живёт без голоса: health tts/stt=false, синхронные голосовые
    эндпоинты отвечают 503, go-core переходит на текст."""
    s = ctx.settings
    if s.voice_engines == "off":
        ctx.voice_error = "VOICE_ENGINES=off"
        return
    errors = []
    silero_path = s.models_dir / "silero" / f"{s.profiles.tts.model}.pt"
    if silero_path.exists():
        try:
            from ai_service.engines.tts.silero import SileroTts

            t0 = time.monotonic()
            ctx.tts = SileroTts(silero_path, ctx.normalizer, s.profiles.tts.sample_rate, s.profiles.tts.voice,
                                threads=max(s.cpu_threads // 2, 1))
            log.info("TTS загружен", extra=log_fields(model=str(silero_path), ms=int((time.monotonic() - t0) * 1000)))
        except Exception as e:  # отсутствие torch / битые веса — не повод не стартовать
            errors.append(f"tts: {type(e).__name__}: {e}")
    else:
        errors.append(f"tts: нет весов {silero_path}")
    whisper_dir = s.models_dir / "whisper" / s.profiles.stt.model
    if whisper_dir.exists():
        try:
            from ai_service.engines.stt.faster_whisper import FasterWhisperStt

            t0 = time.monotonic()
            st = s.profiles.stt
            ctx.stt = FasterWhisperStt(whisper_dir, st.model, st.compute_type, st.beam_size, s.cpu_threads)
            log.info("STT загружен", extra=log_fields(model=ctx.stt.model_name, ms=int((time.monotonic() - t0) * 1000)))
        except Exception as e:
            errors.append(f"stt: {type(e).__name__}: {e}")
    else:
        errors.append(f"stt: нет весов {whisper_dir}")
    ctx.voice_error = "; ".join(errors) or None
    if errors:
        log.warning("голосовые движки недоступны — сервис работает в текстовом режиме",
                    extra=log_fields(errors=errors))
