/**
 * Имитация WebSocket-каналов для mock-режима.
 *
 * Сервера нет, поэтому «сокет» раз в секунду сверяет состояние mock-БД этой
 * вкладки и рассылает те же сообщения, что go-core: snapshot при подключении,
 * затем изменения с монотонным seq. Без сети (navigator.onLine = false,
 * «Offline» в DevTools) канал не открывается и рвётся — так проверяются
 * переподключение и переход на опрос.
 */

import type { RealtimeConnection, RealtimeHandlers } from '../api/types';
import type { Attempt, Evaluation, MonitorMessage, StudentMessage } from '../types';
import { db, lessonById } from './db';

const TICK_MS = 1000;
const AI_HEALTH_EVERY = 15;

function clone<T>(value: T): T {
  return JSON.parse(JSON.stringify(value)) as T;
}

const offline = (): boolean => typeof navigator !== 'undefined' && navigator.onLine === false;

/** Общая механика: открытие с задержкой, тик сверки, обрыв при потере сети. */
function fakeSocket<M>(handlers: RealtimeHandlers<M>, tick: (emit: (m: M) => void, n: number) => void, open: (emit: (m: M) => void) => void): RealtimeConnection {
  let closed = false;
  let n = 0;
  let timer: ReturnType<typeof setInterval> | undefined;
  const emit = (m: M) => {
    if (!closed) handlers.onMessage(m);
  };
  const drop = () => {
    if (closed) return;
    closed = true;
    clearInterval(timer);
    handlers.onClose(1006);
  };
  const start = setTimeout(() => {
    if (closed) return;
    if (offline()) {
      drop();
      return;
    }
    handlers.onOpen();
    open(emit);
    timer = setInterval(() => {
      if (offline()) {
        drop();
        return;
      }
      n += 1;
      tick(emit, n);
    }, TICK_MS);
  }, 250);
  return {
    close() {
      closed = true;
      clearTimeout(start);
      clearInterval(timer);
    },
  };
}

/** Отпечаток оценки: меняется, когда доехал слой или преподаватель скорректировал балл. */
function evalMark(ev: Evaluation | undefined): string {
  return ev ? `${ev.status}|${ev.finalScore}|${ev.override?.at ?? ''}` : '';
}

export function mockLessonMonitor(lessonId: string, handlers: RealtimeHandlers<MonitorMessage>): RealtimeConnection {
  let seq = 0;
  const at = () => new Date().toISOString();
  // Что уже отправлено: по этим отпечаткам считаются изменения.
  let lessonStatus = '';
  const participants = new Map<string, string>();
  const eventsSent = new Map<string, number>();
  const turnsSent = new Map<string, number>();
  const evals = new Map<string, string>();

  const attemptsOf = (): Attempt[] => db.attempts.filter((a) => a.lessonId === lessonId);

  function remember(): void {
    const lesson = lessonById(lessonId);
    lessonStatus = lesson?.status ?? '';
    for (const p of lesson?.participants ?? []) participants.set(p.userId, `${p.status}|${p.attemptId ?? ''}`);
    for (const a of attemptsOf()) {
      eventsSent.set(a.id, db.events[a.id]?.length ?? 0);
      turnsSent.set(a.id, db.dialogues[a.id]?.turns.length ?? 0);
      evals.set(a.id, evalMark(db.evaluations[a.id]));
    }
  }

  return fakeSocket<MonitorMessage>(
    handlers,
    (emit, n) => {
      const lesson = lessonById(lessonId);
      if (!lesson) return;
      if (lesson.status !== lessonStatus) {
        lessonStatus = lesson.status;
        emit({ seq: ++seq, type: 'lessonStatus', at: at(), lessonStatus: lesson.status });
      }
      for (const p of lesson.participants) {
        const mark = `${p.status}|${p.attemptId ?? ''}`;
        if (participants.get(p.userId) !== mark) {
          participants.set(p.userId, mark);
          emit({ seq: ++seq, type: 'participantStatus', at: at(), participant: clone(p) });
        }
      }
      for (const a of attemptsOf()) {
        const events = db.events[a.id] ?? [];
        for (let i = eventsSent.get(a.id) ?? 0; i < events.length; i += 1) {
          emit({ seq: ++seq, type: 'attemptEvent', at: at(), attemptId: a.id, event: { ...clone(events[i]), id: i + 1 } });
        }
        eventsSent.set(a.id, events.length);
        const turns = db.dialogues[a.id]?.turns ?? [];
        for (let i = turnsSent.get(a.id) ?? 0; i < turns.length; i += 1) {
          emit({ seq: ++seq, type: 'dialogueTurn', at: at(), attemptId: a.id, turn: clone(turns[i]) });
        }
        turnsSent.set(a.id, turns.length);
        const ev = db.evaluations[a.id];
        const mark = evalMark(ev);
        if (ev && evals.get(a.id) !== mark) {
          evals.set(a.id, mark);
          emit({ seq: ++seq, type: 'evaluationUpdated', at: at(), attemptId: a.id, evaluation: clone(ev) });
        }
      }
      // В mock-режиме ИИ-сервиса нет — об этом и сообщаем.
      if (n % AI_HEALTH_EVERY === 0) emit({ seq: ++seq, type: 'aiHealth', at: at(), aiHealth: { ok: false } });
    },
    (emit) => {
      remember();
      const lesson = lessonById(lessonId);
      emit({
        seq,
        type: 'snapshot',
        at: at(),
        snapshot: { lesson: lesson ? clone(lesson) : undefined, attempts: clone(attemptsOf()) },
      });
      emit({ seq: ++seq, type: 'aiHealth', at: at(), aiHealth: { ok: false } });
    },
  );
}

export function mockAttemptChannel(attemptId: string, handlers: RealtimeHandlers<StudentMessage>): RealtimeConnection {
  let seq = 0;
  let evMark = '';
  let finished = false;
  return fakeSocket<StudentMessage>(
    handlers,
    (emit) => {
      const attempt = db.attempts.find((a) => a.id === attemptId);
      if (!attempt) return;
      const ev = db.evaluations[attemptId];
      const mark = evalMark(ev);
      if (ev && mark !== evMark) {
        evMark = mark;
        emit({ seq: ++seq, type: 'evaluationUpdated', at: new Date().toISOString(), evaluation: clone(ev) });
      }
      if (!finished && lessonById(attempt.lessonId)?.status === 'finished') {
        finished = true;
        emit({ seq: ++seq, type: 'lessonFinished', at: new Date().toISOString() });
      }
    },
    () => {
      evMark = evalMark(db.evaluations[attemptId]);
    },
  );
}
