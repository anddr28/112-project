package admin

import (
	"errors"
	"net/http"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/ops"
	"lct/gocore/internal/platform/httpx"
)

// GET /admin/backups — журнал копий, новые сверху.
func (h *Handlers) listBackups(w http.ResponseWriter, r *http.Request) error {
	if h.ops == nil {
		return httpx.NotFound("Резервное копирование не настроено")
	}
	list, err := h.ops.ListBackups(r.Context())
	if err != nil {
		return err
	}
	if list == nil {
		list = []public.Backup{}
	}
	httpx.WriteJSON(w, http.StatusOK, list)
	return nil
}

// POST /admin/backups — запуск копирования сейчас: 202 со строкой running, pg_dump идёт
// в фоне (ops). Аудит backup.run пишет ops после коммита строки — здесь не дублируем.
func (h *Handlers) runBackup(w http.ResponseWriter, r *http.Request) error {
	if h.ops == nil {
		return httpx.NotFound("Резервное копирование не настроено")
	}
	by := core.PrincipalFrom(r.Context()).UserID
	b, err := h.ops.StartBackup(r.Context(), &by)
	if err != nil {
		if errors.Is(err, ops.ErrBackupRunning) {
			return httpx.Conflict("Резервное копирование уже выполняется — дождитесь его завершения")
		}
		return err
	}
	httpx.WriteJSON(w, http.StatusAccepted, b)
	return nil
}
