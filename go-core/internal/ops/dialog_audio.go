package ops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"time"

	"github.com/google/uuid"
)

// Озвучка реплик заявителя в голосовом режиме: ai-service пишет её в shared volume как
// dialog/<attempt_id>/<turn_no>.wav (docs/tasks/python-ai-service.md, PY-08: «transient,
// чистится go-core по завершении занятия»). Файл нужен только во время звонка — плеер
// панели разговора; транскрипт остаётся текстом в attempt_dialogue_turns, отчёты и оценка
// аудио не используют. Без чистки volume растёт без предела: ~200 КБ на реплику,
// ~0.4 ГБ на занятие из 200 человек по 12 реплик.
const (
	dialogAudioDir = "dialog"
	// dialogAudioGrace — попытка завершена, но занятие ещё не закрыто (преподаватель забыл
	// его завершить): звонок давно окончен, через сутки озвучка удаляется всё равно.
	dialogAudioGrace = 24 * time.Hour
	// dialogAudioMaxAge — страховка: попытка не менялась неделю (браузер закрыли посреди
	// звонка, занятие так и не завершили) — звонка точно нет, статус уже не важен.
	dialogAudioMaxAge = 7 * 24 * time.Hour
	// orphanDialogAudioAfter — каталог, для которого нет попытки в БД (попытка удалена,
	// volume от другой БД стенда): удаляется, если не менялся сутки.
	orphanDialogAudioAfter = 24 * time.Hour
	dialogAudioChunk       = 500 // попыток на один запрос к БД
)

// sqlDialogAudioDone — какие из попыток уже можно чистить: занятие завершено/отменено
// (звонков в нём больше не будет, статус попытки не важен); попытка в терминальном статусе
// дольше dialogAudioGrace; попытка не менялась дольше dialogAudioMaxAge. Попытки, которых
// нет в БД, в ответ не попадают. Поиск — по PK attempts.
const sqlDialogAudioDone = `
SELECT a.id,
       l.status IN ('finished', 'cancelled')
       OR (a.status IN ('submitted', 'evaluating', 'evaluated', 'expired', 'aborted')
           AND a.updated_at < now() - make_interval(secs => $2::int))
       OR a.updated_at < now() - make_interval(secs => $3::int)
  FROM attempts a
  JOIN lessons l ON l.id = a.lesson_id
 WHERE a.id = ANY ($1)`

// sqlDialogAudioForget — ссылки реплик на удаляемые файлы: фронт не должен получить
// audioUrl на файл, которого нет. Вступление (tts/<hash>.wav, common/…) — общий кэш, не
// трогается: только пути внутри dialog/<attempt_id>/. Поиск — по префиксу PK.
const sqlDialogAudioForget = `
UPDATE attempt_dialogue_turns
   SET audio_path = NULL
 WHERE attempt_id = ANY ($1)
   AND audio_path LIKE 'dialog/' || attempt_id::text || '/%'`

type dialogDir struct {
	id    uuid.UUID
	entry fs.DirEntry
}

// cleanupDialogAudio — удаляет dialog/<attempt_id>/ завершённых попыток (см. sqlDialogAudioDone)
// и давно брошенные каталоги без попытки. Порядок: сначала audio_path в БД, затем файлы — сбой на середине
// оставляет лишний файл (удалится завтра), но никогда не ссылку в пустоту. Файлы удаляются
// через os.Root: симлинк, подложенный в volume, не выведет удаление за пределы TTS_DIR.
// Возвращает число удалённых каталогов.
func (o *Ops) cleanupDialogAudio(ctx context.Context) (int, error) {
	if o.cfg == nil || o.cfg.TTSDir == "" {
		return 0, nil
	}
	root, err := os.OpenRoot(o.cfg.TTSDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("ops: dialog audio: %w", err)
	}
	defer root.Close()

	dirs, err := listDialogDirs(root)
	if err != nil {
		return 0, err
	}
	var (
		removed, failed int
		firstErr        error
	)
	for chunk := range slices.Chunk(dirs, dialogAudioChunk) {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		drop, err := o.dialogDirsToDrop(ctx, chunk)
		if err != nil {
			return removed, err
		}
		if len(drop) == 0 {
			continue
		}
		ids := make([]uuid.UUID, len(drop))
		for i, d := range drop {
			ids[i] = d.id
		}
		if _, err := o.pool.Exec(ctx, sqlDialogAudioForget, ids); err != nil {
			return removed, fmt.Errorf("ops: dialog audio: forget paths: %w", err)
		}
		for _, d := range drop {
			if err := root.RemoveAll(path.Join(dialogAudioDir, d.entry.Name())); err != nil {
				failed++
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			removed++
		}
	}
	if failed > 0 {
		return removed, fmt.Errorf("ops: dialog audio: не удалено каталогов: %d: %w", failed, firstErr)
	}
	return removed, nil
}

// listDialogDirs — подкаталоги dialog/ с именем-UUID в канонической записи (так их называет
// ai-service по attempt_id из запроса). Прочее (симлинки, файлы, чужие имена) не трогается.
func listDialogDirs(root *os.Root) ([]dialogDir, error) {
	d, err := root.Open(dialogAudioDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("ops: dialog audio: %w", err)
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("ops: dialog audio: read %s: %w", dialogAudioDir, err)
	}
	out := make([]dialogDir, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id, err := uuid.Parse(e.Name())
		if err != nil || id.String() != e.Name() {
			continue
		}
		out = append(out, dialogDir{id: id, entry: e})
	}
	return out, nil
}

// dialogDirsToDrop — каталоги из chunk, которые пора удалить.
func (o *Ops) dialogDirsToDrop(ctx context.Context, chunk []dialogDir) ([]dialogDir, error) {
	ids := make([]uuid.UUID, len(chunk))
	for i, d := range chunk {
		ids[i] = d.id
	}
	rows, err := o.pool.Query(ctx, sqlDialogAudioDone, ids, int(dialogAudioGrace/time.Second), int(dialogAudioMaxAge/time.Second))
	if err != nil {
		return nil, fmt.Errorf("ops: dialog audio: attempts: %w", err)
	}
	state := make(map[uuid.UUID]bool, len(chunk))
	for rows.Next() {
		var id uuid.UUID
		var done bool
		if err := rows.Scan(&id, &done); err != nil {
			rows.Close()
			return nil, fmt.Errorf("ops: dialog audio: attempts: %w", err)
		}
		state[id] = done
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ops: dialog audio: attempts: %w", err)
	}
	drop := make([]dialogDir, 0, len(chunk))
	for _, d := range chunk {
		done, known := state[d.id]
		switch {
		case known && done:
			drop = append(drop, d)
		case !known:
			if fi, err := d.entry.Info(); err == nil && time.Since(fi.ModTime()) > orphanDialogAudioAfter {
				drop = append(drop, d)
			}
		}
	}
	return drop, nil
}
