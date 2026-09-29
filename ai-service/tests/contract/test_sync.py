"""Синхронные эндпоинты: /v1/dialog/turn, /v1/stt/transcribe, /v1/tts/sync, /v1/health, /v1/queue."""

from __future__ import annotations

import json

import httpx
import pytest

from ai_service.tasks import voice as voice_mod
from tests.conftest import TOKEN, FakeStt, FakeTts, load_fixture
from tests.contract.oas import assert_valid

WAV = b"RIFF\x24\x00\x00\x00WAVEfmt " + b"\x00" * 200


@pytest.fixture
def no_decode(monkeypatch):
    """Декод аудио — часть голосовой сборки (PyAV); в юнит-тестах подменяем."""
    monkeypatch.setattr(voice_mod, "decode_audio", lambda data, max_sec: (b"", 1500))


async def turn(service, body: dict | None = None, **over) -> dict:
    body = body or load_fixture("dialog_turn.json")
    body.update(over)
    r = await service.post("/v1/dialog/turn", body)
    assert r.status_code == 200, r.text
    res = r.json()
    assert_valid(res, "ai-service.v1.yaml", "DialogTurnResult")
    return res


async def test_dialog_turn_text(service):
    res = await turn(service)
    assert res["operator"] == {"text": "Служба 112, слушаю. Какой подъезд и этаж?", "language": "ru",
                               "confidence": 1.0, "audio_duration_ms": 0, "no_speech": False}
    c = res["caller"]
    assert c["text"].startswith("Третий подъезд") and c["revealed_fact_ids"] == ["entrance"]
    assert c["should_end"] is False and "tts" not in c  # TTS нет в образе — ход без озвучки
    assert res["fallback"] is False
    assert res["engine"]["prompt_version"] == "caller-v1" and res["engine"]["llm_model"] == "test-llm:7b"
    sent = service.backend.llm_calls[0]
    assert sent["options"]["num_predict"] > 0 and sent["format"]["properties"]["reply"]
    system = sent["messages"][0]["content"]
    assert "[никогда] id=secret" in system and "[по вопросу] id=entrance" in system
    assert "причина пожара" in system


async def test_dialog_turn_filters_fact_ids_and_early_end(service):
    service.backend.llm_script["caller"] = [{
        "reply": "Ну всё, до свидания!", "revealed_fact_ids": ["entrance", "made_up", "secret", "entrance"],
        "is_unknown_answer": False, "should_end": True, "end_reason": "устала", "emotional_state": "паника"}]
    res = await turn(service, operator_text="Какой подъезд?")
    # never-факт в id — это утечка: повтор со строгой инструкцией (второй ответ — по умолчанию)
    assert len(service.backend.llm_calls) == 2
    assert "ВНИМАНИЕ" in service.backend.llm_calls[1]["messages"][0]["content"]
    assert res["caller"]["revealed_fact_ids"] == ["entrance"]


async def test_dialog_turn_early_goodbye_suppressed(service):
    service.backend.llm_script["caller"] = [{
        "reply": "Ладно, до свидания.", "revealed_fact_ids": ["made_up", "smoke", "smoke"],
        "is_unknown_answer": False, "should_end": True, "end_reason": "", "emotional_state": ""}]
    res = await turn(service, operator_text="Что у вас горит?")
    assert res["caller"]["should_end"] is False  # turn_no=1, оператор не прощался
    assert res["caller"]["revealed_fact_ids"] == ["smoke"]
    assert res["caller"]["emotional_state"] == "паника"  # из легенды


async def test_dialog_turn_end_after_dispatch(service):
    service.backend.llm_script["caller"] = [{
        "reply": "Спасибо, жду!", "revealed_fact_ids": [], "is_unknown_answer": False, "should_end": True,
        "end_reason": "", "emotional_state": "облегчение"}]
    res = await turn(service, operator_text="Помощь направлена, пожарные выехали.")
    assert res["caller"]["should_end"] is True and res["caller"]["end_reason"]


async def test_dialog_turn_never_leak_falls_back(service):
    leak = {"reply": "Сосед курит в постели, вот и загорелось!", "revealed_fact_ids": [],
            "is_unknown_answer": False, "should_end": False, "end_reason": "", "emotional_state": ""}
    service.backend.llm_script["caller"] = [leak, leak]
    res = await turn(service, operator_text="Почему загорелось?")
    assert res["fallback"] is True
    assert res["caller"]["text"] == "Третий подъезд, пятый этаж!"  # резервная реплика turns[1]
    assert res["caller"]["revealed_fact_ids"] == []


