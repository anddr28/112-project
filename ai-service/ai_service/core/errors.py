"""Ошибки сервиса.

Два вида, и путать их нельзя:
* ``JobError`` — отказ движка при выполнении задачи; уходит в callback как ``AiJobError``
  (коды и retryable — таблица в go-internal.v1.yaml);
* ``HttpError`` — ответ HTTP-эндпоинта (400/401/413/415/429/503) в форме ``ApiError``.
"""

from __future__ import annotations

from typing import Any

# Коды AiJobError по контракту и их «ретраибельность» по умолчанию.
JOB_ERROR_RETRYABLE: dict[str, bool] = {
    "llm_timeout": True,
    "llm_invalid_output": True,
    "model_unavailable": True,
    "lt_unavailable": True,
    "tts_failed": True,
    "stt_failed": True,
    "bad_payload": False,
    "internal": True,
}


class JobError(Exception):
    """Отказ движка/задачи -> callback status=failed."""

    def __init__(self, code: str, message: str, retryable: bool | None = None, details: dict[str, Any] | None = None):
        if code not in JOB_ERROR_RETRYABLE:
            code = "internal"
        super().__init__(f"{code}: {message}")
        self.code = code
        self.message = message
        self.retryable = JOB_ERROR_RETRYABLE[code] if retryable is None else retryable
        self.details = details
        # engine заполняется всегда, в т.ч. при failed (контракт): обработчик кладёт сюда
        # модель/версию промпта, с которыми не получилось.
        self.engine: dict[str, Any] | None = None

    def to_contract(self) -> dict[str, Any]:
        out: dict[str, Any] = {"code": self.code, "message": self.message[:1000], "retryable": self.retryable}
        if self.details:
            out["details"] = self.details
        return out


class HttpError(Exception):
    """Ошибка HTTP-ответа в форме ApiError (+ Retry-After для 429/503)."""

    def __init__(self, status: int, code: str, message: str, retry_after: int | None = None):
        super().__init__(f"{status} {code}: {message}")
        self.status = status
        self.code = code
        self.message = message
        self.retry_after = retry_after


def bad_payload(message: str) -> HttpError:
    return HttpError(400, "bad_payload", message)


def service_busy(message: str, retry_after: int) -> HttpError:
    return HttpError(503, "service_busy", message, retry_after=retry_after)
