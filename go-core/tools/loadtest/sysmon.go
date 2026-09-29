package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Stand — адреса и docker-окружение стенда (всё параметризуется флагами).
type Stand struct {
	Base    string // https://localhost:8443
	Ops     string // http://127.0.0.1:8080 — healthz/readyz/metrics
	Project string // docker compose project
	PGUser  string
	PGDB    string
	// PGContainer — контейнер PostgreSQL вне compose-проекта (go-core запущен локально, БД — make db-up).
	PGContainer string
}

func docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// container — имя контейнера сервиса compose-проекта (включая остановленные/на паузе).
func (s Stand) container(ctx context.Context, service string) (string, error) {
	if service == "postgres" && s.PGContainer != "" {
		return s.PGContainer, nil
	}
	out, err := docker(ctx, "ps", "-a", "--filter", "label=com.docker.compose.project="+s.Project,
		"--filter", "label=com.docker.compose.service="+service, "--format", "{{.Names}}")
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(strings.Split(strings.TrimSpace(out), "\n")[0])
	if name == "" {
		return "", fmt.Errorf("нет контейнера %s в проекте %s", service, s.Project)
	}
	return name, nil
}

// psql выполняет запрос в контейнере postgres (-At: без заголовков, поля через |).
func (s Stand) psql(ctx context.Context, query string) (string, error) {
	pg, err := s.container(ctx, "postgres")
	if err != nil {
		return "", err
	}
	out, err := docker(ctx, "exec", pg, "psql", "-U", s.PGUser, "-d", s.PGDB, "-v", "ON_ERROR_STOP=1", "-At", "-c", query)
	return strings.TrimSpace(out), err
}

func (s Stand) psqlInts(ctx context.Context, query string) ([]int64, error) {
	out, err := s.psql(ctx, query)
	if err != nil {
		return nil, err
	}
	var res []int64
	for f := range strings.SplitSeq(out, "|") {
		v, err := strconv.ParseInt(strings.TrimSpace(f), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("psql %q: %q", query, out)
		}
		res = append(res, v)
	}
	return res, nil
}

// ---------------------------------------------------------------- docker stats

type ContainerUsage struct {
	Name     string  `json:"name"`
	Samples  int     `json:"samples"`
	CPUAvg   float64 `json:"cpuAvgPct"`
	CPUMax   float64 `json:"cpuMaxPct"`
	MemAvgMB float64 `json:"memAvgMiB"`
	MemMaxMB float64 `json:"memMaxMiB"`
}

type statsSampler struct {
	stand   Stand
	mu      sync.Mutex
	samples map[string][][2]float64 // cpu%, memMiB
	stop    context.CancelFunc
	done    chan struct{}
}

// startStats опрашивает `docker stats --no-stream` контейнеров проекта, пока не вызван stop.
func startStats(stand Stand) *statsSampler {
	ctx, cancel := context.WithCancel(context.Background())
	s := &statsSampler{stand: stand, samples: map[string][][2]float64{}, stop: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for ctx.Err() == nil {
			names, err := docker(ctx, "ps", "--filter", "label=com.docker.compose.project="+stand.Project, "--format", "{{.Names}}")
			if err != nil {
				return
			}
			args := append([]string{"stats", "--no-stream", "--format", "{{json .}}"}, strings.Fields(names)...)
			out, err := docker(ctx, args...)
			if err == nil {
				s.parse(out)
			}
			select {
			case <-ctx.Done():
			case <-time.After(500 * time.Millisecond):
			}
		}
	}()
	return s
}

func (s *statsSampler) parse(out string) {
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		var row struct{ Name, CPUPerc, MemUsage string }
		if json.Unmarshal(sc.Bytes(), &row) != nil {
			continue
		}
		cpu, _ := strconv.ParseFloat(strings.TrimSuffix(row.CPUPerc, "%"), 64)
		mem := parseMiB(strings.TrimSpace(strings.Split(row.MemUsage, "/")[0]))
		s.mu.Lock()
		s.samples[row.Name] = append(s.samples[row.Name], [2]float64{cpu, mem})
		s.mu.Unlock()
	}
}

func parseMiB(v string) float64 {
	units := []struct {
		suf string
		k   float64
	}{{"GiB", 1024}, {"MiB", 1}, {"KiB", 1.0 / 1024}, {"GB", 1000 * 1000 * 1000 / 1048576.0}, {"MB", 1000 * 1000 / 1048576.0}, {"kB", 1000 / 1048576.0}, {"B", 1 / 1048576.0}}
	for _, u := range units {
		if strings.HasSuffix(v, u.suf) {
			f, _ := strconv.ParseFloat(strings.TrimSuffix(v, u.suf), 64)
			return f * u.k
		}
	}
	return 0
}

