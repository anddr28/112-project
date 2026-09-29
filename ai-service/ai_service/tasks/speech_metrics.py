"""Детерминированные метрики речи оператора (SpeechMetrics) — без LLM, считаются всегда."""

from __future__ import annotations

from collections import Counter
from collections.abc import Iterable
from itertools import pairwise
from typing import Any

from ai_service.gen import api_models as am
from ai_service.tasks.textutil import tokens, word_count


def count_fillers(texts: Iterable[str], fillers: Iterable[str]) -> Counter[str]:
    """Паразиты по словам и биграммам («как бы», «это самое»); биграмма, засчитанная
    целиком, не считается ещё раз по отдельным словам."""
    single = {f for f in fillers if " " not in f}
    multi = [f.split() for f in fillers if " " in f]
    out: Counter[str] = Counter()
    for text in texts:
        toks = tokens(text)
        used = [False] * len(toks)
        for m in multi:
            n = len(m)
            for i in range(len(toks) - n + 1):
                if not any(used[i : i + n]) and toks[i : i + n] == m:
                    out[" ".join(m)] += 1
                    for j in range(i, i + n):
                        used[j] = True
        for i, t in enumerate(toks):
            if not used[i] and t in single:
                out[t] += 1
    return out


def speech_metrics(transcript: list[am.DialogueTurn], fillers: list[str], floor: float = 0.6) -> dict[str, Any]:
    ops = [t for t in transcript if t.speaker == "operator"]
    words = sum(word_count(t.text) for t in ops)
    talk_ms = sum(t.audio_duration_ms or 0 for t in ops)
    fc = count_fillers((t.text for t in ops), fillers)
    low = sum(1 for t in ops if t.confidence is not None and t.confidence < floor)

    # Время реакции: от конца реплики заявителя (at_ms + длительность озвучки) до начала
    # следующей реплики оператора. Без меток времени (старые попытки) — не считаем.
    pauses: list[int] = []
    ordered = sorted(transcript, key=lambda t: t.turn_no)
    for prev, cur in pairwise(ordered):
        if prev.speaker == "caller" and cur.speaker == "operator" \
                and prev.at_ms is not None and cur.at_ms is not None:
            end = prev.at_ms + (prev.audio_duration_ms or 0)
            pauses.append(max(cur.at_ms - end, 0))

    out: dict[str, Any] = {
        "operator_turns": len(ops),
        "operator_words": words,
        "operator_talk_ms": talk_ms,
        "filler_count": sum(fc.values()),
        "fillers": dict(fc),
        "low_confidence_turns": low,
    }
    if talk_ms > 0:
        out["words_per_min"] = round(words / (talk_ms / 60000), 1)
    if pauses:
        out["avg_response_ms"] = int(sum(pauses) / len(pauses))
        out["max_response_ms"] = max(pauses)
    return out


def fillers_per_100(metrics: dict[str, Any]) -> float:
    w = metrics.get("operator_words") or 0
    return 100 * metrics.get("filler_count", 0) / w if w else 0.0
