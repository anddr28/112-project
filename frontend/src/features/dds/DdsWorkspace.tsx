import type { Dispatch, SetStateAction } from 'react';
import { api } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { EtalonCardView } from '../incident-card/EtalonCardView';
import { ServicesBar } from '../incident-card/ServicesBar';
import { cls } from '../../shared/utils/cls';
import { formatAddress, asCodes } from '../../shared/utils/card';
import { formatClock, formatDateTime, formatTime } from '../../shared/utils/time';
import type { SaveState } from '../attempt-runtime/useAttemptRuntime';
import { DDS_DECISION_SEC, ddsDecision, ownService } from './ddsDecision';
import type { Attempt, IncidentCardDraft, User } from '../../shared/types';

/**
 * Рабочее место диспетчера ДДС (контракт v1.3, perspective = dds).
 *
 * ТЗ «действия с карточками» и Памятка АРМ-112 для ДДС: карточку создал
 * оператор 112 и направил службам списка оповещения. Диспетчер своей службы
 * в течение 30 с ставит «Принята» / «Не принята» (с причиной в комментарии),
 * затем статусы реагирования с комментариями и пишет текст действия.
 *
 * Карточку 112 диспетчер не правит: сервер из черновика берёт только
 * `actionsTaken`. Состав служб тоже задан оператором 112.
 */

