/**
 * HTTP-реализация сервисного слоя по `contracts/openapi/frontend.v1.yaml`.
 *
 * Включается флагом `VITE_USE_MOCKS=false` (см. ./index.ts). Пути, методы и тела
 * запросов — строго из контракта; сессия — cookie (`credentials: 'include'`),
 * мутирующие запросы несут `X-Requested-With: fetch` (CSRF-замок контракта).
 * Ошибки сервера (`ApiError {code, message, details}`) превращаются в
 * `ApiRequestError`, который уже умеет показывать UI.
 */

import { ApiRequestError } from './error';
import type { Api } from './types';
import type { ApiErrorCode } from '../types';

const BASE = (import.meta.env.VITE_API_BASE as string | undefined) ?? '/api/v1';
/** Ход разговора включает STT + модель + озвучку — ему нужен запас по времени. */
const TIMEOUT_MS = 15_000;
const DIALOGUE_TIMEOUT_MS = 25_000;

const STATUS_CODE: Record<number, ApiErrorCode> = {
  400: 'validation',
  401: 'unauthorized',
  403: 'forbidden',
  404: 'not_found',
  409: 'conflict',
  413: 'audio_too_long',
  415: 'audio_unsupported',
  422: 'validation',
  423: 'user_blocked',
  429: 'rate_limited',
  503: 'ai_unavailable',
};

interface RequestOptions {
  body?: unknown;
  form?: FormData;
  query?: Record<string, string | number | undefined>;
  timeoutMs?: number;
}

async function request<T>(method: string, path: string, options: RequestOptions = {}): Promise<T> {
  const url = new URL(BASE + path, window.location.origin);
  for (const [k, v] of Object.entries(options.query ?? {})) {
    if (v !== undefined && v !== '') url.searchParams.set(k, String(v));
  }

  const headers: Record<string, string> = { Accept: 'application/json' };
  if (method !== 'GET') headers['X-Requested-With'] = 'fetch';
  let body: BodyInit | undefined;
  if (options.form) {
    body = options.form;
  } else if (options.body !== undefined) {
    headers['Content-Type'] = 'application/json';
    body = JSON.stringify(options.body);
  }

  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), options.timeoutMs ?? TIMEOUT_MS);
  let res: Response;
  try {
    res = await fetch(url, { method, headers, body, credentials: 'include', signal: controller.signal });
  } catch (e) {
    throw new ApiRequestError(
      'internal',
      e instanceof DOMException && e.name === 'AbortError'
        ? 'Сервер не ответил вовремя. Повторите попытку.'
        : 'Нет связи с сервером. Проверьте подключение.',
    );
  } finally {
    clearTimeout(timer);
  }

  if (res.status === 204) return undefined as T;
  const text = await res.text();
  const data: unknown = text ? JSON.parse(text) : undefined;
  if (res.ok) return data as T;

  const err = (data ?? {}) as { code?: ApiErrorCode; message?: string; details?: Record<string, unknown> };
  const retry = Number(res.headers.get('Retry-After'));
  throw new ApiRequestError(err.code ?? STATUS_CODE[res.status] ?? 'internal', err.message ?? `Ошибка сервера (${res.status})`, {
    status: res.status,
    details: err.details,
    retryAfterSec: Number.isFinite(retry) && retry > 0 ? retry : undefined,
  });
}

/** Отсутствие ресурса, которое для UI означает «ещё нет», а не ошибку. */
async function orNull<T>(p: Promise<T>, codes: ApiErrorCode[]): Promise<T | null> {
  try {
    return await p;
  } catch (e) {
    if (e instanceof ApiRequestError && codes.includes(e.code)) return null;
    throw e;
  }
}

const enc = encodeURIComponent;

