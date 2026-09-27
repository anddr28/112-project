package scoring

import (
	"math"
	"strconv"

	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/settings"
)

// Слой 4 — тайминг.

// TimingResult — детализация тайминга. JSON-форма (snake_case) — ровно evaluations.timing
// jsonb: {spent_ms, limit_sec, delta_ms, within_norm, reaction_ms}; VIEW прогресса читает
// within_norm. Для фронта — ToPublic().
type TimingResult struct {
	SpentMs    int  `json:"spent_ms"`
	LimitSec   int  `json:"limit_sec"`
	DeltaMs    int  `json:"delta_ms"`
	WithinNorm bool `json:"within_norm"`
	ReactionMs int  `json:"reaction_ms"`
}

// PublicTiming — тип поля public.Evaluation.Timing (анонимная структура в сгенерированном
// коде; алиас с теми же полями и тегами — тот же тип, присваивается напрямую).
type PublicTiming = struct {
	DeltaMs    int  `json:"deltaMs"`
	LimitSec   int  `json:"limitSec"`
	ReactionMs int  `json:"reactionMs"`
	SpentMs    int  `json:"spentMs"`
	WithinNorm bool `json:"withinNorm"`
}

// Проверка при компиляции: PublicTiming совпадает с полем сгенерированного типа.
var _ = func(e *public.Evaluation) { e.Timing = PublicTiming{} }

// ToPublic — Evaluation.timing (camelCase).
func (r TimingResult) ToPublic() PublicTiming {
	return PublicTiming{
		DeltaMs:    r.DeltaMs,
		LimitSec:   r.LimitSec,
		ReactionMs: r.ReactionMs,
		SpentMs:    r.SpentMs,
		WithinNorm: r.WithinNorm,
	}
}

// Timing — балл и детализация: 100 до limit·(1+soft%), линейно до 0 к limit·(1+hard%).
// WithinNorm = spent ≤ limit·(1+soft%); DeltaMs = spent − limit·1000 (знаковая, UI рисует
// «+1 мин 12 с» / «−4 с»). reactionMs = first_input_at − call_accepted_at (0, если ввода не было).
// Норматив не задан (limitSec ≤ 0) — штрафовать не за что: 100, в норме.
func Timing(spentMs, limitSec, reactionMs int, tol settings.TimingTolerance) (score float64, r TimingResult) {
	if spentMs < 0 {
		spentMs = 0
	}
	if reactionMs < 0 {
		reactionMs = 0
	}
	if limitSec < 0 {
		limitSec = 0
	}
	r = TimingResult{
		SpentMs:    spentMs,
		LimitSec:   limitSec,
		DeltaMs:    spentMs - limitSec*1000,
		ReactionMs: reactionMs,
	}
	if limitSec == 0 {
		r.WithinNorm = true
		return 100, r
	}
	softPct := nonNeg(tol.SoftPct)
	hardPct := nonNeg(tol.HardPct)
	if hardPct < softPct {
		hardPct = softPct
	}
	limitMs := float64(limitSec) * 1000
	soft := limitMs * (1 + softPct/100)
	hard := limitMs * (1 + hardPct/100)
	spent := float64(spentMs)

	r.WithinNorm = spent <= soft
	switch {
	case spent <= soft:
		score = 100
	case spent >= hard || hard <= soft:
		score = 0
	default:
		score = 100 * (1 - (spent-soft)/(hard-soft))
	}
	return Round2(clamp100(score)), r
}

// ---------------------------------------------------------------- форматирование

// FormatDuration — «45 с», «2 мин», «1 мин 23 с» (как frontend utils/time.ts formatDuration:
// округление до секунд, отрицательное — 0).
func FormatDuration(ms int) string {
	if ms < 0 {
		ms = 0
	}
	total := ms / 1000 // округление без ms+500: у math.MaxInt оно переполняется в минус
	if ms%1000 >= 500 {
		total++
	}
	if total < 60 {
		return strconv.Itoa(total) + " с"
	}
	mm, ss := total/60, total%60
	if ss == 0 {
		return strconv.Itoa(mm) + " мин"
	}
	return strconv.Itoa(mm) + " мин " + strconv.Itoa(ss) + " с"
}

// FormatDelta — знаковая дельта к нормативу: «+1 мин 12 с» / «−4 с» (типографский минус, как во фронте).
func FormatDelta(ms int) string {
	if ms >= 0 {
		return "+" + FormatDuration(ms)
	}
	if ms == math.MinInt {
		ms++ // -MinInt переполняется
	}
	return "−" + FormatDuration(-ms)
}

// FormatClock — «MM:SS», как таймер АРМ-112 (formatClock во фронте).
func FormatClock(ms int) string {
	if ms < 0 {
		ms = 0
	}
	total := ms / 1000
	mm, ss := total/60, total%60
	b := make([]byte, 0, 8)
	if mm < 10 {
		b = append(b, '0')
	}
	b = strconv.AppendInt(b, int64(mm), 10)
	b = append(b, ':')
	if ss < 10 {
		b = append(b, '0')
	}
	b = strconv.AppendInt(b, int64(ss), 10)
	return string(b)
}
