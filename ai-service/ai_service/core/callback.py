"""Доставка результатов в go-core: POST /internal/ai/v1/results (go-internal.v1.yaml).

Правила контракта: 3 повтора с backoff 1/5/30 с, затем результат отбрасывается (задачу
вернёт reaper go-core — осознанная цена stateless). go-core на валидный конверт всегда
отвечает 200 (дубль тоже), 400 — только на мусор: повторять 400 бессмысленно, это ошибка
в нашем коде, её видно в логе ``callback_rejected``.
"""

from __future__ import annotations

import asyncio
import json
import logging
from typing import Any

import httpx

from ai_service.core.logging import log_fields

log = logging.getLogger("ai_service.callback")


class CallbackDeliverer:
    def __init__(self, url: str, token: str, backoff: list[float], timeout: float = 10.0,
                 client: httpx.AsyncClient | None = None):
        self.url = url
        self.token = token
        self.backoff = backoff
        self._client = client or httpx.AsyncClient(timeout=timeout)
        self._own_client = client is None
        self._tasks: set[asyncio.Task[None]] = set()
        self.delivered = 0
        self.dropped = 0

    def enqueue(self, request_id: str, body: dict[str, Any]) -> None:
        # Отдельная корутина на доставку: лежащий go-core не блокирует остальные callback'и,
        # а ожидание backoff'а не держит слоты полос.
        t = asyncio.create_task(self._deliver(request_id, body), name=f"callback-{request_id}")
        self._tasks.add(t)
        t.add_done_callback(self._tasks.discard)

    async def _deliver(self, request_id: str, body: dict[str, Any]) -> None:
        data = json.dumps(body, ensure_ascii=False).encode()
        headers = {"Content-Type": "application/json", "X-Internal-Token": self.token}
        attempt = 0
        while True:
            status, err = 0, ""
            try:
                resp = await self._client.post(self.url, content=data, headers=headers)
                status = resp.status_code
                if status == 200:
                    self.delivered += 1
                    log.debug("callback доставлен", extra=log_fields(request_id=request_id, attempt=attempt + 1))
                    return
                if status == 400:
                    self.dropped += 1
                    log.error("callback_rejected: go-core отверг конверт", extra=log_fields(
                        request_id=request_id, status=status, body=resp.text[:500]))
                    return
                err = resp.text[:300]
            except httpx.HTTPError as e:
                err = f"{type(e).__name__}: {e}"
            if attempt >= len(self.backoff):
                self.dropped += 1
                log.warning("callback_dropped: go-core недоступен, результат отброшен", extra=log_fields(
                    request_id=request_id, status=status, error=err))
                return
            delay = self.backoff[attempt]
            attempt += 1
            log.warning("callback не доставлен, повтор", extra=log_fields(
                request_id=request_id, status=status, error=err, retry_in_s=delay))
            await asyncio.sleep(delay)

    async def drain(self, timeout: float = 5.0) -> None:
        """При остановке: дать уйти уже готовым callback'ам (без ожидания длинных backoff'ов)."""
        if self._tasks:
            _, pending = await asyncio.wait(set(self._tasks), timeout=timeout)
            for t in pending:
                t.cancel()
        if self._own_client:
            await self._client.aclose()
