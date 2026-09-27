// Package audit — журнал аудита (ТЗ: аудит всех значимых действий, хранение ≥ 6 мес;
// db-design Р11, §6): асинхронный батч-писатель (core.Auditor), помесячные партиции
// audit_log и выборка для админки.
//
// Главный принцип: аудит никогда не тормозит и не роняет бизнес-операцию. Log только
// собирает строку и кладёт её в ограниченный канал (переполнение — сброс со счётчиком и
// редким предупреждением), запись в БД — фоновый COPY батчами.
package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/core"
	"lct/gocore/internal/platform/metrics"
)

const (
	queueCap     = 10_000                 // строк в очереди; ~80 КБ указателей в простое
	batchMax     = 256                    // строк в одном COPY
	flushEvery   = 200 * time.Millisecond // максимальная задержка записи от первой строки батча
	flushTimeout = 5 * time.Second        // на один COPY
	retryPause   = 500 * time.Millisecond
	maxUA        = 512 // User-Agent длиннее не нужен для расследований
	maxText      = 200 // action/entity_type/actor_role — короткие идентификаторы

	// maxAtClamp — насколько at может «догонять» предыдущую запись при шаге часов назад.
	// Больший откат (часы стояли в будущем и их исправили) принимается как есть: иначе все
	// следующие записи получали бы одно и то же будущее время, пока часы его не догонят.
	maxAtClamp = time.Minute

	// MaxValueBytes — потолок before/after одной записи (больше — заменяется пометкой).
	MaxValueBytes = 64 << 10
	warnEvery     = 10 * time.Second

	// RoleSystem — actor_role записей планировщика и фоновых задач (actor_id = NULL).
	RoleSystem = "system"
	// Redacted — чем заменяются значения секретных ключей в before/after.
	Redacted = "[REDACTED]"
)

var (
	auditTable   = pgx.Identifier{"audit_log"}
	auditColumns = []string{"at", "actor_id", "actor_role", "action", "entity_type", "entity_id",
		"lesson_id", "before", "after", "ip", "user_agent", "request_id"}

	mWritten = metrics.NewCounter("lct_audit_written_total", "Записи аудита, сохранённые в БД.")
	mDropped = metrics.NewCounterVec("lct_audit_dropped_total", "Записи аудита, отброшенные без сохранения.", "reason")
	mBatches = metrics.NewHistogram("lct_audit_flush_seconds", "Длительность записи батча аудита (COPY), секунды.", metrics.DurationBuckets)
)

// row — строка audit_log. Нулевые значения (uuid.Nil, "", nil, невалидный адрес) = NULL.
type row struct {
	at         time.Time
	actorID    uuid.UUID
	actorRole  string
	action     string
	entityType string
	entityID   uuid.UUID
	lessonID   uuid.UUID
	before     []byte
	after      []byte
	ip         netip.Addr
	userAgent  string
	requestID  uuid.UUID
}

// Writer — асинхронный писатель аудита, реализует core.Auditor.
type Writer struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	ch       chan *row
	quit     chan struct{}
	done     chan struct{}
	started  atomic.Bool
	stopping atomic.Bool
	stopOnce sync.Once

	dropped  atomic.Uint64
	lastWarn atomic.Int64

	// enqMu упорядочивает «метка времени → постановка в очередь»: at строк растёт в том же
	// порядке, в каком COPY раздаёт им id. Иначе при параллельных Log строка с большим at
	// получала бы меньший id, и курсор GET /admin/audit (ORDER BY at DESC, id DESC,
	// beforeId → id < beforeId) пропускал бы записи на границе страниц. lastAt — защита
	// от небольшого шага системных часов назад (NTP), см. maxAtClamp.
	enqMu  sync.Mutex
	lastAt time.Time
	now    func() time.Time // time.Now; подменяется в тестах
}

var _ core.Auditor = (*Writer)(nil)

