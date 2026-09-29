import { useEffect } from 'react';
import { Link } from 'react-router-dom';
import { api } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { Badge, Card, ErrorState, Loading, Metric } from '../../components/ui';
import { formatDateTime, formatDuration, isOlderThan } from '../../shared/utils/time';
import type { SystemHealth } from '../../shared/types';

const HEALTH_LABEL: Record<SystemHealth['status'], string> = { ok: 'в норме', degraded: 'частично', down: 'недоступен' };
const HEALTH_TONE: Record<SystemHealth['status'], 'ok' | 'warn' | 'danger'> = { ok: 'ok', degraded: 'warn', down: 'danger' };
const BREAKER: Record<string, { label: string; tone: 'ok' | 'warn' | 'danger' }> = {
  closed: { label: 'запросы идут', tone: 'ok' },
  half_open: { label: 'пробный запрос', tone: 'warn' },
  open: { label: 'запросы приостановлены', tone: 'danger' },
};
const JOB_TYPE: Record<string, string> = {
  evaluate_grammar: 'проверка грамотности',
  evaluate_semantic: 'проверка смысла',
  evaluate_dialogue: 'оценка разговора',
  generate_scenario: 'генерация сценария',
  tts: 'озвучка реплик',
};
const JOB_STATUS: Record<string, string> = {
  queued: 'в очереди',
  running: 'выполняется',
  done: 'выполнено',
  failed: 'с ошибкой',
  cancelled: 'отменено',
};
const PROFILE_LABEL: Record<string, string> = {
  eval_fast: 'Быстрая оценка',
  eval_thorough: 'Тщательная оценка',
  generate: 'Генерация сценариев',
  dialogue: 'Заявитель в разговоре',
  tts_default: 'Синтез речи',
  stt_default: 'Распознавание речи',
};

/** Состояние контура обновляется само: администратор держит экран открытым. */
const HEALTH_POLL_MS = 5000;

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
 */
