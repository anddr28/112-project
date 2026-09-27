// Package metrics — крошечный реестр метрик в текстовом формате Prometheus 0.0.4 без внешних
// зависимостей (ТЗ: «интеграция с локальными средствами мониторинга», db-design §7).
//
// Почему не prometheus/client_golang: изолированный контур, минимум зависимостей, а нужно нам
// немного — счётчики, gauge, гистограммы с фиксированными бакетами и векторы меток.
//
// Горячий путь (Inc/Add/Observe/With) — только атомики и чтение неизменяемой карты через
// atomic.Pointer: ни мьютексов, ни аллокаций. Запись новой серии (редко) — copy-on-write под
// мьютексом. Кардинальность векторов ограничена: сверх лимита значения меток схлопываются
// в "_other", чтобы опечатка/мусорный ввод не раздул память и выдачу /metrics.
//
// Регистрация идемпотентна: повторный New* с тем же именем и типом возвращает ту же метрику
// (конструкторы доменных пакетов можно вызывать повторно — например, в тестах). То же имя
// с другим типом/метками — ошибка программиста (panic при старте).
package metrics

import (
	"compress/gzip"
	"io"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	// ContentType — текстовый формат экспозиции Prometheus.
	ContentType = "text/plain; version=0.0.4; charset=utf-8"
	// DefaultMaxSeries — потолок серий в одном векторе меток.
	DefaultMaxSeries = 1000
	// OverflowLabel — значение всех меток серии-«переполнения».
	OverflowLabel = "_other"
)

// ================================================================ значения

// Counter — монотонный счётчик (uint64, атомарно).
type Counter struct{ v atomic.Uint64 }

func (c *Counter) Inc()          { c.v.Add(1) }
func (c *Counter) Add(n uint64)  { c.v.Add(n) }
func (c *Counter) Value() uint64 { return c.v.Load() }

// Gauge — произвольное значение float64 (биты в atomic.Uint64, Add — CAS-цикл).
type Gauge struct{ bits atomic.Uint64 }

func (g *Gauge) Set(v float64) { g.bits.Store(math.Float64bits(v)) }
func (g *Gauge) Inc()          { g.Add(1) }
func (g *Gauge) Dec()          { g.Add(-1) }
func (g *Gauge) Value() float64 {
	return math.Float64frombits(g.bits.Load())
}

func (g *Gauge) Add(d float64) {
	for {
		old := g.bits.Load()
		if g.bits.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+d)) {
			return
		}
	}
}

// Histogram — гистограмма с фиксированными верхними границами бакетов.
// counts хранятся НЕкумулятивно (одна атомарная операция на наблюдение), кумулятивные
// суммы считаются при выдаче. Count = сумма бакетов — отдельный атомик не нужен.
type Histogram struct {
	upper  []float64       // строго возрастающие границы без +Inf (общие для всех серий семейства)
	counts []atomic.Uint64 // len(upper)+1; последний — «больше всех границ» (+Inf)
	sum    atomic.Uint64   // float64 bits
}

func newHistogram(upper []float64) *Histogram {
	return &Histogram{upper: upper, counts: make([]atomic.Uint64, len(upper)+1)}
}

// Observe — учесть значение (для длительностей — секунды).
func (h *Histogram) Observe(v float64) {
	// бакетов десяток — линейный проход быстрее бинарного поиска и без ветвлений на границах
	i := 0
	for i < len(h.upper) && v > h.upper[i] {
		i++
	}
	h.counts[i].Add(1)
	for {
		old := h.sum.Load()
		if h.sum.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+v)) {
			return
		}
	}
}

// Count — число наблюдений.
func (h *Histogram) Count() uint64 {
	var n uint64
	for i := range h.counts {
		n += h.counts[i].Load()
	}
	return n
}

// Бакеты по умолчанию.
var (
	// DurationBuckets — латентность HTTP/БД, секунды (5 мс … 10 с).
	DurationBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}
	// SlowBuckets — длинные операции (AI-задачи, бэкап), секунды (100 мс … 30 мин).
	SlowBuckets = []float64{.1, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800}
)

// ================================================================ реестр

type family interface {
	appendTo(b []byte) []byte
}

type registry struct {
	mu    sync.Mutex
	byNm  map[string]family
	names []string                 // отсортированы: детерминированная выдача
	snap  atomic.Pointer[[]family] // неизменяемый срез для скрейпа без блокировок
}

var reg = &registry{byNm: map[string]family{}}

func (r *registry) rebuild() {
	s := make([]family, len(r.names))
	for i, n := range r.names {
		s[i] = r.byNm[n]
	}
	r.snap.Store(&s)
}

