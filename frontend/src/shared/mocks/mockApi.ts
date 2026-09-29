/**
 * Mock-реализация сервисного слоя (`shared/api/types.ts`).
 *
 * Здесь НЕТ выдуманных backend endpoint'ов — только доменные операции поверх
 * in-memory состояния. Все задержки имитируют сетевые, чтобы UI сразу
 * проектировался с loading-состояниями.
 */

import type { AcceptCallResult, Api, ChangeStatusInput, CreateLessonInput, CreateScenarioInput, DialogueTurnInput, GenerateScenarioInput, LoginInput, ResolvedServicesResult } from '../api/types';
import { ApiRequestError } from '../api/error';
import type {
  AiJob, AssignedService, Attempt, AttemptEvent, AttemptEventType, DialogueState, DialogueTurnResponse,
  DialogueTurnView, Evaluation, IncidentCardDraft,
  Lesson, ReactionStatus, ReactionStatusEntry, Scenario, StudentCallScript, TeacherFeedback, User,
} from '../types';
import { computeAllowedNext, isTerminal } from './reactionTransitions';
import {
  attemptById, clearAttemptRuntime, db, dialogueRecord, evaluateComplete, evaluatePartial,
  issueAttempts, lessonById, makeLessonSettings, nextIncidentNo, normalizeWeights,
  persistAttemptRuntime, persistCompletedAttempt, persistFeedback, persistLesson, persistScenario, withDialogueWeight,
  scenarioById, setUserBlocked,
} from './db';
import { dialogueFixture } from './fixtures/dialogue';
import { ATTRIBUTE_GROUPS, INCIDENT_TYPES, FREQUENT_TYPE_IDS, SIGNIFICANT_TYPE_IDS, TYPE_REFERENCE, attributeGroupsFor, resolveServices, searchTypes, typeById } from './fixtures/classifier';
import { SERVICES, serviceByCode } from './fixtures/services';
import { suggestAddresses } from './fixtures/addresses';
import { DEMO_ACCOUNTS } from './fixtures/users';
import { FIELD_LABELS, emptyCard } from '../utils/card';
import { uid } from '../utils/id';
import { buildOverview, buildProgress, evaluatedCount } from './reporting';
import { listAudit, listBackups, listLogs, listSettings, mockHealth, runBackup, updateSetting } from './admin';
import { createMaterial, getMaterial, listMaterials, materialFileUrl, removeMaterial } from './materials';
import { mockAttemptChannel, mockLessonMonitor } from './realtime';
import { certificateHtml, exportBundle, importBundle, scenarioFromAttempt } from './scenarioTransfer';

const SESSION_KEY = 'arm112.session';

/** Единая формулировка отказа: и при входе, и при восстановлении сессии. */
const BLOCKED_MESSAGE = 'Учётная запись заблокирована.';

/**
 * Сетевая задержка. Без сети (navigator.onLine = false — например, «Offline» в
 * DevTools) запрос падает так же, как у httpApi: ошибкой без HTTP-статуса.
 * Так в mock-режиме проверяется устойчивость к обрыву связи.
 */
function delay<T>(value: T, ms = 220): Promise<T> {
  return new Promise((resolve, reject) =>
    setTimeout(() => {
      if (typeof navigator !== 'undefined' && navigator.onLine === false) {
        reject(new ApiRequestError('internal', 'Нет связи с сервером. Проверьте подключение.'));
        return;
      }
      resolve(value);
    }, ms),
  );
}

function fail(message: string): never {
  throw new Error(message);
}

/** Пользователь сессии с одной из ролей — иначе 403, как у сервера. */
function requireRole(...roles: User['role'][]): User {
  const user = sessionUser();
  if (!user) throw new ApiRequestError('unauthorized', 'Сессия истекла — войдите снова', { status: 401 });
  if (!roles.includes(user.role)) throw new ApiRequestError('forbidden', 'Недостаточно прав для этой операции', { status: 403 });
  return user;
}

function clone<T>(value: T): T {
  return JSON.parse(JSON.stringify(value)) as T;
}

/** Собирает AssignedService из кода службы и текущего статуса. */
function buildService(
  code: string,
  source: AssignedService['source'],
  isPrimary: boolean,
  reason: string | undefined,
  existing?: AssignedService,
): AssignedService {
  const ref = serviceByCode(code);
  if (!ref) fail(`Неизвестная служба: ${code}`);

  if (existing) {
    return { ...existing, isPrimary: existing.isPrimary || isPrimary, reason: existing.reason ?? reason };
  }

  /*
   * Памятка ДДС: «Добавлена» и «Получена службой» проставляет система — при
   * направлении карточки и при её поступлении на сервер службы. В учебном
   * контуре доставка мгновенная, поэтому служба сразу оказывается в состоянии
   * «Получена службой», и диспетчеру доступны «Принята» / «Не принята».
   */
  const now = new Date().toISOString();
  const history: ReactionStatusEntry[] = [
    { status: 'Добавлена', at: now, operator: 'система' },
    { status: 'Получена службой', at: now, operator: 'система' },
  ];

  return {
    serviceId: ref.id,
    code: ref.code,
    name: ref.name,
    shortName: ref.shortName,
    isPrimary,
    source,
    reason,
    currentStatus: 'Получена службой',
    currentStatusAt: now,
    history,
    allowedNext: computeAllowedNext('Получена службой', ref.code),
    editable: true,
  };
}

/** Текущий пользователь mock-сессии. */
function sessionUser(): User | undefined {
  const id = sessionStorage.getItem(SESSION_KEY);
  return db.users.find((x) => x.id === id);
}

/**
 * Проверка прав на попытку.
 *
 * Источник истины по правам — backend. Mock воспроизводит одно правило, которое
 * нельзя обеспечить скрытием кнопки: студент работает только со своими
 * попытками. Без этой проверки чужой attemptId в адресной строке открыл бы
 * чужую карточку и чужую оценку.
 */
function requireAttempt(id: string): Attempt {
  const attempt = attemptById(id);
  if (!attempt) fail('Попытка не найдена');
  const user = sessionUser();
  if (user?.role === 'student' && attempt.userId !== user.id) {
    fail('Нет доступа к этой попытке');
  }
  return attempt;
}

/**
 * Подпись оператора в истории статусов.
 *
 * Берётся номер рабочего места из учётной записи. Если его нет, показываем
 * фамилию: выдумывать «оп. 0» нельзя — это выглядит как реальные данные.
 */
function currentOperatorLabel(): string {
  const u = sessionUser();
  if (!u) return 'Система';
  return u.operatorNo ?? currentUserName();
}

/**
 * В скольких занятиях используется сценарий. Считает сервер (здесь — mock),
 * интерфейс получает готовые `inUse` / `lessonsCount` и статус не толкует.
 */
function scenarioLessonsCount(id: string): number {
  return db.lessons.filter((l) => l.scenarioIds.includes(id)).length;
}

