package scenarios

import (
	"context"
	"encoding/json"
	"net/http"
	"path"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/callbacks"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/settings"
	"lct/gocore/internal/store"
)

// ttsPlan — реплики заявителя, которым нужна озвучка: хэш -> текст (без повторов,
// в порядке легенды), голос и темп, которыми считались хэши.
type ttsPlan struct {
	hashes []string
	texts  map[string]string
	voice  string
	rate   float64
}

// assignHashes — tts_hash каждой реплике заявителя = sha256(text|voice|rate) по текущим
// настройкам озвучки (голос заявителя важнее settings.tts.voice); у подсказок оператору
// хэша нет (они не звучат). changed — легенда изменилась и её нужно перезаписать.
func assignHashes(cs *model.CallScript, snap *settings.Snapshot) (plan ttsPlan, changed bool) {
	plan.voice = cs.VoiceOr(snap.TTS.Voice)
	plan.rate = snap.TTS.Rate
	plan.texts = make(map[string]string, len(cs.Turns))
	for i := range cs.Turns {
		t := &cs.Turns[i]
		if t.Speaker != model.SpeakerCaller || strings.TrimSpace(t.Text) == "" {
			if t.TTSHash != "" {
				t.TTSHash, changed = "", true
			}
			continue
		}
		h := convert.TTSHash(t.Text, plan.voice, plan.rate)
		if t.TTSHash != h {
			t.TTSHash, changed = h, true
		}
		if _, dup := plan.texts[h]; !dup {
			plan.texts[h] = t.Text
			plan.hashes = append(plan.hashes, h)
		}
	}
	return plan, changed
}

// enqueueMissing — TTS-задачи для хэшей, которых нет в tts_cache (одна выборка по PK).
// dedup "tts:{hash}": одинаковая реплика в разных сценариях озвучивается один раз,
// провалившаяся задача оживает при повторном подтверждении. Возвращает число новых задач.
func (s *Service) enqueueMissing(ctx context.Context, tx pgx.Tx, scenarioID uuid.UUID, plan ttsPlan, priority int) (int, error) {
	if s.queue == nil || len(plan.hashes) == 0 {
		return 0, nil
	}
	have, err := store.GetTTSFiles(ctx, tx, plan.hashes)
	if err != nil {
		return 0, err
	}
	created := 0
	rate := float32(plan.rate)
	for _, h := range plan.hashes {
		if _, ok := have[h]; ok {
			continue
		}
		jobID := ids.New()
		voice, prio := plan.voice, priority
		req := &aiservice.TtsJobRequest{
			SchemaVersion: components.N1,
			RequestId:     jobID,
			Priority:      &prio,
			Text:          plan.texts[h],
			TextHash:      h,
			Voice:         &voice,
			Rate:          &rate,
		}
		_, isNew, err := s.queue.Enqueue(ctx, tx, core.NewJob{
			ID:       jobID,
			Type:     core.JobTTS,
			Priority: priority,
			DedupKey: "tts:" + h,
			RefType:  core.RefScenario,
			RefID:    scenarioID,
			Payload:  req,
		})
		if err != nil {
			return created, err
		}
		if isNew {
			created++
		}
	}
	return created, nil
}

// ---------------------------------------------------------------- прослушивание

type ttsPreviewInput struct {
	Text  string  `json:"text"`
	Voice *string `json:"voice"`
}