// register — идемпотентная регистрация: существующее семейство того же типа возвращается
// как есть (match проверяет совместимость меток/бакетов), другое — panic.
func register[T family](name string, mk func() T, match func(T) bool) T {
	if !validName(name) {
		panic("metrics: некорректное имя метрики " + strconv.Quote(name))
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if f, ok := reg.byNm[name]; ok {
		t, ok := f.(T)
		if !ok || (match != nil && !match(t)) {
			panic("metrics: " + name + " уже зарегистрирована с другим типом или метками")
		}
		return t
	}
	f := mk()
	reg.byNm[name] = f
	i := sort.SearchStrings(reg.names, name)
	reg.names = slices.Insert(reg.names, i, name)
	reg.rebuild()
	return f
}

// ---------------------------------------------------------------- одиночные метрики

type counterFamily struct {
	name, help string
	c          Counter
}

func (f *counterFamily) appendTo(b []byte) []byte {
	b = appendHeader(b, f.name, "counter", f.help)
	return appendUintSample(b, f.name, nil, f.c.Value())
}

// NewCounter — счётчик без меток.
func NewCounter(name, help string) *Counter {
	return &register(name, func() *counterFamily { return &counterFamily{name: name, help: help} }, nil).c
}

type gaugeFamily struct {
	name, help string
	g          Gauge
}

func (f *gaugeFamily) appendTo(b []byte) []byte {
	b = appendHeader(b, f.name, "gauge", f.help)
	return appendSample(b, f.name, nil, f.g.Value())
}

// NewGauge — gauge без меток.
func NewGauge(name, help string) *Gauge {
	return &register(name, func() *gaugeFamily { return &gaugeFamily{name: name, help: help} }, nil).g
}

type histFamily struct {
	name, help string
	les        []string
	h          *Histogram
}

func (f *histFamily) appendTo(b []byte) []byte {
	b = appendHeader(b, f.name, "histogram", f.help)
	return appendHistogram(b, f.name, nil, f.les, f.h)
}

// NewHistogram — гистограмма без меток. buckets — возрастающие верхние границы.
func NewHistogram(name, help string, buckets []float64) *Histogram {
	up := normBuckets(buckets)
	return register(name, func() *histFamily {
		return &histFamily{name: name, help: help, les: leStrings(up), h: newHistogram(up)}
	}, func(f *histFamily) bool { return slices.Equal(f.h.upper, up) }).h
}

type funcFamily struct {
	name, help, typ string
	fn              atomic.Pointer[func() float64]
}

func (f *funcFamily) appendTo(b []byte) []byte {
	fn := f.fn.Load()
	if fn == nil {
		return b
	}
	b = appendHeader(b, f.name, f.typ, f.help)
	return appendSample(b, f.name, nil, (*fn)())
}

func registerFunc(name, help, typ string, fn func() float64) {
	f := register(name, func() *funcFamily { return &funcFamily{name: name, help: help, typ: typ} },
		func(f *funcFamily) bool { return f.typ == typ })
	f.fn.Store(&fn)
}

// NewGaugeFunc — gauge, значение которого вычисляется при скрейпе (длина очереди и т.п.).
// fn должна быть быстрой и потокобезопасной. Повторная регистрация заменяет fn.
func NewGaugeFunc(name, help string, fn func() float64) { registerFunc(name, help, "gauge", fn) }

// NewCounterFunc — счётчик, значение которого берётся из чужого монотонного источника.
func NewCounterFunc(name, help string, fn func() float64) { registerFunc(name, help, "counter", fn) }

// ---------------------------------------------------------------- коллекторы

// Emitter — запись семейств метрик из коллектора во время скрейпа.
// Серии одного семейства должны идти подряд: Header, затем Sample'ы.
type Emitter struct{ b []byte }

// Header — строки HELP/TYPE семейства (typ: gauge | counter | untyped).
func (e *Emitter) Header(name, typ, help string) { e.b = appendHeader(e.b, name, typ, help) }

// Sample — серия; labels — пары ключ, значение ("type", "tts", "status", "queued").
func (e *Emitter) Sample(name string, v float64, labels ...string) {
	var lbl []byte
	if len(labels) >= 2 {
		lbl = appendLabelPairs(make([]byte, 0, 64), labels)
	}
	e.b = appendSample(e.b, name, lbl, v)
}

// Gauge — семейство из одной серии без меток.
func (e *Emitter) Gauge(name, help string, v float64) {
	e.Header(name, "gauge", help)
	e.b = appendSample(e.b, name, nil, v)
}

// Counter — монотонное значение из внешнего источника (одна серия без меток).
func (e *Emitter) Counter(name, help string, v float64) {
	e.Header(name, "counter", help)
	e.b = appendSample(e.b, name, nil, v)
}

type collectorFamily struct {
	mu sync.Mutex // коллектор может ходить в БД — параллельные скрейпы не дублируют работу
	fn func(*Emitter)
}

func (f *collectorFamily) appendTo(b []byte) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := Emitter{b: b}
	if f.fn != nil {
		f.fn(&e)
	}
	return e.b
}

