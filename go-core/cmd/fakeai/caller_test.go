package main

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/components"
)

// turnReq — проверенный DialogTurnRequest из payload'а (тот же путь, что у хендлера).
func turnReq(t *testing.T, p obj) aiservice.DialogTurnRequest {
	t.Helper()
	req, err := parseDialogTurn(mustJSON(t, p))
	if err != nil {
		t.Fatalf("parseDialogTurn: %v", err)
	}
	return req
}

func turnWith(t *testing.T, opText string, history []any, revealed []any) obj {
	t.Helper()
	p := dialogTurnPayload()
	p["operator_text"] = opText
	if history != nil {
		p["history"] = history
	}
	if revealed != nil {
		p["revealed_fact_ids"] = revealed
	}
	return p
}

func reply(t *testing.T, p obj) components.CallerReply {
	t.Helper()
	req := turnReq(t, p)
	r := callerReply(&req, deref(req.OperatorText))
	if r.RevealedFactIds == nil || r.ShouldEnd == nil || r.IsUnknownAnswer == nil || strings.TrimSpace(r.Text) == "" {
		t.Fatalf("неполный CallerReply: %+v", r)
	}
	return r
}

// historyOps — вступление + n пар «оператор/заявитель».
func historyOps(n int) []any {
	h := []any{obj{"turn_no": 1, "speaker": "caller", "text": "Алло! У нас дым из квартиры идёт, Ленина 14!"}}
	for i := range n {
		h = append(h,
			obj{"turn_no": 2 + 2*i, "speaker": "operator", "text": "Слушаю вас."},
			obj{"turn_no": 3 + 2*i, "speaker": "caller", "text": "Быстрее!"})
	}
	return h
}

func TestCallerReplyScenarios(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		p        obj
		text     string // "" — не проверять
		revealed []string
		unknown  bool
		end      bool
		reason   string
		state    string
	}{
		{"факт по подсказке", turnWith(t, "Служба 112. Какой подъезд?", nil, nil),
			"Господи! Подъезд 3. Приезжайте скорее!", []string{"entrance"}, false, false, "", "паника"},
		{"два факта за раз", turnWith(t, "Какой подъезд? Люди внутри есть?", nil, nil),
			"", []string{"entrance", "person_inside"}, false, false, "", "паника"},
		{"раскрытый факт не повторяется — сам сообщает volunteer", turnWith(t, "Какой подъезд?", nil, []any{"entrance"}),
			"Господи! Дым из квартиры на 5 этаже. Приезжайте скорее!", []string{"smoke"}, false, false, "", "паника"},
		{"раскрытое в истории тоже учитывается", turnWith(t, "Какой подъезд?", []any{
			obj{"turn_no": 1, "speaker": "caller", "text": "Алло!", "revealed_fact_ids": []any{"entrance", "smoke"}},
		}, nil), "Я не знаю, что делать! Приезжайте скорее!", []string{}, false, false, "", "паника"},
		{"never не раскрывается", turnWith(t, "Сосед курит?", nil, []any{"smoke"}),
			"", []string{}, false, false, "", "паника"},
		// Без подсказок факт раскрывается по основам своих значимых слов: «квартиры» раскрыло бы smoke.
		{"вопрос о неизвестном", turnWith(t, "Какой номер квартиры соседа?", nil, []any{"smoke"}),
			"Ой, не знаю я! Не вижу отсюда…", []string{}, true, false, "", "паника"},
		{"помощь направлена после второй реплики", turnWith(t, "Помощь направлена, бригада выехала.", historyOps(1), []any{"smoke"}),
			"Спасибо! Я буду ждать и встречу их. Только быстрее, пожалуйста!", []string{}, false, true,
			"оператор сказал, что помощь направлена", "облегчение"},
		{"помощь направлена первой репликой — ещё рано", turnWith(t, "Помощь направлена.", nil, nil),
			"", []string{"smoke"}, false, false, "", "паника"},
		{"успокоил — заявитель успокаивается", turnWith(t, "Не волнуйтесь! Какой подъезд?", nil, nil),
			"", []string{"entrance"}, false, false, "", "успокаивается"},
		{"предел реплик", turnWith(t, "Какой подъезд?", historyOps(7), nil),
			maxTurnsGoodbye, []string{}, false, true, "max_turns", "паника"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := reply(t, c.p)
			if c.text != "" && r.Text != c.text {
				t.Errorf("text %q, ждали %q", r.Text, c.text)
			}
			if !reflect.DeepEqual(*r.RevealedFactIds, c.revealed) {
				t.Errorf("revealed %v, ждали %v", *r.RevealedFactIds, c.revealed)
			}
			if slices.Contains(*r.RevealedFactIds, "secret") {
				t.Error("раскрыт never-факт")
			}
			if *r.IsUnknownAnswer != c.unknown || *r.ShouldEnd != c.end {
				t.Errorf("unknown=%v end=%v", *r.IsUnknownAnswer, *r.ShouldEnd)
			}
			if deref(r.EndReason) != c.reason || (r.EndReason != nil) != c.end {
				t.Errorf("end_reason %v", r.EndReason)
			}
			if deref(r.EmotionalState) != c.state {
				t.Errorf("emotional_state %q, ждали %q", deref(r.EmotionalState), c.state)
			}
			// Детерминизм.
			if again := reply(t, c.p); !reflect.DeepEqual(again, r) {
				t.Errorf("недетерминированный ответ: %+v / %+v", r, again)
			}
		})
	}
}

