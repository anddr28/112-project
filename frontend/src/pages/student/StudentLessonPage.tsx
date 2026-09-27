import { Link, useNavigate, useParams } from 'react-router-dom';
import { api } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { useAuth } from '../../app/auth';
import { Badge, Card, ErrorState, LessonStatusBadge, Loading } from '../../components/ui';
import { ForbiddenPage } from '../errors/ErrorPages';

export function StudentLessonPage() {
  const { lessonId = '' } = useParams();
  const user = useAuth((s) => s.user);
  const navigate = useNavigate();
  const lesson = useAsync(() => api.lessons.get(lessonId), [lessonId]);
  const assigned = useAsync(() => api.lessons.assigned(), [user?.id]);

  if (lesson.loading || assigned.loading) return <Loading />;
  if (lesson.error) return <ErrorState text={lesson.error} onRetry={lesson.reload} />;
  if (!lesson.data) return <ErrorState text="Занятие не найдено" />;

  /*
   * Занятие открывается по адресу, поэтому сверяем его с назначенными:
   * обучающийся работает только со своими заданиями. Права всё равно
   * проверяет сервер — здесь мы просто не показываем чужое.
   */
  const assignment = (assigned.data ?? []).find((i) => i.lesson.id === lessonId);
  if (!assignment) {
    return (
      <ForbiddenPage text="Это занятие вам не назначено. Доступны только ваши учебные задания — ограничение проверяется и на сервере." />
    );
  }

  const l = lesson.data;
  const attempt = assignment.attempt;
  const dds = l.perspective === 'dds';

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <div className="row row--tight" style={{ marginBottom: 6 }}>
            <Link to="/student" className="small">← Мои занятия</Link>
          </div>
          <h1>{l.title}</h1>
          <div className="row row--tight" style={{ marginTop: 8 }}>
            <LessonStatusBadge status={l.status} />
            <Badge tone="neutral">{l.perspective === 'operator112' ? 'Оператор-112' : 'Диспетчер ДДС'}</Badge>
            <Badge tone="neutral">Норматив {l.timeLimitSec} с</Badge>
            {l.settings.voice.enabled && <Badge tone="accent">Разговор с заявителем</Badge>}
          </div>
        </div>
      </div>

      <div className="grid grid--sidebar">
        <Card title="Порядок работы">
          {dds ? (
            <ol style={{ margin: 0, paddingLeft: 20, lineHeight: 1.9 }}>
              <li>Возьмите в работу карточку, поступившую от оператора 112, — с этого момента идёт отсчёт.</li>
              <li>Изучите карточку: она доступна только для чтения.</li>
              <li>В списке оповещения откройте свою службу.</li>
              <li>В течение 30 с поставьте «Принята» или «Не принята» (с причиной в комментарии).</li>
              <li>Проставляйте статусы реагирования последовательно, с комментариями.</li>
              <li>Запишите текст действия и завершите работу с карточкой.</li>
            </ol>
          ) : (
            <ol style={{ margin: 0, paddingLeft: 20, lineHeight: 1.9 }}>
              <li>Примите входящий вызов — с этого момента идёт отсчёт времени.</li>
              <li>Выслушайте заявителя; при необходимости переспросите.</li>
              <li>Выберите тип происшествия в блоке «Что случилось?».</li>
              <li>Заполните адрес через единую адресную строку.</li>
              <li>Ответьте на дополнительные вопросы опросной карты.</li>
              <li>Заполните заявителя, описание и сведения о пострадавших.</li>
              <li>Проверьте автоматически определённый список служб.</li>
              <li>Проставьте статусы реагирования служб.</li>
              <li>Сохраните карточку — службы будут оповещены.</li>
            </ol>
          )}

          <div className="divider" style={{ margin: '14px 0' }} />

          <p className="muted small">
            Превышение норматива не прерывает работу: таймер станет красным,
            {dds
              ? ' но решение и статусы нужно проставить, а текст действия — записать.'
              : ' но карточку нужно заполнить полностью и корректно.'}
          </p>
        </Card>

        <Card title="Ваша попытка">
          {!attempt ? (
            <p className="muted small">
              {l.status === 'running'
                ? 'Карточка ещё не выдана. Обновите страницу через несколько секунд.'
                : l.status === 'finished' || l.status === 'cancelled'
                  ? 'Занятие закрыто преподавателем.'
                  : 'Занятие не запущено преподавателем.'}
            </p>
          ) : (
            <div className="stack">
              <div>
                <div className="field__label">Карточка происшествия</div>
                <div className="mono">№ {attempt.incidentNo}</div>
              </div>
              <div>
                <div className="field__label">Состояние</div>
                <div>
                  {attempt.status === 'issued'
                    ? dds ? 'Поступила карточка' : 'Поступил вызов'
                    : attempt.status === 'in_progress'
                      ? 'В работе'
                      : attempt.status === 'expired' || attempt.status === 'aborted'
                        ? 'Не выполнена'
                        : 'Завершена'}
                </div>
              </div>

              {attempt.status === 'issued' && (
                <button
                  type="button"
                  className="btn btn--primary btn--block"
                  onClick={() => navigate(`/student/attempts/${attempt.id}/call`)}
                >
                  {dds ? 'Взять в работу' : 'Принять вызов'}
                </button>
              )}
              {attempt.status === 'in_progress' && (
                <button
                  type="button"
                  className="btn btn--primary btn--block"
                  onClick={() => navigate(`/student/attempts/${attempt.id}/arm`)}
                >
                  {dds ? 'Вернуться к карточке' : 'Вернуться в АРМ-112'}
                </button>
              )}
              {(attempt.status === 'evaluated' || attempt.status === 'evaluating' || attempt.status === 'submitted') && (
                <button
                  type="button"
                  className="btn btn--block"
                  onClick={() => navigate(`/student/attempts/${attempt.id}/result`)}
                >
                  Посмотреть результат
                </button>
              )}
            </div>
          )}
        </Card>
      </div>
    </>
  );
}
