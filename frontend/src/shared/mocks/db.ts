/**
 * In-memory состояние mock-слоя + расчёт учебной оценки.
 *
 * Это НЕ имитация backend-эндпоинтов, а хранилище данных прототипа.
 * Вся логика оценивания здесь — FIXTURE: в проде её считает go-core
 * (docs/contracts.md: «go-core считает всё детерминированное; ai-service —
 * всё, что требует NLP/LLM»). Когда появится API, файл удаляется целиком.
 */

import type {
  Attempt, AttemptEvent, Evaluation, FieldError, IncidentCardDraft,
  Lesson, Recommendation, Scenario, TeacherFeedback, User,
} from '../types';
import { emptyCard, getPath, fieldLabel } from '../utils/card';
import { formatDuration } from '../utils/time';
import { attributeLabel, attributeValueLabel, typeById } from './fixtures/classifier';
import { SCENARIOS } from './fixtures/scenarios';
import { USERS } from './fixtures/users';
import { uid } from '../utils/id';

export interface DbState {
  users: User[];
  scenarios: Scenario[];
  lessons: Lesson[];
  attempts: Attempt[];
  drafts: Record<string, IncidentCardDraft>;
  events: Record<string, AttemptEvent[]>;
  evaluations: Record<string, Evaluation>;
  feedback: TeacherFeedback[];
}

const DEFAULT_SETTINGS: Lesson['settings'] = {
  passThreshold: 70,
  weights: { fields: 0.5, semantic: 0.25, grammar: 0.1, timing: 0.15 },
  cardsPerStudent: 1,
  allowReplay: true,
};

export const db: DbState = {
  users: USERS.map((u) => ({ ...u })),
  scenarios: [...SCENARIOS],
  lessons: [
    {
      id: 'ls-demo',
      kind: 'class',
      title: 'Практическое занятие: пожары и запах газа',
      teacherId: 'u-teacher',
      mode: 'cards',
      perspective: 'operator112',
      difficulty: 2,
      timeLimitSec: 30,
      status: 'running',
      scenarioIds: ['sc-fire-apartment', 'sc-gas-smell'],
      participants: [
        { userId: 'u-student', name: 'Рожкова О. И.', status: 'assigned' },
        { userId: 'u-student-2', name: 'Никитин П. А.', status: 'assigned' },
      ],
      settings: DEFAULT_SETTINGS,
      createdAt: '2026-09-17T08:00:00+03:00',
      startedAt: '2026-09-17T09:00:00+03:00',
    },
    {
      id: 'ls-water',
      kind: 'class',
      title: 'Аварии в городском хозяйстве',
      teacherId: 'u-teacher',
      mode: 'cards',
      perspective: 'operator112',
      difficulty: 1,
      timeLimitSec: 45,
      status: 'draft',
      scenarioIds: ['sc-water-pipe'],
      participants: [{ userId: 'u-student', name: 'Рожкова О. И.', status: 'assigned' }],
      settings: DEFAULT_SETTINGS,
      createdAt: '2026-09-17T12:30:00+03:00',
    },
  ],
  attempts: [],
  drafts: {},
  events: {},
  evaluations: {},
  feedback: [],
};

/**
 * Демо-занятие создано уже запущенным — выдаём по нему попытки сразу,
 * чтобы обучающийся при первом входе увидел поступивший вызов, а не пустой экран.
 * Вызов внизу файла: issueAttempts объявлена ниже по тексту.
 */
function bootstrapRunningLessons(): void {
  for (const lesson of db.lessons) {
    if (lesson.status === 'running') issueAttempts(lesson);
  }
}

export function makeLessonSettings(): Lesson['settings'] {
  return { ...DEFAULT_SETTINGS, weights: { ...DEFAULT_SETTINGS.weights } };
}

let incidentCounter = 36814851;

export function nextIncidentNo(): string {
  incidentCounter += 1;
  return String(incidentCounter);
}

export function scenarioById(id: string): Scenario | undefined {
  return db.scenarios.find((s) => s.id === id);
}

