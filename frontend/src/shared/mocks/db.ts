/**
 * In-memory состояние mock-слоя + расчёт учебной оценки.
 *
 * Это НЕ имитация backend-эндпоинтов, а хранилище данных прототипа.
 * Вся логика оценивания здесь — FIXTURE: в проде её считает go-core
 * (docs/contracts.md: «go-core считает всё детерминированное; ai-service —
 * всё, что требует NLP/LLM»). Когда появится API, файл удаляется целиком.
 */

import type {
  Attempt, AttemptEventInput, DialogueEndReason, DialogueResult, DialogueTurnResponse,
  DialogueTurnView, Evaluation, FieldError, IncidentCardDraft,
  Lesson, Recommendation, Scenario, TeacherFeedback, User,
} from '../types';
import { emptyCard, getPath, fieldLabel } from '../utils/card';
import { formatDuration } from '../utils/time';
import { attributeLabel, attributeValueLabel, typeById } from './fixtures/classifier';
import { FILLER_WORDS } from './fixtures/dialogue';
import { SCENARIOS } from './fixtures/scenarios';
import { USERS } from './fixtures/users';
import { uid } from '../utils/id';

/**
 * Состояние разговора одной попытки.
 *
 * `revealedFactIds` — серверная величина: она нужна, чтобы заявитель не
 * повторял уже раскрытые факты, и обучающемуся не отдаётся никогда.
 */
export interface DialogueRecord {
  turns: DialogueTurnView[];
  callEnded: boolean;
  callEndedAt?: string;
  endReason?: DialogueEndReason;
  nextTurnNo: number;
  revealedFactIds: string[];
  /** ответ на последний ход — для идемпотентного повтора после обрыва связи */
  lastResponse?: DialogueTurnResponse;
}

export interface DbState {
  users: User[];
  scenarios: Scenario[];
  lessons: Lesson[];
  attempts: Attempt[];
  drafts: Record<string, IncidentCardDraft>;
  /** события в порядке поступления; id присваивается при чтении */
  events: Record<string, AttemptEventInput[]>;
  evaluations: Record<string, Evaluation>;
  feedback: TeacherFeedback[];
  dialogues: Record<string, DialogueRecord>;
}

/*
 * Голосовой режим по умолчанию выключен: вес разговора равен нулю, итог
 * считается по четырём слоям — ровно как до его появления. Включает
 * преподаватель при создании занятия.
 */
const DEFAULT_SETTINGS: Lesson['settings'] = {
  passThreshold: 70,
  weights: { fields: 0.5, semantic: 0.25, grammar: 0.1, timing: 0.15, dialogue: 0 },
  cardsPerStudent: 1,
  allowReplay: true,
  voice: { enabled: false, input: 'voice', pushToTalk: true, maxTurns: 12, ttsEnabled: true },
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
  dialogues: {},
};

/**
 * Демо-занятие создано уже запущенным — выдаём по нему попытки сразу,
 * чтобы обучающийся при первом входе увидел поступивший вызов, а не пустой экран.
 * Вызов внизу файла: issueAttempts объявлена ниже по тексту.
 */
function bootstrapRunningLessons(): void {
  for (const lesson of db.lessons) {
    if (lesson.status === 'running') issueAttempts(lesson);
    // Завершённое занятие: сданные попытки восстанавливаются с оценкой,
    // остальные закрыты — контракт finishLesson: незавершённые → expired.
    if (lesson.status === 'finished') {
      for (const attempt of issueAttempts(lesson)) {
        if (attempt.status === 'issued' || attempt.status === 'in_progress') attempt.status = 'expired';
      }
    }
  }
}

export function makeLessonSettings(): Lesson['settings'] {
  return {
    ...DEFAULT_SETTINGS,
    weights: { ...DEFAULT_SETTINGS.weights },
    voice: { ...DEFAULT_SETTINGS.voice },
  };
}

/** Доля слоя «Разговор», когда занятие голосовое, а веса не заданы явно. */
const DEFAULT_DIALOGUE_WEIGHT = 0.25;

/**
 * Веса для голосового занятия, если преподаватель их не задал.
 *
 * Значения по умолчанию рассчитаны на занятие без разговора (`dialogue: 0`),
 * и оставить их — значит не засчитать разговор вовсе. Остальные слои
 * пропорционально ужимаются, их соотношение сохраняется.
 */
