import { useEffect, useRef, useState } from 'react';
import type { Dispatch, SetStateAction } from 'react';
import { api } from '../../shared/api';
import { AddressSection } from './AddressSection';
import { WhatHappened } from './WhatHappened';
import { ServicesBar } from './ServicesBar';
import { CallPanel } from '../call-panel/CallPanel';
import { NumberInput } from '../../components/ui';
import { Icon } from '../../components/Icon';
import { useEscape } from '../../components/useEscape';
import { APPLICANT_STATUSES } from '../../shared/types';
import { isAddressFilled } from '../../shared/utils/card';
import { cls } from '../../shared/utils/cls';
import { formatClock } from '../../shared/utils/time';
import type {
  ApplicantStatus, AttemptEventType, AttributeValue, Attempt, IncidentCardDraft,
  StudentCallScript, User,
} from '../../shared/types';
import type { SaveState } from '../attempt-runtime/useAttemptRuntime';

const DESCRIPTION_LIMIT = 1999;

/**
 * Карточка происшествия — рабочее место оператора-112.
 *
 * Структура повторяет реальный ПОВ-112 (docs/СКРИНШОТ КАРТОЧКИ 112ГСИ.docx):
 * верхняя полоса с тремя телефонами и таймером, две колонки (адрес и описание
 * слева, признаки и опросные карты справа), оранжевая панель служб снизу.
 */
