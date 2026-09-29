"""Реестр промптов: prompts/<task>/v<N>.md -> шаблон с версией.

Промпты не хардкодятся в .py: QA/ML правят текст без кода, а версия файла едет в
``Engine.prompt_version`` ("caller-v1") — оценку можно воспроизвести. Формат файла:

    ---
    version: 1
    profile_default: dialog_fast
    ---
    === system ===
    ...текст с ${переменными}...
    === user ===
    ...

Подстановка — string.Template (``${name}``): фигурные скобки JSON-примеров в тексте
промпта не ломают шаблон.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from pathlib import Path
from string import Template
from typing import Any

import yaml

_SECTION = re.compile(r"^===\s*(\w+)\s*===\s*$", re.MULTILINE)


@dataclass(frozen=True)
class PromptTemplate:
    task: str
    version: int
    meta: dict[str, Any]
    sections: dict[str, str]

    @property
    def prompt_version(self) -> str:
        return f"{self.task}-v{self.version}"

    def render(self, section: str, **values: Any) -> str:
        text = self.sections.get(section, "")
        return Template(text).safe_substitute({k: "" if v is None else str(v) for k, v in values.items()}).strip()


def parse_prompt(task: str, text: str) -> PromptTemplate:
    meta: dict[str, Any] = {}
    body = text
    if text.startswith("---"):
        _, fm, body = text.split("---", 2)
        meta = yaml.safe_load(fm) or {}
    parts = _SECTION.split(body)
    sections: dict[str, str] = {}
    if len(parts) == 1:
        sections["system"] = body.strip()
    else:
        for i in range(1, len(parts) - 1, 2):
            sections[parts[i].strip().lower()] = parts[i + 1].strip()
    return PromptTemplate(task=task, version=int(meta.get("version", 1)), meta=meta, sections=sections)


class PromptRegistry:
    def __init__(self, root: Path, active: dict[str, int] | None = None):
        self.root = root
        # Активная версия по задаче; по умолчанию — старшая из лежащих в каталоге.
        self.active = active or {}
        self._cache: dict[tuple[str, int], PromptTemplate] = {}

    def versions(self, task: str) -> list[int]:
        out = []
        for p in (self.root / task).glob("v*.md"):
            m = re.fullmatch(r"v(\d+)\.md", p.name)
            if m:
                out.append(int(m.group(1)))
        return sorted(out)

    def get(self, task: str, version: int | None = None) -> PromptTemplate:
        if version is None:
            version = self.active.get(task)
        if version is None:
            vs = self.versions(task)
            if not vs:
                raise FileNotFoundError(f"нет промптов для задачи {task} в {self.root}")
            version = vs[-1]
        key = (task, version)
        if key not in self._cache:
            path = self.root / task / f"v{version}.md"
            self._cache[key] = parse_prompt(task, path.read_text(encoding="utf-8"))
        return self._cache[key]

    def fewshot(self, name: str) -> str:
        path = self.root / "_fewshot" / f"{name}.md"
        return path.read_text(encoding="utf-8").strip() if path.exists() else ""
