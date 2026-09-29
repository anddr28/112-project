"""Фикстуры: сервис целиком (ASGI) с поддельными Ollama / LanguageTool / go-core на httpx.MockTransport.

Сеть не нужна: все исходящие вызовы сервиса идут через один AsyncClient с MockTransport,
обработчик которого играет роли Ollama (/api/*), LanguageTool (/v2/*) и callback-приёмника
go-core. Ответы LLM подбираются по JSON-схеме запроса (какая задача спрашивает).
"""

from __future__ import annotations

import asyncio
import contextlib
import json
from collections.abc import AsyncIterator, Callable
from pathlib import Path
from typing import Any

import httpx
import pytest

from ai_service.config import Settings
from ai_service.engines.stt.base import SttOutput
from ai_service.main import create_app

ROOT = Path(__file__).resolve().parent.parent
FIXTURES = ROOT / "tests" / "fixtures"
TOKEN = "test-token"
CALLBACK_URL = "http://go-core.test/internal/ai/v1/results"


def load_fixture(name: str) -> dict[str, Any]:
    return json.loads((FIXTURES / name).read_text(encoding="utf-8"))


def schema_kind(fmt: Any) -> str:
    props = set((fmt or {}).get("properties", {}))
    if "reply" in props:
        return "caller"
    if "coherence" in props:
        return "semantic"
    if "tone" in props:
        return "dialogue"
    if "title" in props:
        return "generate"
    return "unknown"


class Backend:
    """Поддельные внешние сервисы + журнал вызовов."""

    def __init__(self) -> None:
        self.callbacks: list[dict[str, Any]] = []
        self.callback_headers: list[dict[str, str]] = []
        self.llm_calls: list[dict[str, Any]] = []
        self.lt_calls: list[dict[str, str]] = []
        self.ollama_up = True
        self.lt_up = True
        self.callback_status = 200
        # kind -> очередь ответов (str — content; Exception — сбой транспорта; callable(body) -> content)
        self.llm_script: dict[str, list[Any]] = {}
        self.llm_default: dict[str, Any] = {}
        self.lt_matches: Callable[[str], list[dict[str, Any]]] = lambda text: []
        self._cb_event = asyncio.Event()

    # ----------------------------------------------------------------- transport
    def handle(self, request: httpx.Request) -> httpx.Response:
        url = request.url
        if url.host == "go-core.test":
            self.callbacks.append(json.loads(request.content))
            self.callback_headers.append(dict(request.headers))
            self._cb_event.set()
            return httpx.Response(self.callback_status, json={"accepted": True, "duplicate": False})
        if url.host == "ollama.test":
            if not self.ollama_up:
                raise httpx.ConnectError("connection refused", request=request)
            if url.path == "/api/tags":
                return httpx.Response(200, json={"models": [{"name": "test-llm:7b"}]})
            if url.path == "/api/ps":
                return httpx.Response(200, json={"models": [{"name": "test-llm:7b"}]})
            if url.path == "/api/chat":
                return self._chat(request)
        if url.host == "lt.test":
            if not self.lt_up:
                raise httpx.ConnectError("connection refused", request=request)
            if url.path == "/v2/languages":
                return httpx.Response(200, json=[{"code": "ru-RU"}])
            if url.path == "/v2/check":
                form = dict(httpx.QueryParams(request.content.decode()))
                self.lt_calls.append(form)
                return httpx.Response(200, json={"software": {"version": "6.5-test"},
                                                 "matches": self.lt_matches(form.get("text", ""))})
        return httpx.Response(404, json={"error": "not found"})

    def _chat(self, request: httpx.Request) -> httpx.Response:
        body = json.loads(request.content)
        self.llm_calls.append(body)
        if not body.get("messages"):  # прогрев
            return httpx.Response(200, json={"message": {"role": "assistant", "content": ""}, "done": True})
        kind = schema_kind(body.get("format"))
        script = self.llm_script.get(kind)
        item = script.pop(0) if script else self.llm_default.get(kind, DEFAULT_LLM.get(kind))
        if isinstance(item, Exception):
            raise item
        if callable(item):
            item = item(body)
        if isinstance(item, httpx.Response):
            return item
        content = item if isinstance(item, str) else json.dumps(item, ensure_ascii=False)
        return httpx.Response(200, json={"model": body["model"], "message": {"role": "assistant", "content": content},
                                         "done": True, "done_reason": "stop", "prompt_eval_count": 300,
                                         "eval_count": 60})

    async def wait_callbacks(self, n: int, timeout: float = 5.0) -> list[dict[str, Any]]:
        loop = asyncio.get_running_loop()
        deadline = loop.time() + timeout
        while len(self.callbacks) < n:
            self._cb_event.clear()
            left = deadline - loop.time()
            if left <= 0:
                raise AssertionError(f"ждали {n} callback'ов, пришло {len(self.callbacks)}")
            with contextlib.suppress(TimeoutError):
                await asyncio.wait_for(self._cb_event.wait(), left)
        return self.callbacks