export function IncidentCard({
  attempt,
  user,
  card,
  callScript,
  elapsedMs,
  exceeded,
  saveState,
  savedAt,
  submitting,
  onChange,
  onFieldChange,
  onServicesChanged,
  onReplay,
  onEvent,
  onSubmit,
}: {
  attempt: Attempt;
  user: User;
  card: IncidentCardDraft;
  callScript: StudentCallScript | null;
  elapsedMs: number;
  exceeded: boolean;
  saveState: SaveState;
  savedAt: string | null;
  submitting: boolean;
  onChange: Dispatch<SetStateAction<IncidentCardDraft>>;
  onFieldChange: (field: string, value: unknown) => void;
  onServicesChanged: () => void;
  onReplay: () => void;
  /** журнал попытки: панель разговора пишет в него удержание кнопки ответа */
  onEvent: (type: AttemptEventType, payload?: Record<string, unknown>) => void;
  onSubmit: () => void;
}) {
  const [reference, setReference] = useState<{ title: string; text: string } | null>(null);
  const [fiasNotice, setFiasNotice] = useState(false);
  const readOnly = submitting || attempt.status !== 'in_progress';
  const addressFilled = isAddressFilled(card.address);

  /** Номер последнего запроса служб: ответы на устаревшие игнорируются. */
  const resolveSeq = useRef(0);

  const whatRef = useRef<HTMLDivElement>(null);
  const addressRef = useRef<HTMLDivElement>(null);
  const aonRef = useRef<HTMLInputElement>(null);
  const providedRef = useRef<HTMLInputElement>(null);
  const onSiteRef = useRef<HTMLInputElement>(null);

  // Горячие клавиши из Инструкции: Alt+T — что случилось, Alt+A — адрес,
  // Alt+F1..F3 — телефоны заявителя.
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (!e.altKey) return;
      const map: Record<string, () => void> = {
        t: () => whatRef.current?.querySelector('input')?.focus(),
        а: () => whatRef.current?.querySelector('input')?.focus(),
        a: () => addressRef.current?.querySelector('input')?.focus(),
        ф: () => addressRef.current?.querySelector('input')?.focus(),
        F1: () => aonRef.current?.focus(),
        F2: () => providedRef.current?.focus(),
        F3: () => onSiteRef.current?.focus(),
      };
      const action = map[e.key] ?? map[e.key.toLowerCase()];
      if (action) {
        e.preventDefault();
        action();
      }
    }
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

  function patch(next: Partial<IncidentCardDraft>) {
    onChange((prev) => ({ ...prev, ...next }));
  }

  /**
   * Пересчёт служб: тип + адрес + признаки. Правила живут в классификаторе.
   *
   * Обновление функциональное: запрос асинхронный, и за это время оператор
   * может успеть набрать текст в другом поле — перезаписывать карточку
   * снимком состояния на момент вызова нельзя.
   */
  async function recalcServices(nextCard: IncidentCardDraft) {
    onChange(nextCard);

    const seq = ++resolveSeq.current;
    try {
      const result = await api.classifier.resolveServices({
        typeIds: nextCard.incidentTypeIds,
        attributes: nextCard.attributes,
        addressFilled: isAddressFilled(nextCard.address),
        addressSource: nextCard.address.source,
        current: nextCard.services,
      });

      // Признаки переключают быстрее, чем отвечает классификатор: ответ на
      // устаревший запрос не должен подменять более свежий список служб.
      if (seq !== resolveSeq.current) return;

      setFiasNotice(result.suppressedByAddressSource);
      onChange((prev) => ({ ...prev, services: result.services }));
    } catch {
      // Список служб остаётся прежним: оператор продолжает заполнять карточку,
      // а недостающую службу может добавить вручную кнопкой «+».
    }
  }

  return (
    <div className="arm">
      {/* ─────────────────────────── верхняя полоса: телефоны, карточка, таймер */}
      <div className="arm-top">
        <div className="arm-top__cell arm-top__disconnect">
          <div className="arm-top__hangup">
            <span className="arm-top__hangup-icon"><Icon name="phone-hangup" size={15} /></span>
            <span>Отключение</span>
          </div>
          <div className="arm-top__minibtns">
            <button type="button" className="arm-mini" disabled title="Доступно после сохранения карточки">записи звонков</button>
            <button type="button" className="arm-mini" disabled title="Обработка СМС: в этой версии недоступна">список СМС</button>
          </div>
        </div>

        <PhoneCell
          inputRef={aonRef}
          label="АОН"
          value={card.phones.aon ?? ''}
          readOnly
          hint="заполняется автоматически при приёме вызова"
          onChange={() => undefined}
        />

        <PhoneCell
          inputRef={providedRef}
          label="предоставленный"
          value={card.phones.provided ?? ''}
          disabled={readOnly}
          showAon
          onAon={() => {
            patch({ phones: { ...card.phones, provided: card.phones.aon } });
            onFieldChange('phones.provided', card.phones.aon);
          }}
          onChange={(v) => {
            patch({ phones: { ...card.phones, provided: v } });
            onFieldChange('phones.provided', v);
          }}
        />

        <PhoneCell
          inputRef={onSiteRef}
          label="телефон на место"
          value={card.phones.onSite ?? ''}
          disabled={readOnly}
          showAon
          onAon={() => {
            patch({ phones: { ...card.phones, onSite: card.phones.aon } });
            onFieldChange('phones.onSite', card.phones.aon);
          }}
          onChange={(v) => {
            patch({ phones: { ...card.phones, onSite: v } });
            onFieldChange('phones.onSite', v);
          }}
        />

        <div className="arm-top__cell arm-top__ident">
          <div className="arm-ident">
            <div className="arm-ident__no">Происшествие {attempt.incidentNo}</div>
            <div className="arm-ident__meta">
              {savedAt ? `Сохр. ${new Date(savedAt).toLocaleString('ru-RU')}` : 'Не сохранена'}
            </div>
            <div className="arm-ident__meta">
              {user.operatorNo ?? 'оп.'}, {user.workstation ?? 'АРМ'}, УМЦ
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

      {/* ─────────────────────────────────────────────────────── тело карточки */}
      <div className="arm-body">
        <div className="arm-col">
          <div className="arm-panel arm-applicant">
            <label className="arm-fld">
              <span className="arm-fld__label">Фамилия и имя заявителя</span>
              <input
                className="arm-line-input"
                value={card.applicant.name ?? ''}
                disabled={readOnly}
                placeholder="Фамилия и имя заявителя"
                onChange={(e) => {
                  // Инструкция п.5.3: фамилия и имя вводятся с заглавной буквы.
                  const v = e.target.value.replace(/(^|\s)([а-яёa-z])/g, (_, s: string, c: string) => s + c.toUpperCase());
                  patch({ applicant: { ...card.applicant, name: v } });
                  onFieldChange('applicant.name', v);
                }}
              />
            </label>

            <label className="arm-fld">
              <span className="arm-fld__label">Статус заявителя</span>
              <select
                className="arm-line-input"
                value={card.applicant.status ?? ''}
                disabled={readOnly}
                onChange={(e) => {
                  const v = (e.target.value || undefined) as ApplicantStatus | undefined;
                  patch({ applicant: { ...card.applicant, status: v } });
                  onFieldChange('applicant.status', v);
                }}
              >
                <option value="">выберите статус</option>
                {APPLICANT_STATUSES.map((s) => <option key={s} value={s}>{s}</option>)}
              </select>
            </label>

            <button
              type="button"
              className={cls('arm-mini', card.applicant.foreignLanguage && 'is-on')}
              disabled={readOnly}
              title="Вызов на иностранном языке"
              onClick={() => {
                const foreignLanguage = !card.applicant.foreignLanguage;
                patch({ applicant: { ...card.applicant, foreignLanguage } });
                onFieldChange('applicant.foreignLanguage', foreignLanguage);
              }}
              style={card.applicant.foreignLanguage ? { background: 'var(--arm-selected)', color: '#fff' } : undefined}
            >
              <Icon name="language" size={13} />
            </button>
          </div>

          <div ref={addressRef}>
            <AddressSection
              value={card.address}
              disabled={readOnly}
              onChange={(address) => {
                const next = { ...card, address };
                onFieldChange('address.raw', address.raw);
                void recalcServices(next);
              }}
            />
          </div>

          {fiasNotice && (
            <div className="arm-note">
              Адрес выбран из ФИАС — службы автоматически не добавляются.
              Добавьте необходимые службы вручную кнопкой «+».
            </div>
          )}

          <div className="arm-panel arm-desc">
            <div className="arm-desc__label">Описание со слов заявителя</div>
            <textarea
              value={card.description}
              disabled={readOnly}
              maxLength={DESCRIPTION_LIMIT}
              placeholder="введите"
              onChange={(e) => {
                patch({ description: e.target.value });
                onFieldChange('description', e.target.value);
              }}
            />
            <div className={cls('arm-desc__counter', card.description.length > DESCRIPTION_LIMIT - 100 && 'arm-desc__counter--over')}>
              {card.description.length} / {DESCRIPTION_LIMIT}
            </div>
          </div>

          {callScript && (
            <CallPanel
              attemptId={attempt.id}
              script={callScript}
              disabled={readOnly}
              onReplay={onReplay}
              onEvent={onEvent}
            />
          )}
        </div>

        <div className="arm-col">
          <div className="arm-panel arm-flags">
            <div className="arm-flags__group">
              <button
                type="button"
                className={cls('arm-flagbtn', card.flags.victimsPresent && 'is-on')}
                disabled={readOnly}
                onClick={() => {
                  const victimsPresent = !card.flags.victimsPresent;
                  patch({ flags: { ...card.flags, victimsPresent, victimsCount: victimsPresent ? (card.flags.victimsCount ?? 1) : undefined } });
                  onFieldChange('flags.victimsPresent', victimsPresent);
                }}
              >
                Пострадавшие
              </button>

              {card.flags.victimsPresent && (
                <NumberInput
                  className="arm-line-input"
                  style={{ width: 56, textAlign: 'center' }}
                  min={1}
                  value={card.flags.victimsCount ?? 1}
                  disabled={readOnly}
                  aria-label="Количество пострадавших"
                  onChange={(victimsCount) => {
                    patch({ flags: { ...card.flags, victimsCount } });
                    onFieldChange('flags.victimsCount', victimsCount);
                  }}
                />
              )}

              <button
                type="button"
                className={cls('arm-flagbtn', card.flags.ambulanceRefusal && 'is-on')}
                disabled={readOnly}
                onClick={() => {
                  const ambulanceRefusal = !card.flags.ambulanceRefusal;
                  patch({ flags: { ...card.flags, ambulanceRefusal } });
                  onFieldChange('flags.ambulanceRefusal', ambulanceRefusal);
                }}
              >
                Нет на месте/<br />Отказ от скорой
              </button>

              <button
                type="button"
                className={cls('arm-flagbtn', card.flags.blocked && 'is-on')}
                disabled={readOnly}
                onClick={() => {
                  const blocked = !card.flags.blocked;
                  patch({ flags: { ...card.flags, blocked } });
                  onFieldChange('flags.blocked', blocked);
                }}
              >
                Нет доступа/<br />Заблокированные
              </button>
            </div>

            <div className="arm-flags__group">
              <button
                type="button"
                className={cls('arm-flagbtn', 'arm-flagbtn--alert', card.flags.noContact && 'is-on')}
                disabled={readOnly}
                onClick={() => {
                  const noContact = !card.flags.noContact;
                  patch({ flags: { ...card.flags, noContact } });
                  onFieldChange('flags.noContact', noContact);
                }}
              >
                нет контакта
              </button>
              <button
                type="button"
                className={cls('arm-flagbtn', 'arm-flagbtn--alert', card.flags.callDropped && 'is-on')}
                disabled={readOnly}
                onClick={() => {
                  const callDropped = !card.flags.callDropped;
                  patch({ flags: { ...card.flags, callDropped } });
                  onFieldChange('flags.callDropped', callDropped);
                }}
              >
                срыв звонка
              </button>
            </div>
          </div>

          <div className="arm-panel" ref={whatRef}>
            <WhatHappened
              selectedIds={card.incidentTypeIds}
              attributes={card.attributes}
              addressFilled={addressFilled}
              disabled={readOnly}
              onChangeTypes={(ids) => {
                // Инструкция п.4.4: при удалении всех типов — возврат к «Что случилось?»
                const next = { ...card, incidentTypeIds: ids, attributes: ids.length === 0 ? {} : card.attributes };
                onFieldChange('incidentTypeIds', ids);
                void recalcServices(next);
              }}
              onChangeAttribute={(code, value: AttributeValue) => {
                const next = { ...card, attributes: { ...card.attributes, [code]: value } };
                onFieldChange(`attributes.${code}`, value);
                void recalcServices(next);
              }}
              onOpenReference={(typeId) => {
                void api.classifier.reference(typeId).then(
                  (r) => {
                    setReference({
                      title: 'Справочная информация',
                      text: r.exists && r.text ? r.text : 'Справочная страница для этого типа происшествия не создана.',
                    });
                  },
                  () => {
                    setReference({
                      title: 'Справочная информация',
                      text: 'Не удалось загрузить справку. Попробуйте позже.',
                    });
                  },
                );
              }}
            />
          </div>
        </div>
      </div>

      {/* ───────────────────────────────── нижняя панель: службы и сохранение */}
      <div>
        <ServicesBar
          attemptId={attempt.id}
          services={card.services}
          disabled={readOnly}
          onChanged={onServicesChanged}
        >
          {/* Вторая строка той же оранжевой панели, не отдельный блок. */}
          <div className="arm-bottom">
            <div className="arm-bottom__label">
              <span className="arm-autosave">
                <span className={cls('arm-autosave__dot', `arm-autosave__dot--${saveState}`)} />
                {saveState === 'saving' ? 'Сохранение…' : saveState === 'saved' ? 'Черновик сохранён' : saveState === 'error' ? 'Ошибка сохранения' : 'Черновик'}
              </span>
            </div>

            <div />

            <div className="arm-bottom__actions">
              <button type="button" className="arm-save" disabled={readOnly} onClick={onSubmit}>
                {submitting ? 'Сохранение…' : 'сохранить'}
              </button>
              {/* Резерв под функции реального АРМ: кнопки видны, но недоступны — см. title. */}
              <button type="button" className="arm-iconbtn" disabled title="Связи между карточками: в этой версии недоступно" aria-label="Связи между карточками"><Icon name="link" /></button>
              <button type="button" className="arm-iconbtn" disabled title="Напоминание: в этой версии недоступно" aria-label="Напоминание"><Icon name="alarm" /></button>
              <button type="button" className="arm-iconbtn" disabled title="Важное происшествие: в этой версии недоступно" aria-label="Важное происшествие"><Icon name="flag" /></button>
              <button type="button" className="arm-iconbtn" disabled title="Уведомления: в этой версии недоступно" aria-label="Уведомления"><Icon name="bell" /></button>
              <button type="button" className="arm-iconbtn" disabled title="Сообщить о проблеме: в этой версии недоступно" aria-label="Сообщить о проблеме"><Icon name="message" /></button>
            </div>
          </div>
        </ServicesBar>
      </div>

      {reference && <ReferenceDialog title={reference.title} text={reference.text} onClose={() => setReference(null)} />}
    </div>
  );
}

