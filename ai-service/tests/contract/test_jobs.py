"""Асинхронные задачи: 202 JobAccepted -> callback AiResult по go-internal.v1.yaml."""

from __future__ import annotations

import copy
import json

import httpx

from tests.conftest import CALLBACK_URL, TOKEN, load_fixture
from tests.contract.oas import assert_ai_result, assert_valid


async def submit(service, kind: str, body: dict) -> dict:
    r = await service.post(f"/v1/jobs/{kind}", body)
    assert r.status_code == 202, r.text
    accepted = r.json()
    assert_valid(accepted, "ai-service.v1.yaml", "JobAccepted")
    assert accepted["request_id"] == body["request_id"]
    return accepted


async def test_grammar_job(service):
    service.backend.lt_matches = lambda text: [
        {"offset": text.find("из под"), "length": 6, "message": "Пишется через дефис: «из-под»",
         "replacements": [{"value": "из-под"}], "rule": {"id": "RU_COMPOUNDS", "issueType": "misspelling",
                                                          "category": {"id": "TYPOS"}}},
        # Имя собственное (улица) — не ошибка: отфильтровывается словарём домена/заглавной буквой.
        {"offset": 0, "length": 3, "message": "Возможно, опечатка", "replacements": [],
         "rule": {"id": "MORFOLOGIK_RULE_RU_RU", "issueType": "misspelling", "category": {"id": "TYPOS"}}},
    ] if "из под" in text else []
    body = load_fixture("grammar_job.json")
    await submit(service, "grammar", body)
    [cb] = await service.backend.wait_callbacks(1)
    assert_ai_result(cb)
    assert cb["status"] == "ok" and cb["type"] == "evaluate_grammar"
    assert cb["attempt_id"] == body["attempt_id"]
    g = cb["grammar"]
    assert [r["rule"] for r in g["remarks"]] == ["RU_COMPOUNDS"]
    assert g["remarks"][0]["severity"] == "error" and g["remarks"][0]["field"] == "description"
    assert g["stats"]["errors_by_severity"]["error"] == 1
    assert 0 < g["score"] < 100
    assert cb["engine"]["rules_version"] == "gr-v1" and cb["engine"]["lt_version"] == "languagetool-6.5-test"
    # шумные правила отключены всегда
    assert "UPPERCASE_SENTENCE_START" in service.backend.lt_calls[0]["disabledRules"]
    assert service.backend.callback_headers[0]["x-internal-token"] == TOKEN


async def test_semantic_job(service):
    body = load_fixture("semantic_job.json")
    await submit(service, "semantic", body)
    [cb] = await service.backend.wait_callbacks(1)
    assert_ai_result(cb)
    s = cb["semantic"]
    # present, present, partial -> 100·2.5/3 = 83.3
    assert s["score"] == 83.3
    assert s["missing_facts"] == [] and s["extra_facts"] == []
    assert s["per_field"][0]["field"] == "description"
    assert 0 <= s["confidence"] <= 1
    assert cb["engine"]["prompt_version"] == "semantic-v1" and cb["engine"]["rules_version"] == "sem-v1"
    assert cb["engine"]["llm_model"] == "test-llm:7b"
    # в промпт ушли факты эталона с id, а не балл
    prompt = service.backend.llm_calls[0]["messages"][1]["content"]
    assert "f1: задымление" in prompt and "x1: открытое пламя" in prompt


async def test_semantic_forbidden_needs_evidence(service):
    service.backend.llm_script["semantic"] = [{
        "facts": [{"id": "f1", "status": "present", "evidence": "дым из кв"},
                  {"id": "f2", "status": "missing", "evidence": ""},
                  {"id": "f3", "status": "missing", "evidence": ""}],
        # первая цитата есть в ответе, вторая — выдумана моделью
        "forbidden_found": [{"id": "x1", "evidence": "дым из кв на 5 этаже"}, {"id": "x9", "evidence": "пламя"}],
        "coherence": "weak", "per_field": [], "summary_for_student": "", "self_confidence": 0.5}]
    await submit(service, "semantic", load_fixture("semantic_job.json"))
    [cb] = await service.backend.wait_callbacks(1)
    s = cb["semantic"]
    assert s["missing_facts"] == ["5 этаж", "возможен человек в квартире"]
    assert s["extra_facts"] == ["открытое пламя"]
    # 100/3 − 15 − 10 = 8.3
    assert s["score"] == 8.3
    assert s["summary_for_student"]


