import { useEffect, useState } from 'react';
import { api } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { Badge, Card, EmptyState, ErrorState, Loading } from '../../components/ui';
import { formatDateTime } from '../../shared/utils/time';
import type { LogLevel } from '../../shared/types';

const LEVELS: Array<{ value: LogLevel; label: string }> = [
  { value: 'debug', label: 'Отладка и выше' },
  { value: 'info', label: 'Информация и выше' },
  { value: 'warn', label: 'Предупреждения и ошибки' },
  { value: 'error', label: 'Только ошибки' },
];

const LEVEL_TONE: Record<LogLevel, 'neutral' | 'accent' | 'warn' | 'danger'> = {
  debug: 'neutral',
  info: 'accent',
  warn: 'warn',
  error: 'danger',
};

const LIMITS = [100, 200, 500, 1000];

/**
 * Системный журнал go-core (v1.4): последние записи кольцевого буфера, новые
 * сверху. «Только ошибки» — отчёт об ошибках и сбоях (ТЗ). Полный журнал —
 * stdout контейнера; здесь то, что нужно администратору без доступа к серверу.
 */
export function AdminLogsPage() {
  const [level, setLevel] = useState<LogLevel>('info');
  const [q, setQ] = useState('');
  const [query, setQuery] = useState('');
  const [limit, setLimit] = useState(200);
  const [auto, setAuto] = useState(false);
  const logs = useAsync(() => api.admin.logs({ level, q: query || undefined, limit }), [level, query, limit]);

  useEffect(() => {
    const id = setTimeout(() => setQuery(q.trim()), 300);
    return () => clearTimeout(id);
  }, [q]);

  // Автообновление — по желанию: при чтении длинной записи список не должен прыгать.
  useEffect(() => {
    if (!auto) return;
    const id = setInterval(logs.reload, 5000);
    return () => clearInterval(id);
  }, [auto, logs.reload]);

  const list = logs.data ?? [];

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <h1>Системный журнал</h1>
          <div className="page-head__sub">Последние записи ядра системы: запросы, задачи ИИ, каналы реального времени, сбои.</div>
        </div>
        <div className="page-head__actions">
          <button type="button" className="btn" onClick={logs.reload} disabled={logs.loading || logs.refreshing}>Обновить</button>
        </div>
      </div>

      <div className="filters" role="group" aria-label="Фильтры журнала">
        <label className="filters__item">
          <span className="field__label">Уровень</span>
          <select className="select" value={level} onChange={(e) => setLevel(e.target.value as LogLevel)}>
            {LEVELS.map((l) => <option key={l.value} value={l.value}>{l.label}</option>)}
          </select>
        </label>
        <label className="filters__item filters__item--wide">
          <span className="field__label">Поиск в сообщении и атрибутах</span>
          <input className="input" type="search" maxLength={100} value={q} onChange={(e) => setQ(e.target.value)} placeholder="Например: breaker, backup, 500" />
        </label>
        <label className="filters__item">
          <span className="field__label">Записей</span>
          <select className="select" value={limit} onChange={(e) => setLimit(Number(e.target.value))}>
            {LIMITS.map((n) => <option key={n} value={n}>{n}</option>)}
          </select>
        </label>
        <label className="filters__check">
          <input type="checkbox" checked={auto} onChange={(e) => setAuto(e.target.checked)} />
          <span>Обновлять каждые 5 с</span>
        </label>
      </div>

      {logs.loading && !logs.data ? (
        <Loading />
      ) : logs.error && !logs.data ? (
        <ErrorState text={logs.error} onRetry={logs.reload} />
      ) : list.length === 0 ? (
        <Card><EmptyState title="Записей нет" text="Под выбранные условия ничего не попало." /></Card>
      ) : (
        <Card title={`Записей: ${list.length}`}>
          <div className="table-scroll">
            <table className="table logs">
              <thead><tr><th>Время</th><th>Уровень</th><th>Сообщение</th><th>Атрибуты</th></tr></thead>
              <tbody>
                {list.map((e, i) => (
                  <tr key={`${e.at}-${i}`} className={e.level === 'error' ? 'logs__error' : undefined}>
                    <td className="mono small nowrap">{formatDateTime(e.at)}</td>
                    <td><Badge tone={LEVEL_TONE[e.level]}>{e.level}</Badge></td>
                    <td>{e.message}</td>
                    <td className="mono xs logs__attrs">
                      {e.attrs ? Object.entries(e.attrs).map(([k, v]) => `${k}=${typeof v === 'string' ? v : JSON.stringify(v)}`).join('  ') : ''}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}
    </>
  );
}
