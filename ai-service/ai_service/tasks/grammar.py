"""Слой 2 — грамматика (LanguageTool) -> GrammarResult."""

from __future__ import annotations

from collections import Counter
from typing import TYPE_CHECKING, Any

from ai_service.core.queue import JobOutcome
from ai_service.gen import api_models as am
from ai_service.tasks import scoring
from ai_service.tasks.textutil import word_count

if TYPE_CHECKING:
    from ai_service.context import AppContext

# issueType LanguageTool -> severity контракта.
SEVERITY = {
    "misspelling": "error",
    "grammar": "error",
    "typographical": "warning",
    "punctuation": "warning",
    "whitespace": "warning",
    "duplication": "warning",
    "inconsistency": "warning",
    "style": "style",
    "register": "style",
    "locale-violation": "style",
    "uncategorized": "warning",
}


def severity_of(issue_type: str, category: str) -> str:
    if category.upper() == "PUNCTUATION":
        return "warning"
    return SEVERITY.get(issue_type.lower(), "warning")


def _is_domain_word(word: str, dictionary: set[str]) -> bool:
    """Орфографическое замечание по слову, которое в карточке 112 нормально: словарь
    домена, имена собственные (улицы, фамилии — с заглавной), слова с цифрами (14а, 5-й)."""
    w = word.strip(".,;:!?«»\"'()")
    if not w:
        return False
    if w.lower().replace("ё", "е") in dictionary:
        return True
    if any(ch.isdigit() for ch in w):
        return True
    return w[0].isupper()


async def run_grammar(ctx: AppContext, req: am.GrammarJobRequest) -> JobOutcome:
    opts = req.options or am.Options()
    lang = (opts.lang or "ru").strip() or "ru"
    lt_lang = "ru-RU" if lang.lower().startswith("ru") else lang
    disabled = list(dict.fromkeys([*ctx.lt_disabled_rules, *(opts.exclude_rules or [])]))
    strict = bool(opts.strict)

    remarks: list[dict[str, Any]] = []
    by_sev: Counter[str] = Counter()
    words = 0
    for item in req.texts:
        text = item.text or ""
        words += word_count(text)
        if not text.strip():
            continue
        for m in await ctx.lt.check(text, lt_lang, disabled):
            frag = text[m.offset : m.offset + m.length]
            if m.issue_type == "misspelling" and _is_domain_word(frag, ctx.lt_dictionary):
                continue
            sev = severity_of(m.issue_type, m.category)
            by_sev[sev] += 1
            remark: dict[str, Any] = {"field": item.field, "offset": m.offset, "length": m.length,
                                      "severity": sev, "message": m.message}
            if m.rule:
                remark["rule"] = m.rule
            if m.suggestions:
                remark["suggestions"] = m.suggestions
            remarks.append(remark)

    score = scoring.grammar_v1(words, by_sev["error"], by_sev["warning"], by_sev["style"], strict)
    value = {
        "score": score,
        "stats": {"words_checked": words,
                  "errors_by_severity": {k: by_sev.get(k, 0) for k in ("error", "warning", "style")}},
        "remarks": remarks,
    }
    engine = {"lt_version": f"languagetool-{ctx.lt.version or 'unknown'}", "rules_version": scoring.GRAMMAR_RULES}
    return JobOutcome(value=value, engine=engine)