// RegisterCollector — функция, выдающая несколько семейств за один проход (статистика пула,
// рантайм, агрегаты из БД). name — ключ регистрации (сортировка выдачи и замена при
// повторной регистрации), не имя метрики. Коллектор, который ходит в БД, обязан сам
// ограничивать время (таймаут) и частоту (кэш) — скрейп не должен нагружать систему.
func RegisterCollector(name string, fn func(e *Emitter)) {
	f := register(name, func() *collectorFamily { return &collectorFamily{} }, nil)
	f.mu.Lock()
	f.fn = fn
	f.mu.Unlock()
}

// ================================================================ векторы меток

type child[T any] struct {
	key string
	lbl []byte // отрендеренные пары `k="v",k2="v2"`
	m   *T
}

type vec[T any] struct {
	name, help string
	labels     []string
	max        int
	mk         func() *T

	mu       sync.Mutex
	children atomic.Pointer[map[string]*child[T]]
	overflow atomic.Pointer[child[T]]
}

func newVec[T any](name, help string, labels []string, mk func() *T) *vec[T] {
	if len(labels) == 0 {
		panic("metrics: вектор " + name + " без меток")
	}
	for _, l := range labels {
		if !validLabel(l) {
			panic("metrics: некорректная метка " + strconv.Quote(l) + " у " + name)
		}
	}
	v := &vec[T]{name: name, help: help, labels: slices.Clone(labels), max: DefaultMaxSeries, mk: mk}
	m := map[string]*child[T]{}
	v.children.Store(&m)
	return v
}

// with — серия по значениям меток. Быстрый путь: ключ собирается в буфере на стеке,
// поиск в неизменяемой карте без блокировок и без аллокаций.
func (v *vec[T]) with(values []string) *T {
	if len(values) != len(v.labels) {
		panic("metrics: неверное число значений меток у " + v.name)
	}
	var kb [192]byte
	key := kb[:0]
	for i, s := range values {
		if i > 0 {
			key = append(key, 0xff)
		}
		key = append(key, s...)
	}
	if c, ok := (*v.children.Load())[string(key)]; ok {
		return c.m
	}
	return v.slow(string(key), values)
}

func (v *vec[T]) slow(key string, values []string) *T {
	v.mu.Lock()
	defer v.mu.Unlock()
	cur := *v.children.Load()
	if c, ok := cur[key]; ok {
		return c.m
	}
	if len(cur) >= v.max {
		if o := v.overflow.Load(); o != nil {
			return o.m
		}
		ov := make([]string, len(v.labels))
		for i := range ov {
			ov[i] = OverflowLabel
		}
		o := &child[T]{key: "\xff" + OverflowLabel, lbl: v.render(ov), m: v.mk()}
		v.overflow.Store(o)
		return o.m
	}
	next := make(map[string]*child[T], len(cur)+1)
	for k, c := range cur {
		next[k] = c
	}
	c := &child[T]{key: key, lbl: v.render(values), m: v.mk()}
	next[key] = c
	v.children.Store(&next)
	return c.m
}

func (v *vec[T]) render(values []string) []byte {
	b := make([]byte, 0, 48)
	for i, l := range v.labels {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, l...)
		b = append(b, `="`...)
		b = appendEscaped(b, values[i], true)
		b = append(b, '"')
	}
	return b
}

