package dialogue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/access"
	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/aiservice"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/settings"
	"lct/gocore/internal/store"
)

const (
	maxReplyWords = 40
	maxSTTHints   = 30
	maxHintRunes  = 64
	// Заявитель переспрашивает, когда речь не распознана (ai-service обычно присылает свою
	// озвученную фразу — это запасной текст).
	reaskText = "Алло? Вы меня слышите?"
	pkTurns   = "attempt_dialogue_turns_pkey"
)

// errDuplicateTurn — ход уже записан (гонка/повтор): отвечаем 409 с сохранённым ответом.
var errDuplicateTurn = errors.New("dialogue: turn already stored")

// ---------------------------------------------------------------- POST /dialogue/turns

// postTurn — синхронный ход разговора. Порядок: разбор входа (аудио остаётся в теле) ->
// контекст попытки и транскрипт одним батчем -> права и состояние -> «один ход в полёте» ->
// ai-service (аудио потоком) -> транзакция записи -> WS после COMMIT -> ответ.
func (s *Service) postTurn(w http.ResponseWriter, r *http.Request) error {
	start := time.Now()
	attemptID, err := httpx.PathUUID(r, "attemptId")
	if err != nil {
		return err
	}
	snap := s.snapshot(r.Context())
	// Аудио читается из тела во время хода: мёртвый клиент (обрыв без FIN) не должен держать
	// «ход в полёте» минутами. Дедлайн с запасом больше всего хода; не поддерживается — ок.
	_ = http.NewResponseController(w).SetReadDeadline(start.Add(s.turnTimeout(snap) + 15*time.Second))

	in, err := readTurnInput(w, r)
	if err != nil {
		return err
	}
	defer in.close()

	a, rows, err := load(r.Context(), s.pool, attemptID, true)
	if err != nil {
		return err
	}
	if err := access.OwnAttempt(core.PrincipalFrom(r.Context()), a.accessRow()); err != nil {
		return err
	}
	tr := summarize(rows)
	if err := precheck(a, &tr, in); err != nil {
		return err
	}

	resp, err := s.runFlight(attemptID, in.turnNo, func() (*public.DialogueTurnResponse, error) {
		// Ход не зависит от соединения лидера: студент оборвал запрос по таймауту фронта —
		// ход всё равно доедет и запишется, повтор получит сохранённый ответ (409/дубль).
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.turnTimeout(snap))
		defer cancel()
		return s.processTurn(ctx, start, snap, a, &tr, in)
	})
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
	return nil
}

// precheck — состояние попытки и нумерация до обращения к ai-service. Повтор уже
// обработанного хода проверяется первым: он несёт сохранённый ответ (даже если разговор
// на этом ходе и закончился).
func precheck(a *attemptCtx, tr *transcript, in *turnInput) error {
	next := tr.next()
	if in.turnNo < next {
		return duplicateConflict(a, tr, in.turnNo)
	}
	switch a.Status {
	case core.AttemptInProgress:
	case core.AttemptIssued:
		return httpx.Conflict("Сначала примите вызов")
	default:
		return httpx.Conflict("Попытка уже завершена — разговор недоступен")
	}
	if a.CallAcceptedAt == nil {
		return httpx.Conflict("Сначала примите вызов")
	}
	if !a.Settings.Voice.Enabled {
		return httpx.Conflict("В этом занятии нет разговора с заявителем")
	}
	if a.CallEndedAt != nil {
		details := map[string]any{"nextTurnNo": next}
		if a.CallEndReason != nil {
			details["endReason"] = *a.CallEndReason
		}
		return httpx.Conflict("Разговор уже завершён").WithDetails(details)
	}
	if in.turnNo > next {
		return httpx.Conflict("Номер реплики не совпадает с ходом разговора").WithDetails(map[string]any{"nextTurnNo": next})
	}
	if tr.operatorTurns >= a.maxTurns() {
		return httpx.Conflict("Лимит реплик в разговоре исчерпан").WithDetails(map[string]any{"nextTurnNo": next})
	}
	if in.isAudio() && a.Settings.Voice.Input == model.VoiceInputText {
		return httpx.Validation("В этом занятии реплики вводятся текстом", map[string]string{"audio": "не принимается"})
	}
	return nil
}

// operatorPart / callerPart — нормализованный ход (из ai-service или локальный запасной).
type operatorPart struct {
	text       string
	source     string // stt | text
	confidence *float32
	durationMs *int
	textRaw    string
	noSpeech   bool
}

