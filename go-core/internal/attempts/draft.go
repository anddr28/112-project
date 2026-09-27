package attempts

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"lct/gocore/internal/access"
	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/store"
)

// maxDraftBytes — потолок тела автосохранения. Карточка с десятком служб и историей статусов —
// единицы КБ (описание ≤ 1999 символов); 256 КБ — с запасом, но не даёт раздуть jsonb.
const maxDraftBytes = 256 << 10

// SQLSTATE, которыми PostgreSQL отвергает текст (\u0000 в jsonb, NUL в text, одиночный
// суррогат \ud800 в jsonb): это ошибка данных клиента (400), а не сервера.
const (
	sqlStateUntranslatable = "22P05"
	sqlStateInvalidText    = "22P02"
	sqlStateBadEncoding    = "22021"
)

// badTextError — 400 вместо 500, если PostgreSQL отверг символы в данных клиента.
func badTextError(err error) error {
	if pe, ok := pg.PgError(err); ok {
		switch pe.Code {
		case sqlStateUntranslatable, sqlStateInvalidText, sqlStateBadEncoding:
			return httpx.BadRequest("Карточка содержит недопустимые символы")
		}
	}
	return nil
}

type savedResponse struct {
	SavedAt time.Time `json:"savedAt"`
}

// ---------------------------------------------------------------- PUT /draft

// putDraft — автосохранение (каждые 1–2 с на студента): тело проверяется на форму
// IncidentCardDraft (checkDraft) и уходит в jsonb как есть (без повторного кодирования), в
// БД — один guarded upsert со слиянием служб (servicesMergeSQL). Кэш метаданных нужен только
// чтобы не ходить в БД с заведомо отвергнутым запросом (чужая/закрытая попытка — например,
// забытая вкладка после сдачи).
func (s *Service) putDraft(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "attemptId")
	if err != nil {
		return err
	}
	body, err := httpx.ReadRawJSON(r, maxDraftBytes)
	if err != nil {
		return err
	}
	body = stripNULEscapes(body)
	if err := checkDraft(body); err != nil {
		return err
	}
	ctx := r.Context()
	p := core.PrincipalFrom(ctx)
	if m, ok := s.meta.get(id, s.now()); ok {
		if err := access.OwnAttempt(p, m.accessRow()); err != nil {
			return err
		}
		if core.AttemptTerminal(m.Status) {
			return errDraftClosed()
		}
	}

	var savedAt time.Time
	if err := s.pool.QueryRow(ctx, sqlPutDraft, id, p.UserID, body).Scan(&savedAt); err != nil {
		if pg.IsNoRows(err) {
			return s.draftRejected(ctx, p, id)
		}
		if bad := badTextError(err); bad != nil {
			return bad
		}
		return fmt.Errorf("attempts: put draft: %w", err)
	}
	httpx.WriteJSON(w, http.StatusOK, savedResponse{SavedAt: savedAt.UTC()})
	return nil
}

func errDraftClosed() error { return errClosed("черновик не принимается") }

// draftRejected — почему guarded upsert ничего не записал: нет попытки (404), чужая (403),
// закрыта или вызов ещё не принят (409). Метаданные читаются мимо кэша и освежают его.
func (s *Service) draftRejected(ctx context.Context, p *core.Principal, id uuid.UUID) error {
	m, err := s.refreshMeta(ctx, id)
	if err != nil {
		return err
	}
	if err := access.OwnAttempt(p, m.accessRow()); err != nil {
		return err
	}
	if core.AttemptTerminal(m.Status) {
		return errDraftClosed()
	}
	if m.Status == core.AttemptIssued {
		return errNotAccepted()
	}
	// Статус успел смениться между upsert и диагностикой — клиент просто повторит автосохранение.
	return httpx.Conflict("Черновик не сохранён — повторите")
}

// ---------------------------------------------------------------- GET /draft

// getDraft — черновик как есть (jsonb без decode/encode) + проверка доступа, одним запросом.
// Нет строки или пустой объект — пустая карточка (фронт всегда получает полную форму).
func (s *Service) getDraft(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "attemptId")
	if err != nil {
		return err
	}
	ctx := r.Context()
	var (
		userID, ownerID uuid.UUID
		data            []byte
	)
	if err := s.pool.QueryRow(ctx, sqlGetDraft, id).Scan(&userID, &ownerID, &data); err != nil {
		if pg.IsNoRows(err) {
			return errNotFound()
		}
		return fmt.Errorf("attempts: get draft: %w", err)
	}
	if err := access.ViewAttempt(core.PrincipalFrom(ctx), &store.AttemptRow{UserID: userID, LessonTeacherID: &ownerID}); err != nil {
		return err
	}
	if !isDraftObject(data) {
		data = emptyDraftJSON
	}
	httpx.WriteRawJSON(w, http.StatusOK, data)
	return nil
}

