// Package store — общие чтения и сборка public-представлений, которыми пользуются
// несколько доменов волны 2 (scenarios, lessons, attempts, dialogue, evaluation, reports,
// realtime-снимок): Scenario, Etalon, Lesson, Attempt, DialogueTurn.
//
// Правила:
//   - функции принимают pg.Querier (пул или транзакция) и делают РОВНО один запрос:
//     списки — одним SELECT с JOIN/подзапросами/json_agg, без N+1;
//   - SQL — константы (pgx кэширует prepared statements по тексту), колонки явно,
//     сканирование вручную;
//   - «не найдено» — pgx.ErrNoRows как есть (вызывающий проверяет pg.IsNoRows);
//   - jsonb декодируется прямо из памяти драйвера (pgtype.BytesScanner) — без
//     промежуточной копии []byte; копия делается только там, где нужен raw (passthrough);
//   - *ToPublic — чистые функции: все обязательные поля контракта заданы, обязательные
//     массивы — [] (не null), время — UTC.
package store

import (
	"bytes"
	"encoding/json"
	"fmt"

	"lct/gocore/internal/model"
)

// defaultLessonSettings — чем добиваются отсутствующие ключи lessons.settings при чтении.
// Дефолты миграций (settings.Defaults), а не живой снимок: go-core всегда пишет settings
// занятия целиком, добивка нужна только строкам, созданным в обход приложения.
var defaultLessonSettings = model.DefaultLessonSettings(nil)

// jsonScan — jsonb -> dst прямо из буфера драйвера. NULL оставляет dst нетронутым
// (вызывающий заранее кладёт туда значение по умолчанию).
type jsonScan struct {
	dst  any
	name string // колонка — для текста ошибки
}

func (s *jsonScan) ScanBytes(b []byte) error {
	if b == nil {
		return nil
	}
	if err := json.Unmarshal(b, s.dst); err != nil {
		return fmt.Errorf("store: jsonb %s: %w", s.name, err)
	}
	return nil
}

// rawScan — копия jsonb (для passthrough). NULL -> nil.
type rawScan struct{ dst *json.RawMessage }

func (s *rawScan) ScanBytes(b []byte) error {
	if b == nil {
		*s.dst = nil
		return nil
	}
	*s.dst = append(json.RawMessage(nil), b...)
	return nil
}

// settingsScan — lessons.settings с добивкой дефолтами (model.ParseLessonSettings).
// Кривое значение (правка руками в БД) не должно ронять чтение занятия и всех его
// попыток: берётся best-effort результат разбора — он всегда валиден.
type settingsScan struct{ dst *model.LessonSettings }

func (s *settingsScan) ScanBytes(b []byte) error {
	*s.dst, _ = model.ParseLessonSettings(b, defaultLessonSettings)
	return nil
}

// isEmptyJSON — jsonb «пусто»: NULL, null, {} или [] (дефолты колонок '{}' / '[]').
func isEmptyJSON(b []byte) bool {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return true
	}
	switch string(b) {
	case "null", "{}", "[]":
		return true
	}
	return false
}

// nullStr — "" -> NULL для необязательных фильтров.
func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
