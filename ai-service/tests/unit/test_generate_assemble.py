from ai_service.gen import api_models as am
from ai_service.tasks.generate import GenLlmOut, assemble, scene_kind, validate_draft

DRAFT = {
    "title": "Пожар", "caller": {"name": "Иван", "phone": "не помню", "role": "сосед", "emotional_state": "паника"},
    "address": {"street": "Тверская улица", "house": "12"}, "persona": "мужчина 40 лет", "opening": "Алло! Пожар!",
    "facts": [{"id": "a", "text": "Горит квартира на 3 этаже", "reveal": "on_request"},
              {"id": "b", "text": "Сосед курит в постели", "reveal": "never"}],
    "description": "Пожар в квартире на 3 этаже.", "services": [], "checklist": [],
}


def spec(difficulty=3, services=("101",), code="101", name="Пожар"):
    return am.Spec(category=am.Category1(code=code, name=name, services=list(services)), difficulty=difficulty,
                   mode="cards")


def test_assemble_difficulty3_keeps_never_and_fixes_invariants():
    res = assemble(GenLlmOut.model_validate(DRAFT), spec(3), "req-1")
    am.ScenarioResult.model_validate(res)
    cs = res["call_script"]
    facts = {f["id"]: f for f in cs["dialogue"]["facts"]}
    assert facts["address"]["reveal"] == "volunteer"  # добавлен факт с адресом
    assert facts["b"]["reveal"] == "never" and "Сосед курит в постели" not in cs["key_facts"]
    assert any("телефон" in f["text"] or "номер" in f["text"] for f in facts.values())
    assert cs["caller"]["phone"].startswith("+7 (9")  # «не помню» -> валидный номер из зерна
    assert res["etalon_card"]["services_to_notify"] == ["101"]  # пустой список -> классификатор
    ids = [c["id"] for c in res["expected_dialogue"]["checklist"]]
    assert ids[0] == "ask_address" and ids[-1] == "say_help_dispatched"
    assert "expected_actions" not in res  # режим cards
    assert len(cs["turns"]) >= 2


def test_assemble_is_deterministic_by_request_id():
    d = GenLlmOut.model_validate(DRAFT)
    assert assemble(d, spec(), "x") == assemble(d, spec(), "x")
    assert assemble(d, spec(), "x")["call_script"]["caller"]["phone"] != assemble(d, spec(), "y")["call_script"][
        "caller"]["phone"]


def test_validate_draft():
    bad = GenLlmOut.model_validate({**DRAFT, "facts": [], "description": " ", "address": {"street": "", "house": ""}})
    errs = validate_draft(bad)
    assert len(errs) == 3


def test_scene_kind():
    assert scene_kind(spec(code="104", name="x")) == "gas"
    assert scene_kind(spec(code="zz", name="ДТП с пострадавшими")) == "dtp"
    assert scene_kind(spec(code="zz", name="Кошка на дереве")) == "generic"
