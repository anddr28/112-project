"""Генерация сценария (PY-10): LLM пишет черновик, код собирает ScenarioResult и чинит инварианты.

Почему не просить модель сразу выдать ScenarioResult: схема контракта глубокая (легенда,
бриф, эталонная карточка, чек-лист, действия), 7b-модели путают вложенность и дублируют
адрес с расхождениями. Плоский черновик + сборка в коде даёт инварианты «по построению»:
адрес легенды = адрес эталона, key_facts = факты брифа, категория и службы — реальные коды
классификатора из запроса. Неисправимое (нет фактов, нет описания) — один повтор с перечнем
ошибок, затем llm_invalid_output.
"""

from __future__ import annotations

import hashlib
import re
from typing import TYPE_CHECKING, Any, Literal

from pydantic import BaseModel, Field

from ai_service.core.errors import JobError
from ai_service.core.queue import JobOutcome
from ai_service.gen import api_models as am
from ai_service.tasks.textutil import contains_any, norm, significant_stems

if TYPE_CHECKING:
    from ai_service.context import AppContext

# Тип легенды -> few-shot протокола; по коду категории, затем по словам в названии/пути.
KIND_MATCH: list[tuple[str, tuple[str, ...], tuple[str, ...]]] = [
    ("gas", ("104", "04"), ("газ",)),
    ("fire", ("101", "01"), ("пожар", "горит", "дым", "возгоран", "задымл")),
    ("medical", ("103", "03"), ("скор", "медиц", "плохо", "сознан", "травм", "сердц", "болезн", "роды", "отравл")),
    ("dtp", (), ("дтп", "столкнов", "наезд", "дорожн", "автомоб")),
    ("police", ("102", "02"), ("полиц", "драк", "краж", "нападен", "хулиган", "грабе", "избие", "угон")),
]
PHONE_RE = re.compile(r"^\+7 \(9\d{2}\) \d{3}-\d{2}-\d{2}$")
ID_RE = re.compile(r"[^a-z0-9_]+")

BASE_ADDRESS_ITEM = {"id": "ask_address", "text": "Уточнил точный адрес происшествия", "kind": "question",
                     "required": True, "weight": 2.0, "hints": ["адрес", "улица", "дом", "где"]}
BASE_DISPATCH_ITEM = {"id": "say_help_dispatched", "text": "Сообщил, что помощь направлена", "kind": "phrase",
                      "required": True, "weight": 2.0, "hints": ["направлена", "выехали", "едут"]}
DEFAULT_FORBIDDEN = ["перезвоните позже", "ждите"]


class GenCaller(BaseModel):
    name: str
    phone: str = ""
    role: str = "очевидец"
    emotional_state: str = "взволнован"


class GenAddress(BaseModel):
    city: str = "Москва"
    street: str
    house: str
    entrance: str = ""
    floor: str = ""
    apartment: str = ""
    landmark: str = ""


class GenFact(BaseModel):
    id: str
    text: str
    reveal: Literal["volunteer", "on_request", "never"]
    hints: list[str] = Field(default_factory=list)


class GenCasualties(BaseModel):
    injured: int = Field(0, ge=0)
    dead: int = Field(0, ge=0)
    trapped: int = Field(0, ge=0)


class GenChecklistItem(BaseModel):
    id: str
    text: str
    kind: Literal["question", "instruction", "phrase", "behavior"]
    required: bool = True
    weight: float = Field(1.0, ge=0, le=5)
    hints: list[str] = Field(default_factory=list)


class GenExpectedAction(BaseModel):
    action_text: str
    required_facts: list[str] = Field(default_factory=list)
    forbidden_facts: list[str] = Field(default_factory=list)


class GenLlmOut(BaseModel):
    title: str
    caller: GenCaller
    address: GenAddress
    persona: str
    speaking_style: str = ""
    opening: str
    fallback_lines: list[str] = Field(default_factory=list)
    facts: list[GenFact]
    unknowns: list[str] = Field(default_factory=list)
    description: str
    actions_taken: str = ""
    casualties: GenCasualties = Field(default_factory=GenCasualties)
    services: list[str] = Field(default_factory=list)
    checklist: list[GenChecklistItem] = Field(default_factory=list)
    forbidden_phrases: list[str] = Field(default_factory=list)
    expected_actions: list[GenExpectedAction] = Field(default_factory=list)
    notes_for_teacher: str = ""


