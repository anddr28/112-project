// Package dialogue — голосовой/интерактивный разговор оператора с ИИ-заявителем
// (docs/voice-mode.md, DESIGN §5 «Разговор»):
//
//   - GET  /attempts/{attemptId}/dialogue        — состояние и транскрипт (восстановление после обрыва);
//   - POST /attempts/{attemptId}/dialogue/turns  — синхронный ход: реплика оператора (аудио/текст)
//     -> ai-service /v1/dialog/turn -> две строки транскрипта -> WS преподавателю;
//   - POST /attempts/{attemptId}/dialogue/end    — «положить трубку»;
//   - GET  /media/tts/{path...}                  — файлы озвучки из общего volume tts_cache;
//   - core.DialogueHooks — вступительная реплика при accept-call и закрытие разговора при submit.
//
// Нумерация: turn_no в БД и во фронте — номер ОБМЕНА (0 — вступление, k — k-я реплика
// оператора и ответ на неё); для ai-service транскрипт перенумеровывается сквозным
// номером на лету (store.DialogueTurnToContract / convert.ExchangeToSeq).
//
// Пакет не импортирует другие доменные пакеты: ai-service — через core.AIClient,
// озвучка — через core.JobQueue, WebSocket — через core.Publisher.
package dialogue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"

	"lct/gocore/internal/config"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/settings"
)

// Deps — зависимости сервиса (связывает internal/app).
type Deps struct {
	Pool      *pgxpool.Pool
	Config    *config.Config
	Settings  *settings.Store
	AI        core.AIClient
	Queue     core.JobQueue
	Publisher core.Publisher
	Log       *slog.Logger
}

// Service — хендлеры разговора и реализация core.DialogueHooks.
type Service struct {
	pool     *pgxpool.Pool
	cfg      *config.Config
	settings *settings.Store
	ai       core.AIClient
	queue    core.JobQueue
	pub      core.Publisher
	log      *slog.Logger

	// Один ход в полёте на попытку (DESIGN §6): дубль того же turnNo ждёт первый и получает
	// его ответ (singleflight по попытке), другой turnNo, пока идёт ход, — 409.
	sf       singleflight.Group
	mu       sync.Mutex
	inflight map[uuid.UUID]int // попытка -> turnNo хода, который сейчас обрабатывается
}

var _ core.DialogueHooks = (*Service)(nil)

// New — без I/O.
func New(d Deps) *Service {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		pool:     d.Pool,
		cfg:      d.Config,
		settings: d.Settings,
		ai:       d.AI,
		queue:    d.Queue,
		pub:      d.Publisher,
		log:      log.With("pkg", "dialogue"),
		inflight: make(map[uuid.UUID]int),
	}
}

// Register — маршруты разговора (паттерны относительно /api/v1, как в frontend.v1.yaml).
// Владение объектом (студент — своя попытка, преподаватель — своё занятие) проверяется
// в хендлерах через internal/access.
func (s *Service) Register(r *httpx.Router) {
	r.Handle("GET /attempts/{attemptId}/dialogue", httpx.Roles(core.RoleStudent, core.RoleTeacher, core.RoleAdmin), s.getDialogue)
	r.Handle("POST /attempts/{attemptId}/dialogue/turns", httpx.Roles(core.RoleStudent), s.postTurn)
	r.Handle("POST /attempts/{attemptId}/dialogue/end", httpx.Roles(core.RoleStudent), s.endDialogue)
	r.Handle("GET /media/tts/{path...}", httpx.Authenticated, s.media)
}

// ---------------------------------------------------------------- один ход в полёте

// flightResult — результат хода, общий для лидера и дублей. turnNo нужен, чтобы запрос,
// попавший в чужой полёт (гонка между проверкой карты и входом в singleflight), не забрал
// ответ на другую реплику.
type flightResult struct {
	turnNo int
	resp   *public.DialogueTurnResponse
	err    error
}

// leaderOnlyError — ошибка, относящаяся только к запросу-лидеру (оборвалась загрузка ЕГО
// аудио). Дубль с тем же turnNo несёт своё аудио целиком и должен попробовать сам.
type leaderOnlyError struct{ err error }

func (e *leaderOnlyError) Error() string { return e.err.Error() }
func (e *leaderOnlyError) Unwrap() error { return e.err }

func errTurnBusy() error {
	return httpx.Conflict("Предыдущая реплика ещё обрабатывается — дождитесь ответа заявителя")
}

