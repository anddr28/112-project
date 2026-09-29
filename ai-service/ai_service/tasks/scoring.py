"""ВСЕ формулы баллов ai-service — с версиями (Engine.rules_version).

Принцип проекта: LLM отвечает на вопросы «есть/частично/нет» по каждому факту/пункту,
а балл считает этот модуль детерминированно. Одинаковые суждения — одинаковый балл;
смена формулы — новая версия (старые оценки воспроизводимы по engine.rules_version).
Веса слоёв и итог считает только go-core.
"""

from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass

GRAMMAR_RULES = "gr-v1"
SEMANTIC_RULES = "sem-v1"
DIALOGUE_RULES = "dlg-v1"


def clamp(v: float, lo: float = 0.0, hi: float = 100.0) -> float:
    return max(lo, min(hi, v))


def round1(v: float) -> float:
    return round(v, 1)


# ------------------------------------------------------------------ грамматика (слой 2)

def grammar_v1(words: int, errors: int, warnings: int, style: int, strict: bool = False) -> float:
    """score = 100 − (12·E + 5·W + (2·S если strict)) · 100 / max(words, 30).

    Нормировка на max(words, 30): в короткой карточке одна ошибка не должна обнулять слой,
    а в длинной — ошибки весят пропорционально объёму текста.
    """
    penalty = 12 * errors + 5 * warnings + (2 * style if strict else 0)
    return round1(clamp(100 - penalty * 100 / max(words, 30)))


# ------------------------------------------------------------------ семантика (слой 3)

FACT_WEIGHT = {"present": 1.0, "partial": 0.5, "missing": 0.0}
COHERENCE_PENALTY = {"ok": 0, "weak": 10, "contradiction": 30}


def semantic_v1(statuses: Iterable[str], forbidden_found: int, coherence: str) -> float:
    """score = 100·Σ(present=1, partial=0.5)/N − 15·|forbidden| − (10 weak | 30 contradiction).

    Без обязательных фактов (N=0) база — 100: оценивать нечего, штрафы — за домыслы и связность.
    """
    st = list(statuses)
    base = 100.0 if not st else 100 * sum(FACT_WEIGHT.get(s, 0.0) for s in st) / len(st)
    return round1(clamp(base - 15 * forbidden_found - COHERENCE_PENALTY.get(coherence, 0)))


def semantic_confidence(self_confidence: float, evidence_share: float, answer_words: int) -> float:
    """0.5·самооценка модели + 0.5·доля суждений с цитатой-доказательством; ответ короче
    5 слов — не выше 0.5 (оценивать почти нечего — пусть смотрит преподаватель)."""
    c = 0.5 * clamp(self_confidence, 0, 1) + 0.5 * clamp(evidence_share, 0, 1)
    if answer_words < 5:
        c = min(c, 0.5)
    return round(clamp(c, 0, 1), 2)


# ------------------------------------------------------------------ разговор (слой 5)

CHECK_WEIGHT = {"done": 1.0, "partial": 0.5, "missed": 0.0}


@dataclass(frozen=True)
class ChecklistJudgement:
    status: str  # done | partial | missed | not_applicable
    required: bool
    weight: float = 1.0


def dialogue_v1(items: Iterable[ChecklistJudgement], forbidden_hits: int, fillers_per_100: float,
                avg_response_ms: int | None) -> float:
    """base = 100·Σ w·(done=1, partial=0.5, missed=0)/Σ w по обязательным пунктам
    (not_applicable исключаются); необязательные — только бонус до +5;
    −10 за каждую запрещённую фразу (не больше 30); −5 при > 5 паразитов на 100 слов;
    −5 при средней паузе ответа > 4 с."""
    items = list(items)
    req = [i for i in items if i.required and i.status != "not_applicable"]
    opt = [i for i in items if not i.required and i.status != "not_applicable"]
    wsum = sum(max(i.weight, 0) for i in req)
    if wsum > 0:
        base = 100 * sum(max(i.weight, 0) * CHECK_WEIGHT.get(i.status, 0) for i in req) / wsum
    else:
        base = 100.0
    if opt:
        owsum = sum(max(i.weight, 0) for i in opt) or 1
        base += 5 * sum(max(i.weight, 0) * CHECK_WEIGHT.get(i.status, 0) for i in opt) / owsum
    base -= min(10 * forbidden_hits, 30)
    if fillers_per_100 > 5:
        base -= 5
    if avg_response_ms is not None and avg_response_ms > 4000:
        base -= 5
    return round1(clamp(base))


def dialogue_confidence(self_confidence: float, low_conf_turns: int, operator_turns: int) -> float:
    """Самооценка модели; −0.2, если больше 30 % реплик оператора распознаны неуверенно
    (содержание могло исказиться при STT — оценку стоит перепроверить)."""
    c = clamp(self_confidence, 0, 1)
    if operator_turns > 0 and low_conf_turns / operator_turns > 0.3:
        c -= 0.2
    return round(clamp(c, 0, 1), 2)