export function AdminDashboard() {
  const users = useAsync(() => api.users.list(), []);
  const lessons = useAsync(() => api.lessons.list(), []);
  const health = useAsync(() => api.admin.health(), []);

  useEffect(() => {
    const id = setInterval(health.reload, HEALTH_POLL_MS);
    return () => clearInterval(id);
  }, [health.reload]);

  if (users.loading || lessons.loading) return <Loading />;
  if (users.error) return <ErrorState text={users.error} onRetry={users.reload} />;
  if (lessons.error) return <ErrorState text={lessons.error} onRetry={lessons.reload} />;

  const allUsers = users.data ?? [];
  const blocked = allUsers.filter((u) => u.status === 'blocked').length;
  const running = (lessons.data ?? []).filter((l) => l.status === 'running').length;
  const h = health.data;
  const ai = h?.aiService;
  const queue = ai?.queue;
  const breaker = ai?.breakerState ? BREAKER[ai.breakerState] : undefined;
  const pending = Object.entries(queue?.pending ?? {}).filter(([, n]) => n > 0);

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <h1>Состояние системы</h1>
          <div className="page-head__sub">
            Технический мониторинг изолированного учебного контура · обновляется каждые 5 с.
          </div>
        </div>
        <div className="page-head__actions">
          <Link className="btn" to="/admin/logs">Системный журнал</Link>
          <Link className="btn" to="/admin/backups">Резервные копии</Link>
        </div>
      </div>

      {/*
        * Показываем только то, что сообщил сервер. Нет ответа — честное
        * «нет данных», а не правдоподобные цифры.
        */}
      <div className="grid grid--4" style={{ marginBottom: 16 }}>
        <Metric label="Учётных записей" value={allUsers.length} note={`заблокировано: ${blocked}`} tone={blocked ? 'warn' : undefined} />
        <Metric label="Занятий идёт" value={running} tone={running ? 'ok' : undefined} />
        <Metric
          label="Состояние контура"
          value={h ? HEALTH_LABEL[h.status] : 'нет данных'}
          note={h ? `ядро: ${h.goCore.version ?? '—'}` : health.error ?? 'сервер состояния не сообщает'}
          tone={h ? HEALTH_TONE[h.status] : undefined}
        />
        <Metric
          label="Последняя резервная копия"
          value={h?.postgres.lastBackupAt ? formatDateTime(h.postgres.lastBackupAt) : 'нет данных'}
          note="ТЗ: не реже раза в сутки"
          tone={h?.postgres.lastBackupAt && isOlderThan(h.postgres.lastBackupAt, 86_400_000) ? 'danger' : undefined}
        />
      </div>

      <div className="grid grid--2">
        <Card title="Компоненты">
          <div className="table-scroll">
            <table className="table">
              <thead><tr><th>Компонент</th><th>Состояние</th><th>Примечание</th></tr></thead>
              <tbody>
                <tr>
                  <td>Ядро системы</td>
                  <td><StateBadge ok={h ? true : undefined} /></td>
                  <td className="muted small">
                    {h ? `${h.goCore.uptimeSec != null ? `работает ${formatDuration(h.goCore.uptimeSec * 1000)}, ` : ''}сессий ${h.goCore.activeSessions ?? 0}, WebSocket ${h.goCore.wsConnections ?? 0}` : '—'}
                  </td>
                </tr>
                <tr>
                  <td>База данных</td>
                  <td><StateBadge ok={h?.postgres.ok} /></td>
                  <td className="muted small">{h?.postgres.latencyMs != null ? `отклик ${h.postgres.latencyMs} мс` : '—'}</td>
                </tr>
                <tr>
                  <td>Служба ИИ</td>
                  <td><StateBadge ok={ai?.ok} /></td>
                  <td className="muted small">{breaker ? <Badge tone={breaker.tone}>{breaker.label}</Badge> : '—'}</td>
                </tr>
                <tr>
                  <td>Языковая модель (Ollama)</td>
                  <td><StateBadge ok={ai?.ollama} /></td>
                  <td className="muted small">{ai?.modelsAvailable?.length ? ai.modelsAvailable.join(', ') : '—'}</td>
                </tr>
                <tr>
                  <td>Проверка грамотности (LanguageTool)</td>
                  <td><StateBadge ok={ai?.languagetool} /></td>
                  <td className="muted small">—</td>
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
          </div>
        </Card>

        <div className="stack">
          <Card title="Очередь задач ИИ">
            {!h ? (
              <p className="muted small">Нет данных.</p>
            ) : (
              <table className="props">
                <tbody>
                  <tr><th>Выполняется сейчас</th><td className="num">{queue?.running ?? 0}</td></tr>
                  <tr>
                    <th>Ожидают</th>
                    <td>
                      {pending.length === 0 ? '0' : pending.map(([type, n]) => (
                        <div key={type} className="small">{JOB_TYPE[type] ?? type}: <b className="num">{n}</b></div>
                      ))}
                    </td>
                  </tr>
                  <tr><th>Разговоров ждут ответа</th><td className="num">{queue?.dialogWaiting ?? 0}</td></tr>
                  <tr><th>Средний ответ заявителя</th><td className="num">{queue?.dialogAvgMs != null ? `${(queue.dialogAvgMs / 1000).toFixed(1).replace('.', ',')} с` : '—'}</td></tr>
                  <tr><th>Оценка ожидания</th><td className="num">{queue?.estWaitSec != null ? `${queue.estWaitSec} с` : '—'}</td></tr>
                  <tr>
                    <th>Задачи по состоянию</th>
                    <td>
                      {Object.entries(h.jobs ?? {}).map(([status, n]) => (
                        <span key={status} className="small" style={{ marginRight: 10 }}>{JOB_STATUS[status] ?? status}: <b className="num">{n}</b></span>
                      ))}
                    </td>
                  </tr>
                </tbody>
              </table>
            )}
          </Card>

          {ai?.profiles && Object.keys(ai.profiles).length > 0 && (
            <Card title="Модели по профилям">
              <table className="props">
                <tbody>
                  {Object.entries(ai.profiles).map(([profile, model]) => (
                    <tr key={profile}><th>{PROFILE_LABEL[profile] ?? profile}</th><td className="mono small">{model}</td></tr>
                  ))}
                </tbody>
              </table>
            </Card>
          )}

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
          </Card>
        </div>
      </div>
    </>
  );
}
