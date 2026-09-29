"""Клиент Ollama: схема в format, починка JSON, коды отказов."""

import json

import httpx
import pytest
from pydantic import BaseModel

from ai_service.config import LlmProfile
from ai_service.core.errors import JobError
from ai_service.engines.llm.ollama import OllamaClient, inline_schema


class Out(BaseModel):
    title: str
    n: int


def client(handler, think=None) -> OllamaClient:
    prof = LlmProfile(model="m:7b", timeout_s=5, think=think)
    http = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    return OllamaClient("http://o", {"eval_fast": prof}, client=http)


def reply(content: str) -> httpx.Response:
    return httpx.Response(200, json={"message": {"content": content}, "prompt_eval_count": 10, "eval_count": 5})


def test_inline_schema_keeps_title_property():
    class Inner(BaseModel):
        x: int

    class M(BaseModel):
        title: str
        items: list[Inner]

    s = inline_schema(M.model_json_schema())
    assert "title" in s["properties"] and "$defs" not in json.dumps(s) and "$ref" not in json.dumps(s)
    assert s["properties"]["items"]["items"]["properties"]["x"]["type"] == "integer"


async def test_schema_sent_and_think_flag():
    seen = {}

    def h(req):
        seen.update(json.loads(req.content))
        return reply('{"title": "a", "n": 1}')

    r = await client(h, think=False).chat("eval_fast", [{"role": "user", "content": "x"}], Out)
    assert r.obj.n == 1 and r.tokens_in == 10 and not r.repaired
    assert seen["format"]["properties"]["n"]["type"] == "integer" and seen["think"] is False
    assert seen["stream"] is False and seen["options"]["seed"] == 42


async def test_repair_then_fail():
    calls = []

    def h(req):
        calls.append(json.loads(req.content))
        return reply('{"title": "a"}')  # нет n

    with pytest.raises(JobError) as e:
        await client(h).chat("eval_fast", [{"role": "user", "content": "x"}], Out)
    assert e.value.code == "llm_invalid_output" and e.value.retryable and len(calls) == 2
    assert e.value.engine["llm_model"] == "m:7b"


async def test_markdown_wrapped_json_accepted():
    r = await client(lambda req: reply('```json\n{"title": "a", "n": 2}\n```')).chat(
        "eval_fast", [{"role": "user", "content": "x"}], Out)
    assert r.obj.n == 2


@pytest.mark.parametrize(("resp", "code"), [
    (httpx.Response(404, json={"error": "model not found"}), "model_unavailable"),
    (httpx.Response(500, text="boom"), "model_unavailable"),
])
async def test_http_errors(resp, code):
    with pytest.raises(JobError) as e:
        await client(lambda req: resp).chat("eval_fast", [{"role": "user", "content": "x"}], Out)
    assert e.value.code == code


async def test_timeout():
    def h(req):
        raise httpx.ReadTimeout("slow", request=req)

    with pytest.raises(JobError) as e:
        await client(h).chat("eval_fast", [{"role": "user", "content": "x"}], Out)
    assert e.value.code == "llm_timeout"
