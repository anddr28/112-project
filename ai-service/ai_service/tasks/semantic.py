"""Слой 3 — семантика: суждения LLM по фактам -> балл по формуле sem-v1."""

from __future__ import annotations

from typing import TYPE_CHECKING, Any, Literal

from pydantic import BaseModel, Field

from ai_service.core.errors import JobError
from ai_service.core.queue import JobOutcome
from ai_service.gen import api_models as am
from ai_service.tasks import scoring
from ai_service.tasks.textutil import norm, significant_stems, word_count

if TYPE_CHECKING:
    from ai_service.context import AppContext

FIELD_LABELS = {
    "description": "Описание происшествия",
    "actions_taken": "Принятые меры",
    "action_text": "Действия диспетчера",
}


class FactJudgement(BaseModel):
    id: str
    status: Literal["present", "partial", "missing"]
    evidence: str = ""


class ForbiddenJudgement(BaseModel):
    id: str
    evidence: str = ""


class FieldComment(BaseModel):
    field: str
    comment: str


class SemanticLlmOut(BaseModel):
    facts: list[FactJudgement]
    forbidden_found: list[ForbiddenJudgement] = Field(default_factory=list)
    coherence: Literal["ok", "weak", "contradiction"] = "ok"
    per_field: list[FieldComment] = Field(default_factory=list)
    summary_for_student: str = ""
    self_confidence: float = Field(0.7, ge=0, le=1)


def _dedupe(items: list[str]) -> list[str]:
    out, seen = [], set()
    for s in items:
        s = (s or "").strip()
        if s and s not in seen:
            seen.add(s)
            out.append(s)
    return out


def select_facts(req: am.SemanticJobRequest) -> tuple[list[str], list[str]]:
    """Обязательные: (card_actions) expected_actions[].required_facts -> scoring.required_facts
    -> call_script.key_facts. Режим действий оценивает текст действий — его факты из
    эталонных действий (PY-05). Запрещённые — scoring + expected_actions."""
    et = req.etalon
    required: list[str] = []
    if req.mode == am.Mode.card_actions and et.expected_actions:
        for a in et.expected_actions:
            required += a.required_facts or []
    if not required and et.scoring and et.scoring.required_facts:
        required = list(et.scoring.required_facts)
    if not required and req.call_script and req.call_script.key_facts:
        required = list(req.call_script.key_facts)
    forbidden: list[str] = list((et.scoring.forbidden_facts or []) if et.scoring else [])
    for a in et.expected_actions or []:
        forbidden += a.forbidden_facts or []
    return _dedupe(required), _dedupe(forbidden)


def answer_fields(req: am.SemanticJobRequest) -> list[tuple[str, str]]:
    """(поле, текст) по free_text_fields: карточка (режим 1) или action_text (режим 2)."""
    names = list(req.free_text_fields) or (
        ["action_text"] if req.mode == am.Mode.card_actions else ["description", "actions_taken"])
    card = req.answer.card
    attrs: dict[str, Any] = (card.attributes if card and card.attributes else {}) or {}
    out = []
    for n in names:
        if n == "description":
            t = card.description if card else None
        elif n == "actions_taken":
            t = card.actions_taken if card else None
        elif n == "action_text":
            t = req.answer.action_text
        elif n in ("answer_turns", "turns"):
            t = "\n".join(x.answer_text for x in req.answer.turns or [])
        else:
            v = attrs.get(n)
            t = v if isinstance(v, str) else None
        out.append((n, (t or "").strip()))
    return out


def _etalon_text(req: am.SemanticJobRequest) -> str:
    parts = []
    c = req.etalon.card
    if req.mode == am.Mode.card_actions and req.etalon.expected_actions:
        parts += [f"- {a.action_text}" for a in req.etalon.expected_actions]
    if c.description:
        parts.append(f"Описание: {c.description}")
    if c.actions_taken:
        parts.append(f"Принятые меры: {c.actions_taken}")
    return "\n".join(parts) or "—"


def _legend(req: am.SemanticJobRequest) -> str:
    cs = req.call_script
    if not cs:
        return "—"
    bits = []
    if cs.address and cs.address.raw:
        bits.append(f"адрес: {cs.address.raw}")
    if cs.key_facts:
        bits.append("; ".join(cs.key_facts))
    return "; ".join(bits) or "—"


