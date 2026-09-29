"""Запись WAV в shared volume tts_cache (go-core раздаёт его как /api/v1/media/tts/{path})."""

from __future__ import annotations

import contextlib
import hashlib
import os
import re
import tempfile
import wave
from pathlib import Path

_HASH = re.compile(r"^[0-9A-Za-z_-]{8,128}$")

NO_SPEECH_PATH = "common/allo_slyshite.wav"
NO_SPEECH_TEXT = "Алло? Вы меня слышите?"


def valid_hash(h: str) -> bool:
    """text_hash попадает в путь файла — только безопасные символы (иначе выход из каталога)."""
    return bool(_HASH.match(h or ""))


def cache_path(text_hash: str) -> str:
    """cache/<hash[:2]>/<hash>.wav — веер каталогов, чтобы не держать тысячи файлов в одном."""
    return f"cache/{text_hash[:2]}/{text_hash}.wav"


def dialog_path(attempt_id: str, turn_no: int) -> str:
    return f"dialog/{attempt_id}/{turn_no}.wav"


def tts_hash(text: str, voice: str, rate: float) -> str:
    """sha256(text|voice|rate) — формат ключа tts_cache go-core (convert.TTSHash)."""
    return hashlib.sha256(f"{text}|{voice}|{rate:.2f}".encode()).hexdigest()


class WavStore:
    def __init__(self, root: Path):
        self.root = root

    def full(self, rel: str) -> Path:
        p = (self.root / rel).resolve()
        if not str(p).startswith(str(self.root.resolve())):
            raise ValueError(f"путь вне TTS_DIR: {rel}")
        return p

    def duration_ms(self, rel: str) -> int | None:
        """Длительность существующего файла (кэш по хэшу не синтезируем повторно)."""
        p = self.full(rel)
        if not p.exists() or p.stat().st_size <= 44:
            return None
        try:
            with wave.open(str(p), "rb") as w:
                return int(w.getnframes() * 1000 / w.getframerate())
        except (wave.Error, EOFError):
            return None

    def write(self, rel: str, pcm16: bytes, sample_rate: int) -> int:
        """Атомарно (temp + rename): go-core может отдавать файл в ту же секунду.
        Права 0644 — файл читает go-core под своим uid."""
        p = self.full(rel)
        p.parent.mkdir(parents=True, exist_ok=True)
        fd, tmp = tempfile.mkstemp(prefix=".wav-", dir=p.parent)
        try:
            with os.fdopen(fd, "wb") as f, wave.open(f, "wb") as w:
                w.setnchannels(1)
                w.setsampwidth(2)
                w.setframerate(sample_rate)
                w.writeframes(pcm16)
            os.chmod(tmp, 0o644)
            os.replace(tmp, p)
        except BaseException:
            with contextlib.suppress(OSError):
                os.unlink(tmp)
            raise
        return int(len(pcm16) / 2 * 1000 / sample_rate)
