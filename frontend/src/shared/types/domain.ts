/**
 * Доменные типы frontend.
 *
 * Источники:
 *   1. `db/migrations/00001_init.sql` — перечисления взяты из CHECK-констрейнтов.
 *   2. `contracts/openapi/_components.yaml` — см. ./contracts.ts.
 *   3. Расширения, которых нет ни там, ни там — помечены TODO(backend) c id GAP
 *      из раздела 12-bis плана. Их нужно согласовать до реализации go-core.
 */

import type { Address, CallScript, GrammarResult, IncidentCard, SemanticResult, Score } from './contracts';

// ─────────────────────────────────────────────────────────── перечисления (SQL)

export type Role = 'admin' | 'teacher' | 'student';
export type UserStatus = 'active' | 'blocked';

export type ScenarioStatus = 'draft' | 'generated' | 'validated' | 'rejected' | 'archived';
export type ScenarioSource = 'generated' | 'manual' | 'ticket' | 'student';

export type LessonKind = 'class' | 'practice';
export type LessonMode = 'cards' | 'card_actions';
export type LessonStatus = 'draft' | 'scheduled' | 'running' | 'finished' | 'cancelled';
export type ParticipantStatus = 'assigned' | 'joined' | 'active' | 'disconnected' | 'finished';

export type AttemptStatus =
  | 'issued'
  | 'in_progress'
  | 'submitted'
  | 'evaluating'
  | 'evaluated'
  | 'expired'
  | 'aborted';

export type EvaluationStatus = 'pending' | 'partial' | 'done' | 'failed';
export type Verdict = 'pending' | 'pass' | 'fail';

export type Difficulty = 1 | 2 | 3;
export type ServiceKind = 'emergency' | 'city';

/**
 * TODO(backend) GAP-07 / B-14: перспективы рабочего места в контракте нет.
 * Предлагается хранить в `lessons.settings.perspective` (jsonb, без миграции).
 */
export type ArmPerspective = 'operator112' | 'dds';

// ─────────────────────────────────────────────────────────────────── сущности

export interface User {
  id: string;
  login: string;
  role: Role;
  lastName: string;
  firstName: string;
  middleName?: string;
  /** профиль ДДС обучающегося (users.service_id); NULL — универсальный */
  serviceId?: string;
  serviceName?: string;
  status: UserStatus;
  /** номер АРМ для шапки рабочего места */
  workstation?: string;
  /** операторский номер, отображается в карточке */
  operatorNo?: string;
}

export interface ServiceRef {
  id: string;
  /** TODO(backend) GAP-08: сид в SQL — «01».., реальный АРМ — «101».. */
  code: string;
  name: string;
  shortName: string;
  kind: ServiceKind;
}

/** Уровень классификатора происшествий (classifier_categories) */
export interface IncidentType {
  id: string;
  code: string;
  name: string;
  /** 1 = раздел, 2 = категория, 3 = подкатегория */
  depth: number;
  /** синонимы для поиска (Инструкция п.4.2: «пожар» → «101») */
  synonyms: string[];
  /** есть ли справочная страница: сплошное подчёркивание vs пунктир */
  hasReference: boolean;
}

/**
 * TODO(backend) GAP-20 / B-04: структуры метаданных признака в контракте нет.
 * `IncidentCard.attributes` — `additionalProperties: true`; валидация обещана
 * «данными classifier_categories.attributes на границе go-core» (docs/contracts.md).
 */
export type AttributeWidget = 'chips-single' | 'chips-multi' | 'bool' | 'text' | 'number';

export interface AttributeOption {
  code: string;
  label: string;
}

export interface AttributeGroup {
  code: string;
  label: string;
  widget: AttributeWidget;
  options?: AttributeOption[];
  required?: boolean;
  /** условие показа: группа видна, если у признака `attr` выбрано одно из `anyOf` */
  visibleWhen?: { attr: string; anyOf: string[] };
  order: number;
}

export type AttributeValue = string | string[] | boolean | number | null;

// ───────────────────────────────────────────────── карточка происшествия (P0)

/**
 * TODO(backend) GAP-01..GAP-06.
 * Контрактный `IncidentCard` заметно проще реальной карточки АРМ-112:
 *   GAP-02  categoryCode: string  → нужен массив (Инструкция п.4.2, мультивыбор)
 *   GAP-03  applicant без статуса → статус обязателен (Инструкция п.5.3)
 *   GAP-04  один телефон          → в АРМ три (АОН / предоставленный / на место)
 *   GAP-05  Address 8 полей       → в АРМ 15 + координаты + источник
 *   GAP-06  casualties            → не покрывает флаги карточки
 *   GAP-01  servicesToNotify      → плоские коды, статусов реагирования НЕТ
 * Здесь — расширение. Поля, совпадающие с контрактом, названы так же.
 */
