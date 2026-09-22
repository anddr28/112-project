/**
 * Жизненный цикл сценария в интерфейсе (решение backend):
 *
 * - draft / generated — обычная правка;
 * - validated, не используется в занятиях — правка + «Создать занятие»;
 * - validated, используется — правки нет, «Создать новую версию» + «Создать занятие».
 *
 * Критерий — признак использования от сервера (`inUse` / `lessonsCount`),
 * а не статус: сам интерфейс не решает, в каких занятиях сценарий стоит.
 */

import { hasErrorCode, isApiError } from '../../shared/api';
import type { Scenario } from '../../shared/types';

export function scenarioLessonsCount(s: Scenario): number {
  return s.lessonsCount ?? 0;
}

export function scenarioInUse(s: Scenario): boolean {
  return s.inUse ?? scenarioLessonsCount(s) > 0;
}

/** Можно ли править этот сценарий на месте. */
export function canEditScenario(s: Scenario): boolean {
  if (s.status === 'draft' || s.status === 'generated') return true;
  return s.status === 'validated' && !scenarioInUse(s);
}

/** Предлагать новую версию вместо правки. */
export function needsNewVersion(s: Scenario): boolean {
  return s.status === 'validated' && scenarioInUse(s);
}

export function inUseText(count: number): string {
  return `Сценарий используется в занятиях (${count}), изменить нельзя — создайте новую версию`;
}

/**
 * Текст ошибки сохранения. 409 означает, что сценарий успел попасть в
 * занятие, пока его правили, — показываем понятное сообщение с числом занятий.
 */
export function scenarioSaveError(e: unknown, s: Scenario, fallback: string): string {
  if (hasErrorCode(e, 'conflict')) {
    const fromServer = isApiError(e) ? e.details?.lessonsCount : undefined;
    const count = typeof fromServer === 'number' ? fromServer : scenarioLessonsCount(s);
    return inUseText(Math.max(count, 1));
  }
  return e instanceof Error ? e.message : fallback;
}

export function versionNo(s: Scenario): number {
  return s.version ?? 1;
}

/** Исходный сценарий цепочки версий среди известных. */
export function rootIdOf(s: Scenario, byId: Map<string, Scenario>): string {
  let root = s;
  while (root.parentScenarioId) {
    const parent = byId.get(root.parentScenarioId);
    if (!parent) return root.parentScenarioId;
    root = parent;
  }
  return root.id;
}

/** Все версии той же цепочки, по возрастанию номера. */
export function versionsOf(s: Scenario, all: Scenario[]): Scenario[] {
  const byId = new Map(all.map((x) => [x.id, x]));
  const root = rootIdOf(s, byId);
  return all
    .filter((x) => rootIdOf(x, byId) === root)
    .sort((a, b) => versionNo(a) - versionNo(b));
}

/**
 * Строки списка: версии одного сценария идут подряд (v1, v2 …), группы —
 * в порядке первого появления в ответе API.
 */
export function groupByVersions(list: Scenario[], all: Scenario[]): Array<{ scenario: Scenario; grouped: boolean }> {
  const byId = new Map(all.map((x) => [x.id, x]));
  const groups = new Map<string, Scenario[]>();
  for (const s of list) {
    const root = rootIdOf(s, byId);
    groups.set(root, [...(groups.get(root) ?? []), s]);
  }
  const chainSize = new Map<string, number>();
  for (const s of all) {
    const root = rootIdOf(s, byId);
    chainSize.set(root, (chainSize.get(root) ?? 0) + 1);
  }
  return [...groups.entries()].flatMap(([root, members]) =>
    members
      .sort((a, b) => versionNo(a) - versionNo(b))
      .map((scenario) => ({ scenario, grouped: (chainSize.get(root) ?? 1) > 1 })),
  );
}

/** Название для отчётов: номер версии — только если она не первая. */
export function scenarioLabel(s: Scenario): string {
  return versionNo(s) > 1 ? `${s.title} (версия ${versionNo(s)})` : s.title;
}