async def test_dialogue_job(service):
    body = load_fixture("dialogue_job.json")
    await submit(service, "dialogue", body)
    [cb] = await service.backend.wait_callbacks(1)
    assert_ai_result(cb)
    d = cb["dialogue"]
    assert [c["id"] for c in d["checklist"]] == ["ask_address", "ask_people", "ask_phone", "say_dispatched", "calm"]
    assert d["checklist"][0]["evidence_turn_no"] == 2
    assert "evidence_turn_no" not in d["checklist"][4]  # 0 — не реплика оператора
    assert d["missing_questions"] == ["Спросил, есть ли люди в квартире"]
    # обязательные: address 1 + people 0 + dispatched 1 + calm 1 = 3/4 -> 75; необязательный phone missed -> +0;
    # средняя пауза ответа 4.75 с > 4 с -> −5
    assert d["score"] == 70.0
    sp = d["speech"]
    assert sp["operator_turns"] == 2 and sp["operator_words"] == 13
    assert sp["avg_response_ms"] == 4750  # (4100−0) и (15200−9800): у реплик заявителя нет длительности озвучки
    assert sp["max_response_ms"] == 5400 and sp["words_per_min"] == 130.0
    assert d["tone"]["politeness"] == 90
    assert cb["engine"]["rules_version"] == "dlg-v1"


async def test_dialogue_forbidden_phrase_detected_without_llm(service):
    body = load_fixture("dialogue_job.json")
    body["transcript"][3]["text"] = "Ждите, перезвоните позже."
    await submit(service, "dialogue", body)
    [cb] = await service.backend.wait_callbacks(1)
    hits = cb["dialogue"]["forbidden_hits"]
    assert {(h["phrase"], h["turn_no"]) for h in hits} == {("перезвоните позже", 4), ("ждите", 4)}