export interface IncidentCardDraft {
  /** GAP-04 */
  phones: {
    aon?: string;
    provided?: string;
    onSite?: string;
    foreign?: boolean;
    channel?: string;
  };
  /** GAP-03 */
  applicant: {
    name?: string;
    status?: ApplicantStatus;
    foreignLanguage?: boolean;
  };
  /** GAP-05 */
  address: AddressDraft;
  /** GAP-02 */
  incidentTypeIds: string[];
  attributes: Record<string, AttributeValue>;
  /** «Описание со слов заявителя», максимум 1999 символов */
  description: string;
  /** Текст действия оператора (режим card_actions; в P0 — доп. поле) */
  actionsTaken: string;
  /** GAP-06 */
  flags: {
    victimsPresent: boolean;
    victimsCount?: number;
    ambulanceRefusal: boolean;
    blocked: boolean;
    noContact: boolean;
    callDropped: boolean;
  };
  /** GAP-01 */
  services: AssignedService[];
}

/** Инструкция п.5.3 — точный закрытый список */
export const APPLICANT_STATUSES = [
  'очевидец',
  'пострадавший',
  'родственник',
  'знакомый',
  'ребенок',
  'участник',
] as const;

export type ApplicantStatus = (typeof APPLICANT_STATUSES)[number];

/** GAP-05: полный адрес реального АРМ. Контрактный `Address` — подмножество. */
export interface AddressDraft {
  /** единая адресная строка, как её ввёл оператор */
  raw: string;
  country: string;
  region: string;
  settlement: string;
  object: string;
  okrug: string;
  district: string;
  street: string;
  house: string;
  building: string;
  structure: string;
  apartment: string;
  entrance: string;
  floor: string;
  code: string;
  /** описательный адрес */
  descriptive: string;
  lat?: number;
  lon?: number;
  /** Инструкция п.4.3: при источнике «fias» службы автоматически НЕ добавляются */
  source?: AddressSource;
}

export type AddressSource = 'yandex_map' | 'yandex_org' | 'fias' | 'map_pick' | 'manual';

export interface AddressSuggestion {
  id: string;
  label: string;
  source: AddressSource;
  value: AddressDraft;
}

// ──────────────────────────────────────────────── статусы реагирования (GAP-01)

/**
 * Источник: docs/Работа с АРМ-112 для ДДС от ОКр_ГСИ.pdf, таблица «Статусы
 * реагирования служб». Подтверждено скриншотами СКРИНШОТ ДДСГСИ.docx.
 *
 * TODO(backend) B-07 / GAP-01: в контракте этой модели нет вообще.
 * Frontend НЕ является источником истины: `allowedNext` приходит с backend,
 * mock-реализация в shared/mocks/reactionTransitions.ts помечена как FIXTURE.
 */
export const REACTION_STATUSES = [
  'Добавлена',
  'Получена службой',
  'Принята',
  'Не принята',
  'Начало реагирования',
  'Прибытие',
  'Проведение работ',
  'Работы завершены',
  'Отказ от выполнения работ',
] as const;

export type ReactionStatus = (typeof REACTION_STATUSES)[number];

/**
 * Статусы, которые проставляет система, а не диспетчер.
 *
 * Памятка ДДС: «Добавлена» ставится при направлении карточки, «Получена
 * службой» — при её поступлении на сервер службы. В выпадающем списке
 * диспетчера их быть не должно.
 */
export const SYSTEM_REACTION_STATUSES: ReactionStatus[] = ['Добавлена', 'Получена службой'];

export interface ReactionStatusEntry {
  status: ReactionStatus;
  at: string;
  /** номер оператора («оп. 227»), «система» для автоматических статусов */
  operator: string;
  squadNumber?: string;
  comment?: string;
}

/** Вариант перехода, разрешённый backend. Фронт рисует строго это. */
export interface AllowedTransition {
  status: ReactionStatus;
  label: string;
  commentRequired: boolean;
  squadNumberRequired: boolean;
}

export interface AssignedService {
  serviceId: string;
  code: string;
  name: string;
  shortName: string;
  /** Инструкция п.8: основные службы подчёркиваются двойной линией */
  isPrimary: boolean;
  source: 'auto' | 'manual' | 'vis';
  /** почему служба определилась — для разбора у преподавателя */
  reason?: string;
  currentStatus: ReactionStatus;
  currentStatusAt: string;
  history: ReactionStatusEntry[];
  /** единственный источник допустимых переходов */
  allowedNext: AllowedTransition[];
  /** false после терминального статуса — карточка закрыта для редактирования */
  editable: boolean;
}

// ──────────────────────────────────────────────────── сценарии, занятия, попытки

export interface Scenario {
  id: string;
  title: string;
  categoryId: string;
  categoryName: string;
  difficulty: Difficulty;
  mode: LessonMode | 'both';
  source: ScenarioSource;
  status: ScenarioStatus;
  callScript: CallScript;
  etalonCard: IncidentCard;
  /** расширенный эталон для сверки карточки студента */
  etalonDraft: IncidentCardDraft;
  requiredFields: string[];
  teacherComment?: string;
  notesForTeacher?: string;
  authorId?: string;
  validatedBy?: string;
  validatedAt?: string;
  createdAt: string;
  etalonVersion: number;
}

