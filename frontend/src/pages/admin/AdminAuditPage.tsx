import { useState } from 'react';
import { api } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { Badge, Card, EmptyState, ErrorState, Loading, Modal } from '../../components/ui';
import { ROLE_LABEL } from '../../app/auth';
import { formatDateTime } from '../../shared/utils/time';
import { fullName } from '../../shared/utils/user';
import type { AuditFilter } from '../../shared/api';
import type { AuditEntry, Role } from '../../shared/types';

const PAGE = 100;

/** Группы действий — префиксы с точкой (контракт: «user.»). */
const ACTION_PREFIXES: Array<[string, string]> = [
  ['user.', 'Учётные записи и вход'],
  ['scenario.', 'Сценарии'],
  ['lesson.', 'Занятия'],
  ['evaluation.', 'Оценки'],
  ['settings.', 'Настройки'],
  ['backup.', 'Резервные копии'],
  ['material.', 'Справочная база'],
];

const ACTION_LABEL: Record<string, string> = {
  'user.login': 'Вход в систему',
  'user.logout': 'Выход',
  'user.create': 'Создана учётная запись',
  'user.update': 'Изменена учётная запись',
  'user.block': 'Учётная запись заблокирована',
  'user.unblock': 'Учётная запись разблокирована',
  'scenario.create': 'Создан сценарий',
  'scenario.update': 'Изменён сценарий',
  'scenario.approve': 'Сценарий подтверждён',
  'scenario.reject': 'Сценарий отклонён',
  'scenario.import': 'Импорт сценариев',
  'lesson.create': 'Создано занятие',
  'lesson.start': 'Занятие запущено',
  'lesson.finish': 'Занятие завершено',
  'evaluation.override': 'Ручная корректировка оценки',
  'settings.update': 'Изменена настройка',
  'backup.run': 'Запуск резервного копирования',
  'material.create': 'Добавлен материал',
  'material.delete': 'Удалён материал',
};

const ENTITY_LABEL: Record<string, string> = {
  user: 'Пользователь',
  scenario: 'Сценарий',
  lesson: 'Занятие',
  attempt: 'Попытка',
  evaluation: 'Оценка',
  setting: 'Настройка',
  backup: 'Резервная копия',
  material: 'Материал',
};

/** Дата из поля <input type="date"> → граница дня в поясе пользователя. */
function dayBound(value: string, end: boolean): string | undefined {
  if (!value) return undefined;
  const d = new Date(`${value}T${end ? '23:59:59.999' : '00:00:00'}`);
  return Number.isNaN(d.getTime()) ? undefined : d.toISOString();
}

/**
 * Журнал аудита (v1.2): кто, что и когда сделал. Новые сверху; следующая
 * страница — по id последней записи (`beforeId`), поэтому новые записи,
 * появившиеся во время просмотра, не сдвигают уже загруженные.
 */
