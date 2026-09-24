package auth

import (
	"math"
	"sync"
	"time"
)

// ipLimiter — ограничение POST /auth/login по IP (контракт: 5/мин на IP).
//
// Считаются только НЕУДАЧНЫЕ попытки (неверный пароль, неизвестный логин, вход в учётку
// под lockout): token bucket «бюджета неудач» (capacity жетонов, пополнение capacity/мин)
// списывается ПОСЛЕ проверки, и только если она не прошла. Верный пароль бюджет не трогает —
// класс из 20 человек за одним NAT/прокси/пробросом портов Docker Desktop входит разом без 429,
// сколько бы входов ни шло одновременно.
//
// Допуск к проверке: в бюджете есть жетон и одновременных проверок с этого IP меньше
// maxInflight. Если разом провалились несколько одновременных попыток, бюджет уходит в минус
// (долг) и IP ждёт, пока пополнение его не погасит: средняя скорость перебора остаётся
// capacity/мин, единовременный всплеск ограничен maxInflight.
//
// Горутины-уборщика нет: протухшие (полные и простаивающие) корзины вычищаются попутно,
// не чаще раза в минуту, а размер карты ограничен — память ограничена без фоновых циклов.
type ipLimiter struct {
	mu          sync.Mutex
	buckets     map[string]*bucket
	capacity    float64
	perSec      float64 // скорость пополнения, жетонов/с
	maxInflight int
	lastSweep   time.Time
	maxKeys     int
}

// tokenEps — допуск сравнения жетонов: пополнение идёт в float64, и ровно через Retry-After
// секунд должен быть целый жетон, а не 0.9999999 (иначе честно выждавший клиент снова получит 429).
const tokenEps = 1e-9

type bucket struct {
	tokens   float64 // бюджет неудач; < 0 — долг после одновременного всплеска неудач
	last     time.Time
	inflight int // допущенные и ещё не завершённые проверки
}

func newIPLimiter(perMinute, maxInflight, maxKeys int) *ipLimiter {
	return &ipLimiter{
		buckets:     make(map[string]*bucket, 64),
		capacity:    float64(perMinute),
		perSec:      float64(perMinute) / 60,
		maxInflight: max(1, maxInflight),
		maxKeys:     maxKeys,
	}
}

// acquire допускает попытку входа. nil — отказ, retryAfter — через сколько секунд повторить.
// Допущенную попытку обязательно завершить release (с тем же *bucket).
func (l *ipLimiter) acquire(key string, now time.Time) (b *bucket, retryAfter int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastSweep) > time.Minute || len(l.buckets) >= l.maxKeys {
		l.sweepLocked(now)
	}
	b = l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.capacity, last: now}
		l.buckets[key] = b
	} else {
		l.refill(b, now)
	}
	if b.tokens < 1-tokenEps {
		return nil, max(1, int(math.Ceil((1-b.tokens)/l.perSec-tokenEps)))
	}
	if b.inflight >= l.maxInflight {
		return nil, 1
	}
	b.inflight++
	return b, 0
}

// release завершает допущенную попытку; failed — проверка не прошла (списать жетон).
// Корзина с незавершёнными попытками из карты не удаляется, поэтому указатель всегда живой.
func (l *ipLimiter) release(b *bucket, now time.Time, failed bool) {
	if b == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if b.inflight > 0 {
		b.inflight--
	}
	if failed {
		l.refill(b, now)
		b.tokens--
	}
}

func (l *ipLimiter) refill(b *bucket, now time.Time) {
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = min(l.capacity, b.tokens+el*l.perSec)
		b.last = now
	}
}

// sweepLocked удаляет простаивающие корзины, которые к now уже полны (их отсутствие
// эквивалентно полной). Если карта всё равно упёрлась в предел (тысячи разных адресов
// за минуту) — удаляем все простаивающие: это вырожденный случай, и держать память под
// атакующего хуже. Корзины с незавершёнными попытками не трогаем никогда.
func (l *ipLimiter) sweepLocked(now time.Time) {
	l.lastSweep = now
	for k, b := range l.buckets {
		if b.inflight == 0 && b.tokens+now.Sub(b.last).Seconds()*l.perSec >= l.capacity {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) >= l.maxKeys {
		for k, b := range l.buckets {
			if b.inflight == 0 {
				delete(l.buckets, k)
			}
		}
	}
}
