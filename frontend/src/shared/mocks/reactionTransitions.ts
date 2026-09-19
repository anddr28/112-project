/**
 * FIXTURE: граф переходов статусов реагирования.
 *
 * Frontend НЕ является источником истины по допустимым переходам: компоненты
 * рисуют выпадающий список строго из `AssignedService.allowedNext`, который
 * приходит с backend. Этот модуль нужен только затем, чтобы mock-слой мог
 * отдавать корректный `allowedNext` до появления API, и живёт в mock-слое,
 * чтобы удалиться вместе с ним.
 *
 * TODO(backend) B-07 / GAP-01: контракта на статусы реагирования нет вообще —
 * в `_components.yaml` у `IncidentCard` только `servicesToNotify: string[]`.
 *
 * Источник правил:
 *   docs/Работа с АРМ-112 для ДДС от ОКр_ГСИ.pdf, «Статусы реагирования служб»:
 *   «Выбрать статусы реагирования можно только последовательно. Сначала
 *    в выпадающем списке будет только 2 первичных статуса: "Принята" и
 *    "Не принята". При выборе "Принята" откроется список со статусами
 *    "Начало реагирования", "Прибытие", "Проведение работ", "Завершение работ"
 *    и "Отказ от выполнения работ". При выборе "Не принята" единственным
 *    доступным вариантом для последующего выбора будет статус "Принята".»
 */

import { SYSTEM_REACTION_STATUSES } from '../types';
import type { AllowedTransition, ReactionStatus } from '../types';

/** Терминальные статусы закрывают карточку для редактирования. */
const TERMINAL_STATUSES: ReactionStatus[] = ['Работы завершены', 'Отказ от выполнения работ'];

export function isTerminal(status: ReactionStatus): boolean {
  return TERMINAL_STATUSES.includes(status);
}

/** Комментарий обязателен: «Не принята», «Отказ от выполнения работ», «Работы завершены». */
const COMMENT_REQUIRED: ReactionStatus[] = [
  'Не принята',
  'Отказ от выполнения работ',
  'Работы завершены',
];

const TRANSITIONS: Record<ReactionStatus, ReactionStatus[]> = {
  'Добавлена': ['Получена службой'],
  'Получена службой': ['Принята', 'Не принята'],
  'Принята': [
    'Начало реагирования',
    'Прибытие',
    'Проведение работ',
    'Работы завершены',
    'Отказ от выполнения работ',
  ],
  'Не принята': ['Принята'],
  'Начало реагирования': ['Прибытие', 'Проведение работ', 'Работы завершены', 'Отказ от выполнения работ'],
  'Прибытие': ['Проведение работ', 'Работы завершены', 'Отказ от выполнения работ'],
  'Проведение работ': ['Работы завершены', 'Отказ от выполнения работ'],
  'Работы завершены': [],
  'Отказ от выполнения работ': [],
};

/**
 * Памятка ДДС: «Служба 103 не проставляет статусы "Не принята" и "Отказ от
 * выполнения работ". Вместо них проставляется статус "Работы завершены:
 * Завершение работ без бригады".»
 */
const FORBIDDEN_BY_SERVICE: Record<string, ReactionStatus[]> = {
  '103': ['Не принята', 'Отказ от выполнения работ'],
};

export function computeAllowedNext(current: ReactionStatus, serviceCode: string): AllowedTransition[] {
  const forbidden = FORBIDDEN_BY_SERVICE[serviceCode] ?? [];

  return TRANSITIONS[current]
    .filter((status) => !forbidden.includes(status) && !SYSTEM_REACTION_STATUSES.includes(status))
    .map((status) => ({
      status,
      label: labelFor(status, serviceCode),
      commentRequired: COMMENT_REQUIRED.includes(status),
      // Номер наряда осмыслен, когда служба реально выезжает.
      squadNumberRequired: false,
    }));
}

function labelFor(status: ReactionStatus, serviceCode: string): string {
  if (serviceCode === '103' && status === 'Работы завершены') {
    return 'Работы завершены: Завершение работ без бригады';
  }
  return status;
}