/** Сценарий в ответе API: с признаком использования и номером версии. */
function withUsage(s: Scenario): Scenario {
  const lessonsCount = scenarioLessonsCount(s.id);
  return { ...clone(s), version: s.version ?? 1, inUse: lessonsCount > 0, lessonsCount };
}

/** Все версии одного сценария: от исходного по ссылкам на родителя. */
function versionChain(s: Scenario): Scenario[] {
  let root = s;
  while (root.parentScenarioId) {
    const parent = scenarioById(root.parentScenarioId);
    if (!parent) break;
    root = parent;
  }
  const chain = [root];
  for (let i = 0; i < chain.length; i++) {
    chain.push(...db.scenarios.filter((x) => x.parentScenarioId === chain[i].id));
  }
  return chain;
}

/**
 * Чего не хватает сценарию для выдачи обучающимся.
 *
 * Проверяется ровно то, без чего попытку невозможно оценить: эталон, по
 * которому сверяется карточка, и легенда, по которой обучающийся её
 * заполняет. Ничего сверх этого не требуем.
 */
function scenarioGaps(s: Scenario): string[] {
  const missing: string[] = [];
  const card = s.etalonDraft;

  if (!s.title.trim()) missing.push('название');
  if (card.incidentTypeIds.length === 0) missing.push('тип происшествия в эталоне');
  if (!card.address.raw.trim()) missing.push('адрес в эталоне');
  if (!card.applicant.name?.trim()) missing.push('ФИО заявителя в эталоне');
  if (!card.applicant.status) missing.push('статус заявителя в эталоне');
  if (!card.description.trim()) missing.push('описание со слов заявителя в эталоне');
  if (s.requiredFields.length === 0) missing.push('обязательные поля');
  if (s.callScript.turns.length === 0) missing.push('реплики заявителя');

  return missing;
}

// ─────────────────────────────────────────── фоновые AI-задачи (ai_jobs)

/** Длительность mock-генерации: очередь + выполнение. На ai-service — минуты. */
const QUEUE_MS = 1500;
const GENERATION_MS = 6000;
const JOBS_KEY = 'arm112.mock.aiJobs';

interface MockJob {
  job: AiJob;
  input: GenerateScenarioInput;
}

/** Задачи хранятся рядом с остальным состоянием mock: опрос переживает F5. */
function readJobs(): Record<string, MockJob> {
  try {
    const raw = sessionStorage.getItem(JOBS_KEY);
    const parsed: unknown = raw ? JSON.parse(raw) : {};
    return parsed && typeof parsed === 'object' ? (parsed as Record<string, MockJob>) : {};
  } catch {
    return {};
  }
}

function writeJobs(jobs: Record<string, MockJob>): void {
  try {
    sessionStorage.setItem(JOBS_KEY, JSON.stringify(jobs));
  } catch {
    // хранилище недоступно: задача живёт до перезагрузки
  }
}

/**
 * Результат генерации: FIXTURE — ближайший готовый сценарий категории
 * (на ai-service — LLM по промпту генерации с брифом и чек-листом).
 */
function completeGeneration(entry: MockJob): void {
  const scenario = entry.job.refId ? scenarioById(entry.job.refId) : undefined;
  if (!scenario || scenario.status !== 'draft') return;
  const done = (s: Scenario) => s.id !== scenario.id && s.status !== 'draft';
  const template =
    db.scenarios.find((s) => done(s) && s.categoryId === entry.input.categoryId) ??
    db.scenarios.find((s) => done(s) && s.status === 'validated') ??
    db.scenarios.find(done);
  if (!template) return;

  const t = clone(template);
  const withDialogue = entry.input.withDialogue !== false;
  Object.assign(scenario, {
    title: `${scenario.categoryName} — учебный сценарий (сгенерирован)`,
    status: 'generated',
    callScript: withDialogue ? t.callScript : { ...t.callScript, dialogue: undefined },
    etalonCard: t.etalonCard,
    etalonDraft: t.etalonDraft,
    requiredFields: t.requiredFields,
    expectedDialogue: withDialogue ? t.expectedDialogue : undefined,
    generationMeta: { model: 'mock-fixture', promptVersion: 'fixture', jobId: entry.job.id },
    notesForTeacher:
      'Черновик от нейросети. Проверьте легенду звонка и состав обязательных полей перед подтверждением.',
  } satisfies Partial<Scenario>);
  persistScenario(scenario);
}

/** Типы событий, означающие содержательное действие обучающегося. */
const USER_INPUT_EVENTS: AttemptEventType[] = [
  'field_changed',
  'choose_value',
  'service_assigned',
  'service_removed',
  'service_status_changed',
];

/** Подпись действия — текущий пользователь сессии, а не фиксированное имя. */
function currentUserName(): string {
  const u = sessionUser();
  if (!u) return 'Система';
  const i = u.firstName ? `${u.firstName[0]}.` : '';
  const m = u.middleName ? `${u.middleName[0]}.` : '';
  return `${u.lastName} ${i}${m}`.trim();
}

/**
 * Счётчик обращений к разговору — на нём держится имитация занятости
 * заявителя (FE-12: примерно каждый восьмой ход отвечает 503). Состояние
 * попытки при этом не меняется: повтор с тем же номером хода проходит.
 */
let dialogueCalls = 0;

/** Миллисекунды от момента приёма вызова — по ним строится транскрипт. */
function atMsOf(attempt: Attempt): number {
  if (!attempt.callAcceptedAt) return 0;
  return Math.max(0, Date.now() - new Date(attempt.callAcceptedAt).getTime());
}

/** Длительность озвучки по длине текста: столько же играла бы запись. */
function speechDurationMs(text: string): number {
  return Math.max(1200, Math.round(text.length * 70));
}

/**
 * Реплика заявителя.
 *
 * Здесь нет языковой модели: ответ выбирается по ключевым словам, а уже
 * раскрытые факты не повторяются. Когда появится ai-service, этот выбор
 * заменяется вызовом `/v1/dialog/turn`, а интерфейс не меняется.
 */
function callerReply(
  scenarioId: string,
  operatorText: string,
  record: { revealedFactIds: string[]; turns: DialogueTurnView[] },
): { text: string; endsCall: boolean; emotionalState?: string; factId?: string } {
  const fixture = dialogueFixture(scenarioId);
  const text = operatorText.toLowerCase();
  const scenario = scenarioById(scenarioId);
  const brief = scenario?.callScript.dialogue;

  /*
   * Бриф, который заполнил преподаватель, важнее готовой фикстуры: заявитель
   * сообщает именно те факты, которые задали в сценарии, и только когда о них
   * спросили. Факты с пометкой «не расскажет» не выдаются никогда.
   */
  if (brief) {
    const fact = brief.facts.find(
      (f) =>
        f.reveal !== 'never' &&
        !record.revealedFactIds.includes(f.id) &&
        (f.hints ?? []).some((hint) => hint.trim() && text.includes(hint.trim().toLowerCase())),
    );
    if (fact) {
      return { text: fact.text, endsCall: false, factId: fact.id };
    }
  }

  const rule = fixture.rules.find((r) => r.match.some((m) => text.includes(m)));
  if (rule) {
    return {
      text: rule.reply,
      endsCall: Boolean(rule.endsCall),
      emotionalState: rule.emotionalState,
      factId: rule.revealsFactId,
    };
  }

  // Ничего по существу не спросили — заявитель торопит, по кругу.
  const asked = record.turns.filter((t) => t.speaker === 'caller').length;
  return { text: fixture.panic[asked % fixture.panic.length], endsCall: false };
}

