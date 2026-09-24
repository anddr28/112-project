package aijobs

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/callbacks"
	"lct/gocore/internal/settings"
)

// Чистая логика политики очереди: классификация ответов, backoff, решения о ретраях,
// тексты ошибок. Без I/O — тестируется таблицами.

// ---------------------------------------------------------------- типы задач

// jobTypes — все типы в фиксированном порядке: индекс = позиция в массивах статистики
// (массивы вместо map — без аллокаций и хэширования на горячем пути).
var jobTypes = [...]core.JobType{
	core.JobEvaluateGrammar,
	core.JobEvaluateSemantic,
	core.JobEvaluateDialogue,
	core.JobGenerateScenario,
	core.JobTTS,
}

const numTypes = len(jobTypes)

// typeIdx — индекс типа в jobTypes; -1 — неизвестный тип.
func typeIdx(t core.JobType) int {
	switch t {
	case core.JobEvaluateGrammar:
		return 0
	case core.JobEvaluateSemantic:
		return 1
	case core.JobEvaluateDialogue:
		return 2
	case core.JobGenerateScenario:
		return 3
	case core.JobTTS:
		return 4
	}
	return -1
}

// defaultAvgSec — стартовая оценка длительности задачи, пока нет своей статистики.
var defaultAvgSec = [numTypes]float64{2, 20, 25, 120, 3}

// defaultPriority — приоритет новой задачи типа (для EstWaitSec без конкретной задачи).
var defaultPriority = [numTypes]int{core.PriorityGrammar, core.PrioritySemantic, core.PriorityDialogue, core.PriorityGenerate, core.PriorityTTS}

// Полосы ai-service (docs/contracts.md): одна полоса LLM (семантика, разговор, генерация —
// семафор=1), отдельные полосы LanguageTool и TTS. Задача ждёт только свою полосу.
const (
	laneLLM = iota
	laneLT
	laneTTS
)

func laneOf(i int) int {
	switch jobTypes[i] {
	case core.JobEvaluateGrammar:
		return laneLT
	case core.JobTTS:
		return laneTTS
	}
	return laneLLM
}

// laneTypes — типы той же полосы (параметр sqlAhead). Срезы статические — не аллоцируются.
var laneTypeNames = [...][]string{
	laneLLM: {string(core.JobEvaluateSemantic), string(core.JobEvaluateDialogue), string(core.JobGenerateScenario)},
	laneLT:  {string(core.JobEvaluateGrammar)},
	laneTTS: {string(core.JobTTS)},
}

// ---------------------------------------------------------------- ответы ai-service на POST /v1/jobs/*

type sendClass int

const (
	sendAccepted  sendClass = iota // 2xx: задача в очереди ai-service (или уже выполнена — callback повторится)
	sendBusy                       // 429/503: очередь полна — ждём Retry-After, не ошибка задачи
	sendAuth                       // 401/403: неверный X-Internal-Token — ошибка конфигурации, задача не виновата
	sendRejected                   // 400/413/415/422: payload не принят — ретраи бессмысленны
	sendClientErr                  // прочие 4xx (404 — ai-service старой версии и т. п.): ретраи с try_count++
	sendServerErr                  // 5xx: ошибка сервиса — breaker + try_count++
	sendNetErr                     // refused/timeout/reset: breaker, try_count не растёт
)

// classifyJobStatus — класс HTTP-ответа на отправку задачи (контракт ai-service.v1.yaml:
// 202 | 400 | 401 | 429). 422 — дефолт FastAPI для невалидного тела, по смыслу = 400.
// 503 трактуется как 429: честный backpressure, breaker не открывается.
func classifyJobStatus(code int) sendClass {
	switch {
	case code >= 200 && code < 300:
		return sendAccepted
	case code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable:
		return sendBusy
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return sendAuth
	case code == http.StatusBadRequest || code == http.StatusRequestEntityTooLarge ||
		code == http.StatusUnsupportedMediaType || code == http.StatusUnprocessableEntity:
		return sendRejected
	case code >= 500:
		return sendServerErr
	}
	return sendClientErr
}

// Задержки.
const (
	backoffBase    = 5 * time.Second
	backoffMax     = 10 * time.Minute
	busyDefault    = 5 * time.Second  // 429 без Retry-After
	busyMax        = 5 * time.Minute  // защита от гигантского Retry-After
	authRetryDelay = 30 * time.Second // 401/403: ждём, пока админ поправит токен
)

// backoff — 5s·4^n (5 с, 20 с, 80 с, 320 с, …), не больше 10 минут. n — сколько раз задача
// уже падала (try_count до инкремента): не долбим упавший Ollama без пауз (db-design Р12).
func backoff(n int) time.Duration {
	d := backoffBase
	for i := 0; i < n && d < backoffMax; i++ {
		d *= 4
	}
	return min(d, backoffMax)
}