def scene_kind(spec: am.Spec) -> str:
    code = (spec.category.code or "").strip().lower()
    hay = norm(" ".join([spec.category.name, *(spec.category.path or [])]))
    for kind, codes, _ in KIND_MATCH:
        if code in codes:
            return kind
    for kind, _, stems in KIND_MATCH:
        if any(w in hay for w in stems):
            return kind
    return "generic"


def seed_of(request_id: str) -> int:
    """Зерно генерации из request_id: повтор той же задачи даёт тот же сценарий
    (идемпотентность), новая задача — новый сценарий."""
    return int.from_bytes(hashlib.sha256(request_id.encode()).digest()[:4], "big")


def _slug(s: str, fallback: str) -> str:
    v = ID_RE.sub("_", (s or "").strip().lower()).strip("_")
    return v[:40] or fallback


def _phone(p: str, seed: int) -> str:
    p = (p or "").strip()
    if PHONE_RE.match(p):
        return p
    digits = re.sub(r"\D", "", p)
    if len(digits) == 11 and digits[1] == "9":
        return f"+7 ({digits[1:4]}) {digits[4:7]}-{digits[7:9]}-{digits[9:11]}"
    n = seed % 10_000_000
    return f"+7 (9{15 + seed % 70:02d}) {n // 10000 % 1000:03d}-{n // 100 % 100:02d}-{n % 100:02d}"


def _clean_list(items: list[str], limit: int) -> list[str]:
    out, seen = [], set()
    for s in items:
        s = " ".join((s or "").split())
        if s and s.lower() not in seen:
            seen.add(s.lower())
            out.append(s)
    return out[:limit]


def validate_draft(d: GenLlmOut) -> list[str]:
    """Неисправимые кодом ошибки черновика — причина повтора генерации."""
    errs = []
    if not [f for f in d.facts if f.text.strip()]:
        errs.append("facts пуст — нужно 5–8 фактов")
    if not d.description.strip():
        errs.append("description (эталонное описание) пусто")
    if not d.address.street.strip() or not d.address.house.strip():
        errs.append("в address нужны street и house")
    if not d.opening.strip() and not d.fallback_lines:
        errs.append("нет вступительной реплики opening")
    return errs


