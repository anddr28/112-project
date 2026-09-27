package attempts

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/reaction"
)

// ddsAttempt — попытка ракурса «Диспетчер ДДС»: занятие dds (card_actions), эталон с формой
// АРМ (карточка 112) и списком оповещения 101 (основная) + 103, обучающийся работает за 101.
func ddsAttempt(t *testing.T, e *env, f fixture) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var lesson, etalon uuid.UUID
	err := e.pool.QueryRow(ctx, `
WITH e AS (
  INSERT INTO etalons (scenario_id, version, is_current, card, card_draft, scoring)
  VALUES ($1, 2, false,
          '{"services_to_notify": ["103", "101"]}',
          '{"description": "Горит квартира на пятом этаже", "actionsTaken": "ЭТАЛОН-ДЕЙСТВИЯ-112",
            "address": {"raw": "Москва, Тверская, 5"}, "phones": {}, "applicant": {"name": "Мария Петровна"},
            "incidentTypeIds": [], "attributes": {}, "flags": {"victimsPresent": false, "ambulanceRefusal": false,
            "blocked": false, "noContact": false, "callDropped": false},
            "services": [{"code": "103"}, {"code": "101", "isPrimary": true, "reason": "пожар"}]}',
          '{"reaction": {"decision": "accept"}}')
  RETURNING id
), l AS (
  INSERT INTO lessons (title, teacher_id, created_by, mode, status, settings, started_at)
  VALUES ('Диспетчер ДДС', $2, $2, 'card_actions', 'running', '{"perspective": "dds"}', now()) RETURNING id
)
SELECT l.id, e.id FROM l, e`, f.scenario, f.teacher).Scan(&lesson, &etalon)
	if err != nil {
		t.Fatalf("dds lesson: %v", err)
	}
	mustExec(t, e.pool, `INSERT INTO lesson_participants (lesson_id, user_id) VALUES ($1, $2)`, lesson, f.student)
	var id uuid.UUID
	err = e.pool.QueryRow(ctx, `
INSERT INTO attempts (lesson_id, user_id, scenario_id, etalon_id, mode, seq_no, time_limit_sec, service_id)
VALUES ($1, $2, $3, $4, 'card_actions', 1, 120, (SELECT id FROM services WHERE code = '101'))
RETURNING id`, lesson, f.student, f.scenario, etalon).Scan(&id)
	if err != nil {
		t.Fatalf("dds attempt: %v", err)
	}
	mustExec(t, e.pool, `INSERT INTO attempt_drafts (attempt_id, data) VALUES ($1, $2::jsonb)`, id, string(emptyDraftJSON))
	return id
}

