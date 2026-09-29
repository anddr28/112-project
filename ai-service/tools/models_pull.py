#!/usr/bin/env python3
"""Скачать веса голосовых движков в каталог моделей (один раз, с интернетом).

Вызывается на этапе сборки образа (WITH_VOICE=1): веса запекаются в образ, на стенде
интернет не нужен. Можно запускать и локально: python tools/models_pull.py ./models

    <out>/silero/v4_ru.pt            — Silero TTS v4 ru (torch.package)
    <out>/whisper/<size>/...         — faster-whisper (CTranslate2), по умолчанию small
"""

from __future__ import annotations

import argparse
import hashlib
import sys
import urllib.request
from pathlib import Path

# Официальный источник + зеркало на HuggingFace (models.silero.ai бывает недоступен из части
# сетей). Зеркало принимается только с совпадающим sha256 (LFS oid файла v4_ru.pt).
SILERO_URLS = [
    ("https://models.silero.ai/models/tts/ru/v4_ru.pt", None),
    ("https://huggingface.co/Derur/silero-models/resolve/main/tts/ru/ru_v4/v4_ru.pt",
     "896ab96347d5bd781ab97959d4fd6885620e5aab52405d3445626eb7c1414b00"),
]


def _download(url: str, dst: Path, timeout: float = 30) -> None:
    tmp = dst.with_suffix(".part")
    with urllib.request.urlopen(url, timeout=timeout) as r, tmp.open("wb") as f:
        while chunk := r.read(1 << 20):
            f.write(chunk)
    tmp.rename(dst)


def pull_silero(out: Path) -> None:
    dst = out / "silero" / "v4_ru.pt"
    if dst.exists() and dst.stat().st_size > 10_000_000:
        print(f"silero: уже есть {dst}")
        return
    dst.parent.mkdir(parents=True, exist_ok=True)
    errors = []
    for url, sha in SILERO_URLS:
        print(f"silero: {url}")
        try:
            _download(url, dst)
        except OSError as e:
            errors.append(f"{url}: {e}")
            continue
        got = hashlib.sha256(dst.read_bytes()).hexdigest()
        if sha and got != sha:
            dst.unlink()
            errors.append(f"{url}: sha256 {got} != {sha}")
            continue
        print(f"silero: {dst} ({dst.stat().st_size // 1024} КБ, sha256 {got[:16]}…)")
        return
    raise SystemExit("silero: не удалось скачать веса:\n  " + "\n  ".join(errors))


def pull_whisper(out: Path, size: str) -> None:
    from faster_whisper.utils import download_model

    dst = out / "whisper" / size
    if (dst / "model.bin").exists():
        print(f"whisper: уже есть {dst}")
        return
    dst.mkdir(parents=True, exist_ok=True)
    path = download_model(size, output_dir=str(dst))
    print(f"whisper: {path}")


def verify(out: Path, size: str) -> None:
    """Проверка, что веса грузятся офлайн — ловим битую загрузку на сборке, а не на стенде."""
    import torch
    from faster_whisper import WhisperModel

    imp = torch.package.PackageImporter(str(out / "silero" / "v4_ru.pt"))
    model = imp.load_pickle("tts_models", "model")
    audio = model.apply_tts(text="проверка", speaker="baya", sample_rate=24000)
    assert audio.numel() > 1000, "silero вернул пустое аудио"
    WhisperModel(str(out / "whisper" / size), device="cpu", compute_type="int8")
    print("проверка весов: ok")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("out", nargs="?", default="/models")
    ap.add_argument("--whisper", default="small")
    ap.add_argument("--verify", action="store_true")
    a = ap.parse_args()
    out = Path(a.out)
    pull_silero(out)
    pull_whisper(out, a.whisper)
    if a.verify:
        verify(out, a.whisper)
    return 0


if __name__ == "__main__":
    sys.exit(main())
