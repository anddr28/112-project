from pathlib import Path

import pytest

from ai_service.engines.tts.store import tts_hash, valid_hash
from ai_service.engines.tts.textnorm import TextNormalizer, phone_words, split_sentences

N = TextNormalizer(Path(__file__).resolve().parents[2] / "config" / "tts_abbrev_ru.yaml")


@pytest.mark.parametrize(("src", "want"), [
    ("ул. Ленина, д. 14, кв. 7", "улица Ленина, дом четырнадцать, квартира семь"),
    ("Звоните 112", "Звоните сто двенадцать"),
    ("ДТП на МКАД", "дэ тэ пэ на мкад"),
    ("5-й этаж", "пятый этаж"),
    ("**Дым** «из окна»", "Дым из окна"),
])
def test_normalize(src, want):
    assert N.normalize(src) == want


def test_no_latin_or_digits_left():
    out = N.normalize("WiFi 2,5 +7 (916) 123-45-67 корп. 2")
    assert not any(ch.isdigit() or "a" <= ch.lower() <= "z" for ch in out)


def test_phone_words():
    assert phone_words("+7 (916) 123-45-67") == (
        "плюс семь, девятьсот шестнадцать, сто двадцать три, сорок пять, шестьдесят семь")


def test_split_sentences():
    parts = split_sentences("Раз. " * 400, max_chars=100)
    assert all(len(p) <= 100 for p in parts) and len(parts) > 10


def test_tts_hash_matches_go_core_format():
    # convert.TTSHash: sha256(text|voice|rate с 2 знаками)
    import hashlib
    assert tts_hash("Алло", "baya", 1.0) == hashlib.sha256("Алло|baya|1.00".encode()).hexdigest()
    assert valid_hash("a" * 64) and not valid_hash("../etc/passwd") and not valid_hash("short")
