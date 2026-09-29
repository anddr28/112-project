/**
 * Исходящая очередь попытки: черновик карточки и журнал событий.
 *
 * ТЗ: обрыв сети до 30 с не должен терять данные. Поэтому всё, что обучающийся
 * ввёл, сначала записывается в localStorage и только потом уходит на сервер:
 *   - черновик — последнее состояние целиком (сервер делает upsert);
 *   - события — очередь с clientSeq (сервер отбрасывает дубли, повтор безопасен).
 * Сетевая ошибка, 5xx и 429 — повтор с нарастающей паузой; возврат сети
 * (`online`) — отправка сразу. Отказ по существу (4xx: попытка закрыта) —
 * данные снимаются с очереди: повторять их бессмысленно.
 * Хранилище переживает F5 и падение вкладки: при следующем открытии рабочего
 * места очередь дочитывается и досылается.
 */

import { api } from '../../shared/api';
import { isApiError } from '../../shared/api/error';
import { setConnectivity } from '../../shared/api/connectivity';
import type { AttemptEventInput, IncidentCardDraft } from '../../shared/types';

const RETRY_MS = [1000, 2000, 4000, 8000, 10_000];
/** Контракт: до 200 событий в пачке. */
const BATCH_MAX = 200;

export type OutboxStatus = 'idle' | 'sending' | 'saved' | 'offline' | 'error';

export interface OutboxState {
  status: OutboxStatus;
  /** время последнего подтверждения черновика сервером */
  savedAt: string | null;
  pendingEvents: number;
  draftPending: boolean;
}

/** Ошибка связи, а не отказ сервера по существу: такие запросы повторяем. */
export function isRetriable(e: unknown): boolean {
  if (!isApiError(e)) return false;
  if (e.status === undefined) return e.code === 'internal';
  return e.status >= 500 || e.status === 429 || e.status === 408;
}

function read<T>(key: string, fallback: T): T {
  try {
    const raw = localStorage.getItem(key);
    return raw ? (JSON.parse(raw) as T) : fallback;
  } catch {
    return fallback;
  }
}

function write(key: string, value: unknown): void {
  try {
    if (value == null) localStorage.removeItem(key);
    else localStorage.setItem(key, JSON.stringify(value));
  } catch {
    // Хранилище переполнено или недоступно: очередь живёт в памяти до перезагрузки.
  }
}

class AttemptOutbox {
  private readonly draftKey: string;
  private readonly eventsKey: string;
  private readonly seqKey: string;
  private draft: { card: IncidentCardDraft; at: string } | null;
  private events: AttemptEventInput[];
  private state: OutboxState;
  private listeners = new Set<(s: OutboxState) => void>();
  private failures = 0;
  private retryTimer: ReturnType<typeof setTimeout> | undefined;
  private inFlight: Promise<boolean> | null = null;
  /** начало текущего обрыва — момент события `disconnected` */
  private offlineSince: string | null = null;
  private readonly attemptId: string;

  constructor(attemptId: string) {
    this.attemptId = attemptId;
    this.draftKey = `arm112.outbox.draft.${attemptId}`;
    this.eventsKey = `arm112.outbox.events.${attemptId}`;
    this.seqKey = `arm112.eventSeq.${attemptId}`;
    this.draft = read(this.draftKey, null);
    this.events = read<AttemptEventInput[]>(this.eventsKey, []);
    this.state = { status: 'idle', savedAt: null, pendingEvents: this.events.length, draftPending: this.draft != null };
  }

  subscribe(listener: (s: OutboxState) => void): () => void {
    this.listeners.add(listener);
    listener(this.state);
    return () => this.listeners.delete(listener);
  }

  /** Неотправленный черновик с прошлого сеанса — он новее серверного. */
  pendingDraft(): IncidentCardDraft | null {
    return this.draft?.card ?? null;
  }

  putDraft(card: IncidentCardDraft): void {
    this.draft = { card, at: new Date().toISOString() };
    write(this.draftKey, this.draft);
    this.update({ draftPending: true });
  }

  /**
   * Сквозной номер события. Хранится между перезагрузками: после F5 нумерация
   * не должна начинаться заново — иначе новые события приняли бы за дубли.
   */
  nextSeq(): number {
    const next = (read<number>(this.seqKey, 0) || 0) + 1;
    write(this.seqKey, next);
    return next;
  }