export function lessonById(id: string): Lesson | undefined {
  return db.lessons.find((l) => l.id === id);
}

export function attemptById(id: string): Attempt | undefined {
  return db.attempts.find((a) => a.id === id);
}

/** Создаёт попытки участникам при старте занятия. */
export function issueAttempts(lesson: Lesson): Attempt[] {
  const created: Attempt[] = [];

  lesson.participants.forEach((participant, index) => {
    const existing = db.attempts.find(
      (a) => a.lessonId === lesson.id && a.userId === participant.userId,
    );
    if (existing) {
      created.push(existing);
      return;
    }

    const scenarioId = lesson.scenarioIds[index % lesson.scenarioIds.length];
    const attempt: Attempt = {
      id: uid('at'),
      lessonId: lesson.id,
      lessonTitle: lesson.title,
      userId: participant.userId,
      scenarioId,
      mode: lesson.mode,
      perspective: lesson.perspective,
      seqNo: 1,
      status: 'issued',
      timeLimitSec: lesson.timeLimitSec,
      issuedAt: new Date().toISOString(),
      replayCount: 0,
      incidentNo: nextIncidentNo(),
      serverNow: new Date().toISOString(),
    };

    db.attempts.push(attempt);
    db.drafts[attempt.id] = emptyCard();
    db.events[attempt.id] = [
      { clientSeq: 0, type: 'issued', at: attempt.issuedAt, payload: { scenarioId } },
    ];
    participant.attemptId = attempt.id;
    created.push(attempt);
  });

  return created;
}

// ───────────────────────────────────────────────── FIXTURE: расчёт оценки

/**
 * Подпись поля для отчёта. Для признаков опросной карты берётся название
 * группы из классификатора, иначе в интерфейс утекал бы технический код
 * («Признак: where» вместо «Где»).
 */
function errorLabel(path: string): string {
  if (path.startsWith('attributes.')) {
    return attributeLabel(path.slice('attributes.'.length)) ?? fieldLabel(path);
  }
  return fieldLabel(path);
}

/** Значение поля в человекочитаемом виде: коды разворачиваются в подписи. */
function readableValue(path: string, value: unknown): unknown {
  if (value == null) return value;

  // Типы происшествия хранятся идентификаторами — в отчёте нужны названия.
  if (path === 'incidentTypeIds') {
    const ids = Array.isArray(value) ? value : [value];
    return ids.map((id) => typeById(String(id))?.name ?? String(id));
  }

  if (!path.startsWith('attributes.')) return value;
  const attr = path.slice('attributes.'.length);
  const codes = Array.isArray(value) ? value : [value];
  const labels = codes.map((c) => attributeValueLabel(attr, String(c)) ?? String(c));
  return Array.isArray(value) ? labels : labels[0];
}

/** Слой 1 — формализованные поля. В проде считает go-core мгновенно. */
function computeFieldErrors(card: IncidentCardDraft, scenario: Scenario): FieldError[] {
  const errors: FieldError[] = [];

  for (const path of scenario.requiredFields) {
    const actual = getPath(card, path);
    const expected = getPath(scenario.etalonDraft, path);

    const actualCodes = normalize(actual);
    const expectedCodes = normalize(expected);

    if (actualCodes.length === 0) {
      errors.push({
        field: path,
        label: errorLabel(path),
        expected: readableValue(path, expected),
        actual: readableValue(path, actual),
        kind: 'missing',
        weight: 1,
      });
      continue;
    }

    // Свободный текст не сравниваем побуквенно — это работа семантического слоя.
    if (path === 'description' || path === 'actionsTaken') continue;

    const sameSet =
      actualCodes.length === expectedCodes.length &&
      actualCodes.every((code) => expectedCodes.includes(code));

    if (!sameSet && expectedCodes.length > 0) {
      errors.push({
        field: path,
        label: errorLabel(path),
        expected: readableValue(path, expected),
        actual: readableValue(path, actual),
        kind: 'wrong',
        weight: 1,
      });
    }
  }

  return errors;
}

