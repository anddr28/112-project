package attempts

import (
	"sync"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
	"lct/gocore/internal/store"
)

// Кэш метаданных попытки для горячих путей (PUT черновика, POST событий, GET событий).
//
// Что кэшируем и почему это безопасно:
//   - user_id, lesson_id, владелец занятия — не меняются никогда;
//   - status — меняют и другие пакеты (lessons: finish → expired; evaluation: → evaluated),
//     поэтому кэшу доверяем только в одну сторону: терминальный статус «липкий» (из него
//     попытка не возвращается), и отказ по нему верен всегда. Нетерминальный статус из кэша
//     решений не принимает — его перепроверяет сам запрос (guarded upsert / FOR UPDATE);
//   - firstInput — монотонен (NULL → время), true из кэша всегда верно.
//
// Свои смены статуса (accept-call, submit) пакет обновляет в кэше после COMMIT; чужие
// доезжают по TTL. Размер ограничен: при переполнении сначала выбрасываются протухшие,
// затем произвольная восьмая часть (порядок обхода map случаен) — O(n) раз на n/8 вставок.
const (
	metaTTL         = 30 * time.Second
	metaTTLTerminal = 5 * time.Minute // терминальный статус не меняется — держим дольше
	metaMaxEntries  = 8192            // ~1–2 МБ: с запасом на 200 одновременных студентов

	// liveEvery — не чаще одного «живого» события ввода (field_changed/choose_value) на
	// попытку в мониторинг за этот интервал: преподавателю нужен факт «печатает сейчас»,
	// а полная хронология — в GET /events. 200 студентов × 2/с — предел нагрузки на WS.
	liveEvery = 500 * time.Millisecond
)

// attemptMeta — то, что нужно горячим путям для проверок доступа и публикации.
type attemptMeta struct {
	UserID     uuid.UUID
	LessonID   uuid.UUID
	OwnerID    uuid.UUID // преподаватель-владелец занятия (teacher_id, у практики — автор)
	Status     string
	FirstInput bool // attempts.first_input_at уже проставлен
}

// accessRow — минимальная AttemptRow для правил internal/access (им нужны только
// владелец попытки и владелец занятия).
func (m *attemptMeta) accessRow() *store.AttemptRow {
	owner := m.OwnerID
	return &store.AttemptRow{UserID: m.UserID, LessonID: m.LessonID, LessonTeacherID: &owner}
}

type metaEntry struct {
	attemptMeta
	expires time.Time
	livePub time.Time // последняя публикация «живого» события ввода в мониторинг
}

type metaCache struct {
	mu  sync.Mutex
	m   map[uuid.UUID]*metaEntry
	max int
}

func newMetaCache(max int) *metaCache {
	return &metaCache{m: make(map[uuid.UUID]*metaEntry, 256), max: max}
}

func ttlFor(status string) time.Duration {
	if core.AttemptTerminal(status) {
		return metaTTLTerminal
	}
	return metaTTL
}

// get — свежая запись или ok=false.
func (c *metaCache) get(id uuid.UUID, now time.Time) (attemptMeta, bool) {
	c.mu.Lock()
	e, ok := c.m[id]
	if !ok || now.After(e.expires) {
		c.mu.Unlock()
		return attemptMeta{}, false
	}
	m := e.attemptMeta
	c.mu.Unlock()
	return m, true
}

// put — запись из БД (заменяет прежнюю, сохраняя отметку троттлинга мониторинга).
func (c *metaCache) put(id uuid.UUID, m attemptMeta, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[id]; ok {
		e.attemptMeta = m
		e.expires = now.Add(ttlFor(m.Status))
		return
	}
	if len(c.m) >= c.max {
		c.evictLocked(now)
	}
	c.m[id] = &metaEntry{attemptMeta: m, expires: now.Add(ttlFor(m.Status))}
}

// setStatus — своя смена статуса после COMMIT (записи нет — ничего не делаем: следующий
// запрос прочитает свежую из БД).
func (c *metaCache) setStatus(id uuid.UUID, status string, now time.Time) {
	c.mu.Lock()
	if e, ok := c.m[id]; ok {
		e.Status = status
		e.expires = now.Add(ttlFor(status))
	}
	c.mu.Unlock()
}

// setFirstInput — attempts.first_input_at проставлен (монотонно).
func (c *metaCache) setFirstInput(id uuid.UUID) {
	c.mu.Lock()
	if e, ok := c.m[id]; ok {
		e.FirstInput = true
	}
	c.mu.Unlock()
}

// allowLive — можно ли сейчас опубликовать «живое» событие ввода этой попытки (и отметить).
func (c *metaCache) allowLive(id uuid.UUID, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[id]
	if !ok {
		return true
	}
	if now.Sub(e.livePub) < liveEvery {
		return false
	}
	e.livePub = now
	return true
}

func (c *metaCache) evictLocked(now time.Time) {
	for id, e := range c.m {
		if now.After(e.expires) {
			delete(c.m, id)
		}
	}
	if len(c.m) < c.max {
		return
	}
	drop := c.max / 8
	for id := range c.m {
		if drop <= 0 {
			break
		}
		delete(c.m, id)
		drop--
	}
}
