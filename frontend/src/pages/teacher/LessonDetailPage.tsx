import { useCallback, useEffect, useReducer, useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { api } from '../../shared/api';
import { Badge, Card, ErrorState, LessonStatusBadge, Loading, Metric } from '../../components/ui';
import { ChannelBadge } from '../../components/ChannelBadge';
import { useRealtimeChannel } from '../../shared/realtime/useRealtimeChannel';
import { currentAction, initialMonitor, monitorReducer } from '../../features/lesson-monitor/monitorState';
import type { MonitorAction } from '../../features/lesson-monitor/monitorState';
import { formatDateTime, formatDuration } from '../../shared/utils/time';
import type { Attempt, Evaluation, MonitorMessage } from '../../shared/types';
import type { ReportFormat } from '../../shared/api';

/** Форматы отчёта по занятию (ТЗ: выгрузка в стандартные форматы). */
const REPORT_FORMATS: Array<[ReportFormat, string]> = [['csv', 'CSV'], ['xlsx', 'Excel'], ['pdf', 'PDF']];

/** Опрос, когда канал реального времени недоступен (FE-08: как было до WebSocket). */
const POLL_MS = 3000;

const PARTICIPANT_LABEL: Record<string, { label: string; tone: 'neutral' | 'accent' | 'ok' | 'warn' | 'danger' }> = {
  assigned: { label: 'Назначен', tone: 'neutral' },
  joined: { label: 'Подключился', tone: 'accent' },
  active: { label: 'Работает', tone: 'warn' },
  disconnected: { label: 'Нет связи', tone: 'danger' },
  finished: { label: 'Завершил', tone: 'ok' },
};

/** У этих попыток уже есть (или вот-вот появится) оценка. */
const EVALUATED_STATUSES = new Set<Attempt['status']>(['submitted', 'evaluating', 'evaluated']);

/**
 * Занятие, попытки и их оценки по REST. Оценки — вместе с попытками: дальше
 * их изменения присылает канал (evaluationUpdated) или следующий шаг опроса.
 */
async function fetchMonitor(lessonId: string, dispatch: (action: MonitorAction) => void): Promise<void> {
  const [lesson, attempts] = await Promise.all([api.lessons.get(lessonId), api.attempts.forLesson(lessonId)]);
  dispatch({ type: 'load', lesson, attempts });
  await Promise.all(
    attempts
      .filter((a) => a.status === 'evaluating' || a.status === 'evaluated')
      .map(async (a) => {
        const evaluation = await api.evaluation.get(a.id).catch(() => null);
        if (evaluation) dispatch({ type: 'evaluation', attemptId: a.id, evaluation });
      }),
  );
}

export function LessonDetailPage() {
  const { lessonId = '' } = useParams();
  const [state, dispatch] = useReducer(monitorReducer, initialMonitor);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

  /** Занятие и попытки по REST: первая загрузка, опрос без канала, после действий преподавателя. */
  const load = useCallback(
    () =>
      fetchMonitor(lessonId, dispatch).then(
        () => setLoadError(null),
        (e: unknown) => setLoadError(e instanceof Error ? e.message : 'Не удалось загрузить занятие'),
      ),
    [lessonId],
  );

  useEffect(() => {
    let cancelled = false;
    fetchMonitor(lessonId, (action) => !cancelled && dispatch(action)).catch((e: unknown) => {
      if (!cancelled) setLoadError(e instanceof Error ? e.message : 'Не удалось загрузить занятие');
    });
    return () => { cancelled = true; };
  }, [lessonId]);

  const isRunning = state.lesson?.status === 'running';
  const channel = useRealtimeChannel<MonitorMessage>({
    enabled: isRunning,
    channelKey: lessonId,
    connect: (since, handlers) => api.realtime.lessonMonitor(lessonId, since, handlers),
    onMessage: (message) => dispatch({ type: 'message', message }),
  });

  // Канал недоступен повторно — экран обновляется опросом, пока канал не вернётся.
  useEffect(() => {
    if (!isRunning || channel.status !== 'polling') return;
    const id = setInterval(() => void load(), POLL_MS);
    return () => clearInterval(id);
  }, [isRunning, channel.status, load]);

  if (!state.lesson) {
    return loadError ? <ErrorState text={loadError} onRetry={() => void load()} /> : <Loading />;
  }

  const l = state.lesson;

  /**
   * Запуск и завершение занятия — операции сервера. Отказ показываем рядом
   * с кнопкой: иначе кнопка осталась бы заблокированной без объяснения.
   */
  async function run(action: () => Promise<unknown>, failure: string) {
    setBusy(true);
    setActionError(null);
    try {
      await action();
      await load();
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
          {(l.status === 'running' || l.status === 'finished') && (
            <Link className="btn" to={`/teacher/analytics?lessonId=${encodeURIComponent(lessonId)}`}>Аналитика занятия</Link>
          )}
        </div>
      </div>

      {actionError && <p className="field__error" role="alert" style={{ marginBottom: 12 }}>{actionError}</p>}

      {/* Отчёт формирует сервер (v1.2); у mock-реализации выгрузки нет — строка не показывается. */}
      {api.lessons.reportUrl && (l.status === 'running' || l.status === 'finished') && (
        <div className="row row--tight report-bar" aria-label="Выгрузка отчёта по занятию">
          <span className="dim small">Отчёт по занятию:</span>
          {REPORT_FORMATS.map(([format, label]) => (
            <a key={format} className="btn btn--sm" href={api.lessons.reportUrl?.(lessonId, format)} download>
              {label}
            </a>
          ))}
        </div>
      )}

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
            <div className="row row--tight">
              {state.aiHealth && <AiHealthBadge health={state.aiHealth} />}
              <ChannelBadge status={channel.status} retryInSec={channel.retryInSec} />
            </div>
          ) : undefined
        }
      >
        {l.participants.length === 0 ? (
          <p className="muted small">Участники не назначены.</p>
        ) : (
          <div className="table-scroll">
            <table className="table">
              <thead>
                <tr>
                  <th>Обучающийся</th><th>Состояние</th><th>Сейчас</th><th>Карточка</th>
                  <th>Время</th>{l.settings.voice.enabled && <th>Разговор</th>}<th>Результат</th><th />
                </tr>
              </thead>
              <tbody>
                {l.participants.map((p) => {
                  const attempt = state.attempts.find((a) => a.userId === p.userId);
                  const live = attempt ? state.live[attempt.id] : undefined;
                  const st = PARTICIPANT_LABEL[p.status] ?? { label: 'Не определено', tone: 'neutral' as const };
                  return (
                    <tr key={p.userId}>
                      <td className="nowrap">
                        <Link to={`/teacher/students/${encodeURIComponent(p.userId)}`} title="Прогресс обучающегося">{p.name}</Link>
                      </td>
                      <td><Badge tone={st.tone}>{st.label}</Badge></td>
                      <td className="muted small">{currentAction(live?.lastEvent) ?? '—'}</td>
                      <td className="mono small">{attempt?.incidentNo ?? '—'}</td>
                      <td className="mono small nowrap"><AttemptTime attempt={attempt} running={isRunning} /></td>
                      {l.settings.voice.enabled && (
                        <td className="small monitor-turn">
                          {live?.lastTurn ? (
                            <span title={live.lastTurn.text}>
                              <span className="dim">{live.turns} · {live.lastTurn.speaker === 'caller' ? 'Заявитель' : 'Оператор'}: </span>
                              {live.lastTurn.text}
                            </span>
                          ) : (
                            <span className="dim">{attempt?.dialogue?.turnsCount ?? 0} ходов</span>
                          )}
                        </td>
                      )}
                      <td><AttemptScore attempt={attempt} evaluation={attempt ? state.evaluations[attempt.id] : undefined} /></td>
                      <td>
                        {attempt && EVALUATED_STATUSES.has(attempt.status) && (
                          <Link className="btn btn--sm" to={`/teacher/attempts/${attempt.id}`}>Разбор</Link>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
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

/** Время в попытке: у идущей — растёт каждую секунду от принятия вызова. */
function AttemptTime({ attempt, running }: { attempt?: Attempt; running: boolean }) {
  const ticking = running && attempt?.status === 'in_progress' && Boolean(attempt.callAcceptedAt);
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!ticking) return;
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [ticking]);

  if (!attempt) return <>—</>;
  if (attempt.timeSpentMs != null) return <>{formatDuration(attempt.timeSpentMs)}</>;
  if (ticking && attempt.callAcceptedAt) {
    const spent = now - Date.parse(attempt.callAcceptedAt);
    const over = spent > attempt.timeLimitSec * 1000;
    return <span style={over ? { color: 'var(--u-danger)' } : undefined}>{formatDuration(spent)}</span>;
  }
  return <>—</>;
}

function AiHealthBadge({ health }: { health: NonNullable<MonitorMessage['aiHealth']> }) {
  if (health.ok === false) return <Badge tone="danger">ИИ: недоступен</Badge>;
  if (health.dialogWaiting) return <Badge tone="warn" value>ИИ: очередь диалогов {health.dialogWaiting}</Badge>;
  return <Badge tone="ok">ИИ: в норме</Badge>;
}

function AttemptScore({ attempt, evaluation }: { attempt?: Attempt; evaluation?: Evaluation }) {
  if (!attempt) return <span className="dim">—</span>;
  if (!evaluation) return <span className="dim small">не оценено</span>;
  if (evaluation.status !== 'done') return <Badge tone="warn">Анализируется…</Badge>;

  return (
    <Badge tone={evaluation.verdict === 'pass' ? 'ok' : 'danger'} value>
      {evaluation.finalScore} · {evaluation.verdict === 'pass' ? 'зачёт' : 'незачёт'}
    </Badge>
  );
}
