import { Link } from 'react-router-dom';
import { api } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { Card, ErrorState, LessonStatusBadge, Loading, Metric, ScenarioStatusBadge } from '../../components/ui';
import { formatDate } from '../../shared/utils/time';

export function TeacherDashboard() {
  const lessons = useAsync(() => api.lessons.list(), []);
  const scenarios = useAsync(() => api.scenarios.list(), []);

  if (lessons.loading || scenarios.loading) return <Loading />;
  if (lessons.error) return <ErrorState text={lessons.error} onRetry={lessons.reload} />;
  if (scenarios.error) return <ErrorState text={scenarios.error} onRetry={scenarios.reload} />;

  const allLessons = lessons.data ?? [];
  const allScenarios = scenarios.data ?? [];

  const running = allLessons.filter((l) => l.status === 'running');
  const needReview = allScenarios.filter((s) => s.status === 'generated');
  const validated = allScenarios.filter((s) => s.status === 'validated');

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <h1>Обзор</h1>
          <div className="page-head__sub">Учебный процесс: сценарии, занятия и результаты обучающихся.</div>
        </div>
        <div className="page-head__actions">
          <Link className="btn" to="/teacher/scenarios">Сценарии</Link>
          <Link className="btn btn--primary" to="/teacher/lessons">Занятия</Link>
        </div>
      </div>

      <div className="grid grid--4" style={{ marginBottom: 16 }}>
        <Metric label="Идут занятия" value={running.length} note={running.length ? 'мониторинг доступен' : 'нет активных'} tone={running.length ? 'ok' : undefined} />
        <Metric label="Сценариев подтверждено" value={validated.length} note="готовы к выдаче" />
        <Metric label="Требуют проверки" value={needReview.length} note="сгенерированы ИИ" tone={needReview.length ? 'warn' : undefined} />
        <Metric label="Всего занятий" value={allLessons.length} />
      </div>

      <div className="grid grid--2">
        <Card
          title="Занятия"
          actions={<Link className="btn btn--sm" to="/teacher/lessons">Все занятия</Link>}
        >
          {allLessons.length === 0 ? (
            <p className="muted small">Занятий пока нет.</p>
          ) : (
            <table className="table">
              <thead>
                <tr><th>Занятие</th><th>Участники</th><th>Норматив</th><th>Статус</th></tr>
              </thead>
              <tbody>
                {allLessons.slice(0, 6).map((lesson) => (
                  <tr key={lesson.id}>
                    <td>
                      <Link to={`/teacher/lessons/${lesson.id}`}>{lesson.title}</Link>
                      <div className="dim small">{formatDate(lesson.createdAt)}</div>
                    </td>
                    <td className="mono">{lesson.participants.length}</td>
                    <td className="mono nowrap">{lesson.timeLimitSec} с</td>
                    <td><LessonStatusBadge status={lesson.status} /></td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Card>

        <Card
          title="Сценарии, требующие проверки"
          actions={<Link className="btn btn--sm" to="/teacher/scenarios">Все сценарии</Link>}
        >
          {needReview.length === 0 ? (
            <p className="muted small">
              Все сгенерированные сценарии проверены. ИИ не является окончательным источником
              истины — сценарий подтверждает преподаватель.
            </p>
          ) : (
            <div className="stack">
              {needReview.map((s) => (
                <div key={s.id} className="row row--between" style={{ paddingBottom: 8, borderBottom: '1px solid var(--u-border)' }}>
                  <div>
                    <Link to={`/teacher/scenarios/${s.id}`}>{s.title}</Link>
                    <div className="dim small">{s.categoryName}</div>
                  </div>
                  <ScenarioStatusBadge status={s.status} />
                </div>
              ))}
            </div>
          )}
        </Card>
      </div>
    </>
  );
}
