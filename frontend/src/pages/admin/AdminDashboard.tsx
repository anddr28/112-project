import { api } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { Card, ErrorState, Loading, Metric } from '../../components/ui';

/**
 * Технический раздел администратора.
 *
 * Сознательно НЕ содержит: изменения оценок, редактирования активных занятий
 * и попыток, действий «от имени обучающегося», удаления учебных результатов.
 * Администратор отвечает за техническое состояние системы, а не за учебный процесс.
 *
 * TODO(backend) J-06..J-10: реальные метрики берутся из /admin/system/health,
 * /admin/logs, /admin/audit, /admin/backups — контракта пока нет.
 */
export function AdminDashboard() {
  const users = useAsync(() => api.users.list(), []);
  const lessons = useAsync(() => api.lessons.list(), []);

  if (users.loading || lessons.loading) return <Loading />;
  if (users.error) return <ErrorState text={users.error} onRetry={users.reload} />;
  if (lessons.error) return <ErrorState text={lessons.error} onRetry={lessons.reload} />;

  const allUsers = users.data ?? [];
  const blocked = allUsers.filter((u) => u.status === 'blocked').length;
  const running = (lessons.data ?? []).filter((l) => l.status === 'running').length;

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
        * Показываем только то, что система действительно знает. Состояние
        * сервисов, очередь задач ИИ и резервные копии доступны лишь во
        * внутренней сети (GAP-16, J-06..J-10), поэтому вместо правдоподобных
        * цифр здесь честное «нет данных».
        */}
      <div className="grid grid--4" style={{ marginBottom: 16 }}>
        <Metric label="Учётных записей" value={allUsers.length} note={`заблокировано: ${blocked}`} tone={blocked ? 'warn' : undefined} />
        <Metric label="Занятий идёт" value={running} tone={running ? 'ok' : undefined} />
        <Metric label="Состояние сервисов" value="нет данных" note="проверка доступна только во внутренней сети" />
        <Metric label="Последняя резервная копия" value="нет данных" note="появится после подключения серверной части" />
      </div>

      <div className="grid grid--2">
        <Card title="Компоненты">
          <table className="table">
            <thead><tr><th>Компонент</th><th>Состояние</th><th>Примечание</th></tr></thead>
            <tbody>
              <tr><td>Ядро системы</td><td><span className="badge badge--neutral">нет данных</span></td><td className="muted small">Контракт взаимодействия с интерфейсом не согласован</td></tr>
              <tr><td>Служба искусственного интеллекта</td><td><span className="badge badge--neutral">нет данных</span></td><td className="muted small">Проверка состояния доступна только внутри внутренней сети</td></tr>
              <tr><td>База данных</td><td><span className="badge badge--neutral">нет данных</span></td><td className="muted small">—</td></tr>
              <tr><td>Синтез речи</td><td><span className="badge badge--neutral">нет данных</span></td><td className="muted small">Озвучка реплик заявителя</td></tr>
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
