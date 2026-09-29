"""Ход диалога (PY-07): реплика оператора (текст | аудио -> STT) -> LLM-заявитель -> TTS.

Синхронный путь — студент ждёт на линии, у go-core таймаут 20 с. Поэтому всё здесь
подчинено дедлайну хода: ожидание полосы LLM и сам вызов режутся по остатку времени, а
при недоступности/медлительности LLM заявитель отвечает сценарной репликой (fallback=true)
— занятие не срывается. Модели не доверяем: раскрытые факты фильтруются по брифу, утечка
факта reveal=never — повтор со строгой инструкцией, затем fallback; раннее «прощание»
модели отсекается.
"""

from __future__ import annotations

import asyncio
import logging
import time
from typing import TYPE_CHECKING, Any

from pydantic import BaseModel, Field

from ai_service.core.errors import HttpError, JobError, service_busy
from ai_service.core.logging import log_fields
from ai_service.core.queue import CLASS_DIALOG
from ai_service.engines.tts.store import NO_SPEECH_PATH, NO_SPEECH_TEXT, dialog_path, tts_hash
from ai_service.gen import api_models as am
from ai_service.tasks.textutil import contains_any, norm, significant_stems, truncate_words
from ai_service.tasks.voice import TtsUnavailable, synthesize_to, transcribe

if TYPE_CHECKING:
    from ai_service.context import AppContext

log = logging.getLogger("ai_service.dialog")

DEFAULT_MAX_TURNS = 12
DEFAULT_MAX_WORDS = 40
TTS_RESERVE_S = 2.5
MIN_LLM_S = 3.0

REVEAL_LABEL = {"volunteer": "сам", "on_request": "по вопросу", "never": "никогда"}
# Оператор завершает разговор: помощь направлена / прощание — только тогда модели можно
# «положить трубку» раньше третьего хода (модели склонны прощаться рано).
END_MARKERS = ("направлен", "направля", "выехал", "выезжа", "выслал", "высыла", "отправил", "едут", "в пути",
               "до свидания", "всего доброго", "всего хорошего", "кладу трубку", "можете положить трубку")


class CallerLlmOut(BaseModel):
    reply: str
    revealed_fact_ids: list[str] = Field(default_factory=list)
    is_unknown_answer: bool = False
    should_end: bool = False
    end_reason: str = ""
    emotional_state: str = ""


def _address(cs: am.CallScript) -> str:
    a = cs.address
    if a.raw:
        return a.raw
    parts = [a.city, a.street, a.house and f"дом {a.house}", a.entrance and f"подъезд {a.entrance}",
             a.floor and f"этаж {a.floor}", a.apartment and f"квартира {a.apartment}", a.landmark]
    return ", ".join(p for p in parts if p) or "не указан"


def render_facts(brief: am.DialogueBrief | None, cs: am.CallScript) -> str:
    if brief is None:
        # Легенда без брифа: key_facts — всё, что заявитель может рассказать по вопросу.
        facts = cs.key_facts or []
        return "\n".join(f"- [по вопросу] id=k{i + 1}: {f}" for i, f in enumerate(facts)) or "- (нет)"
    lines = []
    for f in brief.facts:
        hints = f" (подсказки: {', '.join(f.hints)})" if f.hints and f.reveal.value == "on_request" else ""
        lines.append(f"- [{REVEAL_LABEL[f.reveal.value]}] id={f.id}: {f.text}{hints}")
    return "\n".join(lines)


def render_history(history: list[am.DialogueTurn], limit: int) -> str:
    turns = sorted(history, key=lambda t: t.turn_no)
    head = "(начало разговора опущено)\n" if len(turns) > limit else ""
    lines = [f"{'Оператор' if t.speaker == 'operator' else 'Вы'}: {t.text}" for t in turns[-limit:]]
    return head + ("\n".join(lines) or "(вы только что дозвонились)")