  pushEvent(event: AttemptEventInput): void {
    this.events.push(event);
    write(this.eventsKey, this.events);
    this.update({ pendingEvents: this.events.length });
  }

  /** Всё отправлено или отложено до возврата связи; true — сервер подтвердил всё. */
  flush(): Promise<boolean> {
    if (!this.inFlight) {
      clearTimeout(this.retryTimer);
      this.inFlight = this.send().finally(() => {
        this.inFlight = null;
      });
    }
    return this.inFlight;
  }

  /** Попытка закрыта: хранить её очередь больше незачем. */
  clear(): void {
    clearTimeout(this.retryTimer);
    this.draft = null;
    this.events = [];
    write(this.draftKey, null);
    write(this.eventsKey, null);
    this.update({ draftPending: false, pendingEvents: 0 });
  }

  private async send(): Promise<boolean> {
    if (!this.draft && this.events.length === 0) return true;
    this.update({ status: 'sending' });
    try {
      if (this.draft) {
        const sending = this.draft;
        const res = await api.attempts.updateDraft(this.attemptId, sending.card);
        // Пока шёл запрос, карточку могли изменить: тогда новое состояние ещё впереди.
        if (this.draft === sending) {
          this.draft = null;
          write(this.draftKey, null);
        }
        this.update({ savedAt: res.savedAt, draftPending: this.draft != null });
      }
      while (this.events.length > 0) {
        const batch = this.events.slice(0, BATCH_MAX);
        const res = await api.attempts.postEvents(this.attemptId, batch);
        this.events = this.events.slice(batch.length);
        write(this.eventsKey, this.events);
        // Сервер помнит больше нас (другая вкладка, очищенное хранилище) — догоняем нумерацию.
        if (res.lastSeq > (read<number>(this.seqKey, 0) || 0)) write(this.seqKey, res.lastSeq);
        this.update({ pendingEvents: this.events.length });
      }
      this.recovered();
      this.update({ status: this.draft ? 'sending' : 'saved' });
      // Пока шла отправка, пришли новые правки — отправляем и их.
      if (this.draft || this.events.length > 0) return this.send();
      return true;
    } catch (e) {
      if (isRetriable(e)) {
        this.lost();
        const delay = RETRY_MS[Math.min(this.failures, RETRY_MS.length - 1)];
        this.failures += 1;
        this.retryTimer = setTimeout(() => void this.flush(), delay);
        this.update({ status: 'offline' });
        return false;
      }
      // Отказ по существу (попытка закрыта, черновик не принимается) — не повторяем.
      this.draft = null;
      this.events = [];
      write(this.draftKey, null);
      write(this.eventsKey, null);
      this.update({ status: 'error', draftPending: false, pendingEvents: 0 });
      return false;
    }
  }

  private lost(): void {
    if (this.offlineSince) return;
    this.offlineSince = new Date().toISOString();
    setConnectivity({ offline: true });
  }

  /** Связь вернулась: фиксируем обрыв в хронологии попытки — преподаватель увидит его в разборе. */
  private recovered(): void {
    this.failures = 0;
    if (!this.offlineSince) return;
    const since = this.offlineSince;
    this.offlineSince = null;
    setConnectivity({ offline: false });
    const now = new Date().toISOString();
    this.pushEvent({ clientSeq: this.nextSeq(), type: 'disconnected', at: since });
    this.pushEvent({
      clientSeq: this.nextSeq(),
      type: 'reconnected',
      at: now,
      payload: { durationMs: Date.parse(now) - Date.parse(since) },
    });
  }

  private update(patch: Partial<OutboxState>): void {
    this.state = { ...this.state, ...patch };
    setConnectivity({ pending: this.state.pendingEvents + (this.state.draftPending ? 1 : 0) });
    this.listeners.forEach((l) => l(this.state));
  }
}

const boxes = new Map<string, AttemptOutbox>();

/** Одна очередь на попытку: черновик и события делят её и один счётчик обрывов. */
export function outboxFor(attemptId: string): AttemptOutbox {
  let box = boxes.get(attemptId);
  if (!box) {
    box = new AttemptOutbox(attemptId);
    boxes.set(attemptId, box);
  }
  return box;
}

// Сеть вернулась — досылаем всё, не дожидаясь паузы повтора.
if (typeof window !== 'undefined') {
  window.addEventListener('online', () => boxes.forEach((b) => void b.flush()));
}
