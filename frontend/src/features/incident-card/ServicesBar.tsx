import { useEffect, useState } from 'react';
import type { ReactNode } from 'react';
import { api } from '../../shared/api';
import { useEscape } from '../../components/useEscape';
import { toneFor } from '../reaction-status/machine';
import { cls } from '../../shared/utils/cls';
import { formatDateTime, formatTime } from '../../shared/utils/time';
import type { AssignedService, ReactionStatus, ServiceRef } from '../../shared/types';

/**
 * Нижняя панель: список оповещения + статусы реагирования.
 *
 * Инструкция п.6 и п.8:
 *   — список формируется автоматически по типу происшествия и адресу;
 *   — может дополняться при заполнении подробностей;
 *   — основные службы подчёркиваются двойной линией;
 *   — ручное добавление/удаление — нештатное действие;
 *   — свёрнутый блок показывает последний статус и время.
 *
 * Список допустимых переходов берётся ТОЛЬКО из `service.allowedNext`
 * (в mock-режиме его считает shared/mocks/reactionTransitions.ts).
 *
 * Ракурс «Диспетчер ДДС» (v1.3) — задан `ownServiceId`: состав служб задал
 * оператор 112, поэтому добавления и снятия нет; статус проставляется только
 * своей службе, у чужих — только просмотр истории (сервер ответит 403).
 *
 * `children` — строка действий по карточке («Черновик», «сохранить», иконки).
 * Она приходит из IncidentCard и встаёт второй строкой той же оранжевой
 * панели: раньше это был отдельный блок со своим фоном и высотой, из-за чего
 * панель выглядела как два наложенных друг на друга элемента.
 */
export function ServicesBar({
  attemptId,
  services,
  disabled,
  onChanged,
  ownServiceId,
  children,
}: {
  attemptId: string;
  services: AssignedService[];
  disabled: boolean;
  onChanged: () => void;
  /** ракурс dds: служба обучающегося; без него — поведение оператора 112 */
  ownServiceId?: string;
  children?: ReactNode;
}) {
  const dds = ownServiceId !== undefined;
  const [activeId, setActiveId] = useState<string | null>(null);
  const [addOpen, setAddOpen] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Служба могла быть удалена — выводим активную при рендере, без синхронизации эффектом.
  const active = services.find((s) => s.serviceId === activeId) ?? null;

  return (
    <>
      {error && (
        <div className="arm-note" style={{ margin: '0 8px 6px' }} role="alert">
          {error}
          <button type="button" className="arm-mini" style={{ marginLeft: 8 }} onClick={() => setError(null)}>
            Понятно
          </button>
        </div>
      )}

      {active && (
        <>
          <ServiceHistory service={active} onClose={() => setActiveId(null)} />
          {dds && active.serviceId !== ownServiceId ? (
            <div className="arm-note" style={{ margin: '0 8px 6px' }}>
              Статус этой службы проставляет её диспетчер — вы работаете за свою службу.
            </div>
          ) : (
            <ReactionStatusForm
              key={active.serviceId}
              attemptId={attemptId}
              service={active}
              disabled={disabled}
              onError={setError}
              onDone={onChanged}
            />
          )}
        </>
      )}

      <div className="arm-bottombar">
        <div className="arm-bottom">
          <div className="arm-bottom__label">Службы:</div>

          <div className="arm-services">
            {services.length === 0 && (
              <div style={{ display: 'flex', alignItems: 'center', color: '#fff', fontSize: 11, opacity: 0.9 }}>
                Определяются автоматически после выбора типа происшествия и адреса
              </div>
            )}

            {services.map((s) => {
              const tone = toneFor(s.currentStatus);
              return (
                <button
                  key={s.serviceId}
                  type="button"
                  className={cls(
                    'arm-service',
                    activeId === s.serviceId && 'is-active',
                    dds && s.serviceId === ownServiceId && 'arm-service--own',
                  )}
                  onClick={() => setActiveId(activeId === s.serviceId ? null : s.serviceId)}
                  title={s.reason ? `${s.name}\nОснование: ${s.reason}` : s.name}
                >
                  <span className="arm-service__top">
                    <span className={cls('arm-service__name', s.isPrimary && 'arm-service__name--primary')}>
                      {s.shortName}
                    </span>
                    {s.source === 'vis' && <span className="arm-service__vis">ВИС</span>}
                    {dds && s.serviceId === ownServiceId && <span className="arm-service__vis">ВАША</span>}
                    {!disabled && !dds && (
                      <span
                        role="button"
                        tabIndex={-1}
                        className="arm-service__x"
                        aria-label={`Убрать ${s.shortName}`}
                        onClick={(e) => {
                          e.stopPropagation();
                          setError(null);
                          void api.attempts
                            .removeService(attemptId, s.serviceId)
                            .then(onChanged)
                            .catch((err: unknown) => {
                              // Панель перечитываем в любом случае: состояние
                              // служб на сервере могло измениться.
                              onChanged();
                              setError(err instanceof Error ? err.message : 'Не удалось снять службу');
                            });
                        }}
                      >
                        ✕
                      </span>
                    )}
                  </span>
                  <span
                    className={cls(
                      'arm-service__status',
                      tone === 'rejected' && 'arm-service__status--rejected',
                      tone === 'done' && 'arm-service__status--done',
                    )}
                  >
                    {formatTime(s.currentStatusAt).slice(0, 5)} {s.currentStatus}
                  </span>
                </button>
              );
            })}

            {!dds && (
              <button
                type="button"
                className="arm-addsvc"
                disabled={disabled}
                onClick={() => setAddOpen(true)}
                title="Добавить службу вручную"
                aria-label="Добавить службу"
              >
                +
              </button>
            )}
          </div>

          <div className="arm-bottom__actions" />
        </div>

        {children}
      </div>

      {addOpen && (
        <AddServiceDialog
          attemptId={attemptId}
          assigned={services}
          onClose={() => setAddOpen(false)}
          onDone={onChanged}
        />
      )}
    </>
  );
}

