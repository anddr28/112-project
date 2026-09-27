// Package core — общий словарь домена: роли, статусы, типы задач и событий, субъект запроса.
// Порты между доменными пакетами — в ports.go. Пакет не зависит ни от одного доменного пакета;
// доменные пакеты не импортируют друг друга напрямую — только core-порты (связывает internal/app).
package core

import (
	"context"
	"strings"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------- роли

type Role string

const (
	RoleAdmin   Role = "admin"
	RoleTeacher Role = "teacher"
	RoleStudent Role = "student"
)

func (r Role) Valid() bool { return r == RoleAdmin || r == RoleTeacher || r == RoleStudent }

// ---------------------------------------------------------------- субъект запроса

// Principal — аутентифицированный пользователь запроса (кладёт auth-middleware).
type Principal struct {
	UserID     uuid.UUID
	SessionID  uuid.UUID
	Role       Role
	Login      string
	LastName   string
	FirstName  string
	MiddleName string
	OperatorNo string // "оп. 227" — подпись в истории статусов реагирования
}

// ShortName — «Фамилия И. О.» (подпись действий в отчётах и истории).
func (p *Principal) ShortName() string { return ShortName(p.LastName, p.FirstName, p.MiddleName) }

// OperatorLabel — операторский номер, иначе «Фамилия И. О.» (выдумывать «оп. 0» нельзя).
func (p *Principal) OperatorLabel() string {
	if strings.TrimSpace(p.OperatorNo) != "" {
		return p.OperatorNo
	}
	return p.ShortName()
}

func (p *Principal) Is(roles ...Role) bool {
	for _, r := range roles {
		if p.Role == r {
			return true
		}
	}
	return false
}

// ShortName — «Фамилия И. О.» из частей ФИО.
func ShortName(last, first, middle string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(last))
	if r := []rune(strings.TrimSpace(first)); len(r) > 0 {
		b.WriteString(" ")
		b.WriteRune(r[0])
		b.WriteString(".")
	}
	if r := []rune(strings.TrimSpace(middle)); len(r) > 0 {
		b.WriteString(" ")
		b.WriteRune(r[0])
		b.WriteString(".")
	}
	return b.String()
}

type principalKey struct{}

func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom — субъект запроса или nil (публичный маршрут / фоновая задача).
func PrincipalFrom(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalKey{}).(*Principal)
	return p
}

// RequestMeta — для аудита: кто/откуда (кладёт роутер).
type RequestMeta struct {
	RequestID uuid.UUID
	IP        string
	UserAgent string
}

type metaKey struct{}

func WithRequestMeta(ctx context.Context, m RequestMeta) context.Context {
	return context.WithValue(ctx, metaKey{}, m)
}

func RequestMetaFrom(ctx context.Context) (RequestMeta, bool) {
	m, ok := ctx.Value(metaKey{}).(RequestMeta)
	return m, ok
}

// ---------------------------------------------------------------- статусы (CHECK в БД)

const (
	LessonDraft     = "draft"
	LessonScheduled = "scheduled"
	LessonRunning   = "running"
	LessonFinished  = "finished"
	LessonCancelled = "cancelled"

	LessonKindClass    = "class"
	LessonKindPractice = "practice"

	ParticipantAssigned     = "assigned"
	ParticipantJoined       = "joined"
	ParticipantActive       = "active"
	ParticipantDisconnected = "disconnected"
	ParticipantFinished     = "finished"

	AttemptIssued     = "issued"
	AttemptInProgress = "in_progress"
	AttemptSubmitted  = "submitted"
	AttemptEvaluating = "evaluating"
	AttemptEvaluated  = "evaluated"
	AttemptExpired    = "expired"
	AttemptAborted    = "aborted"

	ScenarioDraft     = "draft"
	ScenarioGenerated = "generated"
	ScenarioValidated = "validated"
	ScenarioRejected  = "rejected"
	ScenarioArchived  = "archived"

	EvalPending = "pending"
	EvalPartial = "partial"
	EvalDone    = "done"
	EvalFailed  = "failed"

	VerdictPending = "pending"
	VerdictPass    = "pass"
	VerdictFail    = "fail"

	ModeCards       = "cards"
	ModeCardActions = "card_actions"
	ModeBoth        = "both" // только у сценария
)

// AttemptTerminal — попытка закрыта для ввода (черновик/события/диалог не принимаются).
func AttemptTerminal(status string) bool {
	switch status {
	case AttemptSubmitted, AttemptEvaluating, AttemptEvaluated, AttemptExpired, AttemptAborted:
		return true
	}
	return false
}