// NewWriter создаёт писатель. Start запускает фоновую запись; до Start записи копятся в очереди.
func NewWriter(pool *pgxpool.Pool, log *slog.Logger) *Writer {
	if log == nil {
		log = slog.Default()
	}
	w := &Writer{
		pool: pool,
		log:  log.With("component", "audit"),
		ch:   make(chan *row, queueCap),
		quit: make(chan struct{}),
		done: make(chan struct{}),
		now:  time.Now,
	}
	metrics.NewGaugeFunc("lct_audit_queue_length", "Записи аудита, ожидающие записи в БД.", func() float64 {
		return float64(len(w.ch))
	})
	return w
}

// Start запускает фоновую запись (одна горутина). Писатель работает до Stop, а не до отмены
// ctx: при остановке сервиса HTTP-обработчики ещё дописывают аудит, и эти записи терять
// нельзя — порядок в app: остановить HTTP → Stop(аудит).
func (w *Writer) Start(ctx context.Context) {
	_ = ctx
	if w.started.CompareAndSwap(false, true) {
		go w.run()
	}
}

// Stop дописывает очередь и останавливает писатель. Возвращает ctx.Err(), если не успел.
func (w *Writer) Stop(ctx context.Context) error {
	w.stopOnce.Do(func() {
		// под enqMu: Log, успевший пройти проверку, уже положил строку в канал (её допишет
		// drain по quit), а следующий увидит stopping — запись не пропадает молча между ними
		w.enqMu.Lock()
		w.stopping.Store(true)
		w.enqMu.Unlock()
		close(w.quit)
		if w.started.CompareAndSwap(false, true) {
			go w.run() // не запускался (CLI) — всё равно дописать накопленное
		}
	})
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Log — неблокирующая постановка записи в очередь (core.Auditor).
func (w *Writer) Log(ctx context.Context, e core.AuditEntry) {
	if e.Action == "" {
		w.log.Warn("пустой action — запись аудита пропущена", "entity_type", e.EntityType)
		return
	}
	if w.stopping.Load() {
		w.drop("stopped")
		return
	}
	r := buildRow(ctx, e, time.Time{}) // at — ниже, под enqMu
	r.before = w.encode(e.Before, e.Action, "before")
	r.after = w.encode(e.After, e.Action, "after")

	w.enqMu.Lock()
	if w.stopping.Load() {
		w.enqMu.Unlock()
		w.drop("stopped")
		return
	}
	at := w.now().UTC()
	if at.Before(w.lastAt) && w.lastAt.Sub(at) <= maxAtClamp {
		at = w.lastAt
	}
	r.at = at
	select {
	case w.ch <- r:
		w.lastAt = at
		w.enqMu.Unlock()
	default:
		w.enqMu.Unlock()
		w.drop("queue_full")
	}
}

// buildRow — актор и метаданные запроса (чистая функция от ctx и записи).
func buildRow(ctx context.Context, e core.AuditEntry, now time.Time) *row {
	r := &row{
		at:         now.UTC(),
		action:     cleanText(e.Action, maxText),
		entityType: cleanText(e.EntityType, maxText),
		entityID:   e.EntityID,
		lessonID:   e.LessonID,
	}
	meta, hasMeta := core.RequestMetaFrom(ctx)
	p := core.PrincipalFrom(ctx)
	switch {
	case e.ActorSystem:
		r.actorRole = RoleSystem
	case e.ActorID != nil && *e.ActorID != uuid.Nil:
		r.actorID = *e.ActorID
		r.actorRole = string(e.ActorRole)
		if r.actorRole == "" && p != nil && p.UserID == r.actorID {
			r.actorRole = string(p.Role)
		}
	case p != nil:
		r.actorID = p.UserID
		r.actorRole = string(p.Role)
	case !hasMeta:
		// вне HTTP-запроса — планировщик, очередь, CLI
		r.actorRole = RoleSystem
	default:
		// HTTP-запрос без сессии (например, неудачный вход с неизвестным логином):
		// актор неизвестен — NULL, но IP/User-Agent сохраняются ниже
		r.actorRole = string(e.ActorRole)
	}
	r.actorRole = cleanText(r.actorRole, maxText)
	if hasMeta {
		r.requestID = meta.RequestID
		if a, err := netip.ParseAddr(strings.TrimSpace(meta.IP)); err == nil {
			r.ip = a.WithZone("") // inet не хранит зону IPv6
		}
		r.userAgent = cleanText(meta.UserAgent, maxUA)
	}
	return r
}

// encode — before/after в JSON-объект с вычищенными секретами; nil — NULL.
func (w *Writer) encode(v any, action, field string) []byte {
	b, err := EncodeValue(v)
	if err != nil {
		w.log.Warn("before/after не сериализуется — сохранено NULL", "action", action, "field", field, "err", err)
		return nil
	}
	return b
}

func (w *Writer) drop(reason string) {
	mDropped.With(reason).Inc()
	total := w.dropped.Add(1)
	now := time.Now().UnixNano()
	last := w.lastWarn.Load()
	if now-last >= int64(warnEvery) && w.lastWarn.CompareAndSwap(last, now) {
		w.log.Warn("запись аудита отброшена", "reason", reason, "dropped_total", total)
	}
}

// ---------------------------------------------------------------- фоновая запись

func (w *Writer) run() {
	defer close(w.done)
	batch := make([]*row, 0, batchMax)
	// Таймер взводится первой строкой пустого батча: в простое писатель не просыпается вовсе.
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	armed := false
	for {
		select {
		case r := <-w.ch:
			batch = append(batch, r)
			batch = w.drainInto(batch)
			if len(batch) >= batchMax {
				w.flush(batch)
				batch = clearBatch(batch)
				if armed {
					timer.Stop()
					armed = false
				}
				continue
			}
			if !armed {
				timer.Reset(flushEvery)
				armed = true
			}
		case <-timer.C:
			armed = false
			if len(batch) > 0 {
				w.flush(batch)
				batch = clearBatch(batch)
			}
		case <-w.quit:
			timer.Stop()
			for {
				batch = w.drainInto(batch)
				if len(batch) == 0 {
					return
				}
				w.flush(batch)
				batch = clearBatch(batch)
			}
		}
	}
}

// drainInto забирает уже лежащие в канале строки без ожидания (до batchMax).
func (w *Writer) drainInto(batch []*row) []*row {
	for len(batch) < batchMax {
		select {
		case r := <-w.ch:
			batch = append(batch, r)
		default:
			return batch
		}
	}
	return batch
}

// clearBatch обнуляет ссылки (строки — мусор для GC) и сбрасывает длину.
func clearBatch(b []*row) []*row {
	clear(b)
	return b[:0]
}

// flush — COPY батча; при ошибке одна повторная попытка, затем батч отбрасывается с логом.
// COPY атомарен: неудачная попытка ничего не вставила, повтор не создаёт дублей.
func (w *Writer) flush(batch []*row) {
	start := time.Now()
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(retryPause)
		}
		ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
		var n int64
		n, err = w.pool.CopyFrom(ctx, auditTable, auditColumns, &rowSource{rows: batch, i: -1})
		cancel()
		if err == nil {
			mWritten.Add(uint64(n))
			mBatches.Observe(time.Since(start).Seconds())
			return
		}
		if attempt == 0 {
			w.log.Warn("запись батча аудита не удалась, повтор", "rows", len(batch), "err", err)
		}
	}
	mDropped.With("db_error").Add(uint64(len(batch)))
	w.dropped.Add(uint64(len(batch)))
	w.log.Error("батч аудита отброшен после повтора", "rows", len(batch),
		"first_action", batch[0].action, "first_at", batch[0].at, "err", err)
}

