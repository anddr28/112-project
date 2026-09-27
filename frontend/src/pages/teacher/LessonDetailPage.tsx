import { useEffect, useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { api } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { Badge, Card, ErrorState, LessonStatusBadge, Loading, Metric } from '../../components/ui';
import { formatDateTime, formatDuration } from '../../shared/utils/time';
import type { Attempt, Evaluation } from '../../shared/types';
import type { ReportFormat } from '../../shared/api';

/** Форматы отчёта по занятию (ТЗ: выгрузка в стандартные форматы). */
const REPORT_FORMATS: Array<[ReportFormat, string]> = [['csv', 'CSV'], ['xlsx', 'Excel'], ['pdf', 'PDF']];

const PARTICIPANT_LABEL: Record<string, { label: string; tone: 'neutral' | 'accent' | 'ok' | 'warn' | 'danger' }> = {
  assigned: { label: 'Назначен', tone: 'neutral' },
  joined: { label: 'Подключился', tone: 'accent' },
  active: { label: 'Работает', tone: 'warn' },
  disconnected: { label: 'Нет связи', tone: 'danger' },
  finished: { label: 'Завершил', tone: 'ok' },
};

export function LessonDetailPage() {
  const { lessonId = '' } = useParams();
  const lesson = useAsync(() => api.lessons.get(lessonId), [lessonId]);
  const attempts = useAsync(() => api.attempts.forLesson(lessonId), [lessonId]);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [tick, setTick] = useState(0);

  const isRunning = lesson.data?.status === 'running';

  // Мониторинг: в проде это WebSocket (GAP-18). Здесь — периодическое обновление.
  useEffect(() => {
    if (!isRunning) return;
    const id = setInterval(() => setTick((t) => t + 1), 3000);
    return () => clearInterval(id);
  }, [isRunning]);

  useEffect(() => {
    if (tick > 0) {
      lesson.reload();
      attempts.reload();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tick]);

  // Заглушка загрузки — только пока показывать нечего.
  if (lesson.loading && !lesson.data) return <Loading />;
  if (lesson.error) return <ErrorState text={lesson.error} onRetry={lesson.reload} />;
  if (!lesson.data) return <ErrorState text="Занятие не найдено" />;

  const l = lesson.data;
  const attemptList = attempts.data ?? [];

  /**
   * Запуск и завершение занятия — операции сервера. Отказ показываем рядом
   * с кнопкой: иначе кнопка осталась бы заблокированной без объяснения.
   */
  async function run(action: () => Promise<unknown>, failure: string) {
    setBusy(true);
    setActionError(null);
    try {
      await action();
      lesson.reload();
      attempts.reload();
    } catch (e) {
      setActionError(e instanceof Error ? e.message : failure);
    } finally {
      setBusy(false);
    }
  }

  const finished = l.participants.filter((p) => p.status === 'finished').length;

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <div className="row row--tight" style={{ marginBottom: 6 }}>
            <Link to="/teacher/lessons" className="small">← Занятия</Link>
          </div>
          <h1>{l.title}</h1>
          <div className="row row--tight" style={{ marginTop: 8 }}>
            <LessonStatusBadge status={l.status} />
            <Badge tone="neutral">{l.perspective === 'operator112' ? 'Оператор-112' : 'Диспетчер ДДС'}</Badge>
            <Badge tone="neutral">Норматив {l.timeLimitSec} с</Badge>
            <Badge tone="neutral">Порог {l.settings.passThreshold}</Badge>
            {l.settings.voice.enabled && <Badge tone="accent">Голосовой режим</Badge>}
          </div>
        </div>

        <div className="page-head__actions">
          {(l.status === 'draft' || l.status === 'scheduled') && (
            <button
              type="button"
              className="btn btn--primary"
              onClick={() => void run(() => api.lessons.start(lessonId), 'Не удалось запустить занятие')}
              disabled={busy}
            >
              Запустить занятие
            </button>
          )}
          {l.status === 'running' && (
            <button
              type="button"
              className="btn btn--danger"
              onClick={() => void run(() => api.lessons.finish(lessonId), 'Не удалось завершить занятие')}
              disabled={busy}
            >
              Завершить занятие
            </button>
          )}
          {/* Отчёт формирует сервер (v1.2); у mock-реализации выгрузки нет. */}
          {api.lessons.reportUrl && (l.status === 'running' || l.status === 'finished') && (
            <span className="row row--tight" aria-label="Выгрузка отчёта по занятию">
              <span className="dim small">Отчёт:</span>
              {REPORT_FORMATS.map(([format, label]) => (
                <a key={format} className="btn" href={api.lessons.reportUrl?.(lessonId, format)} download>
                  {label}
                </a>
              ))}
            </span>
          )}
        </div>
      </div>

      {actionError && <p className="field__error" role="alert" style={{ marginBottom: 12 }}>{actionError}</p>}

      <div className="grid grid--4" style={{ marginBottom: 16 }}>
        <Metric label="Участников" value={l.participants.length} />
        <Metric label="Завершили" value={`${finished} / ${l.participants.length}`} tone={finished === l.participants.length && finished > 0 ? 'ok' : undefined} />
        <Metric label="Сценариев в пуле" value={l.scenarioIds.length} />
        <Metric label="Начато" value={l.startedAt ? formatDateTime(l.startedAt).slice(11) : '—'} note={l.startedAt ? formatDateTime(l.startedAt).slice(0, 10) : 'занятие не запущено'} />
      </div>

      <Card
        title={isRunning ? 'Мониторинг в реальном времени' : 'Участники'}
        actions={
          isRunning ? (
            <span className="dim small">
              {lesson.refreshing || attempts.refreshing ? 'Обновление…' : 'Обновляется автоматически'}
            </span>
          ) : undefined
        }
      >
        {l.participants.length === 0 ? (
          <p className="muted small">Участники не назначены.</p>
        ) : (
          <table className="table">
            <thead>
              <tr><th>Обучающийся</th><th>Состояние</th><th>Карточка</th><th>Время</th><th>Результат</th><th /></tr>
            </thead>
            <tbody>
              {l.participants.map((p) => {
                const attempt = attemptList.find((a) => a.userId === p.userId);
                const st = PARTICIPANT_LABEL[p.status] ?? { label: p.status, tone: 'neutral' as const };
                return (
                  <tr key={p.userId}>
                    <td>{p.name}</td>
                    <td><Badge tone={st.tone}>{st.label}</Badge></td>
                    <td className="mono small">{attempt?.incidentNo ?? '—'}</td>
                    <td className="mono small nowrap">
                      {attempt?.timeSpentMs != null ? formatDuration(attempt.timeSpentMs) : '—'}
                    </td>
                    <td><AttemptScore attempt={attempt} /></td>
                    <td>
                      {attempt && (attempt.status === 'evaluated' || attempt.status === 'evaluating') && (
                        <Link className="btn btn--sm" to={`/teacher/attempts/${attempt.id}`}>Разбор</Link>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </Card>

      {l.status === 'draft' && (
        <p className="muted small" style={{ marginTop: 12 }}>
          Занятие ещё не запущено — обучающиеся не получат карточку, пока вы не нажмёте «Запустить занятие».
        </p>
      )}
    </>
  );
}

function AttemptScore({ attempt }: { attempt?: Attempt }) {
  const [evaluation, setEvaluation] = useState<Evaluation | null>(null);
  /*
   * Опрос мониторинга пересоздаёт объект попытки каждые несколько секунд.
   * Зависимости — идентификатор и статус, иначе оценка перезапрашивалась бы
   * на каждом обновлении списка.
   */
  const attemptId = attempt?.id;
  const attemptStatus = attempt?.status;

  useEffect(() => {
    if (!attemptId) return;
    let cancelled = false;
    void api.evaluation
      .get(attemptId)
      .then((ev) => {
        if (!cancelled) setEvaluation(ev);
      })
      .catch(() => {});
    return () => { cancelled = true; };
  }, [attemptId, attemptStatus]);

  if (!attempt) return <span className="dim">—</span>;
  if (!evaluation) return <span className="dim small">не оценено</span>;
  if (evaluation.status === 'partial') return <Badge tone="warn">Анализируется…</Badge>;

  return (
    <Badge tone={evaluation.verdict === 'pass' ? 'ok' : 'danger'}>
      {evaluation.finalScore} · {evaluation.verdict === 'pass' ? 'зачёт' : 'незачёт'}
    </Badge>
  );
}
