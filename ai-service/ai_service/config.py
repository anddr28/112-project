"""Настройки ai-service: env (pydantic-settings) + профили моделей из config/models.yaml.

Почему профили в YAML, а не в env: их много и они структурные (модель, температура,
лимиты), а QA/ML меняют их по итогам bench без правки кода. Env оставлен для того, что
различается между стендами (адреса, токен, одна модель на всё — LLM_MODEL).
"""

from __future__ import annotations

from functools import cached_property
from pathlib import Path
from typing import Any

import yaml
from pydantic import BaseModel, Field
from pydantic_settings import BaseSettings, SettingsConfigDict

# Корень сервиса (ai-service/): рядом лежат config/ и prompts/. В образе — /app.
SERVICE_ROOT = Path(__file__).resolve().parent.parent

LLM_PROFILES = ("dialog_fast", "eval_fast", "eval_dialogue", "eval_thorough", "generate")


class LlmProfile(BaseModel):
    """Параметры вызова Ollama для одного профиля."""

    model: str
    temperature: float = 0.0
    num_predict: int = 512
    num_ctx: int = 4096
    timeout_s: float = 60.0
    seed: int = 42
    # think: None — поле не отправляется (модели без режима рассуждений его не понимают);
    # False — явно выключить «thinking» у моделей, где он включён по умолчанию (gemma4, qwen3).
    think: bool | None = None


class SttProfile(BaseModel):
    engine: str = "faster_whisper"
    model: str = "small"
    compute_type: str = "int8"
    beam_size: int = 2


class TtsProfile(BaseModel):
    engine: str = "silero"
    model: str = "v4_ru"
    voice: str = "baya"
    sample_rate: int = 24000


class Profiles(BaseModel):
    llm: dict[str, LlmProfile]
    stt: SttProfile
    tts: TtsProfile


class Settings(BaseSettings):
    # env_ignore_empty: в compose переменные пробрасываются как ${VAR:-} — пустая строка
    # значит «не задано», а не «пустая модель/false».
    model_config = SettingsConfigDict(env_file=None, extra="ignore", case_sensitive=False, env_ignore_empty=True)

    internal_api_token: str = "dev-internal-token"
    go_core_url: str = "http://go-core:8080"
    # Полный URL callback'а; пусто — GO_CORE_URL + /internal/ai/v1/results.
    callback_url: str = ""
    ollama_url: str = "http://ollama:11434"
    languagetool_url: str = "http://languagetool:8010"

    # Перекрывает model у всех LLM-профилей (одна модель в памяти — см. models.yaml).
    llm_model: str = ""
    # true/false — явно включить/выключить thinking у всех LLM-профилей (пусто — как в YAML).
    llm_think: bool | None = None
    ollama_keep_alive: str = "30m"
    # Сколько LLM-вызовов одновременно (семафор полосы LLM). 1 — по контракту; больше —
    # только вместе с OLLAMA_NUM_PARALLEL (батчинг на одной модели), по итогам bench.
    llm_concurrency: int = Field(1, ge=1, le=8)
    lt_concurrency: int = Field(4, ge=1, le=32)

    models_dir: Path = Path("/models")
    tts_dir: Path = Path("/data/tts")
    config_dir: Path = SERVICE_ROOT / "config"
    prompts_dir: Path = SERVICE_ROOT / "prompts"

    stt_engine: str = "faster_whisper"
    stt_max_audio_sec: int = 60
    stt_max_bytes: int = 2 * 1024 * 1024
    cpu_threads: int = 4
    # Голосовые движки: auto — грузить, если установлены пакеты и есть веса; off — не грузить.
    voice_engines: str = "auto"

    # Бюджет синхронного хода: чуть меньше таймаута go-core (settings.ai.dialog_timeout_sec,
    # по контракту 20 с). На медленном стенде поднимать вместе с настройкой go-core.
    dialog_deadline_sec: float = 18.0
    dialog_max_waiting: int = 8
    dialog_history_turns: int = 8
    dialog_retry_after_sec: int = 3

    queue_max_semantic: int = 200
    queue_max_grammar: int = 500
    queue_max_generate: int = 20
    queue_max_tts: int = 200
    queue_max_dialogue: int = 200
    # Сколько помнить выполненные задачи (повторный POST -> повторный callback).
    job_registry_size: int = 2000
    job_done_ttl_sec: int = 3600

    callback_retries: str = "1,5,30"
    callback_timeout_sec: float = 10.0

    warmup: bool = True
    log_level: str = "info"
    log_format: str = "json"

    @cached_property
    def callback_endpoint(self) -> str:
        if self.callback_url:
            return self.callback_url
        return self.go_core_url.rstrip("/") + "/internal/ai/v1/results"

    @cached_property
    def callback_backoff(self) -> list[float]:
        out: list[float] = []
        for part in self.callback_retries.split(","):
            part = part.strip()
            if part:
                out.append(max(float(part), 0.0))
        return out

    def queue_max(self, job_type: str) -> int:
        return {
            "evaluate_semantic": self.queue_max_semantic,
            "evaluate_grammar": self.queue_max_grammar,
            "generate_scenario": self.queue_max_generate,
            "tts": self.queue_max_tts,
            "evaluate_dialogue": self.queue_max_dialogue,
        }.get(job_type, 100)

    @cached_property
    def profiles(self) -> Profiles:
        return load_profiles(self.config_dir / "models.yaml", self.llm_model, self.llm_think)


def load_profiles(path: Path, model_override: str = "", think_override: bool | None = None) -> Profiles:
    raw: dict[str, Any] = yaml.safe_load(path.read_text(encoding="utf-8")) or {}
    items: dict[str, Any] = raw.get("profiles") or {}
    llm: dict[str, LlmProfile] = {}
    for name in LLM_PROFILES:
        spec = dict(items.get(name) or {})
        if model_override:
            spec["model"] = model_override
        if think_override is not None:
            spec["think"] = think_override
        spec.setdefault("model", model_override or "qwen2.5:7b-instruct")
        llm[name] = LlmProfile(**spec)
    return Profiles(
        llm=llm,
        stt=SttProfile(**(items.get("stt_default") or {})),
        tts=TtsProfile(**(items.get("tts_default") or {})),
    )


def load_word_list(path: Path) -> list[str]:
    """Файл-список: по строке на элемент, # — комментарий."""
    if not path.exists():
        return []
    out = []
    for line in path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if line and not line.startswith("#"):
            out.append(line)
    return out
