import { useCallback, useEffect, useRef, useState } from 'react';
import { api } from '../../shared/api';
import type { Attempt, AttemptEventType, IncidentCardDraft } from '../../shared/types';

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
export function useEventLog(attemptId: string) {
  const timers = useRef<Map<string, ReturnType<typeof setTimeout>>>(new Map());
  /** Последнее значение поля, ожидающее отправки, и время самого изменения. */
  const queued = useRef<Map<string, { value: unknown; at: string }>>(new Map());

  /*
   * `at` — момент самого изменения, а не момент отправки. Событие уходит после
   * дебаунса, но время первого ввода считается по нему: иначе время реакции
   * обучающегося было бы завышено на длину дебаунса.
   */
  const send = useCallback(
    (field: string, value: unknown, at: string) => {
      void api.attempts
        .addEvent(attemptId, { type: 'field_changed', payload: { field, value }, at })
        .catch(ignoreBackgroundError);
    },
    [attemptId],
  );

  const log = useCallback(
    (type: AttemptEventType, payload?: Record<string, unknown>) => {
      void api.attempts
        .addEvent(attemptId, { type, payload, at: new Date().toISOString() })
        .catch(ignoreBackgroundError);
    },
    [attemptId],
  );

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
          send(field, value, at);
        }, 1000),
      );
    },
    [send],
  );

  /** Немедленно отправляет всё отложенное. Вызывается перед submit. */
  const flush = useCallback(() => {
    timers.current.forEach((t) => clearTimeout(t));
    timers.current.clear();
    const pending = [...queued.current.entries()];
    queued.current.clear();
    for (const [field, item] of pending) send(field, item.value, item.at);
  }, [send]);

  useEffect(() => {
    const pendingTimers = timers.current;
    const pendingValues = queued.current;
    return () => {
      pendingTimers.forEach((t) => clearTimeout(t));
      pendingTimers.clear();
      // Отправляем вдогонку то, что не успело уйти по debounce.
      const rest = [...pendingValues.entries()];
      pendingValues.clear();
      for (const [field, item] of rest) {
        void api.attempts
          .addEvent(attemptId, {
            type: 'field_changed',
            payload: { field, value: item.value },
            at: item.at,
          })
          .catch(ignoreBackgroundError);
      }
    };
  }, [attemptId]);

  return { log, logFieldChange, flush };
}
