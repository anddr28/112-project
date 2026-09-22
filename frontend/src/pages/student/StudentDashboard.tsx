import { Link, useNavigate } from 'react-router-dom';
import { api } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { useAuth } from '../../app/auth';
import { Badge, Card, EmptyState, ErrorState, LessonStatusBadge, Loading, Metric } from '../../components/ui';
import { formatDate, formatDuration } from '../../shared/utils/time';
import type { Attempt } from '../../shared/types';

export function StudentDashboard() {
  const user = useAuth((s) => s.user);
  const navigate = useNavigate();
  const assigned = useAsync(() => api.lessons.assigned(), [user?.id]);

  if (assigned.loading) return <Loading />;
  if (assigned.error) return <ErrorState text={assigned.error} onRetry={assigned.reload} />;

  const items = assigned.data ?? [];
  const active = items.filter((i) => i.lesson.status === 'running');
  const done = items.filter((i) => i.attempt?.status === 'evaluated').length;

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <h1>Мои занятия</h1>
          <div className="page-head__sub">
            Назначенные тренировки на рабочем месте АРМ-112
            {user?.serviceName ? ` · профиль: ${user.serviceName}` : ''}
          </div>
        </div>
      </div>

      <div className="grid grid--3" style={{ marginBottom: 16 }}>
        <Metric label="Назначено занятий" value={items.length} />
        <Metric label="Доступно сейчас" value={active.length} tone={active.length ? 'ok' : undefined} />
        <Metric label="Пройдено попыток" value={done} />
      </div>

      {items.length === 0 ? (
        <Card>
          <EmptyState
            title="Занятий пока нет"
            text="Преподаватель ещё не назначил вам тренировку. Как только занятие будет запущено, оно появится здесь."
          />
        </Card>
      ) : (
        <div className="stack">
          {items.map(({ lesson, attempt }) => {
            const state = describe(attempt, lesson.status);
            // Выносим в переменную: внутри JSX сужение типа по state.action теряется.
            const action = state.action;
            return (
              <Card
                key={lesson.id}
                title={lesson.title}
                actions={<LessonStatusBadge status={lesson.status} />}
                footer={
                  <>
                    <Link className="btn" to={`/student/lessons/${lesson.id}`}>Подробнее</Link>
                    {action && (
                      <button
                        type="button"
                        className="btn btn--primary"
                        onClick={() => navigate(action.to)}
                      >
                        {action.label}
                      </button>
                    )}
                  </>
                }
              >
                <div className="row" style={{ gap: 20 }}>
                  <div>
                    <div className="field__label">Рабочее место</div>
                    <div>{lesson.perspective === 'operator112' ? 'Оператор-112' : 'Диспетчер ДДС'}</div>
                  </div>
                  <div>
                    <div className="field__label">Норматив</div>
                    <div className="mono">{lesson.timeLimitSec} с</div>
                  </div>
                  <div>
                    <div className="field__label">Назначено</div>
                    <div className="small">{formatDate(lesson.createdAt)}</div>
                  </div>
                  <div>
                    <div className="field__label">Состояние</div>
                    <div><Badge tone={state.tone}>{state.label}</Badge></div>
                  </div>
                  {attempt?.timeSpentMs != null && (
                    <div>
                      <div className="field__label">Время выполнения</div>
                      <div className="mono">{formatDuration(attempt.timeSpentMs)}</div>
                    </div>
                  )}
                </div>
              </Card>
            );
          })}
        </div>
      )}
    </>
  );
}

function describe(
  attempt: Attempt | undefined,
  lessonStatus: string,
): { label: string; tone: 'neutral' | 'accent' | 'ok' | 'warn' | 'danger'; action?: { label: string; to: string } } {
  if (!attempt) {
    if (lessonStatus === 'running') return { label: 'Карточка выдаётся…', tone: 'warn' };
    if (lessonStatus === 'finished' || lessonStatus === 'cancelled') {
      return { label: 'Занятие закрыто', tone: 'neutral' };
    }
    return { label: 'Ожидает запуска преподавателем', tone: 'neutral' };
  }

  switch (attempt.status) {
    case 'issued':
      return {
        label: 'Поступил вызов',
        tone: 'warn',
        action: { label: 'Принять вызов', to: `/student/attempts/${attempt.id}/call` },
      };
    case 'in_progress':
      return {
        label: 'Обработка карточки',
        tone: 'warn',
        action: { label: 'Вернуться в АРМ-112', to: `/student/attempts/${attempt.id}/arm` },
      };
    case 'submitted':
    case 'evaluating':
      return {
        label: 'Проверяется',
        tone: 'accent',
        action: { label: 'Посмотреть результат', to: `/student/attempts/${attempt.id}/result` },
      };
    case 'evaluated':
      return {
        label: 'Завершена',
        tone: 'ok',
        action: { label: 'Результат', to: `/student/attempts/${attempt.id}/result` },
      };
    case 'expired':
      // expired: попытку закрыли до сохранения карточки — по таймауту или
      // при завершении занятия (контракт finishLesson).
      return { label: 'Не выполнена', tone: 'danger', action: { label: 'Результат', to: `/student/attempts/${attempt.id}/result` } };
    default:
      return { label: 'Прервана', tone: 'danger' };
  }
}
