/**
 * Mock-реализация сервисного слоя (`shared/api/types.ts`).
 *
 * Здесь НЕТ выдуманных backend endpoint'ов — только доменные операции поверх
 * in-memory состояния. Все задержки имитируют сетевые, чтобы UI сразу
 * проектировался с loading-состояниями.
 */

import type { Api, ChangeStatusInput, CreateLessonInput, CreateScenarioInput, GenerateScenarioInput, LoginInput, ResolvedServicesResult } from '../api/types';
import type {
  AssignedService, Attempt, AttemptEvent, AttemptEventType, Evaluation, IncidentCardDraft,
  Lesson, ReactionStatus, ReactionStatusEntry, Scenario, StudentCallScript, TeacherFeedback, User,
} from '../types';
import { computeAllowedNext, isTerminal } from './reactionTransitions';
import {
  attemptById, db, evaluateComplete, evaluatePartial, issueAttempts,
  lessonById, makeLessonSettings, nextIncidentNo, scenarioById, setUserBlocked,
} from './db';
import { ATTRIBUTE_GROUPS, INCIDENT_TYPES, FREQUENT_TYPE_IDS, SIGNIFICANT_TYPE_IDS, TYPE_REFERENCE, attributeGroupsFor, resolveServices, searchTypes, typeById } from './fixtures/classifier';
import { SERVICES, serviceByCode } from './fixtures/services';
import { suggestAddresses } from './fixtures/addresses';
import { DEMO_ACCOUNTS } from './fixtures/users';
import { FIELD_LABELS, emptyCard } from '../utils/card';
import { uid } from '../utils/id';

const SESSION_KEY = 'arm112.session';

/** Единая формулировка отказа: и при входе, и при восстановлении сессии. */
const BLOCKED_MESSAGE = 'Учётная запись заблокирована.';

function delay<T>(value: T, ms = 220): Promise<T> {
  return new Promise((resolve) => setTimeout(() => resolve(value), ms));
}

