// Package logring — системный журнал go-core для администратора (GET /admin/logs, контракт
// v1.4): последние записи уровня info и выше в кольцевом буфере в памяти.
//
// Handler — «тройник» над обычным slog-обработчиком (stdout, JSON в контейнере): каждая
// запись уходит как раньше и копируется в кольцо. Полный журнал — по-прежнему stdout
// (docker logs); кольцо — быстрый просмотр ошибок и сбоев из UI без доступа к серверу.
//
// Почему это не тормозит горячие пути:
//   - debug-записи (http-лог каждого запроса и т. п.) в кольцо не попадают вовсе, а info+
//     на горячих путях не пишутся (DESIGN §3: логи — события, а не трассировка);
//   - запись в кольцо — копия нескольких полей под коротким мьютексом, без аллокаций на
//     форматирование: в JSON-вид (map) запись превращается только при чтении администратором;
//   - буфер фиксированного размера (по умолчанию 5000): память ограничена, старое вытесняется.
package logring

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

// DefaultSize — ёмкость кольца (контракт: последние 5000 записей).
const DefaultSize = 5000

// Entry — запись журнала (копия slog.Record без ссылок на изменяемые данные вызывающего).
type Entry struct {
	At      time.Time
	Level   slog.Level
	Message string
	Attrs   []slog.Attr // уже с префиксами групп и атрибутами With(...)
}

// Ring — кольцевой буфер записей. Безопасен для конкурентного использования.
type Ring struct {
	mu    sync.Mutex
	buf   []Entry
	next  int  // куда писать следующую запись
	full  bool // буфер хотя бы раз заполнен
	level slog.Level
}

// New — кольцо на size записей (size ≤ 0 — DefaultSize), копирует записи уровня ≥ info.
func New(size int) *Ring {
	if size <= 0 {
		size = DefaultSize
	}
	return &Ring{buf: make([]Entry, size), level: slog.LevelInfo}
}

// Default — кольцо процесса: в него пишет логгер app.NewLogger, читает GET /admin/logs.
var Default = New(DefaultSize)

func (r *Ring) add(e Entry) {
	r.mu.Lock()
	r.buf[r.next] = e
	r.next++
	if r.next == len(r.buf) {
		r.next, r.full = 0, true
	}
	r.mu.Unlock()
}

// Len — сколько записей сейчас в кольце.
func (r *Ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.full {
		return len(r.buf)
	}
	return r.next
}

// Query — фильтр выборки.
type Query struct {
	MinLevel slog.Level
	Contains string // подстрока в сообщении или атрибутах (без учёта регистра); "" — любые
	Limit    int    // ≤ 0 — все подходящие
}

// Snapshot — подходящие записи, новые сверху. Копия под мьютексом занимает микросекунды
// (5000 структур); фильтр по подстроке — уже без блокировки, чтобы не держать писателей.
func (r *Ring) Snapshot(q Query) []Entry {
	r.mu.Lock()
	var all []Entry
	if r.full {
		all = make([]Entry, 0, len(r.buf))
		all = append(all, r.buf[r.next:]...)
		all = append(all, r.buf[:r.next]...)
	} else {
		all = append(make([]Entry, 0, r.next), r.buf[:r.next]...)
	}
	r.mu.Unlock()

	needle := strings.ToLower(strings.TrimSpace(q.Contains))
	out := make([]Entry, 0, min(len(all), max(q.Limit, 0)+1))
	for i := len(all) - 1; i >= 0; i-- {
		e := all[i]
		if e.Level < q.MinLevel {
			continue
		}
		if needle != "" && !e.contains(needle) {
			continue
		}
		out = append(out, e)
		if q.Limit > 0 && len(out) == q.Limit {
			break
		}
	}
	return out
}

func (e *Entry) contains(needle string) bool {
	if strings.Contains(strings.ToLower(e.Message), needle) {
		return true
	}
	for _, a := range e.Attrs {
		if strings.Contains(strings.ToLower(a.Key), needle) ||
			strings.Contains(strings.ToLower(a.Value.String()), needle) {
			return true
		}
	}
	return false
}