DEFAULT_LLM: dict[str, Any] = {
    "caller": {"reply": "Третий подъезд, пятый этаж! Дым прямо из-под двери!", "revealed_fact_ids": ["entrance"],
               "is_unknown_answer": False, "should_end": False, "end_reason": "", "emotional_state": "паника"},
    "semantic": {"facts": [{"id": "f1", "status": "present", "evidence": "дым из кв на 5 этаже"},
                           {"id": "f2", "status": "present", "evidence": "на 5 этаже"},
                           {"id": "f3", "status": "partial", "evidence": "внутри вроде дедушка"}],
                 "forbidden_found": [], "coherence": "ok",
                 "per_field": [{"field": "description", "comment": "Суть передана."}],
                 "summary_for_student": "Адрес и суть записаны верно.", "self_confidence": 0.9},
    "dialogue": {"checklist": [{"id": "ask_address", "status": "done", "evidence_turn_no": 2, "comment": "Спросил"},
                               {"id": "ask_people", "status": "missed", "evidence_turn_no": 0, "comment": "Не спросил"},
                               {"id": "ask_phone", "status": "missed", "evidence_turn_no": 0, "comment": ""},
                               {"id": "say_dispatched", "status": "done", "evidence_turn_no": 4, "comment": ""},
                               {"id": "calm", "status": "done", "evidence_turn_no": 0, "comment": ""}],
                 "forbidden_hits": [], "tone": {"politeness": 90, "calmness": 85, "clarity": 80, "comment": "Спокойно"},
                 "summary_for_student": "Адрес уточнён, но не спросили о людях.", "self_confidence": 0.8},
}


class FakeTts:
    version = "fake-tts"
    sample_rate = 16000

    def voice(self, v: str | None) -> str:
        return v or "baya"

    def synthesize(self, text: str, voice: str | None = None, rate: float = 1.0) -> bytes:
        return b"\x00\x00" * (self.sample_rate // 2)  # 0.5 с тишины


class FakeStt:
    model_name = "fake-whisper:small-int8"
    version = "fake-1"

    def __init__(self, text: str = "какой подъезд и этаж", no_speech: bool = False):
        self.text, self.no_speech = text, no_speech

    def transcribe(self, pcm: Any, hints: list[str] | None, lang: str = "ru") -> SttOutput:
        if self.no_speech:
            return SttOutput(text_raw="", confidence=0.0, no_speech=True)
        return SttOutput(text_raw=self.text, confidence=0.9, no_speech=False,
                         segments=[{"start_ms": 0, "end_ms": 1000, "text": self.text, "confidence": 0.9}])


@pytest.fixture
def settings(tmp_path: Path) -> Settings:
    return Settings(
        internal_api_token=TOKEN, callback_url=CALLBACK_URL, ollama_url="http://ollama.test",
        languagetool_url="http://lt.test", llm_model="test-llm:7b", tts_dir=tmp_path / "tts",
        models_dir=tmp_path / "models", callback_retries="0,0,0", warmup=False, voice_engines="off",
        log_format="text", log_level="warning",
    )


@pytest.fixture
def backend() -> Backend:
    return Backend()


class Service:
    def __init__(self, client: httpx.AsyncClient, backend: Backend, app: Any):
        self.client, self.backend, self.app = client, backend, app

    @property
    def ctx(self) -> Any:
        return self.app.state.ctx

    async def post(self, path: str, body: Any = None, token: str | None = TOKEN, **kw: Any) -> httpx.Response:
        headers = {"X-Internal-Token": token} if token else {}
        if body is not None:
            kw["json"] = body
        return await self.client.post(path, headers=headers, **kw)

    async def get(self, path: str, token: str | None = TOKEN) -> httpx.Response:
        return await self.client.get(path, headers={"X-Internal-Token": token} if token else {})


@pytest.fixture
async def service(settings: Settings, backend: Backend) -> AsyncIterator[Service]:
    http = httpx.AsyncClient(transport=httpx.MockTransport(backend.handle))
    app = create_app(settings, http_client=http, with_voice=False)
    transport = httpx.ASGITransport(app=app)
    async with app.router.lifespan_context(app), httpx.AsyncClient(transport=transport, base_url="http://ai") as c:
        yield Service(c, backend, app)
    await http.aclose()