async def test_dialog_turn_llm_down_fallback(service):
    service.backend.ollama_up = False
    body = load_fixture("dialog_turn.json")
    body["history"].append({"turn_no": 2, "speaker": "operator", "text": "Слушаю"})
    body["history"].append({"turn_no": 3, "speaker": "caller", "text": "Третий подъезд, пятый этаж!"})
    res = await turn(service, body, turn_no=2)
    assert res["fallback"] is True
    assert res["caller"]["text"] == "Там сосед пожилой, не открывает!"  # по кругу: 2-я реплика заявителя


async def test_dialog_turn_max_turns(service):
    body = load_fixture("dialog_turn.json")
    body["call_script"]["dialogue"]["max_turns"] = 3
    res = await turn(service, body, turn_no=3)
    assert res["caller"]["should_end"] is True and res["caller"]["end_reason"] == "max_turns"


async def test_dialog_turn_with_tts(service, settings):
    service.ctx.tts = FakeTts()
    res = await turn(service)
    tts = res["caller"]["tts"]
    assert tts["file_path"] == "dialog/018f6b2a-7c1e-7f00-b000-000000000002/1.wav"
    assert (settings.tts_dir / tts["file_path"]).exists() and tts["duration_ms"] == 500
    assert len(tts["text_hash"]) == 64 and res["engine"]["tts_version"] == "fake-tts"


async def test_dialog_turn_busy_503(service, settings):
    settings.dialog_max_waiting = 0
    r = await service.post("/v1/dialog/turn", load_fixture("dialog_turn.json"))
    assert r.status_code == 503 and r.headers["Retry-After"] == "3"
    assert_valid(r.json(), "_components.yaml", "ApiError")


async def test_dialog_turn_audio_without_stt_503(service):
    body = load_fixture("dialog_turn.json")
    body.pop("operator_text")
    files = {"request": (None, json.dumps(body), "application/json"), "audio": ("turn.wav", WAV, "audio/wav")}
    r = await service.client.post("/v1/dialog/turn", files=files, headers={"X-Internal-Token": TOKEN})
    assert r.status_code == 503 and r.json()["code"] == "stt_unavailable"


async def test_dialog_turn_audio(service, no_decode):
    service.ctx.stt = FakeStt("какой подъезд и этаж")
    body = load_fixture("dialog_turn.json")
    body.pop("operator_text")
    files = {"audio": ("turn.webm", b"\x1a\x45\xdf\xa3" + b"\x00" * 300, "audio/webm"),
             "request": (None, json.dumps(body), "application/json")}
    r = await service.client.post("/v1/dialog/turn", files=files, headers={"X-Internal-Token": TOKEN})
    assert r.status_code == 200, r.text
    res = r.json()
    assert_valid(res, "ai-service.v1.yaml", "DialogTurnResult")
    assert res["operator"]["text"] == "какой подъезд и этаж" and res["operator"]["audio_duration_ms"] == 1500
    assert res["engine"]["stt_model"] == "fake-whisper:small-int8"


async def test_dialog_turn_no_speech(service, no_decode, settings):
    service.ctx.stt = FakeStt(no_speech=True)
    service.ctx.tts = FakeTts()
    body = load_fixture("dialog_turn.json")
    body.pop("operator_text")
    files = {"request": (None, json.dumps(body), "application/json"), "audio": ("a.wav", WAV, "audio/wav")}
    r = await service.client.post("/v1/dialog/turn", files=files, headers={"X-Internal-Token": TOKEN})
    res = r.json()
    assert_valid(res, "ai-service.v1.yaml", "DialogTurnResult")
    assert res["operator"]["no_speech"] is True and res["caller"]["text"] == "Алло? Вы меня слышите?"
    assert res["caller"]["tts"]["file_path"] == "common/allo_slyshite.wav"
    assert service.backend.llm_calls == []  # тишина — без LLM


async def test_dialog_turn_bad_requests(service):
    body = load_fixture("dialog_turn.json")
    body.pop("operator_text")
    r = await service.post("/v1/dialog/turn", body)  # ни текста, ни аудио
    assert r.status_code == 400
    r = await service.post("/v1/dialog/turn", {**load_fixture("dialog_turn.json"), "turn_no": 0})
    assert r.status_code == 400
    files = {"request": (None, json.dumps(load_fixture("dialog_turn.json")), "application/json"),
             "audio": ("a.flac", b"fLaC" + b"\x00" * 100, "audio/flac")}
    service.ctx.stt = FakeStt()
    body = load_fixture("dialog_turn.json")
    body.pop("operator_text")
    files["request"] = (None, json.dumps(body), "application/json")
    r = await service.client.post("/v1/dialog/turn", files=files, headers={"X-Internal-Token": TOKEN})
    assert r.status_code == 415