func TestCallerReplyOptions(t *testing.T) {
	t.Parallel()
	p := turnWith(t, "Какой подъезд?", nil, nil)
	p["options"] = obj{"max_reply_words": 2}
	if r := reply(t, p); r.Text != "Господи! Подъезд…" {
		t.Fatalf("max_reply_words: %q", r.Text)
	}
	// Своя концовка из end_conditions отсутствует — стандартная причина.
	p = turnWith(t, "Помощь направлена.", historyOps(1), []any{"smoke"})
	delete(p["call_script"].(obj)["dialogue"].(obj), "end_conditions")
	if r := reply(t, p); !*r.ShouldEnd || deref(r.EndReason) != "оператор сообщил, что помощь направлена" {
		t.Fatalf("end_reason по умолчанию: %+v", r)
	}
	// Спокойный заявитель: другие формулировки, по завершении — «спокоен».
	p = turnWith(t, "Помощь направлена.", historyOps(1), []any{"smoke"})
	p["call_script"].(obj)["caller"].(obj)["emotional_state"] = "спокоен"
	if r := reply(t, p); r.Text != thanksLines[moodCalm] || deref(r.EmotionalState) != "спокоен" {
		t.Fatalf("спокойный: %+v", r)
	}
	// Регрессия: спокойная заявительница не превращается в «спокоен».
	p["call_script"].(obj)["caller"].(obj)["emotional_state"] = "спокойна"
	if r := reply(t, p); r.Text != thanksLines[moodCalm] || deref(r.EmotionalState) != "спокойна" {
		t.Fatalf("спокойная: %q %q", r.Text, deref(r.EmotionalState))
	}
	// Без emotional_state — «встревожен» по умолчанию, поле не выдумывается.
	p = turnWith(t, "Какой подъезд?", nil, nil)
	delete(p["call_script"].(obj)["caller"].(obj), "emotional_state")
	if r := reply(t, p); r.EmotionalState != nil || !strings.Contains(r.Text, "Подъезд 3.") {
		t.Fatalf("без состояния: %+v", r)
	}
}

