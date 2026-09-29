#!/usr/bin/env python3
"""Заглушка go-core для локальной разработки: принимает callback'и ai-service и складывает их
в ./sink/<request_id>.json. Только stdlib.

    python tools/callback_sink.py --port 8080 --token dev-internal-token
    CALLBACK_URL=http://localhost:8080/internal/ai/v1/results uvicorn ai_service.main:app --port 8000
"""

from __future__ import annotations

import argparse
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=8080)
    ap.add_argument("--token", default="dev-internal-token")
    ap.add_argument("--out", default="sink")
    a = ap.parse_args()
    out = Path(a.out)
    out.mkdir(exist_ok=True)

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self) -> None:
            if self.headers.get("X-Internal-Token") != a.token:
                return self._reply(401, {"code": "unauthorized", "message": "bad token"})
            body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
            rid = body.get("request_id", "unknown")
            (out / f"{rid}.json").write_text(json.dumps(body, ensure_ascii=False, indent=2), encoding="utf-8")
            eng = body.get("engine", {})
            print(f"{body.get('type')} {rid} {body.get('status')} {eng.get('duration_ms')} мс "
                  f"{(body.get('error') or {}).get('code', '')}", flush=True)
            return self._reply(200, {"accepted": True, "duplicate": False})

        def _reply(self, code: int, obj: dict) -> None:
            data = json.dumps(obj).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def log_message(self, *args: object) -> None:
            pass

    print(f"callback sink: http://0.0.0.0:{a.port}/internal/ai/v1/results -> {out}/")
    ThreadingHTTPServer(("0.0.0.0", a.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
