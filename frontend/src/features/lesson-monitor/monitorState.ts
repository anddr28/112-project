/**
 * Состояние живого мониторинга занятия.
 *
 * Один редьюсер для обоих источников: ответа REST (первая загрузка и опрос,
 * когда канал недоступен) и сообщений WebSocket. `snapshot` и REST заменяют
 * занятие и попытки целиком; остальные сообщения дополняют их по месту.
 */

import type { Attempt, AttemptEvent, Evaluation, Lesson, MonitorMessage } from '../../shared/types';
import { fieldLabel } from '../../shared/utils/card';

export interface AttemptLive {
  /** последнее действие обучающегося — «что он делает сейчас» */
  lastEvent?: AttemptEvent;
  turns: number;
  lastTurn?: { speaker: 'operator' | 'caller'; text: string };
}

export interface MonitorState {
  lesson: Lesson | null;
  attempts: Attempt[];
  live: Record<string, AttemptLive>;
  evaluations: Record<string, Evaluation>;
  aiHealth?: MonitorMessage['aiHealth'];
}

export type MonitorAction =
  | { type: 'load'; lesson: Lesson; attempts: Attempt[] }
  | { type: 'evaluation'; attemptId: string; evaluation: Evaluation }
  | { type: 'message'; message: MonitorMessage };

export const initialMonitor: MonitorState = { lesson: null, attempts: [], live: {}, evaluations: {} };

function liveOf(state: MonitorState, attemptId: string, attempts = state.attempts): AttemptLive {
  const known = state.live[attemptId];
  if (known) return known;
  const attempt = attempts.find((a) => a.id === attemptId);
  return { turns: attempt?.dialogue?.turnsCount ?? 0 };
}

export function monitorReducer(state: MonitorState, action: MonitorAction): MonitorState {
  if (action.type === 'load') return { ...state, lesson: action.lesson, attempts: action.attempts };
  if (action.type === 'evaluation') {
    return { ...state, evaluations: { ...state.evaluations, [action.attemptId]: action.evaluation } };
  }

  const m = action.message;
  switch (m.type) {
    case 'snapshot':
      return {
        ...state,
        lesson: m.snapshot?.lesson ?? state.lesson,
        attempts: m.snapshot?.attempts ?? state.attempts,
      };
    case 'participantStatus': {
      const p = m.participant;
      if (!state.lesson || !p) return state;
      const exists = state.lesson.participants.some((x) => x.userId === p.userId);
      const participants = exists
        ? state.lesson.participants.map((x) => (x.userId === p.userId ? { ...x, ...p } : x))
        : [...state.lesson.participants, p];
      return { ...state, lesson: { ...state.lesson, participants } };
    }
    case 'lessonStatus':
      return state.lesson && m.lessonStatus ? { ...state, lesson: { ...state.lesson, status: m.lessonStatus } } : state;
    case 'attemptEvent': {
      if (!m.attemptId || !m.event) return state;
      const live = liveOf(state, m.attemptId);
      return {
        ...state,
        attempts: applyEventToAttempt(state.attempts, m.attemptId, m.event),
        live: { ...state.live, [m.attemptId]: { ...live, lastEvent: m.event } },
      };
    }
    case 'dialogueTurn': {
      if (!m.attemptId || !m.turn) return state;
      const live = liveOf(state, m.attemptId);
      return {
        ...state,
        live: {
          ...state.live,
          [m.attemptId]: {
            ...live,
            turns: m.turn.speaker === 'operator' ? live.turns + 1 : live.turns,
            lastTurn: { speaker: m.turn.speaker, text: m.turn.text },
          },
        },
      };
    }
    case 'evaluationUpdated':
      return m.attemptId && m.evaluation
        ? { ...state, evaluations: { ...state.evaluations, [m.attemptId]: m.evaluation } }
        : state;
    case 'aiHealth':
      return { ...state, aiHealth: m.aiHealth };
    default:
      return state;
  }
}

/** События, меняющие состояние попытки, отражаем без перечитывания списка. */
function applyEventToAttempt(attempts: Attempt[], attemptId: string, event: AttemptEvent): Attempt[] {
  return attempts.map((a) => {
    if (a.id !== attemptId) return a;
    if (event.type === 'call_accepted') return { ...a, status: 'in_progress', callAcceptedAt: a.callAcceptedAt ?? event.at };
    if (event.type === 'submitted') {
      return { ...a, status: 'evaluating', submittedAt: event.at };
    }
    return a;
  });
}

const ACTION_LABEL: Partial<Record<AttemptEvent['type'], string>> = {
  issued: 'ожидает вызова',
  call_accepted: 'принял вызов',
  open_card: 'открыл карточку',
  choose_value: 'заполняет карточку',
  service_assigned: 'оповещает службы',
  service_removed: 'правит состав служб',
  service_status_changed: 'меняет статус службы',
  replay: 'переспрашивает заявителя',
  save: 'сохраняет карточку',
  submitted: 'сдал карточку',
  timer_expired: 'вышел за норматив',
  disconnected: 'нет связи',
  reconnected: 'снова на связи',
  mic_check: 'проверяет гарнитуру',
  ptt_start: 'говорит',
  ptt_stop: 'ждёт ответа заявителя',
  dialogue_operator: 'говорит с заявителем',
  dialogue_caller: 'слушает заявителя',
  dialogue_ended: 'разговор завершён',
};

/** «Что делает сейчас» по последнему событию: «заполняет адрес», «оповещает службы». */
export function currentAction(event?: AttemptEvent): string | undefined {
  if (!event) return undefined;
  if (event.type === 'field_changed') {
    const field = typeof event.payload?.field === 'string' ? event.payload.field : '';
    return field ? `заполняет: ${fieldLabel(field).toLowerCase()}` : 'заполняет карточку';
  }
  return ACTION_LABEL[event.type];
}
