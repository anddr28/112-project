"""Клиент Ollama: /api/chat со структурированным выходом (JSON-схема) + валидация Pydantic.

Почему JSON-схема в ``format``, а не просто ``"json"``: Ollama превращает схему в грамматику
и декодер физически не может выдать поле не того типа или лишний текст вокруг JSON —
для 3b/7b моделей это главный источник «сломанных» ответов. Схема дополнительно
проверяется Pydantic (enum'ы, диапазоны), при ошибке — одна попытка «почини JSON» с тем же
контекстом, затем ``llm_invalid_output``.
"""

from __future__ import annotations

import copy
import json
import logging
import time
from dataclasses import dataclass
from typing import Any, Generic, TypeVar

import httpx
from pydantic import BaseModel, ValidationError

from ai_service.config import LlmProfile
from ai_service.core.errors import JobError
from ai_service.core.logging import log_fields

log = logging.getLogger("ai_service.llm")

T = TypeVar("T", bound=BaseModel)

REPAIR_PROMPT = (
    "Твой предыдущий ответ не прошёл проверку: {error}\n"
    "Верни исправленный ответ: только JSON строго по заданной схеме, без пояснений и без markdown."
)


@dataclass
class LlmResult(Generic[T]):
    obj: T
    model: str
    tokens_in: int
    tokens_out: int
    duration_ms: int
    repaired: bool = False


def inline_schema(schema: dict[str, Any]) -> dict[str, Any]:
    """JSON-схема Pydantic без $ref/$defs и title: грамматика Ollama/llama.cpp надёжнее
    на плоской схеме, а заголовки — лишние токены промпта."""
    defs = schema.get("$defs", {})

    def walk(node: Any) -> Any:
        if isinstance(node, dict):
            if "$ref" in node:
                name = node["$ref"].split("/")[-1]
                return walk(copy.deepcopy(defs[name]))
            # title-аннотацию (строку) выбрасываем, а свойство с именем "title" — нет.
            return {k: walk(v) for k, v in node.items() if k != "$defs" and not (k == "title" and isinstance(v, str))}
        if isinstance(node, list):
            return [walk(v) for v in node]
        return node

    return walk(schema)


def _extract_json(text: str) -> Any:
    """JSON из ответа модели. При структурированном выходе это весь текст; на случай
    format=json у моделей без поддержки схем — вырезаем первый {...} (markdown-обёртки)."""
    text = text.strip()
    try:
        return json.loads(text)
    except json.JSONDecodeError:
        start, end = text.find("{"), text.rfind("}")
        if start >= 0 and end > start:
            return json.loads(text[start : end + 1])
        raise


