package admin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/public"
)

func TestAIHealthMessage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		ai      func() *fakeAI
		ok      bool
		waiting *int
	}{
		{"ai-service в порядке", healthyAI, true, ptrInt(2)},
		{"breaker open", func() *fakeAI { f := healthyAI(); f.breaker = "open"; return f }, false, ptrInt(2)},
		{"статус degraded", func() *fakeAI { f := healthyAI(); f.health.Status = aiservice.Degraded; return f }, false, ptrInt(2)},
		{"health недоступен, очередь отвечает", func() *fakeAI {
			f := healthyAI()
			f.health, f.healthErr = nil, errors.New("timeout")
			return f
		}, false, ptrInt(2)},
		{"очередь недоступна", func() *fakeAI {
			f := healthyAI()
			f.queue, f.queueErr = nil, errors.New("timeout")
			return f
		}, true, nil},
		{"nil без ошибки", func() *fakeAI {
			f := healthyAI()
			f.health, f.queue = nil, nil
			return f
		}, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := New(Deps{AI: tc.ai(), Log: discardLog()})
			m := h.aiHealthMessage(context.Background())
			if m.Type != public.MonitorMessageTypeAiHealth || m.AiHealth == nil || m.AiHealth.Ok == nil {
				t.Fatalf("message = %+v", m)
			}
			if *m.AiHealth.Ok != tc.ok {
				t.Errorf("ok = %v, want %v", *m.AiHealth.Ok, tc.ok)
			}
			if (tc.waiting == nil) != (m.AiHealth.DialogWaiting == nil) ||
				(tc.waiting != nil && *m.AiHealth.DialogWaiting != *tc.waiting) {
				t.Errorf("dialogWaiting = %v, want %v", m.AiHealth.DialogWaiting, tc.waiting)
			}
		})
	}
}

func ptrInt(v int) *int { return &v }

func TestBroadcastAIHealth(t *testing.T) {
	t.Parallel()
	t.Run("нет открытых мониторов — ai-service не опрашивается", func(t *testing.T) {
		t.Parallel()
		ai, pub := healthyAI(), newRecPublisher()
		h := New(Deps{AI: ai, WS: &fakeWS{}, Publisher: pub, Log: discardLog()})
		h.broadcastAIHealth(context.Background())
		if ai.healthCalls() != 0 || len(pub.all()) != 0 {
			t.Fatalf("calls=%d msgs=%d", ai.healthCalls(), len(pub.all()))
		}
	})
	t.Run("сообщение в каждый открытый монитор", func(t *testing.T) {
		t.Parallel()
		l1, l2 := uuid.New(), uuid.New()
		ai, pub := healthyAI(), newRecPublisher()
		h := New(Deps{AI: ai, WS: &fakeWS{lessons: []uuid.UUID{l1, l2}}, Publisher: pub, Log: discardLog()})
		h.broadcastAIHealth(context.Background())
		msgs := pub.all()
		if len(msgs) != 2 || msgs[0].lesson != l1 || msgs[1].lesson != l2 {
			t.Fatalf("msgs = %+v", msgs)
		}
		for _, m := range msgs {
			if m.msg.Type != public.MonitorMessageTypeAiHealth || m.msg.AiHealth == nil || !*m.msg.AiHealth.Ok {
				t.Errorf("msg = %+v", m.msg)
			}
		}
		if ai.healthCalls() != 1 {
			t.Errorf("health опрошен %d раз, want 1 на рассылку", ai.healthCalls())
		}
	})
	t.Run("остановка сервиса — сообщение не шлётся", func(t *testing.T) {
		t.Parallel()
		pub := newRecPublisher()
		ai := healthyAI()
		ai.delay = time.Minute
		h := New(Deps{AI: ai, WS: &fakeWS{lessons: []uuid.UUID{uuid.New()}}, Publisher: pub, Log: discardLog()})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		h.broadcastAIHealth(ctx)
		if len(pub.all()) != 0 {
			t.Fatalf("после отмены отправлено %d сообщений", len(pub.all()))
		}
	})
}

func TestRunAIHealthBroadcast(t *testing.T) {
	t.Parallel()
	t.Run("без зависимостей — сразу выходит", func(t *testing.T) {
		t.Parallel()
		done := make(chan struct{})
		go func() {
			New(Deps{Log: discardLog()}).RunAIHealthBroadcast(context.Background())
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("RunAIHealthBroadcast без зависимостей не вернулся")
		}
	})
	t.Run("смена breaker'а — внеочередная рассылка; отмена — выход", func(t *testing.T) {
		t.Parallel()
		pub := newRecPublisher()
		h := New(Deps{AI: healthyAI(), WS: &fakeWS{lessons: []uuid.UUID{uuid.New()}}, Publisher: pub,
			BroadcastEvery: time.Hour, Log: discardLog()})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			h.RunAIHealthBroadcast(ctx)
			close(done)
		}()
		h.OnBreakerChange("open")
		select {
		case <-pub.ch:
		case <-time.After(5 * time.Second):
			t.Fatal("OnBreakerChange не вызвал рассылку")
		}
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("RunAIHealthBroadcast не остановился по отмене")
		}
	})
	t.Run("тик по расписанию", func(t *testing.T) {
		t.Parallel()
		pub := newRecPublisher()
		h := New(Deps{AI: healthyAI(), WS: &fakeWS{lessons: []uuid.UUID{uuid.New()}}, Publisher: pub,
			BroadcastEvery: 10 * time.Millisecond, Log: discardLog()})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go h.RunAIHealthBroadcast(ctx)
		for i := 0; i < 2; i++ {
			select {
			case <-pub.ch:
			case <-time.After(5 * time.Second):
				t.Fatalf("тик %d не пришёл", i+1)
			}
		}
	})
}

// OnBreakerChange — хук aijobs: никогда не блокирует, даже если рассылка не запущена.
func TestOnBreakerChangeNeverBlocks(t *testing.T) {
	t.Parallel()
	h := New(Deps{Log: discardLog()})
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			h.OnBreakerChange("half_open")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("OnBreakerChange заблокировался")
	}
	if len(h.kick) != 1 {
		t.Fatalf("толчки должны схлопываться в один, в буфере %d", len(h.kick))
	}
}

func TestNewDefaults(t *testing.T) {
	t.Parallel()
	h := New(Deps{})
	if h.every != defaultBroadcastEvery || h.log == nil || h.started.IsZero() || cap(h.kick) != 1 || cap(h.setMu) != 1 {
		t.Fatalf("defaults: every=%v log=%v started=%v", h.every, h.log, h.started)
	}
}
