/**
 * Потеря сессии посреди работы.
 *
 * Сервер отвечает 401, когда сессия истекла или отозвана, и 423, когда учётную
 * запись заблокировали. Без общей реакции каждый экран показывал бы свою ошибку,
 * а обучающийся оставался бы в интерфейсе, который уже ничего не может сохранить.
 * Сервисный слой сообщает о потере сессии, хранилище авторизации — реагирует.
 * Модуль не знает о хранилище, чтобы не замыкать импорты api ↔ auth.
 */

export type SessionLossReason = 'unauthorized' | 'user_blocked';

let listener: ((reason: SessionLossReason) => void) | null = null;

export function onSessionLost(handler: (reason: SessionLossReason) => void): void {
  listener = handler;
}

export function sessionLost(reason: SessionLossReason): void {
  listener?.(reason);
}