type callerPart struct {
	text       string
	source     string // llm | script
	fallback   bool
	audioPath  *string
	durationMs *int
	emotional  *string
	revealed   []string
	shouldEnd  bool
	endReason  string // CallerReply.end_reason как прислал ai-service (свободный текст или "max_turns")
	unknown    bool
	engine     *components.Engine
}

// processTurn — ход под защитой flight-guard (ctx отвязан от соединения лидера).
func (s *Service) processTurn(ctx context.Context, start time.Time, snap *settings.Snapshot, a *attemptCtx, tr *transcript, in *turnInput) (*public.DialogueTurnResponse, error) {
	req := buildRequest(a, tr, in, snap)
	var audio *core.AudioInput
	if in.audio != nil {
		audio = &core.AudioInput{Reader: in.audio.src, Filename: in.audio.filename, ContentType: in.audio.contentType}
	}
	res, aiErr := s.ai.DialogTurn(ctx, req, audio)
	var srcErr error
	if in.audio != nil {
		// С этого момента горутина AIClient тело не читает — можно дочитать форму.
		srcErr = in.audio.src.stop()
	}

	var (
		op     operatorPart
		caller callerPart
	)
	if aiErr != nil {
		if err := s.aiFailure(aiErr, srcErr, in, a); err != nil {
			return nil, err
		}
		// ai-service недоступен, реплика текстовая — заявитель отвечает по сценарию.
		op = operatorPart{text: in.text, source: core.TurnSourceText}
		caller = s.localReply(ctx, a, in.turnNo, snap)
	} else {
		op, caller = s.fromAI(ctx, res, a, in, snap)
	}
	in.readTrailing()

	if op.noSpeech {
		return s.noSpeechResponse(start, a, in.turnNo, caller), nil
	}
	return s.persistTurn(ctx, start, tr.answeredAt(in.turnNo, a.CallAcceptedAt), a, in, op, caller)
}

// aiFailure — ошибка ai-service -> ответ фронту; nil — отвечаем запасной репликой
// (только текстовый ход при недоступном ai-service: демо должно жить без AI).
func (s *Service) aiFailure(err error, srcErr error, in *turnInput, a *attemptCtx) error {
	if srcErr != nil && !errors.Is(srcErr, errAudioStopped) {
		// Оборвалась загрузка аудио у студента — не вина ai-service.
		var mbe *http.MaxBytesError
		if errors.As(srcErr, &mbe) {
			return httpx.AudioTooLong()
		}
		return &leaderOnlyError{err: httpx.BadRequest("Запись реплики не дошла до сервера целиком — повторите")}
	}
	ae, ok := core.AsAIError(err)
	if !ok {
		return httpx.Internal(fmt.Errorf("dialogue: dialog turn: %w", err))
	}
	switch ae.Kind {
	case core.AIBusy:
		return httpx.CallerBusy(ae.RetryAfter)
	case core.AIUnavailable:
		if !in.isAudio() {
			s.log.Warn("ai-service unavailable: local caller reply", "attempt", a.ID, "turn", in.turnNo, "err", ae.Error())
			return nil
		}
		s.log.Warn("ai-service unavailable for audio turn", "attempt", a.ID, "turn", in.turnNo, "err", ae.Error())
		return httpx.CallerBusy(5)
	case core.AIAudioTooLong:
		return httpx.AudioTooLong()
	case core.AIAudioUnsupported:
		return httpx.AudioUnsupported()
	}
	// AIBadRequest — ошибка go-core в payload (aijobs уже залогировал тело ответа).
	return httpx.Internal(fmt.Errorf("dialogue: ai-service rejected turn: %w", err))
}