export function DdsWorkspace({
  attempt,
  user,
  card,
  elapsedMs,
  saveState,
  savedAt,
  submitting,
  onChange,
  onFieldChange,
  onServicesChanged,
  onSubmit,
}: {
  attempt: Attempt;
  user: User;
  card: IncidentCardDraft;
  elapsedMs: number;
  saveState: SaveState;
  savedAt: string | null;
  submitting: boolean;
  onChange: Dispatch<SetStateAction<IncidentCardDraft>>;
  onFieldChange: (field: string, value: unknown) => void;
  onServicesChanged: () => void;
  onSubmit: () => void;
}) {
  const labels = useAsync(() => api.classifier.labels(), []);
  const own = ownService(card, attempt);
  const decision = ddsDecision(own, attempt.callAcceptedAt);
  const decisionLimitMs = DDS_DECISION_SEC * 1000;
  // Пока решения нет — идёт отсчёт; после решения фиксируем, за сколько оно принято.
  const decisionMs = decision?.ms ?? elapsedMs;
  const decisionLate = decisionMs > decisionLimitMs;
  const exceeded = elapsedMs > attempt.timeLimitSec * 1000;

  const typeNames = card.incidentTypeIds.map((id) => labels.data?.types[id] ?? id);
  const info = Object.entries(card.attributes)
    .flatMap(([code, value]) => asCodes(value).map((v) => labels.data?.values[code]?.[v] ?? v))
    .filter(Boolean);

  return (
    <div className="arm arm--dds">
      {/* ─────────────────────────────────── верхняя панель рабочего места ДДС */}
      <div className="arm-top">
        <div className="arm-top__cell dds-role">
          <div className="dds-role__title">Диспетчер ДДС</div>
          <div className="dds-role__service">{attempt.actingService?.name ?? 'служба не определена'}</div>
          <div className="arm-ident__meta">Карточка поступила от оператора Службы 112</div>
        </div>

        <div className="arm-top__cell arm-top__ident">
          <div className="arm-ident">
            <div className="arm-ident__no">Происшествие {attempt.incidentNo}</div>
            <div className="arm-ident__meta">
              {savedAt ? `Сохр. ${new Date(savedAt).toLocaleString('ru-RU')}` : 'Черновик'}
            </div>
            <div className="arm-ident__meta">
              {user.operatorNo ?? 'оп.'}, {user.workstation ?? 'АРМ'}, ДДС
            </div>
          </div>

          <div className={cls('arm-timer', decisionLate && 'arm-timer--exceeded')} title="Время до решения «Принята» / «Не принята»">
            <div className="arm-timer__value">{formatClock(decisionMs)}</div>
            <div>
              <div className="arm-timer__units"><span>решение</span></div>
              <div className="arm-timer__norm">
                {decision ? `${decision.status}` : `норматив ${DDS_DECISION_SEC} с`}
              </div>
            </div>
          </div>

          <div className={cls('arm-timer', exceeded && 'arm-timer--exceeded')} title="Время обработки карточки">
            <div className="arm-timer__value">{formatClock(elapsedMs)}</div>
            <div>
              <div className="arm-timer__units"><span>минут</span><span>секунд</span></div>
              <div className="arm-timer__norm">норматив {attempt.timeLimitSec} с</div>
            </div>
          </div>
        </div>
      </div>

      {/* ───────────────────── строка карточки в «Списке происшествий» ПОВ-112 */}
      <div className="dds-row" role="region" aria-label="Поступившая карточка">
        <div className="dds-row__head">
          <span className="dds-row__cell dds-row__cell--num mono">{attempt.incidentNo}</span>
          <span className="dds-row__cell mono">{formatDateTime(attempt.issuedAt).split(' ')[0]}</span>
          <span className="dds-row__cell dds-row__time mono">{formatTime(attempt.issuedAt).slice(0, 5)}</span>
          <span className="dds-row__cell dds-row__type">{typeNames.join(', ') || 'Тип не указан'}</span>
          <span className="dds-row__cell">Пострадавшие: {card.flags.victimsPresent ? `есть${card.flags.victimsCount ? ` (${card.flags.victimsCount})` : ''}` : 'нет'}</span>
          <span className={cls('dds-row__cell', !decision && 'dds-row__cell--warn')}>
            {own ? own.currentStatus : 'Зарегистрирована'}
          </span>
          <span className="dds-row__cell dds-row__addr">{card.address.raw || formatAddress(card.address) || 'Адрес не указан'}</span>
        </div>
        <div className="dds-row__line">
          <span className="dds-row__label">Службы:</span>
          <span>
            {card.services.map((s, i) => (
              <span key={s.serviceId} className={cls(s.serviceId === own?.serviceId && 'dds-row__own')}>
                {i > 0 && ', '}
                <b>{s.shortName}</b> — {formatTime(s.currentStatusAt).slice(0, 8)} {s.currentStatus}
              </span>
            ))}
          </span>
        </div>
        <div className="dds-row__line">
          <span className="dds-row__label">Заявитель:</span>
          <span>
            {card.applicant.name || '—'}
            {card.phones.aon ? <> · АОН <span className="mono">{card.phones.aon}</span></> : null}
            {card.applicant.status ? ` · ${card.applicant.status}` : ''}
          </span>
        </div>
        {info.length > 0 && (
          <div className="dds-row__line">
            <span className="dds-row__label">Информация:</span>
            <span>{info.join('. ')}.</span>
          </div>
        )}
      </div>

      {/* ──────────────────────────────── карточка 112 и действия диспетчера */}
      <div className="arm-body">
        <div className="arm-col">
          <div className="arm-panel dds-card">
            <div className="arm-desc__label">Карточка оператора 112 — только чтение</div>
            <div>
              <EtalonCardView card={card} />
            </div>
          </div>
        </div>

        <div className="arm-col">
          <div className="arm-panel">
            <div className="arm-desc__label">Порядок работы с карточкой</div>
            <ol className="dds-steps">
              <li className={cls(decision && 'is-done')}>
                Откройте свою службу в списке оповещения и поставьте «Принята» или «Не принята»
                в течение {DDS_DECISION_SEC} с. Для «Не принята» комментарий обязателен.
              </li>
              <li>Проставьте статусы реагирования по ходу работ, указывая комментарии.</li>
              <li className={cls(card.actionsTaken.trim() && 'is-done')}>Запишите текст действия и завершите работу с карточкой.</li>
            </ol>
            {decision && (
              <div className={cls('arm-note', 'dds-decision', decisionLate && 'dds-decision--late')}>
                Решение «{decision.status}»
                {decision.ms != null ? ` принято за ${Math.floor(decision.ms / 1000)} с` : ''}
                {decisionLate ? ` — норматив ${DDS_DECISION_SEC} с превышен` : ''}
              </div>
            )}
          </div>

          <div className="arm-panel arm-desc">
            <label className="arm-desc__label" htmlFor="dds-action">Текст действия</label>
            <textarea
              id="dds-action"
              maxLength={1999}
              value={card.actionsTaken}
              placeholder="Например: Сообщение принято, аварийная бригада направлена на место."
              onChange={(e) => {
                const value = e.target.value;
                onChange((prev) => ({ ...prev, actionsTaken: value }));
                onFieldChange('actionsTaken', value);
              }}
            />
            <div className="arm-desc__counter">{card.actionsTaken.length}/1999</div>
          </div>
        </div>
      </div>

      {/* ────────────────────── нижняя панель: список оповещения и завершение */}
      <div>
        <ServicesBar
          attemptId={attempt.id}
          services={card.services}
          disabled={submitting}
          onChanged={onServicesChanged}
          ownServiceId={own?.serviceId ?? ''}
        >
          <div className="arm-bottom">
            <div className="arm-bottom__label">
              <span className="arm-autosave">
                <span className={cls('arm-autosave__dot', `arm-autosave__dot--${saveState}`)} />
                {saveState === 'saving' ? 'Сохранение…' : saveState === 'saved' ? 'Текст действия сохранён' : saveState === 'error' ? 'Ошибка сохранения' : 'Черновик'}
              </span>
            </div>
            <div />
            <div className="arm-bottom__actions">
              <button type="button" className="arm-save" disabled={submitting} onClick={onSubmit}>
                {submitting ? 'Завершение…' : 'завершить'}
              </button>
            </div>
          </div>
        </ServicesBar>
      </div>
    </div>
  );
}
