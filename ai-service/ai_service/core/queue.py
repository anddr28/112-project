"""Полосы исполнения, приоритеты, реестр задач (идемпотентность по request_id).

Модель (docs/tasks/python-ai-service.md, PY-02):
* полоса ``llm`` — N слотов (по умолчанию 1: одна модель в памяти), ожидающие упорядочены
  ключом ``(класс, priority, seq)``: класс 0 — ход диалога, 1 — evaluate_*, 2 — generate.
  Так фоновая LLM-задача стартует, только когда ни один студент не ждёт ответа заявителя;
  уже стартовавшая — дорабатывает (num_predict ограничен профилем);
* ``lt`` — LanguageTool (несколько слотов: грамматика не стоит за семантикой),
  ``tts`` и ``stt`` — по одному слоту (CPU-движки в одном процессе);
* реестр ``request_id -> задача``: повтор queued/running — 202 без дубля, повтор done —
  202 + повторный callback. Отказы (status=failed) не кэшируются: go-core присылает
  повтор после failed под новым request_id, а повтор того же id (reaper) должен
  считаться заново, а не получить старый отказ.

Никаких внешних брокеров (Celery/Redis отклонены): сервис stateless, потерю очереди при
рестарте компенсирует reaper go-core.
"""

from __future__ import annotations

import asyncio
import heapq
import itertools
import logging
import time
from collections import Counter, OrderedDict
from collections.abc import AsyncIterator, Awaitable, Callable
from contextlib import asynccontextmanager
from dataclasses import dataclass, field
from typing import Any

from ai_service.core.callback import CallbackDeliverer
from ai_service.core.errors import HttpError, JobError
from ai_service.core.logging import log_fields
from ai_service.gen import callback_models as cbm

log = logging.getLogger("ai_service.queue")

# Классы полосы LLM (меньше — важнее).
CLASS_DIALOG = 0
CLASS_EVALUATE = 1
CLASS_GENERATE = 2

JOB_TYPES = ("evaluate_grammar", "evaluate_semantic", "evaluate_dialogue", "generate_scenario", "tts")
JOB_LANE = {
    "evaluate_grammar": "lt",
    "evaluate_semantic": "llm",
    "evaluate_dialogue": "llm",
    "generate_scenario": "llm",
    "tts": "tts",
}
JOB_CLASS = {
    "evaluate_grammar": CLASS_EVALUATE,
    "evaluate_semantic": CLASS_EVALUATE,
    "evaluate_dialogue": CLASS_EVALUATE,
    "generate_scenario": CLASS_GENERATE,
    "tts": CLASS_EVALUATE,
}
# Поле результата AiResult по типу задачи (ровно одно — инвариант go-internal).
RESULT_FIELD = {
    "evaluate_grammar": "grammar",
    "evaluate_semantic": "semantic",
    "evaluate_dialogue": "dialogue",
    "generate_scenario": "scenario",
    "tts": "tts",
}
# Стартовые оценки длительности (с) до первых замеров — порядок величин CPU-стенда.
DEFAULT_DURATION_S = {
    "evaluate_grammar": 1.0,
    "evaluate_semantic": 25.0,
    "evaluate_dialogue": 40.0,
    "generate_scenario": 180.0,
    "tts": 2.0,
}


class _Waiter:
    __slots__ = ("cancelled", "cls", "future", "priority", "seq")

    def __init__(self, cls: int, priority: int, seq: int, future: asyncio.Future[None]):
        self.cls, self.priority, self.seq, self.future = cls, priority, seq, future
        self.cancelled = False

    def __lt__(self, other: _Waiter) -> bool:
        return (self.cls, self.priority, self.seq) < (other.cls, other.priority, other.seq)


