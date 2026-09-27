package auth

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
)

func validEntry(user uuid.UUID, loadedAt time.Time, expires time.Time) *entry {
	e := &entry{principal: core.Principal{UserID: user, Role: core.RoleStudent}, state: stateValid, loadedAt: loadedAt.UnixNano()}
	e.expiresAt.Store(expires.UnixNano())
	e.touchedAt.Store(loadedAt.UnixNano())
	return e
}

func key(s string) tokenKey { return tokenKey(sha256.Sum256([]byte(s))) }

func TestSessionCacheStoreSkipsAfterPurge(t *testing.T) {
	t.Parallel()
	c := newSessionCache()
	u := uuid.New()
	now := time.Now()

	seq := c.purgeSeq.Load()
	c.purgeUser(uuid.New()) // инвалидация, случившаяся «во время» загрузки из БД
	c.store(key("a"), validEntry(u, now, now.Add(time.Hour)), seq)
	if c.get(key("a")) != nil {
		t.Fatal("загрузка, начатая до инвалидации, попала в кэш")
	}
	c.store(key("a"), validEntry(u, now, now.Add(time.Hour)), c.purgeSeq.Load())
	if c.get(key("a")) == nil {
		t.Fatal("актуальная загрузка не попала в кэш")
	}
}

func TestSessionCachePurgeAndMarkDead(t *testing.T) {
	t.Parallel()
	c := newSessionCache()
	u1, u2 := uuid.New(), uuid.New()
	now := time.Now()
	c.put(key("u1-a"), validEntry(u1, now, now.Add(time.Hour)))
	c.put(key("u1-b"), validEntry(u1, now, now.Add(time.Hour)))
	c.put(key("u2"), validEntry(u2, now, now.Add(time.Hour)))
	if n := c.active(); n != 3 {
		t.Fatalf("active=%d, want 3", n)
	}

	c.markDead(key("u1-a"), u1)
	if e := c.get(key("u1-a")); e == nil || e.state != stateDead {
		t.Fatal("markDead не отметил сессию")
	}
	c.purgeUser(u1)
	if c.get(key("u1-b")) != nil {
		t.Fatal("purgeUser оставил живую сессию пользователя")
	}
	if e := c.get(key("u1-a")); e == nil || e.state != stateDead {
		t.Fatal("purgeUser стёр отметку «мертва» (logout снова стал бы валиден до чтения БД)")
	}
	if c.get(key("u2")) == nil {
		t.Fatal("purgeUser задел чужую сессию")
	}
	if n := c.active(); n != 1 {
		t.Fatalf("active=%d, want 1", n)
	}
}

func TestSessionCacheSweep(t *testing.T) {
	t.Parallel()
	c := newSessionCache()
	now := time.Now()
	u := uuid.New()
	c.put(key("old"), validEntry(u, now.Add(-3*time.Minute), now.Add(time.Hour)))
	c.put(key("expired"), validEntry(u, now, now.Add(-time.Second)))
	c.put(key("fresh"), validEntry(u, now, now.Add(time.Hour)))
	c.mu.Lock()
	c.sweepLocked(now.UnixNano())
	c.mu.Unlock()
	if c.get(key("old")) != nil || c.get(key("expired")) != nil {
		t.Fatal("устаревшие записи не вычищены")
	}
	if c.get(key("fresh")) == nil {
		t.Fatal("свежая запись вычищена")
	}
	if n := c.active(); n != 1 {
		t.Fatalf("active=%d", n)
	}
}

func TestSessionCacheBounded(t *testing.T) {
	t.Parallel()
	c := newSessionCache()
	now := time.Now()
	u := uuid.New()
	for i := range cacheMaxEntries + 100 {
		var k tokenKey
		k[0], k[1], k[2] = byte(i), byte(i>>8), byte(i>>16)
		c.put(k, validEntry(u, now, now.Add(time.Hour)))
	}
	if n := len(c.m); n > cacheMaxEntries {
		t.Fatalf("кэш вырос до %d > %d", n, cacheMaxEntries)
	}
}

func TestEntryFresh(t *testing.T) {
	t.Parallel()
	now := time.Now()
	e := validEntry(uuid.New(), now, now.Add(time.Hour))
	if !e.fresh(now.Add(cacheTTL - time.Millisecond).UnixNano()) {
		t.Fatal("запись несвежая раньше cacheTTL")
	}
	if e.fresh(now.Add(cacheTTL).UnixNano()) {
		t.Fatal("запись свежая после cacheTTL")
	}
}
