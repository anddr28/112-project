"""TTS: Silero v4 ru (torch.package, офлайн — без torch.hub).

Модель грузится один раз при старте (lifespan), синтез — в пуле потоков: torch отпускает
GIL, event loop не блокируется. torch импортируется лениво: образ без голоса (WITH_VOICE=0)
этот модуль просто не использует.
"""

from __future__ import annotations

import logging
import threading
from pathlib import Path

from ai_service.engines.tts.textnorm import TextNormalizer, split_sentences

log = logging.getLogger("ai_service.tts")

VOICES = ("aidar", "baya", "kseniya", "xenia", "eugene")


def rate_to_prosody(rate: float) -> str | None:
    """rate контракта (1.0 — норма) -> значение SSML prosody rate Silero."""
    if rate <= 0.75:
        return "x-slow"
    if rate < 0.92:
        return "slow"
    if rate >= 1.3:
        return "x-fast"
    if rate > 1.08:
        return "fast"
    return None


class SileroTts:
    version = "silero-v4-ru"

    def __init__(self, model_path: Path, normalizer: TextNormalizer, sample_rate: int = 24000,
                 default_voice: str = "baya", threads: int = 2):
        import torch

        torch.set_num_threads(max(threads, 1))
        imp = torch.package.PackageImporter(str(model_path))
        self._model = imp.load_pickle("tts_models", "model")
        self._model.to(torch.device("cpu"))
        self._torch = torch
        self._lock = threading.Lock()  # модель не потокобезопасна; полоса TTS и так = 1
        self.normalizer = normalizer
        self.sample_rate = sample_rate
        self.default_voice = default_voice if default_voice in VOICES else "baya"

    def voice(self, voice: str | None) -> str:
        return voice if voice in VOICES else self.default_voice

    def synthesize(self, text: str, voice: str | None = None, rate: float = 1.0) -> bytes:
        """Текст -> PCM 16 бит моно (sample_rate). Блокирующий вызов — звать через to_thread."""
        import numpy as np

        spoken = self.normalizer.normalize(text)
        if not spoken:
            raise ValueError("пустой текст после нормализации")
        speaker = self.voice(voice)
        prosody = rate_to_prosody(rate)
        chunks = []
        pause = np.zeros(int(self.sample_rate * 0.15), dtype=np.float32)
        with self._lock, self._torch.inference_mode():
            for part in split_sentences(spoken):
                if prosody:
                    audio = self._model.apply_tts(
                        ssml_text=f'<speak><prosody rate="{prosody}">{_xml(part)}</prosody></speak>',
                        speaker=speaker, sample_rate=self.sample_rate)
                else:
                    audio = self._model.apply_tts(text=part, speaker=speaker, sample_rate=self.sample_rate,
                                                  put_accent=True, put_yo=True)
                chunks += [audio.numpy().astype(np.float32), pause]
        wav = np.concatenate(chunks[:-1]) if chunks else np.zeros(0, dtype=np.float32)
        return (np.clip(wav, -1.0, 1.0) * 32767).astype("<i2").tobytes()


def _xml(s: str) -> str:
    return s.replace("&", " и ").replace("<", " ").replace(">", " ")
