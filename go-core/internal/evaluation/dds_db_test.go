package evaluation

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"lct/gocore/internal/core"
	"lct/gocore/internal/dds"
)

// Ракурс «Диспетчер ДДС» (v1.3): слой fields — протокол реагирования своей службы
// (решение, норматив решения, обязательные статусы из etalons.scoring.reaction), грамматика —
// текст действия и комментарии диспетчера к статусам, рекомендации — про работу диспетчера.
func TestStartEvaluation_DDS_DB(t *testing.T) {
	t.Parallel()
	f := newDB(t)

	// Эталон ждёт «Принята» за 30 с и «Начало реагирования»; смысл текста действия — по
	// ожидаемым действиям диспетчера (expected_actions), а не по фактам звонка (fireScoring).
	f.exec(t, `UPDATE etalons SET scoring = scoring || '{"reaction": {"decision": "accept", "decision_within_sec": 30,
		"required_statuses": ["Начало реагирования"]}}'::jsonb,
		expected_actions = '[{"action_text": "Сообщение принято, бригада направлена на место",
			"required_facts": ["Сообщение принято", "Бригада направлена на место"]}]'::jsonb WHERE id = $1`, f.etalon)
	lesson := f.lesson(t, core.LessonRunning, `{"perspective": "dds", "pass_threshold": 70}`, f.student)

	// Диспетчер ДДС ЖКХ взял карточку в работу 60 с назад, «Принята» — через 45 с (позже
	// норматива), «Начало реагирования» не проставлено.
	opened := time.Now().UTC().Add(-60 * time.Second).Truncate(time.Millisecond)
	card := map[string]any{
		"address": map[string]any{"raw": "Москва, Тверская, 12"}, "description": "Горит квартира",
		"actionsTaken": "", "incidentTypeIds": []string{}, "attributes": map[string]any{}, "phones": map[string]any{},
		"applicant": map[string]any{}, "flags": map[string]any{"victimsPresent": false, "ambulanceRefusal": false,
			"blocked": false, "noContact": false, "callDropped": false},
		"services": []map[string]any{{
			"serviceId": "x", "code": "zhkh", "name": "ДДС ЖКХ", "shortName": "ДДС ЖКХ", "isPrimary": true,
			"source": "auto", "currentStatus": "Принята", "currentStatusAt": opened.Add(45 * time.Second),
			"allowedNext": []any{}, "editable": true,
			"history": []map[string]any{
				{"status": "Добавлена", "at": opened, "operator": "система"},
				{"status": "Получена службой", "at": opened, "operator": "система"},
				{"status": "Принята", "at": opened.Add(45 * time.Second), "operator": "оп. 227", "comment": "Принято, бригада выезжает"},
			},
		}},
	}
	raw, _ := json.Marshal(card)
	action := "Сообщение принято, дежурная бригада направлена на место"
	a := f.id(t, `INSERT INTO attempts (lesson_id, user_id, scenario_id, etalon_id, mode, seq_no, status, time_limit_sec,
			call_accepted_at, first_input_at, submitted_at, time_spent_ms, card, action_text, service_id)
		VALUES ($1, $2, $3, $4, 'card_actions', 1, 'submitted', 120, $5, $6, $7, 55000, $8, $9,
			(SELECT id FROM services WHERE code = 'zhkh')) RETURNING id`,
		lesson, f.student, f.scenario, f.etalon, opened, opened.Add(45*time.Second), opened.Add(55*time.Second),
		string(raw), action)
	f.start(t, a)

	v := f.view(t, a)
	if got := v["fieldsScore"]; got != float64(57) { // потеряно: норматив 2 + статус 1 из 7
		t.Errorf("fieldsScore = %v", got)
	}
	errs, _ := v["fieldErrors"].([]any)
	fields := map[string]bool{}
	for _, e := range errs {
		fields[e.(map[string]any)["field"].(string)] = true
	}
	if len(errs) != 2 || !fields[dds.FieldDecisionTime] || !fields[dds.FieldStatuses] {
		t.Errorf("fieldErrors = %v", errs)
	}
	if layers := v["layers"].(map[string]any); layers["fields"] != core.LayerDone {
		t.Errorf("layers = %v", layers)
	}
	if eng, _ := v["engine"].(map[string]any); eng == nil || eng["fields"] == nil {
		t.Errorf("engine без источника слоя fields: %v", v["engine"])
	}

	// Грамматика: текст действия и комментарии к статусам своей службы.
	jobs := f.queue.byAttempt(a)
	g, ok := jobs[core.JobEvaluateGrammar]
	if !ok {
		t.Fatalf("нет задачи грамматики: %v", jobs)
	}
	payload, _ := json.Marshal(g.Payload)
	var req struct {
		Texts []struct{ Field, Text string } `json:"texts"`
	}
	_ = json.Unmarshal(payload, &req)
	got := map[string]string{}
	for _, tx := range req.Texts {
		got[tx.Field] = tx.Text
	}
	if got[gramFieldActionText] != action || got[gramFieldReactionComments] != "Принято, бригада выезжает" {
		t.Errorf("тексты грамматики: %+v", req.Texts)
	}
	sem, ok := jobs[core.JobEvaluateSemantic]
	if !ok {
		t.Fatal("нет задачи смыслового слоя по тексту действия")
	}
	sraw, _ := json.Marshal(sem.Payload)
	if !strings.Contains(string(sraw), `"Бригада направлена на место"`) {
		t.Errorf("смысловой слой ДДС без expected_actions: %s", sraw)
	}
	for _, leak := range []string{"горит квартира", "пятый этаж"} { // факты звонка (fireScript/fireScoring)
		if strings.Contains(string(sraw), leak) {
			t.Errorf("смысловой слой ДДС получил факт звонка %q: %s", leak, sraw)
		}
	}

	// Рекомендации (по окончательной оценке) — про протокол диспетчера, а не про «заполните поля».
	f.apply(t, a, core.JobEvaluateGrammar, grammarRes(90))
	f.apply(t, a, core.JobEvaluateSemantic, semanticRes(80, 0.9))
	v = f.view(t, a)
	if v["status"] != core.EvalDone {
		t.Fatalf("status = %v", v["status"])
	}
	recs, _ := v["recommendations"].([]any)
	found := false
	for _, r := range recs {
		m := r.(map[string]any)
		if m["id"] != "rec-missing" {
			continue
		}
		found = true
		if body, _ := m["body"].(string); !strings.HasPrefix(body, "По карточке своей службы") {
			t.Errorf("текст рекомендации: %q", body)
		}
	}
	if !found {
		t.Errorf("нет рекомендации по протоколу: %v", recs)
	}
}

