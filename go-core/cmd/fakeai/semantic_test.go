package main

import (
	"reflect"
	"strings"
	"testing"

	"lct/gocore/internal/gen/aiservice"
)

func semReq(t *testing.T, p obj) *aiservice.SemanticJobRequest {
	t.Helper()
	req := new(aiservice.SemanticJobRequest)
	if err := decodeInto(mustJSON(t, p), req); err != nil {
		t.Fatal(err)
	}
	return req
}

// semWith — semanticPayload с заменой описания/мер ответа.
func semWith(t *testing.T, description, actions string) obj {
	t.Helper()
	p := semanticPayload()
	p["answer"] = obj{"card": obj{"description": description, "actions_taken": actions}}
	return p
}

func TestEvalSemanticCoverage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		p         obj
		score     float32
		missing   []string
		extra     []string
		conf      float32
		summaryIn string
	}{
		{"всё покрыто", semanticPayload(), 100, []string{}, []string{}, 0.86, "Описание полное"},
		{"не хватает факта", semWith(t, "Задымление в квартире на пятом этаже.", "Направлены пожарные."), 67,
			[]string{"в квартире может находиться человек"}, []string{}, 0.86,
			"Зафиксировано 2 из 3 существенных фактов. Не отражено: в квартире может находиться человек."},
		{"число цифрой", semWith(t, "Задымление в квартире, 5 этаж, в квартире может находиться человек.", ""), 100,
			[]string{}, []string{}, 0.86, "Описание полное"},
		{"домысел снижает балл", semWith(t, "Задымление в квартире на пятом этаже, видно открытое пламя, в квартире может находиться человек.", ""),
			90, []string{}, []string{"открытое пламя"}, 0.86, "которых заявитель не сообщал: открытое пламя"},
		{"отрицание — не домысел", semWith(t, "Задымление в квартире на пятом этаже, открытого пламени не видно, в квартире может находиться человек.", ""),
			100, []string{}, []string{}, 0.86, "Описание полное"},
		{"пустой ответ", semWith(t, "", ""), 0,
			[]string{"задымление в квартире", "5 этаж", "в квартире может находиться человек"}, []string{}, 0.58, "не заполнено"},
		{"ни одного факта", semWith(t, "Звонила женщина, что-то случилось во дворе.", ""), 0,
			[]string{"задымление в квартире", "5 этаж", "в квартире может находиться человек"}, []string{}, 0.86,
			"Ни один из 3 существенных фактов"},
		{"короткий текст — низкая уверенность", semWith(t, "Задымление, 5 этаж.", ""), 67,
			[]string{"в квартире может находиться человек"}, []string{}, 0.58, "Зафиксировано 2 из 3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			res := evalSemantic(semReq(t, c.p))
			if res.Score != c.score || res.Confidence != c.conf {
				t.Errorf("score %v conf %v, ждали %v %v", res.Score, res.Confidence, c.score, c.conf)
			}
			if res.MissingFacts == nil || res.ExtraFacts == nil || res.PerField == nil || res.SummaryForStudent == nil {
				t.Fatalf("nil-поля: %+v", res)
			}
			if !reflect.DeepEqual(*res.MissingFacts, c.missing) {
				t.Errorf("missing %q, ждали %q", *res.MissingFacts, c.missing)
			}
			if !reflect.DeepEqual(*res.ExtraFacts, c.extra) {
				t.Errorf("extra %q, ждали %q", *res.ExtraFacts, c.extra)
			}
			if !strings.Contains(*res.SummaryForStudent, c.summaryIn) {
				t.Errorf("summary %q не содержит %q", *res.SummaryForStudent, c.summaryIn)
			}
			for _, pf := range *res.PerField {
				if pf.Score < 0 || pf.Score > 100 || pf.Field == "" || deref(pf.Comment) == "" {
					t.Errorf("per_field %+v", pf)
				}
			}
		})
	}
}

func TestEvalSemanticPerField(t *testing.T) {
	t.Parallel()
	res := evalSemantic(semReq(t, semanticPayload()))
	pf := *res.PerField
	if len(pf) != 2 || pf[0].Field != "description" || pf[0].Score != 100 || pf[1].Field != "actions_taken" || pf[1].Score != 100 {
		t.Fatalf("per_field %+v", pf)
	}
	// Меры без указания служб — 60; пустое поле в per_field не попадает.
	res = evalSemantic(semReq(t, semWith(t, "Задымление в квартире на пятом этаже.", "Поговорил с заявителем.")))
	pf = *res.PerField
	if len(pf) != 2 || pf[1].Score != 60 || !strings.HasPrefix(deref(pf[0].Comment), "Не зафиксировано: ") {
		t.Fatalf("per_field %+v", pf)
	}
	res = evalSemantic(semReq(t, semWith(t, "Задымление в квартире на пятом этаже.", "  ")))
	if pf = *res.PerField; len(pf) != 1 || pf[0].Field != "description" {
		t.Fatalf("пустое actions_taken в per_field: %+v", pf)
	}
}

