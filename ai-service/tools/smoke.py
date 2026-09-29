#!/usr/bin/env python3
"""Сквозной прогон ai-service на реальных движках (главный инструмент QA). Только stdlib.

Нужен запущенный ai-service и приёмник callback'ов (tools/callback_sink.py, каталог ./sink):
    python tools/smoke.py --base http://localhost:8000 --sink sink

health -> tts sync -> dialog turn текстом -> jobs grammar/semantic/dialogue/generate -> ждёт
callback'и в sink -> таблица длительностей. Код возврата 0 — всё прошло.
"""

from __future__ import annotations

import argparse
import json
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

FIX = Path(__file__).resolve().parent.parent / "tests" / "fixtures"


def call(base: str, token: str, method: str, path: str, body: dict | None = None, timeout: float = 60):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(base + path, data=data, method=method,
                                 headers={"X-Internal-Token": token, "Content-Type": "application/json"})
    t0 = time.monotonic()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read() or b"null"), time.monotonic() - t0
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"null"), time.monotonic() - t0


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://localhost:8000")
    ap.add_argument("--token", default="dev-internal-token")
    ap.add_argument("--sink", default="sink")
    ap.add_argument("--timeout", type=float, default=600)
    a = ap.parse_args()
    rows: list[tuple[str, str, float]] = []

    st, h, dt = call(a.base, a.token, "GET", "/v1/health")
    print("health:", json.dumps(h, ensure_ascii=False))
    rows.append(("health", str(st), dt))

    tts_body = {"text": "Алло, помогите!", "text_hash": uuid.uuid4().hex}
    st, r, dt = call(a.base, a.token, "POST", "/v1/tts/sync", tts_body)
    rows.append(("tts/sync", f"{st} {r.get('file_path', r.get('code'))}", dt))

    turn = json.loads((FIX / "dialog_turn.json").read_text(encoding="utf-8"))
    turn["request_id"] = str(uuid.uuid4())
    st, r, dt = call(a.base, a.token, "POST", "/v1/dialog/turn", turn)
    rows.append(("dialog/turn", f"{st} fallback={r.get('fallback')} «{(r.get('caller') or {}).get('text', '')}»", dt))

    pending = {}
    for kind, fx in (("grammar", "grammar_job.json"), ("semantic", "semantic_job.json"),
                     ("dialogue", "dialogue_job.json"), ("generate", "generate_job.json")):
        body = json.loads((FIX / fx).read_text(encoding="utf-8"))
        body["request_id"] = str(uuid.uuid4())
        st, r, dt = call(a.base, a.token, "POST", f"/v1/jobs/{kind}", body)
        rows.append((f"jobs/{kind} (202)", str(st), dt))
        pending[body["request_id"]] = (kind, time.monotonic())

    sink = Path(a.sink)
    deadline = time.monotonic() + a.timeout
    ok = True
    while pending and time.monotonic() < deadline:
        for rid in list(pending):
            f = sink / f"{rid}.json"
            if f.exists():
                kind, t0 = pending.pop(rid)
                res = json.loads(f.read_text(encoding="utf-8"))
                field = {"grammar": "grammar", "semantic": "semantic", "dialogue": "dialogue",
                         "generate": "scenario"}[kind]
                score = (res.get(field) or {}).get("score", (res.get(field) or {}).get("title", ""))
                ok &= res["status"] == "ok"
                rows.append((f"callback {kind}", f"{res['status']} {score} {res.get('error', '')}",
                             time.monotonic() - t0))
        time.sleep(0.5)
    for kind, _ in pending.values():
        ok = False
        rows.append((f"callback {kind}", "TIMEOUT", a.timeout))

    print(f"\n{'шаг':<24} {'результат':<70} {'с':>7}")
    for name, res, dt in rows:
        print(f"{name:<24} {res[:70]:<70} {dt:7.2f}")
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
