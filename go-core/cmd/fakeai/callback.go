package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	callbackSenders  = 4
	callbackQueueCap = 1024
	callbackTimeout  = 10 * time.Second
)

// callbackBackoff — контракт go-internal: 3 повтора недоставленного callback'а
// через 1/5/30 с, затем результат отбрасывается (задачу вернёт reaper go-core).
var callbackBackoff = [...]time.Duration{time.Second, 5 * time.Second, 30 * time.Second}

type delivery struct {
	id      uuid.UUID
	body    []byte
	attempt int // сколько повторов уже было
}

// deliverer — отправка AiResult в go-core. Отправители не спят в backoff'е: неудачная
// доставка перепланируется таймером, так что лежащий go-core не блокирует остальные
// callback'и, а очередь ограничена (переполнение — отброс с предупреждением).
type deliverer struct {
	url, token string
	log        *slog.Logger
	client     *http.Client
	ch         chan delivery
	ctx        context.Context
}

func newDeliverer(cfg config, log *slog.Logger) *deliverer {
	tr := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        callbackSenders * 2,
		MaxIdleConnsPerHost: callbackSenders * 2,
		IdleConnTimeout:     90 * time.Second,
	}
	return &deliverer{
		url: cfg.callbackURL, token: cfg.token, log: log,
		client: &http.Client{Transport: tr, Timeout: callbackTimeout},
		ch:     make(chan delivery, callbackQueueCap),
	}
}

func (d *deliverer) start(ctx context.Context, wg *sync.WaitGroup) {
	d.ctx = ctx
	for range callbackSenders {
		wg.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case x := <-d.ch:
					d.send(ctx, x)
				}
			}
		})
	}
}

func (d *deliverer) enqueue(x delivery) {
	if d.ctx != nil && d.ctx.Err() != nil {
		return
	}
	select {
	case d.ch <- x:
	default:
		d.log.Warn("очередь callback'ов переполнена — результат отброшен", "request_id", x.id)
	}
}

func (d *deliverer) send(ctx context.Context, x delivery) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(x.body))
	if err != nil {
		d.log.Error("callback: некорректный GOCORE_CALLBACK_URL", "url", d.url, "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", d.token)
	resp, err := d.client.Do(req)
	status := 0
	if err == nil {
		status = resp.StatusCode
		// Дочитываем тело, чтобы соединение вернулось в пул keep-alive.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		if status == http.StatusOK {
			d.log.Debug("callback доставлен", "request_id", x.id, "attempt", x.attempt+1)
			return
		}
	}
	if ctx.Err() != nil {
		return
	}
	if x.attempt >= len(callbackBackoff) {
		d.log.Warn("callback не доставлен, результат отброшен", "request_id", x.id, "status", status, "err", err)
		return
	}
	delay := callbackBackoff[x.attempt]
	d.log.Warn("callback не доставлен, повтор", "request_id", x.id, "status", status, "err", err, "retry_in", delay)
	x.attempt++
	time.AfterFunc(delay, func() { d.enqueue(x) })
}
