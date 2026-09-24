package aijobs

import (
	"sync"
	"time"
)

// Circuit breaker — общий для диспетчера и синхронного клиента: ai-service один, и если он
// лежит, бессмысленно и слать задачи, и держать студента 20 с на таймауте хода диалога.
//
//	closed ──N подряд отказов──▶ open ──BreakerOpenSec──▶ half_open ──проба ок──▶ closed
//	                               ▲                          │
//	                               └────────проба упала───────┘
//
// Отказ — только refused / timeout / 5xx (кроме 503). 429/503 «занято» — признак живого
// сервиса (контракт: backpressure не открывает breaker). В half_open пропускается ровно
// одна проба; её результат решает. Поздние ответы запросов, начатых до open, в open
// игнорируются — это устаревшие сведения.

type breakerState uint8

const (
	stateClosed breakerState = iota
	stateOpen
	stateHalfOpen
)

// Строки состояний — как в контракте (SystemHealth.aiService.breakerState).
const (
	BreakerClosed   = "closed"
	BreakerOpen     = "open"
	BreakerHalfOpen = "half_open"
)

func (s breakerState) String() string {
	switch s {
	case stateOpen:
		return BreakerOpen
	case stateHalfOpen:
		return BreakerHalfOpen
	}
	return BreakerClosed
}

// probeTimeout — проба, не отчитавшаяся за это время (паника/утечка), не держит half_open вечно.
const probeTimeout = 30 * time.Second

type breaker struct {
	mu       sync.Mutex
	state    breakerState
	failures int
	openedAt time.Time
	probing  bool
	probeAt  time.Time

	// Параметры читаются на каждом переходе: админ меняет settings.ai без рестарта.
	threshold func() int
	openFor   func() time.Duration
	now       func() time.Time
	onChange  func(from, to breakerState) // вызывается вне мьютекса
}

func newBreaker(threshold func() int, openFor func() time.Duration, onChange func(from, to breakerState)) *breaker {
	return &breaker{threshold: threshold, openFor: openFor, now: time.Now, onChange: onChange}
}

// Allow — можно ли сейчас обращаться к ai-service. probe=true — вызывающий получил
// единственную пробу half_open и обязан отчитаться Success/Failure/Release(true).
func (b *breaker) Allow() (ok, probe bool) {
	b.mu.Lock()
	from := b.state
	now := b.now()
	if b.state == stateOpen && now.Sub(b.openedAt) >= b.openFor() {
		b.state = stateHalfOpen
		b.probing = false
	}
	switch b.state {
	case stateClosed:
		ok = true
	case stateHalfOpen:
		if !b.probing || now.Sub(b.probeAt) >= probeTimeout {
			b.probing, b.probeAt = true, now
			ok, probe = true, true
		}
	}
	to := b.state
	b.mu.Unlock()
	b.notify(from, to)
	return ok, probe
}

// State — текущее состояние (open с истёкшим таймаутом показывается как half_open).
func (b *breaker) State() breakerState {
	b.mu.Lock()
	from := b.state
	if b.state == stateOpen && b.now().Sub(b.openedAt) >= b.openFor() {
		b.state = stateHalfOpen
		b.probing = false
	}
	to := b.state
	b.mu.Unlock()
	b.notify(from, to)
	return to
}

// Open — true, пока breaker не пускает запросы (half_open считается доступным: проба пройдёт).
func (b *breaker) Open() bool { return b.State() == stateOpen }

// Success — сервис ответил (любой осмысленный HTTP-ответ, включая 4xx/429/503).
func (b *breaker) Success() {
	b.mu.Lock()
	from := b.state
	switch b.state {
	case stateClosed:
		b.failures = 0
	case stateHalfOpen:
		b.state = stateClosed
		b.failures = 0
		b.probing = false
	}
	to := b.state
	b.mu.Unlock()
	b.notify(from, to)
}

// Failure — refused / timeout / 5xx.
func (b *breaker) Failure() {
	b.mu.Lock()
	from := b.state
	switch b.state {
	case stateClosed:
		b.failures++
		if b.failures >= max(1, b.threshold()) {
			b.state = stateOpen
			b.openedAt = b.now()
		}
	case stateHalfOpen:
		b.state = stateOpen
		b.openedAt = b.now()
		b.probing = false
	}
	to := b.state
	b.mu.Unlock()
	b.notify(from, to)
}

// Release — проба завершилась без вердикта (вызывающий отменил запрос): отдать пробу другому.
func (b *breaker) Release(probe bool) {
	if !probe {
		return
	}
	b.mu.Lock()
	if b.state == stateHalfOpen {
		b.probing = false
	}
	b.mu.Unlock()
}

func (b *breaker) notify(from, to breakerState) {
	if from != to && b.onChange != nil {
		b.onChange(from, to)
	}
}