async def test_stt_transcribe(service, no_decode):
    service.ctx.stt = FakeStt("улица ленина дом четырнадцать подъезд три")
    files = {"audio": ("a.wav", WAV, "audio/wav"),
             "options": (None, json.dumps({"lang": "ru", "hints": ["Ленина"], "itn": True}), "application/json")}
    r = await service.client.post("/v1/stt/transcribe", files=files, headers={"X-Internal-Token": TOKEN})
    assert r.status_code == 200, r.text
    body = r.json()
    assert_valid(body, "ai-service.v1.yaml", "SttResponse")
    assert body["text"] == "улица ленина дом 14 подъезд 3"
    assert body["text_raw"] == "улица ленина дом четырнадцать подъезд три"


async def test_stt_limits(service, settings):
    service.ctx.stt = FakeStt()
    big = {"audio": ("a.wav", b"RIFF" + b"\x00" * (settings.stt_max_bytes + 10), "audio/wav")}
    r = await service.client.post("/v1/stt/transcribe", files=big, headers={"X-Internal-Token": TOKEN})
    assert r.status_code == 413
    r = await service.client.post("/v1/stt/transcribe", json={}, headers={"X-Internal-Token": TOKEN})
    assert r.status_code == 415


async def test_tts_sync(service, settings):
    body = {"text": "Алло, помогите!", "text_hash": "ab" * 32, "voice": "baya", "rate": 1.0}
    r = await service.post("/v1/tts/sync", body)
    assert r.status_code == 503 and "Retry-After" in r.headers  # без голоса — честный 503
    service.ctx.tts = FakeTts()
    r = await service.post("/v1/tts/sync", body)
    assert r.status_code == 200
    res = r.json()
    assert_valid(res, "_components.yaml", "TtsResult")
    assert res["file_path"] == f"cache/ab/{'ab' * 32}.wav"
    r2 = await service.post("/v1/tts/sync", body)  # из кэша
    assert r2.json() == res
    r = await service.post("/v1/tts/sync", {**body, "text_hash": "../x"})
    assert r.status_code == 400


async def test_health_and_queue(service):
    r = await service.get("/v1/health", token=None)  # health без токена
    assert r.status_code == 200
    h = r.json()
    assert_valid(h, "ai-service.v1.yaml", "Health")
    assert h["ollama"] and h["languagetool"] and not h["tts"] and not h["stt"] and h["status"] == "degraded"
    assert h["profiles"]["dialog_fast"] == "test-llm:7b" and h["models_available"] == ["test-llm:7b"]
    r = await service.get("/v1/queue")
    q = r.json()
    assert_valid(q, "ai-service.v1.yaml", "QueueStatus")
    assert set(q["pending"]) == {"evaluate_grammar", "evaluate_semantic", "evaluate_dialogue",
                                 "generate_scenario", "tts"}
    assert q["llm_loaded"] is True and q["dialog_waiting"] == 0


async def test_health_all_ok_with_voice(service):
    service.ctx.tts, service.ctx.stt = FakeTts(), FakeStt()
    h = (await service.get("/v1/health", token=None)).json()
    assert h["status"] == "ok" and h["profiles"]["stt_default"] == "fake-whisper:small-int8"


async def test_health_ollama_down(service):
    service.backend.ollama_up = False
    service.ctx.health.at = 0
    h = (await service.get("/v1/health", token=None)).json()
    assert h["ollama"] is False and h["status"] == "degraded" and h["models_available"] == []


async def test_unknown_route_is_api_error(service):
    r = await service.get("/v1/nope")
    assert r.status_code == 404
    assert_valid(r.json(), "_components.yaml", "ApiError")


async def test_ollama_invalid_json_repaired(service):
    service.backend.llm_script["caller"] = ["не json вовсе", {
        "reply": "Пятый этаж!", "revealed_fact_ids": [], "is_unknown_answer": False, "should_end": False,
        "end_reason": "", "emotional_state": ""}]
    res = await turn(service)
    assert res["caller"]["text"] == "Пятый этаж!" and res["fallback"] is False
    assert "не прошёл проверку" in service.backend.llm_calls[1]["messages"][-1]["content"]


async def test_ollama_timeout_falls_back(service):
    service.backend.llm_script["caller"] = [httpx.ReadTimeout("timeout")]
    res = await turn(service)
    assert res["fallback"] is True
