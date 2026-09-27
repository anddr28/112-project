/**
 * Интерфейс сервисного слоя frontend.
 *
 * Компоненты знают ТОЛЬКО этот интерфейс. Сигнатуры следуют
 * `contracts/openapi/frontend.v1.yaml`; реализация — mock (`shared/mocks/mockApi.ts`)
 * либо HTTP, выбор в `shared/api/index.ts`. UI от выбора не зависит.
 *
 * Здесь НЕТ путей и HTTP-методов — они живут в реализации поверх fetch.
 */

import type {
  AddressSuggestion, AiJob, AllowedTransition, AssignedService, Attempt, AttemptEvent, AttemptEventInput,
  AttributeGroup, AttributeValue, Difficulty, DialogueState, DialogueTurnResponse,
  DialogueTurnView, Evaluation, IncidentCardDraft,
  IncidentType, Lesson, LessonMode, LessonSettings, ArmPerspective, ReactionStatus, Scenario,
  Role, ServiceRef, StudentCallScript, SystemHealth, TeacherFeedback, User, VoiceSettings,
} from '../types';

export interface LoginInput {
  login: string;
  password: string;
}

export interface DemoAccount {
  login: string;
  password: string;
  label: string;
}

export interface CreateScenarioInput {
  title: string;
  categoryId: string;
  difficulty: Difficulty;
  mode: LessonMode;
}

export interface GenerateScenarioInput {
  categoryId: string;
  difficulty: Difficulty;
  mode: LessonMode;
  teacherComment?: string;
  /** сгенерировать бриф заявителя и чек-лист разговора; по контракту true */
  withDialogue?: boolean;
}

/** Ответ 202 на запуск генерации: сценарий-черновик уже создан, задача в очереди. */
export interface GenerateScenarioAccepted {
  jobId: string;
  scenarioId: string;
  estWaitSec?: number;
}

export interface CreateLessonInput {
  title: string;
  mode: LessonMode;
  perspective: ArmPerspective;
  difficulty?: Difficulty;
  timeLimitSec: number;
  scenarioIds: string[];
  participantIds: string[];
  passThreshold: number;
  allowReplay: boolean;
  /** голосовой режим занятия; не передан — берутся параметры по умолчанию */
  voice?: VoiceSettings;
  /** переопределение весов слоёв; не передано — параметры по умолчанию */
  weights?: LessonSettings['weights'];
}

/**
 * Приём вызова.
 *
 * Вместе с попыткой приходит вступительная реплика заявителя: воспроизвести
 * её нужно тем же действием пользователя, которым он принял вызов, иначе
 * браузер заблокирует автовоспроизведение.
 */
export interface AcceptCallResult {
  attempt: Attempt;
  opening?: DialogueTurnView;
}

/**
 * Ход разговора: реплика оператора голосом или текстом.
 *
 * `turnNo` задаёт клиент — по нему сервер распознаёт повтор после обрыва
 * связи и не проводит один и тот же ход дважды.
 */
export interface DialogueTurnInput {
  attemptId: string;
  turnNo: number;
  /** текстовый режим и запасной вариант при отказе микрофона */
  text?: string;
  /** запись реплики; текст при этом — расшифровка, проверенная обучающимся */
  audio?: Blob;
  clientRecordedAt?: string;
}

/**
 * Словарь подписей для отображения технических кодов.
 *
 * Нужен везде, где в интерфейс попадают пути полей карточки и коды признаков:
 * список обязательных полей, ошибки проверки, хронология действий.
 */
export interface ClassifierLabels {
  /** путь поля карточки → подпись: «applicant.name» → «ФИО заявителя» */
  fields: Record<string, string>;
  /** код признака → название группы: «where» → «Где» */
  attributes: Record<string, string>;
  /** код признака → { код значения → подпись }: «where».«house» → «Дом» */
  values: Record<string, Record<string, string>>;
  /** идентификатор типа происшествия → название */
  types: Record<string, string>;
}

export interface ResolvedServicesResult {
  services: AssignedService[];
  /** Инструкция п.4.3: у адреса из ФИАС службы автоматически не подставляются */
  suppressedByAddressSource: boolean;
}

export interface ChangeStatusInput {
  attemptId: string;
  serviceId: string;
  status: ReactionStatus;
  squadNumber?: string;
  comment?: string;
}

export interface Api {
  auth: {
    login(input: LoginInput): Promise<User>;
    me(): Promise<User | null>;
    logout(): Promise<void>;
    /** Учебные учётные записи для демонстрации. В боевом контуре вернёт []. */
    demoAccounts(): Promise<DemoAccount[]>;
  };

  classifier: {
    incidentTypes(): Promise<IncidentType[]>;
    /**
     * Поиск типа происшествия. Правила поиска (часть слова, синонимы, любой
     * порядок слов) принадлежат классификатору, а не интерфейсу.
     */
    searchTypes(query: string): Promise<IncidentType[]>;
    featured(): Promise<{ frequent: IncidentType[]; significant: IncidentType[] }>;
    attributes(typeId: string): Promise<AttributeGroup[]>;
    reference(typeId: string): Promise<{ exists: boolean; text?: string }>;
    /** Подписи для технических кодов, попадающих в интерфейс. */
    labels(): Promise<ClassifierLabels>;
    resolveServices(input: {
      typeIds: string[];
      attributes: Record<string, AttributeValue>;
      addressFilled: boolean;
      addressSource?: string;
      current: AssignedService[];
    }): Promise<ResolvedServicesResult>;
  };

  services: {
    list(): Promise<ServiceRef[]>;
  };

