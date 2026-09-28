import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { api } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { useAuth } from '../../app/auth';
import { Icon } from '../../components/Icon';
import { ownService } from './ddsDecision';
import { asCodes, formatAddress } from '../../shared/utils/card';
import { cls } from '../../shared/utils/cls';
import { formatTime } from '../../shared/utils/time';
import type { Attempt } from '../../shared/types';

/**
 * Главный экран диспетчера ДДС — «Список происшествий» (mockups/pov112-main.png,
 * pov112-main-cards.png): тёмная таблица, строка карточки и под ней «Службы / Заявитель /
 * Информация». Звонка у диспетчера нет — карточку создал и направил оператор 112.
 *
 * Открытие новой карточки = «взять в работу» (POST accept-call): с этого момента идёт
 * отсчёт 30 с на решение. До этого сервер карточку ещё не собрал (она строится при
 * взятии в работу), поэтому в строке — только номер, дата и время поступления.
 */
export function DdsIncidentList({ attempt: initial }: { attempt: Attempt }) {
  const navigate = useNavigate();
  const user = useAuth((s) => s.user);
  const [attempt, setAttempt] = useState(initial);
  const [expanded, setExpanded] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [now, setNow] = useState(() => new Date());

  const taken = Boolean(attempt.callAcceptedAt);
  const draft = useAsync(() => (taken ? api.attempts.getDraft(attempt.id) : Promise.resolve(null)), [attempt.id, taken]);
  const labels = useAsync(() => (taken ? api.classifier.labels() : Promise.resolve(null)), [taken]);

  useEffect(() => {
    const id = setInterval(() => setNow(new Date()), 1000);
    return () => clearInterval(id);
  }, []);

  async function open() {
    if (busy) return;
    if (attempt.status === 'in_progress') {
      navigate(`/student/attempts/${attempt.id}/arm`);
      return;
    }
    if (attempt.status !== 'issued') {
      navigate(`/student/attempts/${attempt.id}/result`);
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const res = await api.attempts.acceptCall(attempt.id);
      setAttempt(res.attempt);
      navigate(`/student/attempts/${attempt.id}/arm`, { replace: true });
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Не удалось взять карточку в работу');
      setBusy(false);
    }
  }

  const card = draft.data;
  const own = card ? ownService(card, attempt) : undefined;
  const typeNames = card ? card.incidentTypeIds.map((id) => labels.data?.types[id] ?? id) : [];
  const features = card
    ? Object.entries(card.attributes)
        .flatMap(([code, value]) => asCodes(value).map((v) => labels.data?.values[code]?.[v] ?? v))
        .filter(Boolean)
    : [];
  const time = formatTime(attempt.issuedAt);
  const status = attempt.status === 'issued' ? 'Новая' : own?.currentStatus ?? '—';

  return (
    <div className="dds-list">
      <div className="dds-list__top">
        <div className="dds-list__title">Список происшествий</div>
        <div className="dds-list__info">
          <div>
            <div className="dds-list__date">
              {now.toLocaleDateString('ru-RU', { weekday: 'long', day: 'numeric', month: 'long', year: 'numeric' })}
            </div>
            <div className="dds-list__who">
              {user?.operatorNo ?? 'оп.'}, {user?.lastName ?? ''} · {user?.workstation ?? 'АРМ'} · {attempt.actingService?.shortName ?? 'ДДС'}
            </div>
          </div>
          <div className="dds-list__clock mono">{now.toLocaleTimeString('ru-RU', { hour12: false })}</div>
          <button type="button" className="dds-list__exit" onClick={() => navigate('/student')}>Мои занятия</button>
        </div>
      </div>

      <div className="dds-grid" role="table" aria-label="Список происшествий">
        <div className="dds-grid__head" role="row">
          <span />
          <span>Связи</span>
          <span />
          <span>ЧС</span>
          <span />
          <span>Опер.</span>
          <span>АРМ</span>
          <span>Номер</span>
          <span>Дата</span>
          <span>Время</span>
          <span>Тип происшествия</span>
          <span>Постр.</span>
          <span>Статус службы</span>
          <span>Адрес</span>
          <span />
          <span>Проверена</span>
        </div>

        <div className={cls('dds-grid__item', attempt.status === 'issued' && 'is-new')}>
          <div
            className="dds-grid__row"
            role="row"
            tabIndex={0}
            aria-busy={busy}
            title={attempt.status === 'issued' ? 'Открыть карточку — взять в работу' : 'Открыть карточку'}
            onClick={() => void open()}
            onKeyDown={(e) => {
              if (e.key === 'Enter') void open();
            }}
          >
            <button
              type="button"
              className="dds-grid__cell dds-grid__expand"
              aria-label={expanded ? 'Свернуть' : 'Развернуть'}
              onClick={(e) => {
                e.stopPropagation();
                setExpanded((v) => !v);
              }}
            >
              {expanded ? '⌃' : '⌄'}
            </button>
            <span className="dds-grid__cell" />
            <span className="dds-grid__cell dds-grid__icon"><Icon name="bookmark" /></span>
            <span className="dds-grid__cell dds-grid__icon"><Icon name="bolt" /></span>
            <span className="dds-grid__cell dds-grid__icon"><Icon name="alarm" /></span>
            <span className="dds-grid__cell" />
            <span className="dds-grid__cell" />
            <span className="dds-grid__cell mono">{attempt.incidentNo}</span>
            <span className="dds-grid__cell mono">{new Date(attempt.issuedAt).toLocaleDateString('ru-RU', { day: '2-digit', month: '2-digit', year: '2-digit' })}</span>
            <span className="dds-grid__cell dds-grid__time mono">
              {time.slice(0, 5)}<sup>{time.slice(6, 8)}</sup>
            </span>
            <span className="dds-grid__cell dds-grid__type">
              {card ? typeNames.join(', ') || '—' : busy ? 'Открывается…' : 'Карточка от оператора Службы 112'}
            </span>
            <span className="dds-grid__cell">{card ? (card.flags.victimsPresent ? 'Да' : 'Нет') : ''}</span>
            <span className={cls('dds-grid__cell', attempt.status === 'issued' && 'dds-grid__status--new')}>{status}</span>
            <span className="dds-grid__cell dds-grid__addr">{card ? card.address.raw || formatAddress(card.address) : ''}</span>
            <span className="dds-grid__cell dds-grid__icon"><Icon name="clipboard" /></span>
            <span className="dds-grid__cell dds-grid__icon"><Icon name="check" size={16} /></span>
          </div>

          {expanded && (
            <div className="dds-grid__lines">
              {!card ? (
                <div className="dds-grid__line">
                  <span className="dds-grid__label">Описание:</span>
                  <span>
                    Поступила от оператора Службы 112 для {attempt.actingService?.name ?? 'вашей службы'}. Откройте
                    карточку, чтобы взять её в работу: решение «Принята» / «Не принята» — в течение 30 с.
                  </span>
                </div>
              ) : (
                <>
                  <div className="dds-grid__line">
                    <span className="dds-grid__label">Службы:</span>
                    <span>
                      {card.services.map((s, i) => (
                        <span key={s.serviceId}>
                          {i > 0 && ', '}
                          <b>{s.shortName}</b> — {formatTime(s.currentStatusAt)} {s.currentStatus}
                        </span>
                      ))}
                    </span>
                  </div>
                  <div className="dds-grid__line">
                    <span className="dds-grid__label">Заявитель:</span>
                    <span>
                      <b>{card.applicant.name || '—'}</b>
                      {card.phones.aon ? <> АОН <span className="mono">{card.phones.aon}</span></> : null}
                    </span>
                  </div>
                  {features.length > 0 && (
                    <div className="dds-grid__line">
                      <span className="dds-grid__label">Информация:</span>
                      <span><b>{features.join('. ')}.</b></span>
                    </div>
                  )}
                </>
              )}
            </div>
          )}
        </div>

        {error && <div className="dds-grid__error" role="alert">{error}</div>}
      </div>
    </div>
  );
}
