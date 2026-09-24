package scoring

import (
	"slices"
	"strconv"
	"strings"

	"lct/gocore/internal/gen/public"
)

// Рекомендации строятся только по фактическим данным этой попытки (её норматив, её время,
// её поля, её разговор) — универсальных формулировок, способных разойтись с цифрами на
// экране, нет (как buildRecommendations в frontend/src/shared/mocks/db.ts; тексты — оттуда).

// Пороги рекомендаций.
const (
	GrammarRemarksThreshold = 2  // рекомендация по грамматике — при замечаниях > 2 (как во фронте)
	FillerThreshold         = 3  // по словам-паразитам — при количестве > 3
	DialogueLowScore        = 60 // общий совет по разговору — балл ниже, а конкретных замечаний нет
	maxFillerItems          = 5
)

// Стабильные id рекомендаций: одна попытка — не больше одной рекомендации каждого вида,
// повторный расчёт даёт те же id (фронт использует id как key).
const (
	RecMissing           = "rec-missing"
	RecWrong             = "rec-wrong"
	RecExtra             = "rec-extra"
	RecTiming            = "rec-timing"
	RecFacts             = "rec-facts"
	RecDialogueQuestions = "rec-dialogue-questions"
	RecDialogueForbidden = "rec-dialogue-forbidden"
	RecDialogueFillers   = "rec-dialogue-fillers"
	RecDialogueScore     = "rec-dialogue-score"
	RecGrammar           = "rec-grammar"
)

// RecInput — факты попытки для рекомендаций.
type RecInput struct {
	FieldErrors []public.FieldError
	Timing      TimingResult
	// CategoryName — название категории сценария («Отработайте тему «…»»).
	CategoryName string
	// SemanticMissingFacts — semantic.missing_facts от ai-service.
	SemanticMissingFacts []string
	// GrammarRemarks — число замечаний грамматики.
	GrammarRemarks int
	// DialogueMissing — обязательные вопросы протокола, которые не прозвучали
	// (dialogue.missing_questions или required-пункты чек-листа со статусом missed).
	DialogueMissing []string
	// DialogueForbiddenHits — недопустимые фразы оператора (dialogue.forbidden_hits[].phrase).
	DialogueForbiddenHits []string
	// DialogueScore — балл слоя dialogue (nil — голос выключен или слой не доехал).
	DialogueScore *float64
	// SpeechFillerCount / SpeechFillers — dialogue.speech.filler_count / fillers.
	SpeechFillerCount int
	SpeechFillers     map[string]int
}

