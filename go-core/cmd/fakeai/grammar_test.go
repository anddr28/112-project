package main

import (
	"reflect"
	"strings"
	"testing"

	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/components"
)

type gramText = struct {
	Field string `json:"field"`
	Text  string `json:"text"`
}

func grammarReq(t *testing.T, texts []gramText, options obj) *aiservice.GrammarJobRequest {
	t.Helper()
	p := grammarPayload()
	items := make([]any, 0, len(texts))
	for _, tx := range texts {
		items = append(items, obj{"field": tx.Field, "text": tx.Text})
	}
	p["texts"] = items
	if options != nil {
		p["options"] = options
	}
	req := new(aiservice.GrammarJobRequest)
	if err := decodeInto(mustJSON(t, p), req); err != nil {
		t.Fatal(err)
	}
	return req
}

type wantRemark struct {
	rule           string
	offset, length int
	sev            components.GrammarRemarkSeverity
	sugg           string // первая подсказка ("" — не проверять)
}

func TestCheckGrammarRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		text  string
		want  []wantRemark
		score float32
	}{
		{"чистый текст", "Горит квартира.", nil, 100},
		{"пустой", "", nil, 100},
		{"пробелы", "   \n ", nil, 100},
		{"строчная в начале", "горит квартира.", []wantRemark{{ruleUpperStart, 0, 1, components.Error, "Г"}}, 85},
		{"нет точки", "Горит квартира", []wantRemark{{ruleFinalPunct, 13, 1, components.Warning, "."}}, 93},
		{"нет точки, хвостовые пробелы", "Горит квартира  ", []wantRemark{{ruleFinalPunct, 13, 1, components.Warning, "."}}, 93},
		{"опечатка", "Дым в подьезде.", []wantRemark{{ruleSpelling, 6, 8, components.Error, "подъезде"}}, 85},
		{"опечатка с заглавной", "Подьезд задымлен.", []wantRemark{{ruleSpelling, 0, 7, components.Error, "Подъезд"}}, 85},
		{"опечатка в два слова", "Незнаю.", []wantRemark{{ruleSpelling, 0, 6, components.Error, "Не знаю"}}, 85},
		{"сокращения — стиль, не влияют на балл без strict", "Горит кв. 5 на ул. Ленина.", []wantRemark{
			{ruleAbbrev, 6, 3, components.Style, "квартира"},
			{ruleAbbrev, 15, 3, components.Style, "улица"},
		}, 100},
		{"сокращение без точки", "Горит кв 5.", []wantRemark{{ruleAbbrev, 6, 2, components.Style, "квартира"}}, 100},
		{"двойной пробел", "Горит  квартира.", []wantRemark{{ruleDoubleSp, 5, 2, components.Style, " "}}, 100},
		{"новое предложение после точки", "Горит. дым идёт.", []wantRemark{{ruleUpperStart, 7, 1, components.Error, "Д"}}, 85},
		{"новое предложение после перевода строки", "Горит квартира\nдым идёт.", []wantRemark{{ruleUpperStart, 15, 1, components.Error, "Д"}}, 85},
		{"после ? и !", "Горит? да! Горит.", []wantRemark{{ruleUpperStart, 7, 1, components.Error, "Д"}}, 85},
		{"латиница в начале не проверяется", "ok, горит.", nil, 100},
		{"цифра в начале", "5 этаж горит.", nil, 100},
		{"закрывающая кавычка", "Он сказал «горит»", []wantRemark{{ruleFinalPunct, 15, 1, components.Warning, "."}}, 93},
		{"кавычка после точки", "Он сказал «горит.»", nil, 100},
		{"UTF-16 смещения после эмодзи", "🔥 горит.", []wantRemark{{ruleUpperStart, 3, 1, components.Error, "Г"}}, 85},
		{"UTF-16 длина до конца", "🔥🔥 Дым в подьезде.", []wantRemark{{ruleSpelling, 11, 8, components.Error, "подъезде"}}, 85},
		{"сортировка по смещению", "горит в подьезде", []wantRemark{
			{ruleUpperStart, 0, 1, components.Error, "Г"},
			{ruleSpelling, 8, 8, components.Error, "подъезде"},
			{ruleFinalPunct, 15, 1, components.Warning, "."},
		}, 100 - 15*2 - 7},
		{"балл не ниже нуля", strings.Repeat("сдесь ", 10), nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			res := checkGrammar(grammarReq(t, []gramText{{"description", c.text}}, nil))
			if res.Score != c.score {
				t.Errorf("score %v, ждали %v (remarks %+v)", res.Score, c.score, res.Remarks)
			}
			if res.Remarks == nil {
				t.Fatal("remarks nil")
			}
			if c.want == nil && c.score == 0 {
				return // только балл
			}
			if len(res.Remarks) != len(c.want) {
				t.Fatalf("remarks %d, ждали %d: %+v", len(res.Remarks), len(c.want), res.Remarks)
			}
			for i, w := range c.want {
				r := res.Remarks[i]
				if deref(r.Rule) != w.rule || r.Offset != w.offset || r.Length != w.length || r.Severity != w.sev ||
					r.Field != "description" || r.Message == "" {
					t.Errorf("remark %d: %+v (rule %s), ждали %+v", i, r, deref(r.Rule), w)
				}
				if r.Suggestions == nil {
					t.Errorf("remark %d: suggestions nil", i)
				} else if w.sugg != "" && (len(*r.Suggestions) == 0 || (*r.Suggestions)[0] != w.sugg) {
					t.Errorf("remark %d: suggestions %v, ждали %q", i, *r.Suggestions, w.sugg)
				}
			}
		})
	}
}