async def test_generate_job(service):
    draft = {
        "title": "Пожар в квартире многоквартирного дома",  # совпадает с avoid_titles
        "caller": {"name": "Галина Петровна", "phone": "89161234567", "role": "соседка",
                   "emotional_state": "взволнована"},
        "address": {"city": "Москва", "street": "Профсоюзная улица", "house": "45", "entrance": "2", "floor": "5",
                    "apartment": "57", "landmark": ""},
        "persona": "Пожилая соседка, 68 лет", "speaking_style": "сбивается, переспрашивает",
        "opening": "Алло! Помогите, у соседей дым!",
        "fallback_lines": ["Пятый этаж, квартира пятьдесят семь!"],
        "facts": [{"id": "Smoke!", "text": "Из-под двери квартиры 57 идёт дым", "reveal": "volunteer", "hints": []},
                  {"id": "floor", "text": "Пятый этаж, второй подъезд", "reveal": "on_request", "hints": ["этаж"]},
                  {"id": "floor", "text": "В квартире живёт пожилой мужчина", "reveal": "on_request",
                   "hints": ["люди"]},
                  {"id": "smoking", "text": "Сосед курит в постели", "reveal": "never", "hints": []}],
        "unknowns": ["причина пожара"],
        "description": "Задымление в квартире 57 на 5 этаже, внутри может быть пожилой мужчина.",
        "actions_taken": "Направлены 101 и 103.",
        "casualties": {"injured": 0, "dead": 0, "trapped": 1},
        "services": ["101", "103", "999"],
        "checklist": [{"id": "where", "text": "Уточнил адрес и этаж", "kind": "question", "required": True,
                       "weight": 2, "hints": ["адрес"]},
                      {"id": "people", "text": "Спросил о людях", "kind": "question", "required": True,
                       "weight": 1, "hints": []}],
        "forbidden_phrases": [], "expected_actions": [], "notes_for_teacher": "Проверьте этаж.",
    }
    service.backend.llm_script["generate"] = [draft]
    body = load_fixture("generate_job.json")
    await submit(service, "generate", body)
    [cb] = await service.backend.wait_callbacks(1)
    assert_ai_result(cb)
    sc = cb["scenario"]
    assert "attempt_id" not in cb
    assert sc["title"] == "Пожар в квартире многоквартирного дома (вариант 2)"
    cs, card = sc["call_script"], sc["etalon_card"]
    assert card["category_code"] == "fire_residential"
    assert card["services_to_notify"] == ["101", "103"]  # 999 нет в классификаторе
    assert card["address"]["street"] == cs["address"]["street"] == "Профсоюзная улица"
    assert card["address"]["house"] == cs["address"]["house"] == "45"
    assert card["applicant"]["phone"] == cs["caller"]["phone"] == "+7 (916) 123-45-67"
    ids = [f["id"] for f in cs["dialogue"]["facts"]]
    assert len(ids) == len(set(ids)) and "smoke" in ids
    # сложность 2: ловушка never переведена в on_request
    assert all(f["reveal"] != "never" for f in cs["dialogue"]["facts"])
    assert cs["key_facts"] == [f["text"] for f in cs["dialogue"]["facts"]]
    checklist_ids = [c["id"] for c in sc["expected_dialogue"]["checklist"]]
    assert "ask_address" in checklist_ids and "say_help_dispatched" in checklist_ids
    assert sc["expected_dialogue"]["forbidden"]
    assert len(cs["turns"]) >= 4 and cs["turns"][0]["text"] == "Алло! Помогите, у соседей дым!"
    assert sc["expected_actions"][0]["action_text"]  # режим both — действия построены
    assert "999" in sc["notes_for_teacher"] and "пожилая соседка" in sc["notes_for_teacher"]
    # зерно генерации — из request_id (повтор задачи = тот же сценарий)
    assert isinstance(service.backend.llm_calls[0]["options"]["seed"], int)


async def test_generate_invalid_twice_fails(service):
    empty = {"title": "x", "caller": {"name": "a"}, "address": {"street": "", "house": ""}, "persona": "p",
             "opening": "", "facts": [], "description": ""}
    service.backend.llm_script["generate"] = [empty, empty]
    await submit(service, "generate", load_fixture("generate_job.json"))
    [cb] = await service.backend.wait_callbacks(1)
    assert_ai_result(cb)
    assert cb["status"] == "failed" and cb["error"]["code"] == "llm_invalid_output" and cb["error"]["retryable"]
    assert cb["engine"]["llm_model"] == "test-llm:7b"
    # второй запрос — с перечнем ошибок
    assert "не прошёл проверку" in service.backend.llm_calls[1]["messages"][-1]["content"]


async def test_tts_job_without_voice_fails_retryable(service):
    await submit(service, "tts", load_fixture("tts_job.json"))
    [cb] = await service.backend.wait_callbacks(1)
    assert_ai_result(cb)
    assert cb["status"] == "failed" and cb["error"]["code"] == "tts_failed" and cb["error"]["retryable"]


async def test_tts_job_with_engine(service, settings):
    from tests.conftest import FakeTts

    service.ctx.tts = FakeTts()
    body = load_fixture("tts_job.json")
    await submit(service, "tts", body)
    [cb] = await service.backend.wait_callbacks(1)
    assert_ai_result(cb)
    t = cb["tts"]
    assert t["file_path"] == f"cache/{body['text_hash'][:2]}/{body['text_hash']}.wav"
    assert t["duration_ms"] == 500 and t["voice"] == "baya"
    assert (settings.tts_dir / t["file_path"]).read_bytes()[:4] == b"RIFF"


async def test_ollama_down_gives_retryable_failure(service):
    service.backend.ollama_up = False
    await submit(service, "semantic", load_fixture("semantic_job.json"))
    [cb] = await service.backend.wait_callbacks(1)
    assert_ai_result(cb)
    assert cb["error"]["code"] == "model_unavailable" and cb["error"]["retryable"] is True
    assert cb["engine"]["llm_model"] == "test-llm:7b" and cb["engine"]["prompt_version"] == "semantic-v1"


