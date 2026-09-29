/**
 * Типы отчётных и технических разделов (контракт v1.2–v1.4): прогресс
 * обучающегося, аналитика группы, справочная база, журналы администратора,
 * пакет сценариев.
 *
 * Берутся прямо из сгенерированной схемы (`make generate-ts`), а не
 * переписываются руками: у этих структур нет доменных расширений фронта,
 * и ручная копия только разошлась бы со спекой при следующем её изменении.
 */

import type { components } from '../api/gen/frontend.v1';
import type {
  Attempt, AttemptEvent, DialogueTurnView, Evaluation, Lesson, LessonParticipant, LessonStatus,
} from './domain';

type Schemas = components['schemas'];

export type StudentProgress = Schemas['StudentProgress'];
export type AnalyticsOverview = Schemas['AnalyticsOverview'];
export type Insight = Schemas['Insight'];
export type Material = Schemas['Material'];
export type LogEntry = Schemas['LogEntry'];
export type AuditEntry = Schemas['AuditEntry'];
export type Backup = Schemas['Backup'];
export type Setting = Schemas['Setting'];
export type ScenarioBundle = Schemas['ScenarioBundle'];
export type ScenarioImportResult = Schemas['ScenarioImportResult'];
export type CardSource = Schemas['CardSource'];

export type LogLevel = LogEntry['level'];

/**
 * Сообщение канала мониторинга занятия (`MonitorMessage`).
 *
 * Описано через доменные типы, а не сгенерированные: снимок подменяет
 * состояние страницы, которая уже работает с доменными `Lesson` и `Attempt`.
 */
export interface MonitorMessage {
  seq: number;
  type: 'snapshot' | 'participantStatus' | 'attemptEvent' | 'dialogueTurn' | 'evaluationUpdated' | 'lessonStatus' | 'aiHealth';
  at: string;
  snapshot?: { lesson?: Lesson; attempts?: Attempt[] };
  participant?: LessonParticipant;
  attemptId?: string;
  event?: AttemptEvent;
  turn?: DialogueTurnView;
  evaluation?: Evaluation;
  lessonStatus?: LessonStatus;
  aiHealth?: { ok?: boolean; dialogWaiting?: number; dialogAvgMs?: number };
}

/** Сообщение канала попытки обучающегося (`StudentMessage`). */
export interface StudentMessage {
  seq: number;
  type: 'evaluationUpdated' | 'lessonFinished' | 'timerExpired' | 'callerHungUp';
  at: string;
  evaluation?: Evaluation;
  turn?: DialogueTurnView;
}
