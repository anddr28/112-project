"""Интерфейс STT и проверка аудио (формат/размер) — общие для всех движков.

Движок сменяемый (STT_ENGINE: faster_whisper сейчас, gigaam/vosk — по итогам bench ML-01):
реализация получает PCM 16 кГц моно float32 и отдаёт SttOutput.
"""

from __future__ import annotations

import io
from dataclasses import dataclass, field
from typing import Any, Protocol

from ai_service.core.errors import HttpError

SAMPLE_RATE = 16000

_CT_FORMAT = {
    "audio/webm": "webm", "video/webm": "webm",
    "audio/ogg": "ogg", "application/ogg": "ogg", "audio/opus": "ogg",
    "audio/wav": "wav", "audio/x-wav": "wav", "audio/wave": "wav", "audio/vnd.wave": "wav",
    "audio/mpeg": "mp3", "audio/mp3": "mp3",
}


@dataclass
class SttOutput:
    text_raw: str
    confidence: float
    no_speech: bool
    segments: list[dict[str, Any]] = field(default_factory=list)
    language: str = "ru"


class SttEngine(Protocol):
    model_name: str  # "faster-whisper:small-int8"
    version: str

    def transcribe(self, pcm: Any, hints: list[str] | None, lang: str) -> SttOutput: ...


def audio_format(content_type: str | None, head: bytes) -> str:
    """Формат по Content-Type части; octet-stream/пустой — по сигнатуре. Неизвестное — 415."""
    ct = (content_type or "").split(";")[0].strip().lower()
    if ct and ct != "application/octet-stream":
        fmt = _CT_FORMAT.get(ct)
        if fmt is None:
            raise HttpError(415, "unsupported_media",
                            "Формат аудио не поддерживается: ожидается webm/opus, ogg/opus или wav")
        return fmt
    if head[:4] == b"RIFF" and head[8:12] == b"WAVE":
        return "wav"
    if head[:4] == b"OggS":
        return "ogg"
    if head[:4] == b"\x1a\x45\xdf\xa3":
        return "webm"
    if head[:3] == b"ID3" or (len(head) > 1 and head[0] == 0xFF and head[1] & 0xE0 == 0xE0):
        return "mp3"
    raise HttpError(415, "unsupported_media", "Не удалось определить формат аудио")


def decode_audio(data: bytes, max_sec: int) -> tuple[Any, int]:
    """Декод любого контейнера (PyAV/ffmpeg) -> float32 16 кГц моно + длительность, мс.
    Битый файл — 415, длиннее лимита — 413."""
    import numpy as np

    try:
        from faster_whisper.audio import decode_audio as fw_decode

        pcm = fw_decode(io.BytesIO(data), sampling_rate=SAMPLE_RATE)
    except Exception as e:
        raise HttpError(415, "unsupported_media", f"Не удалось декодировать аудио: {type(e).__name__}") from e
    pcm = np.asarray(pcm, dtype=np.float32)
    dur_ms = int(len(pcm) * 1000 / SAMPLE_RATE)
    if dur_ms > max_sec * 1000:
        raise HttpError(413, "payload_too_large", f"Реплика длиннее {max_sec} с")
    return pcm, dur_ms
