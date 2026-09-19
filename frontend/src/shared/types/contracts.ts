/**
 * Типы, зафиксированные контрактом `contracts/openapi/_components.yaml`.
 *
 * Это ЗЕРКАЛО спеки. Менять здесь что-либо можно только вслед за спекой
 * (спека меняется PR'ом с ревью обоих концов — см. docs/contracts.md).
 *
 * Расширения, которых в спеке нет, живут в `./domain.ts` и помечены TODO(backend).
 */

/** Сырой балл слоя 0..100. Веса и итог считает только go-core. */
export type Score = number;

export type JobType = 'evaluate_grammar' | 'evaluate_semantic' | 'generate_scenario' | 'tts';

export type Profile = 'eval_fast' | 'eval_thorough' | 'generate' | 'tts_default';

/** `_components.yaml#/components/schemas/Address` */
export interface Address {
  raw?: string;
  city?: string;
  street?: string;
  house?: string;
  entrance?: string;
  floor?: string;
  apartment?: string;
  /** Ориентир: «напротив школы №14» */
  landmark?: string;
}

/** `_components.yaml#/components/schemas/CallScript` */
export interface CallScript {
  caller: {
    name?: string;
    phone?: string;
    /** «очевидец / пострадавший / сосед / ...» */
    role?: string;
    /** для TTS и реализма: паника, спокоен, ... */
    emotionalState?: string;
  };
  address: Address;
  /**
   * Факты, которые заявитель сообщает по ходу звонка.
   * ВНИМАНИЕ: это источник `required_facts` эталона — студенту НЕ отдаётся.
   * См. StudentCallScript в ./domain.ts.
   */
  keyFacts: string[];
  turns: CallTurn[];
}

export interface CallTurn {
  speaker: 'caller' | 'operator_hint';
  text: string;
  /** sha256(text|voice|rate) — ключ tts_cache; null до озвучки */
  ttsHash?: string;
}

/** `_components.yaml#/components/schemas/IncidentCard` */
export interface IncidentCard {
  categoryCode?: string;
  address?: Address;
  applicant?: { name?: string; phone?: string };
  casualties?: { injured?: number; dead?: number; trapped?: number };
  /** Коды services.code («01»..«04», «gormost», ...) */
  servicesToNotify?: string[];
  /** Свободный текст — слои grammar + semantic */
  description?: string;
  /** Свободный текст — слои grammar + semantic */
  actionsTaken?: string;
  /** Признаки опросной карты, специфичные для категории */
  attributes?: Record<string, unknown>;
}

/** `_components.yaml#/components/schemas/Scoring` */
export interface Scoring {
  requiredFields?: string[];
  fieldWeights?: Record<string, number>;
  requiredFacts?: string[];
  /** Факты, которых в ответе быть не должно (домыслы) */
  forbiddenFacts?: string[];
}

/** `_components.yaml#/components/schemas/ExpectedAction` */
export interface ExpectedAction {
  actionText: string;
  requiredFacts?: string[];
  forbiddenFacts?: string[];
}

export type GrammarSeverity = 'error' | 'warning' | 'style';

/** `_components.yaml#/components/schemas/GrammarRemark` */
export interface GrammarRemark {
  /** путь поля карточки («description», «answer_turns.1») */
  field: string;
  offset: number;
  length: number;
  severity: GrammarSeverity;
  /** id правила LanguageTool */
  rule?: string;
  message: string;
  suggestions?: string[];
}

/** `_components.yaml#/components/schemas/GrammarResult` */
export interface GrammarResult {
  score: Score;
  stats: {
    wordsChecked: number;
    errorsBySeverity?: Partial<Record<GrammarSeverity, number>>;
  };
  remarks: GrammarRemark[];
}

export interface SemanticPerField {
  field: string;
  score: Score;
  comment?: string;
}

/** `_components.yaml#/components/schemas/SemanticResult` */
export interface SemanticResult {
  score: Score;
  /**
   * Уверенность модели. Ниже порога (settings, дефолт 0.7) go-core помечает
   * evaluations «требует ревью преподавателя».
   */
  confidence: number;
  /** Обязательные факты эталона, отсутствующие в ответе */
  missingFacts?: string[];
  /** Факты в ответе, которых нет в легенде — домыслы */
  extraFacts?: string[];
  perField?: SemanticPerField[];
  /** 1–2 предложения обратной связи; UI показывает как есть */
  summaryForStudent?: string;
}

/** `_components.yaml#/components/schemas/ScenarioResult` */
export interface ScenarioResult {
  title: string;
  callScript: CallScript;
  etalonCard: IncidentCard;
  expectedActions?: ExpectedAction[];
  difficultyEstimate?: 1 | 2 | 3;
  /** На что обратить внимание при валидации сценария */
  notesForTeacher?: string;
}

/** `_components.yaml#/components/schemas/Engine` — воспроизводимость оценки */
export interface Engine {
  llmModel?: string;
  promptVersion?: string;
  ltVersion?: string;
  rulesVersion?: string;
  ttsVersion?: string;
  durationMs: number;
  queueWaitMs?: number;
  tokensIn?: number;
  tokensOut?: number;
}

/** `_components.yaml#/components/schemas/ApiError` — формат ошибок всего проекта */
export interface ApiError {
  code: string;
  message: string;
  details?: Record<string, unknown>;
}

/** `ai-service.v1.yaml#/components/schemas/QueueStatus` */
export interface QueueStatus {
  pending: Record<string, number>;
  running: number;
  currentModel?: string;
  llmLoaded?: boolean;
  estWaitSec?: number;
}
