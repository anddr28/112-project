// Package model — формы jsonb-колонок в БД (snake_case), ровно как их описывают
// DESIGN.md §4 и комментарии миграций 00001/00002.
//
// Правила:
//   - здесь только данные и чистые методы над ними — без БД, HTTP и логов;
//   - опциональное в JSON — omitempty (jsonb компактнее, читатели трактуют отсутствие как пусто);
//   - обязательные массивы контрактов (turns, checklist, facts) — без omitempty:
//     писатели нормализуют их в [] (Normalize), чтобы в БД не оказалось null;
//   - camelCase-формы фронта (IncidentCardDraft, FieldError, AttributeGroup) хранятся как
//     сгенерированные public-типы и сюда не дублируются.
package model

import (
	"encoding/json"
	"strings"

	"lct/gocore/internal/gen/components"
)

// ---------------------------------------------------------------- легенда звонка

// Реплики сценария (CallScript.turns[].speaker).
const (
	SpeakerCaller       = "caller"
	SpeakerOperatorHint = "operator_hint"
)

// Раскрытие факта заявителем (DialogueFact.reveal).
const (
	RevealVolunteer = "volunteer"
	RevealOnRequest = "on_request"
	RevealNever     = "never"
)

// CallScript — scenarios.call_script: контрактный CallScript (_components.yaml) плюс
// caller.voice (голос TTS заявителя) и turns[].tts_hash (ключ tts_cache).
type CallScript struct {
	Caller   Caller             `json:"caller"`
	Address  components.Address `json:"address"`
	KeyFacts []string           `json:"key_facts,omitempty"`
	Dialogue *DialogueBrief     `json:"dialogue,omitempty"`
	Turns    []Turn             `json:"turns"`
}

type Caller struct {
	Name           string `json:"name,omitempty"`
	Phone          string `json:"phone,omitempty"`
	Role           string `json:"role,omitempty"`            // очевидец / пострадавший / сосед / ...
	EmotionalState string `json:"emotional_state,omitempty"` // паника, спокоен, ...
	Voice          string `json:"voice,omitempty"`           // голос TTS; пусто — settings.tts.voice
}

// Turn — сценарная реплика. В голосовом режиме первая реплика заявителя — вступление
// (озвучена заранее), остальные — резерв для fallback без LLM.
type Turn struct {
	Speaker string `json:"speaker"` // caller | operator_hint
	Text    string `json:"text"`
	TTSHash string `json:"tts_hash,omitempty"` // sha256(text|voice|rate) — проставляется при approve
}

// DialogueBrief — бриф LLM-заявителя (студенту не отдаётся).
type DialogueBrief struct {
	Persona       string         `json:"persona"`
	SpeakingStyle string         `json:"speaking_style,omitempty"`
	Facts         []DialogueFact `json:"facts"`
	Unknowns      []string       `json:"unknowns,omitempty"`
	EndConditions []string       `json:"end_conditions,omitempty"`
	MaxTurns      int            `json:"max_turns,omitempty"` // 0 — не задано (берётся из занятия)
}

type DialogueFact struct {
	ID     string   `json:"id"`
	Text   string   `json:"text"`
	Reveal string   `json:"reveal"` // volunteer | on_request | never
	Hints  []string `json:"hints,omitempty"`
}

// Normalize — обязательные массивы не nil (в jsonb и в ответах должно быть [], не null).
func (c *CallScript) Normalize() {
	if c.Turns == nil {
		c.Turns = []Turn{}
	}
	if c.Dialogue != nil && c.Dialogue.Facts == nil {
		c.Dialogue.Facts = []DialogueFact{}
	}
}

// OpeningTurn — вступительная реплика заявителя: первая реплика со speaker=caller
// (voice-mode §2; мок фронта берёт ровно её). ok=false — реплик заявителя нет.
func (c *CallScript) OpeningTurn() (idx int, t Turn, ok bool) {
	for i := range c.Turns {
		if c.Turns[i].Speaker == SpeakerCaller && strings.TrimSpace(c.Turns[i].Text) != "" {
			return i, c.Turns[i], true
		}
	}
	return -1, Turn{}, false
}