// rowSource — pgx.CopyFromSource поверх батча; буфер значений переиспользуется
// (pgx кодирует строку сразу после Values, до следующего Next).
type rowSource struct {
	rows []*row
	i    int
	vals [12]any
}

func (s *rowSource) Next() bool { s.i++; return s.i < len(s.rows) }
func (s *rowSource) Err() error { return nil }

func (s *rowSource) Values() ([]any, error) {
	r := s.rows[s.i]
	v := s.vals[:]
	v[0] = r.at
	v[1] = nullUUID(r.actorID)
	v[2] = nullStr(r.actorRole)
	v[3] = r.action
	v[4] = nullStr(r.entityType)
	v[5] = nullUUID(r.entityID)
	v[6] = nullUUID(r.lessonID)
	v[7] = nullJSON(r.before)
	v[8] = nullJSON(r.after)
	if r.ip.IsValid() {
		v[9] = r.ip
	} else {
		v[9] = nil
	}
	v[10] = nullStr(r.userAgent)
	v[11] = nullUUID(r.requestID)
	return v, nil
}

func nullUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullJSON(b []byte) any {
	if b == nil {
		return nil
	}
	return json.RawMessage(b)
}

// cleanText — валидный UTF-8 без NUL (PostgreSQL text их не принимает — иначе весь COPY-батч
// упадёт из-за одной строки) и не длиннее max байт по границе руны.
func cleanText(s string, max int) string {
	if s == "" {
		return s
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, string(utf8.RuneError))
	}
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	s = strings.TrimSpace(s)
	if len(s) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return s
}

