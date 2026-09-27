// Package scoring — детерминированная оценка попытки без БД и HTTP: слой 1 (поля карточки
// против эталона), слой 4 (тайминг), веса слоёв, итог, вердикт, рекомендации, XP.
//
// Всё здесь — чистые функции: одинаковый вход даёт одинаковый результат (важно для
// переоценки, отчётов и тестов). AI-слои (grammar/semantic/dialogue) считает ai-service,
// сюда приходят только их баллы. Поведение повторяет моки фронта
// (frontend/src/shared/mocks/db.ts), улучшения — по go-core/DESIGN.md §5 «Оценка».
package scoring

import (
	"math"

	"lct/gocore/internal/core"
	"lct/gocore/internal/settings"
)

// Round2 — округление до сотых (evaluations.*_score — numeric(5,2)). NaN/Inf → 0, без «-0».
func Round2(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	r := math.Round(x*100) / 100
	if r == 0 {
		return 0
	}
	return r
}

// round4 — веса в JSON: 0.4167 вместо 0.41666666666666663.
func round4(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	r := math.Round(x*10000) / 10000
	if r == 0 {
		return 0
	}
	return r
}

// Verdict — pass, если итог не ниже порога (сравнение по сотым: 69.995 уже округлён в 70).
func Verdict(final, threshold float64) string {
	if Round2(final) >= Round2(threshold) {
		return core.VerdictPass
	}
	return core.VerdictFail
}

// ---------------------------------------------------------------- веса

// EffectiveWeights — фактические веса итога для занятия (evaluations.weights):
//   - голос выключен → dialogue = 0, остальные перенормируются к сумме 1;
//   - голос включён, а dialogue = 0 → dialogue = dialogueDefault, остальные пропорционально
//     ужимаются до (1 − dialogueDefault) — как withDialogueWeight во фронте: веса по умолчанию
//     рассчитаны на занятие без разговора, и без этого разговор не засчитался бы вовсе;
//   - затем нормировка к сумме 1.
//
// Отрицательные/NaN веса считаются нулём. Все нули → веса по умолчанию из settings.
func EffectiveWeights(w settings.Weights, voiceEnabled bool, dialogueDefault float64) settings.Weights {
	if r, ok := effectiveWeights(w, voiceEnabled, dialogueDefault); ok {
		return r
	}
	d := settings.Defaults()
	if r, ok := effectiveWeights(d.ScoreWeights, voiceEnabled, d.DialogueWeightDefault); ok {
		return r
	}
	// Недостижимо при вменяемых дефолтах; поровну — чтобы итог всё равно считался.
	if voiceEnabled {
		return settings.Weights{Fields: 0.2, Semantic: 0.2, Grammar: 0.2, Timing: 0.2, Dialogue: 0.2}
	}
	return settings.Weights{Fields: 0.25, Semantic: 0.25, Grammar: 0.25, Timing: 0.25}
}

func effectiveWeights(w settings.Weights, voiceEnabled bool, dialogueDefault float64) (settings.Weights, bool) {
	w = settings.Weights{
		Fields: nonNeg(w.Fields), Semantic: nonNeg(w.Semantic), Grammar: nonNeg(w.Grammar),
		Timing: nonNeg(w.Timing), Dialogue: nonNeg(w.Dialogue),
	}
	switch {
	case !voiceEnabled:
		w.Dialogue = 0
	case w.Dialogue == 0:
		d := nonNeg(dialogueDefault)
		if d >= 1 {
			d = settings.Defaults().DialogueWeightDefault
		}
		if d > 0 {
			if rest := w.Fields + w.Semantic + w.Grammar + w.Timing; rest > 0 {
				k := (1 - d) / rest
				w.Fields *= k
				w.Semantic *= k
				w.Grammar *= k
				w.Timing *= k
			}
			w.Dialogue = d
		}
	}
	sum := w.Sum()
	if !(sum > 0) || math.IsInf(sum, 0) {
		return settings.Weights{}, false
	}
	return settings.Weights{
		Fields: w.Fields / sum, Semantic: w.Semantic / sum, Grammar: w.Grammar / sum,
		Timing: w.Timing / sum, Dialogue: w.Dialogue / sum,
	}, true
}

func nonNeg(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) || x < 0 {
		return 0
	}
	return x
}

// RoundWeights — веса до 4 знаков для хранения/ответа (итог от этого не меняется:
// Total делит на сумму фактически использованных весов).
func RoundWeights(w settings.Weights) settings.Weights {
	return settings.Weights{
		Fields: round4(w.Fields), Semantic: round4(w.Semantic), Grammar: round4(w.Grammar),
		Timing: round4(w.Timing), Dialogue: round4(w.Dialogue),
	}
}