func (s *statsSampler) Stop() []ContainerUsage {
	s.stop()
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	var res []ContainerUsage
	r1 := func(x float64) float64 { return math.Round(x*10) / 10 }
	for name, xs := range s.samples {
		u := ContainerUsage{Name: name, Samples: len(xs)}
		var cs, msum float64
		for _, x := range xs {
			cs += x[0]
			msum += x[1]
			u.CPUMax = max(u.CPUMax, x[0])
			u.MemMaxMB = max(u.MemMaxMB, x[1])
		}
		u.CPUAvg, u.MemAvgMB = r1(cs/float64(len(xs))), r1(msum/float64(len(xs)))
		u.CPUMax, u.MemMaxMB = r1(u.CPUMax), r1(u.MemMaxMB)
		res = append(res, u)
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Name < res[j].Name })
	return res
}

// ---------------------------------------------------------------- /metrics, /readyz

// scrapeMetrics — значения метрик Prometheus (строка «имя{метки}» → значение).
func scrapeMetrics(ctx context.Context, ops string) (map[string]float64, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ops+"/metrics", nil)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	m := map[string]float64{}
	for line := range strings.SplitSeq(string(b), "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			continue
		}
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err == nil {
			m[line[:i]] = v
		}
	}
	return m, nil
}

func readyz(ctx context.Context, ops string) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ops+"/readyz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ---------------------------------------------------------------- стенд

type BenchInfo struct {
	Host        string `json:"host"`
	CPU         string `json:"cpu"`
	Cores       string `json:"cores"`
	RAM         string `json:"ram"`
	OS          string `json:"os"`
	Docker      string `json:"docker"`
	DockerCPUs  string `json:"dockerCpus"`
	DockerMem   string `json:"dockerMem"`
	GoCoreImage string `json:"goCoreImage"`
	GoCoreVer   string `json:"goCoreVersion"`
	Postgres    string `json:"postgres"`
	GoVersion   string `json:"goVersion"`
	Commit      string `json:"toolCommit"`
}

func sh(ctx context.Context, name string, args ...string) string {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func benchInfo(ctx context.Context, stand Stand, adminHealth map[string]any) BenchInfo {
	b := BenchInfo{
		Host:      sh(ctx, "uname", "-srm"),
		CPU:       sh(ctx, "sysctl", "-n", "machdep.cpu.brand_string"),
		Cores:     sh(ctx, "sysctl", "-n", "hw.ncpu"),
		GoVersion: sh(ctx, "go", "version"),
		Commit:    sh(ctx, "git", "rev-parse", "--short", "HEAD"),
	}
	if b.CPU == "" {
		b.CPU = sh(ctx, "sh", "-c", "grep -m1 'model name' /proc/cpuinfo | cut -d: -f2")
		b.Cores = sh(ctx, "nproc")
	}
	if memB, err := strconv.ParseFloat(sh(ctx, "sysctl", "-n", "hw.memsize"), 64); err == nil {
		b.RAM = fmt.Sprintf("%.0f GiB", memB/(1<<30))
	}
	if v := sh(ctx, "sw_vers", "-productVersion"); v != "" {
		b.OS = "macOS " + v
	} else {
		b.OS = sh(ctx, "sh", "-c", ". /etc/os-release && echo $PRETTY_NAME")
	}
	b.Docker = sh(ctx, "docker", "version", "--format", "{{.Server.Version}} ({{.Server.Os}}/{{.Server.Arch}})")
	info := sh(ctx, "docker", "info", "--format", "{{.NCPU}}|{{.MemTotal}}|{{.OperatingSystem}}")
	if p := strings.Split(info, "|"); len(p) == 3 {
		b.DockerCPUs = p[0]
		if m, err := strconv.ParseFloat(p[1], 64); err == nil {
			b.DockerMem = fmt.Sprintf("%.1f GiB", m/(1<<30))
		}
		b.Docker += ", " + p[2]
	}
	if gc, err := stand.container(ctx, "go-core"); err == nil {
		b.GoCoreImage = sh(ctx, "docker", "inspect", "--format", "{{.Config.Image}} {{.Image}}", gc)
		if len(b.GoCoreImage) > 90 {
			b.GoCoreImage = b.GoCoreImage[:90]
		}
	}
	b.Postgres, _ = stand.psql(ctx, "show server_version")
	if gcore, ok := adminHealth["goCore"].(map[string]any); ok {
		b.GoCoreVer, _ = gcore["version"].(string)
	}
	return b
}