// fromAI — результат ai-service -> части хода. Пустой ответ заявителя (нарушение
// контракта) заменяется запасной репликой, чтобы не терять распознанную речь оператора.
func (s *Service) fromAI(ctx context.Context, res *aiservice.DialogTurnResult, a *attemptCtx, in *turnInput, snap *settings.Snapshot) (operatorPart, callerPart) {
	var op operatorPart
	if in.isAudio() {
		op.text = cleanText(res.Operator.Text)
		op.source = core.TurnSourceSTT
		op.confidence = clamp01(res.Operator.Confidence)
		if d := res.Operator.AudioDurationMs; d > 0 {
			op.durationMs = &d
		}
		if raw := cleanText(convert.Deref(res.Operator.TextRaw)); raw != op.text {
			op.textRaw = raw
		}
		op.noSpeech = convert.Deref(res.Operator.NoSpeech) || op.text == ""
	} else {
		op.text, op.source = in.text, core.TurnSourceText
	}

	caller := callerPart{
		text:      cleanText(res.Caller.Text),
		source:    core.TurnSourceLLM,
		shouldEnd: convert.Deref(res.Caller.ShouldEnd),
		endReason: strings.TrimSpace(convert.Deref(res.Caller.EndReason)),
		unknown:   convert.Deref(res.Caller.IsUnknownAnswer),
		emotional: convert.NonEmptyPtr(res.Caller.EmotionalState),
		engine:    &res.Engine,
	}
	if convert.Deref(res.Fallback) {
		caller.source, caller.fallback = core.TurnSourceScript, true
	}
	if op.noSpeech {
		caller.source = core.TurnSourceScript // служебная фраза «не слышу», не реплика модели
		if caller.text == "" {
			caller.text = reaskText
		}
	} else if caller.text == "" {
		s.log.Warn("ai-service returned empty caller reply: local reply", "attempt", a.ID, "turn", in.turnNo)
		return op, s.localReply(ctx, a, in.turnNo, snap)
	}
	if a.Settings.Voice.TTSEnabled && res.Caller.Tts != nil {
		if p, ok := cleanMediaPath(res.Caller.Tts.FilePath); ok {
			caller.audioPath = &p
			if d := res.Caller.Tts.DurationMs; d > 0 {
				caller.durationMs = &d
			}
		} else if res.Caller.Tts.FilePath != "" {
			s.log.Warn("ai-service returned bad tts path", "attempt", a.ID, "turn", in.turnNo)
		}
	}
	if caller.durationMs == nil {
		d := speechDurationMs(caller.text)
		caller.durationMs = &d
	}
	if caller.emotional == nil {
		caller.emotional = convert.NonEmpty(a.Script.Caller.EmotionalState)
	}
	if !op.noSpeech {
		caller.revealed = allowedFacts(a.Script, convert.SliceFromPtr(res.Caller.RevealedFactIds))
	}
	return op, caller
}

// noSpeechResponse — речь не распознана: ничего не сохраняется, номер хода не расходуется,
// заявитель переспрашивает (его реплика — только в ответе).
func (s *Service) noSpeechResponse(start time.Time, a *attemptCtx, turnNo int, caller callerPart) *public.DialogueTurnResponse {
	now := time.Now().UTC()
	row := store.DialogueTurnRow{
		AttemptID:      a.ID,
		TurnNo:         turnNo,
		Speaker:        core.SpeakerCaller,
		Text:           caller.text,
		Source:         caller.source,
		AudioPath:      caller.audioPath,
		DurationMs:     caller.durationMs,
		At:             now,
		AtMs:           msSince(a.CallAcceptedAt, now),
		EmotionalState: caller.emotional,
	}
	latency := int(time.Since(start).Milliseconds())
	return &public.DialogueTurnResponse{
		TurnNo:     turnNo,
		Caller:     store.DialogueTurnToView(&row),
		NoSpeech:   true,
		CallEnded:  false,
		Fallback:   false,
		NextTurnNo: turnNo,
		LatencyMs:  &latency,
	}
}

// turnMeta — attempt_dialogue_turns.meta; форма = model.TurnMeta, engine как пришёл
// (без перегонки через map).
type turnMeta struct {
	Engine          *components.Engine `json:"engine,omitempty"`
	LatencyMs       int                `json:"latency_ms,omitempty"`
	Fallback        bool               `json:"fallback,omitempty"`
	TextRaw         string             `json:"text_raw,omitempty"`
	IsUnknownAnswer bool               `json:"is_unknown_answer,omitempty"`
}