export interface Lesson {
  id: string;
  kind: LessonKind;
  title: string;
  teacherId: string;
  mode: LessonMode;
  /** TODO(backend) GAP-07 */
  perspective: ArmPerspective;
  difficulty?: Difficulty;
  timeLimitSec: number;
  status: LessonStatus;
  scenarioIds: string[];
  participants: LessonParticipant[];
  settings: LessonSettings;
  createdAt: string;
  startedAt?: string;
  finishedAt?: string;
}

export interface LessonSettings {
  passThreshold: number;
  weights: { fields: number; semantic: number; grammar: number; timing: number };
  cardsPerStudent: number;
  allowReplay: boolean;
}

export interface LessonParticipant {
  userId: string;
  name: string;
  status: ParticipantStatus;
  attemptId?: string;
  joinedAt?: string;
  finishedAt?: string;
}

export interface Attempt {
  id: string;
  lessonId: string;
  lessonTitle: string;
  userId: string;
  scenarioId: string;
  mode: LessonMode;
  perspective: ArmPerspective;
  seqNo: number;
  status: AttemptStatus;
  timeLimitSec: number;
  issuedAt: string;
  callAcceptedAt?: string;
  firstInputAt?: string;
  submittedAt?: string;
  timeSpentMs?: number;
  replayCount: number;
  card?: IncidentCardDraft;
  /** номер карточки происшествия, как в реальном АРМ */
  incidentNo: string;
  /** серверное время на момент ответа — база для расчёта таймера */
  serverNow: string;
}

/** Урезанный call script для студента: без keyFacts (GAP-11). */
export interface StudentCallScript {
  caller: CallScript['caller'];
  turns: CallTurnView[];
  allowReplay: boolean;
}

export interface CallTurnView {
  index: number;
  speaker: 'caller' | 'operator_hint';
  text: string;
  /** TODO(backend) GAP-10/GAP-15: контракт даёт только ttsHash, не URL и не длительность */
  audioUrl?: string;
  durationMs?: number;
}

// ─────────────────────────────────────────────────────────────────── события

export type AttemptEventType =
  | 'issued'
  | 'call_accepted'
  | 'open_card'
  | 'field_changed'
  | 'choose_value'
  | 'service_assigned'
  /** TODO(backend) снятие службы вручную — тип события фронта, требует согласования */
  | 'service_removed'
  | 'service_status_changed'
  | 'replay'
  | 'save'
  | 'submitted'
  | 'timer_expired'
  | 'disconnected'
  | 'reconnected';

export interface AttemptEvent {
  clientSeq: number;
  type: AttemptEventType;
  payload?: Record<string, unknown>;
  at: string;
}

// ─────────────────────────────────────────────────────────────────── оценка

export interface FieldError {
  field: string;
  label: string;
  expected?: unknown;
  actual?: unknown;
  kind: 'missing' | 'wrong' | 'extra';
  weight: number;
}

export interface Recommendation {
  id: string;
  kind: 'weak_category' | 'weak_field' | 'slow_timing' | 'grammar_pattern' | 'general';
  body: string;
  /**
   * Перечисление, относящееся к рекомендации: названия незаполненных полей
   * и подобное. Списком, а не строкой через запятую: «Адрес, Где повреждение»
   * читается как одно поле, хотя это два.
   */
  items?: string[];
}

export interface TeacherFeedback {
  id: string;
  attemptId: string;
  teacherName: string;
  field?: string;
  comment: string;
  recommendation?: string;
  createdAt: string;
}

export interface Evaluation {
  attemptId: string;
  /** docs/contracts.md: поля и тайминг считаются мгновенно → partial */
  status: EvaluationStatus;
  fieldsScore?: Score;
  timingScore?: Score;
  grammarScore?: Score;
  semanticScore?: Score;
  totalScore: number;
  verdict: Verdict;
  fieldErrors: FieldError[];
  grammar?: GrammarResult;
  semantic?: SemanticResult;
  timing: {
    spentMs: number;
    limitSec: number;
    deltaMs: number;
    withinNorm: boolean;
    reactionMs: number;
  };
  /** docs/contracts.md: confidence < 0.7 → «требует ревью преподавателя» */
  needsReview: boolean;
  /** poison-pill: try_count ≥ max_tries → «AI-слой недоступен» */
  aiUnavailable: boolean;
  recommendations: Recommendation[];
  override?: {
    score: number;
    verdict: Verdict;
    reason: string;
    by: string;
    at: string;
  };
  finalScore: number;
}

export type { Address, CallScript, IncidentCard, GrammarResult, SemanticResult, Score };
