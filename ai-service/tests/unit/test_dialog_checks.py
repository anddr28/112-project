from ai_service.gen import api_models as am
from ai_service.tasks.dialog_turn import allowed_facts, fallback_line, never_leak, render_history
from ai_service.tasks.textutil import contains_phrase, norm, truncate_words

BRIEF = am.DialogueBrief(persona="соседка", facts=[
    am.DialogueFact(id="smoke", text="дым из квартиры на 5 этаже", reveal="volunteer"),
    am.DialogueFact(id="entrance", text="подъезд 3", reveal="on_request", hints=["подъезд"]),
    am.DialogueFact(id="secret", text="сосед часто курит в постели", reveal="never"),
])
CS = am.CallScript(caller=am.Caller(name="Мария"), address=am.Address(raw="Ленина 14"), dialogue=BRIEF,
                   turns=[am.Turn(speaker="caller", text="Алло!"), am.Turn(speaker="caller", text="Резерв 1"),
                          am.Turn(speaker="caller", text="Резерв 2")])


def test_never_leak():
    assert never_leak("Да он курил в постели, наверное", BRIEF) == ["secret"]
    assert never_leak("Не знаю, почему загорелось", BRIEF) == []
    assert never_leak("что угодно", None) == []


def test_allowed_facts():
    assert allowed_facts(["entrance", "secret", "nope", "entrance", " smoke "], BRIEF, CS) == ["entrance", "smoke"]


def test_fallback_cycles_reserve_lines():
    h = [am.DialogueTurn(turn_no=1, speaker="caller", text="Алло!")]
    assert fallback_line(CS, h) == "Резерв 1"
    h += [am.DialogueTurn(turn_no=2, speaker="operator", text="?"),
          am.DialogueTurn(turn_no=3, speaker="caller", text="Резерв 1")]
    assert fallback_line(CS, h) == "Резерв 2"
    h += [am.DialogueTurn(turn_no=4, speaker="operator", text="?"),
          am.DialogueTurn(turn_no=5, speaker="caller", text="Резерв 2")]
    assert fallback_line(CS, h) == "Резерв 1"


def test_render_history_window():
    h = [am.DialogueTurn(turn_no=i, speaker="caller" if i % 2 else "operator", text=f"r{i}") for i in range(1, 12)]
    out = render_history(h, 4)
    assert out.startswith("(начало разговора опущено)") and "r11" in out and "r7" not in out


def test_truncate_words():
    assert truncate_words("Раз два. Три четыре пять шесть", 4) == "Раз два."
    assert truncate_words("один два три четыре пять", 3) == "один два три…"
    assert truncate_words("коротко", 40) == "коротко"


def test_contains_phrase_by_stems():
    assert contains_phrase(norm("Помощь уже направлена, ждите"), "ждите")
    assert contains_phrase(norm("Перезвоните, пожалуйста, позже"), "перезвоните позже") is False
    assert contains_phrase(norm("вам перезвонят позже"), "перезвоните позже") is False