class PriorityLane:
    """Полоса с ``slots`` параллельными исполнителями и приоритетной очередью ожидающих.

    Не asyncio.PriorityQueue + воркеры: синхронный ход диалога должен ждать слот в том же
    хендлере (студент на линии), а фоновые задачи — в своей корутине; обоим нужен общий
    упорядоченный «турникет», а не очередь сообщений.
    """

    def __init__(self, name: str, slots: int):
        self.name = name
        self.slots = slots
        self.running = 0
        self._heap: list[_Waiter] = []
        self._seq = itertools.count()
        self._waiting: Counter[int] = Counter()

    def waiting(self, cls: int | None = None) -> int:
        if cls is None:
            return sum(self._waiting.values())
        return self._waiting[cls]

    def ahead_of(self, cls: int, priority: int) -> int:
        """Сколько ожидающих будут обслужены раньше новой задачи (cls, priority)."""
        return sum(1 for w in self._heap if not w.cancelled and (w.cls, w.priority) <= (cls, priority))

    @asynccontextmanager
    async def slot(self, cls: int, priority: int = 5, timeout: float | None = None) -> AsyncIterator[float]:
        """Занять слот; отдаёт время ожидания (мс). TimeoutError — не дождались за timeout."""
        t0 = time.monotonic()
        await self._acquire(cls, priority, timeout)
        try:
            yield (time.monotonic() - t0) * 1000
        finally:
            self._release()

    async def _acquire(self, cls: int, priority: int, timeout: float | None) -> None:
        if self.running < self.slots and not self._heap:
            self.running += 1
            return
        fut: asyncio.Future[None] = asyncio.get_running_loop().create_future()
        w = _Waiter(cls, priority, next(self._seq), fut)
        heapq.heappush(self._heap, w)
        self._waiting[cls] += 1
        try:
            if timeout is None:
                await fut
            else:
                await asyncio.wait_for(asyncio.shield(fut), timeout)
        except BaseException:
            if fut.done() and not fut.cancelled():
                # Слот уже выдан, но ждущий ушёл (отмена/таймаут на границе) — вернуть.
                self._release()
            else:
                w.cancelled = True
                self._waiting[cls] -= 1
                fut.cancel()
            raise

    def _release(self) -> None:
        while self._heap:
            w = heapq.heappop(self._heap)
            if w.cancelled:
                continue
            self._waiting[w.cls] -= 1
            w.future.set_result(None)  # слот переходит к ожидающему: running не меняется
            return
        self.running -= 1


class Ema:
    """Скользящее среднее (≈ по 20 последним) — оценки ожидания для go-core и панели."""

    def __init__(self, initial: float, alpha: float = 0.1):
        self.value = initial
        self.alpha = alpha
        self.n = 0

    def observe(self, v: float) -> None:
        self.value = v if self.n == 0 else self.value * (1 - self.alpha) + v * self.alpha
        self.n += 1


@dataclass
class JobEntry:
    request_id: str
    job_type: str
    attempt_id: str | None
    priority: int
    status: str = "queued"  # queued | running | done
    submitted_at: float = field(default_factory=time.monotonic)
    done_at: float = 0.0
    result: dict[str, Any] | None = None
    task: asyncio.Task[None] | None = None


@dataclass
class JobOutcome:
    """Итог обработчика задачи: поле результата (dict по схеме контракта) + engine."""

    value: dict[str, Any]
    engine: dict[str, Any]


JobHandler = Callable[[Any], Awaitable[JobOutcome]]