function fail(message: string): never {
  throw new Error(message);
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

let seqCounter = 1;

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
  },

  address: {
    suggest: (query) => delay(suggestAddresses(query), 180),
  },

  scenarios: {
    list: () => delay(clone(db.scenarios)),

    get: (id) => {
      const s = scenarioById(id);
      if (!s) fail('Сценарий не найден');
      return delay(clone(s));
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
      return delay(clone(scenario), 300);
    },

    /**
     * В проде: go-core кладёт ai_jobs(queued) → диспетчер → ai-service →
     * callback /internal/ai/v1/results → scenarios(status=generated) + etalons v1.
     * Занимает минуты. Здесь — задержка и клонирование ближайшего образца.
     */
    async generate(input: GenerateScenarioInput) {
      const type = typeById(input.categoryId);
      const template =
        db.scenarios.find((s) => s.categoryId === input.categoryId) ??
        db.scenarios.find((s) => s.status === 'validated') ??
        db.scenarios[0];

      const scenario: Scenario = {
        ...clone(template),
        id: uid('sc'),
        title: `${type?.name ?? 'Происшествие'} — учебный сценарий (сгенерирован)`,
        categoryId: input.categoryId,
        categoryName: type?.name ?? input.categoryId,
        difficulty: input.difficulty,
        mode: input.mode,
        source: 'generated',
        status: 'generated',
        etalonVersion: 1,
        createdAt: new Date().toISOString(),
        validatedBy: undefined,
        validatedAt: undefined,
        teacherComment: input.teacherComment,
        notesForTeacher:
          'Черновик от нейросети. Проверьте легенду звонка и состав обязательных полей перед подтверждением.',
      };

      db.scenarios.unshift(scenario);
      return delay(clone(scenario), 1800);
    },

    async update(id, patch) {
      const s = scenarioById(id);
      if (!s) fail('Сценарий не найден');
      Object.assign(s, patch);
      return delay(clone(s), 250);
    },

    async approve(id) {
      const s = scenarioById(id);
      if (!s) fail('Сценарий не найден');
      s.status = 'validated';
      s.validatedBy = currentUserName();
      s.validatedAt = new Date().toISOString();
      return delay(clone(s), 300);
    },

    async reject(id, reason) {
      const s = scenarioById(id);
      if (!s) fail('Сценарий не найден');
      s.status = 'rejected';
      s.teacherComment = reason;
      return delay(clone(s), 250);
    },
  },

  lessons: {
    defaultSettings: () => delay(makeLessonSettings(), 60),

    list: () => delay(clone(db.lessons)),

    get: (id) => {
      const l = lessonById(id);
      if (!l) fail('Занятие не найдено');
      return delay(clone(l));
    },

    async create(input: CreateLessonInput) {
      // Автор занятия — текущий пользователь сессии: занятие не должно
      // технически принадлежать другому преподавателю.
      const author = sessionUser();
      if (!author) fail('Сессия не найдена: войдите заново');

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
        settings: {
          ...makeLessonSettings(),
          passThreshold: input.passThreshold,
          allowReplay: input.allowReplay,
        },
        createdAt: new Date().toISOString(),
      };
      db.lessons.unshift(lesson);
      return delay(clone(lesson), 350);
    },

    async start(id) {
      const lesson = lessonById(id);
      if (!lesson) fail('Занятие не найдено');
      if (lesson.scenarioIds.length === 0) fail('В занятии нет ни одного сценария');
      lesson.status = 'running';
      lesson.startedAt = new Date().toISOString();
      issueAttempts(lesson);
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
      return delay(clone(lesson), 300);
    },

    async assigned(userId) {
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
    get: (id) => {
      const a = requireAttempt(id);
      a.serverNow = new Date().toISOString();
      return delay(clone(a));
    },

    forLesson: (lessonId) => delay(clone(db.attempts.filter((a) => a.lessonId === lessonId))),

    async acceptCall(id) {
      const attempt = requireAttempt(id);
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
      return delay(clone(attempt), 260);
    },

    async getDraft(id) {
      requireAttempt(id);
      return delay(clone(db.drafts[id] ?? emptyCard()), 160);
    },

    async updateDraft(id, card) {
      requireAttempt(id);
      db.drafts[id] = clone(card);
      return delay({ savedAt: new Date().toISOString() }, 200);
    },

    async addEvent(id, event) {
      const attempt = requireAttempt(id);

      /*
       * Время первого ввода берётся из первого содержательного действия
       * обучающегося, а не из автосохранения: autosave срабатывает с задержкой
       * и после отправки карточки, и по нему время реакции было бы завышено.
       */
      if (!attempt.firstInputAt && USER_INPUT_EVENTS.includes(event.type)) {
        attempt.firstInputAt = event.at;
      }

      db.events[id] = [...(db.events[id] ?? []), { ...event, clientSeq: seqCounter++ }];
      return delay(undefined, 20);
    },

    async events(id) {
      requireAttempt(id);
      return delay(clone(db.events[id] ?? []), 120);
    },

    async changeServiceStatus({ attemptId, serviceId, status, squadNumber, comment }: ChangeStatusInput) {
      requireAttempt(attemptId);
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

      return delay(clone(service), 240);
    },

    async addService(id, serviceCode) {
      requireAttempt(id);
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
      return delay(clone(service), 240);
    },

    async removeService(id, serviceId) {
      requireAttempt(id);
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
      return delay(undefined, 180);
    },

    async replay(id) {
      const attempt = requireAttempt(id);
      attempt.replayCount += 1;
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

      const now = new Date();
      db.drafts[id] = clone(card);
      attempt.card = clone(card);
      attempt.submittedAt = now.toISOString();
      attempt.status = 'evaluating';
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

    async override(attemptId, score, reason, by) {
      const ev = db.evaluations[attemptId];
      if (!ev) fail('Оценка ещё не сформирована');
      const attempt = attemptById(attemptId);
      const lesson = attempt ? lessonById(attempt.lessonId) : undefined;

      ev.override = { score, verdict: score >= (lesson?.settings.passThreshold ?? 70) ? 'pass' : 'fail', reason, by, at: new Date().toISOString() };
      ev.finalScore = score;
      ev.verdict = ev.override.verdict;
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
      };
      return delay(view, 260);
    },
  },

  feedback: {
    async list(attemptId) {
      requireAttempt(attemptId);
      return delay(clone(db.feedback.filter((f) => f.attemptId === attemptId)), 140);
    },

    async add(attemptId, comment, recommendation, by) {
      const item: TeacherFeedback = {
        id: uid('fb'),
        attemptId,
        teacherName: by,
        comment,
        recommendation: recommendation || undefined,
        createdAt: new Date().toISOString(),
      };
      db.feedback.push(item);
      return delay(clone(item), 240);
    },
  },

  reaction: {
    allowedNext: (current: ReactionStatus, serviceCode: string) =>
      delay(computeAllowedNext(current, serviceCode), 60),
  },
};

export type { Attempt, Evaluation, IncidentCardDraft, Lesson, Scenario, User, AttemptEvent };
