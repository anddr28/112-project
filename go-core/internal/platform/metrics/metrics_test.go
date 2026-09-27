package metrics

import (
	"bytes"
	"compress/gzip"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Реестр глобальный и переживает повторные запуски (-count=N): у каждого запуска теста
// свои имена метрик — uniq добавляет суффикс.
var seq atomic.Int64

func uniq(base string) string { return base + "_r" + strconv.FormatInt(seq.Add(1), 10) }

func scrape(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	if err := WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func mustContain(t *testing.T, out string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(out, l+"\n") {
			t.Errorf("нет строки %q", l)
		}
	}
}

func TestCounterGauge(t *testing.T) {
	t.Parallel()
	cn, gn := uniq("t_cg_requests_total"), uniq("t_cg_temp")
	c := NewCounter(cn, "Запросы.")
	c.Inc()
	c.Add(41)
	if c.Value() != 42 {
		t.Fatalf("counter %d", c.Value())
	}
	if NewCounter(cn, "другой help") != c {
		t.Fatal("повторная регистрация — та же метрика")
	}
	g := NewGauge(gn, "Температура.")
	g.Set(1.5)
	g.Inc()
	g.Dec()
	g.Add(-3)
	if g.Value() != -1.5 {
		t.Fatalf("gauge %v", g.Value())
	}
	out := scrape(t)
	mustContain(t, out,
		"# HELP "+cn+" Запросы.",
		"# TYPE "+cn+" counter",
		cn+" 42",
		"# TYPE "+gn+" gauge",
		gn+" -1.5")
}

func TestGaugeConcurrentAdd(t *testing.T) {
	t.Parallel()
	gn := uniq("t_conc_gauge")
	g := NewGauge(gn, "")
	c := NewCounter(uniq("t_conc_counter"), "")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Go(func() {
			for j := 0; j < 1000; j++ {
				g.Add(0.5)
				c.Inc()
			}
		})
	}
	wg.Wait()
	if g.Value() != 8000 || c.Value() != 16000 {
		t.Fatalf("гонка: %v %d", g.Value(), c.Value())
	}
	if strings.Contains(scrape(t), "# HELP "+gn+" ") {
		t.Fatal("пустой help не выводится")
	}
}

func TestHistogram(t *testing.T) {
	t.Parallel()
	hn := uniq("t_hist_seconds")
	h := NewHistogram(hn, "Длительность.", []float64{0.1, 1, math.Inf(1)})
	for _, v := range []float64{0.05, 0.1, 0.5, 2, 3} {
		h.Observe(v)
	}
	if h.Count() != 5 {
		t.Fatalf("count %d", h.Count())
	}
	mustContain(t, scrape(t),
		"# TYPE "+hn+" histogram",
		hn+`_bucket{le="0.1"} 2`, // граница включительно
		hn+`_bucket{le="1"} 3`,
		hn+`_bucket{le="+Inf"} 5`,
		hn+"_sum 5.65",
		hn+"_count 5")
	defer func() {
		if recover() == nil {
			t.Fatal("невозрастающие бакеты приняты")
		}
	}()
	NewHistogram(uniq("t_hist_bad"), "", []float64{1, 1})
}

func TestVectorsAndEscaping(t *testing.T) {
	t.Parallel()
	vn, gvn, hvn := uniq("t_vec_total"), uniq("t_vec_gauge"), uniq("t_vec_seconds")
	cv := NewCounterVec(vn, "По типу.", "type", "status")
	cv.With("tts", "done").Add(3)
	cv.With("tts", "done").Inc()
	cv.With(`кав"ычки\n`, "line\nbreak").Inc()
	gv := NewGaugeVec(gvn, "", "queue")
	gv.With("grammar").Set(7)
	hv := NewHistogramVec(hvn, "", []float64{1}, "route")
	hv.With("GET /x").Observe(0.5)
	out := scrape(t)
	mustContain(t, out,
		vn+`{type="tts",status="done"} 4`,
		vn+`{type="кав\"ычки\\n",status="line\nbreak"} 1`,
		gvn+`{queue="grammar"} 7`,
		hvn+`_bucket{route="GET /x",le="1"} 1`,
		hvn+`_count{route="GET /x"} 1`)
	if NewCounterVec(vn, "", "type", "status") != cv {
		t.Fatal("повторная регистрация вектора")
	}
	for name, fn := range map[string]func(){
		"другие метки":     func() { NewCounterVec(vn, "", "kind") },
		"другой тип":       func() { NewGauge(vn, "") },
		"число значений":   func() { cv.With("only-one") },
		"плохое имя":       func() { NewCounter("t-bad-name", "") },
		"имя с цифры":      func() { NewCounter("1abc", "") },
		"метка le":         func() { NewCounterVec("t_vec_le", "", "le") },
		"метка __":         func() { NewCounterVec("t_vec_dunder", "", "__x") },
		"вектор без меток": func() { NewCounterVec("t_vec_nolabels", "") },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: ожидалась паника", name)
				}
			}()
			fn()
		}()
	}
}

func TestVectorOverflow(t *testing.T) {
	t.Parallel()
	on := uniq("t_overflow_total")
	cv := NewCounterVec(on, "", "user")
	cv.v.max = 3
	for _, u := range []string{"a", "b", "c", "d", "e"} {
		cv.With(u).Inc()
	}
	cv.With("a").Inc() // существующая серия — не в переполнение
	out := scrape(t)
	mustContain(t, out, on+`{user="a"} 2`, on+`{user="_other"} 2`)
	if strings.Contains(out, on+`{user="d"}`) {
		t.Fatal("серия сверх лимита")
	}
}

