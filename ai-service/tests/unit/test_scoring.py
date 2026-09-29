"""Формулы баллов — таблицы кейсов (версии gr-v1 / sem-v1 / dlg-v1)."""

import pytest

from ai_service.tasks import scoring as s
from ai_service.tasks.scoring import ChecklistJudgement as J


@pytest.mark.parametrize(("words", "e", "w", "st", "strict", "want"), [
    (0, 0, 0, 0, False, 100.0),
    (30, 1, 0, 0, False, 60.0),        # 100 − 12·100/30
    (10, 1, 0, 0, False, 60.0),        # короткий текст нормируется на 30 слов
    (60, 1, 1, 0, False, 71.7),        # 100 − 17·100/60
    (60, 0, 0, 3, False, 100.0),       # style не считается без strict
    (60, 0, 0, 3, True, 90.0),
    (30, 5, 0, 0, False, 0.0),         # clamp
])
def test_grammar_v1(words, e, w, st, strict, want):
    assert s.grammar_v1(words, e, w, st, strict) == want


@pytest.mark.parametrize(("statuses", "forb", "coh", "want"), [
    (["present", "present"], 0, "ok", 100.0),
    (["present", "partial", "missing"], 0, "ok", 50.0),
    (["present", "partial", "missing"], 1, "ok", 35.0),
    (["present"], 0, "weak", 90.0),
    (["present"], 0, "contradiction", 70.0),
    ([], 0, "ok", 100.0),
    (["missing"], 2, "contradiction", 0.0),
])
def test_semantic_v1(statuses, forb, coh, want):
    assert s.semantic_v1(statuses, forb, coh) == want


def test_semantic_confidence():
    assert s.semantic_confidence(0.9, 1.0, 20) == 0.95
    assert s.semantic_confidence(1.0, 1.0, 3) == 0.5   # короткий ответ — не выше 0.5
    assert s.semantic_confidence(0.4, 0.5, 20) == 0.45


def test_dialogue_v1():
    items = [J("done", True, 2), J("partial", True, 1), J("missed", True, 1), J("done", False, 1),
             J("not_applicable", True, 5)]
    # (2·1 + 1·0.5 + 0)/4 = 62.5 + бонус необязательного 5 = 67.5
    assert s.dialogue_v1(items, 0, 0, 1000) == 67.5
    assert s.dialogue_v1(items, 1, 0, 1000) == 57.5
    assert s.dialogue_v1(items, 5, 0, 1000) == 37.5      # штраф за фразы не больше 30
    assert s.dialogue_v1(items, 0, 6, 5000) == 57.5      # паразиты и медленные ответы
    assert s.dialogue_v1([J("done", False)], 0, 0, None) == 100.0
    assert s.dialogue_v1([], 0, 0, None) == 100.0


def test_dialogue_confidence():
    assert s.dialogue_confidence(0.8, 0, 4) == 0.8
    assert s.dialogue_confidence(0.8, 2, 4) == 0.6       # >30 % неуверенного STT
    assert s.dialogue_confidence(0.1, 4, 4) == 0.0