function ReactionStatusForm({
  attemptId,
  service,
  disabled,
  onError,
  onDone,
}: {
  attemptId: string;
  service: AssignedService;
  disabled: boolean;
  onError: (msg: string | null) => void;
  onDone: () => void;
}) {
  const [status, setStatus] = useState<ReactionStatus | ''>('');
  const [squad, setSquad] = useState('');
  const [comment, setComment] = useState('');
  const [busy, setBusy] = useState(false);

  const transition = service.allowedNext.find((t) => t.status === status);
  const commentRequired = transition?.commentRequired ?? false;
  const canSubmit = Boolean(status) && (!commentRequired || comment.trim().length > 0) && !busy && !disabled;

  if (!service.editable) {
    return (
      <div className="arm-note" style={{ margin: '0 8px 6px' }}>
        Статус «{service.currentStatus}» закрывает карточку для редактирования этой службой.
      </div>
    );
  }

  async function submit() {
    if (!status) return;
    setBusy(true);
    onError(null);
    try {
      await api.attempts.changeServiceStatus({
        attemptId,
        serviceId: service.serviceId,
        status,
        squadNumber: squad || undefined,
        comment: comment || undefined,
      });
      setStatus('');
      setSquad('');
      setComment('');
      onDone();
    } catch (e) {
      onError(e instanceof Error ? e.message : 'Не удалось изменить статус');
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="arm-statusbar">
      <select
        value={status}
        disabled={disabled || service.allowedNext.length === 0}
        onChange={(e) => setStatus(e.target.value as ReactionStatus)}
        aria-label="Статус реагирования"
      >
        <option value="">Статус</option>
        {service.allowedNext.map((t) => (
          <option key={t.status} value={t.status}>{t.label}</option>
        ))}
      </select>

      <input
        value={squad}
        disabled={disabled}
        placeholder="Номер наряда"
        onChange={(e) => setSquad(e.target.value)}
        aria-label="Номер наряда"
      />

      <input
        value={comment}
        disabled={disabled}
        placeholder={commentRequired ? 'Комментарий обязателен: укажите причину' : 'Комментарий'}
        onChange={(e) => setComment(e.target.value)}
        aria-label="Комментарий к статусу"
        style={commentRequired && !comment.trim() ? { borderColor: 'var(--arm-red)' } : undefined}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && canSubmit) void submit();
        }}
      />

      <div className="row row--tight">
        <button type="button" className="arm-statusbar__ok" disabled={!canSubmit} onClick={() => void submit()} title="Сохранить статус">✓</button>
        <button
          type="button"
          className="arm-statusbar__cancel"
          onClick={() => {
            setStatus('');
            setSquad('');
            setComment('');
          }}
          title="Отмена"
        >
          ✕
        </button>
      </div>
    </div>
  );
}