export function withDialogueWeight(weights: Lesson['settings']['weights']): Lesson['settings']['weights'] {
  if (weights.dialogue > 0) return weights;
  const rest = 1 - DEFAULT_DIALOGUE_WEIGHT;
  const sum = weights.fields + weights.semantic + weights.grammar + weights.timing;
  if (sum <= 0) return { ...weights, dialogue: DEFAULT_DIALOGUE_WEIGHT };
  return {
    fields: (weights.fields / sum) * rest,
    semantic: (weights.semantic / sum) * rest,
    grammar: (weights.grammar / sum) * rest,
    timing: (weights.timing / sum) * rest,
    dialogue: DEFAULT_DIALOGUE_WEIGHT,
  };
}

/**
 * Перенормировка весов под голосовой режим.
 *
 * Выключенный разговор не должен «съедать» свою долю: его вес уходит в ноль,
 * а остальные слои делят единицу между собой.
 */
export function normalizeWeights(
  weights: Lesson['settings']['weights'],
  voiceEnabled: boolean,
): Lesson['settings']['weights'] {
  const next = { ...weights, dialogue: voiceEnabled ? weights.dialogue : 0 };
  const sum = next.fields + next.semantic + next.grammar + next.timing + next.dialogue;
  if (sum <= 0) return next;
  return {
    fields: next.fields / sum,
    semantic: next.semantic / sum,
    grammar: next.grammar / sum,
    timing: next.timing / sum,
    dialogue: next.dialogue / sum,
  };
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
    /*
     * Идентификатор детерминирован: после перезагрузки страницы mock-БД
     * собирается заново, и со случайным идентификатором адрес рабочего места
     * становился бы недействительным, а восстанавливать разговор было бы не к
     * чему. Пара «занятие + обучающийся» уникальна по построению.
     */
    const attempt: Attempt = {
      id: `at-${lesson.id}-${participant.userId}`,
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
      voice: { ...lesson.settings.voice },
    };

    db.attempts.push(attempt);
    db.drafts[attempt.id] = emptyCard();
    db.events[attempt.id] = [
      { clientSeq: 0, type: 'issued', at: attempt.issuedAt, payload: { scenarioId } },
    ];
    participant.attemptId = attempt.id;
    restoreAttemptRuntime(attempt);
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

  const missedQuestions = ev.dialogue?.missingQuestions ?? [];
  if (missedQuestions.length > 0) {
    out.push({
      id: uid('rec'),
      kind: 'dialogue_pattern',
      body:
        'В разговоре с заявителем не заданы обязательные вопросы протокола. ' +
        'Задавайте их до того, как заявитель положит трубку:',
      items: missedQuestions,
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

  const voiceOn = lesson.settings.voice.enabled;

  const ev: Evaluation = {
    attemptId: attempt.id,
    status: 'partial',
    fieldsScore,
    timingScore,
    totalScore: 0,
    verdict: 'pending',
    fieldErrors,
    weights: normalizeWeights(lesson.settings.weights, voiceOn),
    layers: {
      fields: 'done',
      timing: 'done',
      grammar: 'queued',
      semantic: 'queued',
      dialogue: voiceOn ? 'queued' : 'skipped',
    },
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
  const voiceOn = lesson.settings.voice.enabled;
  const dialogue = voiceOn ? computeDialogue(ev.attemptId, scenario) : undefined;

  const next: Evaluation = {
    ...ev,
    status: 'done',
    grammar,
    semantic,
    dialogue,
    grammarScore: grammar?.score,
    semanticScore: semantic?.score,
    dialogueScore: dialogue?.score,
    layers: {
      ...ev.layers,
      grammar: grammar ? 'done' : 'skipped',
      semantic: semantic ? 'done' : 'skipped',
      dialogue: voiceOn ? (dialogue ? 'done' : 'skipped') : 'skipped',
    },
    needsReview:
      (semantic?.confidence ?? 1) < 0.7 || (dialogue?.confidence ?? 1) < 0.7,
  };

  next.totalScore = weighted(next, lesson);
  next.finalScore = next.override?.score ?? next.totalScore;
  next.verdict = next.finalScore >= lesson.settings.passThreshold ? 'pass' : 'fail';
  next.recommendations = buildRecommendations(next, scenario);
  return next;
}

/** Итог по весам занятия. Считает только go-core — ai-service весов не видит. */
function weighted(ev: Evaluation, lesson: Lesson): number {
  const w = ev.weights ?? normalizeWeights(lesson.settings.weights, lesson.settings.voice.enabled);
  const parts: Array<[number | undefined, number]> = [
    [ev.fieldsScore, w.fields],
    [ev.semanticScore, w.semantic],
    [ev.grammarScore, w.grammar],
    [ev.timingScore, w.timing],
    [ev.dialogueScore, w.dialogue],
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


// ──────────────────────────────── разговор: состояние, персист, оценка

/** Пустое состояние разговора: до первого хода ходов нет. */
export function emptyDialogue(): DialogueRecord {
  return { turns: [], callEnded: false, nextTurnNo: 1, revealedFactIds: [] };
}

export function dialogueRecord(attemptId: string): DialogueRecord {
  db.dialogues[attemptId] ??= emptyDialogue();
  return db.dialogues[attemptId];
}

/**
 * Состояние активной попытки переживает перезагрузку страницы.
 *
 * Остальная mock-БД собирается заново при каждой загрузке — так и задумано,
 * это демонстрационные данные. Но попытка — это работа обучающегося: если
 * после F5 исчезают принятый вызов, черновик и транскрипт разговора, проверить
 * восстановление невозможно. Сохраняем ровно то, что на настоящем сервере
 * лежало бы в `attempts`, `attempt_drafts` и `attempt_dialogue_turns`.
 *
 * Здесь — активная попытка. Сданные и закрытые хранятся отдельно вместе
 * с оценкой (см. «завершённые попытки» ниже).
 */
const ATTEMPT_KEY = 'arm112.mock.attemptRuntime';

interface PersistedAttempt {
  status: Attempt['status'];
  callAcceptedAt?: string;
  firstInputAt?: string;
  replayCount: number;
  draft?: IncidentCardDraft;
  dialogue?: DialogueRecord;
  /** журнал действий: без него после F5 хронология преподавателя теряла начало попытки */
  events?: AttemptEventInput[];
}

function readPersisted(): Record<string, PersistedAttempt> {
  try {
    const raw = sessionStorage.getItem(ATTEMPT_KEY);
    const parsed: unknown = raw ? JSON.parse(raw) : {};
    return parsed && typeof parsed === 'object' ? (parsed as Record<string, PersistedAttempt>) : {};
  } catch {
    // приватный режим или запрет хранилища — работаем только с памятью
    return {};
  }
}

function writePersisted(value: Record<string, PersistedAttempt>): void {
  try {
    sessionStorage.setItem(ATTEMPT_KEY, JSON.stringify(value));
  } catch {
    // хранилище недоступно: состояние живёт до перезагрузки
  }
}

/** Сохраняет активную попытку; завершённую — удаляет из хранилища. */
export function persistAttemptRuntime(attempt: Attempt): void {
  const all = readPersisted();

  if (attempt.status !== 'issued' && attempt.status !== 'in_progress') {
    delete all[attempt.id];
    writePersisted(all);
    return;
  }

  all[attempt.id] = {
    status: attempt.status,
    callAcceptedAt: attempt.callAcceptedAt,
    firstInputAt: attempt.firstInputAt,
    replayCount: attempt.replayCount,
    draft: db.drafts[attempt.id],
    dialogue: db.dialogues[attempt.id],
    events: db.events[attempt.id],
  };
  writePersisted(all);
}

export function clearAttemptRuntime(attemptId: string): void {
  const all = readPersisted();
  delete all[attemptId];
  writePersisted(all);
}

/** Возвращает попытке состояние, сохранённое до перезагрузки страницы. */
/**
 * Занятия, созданные во время работы, тоже переживают перезагрузку.
 *
 * Фикстурные занятия собираются заново при каждой загрузке, а созданное
 * преподавателем существовало бы только до F5 — вместе с ним пропадала бы
 * и попытка обучающегося, хотя её состояние сохранено. Храним рядом с
 * попытками и по той же причине: без этого проверить восстановление нельзя.
 */
const LESSONS_KEY = 'arm112.mock.lessons';

function readCreatedLessons(): Lesson[] {
  try {
    const raw = sessionStorage.getItem(LESSONS_KEY);
    const parsed: unknown = raw ? JSON.parse(raw) : [];
    return Array.isArray(parsed) ? (parsed as Lesson[]) : [];
  } catch {
    return [];
  }
}

/** Запоминает занятие целиком: создание, запуск и завершение. */
export function persistLesson(lesson: Lesson): void {
  const all = readCreatedLessons().filter((l) => l.id !== lesson.id);
  all.push(JSON.parse(JSON.stringify(lesson)) as Lesson);
  try {
    sessionStorage.setItem(LESSONS_KEY, JSON.stringify(all));
  } catch {
    // хранилище недоступно: занятие живёт до перезагрузки
  }
}

/**
 * Сценарии, созданные или отредактированные преподавателем.
 *
 * Фикстурные сценарии собираются заново при каждой загрузке. Работа
 * преподавателя — нет: без сохранения созданный сценарий исчезал бы при F5
 * вместе с эталоном и чек-листом. Хранится тем же способом, что занятия
 * и активная попытка.
 */
const SCENARIOS_KEY = 'arm112.mock.scenarios';

function readSavedScenarios(): Scenario[] {
  try {
    const raw = sessionStorage.getItem(SCENARIOS_KEY);
    const parsed: unknown = raw ? JSON.parse(raw) : [];
    return Array.isArray(parsed) ? (parsed as Scenario[]) : [];
  } catch {
    return [];
  }
}

/** Запоминает сценарий целиком: и новый, и правку существующего. */
export function persistScenario(scenario: Scenario): void {
  const all = readSavedScenarios().filter((x) => x.id !== scenario.id);
  all.push(JSON.parse(JSON.stringify(scenario)) as Scenario);
  try {
    sessionStorage.setItem(SCENARIOS_KEY, JSON.stringify(all));
  } catch {
    // хранилище недоступно: сценарий живёт до перезагрузки
  }
}

function restoreSavedScenarios(): void {
  for (const saved of readSavedScenarios()) {
    const index = db.scenarios.findIndex((x) => x.id === saved.id);
    if (index >= 0) db.scenarios[index] = saved;
    else db.scenarios.unshift(saved);
  }
}

/*
 * Сохранённое состояние важнее фикстуры: запущенное или завершённое
 * демо-занятие иначе после F5 возвращалось бы в исходный статус, и одно
 * и то же занятие выглядело бы на разных экранах по-разному.
 */
function restoreCreatedLessons(): void {
  for (const lesson of readCreatedLessons()) {
    const index = db.lessons.findIndex((l) => l.id === lesson.id);
    if (index >= 0) db.lessons[index] = lesson;
    else db.lessons.push(lesson);
  }
}

function restoreAttemptRuntime(attempt: Attempt): void {
  if (restoreCompletedAttempt(attempt)) return;
  const saved = readPersisted()[attempt.id];
  if (!saved) return;

  attempt.status = saved.status;
  attempt.callAcceptedAt = saved.callAcceptedAt;
  attempt.firstInputAt = saved.firstInputAt;
  attempt.replayCount = saved.replayCount;
  if (saved.draft) db.drafts[attempt.id] = saved.draft;
  if (saved.events) db.events[attempt.id] = saved.events;
  if (saved.dialogue) restoreDialogue(attempt, saved.dialogue);
}

function restoreDialogue(attempt: Attempt, record: DialogueRecord): void {
  // Записи голоса жили в памяти прошлой вкладки: их ссылки после F5 мертвы.
  for (const turn of record.turns) {
    if (turn.audio?.audioUrl.startsWith('blob:')) delete turn.audio;
  }
  db.dialogues[attempt.id] = record;
  attempt.dialogue = {
    turnsCount: record.turns.length,
    callEnded: record.callEnded,
    callEndedAt: record.callEndedAt,
  };
}

// ───────────────────────────────────────── завершённые попытки и комментарии

/**
 * Сданная или закрытая попытка с оценкой. На сервере это строки `attempts`,
 * `evaluations`, `attempt_events`; здесь — запись в sessionStorage, чтобы
 * результат и отчёт не пропадали после F5 так же, как не пропадают в БД.
 */
interface CompletedAttempt {
  attempt: Pick<Attempt, 'status' | 'callAcceptedAt' | 'firstInputAt' | 'submittedAt' | 'timeSpentMs' | 'replayCount' | 'card'>;
  evaluation?: Evaluation;
  events?: AttemptEventInput[];
  dialogue?: DialogueRecord;
}

const COMPLETED_KEY = 'arm112.mock.completedAttempts';
const FEEDBACK_KEY = 'arm112.mock.feedback';

function readCompleted(): Record<string, CompletedAttempt> {
  try {
    const raw = sessionStorage.getItem(COMPLETED_KEY);
    const parsed: unknown = raw ? JSON.parse(raw) : {};
    return parsed && typeof parsed === 'object' ? (parsed as Record<string, CompletedAttempt>) : {};
  } catch {
    return {};
  }
}

export function persistCompletedAttempt(attempt: Attempt): void {
  const all = readCompleted();
  all[attempt.id] = {
    attempt: {
      status: attempt.status,
      callAcceptedAt: attempt.callAcceptedAt,
      firstInputAt: attempt.firstInputAt,
      submittedAt: attempt.submittedAt,
      timeSpentMs: attempt.timeSpentMs,
      replayCount: attempt.replayCount,
      card: attempt.card,
    },
    evaluation: db.evaluations[attempt.id],
    events: db.events[attempt.id],
    dialogue: db.dialogues[attempt.id],
  };
  try {
    sessionStorage.setItem(COMPLETED_KEY, JSON.stringify(all));
  } catch {
    // хранилище недоступно: результат живёт до перезагрузки
  }
}

function restoreCompletedAttempt(attempt: Attempt): boolean {
  const saved = readCompleted()[attempt.id];
  if (!saved) return false;
  Object.assign(attempt, saved.attempt);
  if (saved.attempt.card) db.drafts[attempt.id] = saved.attempt.card;
  if (saved.events) db.events[attempt.id] = saved.events;
  if (saved.dialogue) restoreDialogue(attempt, saved.dialogue);
  if (saved.evaluation) db.evaluations[attempt.id] = saved.evaluation;

  // Перезагрузка пришлась на расчёт AI-слоёв: досчитываем, а не зависаем в «проверяется».
  const ev = db.evaluations[attempt.id];
  const scenario = scenarioById(attempt.scenarioId);
  const lesson = lessonById(attempt.lessonId);
  if (attempt.status === 'evaluating' && ev && scenario && lesson && attempt.card) {
    db.evaluations[attempt.id] = evaluateComplete(ev, attempt.card, scenario, lesson);
    attempt.status = 'evaluated';
    persistCompletedAttempt(attempt);
  }
  return true;
}

export function persistFeedback(): void {
  try {
    sessionStorage.setItem(FEEDBACK_KEY, JSON.stringify(db.feedback));
  } catch {
    // хранилище недоступно: комментарий живёт до перезагрузки
  }
}

function restoreFeedback(): void {
  try {
    const raw = sessionStorage.getItem(FEEDBACK_KEY);
    if (raw) db.feedback = JSON.parse(raw) as TeacherFeedback[];
  } catch {
    // повреждённая запись — начинаем с пустого списка
  }
}

/**
 * FIXTURE: оценка разговора.
 *
 * В проде этот слой считает ai-service: чек-лист протокола и тон — языковой
 * моделью, речевые метрики — детерминированно. Здесь детерминированно всё:
 * пункт считается выполненным, если в репликах оператора встретилась одна из
 * подсказок пункта. Этого хватает, чтобы собрать и проверить интерфейс.
 */
/** Значимые слова пункта чек-листа: служебные и короткие отбрасываем. */
function meaningfulWords(text: string): string[] {
  const stop = ['уточнить', 'спросить', 'сообщить', 'выяснить', 'задать', 'нужно', 'чтобы', 'какие', 'какой'];
  return text
    .toLowerCase()
    .split(/[^а-яё]+/i)
    .filter((w) => w.length >= 5 && !stop.includes(w));
}

function computeDialogue(attemptId: string, scenario: Scenario): DialogueResult | undefined {
  const record = db.dialogues[attemptId];
  const expected = scenario.expectedDialogue;
  if (!record || !expected) return undefined;

  const operator = record.turns.filter((t) => t.speaker === 'operator');
  const checklist = expected.checklist.map((item) => {
    /*
     * Ключевые слова задаёт преподаватель. Если он их не указал, ищем по
     * значимым словам самого пункта: иначе чек-лист, составленный вручную,
     * нельзя было бы выполнить ни одной репликой. В проде соответствие
     * определяет языковая модель, здесь — прямое совпадение.
     */
    const hints = item.hints?.length ? item.hints : meaningfulWords(item.text);
    const evidence = operator.find((turn) =>
      hints.some((hint) => turn.text.toLowerCase().includes(hint.toLowerCase())),
    );
    return {
      id: item.id,
      text: item.text,
      kind: item.kind,
      required: item.required,
      status: evidence ? ('done' as const) : ('missed' as const),
      evidenceTurnNo: evidence?.turnNo,
    };
  });

  const totalWeight = expected.checklist.reduce((acc, item) => acc + (item.weight ?? 1), 0) || 1;
  const doneWeight = expected.checklist.reduce(
    (acc, item) =>
      acc + (checklist.find((c) => c.id === item.id)?.status === 'done' ? item.weight ?? 1 : 0),
    0,
  );
  const score = Math.round((doneWeight / totalWeight) * 100);

  const words = operator.reduce((acc, t) => acc + t.text.trim().split(/\s+/).filter(Boolean).length, 0);
  const spanMs = operator.length > 0 ? Math.max(1, operator[operator.length - 1].atMs) : 0;
  const wordsPerMin = spanMs > 0 ? Math.round((words / (spanMs / 60000)) * 10) / 10 : 0;

  const fillers: Record<string, number> = {};
  for (const turn of operator) {
    const text = ` ${turn.text.toLowerCase()} `;
    for (const filler of FILLER_WORDS) {
      const count = text.split(` ${filler} `).length - 1;
      if (count > 0) fillers[filler] = (fillers[filler] ?? 0) + count;
    }
  }
  const fillerCount = Object.values(fillers).reduce((a, b) => a + b, 0);

  // Пауза перед ответом оператора: от реплики заявителя до следующей его реплики.
  const gaps: number[] = [];
  record.turns.forEach((turn, i) => {
    if (turn.speaker !== 'operator' || i === 0) return;
    const prev = record.turns[i - 1];
    if (prev.speaker === 'caller') gaps.push(Math.max(0, turn.atMs - prev.atMs));
  });
  const avgResponseMs = gaps.length ? Math.round(gaps.reduce((a, b) => a + b, 0) / gaps.length) : 0;

  const forbiddenHits = (expected.forbidden ?? []).flatMap((phrase) => {
    const hit = operator.find((t) => t.text.toLowerCase().includes(phrase.toLowerCase()));
    return hit ? [{ phrase, turnNo: hit.turnNo }] : [];
  });

  const missingQuestions = checklist
    .filter((item) => item.status === 'missed' && item.required)
    .map((item) => item.text);

  const polite = operator.some((t) => /пожалуйста|спасибо|будьте добры/i.test(t.text));
  const shouting = operator.some((t) => t.text === t.text.toUpperCase() && t.text.length > 10);

  return {
    score,
    // Короткий разговор — недостаточно материала: помечаем как требующий ревью.
    confidence: operator.length >= 3 ? 0.82 : 0.55,
    checklist,
    missingQuestions,
    forbiddenHits,
    speech: {
      operatorTurns: operator.length,
      operatorWords: words,
      wordsPerMin,
      fillerCount,
      fillers,
      avgResponseMs,
      maxResponseMs: gaps.length ? Math.max(...gaps) : 0,
      lowConfidenceTurns: operator.filter((t) => (t.confidence ?? 1) < 0.6).length,
    },
    tone: {
      politeness: polite ? 90 : 65,
      calmness: shouting ? 55 : 85,
      clarity: fillerCount > 3 ? 60 : 85,
      comment: polite
        ? 'Обращение вежливое, формулировки понятные.'
        : 'Не хватает вежливых формулировок при обращении к заявителю.',
    },
    summaryForStudent:
      missingQuestions.length === 0
        ? 'Протокол опроса выполнен: все обязательные вопросы заданы.'
        : `Не задано обязательных вопросов: ${missingQuestions.length}.`,
  };
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
restoreSavedScenarios();
restoreCreatedLessons();
restoreFeedback();
bootstrapRunningLessons();