function normalize(value: unknown): string[] {
  if (value == null || value === '') return [];
  if (typeof value === 'boolean') return value ? ['true'] : [];
  if (Array.isArray(value)) return value.map(String);
  if (typeof value === 'object') return [];
  // Массивы, объекты, null и boolean разобраны выше — остался примитив.
  return [String(value)];
}

/**
 * Текст смысловой оценки строится по фактическому покрытию.
 *
 * Прежняя формулировка «Адрес и характер происшествия записаны» выдавалась
 * всегда и противоречила данным, когда описание было пустым.
 */
function semanticSummary(text: string, covered: number, total: number, missing: string[]): string {
  if (text.trim() === '') {
    return 'Описание со слов заявителя не заполнено, поэтому смысловую полноту оценить не по чему.';
  }
  if (total === 0) return 'Для этого сценария существенные факты не заданы.';
  if (covered === 0) {
    return `Ни один из ${total} существенных фактов обращения в описании не зафиксирован.`;
  }
  if (missing.length === 0) {
    return 'Описание полное: все существенные обстоятельства из обращения зафиксированы.';
  }
  return `Зафиксировано ${covered} из ${total} существенных фактов. Не отражено: ${missing[0]}.`;
}

/** Слой 4 — тайминг. Норма из settings.timing_tolerance: soft 20%, hard 100%. */
function computeTimingScore(spentMs: number, limitSec: number): number {
  const limitMs = limitSec * 1000;
  const soft = limitMs * 1.2;
  const hard = limitMs * 2;
  if (spentMs <= soft) return 100;
  if (spentMs >= hard) return 0;
  return Math.round(100 * (1 - (spentMs - soft) / (hard - soft)));
}

/**
 * Слой 2 — грамматика. В проде — LanguageTool на ai-service.
 *
 * Если свободного текста нет, слой не оценивается: балл 100 за пустое поле
 * противоречил бы строке «проверено слов: 0» и завышал бы итог. Слой без
 * оценки исключается из взвешивания.
 */
function computeGrammar(card: IncidentCardDraft): Evaluation['grammar'] {
  const text = card.description ?? '';
  if (text.trim() === '') return undefined;
  const remarks: NonNullable<Evaluation['grammar']>['remarks'] = [];

  const lowerStart = /^[а-яё]/.exec(text);
  if (lowerStart) {
    remarks.push({
      field: 'description',
      offset: 0,
      length: 1,
      severity: 'error',
      rule: 'UPPERCASE_SENTENCE_START',
      message: 'Предложение следует начинать с заглавной буквы.',
      suggestions: [text.charAt(0).toUpperCase()],
    });
  }

  for (const m of text.matchAll(/\b(кв|д|ул|эт)\b\.?/gi)) {
    if (m.index === undefined) continue;
    remarks.push({
      field: 'description',
      offset: m.index,
      length: m[0].length,
      severity: 'style',
      rule: 'ABBREVIATION',
      message: 'В описании со слов заявителя сокращения лучше раскрывать полностью.',
      suggestions: [],
    });
  }

  if (text.length > 0 && !/[.!?]$/.test(text.trim())) {
    remarks.push({
      field: 'description',
      offset: Math.max(0, text.trimEnd().length - 1),
      length: 1,
      severity: 'warning',
      rule: 'MISSING_FINAL_PUNCT',
      message: 'В конце предложения отсутствует знак препинания.',
      suggestions: ['.'],
    });
  }

  const wordsChecked = text.trim() ? text.trim().split(/\s+/).length : 0;
  const errors = remarks.filter((r) => r.severity === 'error').length;
  const warnings = remarks.filter((r) => r.severity === 'warning').length;
  const styles = remarks.filter((r) => r.severity === 'style').length;
  const score = Math.max(0, 100 - errors * 15 - warnings * 7 - styles * 3);

  return {
    score,
    stats: { wordsChecked, errorsBySeverity: { error: errors, warning: warnings, style: styles } },
    remarks,
  };
}

