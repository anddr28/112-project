// Package ids — генерация идентификаторов. UUIDv7 (docs/db-design.md, Р1): время в старших
// битах, вставки в btree монотонны, id известен до INSERT (батчинг, идемпотентность).
package ids

import (
	"github.com/google/uuid"
)

// New возвращает UUIDv7. uuid.NewV7 падает только при отказе crypto/rand — это фатально.
func New() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		panic("ids: crypto/rand failed: " + err.Error())
	}
	return id
}

// Parse — строгий разбор UUID из пути/тела запроса.
func Parse(s string) (uuid.UUID, bool) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

// Ptr — указатель на копию (для опциональных полей сгенерированных типов).
func Ptr(id uuid.UUID) *uuid.UUID { return &id }

// OrNil — nil для uuid.Nil (для nullable колонок).
func OrNil(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}