def never_leak(reply: str, brief: am.DialogueBrief | None) -> list[str]:
    """id фактов reveal=never, «проговорённых» в реплике: совпали ≥ 2 ключевых основ факта
    (для короткого факта — все), и среди них — одна из двух самых длинных. Основы режутся
    на 2 буквы (≥ 4): «курит» ~ «курил», «постели» ~ «постель»."""
    if brief is None:
        return []
    words = norm(reply).split()
    leaked = []
    for f in brief.facts:
        if f.reveal.value != "never":
            continue
        stems = sorted(significant_stems(f.text), key=len, reverse=True)
        if not stems:
            continue
        keys = [s[: max(4, len(s) - 2)] for s in stems]
        hit = [any(w.startswith(k) for w in words) for k in keys]
        if sum(hit) >= min(2, len(keys)) and any(hit[:2]):
            leaked.append(f.id)
    return leaked


def fallback_line(cs: am.CallScript, history: list[am.DialogueTurn]) -> str:
    """Сценарная реплика по кругу: turns[0] — вступление (уже прозвучало), дальше — резерв."""
    lines = [t.text for t in cs.turns if t.speaker.value == "caller" and t.text.strip()]
    if not lines:
        return "Алло! Вы меня слышите? Приезжайте скорее!"
    reserve = lines[1:] or lines
    k = sum(1 for t in history if t.speaker == "caller")
    return reserve[max(k - 1, 0) % len(reserve)]


def allowed_facts(ids: list[str], brief: am.DialogueBrief | None, cs: am.CallScript) -> list[str]:
    if brief is None:
        return []
    ok = {f.id for f in brief.facts if f.reveal.value != "never"}
    out: list[str] = []
    for i in ids:
        i = i.strip()
        if i in ok and i not in out:
            out.append(i)
    return out


class TurnState:
    """Замеры хода — для Engine и лога (QA/bench читают stt_ms/llm_ms/tts_ms)."""

    def __init__(self, deadline_s: float) -> None:
        self.t0 = time.monotonic()
        self.deadline_s = deadline_s
        self.engine: dict[str, Any] = {}
        self.stt_ms = self.llm_ms = self.tts_ms = self.wait_ms = 0
        self.fallback_reason: str | None = None

    def left(self) -> float:
        return self.deadline_s - (time.monotonic() - self.t0)


async def run_dialog_turn(ctx: AppContext, req: am.DialogTurnRequest, audio: bytes | None,
                          audio_type: str | None) -> dict[str, Any]:
    # Бюджет хода внутри таймаута go-core (20 с по контракту): запас на сеть и запись ответа.
    st = TurnState(ctx.settings.dialog_deadline_sec)
    # Backpressure до STT: если полоса LLM забита ходами, честно просим повторить (503),
    # а не держим студента 20 с и не жжём STT впустую.
    if ctx.jobs.dialog_waiting() >= ctx.settings.dialog_max_waiting:
        raise service_busy("Заявитель не отвечает: полоса перегружена, повторите реплику",
                           ctx.settings.dialog_retry_after_sec)

    opts = req.options or am.Options4()
    tts_opts = opts.tts or am.Tts()
    tts_on = tts_opts.enabled is not False
    voice, rate = tts_opts.voice, (tts_opts.rate if tts_opts.rate and tts_opts.rate > 0 else 1.0)
    max_words = opts.max_reply_words if opts.max_reply_words and opts.max_reply_words > 0 else DEFAULT_MAX_WORDS
    cs = req.call_script
    brief = cs.dialogue

    # 1. Реплика оператора: текст важнее аудио (контракт), STT не вызывается.
    op_text = (req.operator_text or "").strip()
    if op_text:
        operator: dict[str, Any] = {"text": op_text, "language": (opts.lang or "ru"), "confidence": 1.0,
                                    "audio_duration_ms": 0, "no_speech": False}
    elif audio is not None:
        operator, stt_engine = await transcribe(ctx, audio, audio_type, opts.stt)
        st.stt_ms = stt_engine["duration_ms"]
        st.engine.update(stt_model=stt_engine["stt_model"], stt_version=stt_engine["stt_version"])
        op_text = operator["text"].strip()
    else:
        raise HttpError(400, "bad_payload", "Нужен operator_text или часть audio")

    # 2. Тишина: заявитель переспрашивает служебной фразой, без LLM; транскрипт не пополняется.
    if operator.get("no_speech") or not op_text:
        operator["no_speech"] = True
        caller: dict[str, Any] = {"text": NO_SPEECH_TEXT, "revealed_fact_ids": [], "is_unknown_answer": False,
                                  "should_end": False}
        if cs.caller.emotional_state:
            caller["emotional_state"] = cs.caller.emotional_state
        if tts_on:
            await _attach_tts(ctx, st, caller, NO_SPEECH_PATH, NO_SPEECH_TEXT, voice, rate, reuse=True)
        return _result(req, operator, caller, st, fallback=False)

    # 3. LLM-заявитель.
    caller = await _llm_reply(ctx, st, req, op_text, max_words)
    if caller is None:
        caller = {"text": fallback_line(cs, list(req.history)), "revealed_fact_ids": [], "is_unknown_answer": False,
                  "should_end": False}
        if cs.caller.emotional_state:
            caller["emotional_state"] = cs.caller.emotional_state

    max_turns = brief.max_turns if brief and brief.max_turns else DEFAULT_MAX_TURNS
    if req.turn_no >= max_turns and not caller["should_end"]:
        caller["should_end"], caller["end_reason"] = True, "max_turns"

    # 4. Озвучка ответа.
    if tts_on:
        await _attach_tts(ctx, st, caller, dialog_path(str(req.attempt_id), req.turn_no), caller["text"], voice, rate,
                          reuse=False)
    return _result(req, operator, caller, st, fallback=st.fallback_reason is not None)