// handleTTSPreview — POST /scenarios/{id}/tts-preview: реплика голосом заявителя.
// Кэш по хэшу проверяется до ai-service: повторное прослушивание (и уже озвученные
// при approve реплики) не тратят Silero. Ошибки ai-service — 503 ai_unavailable.
func (s *Service) handleTTSPreview(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "scenarioId")
	if err != nil {
		return errNotFound()
	}
	var in ttsPreviewInput
	if err := httpx.ReadJSON(r, &in); err != nil {
		return err
	}
	text := strings.TrimSpace(in.Text)
	switch n := runes(text); {
	case n == 0:
		return httpx.Validation("Введите текст реплики", map[string]string{"text": "Обязательное поле"})
	case n > previewTextMax:
		return httpx.Validation("Реплика слишком длинная", map[string]string{"text": "Не длиннее 500 символов"})
	}
	ctx := r.Context()

	var callerVoice string
	if err := s.pool.QueryRow(ctx, sqlCallerVoice, id).Scan(&callerVoice); err != nil {
		if pg.IsNoRows(err) {
			return errNotFound()
		}
		return err
	}
	snap := s.snap(ctx)
	voice := strings.TrimSpace(deref(in.Voice))
	if voice == "" {
		voice = strings.TrimSpace(callerVoice)
	}
	if voice == "" {
		voice = snap.TTS.Voice
	}
	rate := snap.TTS.Rate
	hash := convert.TTSHash(text, voice, rate)

	cached, err := store.GetTTSFiles(ctx, s.pool, []string{hash})
	if err != nil {
		return err
	}
	if f, ok := cached[hash]; ok {
		httpx.WriteJSON(w, http.StatusOK, audioRef(f.FilePath, deref(f.DurationMs)))
		return nil
	}

	if s.ai == nil {
		return httpx.AIUnavailable("Озвучка временно недоступна. Повторите позже.")
	}
	rate32 := float32(rate)
	res, err := s.ai.TTSSync(ctx, &aiservice.TtsSyncRequest{Text: text, TextHash: hash, Voice: &voice, Rate: &rate32})
	if err != nil {
		e := httpx.AIUnavailable("Озвучка временно недоступна. Повторите позже.").Wrap(err)
		if ae, ok := core.AsAIError(err); ok {
			e.RetryAfter = ae.RetryAfter
			if ae.Kind == core.AIBadRequest {
				s.log.Error("tts preview rejected by ai-service", "scenario", id, "err", err)
			}
		}
		return e
	}
	if !safeRelPath(res.FilePath) {
		s.log.Error("tts preview: unsafe file path from ai-service", "scenario", id)
		return httpx.AIUnavailable("Озвучка временно недоступна. Повторите позже.")
	}
	if _, err := s.pool.Exec(ctx, sqlUpsertTTS, hash, voice, rate, res.FilePath, res.DurationMs); err != nil {
		// Файл уже есть — отдаём его, кэш наполнится при следующем прослушивании/approve.
		s.log.Warn("tts preview: cache upsert failed", "err", err)
	}
	httpx.WriteJSON(w, http.StatusOK, audioRef(res.FilePath, res.DurationMs))
	return nil
}

func audioRef(filePath string, durationMs int) public.AudioRef {
	mime := convert.AudioMime(filePath)
	if mime == "" {
		mime = "audio/wav" // контракт: default audio/wav
	}
	return public.AudioRef{AudioUrl: convert.MediaURL(filePath), DurationMs: durationMs, Mime: &mime}
}

// safeRelPath — путь файла озвучки относительный и не выходит из volume (ai-service
// доверенный, но путь потом склеивается с каталогом раздачи).
func safeRelPath(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return false
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == ".." {
			return false
		}
	}
	return path.Clean(p) != ".."
}

// ---------------------------------------------------------------- результат TTS-задачи

// ttsHandler — core.AIResultHandler для задач tts: строка tts_cache (после неё
// сценарий становится ttsReady, а вступление звонка — со звуком).
type ttsHandler struct{ s *Service }

func (h ttsHandler) ApplyResult(ctx context.Context, tx pgx.Tx, job core.JobRecord, res *callbacks.AiResult) error {
	s := h.s
	t := res.Tts
	if t == nil {
		s.log.Warn("tts result without payload", "job", job.ID)
		return nil
	}
	// Ключ кэша — хэш, который посчитал go-core (на него ссылаются реплики легенды).
	var req aiservice.TtsJobRequest
	_ = json.Unmarshal(job.Payload, &req)
	hash := strings.TrimSpace(req.TextHash)
	if hash == "" {
		hash = strings.TrimSpace(t.TextHash)
	} else if t.TextHash != "" && t.TextHash != hash {
		s.log.Warn("tts result hash differs from job; job hash kept", "job", job.ID)
	}
	if hash == "" || !safeRelPath(t.FilePath) {
		s.log.Warn("tts result rejected: empty hash or unsafe path", "job", job.ID)
		return nil
	}
	snap := s.snap(ctx)
	voice := firstNonEmpty(deref(req.Voice), deref(t.Voice), snap.TTS.Voice)
	rate := snap.TTS.Rate
	switch {
	case req.Rate != nil:
		rate = float64(*req.Rate)
	case t.Rate != nil:
		rate = float64(*t.Rate)
	}
	var dur *int
	if t.DurationMs > 0 {
		d := t.DurationMs
		dur = &d
	}
	if _, err := tx.Exec(ctx, sqlUpsertTTS, hash, voice, rate, t.FilePath, dur); err != nil {
		return err
	}
	return nil
}

// ApplyFailure — озвучка не удалась: реплика останется без звука (UI показывает текст,
// ttsReady=false); повторное подтверждение сценария оживит задачу.
func (h ttsHandler) ApplyFailure(_ context.Context, _ pgx.Tx, job core.JobRecord, code, message string) error {
	h.s.log.Warn("tts job failed", "job", job.ID, "ref", job.RefID, "code", code, "msg", message)
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