async def test_lt_down_gives_lt_unavailable(service):
    service.backend.lt_up = False
    await submit(service, "grammar", load_fixture("grammar_job.json"))
    [cb] = await service.backend.wait_callbacks(1)
    assert cb["status"] == "failed" and cb["error"]["code"] == "lt_unavailable"


async def test_idempotent_resubmit(service):
    body = load_fixture("semantic_job.json")
    await submit(service, "semantic", body)
    again = await submit(service, "semantic", body)  # в очереди/в работе — без дубля
    assert again["status"] in ("queued", "running")
    await service.backend.wait_callbacks(1)
    done = await submit(service, "semantic", body)  # выполнена — callback уходит повторно
    assert done["status"] == "done"
    cbs = await service.backend.wait_callbacks(2)
    assert len(service.backend.llm_calls) == 1  # LLM звали один раз
    assert cbs[0] == cbs[1]


async def test_failed_result_not_cached(service):
    service.backend.ollama_up = False
    body = load_fixture("semantic_job.json")
    await submit(service, "semantic", body)
    await service.backend.wait_callbacks(1)
    service.backend.ollama_up = True
    again = await submit(service, "semantic", body)
    assert again["status"] == "queued"  # тот же request_id после отказа — считаем заново
    cbs = await service.backend.wait_callbacks(2)
    assert cbs[1]["status"] == "ok"


async def test_queue_full_429(service, settings):
    settings.queue_max_generate = 1
    service.backend.llm_default["generate"] = lambda body: httpx.Response(500)  # держим задачу в работе
    lane = service.ctx.jobs.lanes["llm"]
    lane.running = lane.slots  # полоса занята — задачи копятся в очереди
    body = load_fixture("generate_job.json")
    await submit(service, "generate", body)
    second = copy.deepcopy(body)
    second["request_id"] = "018f6b2a-7c1e-7f00-a000-000000000042"
    r = await service.post("/v1/jobs/generate", second)
    assert r.status_code == 429
    assert int(r.headers["Retry-After"]) >= 1
    assert_valid(r.json(), "_components.yaml", "ApiError")
    lane.running = 0


async def test_bad_payloads(service):
    body = load_fixture("semantic_job.json")
    r = await service.post("/v1/jobs/semantic", {**body, "schema_version": "2"})
    assert r.status_code == 400 and r.json()["code"] == "bad_payload"
    r = await service.post("/v1/jobs/semantic", {k: v for k, v in body.items() if k != "etalon"})
    assert r.status_code == 400 and "etalon" in r.json()["message"]
    r = await service.post("/v1/jobs/semantic", {**body, "mode": "voice"})
    assert r.status_code == 400
    r = await service.post("/v1/jobs/tts", {**load_fixture("tts_job.json"), "text_hash": "../../etc"})
    assert r.status_code == 400
    r = await service.client.post("/v1/jobs/grammar", content=b"not json",
                                  headers={"X-Internal-Token": TOKEN, "Content-Type": "application/json"})
    assert r.status_code == 400
    assert_valid(r.json(), "_components.yaml", "ApiError")


async def test_auth(service):
    body = load_fixture("grammar_job.json")
    r = await service.post("/v1/jobs/grammar", body, token=None)
    assert r.status_code == 401
    r = await service.post("/v1/jobs/grammar", body, token="wrong")
    assert r.status_code == 401 and r.json()["code"] == "unauthorized"
    assert (await service.get("/v1/queue", token=None)).status_code == 401


async def test_callback_retries_then_drops(service):
    service.backend.callback_status = 500
    await submit(service, "grammar", load_fixture("grammar_job.json"))
    cbs = await service.backend.wait_callbacks(4)  # 1 + 3 повтора (backoff 0 в тестах)
    assert len(cbs) == 4 and all(json.dumps(c) == json.dumps(cbs[0]) for c in cbs)
    assert service.ctx.jobs.deliverer.dropped == 1
    assert CALLBACK_URL.endswith("/internal/ai/v1/results")