// runFlight выполняет ход под защитой «один ход на попытку».
//
// Почему своя карта рядом с singleflight: singleflight склеивает вызовы по ключу, но не
// умеет атомарно сравнить номер хода — поэтому быстрый отказ другому turnNo делается по
// карте, а результат несёт свой turnNo для проверки после склейки. Ожидание дубля не
// прерывается по его ctx: лидер читает тело своего запроса, и отпустить хендлер лидера,
// пока ход идёт, нельзя; сам ход ограничен таймаутом (turnTimeout).
func (s *Service) runFlight(attemptID uuid.UUID, turnNo int, fn func() (*public.DialogueTurnResponse, error)) (*public.DialogueTurnResponse, error) {
	key := attemptID.String()
	for try := 0; try < 2; try++ {
		s.mu.Lock()
		cur, busy := s.inflight[attemptID]
		s.mu.Unlock()
		if busy && cur != turnNo {
			return nil, errTurnBusy()
		}

		ran := false // выполнялась ли МОЯ функция (лидер) — shared этого не различает
		v, _, _ := s.sf.Do(key, func() (any, error) {
			ran = true
			s.mu.Lock()
			s.inflight[attemptID] = turnNo
			s.mu.Unlock()
			defer func() {
				s.mu.Lock()
				delete(s.inflight, attemptID)
				s.mu.Unlock()
			}()
			res := &flightResult{turnNo: turnNo}
			res.resp, res.err = safeCall(fn)
			return res, nil
		})
		fr, _ := v.(*flightResult)
		if fr == nil {
			return nil, httpx.Internal(fmt.Errorf("dialogue: пустой результат хода"))
		}
		if fr.turnNo != turnNo {
			return nil, errTurnBusy()
		}
		var lo *leaderOnlyError
		if !ran && errors.As(fr.err, &lo) {
			continue // у лидера оборвалась загрузка — пробуем со своим аудио
		}
		if errors.As(fr.err, &lo) {
			return fr.resp, lo.err // клиенту — исходная ошибка без обёртки
		}
		return fr.resp, fr.err
	}
	return nil, errTurnBusy()
}

// safeCall — паника хода превращается в 500, а не валит процесс/дубли.
func safeCall(fn func() (*public.DialogueTurnResponse, error)) (resp *public.DialogueTurnResponse, err error) {
	defer func() {
		if p := recover(); p != nil {
			resp, err = nil, httpx.Internal(fmt.Errorf("dialogue: panic in turn: %v", p))
		}
	}()
	return fn()
}

// ---------------------------------------------------------------- публикация

// publishMonitor / publishStudent — вызывать только из pg.OnCommit (после COMMIT).
func (s *Service) publishMonitor(lessonID uuid.UUID, m public.MonitorMessage) {
	if s.pub != nil {
		s.pub.Monitor(lessonID, m)
	}
}

func (s *Service) publishStudent(attemptID uuid.UUID, m public.StudentMessage) {
	if s.pub != nil {
		s.pub.Student(attemptID, m)
	}
}

// publishTurns — реплики и события хода преподавателю (+ «заявитель положил трубку» студенту).
func (s *Service) publishTurns(lessonID, attemptID uuid.UUID, turns []public.DialogueTurnView, events []public.AttemptEvent, callerHungUp *public.DialogueTurnView) {
	for i := range turns {
		t := turns[i]
		s.publishMonitor(lessonID, public.MonitorMessage{
			Type:      public.MonitorMessageTypeDialogueTurn,
			AttemptId: &attemptID,
			Turn:      &t,
		})
	}
	s.publishEvents(lessonID, attemptID, events)
	if callerHungUp != nil {
		t := *callerHungUp
		s.publishStudent(attemptID, public.StudentMessage{Type: public.StudentMessageTypeCallerHungUp, Turn: &t})
	}
}

func (s *Service) publishEvents(lessonID, attemptID uuid.UUID, events []public.AttemptEvent) {
	for i := range events {
		ev := events[i]
		s.publishMonitor(lessonID, public.MonitorMessage{
			Type:      public.MonitorMessageTypeAttemptEvent,
			AttemptId: &attemptID,
			Event:     &ev,
		})
	}
}

// ---------------------------------------------------------------- таймауты

// dialogTimeout — таймаут синхронного хода к ai-service (settings.ai.dialog_timeout_sec,
// иначе GOCORE_AI_SYNC_TIMEOUT, контракт — 20 с). Сам таймаут ставит AIClient; здесь —
// чтобы ограничить весь ход (AI + БД) и чтение тела запроса.
func (s *Service) dialogTimeout(snap *settings.Snapshot) time.Duration {
	d := 20 * time.Second
	if s.cfg != nil && s.cfg.AISyncTimeout > 0 {
		d = s.cfg.AISyncTimeout
	}
	if snap != nil && snap.AI.DialogTimeoutSec > 0 {
		d = time.Duration(snap.AI.DialogTimeoutSec) * time.Second
	}
	return d
}

// turnTimeout — весь ход целиком: ai-service + запись в БД с запасом.
func (s *Service) turnTimeout(snap *settings.Snapshot) time.Duration {
	return s.dialogTimeout(snap) + 10*time.Second
}

func (s *Service) snapshot(ctx context.Context) *settings.Snapshot {
	if s.settings == nil {
		d := settings.Defaults()
		return &d
	}
	return s.settings.Get(ctx)
}
