/**
 * Чем получен результат ИИ — по данным сервера, без догадок интерфейса.
 *
 * Преподаватель должен видеть, какая модель сгенерировала сценарий и какая
 * посчитала слои оценки: на стенде без ai-service это имитатор (fake-llm),
 * и выдавать его результат за работу нейросети нельзя.
 */

import type { EvaluationEngine } from '../types';

/** Модель генерации сценария: go-core кладёт `llm_model`, mock — `model`. */
export function generationModel(meta: Record<string, unknown> | undefined): string | undefined {
  const value = meta?.llm_model ?? meta?.llmModel ?? meta?.model;
  return typeof value === 'string' && value.trim() ? value : undefined;
}

/** Модели и версии, которыми считались AI-слои, без повторов. */
export function evaluationEngines(engine: EvaluationEngine | undefined): string[] {
  if (!engine) return [];
  const names = [engine.semantic, engine.dialogue, engine.grammar].flatMap((e) =>
    e ? [e.llmModel, e.ltVersion].filter((v): v is string => Boolean(v)) : [],
  );
  return [...new Set(names)];
}
