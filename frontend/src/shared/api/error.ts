/**
 * Ошибка сервисного слоя с машинным кодом.
 *
 * Интерфейс принимает решения по `code` (`caller_busy` — предложить повтор,
 * `audio_too_long` — сократить реплику), а текст показывает пользователю как
 * есть. Разбирать русские сообщения регулярками нельзя: они меняются.
 *
 * Класс общий для обеих реализаций `Api`: mock бросает его напрямую, будущий
 * http-клиент заполнит ещё и `status` из ответа (FE-01).
 */

import type { ApiErrorCode } from '../types';

export class ApiRequestError extends Error {
  readonly code: ApiErrorCode;
  /** HTTP-статус; у mock-реализации отсутствует */
  readonly status?: number;
  readonly details?: Record<string, unknown>;
  /** сколько секунд подождать перед повтором (`Retry-After`) */
  readonly retryAfterSec?: number;

  constructor(
    code: ApiErrorCode,
    message: string,
    options: { status?: number; details?: Record<string, unknown>; retryAfterSec?: number } = {},
  ) {
    super(message);
    this.name = 'ApiRequestError';
    this.code = code;
    this.status = options.status;
    this.details = options.details;
    this.retryAfterSec = options.retryAfterSec;
  }
}

export function isApiError(error: unknown): error is ApiRequestError {
  return error instanceof ApiRequestError;
}

/** Проверка конкретного кода: `hasErrorCode(e, 'caller_busy')`. */
export function hasErrorCode(error: unknown, code: ApiErrorCode): boolean {
  return isApiError(error) && error.code === code;
}
