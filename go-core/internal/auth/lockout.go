package auth

import (
	"crypto/sha256"
	"strings"
	"sync"
	"time"
)

// ghostLocks — lockout для НЕСУЩЕСТВУЮЩИХ логинов, зеркальный lockout'у в users
// (failed_login_count / locked_until). Без него серия неверных паролей к реальному логину
// заканчивается 429 «Слишком много неудачных попыток…», а к выдуманному — вечным 401,
// и по одному этому отличию снаружи видно, какие учётки существуют. С ним ответ на любой
// логин одинаков: MaxFailed раз 401, затем 429 на LockMinutes.
//
// Состояние в памяти (ключ — sha256 логина в нижнем регистре: users.login — citext, память
// на запись фиксирована). Записи без действующей блокировки, не тронутые ghostKeep, вычищаются
// попутно; при переполнении — все без действующей блокировки, затем — всё.
type ghostLocks struct {
	mu        sync.Mutex
	m         map[[32]byte]*ghost
	lastSweep time.Time
	maxKeys   int
}

type ghost struct {
	fails       int
	lockedUntil time.Time
	seen        time.Time
}

const (
	ghostKeep    = 24 * time.Hour
	ghostMaxKeys = 10000
)

func newGhostLocks(maxKeys int) *ghostLocks {
	return &ghostLocks{m: make(map[[32]byte]*ghost, 64), maxKeys: maxKeys}
}

func ghostKey(login string) [32]byte { return sha256.Sum256([]byte(strings.ToLower(login))) }

// lockedUntil — момент окончания блокировки логина, если она действует.
func (g *ghostLocks) lockedUntil(login string, now time.Time) (time.Time, bool) {
	k := ghostKey(login)
	g.mu.Lock()
	defer g.mu.Unlock()
	if e := g.m[k]; e != nil && e.lockedUntil.After(now) {
		return e.lockedUntil, true
	}
	return time.Time{}, false
}

// fail — неудачная попытка входа. Как loginFailSQL: счётчик +1; на пороге maxFailed —
// блокировка на lockFor и сброс счётчика. locked=true — блокировка поставлена этой попыткой.
func (g *ghostLocks) fail(login string, now time.Time, maxFailed int, lockFor time.Duration) (until time.Time, locked bool) {
	k := ghostKey(login)
	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Sub(g.lastSweep) > time.Minute || len(g.m) >= g.maxKeys {
		g.sweepLocked(now)
	}
	e := g.m[k]
	if e == nil {
		e = &ghost{}
		g.m[k] = e
	}
	e.seen = now
	if e.fails+1 >= max(1, maxFailed) {
		e.fails = 0
		e.lockedUntil = now.Add(lockFor)
		return e.lockedUntil, true
	}
	e.fails++
	return time.Time{}, false
}

func (g *ghostLocks) sweepLocked(now time.Time) {
	g.lastSweep = now
	for k, e := range g.m {
		if !e.lockedUntil.After(now) && now.Sub(e.seen) > ghostKeep {
			delete(g.m, k)
		}
	}
	if len(g.m) >= g.maxKeys {
		for k, e := range g.m {
			if !e.lockedUntil.After(now) {
				delete(g.m, k)
			}
		}
	}
	if len(g.m) >= g.maxKeys {
		clear(g.m)
	}
}
