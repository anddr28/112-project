package admin

import (
	"context"
	"time"

	"lct/gocore/internal/gen/public"
)

// broadcastBudget — сколько ждать ai-service за один тик (Health/Queue кэшируются в aijobs
// на 5 с, при недоступности сервиса ошибка тоже кэшируется — следующий тик не ждёт).
const broadcastBudget = 5 * time.Second

// RunAIHealthBroadcast — каждые BroadcastEvery (10 с) шлёт MonitorMessage{type: aiHealth}
// во все мониторы занятий, открытые прямо сейчас (преподаватель видит «ИИ перегружен/
// недоступен» и очередь голосовых ходов). Нет открытых мониторов — ai-service не опрашивается.
// Блокирует до отмены ctx; запускать в отдельной горутине.
func (h *Handlers) RunAIHealthBroadcast(ctx context.Context) {
	if h.ws == nil || h.pub == nil || h.ai == nil {
		return
	}
	t := time.NewTicker(h.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-h.kick:
		}
		h.broadcastAIHealth(ctx)
	}
}

// OnBreakerChange — хук aijobs.Service.OnBreakerChange: смена состояния breaker'а
// рассылается сразу, не дожидаясь тика. Не блокирует (требование хука).
func (h *Handlers) OnBreakerChange(string) {
	select {
	case h.kick <- struct{}{}:
	default: // рассылка уже запрошена
	}
}

func (h *Handlers) broadcastAIHealth(ctx context.Context) {
	lessons := h.ws.WatchedLessons()
	if len(lessons) == 0 {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, broadcastBudget)
	msg := h.aiHealthMessage(cctx)
	cancel()
	if ctx.Err() != nil {
		return // остановка сервиса: не шлём сообщение, собранное из отменённых вызовов
	}
	for _, id := range lessons {
		h.pub.Monitor(id, msg) // хаб копирует сообщение и сам ставит seq/at; неблокирующий
	}
}

// aiHealthMessage — {ok, dialogWaiting, dialogAvgMs}. ok — ai-service отвечает, сообщает
// status=ok и breaker не открыт (иначе AI-слои оценки и ИИ-заявитель работают с отказами).
func (h *Handlers) aiHealthMessage(ctx context.Context) public.MonitorMessage {
	m := public.MonitorMessage{Type: public.MonitorMessageTypeAiHealth}
	ah := newOf(m.AiHealth)

	hl, err := h.ai.Health(ctx)
	ok := err == nil && hl != nil && string(hl.Status) == "ok" && h.ai.BreakerState() != "open"
	ah.Ok = &ok
	if q, err := h.ai.Queue(ctx); err == nil && q != nil {
		ah.DialogWaiting = q.DialogWaiting
		ah.DialogAvgMs = q.DialogAvgMs
	}
	m.AiHealth = ah
	return m
}