// AttrMap — атрибуты записи для JSON (LogEntry.attrs). Ключи групп — через точку.
func (e *Entry) AttrMap() map[string]any {
	if len(e.Attrs) == 0 {
		return nil
	}
	m := make(map[string]any, len(e.Attrs))
	for _, a := range e.Attrs {
		m[a.Key] = valueOf(a.Value)
	}
	return m
}

func valueOf(v slog.Value) any {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindInt64:
		return v.Int64()
	case slog.KindUint64:
		return v.Uint64()
	case slog.KindFloat64:
		return v.Float64()
	case slog.KindBool:
		return v.Bool()
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().UTC()
	}
	return v.String()
}

// ---------------------------------------------------------------- slog.Handler

// Handler — тройник: запись уходит во внутренний обработчик (если его уровень пропускает)
// и копируется в кольцо (если уровень ≥ info). Уровень кольца не зависит от уровня stdout:
// при GOCORE_LOG_LEVEL=warn info-записи всё равно видны администратору.
type Handler struct {
	inner  slog.Handler
	ring   *Ring
	attrs  []slog.Attr // накопленные With(...), уже с префиксом групп
	prefix string      // "group1.group2." для атрибутов записи
}

// NewHandler — тройник над inner в кольцо ring.
func NewHandler(inner slog.Handler, ring *Ring) *Handler {
	return &Handler{inner: inner, ring: ring}
}

func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.ring.level || h.inner.Enabled(ctx, l)
}

func (h *Handler) Handle(ctx context.Context, rec slog.Record) error {
	var err error
	if h.inner.Enabled(ctx, rec.Level) {
		err = h.inner.Handle(ctx, rec)
	}
	if rec.Level >= h.ring.level {
		e := Entry{At: rec.Time.UTC(), Level: rec.Level, Message: rec.Message}
		if n := len(h.attrs) + rec.NumAttrs(); n > 0 {
			e.Attrs = make([]slog.Attr, 0, n)
			e.Attrs = append(e.Attrs, h.attrs...)
			rec.Attrs(func(a slog.Attr) bool {
				e.Attrs = appendFlat(e.Attrs, h.prefix, a)
				return true
			})
		}
		if e.At.IsZero() {
			e.At = time.Now().UTC()
		}
		h.ring.add(e)
	}
	return err
}

func (h *Handler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.inner = h.inner.WithAttrs(as)
	c.attrs = slices.Clip(h.attrs)
	for _, a := range as {
		c.attrs = appendFlat(c.attrs, h.prefix, a)
	}
	return &c
}

func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.inner = h.inner.WithGroup(name)
	c.prefix = h.prefix + name + "."
	return &c
}

// appendFlat — атрибут в плоском виде: группы раскрываются в "g.key", LogValuer
// разрешается, произвольные значения (ошибки, структуры) фиксируются строкой сейчас —
// кольцо не должно удерживать объекты вызывающего (и видеть их позднейшие изменения).
func appendFlat(dst []slog.Attr, prefix string, a slog.Attr) []slog.Attr {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p += a.Key + "."
		}
		for _, g := range v.Group() {
			dst = appendFlat(dst, p, g)
		}
		return dst
	}
	if a.Key == "" {
		return dst
	}
	if v.Kind() == slog.KindAny {
		switch x := v.Any().(type) {
		case error:
			v = slog.StringValue(x.Error())
		case fmt.Stringer:
			v = slog.StringValue(x.String())
		default:
			v = slog.StringValue(fmt.Sprint(x))
		}
	}
	return append(dst, slog.Attr{Key: prefix + a.Key, Value: v})
}

// ParseLevel — уровень из параметра запроса (debug|info|warn|error); ok=false — неизвестный.
func ParseLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, true
	case "", "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return 0, false
}

// LevelName — уровень записи как в контракте LogEntry.level.
func LevelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	case l >= slog.LevelInfo:
		return "info"
	}
	return "debug"
}
