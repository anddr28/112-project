package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type user struct {
	ID    string `json:"id"`
	Login string `json:"login"`
	Role  string `json:"role"`
}

type lesson struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

type attempt struct {
	ID       string `json:"id"`
	LessonID string `json:"lessonId"`
	Status   string `json:"status"`
	SeqNo    int    `json:"seqNo"`
}

type assignedItem struct {
	Lesson  lesson   `json:"lesson"`
	Attempt *attempt `json:"attempt"`
}

// ensureUsers создаёт учётки prefix_<role><NNN> (идемпотентно: 409 = уже есть) и возвращает их
// в порядке номеров. Пароль у всех один (учётки нагрузочного теста, не демо).
func ensureUsers(ctx context.Context, admin *Client, prefix, role, letter string, n int, password string) ([]user, error) {
	logins := make([]string, n)
	for i := range n {
		logins[i] = fmt.Sprintf("%s_%s%03d", prefix, letter, i+1)
	}
	existing, err := pageAll[user](ctx, admin, "/users", 500)
	if err != nil {
		return nil, err
	}
	byLogin := map[string]user{}
	for _, u := range existing {
		byLogin[u.Login] = u
	}
	sem := make(chan struct{}, 4)
	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	for i, l := range logins {
		if _, ok := byLogin[l]; ok {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r := admin.Call(ctx, http.MethodPost, "/users", "", map[string]any{
				"login": l, "password": password, "role": role,
				"lastName": "Нагрузочный", "firstName": fmt.Sprintf("%s %d", role, i+1),
			}, http.StatusConflict)
			var u user
			if r.Status == http.StatusCreated && r.JSON(&u) == nil {
				mu.Lock()
				byLogin[l] = u
				mu.Unlock()
				return
			}
			if r.Status != http.StatusConflict {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("create %s: %s", l, r)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	var out []user
	for _, l := range logins {
		u, ok := byLogin[l]
		if !ok {
			// 409 без записи в списке — перечитываем
			all, err := pageAll[user](ctx, admin, "/users", 500)
			if err != nil {
				return nil, err
			}
			for _, x := range all {
				byLogin[x.Login] = x
			}
			if u, ok = byLogin[l]; !ok {
				return nil, fmt.Errorf("учётка %s не найдена после создания", l)
			}
		}
		if u.Role != "" && u.Role != role {
			return nil, fmt.Errorf("учётка %s уже есть с ролью %s (нужна %s)", l, u.Role, role)
		}
		out = append(out, u)
	}
	return out, nil
}

// loginAll — вход каждого виртуального пользователя своей сессией (параллелизм ограничен:
// argon2id на сервере идёт под семафором, спешить в подготовке незачем).
func loginAll(ctx context.Context, base string, users []user, password string, timeout time.Duration) ([]*Client, error) {
	clients := make([]*Client, len(users))
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	errs := make([]error, len(users))
	for i, u := range users {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			c := newClient(base, u.Login, nil, timeout)
			var err error
			for try := range 5 {
				if err = c.Login(ctx, u.Login, password); err == nil {
					break
				}
				time.Sleep(time.Duration(try+1) * time.Second)
			}
			clients[i], errs[i] = c, err
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return clients, nil
}

type scenario struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Mode   string `json:"mode"`
}

// scenarioPool — подтверждённые сценарии режима cards (демо-сиды).
func scenarioPool(ctx context.Context, t *Client, limit int) ([]string, error) {
	all, err := pageAll[scenario](ctx, t, "/scenarios", 100)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, s := range all {
		if s.Status == "validated" && s.Mode == "cards" {
			ids = append(ids, s.ID)
		}
		if len(ids) == limit {
			break
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("нет подтверждённых сценариев режима cards (демо-сиды?)")
	}
	return ids, nil
}

// createLesson — занятие без голоса (заполнение карточек), cardsPerStudent карточек на студента.
func createLesson(ctx context.Context, t *Client, title string, scenarios []string, participants []user, cards, limitSec int) (lesson, error) {
	ids := make([]string, len(participants))
	for i, u := range participants {
		ids[i] = u.ID
	}
	body := map[string]any{
		"title": title, "mode": "cards", "perspective": "operator112", "timeLimitSec": limitSec,
		"scenarioIds": scenarios, "participantIds": ids, "passThreshold": 60, "allowReplay": true,
		"voice": map[string]any{"enabled": false}, "cardsPerStudent": cards,
	}
	var l lesson
	r := t.Call(ctx, http.MethodPost, "/lessons", "", body)
	if r.Status != http.StatusCreated || r.JSON(&l) != nil {
		return l, fmt.Errorf("create lesson: %s", r)
	}
	r = t.Call(ctx, http.MethodPost, "/lessons/"+l.ID+"/start", "", nil)
	if !r.OK() {
		return l, fmt.Errorf("start lesson: %s", r)
	}
	l.Status = "running"
	return l, nil
}

func finishLesson(ctx context.Context, t *Client, id string) {
	if r := t.Call(ctx, http.MethodPost, "/lessons/"+id+"/finish", "", nil, http.StatusConflict); !r.OK() && r.Status != http.StatusConflict {
		log.Printf("finish lesson %s: %s", id, r)
	}
}

// finishStale завершает идущие занятия нагрузочного теста, оставшиеся от прерванных прогонов.
func finishStale(ctx context.Context, teachers []*Client, titlePrefix string) {
	for _, t := range teachers {
		ls, err := pageAll[lesson](ctx, t, "/lessons", 100)
		if err != nil {
			continue
		}
		for _, l := range ls {
			if strings.HasPrefix(l.Title, titlePrefix) && (l.Status == "running" || l.Status == "scheduled") {
				finishLesson(ctx, t, l.ID)
			}
		}
	}
}

// waitAIDrain ждёт, пока не останется незавершённых оценок (AI-слои доехали) — чтобы очередь
// предыдущей группы не влияла на следующую.
func waitAIDrain(ctx context.Context, ops string, timeout time.Duration) (time.Duration, bool) {
	start := time.Now()
	for time.Since(start) < timeout {
		m, err := scrapeMetrics(ctx, ops)
		if err == nil && m[`lct_evaluations_open{status="pending"}`]+m[`lct_evaluations_open{status="partial"}`] == 0 {
			return time.Since(start), true
		}
		select {
		case <-ctx.Done():
			return time.Since(start), false
		case <-time.After(2 * time.Second):
		}
	}
	return time.Since(start), false
}

// currentAttempt — текущая (не сданная) попытка студента в занятии.
func currentAttempt(ctx context.Context, c *Client, lessonID string) (*attempt, Resp) {
	r := c.Call(ctx, http.MethodGet, "/lessons/assigned?limit=50", "GET /lessons/assigned", nil)
	var items []assignedItem
	if !r.OK() || r.JSON(&items) != nil {
		return nil, r
	}
	for _, it := range items {
		if it.Lesson.ID == lessonID && it.Attempt != nil && (it.Attempt.Status == "issued" || it.Attempt.Status == "in_progress") {
			return it.Attempt, r
		}
	}
	return nil, r
}