/** Справочная информация по типу происшествия (Инструкция п.4.2). */
function ReferenceDialog({ title, text, onClose }: { title: string; text: string; onClose: () => void }) {
  useEscape(onClose);

  return (
    <div className="modal-backdrop" role="presentation" onClick={(e) => e.target === e.currentTarget && onClose()}>
      <div className="arm-modal" role="dialog" aria-modal="true" aria-label={title}>
        <div className="arm-modal__head">
          <span className="arm-modal__title">{title}</span>
          <button type="button" className="arm-survey__close" style={{ color: 'var(--arm-text)' }} onClick={onClose}>✕</button>
        </div>
        <div style={{ padding: '0 16px 16px', fontSize: 13, lineHeight: 1.5 }}>{text}</div>
      </div>
    </div>
  );
}

function PhoneCell({
  inputRef,
  label,
  value,
  disabled,
  readOnly,
  showAon,
  hint,
  onChange,
  onAon,
}: {
  inputRef: React.RefObject<HTMLInputElement | null>;
  label: string;
  value: string;
  disabled?: boolean;
  readOnly?: boolean;
  showAon?: boolean;
  hint?: string;
  onChange: (v: string) => void;
  onAon?: () => void;
}) {
  return (
    <div className="arm-top__cell">
      <div className="arm-phone__label">
        <span>{label}</span>
        {hint && <span style={{ fontSize: 9 }}>{hint}</span>}
      </div>
      <div className="arm-phone__row">
        <input
          ref={inputRef}
          className="arm-phone__input"
          value={value}
          disabled={disabled || readOnly}
          placeholder="+7 (   )   -  -"
          onChange={(e) => onChange(e.target.value)}
          aria-label={label}
        />
        {showAon && (
          <button type="button" className="arm-mini arm-phone__aon-btn" disabled={disabled} onClick={onAon} title="Скопировать номер АОН">
            АОН
          </button>
        )}
      </div>
    </div>
  );
}
