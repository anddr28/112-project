package auth

import (
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

// Регрессия (review: «лимитер режет пачку верных входов с одного IP»): жетон раньше брался
// до проверки и возвращался после неё, поэтому 6-й одновременный вход класса за NAT получал 429.
func TestLimiterConcurrentSuccessesNotLimited(t *testing.T) {
	t.Parallel()
	l := newIPLimiter(5, 64, 1000)
	var held []*bucket
	for i := range 20 {
		b, retry := l.acquire("10.0.0.1", t0)
		if b == nil {
			t.Fatalf("вход %d из 20 одновременных отклонён (retry=%d)", i+1, retry)
		}
		held = append(held, b)
	}
	for _, b := range held {
		l.release(b, t0, false)
	}
	b := l.buckets["10.0.0.1"]
	if b.inflight != 0 || b.tokens != 5 {
		t.Fatalf("после успешных входов: inflight=%d tokens=%v, want 0/5", b.inflight, b.tokens)
	}
}

func TestLimiterConcurrentGoroutines(t *testing.T) {
	t.Parallel()
	l := newIPLimiter(5, 64, 1000)
	var wg sync.WaitGroup
	var mu sync.Mutex
	rejected := 0
	for range 40 {
		wg.Go(func() {
			b, _ := l.acquire("10.0.0.2", time.Now())
			if b == nil {
				mu.Lock()
				rejected++
				mu.Unlock()
				return
			}
			l.release(b, time.Now(), false)
		})
	}
	wg.Wait()
	if rejected != 0 {
		t.Fatalf("rejected=%d, want 0", rejected)
	}
}

func TestLimiterFailuresCharge(t *testing.T) {
	t.Parallel()
	l := newIPLimiter(5, 64, 1000)
	ip := "192.0.2.10"
	for i := range 5 {
		b, _ := l.acquire(ip, t0)
		if b == nil {
			t.Fatalf("неудача %d: отказ до исчерпания бюджета", i+1)
		}
		l.release(b, t0, true)
	}
	b, retry := l.acquire(ip, t0)
	if b != nil {
		t.Fatal("6-я попытка после 5 неудач допущена")
	}
	if retry != 12 { // 1 жетон / (5/60 в секунду)
		t.Fatalf("retryAfter=%d, want 12", retry)
	}
	// другой IP не затронут
	if b, _ := l.acquire("192.0.2.11", t0); b == nil {
		t.Fatal("чужой IP ограничен")
	}
	// через 12 с появляется жетон
	if b, _ := l.acquire(ip, t0.Add(12*time.Second)); b == nil {
		t.Fatal("после пополнения попытка не допущена")
	}
}

func TestLimiterBurstDebt(t *testing.T) {
	t.Parallel()
	l := newIPLimiter(5, 64, 1000)
	ip := "192.0.2.20"
	var held []*bucket
	for range 10 {
		b, _ := l.acquire(ip, t0)
		if b == nil {
			t.Fatal("всплеск в пределах maxInflight отклонён")
		}
		held = append(held, b)
	}
	for _, b := range held {
		l.release(b, t0, true) // все 10 провалились разом: бюджет 5-10 = -5
	}
	b, retry := l.acquire(ip, t0)
	if b != nil {
		t.Fatal("после долга попытка допущена")
	}
	if retry != 72 { // (1-(-5)) / (5/60)
		t.Fatalf("retryAfter=%d, want 72", retry)
	}
	if b, _ := l.acquire(ip, t0.Add(71*time.Second)); b != nil {
		t.Fatal("долг погашен раньше срока")
	}
	if b, _ := l.acquire(ip, t0.Add(72*time.Second)); b == nil {
		t.Fatal("долг не погашен в срок")
	}
}

func TestLimiterInflightCap(t *testing.T) {
	t.Parallel()
	l := newIPLimiter(5, 3, 1000)
	var held []*bucket
	for range 3 {
		b, _ := l.acquire("ip", t0)
		if b == nil {
			t.Fatal("в пределах maxInflight отказ")
		}
		held = append(held, b)
	}
	b, retry := l.acquire("ip", t0)
	if b != nil || retry != 1 {
		t.Fatalf("4-я одновременная: b=%v retry=%d, want nil/1", b, retry)
	}
	l.release(held[0], t0, false)
	if b, _ := l.acquire("ip", t0); b == nil {
		t.Fatal("после завершения одной проверки место не освободилось")
	}
}

func TestLimiterSweepKeepsInflight(t *testing.T) {
	t.Parallel()
	l := newIPLimiter(5, 64, 3)
	busy, _ := l.acquire("busy", t0)
	for _, ip := range []string{"a", "b"} {
		b, _ := l.acquire(ip, t0)
		l.release(b, t0, true) // неполные корзины
	}
	// карта на пределе: следующая вставка чистит всё простаивающее, но не "busy"
	if b, _ := l.acquire("c", t0); b == nil {
		t.Fatal("новый IP не допущен")
	}
	if l.buckets["busy"] != busy {
		t.Fatal("корзина с незавершённой проверкой удалена при чистке")
	}
	if _, ok := l.buckets["a"]; ok {
		t.Fatal("простаивающая корзина не вычищена при переполнении")
	}
	l.release(busy, t0, false)
	if busy.inflight != 0 {
		t.Fatalf("inflight=%d", busy.inflight)
	}
	// полные простаивающие корзины уходят при плановой чистке (раз в минуту)
	l2 := newIPLimiter(5, 64, 1000)
	b, _ := l2.acquire("x", t0)
	l2.release(b, t0, false)
	l2.acquire("y", t0.Add(2*time.Minute))
	if _, ok := l2.buckets["x"]; ok {
		t.Fatal("полная корзина не вычищена")
	}
	l2.release(nil, t0, true) // nil — без паники
}
