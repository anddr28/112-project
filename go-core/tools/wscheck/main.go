// Command wscheck — ручная проверка WebSocket-мониторинга: входит преподавателем, подключается
// к /ws/lessons/{id}/monitor и печатает сообщения (snapshot, participantStatus, …).
//
//	go run ./tools/wscheck -base https://localhost:8443 -lesson <uuid> [-login teacher -password teacher] [-n 5]
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

func main() {
	base := flag.String("base", "https://localhost:8443", "адрес go-core")
	lesson := flag.String("lesson", "", "id занятия")
	login := flag.String("login", "teacher", "логин")
	password := flag.String("password", "teacher", "пароль")
	n := flag.Int("n", 3, "сколько сообщений прочитать")
	since := flag.String("since", "", "since (seq)")
	flag.Parse()
	if *lesson == "" {
		fmt.Fprintln(os.Stderr, "нужен -lesson")
		os.Exit(2)
	}
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} // CA контура самоподписанный
	body, _ := json.Marshal(map[string]string{"login": *login, "password": *password})
	req, _ := http.NewRequest(http.MethodPost, *base+"/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "fetch")
	resp, err := hc.Do(req)
	if err != nil || resp.StatusCode != 200 {
		fmt.Fprintln(os.Stderr, "login failed:", err, resp)
		os.Exit(1)
	}
	resp.Body.Close()

	wsURL := strings.Replace(*base, "http", "ws", 1) + "/api/v1/ws/lessons/" + *lesson + "/monitor"
	if *since != "" {
		wsURL += "?since=" + url.QueryEscape(*since)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: hc,
		HTTPHeader: http.Header{"Origin": []string{*base}}})
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial:", err)
		os.Exit(1)
	}
	defer c.CloseNow()
	for i := 0; i < *n; i++ {
		_, data, err := c.Read(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read:", err)
			os.Exit(1)
		}
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		extra := ""
		if s, ok := m["snapshot"].(map[string]any); ok {
			atts, _ := s["attempts"].([]any)
			extra = fmt.Sprintf(" attempts=%d", len(atts))
		}
		fmt.Printf("seq=%v type=%v%s bytes=%d\n", m["seq"], m["type"], extra, len(data))
	}
}
