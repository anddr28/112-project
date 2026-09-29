package materials

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/platform/pg"
)

// chunkSize — сколько байт файла читается из БД за один запрос. 1 МБ: файл 20 МБ — 20
// коротких запросов, память на загрузку — один кусок, соединение пула не держится весь
// ответ (медленный клиент не занимает пул).
const chunkSize = 1 << 20

// sqlChunk — кусок bytea. При STORAGE EXTERNAL PostgreSQL читает только нужные TOAST-чанки,
// без распаковки всего значения.
const sqlChunk = `SELECT substring(file_data FROM $2::int FOR $3::int) FROM materials WHERE id = $1`

// errGone — материал удалили во время отдачи.
var errGone = errors.New("materials: файл удалён во время скачивания")

// dbReader — io.ReadSeeker поверх materials.file_data для http.ServeContent (Range,
// If-Range, HEAD). Размер известен заранее (size_bytes).
type dbReader struct {
	ctx  context.Context
	pool *pgxpool.Pool
	id   uuid.UUID
	size int64
	off  int64

	buf    []byte
	bufOff int64 // смещение buf[0] в файле
}

func (d *dbReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = d.off + offset
	case io.SeekEnd:
		abs = d.size + offset
	default:
		return 0, errors.New("materials: seek: неизвестный whence")
	}
	if abs < 0 {
		return 0, errors.New("materials: seek: отрицательное смещение")
	}
	d.off = abs
	return abs, nil
}

func (d *dbReader) Read(p []byte) (int, error) {
	if d.off >= d.size {
		return 0, io.EOF
	}
	if d.off < d.bufOff || d.off >= d.bufOff+int64(len(d.buf)) {
		if err := d.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(p, d.buf[d.off-d.bufOff:])
	d.off += int64(n)
	return n, nil
}

// fill — следующий кусок с текущего смещения (substring нумерует байты с 1).
func (d *dbReader) fill() error {
	n := min(int64(chunkSize), d.size-d.off)
	var chunk []byte
	if err := d.pool.QueryRow(d.ctx, sqlChunk, d.id, d.off+1, n).Scan(&chunk); err != nil {
		if pg.IsNoRows(err) {
			return errGone
		}
		return fmt.Errorf("materials: read chunk: %w", err)
	}
	if len(chunk) == 0 {
		return errGone // файл заменили более коротким или удалили
	}
	d.buf, d.bufOff = chunk, d.off
	return nil
}
