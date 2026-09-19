import { useEffect, useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { api } from '../../shared/api';
import { useAuth } from '../../app/auth';
import { shortName } from '../../shared/utils/user';
import { useAsync } from '../../shared/api/useAsync';
import { Card, ErrorState, Field, Loading, Metric, Modal, NumberInput } from '../../components/ui';
import { EvaluationView } from '../../features/evaluation/EvaluationView';
import { EtalonCardView } from '../../features/incident-card/EtalonCardView';
import { formatDateTime, formatDelta, formatDuration } from '../../shared/utils/time';
import { labelForPath, labelForValue } from '../../shared/utils/labels';
import type { ClassifierLabels } from '../../shared/api';
import type { Evaluation } from '../../shared/types';

const EVENT_LABEL: Record<string, string> = {
  issued: 'Карточка выдана',
  call_accepted: 'Вызов принят — старт таймера',
  open_card: 'Открыта карточка',
  field_changed: 'Изменено поле',
  choose_value: 'Выбрано значение',
  service_assigned: 'Назначена служба',
  service_removed: 'Служба снята вручную',
  service_status_changed: 'Изменён статус службы',
  replay: 'Переспросил заявителя',
  save: 'Сохранение',
  submitted: 'Карточка отправлена',
  timer_expired: 'Истёк норматив',
  disconnected: 'Потеряна связь',
  reconnected: 'Связь восстановлена',
};

export function AttemptReportPage() {
  const { attemptId = '' } = useParams();
  const attempt = useAsync(() => api.attempts.get(attemptId), [attemptId]);
  const events = useAsync(() => api.attempts.events(attemptId), [attemptId]);
  const feedback = useAsync(() => api.feedback.list(attemptId), [attemptId]);
  const labels = useAsync(() => api.classifier.labels(), []);
  const [evaluation, setEvaluation] = useState<Evaluation | null>(null);
  const [tab, setTab] = useState<'evaluation' | 'compare' | 'timeline'>('evaluation');
  const [overrideOpen, setOverrideOpen] = useState(false);
  const [feedbackOpen, setFeedbackOpen] = useState(false);

  useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout>;
    function poll() {
      void api.evaluation
        .get(attemptId)
        .then((ev) => {
          if (cancelled) return;
          setEvaluation(ev);
          // Оценки нет вообще — попытка не завершена, ждать нечего.
          if (ev && (ev.status === 'partial' || ev.status === 'pending')) {
            timer = setTimeout(poll, 1500);
          }
        })
        .catch(() => {});
    }
    poll();
    return () => { cancelled = true; clearTimeout(timer); };
  }, [attemptId]);

  const scenarioId = attempt.data?.scenarioId;
  const scenario = useAsync(
    () => (scenarioId ? api.scenarios.get(scenarioId) : Promise.resolve(null)),
    [scenarioId],
  );
  const lessonId = attempt.data?.lessonId;
  const lesson = useAsync(
    () => (lessonId ? api.lessons.get(lessonId) : Promise.resolve(null)),
    [lessonId],
  );

  if (attempt.loading) return <Loading />;
  if (attempt.error) return <ErrorState text={attempt.error} onRetry={attempt.reload} />;
  if (!attempt.data) return <ErrorState text="Попытка не найдена" />;

  // Порог и веса показываем только когда известны настройки занятия:
  // подстановка «обычных» значений расходилась бы с фактическими.
  if (lesson.loading) return <Loading />;
  if (lesson.error) return <ErrorState text={lesson.error} onRetry={lesson.reload} />;
  if (!lesson.data) return <ErrorState text="Занятие не найдено" />;

  const a = attempt.data;
  const threshold = lesson.data.settings.passThreshold;
  const participant = lesson.data.participants.find((p) => p.userId === a.userId);

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <div className="row row--tight" style={{ marginBottom: 6 }}>
            <Link to={`/teacher/lessons/${a.lessonId}`} className="small">← Занятие</Link>
          </div>
          <h1>Разбор попытки</h1>
          <div className="page-head__sub">
            {participant?.name ?? a.userId} · {scenario.data?.title ?? '—'} · карточка № {a.incidentNo}
          </div>
        </div>
        <div className="page-head__actions">
          <button type="button" className="btn" onClick={() => setFeedbackOpen(true)}>Оставить комментарий</button>
          <button
            type="button"
            className="btn"
            disabled={!evaluation || evaluation.status !== 'done'}
            title={evaluation?.status === 'done' ? undefined : 'Доступно после завершения автоматической проверки'}
            onClick={() => setOverrideOpen(true)}
          >
            Ручная корректировка оценки
          </button>
        </div>
      </div>

      <div className="grid grid--4" style={{ marginBottom: 16 }}>
        <Metric label="Дата и время" value={a.submittedAt ? formatDateTime(a.submittedAt).slice(11) : '—'} note={a.submittedAt ? formatDateTime(a.submittedAt).slice(0, 10) : ''} />
        <Metric label="Время заполнения" value={a.timeSpentMs != null ? formatDuration(a.timeSpentMs) : '—'} />
        <Metric
          label="Отклонение от норматива"
          value={evaluation ? formatDelta(evaluation.timing.deltaMs) : '—'}
          note={`норматив ${a.timeLimitSec} с`}
          tone={evaluation?.timing.withinNorm === false ? 'danger' : evaluation ? 'ok' : undefined}
        />
        <Metric
          label="Итоговый балл"
          value={evaluation?.status === 'done' ? evaluation.finalScore : '—'}
          note={evaluation?.override ? 'скорректирован вручную' : `порог ${threshold}`}
          tone={evaluation?.status === 'done' ? (evaluation.finalScore >= threshold ? 'ok' : 'danger') : undefined}
        />
      </div>

      <div className="row row--tight" style={{ marginBottom: 14 }}>
        <button type="button" className={`btn ${tab === 'evaluation' ? 'btn--primary' : ''}`} onClick={() => setTab('evaluation')}>Оценка</button>
        <button type="button" className={`btn ${tab === 'compare' ? 'btn--primary' : ''}`} onClick={() => setTab('compare')}>Карточка ↔ эталон</button>
        <button type="button" className={`btn ${tab === 'timeline' ? 'btn--primary' : ''}`} onClick={() => setTab('timeline')}>Хронология действий</button>
      </div>

      {tab === 'evaluation' && (
        evaluation ? (
          <EvaluationView evaluation={evaluation} threshold={threshold} weights={lesson.data.settings.weights} />
        ) : (
          <Card><Loading text="Оценка формируется…" /></Card>
        )
      )}

      {tab === 'compare' && (
        <div className="grid grid--2">
          <Card title="Карточка обучающегося">
            {a.card ? (
              <EtalonCardView card={a.card} highlight={evaluation?.fieldErrors.map((e) => e.field)} />
            ) : (
              <p className="muted small">Карточка не сохранена.</p>
            )}
          </Card>
          <Card title="Эталон">
            {scenario.data ? (
              <EtalonCardView card={scenario.data.etalonDraft} requiredFields={scenario.data.requiredFields} />
            ) : (
              <Loading />
            )}
          </Card>
        </div>
      )}

      {tab === 'timeline' && (
        <Card title="Хронология действий">
          {events.loading ? (
            <Loading />
          ) : (events.data ?? []).length === 0 ? (
            <p className="muted small">Событий нет.</p>
          ) : (
            <table className="table">
              <thead><tr><th style={{ width: 90 }}>Время</th><th>Событие</th><th>Детали</th></tr></thead>
              <tbody>
                {(events.data ?? []).map((e) => (
                  <tr key={e.clientSeq}>
                    <td className="mono small nowrap">{formatDateTime(e.at).slice(11)}</td>
                    <td>{EVENT_LABEL[e.type] ?? 'Действие обучающегося'}</td>
                    <td className="muted small">{renderPayload(e.payload, labels.data ?? undefined)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Card>
      )}

      {(feedback.data ?? []).length > 0 && (
        <div style={{ marginTop: 16 }}>
          <Card title="Комментарии преподавателя">
            <div className="stack">
              {(feedback.data ?? []).map((n) => (
                <div key={n.id}>
                  <div className="field__label">{n.teacherName} · {formatDateTime(n.createdAt)}</div>
                  <p>{n.comment}</p>
                  {n.recommendation && <p className="muted small">Рекомендация: {n.recommendation}</p>}
                </div>
              ))}
            </div>
          </Card>
        </div>
      )}

      {feedbackOpen && (
        <FeedbackModal
          attemptId={attemptId}
          onClose={() => setFeedbackOpen(false)}
          onDone={() => { feedback.reload(); setFeedbackOpen(false); }}
        />
      )}

      {overrideOpen && evaluation && (
        <OverrideModal
          attemptId={attemptId}
          current={evaluation}
          onClose={() => setOverrideOpen(false)}
          onDone={(ev) => { setEvaluation(ev); setOverrideOpen(false); }}
        />
      )}
    </>
  );
}

/** Ключи полезной нагрузки события — технические; в отчёте показываем их по-русски. */
const PAYLOAD_LABEL: Record<string, string> = {
  field: 'поле',
  value: 'значение',
  service: 'служба',
  status: 'статус',
  source: 'источник',
  hasComment: 'комментарий',
  scenarioId: 'сценарий',
  lineIndex: 'реплика',
};

const PAYLOAD_VALUE: Record<string, string> = {
  true: 'есть',
  false: 'нет',
  manual: 'вручную',
  auto: 'автоматически',
  vis: 'внешняя система',
};

function renderPayload(payload?: Record<string, unknown>, labels?: ClassifierLabels): string {
  if (!payload) return '';

  // Путь изменённого поля нужен, чтобы развернуть его значение в подпись.
  const path = typeof payload.field === 'string' ? payload.field : '';

  return Object.entries(payload)
    .map(([k, v]) => {
      const key = PAYLOAD_LABEL[k] ?? 'сведения';

      if (k === 'field' && typeof v === 'string') return `${key}: ${labelForPath(v, labels)}`;
      if (k === 'value') return `${key}: ${labelForValue(path, v, labels)}`;
      if (k === 'scenarioId') return `${key}: учебный сценарий`;

      const raw = Array.isArray(v) ? v.join(', ') : String(v);
      return `${key}: ${PAYLOAD_VALUE[raw] ?? raw}`;
    })
    .join(' · ');
}

function FeedbackModal({ attemptId, onClose, onDone }: { attemptId: string; onClose: () => void; onDone: () => void }) {
  const [comment, setComment] = useState('');
  const [recommendation, setRecommendation] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const author = useAuthorName();

  return (
    <Modal
      title="Комментарий к попытке"
      onClose={onClose}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose}>Отмена</button>
          <button
            type="button"
            className="btn btn--primary"
            disabled={busy || !comment.trim()}
            onClick={() => {
              setBusy(true);
              setError(null);
              void api.feedback
                .add(attemptId, comment, recommendation, author)
                .then(onDone)
                .catch((e: unknown) => {
                  setBusy(false);
                  setError(e instanceof Error ? e.message : 'Не удалось сохранить комментарий');
                });
            }}
          >
            Сохранить
          </button>
        </>
      }
    >
      <div className="stack">
        <Field label="Комментарий" hint="Виден обучающемуся на экране результата">
          <textarea className="textarea" value={comment} onChange={(e) => setComment(e.target.value)} />
        </Field>
        <Field label="Рекомендация">
          <input className="input" value={recommendation} onChange={(e) => setRecommendation(e.target.value)} />
        </Field>
        {error && <p className="field__error" role="alert">{error}</p>}
      </div>
    </Modal>
  );
}

/**
 * Ручная корректировка оценки.
 *
 * Отдельное явное действие, а не редактирование балла на месте. Причина
 * обязательна — в модели БД это CHECK-констрейнт, и корректировка попадает
 * в журнал аудита. Автоматическая оценка при этом не подменяется: обе величины
 * остаются видимыми и обучающемуся, и преподавателю.
 */
function OverrideModal({
  attemptId,
  current,
  onClose,
  onDone,
}: {
  attemptId: string;
  current: Evaluation;
  onClose: () => void;
  onDone: (ev: Evaluation) => void;
}) {
  const [score, setScore] = useState(current.totalScore);
  const [reason, setReason] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const author = useAuthorName();

  return (
    <Modal
      title="Ручная корректировка оценки"
      onClose={onClose}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose}>Отмена</button>
          <button
            type="button"
            className="btn btn--primary"
            disabled={busy || !reason.trim()}
            onClick={() => {
              setBusy(true);
              setError(null);
              void api.evaluation
                .override(attemptId, score, reason, author)
                .then(onDone)
                .catch((e: unknown) => {
                  setBusy(false);
                  setError(e instanceof Error ? e.message : 'Не удалось применить корректировку');
                });
            }}
          >
            Применить корректировку
          </button>
        </>
      }
    >
      <div className="stack">
        <div className="card" style={{ boxShadow: 'none' }}>
          <div className="card__body row row--between">
            <span className="muted small">Автоматическая оценка</span>
            <b className="mono">{current.totalScore}</b>
          </div>
        </div>

        <Field label="Новый балл">
          <NumberInput min={0} max={100} value={score} onChange={setScore} />
        </Field>

        <Field
          label="Причина корректировки"
          hint="Обязательна. Корректировка фиксируется в журнале аудита и отображается обучающемуся."
        >
          <textarea className="textarea" value={reason} onChange={(e) => setReason(e.target.value)} />
        </Field>

        {error && <p className="field__error" role="alert">{error}</p>}

        <p className="field__hint">
          Автоматическая оценка не удаляется: на экране результата будут видны обе величины
          с указанием автора и причины.
        </p>
      </div>
    </Modal>
  );
}

/** Автор действия — текущий авторизованный преподаватель, а не фиксированное имя. */
function useAuthorName(): string {
  const user = useAuth((s) => s.user);
  return user ? shortName(user) : '';
}
