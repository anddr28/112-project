package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/callbacks"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/pg"
)

// Порты между доменными пакетами. Реализации создаются в internal/app и передаются
// в конструкторы. Правило: доменный пакет импортирует core и platform/*, но не другой
// доменный пакет — так пакеты собираются и тестируются независимо.

// ================================================================ аутентификация

var (
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrUserBlocked     = errors.New("user blocked")
)

// Authenticator — сессия по cookie lct_session (реализация: auth.Service).
// Возвращает ErrUnauthenticated (нет/истекла/отозвана) или ErrUserBlocked.
type Authenticator interface {
	Authenticate(r *http.Request) (*Principal, error)
}

// ================================================================ аудит

// Auditor — журнал аудита (реализация: audit.Writer, асинхронный батч).
// Log не блокирует и не возвращает ошибок: аудит не должен ронять бизнес-операцию.
// Актор, IP, User-Agent, request_id берутся из ctx (Principal / RequestMeta), если не заданы явно.
// Before/After — любые JSON-сериализуемые значения; секреты (password, token, hash)
// вычищаются writer'ом по именам ключей (контракт приложения, db-design §6).
type Auditor interface {
	Log(ctx context.Context, e AuditEntry)
}

type AuditEntry struct {
	Action     string // user.login | user.login_failed | user.create | scenario.approve | evaluation.override | ...
	EntityType string // user | scenario | etalon | lesson | attempt | evaluation | setting | backup | session
	EntityID   uuid.UUID
	LessonID   uuid.UUID
	Before     any
	After      any
	// Явный актор (вход в систему: Principal ещё нет). Nil — из ctx; система — ActorSystem=true.
	ActorID     *uuid.UUID
	ActorRole   Role
	ActorSystem bool
}

// ================================================================ realtime

// Publisher — отправка сообщений в WebSocket-каналы (реализация: realtime.Hub).
// Хаб сам проставляет Seq (монотонный в пределах канала) и At. Неблокирующий:
// медленный клиент не тормозит бизнес-операцию (буфер + since-replay).
// Вызывать ПОСЛЕ коммита (pg.OnCommit).
type Publisher interface {
	Monitor(lessonID uuid.UUID, m public.MonitorMessage)  // /ws/lessons/{id}/monitor — преподаватель
	Student(attemptID uuid.UUID, m public.StudentMessage) // /ws/attempts/{id} — обучающийся
}

// LessonMonitor — нужное WS-хабу от занятий (реализация: lessons.Service).
type LessonMonitor interface {
	// CanMonitor — nil, если субъект может смотреть мониторинг (преподаватель-владелец или админ).
	CanMonitor(ctx context.Context, p *Principal, lessonID uuid.UUID) error
	// Snapshot — первое сообщение канала мониторинга.
	Snapshot(ctx context.Context, lessonID uuid.UUID) (*public.Lesson, []public.Attempt, error)
}

// AttemptPresence — присутствие обучающегося (WS попытки открыт/закрыт).
type AttemptPresence interface {
	// AttemptAccess — nil, если субъект владелец попытки; возвращает lessonID.
	AttemptAccess(ctx context.Context, p *Principal, attemptID uuid.UUID) (lessonID uuid.UUID, err error)
	// SetOnline — участник онлайн/офлайн: lesson_participants.status/last_seen_at + monitor participantStatus.
	SetOnline(ctx context.Context, attemptID uuid.UUID, online bool)
}

// ================================================================ очередь AI

// NewJob — задача в ai_jobs. Payload — полное тело запроса к ai-service
// (aiservice.*JobRequest) с request_id = ID, schema_version = "1", priority = Priority.
type NewJob struct {
	ID       uuid.UUID // обязателен: сгенерируйте ids.New() ДО построения payload
	Type     JobType
	Priority int // 1..9
	DedupKey string
	RefType  string
	RefID    uuid.UUID
	Payload  any
	MaxTries int       // 0 — из settings.ai.max_tries
	RunAfter time.Time // zero — сейчас
}

// JobQueue — очередь ai_jobs в PostgreSQL (реализация: aijobs.Queue).
type JobQueue interface {
	// Enqueue вставляет задачу в транзакции q. При конфликте dedup_key возвращает id
	// существующей задачи и created=false. Диспетчер будится сам после COMMIT (OnCommit).
	Enqueue(ctx context.Context, q pg.Querier, j NewJob) (jobID uuid.UUID, created bool, err error)
	// Available — ai-service принимает задачи (breaker не open).
	Available() bool
	// EstWaitSec — оценка ожидания новой задачи этого типа (для estWaitSec в ответах).
	EstWaitSec(t JobType) int
}

// JobRecord — задача, по которой пришёл результат.
type JobRecord struct {
	ID        uuid.UUID
	Type      JobType
	Status    string
	RefType   string
	RefID     uuid.UUID
	TryCount  int
	MaxTries  int
	Payload   json.RawMessage
	CreatedAt time.Time
}

