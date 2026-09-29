"""Проверка JSON по схемам OpenAPI-спек contracts/openapi (независимо от сгенерированных моделей).

Сгенерированные Pydantic-модели — это наш же код; контрактный тест должен сверять ответ
с первоисточником. Спеки OpenAPI 3.0 грузятся как JSON Schema (draft 7 совместим по
используемому подмножеству), межфайловые $ref (_components.yaml#/...) резолвятся реестром.
"""

from __future__ import annotations

from functools import cache
from pathlib import Path
from typing import Any

import yaml
from jsonschema import Draft7Validator, FormatChecker
from referencing import Registry, Resource
from referencing.jsonschema import DRAFT7

SPECS = Path(__file__).resolve().parents[3] / "contracts" / "openapi"
BASE = "file:///contracts/"


@cache
def registry() -> Registry:
    resources = []
    for name in ("_components.yaml", "ai-service.v1.yaml", "go-internal.v1.yaml"):
        doc = yaml.safe_load((SPECS / name).read_text(encoding="utf-8"))
        resources.append((BASE + name, Resource.from_contents(doc, default_specification=DRAFT7)))
    return Registry().with_resources(resources)


def errors(instance: Any, spec: str, schema: str) -> list[str]:
    ref = f"{BASE}{spec}#/components/schemas/{schema}"
    v = Draft7Validator({"$ref": ref}, registry=registry(), format_checker=FormatChecker())
    return [f"{'/'.join(str(p) for p in e.absolute_path)}: {e.message}" for e in v.iter_errors(instance)]


def assert_valid(instance: Any, spec: str, schema: str) -> None:
    errs = errors(instance, spec, schema)
    assert not errs, f"{schema} не соответствует {spec}:\n" + "\n".join(errs[:20])


def assert_ai_result(body: dict[str, Any]) -> None:
    """AiResult по go-internal.v1.yaml + инвариант конверта: ok — ровно одно поле результата
    нужного типа, failed — поле error (так же проверяет go-core, validateEnvelope)."""
    assert_valid(body, "go-internal.v1.yaml", "AiResult")
    fields = [f for f in ("grammar", "semantic", "dialogue", "scenario", "tts") if f in body]
    expected = {"evaluate_grammar": "grammar", "evaluate_semantic": "semantic", "evaluate_dialogue": "dialogue",
                "generate_scenario": "scenario", "tts": "tts"}[body["type"]]
    if body["status"] == "ok":
        assert fields == [expected], f"status=ok: ожидалось ровно поле {expected}, есть {fields}"
        assert "error" not in body
    else:
        assert fields == [], f"status=failed не должен нести результат: {fields}"
        assert body["error"]["code"]
    assert "duration_ms" in body["engine"]