// isDraftObject — непустой JSON-объект (jsonb '{}' — дефолт колонки — это «черновика нет»).
func isDraftObject(b []byte) bool {
	b = bytes.TrimSpace(b)
	return len(b) > 2 && b[0] == '{'
}

// ---------------------------------------------------------------- GET /call-script

// callScript — легенда для обучающегося (StudentCallScript): заявитель без эмоций/голоса,
// реплики с аудио из tts_cache. НИКОГДА — keyFacts, бриф заявителя, эталон (GAP-11).
// В голосовом режиме — только вступительная реплика: дальше разговор идёт через /dialogue.
// До приёма вызова (issued) реплик нет вовсе: экрану входящего вызова нужен только номер
// заявителя, а легенда до старта таймера позволила бы заполнить карточку «вне норматива».
func (s *Service) callScript(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathUUID(r, "attemptId")
	if err != nil {
		return err
	}
	ctx := r.Context()
	var (
		userID, ownerID uuid.UUID
		status          string
		ls              model.LessonSettings
		cs              model.CallScript
		hashes, paths   []string
		durations       []int32
		dds             bool
	)
	err = s.pool.QueryRow(ctx, sqlCallScript, id).Scan(&userID, &ownerID, &status, &settingsScan{dst: &ls},
		&callScriptScan{dst: &cs}, &hashes, &paths, &durations, &dds)
	if err != nil {
		if pg.IsNoRows(err) {
			return errNotFound()
		}
		return fmt.Errorf("attempts: call script: %w", err)
	}
	if err := access.ViewAttempt(core.PrincipalFrom(ctx), &store.AttemptRow{UserID: userID, LessonTeacherID: &ownerID}); err != nil {
		return err
	}
	cs.Normalize()
	files := ttsIndex{hashes: hashes, paths: paths, durations: durations}
	out := studentCallScript(&cs, ls, files)
	if status == core.AttemptIssued || dds {
		// Ракурс dds: диспетчер ДДС с заявителем не говорит — всё, что известно, уже в
		// карточке 112 (номер заявителя — в АОН).
		out.Turns = []public.CallTurnView{}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

// ttsIndex — найденные файлы озвучки (реплик единицы — линейный поиск быстрее карты).
type ttsIndex struct {
	hashes, paths []string
	durations     []int32
}

func (t ttsIndex) lookup(hash string) (path string, durationMs int, ok bool) {
	if hash == "" {
		return "", 0, false
	}
	for i, h := range t.hashes {
		if h == hash && i < len(t.paths) {
			if i < len(t.durations) {
				durationMs = int(t.durations[i])
			}
			return t.paths[i], durationMs, true
		}
	}
	return "", 0, false
}

// studentCallScript — проекция легенды для студента (чистая функция).
func studentCallScript(cs *model.CallScript, ls model.LessonSettings, files ttsIndex) public.StudentCallScript {
	out := public.StudentCallScript{
		AllowReplay: ls.AllowReplay,
		Voice:       convert.VoiceToPublic(ls.Voice),
	}
	out.Caller.Name = convert.NonEmpty(cs.Caller.Name)
	out.Caller.Phone = convert.NonEmpty(cs.Caller.Phone)
	out.Caller.Role = convert.NonEmpty(cs.Caller.Role)

	if ls.Voice.Enabled {
		out.Turns = make([]public.CallTurnView, 0, 1)
		if idx, t, ok := cs.OpeningTurn(); ok {
			out.Turns = append(out.Turns, callTurnView(idx, t, files))
		}
		return out
	}
	out.Turns = make([]public.CallTurnView, 0, len(cs.Turns))
	for i, t := range cs.Turns {
		if strings.TrimSpace(t.Text) == "" {
			continue // пустая реплика в UI — пустой пузырь; индекс сохраняет позицию в сценарии
		}
		out.Turns = append(out.Turns, callTurnView(i, t, files))
	}
	return out
}

// callTurnView — реплика с аудио (если озвучена) и длительностью: из tts_cache, иначе оценка
// по длине текста — как у мока фронта (max(2200, символы × 70) мс), по ней идёт автопоказ реплик.
func callTurnView(idx int, t model.Turn, files ttsIndex) public.CallTurnView {
	speaker := public.CallTurnViewSpeakerCaller
	if t.Speaker == model.SpeakerOperatorHint {
		speaker = public.CallTurnViewSpeakerOperatorHint
	}
	v := public.CallTurnView{Index: idx, Speaker: speaker, Text: t.Text}
	duration := 0
	if path, d, ok := files.lookup(t.TTSHash); ok {
		if u := convert.MediaURL(path); u != "" {
			v.AudioUrl = &u
		}
		duration = d
	}
	if duration <= 0 {
		duration = max(2200, utf8.RuneCountInString(t.Text)*70)
	}
	v.DurationMs = &duration
	return v
}
