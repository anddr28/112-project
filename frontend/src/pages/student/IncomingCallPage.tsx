import { useEffect, useState } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { api } from '../../shared/api';
import { Icon } from '../../components/Icon';
import { DdsIncidentList } from '../../features/dds/DdsIncidentList';
import type { Attempt, StudentCallScript } from '../../shared/types';

/**
 * Экран входящего вызова.
 *
 * Инструкция п.3: при поступлении вызова открывается окно с информацией
 * о вызове и единственной кнопкой «Принять»; после принятия система
 * автоматически открывает новую карточку — и с этого момента идёт таймер.
 *
 * Данные берутся из callScript, а не из сценария целиком: Scenario несёт
 * эталонную карточку, keyFacts и требуемые поля, то есть правильные ответы.
 * Обучающемуся они не передаются даже в сетевом ответе.
 *
 * Ракурс «Диспетчер ДДС» (v1.3): звонка нет — вместо окна вызова показывается
 * «Список происшествий» (features/dds/DdsIncidentList): карточку создал оператор 112.
 */
export function IncomingCallPage() {
  const { attemptId = '' } = useParams();
  const navigate = useNavigate();
  const [attempt, setAttempt] = useState<Attempt | null>(null);
  const [script, setScript] = useState<StudentCallScript | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    api.attempts
      .get(attemptId)
      .then(async (a) => {
        if (cancelled) return;
        setAttempt(a);
        // ДДС: список происшествий показывается и после взятия в работу (к нему
        // возвращает «✕» карточки); реплик заявителя у диспетчера нет.
        if (a.perspective === 'dds') return;
        if (a.callAcceptedAt) {
          navigate(`/student/attempts/${attemptId}/arm`, { replace: true });
          return;
        }
        const s = await api.callScript.get(attemptId);
        if (!cancelled) setScript(s);
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(e instanceof Error ? e.message : 'Не удалось загрузить вызов');
      });
    return () => { cancelled = true; };
  }, [attemptId, navigate]);

  async function accept() {
    setBusy(true);
    try {
      await api.attempts.acceptCall(attemptId);
      navigate(`/student/attempts/${attemptId}/arm`, { replace: true });
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Не удалось принять вызов');
      setBusy(false);
    }
  }

  if (error) {
    return (
      <div className="call-screen">
        <div className="call-card">
          <h2>Ошибка</h2>
          <p className="muted" style={{ margin: '8px 0 16px' }}>{error}</p>
          <button type="button" className="btn" onClick={() => navigate('/student')}>К занятиям</button>
        </div>
      </div>
    );
  }

  // Диспетчер ДДС звонков не принимает: его экран — «Список происшествий».
  if (attempt?.perspective === 'dds') return <DdsIncidentList attempt={attempt} />;

  if (!attempt || !script) {
    return (
      <div className="call-screen">
        <div className="call-card"><div className="spinner" style={{ margin: '0 auto' }} /></div>
      </div>
    );
  }

  return (
    <div className="call-screen">
      <div className="call-card">
        <div className="call-card__pulse"><Icon name="phone" size={22} /></div>
        <div className="call-card__title">Входящий вызов 112</div>
        <div className="call-card__phone mono">{script.caller.phone ?? 'номер не определён'}</div>
        <div className="call-card__meta">
          Канал связи: мобильный · Карточка № {attempt.incidentNo}
        </div>

        <div className="call-card__meta" style={{ marginTop: 14, lineHeight: 1.5 }}>
          Норматив заполнения карточки — <b>{attempt.timeLimitSec} с</b>.<br />
          Отсчёт начнётся после принятия вызова.
        </div>

        <button type="button" className="call-card__accept" disabled={busy} onClick={() => void accept()}>
          {busy ? 'Соединение…' : 'Принять'}
        </button>

        <button
          type="button"
          className="arm-mini"
          style={{ marginTop: 10 }}
          onClick={() => navigate('/student')}
        >
          Вернуться к занятиям
        </button>
      </div>
    </div>
  );
}
