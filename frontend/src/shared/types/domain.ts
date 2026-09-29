/**
 * Доменные типы frontend.
 *
 * Источники:
 *   1. `db/migrations/00001_init.sql` — перечисления взяты из CHECK-констрейнтов.
 *   2. `contracts/openapi/_components.yaml` — см. ./contracts.ts.
 *   3. Расширения, которых нет ни там, ни там — помечены TODO(backend) c id GAP
 *      из раздела 12-bis плана. Их нужно согласовать до реализации go-core.
 */

import type { Address, CallScript, Engine, ExpectedAction, GrammarResult, IncidentCard, Scoring, SemanticResult, Score } from './contracts';

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

/** Рабочее место обучающегося: оператор-112 или диспетчер ДДС (frontend.v1.yaml). */
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
  /** учебные группы пользователя */
  groupIds?: string[];
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
  /** родитель в дереве классификатора */
  parentId?: string;
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

/** Фоновая AI-задача (frontend.v1.yaml → AiJob). */
export type AiJobType = 'evaluate_grammar' | 'evaluate_semantic' | 'evaluate_dialogue' | 'generate_scenario' | 'tts';
export type AiJobStatus = 'queued' | 'running' | 'done' | 'failed' | 'cancelled';
export interface AiJob {
  id: string;
  type: AiJobType;
  status: AiJobStatus;
  refType?: string;
  refId?: string;
  tryCount?: number;
  error?: string;
  queuePosition?: number;
  estWaitSec?: number;
  createdAt: string;
  finishedAt?: string;
}