export const httpApi: Api = {
  auth: {
    login: (input) => request('POST', '/auth/login', { body: input }),
    me: () => orNull(request('GET', '/auth/me'), ['unauthorized']),
    logout: () => request('POST', '/auth/logout'),
    demoAccounts: () => request('GET', '/auth/demo-accounts'),
  },

  classifier: {
    incidentTypes: () => request('GET', '/classifier/types'),
    searchTypes: (q) => request('GET', '/classifier/types/search', { query: { q } }),
    featured: () => request('GET', '/classifier/featured'),
    attributes: (typeId) => request('GET', `/classifier/types/${enc(typeId)}/attributes`),
    reference: (typeId) => request('GET', `/classifier/types/${enc(typeId)}/reference`),
    labels: () => request('GET', '/classifier/labels'),
    resolveServices: (input) => request('POST', '/classifier/resolve-services', { body: input }),
  },

  services: {
    list: () => request('GET', '/services'),
  },

  users: {
    list: () => request('GET', '/users'),
    setBlocked: (userId, blocked) => request('PUT', `/users/${enc(userId)}/blocked`, { body: { blocked } }),
  },

  address: {
    suggest: (q) => request('GET', '/address/suggest', { query: { q } }),
  },

  aiJobs: {
    get: (jobId) => request('GET', `/ai-jobs/${enc(jobId)}`),
  },

  scenarios: {
    list: () => request('GET', '/scenarios'),
    get: (id) => request('GET', `/scenarios/${enc(id)}`),
    create: (input) => request('POST', '/scenarios', { body: input }),
    generate: (input) => request('POST', '/scenarios/generate', { body: input }),
    update: (id, patch) => request('PATCH', `/scenarios/${enc(id)}`, { body: patch }),
    // BACKEND DEPENDENCY: endpoint объявлен backend, в frontend.v1.yaml его пока нет.
    createVersion: (id) => request('POST', `/scenarios/${enc(id)}/versions`),
    approve: (id) => request('POST', `/scenarios/${enc(id)}/approve`),
    reject: (id, reason) => request('POST', `/scenarios/${enc(id)}/reject`, { body: { reason } }),
  },

  lessons: {
    defaultSettings: () => request('GET', '/lessons/default-settings'),
    list: () => request('GET', '/lessons'),
    get: (id) => request('GET', `/lessons/${enc(id)}`),
    create: (input) => request('POST', '/lessons', { body: input }),
    start: (id) => request('POST', `/lessons/${enc(id)}/start`),
    finish: (id) => request('POST', `/lessons/${enc(id)}/finish`),
    assigned: () => request('GET', '/lessons/assigned'),
  },

  attempts: {
    get: (id) => request('GET', `/attempts/${enc(id)}`),
    forLesson: (lessonId) => request('GET', `/lessons/${enc(lessonId)}/attempts`),
    acceptCall: (id) => request('POST', `/attempts/${enc(id)}/accept-call`),
    getDraft: (id) => request('GET', `/attempts/${enc(id)}/draft`),
    updateDraft: (id, card) => request('PUT', `/attempts/${enc(id)}/draft`, { body: card }),
    postEvents: (id, events) => request('POST', `/attempts/${enc(id)}/events`, { body: { events } }),
    events: (id) => request('GET', `/attempts/${enc(id)}/events`),
    changeServiceStatus: ({ attemptId, serviceId, ...body }) =>
      request('POST', `/attempts/${enc(attemptId)}/services/${enc(serviceId)}/status`, { body }),
    addService: (id, serviceCode) => request('POST', `/attempts/${enc(id)}/services`, { body: { serviceCode } }),
    removeService: (id, serviceId) => request('DELETE', `/attempts/${enc(id)}/services/${enc(serviceId)}`),
    replay: (id) => request('POST', `/attempts/${enc(id)}/replay`),
    submit: (id, card) => request('POST', `/attempts/${enc(id)}/submit`, { body: { card } }),
  },

  evaluation: {
    // 404 — оценки ещё нет (попытка не сдана): для UI это не ошибка.
    get: (attemptId) => orNull(request('GET', `/attempts/${enc(attemptId)}/evaluation`), ['not_found']),
    override: (attemptId, input) => request('POST', `/attempts/${enc(attemptId)}/evaluation/override`, { body: input }),
  },

  callScript: {
    get: (attemptId) => request('GET', `/attempts/${enc(attemptId)}/call-script`),
  },

  dialogue: {
    get: (attemptId) => request('GET', `/attempts/${enc(attemptId)}/dialogue`),
    turn: ({ attemptId, turnNo, text, audio, clientRecordedAt }) => {
      const path = `/attempts/${enc(attemptId)}/dialogue/turns`;
      // Контракт: голос — multipart {turnNo, audio, clientRecordedAt}; текст — JSON {turnNo, text}.
      if (audio) {
        const form = new FormData();
        form.set('turnNo', String(turnNo));
        form.set('audio', audio, 'turn.webm');
        if (clientRecordedAt) form.set('clientRecordedAt', clientRecordedAt);
        return request('POST', path, { form, timeoutMs: DIALOGUE_TIMEOUT_MS });
      }
      return request('POST', path, { body: { turnNo, text: text ?? '' }, timeoutMs: DIALOGUE_TIMEOUT_MS });
    },
    end: (attemptId) => request('POST', `/attempts/${enc(attemptId)}/dialogue/end`),
  },

  feedback: {
    list: (attemptId) => request('GET', `/attempts/${enc(attemptId)}/feedback`),
    add: (attemptId, input) => request('POST', `/attempts/${enc(attemptId)}/feedback`, { body: input }),
  },

  reaction: {
    allowedNext: (current, serviceCode) => request('GET', '/reaction/transitions', { query: { current, serviceCode } }),
  },
};