// Ракурс ДДС без эталонных действий: смысловой слой не оценивается (skipped), задача в
// ai-service не ставится, факты звонка (scoring.required_facts) не используются, итог —
// по остальным слоям с перенормировкой весов.
func TestStartEvaluation_DDS_NoExpectedActions_DB(t *testing.T) {
	t.Parallel()
	f := newDB(t) // эталон: required_facts звонка, expected_actions = []
	lesson := f.lesson(t, core.LessonRunning, `{"perspective": "dds", "pass_threshold": 70}`, f.student)

	opened := time.Now().UTC().Add(-40 * time.Second).Truncate(time.Millisecond)
	card := map[string]any{
		"address": map[string]any{"raw": "Москва, Тверская, 12"}, "description": "Горит квартира",
		"actionsTaken": "", "incidentTypeIds": []string{}, "attributes": map[string]any{}, "phones": map[string]any{},
		"applicant": map[string]any{}, "flags": map[string]any{"victimsPresent": false, "ambulanceRefusal": false,
			"blocked": false, "noContact": false, "callDropped": false},
		"services": []map[string]any{{
			"serviceId": "x", "code": "zhkh", "name": "ДДС ЖКХ", "shortName": "ДДС ЖКХ", "isPrimary": true,
			"source": "auto", "currentStatus": "Принята", "currentStatusAt": opened.Add(10 * time.Second),
			"allowedNext": []any{}, "editable": true,
			"history": []map[string]any{
				{"status": "Получена службой", "at": opened, "operator": "система"},
				{"status": "Принята", "at": opened.Add(10 * time.Second), "operator": "оп. 227"},
			},
		}},
	}
	raw, _ := json.Marshal(card)
	a := f.id(t, `INSERT INTO attempts (lesson_id, user_id, scenario_id, etalon_id, mode, seq_no, status, time_limit_sec,
			call_accepted_at, first_input_at, submitted_at, time_spent_ms, card, action_text, service_id)
		VALUES ($1, $2, $3, $4, 'card_actions', 1, 'submitted', 120, $5, $6, $7, 20000, $8, $9,
			(SELECT id FROM services WHERE code = 'zhkh')) RETURNING id`,
		lesson, f.student, f.scenario, f.etalon, opened, opened.Add(10*time.Second), opened.Add(20*time.Second),
		string(raw), "Сообщение принято, бригада направлена на место")
	f.start(t, a)

	if _, ok := f.queue.byAttempt(a)[core.JobEvaluateSemantic]; ok {
		t.Fatal("ДДС без expected_actions: смысловая задача не должна ставиться")
	}
	v := f.view(t, a)
	if layers := v["layers"].(map[string]any); layers["semantic"] != core.LayerSkipped {
		t.Fatalf("layers = %v", layers)
	}
	if _, ok := v["semanticScore"]; ok {
		t.Errorf("semanticScore = %v, слой пропущен", v["semanticScore"])
	}
	if _, ok := v["semantic"]; ok {
		t.Errorf("semantic = %v — факты звонка не должны появляться", v["semantic"])
	}

	f.apply(t, a, core.JobEvaluateGrammar, grammarRes(90))
	v = f.view(t, a)
	if v["status"] != core.EvalDone {
		t.Fatalf("status = %v (смысловой слой не должен держать финализацию)", v["status"])
	}
	w := v["weights"].(map[string]any)
	fs, gs, ts := v["fieldsScore"].(float64), v["grammarScore"].(float64), v["timingScore"].(float64)
	wf, wg, wt := w["fields"].(float64), w["grammar"].(float64), w["timing"].(float64)
	want := math.Round((fs*wf + gs*wg + ts*wt) / (wf + wg + wt))
	if got := v["totalScore"].(float64); got != want {
		t.Errorf("totalScore = %v, want %v (без смыслового слоя, веса перенормированы)", got, want)
	}
}