def evidence_found(evidence: str, answer_norm: str) -> bool:
    """Цитата действительно из ответа: ≥ половины её значимых основ есть в тексте."""
    st = significant_stems(evidence, min_len=3)
    if not st:
        return False
    hits = sum(1 for s in st if s in answer_norm)
    return hits * 2 >= len(st)


async def run_semantic(ctx: AppContext, req: am.SemanticJobRequest) -> JobOutcome:
    fields = answer_fields(req)
    answer_text = "\n".join(t for _, t in fields if t)
    required, forbidden = select_facts(req)
    profile = (req.profile.value if req.profile else "eval_fast")
    prompt = ctx.prompts.get("semantic")
    engine: dict[str, Any] = {"prompt_version": prompt.prompt_version, "rules_version": scoring.SEMANTIC_RULES}

    if not answer_text:
        # Пустой ответ: LLM звать не о чем — всё обязательное «не зафиксировано».
        value = {"score": 0, "confidence": 1.0, "missing_facts": required, "extra_facts": [],
                 "per_field": [{"field": n, "score": 0, "comment": "Поле не заполнено."} for n, _ in fields],
                 "summary_for_student": "Свободный текст не заполнен — ключевые факты происшествия не зафиксированы."}
        return JobOutcome(value=value, engine=engine)

    fact_ids = {f"f{i + 1}": f for i, f in enumerate(required)}
    forb_ids = {f"x{i + 1}": f for i, f in enumerate(forbidden)}
    cat = req.scenario_context.category if req.scenario_context and req.scenario_context.category else None
    role_desc = ("диспетчер ДДС описал свои действия по карточке происшествия."
                 if req.mode == am.Mode.card_actions else
                 "оператор 112 заполнил свободные поля карточки происшествия по звонку.")
    values = {
        "role_desc": role_desc,
        "category": (cat.name or cat.code) if cat else "—",
        "legend": _legend(req),
        "etalon": _etalon_text(req),
        "required": "\n".join(f"{k}: {v}" for k, v in fact_ids.items()) or "— (нет; оцени только домыслы и связность)",
        "forbidden": "\n".join(f"{k}: {v}" for k, v in forb_ids.items()) or "— (нет)",
        "answer": "\n".join(f"[{n}] {FIELD_LABELS.get(n, n)}: {t or '(пусто)'}" for n, t in fields),
    }
    messages = [{"role": "system", "content": prompt.render("system", **values)},
                {"role": "user", "content": prompt.render("user", **values)}]
    try:
        res = await ctx.llm.chat(profile, messages, SemanticLlmOut)
    except JobError as e:
        e.engine = {**engine, **(e.engine or {})}
        raise
    out = res.obj
    engine.update(llm_model=res.model, tokens_in=res.tokens_in, tokens_out=res.tokens_out)

    answer_norm = norm(answer_text)
    by_id = {j.id.strip(): j for j in out.facts}
    statuses, missing, supported = [], [], 0
    for fid, text in fact_ids.items():
        j = by_id.get(fid)
        status = j.status if j else "missing"  # модель пропустила факт — считаем не найденным
        statuses.append(status)
        if status == "missing":
            missing.append(text)
            supported += 1 if j else 0
        elif j and evidence_found(j.evidence, answer_norm):
            supported += 1
    extra = []
    for fj in out.forbidden_found:
        text = forb_ids.get(fj.id.strip())
        # Штраф только за домысел с цитатой из ответа: выдуманное моделью «нарушение» не
        # должно снижать балл.
        if text and text not in extra and evidence_found(fj.evidence, answer_norm):
            extra.append(text)

    score = scoring.semantic_v1(statuses, len(extra), out.coherence)
    evidence_share = supported / len(fact_ids) if fact_ids else 1.0
    confidence = scoring.semantic_confidence(out.self_confidence, evidence_share, word_count(answer_text))

    comments = {c.field.strip(): c.comment.strip() for c in out.per_field}
    per_field = []
    for n, t in fields:
        if not t:
            continue
        item: dict[str, Any] = {"field": n, "score": score}
        if comments.get(n):
            item["comment"] = comments[n]
        per_field.append(item)
    summary = out.summary_for_student.strip() or (
        "Ключевые факты переданы." if not missing else "Не зафиксировано: " + "; ".join(missing) + ".")
    value = {"score": score, "confidence": confidence, "missing_facts": missing, "extra_facts": extra,
             "per_field": per_field, "summary_for_student": summary}
    return JobOutcome(value=value, engine=engine)
