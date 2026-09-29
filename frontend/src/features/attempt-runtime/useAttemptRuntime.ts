import { useCallback, useEffect, useRef, useState } from 'react';
import { outboxFor } from './outbox';
import type { OutboxState } from './outbox';
import type { Attempt, AttemptEventType, IncidentCardDraft } from '../../shared/types';

/** `offline` — нет связи: данные сохранены на устройстве и будут досланы. */
export type SaveState = 'idle' | 'saving' | 'saved' | 'offline' | 'error';

/**
 * Таймер попытки.
 *
 * Инструкция АРМ-112, п.4: «При открытии новой карточки начинается отсчёт
 * таймера… Таймер останавливается после нажатия кнопки "Сохранить"».
 * Отсчёт идёт ВВЕРХ; превышение норматива подсвечивается, но работу не прерывает.
 *
 * Источник истины — backend: считаем от `callAcceptedAt` с поправкой на
 * расхождение часов (`serverNow`), а не накоплением интервалов. Поэтому
 * сворачивание вкладки или сон машины не ломают учёт времени.
 */
export function useAttemptTimer(attempt: Attempt | null): { elapsedMs: number; exceeded: boolean } {
  const [now, setNow] = useState(0);

  // Поправка на расхождение часов клиента и сервера. Считается в эффекте,
  // а не в рендере: рендер обязан оставаться чистым.
  const serverNow = attempt?.serverNow;
  const offsetRef = useRef(0);

  useEffect(() => {
    offsetRef.current = serverNow ? new Date(serverNow).getTime() - Date.now() : 0;
  }, [serverNow]);

  const running = Boolean(attempt?.callAcceptedAt) && !attempt?.submittedAt;

  useEffect(() => {
    if (!running) return;
    const advance = () => setNow(Date.now() + offsetRef.current);
    advance();
    const id = setInterval(advance, 250);
    return () => clearInterval(id);
  }, [running]);

  if (!attempt?.callAcceptedAt) return { elapsedMs: 0, exceeded: false };

  const started = new Date(attempt.callAcceptedAt).getTime();
  const end = attempt.submittedAt ? new Date(attempt.submittedAt).getTime() : now;
  const elapsedMs = now === 0 && !attempt.submittedAt ? 0 : Math.max(0, end - started);

  return { elapsedMs, exceeded: elapsedMs > attempt.timeLimitSec * 1000 };
}

/**
 * Автосохранение черновика.
 *
 * Черновик пишется целиком (upsert по attempt_id) через исходящую очередь
 * попытки: правка сразу сохраняется на устройстве, а на сервер уходит после
 * паузы ввода. При обрыве связи состояние — `offline`: данные не потеряны и
 * будут досланы, как только сервер ответит.
 */
const DRAFT_DEBOUNCE_MS = 900;

export function useAutosave(attemptId: string, card: IncidentCardDraft, enabled: boolean) {
  const box = outboxFor(attemptId);
  const [state, setState] = useState<OutboxState | null>(null);
  const firstRun = useRef(true);

  useEffect(() => box.subscribe(setState), [box]);

  useEffect(() => {
    if (!enabled) return;
    if (firstRun.current) {
      firstRun.current = false;
      return;
    }
    box.putDraft(card);
    const timer = setTimeout(() => void box.flush(), DRAFT_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [box, card, enabled]);

  // Уход с экрана раньше паузы: черновик уже в очереди, досылаем его сразу.
  useEffect(() => () => void box.flush(), [box]);

  /** Немедленная отправка (перед сдачей и закрытием карточки); true — сервер подтвердил. */
  const saveNow = useCallback(() => {
    box.putDraft(card);
    return box.flush();
  }, [box, card]);

  const saveState: SaveState = !state
    ? 'idle'
    : state.status === 'offline'
      ? 'offline'
      : state.status === 'error'
        ? 'error'
        : state.draftPending || state.status === 'sending'
          ? 'saving'
          : state.savedAt
            ? 'saved'
            : 'idle';

  return { saveState, savedAt: state?.savedAt ?? null, saveNow };
}

/**
 * Журнал событий попытки.
 *
 * Изменения текстовых полей дебаунсятся, чтобы не слать событие на каждый
 * символ. События нумеруются сразу (clientSeq) и лежат в исходящей очереди до
 * подтверждения сервером: повтор после обрыва безопасен, сервер отбрасывает
 * дубли. Отложенные события отправляются перед сдачей и при уходе с экрана —
 * иначе последнее действие не попало бы в хронологию у преподавателя.
 */
/** Пауза, за которую события копятся в одну пачку. */
const EVENT_BATCH_MS = 500;

export function useEventLog(attemptId: string) {
  const box = outboxFor(attemptId);
  const timers = useRef<Map<string, ReturnType<typeof setTimeout>>>(new Map());
  /** Последнее значение поля, ожидающее отправки, и время самого изменения. */
  const queued = useRef<Map<string, { value: unknown; at: string }>>(new Map());
  const batchTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const enqueue = useCallback(
    (type: AttemptEventType, payload: Record<string, unknown> | undefined, at: string) => {
      box.pushEvent({ clientSeq: box.nextSeq(), type, payload, at });
      if (!batchTimer.current) {
        batchTimer.current = setTimeout(() => {
          batchTimer.current = null;
          void box.flush();
        }, EVENT_BATCH_MS);
      }
    },
    [box],
  );

  const log = useCallback(
    (type: AttemptEventType, payload?: Record<string, unknown>) => {
      enqueue(type, payload, new Date().toISOString());
    },
    [enqueue],
  );

  /*
   * `at` — момент самого изменения, а не момент отправки. Событие уходит после
   * дебаунса, но время первого ввода считается по нему: иначе время реакции
   * обучающегося было бы завышено на длину дебаунса.
   */
  const logFieldChange = useCallback(
    (field: string, value: unknown) => {
      const existing = timers.current.get(field);
      if (existing) clearTimeout(existing);

      const at = new Date().toISOString();
      queued.current.set(field, { value, at });
      timers.current.set(
        field,
        setTimeout(() => {
          timers.current.delete(field);
          queued.current.delete(field);
          enqueue('field_changed', { field, value }, at);
        }, 1000),
      );
    },
    [enqueue],
  );

  /** Переносит отложенные изменения полей в очередь (с их исходным временем). */
  const drainFieldChanges = useCallback(() => {
    timers.current.forEach((t) => clearTimeout(t));
    timers.current.clear();
    const pending = [...queued.current.entries()];
    queued.current.clear();
    for (const [field, item] of pending) {
      box.pushEvent({ clientSeq: box.nextSeq(), type: 'field_changed', payload: { field, value: item.value }, at: item.at });
    }
  }, [box]);

  /** Немедленно отправляет всё отложенное. Вызывается перед submit; true — сервер подтвердил. */
  const flush = useCallback((): Promise<boolean> => {
    drainFieldChanges();
    if (batchTimer.current) clearTimeout(batchTimer.current);
    batchTimer.current = null;
    return box.flush();
  }, [box, drainFieldChanges]);

  useEffect(
    () => () => {
      // Уход с экрана: отложенное — в очередь и на сервер. Не дошло — дошлётся при следующем открытии.
      drainFieldChanges();
      if (batchTimer.current) clearTimeout(batchTimer.current);
      batchTimer.current = null;
      void box.flush();
    },
    [box, drainFieldChanges],
  );

  return { log, logFieldChange, flush };
}