async def _llm_reply(ctx: AppContext, st: TurnState, req: am.DialogTurnRequest, op_text: str,
                     max_words: int) -> dict[str, Any] | None:
    """Ответ модели после пост-проверок; None — fallback (причина в st.fallback_reason)."""
    cs = req.call_script
    brief = cs.dialogue
    profile = req.profile.value if req.profile and req.profile.value == "dialog_fast" else "dialog_fast"
    prompt = ctx.prompts.get("caller")
    st.engine["prompt_version"] = prompt.prompt_version
    revealed = set(req.revealed_fact_ids or [])
    for t in req.history:
        revealed.update(t.revealed_fact_ids or [])
    values = {
        "persona": brief.persona if brief else (cs.caller.role or "очевидец происшествия"),
        "caller_name": cs.caller.name or "не представился",
        "caller_role": cs.caller.role or "очевидец",
        "emotional_state": cs.caller.emotional_state or "взволнован",
        "speaking_style": (brief.speaking_style if brief and brief.speaking_style else "короткие взволнованные фразы"),
        "address": _address(cs),
        "phone": cs.caller.phone or "не помнит",
        "facts": render_facts(brief, cs),
        "revealed": ", ".join(sorted(revealed)) or "пока ничего",
        "unknowns": "; ".join(brief.unknowns) if brief and brief.unknowns else "всё, чего нет в таблице фактов",
        "end_conditions": "; ".join(brief.end_conditions) if brief and brief.end_conditions
        else "оператор сообщил, что помощь направлена",
        "max_words": max_words,
        "history": render_history(list(req.history), ctx.settings.dialog_history_turns),
        "operator_text": op_text,
    }
    system = prompt.render("system", **values)
    user = prompt.render("user", **values)

    reserve = TTS_RESERVE_S if ctx.tts is not None else 0.5
    wait_budget = st.left() - reserve - MIN_LLM_S
    if wait_budget <= 0:
        st.fallback_reason = "deadline"
        return None
    try:
        async with ctx.jobs.lanes["llm"].slot(CLASS_DIALOG, 1, timeout=wait_budget) as waited:
            st.wait_ms = int(waited)
            t_llm = time.monotonic()
            try:
                out, leaked = await _ask(ctx, st, profile, system, user, max(st.left() - reserve, MIN_LLM_S), brief)
                if leaked:
                    strict = system + "\n\n" + prompt.render("strict")
                    out, leaked = await _ask(ctx, st, profile, strict, user, max(st.left() - reserve, 1.0), brief)
            finally:
                st.llm_ms = int((time.monotonic() - t_llm) * 1000)
    except TimeoutError:
        st.fallback_reason = "llm_lane_timeout"
        return None
    except JobError as e:
        st.fallback_reason = e.code
        return None
    if leaked:
        st.fallback_reason = "never_fact_leak"
        return None

    text = truncate_words(" ".join(out.reply.split()), max_words)
    if not text:
        st.fallback_reason = "empty_reply"
        return None
    caller: dict[str, Any] = {
        "text": text,
        "revealed_fact_ids": allowed_facts(out.revealed_fact_ids, brief, cs),
        "is_unknown_answer": bool(out.is_unknown_answer),
        "should_end": bool(out.should_end),
    }
    if caller["should_end"]:
        said_end = contains_any(norm(op_text), END_MARKERS) is not None
        if req.turn_no < 3 and not said_end:
            caller["should_end"] = False  # модели свойственно «прощаться» раньше времени
        else:
            caller["end_reason"] = out.end_reason.strip() or "оператор сообщил, что помощь направлена"
    state = out.emotional_state.strip() or (cs.caller.emotional_state or "")
    if state:
        caller["emotional_state"] = state[:60]
    return caller