// AIResultHandler — применение результата задачи доменом (регистрируется по типу).
// Вызывается в транзакции, где строка ai_jobs уже заблокирована (FOR UPDATE) и не терминальна;
// после успешного возврата aijobs сам переводит задачу в done/failed. Эффекты наружу —
// через pg.OnCommit(ctx, ...).
type AIResultHandler interface {
	ApplyResult(ctx context.Context, tx pgx.Tx, job JobRecord, res *callbacks.AiResult) error
	// ApplyFailure — задача окончательно провалена (ретраи исчерпаны, retryable=false,
	// 400 от ai-service, poison-pill reaper'а). code — AiJobError.code или "dispatch_failed".
	ApplyFailure(ctx context.Context, tx pgx.Tx, job JobRecord, code, message string) error
}

// AIResultRouter — регистрация обработчиков результатов (реализация: aijobs.Callbacks).
type AIResultRouter interface {
	Register(t JobType, h AIResultHandler)
}

// ================================================================ синхронный ai-service

// AIClient — синхронные вызовы ai-service (реализация: aijobs.SyncClient).
// Ошибки — *AIError. Таймаут диалога — config.AISyncTimeout (контракт: 20 с).
type AIClient interface {
	DialogTurn(ctx context.Context, req *aiservice.DialogTurnRequest, audio *AudioInput) (*aiservice.DialogTurnResult, error)
	TTSSync(ctx context.Context, req *aiservice.TtsSyncRequest) (*components.TtsResult, error)
	Health(ctx context.Context) (*aiservice.Health, error)
	Queue(ctx context.Context) (*aiservice.QueueStatus, error)
	BreakerState() string // closed | open | half_open
}

// AudioInput — реплика оператора для multipart-прокси в ai-service (аудио НЕ сохраняется).
type AudioInput struct {
	Reader      io.Reader
	Filename    string
	ContentType string // audio/webm | audio/ogg | audio/wav
}

type AIErrorKind int

const (
	AIBusy             AIErrorKind = iota + 1 // 503/429: полоса занята — caller_busy, breaker не трогаем
	AIUnavailable                             // refused / timeout / 5xx / breaker open — ai_unavailable
	AIAudioTooLong                            // 413
	AIAudioUnsupported                        // 415
	AIBadRequest                              // 400 — ошибка go-core в payload (логировать!)
)

type AIError struct {
	Kind       AIErrorKind
	RetryAfter int // секунды, если ai-service прислал Retry-After
	Message    string
	Err        error
}

func (e *AIError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("ai-service: %s: %v", e.Message, e.Err)
	}
	return "ai-service: " + e.Message
}

func (e *AIError) Unwrap() error { return e.Err }

// AsAIError — разбор ошибки AIClient.
func AsAIError(err error) (*AIError, bool) {
	var ae *AIError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// ================================================================ оценка и попытки

// Evaluator — старт оценки при сдаче попытки (реализация: evaluation.Service).
// Вызывается в транзакции submit ПОСЛЕ того, как attempts.card/action_text/submitted_at/
// time_spent_ms/status записаны. Считает слои fields+timing, создаёт evaluations(partial),
// ставит AI-задачи (grammar/semantic/dialogue), публикует evaluationUpdated после COMMIT.
type Evaluator interface {
	StartEvaluation(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID) error
}

// AttemptIssuer — выдача следующей карточки (реализация: lessons.Service).
// issued=false — лимит cardsPerStudent исчерпан или занятие не running.
type AttemptIssuer interface {
	IssueNext(ctx context.Context, tx pgx.Tx, lessonID, userID uuid.UUID) (attemptID uuid.UUID, issued bool, err error)
}

// DialogueHooks — разговор в жизненном цикле попытки (реализация: dialogue.Service).
type DialogueHooks interface {
	// OnCallAccepted — вступительная реплика (turn 0) в голосовом режиме; nil — голос выключен.
	// Идемпотентно: повторный accept-call вернёт ту же реплику.
	OnCallAccepted(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID) (*public.DialogueTurnView, error)
	// CloseOnSubmit — закрыть разговор при сдаче (reason=submitted), если он ещё открыт.
	CloseOnSubmit(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, at time.Time) error
}

// ================================================================ справочники

// Catalog — классификатор и службы из памяти (реализация: classifier.Catalog).
// Потокобезопасен; данные перечитываются после импорта/сида (Reload).
type Catalog interface {
	TypeByID(id string) (IncidentTypeInfo, bool)
	TypeByCode(code string) (IncidentTypeInfo, bool)
	ServiceByID(id string) (ServiceInfo, bool)
	ServiceByCode(code string) (ServiceInfo, bool)
	// FieldLabel — подпись поля карточки по пути ("applicant.name" -> "ФИО заявителя";
	// "attributes.where" -> подпись признака; неизвестное -> "Поле карточки").
	FieldLabel(path string) string
	AttributeLabel(code string) (string, bool)
	AttributeValueLabel(attr, value string) (string, bool)
}

type IncidentTypeInfo struct {
	ID       string // uuid classifier_categories.id строкой
	Code     string
	Name     string
	ParentID string
	Depth    int
	Path     []string // имена от корня до типа включительно (scenario_context.category.path)
	Services []string // коды служб по классификатору
	// Attributes — коды признаков опросной карты (для GenerateJobRequest.spec.category.attributes)
	Attributes []string
}

type ServiceInfo struct {
	ID        string
	Code      string
	Name      string
	ShortName string
	Kind      string // emergency | city
}
