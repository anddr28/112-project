package auth

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
)

// Кэш сессий: ключ — sha256 токена (массив, без аллокации hex-строки на каждый запрос).
//
// Запись свежа cacheTTL (30 с): в этом окне запрос аутентифицируется без БД. Отзыв на этом
// инстансе (logout, блокировка, смена пароля/роли) — явная инвалидация, поэтому видна сразу;
// окно 30 с — только для правок «мимо» go-core (руками в БД, другой инстанс).
//
// Размер ограничен maxEntries; устаревшие записи вычищаются попутно при вставке (не чаще раза
// в минуту) — без фоновой горутины.
const (
	cacheTTL        = 30 * time.Second
	cacheSweepEvery = time.Minute
	cacheMaxEntries = 20000
	// после cacheKeep запись точно не нужна: свежесть давно истекла, её всё равно перечитают
	cacheKeep = 2 * time.Minute
)

type tokenKey [32]byte

// entryState — итог проверки сессии, закэшированный вместе с записью.
type entryState uint8

const (
	stateValid   entryState = iota
	stateDead               // отозвана / истекла / пользователь удалён — 401
	stateBlocked            // пользователь заблокирован — 423
)

type entry struct {
	principal core.Principal // неизменяем после публикации; наружу отдаётся копия
	state     entryState
	loadedAt  int64 // unix nano: момент загрузки из БД (неизменяем)

	expiresAt atomic.Int64 // unix nano: auth_sessions.expires_at (скользит)
	touchedAt atomic.Int64 // unix nano: последняя запись last_seen_at в БД
}

func (e *entry) fresh(now int64) bool { return now-e.loadedAt < int64(cacheTTL) }

type sessionCache struct {
	mu        sync.RWMutex
	m         map[tokenKey]*entry
	lastSweep int64
	// purgeSeq растёт при каждой инвалидации пользователя. Загрузчик запоминает его ДО
	// запроса в БД и не кладёт результат в кэш, если за время запроса была инвалидация:
	// иначе чтение, начатое до COMMIT блокировки, закэшировало бы «живую» сессию на 30 с.
	purgeSeq atomic.Uint64
}

func newSessionCache() *sessionCache {
	return &sessionCache{m: make(map[tokenKey]*entry, 256)}
}

func (c *sessionCache) get(k tokenKey) *entry {
	c.mu.RLock()
	e := c.m[k]
	c.mu.RUnlock()
	return e
}

// store кладёт запись, если с момента seq не было инвалидаций.
func (c *sessionCache) store(k tokenKey, e *entry, seq uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.purgeSeq.Load() != seq {
		return
	}
	c.putLocked(k, e)
}

// put кладёт запись безусловно (новая сессия при входе, отметка «мертва» при выходе).
func (c *sessionCache) put(k tokenKey, e *entry) {
	c.mu.Lock()
	c.putLocked(k, e)
	c.mu.Unlock()
}

func (c *sessionCache) putLocked(k tokenKey, e *entry) {
	now := time.Now().UnixNano()
	if now-c.lastSweep > int64(cacheSweepEvery) || len(c.m) >= cacheMaxEntries {
		c.sweepLocked(now)
	}
	c.m[k] = e
}

// sweepLocked удаляет давно устаревшие записи; при переполнении — ещё и случайную десятую
// часть (порядок обхода map случаен): вытесненные просто перечитаются из БД.
func (c *sessionCache) sweepLocked(now int64) {
	c.lastSweep = now
	for k, e := range c.m {
		if now-e.loadedAt > int64(cacheKeep) || (e.state == stateValid && now > e.expiresAt.Load()) {
			delete(c.m, k)
		}
	}
	if len(c.m) >= cacheMaxEntries {
		drop := len(c.m) / 10
		for k := range c.m {
			if drop <= 0 {
				break
			}
			delete(c.m, k)
			drop--
		}
	}
}

// markDead — сессия отозвана на этом инстансе (logout): параллельные запросы с тем же
// токеном сразу получают 401 без БД, а загрузка, начатая до отзыва, не перезапишет отметку.
func (c *sessionCache) markDead(k tokenKey, userID uuid.UUID) {
	e := &entry{principal: core.Principal{UserID: userID}, state: stateDead, loadedAt: time.Now().UnixNano()}
	c.mu.Lock()
	c.putLocked(k, e)
	c.purgeSeq.Add(1)
	c.mu.Unlock()
}

// purgeUser удаляет все записи пользователя. Полный обход — операция редкая (действие
// администратора), а вторичный индекс user→ключи стоил бы памяти и кода на каждом входе.
func (c *sessionCache) purgeUser(userID uuid.UUID) {
	c.mu.Lock()
	for k, e := range c.m {
		if e.principal.UserID == userID && e.state != stateDead {
			delete(c.m, k)
		}
	}
	c.purgeSeq.Add(1)
	c.mu.Unlock()
}

// active — число живых сессий, проверенных за последние cacheKeep (для /admin/health).
func (c *sessionCache) active() int {
	now := time.Now().UnixNano()
	n := 0
	c.mu.RLock()
	for _, e := range c.m {
		if e.state == stateValid && now-e.loadedAt <= int64(cacheKeep) && now < e.expiresAt.Load() {
			n++
		}
	}
	c.mu.RUnlock()
	return n
}