func (m turnMeta) bytes() []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// persistTurn — транзакция: блокировка попытки (ещё идёт, не завершена, номер верный) ->
// один батч (обмен, счётчик/завершение, события) -> WS после COMMIT. notBefore — когда
// появилась реплика заявителя, на которую отвечает оператор (нижняя граница его времени).
func (s *Service) persistTurn(ctx context.Context, start, notBefore time.Time, a *attemptCtx, in *turnInput, op operatorPart, caller callerPart) (*public.DialogueTurnResponse, error) {
	now := eventTime(time.Now())
	opAt := operatorTime(in.recordedAt, start, notBefore, convert.Deref(op.durationMs), now)
	latency := int(now.Sub(start).Milliseconds())
	if caller.revealed == nil {
		caller.revealed = []string{}
	}

	opRow := store.DialogueTurnRow{
		AttemptID: a.ID, TurnNo: in.turnNo, Speaker: core.SpeakerOperator, Text: op.text, Source: op.source,
		Confidence: op.confidence, DurationMs: op.durationMs, At: opAt, AtMs: msSince(a.CallAcceptedAt, opAt),
		RevealedFactIDs: []string{},
	}
	callerRow := store.DialogueTurnRow{
		AttemptID: a.ID, TurnNo: in.turnNo, Speaker: core.SpeakerCaller, Text: caller.text, Source: caller.source,
		AudioPath: caller.audioPath, DurationMs: caller.durationMs, At: now, AtMs: max(msSince(a.CallAcceptedAt, now), opRow.AtMs),
		EmotionalState: caller.emotional, RevealedFactIDs: caller.revealed,
	}
	opMeta := turnMeta{TextRaw: op.textRaw}.bytes()
	callerMeta := turnMeta{Engine: caller.engine, LatencyMs: latency, Fallback: caller.fallback, IsUnknownAnswer: caller.unknown}.bytes()

	endReason := endReasonFor(caller, in.turnNo, a.maxTurns())
	turnPayload := map[string]any{"turnNo": in.turnNo}
	evs := []pendingEvent{
		{typ: core.EventDialogueOperator, payload: turnPayload, at: opAt},
		{typ: core.EventDialogueCaller, payload: turnPayload, at: now},
	}
	var endAt *time.Time
	var endReasonPtr *string
	if endReason != "" {
		endAt, endReasonPtr = &now, &endReason
		evs = append(evs, pendingEvent{typ: core.EventDialogueEnded, payload: map[string]any{"reason": endReason}, at: now})
	}

	opView := store.DialogueTurnToView(&opRow)
	callerView := store.DialogueTurnToView(&callerRow)

	err := pg.WithTx(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		var (
			status  string
			endedAt *time.Time
			maxOp   int
		)
		if err := tx.QueryRow(ctx, sqlLockForTurn, a.ID).Scan(&status, &endedAt, &maxOp); err != nil {
			if pg.IsNoRows(err) {
				return httpx.NotFound("Попытка не найдена")
			}
			return fmt.Errorf("dialogue: lock attempt: %w", err)
		}
		switch {
		case maxOp >= in.turnNo:
			return errDuplicateTurn
		case status != core.AttemptInProgress:
			return httpx.Conflict("Попытка уже завершена — реплика не сохранена")
		case endedAt != nil:
			return httpx.Conflict("Разговор уже завершён — реплика не сохранена")
		case maxOp+1 != in.turnNo:
			return httpx.Conflict("Номер реплики не совпадает с ходом разговора").WithDetails(map[string]any{"nextTurnNo": maxOp + 1})
		}

		b := &pgx.Batch{}
		b.Queue(sqlInsertExchange, a.ID, in.turnNo,
			opRow.Text, opRow.Source, opRow.Confidence, opRow.DurationMs, opRow.At, opRow.AtMs, opMeta,
			callerRow.Text, callerRow.Source, callerRow.AudioPath, callerRow.DurationMs, callerRow.At, callerRow.AtMs,
			callerRow.EmotionalState, callerRow.RevealedFactIDs, callerMeta)
		b.Queue(sqlUpdateAfterTurn, a.ID, endAt, endReasonPtr)
		if err := queueEvents(b, a.ID, evs); err != nil {
			return err
		}
		br := tx.SendBatch(ctx, b)
		if _, err := br.Exec(); err != nil {
			br.Close()
			if pg.IsUniqueViolation(err) && pg.ConstraintName(err) == pkTurns {
				return errDuplicateTurn
			}
			return fmt.Errorf("dialogue: insert exchange: %w", err)
		}
		if _, err := br.Exec(); err != nil {
			br.Close()
			return fmt.Errorf("dialogue: update attempt: %w", err)
		}
		published, err := insertEvents(br, evs)
		if cerr := br.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("dialogue: turn batch: %w", cerr)
		}
		if err != nil {
			return err
		}

		lessonID, attemptID := a.LessonID, a.ID
		var hungUp *public.DialogueTurnView
		if endReason == core.EndCallerHungUp {
			hungUp = &callerView
		}
		pg.OnCommit(ctx, func() {
			s.publishTurns(lessonID, attemptID, []public.DialogueTurnView{opView, callerView}, published, hungUp)
		})
		return nil
	})
	if errors.Is(err, errDuplicateTurn) {
		// Ход записал параллельный запрос (другой инстанс) — отдаём сохранённый ответ.
		a2, rows, lerr := load(ctx, s.pool, a.ID, false)
		if lerr != nil {
			return nil, lerr
		}
		tr := summarize(rows)
		return nil, duplicateConflict(a2, &tr, in.turnNo)
	}
	if err != nil {
		return nil, err
	}

	resp := &public.DialogueTurnResponse{
		TurnNo:     in.turnNo,
		Operator:   &opView,
		Caller:     callerView,
		NoSpeech:   false,
		CallEnded:  endReason != "",
		Fallback:   caller.fallback,
		NextTurnNo: in.turnNo + 1,
		LatencyMs:  &latency,
	}
	if endReason != "" {
		resp.EndReason = &endReason
	}
	return resp, nil
}

