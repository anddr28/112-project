package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/pg"
)

// Лёгкие чтения для горячих путей попытки (call-script, accept-call, ход диалога):
// им не нужны эталон и подзапросы GetScenario — только легенда и файлы озвучки.

const sqlGetCallScript = `SELECT call_script FROM scenarios WHERE id = $1`

// GetCallScript — только scenarios.call_script (pgx.ErrNoRows, если сценария нет).
func GetCallScript(ctx context.Context, q pg.Querier, scenarioID uuid.UUID) (*model.CallScript, error) {
	cs := new(model.CallScript)
	if err := q.QueryRow(ctx, sqlGetCallScript, scenarioID).Scan(&jsonScan{dst: cs, name: "scenarios.call_script"}); err != nil {
		return nil, err
	}
	cs.Normalize()
	return cs, nil
}

// TTSFile — строка tts_cache (файл озвучки в volume, путь относительный).
type TTSFile struct {
	Hash       string
	Voice      string
	Rate       float64
	FilePath   string
	DurationMs *int
}

const sqlGetTTSFiles = `
SELECT text_hash, voice, rate, file_path, duration_ms
  FROM tts_cache
 WHERE text_hash = ANY($1::text[])`

// GetTTSFiles — файлы озвучки по хэшам одним запросом (по PK). Отсутствующих хэшей
// в карте нет. Пустой вход — пустая карта без запроса.
func GetTTSFiles(ctx context.Context, q pg.Querier, hashes []string) (map[string]TTSFile, error) {
	out := make(map[string]TTSFile, len(hashes))
	if len(hashes) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, sqlGetTTSFiles, hashes)
	if err != nil {
		return nil, fmt.Errorf("store: tts files: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var f TTSFile
		if err := rows.Scan(&f.Hash, &f.Voice, &f.Rate, &f.FilePath, &f.DurationMs); err != nil {
			return nil, fmt.Errorf("store: scan tts file: %w", err)
		}
		out[f.Hash] = f
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: tts files: %w", err)
	}
	return out, nil
}
