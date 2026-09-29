import { useEffect, useRef, useState } from 'react';
import type { RealtimeConnection, RealtimeHandlers } from '../api/types';

/**
 * Состояние канала для индикатора:
 *   connecting   — первое подключение;
 *   open         — поток идёт;
 *   reconnecting — связь потеряна, следующая попытка через `retryInSec`;
 *   polling      — канал недоступен повторно: экран обновляется опросом,
 *                  попытки переподключиться продолжаются в фоне.
 */
export type ChannelStatus = 'idle' | 'connecting' | 'open' | 'reconnecting' | 'polling';

/** Экспоненциальная пауза: 1 → 2 → 4 → 8 → 15 с (FE-08). */
const BACKOFF_MS = [1000, 2000, 4000, 8000, 15_000];
/** Столько неудач подряд — и экран переходит на опрос, не дожидаясь канала. */
const FAILURES_BEFORE_POLLING = 3;
/** Код закрытия «сессия отозвана» (go-core): переподключаться бессмысленно. */
const POLICY_VIOLATION = 1008;

interface Options<M> {
  /** false — канал не нужен (занятие не идёт) и закрывается */
  enabled: boolean;
  /** смена ключа — другой канал: seq начинается заново */
  channelKey: string;
  connect: (since: number | undefined, handlers: RealtimeHandlers<M>) => RealtimeConnection;
  onMessage: (message: M) => void;
}

/**
 * Канал реального времени с переподключением.
 *
 * Каждое сообщение несёт монотонный seq: при переподключении передаётся
 * последний полученный (`since`), и сервер досылает пропущенное из буфера
 * либо присылает snapshot, который целиком заменяет состояние экрана.
 * Повторы (seq ≤ последнего) отбрасываются: после обрыва сервер может
 * прислать то, что клиент уже успел получить.
 */
export function useRealtimeChannel<M extends { seq: number; type: string }>({
  enabled,
  channelKey,
  connect,
  onMessage,
}: Options<M>): { status: ChannelStatus; retryInSec?: number } {
  const [status, setStatus] = useState<ChannelStatus>('idle');
  const [retryInSec, setRetryInSec] = useState<number | undefined>();
  // Колбэки меняют идентичность на каждом рендере — соединение от этого не пересоздаётся.
  const handlers = useRef({ connect, onMessage });
  useEffect(() => {
    handlers.current = { connect, onMessage };
  });

  useEffect(() => {
    if (!enabled) return;
    let lastSeq: number | undefined;
    let failures = 0;
    let conn: RealtimeConnection | null = null;
    let retryTimer: ReturnType<typeof setTimeout> | undefined;
    let stopped = false;

    const open = () => {
      if (stopped) return;
      setStatus((s) => (s === 'polling' ? s : failures === 0 ? 'connecting' : 'reconnecting'));
      let opened = false;
      conn = handlers.current.connect(lastSeq, {
        onOpen() {
          opened = true;
          failures = 0;
          setRetryInSec(undefined);
          setStatus('open');
        },
        onMessage(message) {
          if (message.type !== 'snapshot' && lastSeq != null && message.seq <= lastSeq) return;
          lastSeq = message.seq;
          handlers.current.onMessage(message);
        },
        onClose(code) {
          conn = null;
          if (stopped) return;
          if (!opened) failures += 1;
          if (code === POLICY_VIOLATION) {
            // Сессия закрыта сервером: опрос получит 401, и приложение уведёт на вход.
            setStatus('polling');
            return;
          }
          const delay = BACKOFF_MS[Math.min(failures, BACKOFF_MS.length - 1)];
          setRetryInSec(Math.round(delay / 1000));
          setStatus(failures >= FAILURES_BEFORE_POLLING ? 'polling' : 'reconnecting');
          retryTimer = setTimeout(open, delay);
        },
      });
    };
    open();

    // Сеть вернулась — не ждём конца паузы.
    const onOnline = () => {
      if (conn || stopped) return;
      clearTimeout(retryTimer);
      open();
    };
    window.addEventListener('online', onOnline);

    return () => {
      stopped = true;
      clearTimeout(retryTimer);
      window.removeEventListener('online', onOnline);
      conn?.close();
    };
  }, [enabled, channelKey]);

  // Канал выключен — состояние «нет канала», что бы ни осталось от прошлого подключения.
  return enabled ? { status, retryInSec } : { status: 'idle' };
}