async def _ask(ctx: AppContext, st: TurnState, profile: str, system: str, user: str, timeout: float,
               brief: am.DialogueBrief | None) -> tuple[CallerLlmOut, list[str]]:
    res = await ctx.llm.chat(profile, [{"role": "system", "content": system}, {"role": "user", "content": user}],
                             CallerLlmOut, timeout=timeout)
    st.engine.update(llm_model=res.model)
    st.engine["tokens_in"] = st.engine.get("tokens_in", 0) + res.tokens_in
    st.engine["tokens_out"] = st.engine.get("tokens_out", 0) + res.tokens_out
    never_ids = {f.id for f in brief.facts if f.reveal.value == "never"} if brief else set()
    leaked = never_leak(res.obj.reply, brief) + [i for i in res.obj.revealed_fact_ids if i in never_ids]
    return res.obj, sorted(set(leaked))


async def _attach_tts(ctx: AppContext, st: TurnState, caller: dict[str, Any], rel: str, text: str,
                      voice: str | None, rate: float, *, reuse: bool) -> None:
    """Озвучка ответа. Недоступный TTS не срывает ход: go-core покажет текст и оценит
    длительность сам (реплика без tts)."""
    if ctx.tts is None:
        return
    t = time.monotonic()
    try:
        dur, v = await asyncio.wait_for(
            synthesize_to(ctx, rel, text, voice, rate, reuse=reuse, cls=CLASS_DIALOG), timeout=max(st.left(), 1.0))
    except (TtsUnavailable, TimeoutError) as e:
        log.warning("ход без озвучки", extra=log_fields(error=str(e)))
        return
    finally:
        st.tts_ms = int((time.monotonic() - t) * 1000)
    caller["tts"] = {"text_hash": tts_hash(text, v, rate), "file_path": rel, "duration_ms": dur, "voice": v,
                     "rate": rate}
    st.engine["tts_version"] = ctx.tts.version


def _result(req: am.DialogTurnRequest, operator: dict[str, Any], caller: dict[str, Any], st: TurnState,
            fallback: bool) -> dict[str, Any]:
    duration = int((time.monotonic() - st.t0) * 1000)
    engine = {**st.engine, "duration_ms": duration, "queue_wait_ms": st.wait_ms}
    log.info("ход диалога", extra=log_fields(
        request_id=str(req.request_id), attempt_id=str(req.attempt_id), turn_no=req.turn_no, stt_ms=st.stt_ms,
        llm_ms=st.llm_ms, tts_ms=st.tts_ms, wait_ms=st.wait_ms, total_ms=duration, fallback=fallback,
        fallback_reason=st.fallback_reason, no_speech=operator.get("no_speech"),
        revealed=caller.get("revealed_fact_ids"), should_end=caller.get("should_end")))
    return {"schema_version": "1", "request_id": str(req.request_id), "attempt_id": str(req.attempt_id),
            "turn_no": req.turn_no, "operator": operator, "caller": caller, "fallback": fallback, "engine": engine}
