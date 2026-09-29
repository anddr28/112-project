// Command loadtest — нагрузочное тестирование и проверка устойчивости go-core через публичный
// API (как ходит SPA: HTTPS/HTTP2, cookie-сессия, X-Requested-With: fetch, WebSocket).
//
//	go run ./tools/loadtest load   [-base https://localhost:8443] [-groups 1,2,3] [-window 30s] ...
//	go run ./tools/loadtest outage [-base https://localhost:8443] [-scenarios proxy:5s,proxy:15s,...] ...
//
// load — три группы по -window (по умолчанию 30 с) при 100 одновременных пользователях;
// подготовка (учётки, вход, занятия) в замер не входит. outage — клиент «как SPA» (outbox
// событий с clientSeq, автосохранение черновика, WebSocket с since) через встроенный TCP-прокси
// с управляемым обрывом, а также сбои на стороне сервера (docker pause / network disconnect /
// restart go-core, остановка PostgreSQL). Результаты — JSON и Markdown в -out.
// Нужен docker CLI (docker stats, psql в контейнере postgres, внесение сбоев).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 || (os.Args[1] != "load" && os.Args[1] != "outage") {
		fmt.Fprintln(os.Stderr, "использование: loadtest load|outage [флаги]; -h — справка по флагам")
		os.Exit(2)
	}
	mode := os.Args[1]
	fs := flag.NewFlagSet(mode, flag.ExitOnError)
	base := fs.String("base", "https://localhost:8443", "адрес go-core для браузеров (SPA + API + WS)")
	ops := fs.String("ops", "http://127.0.0.1:8080", "адрес healthz/readyz/metrics go-core")
	project := fs.String("project", "lct112", "имя docker compose проекта стенда")
	pgContainer := fs.String("pg-container", "", "контейнер PostgreSQL вне compose-проекта (например lct-pg из make db-up)")
	pgUser := fs.String("pg-user", "lct", "пользователь PostgreSQL (psql в контейнере postgres)")
	pgDB := fs.String("pg-db", "lct", "база PostgreSQL")
	adminCred := fs.String("admin", "admin:admin", "логин:пароль администратора (создаёт учётки теста)")
	prefix := fs.String("prefix", "lt", "префикс логинов учёток теста")
	password := fs.String("password", "LoadTest-2026!", "пароль учёток теста")
	out := fs.String("out", "../docs/testing/results", "каталог результатов (JSON + Markdown)")
	tag := fs.String("tag", "", "метка в имени файлов результатов (по умолчанию — дата-время)")
	timeout := fs.Duration("timeout", 15*time.Second, "таймаут запроса клиента (у SPA — 15 с)")
	evalWait := fs.Duration("eval-wait", 5*time.Minute, "сколько ждать итоговой оценки AI-слоёв")

	// load
	students := fs.Int("students", 100, "load: обучающихся")
	teachers := fs.Int("teachers", 15, "load: преподавателей (группа 2)")
	admins := fs.Int("admins", 5, "load: администраторов (группа 2)")
	window := fs.Duration("window", 30*time.Second, "load: длительность замера каждой группы")
	groups := fs.String("groups", "1,2,3", "load: какие группы запускать")
	think := fs.Float64("think", 1, "load: множитель пауз пользователей (1 — реалистично, 0.2 — стресс)")
	drain := fs.Duration("drain-wait", 5*time.Minute, "load: сколько ждать опустошения очереди оценок между группами")

	// outage
	scen := fs.String("scenarios", "proxy:5s,proxy:15s,proxy:29s,pause:20s,netdisconnect:20s,restart,pgrestart:10s",
		"outage: сценарии через запятую: proxy:D (обрыв сети клиента), blackhole:D, pause:D, netdisconnect:D, restart, pgrestart:D, pgkill:D")
	recovery := fs.Duration("recovery-limit", 30*time.Second, "outage: допустимое время восстановления (ТЗ — 30 с)")
	_ = fs.Parse(os.Args[2:])

	al, ap, ok := strings.Cut(*adminCred, ":")
	if !ok {
		log.Fatal("-admin: нужно логин:пароль")
	}
	stand := Stand{Base: strings.TrimRight(*base, "/"), Ops: strings.TrimRight(*ops, "/"), Project: *project, PGUser: *pgUser, PGDB: *pgDB, PGContainer: *pgContainer}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if !readyz(ctx, stand.Ops) {
		log.Fatalf("go-core не готов: %s/readyz", stand.Ops)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	stamp := *tag
	if stamp == "" {
		stamp = time.Now().Format("20060102-150405")
	}

	switch mode {
	case "load":
		cfg := loadConfig{Stand: stand, AdminLogin: al, AdminPass: ap, Prefix: *prefix, Password: *password,
			Students: *students, Teachers: *teachers, Admins: *admins, Window: *window, Think: *think,
			Groups: strings.Split(*groups, ","), Timeout: *timeout, EvalWait: *evalWait, DrainWait: *drain}
		meta, res, err := runLoad(ctx, cfg)
		path := filepath.Join(*out, "load-"+stamp)
		if meta != nil {
			if werr := writeLoad(path, meta, res); werr != nil {
				log.Print(werr)
			} else {
				log.Printf("результаты: %s.json, %s.md", path, path)
			}
		}
		if err != nil {
			log.Fatal(err)
		}
		for _, g := range res {
			if !g.Pass {
				os.Exit(1)
			}
		}
	case "outage":
		cfg := outageConfig{Stand: stand, AdminLogin: al, AdminPass: ap, Prefix: *prefix, Password: *password,
			Scenarios: strings.Split(*scen, ","), Timeout: *timeout, EvalWait: *evalWait, Recovery: *recovery}
		meta, res, err := runOutage(ctx, cfg)
		path := filepath.Join(*out, "outage-"+stamp)
		if meta != nil {
			if werr := writeOutage(path, meta, res); werr != nil {
				log.Print(werr)
			} else {
				log.Printf("результаты: %s.json, %s.md", path, path)
			}
		}
		if err != nil {
			log.Fatal(err)
		}
		for _, r := range res {
			if !r.Pass {
				os.Exit(1)
			}
		}
	}
}