def assemble(d: GenLlmOut, spec: am.Spec, request_id: str) -> dict[str, Any]:
    """Черновик LLM -> ScenarioResult (dict по схеме контракта) + заметки о починках."""
    seed = seed_of(request_id)
    notes: list[str] = []
    a = d.address
    city = a.city.strip() or "Москва"
    street, house = a.street.strip(), a.house.strip()
    spoken = f"{street}, {house}" + (f", подъезд {a.entrance.strip()}" if a.entrance.strip() else "")
    full_raw = f"{city}, {street}, {house}"
    phone = _phone(d.caller.phone, seed)

    # Бриф: уникальные id, обязательные факты адреса и телефона.
    facts: list[dict[str, Any]] = []
    seen: set[str] = set()
    for i, f in enumerate(d.facts):
        text = " ".join(f.text.split())
        if not text:
            continue
        fid = _slug(f.id, f"fact_{i + 1}")
        base, k = fid, 2
        while fid in seen:
            fid, k = f"{base}_{k}", k + 1
        seen.add(fid)
        item: dict[str, Any] = {"id": fid, "text": text, "reveal": f.reveal}
        hints = _clean_list(f.hints, 6)
        if hints:
            item["hints"] = hints
        facts.append(item)
    street_stems = significant_stems(street)
    if not any(s in norm(f["text"]) for f in facts for s in street_stems[:1]) and "address" not in seen:
        facts.insert(0, {"id": "address", "text": f"{city}, {street}, дом {house}" + (
            f", подъезд {a.entrance.strip()}" if a.entrance.strip() else ""), "reveal": "volunteer",
            "hints": ["адрес", "улица", "дом", "где"]})
        notes.append("Добавлен факт с адресом (в брифе его не было).")
    if not any("телефон" in norm(f["text"]) or "номер" in norm(f["text"]) for f in facts):
        fid = "phone" if "phone" not in {f["id"] for f in facts} else "caller_phone"
        facts.append({"id": fid, "text": f"Звонит со своего мобильного, номер {phone}", "reveal": "on_request",
                      "hints": ["телефон", "номер", "перезвонить"]})
    never = [f for f in facts if f["reveal"] == "never"]
    if spec.difficulty < 3 and never:
        for f in never:
            f["reveal"] = "on_request"
        notes.append("Факт-ловушка (never) переведён в «по вопросу»: такие факты — только для сложности 3.")
    if not any(f["reveal"] == "volunteer" for f in facts):
        facts[0]["reveal"] = "volunteer"

    key_facts = [f["text"] for f in facts if f["reveal"] != "never"]
    opening = " ".join(d.opening.split()) or (d.fallback_lines[0] if d.fallback_lines else "Алло! Помогите!")
    reserve = _clean_list(d.fallback_lines, 5)
    for f in facts:  # резерв хотя бы из трёх реплик — fallback при недоступности LLM
        if len(reserve) >= 3:
            break
        if f["reveal"] == "on_request" and f["text"] not in reserve:
            reserve.append(f["text"] + ".")
    turns = [{"speaker": "caller", "text": opening}] + [{"speaker": "caller", "text": t} for t in reserve
                                                        if t != opening]

    call_script = {
        "caller": {"name": d.caller.name.strip() or "Неизвестен", "phone": phone,
                   "role": d.caller.role.strip() or "очевидец",
                   "emotional_state": d.caller.emotional_state.strip() or "взволнован"},
        "address": {k: v for k, v in {
            "raw": spoken, "city": city, "street": street, "house": house, "entrance": a.entrance.strip(),
            "floor": a.floor.strip(), "apartment": a.apartment.strip(), "landmark": a.landmark.strip()}.items() if v},
        "key_facts": key_facts,
        "dialogue": {
            "persona": " ".join(d.persona.split()) or f"{d.caller.name}, {d.caller.role}",
            "speaking_style": " ".join(d.speaking_style.split()) or "короткие взволнованные фразы",
            "facts": facts,
            "unknowns": _clean_list(d.unknowns, 4),
            "end_conditions": ["Оператор сообщил, что помощь направлена", "Оператор попрощался"],
            "max_turns": 12,
        },
        "turns": turns,
    }

    # Эталонная карточка: категория и службы — только реальные коды классификатора из запроса.
    allowed = [s for s in (spec.category.services or []) if s]
    services = [s.strip() for s in d.services if s.strip()]
    if allowed:
        bad = [s for s in services if s not in allowed]
        services = [s for s in dict.fromkeys(services) if s in allowed]
        if bad:
            notes.append(f"Убраны коды служб не из классификатора: {', '.join(bad)}.")
        if not services:
            services = list(allowed)
            notes.append("Службы эталона взяты из классификатора категории.")
    else:
        services = list(dict.fromkeys(services))
    card_address = {k: v for k, v in {
        "raw": full_raw, "city": city, "street": street, "house": house, "entrance": a.entrance.strip(),
        "floor": a.floor.strip(), "apartment": a.apartment.strip(), "landmark": a.landmark.strip()}.items() if v}
    card: dict[str, Any] = {
        "category_code": spec.category.code,
        "address": card_address,
        "applicant": {"name": call_script["caller"]["name"], "phone": phone},
        "services_to_notify": services,
        "description": " ".join(d.description.split()),
        "actions_taken": " ".join(d.actions_taken.split()),
        "attributes": {},
    }
    cas = {k: v for k, v in d.casualties.model_dump().items() if v}
    if cas:
        card["casualties"] = cas

    # Чек-лист протокола: уникальные id, обязательные ask_address и say_help_dispatched.
    checklist: list[dict[str, Any]] = []
    cids: set[str] = set()
    for i, c in enumerate(d.checklist[:10]):
        if not c.text.strip():
            continue
        cid = _slug(c.id, f"chk_{i + 1}")
        base, k = cid, 2
        while cid in cids:
            cid, k = f"{base}_{k}", k + 1
        cids.add(cid)
        item = {"id": cid, "text": " ".join(c.text.split()), "kind": c.kind, "required": c.required,
                "weight": c.weight}
        hints = _clean_list(c.hints, 6)
        if hints:
            item["hints"] = hints
        checklist.append(item)
    _ensure_item(checklist, cids, BASE_ADDRESS_ITEM, ("адрес",), notes)
    _ensure_item(checklist, cids, BASE_DISPATCH_ITEM, ("направлен", "выехал", "помощь"), notes)
    forbidden = _clean_list(d.forbidden_phrases, 5) or list(DEFAULT_FORBIDDEN)

    result: dict[str, Any] = {
        "title": _title(d.title, spec),
        "call_script": call_script,
        "etalon_card": card,
        "expected_dialogue": {"checklist": checklist, "forbidden": forbidden, "max_operator_turns": 10},
        "difficulty_estimate": spec.difficulty,
    }
    if spec.mode.value != "cards":
        actions = []
        for ea in d.expected_actions[:3]:
            if ea.action_text.strip():
                actions.append({"action_text": " ".join(ea.action_text.split()),
                                "required_facts": _clean_list(ea.required_facts, 6),
                                "forbidden_facts": _clean_list(ea.forbidden_facts, 4)})
        if not actions and card["actions_taken"]:
            actions.append({"action_text": card["actions_taken"], "required_facts": key_facts[:3],
                            "forbidden_facts": []})
            notes.append("Эталонные действия построены из «Принятых мер» — проверьте формулировку.")
        if actions:
            result["expected_actions"] = actions
    teacher = " ".join(d.notes_for_teacher.split())
    comment = (spec.teacher_comment or "").strip()
    if comment:
        teacher = (teacher + f" Учтён комментарий преподавателя: «{comment[:200]}».").strip()
    result["notes_for_teacher"] = " ".join([teacher, *notes]).strip() or \
        "Проверьте адрес, факты брифа заявителя и чек-лист разговора перед подтверждением."
    return result


