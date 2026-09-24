package store

import (
	"strconv"
	"time"

	"github.com/google/uuid"
)

// Keyset-пагинация списков (см. httpx.Page): курсор — значения полей сортировки последней
// строки страницы. Первая страница — тот же SQL с «граничным» курсором (дальше любой
// реальной строки): один текст запроса, план всегда индексный.

// MaxListLimit — потолок строк списка, если вызывающий лимит не задал (страховка).
const MaxListLimit = 1000

var (
	// keysetTop — позже любого created_at (сортировка по убыванию начинается «отсюда»).
	keysetTop = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
	// uuidMax — больше любого id.
	uuidMax = uuid.UUID{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
)

func limitOr(n int) int {
	if n <= 0 || n > MaxListLimit {
		return MaxListLimit
	}
	return n
}

// TimeKey — курсор списков «новые сверху»: (created_at DESC, id DESC). Нулевой — первая страница.
type TimeKey struct {
	At time.Time
	ID uuid.UUID
}

// IsZero — первая страница.
func (k TimeKey) IsZero() bool { return k.ID == uuid.Nil && k.At.IsZero() }

// Parts — значения для httpx.EncodeCursor.
func (k TimeKey) Parts() []string {
	return []string{k.At.UTC().Format(time.RFC3339Nano), k.ID.String()}
}

// args — параметры SQL (граничные для первой страницы).
func (k TimeKey) args() (time.Time, uuid.UUID) {
	if k.IsZero() {
		return keysetTop, uuidMax
	}
	return k.At, k.ID
}

// ParseTimeKey — курсор из частей (false — не наш формат).
func ParseTimeKey(parts []string) (TimeKey, bool) {
	if len(parts) != 2 {
		return TimeKey{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return TimeKey{}, false
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return TimeKey{}, false
	}
	return TimeKey{At: at, ID: id}, true
}

// AssignedRank — порядок занятий на дашборде обучающегося: что можно делать сейчас — сверху.
// Та же шкала — в SQL (sqlAssignedRank).
func AssignedRank(status string) int {
	switch status {
	case "running":
		return 0
	case "scheduled":
		return 1
	case "draft":
		return 2
	case "finished":
		return 3
	}
	return 4
}

// AssignedKey — курсор списка занятий обучающегося: (ранг статуса ASC, created_at DESC, id DESC).
type AssignedKey struct {
	Rank int
	TimeKey
}

// Parts — значения для httpx.EncodeCursor.
func (k AssignedKey) Parts() []string {
	return append([]string{strconv.Itoa(k.Rank)}, k.TimeKey.Parts()...)
}

// ParseAssignedKey — курсор из частей (false — не наш формат).
func ParseAssignedKey(parts []string) (AssignedKey, bool) {
	if len(parts) != 3 {
		return AssignedKey{}, false
	}
	rank, err := strconv.Atoi(parts[0])
	if err != nil || rank < 0 || rank > 4 {
		return AssignedKey{}, false
	}
	tk, ok := ParseTimeKey(parts[1:])
	if !ok {
		return AssignedKey{}, false
	}
	return AssignedKey{Rank: rank, TimeKey: tk}, true
}

func (k AssignedKey) args() (int, time.Time, uuid.UUID) {
	if k.IsZero() {
		return -1, keysetTop, uuidMax
	}
	return k.Rank, k.At, k.ID
}