// parseRetryAfter — Retry-After в секундах (целое или HTTP-дата). ok=false — заголовка нет/мусор.
func parseRetryAfter(h string, now time.Time) (time.Duration, bool) {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(h); err == nil {
		if n < 0 {
			return 0, false
		}
		return time.Duration(n) * time.Second, true
	}
	if t, err := http.ParseTime(h); err == nil {
		d := t.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// busyDelay — пауза после 429/503 задачи: Retry-After в пределах [1 с, 5 мин], по умолчанию 5 с.
func busyDelay(h string, now time.Time) time.Duration {
	d, ok := parseRetryAfter(h, now)
	if !ok {
		return busyDefault
	}
	return max(time.Second, min(d, busyMax))
}

// ---------------------------------------------------------------- callback status=failed

// Коды ошибок задачи сверх AiJobError.code (контракт ApplyFailure в core.AIResultHandler).
const (
	CodeDispatchFailed = "dispatch_failed" // ai-service не принял задачу (5xx/4xx), попытки исчерпаны
	CodeReaperTimeout  = "reaper_timeout"  // результата нет дольше reaperMaxResends × settings.ai.reaper_after_sec
	CodeBadPayload     = "bad_payload"     // ai-service отверг payload (400) — ошибка go-core
	CodeAIUnavailable  = "ai_unavailable"  // ai-service недоступен дольше reaper_after_sec (только evaluate_*)
)

const (
	codeInvalidOutput = string(callbacks.LlmInvalidOutput)
	// codeResent — ai_jobs.error живой задачи, которую reaper переотправил (не отказ).
	codeResent = "resent"
)

// reaperMaxResends — сколько раз reaper переотправляет задачу без результата (раз в
// settings.ai.reaper_after_sec), прежде чем признать её зависшей: 12 × 15 мин = 3 ч по умолчанию.
// Попытки (try_count) переотправка не тратит — повтор идемпотентен, а долгая очередь
// ai-service (фоновые LLM-задачи ждут, пока идут ходы диалога) — не отказ задачи. Предел
// нужен против задачи, которую ai-service теряет раз за разом (падение процесса на ней).
const reaperMaxResends = 12

// reaperHoldSec — сколько секунд с первой отправки задача может оставаться без результата.
func reaperHoldSec(ai *settings.AI) float64 {
	return float64(max(ai.ReaperAfterSec, 1)) * reaperMaxResends
}

// evaluateTypeNames — типы оценочных задач: при долгой недоступности ai-service они
// проваливаются (ai_unavailable), чтобы оценка закрылась по доступным слоям с «ревью».
var evaluateTypeNames = []string{
	string(core.JobEvaluateGrammar), string(core.JobEvaluateSemantic), string(core.JobEvaluateDialogue),
}

// shouldRetry — повторять ли задачу после callback'а status=failed. prevCode — код
// предыдущего failed-callback'а этой задачи ("" — не было).
// bad_payload не повторяется никогда (даже если ai-service ошибочно прислал retryable=true);
// llm_invalid_output — «retryable 1 раз» (контракт AiJobError.code): второй невалидный ответ
// подряд — отказ; после отказа другого рода (таймаут, reaper, 5xx) один повтор положен.
// Общий предел — max_tries.
func shouldRetry(code string, retryable bool, tryCount, maxTries int, prevCode string) bool {
	if !retryable || code == CodeBadPayload {
		return false
	}
	if code == codeInvalidOutput && prevCode == codeInvalidOutput {
		return false
	}
	return tryCount+1 < maxTries
}

const maxErrText = 500

// errorText — текст ai_jobs.error: "code: message" (код — префикс до ": ", его читает humanError).
func errorText(code, msg string) string {
	msg = strings.TrimSpace(msg)
	if len(msg) > maxErrText {
		msg = truncateUTF8(msg, maxErrText) + "…"
	}
	if msg == "" {
		return code
	}
	return code + ": " + msg
}

// truncateUTF8 обрезает строку не длиннее n байт, не разрывая руну.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// humanError — ai_jobs.error для пользователя (AiJob.error показывается фронтом как есть).
// Технические подробности остаются в БД и логах.
func humanError(stored string) string {
	code, _, _ := strings.Cut(stored, ":")
	switch strings.TrimSpace(code) {
	case "llm_timeout":
		return "Нейросеть не успела подготовить ответ. Повторите попытку позже."
	case "llm_invalid_output":
		return "Нейросеть вернула некорректный ответ. Повторите попытку."
	case "model_unavailable":
		return "Модель нейросети сейчас недоступна. Повторите попытку позже."
	case "lt_unavailable":
		return "Сервис проверки орфографии недоступен. Повторите попытку позже."
	case "tts_failed":
		return "Не удалось озвучить реплику."
	case "stt_failed":
		return "Не удалось распознать речь."
	case CodeBadPayload:
		return "Сервис ИИ отклонил данные задачи. Обратитесь к администратору."
	case CodeDispatchFailed:
		return "Сервис ИИ не принял задачу. Повторите попытку позже."
	case CodeReaperTimeout:
		return "Сервис ИИ не вернул результат вовремя. Повторите попытку позже."
	case CodeAIUnavailable:
		return "Сервис ИИ был недоступен слишком долго, задача не выполнена."
	case "busy":
		return "Сервис ИИ перегружен, задача ждёт своей очереди."
	case codeResent:
		return "Сервис ИИ долго не возвращает результат, задача отправлена повторно."
	case "unreachable":
		return "Сервис ИИ временно недоступен, задача будет отправлена повторно."
	case "auth":
		return "Сервис ИИ отклонил запрос: ошибка настройки. Обратитесь к администратору."
	}
	return "Внутренняя ошибка сервиса ИИ. Повторите попытку позже."
}