export function AdminAuditPage() {
  const users = useAsync(() => api.users.list(), []);
  const [actorId, setActorId] = useState('');
  const [action, setAction] = useState('');
  const [entityType, setEntityType] = useState('');
  const [from, setFrom] = useState('');
  const [to, setTo] = useState('');
  const [extra, setExtra] = useState<{ key: string; items: AuditEntry[]; more: boolean }>({ key: '', items: [], more: false });
  const [loadingMore, setLoadingMore] = useState(false);
  const [moreError, setMoreError] = useState<string | null>(null);
  const [open, setOpen] = useState<AuditEntry | null>(null);

  const filter: AuditFilter = {
    actorId: actorId || undefined,
    action: action || undefined,
    entityType: entityType || undefined,
    from: dayBound(from, false),
    to: dayBound(to, true),
    limit: PAGE,
  };
  const key = JSON.stringify(filter);
  const first = useAsync(() => api.admin.audit(filter), [key]);

  // Догруженные страницы принадлежат своему набору фильтров: сменили фильтр — начинаем сначала.
  const extraItems = extra.key === key ? extra.items : [];
  const items = [...(first.data ?? []), ...extraItems];
  const more = extra.key === key ? extra.more : (first.data?.length ?? 0) === PAGE;
  const error = first.error ?? moreError;

  async function loadMore() {
    const lastId = items[items.length - 1]?.id;
    if (lastId == null) return;
    setLoadingMore(true);
    setMoreError(null);
    try {
      const page = await api.admin.audit({ ...filter, beforeId: lastId });
      setExtra({ key, items: [...extraItems, ...page], more: page.length === PAGE });
    } catch (e) {
      setMoreError(e instanceof Error ? e.message : 'Не удалось загрузить журнал');
    } finally {
      setLoadingMore(false);
    }
  }

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <h1>Журнал аудита</h1>
          <div className="page-head__sub">Действия пользователей: вход, изменения сценариев и занятий, корректировки оценок, настройки.</div>
        </div>
      </div>

      <div className="filters" role="group" aria-label="Фильтры журнала">
        <label className="filters__item">
          <span className="field__label">Пользователь</span>
          <select className="select" value={actorId} onChange={(e) => setActorId(e.target.value)}>
            <option value="">Все</option>
            {(users.data ?? []).map((u) => <option key={u.id} value={u.id}>{fullName(u)}</option>)}
          </select>
        </label>
        <label className="filters__item">
          <span className="field__label">Действие</span>
          <select className="select" value={action} onChange={(e) => setAction(e.target.value)}>
            <option value="">Все</option>
            {ACTION_PREFIXES.map(([p, label]) => <option key={p} value={p}>{label}</option>)}
          </select>
        </label>
        <label className="filters__item">
          <span className="field__label">Объект</span>
          <select className="select" value={entityType} onChange={(e) => setEntityType(e.target.value)}>
            <option value="">Все</option>
            {Object.entries(ENTITY_LABEL).map(([k, label]) => <option key={k} value={k}>{label}</option>)}
          </select>
        </label>
        <label className="filters__item">
          <span className="field__label">С даты</span>
          <input className="input" type="date" value={from} onChange={(e) => setFrom(e.target.value)} />
        </label>
        <label className="filters__item">
          <span className="field__label">По дату</span>
          <input className="input" type="date" value={to} onChange={(e) => setTo(e.target.value)} />
        </label>
      </div>

      {first.error && !first.data ? (
        <ErrorState text={first.error} onRetry={first.reload} />
      ) : first.loading && !first.data ? (
        <Loading />
      ) : items.length === 0 ? (
        <Card><EmptyState title="Записей нет" text="Под выбранные условия не попало ни одной записи." /></Card>
      ) : (
        <Card title={`Показано: ${items.length}`} actions={first.refreshing ? <span className="dim small">Обновление…</span> : undefined}>
          {error && <p className="field__error" role="alert">{error}</p>}
          <div className="table-scroll">
            <table className="table table--clickable">
              <thead><tr><th>Время</th><th>Пользователь</th><th>Действие</th><th>Объект</th><th>Изменения</th><th>IP</th></tr></thead>
              <tbody>
                {items.map((e) => (
                  <tr
                    key={e.id}
                    tabIndex={0}
                    onClick={() => setOpen(e)}
                    onKeyDown={(ev) => { if (ev.key === 'Enter') setOpen(e); }}
                  >
                    <td className="mono small nowrap">{formatDateTime(e.at)}</td>
                    <td className="nowrap">
                      {e.actorName ?? 'Система'}
                      {e.actorRole && <span className="dim small"> · {ROLE_LABEL[e.actorRole as Role] ?? e.actorRole}</span>}
                    </td>
                    <td>{ACTION_LABEL[e.action] ?? <span className="mono small">{e.action}</span>}</td>
                    <td className="muted small">{e.entityType ? ENTITY_LABEL[e.entityType] ?? e.entityType : '—'}</td>
                    <td>{e.before || e.after ? <Badge tone="accent">{changedKeys(e).length} изм.</Badge> : <span className="dim">—</span>}</td>
                    <td className="mono small">{e.ip ?? '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {more && (
            <div style={{ marginTop: 12 }}>
              <button type="button" className="btn" onClick={() => void loadMore()} disabled={loadingMore}>
                {loadingMore ? 'Загрузка…' : `Показать ещё ${PAGE}`}
              </button>
            </div>
          )}
        </Card>
      )}

      {open && <AuditDiffModal entry={open} onClose={() => setOpen(null)} />}
    </>
  );
}

function changedKeys(e: AuditEntry): string[] {
  const keys = new Set([...Object.keys(e.before ?? {}), ...Object.keys(e.after ?? {})]);
  return [...keys].filter((k) => JSON.stringify(e.before?.[k]) !== JSON.stringify(e.after?.[k]));
}

function show(value: unknown): string {
  if (value === undefined) return '—';
  return typeof value === 'string' ? value : JSON.stringify(value, null, 1);
}

/** До / после — построчно по полям; изменённые поля подсвечены. */
function AuditDiffModal({ entry: e, onClose }: { entry: AuditEntry; onClose: () => void }) {
  const keys = [...new Set([...Object.keys(e.before ?? {}), ...Object.keys(e.after ?? {})])];
  const changed = new Set(changedKeys(e));
  return (
    <Modal title={ACTION_LABEL[e.action] ?? e.action} onClose={onClose} wide>
      <table className="props" style={{ marginBottom: 12 }}>
        <tbody>
          <tr><th>Время</th><td>{formatDateTime(e.at)}</td></tr>
          <tr><th>Пользователь</th><td>{e.actorName ?? 'Система'}</td></tr>
          <tr><th>Действие</th><td className="mono small">{e.action}</td></tr>
          {e.entityType && <tr><th>Объект</th><td>{ENTITY_LABEL[e.entityType] ?? e.entityType} <span className="mono small dim">{e.entityId}</span></td></tr>}
          {e.ip && <tr><th>IP</th><td className="mono small">{e.ip}</td></tr>}
        </tbody>
      </table>
      {keys.length === 0 ? (
        <p className="muted small">Запись не содержит изменённых данных.</p>
      ) : (
        <div className="table-scroll">
          <table className="table diff">
            <thead><tr><th>Поле</th><th>Было</th><th>Стало</th></tr></thead>
            <tbody>
              {keys.map((k) => (
                <tr key={k} className={changed.has(k) ? 'diff__changed' : undefined}>
                  <td className="mono small">{k}</td>
                  <td className="diff__before"><pre>{show(e.before?.[k])}</pre></td>
                  <td className="diff__after"><pre>{show(e.after?.[k])}</pre></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Modal>
  );
}
