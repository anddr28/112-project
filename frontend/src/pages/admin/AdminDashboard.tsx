import { api } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { Badge, Card, ErrorState, Loading, Metric } from '../../components/ui';
import { formatDateTime } from '../../shared/utils/time';
import type { SystemHealth } from '../../shared/types';

const HEALTH_LABEL: Record<SystemHealth['status'], string> = { ok: 'в норме', degraded: 'частично', down: 'недоступен' };
const HEALTH_TONE: Record<SystemHealth['status'], 'ok' | 'warn' | 'danger'> = { ok: 'ok', degraded: 'warn', down: 'danger' };

/** Состояние компонента по ответу сервера; нет ответа — «нет данных». */
function StateBadge({ ok }: { ok: boolean | undefined }) {
  if (ok === undefined) return <Badge tone="neutral">нет данных</Badge>;
  return ok ? <Badge tone="ok">работает</Badge> : <Badge tone="danger">недоступен</Badge>;
}

/**
 * Технический раздел администратора.
 *
 * Сознательно НЕ содержит: изменения оценок, редактирования активных занятий
 * и попыток, действий «от имени обучающегося», удаления учебных результатов.
 * Администратор отвечает за техническое состояние системы, а не за учебный процесс.
 *
 * Состояние контура — GET /admin/health (v1.2). Журнал аудита и резервные
 * копии в контракте есть (/admin/audit, /admin/backups), экранов под них пока нет.
 */
export function AdminDashboard() {
  const users = useAsync(() => api.users.list(), []);
  const lessons = useAsync(() => api.lessons.list(), []);
  // У mock-реализации сервера нет — и состояния контура тоже.
  const health = useAsync(() => (api.admin ? api.admin.health() : Promise.resolve(null)), []);

  if (users.loading || lessons.loading) return <Loading />;
  if (users.error) return <ErrorState text={users.error} onRetry={users.reload} />;
  if (lessons.error) return <ErrorState text={lessons.error} onRetry={lessons.reload} />;

  const allUsers = users.data ?? [];
  const blocked = allUsers.filter((u) => u.status === 'blocked').length;
  const running = (lessons.data ?? []).filter((l) => l.status === 'running').length;
  const h = health.data;
  const ai = h?.aiService;
  const jobsQueued = (h?.jobs?.queued ?? 0) + (h?.jobs?.running ?? 0);

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <h1>Состояние системы</h1>
          <div className="page-head__sub">
            Технический мониторинг изолированного учебного контура.
          </div>
        </div>
      </div>

      {/*
        * Показываем только то, что сообщил сервер. Нет ответа (mock-режим
        * или ядро недоступно) — честное «нет данных», а не правдоподобные цифры.
        */}
      <div className="grid grid--4" style={{ marginBottom: 16 }}>
        <Metric label="Учётных записей" value={allUsers.length} note={`заблокировано: ${blocked}`} tone={blocked ? 'warn' : undefined} />
        <Metric label="Занятий идёт" value={running} tone={running ? 'ok' : undefined} />
        <Metric
          label="Состояние контура"
          value={h ? HEALTH_LABEL[h.status] : 'нет данных'}
          note={h ? `задач ИИ в работе: ${jobsQueued}` : health.error ?? 'сервер состояния не сообщает'}
          tone={h ? HEALTH_TONE[h.status] : undefined}
        />
        <Metric
          label="Последняя резервная копия"
          value={h?.postgres.lastBackupAt ? formatDateTime(h.postgres.lastBackupAt) : 'нет данных'}
          note="ТЗ: не реже раза в сутки"
        />
      </div>

      <div className="grid grid--2">
        <Card title="Компоненты">
          <table className="table">
            <thead><tr><th>Компонент</th><th>Состояние</th><th>Примечание</th></tr></thead>
            <tbody>
              <tr>
                <td>Ядро системы</td>
                <td><StateBadge ok={h ? true : undefined} /></td>
                <td className="muted small">{h ? `версия ${h.goCore.version ?? '—'}, сессий ${h.goCore.activeSessions ?? 0}, WebSocket ${h.goCore.wsConnections ?? 0}` : '—'}</td>
              </tr>
              <tr>
                <td>База данных</td>
                <td><StateBadge ok={h?.postgres.ok} /></td>
                <td className="muted small">{h?.postgres.latencyMs != null ? `отклик ${h.postgres.latencyMs} мс` : '—'}</td>
              </tr>
              <tr>
                <td>Служба искусственного интеллекта</td>
                <td><StateBadge ok={ai?.ok} /></td>
                <td className="muted small">
                  {ai?.modelsAvailable?.length ? `модели: ${ai.modelsAvailable.join(', ')}` : '—'}
                  {ai?.breakerState === 'open' ? ' · запросы приостановлены' : ''}
                </td>
              </tr>
              <tr>
                <td>Распознавание речи</td>
                <td><StateBadge ok={ai?.stt} /></td>
                <td className="muted small">{ai?.profiles?.stt_default ?? '—'}</td>
              </tr>
              <tr>
                <td>Синтез речи</td>
                <td><StateBadge ok={ai?.tts} /></td>
                <td className="muted small">{ai?.profiles?.tts_default ?? 'озвучка реплик заявителя'}</td>
              </tr>
            </tbody>
          </table>
        </Card>

        <Card title="Разграничение полномочий">
          <p className="muted small" style={{ marginBottom: 10 }}>
            Администратор управляет техническим состоянием системы и не управляет
            учебным процессом. В этом разделе отсутствуют функции:
          </p>
          <ul style={{ margin: 0, paddingLeft: 18, lineHeight: 1.8 }}>
            <li>изменение оценок обучающихся;</li>
            <li>редактирование активного занятия или сценария преподавателя;</li>
            <li>работа с карточкой от имени обучающегося;</li>
            <li>удаление попыток, событий и результатов обучения.</li>
          </ul>
          <p className="field__hint" style={{ marginTop: 12 }}>
            Ограничения проверяются на сервере; сокрытие элементов интерфейса
            не является механизмом защиты.
          </p>
        </Card>
      </div>
    </>
  );
}