function ServiceHistory({ service, onClose }: { service: AssignedService; onClose: () => void }) {
  return (
    <div className="arm-history">
      <div className="arm-history__head">
        <span>{service.name}</span>
        <button type="button" className="arm-history__close" onClick={onClose} aria-label="Закрыть историю">✕</button>
      </div>
      {service.history.map((entry, i) => (
        <div className="arm-history__row" key={`${entry.status}-${i}`}>
          <span>{entry.operator}</span>
          <span>{formatDateTime(entry.at)} {entry.status}</span>
          <span>{entry.comment ?? ''}{entry.squadNumber ? ` (наряд ${entry.squadNumber})` : ''}</span>
        </div>
      ))}
    </div>
  );
}

function AddServiceDialog({
  attemptId,
  assigned,
  onClose,
  onDone,
}: {
  attemptId: string;
  assigned: AssignedService[];
  onClose: () => void;
  onDone: () => void;
}) {
  const [all, setAll] = useState<ServiceRef[]>([]);
  const [query, setQuery] = useState('');
  const [picked, setPicked] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEscape(onClose);

  useEffect(() => {
    let cancelled = false;
    void api.services.list().then(
      (list) => {
        if (!cancelled) setAll(list);
      },
      () => {
        if (!cancelled) setError('Не удалось загрузить справочник служб');
      },
    );
    return () => { cancelled = true; };
  }, []);

  const assignedCodes = assigned.map((s) => s.code);
  const list = all.filter(
    (s) =>
      s.name.toLowerCase().includes(query.toLowerCase()) ||
      s.shortName.toLowerCase().includes(query.toLowerCase()),
  );

  async function save() {
    setBusy(true);
    setError(null);
    try {
      for (const code of picked) {
        await api.attempts.addService(attemptId, code);
      }
      onDone();
      onClose();
    } catch (e) {
      // Часть служб могла добавиться: обновляем панель и оставляем окно открытым.
      onDone();
      setError(e instanceof Error ? e.message : 'Не удалось добавить службу');
      setBusy(false);
    }
  }

  return (
    <div className="modal-backdrop" role="presentation" onClick={(e) => e.target === e.currentTarget && onClose()}>
      <div className="arm-modal" role="dialog" aria-modal="true" aria-label="Добавьте службы">
        <div className="arm-modal__head">
          <span className="arm-modal__title">Добавьте службы</span>
          <button type="button" className="arm-survey__close" style={{ color: 'var(--arm-text)' }} onClick={onClose}>✕</button>
        </div>

        <div className="arm-modal__search">
          <input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="Поиск ..." autoFocus />
        </div>

        <div className="arm-note" style={{ margin: '0 16px 8px' }}>
          В штатной ситуации список формируется автоматически по классификатору.
          Ручное изменение — нештатное действие и фиксируется в журнале.
        </div>

        <div className="arm-modal__list">
          {list.map((s) => {
            const isAssigned = assignedCodes.includes(s.code);
            const isPicked = picked.includes(s.code);
            return (
              <button
                key={s.id}
                type="button"
                className={cls('arm-modal__item', (isAssigned || isPicked) && 'is-selected')}
                disabled={isAssigned}
                onClick={() =>
                  setPicked(isPicked ? picked.filter((c) => c !== s.code) : [...picked, s.code])
                }
              >
                {s.name}
                {isAssigned && ' — уже назначена'}
              </button>
            );
          })}
        </div>

        {error && <div className="arm-note" role="alert" style={{ margin: '0 16px 8px' }}>{error}</div>}

        <div className="arm-modal__foot">
          <button type="button" className="arm-modal__save" disabled={busy} onClick={() => void save()}>
            {busy ? 'Сохранение…' : 'Сохранить и закрыть'}
          </button>
        </div>
      </div>
    </div>
  );
}