// PublicWeights — Evaluation.weights для фронта: все пять слоёв всегда присутствуют
// (UI подписывает доли плиток по этим ключам).
func PublicWeights(w settings.Weights) map[string]float32 {
	return map[string]float32{
		core.LayerFields:   float32(round4(w.Fields)),
		core.LayerSemantic: float32(round4(w.Semantic)),
		core.LayerGrammar:  float32(round4(w.Grammar)),
		core.LayerTiming:   float32(round4(w.Timing)),
		core.LayerDialogue: float32(round4(w.Dialogue)),
	}
}

// WeightsFromMap — веса из evaluations.weights / lessons.settings.weights, разобранных в map.
func WeightsFromMap(m map[string]float64) settings.Weights {
	return settings.Weights{
		Fields: m[core.LayerFields], Semantic: m[core.LayerSemantic], Grammar: m[core.LayerGrammar],
		Timing: m[core.LayerTiming], Dialogue: m[core.LayerDialogue],
	}
}

// ---------------------------------------------------------------- итог

// LayerScores — баллы слоёв; nil — слоя нет (ещё считается, skipped или failed).
type LayerScores struct {
	Fields, Semantic, Grammar, Timing, Dialogue *float64
}

// Total — итог = Σ(score·w) / Σ(w по слоям с баллом и w > 0); ни одного такого слоя → 0.
// Ключи scores — core.Layer*. Пока AI-слои не доехали, итог «частичный» по готовым слоям.
func Total(scores map[string]*float64, w settings.Weights) float64 {
	return TotalOf(LayerScores{
		Fields:   scores[core.LayerFields],
		Semantic: scores[core.LayerSemantic],
		Grammar:  scores[core.LayerGrammar],
		Timing:   scores[core.LayerTiming],
		Dialogue: scores[core.LayerDialogue],
	}, w)
}

// TotalOf — то же без map (горячий путь пересчёта при каждом доехавшем слое).
func TotalOf(s LayerScores, w settings.Weights) float64 {
	var sum, used float64
	add := func(score *float64, weight float64) {
		if score == nil || math.IsNaN(*score) || !(weight > 0) || math.IsInf(weight, 0) {
			return
		}
		sum += clamp100(*score) * weight
		used += weight
	}
	add(s.Fields, w.Fields)
	add(s.Semantic, w.Semantic)
	add(s.Grammar, w.Grammar)
	add(s.Timing, w.Timing)
	add(s.Dialogue, w.Dialogue)
	if used == 0 {
		return 0
	}
	return Round2(clamp100(sum / used))
}

// ---------------------------------------------------------------- XP

// Причины начисления (xp_ledger.reason, CHECK в 00002).
const (
	XPReasonAttemptEvaluated = "attempt_evaluated"
	XPReasonWithinNorm       = "within_norm"
	XPReasonPassBonus        = "pass_bonus"
	XPReasonLessonCompleted  = "lesson_completed"
)

type XPInput struct {
	Final      float64 // итоговый балл (с учётом override)
	Passed     bool
	WithinNorm bool
}

type XPEntry struct {
	Reason string
	Delta  int
}

// XPEntries — начисления за оценённую попытку (идемпотентность — уникальный индекс
// (attempt_id, reason) в БД). Нулевые/отрицательные правила не пишутся: строка
// «+0 XP» в журнале ничего не объясняет. lesson_completed начисляет evaluation отдельно
// (зависит от остальных попыток занятия) — правило r.LessonCompleted.
func XPEntries(in XPInput, r settings.XPRules) []XPEntry {
	out := make([]XPEntry, 0, 3)
	if r.AttemptEvaluated > 0 {
		out = append(out, XPEntry{Reason: XPReasonAttemptEvaluated, Delta: r.AttemptEvaluated})
	}
	if in.WithinNorm && r.WithinNorm > 0 {
		out = append(out, XPEntry{Reason: XPReasonWithinNorm, Delta: r.WithinNorm})
	}
	if in.Passed && r.PassBonusPer10Points > 0 {
		f := clamp100(Round2(in.Final))
		if bonus := r.PassBonusPer10Points * int(math.Floor(f/10)); bonus > 0 {
			out = append(out, XPEntry{Reason: XPReasonPassBonus, Delta: bonus})
		}
	}
	return out
}

// XPTotal — сумма начислений (Evaluation.xpEarned).
func XPTotal(entries []XPEntry) int {
	n := 0
	for _, e := range entries {
		n += e.Delta
	}
	return n
}
