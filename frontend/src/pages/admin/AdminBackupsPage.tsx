import { useEffect, useState } from 'react';
import { api, hasErrorCode } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { Badge, Card, EmptyState, ErrorState, Loading } from '../../components/ui';
import { formatDateTime, formatDuration, isOlderThan } from '../../shared/utils/time';
import type { Backup } from '../../shared/types';

/** Пока идёт копирование, журнал перечитывается — итог появится без F5. */
const POLL_MS = 3000;
const DAY_MS = 86_400_000;

const STATUS: Record<Backup['status'], { label: string; tone: 'ok' | 'warn' | 'danger' }> = {
  running: { label: 'Выполняется', tone: 'warn' },
  done: { label: 'Готова', tone: 'ok' },
  failed: { label: 'Ошибка', tone: 'danger' },
};

function formatBytes(n?: number): string {
  if (n == null) return '—';
  if (n < 1024 * 1024) return `${Math.round(n / 1024)} КБ`;
  return `${(n / 1024 / 1024).toFixed(1).replace('.', ',')} МБ`;
}

/**
 * Резервные копии (ТЗ: не реже раза в сутки). Запуск вручную асинхронный:
 * сервер отвечает 202 с записью в статусе «выполняется», повторный запуск
 * во время копирования — 409.
 */
export function AdminBackupsPage() {
  const backups = useAsync(() => api.admin.backups(), []);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<{ tone: 'ok' | 'warn' | 'danger'; text: string } | null>(null);
  const list = backups.data ?? [];
  const running = list.some((b) => b.status === 'running');

  useEffect(() => {
    if (!running) return;
    const id = setInterval(backups.reload, POLL_MS);
    return () => clearInterval(id);
  }, [running, backups.reload]);

  async function run() {
    setBusy(true);
    setMessage(null);
    try {
      await api.admin.runBackup();
      setMessage({ tone: 'ok', text: 'Резервное копирование запущено. Результат появится в журнале.' });
    } catch (e) {
      setMessage(
        hasErrorCode(e, 'conflict')
          ? { tone: 'warn', text: 'Копирование уже выполняется — дождитесь его завершения.' }
          : { tone: 'danger', text: e instanceof Error ? e.message : 'Не удалось запустить копирование' },
      );
    } finally {
      setBusy(false);
      backups.reload();
    }
  }

  if (backups.loading) return <Loading />;
  if (backups.error && !backups.data) return <ErrorState text={backups.error} onRetry={backups.reload} />;

  const lastDone = list.find((b) => b.status === 'done');
  const stale = !lastDone || isOlderThan(lastDone.finishedAt ?? lastDone.startedAt, DAY_MS);

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <h1>Резервные копии</h1>
          <div className="page-head__sub">Копии базы данных по расписанию и вручную. Требование ТЗ — не реже раза в сутки.</div>
        </div>
        <div className="page-head__actions">
          <button type="button" className="btn btn--primary" onClick={() => void run()} disabled={busy || running}>
            {running ? 'Копирование идёт…' : busy ? 'Запуск…' : 'Запустить сейчас'}
          </button>
        </div>
      </div>

      {message && (
        <p role="status" className="small" style={{ marginBottom: 12, color: `var(--u-${message.tone})` }}>{message.text}</p>
      )}

      <div className="summary" style={{ marginBottom: 16 }}>
        <div className="summary__item">
          <span className="summary__label">Последняя успешная</span>
          <span className={`summary__value${stale ? ' summary__value--danger' : ' summary__value--ok'}`}>
            {lastDone ? formatDateTime(lastDone.finishedAt ?? lastDone.startedAt) : 'нет'}
          </span>
          {stale && <span className="summary__note">старше суток — нарушено требование ТЗ</span>}
        </div>
        <div className="summary__item">
          <span className="summary__label">Всего в журнале</span>
          <span className="summary__value">{list.length}</span>
        </div>
        <div className="summary__item">
          <span className="summary__label">С ошибкой</span>
          <span className={`summary__value${list.some((b) => b.status === 'failed') ? ' summary__value--warn' : ''}`}>
            {list.filter((b) => b.status === 'failed').length}
          </span>
        </div>
      </div>

      <Card title="Журнал резервного копирования" actions={backups.refreshing ? <span className="dim small">Обновление…</span> : undefined}>
        {list.length === 0 ? (
          <EmptyState title="Копий пока нет" text="Запустите копирование вручную или дождитесь расписания." />
        ) : (
          <div className="table-scroll">
            <table className="table">
              <thead>
                <tr><th>Начало</th><th>Длительность</th><th>Состояние</th><th>Запуск</th><th>Размер</th><th>Файл / ошибка</th><th>SHA-256</th></tr>
              </thead>
              <tbody>
                {list.map((b) => (
                  <tr key={b.id}>
                    <td className="mono small nowrap">{formatDateTime(b.startedAt)}</td>
                    <td className="mono small nowrap">
                      {b.finishedAt ? formatDuration(Date.parse(b.finishedAt) - Date.parse(b.startedAt)) : '—'}
                    </td>
                    <td><Badge tone={STATUS[b.status].tone}>{STATUS[b.status].label}</Badge></td>
                    <td className="muted small">{b.triggeredBy ? 'вручную' : 'по расписанию'}</td>
                    <td className="num nowrap">{formatBytes(b.sizeBytes)}</td>
                    <td className="small">
                      {b.error ? <span style={{ color: 'var(--u-danger)' }}>{b.error}</span> : <span className="mono">{b.filePath ?? '—'}</span>}
                    </td>
                    <td className="mono xs" title={b.sha256}>{b.sha256 ? `${b.sha256.slice(0, 12)}…` : '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </>
  );
}
