package httpx

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Ошибка данных PostgreSQL (NUL в тексте, 22P05) — 400 validation, а не 500.
func TestAsErrorPgDataException(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"22P05", "22021", "22P02", "22001"} {
		e := AsError(fmt.Errorf("insert: %w", &pgconn.PgError{Code: code}))
		if e.Status != http.StatusBadRequest || e.Code != CodeValidation {
			t.Fatalf("%s: got %d %s", code, e.Status, e.Code)
		}
	}
	if e := AsError(fmt.Errorf("x: %w", &pgconn.PgError{Code: "23505"})); e.Status != http.StatusInternalServerError {
		t.Fatalf("23505 must stay internal unless mapped by the handler, got %d", e.Status)
	}
}
