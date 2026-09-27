package auth

import (
	"fmt"
	"testing"
	"time"
)

func TestGhostLocksMirrorDBLockout(t *testing.T) {
	t.Parallel()
	g := newGhostLocks(100)
	const login = "no_such_user"
	for i := 1; i <= 4; i++ {
		if _, locked := g.fail(login, t0, 5, 15*time.Minute); locked {
			t.Fatalf("блокировка после %d неудач, want после 5", i)
		}
		if _, locked := g.lockedUntil(login, t0); locked {
			t.Fatalf("lockedUntil после %d неудач", i)
		}
	}
	until, locked := g.fail(login, t0, 5, 15*time.Minute)
	if !locked || !until.Equal(t0.Add(15*time.Minute)) {
		t.Fatalf("5-я неудача: locked=%v until=%v", locked, until)
	}
	// citext: регистр логина не важен
	for _, l := range []string{login, "NO_SUCH_USER", "No_Such_User"} {
		if u, ok := g.lockedUntil(l, t0.Add(time.Minute)); !ok || !u.Equal(until) {
			t.Fatalf("%q: не заблокирован", l)
		}
	}
	if _, ok := g.lockedUntil(login, until); ok {
		t.Fatal("блокировка не истекла в срок")
	}
	// счётчик после блокировки сброшен: снова 5 неудач до следующей
	for i := 1; i <= 4; i++ {
		if _, locked := g.fail(login, until.Add(time.Second), 5, time.Minute); locked {
			t.Fatalf("повторная блокировка после %d неудач", i)
		}
	}
	if _, locked := g.fail(login, until.Add(time.Second), 5, time.Minute); !locked {
		t.Fatal("повторная блокировка не поставлена")
	}
}

func TestGhostLocksEdgeCases(t *testing.T) {
	t.Parallel()
	g := newGhostLocks(100)
	// maxFailed <= 0 — как max(1, …) в хендлере: блокировка с первой неудачи
	if _, locked := g.fail("кириллица", t0, 0, time.Minute); !locked {
		t.Fatal("maxFailed=0: нет блокировки")
	}
	if _, ok := g.lockedUntil("КИРИЛЛИЦА", t0); !ok {
		t.Fatal("кириллический логин в другом регистре не найден")
	}
	if _, ok := g.lockedUntil("", t0); ok {
		t.Fatal("пустой логин заблокирован")
	}
}

func TestGhostLocksBounded(t *testing.T) {
	t.Parallel()
	g := newGhostLocks(10)
	for i := range 50 {
		g.fail(fmt.Sprintf("u%d", i), t0, 5, time.Minute)
	}
	if len(g.m) > 10 {
		t.Fatalf("карта выросла до %d > 10", len(g.m))
	}
	// действующие блокировки переживают чистку при переполнении, пока их меньше предела
	g2 := newGhostLocks(3)
	g2.fail("locked", t0, 1, time.Hour)
	g2.fail("a", t0, 5, time.Minute)
	g2.fail("b", t0, 5, time.Minute)
	g2.fail("c", t0, 5, time.Minute)
	if _, ok := g2.lockedUntil("locked", t0); !ok {
		t.Fatal("действующая блокировка вычищена")
	}
	// старые записи без блокировки уходят при плановой чистке
	g3 := newGhostLocks(100)
	g3.fail("old", t0, 5, time.Minute)
	g3.fail("new", t0.Add(ghostKeep+2*time.Minute), 5, time.Minute)
	if len(g3.m) != 1 {
		t.Fatalf("устаревшая запись не вычищена: %d", len(g3.m))
	}
}
