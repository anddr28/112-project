"""Слой 5 — оценка разговора: чек-лист протокола и тон (LLM) + метрики речи (без LLM)."""

from __future__ import annotations

import logging
from typing import TYPE_CHECKING, Any, Literal

from pydantic import BaseModel, Field

from ai_service.core.errors import JobError
from ai_service.core.logging import log_fields
from ai_service.core.queue import JobOutcome
from ai_service.gen import api_models as am
from ai_service.tasks import scoring
from ai_service.tasks.speech_metrics import fillers_per_100, speech_metrics
from ai_service.tasks.textutil import contains_any, contains_phrase, norm

if TYPE_CHECKING:
    from ai_service.context import AppContext

log = logging.getLogger("ai_service.dialogue_eval")

KIND_LABELS = {"question": "вопрос", "instruction": "указание заявителю", "phrase": "формула", "behavior": "поведение"}


class ItemJudgement(BaseModel):
    id: str
    status: Literal["done", "partial", "missed", "not_applicable"]
    evidence_turn_no: int = 0
    comment: str = ""


class ForbiddenHitOut(BaseModel):
    phrase: str
    turn_no: int = 0


class ToneOut(BaseModel):
    politeness: int = Field(80, ge=0, le=100)
    calmness: int = Field(80, ge=0, le=100)
    clarity: int = Field(80, ge=0, le=100)
    comment: str = ""


class DialogueLlmOut(BaseModel):
    checklist: list[ItemJudgement]
    forbidden_hits: list[ForbiddenHitOut] = Field(default_factory=list)
    tone: ToneOut = Field(default_factory=ToneOut)
    summary_for_student: str = ""
    self_confidence: float = Field(0.7, ge=0, le=1)


def render_transcript(transcript: list[am.DialogueTurn], floor: float) -> str:
    lines = []
    for t in sorted(transcript, key=lambda x: x.turn_no):
        who = "Оператор" if t.speaker == "operator" else "Заявитель"
        mark = ""
        if t.speaker == "operator" and t.confidence is not None and t.confidence < floor:
            mark = " (распознано неуверенно)"
        lines.append(f"[{t.turn_no}] {who}{mark}: {t.text}")
    return "\n".join(lines)


def deterministic_forbidden(transcript: list[am.DialogueTurn], forbidden: list[str]) -> list[dict[str, Any]]:
    """Дословные (по основам слов) вхождения запрещённых фраз — без LLM, надёжно."""
    hits = []
    for t in transcript:
        if t.speaker != "operator":
            continue
        tn = norm(t.text)
        for phrase in forbidden:
            if contains_phrase(tn, phrase):
                hits.append({"phrase": phrase, "turn_no": t.turn_no})
    return hits


def heuristic_status(item: am.DialogueChecklistItem, transcript: list[am.DialogueTurn]) -> tuple[str, int]:
    """Запасное суждение по подсказкам пункта, если модель пропустила пункт в ответе."""
    for t in transcript:
        if t.speaker == "operator" and item.hints and contains_any(norm(t.text), item.hints):
            return "done", t.turn_no
    return "missed", 0


