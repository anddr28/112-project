"""Общее для роутов: токен, разбор JSON-тела по сгенерированной модели, ApiError-ответы."""

from __future__ import annotations

import hmac
import json
from typing import Any, TypeVar

from fastapi import Request
from fastapi.responses import JSONResponse
from pydantic import BaseModel, ValidationError

from ai_service.context import AppContext
from ai_service.core.errors import HttpError

M = TypeVar("M", bound=BaseModel)

MAX_JSON_BODY = 8 * 1024 * 1024  # легенда + эталон + транскрипт с запасом


def ctx_of(request: Request) -> AppContext:
    return request.app.state.ctx


def check_token(request: Request) -> None:
    token = request.app.state.ctx.settings.internal_api_token
    got = request.headers.get("X-Internal-Token", "")
    if not token or not hmac.compare_digest(got.encode(), token.encode()):
        raise HttpError(401, "unauthorized", "Неверный X-Internal-Token")


def error_response(e: HttpError) -> JSONResponse:
    headers = {"Retry-After": str(e.retry_after)} if e.retry_after is not None else None
    return JSONResponse({"code": e.code, "message": e.message}, status_code=e.status, headers=headers)


def parse_model(raw: bytes | str, model: type[M], what: str = "тело запроса") -> M:
    """JSON -> сгенерированная Pydantic-модель контракта. Любое нарушение — 400 bad_payload
    (go-core переводит задачу в failed без ретраев: это ошибка его payload'а)."""
    try:
        data: Any = json.loads(raw)
    except (json.JSONDecodeError, UnicodeDecodeError) as e:
        raise HttpError(400, "bad_payload", f"{what}: невалидный JSON ({e.msg if hasattr(e, 'msg') else e})") from e
    if not isinstance(data, dict):
        raise HttpError(400, "bad_payload", f"{what}: ожидается JSON-объект")
    if "schema_version" in model.model_fields and data.get("schema_version") != "1":
        raise HttpError(400, "bad_payload", 'Неподдерживаемая schema_version: ожидается "1"')
    try:
        return model.model_validate(data)
    except ValidationError as e:
        errs = []
        for err in e.errors()[:5]:
            loc = ".".join(str(x) for x in err.get("loc", ()))
            errs.append(f"{loc}: {err.get('msg')}")
        raise HttpError(400, "bad_payload", f"{what} не соответствует контракту: " + "; ".join(errs)) from e


async def read_body(request: Request, limit: int = MAX_JSON_BODY) -> bytes:
    cl = request.headers.get("content-length")
    if cl and cl.isdigit() and int(cl) > limit:
        raise HttpError(413, "payload_too_large", f"Тело запроса больше {limit // (1024 * 1024)} МБ")
    body = b""
    async for chunk in request.stream():
        body += chunk
        if len(body) > limit:
            raise HttpError(413, "payload_too_large", f"Тело запроса больше {limit // (1024 * 1024)} МБ")
    return body
