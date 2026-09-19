/**
 * Визуальная группировка статусов реагирования служб.
 *
 * Здесь нет правил переходов: допустимые следующие статусы приходят с backend
 * в `AssignedService.allowedNext`, а на время работы без API их подставляет
 * mock-слой (`shared/mocks/reactionTransitions.ts`). Frontend решает только,
 * каким цветом показать статус, — это его зона ответственности.
 */

import { SYSTEM_REACTION_STATUSES } from '../../shared/types';
import type { ReactionStatus } from '../../shared/types';

export type StatusTone = 'system' | 'accepted' | 'rejected' | 'progress' | 'done';

/** Цветовая группа статуса — используется и в АРМ, и в отчёте преподавателя. */
export function toneFor(status: ReactionStatus): StatusTone {
  if (SYSTEM_REACTION_STATUSES.includes(status)) return 'system';
  if (status === 'Не принята' || status === 'Отказ от выполнения работ') return 'rejected';
  if (status === 'Принята') return 'accepted';
  if (status === 'Работы завершены') return 'done';
  return 'progress';
}