// sorted — серии в детерминированном порядке (для выдачи; не горячий путь).
func (v *vec[T]) sorted() []*child[T] {
	cur := *v.children.Load()
	out := make([]*child[T], 0, len(cur)+1)
	for _, c := range cur {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	if o := v.overflow.Load(); o != nil {
		out = append(out, o)
	}
	return out
}

func (v *vec[T]) sameLabels(labels []string) bool { return slices.Equal(v.labels, labels) }

// CounterVec — счётчики с фиксированным набором меток.
type CounterVec struct{ v *vec[Counter] }

// With — счётчик серии (значения меток — в порядке объявления).
func (c *CounterVec) With(values ...string) *Counter { return c.v.with(values) }

func (c *CounterVec) appendTo(b []byte) []byte {
	b = appendHeader(b, c.v.name, "counter", c.v.help)
	for _, ch := range c.v.sorted() {
		b = appendUintSample(b, c.v.name, ch.lbl, ch.m.Value())
	}
	return b
}

// NewCounterVec — вектор счётчиков.
func NewCounterVec(name, help string, labels ...string) *CounterVec {
	return register(name, func() *CounterVec {
		return &CounterVec{v: newVec(name, help, labels, func() *Counter { return new(Counter) })}
	}, func(c *CounterVec) bool { return c.v.sameLabels(labels) })
}

// GaugeVec — gauge с фиксированным набором меток.
type GaugeVec struct{ v *vec[Gauge] }

func (g *GaugeVec) With(values ...string) *Gauge { return g.v.with(values) }

func (g *GaugeVec) appendTo(b []byte) []byte {
	b = appendHeader(b, g.v.name, "gauge", g.v.help)
	for _, ch := range g.v.sorted() {
		b = appendSample(b, g.v.name, ch.lbl, ch.m.Value())
	}
	return b
}

// NewGaugeVec — вектор gauge.
func NewGaugeVec(name, help string, labels ...string) *GaugeVec {
	return register(name, func() *GaugeVec {
		return &GaugeVec{v: newVec(name, help, labels, func() *Gauge { return new(Gauge) })}
	}, func(g *GaugeVec) bool { return g.v.sameLabels(labels) })
}

// HistogramVec — гистограммы с фиксированным набором меток и общими бакетами.
type HistogramVec struct {
	v   *vec[Histogram]
	les []string
}

func (h *HistogramVec) With(values ...string) *Histogram { return h.v.with(values) }

func (h *HistogramVec) appendTo(b []byte) []byte {
	b = appendHeader(b, h.v.name, "histogram", h.v.help)
	for _, ch := range h.v.sorted() {
		b = appendHistogram(b, h.v.name, ch.lbl, h.les, ch.m)
	}
	return b
}

// NewHistogramVec — вектор гистограмм.
func NewHistogramVec(name, help string, buckets []float64, labels ...string) *HistogramVec {
	up := normBuckets(buckets)
	return register(name, func() *HistogramVec {
		return &HistogramVec{
			v:   newVec(name, help, labels, func() *Histogram { return newHistogram(up) }),
			les: leStrings(up),
		}
	}, func(h *HistogramVec) bool { return h.v.sameLabels(labels) && len(h.les) == len(up) })
}

// ================================================================ выдача

var bufPool = sync.Pool{New: func() any { b := make([]byte, 0, 32<<10); return &b }}

var gzPool = sync.Pool{New: func() any {
	w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
	return w
}}

// appendAll — все семейства в порядке имён.
func appendAll(b []byte) []byte {
	if s := reg.snap.Load(); s != nil {
		for _, f := range *s {
			b = f.appendTo(b)
		}
	}
	return b
}

// WriteTo — выдача в текстовом формате (для CLI/тестов).
func WriteTo(w io.Writer) error {
	bp := bufPool.Get().(*[]byte)
	defer putBuf(bp)
	*bp = appendAll((*bp)[:0])
	_, err := w.Write(*bp)
	return err
}

func putBuf(bp *[]byte) {
	if cap(*bp) <= 4<<20 { // гигантские буферы в пуле не держим
		bufPool.Put(bp)
	}
}

// Handler — GET /metrics. Сжимает gzip, если скрейпер его принимает (Prometheus — да):
// выдача с гистограммами по маршрутам — сотни КБ текста, gzip ужимает её на порядок.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		bp := bufPool.Get().(*[]byte)
		defer putBuf(bp)
		*bp = appendAll((*bp)[:0])

		h := w.Header()
		h.Set("Content-Type", ContentType)
		h.Set("Cache-Control", "no-store")
		h.Add("Vary", "Accept-Encoding")
		if acceptsGzip(r.Header.Get("Accept-Encoding")) && len(*bp) > 1024 {
			h.Set("Content-Encoding", "gzip")
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodHead {
				return
			}
			gz := gzPool.Get().(*gzip.Writer)
			gz.Reset(w)
			_, _ = gz.Write(*bp)
			_ = gz.Close()
			gz.Reset(io.Discard) // не держать ссылку на ResponseWriter в пуле
			gzPool.Put(gz)
			return
		}
		h.Set("Content-Length", strconv.Itoa(len(*bp)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(*bp)
		}
	})
}

