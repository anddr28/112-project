package main

import (
	"math"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Recorder копит задержки по ручкам. Учитываются только запросы, НАЧАТЫЕ внутри окна
// измерения [from, to): подготовка (создание пользователей, вход, старт занятий) и хвост
// после окна в статистику не попадают.
type Recorder struct {
	mu       sync.Mutex
	from, to time.Time
	eps      map[string]*epRec
	secs     map[int]*secRec
}

type epRec struct {
	lat        []time.Duration
	status     map[int]int
	unexpected int
}

type secRec struct {
	n, errs int
	max     time.Duration
}

func NewRecorder() *Recorder {
	return &Recorder{eps: map[string]*epRec{}, secs: map[int]*secRec{}}
}

func (r *Recorder) SetWindow(from, to time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.from, r.to = from, to
	r.mu.Unlock()
}

func (r *Recorder) add(label string, start time.Time, d time.Duration, status int, expected bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.from.IsZero() || start.Before(r.from) || !start.Before(r.to) {
		return
	}
	e := r.eps[label]
	if e == nil {
		e = &epRec{status: map[int]int{}}
		r.eps[label] = e
	}
	e.lat = append(e.lat, d)
	e.status[status]++
	if !expected {
		e.unexpected++
	}
	s := int(start.Sub(r.from) / time.Second)
	sr := r.secs[s]
	if sr == nil {
		sr = &secRec{}
		r.secs[s] = sr
	}
	sr.n++
	if !expected {
		sr.errs++
	}
	sr.max = max(sr.max, d)
}

// EPSummary — сводка по ручке (миллисекунды).
type EPSummary struct {
	Endpoint string         `json:"endpoint"`
	Count    int            `json:"count"`
	RPS      float64        `json:"rps"`
	Mean     float64        `json:"meanMs"`
	P50      float64        `json:"p50Ms"`
	P90      float64        `json:"p90Ms"`
	P95      float64        `json:"p95Ms"`
	P99      float64        `json:"p99Ms"`
	Max      float64        `json:"maxMs"`
	Over2s   int            `json:"over2s"`
	Errors   int            `json:"errors"`
	Statuses map[string]int `json:"statuses"`
}

type SecBucket struct {
	Sec    int     `json:"sec"`
	Count  int     `json:"count"`
	Errors int     `json:"errors"`
	MaxMs  float64 `json:"maxMs"`
}

func ms(d time.Duration) float64 { return math.Round(float64(d)/float64(time.Millisecond)*10) / 10 }

func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	return sorted[min(max(i, 0), len(sorted)-1)]
}

func summarize(name string, lat []time.Duration, status map[int]int, unexpected int, window time.Duration) EPSummary {
	s := slices.Clone(lat)
	slices.Sort(s)
	var sum time.Duration
	over := 0
	for _, d := range s {
		sum += d
		if d > 2*time.Second {
			over++
		}
	}
	st := map[string]int{}
	for k, v := range status {
		key := strconv.Itoa(k)
		if k == 0 {
			key = "net_error"
		}
		st[key] = v
	}
	out := EPSummary{Endpoint: name, Count: len(s), Statuses: st, Errors: unexpected, Over2s: over}
	if len(s) > 0 {
		out.Mean = ms(sum / time.Duration(len(s)))
		out.P50, out.P90, out.P95, out.P99 = ms(pct(s, 50)), ms(pct(s, 90)), ms(pct(s, 95)), ms(pct(s, 99))
		out.Max = ms(s[len(s)-1])
	}
	if window > 0 {
		out.RPS = math.Round(float64(len(s))/window.Seconds()*10) / 10
	}
	return out
}

// Summary — сводки по ручкам (по убыванию числа запросов), общий итог и посекундная лента.
func (r *Recorder) Summary() (eps []EPSummary, total EPSummary, timeline []SecBucket) {
	r.mu.Lock()
	defer r.mu.Unlock()
	window := r.to.Sub(r.from)
	var all []time.Duration
	allSt := map[int]int{}
	allUn := 0
	for name, e := range r.eps {
		eps = append(eps, summarize(name, e.lat, e.status, e.unexpected, window))
		all = append(all, e.lat...)
		for k, v := range e.status {
			allSt[k] += v
		}
		allUn += e.unexpected
	}
	sort.Slice(eps, func(i, j int) bool {
		if eps[i].Count != eps[j].Count {
			return eps[i].Count > eps[j].Count
		}
		return eps[i].Endpoint < eps[j].Endpoint
	})
	total = summarize("ВСЕ ЗАПРОСЫ", all, allSt, allUn, window)
	for s, b := range r.secs {
		timeline = append(timeline, SecBucket{Sec: s, Count: b.n, Errors: b.errs, MaxMs: ms(b.max)})
	}
	sort.Slice(timeline, func(i, j int) bool { return timeline[i].Sec < timeline[j].Sec })
	return eps, total, timeline
}

// durStats — p50/p95/max набора длительностей (секунды) для отчётов о времени до события.
type DurStats struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50Sec"`
	P95 float64 `json:"p95Sec"`
	Max float64 `json:"maxSec"`
}

func durStats(d []time.Duration) DurStats {
	s := slices.Clone(d)
	slices.Sort(s)
	if len(s) == 0 {
		return DurStats{}
	}
	sec := func(x time.Duration) float64 { return math.Round(x.Seconds()*100) / 100 }
	return DurStats{N: len(s), P50: sec(pct(s, 50)), P95: sec(pct(s, 95)), Max: sec(s[len(s)-1])}
}
