"""POST /v1/jobs/{grammar|semantic|dialogue|generate|tts} — fire-and-forget: 202 + callback."""

from __future__ import annotations

from collections.abc import Awaitable, Callable
from functools import partial
from typing import Any

from fastapi import APIRouter, Request
from fastapi.responses import JSONResponse
from pydantic import BaseModel

from ai_service.api.common import check_token, ctx_of, parse_model, read_body
from ai_service.core.errors import HttpError
from ai_service.engines.tts.store import valid_hash
from ai_service.gen import api_models as am
from ai_service.tasks.dialogue_eval import run_dialogue_eval
from ai_service.tasks.generate import run_generate
from ai_service.tasks.grammar import run_grammar
from ai_service.tasks.semantic import run_semantic
from ai_service.tasks.voice import run_tts_job

router = APIRouter()


def _check_grammar(r: am.GrammarJobRequest) -> None:
    for i, t in enumerate(r.texts):
        if not t.field.strip():
            raise HttpError(400, "bad_payload", f"texts[{i}].field пуст")


def _check_generate(r: am.GenerateJobRequest) -> None:
    if not r.spec.category.code.strip() or not r.spec.category.name.strip():
        raise HttpError(400, "bad_payload", "spec.category: нужны code и name")


def _check_tts(r: am.TtsJobRequest) -> None:
    if not r.text.strip():
        raise HttpError(400, "bad_payload", "text пуст")
    if not valid_hash(r.text_hash):
        raise HttpError(400, "bad_payload", "text_hash: ожидается 8..128 символов [0-9A-Za-z_-]")


# kind -> (тип задачи, модель запроса, доп. проверка, обработчик)
KINDS: dict[str, tuple[str, type[BaseModel], Callable[[Any], None] | None, Callable[..., Awaitable[Any]]]] = {
    "grammar": ("evaluate_grammar", am.GrammarJobRequest, _check_grammar, run_grammar),
    "semantic": ("evaluate_semantic", am.SemanticJobRequest, None, run_semantic),
    "dialogue": ("evaluate_dialogue", am.DialogueJobRequest, None, run_dialogue_eval),
    "generate": ("generate_scenario", am.GenerateJobRequest, _check_generate, run_generate),
    "tts": ("tts", am.TtsJobRequest, _check_tts, run_tts_job),
}


def _make_route(kind: str) -> Callable[[Request], Awaitable[JSONResponse]]:
    job_type, model, check, handler = KINDS[kind]

    async def submit(request: Request) -> JSONResponse:
        check_token(request)
        ctx = ctx_of(request)
        req: Any = parse_model(await read_body(request), model)
        if check:
            check(req)
        attempt = getattr(req, "attempt_id", None)
        accepted = ctx.jobs.submit(job_type, str(req.request_id), str(attempt) if attempt else None,
                                   req.priority or 5, req, partial(handler, ctx))
        return JSONResponse(am.JobAccepted.model_validate(accepted).model_dump(mode="json", exclude_none=True),
                            status_code=202)

    submit.__name__ = f"submit_{kind}_job"
    return submit


for _kind in KINDS:
    router.add_api_route(f"/v1/jobs/{_kind}", _make_route(_kind), methods=["POST"], status_code=202,
                         summary=f"Задача {KINDS[_kind][0]} (202 + callback)")
