import { useCallback, useEffect, useState } from 'react';
import { Navigate, useNavigate, useParams } from 'react-router-dom';
import { api } from '../../shared/api';
import { useAuth } from '../../app/auth';
import { IncidentCard } from '../../features/incident-card/IncidentCard';
import { DdsWorkspace } from '../../features/dds/DdsWorkspace';
import { ddsDecision, ownService } from '../../features/dds/ddsDecision';
import { useAttemptTimer, useAutosave, useEventLog } from '../../features/attempt-runtime/useAttemptRuntime';
import { isRetriable, outboxFor } from '../../features/attempt-runtime/outbox';
import { ConnectionBanner } from '../../components/ConnectionBanner';
import { useConnectivity } from '../../shared/api/connectivity';
import { useRealtimeChannel } from '../../shared/realtime/useRealtimeChannel';
import { emptyCard } from '../../shared/utils/card';
import type { Attempt, IncidentCardDraft, StudentCallScript, StudentMessage, User } from '../../shared/types';

/** Сдача не прошла из-за связи: карточка остаётся на экране, данные — в очереди. */
const OFFLINE_SUBMIT = 'Нет связи с сервером — карточка не отправлена. Всё введённое сохранено на этом устройстве; нажмите «Сохранить» ещё раз, когда связь восстановится.';

export function Arm112Page() {
  const { attemptId = '' } = useParams();
  const user = useAuth((s) => s.user);

  if (!user) return <Navigate to="/login" replace />;

  // key по попытке: смена карточки — это новый сеанс работы, а не обновление
  // состояния существующего. Перемонтирование сбрасывает таймер и черновик.
  return <Arm112Workspace key={attemptId} attemptId={attemptId} user={user} />;
}

type LoadState =
  | { phase: 'loading' }
  | { phase: 'error'; message: string }
  | { phase: 'ready'; attempt: Attempt; callScript: StudentCallScript };

