import { useState } from 'react';
import type { Dispatch, SetStateAction } from 'react';
import { api } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { Icon } from '../../components/Icon';
import { DdsServicesBar } from './DdsServicesBar';
import { DDS_DECISION_SEC, ddsDecision, ownService } from './ddsDecision';
import { cls } from '../../shared/utils/cls';
import { formatAddress, asCodes } from '../../shared/utils/card';
import { formatClock, formatDateTime } from '../../shared/utils/time';
import type { SaveState } from '../attempt-runtime/useAttemptRuntime';
import type { Attempt, IncidentCardDraft } from '../../shared/types';

const ACTION_LIMIT = 1999;

/**
 * Карточка происшествия на рабочем месте диспетчера ДДС (контракт v1.3, perspective = dds).
 *
 * Разметка — mockups/pov112-incident-card-department.png: та же верхняя полоса АРМ
 * (телефоны заявителя, «Происшествие № / Сохр.», кнопки «просмотр» / «дополнение»),
 * слева заявитель, адрес и лента сообщений, справа сводка пострадавших и тёмная шапка
 * типа происшествия с признаками, внизу — тёмный список оповещения.
 *
 * Карточку создал оператор 112, диспетчер её не правит (сервер из черновика берёт только
 * `actionsTaken`). Своё — статусы своей службы (карандаш на плашке) и дополнение:
 * «дополнение» открывает ввод текста действия, «просмотр» — ленту сообщений.
 */