// ---------------------------------------------------------------- слои оценки

const (
	LayerFields   = "fields"
	LayerGrammar  = "grammar"
	LayerSemantic = "semantic"
	LayerTiming   = "timing"
	LayerDialogue = "dialogue"
)

// Состояния AI-слоёв (Evaluation.layers).
const (
	LayerQueued  = "queued"
	LayerRunning = "running"
	LayerDone    = "done"
	LayerFailed  = "failed"
	LayerSkipped = "skipped"
)

// ---------------------------------------------------------------- AI-задачи

type JobType string

const (
	JobEvaluateGrammar  JobType = "evaluate_grammar"
	JobEvaluateSemantic JobType = "evaluate_semantic"
	JobEvaluateDialogue JobType = "evaluate_dialogue"
	JobGenerateScenario JobType = "generate_scenario"
	JobTTS              JobType = "tts"
)

// Path — путь ai-service для задачи (POST /v1/jobs/{path}).
func (t JobType) Path() string {
	switch t {
	case JobEvaluateGrammar:
		return "/v1/jobs/grammar"
	case JobEvaluateSemantic:
		return "/v1/jobs/semantic"
	case JobEvaluateDialogue:
		return "/v1/jobs/dialogue"
	case JobGenerateScenario:
		return "/v1/jobs/generate"
	case JobTTS:
		return "/v1/jobs/tts"
	}
	return ""
}

// Layer — слой оценки, который закрывает задача ("" — не оценочная задача).
func (t JobType) Layer() string {
	switch t {
	case JobEvaluateGrammar:
		return LayerGrammar
	case JobEvaluateSemantic:
		return LayerSemantic
	case JobEvaluateDialogue:
		return LayerDialogue
	}
	return ""
}

const (
	JobQueued    = "queued"
	JobRunning   = "running"
	JobDone      = "done"
	JobFailed    = "failed"
	JobCancelled = "cancelled"
)

// Приоритеты ai_jobs (1 — важнее; контракт JobBase.priority 1..9).
const (
	PriorityGrammar  = 2 // LT быстрый, студент ждёт результат
	PrioritySemantic = 3
	PriorityDialogue = 3
	PriorityTTSLive  = 3 // озвучка, нужная прямо на занятии
	PriorityTTS      = 5 // предгенерация при approve
	PriorityGenerate = 7 // фон
)

// Ссылки ai_jobs.ref_type.
const (
	RefAttempt  = "attempt"
	RefScenario = "scenario"
	RefTTS      = "tts"
)

// ---------------------------------------------------------------- события попытки

const (
	EventIssued               = "issued"
	EventCallAccepted         = "call_accepted"
	EventOpenCard             = "open_card"
	EventFieldChanged         = "field_changed"
	EventChooseValue          = "choose_value"
	EventServiceAssigned      = "service_assigned"
	EventServiceRemoved       = "service_removed"
	EventServiceStatusChanged = "service_status_changed"
	EventReplay               = "replay"
	EventSave                 = "save"
	EventSubmitted            = "submitted"
	EventTimerExpired         = "timer_expired"
	EventDisconnected         = "disconnected"
	EventReconnected          = "reconnected"
	EventMicCheck             = "mic_check"
	EventPTTStart             = "ptt_start"
	EventPTTStop              = "ptt_stop"
	EventDialogueOperator     = "dialogue_operator"
	EventDialogueCaller       = "dialogue_caller"
	EventDialogueEnded        = "dialogue_ended"
)

// UserInputEvent — содержательное действие обучающегося: первое такое событие задаёт
// attempts.first_input_at (время реакции). Автосохранение (save) сюда не входит.
func UserInputEvent(t string) bool {
	switch t {
	case EventFieldChanged, EventChooseValue, EventServiceAssigned, EventServiceRemoved, EventServiceStatusChanged:
		return true
	}
	return false
}

// ---------------------------------------------------------------- разговор

const (
	SpeakerOperator = "operator"
	SpeakerCaller   = "caller"

	TurnSourceSTT    = "stt"
	TurnSourceText   = "text"
	TurnSourceScript = "script"
	TurnSourceLLM    = "llm"

	EndOperatorHungUp = "operator_hung_up"
	EndCallerHungUp   = "caller_hung_up"
	EndMaxTurns       = "max_turns"
	EndSubmitted      = "submitted"
	EndTimeout        = "timeout"
)