// HasCallerTurn — есть хотя бы одна непустая реплика заявителя (условие approve).
func (c *CallScript) HasCallerTurn() bool {
	_, _, ok := c.OpeningTurn()
	return ok
}

// VoiceOr — голос заявителя или голос по умолчанию (settings.tts.voice).
func (c *CallScript) VoiceOr(def string) string {
	if v := strings.TrimSpace(c.Caller.Voice); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------- эталон

// DefaultRequiredFields — обязательные поля нового эталона (как у мока фронта при
// создании сценария и как дефолт для сгенерированного эталона).
func DefaultRequiredFields() []string {
	return []string{"applicant.name", "applicant.status", "address.raw", "incidentTypeIds", "description"}
}

// Scoring — etalons.scoring: контрактный Scoring + required_fields — пути IncidentCardDraft
// ("applicant.name", "address.raw", "incidentTypeIds", "attributes.where", ...).
type Scoring struct {
	RequiredFields []string           `json:"required_fields,omitempty"`
	FieldWeights   map[string]float64 `json:"field_weights,omitempty"`
	RequiredFacts  []string           `json:"required_facts,omitempty"`
	ForbiddenFacts []string           `json:"forbidden_facts,omitempty"`
}

// FieldWeight — вес поля в слое 1 (по умолчанию 1; отрицательный трактуется как 0).
func (s *Scoring) FieldWeight(path string) float64 {
	if s != nil && s.FieldWeights != nil {
		if w, ok := s.FieldWeights[path]; ok {
			if w < 0 {
				return 0
			}
			return w
		}
	}
	return 1
}

// IsZero — правил нет (etalons.scoring = '{}').
func (s *Scoring) IsZero() bool {
	return s == nil || (len(s.RequiredFields) == 0 && len(s.FieldWeights) == 0 &&
		len(s.RequiredFacts) == 0 && len(s.ForbiddenFacts) == 0)
}

// ExpectedAction — элемент etalons.expected_actions (режим «действия с карточками»).
type ExpectedAction struct {
	ActionText     string   `json:"action_text"`
	RequiredFacts  []string `json:"required_facts,omitempty"`
	ForbiddenFacts []string `json:"forbidden_facts,omitempty"`
}

// Виды пунктов чек-листа разговора.
const (
	ChecklistQuestion    = "question"
	ChecklistInstruction = "instruction"
	ChecklistPhrase      = "phrase"
	ChecklistBehavior    = "behavior"
)

// ExpectedDialogue — etalons.expected_dialogue: чек-лист протокола разговора.
// '{}' в БД — чек-листа нет (читатели получают nil, см. store).
type ExpectedDialogue struct {
	Checklist        []ChecklistItem `json:"checklist"`
	Forbidden        []string        `json:"forbidden,omitempty"`
	MaxOperatorTurns int             `json:"max_operator_turns,omitempty"`
}

type ChecklistItem struct {
	ID       string   `json:"id"`
	Text     string   `json:"text"`
	Kind     string   `json:"kind"` // question | instruction | phrase | behavior
	Required bool     `json:"required"`
	Weight   *float64 `json:"weight,omitempty"` // nil — 1 (контракт: default 1; 0 — пункт без веса)
	Hints    []string `json:"hints,omitempty"`
}

// Normalize — checklist не nil.
func (e *ExpectedDialogue) Normalize() {
	if e != nil && e.Checklist == nil {
		e.Checklist = []ChecklistItem{}
	}
}

// Item — пункт чек-листа по id (линейный поиск: пунктов единицы-десятки).
func (e *ExpectedDialogue) Item(id string) (*ChecklistItem, bool) {
	if e == nil {
		return nil, false
	}
	for i := range e.Checklist {
		if e.Checklist[i].ID == id {
			return &e.Checklist[i], true
		}
	}
	return nil, false
}

// ---------------------------------------------------------------- оценка

// TimingBlob — evaluations.timing (VIEW student_progress читает within_norm).
type TimingBlob struct {
	SpentMs    int  `json:"spent_ms"`
	LimitSec   int  `json:"limit_sec"`
	DeltaMs    int  `json:"delta_ms"` // spent − limit·1000 (отрицательное — уложился с запасом)
	WithinNorm bool `json:"within_norm"`
	ReactionMs int  `json:"reaction_ms"`
}

// GrammarStats — evaluations.grammar_stats (GrammarResult.stats от ai-service).
type GrammarStats struct {
	WordsChecked     int            `json:"words_checked"`
	ErrorsBySeverity map[string]int `json:"errors_by_severity,omitempty"`
}

// Layers — evaluations.layers: {"grammar":"queued|done|failed|skipped", ...}.
// В БД только терминальные состояния и queued; running накладывается при чтении из ai_jobs.
type Layers map[string]string

// ---------------------------------------------------------------- разговор

// TurnMeta — attempt_dialogue_turns.meta.
type TurnMeta struct {
	Engine          map[string]any `json:"engine,omitempty"` // components.Engine как пришёл
	LatencyMs       int            `json:"latency_ms,omitempty"`
	Fallback        bool           `json:"fallback,omitempty"`
	TextRaw         string         `json:"text_raw,omitempty"` // STT до нормализации (ITN)
	IsUnknownAnswer bool           `json:"is_unknown_answer,omitempty"`
}

// ---------------------------------------------------------------- генерация

// GenerationMeta — scenarios.generation_meta. Плоский объект: ключи engine генерации
// (llm_model, prompt_version, duration_ms, tokens_in, ...) лежат на верхнем уровне рядом
// с известными полями; всё неизвестное сохраняется в Extra без потерь (round-trip).
type GenerationMeta struct {
	NotesForTeacher    string
	DifficultyEstimate int
	JobID              string // uuid задачи generate_scenario
	RejectedReason     string
	WithDialogue       *bool          // запрошен ли бриф/чек-лист (GenerateScenarioInput.withDialogue)
	Extra              map[string]any // engine и прочие ключи как есть
}

const (
	gmNotes      = "notes_for_teacher"
	gmDifficulty = "difficulty_estimate"
	gmJobID      = "job_id"
	gmRejected   = "rejected_reason"
	gmDialogue   = "with_dialogue"
)

// IsZero — метаданных нет ('{}').
func (g *GenerationMeta) IsZero() bool {
	return g.NotesForTeacher == "" && g.DifficultyEstimate == 0 && g.JobID == "" &&
		g.RejectedReason == "" && g.WithDialogue == nil && len(g.Extra) == 0
}

// SetEngine раскладывает engine ai-service в плоские ключи (перезаписывая старые).
func (g *GenerationMeta) SetEngine(e *components.Engine) {
	if e == nil {
		return
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return
	}
	if g.Extra == nil {
		g.Extra = make(map[string]any, len(m))
	}
	for k, v := range m {
		g.Extra[k] = v
	}
}

// Map — плоское представление (то же, что уходит в jsonb). Новая карта: вызывающий
// может её менять.
func (g *GenerationMeta) Map() map[string]any {
	m := make(map[string]any, len(g.Extra)+5)
	for k, v := range g.Extra {
		m[k] = v
	}
	if g.NotesForTeacher != "" {
		m[gmNotes] = g.NotesForTeacher
	}
	if g.DifficultyEstimate != 0 {
		m[gmDifficulty] = g.DifficultyEstimate
	}
	if g.JobID != "" {
		m[gmJobID] = g.JobID
	}
	if g.RejectedReason != "" {
		m[gmRejected] = g.RejectedReason
	}
	if g.WithDialogue != nil {
		m[gmDialogue] = *g.WithDialogue
	}
	return m
}

func (g GenerationMeta) MarshalJSON() ([]byte, error) { return json.Marshal(g.Map()) }

func (g *GenerationMeta) UnmarshalJSON(b []byte) error {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*g = GenerationMeta{}
	for k, v := range m {
		switch k {
		case gmNotes:
			g.NotesForTeacher, _ = v.(string)
		case gmDifficulty:
			if f, ok := v.(float64); ok {
				g.DifficultyEstimate = int(f)
			}
		case gmJobID:
			g.JobID, _ = v.(string)
		case gmRejected:
			g.RejectedReason, _ = v.(string)
		case gmDialogue:
			if bv, ok := v.(bool); ok {
				g.WithDialogue = &bv
			}
		default:
			if g.Extra == nil {
				g.Extra = make(map[string]any, len(m))
			}
			g.Extra[k] = v
		}
	}
	return nil
}
