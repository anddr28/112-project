// Package convert — чистые преобразования форм без БД и сети:
//   - snake_case (model/components, ai-service и jsonb) <-> camelCase (public, фронт);
//   - проекции карточки: IncidentCardDraft (форма АРМ) <-> контрактный IncidentCard;
//   - результаты AI-слоёв -> public для страницы результата;
//   - нумерация реплик разговора, URL медиа, канонический tts_hash.
//
// Копирование «неглубокое»: строки/карты атрибутов могут разделяться между входом и
// выходом — вызывающий не мутирует результат через указатели входа (и наоборот).
// Обязательные массивы контракта в результатах — всегда не nil.
package convert

import (
	"strings"
	"time"
)

// Ptr — указатель на копию значения.
func Ptr[T any](v T) *T { return &v }

// Deref — значение или нулевое значение типа для nil.
func Deref[T any](p *T) T {
	if p == nil {
		var z T
		return z
	}
	return *p
}

// StrOr — *p или def для nil.
func StrOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

// NonEmpty — nil для пустой (после trim) строки, иначе указатель на копию как есть.
// Для опциональных полей контрактов: не шлём "" там, где «не задано».
func NonEmpty(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

// NonEmptyPtr — то же для указателя (nil и "" -> nil).
func NonEmptyPtr(p *string) *string {
	if p == nil || strings.TrimSpace(*p) == "" {
		return nil
	}
	return p
}

// Str — trim(*p) или "".
func Str(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// Slice — s или пустой срез (обязательные массивы в JSON — [], не null).
func Slice[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// SlicePtr — указатель на копию заголовка среза; nil для пустого (опциональные массивы).
func SlicePtr[T any](s []T) *[]T {
	if len(s) == 0 {
		return nil
	}
	return &s
}

// SliceFromPtr — *p или nil.
func SliceFromPtr[T any](p *[]T) []T {
	if p == nil {
		return nil
	}
	return *p
}

// NonNilSlicePtr — указатель на s или на пустой срез (опциональный массив, который
// фронту удобнее всегда видеть как []).
func NonNilSlicePtr[T any](s []T) *[]T {
	if s == nil {
		s = []T{}
	}
	return &s
}

// UTC — указатель на время в UTC (nil сохраняется).
func UTC(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// IntPtrIf — nil для 0 (опциональные счётчики, где 0 = «не задано»).
func IntPtrIf(v int) *int {
	if v == 0 {
		return nil
	}
	return &v
}

// copyStrings — независимая копия (nil сохраняется).
func copyStrings(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}