// ---------------------------------------------------------------- before/after

// EncodeValue — значение before/after в JSON для audit_log: секреты вычищены (контракт
// приложения, db-design §6), не-объекты завёрнуты в {"value": …} (контракт AuditEntry —
// объект), \u0000 заменён, невалидный UTF-8 и одиночные суррогаты \uD800–\uDFFF
// нормализованы в U+FFFD (jsonb их не принимает — упал бы весь COPY-батч).
// nil/null — nil (NULL в БД).
func EncodeValue(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	var b []byte
	switch t := v.(type) {
	case json.RawMessage:
		b = bytes.Clone(t) // буфер вызывающего живёт дальше, а строка ждёт в очереди
	case []byte:
		b = bytes.Clone(t)
	default:
		var err error
		if b, err = marshal(v); err != nil {
			return nil, err
		}
	}
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil, nil
	}
	if _, raw := v.(json.RawMessage); raw || isBytes(v) {
		if !json.Valid(b) {
			// сырые байты не JSON — сохраняем как строку, а не теряем
			var err error
			if b, err = marshal(map[string]string{"value": string(b)}); err != nil {
				return nil, err
			}
		}
	}
	if mayContainSecrets(b) {
		var err error
		if b, err = redactJSON(b); err != nil {
			return nil, err
		}
	} else if !utf8.Valid(b) || hasSurrogateEscape(b) {
		// сырые байты вызывающего (json.RawMessage, []byte, вложенный RawMessage) json.Valid
		// не проверяет на UTF-8; декодер Go заменяет такое на U+FFFD, повторный Marshal чист
		var err error
		if b, err = normalizeJSON(b); err != nil {
			return nil, err
		}
	}
	if b[0] != '{' {
		out := make([]byte, 0, len(b)+10)
		out = append(out, `{"value":`...)
		out = append(out, b...)
		b = append(out, '}')
	}
	if len(b) > MaxValueBytes {
		// before/after — компактные структуры (DESIGN §3); гигантское значение раздуло бы журнал
		b = []byte(`{"truncated":true,"bytes":` + strconv.Itoa(len(b)) + `}`)
	}
	replaceNULEscapes(b)
	return b, nil
}

// replaceNULEscapes — escape \u0000 (jsonb его не принимает, COPY упал бы целым батчем)
// заменяется на escape U+FFFD той же длины — на месте. Учитываются только настоящие escape'ы:
// «\\u0000» (экранированный обратный слеш + текст) не трогается.
func replaceNULEscapes(b []byte) {
	for i := 0; i+1 < len(b); i++ {
		if b[i] != '\\' {
			continue
		}
		if b[i+1] == 'u' && i+5 < len(b) && string(b[i+2:i+6]) == "0000" {
			copy(b[i+2:i+6], "fffd")
			i += 5
			continue
		}
		i++ // пропустить экранированный символ (в т.ч. второй '\')
	}
}