/** Слой 3 — семантика. В проде — LLM на ai-service. */
function computeSemantic(card: IncidentCardDraft, scenario: Scenario): Evaluation['semantic'] {
  const text = (card.description ?? '').toLowerCase();
  const facts = scenario.callScript.keyFacts;

  const missing = facts.filter((fact) => {
    const words = fact.toLowerCase().split(/\s+/).filter((w) => w.length > 4);
    if (words.length === 0) return false;
    const hits = words.filter((w) => text.includes(w.slice(0, Math.max(4, w.length - 2))));
    return hits.length / words.length < 0.4;
  });

  const covered = facts.length - missing.length;
  const score = facts.length === 0 ? 100 : Math.round((covered / facts.length) * 100);

  return {
    score,
    confidence: text.length < 40 ? 0.58 : 0.86,
    missingFacts: missing,
    extraFacts: [],
    perField: [
      {
        field: 'description',
        score,
        comment:
          missing.length === 0
            ? 'Ключевые обстоятельства происшествия переданы полностью.'
            : `Не зафиксировано: ${missing.join('; ')}.`,
      },
    ],
    summaryForStudent: semanticSummary(text, covered, facts.length, missing),
  };
}

/**
 * Рекомендации строятся только по фактическим данным этой попытки:
 * её норматив, её затраченное время, её незаполненные поля. Универсальных
 * формулировок, способных разойтись с цифрами на экране, здесь нет.
 */
function buildRecommendations(ev: Evaluation, scenario: Scenario): Recommendation[] {
  const out: Recommendation[] = [];

  const missing = ev.fieldErrors.filter((e) => e.kind === 'missing');
  if (missing.length > 0) {
    out.push({
      id: uid('rec'),
      kind: 'weak_field',
      body: 'Перед сохранением карточки заполните обязательные поля:',
      items: missing.map((e) => e.label),
    });
  }

  const wrong = ev.fieldErrors.filter((e) => e.kind === 'wrong');
  if (wrong.length > 0) {
    out.push({
      id: uid('rec'),
      kind: 'weak_field',
      body: 'Значения не совпадают с эталоном:',
      items: wrong.map((e) => e.label),
    });
  }

  if (!ev.timing.withinNorm) {
    const over = formatDuration(Math.max(0, ev.timing.deltaMs));
    out.push({
      id: uid('rec'),
      kind: 'slow_timing',
      body:
        `Норматив заполнения — ${ev.timing.limitSec} с, затрачено ` +
        `${formatDuration(ev.timing.spentMs)} (превышение на ${over}). ` +
        'Начинайте вводить адрес одновременно с разговором, не дожидаясь конца обращения.',
    });
  }

  const missingFacts = ev.semantic?.missingFacts ?? [];
  if (missingFacts.length > 0) {
    out.push({
      id: uid('rec'),
      kind: 'general',
      body: `Отработайте тему «${scenario.categoryName}». В описании со слов заявителя не зафиксировано:`,
      items: missingFacts,
    });
  }

  const remarks = ev.grammar?.remarks ?? [];
  if (remarks.length > 2) {
    out.push({
      id: uid('rec'),
      kind: 'grammar_pattern',
      body:
        `Замечаний к тексту: ${remarks.length}. Избегайте сокращений и пишите ` +
        'полными предложениями — карточку читает диспетчер службы.',
    });
  }

  return out;
}


/**
 * Первый этап: поля + тайминг считаются мгновенно → status `partial`.
 * docs/contracts.md: «go-core ──┐ мгновенно: поля + тайминг, evaluations(partial)».
 */
