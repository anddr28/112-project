package evaluation

import (
	"testing"

	"lct/gocore/internal/scoring"
	"lct/gocore/internal/settings"
)

// Регрессия: вес разговора 0 при включённом голосе — осознанный выбор преподавателя;
// оценка не должна подмешивать долю по умолчанию (иначе UI показывает 0 %, а итог считает 25 %).
func TestEvaluationKeepsTeacherZeroDialogueWeight(t *testing.T) {
	t.Parallel()
	w := scoring.EffectiveWeights(settings.Weights{Fields: 0.5, Semantic: 0.25, Grammar: 0.1, Timing: 0.15}, true, 0)
	if w.Dialogue != 0 {
		t.Fatalf("dialogue weight = %v, want 0", w.Dialogue)
	}
	if s := w.Sum(); s < 0.999 || s > 1.001 {
		t.Fatalf("sum = %v", s)
	}
}
