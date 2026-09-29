"""Текстовые утилиты для детерминированных проверок (без морфологии: грубые «основы»).

Лемматизатор (pymorphy) был бы точнее, но для пост-фильтров (утечка never-факта, запрещённая
фраза, прощание) хватает нижнего регистра, ё→е и обрезки окончаний: ошибка здесь стоит
одного лишнего повтора LLM, а не неверного балла.
"""

from __future__ import annotations

import re
from collections.abc import Iterable

_WORD = re.compile(r"[0-9a-zа-яё]+(?:-[0-9a-zа-яё]+)*", re.IGNORECASE)

# Служебные слова — не «значимые» для поиска совпадений.
STOP_WORDS = frozenset(
    (
        "и в во на с со к ко о об от до по за из у а но или ли же не ни что как это то так уже еще ещё "
        "бы был была были быть есть нет да он она оно они мы вы я ты его ее её их им ей там тут где когда "
        "который которая которые очень может можно только все всё весь вся при для над под без через"
    ).split()
)

_ENDINGS = sorted(
    ("ами ями ого его ому ему ыми ими ой ей ий ый ая яя ое ее ую юю ых их ом ем ам ям ах ях ов ев "
     "ы и а я о е у ю ь").split(),
    key=len, reverse=True,
)


def norm(text: str) -> str:
    """Нижний регистр, ё→е, пунктуация -> пробел, схлопнутые пробелы."""
    t = text.lower().replace("ё", "е")
    return " ".join(_WORD.findall(t))


def tokens(text: str) -> list[str]:
    return norm(text).split()


def word_count(text: str) -> int:
    return len(_WORD.findall(text))


def stem(word: str) -> str:
    """Грубая основа: отрезать одно окончание, оставив ≥ 4 букв (числа — как есть)."""
    w = word.lower().replace("ё", "е")
    if w.isdigit() or len(w) <= 4:
        return w
    for e in _ENDINGS:
        if w.endswith(e) and len(w) - len(e) >= 4:
            return w[: -len(e)]
    return w


def significant_stems(text: str, min_len: int = 4) -> list[str]:
    out, seen = [], set()
    for w in tokens(text):
        if w in STOP_WORDS or (len(w) < min_len and not w.isdigit()):
            continue
        s = stem(w)
        if s not in seen:
            seen.add(s)
            out.append(s)
    return out


def contains_phrase(text_norm: str, phrase: str) -> bool:
    """Фраза (по основам слов, в том же порядке, подряд) встречается в нормализованном тексте."""
    ps = [stem(w) for w in tokens(phrase)]
    if not ps:
        return False
    ts = text_norm.split()
    n = len(ps)
    return any(all(ts[i + j].startswith(ps[j]) for j in range(n)) for i in range(len(ts) - n + 1))


def contains_any(text_norm: str, phrases: Iterable[str]) -> str | None:
    for p in phrases:
        if contains_phrase(text_norm, p):
            return p
    return None


def truncate_words(text: str, max_words: int) -> str:
    """Обрезать до max_words по границе предложения (если возможно) — TTS не должен
    читать оборванное слово."""
    words = text.split()
    if len(words) <= max_words:
        return text.strip()
    cut = " ".join(words[:max_words])
    m = max(cut.rfind("."), cut.rfind("!"), cut.rfind("?"), cut.rfind("…"))
    if m >= len(cut) // 3:
        return cut[: m + 1].strip()
    return cut.rstrip(",;:— ") + "…"