class JobManager:
    def __init__(self, settings: Any, deliverer: CallbackDeliverer):
        self.settings = settings
        self.deliverer = deliverer
        self.lanes = {
            "llm": PriorityLane("llm", settings.llm_concurrency),
            "lt": PriorityLane("lt", settings.lt_concurrency),
            "tts": PriorityLane("tts", 1),
            "stt": PriorityLane("stt", 1),
        }
        self._jobs: OrderedDict[str, JobEntry] = OrderedDict()
        self._pending: Counter[str] = Counter()
        self.durations = {t: Ema(DEFAULT_DURATION_S[t]) for t in JOB_TYPES}
        self.dialog_ms = Ema(0.0)
        self.completed: Counter[str] = Counter()
        self._closing = False

    # ------------------------------------------------------------------ submit

    def submit(self, job_type: str, request_id: str, attempt_id: str | None, priority: int,
               payload: Any, handler: JobHandler) -> dict[str, Any]:
        """POST /v1/jobs/*: JobAccepted или HttpError(429)."""
        if self._closing:
            raise HttpError(503, "shutting_down", "ai-service останавливается, повторите позже", retry_after=5)
        self._evict()
        existing = self._jobs.get(request_id)
        if existing is not None:
            self._jobs.move_to_end(request_id)
            if existing.status == "done" and existing.result is not None:
                # Повтор выполненной: callback мог потеряться — отправляем снова.
                self.deliverer.enqueue(request_id, existing.result)
                return {"request_id": request_id, "status": "done", "queue_position": 0, "est_wait_sec": 0}
            if existing.status == "running":
                return {"request_id": request_id, "status": "running", "queue_position": 0,
                        "est_wait_sec": self._secs(self.durations[existing.job_type].value)}
            lane = self.lanes[JOB_LANE[existing.job_type]]
            pos = lane.ahead_of(JOB_CLASS[existing.job_type], existing.priority)
            return {"request_id": request_id, "status": "queued", "queue_position": pos,
                    "est_wait_sec": self.est_wait(existing.job_type, pos)}

        if self._pending[job_type] >= self.settings.queue_max(job_type):
            wait = min(max(self.est_wait(job_type), 1), 300)
            raise HttpError(429, "queue_full", "Очередь ai-service заполнена, повторите позже", retry_after=wait)

        lane = self.lanes[JOB_LANE[job_type]]
        pos = lane.ahead_of(JOB_CLASS[job_type], priority) + lane.running
        entry = JobEntry(request_id=request_id, job_type=job_type, attempt_id=attempt_id, priority=priority)
        self._jobs[request_id] = entry
        self._pending[job_type] += 1
        entry.task = asyncio.create_task(self._run(entry, payload, handler), name=f"job-{job_type}-{request_id}")
        return {"request_id": request_id, "status": "queued", "queue_position": pos,
                "est_wait_sec": self.est_wait(job_type, pos)}

    # ------------------------------------------------------------------ run

    async def _run(self, entry: JobEntry, payload: Any, handler: JobHandler) -> None:
        lane = self.lanes[JOB_LANE[entry.job_type]]
        base: dict[str, Any] = {"schema_version": "1", "request_id": entry.request_id, "type": entry.job_type}
        if entry.attempt_id:
            base["attempt_id"] = entry.attempt_id
        ok = False
        started = time.monotonic()
        wait_ms = 0
        try:
            async with lane.slot(JOB_CLASS[entry.job_type], entry.priority) as waited:
                wait_ms = int(waited)
                self._pending[entry.job_type] -= 1
                entry.status = "running"
                started = time.monotonic()
                try:
                    outcome = await handler(payload)
                    engine = dict(outcome.engine)
                    result = {**base, "status": "ok", RESULT_FIELD[entry.job_type]: outcome.value}
                    ok = True
                except JobError as e:
                    engine = dict(getattr(e, "engine", None) or {})
                    result = {**base, "status": "failed", "error": e.to_contract()}
                    log.warning("задача завершилась отказом", extra=log_fields(
                        request_id=entry.request_id, type=entry.job_type, code=e.code, error=e.message))
                except Exception as e:  # движки не должны ронять сервис: отказ internal
                    log.exception("ошибка обработки задачи", extra=log_fields(
                        request_id=entry.request_id, type=entry.job_type))
                    engine = {}
                    result = {**base, "status": "failed",
                              "error": JobError("internal", f"{type(e).__name__}: {e}").to_contract()}
        except asyncio.CancelledError:
            # Остановка сервиса: задачу забываем, go-core вернёт её reaper'ом.
            if entry.status == "queued":
                self._pending[entry.job_type] -= 1
            self._jobs.pop(entry.request_id, None)
            raise

        duration_ms = int((time.monotonic() - started) * 1000)
        engine["duration_ms"] = int(engine.get("duration_ms") or duration_ms)
        engine["queue_wait_ms"] = wait_ms
        result["engine"] = engine
        body = self._validate(result, entry)
        if body.get("status") == "ok" and ok:
            entry.status, entry.result, entry.done_at = "done", body, time.monotonic()
            self.durations[entry.job_type].observe(duration_ms / 1000)
        else:
            self._jobs.pop(entry.request_id, None)
        self.completed[f"{entry.job_type}:{body.get('status')}"] += 1
        log.info("задача выполнена", extra=log_fields(
            request_id=entry.request_id, attempt_id=entry.attempt_id, type=entry.job_type, status=body.get("status"),
            duration_ms=duration_ms, queue_wait_ms=wait_ms, model=engine.get("llm_model"),
            prompt=engine.get("prompt_version")))
        self.deliverer.enqueue(entry.request_id, body)

    def _validate(self, result: dict[str, Any], entry: JobEntry) -> dict[str, Any]:
        """Результат обязан соответствовать контракту: лучше честный failed/internal,
        чем callback, который go-core отвергнет 400 и задача зависнет до reaper'а."""
        try:
            return cbm.AiResult.model_validate(result).model_dump(mode="json", exclude_none=True)
        except Exception as e:
            log.error("результат не соответствует контракту", extra=log_fields(
                request_id=entry.request_id, type=entry.job_type, error=str(e)[:2000]))
            bad = {k: v for k, v in result.items() if k not in RESULT_FIELD.values()}
            err = JobError("internal", "результат не прошёл проверку контракта")
            bad.update(status="failed", error=err.to_contract())
            bad["engine"] = {"duration_ms": int(result.get("engine", {}).get("duration_ms", 0))}
            return cbm.AiResult.model_validate(bad).model_dump(mode="json", exclude_none=True)

    # ------------------------------------------------------------------ status

    def est_wait(self, job_type: str, ahead: int | None = None) -> int:
        lane = self.lanes[JOB_LANE[job_type]]
        if ahead is None:
            ahead = lane.ahead_of(JOB_CLASS[job_type], 5) + lane.running
        # Грубо: впереди стоящие задачи этой полосы — со средней длительностью своего типа
        # не различаем, берём тип новой задачи (для LLM-полосы это консервативно).
        per = self.durations[job_type].value
        return self._secs((ahead / max(lane.slots, 1) + 1) * per)

    @staticmethod
    def _secs(v: float) -> int:
        return max(int(v + 0.999), 0)

    def pending(self) -> dict[str, int]:
        return {t: max(self._pending[t], 0) for t in JOB_TYPES}

    def running(self) -> int:
        return sum(lane.running for lane in self.lanes.values())

    def dialog_waiting(self) -> int:
        return self.lanes["llm"].waiting(CLASS_DIALOG)

    def _evict(self) -> None:
        now = time.monotonic()
        ttl = self.settings.job_done_ttl_sec
        limit = self.settings.job_registry_size
        for rid in list(self._jobs):
            e = self._jobs[rid]
            if e.status == "done" and now - e.done_at > ttl:
                del self._jobs[rid]
        # LRU: вытесняем только выполненные — задачи в работе забывать нельзя.
        if len(self._jobs) > limit:
            for rid in list(self._jobs):
                if len(self._jobs) <= limit:
                    break
                if self._jobs[rid].status == "done":
                    del self._jobs[rid]

    async def shutdown(self, grace_sec: float = 30.0) -> None:
        """SIGTERM: новые задачи не принимаем, текущим даём grace_sec, остальные отменяем."""
        self._closing = True
        running = [e.task for e in self._jobs.values() if e.task and not e.task.done() and e.status == "running"]
        queued = [e.task for e in self._jobs.values() if e.task and not e.task.done() and e.status == "queued"]
        for t in queued:
            t.cancel()
        if running:
            _, still = await asyncio.wait(running, timeout=grace_sec)
            for t in still:
                t.cancel()
        await asyncio.gather(*(t for t in running + queued), return_exceptions=True)
