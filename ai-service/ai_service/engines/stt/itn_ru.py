"""ITN (inverse text normalization) для русской речи оператора: числительные -> цифры.

«дом четырнадцать подъезд три» -> «дом 14 подъезд 3», «сто двенадцать» -> «112»,
«пятый этаж» -> «5 этаж», телефон «восемь девятьсот шестнадцать ...» -> «8 916 ...».

Только правила (0–999 999 + порядковые + группы для телефонов), без ML: предсказуемо и
тестируемо. Решения, принятые осознанно:
* одиночное «один/одна» не трогаем («одна женщина», «одну минуту») — цифра там хуже слова;
* порядковые переводим только рядом с адресными словами («пятый этаж», «третий подъезд») или
  в составе числа («двадцать пятый»): «второй раз» остаётся словами;
* числа, которые нельзя сложить в одно («восемь девятьсот»), идут отдельными группами — так
  диктуют телефоны.
"""

from __future__ import annotations

import re

_UNITS = {
    "ноль": 0, "нуль": 0, "нуля": 0,
    "один": 1, "одна": 1, "одно": 1, "одну": 1, "одного": 1, "одной": 1, "одному": 1, "одним": 1, "одном": 1,
    "два": 2, "две": 2, "двух": 2, "двум": 2, "двумя": 2,
    "три": 3, "трех": 3, "трем": 3, "тремя": 3,
    "четыре": 4, "четырех": 4, "четырем": 4, "четырьмя": 4,
    "пять": 5, "пяти": 5, "пятью": 5, "шесть": 6, "шести": 6, "шестью": 6,
    "семь": 7, "семи": 7, "семью": 7, "восемь": 8, "восьми": 8, "восемью": 8,
    "девять": 9, "девяти": 9, "девятью": 9,
}
_TEENS = {
    "десять": 10, "одиннадцать": 11, "двенадцать": 12, "тринадцать": 13, "четырнадцать": 14,
    "пятнадцать": 15, "шестнадцать": 16, "семнадцать": 17, "восемнадцать": 18, "девятнадцать": 19,
}
_TEENS.update({k[:-1] + "и": v for k, v in list(_TEENS.items())})  # десяти, четырнадцати …
_TENS = {
    "двадцать": 20, "двадцати": 20, "тридцать": 30, "тридцати": 30, "сорок": 40, "сорока": 40,
    "пятьдесят": 50, "пятидесяти": 50, "шестьдесят": 60, "шестидесяти": 60,
    "семьдесят": 70, "семидесяти": 70, "восемьдесят": 80, "восьмидесяти": 80,
    "девяносто": 90, "девяноста": 90,
}
_HUNDREDS = {
    "сто": 100, "ста": 100, "двести": 200, "двухсот": 200, "триста": 300, "трехсот": 300,
    "четыреста": 400, "четырехсот": 400, "пятьсот": 500, "пятисот": 500, "шестьсот": 600, "шестисот": 600,
    "семьсот": 700, "семисот": 700, "восемьсот": 800, "восьмисот": 800, "девятьсот": 900, "девятисот": 900,
}
_THOUSAND = {"тысяча", "тысячи", "тысяч", "тысячу", "тысячей"}

_ORD_STEMS = {
    "перв": 1, "втор": 2, "трет": 3, "четверт": 4, "пят": 5, "шест": 6, "седьм": 7, "восьм": 8, "девят": 9,
    "десят": 10, "одиннадцат": 11, "двенадцат": 12, "тринадцат": 13, "четырнадцат": 14, "пятнадцат": 15,
    "шестнадцат": 16, "семнадцат": 17, "восемнадцат": 18, "девятнадцат": 19, "двадцат": 20, "тридцат": 30,
    "сороков": 40, "пятидесят": 50, "шестидесят": 60, "семидесят": 70, "восьмидесят": 80, "девяност": 90,
    "сот": 100,
}
_ORD_END = re.compile(
    r"^(ый|ой|ий|ая|яя|ое|ее|ые|ие|ого|его|ому|ему|ом|ем|ую|юю|ых|их|ым|им|ыми|ими|"
    r"ья|ье|ьи|ьего|ьему|ьем|ью|ьей|ьих|ьим)$"
)
# Порядковые переводим в цифры только рядом с этими словами (основы).
_ORD_CONTEXT = ("этаж", "подъезд", "корпус", "дом", "квартир", "строени", "лини", "микрорайон", "километр",
                "секци", "вход", "выход", "съезд", "парковк", "уровн", "блок", "участ", "павильон", "платформ")

_TOKEN = re.compile(r"[A-Za-zА-Яа-яЁё]+|\d+|\s+|[^\sA-Za-zА-Яа-яЁё\d]")

# Разряды: чем больше, тем «старше» слово внутри группы < 1000.
_U, _T1, _T10, _H = 1, 2, 3, 4


