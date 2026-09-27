import { useEffect, useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { api } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { Badge, Card, EmptyState, ErrorState, Loading, Metric } from '../../components/ui';
import { EvaluationView } from '../../features/evaluation/EvaluationView';
import { EtalonCardView } from '../../features/incident-card/EtalonCardView';
import { DdsProtocolView } from '../../features/dds/DdsProtocolView';
import type { Evaluation } from '../../shared/types';

export function ResultPage() {
  const { attemptId = '' } = useParams();
  const attempt = useAsync(() => api.attempts.get(attemptId), [attemptId]);
  const feedback = useAsync(() => api.feedback.list(attemptId), [attemptId]);
  const [evaluation, setEvaluation] = useState<Evaluation | null>(null);
  /** оценка существует и ещё считается; false — проверять пока нечего */
  const [awaiting, setAwaiting] = useState(true);

  /*
   * Слои оценки доезжают асинхронно: опрашиваем, пока status не стал done.
   * Опрос прекращается, если оценки нет вообще: попытка ещё не завершена,
   * и ждать нечего — иначе экран крутил бы запрос бесконечно.
   */
  useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout>;

    function poll() {
      void api.evaluation
        .get(attemptId)
        .then((ev) => {
          if (cancelled) return;
          setEvaluation(ev);
          setAwaiting(ev != null);
          if (ev && (ev.status === 'pending' || ev.status === 'partial')) {
            timer = setTimeout(poll, 1500);
          }
        })
        // Ошибку самой попытки показывает useAsync ниже; здесь важно лишь
        // остановить опрос, а не оставить висеть «Проверяем карточку…».
        .catch(() => {
          if (!cancelled) setAwaiting(false);
        });
    }
    poll();

    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [attemptId]);

  const lessonId = attempt.data?.lessonId;
  const lesson = useAsync(
    () => (lessonId ? api.lessons.get(lessonId) : Promise.resolve(null)),
    [lessonId],
  );

  if (attempt.loading) return <Loading />;
  if (attempt.error) return <ErrorState text={attempt.error} onRetry={attempt.reload} />;
  if (!attempt.data) return <ErrorState text="Попытка не найдена" />;

  // Порог зачёта показываем только когда он известен из настроек занятия:
  // подстановка «обычных» 70 баллов расходилась бы с фактическим порогом.
  if (lesson.loading) return <Loading />;
  if (lesson.error) return <ErrorState text={lesson.error} onRetry={lesson.reload} />;
  if (!lesson.data) return <ErrorState text="Занятие не найдено" />;

  const threshold = lesson.data.settings.passThreshold;
  const notes = feedback.data ?? [];

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <div className="row row--tight" style={{ marginBottom: 6 }}>
            <Link to="/student" className="small">← Мои занятия</Link>
          </div>
          <h1>Результат обработки карточки</h1>
          <div className="page-head__sub">
            {attempt.data.lessonTitle} · карточка № {attempt.data.incidentNo}
          </div>
        </div>
      </div>

      {!evaluation ? (
        <Card>
          {awaiting ? (
            <Loading text="Проверяем карточку…" />
          ) : (
            attempt.data.status === 'expired' || attempt.data.status === 'aborted' ? (
              <EmptyState
                title="Попытка не выполнена"
                text="Попытка закрыта до сохранения карточки, поэтому оценки нет."
              />
            ) : (
              <EmptyState
                title="Попытка ещё не завершена"
                text="Результат появится после того, как карточка будет сохранена."
              />
            )
          )}
        </Card>
      ) : (
        <>
          <div className="grid grid--3" style={{ marginBottom: 16 }}>
            <Metric
              label="Итоговый балл"
              value={evaluation.status === 'partial' ? '—' : evaluation.finalScore}
              note={`порог зачёта ${threshold}`}
              tone={evaluation.status === 'partial' ? undefined : evaluation.finalScore >= threshold ? 'ok' : 'danger'}
            />
            <div className="metric">
              <span className="metric__label">Вердикт</span>
              <span className="metric__value" style={{ fontSize: 'var(--u-fs-lg)' }}>
                {evaluation.verdict === 'pending' ? (
                  <Badge tone="accent">Проверяется</Badge>
                ) : evaluation.verdict === 'pass' ? (
                  <Badge tone="ok">Зачёт</Badge>
                ) : (
                  <Badge tone="danger">Незачёт</Badge>
                )}
              </span>
              {evaluation.override && <span className="metric__note">с учётом ручной корректировки</span>}
            </div>
            {attempt.data.perspective === 'dds' ? (
              <Metric label="Служба" value={attempt.data.actingService?.shortName ?? '—'} note="рабочее место диспетчера ДДС" />
            ) : (
              <Metric label="Переспрашиваний" value={attempt.data.replayCount} note="повторных обращений к заявителю" />
            )}
          </div>

          <EvaluationView evaluation={evaluation} threshold={threshold} weights={lesson.data.settings.weights} />
        </>
      )}

      {notes.length > 0 && (
        <div style={{ marginTop: 16 }}>
          <Card title="Комментарий преподавателя">
            <div className="stack">
              {notes.map((n) => (
                <div key={n.id} style={{ paddingBottom: 10, borderBottom: '1px solid var(--u-border)' }}>
                  <div className="field__label">{n.teacherName}</div>
                  <p>{n.comment}</p>
                  {n.recommendation && (
                    <p className="muted small" style={{ marginTop: 4 }}>Рекомендация: {n.recommendation}</p>
                  )}
                </div>
              ))}
            </div>
          </Card>
        </div>
      )}

      {attempt.data.card && attempt.data.perspective === 'dds' && (
        <div style={{ marginTop: 16 }}>
          <Card title="Ваш протокол реагирования">
            <DdsProtocolView attempt={attempt.data} card={attempt.data.card} />
          </Card>
        </div>
      )}

      {attempt.data.card && (
        <div style={{ marginTop: 16 }}>
          <Card title={attempt.data.perspective === 'dds' ? 'Карточка оператора 112' : 'Ваша карточка'}>
            <EtalonCardView
              card={attempt.data.card}
              highlight={evaluation?.fieldErrors.map((e) => e.field)}
            />
          </Card>
        </div>
      )}
    </>
  );
}
