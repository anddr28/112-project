package dialogue

import (
	"net/http"
	"os"
	"strings"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/platform/httpx"
)

const maxMediaPath = 512

// media — GET /media/tts/{path...}: файл озвучки из общего volume tts_cache.
// Путь проверяется строго (без "..", абсолютных путей, обратных слэшей, NUL), файл
// открывается через os.Root — выйти за пределы TTS_DIR нельзя ни путём, ни симлинком.
// Отдаются только аудио-расширения. Контент адресуется хэшем/номером хода и не меняется —
// кэш браузера на сутки (SecurityHeaders ставит no-store по умолчанию, здесь перекрываем).
func (s *Service) media(w http.ResponseWriter, r *http.Request) error {
	notFound := httpx.NotFound("Файл озвучки не найден")
	rel, ok := cleanMediaPath(r.PathValue("path"))
	if !ok || s.cfg == nil || s.cfg.TTSDir == "" {
		return notFound
	}
	ctype := convert.AudioMime(rel)
	if ctype == "" {
		return notFound
	}
	f, err := os.OpenInRoot(s.cfg.TTSDir, rel)
	if err != nil {
		return notFound
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return notFound
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Cache-Control", "private, max-age=86400")
	h.Set("X-Content-Type-Options", "nosniff")
	// Range/If-Modified-Since/HEAD — стандартно (перемотка в <audio>).
	http.ServeContent(w, r, "", st.ModTime(), f)
	return nil
}

// cleanMediaPath — относительный путь внутри tts_cache ("dialog/<attempt>/3.wav").
// Отвергает пустой, абсолютный, с "..", "." или пустыми сегментами, обратным слэшем, NUL.
// Тот же фильтр применяется к путям, которые присылает ai-service, до записи в БД.
func cleanMediaPath(p string) (string, bool) {
	if p == "" || len(p) > maxMediaPath || p[0] == '/' || strings.ContainsAny(p, "\\\x00") {
		return "", false
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	return p, true
}