/** Состояние разговора в том виде, в каком его получает интерфейс. */
function dialogueView(attempt: Attempt): DialogueState {
  const record = dialogueRecord(attempt.id);
  const maxTurns = attempt.voice.maxTurns;
  const used = record.turns.filter((t) => t.speaker === 'operator').length;

  return {
    attemptId: attempt.id,
    turns: clone(record.turns),
    callEnded: record.callEnded,
    callEndedAt: record.callEndedAt,
    endReason: record.endReason,
    input: attempt.voice.input,
    nextTurnNo: record.nextTurnNo,
    turnsLeft: Math.max(0, maxTurns - used),
  };
}

/** Завершение разговора: карточка при этом остаётся доступной для правки. */
function closeDialogue(attempt: Attempt, reason: DialogueState['endReason']): void {
  const record = dialogueRecord(attempt.id);
  if (record.callEnded) return;

  record.callEnded = true;
  record.callEndedAt = new Date().toISOString();
  record.endReason = reason;

  attempt.dialogue = {
    turnsCount: record.turns.length,
    callEnded: true,
    callEndedAt: record.callEndedAt,
  };

  db.events[attempt.id] = [
    ...(db.events[attempt.id] ?? []),
    {
      clientSeq: seqCounter++,
      type: 'dialogue_ended',
      at: record.callEndedAt,
      payload: { reason: reason ?? 'operator_hung_up' },
    },
  ];
}

let seqCounter = 1;
/**
 * Принятые clientSeq по попыткам — для отбрасывания повторов (идемпотентность
 * батча). На сервере это уникальный индекс (attempt_id, client_seq); здесь —
 * запись в sessionStorage, чтобы дедупликация переживала F5 так же, как БД.
 */
function seenSeqs(attemptId: string): Set<number> {
  try {
    const raw = sessionStorage.getItem(`arm112.mock.eventSeen.${attemptId}`);
    return new Set(raw ? (JSON.parse(raw) as number[]) : []);
  } catch {
    return new Set();
  }
}

function saveSeenSeqs(attemptId: string, seen: Set<number>): void {
  try {
    sessionStorage.setItem(`arm112.mock.eventSeen.${attemptId}`, JSON.stringify([...seen]));
  } catch {
    // хранилище недоступно: дедупликация живёт до перезагрузки
  }
}