class OllamaClient:
    def __init__(self, base_url: str, profiles: dict[str, LlmProfile], keep_alive: str = "30m",
                 client: httpx.AsyncClient | None = None):
        self.base_url = base_url.rstrip("/")
        self.profiles = profiles
        self.keep_alive = keep_alive
        # Таймаут задаётся на каждый вызов (профиль); здесь — только подключение.
        self._client = client or httpx.AsyncClient(timeout=httpx.Timeout(60.0, connect=3.0))
        self.last_model: str | None = None

    def profile(self, name: str) -> LlmProfile:
        return self.profiles.get(name) or self.profiles["eval_fast"]

    async def aclose(self) -> None:
        await self._client.aclose()

    async def chat(self, profile_name: str, messages: list[dict[str, str]], out_model: type[T], *,
                   timeout: float | None = None, num_predict: int | None = None,
                   seed: int | None = None) -> LlmResult[T]:
        prof = self.profile(profile_name)
        schema = inline_schema(out_model.model_json_schema())
        deadline = time.monotonic() + (timeout if timeout is not None else prof.timeout_s)
        tokens_in = tokens_out = 0
        t0 = time.monotonic()
        msgs = list(messages)
        last_error = ""
        for attempt in range(2):
            remaining = deadline - time.monotonic()
            if remaining <= 0.5:
                raise self._err("llm_timeout", f"генерация не уложилась в {prof.timeout_s:.0f} с", prof)
            data = await self._call(prof, msgs, schema, remaining, num_predict, seed)
            tokens_in += int(data.get("prompt_eval_count") or 0)
            tokens_out += int(data.get("eval_count") or 0)
            content = (data.get("message") or {}).get("content") or ""
            try:
                obj = out_model.model_validate(_extract_json(content))
            except (json.JSONDecodeError, ValidationError, ValueError) as e:
                last_error = _short_error(e)
                truncated = data.get("done_reason") == "length"
                log.warning("LLM вернула невалидный ответ", extra=log_fields(
                    model=prof.model, attempt=attempt + 1, error=last_error, truncated=truncated,
                    content=content[:300]))
                msgs = [*messages, {"role": "assistant", "content": content[:4000]},
                        {"role": "user", "content": REPAIR_PROMPT.format(
                            error=last_error + (" (ответ оборван — пиши короче)" if truncated else ""))}]
                continue
            return LlmResult(obj=obj, model=prof.model, tokens_in=tokens_in, tokens_out=tokens_out,
                             duration_ms=int((time.monotonic() - t0) * 1000), repaired=attempt > 0)
        raise self._err("llm_invalid_output", f"LLM дважды вернула невалидный JSON: {last_error}", prof,
                        tokens_in=tokens_in, tokens_out=tokens_out)

    async def _call(self, prof: LlmProfile, messages: list[dict[str, str]], schema: dict[str, Any],
                    timeout: float, num_predict: int | None, seed: int | None = None) -> dict[str, Any]:
        body: dict[str, Any] = {
            "model": prof.model,
            "messages": messages,
            "stream": False,
            "format": schema,
            "keep_alive": self.keep_alive,
            "options": {
                "temperature": prof.temperature,
                "num_predict": num_predict or prof.num_predict,
                "num_ctx": prof.num_ctx,
                "seed": prof.seed if seed is None else seed,
            },
        }
        if prof.think is not None:
            body["think"] = prof.think
        try:
            resp = await self._client.post(f"{self.base_url}/api/chat", json=body, timeout=httpx.Timeout(
                timeout, connect=3.0))
        except httpx.TimeoutException as e:
            raise self._err("llm_timeout", f"генерация не уложилась в {timeout:.0f} с", prof) from e
        except httpx.HTTPError as e:
            raise self._err("model_unavailable", f"Ollama недоступна: {type(e).__name__}", prof) from e
        if resp.status_code == 404:
            raise self._err("model_unavailable", f"модель {prof.model} не загружена в Ollama (ollama pull)", prof)
        if resp.status_code >= 400:
            raise self._err("model_unavailable", f"Ollama HTTP {resp.status_code}: {resp.text[:200]}", prof)
        self.last_model = prof.model
        return resp.json()

    @staticmethod
    def _err(code: str, message: str, prof: LlmProfile, **engine: Any) -> JobError:
        e = JobError(code, message)
        e.engine = {"llm_model": prof.model, **engine}
        return e

    # ------------------------------------------------------------------ служебное

    async def tags(self) -> list[str]:
        resp = await self._client.get(f"{self.base_url}/api/tags", timeout=3.0)
        resp.raise_for_status()
        return [m.get("name", "") for m in resp.json().get("models", [])]

    async def loaded(self) -> list[str]:
        resp = await self._client.get(f"{self.base_url}/api/ps", timeout=3.0)
        resp.raise_for_status()
        return [m.get("name", "") for m in resp.json().get("models", [])]

    async def warmup(self, profile_name: str) -> None:
        """Загрузить веса заранее: первая реплика заявителя не должна ждать 30 с загрузки."""
        prof = self.profile(profile_name)
        body: dict[str, Any] = {"model": prof.model, "messages": [], "keep_alive": self.keep_alive, "stream": False}
        resp = await self._client.post(f"{self.base_url}/api/chat", json=body,
                                       timeout=httpx.Timeout(180.0, connect=3.0))
        resp.raise_for_status()


def _short_error(e: Exception) -> str:
    if isinstance(e, ValidationError):
        parts = []
        for err in e.errors()[:5]:
            loc = ".".join(str(x) for x in err.get("loc", ()))
            parts.append(f"{loc}: {err.get('msg')}")
        return "; ".join(parts)
    return str(e)[:300]