export function DdsWorkspace({
  attempt,
  card,
  elapsedMs,
  saveState,
  savedAt,
  submitting,
  onChange,
  onFieldChange,
  onServicesChanged,
  onSubmit,
  onClose,
}: {
  attempt: Attempt;
  card: IncidentCardDraft;
  elapsedMs: number;
  saveState: SaveState;
  savedAt: string | null;
  submitting: boolean;
  onChange: Dispatch<SetStateAction<IncidentCardDraft>>;
  onFieldChange: (field: string, value: unknown) => void;
  onServicesChanged: () => void;
  onSubmit: () => void;
  /** ✕ — закрыть карточку (к списку происшествий) без сдачи попытки */
  onClose: () => void;
}) {
  const [tab, setTab] = useState<'view' | 'supplement'>('view');
  const labels = useAsync(() => api.classifier.labels(), []);
  const own = ownService(card, attempt);
  const decision = ddsDecision(own, attempt.callAcceptedAt);
  // Пока решения нет — идёт отсчёт; после решения фиксируем, за сколько оно принято.
  const decisionMs = decision?.ms ?? elapsedMs;
  const decisionLate = decisionMs > DDS_DECISION_SEC * 1000;
  const exceeded = elapsedMs > attempt.timeLimitSec * 1000;

  const typeNames = card.incidentTypeIds.map((id) => labels.data?.types[id] ?? id);
  const features = Object.entries(card.attributes)
    .flatMap(([code, value]) => asCodes(value).map((v) => labels.data?.values[code]?.[v] ?? v))
    .filter(Boolean);
  const address = card.address.raw || formatAddress(card.address);
  const yesNo = (v: boolean | undefined) => (v ? 'есть' : 'нет');

  return (
    <div className="arm arm--dds">
      {/* ─────────────────────────── верхняя полоса: телефоны, карточка, таймеры */}
      <div className="arm-top">
        <div className="arm-top__cell arm-top__disconnect">
          <div className="arm-top__hangup">
            <span className="arm-top__hangup-icon"><Icon name="phone-hangup" size={15} /></span>
            <span>не подключен</span>
          </div>
        </div>

        <PhoneView label="АОН" value={card.phones.aon} />
        <PhoneView label="предоставленный" value={card.phones.provided} />
        <PhoneView label="телефон на место" value={card.phones.onSite} />

        <div className="arm-top__cell arm-top__ident">
          <div className="arm-ident">
            <div className="arm-ident__no">Происшествие {attempt.incidentNo}</div>
            <div className="arm-ident__meta">Сохр. {formatDateTime(attempt.issuedAt).replace(' ', ' в ')}</div>
            <div className="arm-ident__meta">Опер. Служба 112</div>
          </div>

          <div className={cls('arm-timer', decisionLate && 'arm-timer--exceeded')} title="Время до решения «Принята» / «Не принята»">
            <div className="arm-timer__value">{formatClock(decisionMs)}</div>
            <div>
              <div className="arm-timer__units"><span>решение</span></div>
              <div className="arm-timer__norm">{decision ? decision.status : `норматив ${DDS_DECISION_SEC} с`}</div>
            </div>
          </div>

          <div className={cls('arm-timer', exceeded && 'arm-timer--exceeded')} title="Время работы с карточкой">
            <div className="arm-timer__value">{formatClock(elapsedMs)}</div>
            <div>
              <div className="arm-timer__units"><span>минут</span><span>секунд</span></div>
              <div className="arm-timer__norm">норматив {attempt.timeLimitSec} с</div>
            </div>
          </div>
        </div>

        <div className="arm-top__cell dds-tabs">
          <button type="button" className={cls('dds-tab', tab === 'view' && 'is-on')} onClick={() => setTab('view')}>
            просмотр
          </button>
          <button type="button" className={cls('dds-tab', tab === 'supplement' && 'is-on')} onClick={() => setTab('supplement')}>
            дополнение
          </button>
        </div>
      </div>

      {/* ─────────────────────────────────────────────── карточка (только чтение) */}
      <div className="arm-body">
        <div className="arm-col">
          <div className="arm-panel dds-line">
            <b>{card.applicant.name || '—'}</b> <span className="dds-line__muted">{card.applicant.status ?? ''}</span>
          </div>

          <div className="arm-panel dds-line dds-line--address">
            <div>
              <b>{address || 'Адрес не указан'}</b>
              {card.address.descriptive && <div className="dds-line__sub">{card.address.descriptive}</div>}
            </div>
            <Icon name="pin" size={14} className="dds-line__icon" />
          </div>

          {tab === 'view' ? (
            <div className="arm-panel dds-feed" aria-label="Лента сообщений">
              {card.description.trim() && (
                <div className="dds-feed__row">
                  <div className="dds-feed__meta">{formatDateTime(attempt.issuedAt)} · Служба 112</div>
                  <div>{card.description}</div>
                </div>
              )}
              {card.actionsTaken.trim() && (
                <div className="dds-feed__row">
                  <div className="dds-feed__meta">
                    {savedAt ? formatDateTime(savedAt) : 'черновик'} · {attempt.actingService?.shortName ?? 'ДДС'} · дополнение
                  </div>
                  <div className="dds-feed__text">{card.actionsTaken}</div>
                </div>
              )}
            </div>
          ) : (
            <div className="arm-panel arm-desc dds-feed">
              <label className="arm-desc__label" htmlFor="dds-action">
                Дополнение — текст действия диспетчера {attempt.actingService?.shortName ?? ''}
              </label>
              <textarea
                id="dds-action"
                maxLength={ACTION_LIMIT}
                value={card.actionsTaken}
                disabled={submitting}
                placeholder="Например: Сообщение принято, аварийная бригада направлена на место."
                onChange={(e) => {
                  const value = e.target.value;
                  onChange((prev) => ({ ...prev, actionsTaken: value }));
                  onFieldChange('actionsTaken', value);
                }}
              />
              <div className="arm-desc__counter">
                {saveState === 'saving' ? 'Сохранение… · ' : saveState === 'error' ? 'Ошибка сохранения · ' : saveState === 'saved' ? 'Сохранено · ' : ''}
                {card.actionsTaken.length}/{ACTION_LIMIT}
              </div>
            </div>
          )}
        </div>

        <div className="arm-col">
          <div className="arm-panel dds-line">
            Пострадавшие: {yesNo(card.flags.victimsPresent)}
            {card.flags.victimsPresent && card.flags.victimsCount ? ` (${card.flags.victimsCount})` : ''}
            <span className="dds-line__gap" />Отказ от скорой: {yesNo(card.flags.ambulanceRefusal)}
            <span className="dds-line__gap" />Заблокированные: {yesNo(card.flags.blocked)}
          </div>

          <div className="dds-type">
            <div className="dds-type__head">{typeNames.join(', ') || 'Тип происшествия не указан'}</div>
            <div className="arm-panel dds-line"><b>{features.length > 0 ? `${features.join('. ')}.` : '—'}</b></div>
            <div className="arm-panel dds-line">Класс.: <b>{typeNames.length > 0 ? `${typeNames.join('; ')};` : ''}</b></div>
            <div className="arm-panel dds-line">[ВИС] Класс.:</div>
          </div>
        </div>
      </div>

      {/* ───────────────────────────── нижняя панель: список оповещения и действия */}
      <DdsServicesBar
        attemptId={attempt.id}
        services={card.services}
        ownServiceId={own?.serviceId}
        disabled={submitting}
        onChanged={onServicesChanged}
        actions={
          <>
            <button type="button" className="dds-finish" disabled={submitting} onClick={onSubmit}>
              {submitting ? 'завершение…' : 'завершить'}
            </button>
            <button
              type="button"
              className="dds-iconbtn"
              disabled
              title="Сообщить о проблеме: в учебной версии нет — сервер не поддерживает"
              aria-label="Сообщить о проблеме"
            >
              <Icon name="message" />
            </button>
            <button
              type="button"
              className="dds-iconbtn"
              disabled={submitting}
              title="Закрыть карточку — вернуться к списку происшествий. Работа не завершается."
              aria-label="Закрыть карточку"
              onClick={onClose}
            >
              ✕
            </button>
          </>
        }
      />
    </div>
  );
}

/** Телефон заявителя в верхней полосе — только просмотр (карточку заполнил оператор 112). */
function PhoneView({ label, value }: { label: string; value?: string }) {
  return (
    <div className="arm-top__cell">
      <div className="arm-phone__label"><span>{label}</span></div>
      <div className="arm-phone__row">
        <input className="arm-phone__input" value={value ?? ''} readOnly disabled aria-label={label} placeholder="—" />
      </div>
    </div>
  );
}
