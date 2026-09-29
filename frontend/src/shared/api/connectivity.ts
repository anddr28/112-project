import { create } from 'zustand';

/**
 * Связь с сервером глазами обучающегося: есть ли обрыв и сколько данных ждёт
 * отправки. Ставит исходящая очередь попытки (`features/attempt-runtime/outbox`),
 * читает индикатор «нет связи — данные сохранятся».
 */
interface ConnectivityState {
  offline: boolean;
  /** черновик + события, ещё не подтверждённые сервером */
  pending: number;
}

export const useConnectivity = create<ConnectivityState>(() => ({ offline: false, pending: 0 }));

export function setConnectivity(patch: Partial<ConnectivityState>): void {
  useConnectivity.setState(patch);
}
