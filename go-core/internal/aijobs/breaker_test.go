package aijobs

import (
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type transition struct{ from, to breakerState }

func newTestBreaker(threshold int, openFor time.Duration) (*breaker, *fakeClock, *[]transition) {
	clk := &fakeClock{t: time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)}
	var trs []transition
	b := newBreaker(func() int { return threshold }, func() time.Duration { return openFor },
		func(from, to breakerState) { trs = append(trs, transition{from, to}) })
	b.now = clk.now
	return b, clk, &trs
}

func TestBreakerOpensAfterThreshold(t *testing.T) {
	t.Parallel()
	b, _, trs := newTestBreaker(3, 30*time.Second)
	for i := 0; i < 2; i++ {
		b.Failure()
	}
	if b.State() != stateClosed {
		t.Fatal("opened before threshold")
	}
	b.Success() // успех в closed сбрасывает счётчик
	b.Failure()
	b.Failure()
	if b.State() != stateClosed {
		t.Fatal("failures were not reset by success")
	}
	b.Failure()
	if b.State() != stateOpen || !b.Open() {
		t.Fatalf("state = %s, want open", b.State())
	}
	if ok, _ := b.Allow(); ok {
		t.Error("open breaker lets requests through")
	}
	if len(*trs) != 1 || (*trs)[0] != (transition{stateClosed, stateOpen}) {
		t.Errorf("transitions = %v", *trs)
	}
	// Поздние ответы в open игнорируются.
	b.Success()
	if b.State() != stateOpen {
		t.Error("late success closed an open breaker")
	}
}

func TestBreakerHalfOpenSingleProbe(t *testing.T) {
	t.Parallel()
	b, clk, trs := newTestBreaker(1, 30*time.Second)
	b.Failure()
	clk.add(29 * time.Second)
	if ok, _ := b.Allow(); ok {
		t.Fatal("allowed before open period elapsed")
	}
	clk.add(time.Second)
	ok, probe := b.Allow()
	if !ok || !probe {
		t.Fatalf("after open period: ok=%v probe=%v, want probe", ok, probe)
	}
	if b.State() != stateHalfOpen || b.Open() {
		t.Fatalf("state = %s, want half_open (and not Open())", b.State())
	}
	if ok, _ := b.Allow(); ok {
		t.Error("second caller got a probe while one is in flight")
	}
	// Проба без вердикта — отдаём другому.
	b.Release(true)
	ok, probe = b.Allow()
	if !ok || !probe {
		t.Fatal("released probe was not handed out again")
	}
	b.Release(false) // не проба — ничего не меняет
	if ok, _ := b.Allow(); ok {
		t.Error("Release(false) freed the probe")
	}
	b.Success()
	if b.State() != stateClosed {
		t.Fatalf("probe success: state = %s", b.State())
	}
	want := []transition{{stateClosed, stateOpen}, {stateOpen, stateHalfOpen}, {stateHalfOpen, stateClosed}}
	if len(*trs) != len(want) {
		t.Fatalf("transitions = %v, want %v", *trs, want)
	}
	for i := range want {
		if (*trs)[i] != want[i] {
			t.Errorf("transition %d = %v, want %v", i, (*trs)[i], want[i])
		}
	}
}

func TestBreakerProbeFailureReopens(t *testing.T) {
	t.Parallel()
	b, clk, _ := newTestBreaker(5, 10*time.Second)
	for i := 0; i < 5; i++ {
		b.Failure()
	}
	clk.add(10 * time.Second)
	if ok, probe := b.Allow(); !ok || !probe {
		t.Fatal("no probe")
	}
	b.Failure()
	if b.State() != stateOpen {
		t.Fatalf("failed probe: state = %s, want open", b.State())
	}
	clk.add(9 * time.Second) // open отсчитывается заново
	if ok, _ := b.Allow(); ok {
		t.Error("reopened breaker allowed too early")
	}
}

func TestBreakerStuckProbeExpires(t *testing.T) {
	t.Parallel()
	b, clk, _ := newTestBreaker(1, time.Second)
	b.Failure()
	clk.add(time.Second)
	if ok, probe := b.Allow(); !ok || !probe {
		t.Fatal("no probe")
	}
	clk.add(probeTimeout - time.Second)
	if ok, _ := b.Allow(); ok {
		t.Fatal("probe handed out twice before timeout")
	}
	clk.add(time.Second)
	if ok, probe := b.Allow(); !ok || !probe {
		t.Error("stuck probe was never replaced")
	}
}

func TestBreakerStateStrings(t *testing.T) {
	t.Parallel()
	for st, want := range map[breakerState]string{stateClosed: BreakerClosed, stateOpen: BreakerOpen, stateHalfOpen: BreakerHalfOpen} {
		if st.String() != want {
			t.Errorf("%d.String() = %q, want %q", st, st.String(), want)
		}
	}
}

func TestBreakerThresholdBelowOne(t *testing.T) {
	t.Parallel()
	b, _, _ := newTestBreaker(0, time.Second) // кривая настройка — хотя бы один отказ
	b.Failure()
	if b.State() != stateOpen {
		t.Error("threshold 0 must behave as 1")
	}
}