// Регрессия: options.strict (контракт: default false) — style-замечания снижают балл
// только при strict=true; раньше имитатор штрафовал за стиль всегда.
func TestCheckGrammarStrictStyle(t *testing.T) {
	t.Parallel()
	texts := []gramText{{"description", "Горит кв. 5 на ул. Ленина,  д. 14."}}
	for _, c := range []struct {
		name    string
		options obj
		score   float32
	}{
		{"без options", nil, 100},
		{"strict по умолчанию", obj{"lang": "ru"}, 100},
		{"strict=false", obj{"strict": false}, 100},
		{"strict=true", obj{"strict": true}, 100 - 3*4},
	} {
		res := checkGrammar(grammarReq(t, texts, c.options))
		if len(res.Remarks) != 4 {
			t.Fatalf("%s: remarks %+v", c.name, res.Remarks)
		}
		if got := (*res.Stats.ErrorsBySeverity)["style"]; got != 4 {
			t.Fatalf("%s: style %d", c.name, got)
		}
		if res.Score != c.score {
			t.Errorf("%s: score %v, ждали %v", c.name, res.Score, c.score)
		}
	}
	// Ошибки и предупреждения штрафуются в обоих режимах одинаково.
	for _, strict := range []bool{false, true} {
		res := checkGrammar(grammarReq(t, []gramText{{"description", "горит квартира"}}, obj{"strict": strict}))
		if res.Score != 100-15-7 {
			t.Errorf("strict=%v: score %v", strict, res.Score)
		}
	}
}

func TestCheckGrammarExcludeRules(t *testing.T) {
	t.Parallel()
	texts := []gramText{{"description", "горит кв. 5"}}
	res := checkGrammar(grammarReq(t, texts, obj{"exclude_rules": []any{ruleUpperStart, ruleFinalPunct}}))
	if len(res.Remarks) != 1 || deref(res.Remarks[0].Rule) != ruleAbbrev || res.Score != 100 {
		t.Fatalf("remarks %+v score %v", res.Remarks, res.Score)
	}
	res = checkGrammar(grammarReq(t, texts, obj{"exclude_rules": []any{}}))
	if len(res.Remarks) != 3 {
		t.Fatalf("пустой exclude_rules: %+v", res.Remarks)
	}
}

func TestCheckGrammarMultipleTextsAndStats(t *testing.T) {
	t.Parallel()
	req := grammarReq(t, []gramText{
		{"description", "горит."},
		{"actions_taken", "Направлены службы"},
		{"empty", ""},
	}, nil)
	res := checkGrammar(req)
	if len(res.Remarks) != 2 {
		t.Fatalf("remarks %+v", res.Remarks)
	}
	if res.Remarks[0].Field != "description" || res.Remarks[0].Offset != 0 ||
		res.Remarks[1].Field != "actions_taken" || res.Remarks[1].Offset != 16 {
		t.Fatalf("поля/смещения считаются по каждому тексту отдельно: %+v", res.Remarks)
	}
	if res.Stats.WordsChecked != 3 {
		t.Fatalf("words_checked %d", res.Stats.WordsChecked)
	}
	want := map[string]int{"error": 1, "warning": 1, "style": 0}
	if !reflect.DeepEqual(*res.Stats.ErrorsBySeverity, want) {
		t.Fatalf("errors_by_severity %v", *res.Stats.ErrorsBySeverity)
	}
	if res.Score != 78 {
		t.Fatalf("score %v", res.Score)
	}
	// Детерминизм.
	if again := checkGrammar(req); !reflect.DeepEqual(again, res) {
		t.Fatal("недетерминированный результат")
	}
}

func TestMisspelled(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in, bad, good string
		ok            bool
	}{
		{"подьезде", "подьезд", "подъезд", true},
		{"пожалуста", "пожалуста", "пожалуйста", true},
		{"подъезд", "", "", false},
		{"", "", "", false},
		{"евакуация", "евакуац", "эвакуац", true},
	} {
		bad, good, ok := misspelled(c.in)
		if bad != c.bad || good != c.good || ok != c.ok {
			t.Errorf("misspelled(%q) = %q %q %v", c.in, bad, good, ok)
		}
	}
	// Замены сохраняют словоформу (префиксная замена).
	for _, m := range misspellings {
		if m.bad == m.good || m.bad == "" || m.good == "" {
			t.Errorf("плохая пара %+v", m)
		}
	}
}
