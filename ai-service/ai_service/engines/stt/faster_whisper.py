"""STT: faster-whisper (CTranslate2, int8 на CPU). Веса — в MODELS_DIR (запечены в образ)."""

from __future__ import annotations

import math
import threading
from pathlib import Path
from typing import Any

from ai_service.engines.stt.base import SttOutput

NO_SPEECH_PROB = 0.6


class FasterWhisperStt:
    version = "faster-whisper-1"

    def __init__(self, model_dir: Path, model: str = "small", compute_type: str = "int8", beam_size: int = 2,
                 cpu_threads: int = 4):
        from faster_whisper import WhisperModel

        self._model = WhisperModel(str(model_dir), device="cpu", compute_type=compute_type, cpu_threads=cpu_threads)
        self._lock = threading.Lock()
        self.beam_size = beam_size
        self.model_name = f"faster-whisper:{model}-{compute_type}"

    def transcribe(self, pcm: Any, hints: list[str] | None, lang: str = "ru") -> SttOutput:
        # Подсказки из легенды (улица, имя заявителя) — в initial_prompt: whisper охотнее
        # пишет редкие названия так, как они звучат в сценарии.
        prompt = "Служба 112. " + ", ".join(h for h in (hints or [])[:30] if h) if hints else "Служба 112."
        with self._lock:
            segments, _info = self._model.transcribe(
                pcm, language=lang or "ru", beam_size=self.beam_size, vad_filter=True,
                initial_prompt=prompt, condition_on_previous_text=False, no_speech_threshold=NO_SPEECH_PROB,
            )
            segs = list(segments)
        parts, confs, out_segs = [], [], []
        all_silent = True
        for s in segs:
            text = s.text.strip()
            if not text:
                continue
            conf = min(max(math.exp(s.avg_logprob), 0.0), 1.0)
            if s.no_speech_prob <= NO_SPEECH_PROB:
                all_silent = False
            parts.append(text)
            confs.append(conf)
            out_segs.append({"start_ms": int(s.start * 1000), "end_ms": int(s.end * 1000), "text": text,
                             "confidence": round(conf, 3)})
        no_speech = not parts or all_silent
        return SttOutput(
            text_raw="" if no_speech else " ".join(parts),
            confidence=0.0 if no_speech else round(sum(confs) / len(confs), 3),
            no_speech=no_speech,
            segments=[] if no_speech else out_segs,
            language=lang or "ru",
        )
