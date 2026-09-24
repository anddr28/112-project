#!/usr/bin/env python3
"""
Сквозная проверка go-core через публичный API (как ходит SPA): HTTPS, cookie-сессия,
X-Requested-With: fetch. Нужен запущенный go-core (демо-сиды) и ai-service или cmd/fakeai.

    python3 go-core/tools/e2e/e2e.py [--base https://localhost:8443] [--timeout 90]

Только stdlib. Код возврата 0 — все проверки прошли; иначе печатает первую упавшую.
"""
import argparse
import http.cookiejar
import json
import ssl
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

CTX = ssl.create_default_context()
CTX.check_hostname = False
CTX.verify_mode = ssl.CERT_NONE

PASSED = 0


def ok(cond, msg):
    global PASSED
    if not cond:
        print(f"FAIL: {msg}")
        sys.exit(1)
    PASSED += 1
    print(f"  ok  {msg}")


class Client:
    def __init__(self, base):
        self.base = base.rstrip("/") + "/api/v1"
        self.jar = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(
            urllib.request.HTTPCookieProcessor(self.jar), urllib.request.HTTPSHandler(context=CTX))

    def req(self, method, path, body=None, query=None, raw=False, headers=None, csrf=True):
        url = self.base + path
        if query:
            url += "?" + urllib.parse.urlencode(query)
        data = None
        h = {"Accept": "application/json"}
        if method != "GET" and csrf:
            h["X-Requested-With"] = "fetch"
        if body is not None:
            data = json.dumps(body).encode()
            h["Content-Type"] = "application/json"
        if headers:
            h.update(headers)
        r = urllib.request.Request(url, data=data, method=method, headers=h)
        try:
            with self.opener.open(r, timeout=60) as resp:
                content = resp.read()
                ctype = resp.headers.get("Content-Type", "")
                if raw:
                    return resp.status, content, resp.headers
                return resp.status, (json.loads(content) if content and "json" in ctype else content)
        except urllib.error.HTTPError as e:
            content = e.read()
            try:
                parsed = json.loads(content)
            except Exception:
                parsed = content
            if raw:
                return e.code, content, e.headers
            return e.code, parsed

    def multipart(self, path, fields, files):
        boundary = "----e2e" + uuid.uuid4().hex
        parts = []
        for k, v in fields.items():
            parts.append(f'--{boundary}\r\nContent-Disposition: form-data; name="{k}"\r\n\r\n{v}\r\n'.encode())
        for k, (fname, ctype, content) in files.items():
            parts.append(f'--{boundary}\r\nContent-Disposition: form-data; name="{k}"; filename="{fname}"\r\n'
                         f'Content-Type: {ctype}\r\n\r\n'.encode() + content + b"\r\n")
        parts.append(f"--{boundary}--\r\n".encode())
        data = b"".join(parts)
        r = urllib.request.Request(self.base + path, data=data, method="POST", headers={
            "Content-Type": f"multipart/form-data; boundary={boundary}", "X-Requested-With": "fetch",
            "Accept": "application/json"})
        try:
            with self.opener.open(r, timeout=60) as resp:
                return resp.status, json.loads(resp.read() or b"null")
        except urllib.error.HTTPError as e:
            return e.code, json.loads(e.read() or b"null")

    def login(self, login, password):
        st, u = self.req("POST", "/auth/login", {"login": login, "password": password})
        ok(st == 200 and u.get("login") == login, f"login {login} -> {st}")
        return u


