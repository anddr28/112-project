import { Badge } from './ui';
import type { ChannelStatus } from '../shared/realtime/useRealtimeChannel';

/**
 * Индикатор канала реального времени. Преподавателю важно понимать,
 * насколько свежа картина: живой поток, переподключение или опрос.
 */
export function ChannelBadge({ status, retryInSec }: { status: ChannelStatus; retryInSec?: number }) {
  switch (status) {
    case 'open':
      return <span role="status"><Badge tone="ok">● В реальном времени</Badge></span>;
    case 'connecting':
      return <span role="status"><Badge tone="neutral">Подключение…</Badge></span>;
    case 'reconnecting':
      return (
        <span role="status">
          <Badge tone="warn" value>Связь потеряна{retryInSec ? ` · повтор через ${retryInSec} с` : ''}</Badge>
        </span>
      );
    case 'polling':
      return (
        <span role="status" title="Канал реального времени недоступен; попытки подключиться продолжаются">
          <Badge tone="warn" value>Обновление раз в 3 с</Badge>
        </span>
      );
    default:
      return null;
  }
}