func acceptsGzip(ae string) bool {
	for part := range strings.SplitSeq(ae, ",") {
		enc, params, _ := strings.Cut(part, ";")
		if !strings.EqualFold(strings.TrimSpace(enc), "gzip") {
			continue
		}
		if q, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(q), 64)
			return err == nil && f > 0
		}
		return true
	}
	return false
}

// ---------------------------------------------------------------- форматирование

func appendHeader(b []byte, name, typ, help string) []byte {
	if help != "" {
		b = append(b, "# HELP "...)
		b = append(b, name...)
		b = append(b, ' ')
		b = appendEscaped(b, help, false)
		b = append(b, '\n')
	}
	b = append(b, "# TYPE "...)
	b = append(b, name...)
	b = append(b, ' ')
	b = append(b, typ...)
	return append(b, '\n')
}

func appendSeriesName(b []byte, name string, lbl []byte) []byte {
	b = append(b, name...)
	if len(lbl) > 0 {
		b = append(b, '{')
		b = append(b, lbl...)
		b = append(b, '}')
	}
	return append(b, ' ')
}

func appendSample(b []byte, name string, lbl []byte, v float64) []byte {
	b = appendSeriesName(b, name, lbl)
	b = appendFloat(b, v)
	return append(b, '\n')
}

func appendUintSample(b []byte, name string, lbl []byte, v uint64) []byte {
	b = appendSeriesName(b, name, lbl)
	b = strconv.AppendUint(b, v, 10)
	return append(b, '\n')
}

func appendHistogram(b []byte, name string, lbl []byte, les []string, h *Histogram) []byte {
	var cum uint64
	for i := range h.counts {
		cum += h.counts[i].Load()
		b = append(b, name...)
		b = append(b, "_bucket{"...)
		if len(lbl) > 0 {
			b = append(b, lbl...)
			b = append(b, ',')
		}
		b = append(b, `le="`...)
		if i < len(les) {
			b = append(b, les[i]...)
		} else {
			b = append(b, "+Inf"...)
		}
		b = append(b, `"} `...)
		b = strconv.AppendUint(b, cum, 10)
		b = append(b, '\n')
	}
	b = appendSeriesNameSuffix(b, name, "_sum", lbl)
	b = appendFloat(b, math.Float64frombits(h.sum.Load()))
	b = append(b, '\n')
	b = appendSeriesNameSuffix(b, name, "_count", lbl)
	b = strconv.AppendUint(b, cum, 10)
	return append(b, '\n')
}

func appendSeriesNameSuffix(b []byte, name, suffix string, lbl []byte) []byte {
	b = append(b, name...)
	b = append(b, suffix...)
	if len(lbl) > 0 {
		b = append(b, '{')
		b = append(b, lbl...)
		b = append(b, '}')
	}
	return append(b, ' ')
}

func appendFloat(b []byte, v float64) []byte {
	switch {
	case math.IsInf(v, 1):
		return append(b, "+Inf"...)
	case math.IsInf(v, -1):
		return append(b, "-Inf"...)
	case math.IsNaN(v):
		return append(b, "NaN"...)
	case v == math.Trunc(v) && math.Abs(v) < 1e15:
		// целые (байты, unix-время, счётчики) — без экспоненты: читаемо и короче
		return strconv.AppendInt(b, int64(v), 10)
	}
	return strconv.AppendFloat(b, v, 'g', -1, 64)
}

// appendEscaped — экранирование по формату 0.0.4: в HELP — \ и \n; в значениях меток ещё и ".
func appendEscaped(b []byte, s string, quote bool) []byte {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\':
			b = append(b, `\\`...)
		case c == '\n':
			b = append(b, `\n`...)
		case c == '"' && quote:
			b = append(b, `\"`...)
		default:
			b = append(b, c)
		}
	}
	return b
}

func appendLabelPairs(b []byte, kv []string) []byte {
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, kv[i]...)
		b = append(b, `="`...)
		b = appendEscaped(b, kv[i+1], true)
		b = append(b, '"')
	}
	return b
}

func normBuckets(in []float64) []float64 {
	out := make([]float64, 0, len(in))
	for _, v := range in {
		if math.IsInf(v, 1) || math.IsNaN(v) {
			continue
		}
		if len(out) > 0 && v <= out[len(out)-1] {
			panic("metrics: границы бакетов должны строго возрастать")
		}
		out = append(out, v)
	}
	return out
}

func leStrings(up []float64) []string {
	s := make([]string, len(up))
	for i, v := range up {
		s[i] = string(appendFloat(nil, v))
	}
	return s
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c == '_' || c == ':' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

func validLabel(s string) bool {
	if s == "" || strings.HasPrefix(s, "__") || s == "le" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}