def _classify(word: str) -> tuple[str, int] | None:
    w = word.lower().replace("ё", "е")
    if w in _UNITS:
        return "u", _UNITS[w]
    if w in _TEENS:
        return "teen", _TEENS[w]
    if w in _TENS:
        return "ten", _TENS[w]
    if w in _HUNDREDS:
        return "hund", _HUNDREDS[w]
    if w in _THOUSAND:
        return "thou", 1000
    return None


def _ordinal(word: str) -> int | None:
    w = word.lower().replace("ё", "е")
    for stem in sorted(_ORD_STEMS, key=len, reverse=True):
        if w.startswith(stem) and _ORD_END.match(w[len(stem):]):
            return _ORD_STEMS[stem]
    return None


class _Num:
    """Накопитель одного числа: группа < 1000 + тысячи."""

    def __init__(self) -> None:
        self.thousands: int | None = None
        self.group = 0
        self.last = 0  # старший разряд последнего слова группы (_U/_T1/_T10/_H), 0 — пусто
        self.words = 0
        self.only_one = True  # число — одиночное «один» (не переводим)

    def accepts(self, kind: str) -> bool:
        if kind == "thou":
            return self.thousands is None and self.words > 0
        order = {"u": _U, "teen": _T1, "ten": _T10, "hund": _H}[kind]
        if self.last == 0:
            return True
        if self.last == _H:
            return order < _H
        if self.last == _T10:
            return order == _U
        return False

    def add(self, kind: str, value: int) -> None:
        self.words += 1
        if kind == "thou":
            self.thousands = self.group or 1
            self.group, self.last = 0, 0
            self.only_one = False
            return
        self.group += value
        self.last = {"u": _U, "teen": _T1, "ten": _T10, "hund": _H}[kind]
        if not (kind == "u" and value == 1 and self.words == 1):
            self.only_one = False

    def value(self) -> int:
        return (self.thousands or 0) * 1000 + self.group


def itn(text: str) -> str:
    toks = _TOKEN.findall(text)
    words_idx = [i for i, t in enumerate(toks) if t[0].isalpha()]
    out = list(toks)
    i = 0
    while i < len(words_idx):
        idx = words_idx[i]
        c = _classify(toks[idx])
        if c is None:
            ordv = _ordinal(toks[idx])
            if ordv is not None and _near_context(toks, words_idx, i):
                out[idx] = str(ordv)
            i += 1
            continue
        # Серия числительных, разделённых только пробелами/запятыми.
        run = [i]
        j = i + 1
        while j < len(words_idx) and _only_sep(toks, words_idx[j - 1], words_idx[j]) and (
                _classify(toks[words_idx[j]]) or _ordinal(toks[words_idx[j]]) is not None):
            run.append(j)
            j += 1
        numbers: list[tuple[_Num, list[int], int | None]] = []  # (число, индексы слов, порядковое)
        cur = _Num()
        cur_idx: list[int] = []
        ordinal_tail: int | None = None
        consumed = 0
        for k in run:
            w = toks[words_idx[k]]
            ck = _classify(w)
            if ck is None:
                ov = _ordinal(w)
                if ov is None:
                    break
                # Порядковое в хвосте числа: «двадцать пятый» -> 25 (+ разряд совместим).
                kind = "u" if ov < 10 else "teen" if ov < 20 else "ten" if ov < 100 else "hund"
                if cur.words and cur.accepts(kind):
                    cur.add(kind, ov)
                    cur_idx.append(k)
                    ordinal_tail = ov
                    consumed += 1
                break
            kind, v = ck
            if cur.words and not cur.accepts(kind):
                numbers.append((cur, cur_idx, None))
                cur, cur_idx = _Num(), []
            cur.add(kind, v)
            cur_idx.append(k)
            consumed += 1
        if cur.words:
            numbers.append((cur, cur_idx, ordinal_tail))
        for num, idxs, _ in numbers:
            if num.only_one and len(numbers) == 1 and not _near_context(toks, words_idx, idxs[0], after=False):
                continue  # одиночное «один/одна» оставляем словом (кроме «корпус один», «подъезд один»)
            first, last = words_idx[idxs[0]], words_idx[idxs[-1]]
            out[first] = str(num.value())
            for p in range(first + 1, last + 1):
                out[p] = ""
        i += max(consumed, 1)
    return re.sub(r"[ \t]{2,}", " ", "".join(out)).strip()


def _only_sep(toks: list[str], a: int, b: int) -> bool:
    between = "".join(toks[a + 1 : b])
    return between.strip(" ,-") == "" and len(between) <= 4


def _near_context(toks: list[str], words_idx: list[int], i: int, after: bool = True) -> bool:
    for j in (i - 1, i + 1) if after else (i - 1,):
        if 0 <= j < len(words_idx):
            w = toks[words_idx[j]].lower().replace("ё", "е")
            if any(w.startswith(s) for s in _ORD_CONTEXT):
                return True
    return False
