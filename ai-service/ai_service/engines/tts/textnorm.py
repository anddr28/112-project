"""Нормализация текста перед Silero — обязательна (PY-08).

Silero читает только кириллицу словами: «д. 14» звучит как «дэ четырнадцать», латиница
и цифры — молчанием или мусором. Здесь: сокращения и аббревиатуры (config/tts_abbrev_ru.yaml),
телефоны группами, числа словами (num2words), латиница транслитом, чистка markdown/кавычек.
Падежи числительных не согласуем («на 5 этаже» -> «на пять этаже») — для заявителя в
панике это естественно, а морфология здесь дороже пользы.
"""

from __future__ import annotations

import re
from pathlib import Path

import yaml
from num2words import num2words

_LATIN = {
    "a": "а", "b": "б", "c": "к", "d": "д", "e": "е", "f": "ф", "g": "г", "h": "х", "i": "и", "j": "дж",
    "k": "к", "l": "л", "m": "м", "n": "н", "o": "о", "p": "п", "q": "к", "r": "р", "s": "с", "t": "т",
    "u": "у", "v": "в", "w": "в", "x": "кс", "y": "й", "z": "з",
}
_PHONE = re.compile(r"(?:\+7|8)[\s\-(]*\d{3}[\s\-)]*\d{3}[\s\-]*\d{2}[\s\-]*\d{2}")
_ORDINAL = re.compile(r"(\d+)-(й|я|е|го|му|м|х|ю|ое|ая|ый|ий|ой)\b", re.IGNORECASE)
_DECIMAL = re.compile(r"(\d+)[,.](\d+)")
_NUMBER = re.compile(r"\d+")
_MARKDOWN = re.compile(r"[*_#`~>\[\]{}|<>]")
_QUOTES = re.compile(r"[«»\"“”„'`]")
_SPACES = re.compile(r"\s+")

# Порядковое с наращением: окончание -> падеж/род для num2words не поддерживается,
# поэтому читаем именительный мужской («5-й» -> «пятый», «5-я» -> «пятая»).
_ORD_FEM = {"я", "ая"}
_ORD_NEUT = {"е", "ое"}


class TextNormalizer:
    def __init__(self, abbrev_path: Path | None = None):
        self.abbrev: list[tuple[re.Pattern[str], str]] = []
        self.acronyms: list[tuple[re.Pattern[str], str]] = []
        if abbrev_path and abbrev_path.exists():
            data = yaml.safe_load(abbrev_path.read_text(encoding="utf-8")) or {}
            # Длинные сокращения раньше коротких: «пр-т» не должен съесться «пр».
            for k, v in sorted((data.get("abbrev") or {}).items(), key=lambda kv: -len(kv[0])):
                pat = re.compile(r"(?<![\wА-Яа-яЁё])" + re.escape(k) + r"(?=[\s\d,;:!?)]|$)", re.IGNORECASE)
                self.abbrev.append((pat, v))
            for k, v in (data.get("acronyms") or {}).items():
                self.acronyms.append((re.compile(r"(?<![\wА-Яа-яЁё])" + re.escape(k) + r"(?![\wА-Яа-яЁё])"), v))

    def normalize(self, text: str) -> str:
        t = _MARKDOWN.sub(" ", text)
        t = _QUOTES.sub("", t)
        t = t.replace("—", ", ").replace("–", ", ")
        t = _PHONE.sub(lambda m: " " + phone_words(m.group(0)) + " ", t)
        for pat, rep in self.acronyms:
            t = pat.sub(rep, t)
        for pat, rep in self.abbrev:
            t = pat.sub(rep + " ", t)
        t = _ORDINAL.sub(_ordinal_words, t)
        t = _DECIMAL.sub(lambda m: _num(m.group(1)) + " запятая " + _num(m.group(2)), t)
        t = _NUMBER.sub(lambda m: " " + _num(m.group(0)) + " ", t)
        t = re.sub(r"[A-Za-z]+", lambda m: translit(m.group(0)), t)
        t = _SPACES.sub(" ", t).strip()
        t = re.sub(r"\s+([,.!?;:…])", r"\1", t)
        return t


def _num(digits: str) -> str:
    n = int(digits)
    if len(digits) > 1 and digits.startswith("0"):
        return " ".join(num2words(int(d), lang="ru") for d in digits)  # «007» — по цифрам
    if n > 999_999_999:
        return " ".join(num2words(int(d), lang="ru") for d in digits)
    return num2words(n, lang="ru")


def _ordinal_words(m: re.Match[str]) -> str:
    n = int(m.group(1))
    suffix = m.group(2).lower()
    word = num2words(n, lang="ru", to="ordinal")
    if suffix in _ORD_FEM and word.endswith(("ый", "ой", "ий")):
        word = word[:-2] + "ая"
    elif suffix in _ORD_NEUT and word.endswith(("ый", "ой", "ий")):
        word = word[:-2] + "ое"
    return word


def phone_words(phone: str) -> str:
    """+7 (916) 123-45-67 -> «плюс семь, девятьсот шестнадцать, сто двадцать три, сорок пять,
    шестьдесят семь» — так диктуют номер по телефону."""
    digits = re.sub(r"\D", "", phone)
    prefix = "плюс семь" if phone.strip().startswith("+") else "восемь"
    rest = digits[1:]
    groups = [rest[0:3], rest[3:6], rest[6:8], rest[8:10]]
    return ", ".join([prefix, *(_num(g) for g in groups if g)])


def translit(word: str) -> str:
    return "".join(_LATIN.get(ch.lower(), ch) for ch in word)


def split_sentences(text: str, max_chars: int = 800) -> list[str]:
    """Silero ограничивает длину одного вызова — режем по предложениям."""
    parts = re.split(r"(?<=[.!?…])\s+", text)
    out: list[str] = []
    cur = ""
    for p in parts:
        if len(cur) + len(p) + 1 <= max_chars:
            cur = (cur + " " + p).strip()
        else:
            if cur:
                out.append(cur)
            while len(p) > max_chars:
                out.append(p[:max_chars])
                p = p[max_chars:]
            cur = p
    if cur:
        out.append(cur)
    return out
