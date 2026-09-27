package main

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func startedDeliverer(t *testing.T, url string) (*deliverer, context.CancelFunc) {
	t.Helper()
	cfg := testConfig(t)
	cfg.callbackURL = url
	d := newDeliverer(cfg, discardLog())
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	d.start(ctx, &wg)
	t.Cleanup(func() { cancel(); wg.Wait() })
	return d, cancel
}

// Контракт go-internal: недоставленный callback повторяется (1/5/30 с).
func TestDelivererRetriesFailedCallback(t *testing.T) {
	t.Parallel()
	sk := newSink(t)
	sk.fail.Store(1) // первый ответ — 503
	d, _ := startedDeliverer(t, sk.srv.URL)
	body := []byte(`{"schema_version":"1"}`)
	start := time.Now()
	d.enqueue(delivery{id: uuid.New(), body: body})
	got := sk.next(t, 5*time.Second)
	if !bytes.Equal(got, body) {
		t.Fatalf("тело %s", got)
	}
	if el := time.Since(start); el < callbackBackoff[0] {
		t.Fatalf("повтор раньше backoff: %v", el)
	}
	if n := sk.calls.Load(); n != 2 {
		t.Fatalf("попыток %d, ждали 2", n)
	}
}

func TestDelivererStopsAfterShutdown(t *testing.T) {
	t.Parallel()
	sk := newSink(t)
	d, cancel := startedDeliverer(t, sk.srv.URL)
	cancel()
	d.enqueue(delivery{id: uuid.New(), body: []byte(`{}`)})
	if len(d.ch) != 0 {
		t.Fatal("после остановки callback поставлен в очередь")
	}
	sk.none(t, 50*time.Millisecond)
}

// Переполнение очереди callback'ов — отброс с предупреждением, а не блокировка воркера.
func TestDelivererOverflowDoesNotBlock(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.callbackURL = "http://127.0.0.1:1/"
	d := newDeliverer(cfg, discardLog()) // не запущен — никто не читает канал
	done := make(chan struct{})
	go func() {
		for range callbackQueueCap + 10 {
			d.enqueue(delivery{id: uuid.New(), body: []byte(`{}`)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("enqueue заблокировался на полной очереди")
	}
	if len(d.ch) != callbackQueueCap {
		t.Fatalf("в очереди %d", len(d.ch))
	}
}

func TestDelivererBadURLDoesNotPanic(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.callbackURL = "http://[::1"
	d := newDeliverer(cfg, discardLog())
	d.send(context.Background(), delivery{id: uuid.New(), body: []byte(`{}`)})
	if len(d.ch) != 0 {
		t.Fatal("некорректный URL не должен ретраиться")
	}
}
