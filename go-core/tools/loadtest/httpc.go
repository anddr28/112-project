package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Client — «браузер» одного пользователя: своя cookie-сессия (lct_session), свой пул
// соединений (HTTP/2 поверх TLS, как у браузера), отдельный HTTP/1.1-клиент для WebSocket
// с той же cookie. Каждый запрос пишется в Recorder (если задан и окно измерения открыто).
type Client struct {
	Base string // https://host:port — без /api/v1
	Name string
	HC   *http.Client
	WSHC *http.Client
	Rec  *Recorder
}

// newClient: timeout — потолок запроса (у SPA — 15 с, httpApi.ts TIMEOUT_MS).
func newClient(base, name string, rec *Recorder, timeout time.Duration) *Client {
	jar, _ := cookiejar.New(nil)
	tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // CA контура самоподписанный
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 15 * time.Second}
	tr := &http.Transport{
		TLSClientConfig:       tlsCfg,
		ForceAttemptHTTP2:     true,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: timeout,
	}
	wsTr := &http.Transport{
		TLSClientConfig:     tlsCfg.Clone(),
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	return &Client{
		Base: strings.TrimRight(base, "/"),
		Name: name,
		HC:   &http.Client{Jar: jar, Timeout: timeout, Transport: tr},
		WSHC: &http.Client{Jar: jar, Transport: wsTr},
		Rec:  rec,
	}
}

// Resp — результат запроса. Status 0 — сетевая ошибка/таймаут (Err).
type Resp struct {
	Status int
	Body   []byte
	Header http.Header
	Dur    time.Duration
	Err    error
}

func (r Resp) OK() bool { return r.Err == nil && r.Status >= 200 && r.Status < 300 }

func (r Resp) JSON(v any) error {
	if r.Err != nil {
		return r.Err
	}
	if err := json.Unmarshal(r.Body, v); err != nil {
		return fmt.Errorf("decode %d: %w (%.200s)", r.Status, err, r.Body)
	}
	return nil
}

func (r Resp) String() string {
	if r.Err != nil {
		return "err: " + r.Err.Error()
	}
	return fmt.Sprintf("%d %.300s", r.Status, r.Body)
}

// Call выполняет запрос к /api/v1+path. label — имя ручки в отчёте (шаблон пути);
// expect — дополнительные «штатные» коды (кроме 2xx), которые не считаются ошибкой.
func (c *Client) Call(ctx context.Context, method, path, label string, body any, expect ...int) Resp {
	var rd io.Reader
	var raw []byte
	if body != nil {
		switch b := body.(type) {
		case []byte:
			raw = b
		default:
			var err error
			if raw, err = json.Marshal(b); err != nil {
				return Resp{Err: err}
			}
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+"/api/v1"+path, rd)
	if err != nil {
		return Resp{Err: err}
	}
	req.Header.Set("Accept", "application/json")
	if method != http.MethodGet {
		req.Header.Set("X-Requested-With", "fetch")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	resp, err := c.HC.Do(req)
	var r Resp
	if err != nil {
		r = Resp{Err: err, Dur: time.Since(start)}
	} else {
		b, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		r = Resp{Status: resp.StatusCode, Body: b, Header: resp.Header, Dur: time.Since(start), Err: rerr}
		if rerr != nil {
			r.Status = 0
		}
	}
	if label == "" {
		label = method + " " + path
	}
	if ctx.Err() != nil && r.Err != nil {
		// запрос оборван нами (конец окна) — не ошибка системы, не учитываем
		return r
	}
	expected := r.OK()
	for _, e := range expect {
		if r.Status == e {
			expected = true
		}
	}
	c.Rec.add(label, start, r.Dur, r.Status, expected)
	return r
}

// Login — POST /auth/login (cookie сессии остаётся в jar).
func (c *Client) Login(ctx context.Context, login, password string) error {
	r := c.Call(ctx, http.MethodPost, "/auth/login", "POST /auth/login",
		map[string]string{"login": login, "password": password})
	if !r.OK() {
		return fmt.Errorf("login %s: %s", login, r)
	}
	return nil
}

// DialWS открывает WebSocket (path — от /api/v1). Рукопожатие пишется в Recorder как label.
func (c *Client) DialWS(ctx context.Context, path, label string) (*websocket.Conn, error) {
	u, err := url.Parse(c.Base)
	if err != nil {
		return nil, err
	}
	scheme := "wss"
	if u.Scheme == "http" {
		scheme = "ws"
	}
	wsURL := scheme + "://" + u.Host + "/api/v1" + path
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	start := time.Now()
	conn, resp, err := websocket.Dial(dctx, wsURL, &websocket.DialOptions{
		HTTPClient: c.WSHC,
		HTTPHeader: http.Header{"Origin": []string{c.Base}},
	})
	status := 101
	if err != nil {
		status = 0
		if resp != nil {
			status = resp.StatusCode
		}
		if ctx.Err() != nil {
			return nil, err
		}
	}
	c.Rec.add(label, start, time.Since(start), status, err == nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(4 << 20)
	return conn, nil
}

func isCtxErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// pageAll обходит постраничный список по X-Next-Cursor.
func pageAll[T any](ctx context.Context, c *Client, path string, limit int) ([]T, error) {
	var all []T
	cursor := ""
	for range 100 {
		p := fmt.Sprintf("%s%slimit=%d", path, sep(path), limit)
		if cursor != "" {
			p += "&cursor=" + url.QueryEscape(cursor)
		}
		r := c.Call(ctx, http.MethodGet, p, "", nil)
		var page []T
		if err := r.JSON(&page); err != nil || !r.OK() {
			return nil, fmt.Errorf("GET %s: %s", p, r)
		}
		all = append(all, page...)
		cursor = r.Header.Get("X-Next-Cursor")
		if cursor == "" {
			return all, nil
		}
	}
	return all, errors.New("pagination did not terminate")
}

func sep(p string) string {
	if strings.Contains(p, "?") {
		return "&"
	}
	return "?"
}