func TestDDSFlow_DB(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{})
	id := ddsAttempt(t, e, f)
	st := f.asStudent()
	base := "/attempts/" + id.String()

	// Взять карточку в работу: без вступительной реплики (хук разговора не зовётся, даже если
	// он её вернул бы), служба обучающегося в ответе.
	e.dlg.opening = &public.DialogueTurnView{TurnNo: 0, Text: "не должно прийти"}
	r := e.do(st, http.MethodPost, base+"/accept-call", nil)
	expect(t, r, http.StatusOK, "")
	var acc acceptResponse
	r.json(t, &acc)
	if acc.Opening != nil || acc.Attempt.ActingService == nil || acc.Attempt.ActingService.Code != "101" ||
		acc.Attempt.Perspective != public.Dds {
		t.Fatalf("accept: %s", r.body)
	}

	// Черновик = карточка 112: поля эталона, АОН, службы в «Получена службой», без действий эталона.
	d := e.draft(id)
	if d.Description != "Горит квартира на пятом этаже" || d.ActionsTaken != "" ||
		d.Phones.Aon == nil || *d.Phones.Aon != "+79991234567" {
		t.Errorf("карточка 112: %+v", d)
	}
	if got := strings.Join(codes(d.Services), ","); got != "101,103" {
		t.Errorf("службы (основная первой): %s", got)
	}
	for _, sv := range d.Services {
		if sv.CurrentStatus != reaction.StatusReceived || (sv.Code == "101") != sv.IsPrimary {
			t.Errorf("служба %s: %+v", sv.Code, sv)
		}
	}

	// Диспетчер ДДС с заявителем не говорит; состав служб не меняет.
	var cs public.StudentCallScript
	r = e.do(st, http.MethodGet, base+"/call-script", nil)
	expect(t, r, http.StatusOK, "")
	r.json(t, &cs)
	if len(cs.Turns) != 0 {
		t.Errorf("call-script в ракурсе dds: %d реплик", len(cs.Turns))
	}
	expect(t, e.do(st, http.MethodPost, base+"/replay", nil), http.StatusConflict, "conflict")
	expect(t, e.do(st, http.MethodPost, base+"/services", map[string]string{"serviceCode": "102"}), http.StatusConflict, "conflict")
	expect(t, e.do(st, http.MethodDelete, base+"/services/103", nil), http.StatusConflict, "conflict")

	// Статусы — только своей службы.
	r = e.do(st, http.MethodPost, base+"/services/103/status", map[string]string{"status": "Принята"})
	expect(t, r, http.StatusForbidden, "forbidden")
	if _, msg, _ := r.apiError(t); !strings.Contains(msg, "01 Пожарные") {
		t.Errorf("сообщение без своей службы: %q", msg)
	}
	r = e.do(st, http.MethodPost, base+"/services/101/status", map[string]string{"status": "Принята"})
	expect(t, r, http.StatusOK, "")
	r = e.do(st, http.MethodPost, base+"/services/svc-101/status", map[string]string{"status": "Начало реагирования", "comment": "Выехал расчёт"})
	expect(t, r, http.StatusOK, "")

	// Автосохранение: из тела берётся только текст действия — поля 112 и службы серверные.
	body := d
	body.Description = "ПОДМЕНА"
	body.Services = []public.AssignedService{}
	body.ActionsTaken = "Черновик действия"
	expect(t, e.do(st, http.MethodPut, base+"/draft", body), http.StatusOK, "")
	d = e.draft(id)
	if d.Description != "Горит квартира на пятом этаже" || d.ActionsTaken != "Черновик действия" || len(d.Services) != 2 {
		t.Errorf("черновик после PUT: desc=%q action=%q services=%d", d.Description, d.ActionsTaken, len(d.Services))
	}
	if sv := d.Services[0]; sv.CurrentStatus != reaction.StatusStarted || len(sv.History) != 4 {
		t.Errorf("статусы 101 потеряны автосохранением: %+v", sv)
	}

	// Сдача: карточка — серверная, текст действия — из тела.
	body.ActionsTaken = "Сообщение принято, дежурная бригада направлена на место"
	r = e.do(st, http.MethodPost, base+"/submit", map[string]any{"card": body})
	expect(t, r, http.StatusOK, "")
	a := e.attemptRow(id)
	if a.actionText == nil || *a.actionText != body.ActionsTaken || e.eval.n() != 1 {
		t.Fatalf("submit: %+v, оценок %d", a, e.eval.n())
	}
	card := string(a.card)
	if !strings.Contains(card, "Горит квартира на пятом этаже") || strings.Contains(card, "ПОДМЕНА") ||
		!strings.Contains(card, "Начало реагирования") {
		t.Errorf("сданная карточка: %s", card)
	}
	if acts := e.aud.actions(); len(acts) < 2 || acts[0] != "attempt.accept" {
		t.Errorf("аудит: %v", acts)
	}
}

// Попытка ракурса 112 в занятии dds (выдана до миграции 00004, service_id NULL) работает
// по-старому: службы добавляются, статусы меняются у любой службы.
func TestDDSLegacyAttempt_DB(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := seed(t, e.pool, lessonOpts{})
	mustExec(t, e.pool, `UPDATE lessons SET settings = settings || '{"perspective": "dds"}' WHERE id = $1`, f.lesson)
	id := e.accepted(f, f.student)
	r := e.do(f.asStudent(), http.MethodPost, "/attempts/"+id.String()+"/services", map[string]string{"serviceCode": "102"})
	expect(t, r, http.StatusOK, "")
	r = e.do(f.asStudent(), http.MethodPost, "/attempts/"+id.String()+"/services/102/status", map[string]string{"status": "Принята"})
	expect(t, r, http.StatusOK, "")
	if core.AttemptTerminal(e.attemptRow(id).status) {
		t.Fatal("попытка закрыта")
	}
}