func TestFuncsAndCollectors(t *testing.T) {
	t.Parallel()
	qn, tn, coll := uniq("t_func_queue"), uniq("t_func_total"), uniq("t_collector")
	jn, sn, cnt, rn := uniq("t_coll_jobs"), uniq("t_coll_single"), uniq("t_coll_count"), uniq("t_coll_replaced")
	NewGaugeFunc(qn, "Очередь.", func() float64 { return 5 })
	NewGaugeFunc(qn, "Очередь.", func() float64 { return 6 }) // замена fn
	NewCounterFunc(tn, "", func() float64 { return 1e6 })
	RegisterCollector(coll, func(e *Emitter) {
		e.Header(jn, "gauge", "Задачи.")
		e.Sample(jn, 2, "type", "tts", "status", "queued")
		e.Sample(jn, 0.25, "odd") // непарные метки — серия без меток
		e.Gauge(sn, "", 1)
		e.Counter(cnt, "", 3)
	})
	out := scrape(t)
	mustContain(t, out,
		qn+" 6",
		"# TYPE "+tn+" counter",
		tn+" 1000000",
		jn+`{type="tts",status="queued"} 2`,
		jn+" 0.25",
		sn+" 1",
		"# TYPE "+cnt+" counter")
	RegisterCollector(coll, func(e *Emitter) { e.Gauge(rn, "", 9) })
	out = scrape(t)
	if strings.Contains(out, sn+" ") || !strings.Contains(out, rn+" 9\n") {
		t.Fatal("повторная регистрация коллектора не заменила функцию")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("gauge-func поверх counter-func")
		}
	}()
	NewCounterFunc(qn, "", func() float64 { return 0 })
}

func TestObserveHTTPAndBuiltins(t *testing.T) {
	t.Parallel()
	route := "GET /" + uniq("t_observe") + "/{id}"
	ObserveHTTP(route, 204, 30*time.Millisecond)
	ObserveHTTP(route, 503, time.Second)
	ObserveHTTP(route, 99, time.Millisecond) // вне классов — unknown
	RegisterPool(nil)                        // без пула — ничего, не паника
	out := scrape(t)
	mustContain(t, out,
		`http_request_duration_seconds_count{route="`+route+`",status_class="2xx"} 1`,
		`http_requests_total{route="`+route+`",status_class="5xx"} 1`,
		`http_requests_total{route="`+route+`",status_class="unknown"} 1`,
		"# TYPE go_goroutines gauge",
		"# TYPE process_uptime_seconds gauge")
	if strings.Contains(out, "pgxpool_total_conns") {
		t.Fatal("метрики пула без пула")
	}
}

func TestAppendFloat(t *testing.T) {
	t.Parallel()
	for v, want := range map[float64]string{
		0: "0", 42: "42", -7: "-7", 1.5: "1.5", 0.005: "0.005", 1e20: "1e+20",
		math.Inf(1): "+Inf", math.Inf(-1): "-Inf", 1700000000: "1700000000",
	} {
		if got := string(appendFloat(nil, v)); got != want {
			t.Errorf("appendFloat(%v) = %q, want %q", v, got, want)
		}
	}
	if got := string(appendFloat(nil, math.NaN())); got != "NaN" {
		t.Errorf("NaN: %q", got)
	}
}

func TestAcceptsGzip(t *testing.T) {
	t.Parallel()
	for ae, want := range map[string]bool{
		"":                    false,
		"gzip":                true,
		"GZIP":                true,
		"deflate, gzip;q=1.0": true,
		"gzip;q=0":            false,
		"gzip; q=0.5, br":     true,
		"br, identity":        false,
		"x-gzip":              false,
		"gzip;q=мусор":        false,
	} {
		if acceptsGzip(ae) != want {
			t.Errorf("acceptsGzip(%q) = %v", ae, !want)
		}
	}
}

func TestHandler(t *testing.T) {
	t.Parallel()
	hn := uniq("t_handler_total")
	NewCounter(hn, "").Inc()
	h := Handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	if rr.Code != 200 || rr.Header().Get("Content-Type") != ContentType || rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("plain: %d %v", rr.Code, rr.Header())
	}
	if !strings.Contains(rr.Body.String(), hn+" 1\n") || rr.Header().Get("Content-Length") == "" {
		t.Fatal("plain тело")
	}

	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("gzip не включился")
	}
	zr, err := gzip.NewReader(rr.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(zr)
	if !strings.Contains(string(body), hn+" 1\n") {
		t.Fatal("gzip тело")
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("HEAD", "/metrics", nil))
	if rr.Code != 200 || rr.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %d", rr.Code, rr.Body.Len())
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/metrics", nil))
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") == "" {
		t.Fatalf("POST: %d", rr.Code)
	}
}

// Выдача детерминирована: семейства по имени, серии по ключу.
func TestDeterministicOutput(t *testing.T) {
	t.Parallel()
	dn := uniq("t_det_total")
	cv := NewCounterVec(dn, "", "k")
	for _, k := range []string{"b", "a", "c"} {
		cv.With(k).Inc()
	}
	NewCounter(dn+"_a", "").Inc() // имя семейства больше dn — идёт после него
	out := scrape(t)
	ia, ib, ic := strings.Index(out, dn+`{k="a"}`), strings.Index(out, dn+`{k="b"}`), strings.Index(out, dn+`{k="c"}`)
	if ia < 0 || !(ia < ib && ib < ic) {
		t.Fatal("серии не отсортированы")
	}
	if strings.Index(out, "# TYPE "+dn+" ") > strings.Index(out, "# TYPE "+dn+"_a ") {
		t.Fatal("семейства не отсортированы по имени")
	}
}