  users: {
    /** Весь список: сервер отдаёт страницами (v1.2, X-Next-Cursor). */
    list(): Promise<User[]>;
    /**
     * Блокировка и разблокировка учётной записи (J-04 / J-05).
     *
     * Причина не требуется: это техническая операция администратора.
     * Заблокированный пользователь не может войти, а его действующая сессия
     * перестаёт быть валидной при следующей проверке.
     */
    setBlocked(userId: string, blocked: boolean): Promise<User>;
    /**
     * Создание учётной записи (POST /users, только admin). Пароль хэширует сервер.
     * У mock-реализации метода нет: учётки mock — фикстуры.
     */
    create?(input: CreateUserInput): Promise<User>;
  };

  address: {
    suggest(query: string): Promise<AddressSuggestion[]>;
  };

  aiJobs: {
    /** Состояние фоновой AI-задачи (генерация сценария и т. п.). */
    get(jobId: string): Promise<AiJob>;
  };

  scenarios: {
    list(): Promise<Scenario[]>;
    get(id: string): Promise<Scenario>;
    create(input: CreateScenarioInput): Promise<Scenario>;
    /** Асинхронно: 202 → опрос `aiJobs.get(jobId)` до done/failed (минуты на реальном ai-service). */
    generate(input: GenerateScenarioInput): Promise<GenerateScenarioAccepted>;
    /** 409 `conflict`, если сценарий уже используется в занятиях. */
    update(id: string, patch: Partial<Scenario>): Promise<Scenario>;
    /**
     * Новая версия (draft) — копия сценария со ссылкой на родителя; родитель
     * не меняется. POST /scenarios/{id}/versions — frontend.v1.yaml v1.2.
     */
    createVersion(id: string): Promise<Scenario>;
    approve(id: string): Promise<Scenario>;
    reject(id: string, reason: string): Promise<Scenario>;
  };

  lessons: {
    /**
     * Параметры занятия по умолчанию.
     *
     * Нужны форме создания, чтобы веса слоёв показывались те же, по которым
     * потом считается балл, а не продублированные числом в разметке.
     */
    defaultSettings(): Promise<LessonSettings>;
    list(): Promise<Lesson[]>;
    get(id: string): Promise<Lesson>;
    create(input: CreateLessonInput): Promise<Lesson>;
    start(id: string): Promise<Lesson>;
    finish(id: string): Promise<Lesson>;
    /**
     * Адрес выгрузки отчёта по занятию (v1.2: CSV / Excel / PDF). Файл формирует
     * сервер; у mock-реализации метода нет — кнопки выгрузки не показываются.
     */
    reportUrl?(lessonId: string, format: ReportFormat): string;
    /** Занятия текущего обучающегося — пользователь определяется по сессии. */
    assigned(): Promise<Array<{ lesson: Lesson; attempt?: Attempt }>>;
  };

  attempts: {
    get(id: string): Promise<Attempt>;
    forLesson(lessonId: string): Promise<Attempt[]>;
    acceptCall(id: string): Promise<AcceptCallResult>;
    getDraft(id: string): Promise<IncidentCardDraft>;
    updateDraft(id: string, card: IncidentCardDraft): Promise<{ savedAt: string }>;
    /** Пачка событий (до 200). Дубли по clientSeq сервер отбрасывает молча. */
    postEvents(id: string, events: AttemptEventInput[]): Promise<{ accepted: number; lastSeq: number }>;
    events(id: string): Promise<AttemptEvent[]>;
    changeServiceStatus(input: ChangeStatusInput): Promise<AssignedService>;
    addService(id: string, serviceCode: string): Promise<AssignedService>;
    removeService(id: string, serviceId: string): Promise<void>;
    replay(id: string): Promise<{ replayCount: number }>;
    submit(id: string, card: IncidentCardDraft): Promise<Attempt>;
  };

  evaluation: {
    get(attemptId: string): Promise<Evaluation | null>;
    /** Ручная корректировка: автора и время фиксирует сервер (audit_log). */
    override(attemptId: string, input: { score: number; reason: string }): Promise<Evaluation>;
  };

  callScript: {
    /** Урезанный вариант без keyFacts — GAP-11. */
    get(attemptId: string): Promise<StudentCallScript>;
  };

  /**
   * Разговор с заявителем.
   *
   * Транскрипт и нумерация ходов принадлежат серверу: интерфейс не считает
   * номера сам, а берёт `nextTurnNo` из ответа. Это же позволяет восстановить
   * разговор после перезагрузки страницы.
   */
  dialogue: {
    get(attemptId: string): Promise<DialogueState>;
    /**
     * Ход разговора. Речь оператора распознаёт сервер (ai-service внутри
     * этого запроса) — отдельного STT-endpoint контракт не предусматривает.
     */
    turn(input: DialogueTurnInput): Promise<DialogueTurnResponse>;
    /** «Положить трубку»: разговор закрыт, карточку можно дозаполнить. */
    end(attemptId: string): Promise<DialogueState>;
  };

  feedback: {
    list(attemptId: string): Promise<TeacherFeedback[]>;
    /** Автор комментария — текущий преподаватель по сессии. */
    add(attemptId: string, input: { field?: string; comment: string; recommendation?: string }): Promise<TeacherFeedback>;
  };

  reaction: {
    /** В проде приходит внутри AssignedService — здесь отдельно для mock-режима. */
    allowedNext(current: ReactionStatus, serviceCode: string): Promise<AllowedTransition[]>;
  };

  /**
   * Технический раздел администратора. Состояние контура знает только сервер:
   * у mock-реализации раздела нет, и экран честно пишет «нет данных».
   */
  admin?: {
    health(): Promise<SystemHealth>;
  };
}

export type ReportFormat = 'csv' | 'xlsx' | 'pdf';

/** frontend.v1.yaml: CreateUserInput */
export interface CreateUserInput {
  login: string;
  password: string;
  role: Role;
  lastName: string;
  firstName: string;
  middleName?: string;
}
