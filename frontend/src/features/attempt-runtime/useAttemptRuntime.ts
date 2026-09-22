import { useCallback, useEffect, useRef, useState } from 'react';
import { api } from '../../shared/api';
import type { Attempt, AttemptEventInput, AttemptEventType, IncidentCardDraft } from '../../shared/types';

export type SaveState = 'idle' | 'saving' | 'saved' | 'error';

/**
 * Журнал событий и досылка черновика — фоновые каналы: они не должны ни
 * прерывать работу обучающегося, ни падать необработанным отказом промиса.
 * Видимые ошибки сохранения показывает состояние `saveState`.
 */
function ignoreBackgroundError(): void {}

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
 * Автосохранение черновика и журнал событий попытки.
 *
 * Черновик пишется целиком (в модели БД `attempt_drafts` — upsert по attempt_id).
 * События ставятся в очередь и уходят батчем — отдельной системы логирования
 * на фронте не заводим, канал один.
 */
export function useAutosave(attemptId: string, card: IncidentCardDraft, enabled: boolean) {
  const [saveState, setSaveState] = useState<SaveState>('idle');
  const [savedAt, setSavedAt] = useState<string | null>(null);
  const firstRun = useRef(true);
  /** Состояние, изменённое, но ещё не подтверждённое сервером. */
  const unsaved = useRef<IncidentCardDraft | null>(null);

  useEffect(() => {
    if (!enabled) return;
    if (firstRun.current) {
      firstRun.current = false;
      return;
    }

    unsaved.current = card;
    setSaveState('saving');

    const timer = setTimeout(() => {
      const sending = card;
      api.attempts
        .updateDraft(attemptId, sending)
        .then((res) => {
          /*
           * Пока запрос шёл, оператор мог изменить карточку снова. Тогда
           * актуально новое состояние, а не подтверждённое сервером: сбрасывать
           * «несохранённое» и показывать «сохранено» нельзя — иначе правка,
           * сделанная во время запроса, потерялась бы при уходе с экрана.
           */
          if (unsaved.current !== sending) return;
          unsaved.current = null;
          setSaveState('saved');
          setSavedAt(res.savedAt);
        })
        .catch(() => {
          if (unsaved.current !== sending) return;
          setSaveState('error');
        });
    }, 900);

    return () => clearTimeout(timer);
  }, [attemptId, card, enabled]);

  /*
   * Уход с экрана раньше, чем сработал debounce, не должен терять правку:
   * отправляем последнее состояние вдогонку. Состояние компонента при этом
   * не трогаем — его уже нет.
   */
  useEffect(
    () => () => {
      if (unsaved.current) {
        // Компонента уже нет — показать ошибку негде, но и ронять её наружу
        // необработанным отказом промиса нельзя.
        void api.attempts.updateDraft(attemptId, unsaved.current).catch(ignoreBackgroundError);
      }
    },
    [attemptId],
  );

  const saveNow = useCallback(async () => {
    setSaveState('saving');
    const sending = card;
    unsaved.current = sending;
    try {
      const res = await api.attempts.updateDraft(attemptId, sending);
      if (unsaved.current !== sending) return;
      unsaved.current = null;
      setSaveState('saved');
      setSavedAt(res.savedAt);
    } catch {
      if (unsaved.current === sending) setSaveState('error');
    }
  }, [attemptId, card]);

  return { saveState, savedAt, saveNow };
}

/**
 * Журнал событий попытки.
 *
 * Изменения текстовых полей дебаунсятся, чтобы не слать событие на каждый
 * символ. Отложенные события обязательно отправляются перед завершением
 * попытки и при уходе с экрана — иначе последнее действие обучающегося
 * не попало бы в хронологию и в разбор у преподавателя.
 */
/** Пауза, за которую события копятся в одну пачку (контракт: до 200 в пачке). */
const EVENT_BATCH_MS = 500;
const EVENT_BATCH_MAX = 200;

/**
 * Сквозной номер события попытки. Хранится между перезагрузками вкладки:
 * по нему сервер отбрасывает повторы, и после F5 нумерация не должна
 * начинаться заново — иначе новые события приняли бы за дубли.
 */
function nextClientSeq(attemptId: string): number {
  const key = `arm112.eventSeq.${attemptId}`;
  let current = 0;
  try {
    current = Number(sessionStorage.getItem(key)) || 0;
  } catch {
    // хранилище недоступно — нумерация живёт до перезагрузки
  }
  const next = current + 1;
  try {
    sessionStorage.setItem(key, String(next));
  } catch {
    // см. выше
  }
  return next;
}

export function useEventLog(attemptId: string) {
  const timers = useRef<Map<string, ReturnType<typeof setTimeout>>>(new Map());
  /** Последнее значение поля, ожидающее отправки, и время самого изменения. */
  const queued = useRef<Map<string, { value: unknown; at: string }>>(new Map());
  /** События, пронумерованные и ещё не подтверждённые сервером. */
  const outbox = useRef<AttemptEventInput[]>([]);
  const batchTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  /**
   * Отправляет накопленное пачкой. При сбое события возвращаются в очередь:
   * повтор безопасен — сервер отбрасывает уже принятые clientSeq.
   */
  const sendBatch = useCallback(async (): Promise<void> => {
    if (batchTimer.current) clearTimeout(batchTimer.current);
    batchTimer.current = null;
    while (outbox.current.length > 0) {
      const batch = outbox.current.splice(0, EVENT_BATCH_MAX);
      try {
        await api.attempts.postEvents(attemptId, batch);
      } catch {
        outbox.current = [...batch, ...outbox.current];
        return;
      }
    }
  }, [attemptId]);

  const enqueue = useCallback(
    (type: AttemptEventType, payload: Record<string, unknown> | undefined, at: string) => {
      outbox.current.push({ clientSeq: nextClientSeq(attemptId), type, payload, at });
      if (!batchTimer.current) {
        batchTimer.current = setTimeout(() => void sendBatch().catch(ignoreBackgroundError), EVENT_BATCH_MS);
      }
    },
    [attemptId, sendBatch],
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

  /** Немедленно отправляет всё отложенное. Вызывается перед submit. */
  const flush = useCallback((): Promise<void> => {
    timers.current.forEach((t) => clearTimeout(t));
    timers.current.clear();
    const pending = [...queued.current.entries()];
    queued.current.clear();
    for (const [field, item] of pending) enqueue('field_changed', { field, value: item.value }, item.at);
    return sendBatch();
  }, [enqueue, sendBatch]);

  useEffect(() => {
    const pendingTimers = timers.current;
    const pendingValues = queued.current;
    return () => {
      pendingTimers.forEach((t) => clearTimeout(t));
      pendingTimers.clear();
      // Отправляем вдогонку то, что не успело уйти по debounce и в пачку.
      const rest = [...pendingValues.entries()];
      pendingValues.clear();
      for (const [field, item] of rest) {
        outbox.current.push({
          clientSeq: nextClientSeq(attemptId),
          type: 'field_changed',
          payload: { field, value: item.value },
          at: item.at,
        });
      }
      if (batchTimer.current) clearTimeout(batchTimer.current);
      batchTimer.current = null;
      const batch = outbox.current.splice(0);
      if (batch.length > 0) void api.attempts.postEvents(attemptId, batch).catch(ignoreBackgroundError);
    };
  }, [attemptId]);

  return { log, logFieldChange, flush };
}
