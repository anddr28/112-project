"""Синхронные голосовые эндпоинты: /v1/stt/transcribe, /v1/dialog/turn, /v1/tts/sync.

Аудио оператора не сохраняется: читается в память, декодируется и выбрасывается.
"""

from __future__ import annotations

import time

from fastapi import APIRouter, Request
from fastapi.responses import JSONResponse
from starlette.datastructures import UploadFile

from ai_service.api.common import check_token, ctx_of, parse_model, read_body
from ai_service.core.errors import HttpError
from ai_service.gen import api_models as am
from ai_service.tasks.dialog_turn import run_dialog_turn
from ai_service.tasks.voice import transcribe, tts_sync

router = APIRouter()

MAX_JSON_PART = 4 * 1024 * 1024


async def _multipart(request: Request) -> tuple[dict[str, str], bytes | None, str | None]:
    """Части формы: JSON-поля строками + аудио (байты, Content-Type). Порядок частей любой."""
    ctx = ctx_of(request)
    limit = ctx.settings.stt_max_bytes + 2 * MAX_JSON_PART + 64 * 1024
    cl = request.headers.get("content-length")
    if cl and cl.isdigit() and int(cl) > limit:
        raise HttpError(413, "payload_too_large", "Тело запроса слишком большое (аудио до 2 МБ)")
    try:
        form = await request.form(max_files=2, max_fields=10, max_part_size=MAX_JSON_PART)
    except HttpError:
        raise
    except Exception as e:
        raise HttpError(400, "bad_payload", f"Ожидается multipart/form-data: {e}") from e
    fields: dict[str, str] = {}
    audio: bytes | None = None
    audio_type: str | None = None
    try:
        for name, value in form.multi_items():
            if isinstance(value, UploadFile):
                data = await value.read(ctx.settings.stt_max_bytes + 1)
                if name == "audio":
                    audio, audio_type = data, value.content_type
                elif name in ("request", "options"):
                    fields[name] = data.decode("utf-8", errors="replace")
            else:
                fields[name] = value
    finally:
        await form.close()
    if audio is not None and len(audio) > ctx.settings.stt_max_bytes:
        raise HttpError(413, "payload_too_large", "Аудио больше 2 МБ")
    return fields, audio, audio_type


def _is_multipart(request: Request) -> bool:
    return request.headers.get("content-type", "").lower().startswith("multipart/form-data")


@router.post("/v1/stt/transcribe", summary="Распознавание реплики оператора (синхронно)")
async def stt_transcribe(request: Request) -> JSONResponse:
    check_token(request)
    ctx = ctx_of(request)
    if not _is_multipart(request):
        raise HttpError(415, "unsupported_media", "Ожидается multipart/form-data")
    fields, audio, audio_type = await _multipart(request)
    if audio is None:
        raise HttpError(400, "bad_payload", "Нет части audio")
    opts = parse_model(fields["options"], am.SttOptions, "options") if fields.get("options") else None
    result, engine = await transcribe(ctx, audio, audio_type, opts)
    body = am.SttResponse.model_validate({**result, "engine": engine}).model_dump(mode="json", exclude_none=True)
    return JSONResponse(body)


@router.post("/v1/dialog/turn", summary="Ход диалога: реплика оператора -> ответ заявителя")
async def dialog_turn(request: Request) -> JSONResponse:
    check_token(request)
    ctx = ctx_of(request)
    t0 = time.monotonic()
    audio: bytes | None = None
    audio_type: str | None = None
    if _is_multipart(request):
        fields, audio, audio_type = await _multipart(request)
        if "request" not in fields:
            raise HttpError(400, "bad_payload", "Нет части request")
        req = parse_model(fields["request"], am.DialogTurnRequest, "request")
    else:
        req = parse_model(await read_body(request), am.DialogTurnRequest)
    result = await run_dialog_turn(ctx, req, audio, audio_type)
    body = am.DialogTurnResult.model_validate(result).model_dump(mode="json", exclude_none=True)
    ctx.jobs.dialog_ms.observe((time.monotonic() - t0) * 1000)
    return JSONResponse(body)


@router.post("/v1/tts/sync", summary="Синхронная озвучка (предпрослушивание, служебные фразы)")
async def tts_sync_route(request: Request) -> JSONResponse:
    check_token(request)
    ctx = ctx_of(request)
    req = parse_model(await read_body(request, 1024 * 1024), am.TtsSyncRequest)
    result = await tts_sync(ctx, req)
    return JSONResponse(am.TtsResult.model_validate(result).model_dump(mode="json", exclude_none=True))