// Без брифа (текстовые сценарии) — сценарные реплики заявителя по кругу.
func TestCallerReplyWithoutBrief(t *testing.T) {
	t.Parallel()
	base := dialogTurnPayload()
	cs := base["call_script"].(obj)
	delete(cs, "dialogue")
	cs["turns"] = []any{
		obj{"speaker": "caller", "text": "Алло, пожар!"},
		obj{"speaker": "caller", "text": "Третий подъезд!"},
		obj{"speaker": "operator", "text": "Понял."},
		obj{"speaker": "caller", "text": "Быстрее!"},
	}
	for ops, want := range map[int]string{0: "Третий подъезд!", 1: "Быстрее!", 2: "Третий подъезд!"} {
		p := clone(t, base)
		p["operator_text"] = "Что случилось?"
		p["history"] = historyOps(ops)
		if r := reply(t, p); r.Text != want || len(*r.RevealedFactIds) != 0 {
			t.Errorf("история из %d реплик: %q, ждали %q", ops, r.Text, want)
		}
	}
	// Только вступление — «паника» по настроению.
	cs["turns"] = []any{obj{"speaker": "caller", "text": "Алло, пожар!"}}
	base["operator_text"] = "Что случилось?"
	if r := reply(t, base); r.Text != panicLines[moodPanic][0] {
		t.Fatalf("без реплик: %q", r.Text)
	}
}

func TestMoodOf(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]mood{
		"": moodAgitated, "встревожен": moodAgitated, "взволнована": moodAgitated, "паника": moodPanic,
		"Напугана": moodPanic, "ПЛАЧЕТ": moodPanic, "кричит": moodPanic, "спокоен": moodCalm,
		"облегчение": moodCalm, "рассудительна": moodCalm, "злой": moodAgitated,
		// Регрессия: «спокоен» (так пишет emotionFor для мужчины) — спокойный, а не встревоженный.
		"спокойна": moodCalm, "Спокоен, отвечает коротко": moodCalm,
		"беспокойна": moodAgitated, "беспокоится за соседа": moodAgitated, "успокаивается": moodAgitated,
	} {
		if got := moodOf(in); got != want {
			t.Errorf("moodOf(%q) = %d, ждали %d", in, got, want)
		}
	}
}

func TestFakeSTT(t *testing.T) {
	t.Parallel()
	silence := fakeSTT(1, audioInfo{size: minSpeechBytes - 1, durationMs: 300})
	if !deref(silence.NoSpeech) || silence.Text != "" || deref(silence.Confidence) != 0 || silence.Segments == nil ||
		len(*silence.Segments) != 0 || silence.AudioDurationMs != 300 {
		t.Fatalf("тишина: %+v", silence)
	}
	for turn := -1; turn <= 11; turn++ {
		r := fakeSTT(turn, audioInfo{size: minSpeechBytes, durationMs: 2500})
		want := sttPhrases[(max(turn, 1)-1)%len(sttPhrases)]
		if r.Text != want || deref(r.NoSpeech) || deref(r.Confidence) != 0.5 || deref(r.Language) != "ru" {
			t.Fatalf("turn %d: %+v", turn, r)
		}
		seg := *r.Segments
		if len(seg) != 1 || seg[0].EndMs != 2500 || seg[0].Text != want {
			t.Fatalf("segments %+v", seg)
		}
		if raw := deref(r.TextRaw); raw != strings.ToLower(raw) || strings.ContainsAny(raw, ".,?!") {
			t.Fatalf("text_raw %q", raw)
		}
	}
}

func TestRawASR(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"Служба 112, слушаю вас. Что у вас случилось?": "служба сто двенадцать слушаю вас что у вас случилось",
		"Кто-нибудь пострадал?":                        "кто-нибудь пострадал",
		"": "",
	} {
		if got := rawASR(in); got != want {
			t.Errorf("rawASR(%q) = %q", in, got)
		}
	}
}

func TestAskedUnknown(t *testing.T) {
	t.Parallel()
	unknowns := []string{"Причина возгорания", "Фамилия соседа из горящей квартиры"}
	for text, want := range map[string]bool{
		"а что стало причиной возгорания?": true,
		"как фамилия соседа?":              true, // 2 из 4 основ — половина
		"сосед дома?":                      false,
		"":                                 false,
		"из-за чего возгорание, какая причина": true,
	} {
		if got := askedUnknown(unknowns, norm(text)); got != want {
			t.Errorf("askedUnknown(%q) = %v", text, got)
		}
	}
	if askedUnknown([]string{"", "да"}, "да") {
		t.Error("пункт без значимых слов не должен срабатывать")
	}
}