def wav_bytes(ms=1500):
    import struct
    rate = 16000
    n = rate * ms // 1000
    data = b"".join(struct.pack("<h", int(3000 * ((i // 40) % 2 * 2 - 1))) for i in range(n))
    hdr = b"RIFF" + struct.pack("<I", 36 + len(data)) + b"WAVEfmt " + struct.pack("<IHHIIHH", 16, 1, 1, rate, rate * 2, 2, 16) + b"data" + struct.pack("<I", len(data))
    return hdr + data


def walk_pages(c, path, limit, query=None):
    """Обходит список страницами по X-Next-Cursor; возвращает (все элементы, число страниц)."""
    items, pages, cursor = [], 0, None
    while True:
        q = dict(query or {}, limit=limit)
        if cursor:
            q["cursor"] = cursor
        st, raw, hdr = c.req("GET", path, query=q, raw=True)
        ok(st == 200, f"страница {path} ({st})")
        page = json.loads(raw)
        ok(len(page) <= limit, f"страница {path} не длиннее limit={limit}")
        items += page
        pages += 1
        cursor = hdr.get("X-Next-Cursor")
        if not cursor:
            return items, pages
        ok(pages < 1000, "пагинация завершается")


def wait_for(fn, timeout, what):
    t0 = time.time()
    while time.time() - t0 < timeout:
        v = fn()
        if v:
            return v
        time.sleep(0.5)
    ok(False, f"timeout waiting: {what}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="https://localhost:8443")
    ap.add_argument("--timeout", type=int, default=90)
    a = ap.parse_args()
    T = a.timeout

    print("== анонимно")
    anon = Client(a.base)
    st, body = anon.req("GET", "/auth/me")
    ok(st == 401 and body.get("code") == "unauthorized", "me без сессии -> 401 unauthorized")
    st, demo = anon.req("GET", "/auth/demo-accounts")
    ok(st == 200 and any(d["login"] == "teacher" for d in demo), "demo-accounts")
    st, body = anon.req("POST", "/auth/login", {"login": "teacher", "password": "teacher"}, csrf=False)
    ok(st == 403, "CSRF: POST без X-Requested-With -> 403")
    st, body = anon.req("POST", "/auth/login", {"login": "teacher", "password": "wrong-password"})
    ok(st == 401, "неверный пароль -> 401")

    print("== преподаватель: справочники")
    t = Client(a.base)
    teacher = t.login("teacher", "teacher")
    st, me = t.req("GET", "/auth/me")
    ok(st == 200 and me["role"] == "teacher", "me teacher")
    st, types = t.req("GET", "/classifier/types")
    ok(st == 200 and len(types) >= 20, f"classifier types ({len(types)})")
    st, found = t.req("GET", "/classifier/types/search", query={"q": "пожар"})
    ok(st == 200 and len(found) >= 1, "поиск типа 'пожар'")
    fire = found[0]
    st, attrs = t.req("GET", f"/classifier/types/{fire['id']}/attributes")
    ok(st == 200 and isinstance(attrs, list), "опросная карта")
    st, feat = t.req("GET", "/classifier/featured")
    ok(st == 200 and "frequent" in feat and "significant" in feat, "featured")
    st, labels = t.req("GET", "/classifier/labels")
    ok(st == 200 and labels["fields"].get("applicant.name"), "labels")
    st, svcs = t.req("GET", "/services")
    ok(st == 200 and any(s["code"] == "101" for s in svcs), "службы (код 101)")
    st, addr = t.req("GET", "/address/suggest", query={"q": "Тверская 12"})
    ok(st == 200 and len(addr) >= 1, "подсказка адреса")
    st, tr = t.req("GET", "/reaction/transitions", query={"current": "Получена службой", "serviceCode": "101"})
    ok(st == 200 and any(x["status"] == "Принята" for x in tr), "переходы статусов")

    print("== преподаватель: сценарии")
    st, scs = t.req("GET", "/scenarios")
    ok(st == 200 and len(scs) >= 1, f"сценарии ({len(scs)})")
    paged, pages = walk_pages(t, "/scenarios", 2)
    ok([x["id"] for x in paged] == [x["id"] for x in scs] and pages >= 2,
       f"сценарии постранично по 2 = полный список ({pages} стр., без дублей и пропусков)")
    st, bad = t.req("GET", "/scenarios", query={"cursor": "испорчен"})
    ok(st == 400 and bad.get("code") == "validation", "испорченный курсор -> 400 validation")
    st, bad = t.req("GET", "/scenarios", query={"limit": 0})
    ok(st == 400, "limit=0 -> 400")
    validated = [s for s in scs if s["status"] == "validated"]
    ok(len(validated) >= 1, "есть подтверждённые демо-сценарии")
    st, sc = t.req("POST", "/scenarios", {"title": "E2E сценарий " + uuid.uuid4().hex[:6], "categoryId": fire["id"], "difficulty": 1, "mode": "cards"})
    ok(st == 201 and sc["status"] == "draft", "создание сценария (draft)")
    st, sc2 = t.req("PATCH", f"/scenarios/{sc['id']}", {
        "callScript": {"caller": {"name": "Иван", "phone": "+79990001122", "role": "очевидец"},
                       "address": {"raw": "Москва, Тверская улица, 12"}, "keyFacts": ["дым из окна"],
                       "turns": [{"speaker": "caller", "text": "Алло, у нас дым из окна!"}]},
        "etalonDraft": dict(sc["etalonDraft"], description="Дым из окна квартиры", incidentTypeIds=[fire["id"]],
                            applicant={"name": "Иван", "status": "очевидец"},
                            address=dict(sc["etalonDraft"]["address"], raw="Москва, Тверская улица, 12", street="Тверская", house="12")),
    })
    ok(st == 200 and sc2["etalonVersion"] >= 2, f"правка эталона -> новая версия ({sc2.get('etalonVersion')})")
    st, ap_ = t.req("POST", f"/scenarios/{sc['id']}/approve")
    ok(st == 200 and ap_["status"] == "validated", "approve")
    st, gen = t.req("POST", "/scenarios/generate", {"categoryId": fire["id"], "difficulty": 2, "mode": "cards", "withDialogue": True})
    ok(st == 202 and gen.get("jobId"), "генерация: 202 jobId")
    job = wait_for(lambda: (lambda r: r[1] if r[0] == 200 and r[1]["status"] in ("done", "failed") else None)(
        t.req("GET", f"/ai-jobs/{gen['jobId']}")), T, "генерация сценария")
    ok(job["status"] == "done", f"генерация завершена ({job['status']} {job.get('error', '')})")
    st, gsc = t.req("GET", f"/scenarios/{gen['scenarioId']}")
    ok(st == 200 and gsc["status"] == "generated" and gsc["callScript"]["turns"], "сгенерированный сценарий с легендой")
    ok(bool(gsc.get("expectedDialogue", {}).get("checklist")), "сгенерирован чек-лист разговора")
    st, ap2 = t.req("POST", f"/scenarios/{gen['scenarioId']}/approve")
    ok(st == 200 and ap2["status"] == "validated", "approve сгенерированного")
    st, prev = t.req("POST", f"/scenarios/{gen['scenarioId']}/tts-preview", {"text": "Алло, помогите!"})
    ok(st == 200 and prev["audioUrl"].startswith("/api/v1/media/tts/"), "tts-preview")
    st, audio, hdr = t.req("GET", prev["audioUrl"][len("/api/v1"):], raw=True)
    ok(st == 200 and audio[:4] == b"RIFF", "аудио озвучки отдаётся")

    print("== занятие (голос)")
    st, users = t.req("GET", "/users", query={"role": "student"})
    ok(st == 200 and len(users) >= 1, f"студенты для занятия ({len(users)})")
    student = next(u for u in users if u["login"] == "student")
    st, ds = t.req("GET", "/lessons/default-settings")
    ok(st == 200 and "voice" in ds and "dialogue" in ds["weights"], "default-settings")
    st, lesson = t.req("POST", "/lessons", {
        "title": "E2E голосовое занятие", "mode": "cards", "perspective": "operator112", "timeLimitSec": 120,
        "scenarioIds": [gen["scenarioId"]], "participantIds": [student["id"]], "passThreshold": 60, "allowReplay": True,
        "voice": {"enabled": True, "input": "both", "pushToTalk": True, "maxTurns": 6, "ttsEnabled": True}})
    ok(st == 201 and lesson["status"] == "draft", "создание занятия")
    ok(abs(sum(lesson["settings"]["weights"].values()) - 1) < 0.01 and lesson["settings"]["weights"]["dialogue"] > 0, "веса с разговором нормированы")
    st, lesson = t.req("POST", f"/lessons/{lesson['id']}/start")
    ok(st == 200 and lesson["status"] == "running", "старт занятия")
    st, la = t.req("GET", f"/lessons/{lesson['id']}/attempts")
    ok(st == 200 and len(la) == 1 and la[0]["status"] == "issued", "попытка выдана")

    print("== обучающийся")
    s = Client(a.base)
    s.login("student", "student")
    st, assigned = s.req("GET", "/lessons/assigned")
    mine = next(x for x in assigned if x["lesson"]["id"] == lesson["id"])
    att = mine["attempt"]
    ok(att and att["status"] == "issued" and att["voice"]["enabled"], "назначенное занятие с попыткой")
    st, forbidden = s.req("GET", f"/lessons/{lesson['id']}/attempts")
    ok(st == 403, "студенту нельзя список попыток занятия")
    st, cs = s.req("GET", f"/attempts/{att['id']}/call-script")
    ok(st == 200 and "keyFacts" not in json.dumps(cs) and "dialogue" not in cs, "урезанная легенда без фактов и брифа")
    st, acc = s.req("POST", f"/attempts/{att['id']}/accept-call")
    ok(st == 200 and acc["attempt"]["status"] == "in_progress" and acc.get("opening"), "принять вызов + вступление")
    st, acc2 = s.req("POST", f"/attempts/{att['id']}/accept-call")
    ok(st == 200 and acc2["attempt"]["callAcceptedAt"] == acc["attempt"]["callAcceptedAt"], "accept-call идемпотентен")
    st, draft = s.req("GET", f"/attempts/{att['id']}/draft")
    ok(st == 200 and draft["phones"].get("aon"), "черновик с АОН")
    draft["description"] = "Дым из квартиры на пятом этаже, внутри может быть человек."
    draft["incidentTypeIds"] = [fire["id"]]
    draft["applicant"] = {"name": "Мария", "status": "очевидец"}
    st, saved = s.req("PUT", f"/attempts/{att['id']}/draft", draft)
    ok(st == 200 and saved.get("savedAt"), "автосохранение")
    now = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    evs = [{"clientSeq": i, "type": "field_changed", "at": now, "payload": {"field": "description"}} for i in range(1, 6)]
    st, r = s.req("POST", f"/attempts/{att['id']}/events", {"events": evs})
    ok(st == 200 and r["accepted"] == 5 and r["lastSeq"] == 5, "батч событий")
    st, r = s.req("POST", f"/attempts/{att['id']}/events", {"events": evs})
    ok(st == 200 and r["accepted"] == 0, "повтор батча — дубли отброшены")
    st, svc = s.req("POST", f"/attempts/{att['id']}/services", {"serviceCode": "101"})
    ok(st == 200 and svc["currentStatus"] == "Получена службой", "служба добавлена")
    st, bad = s.req("POST", f"/attempts/{att['id']}/services/{svc['serviceId']}/status", {"status": "Работы завершены"})
    ok(st in (400, 409), "недопустимый/без комментария переход отклонён")
    st, svc = s.req("POST", f"/attempts/{att['id']}/services/{svc['serviceId']}/status", {"status": "Принята"})
    ok(st == 200 and svc["currentStatus"] == "Принята", "статус «Принята»")

    st, dlg = s.req("GET", f"/attempts/{att['id']}/dialogue")
    ok(st == 200 and dlg["nextTurnNo"] == 1 and len(dlg["turns"]) == 1, "разговор: вступление, nextTurnNo=1")
    st, turn = s.req("POST", f"/attempts/{att['id']}/dialogue/turns", {"turnNo": 1, "text": "Служба 112, слушаю. Какой адрес, подъезд и этаж?"})
    ok(st == 200 and turn["caller"]["text"] and turn["nextTurnNo"] == 2, f"ход 1 текстом ({st})")
    st, dup = s.req("POST", f"/attempts/{att['id']}/dialogue/turns", {"turnNo": 1, "text": "повтор"})
    ok(st == 409 and dup.get("details", {}).get("nextTurnNo") == 2, "повтор хода -> 409 с nextTurnNo")
    st, turn2 = s.multipart(f"/attempts/{att['id']}/dialogue/turns", {"turnNo": "2"}, {"audio": ("turn.wav", "audio/wav", wav_bytes())})
    ok(st in (200, 503), f"ход 2 голосом ({st})")
    nt = 3 if st == 200 and not turn2.get("noSpeech") else 2
    st, turn3 = s.req("POST", f"/attempts/{att['id']}/dialogue/turns", {"turnNo": nt, "text": "Есть ли пострадавшие? Помощь уже направлена, оставайтесь на связи."})
    ok(st == 200, "ход с «помощь направлена»")
    st, dlg = s.req("GET", f"/attempts/{att['id']}/dialogue")
    ok(st == 200 and len(dlg["turns"]) >= 5, f"транскрипт ({len(dlg['turns'])} реплик)")
    for tt in dlg["turns"]:
        if tt["speaker"] == "caller" and tt.get("audio"):
            st2, a2, _ = s.req("GET", tt["audio"]["audioUrl"][len("/api/v1"):], raw=True)
            ok(st2 == 200, "аудио реплики заявителя доступно")
            break

    st, sub = s.req("POST", f"/attempts/{att['id']}/submit", {"card": draft})
    ok(st == 200 and sub["status"] in ("evaluating", "evaluated"), "сдача карточки")
    st, sub2 = s.req("POST", f"/attempts/{att['id']}/submit", {"card": draft})
    ok(st == 200, "submit идемпотентен")
    st, ev = s.req("GET", f"/attempts/{att['id']}/evaluation")
    ok(st == 200 and ev["status"] in ("partial", "done") and ev["fieldsScore"] is not None, "оценка partial: поля+тайминг")
    ev = wait_for(lambda: (lambda r: r[1] if r[0] == 200 and r[1]["status"] == "done" else None)(
        s.req("GET", f"/attempts/{att['id']}/evaluation")), T, "оценка всех слоёв")
    ok(ev["verdict"] in ("pass", "fail") and ev.get("grammarScore") is not None and ev.get("semanticScore") is not None, "оценка done: вердикт, грамматика, семантика")
    ok(ev.get("dialogueScore") is not None and ev.get("dialogue", {}).get("checklist"), "слой разговора с чек-листом")
    ok(isinstance(ev["recommendations"], list) and isinstance(ev["fieldErrors"], list), "рекомендации и ошибки полей — массивы")
    st, att_after = s.req("GET", f"/attempts/{att['id']}")
    ok(st == 200 and att_after["status"] == "evaluated", "попытка evaluated")
    st, prog = s.req("GET", f"/users/{student['id']}/progress")
    ok(st == 200 and prog["attemptsDone"] >= 1 and prog["xp"] > 0, f"прогресс: XP={prog.get('xp')}")

    print("== преподаватель: ревью, отчёты")
    xp_before = ev.get("xpEarned", 0)
    st, prog0 = s.req("GET", f"/users/{student['id']}/progress")
    st, low = t.req("POST", f"/attempts/{att['id']}/evaluation/override", {"score": 10, "reason": "Проверка пересчёта XP"})
    ok(st == 200 and low["verdict"] == "fail", "корректировка на незачёт")
    ok(low.get("xpEarned", 0) < xp_before or ev["verdict"] == "fail", f"XP за попытку пересчитан: {xp_before} -> {low.get('xpEarned')}")
    st, prog1 = s.req("GET", f"/users/{student['id']}/progress")
    ok(st == 200 and (prog1["xp"] < prog0["xp"] or ev["verdict"] == "fail"), f"XP в прогрессе пересчитан: {prog0['xp']} -> {prog1['xp']}")
    ok(any(x["reason"] != "pass_bonus" for x in prog1["xpLog"]) and not any(
        x["reason"] == "pass_bonus" and x.get("attemptId") == att["id"] for x in prog1["xpLog"]), "бонус за зачёт снят")
    st, ov = t.req("POST", f"/attempts/{att['id']}/evaluation/override", {"score": 88, "reason": "Учтён разговор"})
    ok(st == 200 and ov["finalScore"] == 88 and ov["override"]["reason"] == "Учтён разговор", "ручная корректировка")
    st, prog2 = s.req("GET", f"/users/{student['id']}/progress")
    ok(ov.get("xpEarned", 0) > low.get("xpEarned", 0) and prog2["xp"] > prog1["xp"],
       f"зачёт после корректировки вернул бонус: XP {prog1['xp']} -> {prog2['xp']}")
    st, fb = t.req("POST", f"/attempts/{att['id']}/feedback", {"comment": "Уточняйте подъезд раньше", "field": "address.entrance"})
    ok(st == 201 and fb["teacherName"], "комментарий преподавателя")
    st, fbl = s.req("GET", f"/attempts/{att['id']}/feedback")
    ok(st == 200 and len(fbl) == 1, "студент видит комментарий")
    st, evs2 = t.req("GET", f"/attempts/{att['id']}/events")
    ok(st == 200 and len(evs2) >= 8, f"хронология ({len(evs2)} событий)")
    for fmt, magic in (("csv", b"\xef\xbb\xbf"), ("xlsx", b"PK"), ("pdf", b"%PDF")):
        st, body, hdr = t.req("GET", f"/lessons/{lesson['id']}/report", query={"format": fmt}, raw=True)
        ok(st == 200 and body[:len(magic)] == magic, f"отчёт {fmt} ({len(body)} байт)")
    st, fin = t.req("POST", f"/lessons/{lesson['id']}/finish")
    ok(st == 200 and fin["status"] == "finished", "завершение занятия")

    print("== администратор")
    adm = Client(a.base)
    adm.login("admin", "admin")
    st, h = adm.req("GET", "/admin/health")
    ok(st == 200 and h["postgres"]["ok"] and "aiService" in h, f"health ({h.get('status')})")
    st, sets = adm.req("GET", "/admin/settings")
    ok(st == 200 and any(x["key"] == "pass_threshold" for x in sets), "настройки")
    st, upd = adm.req("PUT", "/admin/settings/time_limit_sec", {"value": 35})
    ok(st == 200 and upd["value"] == 35, "изменение настройки")
    st, bad = adm.req("PUT", "/admin/settings/time_limit_sec", {"value": "abc"})
    ok(st == 400, "невалидная настройка -> 400")
    adm.req("PUT", "/admin/settings/time_limit_sec", {"value": 30})
    # аудит пишется асинхронно батчами (≤200 мс) — ждём
    aud = wait_for(lambda: (lambda r: r[1] if r[0] == 200 and any(x["action"] == "evaluation.override" for x in r[1]) else None)(
        adm.req("GET", "/admin/audit", query={"action": "evaluation.", "limit": 50})), 10, "аудит override")
    ok(bool(aud), "аудит содержит override (фильтр по префиксу action)")
    st, ulist = adm.req("GET", "/users")
    ok(st == 200 and len(ulist) >= 4, "список пользователей")
    upaged, upages = walk_pages(adm, "/users", 2)
    ok([x["id"] for x in upaged] == [x["id"] for x in ulist] and upages >= 2, f"пользователи постранично ({upages} стр.)")
    st, lall = adm.req("GET", "/lessons", query={"limit": 200})
    lpaged, lpages = walk_pages(adm, "/lessons", 1)
    ok([x["id"] for x in lpaged] == [x["id"] for x in lall] and lpages == len(lall),
       f"занятия (админ) постранично по 1: {lpages} стр. = полный список")
    spaged, _ = walk_pages(s, "/lessons/assigned", 1)
    st, sall = s.req("GET", "/lessons/assigned")
    ok([x["lesson"]["id"] for x in spaged] == [x["lesson"]["id"] for x in sall], "назначенные занятия постранично")
    ranks = {"running": 0, "scheduled": 1, "draft": 2, "finished": 3}
    ok([ranks.get(x["lesson"]["status"], 4) for x in sall] == sorted(ranks.get(x["lesson"]["status"], 4) for x in sall),
       "назначенные занятия: сначала идущие")
    login = "e2e" + uuid.uuid4().hex[:6]
    st, nu = adm.req("POST", "/users", {"login": login, "password": "Secret123!", "role": "student", "lastName": "Тестов", "firstName": "Тест"})
    ok(st == 201, "создание пользователя")
    st, dupu = adm.req("POST", "/users", {"login": login, "password": "Secret123!", "role": "student", "lastName": "Тестов", "firstName": "Тест"})
    ok(st == 409, "логин занят -> 409")
    nc = Client(a.base)
    nc.login(login, "Secret123!")
    st, bl = adm.req("PUT", f"/users/{nu['id']}/blocked", {"blocked": True})
    ok(st == 200 and bl["status"] == "blocked", "блокировка")
    st, me2 = nc.req("GET", "/auth/me")
    ok(st in (401, 423), f"заблокированная сессия отклонена ({st})")
    st, _ = t.req("GET", "/admin/health")
    ok(st == 403, "преподавателю админка недоступна")
    st, bk = adm.req("POST", "/admin/backups")
    ok(st in (202, 409), f"бэкап запущен ({st})")
    st, t_out = t.req("POST", "/auth/logout")
    ok(st == 204, "logout")
    st, _ = t.req("GET", "/auth/me")
    ok(st == 401, "после logout сессии нет")

    print(f"\nВСЕ ПРОВЕРКИ ПРОЙДЕНЫ: {PASSED}")


if __name__ == "__main__":
    main()