export function evaluatePartial(attempt: Attempt, card: IncidentCardDraft, scenario: Scenario, lesson: Lesson): Evaluation {
  const fieldErrors = computeFieldErrors(card, scenario);
  const total = scenario.requiredFields.length || 1;
  const fieldsScore = Math.max(0, Math.round(((total - fieldErrors.length) / total) * 100));

  const spentMs = attempt.timeSpentMs ?? 0;
  const limitSec = attempt.timeLimitSec;
  const timingScore = computeTimingScore(spentMs, limitSec);
  const reactionMs =
    attempt.firstInputAt && attempt.callAcceptedAt
      ? new Date(attempt.firstInputAt).getTime() - new Date(attempt.callAcceptedAt).getTime()
      : 0;

  const ev: Evaluation = {
    attemptId: attempt.id,
    status: 'partial',
    fieldsScore,
    timingScore,
    totalScore: 0,
    verdict: 'pending',
    fieldErrors,
    timing: {
      spentMs,
      limitSec,
      deltaMs: spentMs - limitSec * 1000,
      withinNorm: spentMs <= limitSec * 1000 * 1.2,
      reactionMs,
    },
    needsReview: false,
    aiUnavailable: false,
    recommendations: [],
    finalScore: 0,
  };

  ev.totalScore = weighted(ev, lesson);
  ev.finalScore = ev.totalScore;
  return ev;
}

/** Второй этап: доезжают AI-слои → status `done`, verdict, рекомендации. */
export function evaluateComplete(ev: Evaluation, card: IncidentCardDraft, scenario: Scenario, lesson: Lesson): Evaluation {
  const grammar = computeGrammar(card);
  const semantic = computeSemantic(card, scenario);

  const next: Evaluation = {
    ...ev,
    status: 'done',
    grammar,
    semantic,
    grammarScore: grammar?.score,
    semanticScore: semantic?.score,
    needsReview: (semantic?.confidence ?? 1) < 0.7,
  };

  next.totalScore = weighted(next, lesson);
  next.finalScore = next.override?.score ?? next.totalScore;
  next.verdict = next.finalScore >= lesson.settings.passThreshold ? 'pass' : 'fail';
  next.recommendations = buildRecommendations(next, scenario);
  return next;
}

/** Итог по весам занятия. Считает только go-core — ai-service весов не видит. */
function weighted(ev: Evaluation, lesson: Lesson): number {
  const w = lesson.settings.weights;
  const parts: Array<[number | undefined, number]> = [
    [ev.fieldsScore, w.fields],
    [ev.semanticScore, w.semantic],
    [ev.grammarScore, w.grammar],
    [ev.timingScore, w.timing],
  ];

  let sum = 0;
  let usedWeight = 0;
  for (const [score, weight] of parts) {
    if (score == null) continue;
    sum += score * weight;
    usedWeight += weight;
  }
  return usedWeight === 0 ? 0 : Math.round(sum / usedWeight);
}

/**
 * Блокировка учётных записей переживает перезагрузку страницы.
 *
 * Остальное состояние прототипа живёт только в памяти, но блокировка — это
 * решение администратора, а не демонстрационные данные: если она исчезает
 * при обновлении страницы, проверить её невозможно. Когда появится backend,
 * хранилище удаляется вместе со всем mock-слоем (J-04 / J-05).
 */
const BLOCKED_KEY = 'arm112.mock.blockedUsers';

function readBlocked(): string[] {
  try {
    const raw = localStorage.getItem(BLOCKED_KEY);
    const parsed: unknown = raw ? JSON.parse(raw) : [];
    return Array.isArray(parsed) ? parsed.filter((x): x is string => typeof x === 'string') : [];
  } catch {
    // приватный режим или запрет хранилища — работаем только с памятью
    return [];
  }
}

function writeBlocked(ids: string[]): void {
  try {
    localStorage.setItem(BLOCKED_KEY, JSON.stringify(ids));
  } catch {
    // хранилище недоступно: блокировка останется действовать до перезагрузки
  }
}

/** Меняет состояние учётной записи и запоминает его между перезагрузками. */
export function setUserBlocked(userId: string, blocked: boolean): User {
  const user = db.users.find((u) => u.id === userId);
  if (!user) throw new Error('Учётная запись не найдена');

  user.status = blocked ? 'blocked' : 'active';

  const ids = readBlocked().filter((id) => id !== userId);
  if (blocked) ids.push(userId);
  writeBlocked(ids);

  return user;
}

function applyBlockedUsers(): void {
  const ids = readBlocked();
  for (const user of db.users) {
    if (ids.includes(user.id)) user.status = 'blocked';
  }
}

applyBlockedUsers();
bootstrapRunningLessons();
