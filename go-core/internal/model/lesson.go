package model

import (
	"bytes"
	"encoding/json"
	"fmt"

	"lct/gocore/internal/settings"
)

// Ракурс АРМ (GAP-07, lessons.settings.perspective).
const (
	PerspectiveOperator112 = "operator112"
	PerspectiveDDS         = "dds"
)

// Источник карточек пула занятия (lessons.settings.card_source, контракт v1.4 CardSource):
// сгенерированные системой (source ≠ student), сформированные обучающимися (source =
// student) или смешанный пул (по умолчанию — как было до v1.4).
const (
	CardSourceGenerated = "generated"
	CardSourceStudent   = "student"
	CardSourceMixed     = "mixed"
)

// ValidCardSource — значение из enum CardSource.
func ValidCardSource(s string) bool {
	return s == CardSourceGenerated || s == CardSourceStudent || s == CardSourceMixed
}

// CardSourceAllows — сценарий с источником scenarioSource допустим в пуле с источником cardSource.
func CardSourceAllows(cardSource, scenarioSource string) bool {
	switch cardSource {
	case CardSourceGenerated:
		return scenarioSource != "student"
	case CardSourceStudent:
		return scenarioSource == "student"
	}
	return true
}

// Ввод реплик оператора (lessons.settings.voice.input).
const (
	VoiceInputVoice = "voice"
	VoiceInputText  = "text"
	VoiceInputBoth  = "both"
)

// LessonSettings — lessons.settings (snake_case). Пишет только go-core (lessons) и всегда
// целиком; частичные строки (руками в БД, старые данные) добиваются дефолтами при чтении.
type LessonSettings struct {
	PassThreshold   float64          `json:"pass_threshold"`
	Weights         settings.Weights `json:"weights"`
	CardsPerStudent int              `json:"cards_per_student"`
	AllowReplay     bool             `json:"allow_replay"`
	Voice           settings.Voice   `json:"voice"`
	Perspective     string           `json:"perspective"` // operator112 | dds
	CardSource      string           `json:"card_source"` // generated | student | mixed (v1.4)
}

// IsDDS — ракурс «Диспетчер ДДС»: карточка приходит от оператора 112, обучающийся
// работает за диспетчера своей службы (контракт v1.3).
func (s *LessonSettings) IsDDS() bool { return s.Perspective == PerspectiveDDS }

// DefaultLessonSettings — настройки нового занятия из снимка настроек платформы
// (s == nil — дефолты миграций). Веса — как в settings.score_weights (dialogue там 0);
// пересчёт под голосовой режим — забота lessons/scoring.
func DefaultLessonSettings(s *settings.Snapshot) LessonSettings {
	if s == nil {
		d := settings.Defaults()
		s = &d
	}
	out := LessonSettings{
		PassThreshold:   s.PassThreshold,
		Weights:         s.ScoreWeights,
		CardsPerStudent: s.CardsPerStudent,
		AllowReplay:     s.AllowReplay,
		Voice:           s.Voice,
		Perspective:     PerspectiveOperator112,
		CardSource:      CardSourceMixed,
	}
	out.fix()
	return out
}

// ParseLessonSettings — разбор lessons.settings поверх def: ключи, которых нет в jsonb,
// остаются как в def (encoding/json декодирует во вложенные структуры, не обнуляя их).
// Best effort: при ошибке типа поля остальные поля всё равно разобраны (так работает
// encoding/json), при синтаксической — вернётся def; результат пригоден к использованию
// всегда, ошибка — для лога.
func ParseLessonSettings(raw []byte, def LessonSettings) (LessonSettings, error) {
	out := def
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) || bytes.Equal(raw, []byte("{}")) {
		return out, nil
	}
	err := json.Unmarshal(raw, &out)
	out.fix()
	if err != nil {
		return out, fmt.Errorf("lessons.settings: %w", err)
	}
	return out, nil
}

// fix — инварианты, без которых остальной код ломается (деление на 0, пустой enum в ответе).
func (s *LessonSettings) fix() {
	if s.CardsPerStudent < 1 {
		s.CardsPerStudent = 1
	}
	if s.Perspective != PerspectiveDDS {
		s.Perspective = PerspectiveOperator112
	}
	if !ValidCardSource(s.CardSource) {
		s.CardSource = CardSourceMixed // занятия до v1.4: пул не проверялся по источнику
	}
	switch s.Voice.Input {
	case VoiceInputVoice, VoiceInputText, VoiceInputBoth:
	default:
		s.Voice.Input = VoiceInputVoice
	}
	if s.Voice.MaxTurns <= 0 {
		s.Voice.MaxTurns = 12
	}
}