func TestEvalSemanticFactSources(t *testing.T) {
	t.Parallel()
	// Нет scoring.required_facts — берутся call_script.key_facts.
	p := semWith(t, "Дым из квартиры на пятом этаже, подъезд 3.", "")
	p["etalon"] = obj{"card": obj{}}
	res := evalSemantic(semReq(t, p))
	if res.Score != 100 || len(*res.MissingFacts) != 0 {
		t.Fatalf("key_facts: %+v %v", res.Score, *res.MissingFacts)
	}

	// Ни фактов эталона, ни легенды — «факты не заданы», 100.
	delete(p, "call_script")
	res = evalSemantic(semReq(t, p))
	if res.Score != 100 || !strings.Contains(deref(res.SummaryForStudent), "не заданы") {
		t.Fatalf("без фактов: %v %q", res.Score, deref(res.SummaryForStudent))
	}

	// Пустые и повторяющиеся факты отбрасываются.
	p = semanticPayload()
	p["etalon"].(obj)["scoring"] = obj{"required_facts": []any{"5 этаж", " ", "5 этаж", ""}}
	res = evalSemantic(semReq(t, p))
	if res.Score != 100 || len(*res.MissingFacts) != 0 {
		t.Fatalf("дубли: %v %v", res.Score, *res.MissingFacts)
	}
	p["answer"] = obj{"card": obj{"description": "Горит дом."}}
	if res = evalSemantic(semReq(t, p)); !reflect.DeepEqual(*res.MissingFacts, []string{"5 этаж"}) {
		t.Fatalf("дубли в missing: %q", *res.MissingFacts)
	}
}

func TestEvalSemanticCardActionsMode(t *testing.T) {
	t.Parallel()
	p := semanticPayload()
	p["mode"] = "card_actions"
	p["etalon"] = obj{
		"card": obj{},
		"expected_actions": []any{obj{
			"action_text":     "Направить пожарных",
			"required_facts":  []any{"пожарные на 5 этаж", "эвакуация жильцов"},
			"forbidden_facts": []any{"отключить газ"},
		}},
	}
	p["answer"] = obj{"action_text": "Направить пожарных на пятый этаж, организовать эвакуацию жильцов."}
	p["free_text_fields"] = []any{"action_text"}
	res := evalSemantic(semReq(t, p))
	if res.Score != 100 || len(*res.MissingFacts) != 0 || len(*res.ExtraFacts) != 0 {
		t.Fatalf("card_actions: %v %v %v", res.Score, *res.MissingFacts, *res.ExtraFacts)
	}
	if pf := *res.PerField; len(pf) != 1 || pf[0].Field != "action_text" {
		t.Fatalf("per_field %+v", pf)
	}
	// Запрещённый факт из expected_actions.
	p["answer"] = obj{"action_text": "Направить пожарных на пятый этаж, эвакуация жильцов, отключить газ в доме."}
	if res = evalSemantic(semReq(t, p)); !reflect.DeepEqual(*res.ExtraFacts, []string{"отключить газ"}) || res.Score != 90 {
		t.Fatalf("forbidden из expected_actions: %v %v", res.Score, *res.ExtraFacts)
	}
	// Пустой список полей в режиме 2 — action_text по умолчанию.
	p["free_text_fields"] = []any{}
	if res = evalSemantic(semReq(t, p)); len(*res.PerField) != 1 || (*res.PerField)[0].Field != "action_text" {
		t.Fatalf("поля по умолчанию: %+v", *res.PerField)
	}
	// Нет action_text, но есть карточка — оцениваем карточку.
	p["answer"] = obj{"card": obj{"description": "Пожарные на пятый этаж, эвакуация жильцов."}}
	p["free_text_fields"] = []any{"action_text"}
	if res = evalSemantic(semReq(t, p)); res.Score != 100 {
		t.Fatalf("fallback на карточку: %v %v", res.Score, *res.MissingFacts)
	}
}