function Arm112Workspace({ attemptId, user }: { attemptId: string; user: User }) {
  const navigate = useNavigate();
  const [state, setState] = useState<LoadState>({ phase: 'loading' });
  const [card, setCard] = useState<IncidentCardDraft>(() => emptyCard());
  const [submitting, setSubmitting] = useState(false);
  const [submitNotice, setSubmitNotice] = useState<string | null>(null);

  const attempt = state.phase === 'ready' ? state.attempt : null;

  const { elapsedMs, exceeded } = useAttemptTimer(attempt);
  const { saveState, savedAt, saveNow } = useAutosave(attemptId, card, attempt?.status === 'in_progress');
  const { log, logFieldChange, flush } = useEventLog(attemptId);

  useEffect(() => {
    let cancelled = false;

    Promise.all([
      api.attempts.get(attemptId),
      api.attempts.getDraft(attemptId),
      api.callScript.get(attemptId),
    ])
      .then(([a, draft, callScript]) => {
        if (cancelled) return;
        // Черновик, не дошедший до сервера в прошлый раз (обрыв, F5), новее серверного.
        const box = outboxFor(attemptId);
        setCard(box.pendingDraft() ?? draft);
        void box.flush();
        setState({ phase: 'ready', attempt: a, callScript });
      })
      .catch((e: unknown) => {
        if (cancelled) return;
        setState({ phase: 'error', message: e instanceof Error ? e.message : 'Не удалось открыть карточку' });
      });

    return () => { cancelled = true; };
  }, [attemptId]);

  const dds = attempt?.perspective === 'dds';

  /*
   * Канал попытки: преподаватель завершил занятие — карточка закрыта сервером
   * (незавершённые попытки → expired), работать дальше нельзя. Отложенное
   * досылаем и уходим на экран результата.
   */
  useRealtimeChannel<StudentMessage>({
    enabled: attempt?.status === 'in_progress',
    channelKey: attemptId,
    connect: (_since, handlers) => api.realtime.attempt(attemptId, handlers),
    onMessage: (message) => {
      if (message.type !== 'lessonFinished') return;
      void flush().finally(() => navigate(`/student/attempts/${attemptId}/result`, { replace: true }));
    },
  });

  /** Перечитывает черновик после операций, меняющих состав или статусы служб. */
  const reloadDraft = useCallback(() => {
    void api.attempts
      .getDraft(attemptId)
      // ДДС: текст действия мог быть набран, но ещё не дойти до сервера —
      // статус службы не должен затирать его серверной копией.
      .then((draft) => setCard((prev) => (dds ? { ...draft, actionsTaken: prev.actionsTaken } : draft)))
      // Перечитывание черновика — вспомогательное действие: на экране остаётся
      // актуальная карточка, отдельного экрана ошибки здесь не нужно.
      .catch(() => {});
  }, [attemptId, dds]);

  /**
   * ДДС (v1.3): карточку сервер берёт из своего черновика, из тела — только
   * текст действия. Проверок полей карточки 112 здесь нет: её заполнял оператор.
   */
  async function submitDds() {
    if (!attempt) return;
    const own = ownService(card, attempt);
    const hints: string[] = [];
    if (!ddsDecision(own, attempt.callAcceptedAt)) hints.push('• Не проставлено решение «Принята» / «Не принята»');
    if (!card.actionsTaken.trim()) hints.push('• Не заполнен текст действия');
    const question =
      hints.length > 0
        ? `${hints.join('\n')}\n\nЗавершить работу с карточкой в таком виде?`
        : 'Завершить работу с карточкой?';
    if (!window.confirm(question)) return;

    await deliverAndSubmit(() => api.attempts.submit(attemptId, card, card.actionsTaken), 'Не удалось завершить работу с карточкой');
  }

  /**
   * Сдача: сначала отложенные события, затем черновик — хронология не должна
   * потерять последнее действие. Без связи сдача откладывается, а не ломает
   * экран: карточка остаётся, данные ждут в очереди на устройстве.
   */
  async function deliverAndSubmit(send: () => Promise<unknown>, failure: string) {
    setSubmitting(true);
    setSubmitNotice(null);
    const delivered = (await flush()) && (await saveNow());
    if (!delivered && useConnectivity.getState().offline) {
      setSubmitNotice(OFFLINE_SUBMIT);
      setSubmitting(false);
      return;
    }
    try {
      await send();
      outboxFor(attemptId).clear();
      navigate(`/student/attempts/${attemptId}/result`, { replace: true });
    } catch (e) {
      if (isRetriable(e)) {
        setSubmitNotice(OFFLINE_SUBMIT);
        setSubmitting(false);
        return;
      }
      setState({ phase: 'error', message: e instanceof Error ? e.message : failure });
      setSubmitting(false);
    }
  }

  /**
   * «✕» — закрыть карточку без сдачи: отложенные события и черновик уходят на сервер,
   * попытка остаётся «в работе» (отсчёт идёт от callAcceptedAt на сервере). Диспетчер
   * ДДС возвращается к списку происшествий, оператор 112 — к своим занятиям.
   */
  async function close() {
    await flush();
    await saveNow();
    navigate(dds ? `/student/attempts/${attemptId}/call` : '/student');
  }

  async function submit() {
    if (!attempt) return;

    const missing = validate(card);
    const question =
      missing.length > 0
        ? `Не заполнены обязательные поля:\n\n${missing.join('\n')}\n\nОповестить службы и сохранить карточку в таком виде?`
        : 'Оповестить службы и сохранить карточку?';
    if (!window.confirm(question)) return;

    await deliverAndSubmit(() => api.attempts.submit(attemptId, card), 'Не удалось сохранить карточку');
  }

  if (state.phase === 'loading') {
    return (
      <div className="arm__lock">
        <div>
          <div className="spinner" style={{ margin: '0 auto 12px' }} />
          <div>Открывается рабочее место…</div>
        </div>
      </div>
    );
  }

  if (state.phase === 'error') {
    return (
      <div className="arm__lock">
        <div>
          <h2>Не удалось открыть карточку</h2>
          <p className="muted" style={{ margin: '8px 0 16px' }}>{state.message}</p>
          <button type="button" className="btn" onClick={() => navigate('/student')}>К списку занятий</button>
        </div>
      </div>
    );
  }

  // Вызов ещё не принят — работать в АРМ нельзя.
  if (state.attempt.status === 'issued') {
    return <Navigate to={`/student/attempts/${attemptId}/call`} replace />;
  }

  // Попытка завершена — показываем результат, а не редактируемую карточку.
  if (state.attempt.status !== 'in_progress') {
    return <Navigate to={`/student/attempts/${attemptId}/result`} replace />;
  }

  const banner = <ConnectionBanner notice={submitNotice} onDismiss={() => setSubmitNotice(null)} />;

  if (dds) {
    return (
      <>
        {banner}
        <DdsWorkspace
          attempt={state.attempt}
          card={card}
          elapsedMs={elapsedMs}
          saveState={saveState}
          savedAt={savedAt}
          submitting={submitting}
          onChange={setCard}
          onFieldChange={logFieldChange}
          onServicesChanged={reloadDraft}
          onSubmit={() => void submitDds()}
          onClose={() => void close()}
        />
      </>
    );
  }

  return (
    <>
      {banner}
      <IncidentCard
        attempt={state.attempt}
        user={user}
        card={card}
        callScript={state.callScript}
        elapsedMs={elapsedMs}
        exceeded={exceeded}
        saveState={saveState}
        savedAt={savedAt}
        submitting={submitting}
        onChange={setCard}
        onFieldChange={logFieldChange}
        onServicesChanged={reloadDraft}
        onEvent={log}
        onReplay={() => {
          void api.attempts.replay(attemptId).catch(() => {});
          log('replay');
        }}
        onSubmit={() => void submit()}
        onClose={() => void close()}
      />
    </>
  );
}

/**
 * Клиентская проверка — только подсказка оператору перед сохранением.
 * Обязательность по эталону и допустимость значений проверяет сервер.
 */
function validate(card: IncidentCardDraft): string[] {
  const missing: string[] = [];
  if (card.incidentTypeIds.length === 0) missing.push('• Тип происшествия');
  if (!card.address.raw.trim()) missing.push('• Адрес происшествия');
  if (!card.applicant.name?.trim()) missing.push('• ФИО заявителя');
  if (!card.applicant.status) missing.push('• Статус заявителя');
  if (!card.description.trim()) missing.push('• Описание со слов заявителя');
  return missing;
}
