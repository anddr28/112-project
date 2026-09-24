// Package httpx — HTTP-каркас публичного API: ошибки контракта (ApiError), JSON I/O,
// роутер с RBAC и CSRF-замком, middleware (request id, recover, access log, метрики).
package httpx

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgconn"
)

// Машинные коды ApiError (frontend.v1.yaml, ApiError.code). Фронт принимает решения по коду,
// сообщение показывает как есть — поэтому message всегда по-русски и по делу.
const (
	CodeUnauthorized     = "unauthorized"
	CodeForbidden        = "forbidden"
	CodeNotFound         = "not_found"
	CodeValidation       = "validation"
	CodeConflict         = "conflict"
	CodeUserBlocked      = "user_blocked"
	CodeAIUnavailable    = "ai_unavailable"
	CodeCallerBusy       = "caller_busy"
	CodeAudioTooLong     = "audio_too_long"
	CodeAudioUnsupported = "audio_unsupported"
	CodeRateLimited      = "rate_limited"
	CodeInternal         = "internal"
)

// Error — ошибка, которую хендлер возвращает, а роутер превращает в ApiError-ответ.
type Error struct {
	Status     int
	Code       string
	Message    string
	Details    map[string]any
	RetryAfter int   // секунды -> заголовок Retry-After
	Err        error // внутренняя причина (в лог, не клиенту)
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%d %s: %s: %v", e.Status, e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// WithDetails добавляет details (копия не создаётся — ошибки конструируются на месте).
func (e *Error) WithDetails(kv map[string]any) *Error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	for k, v := range kv {
		e.Details[k] = v
	}
	return e
}

// Wrap — сохранить внутреннюю причину для лога.
func (e *Error) Wrap(err error) *Error { e.Err = err; return e }

func newErr(status int, code, msg string) *Error {
	return &Error{Status: status, Code: code, Message: msg}
}

func BadRequest(msg string) *Error { return newErr(http.StatusBadRequest, CodeValidation, msg) }

// Validation — 400 с ошибками по полям: details.fields = {"login": "обязательно"}.
func Validation(msg string, fields map[string]string) *Error {
	e := newErr(http.StatusBadRequest, CodeValidation, msg)
	if len(fields) > 0 {
		e.Details = map[string]any{"fields": fields}
	}
	return e
}

func Unauthorized() *Error {
	return newErr(http.StatusUnauthorized, CodeUnauthorized, "Требуется вход в систему")
}

func Forbidden(msg string) *Error {
	if msg == "" {
		msg = "Недостаточно прав для этой операции"
	}
	return newErr(http.StatusForbidden, CodeForbidden, msg)
}

func NotFound(msg string) *Error {
	if msg == "" {
		msg = "Не найдено"
	}
	return newErr(http.StatusNotFound, CodeNotFound, msg)
}

func Conflict(msg string) *Error { return newErr(http.StatusConflict, CodeConflict, msg) }

// Unprocessable — 422: запрос корректен, но бизнес-правило не пускает (фронт: code=validation).
func Unprocessable(msg string) *Error {
	return newErr(http.StatusUnprocessableEntity, CodeValidation, msg)
}

func UserBlocked() *Error {
	return newErr(http.StatusLocked, CodeUserBlocked, "Учётная запись заблокирована.")
}

func TooManyRequests(msg string, retryAfterSec int) *Error {
	if msg == "" {
		msg = "Слишком много запросов. Повторите позже."
	}
	e := newErr(http.StatusTooManyRequests, CodeRateLimited, msg)
	e.RetryAfter = retryAfterSec
	return e
}

func AIUnavailable(msg string) *Error {
	if msg == "" {
		msg = "Сервис ИИ временно недоступен. Повторите позже."
	}
	return newErr(http.StatusServiceUnavailable, CodeAIUnavailable, msg)
}

// CallerBusy — 503 caller_busy: «заявитель не отвечает, повторите»; попытка не страдает.
func CallerBusy(retryAfterSec int) *Error {
	if retryAfterSec <= 0 {
		retryAfterSec = 3
	}
	e := newErr(http.StatusServiceUnavailable, CodeCallerBusy, "Заявитель не отвечает, повторите реплику.")
	e.RetryAfter = retryAfterSec
	return e
}

func AudioTooLong() *Error {
	return newErr(http.StatusRequestEntityTooLarge, CodeAudioTooLong, "Реплика слишком длинная: не больше 60 секунд и 2 МБ.")
}

func AudioUnsupported() *Error {
	return newErr(http.StatusUnsupportedMediaType, CodeAudioUnsupported, "Формат аудио не поддерживается (нужен webm/opus, ogg/opus или wav).")
}

// RequestTimeout — 503: запрос не уложился в серверный потолок (перегрузка/зависание БД).
// Код internal (в контракте нет отдельного), Retry-After — клиент может повторить.
func RequestTimeout(err error) *Error {
	return &Error{Status: http.StatusServiceUnavailable, Code: CodeInternal,
		Message: "Сервер не успел обработать запрос. Повторите через несколько секунд.", RetryAfter: 3, Err: err}
}

func Internal(err error) *Error {
	return &Error{Status: http.StatusInternalServerError, Code: CodeInternal, Message: "Внутренняя ошибка сервера", Err: err}
}

// AsError приводит произвольную ошибку к *Error (неизвестные — 500 internal).
// Ошибки ДАННЫХ PostgreSQL (класс 22: NUL-символ в тексте, неверная кодировка, переполнение
// поля, не тот формат) — это мусор во входе клиента, а не авария сервера: 400 validation.
func AsError(err error) *Error {
	var he *Error
	if errors.As(err, &he) {
		return he
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && len(pe.Code) == 5 && pe.Code[:2] == "22" {
		return BadRequest("Недопустимые данные в запросе (символы или формат значения)").Wrap(err)
	}
	return Internal(err)
}

type apiErrorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// WriteError пишет ApiError-ответ.
func WriteError(w http.ResponseWriter, e *Error) {
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	WriteJSON(w, e.Status, apiErrorBody{Code: e.Code, Message: e.Message, Details: e.Details})
}