export const mockApi: Api = {
  auth: {
    async login({ login, password }: LoginInput) {
      const account = DEMO_ACCOUNTS.find((a) => a.login === login && a.password === password);
      if (!account) {
        await delay(null, 400);
        fail('Неверный логин или пароль');
      }
      const user = db.users.find((u) => u.login === account.login);
      if (!user) fail('Учётная запись не найдена');
      // Проверка блокировки — до создания сессии, а не в интерфейсе.
      if (user.status === 'blocked') fail(BLOCKED_MESSAGE);

      sessionStorage.setItem(SESSION_KEY, user.id);
      return delay(clone(user), 350);
    },

    async me() {
      const id = sessionStorage.getItem(SESSION_KEY);
      if (!id) return delay(null, 60);

      const user = db.users.find((u) => u.id === id) ?? null;

      /*
       * Учётную запись могли заблокировать уже после входа: тогда сохранённая
       * сессия перестаёт давать доступ и удаляется, а пользователь получает
       * объяснение, а не молчаливый выход.
       */
      if (user?.status === 'blocked') {
        sessionStorage.removeItem(SESSION_KEY);
        await delay(null, 60);
        fail(BLOCKED_MESSAGE);
      }

      return delay(user ? clone(user) : null, 60);
    },

    async logout() {
      sessionStorage.removeItem(SESSION_KEY);
      return delay(undefined, 60);
    },

    demoAccounts: () =>
      delay(DEMO_ACCOUNTS.map(({ login, password, label }) => ({ login, password, label })), 40),
  },

  classifier: {
    incidentTypes: () => delay(clone(INCIDENT_TYPES)),

    searchTypes: (query) => delay(clone(searchTypes(query)), 90),

    featured: () =>
      delay({
        frequent: FREQUENT_TYPE_IDS.map(typeById).filter((t): t is NonNullable<typeof t> => Boolean(t)),
        significant: SIGNIFICANT_TYPE_IDS.map(typeById).filter((t): t is NonNullable<typeof t> => Boolean(t)),
      }),

    attributes: (typeId) => delay(clone(attributeGroupsFor(typeId)), 160),

    reference: (typeId) =>
      delay({ exists: Boolean(TYPE_REFERENCE[typeId]), text: TYPE_REFERENCE[typeId] }, 140),

    labels: () => {
      const attributes: Record<string, string> = {};
      const values: Record<string, Record<string, string>> = {};

      for (const groups of Object.values(ATTRIBUTE_GROUPS)) {
        for (const g of groups) {
          attributes[g.code] = g.label;
          if (g.options) {
            values[g.code] = Object.fromEntries(g.options.map((o) => [o.code, o.label]));
          }
        }
      }

      const types = Object.fromEntries(INCIDENT_TYPES.map((t) => [t.id, t.name]));
      return delay({ fields: { ...FIELD_LABELS }, attributes, values, types }, 120);
    },

    async resolveServices({ typeIds, attributes, addressFilled, addressSource, current }): Promise<ResolvedServicesResult> {
      const suppressed = addressFilled && addressSource === 'fias';
      const resolved = resolveServices(typeIds, attributes, addressFilled, addressSource);

      // Ручные службы оператор добавил осознанно — автоопределение их не трогает.
      const manual = current.filter((s) => s.source === 'manual');
      const next: AssignedService[] = [];

      for (const item of resolved) {
        const existing = current.find((s) => s.code === item.code);
        next.push(buildService(item.code, 'auto', item.isPrimary, item.reason, existing));
      }
      for (const m of manual) {
        if (!next.some((s) => s.code === m.code)) next.push(m);
      }

      return delay({ services: next, suppressedByAddressSource: suppressed }, 260);
    },
  },

  services: {
    list: () => delay(clone(SERVICES)),
  },

  users: {
    list: () => delay(clone(db.users), 160),

    async setBlocked(userId, blocked) {
      const actor = sessionUser();
      if (actor?.role !== 'admin') fail('Недостаточно прав для этой операции');
      if (actor.id === userId) fail('Нельзя заблокировать текущую учётную запись');

      const user = setUserBlocked(userId, blocked);
      return delay(clone(user), 220);
    },

    async progress(userId) {
      const actor = requireRole('teacher', 'student', 'admin');
      if (actor.role === 'student' && actor.id !== userId) {
        throw new ApiRequestError('forbidden', 'Прогресс другого обучающегося недоступен', { status: 403 });
      }
      if (!db.users.some((u) => u.id === userId && u.role === 'student')) {
        throw new ApiRequestError('not_found', 'Обучающийся не найден', { status: 404 });
      }
      return delay(buildProgress(userId), 260);
    },

    /** PDF формирует сервер; без него — HTML той же формы, чтобы проверить сценарий выдачи. */
    async certificate(userId) {
      const actor = requireRole('teacher', 'student');
      if (actor.role === 'student' && actor.id !== userId) {
        throw new ApiRequestError('forbidden', 'Сертификат другого обучающегося недоступен', { status: 403 });
      }
      if (evaluatedCount(userId) === 0) {
        throw new ApiRequestError('conflict', 'Сертификат выдаётся после первой оценённой карточки', { status: 409 });
      }
      const html = certificateHtml(buildProgress(userId));
      return delay({ blob: new Blob([html], { type: 'text/html;charset=utf-8' }), fileName: 'Сертификат (демонстрационный).html' }, 400);
    },
  },

  address: {
    suggest: (query) => delay(suggestAddresses(query), 180),
  },

  aiJobs: {
    async get(jobId) {
      const jobs = readJobs();
      const entry = jobs[jobId];
      if (!entry) throw new ApiRequestError('not_found', 'Задача не найдена');

      const { job } = entry;
      if (job.status === 'queued' || job.status === 'running') {
        const elapsed = Date.now() - Date.parse(job.createdAt);
        if (elapsed < QUEUE_MS) {
          job.status = 'queued';
          job.queuePosition = 1;
        } else if (elapsed < GENERATION_MS) {
          job.status = 'running';
          job.queuePosition = undefined;
          job.tryCount = 1;
        } else {
          completeGeneration(entry);
          job.status = 'done';
          job.finishedAt = new Date().toISOString();
        }
        job.estWaitSec = Math.max(0, Math.ceil((GENERATION_MS - elapsed) / 1000));
        writeJobs(jobs);
      }
      return delay(clone(job), 150);
    },
  },

  scenarios: {
    list: () => delay(db.scenarios.map(withUsage)),

    async get(id) {
      const s = scenarioById(id);
      if (!s) fail('Сценарий не найден');
      return delay(withUsage(s));
    },

    async create(input: CreateScenarioInput) {
      const type = typeById(input.categoryId);
      const scenario: Scenario = {
        id: uid('sc'),
        title: input.title,
        categoryId: input.categoryId,
        categoryName: type?.name ?? input.categoryId,
        difficulty: input.difficulty,
        mode: input.mode,
        source: 'manual',
        status: 'draft',
        etalonVersion: 1,
        createdAt: new Date().toISOString(),
        callScript: { caller: {}, address: {}, keyFacts: [], turns: [] },
        etalonCard: {},
        etalonDraft: emptyCard(),
        requiredFields: ['applicant.name', 'applicant.status', 'address.raw', 'incidentTypeIds', 'description'],
      };
      db.scenarios.unshift(scenario);
      persistScenario(scenario);
      return delay(clone(scenario), 300);
    },

    /**
     * Контракт generateScenario: 202 {jobId, scenarioId}. Сразу создаётся
     * scenarios(status=draft, source=generated) и задача generate_scenario;
     * фронт опрашивает GET /ai-jobs/{jobId}. В проде задача идёт через
     * ai-service минуты, здесь — секунды (см. aiJobs.get).
     */
    async generate(input: GenerateScenarioInput) {
      const type = typeById(input.categoryId);
      const now = new Date().toISOString();
      const scenario: Scenario = {
        id: uid('sc'),
        title: `${type?.name ?? 'Происшествие'} — генерируется…`,
        categoryId: input.categoryId,
        categoryName: type?.name ?? input.categoryId,
        difficulty: input.difficulty,
        mode: input.mode,
        source: 'generated',
        status: 'draft',
        etalonVersion: 1,
        createdAt: now,
        callScript: { caller: {}, address: {}, keyFacts: [], turns: [] },
        etalonCard: {},
        etalonDraft: emptyCard(),
        requiredFields: ['applicant.name', 'applicant.status', 'address.raw', 'incidentTypeIds', 'description'],
        teacherComment: input.teacherComment,
      };
      db.scenarios.unshift(scenario);
      persistScenario(scenario);

      const job: AiJob = {
        id: uid('job'),
        type: 'generate_scenario',
        status: 'queued',
        refType: 'scenario',
        refId: scenario.id,
        tryCount: 0,
        queuePosition: 1,
        estWaitSec: Math.ceil(GENERATION_MS / 1000),
        createdAt: now,
      };
      const jobs = readJobs();
      jobs[job.id] = { job, input };
      writeJobs(jobs);
      return delay({ jobId: job.id, scenarioId: scenario.id, estWaitSec: job.estWaitSec }, 300);
    },

    /*
     * Решение backend: запрет правки определяется использованием в занятиях,
     * а не статусом. Подтверждённый, но ещё не выданный сценарий правится.
     */
    async update(id, patch) {
      const s = scenarioById(id);
      if (!s) fail('Сценарий не найден');
      const lessonsCount = scenarioLessonsCount(id);
      if (lessonsCount > 0) {
        throw new ApiRequestError(
          'conflict',
          `Сценарий используется в занятиях (${lessonsCount}), изменить нельзя — создайте новую версию`,
          { status: 409, details: { lessonsCount } },
        );
      }
      // Служебные поля версии и использования правкой не меняются.
      const { inUse: _inUse, lessonsCount: _count, version: _version, parentScenarioId: _parent, ...editable } = patch;
      Object.assign(s, editable);
      // Правка эталона — новая версия: завершённые попытки ссылаются на свою.
      if (patch.etalonDraft || patch.requiredFields) s.etalonVersion += 1;
      persistScenario(s);
      return delay(withUsage(s), 250);
    },

    /**
     * POST /scenarios/{id}/versions (v1.2): копия в статусе
     * draft со ссылкой на родителя. Родитель и занятия на нём не меняются.
     */
    async createVersion(id) {
      const parent = scenarioById(id);
      if (!parent) fail('Сценарий не найден');
      const chain = versionChain(parent);
      const copy: Scenario = {
        ...clone(parent),
        id: uid('sc'),
        status: 'draft',
        version: Math.max(...chain.map((x) => x.version ?? 1)) + 1,
        parentScenarioId: parent.id,
        authorId: sessionUser()?.id ?? parent.authorId,
        createdAt: new Date().toISOString(),
        etalonVersion: 1,
        ttsReady: false,
      };
      delete copy.validatedBy;
      delete copy.validatedAt;
      delete copy.inUse;
      delete copy.lessonsCount;
      db.scenarios.unshift(copy);
      persistScenario(copy);
      return delay(withUsage(copy), 300);
    },

    async approve(id) {
      const s = scenarioById(id);
      if (!s) fail('Сценарий не найден');

      const missing = scenarioGaps(s);
      if (missing.length > 0) {
        throw new ApiRequestError(
          'validation',
          `Сценарий нельзя подтвердить: не заполнено — ${missing.join(', ')}`,
          { details: { fields: missing } },
        );
      }

      s.status = 'validated';
      s.validatedBy = currentUserName();
      s.validatedAt = new Date().toISOString();
      persistScenario(s);
      return delay(withUsage(s), 300);
    },

    async reject(id, reason) {
      const s = scenarioById(id);
      if (!s) fail('Сценарий не найден');
      s.status = 'rejected';
      s.teacherComment = reason;
      persistScenario(s);
      return delay(withUsage(s), 250);
    },

    // Синтеза речи без ai-service нет — отвечаем как сервер при недоступном TTS.
    async ttsPreview(id) {
      requireRole('teacher');
      if (!scenarioById(id)) fail('Сценарий не найден');
      await delay(null, 600);
      throw new ApiRequestError('ai_unavailable', 'Синтез речи недоступен: сервис ИИ не подключён', { status: 503 });
    },

    async exportBundle(ids) {
      requireRole('teacher', 'admin');
      return delay(exportBundle(ids), 300);
    },

    async importBundle(bundle) {
      const actor = requireRole('teacher', 'admin');
      return delay(importBundle(bundle, actor), 500);
    },

    async fromAttempt(attemptId, title) {
      const actor = requireRole('teacher');
      return delay(withUsage(scenarioFromAttempt(attemptId, title, actor)), 350);
    },
  },

  lessons: {
    defaultSettings: () => delay(makeLessonSettings(), 60),

    list: () => delay(clone(db.lessons)),

    async get(id) {
      const l = lessonById(id);
      if (!l) fail('Занятие не найдено');
      return delay(clone(l));
    },

    async create(input: CreateLessonInput) {
      // Автор занятия — текущий пользователь сессии: занятие не должно
      // технически принадлежать другому преподавателю.
      const author = sessionUser();
      if (!author) fail('Сессия не найдена: войдите заново');

      /*
       * Голосовое занятие невозможно провести по сценарию без брифа заявителя
       * и чек-листа протокола: заявителю нечего отвечать, а разговор нечем
       * оценивать. Контракт отвечает на это 422 — здесь та же проверка.
       */
      // В занятие попадают только подтверждённые сценарии (версия-черновик — нет).
      const unapproved = input.scenarioIds.filter((id) => scenarioById(id)?.status !== 'validated');
      if (unapproved.length > 0) {
        throw new ApiRequestError('validation', 'В занятие можно включить только подтверждённые сценарии', {
          details: { scenarioIds: unapproved },
        });
      }

      /*
       * v1.4: источник карточек пула. «Сформированные обучающимися» — только
       * сценарии source=student, «сгенерированные системой» — все остальные.
       */
      const source = input.cardSource ?? 'mixed';
      if (source !== 'mixed') {
        const mismatched = input.scenarioIds.filter((id) => {
          const own = scenarioById(id)?.source === 'student';
          return source === 'student' ? !own : own;
        });
        if (mismatched.length > 0) {
          throw new ApiRequestError(
            'validation',
            source === 'student'
              ? 'В пул «карточки обучающихся» можно включить только сценарии из карточек обучающихся'
              : 'В пул «сгенерированные системой» нельзя включать сценарии из карточек обучающихся',
            { status: 422, details: { scenarioIds: mismatched } },
          );
        }
      }

      if (input.voice?.enabled) {
        const notReady = input.scenarioIds
          .map((id) => scenarioById(id))
          .filter((sc) => sc && (!sc.callScript.dialogue || !sc.expectedDialogue?.checklist.length))
          .map((sc) => sc?.title ?? '');
        if (notReady.length > 0) {
          throw new ApiRequestError(
            'validation',
            `Для голосового занятия нужны бриф заявителя и чек-лист разговора. Не заполнены: ${notReady.join(', ')}`,
            { details: { scenarios: notReady } },
          );
        }
      }

      /*
       * Участник должен существовать: иначе занятие выдаст попытку
       * несуществующему пользователю, и обучающийся её просто не увидит.
       */
      const unknown = input.participantIds.filter((id) => !db.users.some((u) => u.id === id));
      if (unknown.length > 0) {
        throw new ApiRequestError('validation', `Неизвестные участники: ${unknown.join(', ')}`, {
          details: { participantIds: unknown },
        });
      }

      const lesson: Lesson = {
        id: uid('ls'),
        kind: 'class',
        title: input.title,
        teacherId: author.id,
        mode: input.mode,
        perspective: input.perspective,
        difficulty: input.difficulty,
        timeLimitSec: input.timeLimitSec,
        status: 'draft',
        scenarioIds: input.scenarioIds,
        participants: input.participantIds.map((userId) => {
          const u = db.users.find((x) => x.id === userId);
          return {
            userId,
            name: u ? `${u.lastName} ${u.firstName[0]}. ${u.middleName?.[0] ?? ''}.`.trim() : userId,
            status: 'assigned' as const,
          };
        }),
        settings: (() => {
          const base = makeLessonSettings();
          const voice = input.voice ? { ...base.voice, ...input.voice } : base.voice;
          return {
            ...base,
            passThreshold: input.passThreshold,
            allowReplay: input.allowReplay,
            voice,
            cardSource: input.cardSource,
            // Вес разговора имеет смысл только при включённом голосе;
            // иначе его доля перераспределяется между остальными слоями.
            weights: normalizeWeights(
              input.weights ?? (voice.enabled ? withDialogueWeight(base.weights) : base.weights),
              voice.enabled,
            ),
          };
        })(),
        createdAt: new Date().toISOString(),
      };
      db.lessons.unshift(lesson);
      // Созданное занятие должно пережить перезагрузку страницы: иначе вместе
      // с ним исчезнет и попытка обучающегося, состояние которой сохранено.
      persistLesson(lesson);
      return delay(clone(lesson), 350);
    },

    async start(id) {
      const lesson = lessonById(id);
      if (!lesson) fail('Занятие не найдено');
      if (lesson.scenarioIds.length === 0) fail('В занятии нет ни одного сценария');
      lesson.status = 'running';
      lesson.startedAt = new Date().toISOString();
      issueAttempts(lesson);
      persistLesson(lesson);
      return delay(clone(lesson), 400);
    },

    async finish(id) {
      const lesson = lessonById(id);
      if (!lesson) fail('Занятие не найдено');
      lesson.status = 'finished';
      lesson.finishedAt = new Date().toISOString();
      for (const p of lesson.participants) {
        if (p.status !== 'finished') p.status = 'finished';
      }
      // Контракт finishLesson: незавершённые попытки -> expired. Иначе
      // обучающийся видел бы завершённое занятие с поступившим вызовом.
      for (const attempt of db.attempts) {
        if (attempt.lessonId !== lesson.id) continue;
        if (attempt.status !== 'issued' && attempt.status !== 'in_progress') continue;
        attempt.status = 'expired';
        persistAttemptRuntime(attempt);
        persistCompletedAttempt(attempt);
      }
      persistLesson(lesson);
      return delay(clone(lesson), 300);
    },

    async assigned() {
      const userId = sessionUser()?.id;
      if (!userId) throw new ApiRequestError('unauthorized', 'Сессия истекла — войдите снова');
      const out = db.lessons
        .filter((l) => l.participants.some((p) => p.userId === userId))
        .map((lesson) => {
          const attempt = db.attempts.find((a) => a.lessonId === lesson.id && a.userId === userId);
          return { lesson: clone(lesson), attempt: attempt ? clone(attempt) : undefined };
        });
      return delay(out, 240);
    },
  },

  attempts: {
    async get(id) {
      const a = requireAttempt(id);
      a.serverNow = new Date().toISOString();
      return delay(clone(a));
    },

    forLesson: (lessonId) => delay(clone(db.attempts.filter((a) => a.lessonId === lessonId))),

    async acceptCall(id): Promise<AcceptCallResult> {
      const attempt = requireAttempt(id);
      // Контракт acceptCall: 409, если попытка не в issued/in_progress.
      if (attempt.status !== 'issued' && attempt.status !== 'in_progress') {
        throw new ApiRequestError('conflict', 'Попытка закрыта — принять вызов нельзя');
      }
      if (!attempt.callAcceptedAt) {
        const now = new Date().toISOString();
        attempt.callAcceptedAt = now;
        attempt.status = 'in_progress';
        attempt.incidentNo = attempt.incidentNo || nextIncidentNo();

        const scenario = scenarioById(attempt.scenarioId);
        const draft = db.drafts[attempt.id] ?? emptyCard();
        // Инструкция п.4.1: при приёме вызова поле АОН заполняется автоматически.
        draft.phones.aon = scenario?.callScript.caller.phone ?? '';
        db.drafts[attempt.id] = draft;

        db.events[attempt.id] = [
          ...(db.events[attempt.id] ?? []),
          { clientSeq: seqCounter++, type: 'call_accepted', at: now },
          { clientSeq: seqCounter++, type: 'open_card', at: now },
        ];

        const lesson = lessonById(attempt.lessonId);
        const participant = lesson?.participants.find((p) => p.userId === attempt.userId);
        if (participant) {
          participant.status = 'active';
          participant.joinedAt = now;
          participant.attemptId = attempt.id;
        }
      }
      attempt.serverNow = new Date().toISOString();

      /*
       * Вступительная реплика отдаётся вместе с попыткой: браузер разрешает
       * воспроизведение только в ответ на действие пользователя, а таким
       * действием является само нажатие «Принять».
       */
      const record = dialogueRecord(attempt.id);
      let opening = record.turns.find((t) => t.turnNo === 0);

      if (!opening && attempt.voice.enabled) {
        const fixture = dialogueFixture(attempt.scenarioId);
        const scenario = scenarioById(attempt.scenarioId);
        // Первая реплика заявителя из сценария; фикстура — только запасной вариант.
        const scripted = scenario?.callScript.turns.find((t) => t.speaker === 'caller')?.text;
        opening = {
          turnNo: 0,
          speaker: 'caller',
          text: scripted ?? fixture.opening,
          atMs: 0,
          at: attempt.callAcceptedAt,
          durationMs: speechDurationMs(fixture.opening),
          source: 'script',
          emotionalState: scenario?.callScript.caller.emotionalState,
        };
        record.turns.push(opening);
        attempt.dialogue = { turnsCount: record.turns.length, callEnded: false };
      }

      persistAttemptRuntime(attempt);
      return delay({ attempt: clone(attempt), opening: opening ? clone(opening) : undefined }, 260);
    },

    async getDraft(id) {
      requireAttempt(id);
      return delay(clone(db.drafts[id] ?? emptyCard()), 160);
    },

    async updateDraft(id, card) {
      const attempt = requireAttempt(id);
      db.drafts[id] = clone(card);
      persistAttemptRuntime(attempt);
      return delay({ savedAt: new Date().toISOString() }, 200);
    },

    async postEvents(id, events) {
      const attempt = requireAttempt(id);
      if (events.length === 0 || events.length > 200) {
        throw new ApiRequestError('validation', 'В пачке должно быть от 1 до 200 событий');
      }
      // Контракт: дубли по clientSeq отбрасываются молча — повтор после обрыва безопасен.
      const seen = seenSeqs(id);
      let accepted = 0;
      for (const event of events) {
        if (seen.has(event.clientSeq)) continue;
        seen.add(event.clientSeq);
        accepted += 1;
        /*
         * Время первого ввода берётся из первого содержательного действия
         * обучающегося, а не из автосохранения: autosave срабатывает с задержкой
         * и после отправки карточки, и по нему время реакции было бы завышено.
         */
        if (!attempt.firstInputAt && USER_INPUT_EVENTS.includes(event.type)) {
          attempt.firstInputAt = event.at;
        }
        db.events[id] = [...(db.events[id] ?? []), clone(event)];
      }
      saveSeenSeqs(id, seen);
      persistAttemptRuntime(attempt);
      return delay({ accepted, lastSeq: Math.max(0, ...seen) }, 20);
    },

    async events(id) {
      requireAttempt(id);
      // id присваивает сервер: порядковый номер сохранённого события попытки.
      const stored: AttemptEvent[] = (db.events[id] ?? []).map((e, i) => ({ ...clone(e), id: i + 1 }));
      return delay(stored, 120);
    },

    async changeServiceStatus({ attemptId, serviceId, status, squadNumber, comment }: ChangeStatusInput) {
      const attempt = requireAttempt(attemptId);
      const draft = db.drafts[attemptId];
      if (!draft) fail('Черновик карточки не найден');

      const service = draft.services.find((s) => s.serviceId === serviceId);
      if (!service) fail('Служба не назначена на карточку');

      const allowed = service.allowedNext.find((t) => t.status === status);
      if (!allowed) fail(`Переход в статус «${status}» сейчас недопустим`);
      if (allowed.commentRequired && !comment?.trim()) {
        fail(`Для статуса «${status}» комментарий обязателен`);
      }

      const now = new Date().toISOString();
      service.currentStatus = status;
      service.currentStatusAt = now;
      service.history.push({ status, at: now, operator: currentOperatorLabel(), squadNumber, comment });
      service.allowedNext = computeAllowedNext(status, service.code);
      service.editable = !isTerminal(status);

      db.events[attemptId] = [
        ...(db.events[attemptId] ?? []),
        {
          clientSeq: seqCounter++,
          type: 'service_status_changed',
          at: now,
          payload: { service: service.shortName, status, hasComment: Boolean(comment) },
        },
      ];
      // Статус — действие оператора: сохраняем сразу, а не с ближайшим
      // автосохранением карточки, иначе F5 в эту секунду его терял.
      persistAttemptRuntime(attempt);

      return delay(clone(service), 240);
    },

    async addService(id, serviceCode) {
      const attempt = requireAttempt(id);
      const draft = db.drafts[id];
      if (!draft) fail('Черновик карточки не найден');
      if (draft.services.some((s) => s.code === serviceCode)) fail('Служба уже назначена');

      const service = buildService(serviceCode, 'manual', false, 'добавлена оператором вручную');
      draft.services.push(service);

      db.events[id] = [
        ...(db.events[id] ?? []),
        {
          clientSeq: seqCounter++,
          type: 'service_assigned',
          at: new Date().toISOString(),
          payload: { service: service.shortName, source: 'manual' },
        },
      ];
      persistAttemptRuntime(attempt);
      return delay(clone(service), 240);
    },

    async removeService(id, serviceId) {
      const attempt = requireAttempt(id);
      const draft = db.drafts[id];
      if (!draft) fail('Черновик карточки не найден');

      const service = draft.services.find((s) => s.serviceId === serviceId);
      if (!service) fail('Служба не назначена на карточку');
      if (service.currentStatus !== 'Получена службой' && service.currentStatus !== 'Добавлена') {
        fail('Служба уже приступила к реагированию — снять её нельзя');
      }

      draft.services = draft.services.filter((s) => s.serviceId !== serviceId);

      // Ручное снятие — нештатное действие, интерфейс обещает его фиксацию.
      db.events[id] = [
        ...(db.events[id] ?? []),
        {
          clientSeq: seqCounter++,
          type: 'service_removed',
          at: new Date().toISOString(),
          payload: { service: service.shortName },
        },
      ];
      persistAttemptRuntime(attempt);
      return delay(undefined, 180);
    },

    async replay(id) {
      const attempt = requireAttempt(id);
      attempt.replayCount += 1;
      persistAttemptRuntime(attempt);
      db.events[id] = [
        ...(db.events[id] ?? []),
        { clientSeq: seqCounter++, type: 'replay', at: new Date().toISOString() },
      ];
      return delay({ replayCount: attempt.replayCount }, 120);
    },

    async submit(id, card) {
      const attempt = requireAttempt(id);
      const scenario = scenarioById(attempt.scenarioId);
      const lesson = lessonById(attempt.lessonId);
      if (!scenario || !lesson) fail('Сценарий или занятие не найдены');
      // Контракт submitAttempt: 409, если попытка не в in_progress — например,
      // преподаватель завершил занятие, пока карточка была открыта.
      if (attempt.status !== 'in_progress') {
        throw new ApiRequestError('conflict', 'Попытка уже закрыта — карточку сохранить нельзя');
      }

      const now = new Date();
      db.drafts[id] = clone(card);
      attempt.card = clone(card);
      attempt.submittedAt = now.toISOString();
      attempt.status = 'evaluating';

      // Сохранение карточки закрывает разговор — оценивать будем весь транскрипт.
      if (attempt.voice.enabled) closeDialogue(attempt, 'submitted');
      // Попытка завершена: хранить её состояние между перезагрузками больше незачем.
      clearAttemptRuntime(attempt.id);
      attempt.timeSpentMs = attempt.callAcceptedAt
        ? now.getTime() - new Date(attempt.callAcceptedAt).getTime()
        : 0;

      db.events[id] = [
        ...(db.events[id] ?? []),
        { clientSeq: seqCounter++, type: 'save', at: now.toISOString() },
        { clientSeq: seqCounter++, type: 'submitted', at: now.toISOString() },
      ];

      // Слои 1 и 4 считаются мгновенно → partial (docs/contracts.md).
      const partial = evaluatePartial(attempt, card, scenario, lesson);
      db.evaluations[id] = partial;
      persistCompletedAttempt(attempt);

      // Слои 2 и 3 доезжают callback'ом от ai-service — здесь имитируем задержку.
      setTimeout(() => {
        const current = db.evaluations[id];
        if (!current) return;
        db.evaluations[id] = evaluateComplete(current, card, scenario, lesson);
        const a = attemptById(id);
        if (a) a.status = 'evaluated';
        const participant = lesson.participants.find((p) => p.userId === attempt.userId);
        if (participant) {
          participant.status = 'finished';
          participant.finishedAt = new Date().toISOString();
        }
        if (a) persistCompletedAttempt(a);
        persistLesson(lesson);
      }, 4200);

      return delay(clone(attempt), 420);
    },
  },

  evaluation: {
    async get(attemptId) {
      requireAttempt(attemptId);
      const ev = db.evaluations[attemptId];
      return delay(ev ? clone(ev) : null, 160);
    },

    async override(attemptId, { score, reason }) {
      const by = currentUserName();
      const ev = db.evaluations[attemptId];
      if (!ev) fail('Оценка ещё не сформирована');
      const attempt = attemptById(attemptId);
      const lesson = attempt ? lessonById(attempt.lessonId) : undefined;

      ev.override = { score, verdict: score >= (lesson?.settings.passThreshold ?? 70) ? 'pass' : 'fail', reason, by, at: new Date().toISOString() };
      ev.finalScore = score;
      ev.verdict = ev.override.verdict;
      if (attempt) persistCompletedAttempt(attempt);
      return delay(clone(ev), 300);
    },
  },

  callScript: {
    async get(attemptId) {
      const attempt = requireAttempt(attemptId);
      const scenario = scenarioById(attempt.scenarioId);
      if (!scenario) fail('Сценарий не найден');
      const lesson = lessonById(attempt.lessonId);

      // GAP-11: keyFacts — источник required_facts эталона, студенту не отдаём.
      const view: StudentCallScript = {
        caller: {
          name: scenario.callScript.caller.name,
          phone: scenario.callScript.caller.phone,
          role: scenario.callScript.caller.role,
          emotionalState: scenario.callScript.caller.emotionalState,
        },
        turns: scenario.callScript.turns.map((turn, index) => ({
          index,
          speaker: turn.speaker,
          text: turn.text,
          // TODO(backend) GAP-10/GAP-15: контракт даёт только ttsHash, не URL.
          audioUrl: undefined,
          durationMs: Math.max(2200, turn.text.length * 70),
        })),
        allowReplay: lesson?.settings.allowReplay ?? true,
        voice: { ...attempt.voice },
      };
      return delay(view, 260);
    },
  },

  dialogue: {
    async get(attemptId) {
      const attempt = requireAttempt(attemptId);
      return delay(dialogueView(attempt), 140);
    },

    async turn({ attemptId, turnNo, text, audio }: DialogueTurnInput): Promise<DialogueTurnResponse> {
      const attempt = requireAttempt(attemptId);
      if (attempt.status !== 'in_progress') {
        throw new ApiRequestError('conflict', 'Попытка не в работе — разговор недоступен');
      }

      const record = dialogueRecord(attemptId);
      if (record.callEnded) {
        throw new ApiRequestError('conflict', 'Разговор уже завершён');
      }

      /*
       * Повтор того же хода после обрыва связи не должен проводить его дважды:
       * отдаём сохранённый ответ. Более старый номер — рассинхронизация клиента.
       */
      if (turnNo < record.nextTurnNo) {
        if (turnNo === record.nextTurnNo - 1 && record.lastResponse) {
          return delay(clone(record.lastResponse), 120);
        }
        throw new ApiRequestError('conflict', 'Этот ход уже обработан', {
          details: { nextTurnNo: record.nextTurnNo },
        });
      }

      // FIXTURE: имитация занятой AI-полосы. Состояние попытки не меняется,
      // повтор с тем же номером хода проходит штатно.
      dialogueCalls += 1;
      if (dialogueCalls % 8 === 0) {
        throw new ApiRequestError('caller_busy', 'Заявитель не отвечает, попробуйте ещё раз', {
          retryAfterSec: 3,
        });
      }

      const spoken = (text ?? '').trim();

      /*
       * Распознавания речи в mock-слое нет. Запись без текста — это честный
       * «не распознал»: выдумывать за обучающегося реплику нельзя, иначе
       * транскрипт и оценка разговора станут фикцией.
       */
      if (!spoken) {
        const response: DialogueTurnResponse = {
          turnNo,
          noSpeech: true,
          callEnded: false,
          // fallback означает «ответ взят из сценария вместо модели»,
          // а здесь просто нечего распознавать.
          fallback: false,
          nextTurnNo: record.nextTurnNo,
          latencyMs: 300,
        };
        return delay(response, 400);
      }

      const now = new Date();
      const operatorTurn: DialogueTurnView = {
        turnNo,
        speaker: 'operator',
        text: spoken,
        atMs: atMsOf(attempt),
        at: now.toISOString(),
        source: audio ? 'stt' : 'text',
        confidence: audio ? 0.72 : undefined,
        // Запись хранится в памяти вкладки; на сервере это файл из /media.
        audio: audio
          ? { audioUrl: URL.createObjectURL(audio), durationMs: 0, mime: audio.type || undefined }
          : undefined,
      };
      record.turns.push(operatorTurn);

      const reply = callerReply(attempt.scenarioId, spoken, record);
      if (reply.factId && !record.revealedFactIds.includes(reply.factId)) {
        record.revealedFactIds.push(reply.factId);
      }

      const callerTurn: DialogueTurnView = {
        turnNo,
        speaker: 'caller',
        text: reply.text,
        atMs: operatorTurn.atMs + 900,
        at: new Date(now.getTime() + 900).toISOString(),
        durationMs: speechDurationMs(reply.text),
        source: 'llm',
        emotionalState: reply.emotionalState,
      };
      record.turns.push(callerTurn);
      record.nextTurnNo = turnNo + 1;

      const used = record.turns.filter((t) => t.speaker === 'operator').length;
      const limitReached = used >= attempt.voice.maxTurns;
      if (reply.endsCall) closeDialogue(attempt, 'caller_hung_up');
      else if (limitReached) closeDialogue(attempt, 'max_turns');

      attempt.dialogue = {
        turnsCount: record.turns.length,
        callEnded: record.callEnded,
        callEndedAt: record.callEndedAt,
      };

      db.events[attemptId] = [
        ...(db.events[attemptId] ?? []),
        { clientSeq: seqCounter++, type: 'dialogue_operator', at: operatorTurn.at ?? now.toISOString(), payload: { turnNo } },
        { clientSeq: seqCounter++, type: 'dialogue_caller', at: callerTurn.at ?? now.toISOString(), payload: { turnNo } },
      ];

      const response: DialogueTurnResponse = {
        turnNo,
        operator: clone(operatorTurn),
        caller: clone(callerTurn),
        noSpeech: false,
        callEnded: record.callEnded,
        endReason: record.endReason,
        fallback: false,
        nextTurnNo: record.nextTurnNo,
        latencyMs: 1400,
      };
      record.lastResponse = response;
      persistAttemptRuntime(attempt);

      // Задержка близка к бюджету настоящего хода (STT + модель + озвучка).
      return delay(clone(response), 1400);
    },

    async end(attemptId) {
      const attempt = requireAttempt(attemptId);
      closeDialogue(attempt, 'operator_hung_up');
      persistAttemptRuntime(attempt);
      return delay(dialogueView(attempt), 200);
    },
  },

  feedback: {
    async list(attemptId) {
      requireAttempt(attemptId);
      return delay(clone(db.feedback.filter((f) => f.attemptId === attemptId)), 140);
    },

    async add(attemptId, { field, comment, recommendation }) {
      const item: TeacherFeedback = {
        id: uid('fb'),
        attemptId,
        field,
        teacherName: currentUserName(),
        comment,
        recommendation: recommendation || undefined,
        createdAt: new Date().toISOString(),
      };
      db.feedback.push(item);
      persistFeedback();
      return delay(clone(item), 240);
    },
  },

  reaction: {
    allowedNext: (current: ReactionStatus, serviceCode: string) =>
      delay(computeAllowedNext(current, serviceCode), 60),
  },

  analytics: {
    async overview(filter) {
      const actor = requireRole('teacher', 'admin');
      if (filter.lessonId && !lessonById(filter.lessonId)) {
        throw new ApiRequestError('not_found', 'Занятие не найдено', { status: 404 });
      }
      return delay(buildOverview(filter, actor.role === 'teacher' ? actor.id : undefined), 450);
    },
  },

  materials: {
    list: (f) => delay(listMaterials(f), 200),
    get: (id) => delay(null, 150).then(() => getMaterial(id)),
    async create(input) {
      const actor = requireRole('teacher', 'admin');
      const created = await createMaterial(input, actor);
      return delay(created, 300);
    },
    async remove(id) {
      const actor = requireRole('teacher', 'admin');
      removeMaterial(id, actor);
      return delay(undefined, 200);
    },
    fileUrl: (m) => materialFileUrl(m.id),
  },

  realtime: {
    // Буфера пропущенных сообщений у mock нет: каждое подключение начинается со snapshot.
    lessonMonitor: (lessonId, _since, handlers) => mockLessonMonitor(lessonId, handlers),
    attempt: (attemptId, handlers) => mockAttemptChannel(attemptId, handlers),
  },

  admin: {
    health: () => delay(null, 200).then(() => {
      requireRole('admin');
      return mockHealth();
    }),
    settings: () => delay(null, 180).then(() => {
      requireRole('admin');
      return listSettings();
    }),
    updateSetting: (key, value) => delay(null, 250).then(() => {
      requireRole('admin');
      return updateSetting(key, value);
    }),
    audit: (f) => delay(null, 220).then(() => {
      requireRole('admin');
      return listAudit(f);
    }),
    backups: () => delay(null, 180).then(() => {
      requireRole('admin');
      return listBackups();
    }),
    runBackup: () => delay(null, 300).then(() => runBackup(requireRole('admin').id)),
    logs: (f) => delay(null, 200).then(() => {
      requireRole('admin');
      return listLogs(f);
    }),
  },
};

export type { Attempt, Evaluation, IncidentCardDraft, Lesson, Scenario, User, AttemptEvent };
