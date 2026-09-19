import { useEffect, useState } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { api } from '../../shared/api';
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
        <div className="call-card__pulse">📞</div>
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
