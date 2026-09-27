package dds

import (
	"strings"
	"testing"
	"time"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/reaction"
)

var t0 = time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)

func lookup(code string) (core.ServiceInfo, bool) {
	names := map[string]string{"101": "Пожарные", "103": "Скорая", "zhkh": "ДДС ЖКХ", "vodokanal": "Мосводоканал"}
	n, ok := names[code]
	if !ok {
		return core.ServiceInfo{}, false
	}
	return core.ServiceInfo{ID: "id-" + code, Code: code, Name: n, ShortName: n, Kind: "city"}, true
}

func TestSplitCodes(t *testing.T) {
	t.Parallel()
	if got := SplitCodes(""); got != nil {
		t.Errorf("пусто: %v", got)
	}
	got := SplitCodes(" 101, zhkh ,,vodokanal")
	if strings.Join(got, "|") != "101|zhkh|vodokanal" {
		t.Errorf("got %v", got)
	}
}

func TestPick(t *testing.T) {
	t.Parallel()
	codes := []string{"101", "zhkh"}
	cases := []struct {
		name    string
		codes   []string
		profile string
		want    string
		ok      bool
	}{
		{"профиль в списке оповещения", codes, "zhkh", "zhkh", true},
		{"регистр кода не важен", codes, "ZHKH", "zhkh", true},
		{"непрофильная карточка", codes, "gormost", "", false},
		{"без профиля — основная служба", codes, "", "101", true},
		{"без профиля и без служб", nil, "", "", false},
		{"профиль и нет служб", nil, "zhkh", "", false},
	}
	for _, tc := range cases {
		got, ok := Pick(tc.codes, tc.profile)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: got %q,%v want %q,%v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestIncomingCard(t *testing.T) {
	t.Parallel()
	et := convert.EmptyDraft()
	et.Description = "Прорыв трубы во дворе"
	et.ActionsTaken = "Эталонные действия оператора 112"
	et.IncidentTypeIds = []string{"it-1"}
	et.Attributes = map[string]any{"where": "двор"}
	et.Services = []public.AssignedService{{Code: "zhkh", IsPrimary: true, Reason: ptr("зона ответственности")}}

	card := IncomingCard(&et, []string{"101", "zhkh", "unknown"}, "vodokanal", "+7 999 000-00-00", lookup, t0)

	if card.ActionsTaken != "" {
		t.Errorf("actionsTaken эталона утёк в карточку: %q", card.ActionsTaken)
	}
	if card.Description != et.Description || convert.Deref(card.Phones.Aon) != "+7 999 000-00-00" {
		t.Errorf("поля 112: %+v", card)
	}
	var codes []string
	for _, sv := range card.Services {
		codes = append(codes, sv.Code)
		if sv.CurrentStatus != reaction.StatusReceived || len(sv.History) != 2 || len(sv.AllowedNext) == 0 {
			t.Errorf("служба %s: %+v", sv.Code, sv)
		}
		if sv.Source != public.AssignedServiceSourceAuto {
			t.Errorf("источник службы %s: %s", sv.Code, sv.Source)
		}
	}
	// неизвестная справочнику служба пропущена, служба обучающегося добавлена в конец
	if strings.Join(codes, "|") != "101|zhkh|vodokanal" {
		t.Errorf("службы: %v", codes)
	}
	if !card.Services[1].IsPrimary || card.Services[0].IsPrimary || convert.Deref(card.Services[1].Reason) != "зона ответственности" {
		t.Errorf("основная служба/причина из эталона: %+v", card.Services)
	}

	// Карточка не делит срезы и карты с эталоном.
	card.IncidentTypeIds[0] = "x"
	card.Attributes["where"] = "y"
	if et.IncidentTypeIds[0] != "it-1" || et.Attributes["where"] != "двор" {
		t.Error("карточка разделяет память с эталоном")
	}

	// АОН, заполненный в эталоне, не перетирается; без эталона — пустая карточка со службами.
	et.Phones.Aon = ptr("111")
	if c := IncomingCard(&et, []string{"zhkh"}, "zhkh", "222", lookup, t0); convert.Deref(c.Phones.Aon) != "111" {
		t.Errorf("АОН эталона: %v", convert.Deref(c.Phones.Aon))
	}
	c := IncomingCard(nil, []string{"103", "zhkh"}, "zhkh", "", lookup, t0)
	if len(c.Services) != 2 || !c.Services[0].IsPrimary || c.IncidentTypeIds == nil {
		t.Errorf("без эталона: %+v", c)
	}
}

// card — карточка с историей службы zhkh: взята в работу в t0, записи — смещения в секундах.
func card(entries ...public.ReactionStatusEntry) *public.IncidentCardDraft {
	c := IncomingCard(nil, []string{"101", "zhkh"}, "zhkh", "", lookup, t0)
	sv := FindService(&c, "zhkh")
	sv.History = append(sv.History, entries...)
	return &c
}

func at(sec int, st public.ReactionStatus, comment string) public.ReactionStatusEntry {
	e := public.ReactionStatusEntry{Status: st, At: t0.Add(time.Duration(sec) * time.Second), Operator: "оп. 227"}
	if comment != "" {
		e.Comment = &comment
	}
	return e
}

func TestEvaluate(t *testing.T) {
	t.Parallel()
	opened := t0
	accept := &model.ReactionExpectation{Decision: model.DecisionAccept, DecisionWithinSec: 30,
		RequiredStatuses: []string{string(reaction.StatusStarted)}}
	reject := &model.ReactionExpectation{Decision: model.DecisionReject}

	type want struct {
		score  float64
		fields []string
	}
	cases := []struct {
		name string
		card *public.IncidentCardDraft
		exp  *model.ReactionExpectation
		want want
	}{
		{"всё по протоколу", card(at(12, reaction.StatusAccepted, ""), at(40, reaction.StatusStarted, "Бригада выехала")),
			accept, want{100, nil}},
		{"решение позже норматива", card(at(45, reaction.StatusAccepted, ""), at(50, reaction.StatusStarted, "")),
			accept, want{71, []string{FieldDecisionTime}}}, // потеряно 2 из 7
		{"нет решения", card(), accept, want{14, []string{FieldDecision, FieldDecisionTime, FieldStatuses}}},
		{"отказ вместо приёма", card(at(10, reaction.StatusNotAccepted, "Не наша зона")),
			accept, want{29, []string{FieldDecision, FieldRefusal, FieldStatuses}}}, // решение вовремя: потеряно 5 из 7
		{"отказ, затем приём", card(at(10, reaction.StatusNotAccepted, "ошибся"), at(20, reaction.StatusAccepted, ""),
			at(25, reaction.StatusStarted, "")), accept, want{86, []string{FieldRefusal}}},
		{"верный отказ", card(at(8, reaction.StatusNotAccepted, "Реагирование по карточке 123")),
			reject, want{100, nil}},
		{"принял, а надо было отказать", card(at(8, reaction.StatusAccepted, "")),
			reject, want{40, []string{FieldDecision}}},
		{"эталон не задан — «Принята» за 30 с", card(at(29, reaction.StatusAccepted, "")), nil, want{100, nil}},
	}
	for _, tc := range cases {
		score, errs := Evaluate(tc.card, "zhkh", tc.exp, &opened)
		var fields []string
		for _, e := range errs {
			fields = append(fields, e.Field)
			if e.Label == "" || e.Weight <= 0 {
				t.Errorf("%s: неполная ошибка %+v", tc.name, e)
			}
		}
		if score != tc.want.score || strings.Join(fields, ",") != strings.Join(tc.want.fields, ",") {
			t.Errorf("%s: score %v errs %v, want %v %v", tc.name, score, fields, tc.want.score, tc.want.fields)
		}
		if errs == nil {
			t.Errorf("%s: errs == nil (обязательный массив)", tc.name)
		}
	}

	// Время взятия в работу неизвестно — норматив не подтвердить.
	_, errs := Evaluate(card(at(5, reaction.StatusAccepted, "")), "zhkh", nil, nil)
	if len(errs) != 1 || errs[0].Field != FieldDecisionTime {
		t.Errorf("без openedAt: %+v", errs)
	}
	// Статусы чужой службы не засчитываются своей.
	c := card()
	FindService(c, "101").History = append(FindService(c, "101").History, at(3, reaction.StatusAccepted, ""))
	if score, _ := Evaluate(c, "zhkh", nil, &opened); score != 17 { // потеряно 3+2 из 6
		t.Errorf("чужая служба: %v", score)
	}
	// Фактическое время — в отчёте секундами.
	_, errs = Evaluate(card(at(42, reaction.StatusAccepted, "")), "zhkh", nil, &opened)
	if len(errs) != 1 || errs[0].Actual != "42 с" || errs[0].Expected != "не позже 30 с" {
		t.Errorf("время: %+v", errs)
	}
}

func TestComments(t *testing.T) {
	t.Parallel()
	c := card(at(5, reaction.StatusNotAccepted, " Не наша зона "), at(9, reaction.StatusAccepted, ""),
		at(20, reaction.StatusStarted, "Бригада выехала"))
	if got := Comments(c, "zhkh"); got != "Не наша зона\nБригада выехала" {
		t.Errorf("got %q", got)
	}
	if got := Comments(c, "101"); got != "" {
		t.Errorf("чужая служба: %q", got)
	}
}

func TestResolved(t *testing.T) {
	t.Parallel()
	var nilExp *model.ReactionExpectation
	if r := nilExp.Resolved(); r.Decision != model.DecisionAccept || r.DecisionWithinSec != 30 || len(r.RequiredStatuses) != 0 {
		t.Errorf("nil: %+v", r)
	}
	r := (&model.ReactionExpectation{Decision: "maybe", DecisionWithinSec: 1}).Resolved()
	if r.Decision != model.DecisionAccept || r.DecisionWithinSec != model.MinDecisionWithinSec {
		t.Errorf("кривые значения: %+v", r)
	}
	r = (&model.ReactionExpectation{Decision: model.DecisionReject, DecisionWithinSec: 9999}).Resolved()
	if r.Decision != model.DecisionReject || r.DecisionWithinSec != model.MaxDecisionWithinSec {
		t.Errorf("reject/max: %+v", r)
	}
}

func ptr[T any](v T) *T { return &v }