// endReasonFor — чем закончился разговор после хода turnNo ("" — продолжается).
// Заявитель положил трубку сам (should_end по end_conditions) важнее лимита реплик; но
// should_end с end_reason="max_turns" — это ai-service закрыл разговор по лимиту брифа
// (python-ai-service.md: `turn_no ≥ dialogue.max_turns` → should_end, end_reason
// "max_turns"), а не заявитель: причина — max_turns, WS «заявитель положил трубку» не шлётся.
func endReasonFor(caller callerPart, turnNo, maxTurns int) string {
	switch {
	case caller.shouldEnd && caller.endReason != core.EndMaxTurns:
		return core.EndCallerHungUp
	case caller.shouldEnd, turnNo >= maxTurns:
		return core.EndMaxTurns
	}
	return ""
}

// ---------------------------------------------------------------- запрос к ai-service

// ttsOptions / turnOptions — те же анонимные структуры, что в aiservice.DialogTurnRequest
// (псевдонимы типа: генератор не дал им имён).
type ttsOptions = struct {
	Enabled *bool    `json:"enabled,omitempty"`
	Rate    *float32 `json:"rate,omitempty"`
	Voice   *string  `json:"voice,omitempty"`
}

type turnOptions = struct {
	Lang          *string               `json:"lang,omitempty"`
	MaxReplyWords *int                  `json:"max_reply_words,omitempty"`
	Stt           *aiservice.SttOptions `json:"stt,omitempty"`
	Tts           *ttsOptions           `json:"tts,omitempty"`
}

// buildRequest — контекст хода для stateless ai-service: легенда с брифом, вся история
// (сквозная нумерация), раскрытые факты, реплика оператора текстом или аудио.
func buildRequest(a *attemptCtx, tr *transcript, in *turnInput, snap *settings.Snapshot) *aiservice.DialogTurnRequest {
	lang := "ru"
	words := maxReplyWords
	voice := a.Script.VoiceOr(snap.TTS.Voice)
	rate := float32(snap.TTS.Rate)
	ttsOn := a.Settings.Voice.TTSEnabled
	profile := components.DialogFast
	revealed := tr.revealed()

	opts := &turnOptions{
		Lang:          &lang,
		MaxReplyWords: &words,
		Tts:           &ttsOptions{Enabled: &ttsOn, Voice: &voice, Rate: &rate},
	}
	req := &aiservice.DialogTurnRequest{
		SchemaVersion:   components.N1,
		RequestId:       ids.New(),
		AttemptId:       a.ID,
		TurnNo:          in.turnNo,
		Profile:         &profile,
		CallScript:      convert.CallScriptToContract(a.Script),
		History:         store.DialogueTurnsToContract(tr.rows), // [] — не null (ai-service отвергает null)
		RevealedFactIds: &revealed,
		Options:         opts,
	}
	if in.isAudio() {
		sttProfile := components.SttDefault
		itn := true
		hints := sttHints(a.Script)
		opts.Stt = &aiservice.SttOptions{Lang: &lang, Profile: &sttProfile, Itn: &itn, Hints: convert.SlicePtr(hints)}
	} else {
		text := in.text
		req.OperatorText = &text
	}
	return req
}