// Recommendations — рекомендации по попытке. Никогда не nil.
func Recommendations(in RecInput) []public.Recommendation {
	out := make([]public.Recommendation, 0, 4)

	var missing, wrong, extra []string
	extraGeneric := false
	for i := range in.FieldErrors {
		e := &in.FieldErrors[i]
		switch e.Kind {
		case public.FieldErrorKindMissing:
			missing = appendUnique(missing, e.Label)
		case public.FieldErrorKindWrong:
			wrong = appendUnique(wrong, e.Label)
		case public.FieldErrorKindExtra:
			if CanonicalPath(e.Field) == "services" {
				diff := listDiff(e.Actual, e.Expected)
				for _, s := range diff {
					extra = appendUnique(extra, s)
				}
				if len(diff) == 0 {
					// Лишние службы не выделить по именам (две службы с одним именем, пустой
					// actual) — штраф в балле есть, значит, и рекомендация должна быть.
					extra = appendUnique(extra, e.Label)
				}
			} else {
				extraGeneric = true
				extra = appendUnique(extra, e.Label)
			}
		}
	}
	if len(missing) > 0 {
		out = append(out, rec(RecMissing, public.WeakField,
			"Перед сохранением карточки заполните обязательные поля:", missing))
	}
	if len(wrong) > 0 {
		out = append(out, rec(RecWrong, public.WeakField, "Значения не совпадают с эталоном:", wrong))
	}
	if len(extra) > 0 {
		body := "Назначены службы сверх необходимых — лишний выезд отвлекает силы от других вызовов:"
		if extraGeneric {
			body = "Указано больше, чем требует ситуация по эталону:"
		}
		out = append(out, rec(RecExtra, public.WeakField, body, extra))
	}

	// Без норматива превышать нечего (Timing даёт within_norm=true); пустой evaluations.timing
	// ('{}' по умолчанию в БД) не должен превращаться в «норматив 0 с, превышение на 0 с».
	if !in.Timing.WithinNorm && in.Timing.LimitSec > 0 {
		d := in.Timing.DeltaMs
		if d < 0 {
			d = 0
		}
		out = append(out, rec(RecTiming, public.SlowTiming,
			"Норматив заполнения — "+strconv.Itoa(in.Timing.LimitSec)+" с, затрачено "+
				FormatDuration(in.Timing.SpentMs)+" (превышение на "+FormatDuration(d)+"). "+
				"Начинайте вводить адрес одновременно с разговором, не дожидаясь конца обращения.", nil))
	}

	if facts := nonEmpty(in.SemanticMissingFacts); len(facts) > 0 {
		body := "В описании со слов заявителя не зафиксировано:"
		if c := strings.TrimSpace(in.CategoryName); c != "" {
			body = "Отработайте тему «" + c + "». " + body
		}
		out = append(out, rec(RecFacts, public.General, body, facts))
	}

	dialogueRecs := 0
	if q := nonEmpty(in.DialogueMissing); len(q) > 0 {
		out = append(out, rec(RecDialogueQuestions, public.DialoguePattern,
			"В разговоре с заявителем не заданы обязательные вопросы протокола. "+
				"Задавайте их до того, как заявитель положит трубку:", q))
		dialogueRecs++
	}
	if hits := nonEmpty(in.DialogueForbiddenHits); len(hits) > 0 {
		out = append(out, rec(RecDialogueForbidden, public.DialoguePattern,
			"В разговоре прозвучали недопустимые фразы — оператор не должен их произносить:", hits))
		dialogueRecs++
	}
	if in.SpeechFillerCount > FillerThreshold {
		out = append(out, rec(RecDialogueFillers, public.DialoguePattern,
			"Слов-паразитов в речи: "+strconv.Itoa(in.SpeechFillerCount)+". Говорите короче и увереннее: "+
				"заявитель в стрессе, лишние слова мешают ему понять вас.", topFillers(in.SpeechFillers)))
		dialogueRecs++
	}
	if dialogueRecs == 0 && in.DialogueScore != nil && *in.DialogueScore < DialogueLowScore {
		out = append(out, rec(RecDialogueScore, public.DialoguePattern,
			"Разговор с заявителем оценён на "+formatNum(Round2(*in.DialogueScore))+" из 100. "+
				"Разберите транскрипт и комментарий к разговору в результатах.", nil))
	}

	if in.GrammarRemarks > GrammarRemarksThreshold {
		out = append(out, rec(RecGrammar, public.GrammarPattern,
			"Замечаний к тексту: "+strconv.Itoa(in.GrammarRemarks)+". Избегайте сокращений и пишите "+
				"полными предложениями — карточку читает диспетчер службы.", nil))
	}
	return out
}

func rec(id string, kind public.RecommendationKind, body string, items []string) public.Recommendation {
	r := public.Recommendation{Id: id, Kind: kind, Body: body}
	if len(items) > 0 {
		r.Items = &items
	}
	return r
}

func appendUnique(dst []string, s string) []string {
	if s = strings.TrimSpace(s); s == "" || slices.Contains(dst, s) {
		return dst
	}
	return append(dst, s)
}

// nonEmpty — без пустых строк и повторов, порядок сохранён (копия: вход не трогаем).
func nonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		out = appendUnique(out, s)
	}
	return out
}

// listDiff — элементы a, которых нет в b (FieldError.actual/expected — []string из
// FieldErrors или []any после чтения jsonb из БД).
func listDiff(a, b any) []string {
	as, bs := toStrings(a), toStrings(b)
	var out []string
	for _, s := range as {
		if !slices.Contains(bs, s) {
			out = append(out, s)
		}
	}
	return out
}

func toStrings(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		if x != "" {
			return []string{x}
		}
	}
	return nil
}

// topFillers — «ну» — 4, по убыванию частоты (при равенстве — по алфавиту), не больше 5.
func topFillers(m map[string]int) []string {
	if len(m) == 0 {
		return nil
	}
	type kv struct {
		w string
		n int
	}
	list := make([]kv, 0, len(m))
	for w, n := range m {
		if w = strings.TrimSpace(w); w != "" && n > 0 {
			list = append(list, kv{w, n})
		}
	}
	slices.SortFunc(list, func(a, b kv) int {
		if a.n != b.n {
			return b.n - a.n
		}
		return strings.Compare(a.w, b.w)
	})
	if len(list) > maxFillerItems {
		list = list[:maxFillerItems]
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, "«"+e.w+"» — "+strconv.Itoa(e.n))
	}
	return out
}
