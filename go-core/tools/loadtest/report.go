package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func benchMD(sb *strings.Builder, meta map[string]any) {
	b, ok := meta["bench"].(BenchInfo)
	if !ok {
		return
	}
	fmt.Fprintf(sb, "Стенд: %s, %s ядер, %s RAM, %s; Docker %s (%s CPU, %s); go-core %s (%s); PostgreSQL %s; коммит инструмента %s.\n\n",
		b.CPU, b.Cores, b.RAM, b.OS, b.Docker, b.DockerCPUs, b.DockerMem, b.GoCoreVer, b.GoCoreImage, b.Postgres, b.Commit)
}

func epTable(sb *strings.Builder, eps []EPSummary, total EPSummary) {
	sb.WriteString("| Ручка | Запросов | RPS | p50, мс | p95, мс | p99, мс | max, мс | > 2 с | Ошибок | Коды |\n")
	sb.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|---|\n")
	row := func(e EPSummary, bold bool) {
		var codes []string
		for k, v := range e.Statuses {
			codes = append(codes, fmt.Sprintf("%s×%d", k, v))
		}
		name := "`" + e.Endpoint + "`"
		if bold {
			name = "**" + e.Endpoint + "**"
		}
		fmt.Fprintf(sb, "| %s | %d | %.1f | %.0f | %.0f | %.0f | %.0f | %d | %d | %s |\n",
			name, e.Count, e.RPS, e.P50, e.P95, e.P99, e.Max, e.Over2s, e.Errors, strings.Join(sortedStrings(codes), " "))
	}
	for _, e := range eps {
		row(e, false)
	}
	row(total, true)
	sb.WriteString("\n")
}

func sortedStrings(s []string) []string {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s
}

func writeLoad(path string, meta map[string]any, res []GroupResult) error {
	if err := writeJSON(path+".json", map[string]any{"meta": meta, "groups": res}); err != nil {
		return err
	}
	var sb strings.Builder
	sb.WriteString("# Нагрузочный прогон go-core\n\n")
	benchMD(&sb, meta)
	for _, g := range res {
		fmt.Fprintf(&sb, "## Группа %s. %s — %s\n\n%s\n\nПользователи: %v; окно %.0f с, начало %s.\n\n",
			g.ID, g.Title, g.Verdict, g.Description, g.VUs, g.WindowSec, g.StartedAt.Format("2006-01-02 15:04:05"))
		epTable(&sb, g.Endpoints, g.Total)
		d := g.DB
		fmt.Fprintf(&sb, "Запись в БД за окно: подтверждённых пишущих запросов %d (%.1f/с); строк attempt_events %d (%.1f/с, из них клиентских %d); "+
			"upsert черновиков %d (%.1f/с); pg_stat: строк записано %d, транзакций %d (окно + 11 с).\n\n",
			d.WriteRequestsOK, d.WriteRequestsRate, d.EventRows, d.EventRowsRate, d.ClientEventRows, d.DraftUpserts, d.DraftUpsertsRate,
			d.TuplesWritten, d.XactCommits)
		sb.WriteString("| Контейнер | CPU ср., % | CPU max, % | RAM ср., МиБ | RAM max, МиБ | Замеров |\n|---|---:|---:|---:|---:|---:|\n")
		for _, c := range g.Containers {
			fmt.Fprintf(&sb, "| %s | %.1f | %.1f | %.0f | %.0f | %d |\n", c.Name, c.CPUAvg, c.CPUMax, c.MemAvgMB, c.MemMaxMB, c.Samples)
		}
		fmt.Fprintf(&sb, "\nСчётчики: %v\n\n", g.Counters)
		if g.Extra != nil {
			b, _ := json.Marshal(g.Extra)
			fmt.Fprintf(&sb, "Дополнительно: `%s`\n\n", b)
		}
	}
	return os.WriteFile(path+".md", []byte(sb.String()), 0o644)
}

func writeOutage(path string, meta map[string]any, res []OutageResult) error {
	if err := writeJSON(path+".json", map[string]any{"meta": meta, "scenarios": res}); err != nil {
		return err
	}
	var sb strings.Builder
	sb.WriteString("# Проверка устойчивости к сбоям\n\n")
	benchMD(&sb, meta)
	sb.WriteString("Время — секунды от снятия сбоя. -1 — не наступило.\n\n")
	sb.WriteString("| Сценарий | Сбой, с | 1-й успешный запрос | outbox событий | черновик | WS переподключён | WS пропущенное | readyz | Итог восст. | Событий (в очереди) | exactly-once | черновик = клиент | submit + оценка | Итог |\n")
	sb.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|---|---|---|---|---|\n")
	yes := func(b bool) string {
		if b {
			return "да"
		}
		return "НЕТ"
	}
	for _, r := range res {
		ws := fmt.Sprintf("%.2f", r.WSCatchupSec)
		if r.WSMissed == 0 {
			ws = "—"
		} else {
			ws += fmt.Sprintf(" (%d/%d)", r.WSReplayed, r.WSMissed)
		}
		pass := "PASS"
		if !r.Pass {
			pass = "FAIL"
		}
		fmt.Fprintf(&sb, "| %s | %.1f | %.2f | %.2f | %.2f | %.2f | %s | %.2f | **%.2f** | %d (%d) | %s | %s | %s | %s |\n",
			r.Name, r.OutageSec, r.FirstOKSec, r.EventsFlushSec, r.DraftFlushSec, r.WSReconnectSec, ws, r.ReadyzOKSec, r.RecoverySec,
			r.EventsProduced, r.BacklogAtRestore, yes(r.ExactlyOnce), yes(r.DraftMatches), yes(r.SubmitOK && r.EvaluationDone), pass)
	}
	sb.WriteString("\n")
	for _, r := range res {
		fmt.Fprintf(&sb, "- **%s** — %s БД: %v, API: %v (count, distinct, min, max clientSeq); повторно отправлено дублей %d; "+
			"неуспешных запросов %d; переподключений WS %d, разрывов seq %d; живой поток после восстановления: %s; "+
			"оценка после сдачи за %.1f с; правка оценки применена: %s. %s\n",
			r.Name, r.Description, r.DBEvents, r.APIEvents, r.EventsResent, r.FailedRequests, r.WSReconnects, r.WSGaps,
			yes(r.WSLiveAfter), r.EvaluationSec, yes(r.OverrideApplied), strings.Join(r.Notes, "; "))
	}
	return os.WriteFile(path+".md", []byte(sb.String()), 0o644)
}