async def run_dialogue_eval(ctx: AppContext, req: am.DialogueJobRequest) -> JobOutcome:
    opts = req.options or am.Options3()
    floor = opts.stt_confidence_floor if opts.stt_confidence_floor is not None else 0.6
    transcript = list(req.transcript)
    speech = speech_metrics(transcript, ctx.fillers, floor)
    exp = req.etalon.expected_dialogue
    forbidden = [f for f in (exp.forbidden or []) if f.strip()]
    profile = req.profile.value if req.profile else "eval_dialogue"
    prompt = ctx.prompts.get("dialogue_eval")
    engine: dict[str, Any] = {"prompt_version": prompt.prompt_version, "rules_version": scoring.DIALOGUE_RULES}

    operator_turns = {t.turn_no for t in transcript if t.speaker == "operator"}
    if not operator_turns:
        # Оператор не сказал ни слова — оценивать LLM нечего: всё обязательное не выполнено.
        checklist = [{"id": i.id, "status": "missed", "comment": "Оператор не вёл разговор."} for i in exp.checklist]
        value = {"score": 0, "confidence": 1.0, "checklist": checklist,
                 "missing_questions": [i.text for i in exp.checklist if i.required],
                 "forbidden_hits": [], "speech": speech,
                 "summary_for_student": "Разговор с заявителем не состоялся: протокол опроса не выполнен."}
        return JobOutcome(value=value, engine=engine)

    cs = req.call_script
    cat = req.scenario_context.category if req.scenario_context and req.scenario_context.category else None
    legend_bits = [cs.address.raw] if cs.address and cs.address.raw else []
    legend_bits += cs.key_facts or []
    checklist_lines = []
    for i in exp.checklist:
        hint = f" (подсказки: {', '.join(i.hints)})" if i.hints else ""
        req_mark = "обязательный" if i.required else "желательный"
        checklist_lines.append(f"{i.id}: {i.text} — {KIND_LABELS.get(i.kind.value, i.kind.value)}, {req_mark}{hint}")
    values = {
        "category": (cat.name or cat.code) if cat else "—",
        "legend": "; ".join(legend_bits) or "—",
        "checklist": "\n".join(checklist_lines),
        "forbidden": "; ".join(f"«{f}»" for f in forbidden) or "— (нет)",
        "transcript": render_transcript(transcript, floor),
    }
    messages = [{"role": "system", "content": prompt.render("system", **values)},
                {"role": "user", "content": prompt.render("user", **values)}]
    try:
        res = await ctx.llm.chat(profile, messages, DialogueLlmOut)
    except JobError as e:
        # Контракт: слой — failed retryable; метрики речи хотя бы в логе (их пересчитает повтор).
        log.warning("оценка разговора без LLM невозможна", extra=log_fields(
            attempt_id=str(req.attempt_id), speech=speech, error=e.message))
        e.engine = {**engine, **(e.engine or {})}
        raise
    out = res.obj
    engine.update(llm_model=res.model, tokens_in=res.tokens_in, tokens_out=res.tokens_out)

    by_id = {j.id.strip(): j for j in out.checklist}
    checklist: list[dict[str, Any]] = []
    judgements: list[scoring.ChecklistJudgement] = []
    missing_questions: list[str] = []
    for item in exp.checklist:
        j = by_id.get(item.id)
        if j is not None:
            status, turn, comment = j.status, j.evidence_turn_no, j.comment.strip()
        else:
            status, turn = heuristic_status(item, transcript)
            comment = "Оценено по ключевым словам (модель пропустила пункт)."
        entry: dict[str, Any] = {"id": item.id, "status": status}
        # Номер реплики — только реальная реплика оператора (UI подсвечивает её).
        if status in ("done", "partial") and turn in operator_turns:
            entry["evidence_turn_no"] = turn
        if comment:
            entry["comment"] = comment
        checklist.append(entry)
        judgements.append(scoring.ChecklistJudgement(status=status, required=item.required,
                                                     weight=float(item.weight if item.weight is not None else 1)))
        if item.required and status == "missed":
            missing_questions.append(item.text)
        elif item.required and status == "partial":
            missing_questions.append(f"{item.text} (частично)")

    hits = deterministic_forbidden(transcript, forbidden)
    seen = {(h["phrase"], h["turn_no"]) for h in hits}
    for h in out.forbidden_hits:
        # Суждение модели принимаем, только если фраза из списка и номер — реплика оператора.
        phrase = next((f for f in forbidden if norm(f) == norm(h.phrase) or norm(f) in norm(h.phrase)), None)
        if phrase and h.turn_no in operator_turns and (phrase, h.turn_no) not in seen:
            seen.add((phrase, h.turn_no))
            hits.append({"phrase": phrase, "turn_no": h.turn_no})

    score = scoring.dialogue_v1(judgements, len(hits), fillers_per_100(speech), speech.get("avg_response_ms"))
    confidence = scoring.dialogue_confidence(out.self_confidence, speech["low_confidence_turns"],
                                             speech["operator_turns"])
    tone = {"politeness": out.tone.politeness, "calmness": out.tone.calmness, "clarity": out.tone.clarity}
    if out.tone.comment.strip():
        tone["comment"] = out.tone.comment.strip()
    value = {
        "score": score, "confidence": confidence, "checklist": checklist, "missing_questions": missing_questions,
        "forbidden_hits": hits, "speech": speech, "tone": tone,
        "summary_for_student": out.summary_for_student.strip() or "Разговор оценён по чек-листу протокола.",
    }
    return JobOutcome(value=value, engine=engine)
