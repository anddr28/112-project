package users

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/platform/pgtest"
)

// Постраничный обход GET /users даёт ровно полный список в порядке ФИО: однофамильцы и
// полные тёзки (порядок решает id) не теряются и не дублируются на границах страниц.
func TestDBListPagination(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	names := [][2]string{{"Иванов", "Иван"}, {"Иванов", "Иван"}, {"Иванов", "Пётр"}, {"Андреева", "Анна"},
		{"Яковлев", "Олег"}, {"Иванов", "Иван"}, {"Борисова", "Вера"}, {"Андреева", "Анна"}}
	for i, n := range names {
		if _, err := pool.Exec(ctx, `INSERT INTO users (login, password_hash, role, last_name, first_name)
			VALUES ($1, 'x', 'student', $2, $3)`, "pg_u"+string(rune('a'+i)), n[0], n[1]); err != nil {
			t.Fatal(err)
		}
	}
	full, err := List(ctx, pool, ListFilter{Role: "student"})
	if err != nil || len(full) != len(names) {
		t.Fatalf("full: %d %v", len(full), err)
	}
	for _, limit := range []int{1, 2, 3, 5, 8} {
		var got []uuid.UUID
		f := ListFilter{Role: "student"}
		for guard := 0; ; guard++ {
			if guard > 20 {
				t.Fatal("pagination did not terminate")
			}
			f.Limit = limit + 1
			page, err := List(ctx, pool, f)
			if err != nil {
				t.Fatal(err)
			}
			more := len(page) > limit
			if more {
				page = page[:limit]
			}
			for i := range page {
				got = append(got, page[i].Id)
			}
			if !more {
				break
			}
			f.After = KeyOf(&page[len(page)-1])
		}
		if len(got) != len(full) {
			t.Fatalf("limit %d: %d users, want %d", limit, len(got), len(full))
		}
		for i := range got {
			if got[i] != full[i].Id {
				t.Fatalf("limit %d: order/duplicates differ at %d", limit, i)
			}
		}
	}
	k := KeyOf(&full[3])
	if back, ok := ParseNameKey(k.Parts()); !ok || back != k {
		t.Fatalf("name key round trip: %+v %v", back, ok)
	}
	if _, ok := ParseNameKey([]string{"a", "b"}); ok {
		t.Fatal("short key accepted")
	}
}