// hasSurrogateEscape — в JSON есть escape суррогатной половины (\uD800–\uDFFF). Пара
// jsonb допустима, одиночная половина — нет; различать не стоит труда: при любом таком
// escape значение проходит нормализацию (пара превращается в тот же символ UTF-8).
func hasSurrogateEscape(b []byte) bool {
	for i := 0; i+5 < len(b); i++ {
		if b[i] != '\\' {
			continue
		}
		if b[i+1] == 'u' && (b[i+2] == 'd' || b[i+2] == 'D') {
			switch b[i+3] {
			case '8', '9', 'a', 'b', 'c', 'd', 'e', 'f', 'A', 'B', 'C', 'D', 'E', 'F':
				return true
			}
		}
		i++ // пропустить экранированный символ (в т.ч. второй '\')
	}
	return false
}

func isBytes(v any) bool { _, ok := v.([]byte); return ok }

func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Подстроки, по которым ключ секретен безусловно (после приведения к нижнему регистру и
// удаления разделителей: refresh_token_hash → refreshtokenhash).
var secretSubstrings = []string{
	"password", "passwd", "passphrase", "secret", "token", "cookie", "hash",
	"credential", "apikey", "privatekey", "authorization",
}

// Быстрый фильтр: если ни одного триггера нет во всём JSON, ключей-секретов там точно нет —
// обходим без разбора (подавляющее большинство записей).
var secretTriggers = []string{"pass", "pwd", "secret", "token", "cookie", "hash", "session",
	"credential", "apikey", "api_key", "privatekey", "private_key", "authorization"}

func mayContainSecrets(b []byte) bool {
	lower := bytes.ToLower(b)
	for _, t := range secretTriggers {
		if bytes.Contains(lower, []byte(t)) {
			return true
		}
	}
	return false
}

// SensitiveKey — ключ, значение которого нельзя писать в журнал.
// «pass», «pwd», «session» — омонимы (pass_threshold, passBonus, sessionId — не секреты):
// секретны, только если это последнее слово ключа (pass, newPass, user_session).
func SensitiveKey(k string) bool {
	norm := make([]byte, 0, len(k))
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c >= 'A' && c <= 'Z':
			norm = append(norm, c+('a'-'A'))
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
			norm = append(norm, c)
		}
	}
	for _, s := range secretSubstrings {
		if bytes.Contains(norm, []byte(s)) {
			return true
		}
	}
	switch lastWord(k) {
	case "pass", "pwd", "session":
		return true
	}
	return false
}

// lastWord — последнее слово ключа (snake_case, kebab-case, camelCase, CAPS) в нижнем регистре.
func lastWord(k string) string {
	isLower := func(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') }
	isUpper := func(c byte) bool { return c >= 'A' && c <= 'Z' }
	end := len(k)
	for end > 0 && !isLower(k[end-1]) && !isUpper(k[end-1]) {
		end--
	}
	start := end
	for start > 0 && isLower(k[start-1]) {
		start--
	}
	if start > 0 && isUpper(k[start-1]) {
		start--
		if start == end-1 { // хвост целиком из заглавных: PASS, USER_PASS
			for start > 0 && isUpper(k[start-1]) {
				start--
			}
		}
	}
	return strings.ToLower(k[start:end])
}

func redactJSON(b []byte) ([]byte, error) {
	v, err := decodeJSON(b)
	if err != nil {
		return nil, err
	}
	return marshal(redactValue(v))
}

// normalizeJSON — декодирование и повторная сериализация (UTF-8 и суррогаты — см. EncodeValue).
func normalizeJSON(b []byte) ([]byte, error) {
	v, err := decodeJSON(b)
	if err != nil {
		return nil, err
	}
	return marshal(v)
}

func decodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // числа без потери точности (int64 id)
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if SensitiveKey(k) {
				t[k] = Redacted
			} else {
				t[k] = redactValue(x)
			}
		}
	case []any:
		for i, x := range t {
			t[i] = redactValue(x)
		}
	}
	return v
}