// Регрессия: в режиме card_actions факты берутся из expected_actions[].required_facts даже
// при заполненном scoring.required_facts (go-core кладёт туда key_facts легенды — обстоятельства
// звонка): оценивается текст действий, как в ai-service (PY-05). В режиме cards — наоборот.
func TestSemanticFactsPrecedence(t *testing.T) {
	t.Parallel()
	etalon := obj{
		"card":             obj{},
		"scoring":          obj{"required_facts": []any{"из-под двери валит чёрный дым"}},
		"expected_actions": []any{obj{"action_text": "x", "required_facts": []any{"пожарные на 5 этаж"}}},
	}
	p := semanticPayload()
	p["etalon"] = etalon
	p["mode"] = "card_actions"
	req, _ := semanticFacts(semReq(t, p))
	if !reflect.DeepEqual(req, []string{"пожарные на 5 этаж"}) {
		t.Fatalf("card_actions: %q", req)
	}
	p["mode"] = "cards"
	if req, _ = semanticFacts(semReq(t, p)); !reflect.DeepEqual(req, []string{"из-под двери валит чёрный дым"}) {
		t.Fatalf("cards: %q", req)
	}
	// card_actions без фактов в действиях — scoring, затем key_facts легенды.
	p["mode"] = "card_actions"
	p["etalon"] = obj{"card": obj{}, "scoring": obj{"required_facts": []any{"дым"}},
		"expected_actions": []any{obj{"action_text": "x"}}}
	if req, _ = semanticFacts(semReq(t, p)); !reflect.DeepEqual(req, []string{"дым"}) {
		t.Fatalf("fallback на scoring: %q", req)
	}
	p["etalon"] = obj{"card": obj{}}
	if req, _ = semanticFacts(semReq(t, p)); !reflect.DeepEqual(req, []string{"дым из квартиры на 5 этаже", "подъезд 3"}) {
		t.Fatalf("fallback на key_facts: %q", req)
	}
	// Запрещённые — из обоих источников без дублей.
	p["etalon"] = obj{"card": obj{}, "scoring": obj{"forbidden_facts": []any{"а", "б"}},
		"expected_actions": []any{obj{"action_text": "x", "forbidden_facts": []any{"б", "в"}}}}
	if _, forb := semanticFacts(semReq(t, p)); !reflect.DeepEqual(forb, []string{"а", "б", "в"}) {
		t.Fatalf("forbidden %q", forb)
	}
	// Ответ с действиями по эталону — 100 при любом содержимом scoring.
	p["etalon"] = etalon
	p["answer"] = obj{"action_text": "Направить пожарных на пятый этаж."}
	p["free_text_fields"] = []any{"action_text"}
	if res := evalSemantic(semReq(t, p)); res.Score != 100 {
		t.Fatalf("действия по эталону: %v %q", res.Score, *res.MissingFacts)
	}
}

func TestEvalSemanticOtherFields(t *testing.T) {
	t.Parallel()
	p := semanticPayload()
	p["answer"] = obj{
		"card": obj{"attributes": obj{"notes": "Задымление в квартире на пятом этаже, в квартире может находиться человек.", "n": 5}},
		"turns": []any{
			obj{"turn_no": 1, "answer_text": "Задымление в квартире."},
			obj{"turn_no": 2, "answer_text": "Пятый этаж, в квартире может находиться человек."},
		},
	}
	p["free_text_fields"] = []any{"notes", "n"}
	res := evalSemantic(semReq(t, p))
	if res.Score != 100 {
		t.Fatalf("атрибут опросной карты: %v %v", res.Score, *res.MissingFacts)
	}
	// Многореплика (answer.turns) — реплики склеиваются.
	p["free_text_fields"] = []any{"answer_turns"}
	if res = evalSemantic(semReq(t, p)); res.Score != 100 || (*res.PerField)[0].Field != "answer_turns" {
		t.Fatalf("answer_turns: %v %v", res.Score, *res.MissingFacts)
	}
	// Режим 1 без списка полей — description + actions_taken.
	p = semanticPayload()
	p["free_text_fields"] = []any{}
	if res = evalSemantic(semReq(t, p)); res.Score != 100 || len(*res.PerField) != 2 {
		t.Fatalf("поля по умолчанию: %v %+v", res.Score, *res.PerField)
	}
}

func TestFactCoveredHeuristics(t *testing.T) {
	t.Parallel()
	cases := []struct {
		text, fact string
		want       bool
	}{
		{"горит квартира на пятом этаже", "5 этаж", true},
		{"горит квартира на 5 этаже", "5 этаж", true},
		{"четвертый этаж, дым", "четвёртый этаж", true}, // ё ≡ е
		{"ЧЕТВЁРТЫЙ ЭТАЖ", "четвертый этаж", true},
		{"горит квартира", "пострадавших нет", false},
		{"в подъезде дым", "", true}, // факт без значимых слов засчитан
		{"двое детей в квартире", "2 ребёнка в квартире", true},
		{"", "задымление", false},
		{"12 этаж", "12 этаж", true},
		{"двенадцатый этаж", "12 этаж", true},
	}
	for _, c := range cases {
		nt := norm(c.text)
		if got := factCovered(nt, tokens(nt), c.fact); got != c.want {
			t.Errorf("factCovered(%q, %q) = %v", c.text, c.fact, got)
		}
	}
}

func TestForbiddenPresentNegation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		text, fact string
		want       bool
	}{
		{"видно открытое пламя", "открытое пламя", true},
		{"открытого пламени не видно", "открытое пламя", false},
		{"нет открытого пламени", "открытое пламя", false},
		{"без открытого пламени", "открытое пламя", false},
		{"пламя", "открытое пламя", false}, // 1 из 2 слов < 60%
		{"дым", "а б", false}, // нет значимых слов
	}
	for _, c := range cases {
		if got := forbiddenPresent(tokens(norm(c.text)), c.fact); got != c.want {
			t.Errorf("forbiddenPresent(%q, %q) = %v", c.text, c.fact, got)
		}
	}
}
