package ids

import (
	"testing"

	"github.com/google/uuid"
)

func TestNewIsMonotonicV7(t *testing.T) {
	t.Parallel()
	prev := New()
	if prev.Version() != 7 || prev.Variant() != uuid.RFC4122 {
		t.Fatalf("не UUIDv7: %v (version %d)", prev, prev.Version())
	}
	seen := map[uuid.UUID]bool{prev: true}
	for i := 0; i < 5000; i++ {
		id := New()
		if seen[id] {
			t.Fatalf("повтор: %v", id)
		}
		seen[id] = true
		if id.String() <= prev.String() {
			t.Fatalf("не монотонно: %v после %v", id, prev)
		}
		prev = id
	}
}

func TestParsePtrOrNil(t *testing.T) {
	t.Parallel()
	id := New()
	if got, ok := Parse(id.String()); !ok || got != id {
		t.Fatal("Parse")
	}
	for _, bad := range []string{"", "не-uuid", "018f6b2a-0000-7000-8000", id.String() + "0"} {
		if got, ok := Parse(bad); ok || got != uuid.Nil {
			t.Errorf("Parse(%q) принят", bad)
		}
	}
	p := Ptr(id)
	id2 := id
	id2[0] ^= 0xff
	if *p != id || *p == id2 {
		t.Fatal("Ptr")
	}
	if OrNil(uuid.Nil) != nil {
		t.Fatal("OrNil(Nil)")
	}
	if got := OrNil(id); got == nil || *got != id {
		t.Fatal("OrNil(id)")
	}
}
