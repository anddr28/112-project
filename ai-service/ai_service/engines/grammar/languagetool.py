"""Клиент self-hosted LanguageTool (HTTP API v2)."""

from __future__ import annotations

from dataclasses import dataclass

import httpx

from ai_service.core.errors import JobError


@dataclass
class LtMatch:
    offset: int
    length: int
    rule: str
    issue_type: str
    category: str
    message: str
    suggestions: list[str]


class LanguageToolClient:
    def __init__(self, base_url: str, client: httpx.AsyncClient | None = None, timeout: float = 20.0):
        self.base_url = base_url.rstrip("/")
        self._client = client or httpx.AsyncClient(timeout=httpx.Timeout(timeout, connect=3.0))
        self.version: str | None = None

    async def aclose(self) -> None:
        await self._client.aclose()

    async def check(self, text: str, language: str = "ru-RU", disabled_rules: list[str] | None = None) -> list[LtMatch]:
        data = {"text": text, "language": language, "enabledOnly": "false"}
        if disabled_rules:
            data["disabledRules"] = ",".join(disabled_rules)
        try:
            resp = await self._client.post(f"{self.base_url}/v2/check", data=data)
        except httpx.HTTPError as e:
            raise JobError("lt_unavailable", f"LanguageTool недоступен: {type(e).__name__}") from e
        if resp.status_code >= 500:
            raise JobError("lt_unavailable", f"LanguageTool HTTP {resp.status_code}")
        if resp.status_code >= 400:
            # 400 от LT — обычно неизвестное правило в disabledRules или язык: это ошибка
            # конфигурации, повторять тот же запрос бессмысленно.
            raise JobError("internal", f"LanguageTool отклонил запрос: HTTP {resp.status_code} {resp.text[:200]}",
                           retryable=False)
        body = resp.json()
        self.version = (body.get("software") or {}).get("version") or self.version
        out = []
        for m in body.get("matches", []):
            rule = m.get("rule") or {}
            out.append(LtMatch(
                offset=int(m.get("offset", 0)),
                length=max(int(m.get("length", 1)), 1),
                rule=str(rule.get("id", "")),
                issue_type=str(rule.get("issueType", "")),
                category=str((rule.get("category") or {}).get("id", "")),
                message=str(m.get("message", "")),
                suggestions=[r.get("value", "") for r in (m.get("replacements") or [])[:3] if r.get("value")],
            ))
        return out

    async def ping(self) -> bool:
        resp = await self._client.get(f"{self.base_url}/v2/languages", timeout=3.0)
        return resp.status_code == 200