def _ensure_item(checklist: list[dict[str, Any]], cids: set[str], base: dict[str, Any], words: tuple[str, ...],
                 notes: list[str]) -> None:
    if base["id"] in cids:
        return
    for item in checklist:  # пункт есть по смыслу, но с другим id — переименовать
        if contains_any(norm(item["text"]), words):
            cids.discard(item["id"])
            item["id"] = base["id"]
            cids.add(base["id"])
            return
    checklist.insert(0 if base["id"] == "ask_address" else len(checklist), dict(base))
    cids.add(base["id"])
    notes.append(f"В чек-лист добавлен обязательный пункт «{base['text']}».")


def _title(title: str, spec: am.Spec) -> str:
    t = " ".join((title or "").split())[:150] or f"{spec.category.name} — учебный сценарий"
    avoid = {norm(x) for x in spec.avoid_titles or []}
    base, k = t, 2
    while norm(t) in avoid:
        t, k = f"{base} (вариант {k})", k + 1
    return t


async def run_generate(ctx: AppContext, req: am.GenerateJobRequest) -> JobOutcome:
    spec = req.spec
    prompt = ctx.prompts.get("generate")
    profile = req.profile.value if req.profile else "generate"
    engine: dict[str, Any] = {"prompt_version": prompt.prompt_version}
    kind = scene_kind(spec)
    mode = spec.mode.value
    actions_rule = ("не нужно (режим cards) — верни пустой список." if mode == "cards" else
                    "1–2 эталонных действия диспетчера ДДС: action_text (что сделать), required_facts (2–4 факта, "
                    "которые обязательно должны быть в описании действий), forbidden_facts (домыслы).")
    values = {
        "services": ", ".join(spec.category.services or []) or "101, 102, 103, 104",
        "actions_rule": actions_rule,
        "avoid_titles": "; ".join(f"«{t}»" for t in (spec.avoid_titles or [])[:20]) or "—",
        "category": spec.category.name,
        "category_code": spec.category.code,
        "category_path": " → ".join(spec.category.path or [spec.category.name]),
        "attributes": ", ".join(spec.category.attributes or []) or "—",
        "difficulty": spec.difficulty,
        "mode": mode,
        "teacher_comment": (spec.teacher_comment or "").strip() or "—",
        "fewshot": ctx.prompts.fewshot(kind) or ctx.prompts.fewshot("generic"),
    }
    system = prompt.render("system", **values)
    user = prompt.render("user", **values)
    messages = [{"role": "system", "content": system}, {"role": "user", "content": user}]
    seed = seed_of(str(req.request_id))
    tokens_in = tokens_out = 0
    errors: list[str] = []
    for attempt in range(2):
        try:
            res = await ctx.llm.chat(profile, messages, GenLlmOut, seed=seed + attempt)
        except JobError as e:
            e.engine = {**engine, **(e.engine or {})}
            raise
        tokens_in += res.tokens_in
        tokens_out += res.tokens_out
        engine.update(llm_model=res.model, tokens_in=tokens_in, tokens_out=tokens_out)
        errors = validate_draft(res.obj)
        if not errors:
            value = assemble(res.obj, spec, str(req.request_id))
            # Итог обязан пройти схему ScenarioResult — иначе go-core отвергнет callback.
            am.ScenarioResult.model_validate(value)
            return JobOutcome(value=value, engine=engine)
        messages = [*messages, {"role": "assistant", "content": res.obj.model_dump_json()[:6000]},
                    {"role": "user", "content": prompt.render("retry", errors="; ".join(errors))}]
    err = JobError("llm_invalid_output", "сценарий не прошёл проверку: " + "; ".join(errors))
    err.engine = engine
    raise err