// sttHints — подсказки распознаванию (≤ 30): адрес, имя заявителя, ключевые слова фактов
// брифа — то, что оператор произносит и что STT чаще всего путает.
func sttHints(cs *model.CallScript) []string {
	out := make([]string, 0, maxSTTHints)
	seen := make(map[string]struct{}, maxSTTHints)
	add := func(v string) {
		v = strings.TrimSpace(v)
		n := utf8.RuneCountInString(v)
		// Голые числа («12») распознаванию не помогают — их нормализует ITN.
		if n < 2 || n > maxHintRunes || len(out) >= maxSTTHints || !hasLetter(v) {
			return
		}
		k := strings.ToLower(v)
		if _, ok := seen[k]; ok {
			return
		}
		seen[k] = struct{}{}
		out = append(out, v)
	}
	ad := &cs.Address
	for _, p := range []*string{ad.Street, ad.House, ad.City, ad.Landmark, ad.Entrance, ad.Apartment} {
		add(convert.Deref(p))
	}
	add(cs.Caller.Name)
	if cs.Dialogue != nil {
		for i := range cs.Dialogue.Facts {
			for _, h := range cs.Dialogue.Facts[i].Hints {
				add(h)
			}
		}
	}
	return out
}

func hasLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// allowedFacts — раскрытые факты только из брифа и не reveal=never (пост-фильтр: модель
// могла выдумать id или «выдать» запрещённое); без дублей.
func allowedFacts(cs *model.CallScript, ids []string) []string {
	out := []string{}
	if cs.Dialogue == nil || len(ids) == 0 {
		return out
	}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		allowed := false
		for i := range cs.Dialogue.Facts {
			f := &cs.Dialogue.Facts[i]
			if f.ID == id {
				allowed = f.Reveal != model.RevealNever
				break
			}
		}
		if !allowed {
			continue
		}
		dup := false
		for _, v := range out {
			if v == id {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, id)
		}
	}
	return out
}

// ---------------------------------------------------------------- мелочи

// clockSlack — допуск при сверке часов браузера с окном, известным серверу (кодирование
// записи, сеть, округления).
const clockSlack = 2 * time.Second

// operatorTime — когда оператор начал говорить, по часам СЕРВЕРА (at_ms реплик заявителя и
// call_accepted_at — серверные; паузы и время ответа в оценке разговора считаются по ним).
//
// Точное начало речи знает только браузер (clientRecordedAt — нажатие PTT), но его часы в
// изолированной сети без NTP расходятся с серверными на секунды и минуты. Поэтому
// clientRecordedAt принимается, только если попадает (с допуском clockSlack) в окно, которое
// сервер знает сам:
//   - не раньше notBefore — появления реплики заявителя, на которую оператор отвечает
//     (запись начинается только после ответа на прошлый ход; для первой — вступление);
//   - не позже latest = «приход запроса − длительность записи» (spokenMs, из STT): запись
//     закончилась до отправки (фронт шлёт её после проверки расшифровки — позже, не раньше).
//
// Вне окна или без clientRecordedAt — серверная оценка latest (для текста — приход
// запроса). Итог не раньше notBefore и не позже now.
func operatorTime(recordedAt *time.Time, start, notBefore time.Time, spokenMs int, now time.Time) time.Time {
	latest := start.Add(-time.Duration(max(spokenMs, 0)) * time.Millisecond)
	at := latest
	if recordedAt != nil {
		t := *recordedAt
		if !t.Before(notBefore.Add(-clockSlack)) && !t.After(latest.Add(clockSlack)) {
			at = t
		}
	}
	if at.Before(notBefore) {
		at = notBefore
	}
	if at.After(now) {
		at = now
	}
	return eventTime(at)
}

// msSince — смещение от принятия вызова, мс (≥ 0).
func msSince(acceptedAt *time.Time, t time.Time) int {
	if acceptedAt == nil {
		return 0
	}
	return max(0, int(t.Sub(*acceptedAt).Milliseconds()))
}

// speechDurationMs — оценка длительности озвучки, когда файла нет (как у мока фронта).
func speechDurationMs(text string) int {
	return max(1200, utf8.RuneCountInString(text)*70)
}

func clamp01(p *float32) *float32 {
	if p == nil {
		return nil
	}
	v := *p
	switch {
	case v != v: // NaN
		return nil
	case v < 0:
		v = 0
	case v > 1:
		v = 1
	}
	return &v
}