export interface Scenario {
  id: string;
  title: string;
  categoryId: string;
  categoryName: string;
  difficulty: Difficulty;
  mode: LessonMode | 'both';
  source: ScenarioSource;
  status: ScenarioStatus;
  /**
   * Легенда звонка. `dialogue` — бриф ИИ-заявителя: он определяет, что и когда
   * заявитель готов рассказать. Преподавательские данные: обучающемуся не
   * отдаются ни бриф, ни keyFacts (см. StudentCallScript).
   */
  callScript: CallScript & { dialogue?: DialogueBrief };
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
  /** чек-лист протокола опроса — по нему оценивается разговор */
  expectedDialogue?: ExpectedDialogue;
  /** модель и версия промпта генерации — для контроля качества */
  generationMeta?: Record<string, unknown>;
  /** все реплики озвучены и лежат в кэше */
  ttsReady?: boolean;
  /** переопределение весов и обязательных полей для сценария */
  scoring?: Scoring;
  /** эталон режима «действия с карточками» */
  expectedActions?: ExpectedAction[];
  /*
   * Версии сценария. Решение backend по lifecycle: сценарий, который уже
   * используется в занятиях, не правится — от него создаётся новая версия
   * (POST /scenarios/{id}/versions). Поля — frontend.v1.yaml v1.2.
   */
  /** используется хотя бы в одном занятии — признак отдаёт сервер, не статус */
  inUse?: boolean;
  /** число занятий, где используется сценарий (для сообщения об ошибке) */
  lessonsCount?: number;
  /** номер версии в цепочке; у исходного сценария — 1 */
  version?: number;
  /** сценарий, копией которого создана эта версия */
  parentScenarioId?: string;
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

/** `frontend.v1.yaml#/VoiceSettings` — голосовой режим занятия. */
export type VoiceInput = 'voice' | 'text' | 'both';

export interface VoiceSettings {
  /** интерактивный разговор с ИИ-заявителем вместо сценарных реплик */
  enabled: boolean;
  /** voice — микрофон; text — реплики текстом; both — на выбор обучающегося */
  input: VoiceInput;
  /** true — удерживать кнопку; false — «клик-старт / клик-стоп» */
  pushToTalk: boolean;
  maxTurns: number;
  ttsEnabled: boolean;
}

export interface LessonSettings {
  passThreshold: number;
  /**
   * Сумма = 1. При `voice.enabled = false` вес разговора равен нулю,
   * и итог считается по оставшимся слоям.
   */
  weights: { fields: number; semantic: number; grammar: number; timing: number; dialogue: number };
  cardsPerStudent: number;
  allowReplay: boolean;
  voice: VoiceSettings;
  /**
   * v1.4, «действия с карточками»: пул занятия — из сгенерированных системой
   * карточек, из сформированных обучающимися или смешанный. Нет — mixed.
   */
  cardSource?: 'generated' | 'student' | 'mixed';
}

export interface LessonParticipant {
  userId: string;
  name: string;
  status: ParticipantStatus;
  /** прошёл проверку микрофона (событие mic_check) */
  micReady?: boolean;
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
  /**
   * v1.3, только ракурс dds: служба, за диспетчера которой работает обучающийся
   * (профиль ДДС или основная служба карточки). Ракурс определяется по попытке,
   * а не по занятию: попытки, выданные раньше, работают по-старому.
   */
  actingService?: ServiceRef;
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
  /** режим разговора этой попытки (копия настроек занятия) */
  voice: VoiceSettings;
  /** краткое состояние разговора; полный транскрипт — отдельным запросом */
  dialogue?: {
    turnsCount: number;
    callEnded: boolean;
    callEndedAt?: string;
  };
}

/** Урезанный call script для студента: без keyFacts (GAP-11). */
export interface StudentCallScript {
  caller: CallScript['caller'];
  /**
   * В голосовом режиме — только вступительная реплика; дальнейшие приходят
   * в состоянии разговора. В текстовом — все сценарные реплики.
   */
  turns: CallTurnView[];
  allowReplay: boolean;
  voice: VoiceSettings;
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
  | 'reconnected'
  // ───────────────────────────────────── голосовой режим (frontend.v1.yaml)
  | 'mic_check'
  | 'ptt_start'
  | 'ptt_stop'
  | 'dialogue_operator'
  | 'dialogue_caller'
  | 'dialogue_ended';

/** Событие попытки, как его отправляет клиент (frontend.v1.yaml → AttemptEventInput). */
export interface AttemptEventInput {
  /** сквозной номер события попытки, начиная с 1; по нему сервер отбрасывает дубли */
  clientSeq: number;
  type: AttemptEventType;
  payload?: Record<string, unknown>;
  at: string;
}

/** Сохранённое событие: к клиентским полям сервер добавляет id. */
export interface AttemptEvent extends AttemptEventInput {
  id: number;
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
  kind:
    | 'weak_category'
    | 'weak_field'
    | 'slow_timing'
    | 'grammar_pattern'
    | 'dialogue_pattern'
    | 'general';
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

/** Состояние контура для администратора (frontend.v1.yaml v1.2: SystemHealth). */
export interface SystemHealth {
  status: 'ok' | 'degraded' | 'down';
  goCore: { version?: string; uptimeSec?: number; activeSessions?: number; wsConnections?: number };
  postgres: { ok?: boolean; latencyMs?: number; lastBackupAt?: string };
  aiService: {
    ok?: boolean;
    breakerState?: 'closed' | 'open' | 'half_open';
    ollama?: boolean;
    languagetool?: boolean;
    tts?: boolean;
    stt?: boolean;
    modelsAvailable?: string[];
    profiles?: Record<string, string>;
    queue?: { pending?: Record<string, number>; running?: number; dialogWaiting?: number; dialogAvgMs?: number; estWaitSec?: number };
  };
  /** задачи ai_jobs по статусам */
  jobs?: Record<string, number>;
}

/** Чем посчитаны AI-слои: модель и версии по каждому слою (camelCase, как отдаёт go-core). */
export interface EvaluationEngine {
  rulesVersion?: string;
  /** слой полей: `dds_reaction` — протокол реагирования диспетчера ДДС (v1.3) */
  fields?: { source?: string };
  grammar?: Engine;
  semantic?: Engine;
  dialogue?: Engine;
  /** код отказа слоя, если слой не доехал */
  errors?: Record<string, string>;
}

export interface Evaluation {
  attemptId: string;
  /** docs/contracts.md: поля и тайминг считаются мгновенно → partial */
  status: EvaluationStatus;
  fieldsScore?: Score;
  timingScore?: Score;
  grammarScore?: Score;
  semanticScore?: Score;
  dialogueScore?: Score;
  totalScore: number;
  verdict: Verdict;
  fieldErrors: FieldError[];
  grammar?: GrammarResult;
  semantic?: SemanticResult;
  dialogue?: DialogueResult;
  /**
   * Веса, по которым посчитан итог. Интерфейс показывает их, а не собственные
   * константы: занятие могло переопределить веса, и подпись «· 50%» обязана
   * совпадать с формулой.
   */
  weights: LessonSettings['weights'];
  /** состояние AI-слоёв для индикаторов «считается / не выполнено» */
  layers?: Partial<Record<EvaluationLayer, EvaluationLayerState>>;
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
  /** модели и версии, которыми считали слои (frontend.v1.yaml: Evaluation.engine) */
  engine?: EvaluationEngine;
  /** опыт, начисленный за попытку */
  xpEarned?: number;
}

// ──────────────────────────────────────────────────── разговор с заявителем

/** Ссылка на озвученную реплику (`frontend.v1.yaml#/AudioRef`). */
export interface AudioRef {
  /** относительный URL для воспроизведения; авторизация по сессии */
  audioUrl: string;
  durationMs: number;
  mime?: string;
}

export type DialogueSpeaker = 'operator' | 'caller';

/** Источник текста реплики: распознавание, ручной ввод, сценарий, модель. */
export type DialogueTurnSource = 'stt' | 'text' | 'script' | 'llm';

export interface DialogueTurnView {
  turnNo: number;
  speaker: DialogueSpeaker;
  text: string;
  /** миллисекунды от момента принятия вызова */
  atMs: number;
  at?: string;
  durationMs?: number;
  /** уверенность распознавания; только для реплик оператора */
  confidence?: number;
  source: DialogueTurnSource;
  audio?: AudioRef;
  /** только для заявителя — индикатор состояния в интерфейсе */
  emotionalState?: string;
}

export type DialogueEndReason =
  | 'operator_hung_up'
  | 'caller_hung_up'
  | 'max_turns'
  | 'submitted'
  | 'timeout';

export interface DialogueState {
  attemptId: string;
  turns: DialogueTurnView[];
  callEnded: boolean;
  callEndedAt?: string;
  endReason?: DialogueEndReason;
  input: VoiceInput;
  /** номер следующего хода — единственный источник нумерации для интерфейса */
  nextTurnNo: number;
  turnsLeft?: number;
}

export interface DialogueTurnResponse {
  turnNo: number;
  operator?: DialogueTurnView;
  caller?: DialogueTurnView;
  /** речь не распознана — реплика оператора не сохранена */
  noSpeech: boolean;
  callEnded: boolean;
  endReason?: DialogueEndReason;
  /** модель недоступна — ответ взят из сценарных реплик */
  fallback: boolean;
  nextTurnNo: number;
  latencyMs?: number;
}

// ─────────────────────────────────────── бриф заявителя и чек-лист протокола

/** Когда заявитель сообщает факт: сам, по вопросу или не сообщает никогда. */
export type FactReveal = 'volunteer' | 'on_request' | 'never';

export interface DialogueFact {
  id: string;
  text: string;
  reveal: FactReveal;
  /** слова, по которым модель понимает, что факт спрашивают */
  hints?: string[];
}

/** Преподавательские данные: обучающемуся не отдаются. */
export interface DialogueBrief {
  persona: string;
  speakingStyle?: string;
  facts: DialogueFact[];
  unknowns?: string[];
  endConditions?: string[];
  maxTurns?: number;
}

export type ChecklistItemKind = 'question' | 'instruction' | 'phrase' | 'behavior';

export interface DialogueChecklistItem {
  id: string;
  text: string;
  kind: ChecklistItemKind;
  required: boolean;
  weight?: number;
  hints?: string[];
}

export interface ExpectedDialogue {
  checklist: DialogueChecklistItem[];
  forbidden?: string[];
  maxOperatorTurns?: number;
}

// ──────────────────────────────────────────────────────── оценка разговора

export type ChecklistStatus = 'done' | 'partial' | 'missed' | 'not_applicable';

export interface DialogueChecklistResult {
  id: string;
  /** текст пункта подставляет сервер — интерфейс не ищет его по идентификатору */
  text: string;
  kind?: ChecklistItemKind;
  required?: boolean;
  status: ChecklistStatus;
  /** номер реплики, подтверждающей выполнение пункта */
  evidenceTurnNo?: number;
  comment?: string;
}

export interface DialogueResult {
  score: Score;
  confidence: number;
  checklist: DialogueChecklistResult[];
  missingQuestions?: string[];
  forbiddenHits?: Array<{ phrase: string; turnNo: number }>;
  speech: {
    operatorTurns: number;
    operatorWords: number;
    operatorTalkMs?: number;
    wordsPerMin?: number;
    fillerCount?: number;
    fillers?: Record<string, number>;
    avgResponseMs?: number;
    maxResponseMs?: number;
    lowConfidenceTurns?: number;
  };
  tone?: {
    politeness?: Score;
    calmness?: Score;
    clarity?: Score;
    comment?: string;
  };
  summaryForStudent?: string;
}

// ─────────────────────────────────────────────── состояние слоёв и ошибки

export type EvaluationLayer = 'fields' | 'timing' | 'grammar' | 'semantic' | 'dialogue';

export type EvaluationLayerState = 'queued' | 'running' | 'done' | 'failed' | 'skipped';

/**
 * Машинные коды ошибок сервисного слоя (`frontend.v1.yaml#/ApiError.code`).
 *
 * Интерфейс принимает решения по коду, а не по тексту сообщения: текст
 * показывается пользователю и может меняться, код — часть контракта.
 */
export type ApiErrorCode =
  | 'unauthorized'
  | 'forbidden'
  | 'not_found'
  | 'validation'
  | 'conflict'
  | 'user_blocked'
  | 'ai_unavailable'
  | 'caller_busy'
  | 'audio_too_long'
  | 'audio_unsupported'
  | 'rate_limited'
  | 'internal';

export type { Address, CallScript, IncidentCard, GrammarResult, SemanticResult, Score };
