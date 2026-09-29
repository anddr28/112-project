"""Полоса LLM: приоритеты (диалог > оценка > генерация), таймаут ожидания, отмена."""

import asyncio

import pytest

from ai_service.core.queue import CLASS_DIALOG, CLASS_EVALUATE, CLASS_GENERATE, PriorityLane


async def test_priority_order():
    lane = PriorityLane("llm", 1)
    order: list[str] = []
    gate = asyncio.Event()

    async def worker(name: str, cls: int, prio: int = 5) -> None:
        async with lane.slot(cls, prio):
            order.append(name)
            if name == "first":
                await gate.wait()

    first = asyncio.create_task(worker("first", CLASS_GENERATE))
    await asyncio.sleep(0)
    tasks = [asyncio.create_task(worker(n, c, p)) for n, c, p in [
        ("gen", CLASS_GENERATE, 7), ("eval-7", CLASS_EVALUATE, 7), ("eval-3", CLASS_EVALUATE, 3),
        ("dialog", CLASS_DIALOG, 1)]]
    await asyncio.sleep(0.01)
    assert lane.waiting() == 4 and lane.waiting(CLASS_DIALOG) == 1
    gate.set()
    await asyncio.gather(first, *tasks)
    assert order == ["first", "dialog", "eval-3", "eval-7", "gen"]
    assert lane.running == 0 and lane.waiting() == 0


async def test_slot_timeout_and_cancel_do_not_leak():
    lane = PriorityLane("llm", 1)
    hold = asyncio.Event()

    async def holder() -> None:
        async with lane.slot(CLASS_GENERATE):
            await hold.wait()

    h = asyncio.create_task(holder())
    await asyncio.sleep(0)
    with pytest.raises(TimeoutError):
        async with lane.slot(CLASS_DIALOG, timeout=0.01):
            pass
    assert lane.waiting(CLASS_DIALOG) == 0

    waiter = asyncio.create_task(lane.slot(CLASS_EVALUATE).__aenter__())
    await asyncio.sleep(0.01)
    waiter.cancel()
    await asyncio.gather(waiter, return_exceptions=True)
    assert lane.waiting() == 0
    hold.set()
    await h
    assert lane.running == 0
    async with lane.slot(CLASS_EVALUATE):  # полоса снова свободна
        assert lane.running == 1


async def test_parallel_slots():
    lane = PriorityLane("lt", 2)
    active, peak = 0, 0

    async def w() -> None:
        nonlocal active, peak
        async with lane.slot(CLASS_EVALUATE):
            active += 1
            peak = max(peak, active)
            await asyncio.sleep(0.01)
            active -= 1

    await asyncio.gather(*(w() for _ in range(6)))
    assert peak == 2
