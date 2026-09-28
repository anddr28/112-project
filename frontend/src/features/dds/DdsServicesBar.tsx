import { useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { Icon } from '../../components/Icon';
import { useEscape } from '../../components/useEscape';
import { ReactionStatusForm, ServiceHistory } from '../incident-card/ServicesBar';
import { toneFor } from '../reaction-status/machine';
import { cls } from '../../shared/utils/cls';
import { formatTime } from '../../shared/utils/time';
import type { AssignedService } from '../../shared/types';

/** Ширина всплывающей панели истории / формы статуса, px (как в макете). */
const POP_WIDTH = 440;

/**
 * Список оповещения на рабочем месте диспетчера ДДС (mockups/pov112-incident-card-department.png,
 * pov112-reaction-status*.png, greyservices2.jpg).
 *
 * Тёмная нижняя панель. Клик по плашке — синяя история статусов над ней (плашка
 * помечается «⌄»); карандаш — только у своей службы — открывает строку ввода статуса
 * («Статус ▾ | Номер наряда | Комментарий | ✓ | ✕»). Чужие службы — только история:
 * их статусы проставляют их диспетчеры (сервер ответит 403). Состав служб задал оператор
 * 112 — добавления и снятия нет.
 *
 * Переходы статусов — только `allowedNext`, обязательность комментария — `commentRequired`
 * (та же форма, что у оператора 112).
 */
export function DdsServicesBar({
  attemptId,
  services,
  ownServiceId,
  disabled,
  onChanged,
  actions,
}: {
  attemptId: string;
  services: AssignedService[];
  /** служба обучающегося (attempt.actingService) — только у неё есть карандаш */
  ownServiceId: string | undefined;
  disabled: boolean;
  onChanged: () => void;
  /** правая часть панели: «завершить», «!», «✕» */
  actions: ReactNode;
}) {
  const wrapRef = useRef<HTMLDivElement>(null);
  const [open, setOpen] = useState<{ id: string; view: 'history' | 'status'; left: number } | null>(null);
  const [error, setError] = useState<string | null>(null);

  const active = open ? services.find((s) => s.serviceId === open.id) ?? null : null;
  useEscape(() => setOpen(null));

  /** Панель открывается над плашкой и не выходит за правый край окна. */
  function place(tile: HTMLElement): number {
    const wrap = wrapRef.current?.getBoundingClientRect();
    const rect = tile.getBoundingClientRect();
    if (!wrap) return 0;
    return Math.max(0, Math.min(rect.left - wrap.left, wrap.width - POP_WIDTH));
  }

  function toggle(s: AssignedService, view: 'history' | 'status', tile: HTMLElement) {
    setError(null);
    setOpen((cur) => (cur?.id === s.serviceId && cur.view === view ? null : { id: s.serviceId, view, left: place(tile) }));
  }

  return (
    <div className="dds-barwrap" ref={wrapRef}>
      {active && open && (
        <div className={cls('dds-pop', open.view === 'status' && 'dds-pop--status')} style={{ left: open.left }}>
          {error && <div className="dds-pop__error" role="alert">{error}</div>}
          {open.view === 'history' ? (
            <ServiceHistory service={active} onClose={() => setOpen(null)} />
          ) : (
            <ReactionStatusForm
              key={active.serviceId}
              attemptId={attemptId}
              service={active}
              disabled={disabled}
              onError={setError}
              onDone={() => {
                setOpen(null);
                onChanged();
              }}
              onCancel={() => setOpen(null)}
            />
          )}
        </div>
      )}

      <div className="dds-bar">
        <div className="dds-bar__label">Службы:</div>

        <div className="dds-services">
          {services.map((s) => {
            const own = s.serviceId === ownServiceId;
            const isOpen = open?.id === s.serviceId;
            const tone = toneFor(s.currentStatus);
            return (
              <div key={s.serviceId} className={cls('dds-svc', isOpen && 'is-open')}>
                <button
                  type="button"
                  className="dds-svc__main"
                  aria-expanded={isOpen && open?.view === 'history'}
                  title={own ? `${s.name} — ваша служба` : s.name}
                  onClick={(e) => toggle(s, 'history', e.currentTarget.parentElement ?? e.currentTarget)}
                >
                  <span className="dds-svc__chev" aria-hidden>{isOpen ? '⌄' : '⌃'}</span>
                  <span className={cls('dds-svc__name', s.isPrimary && 'dds-svc__name--primary')}>{s.shortName}</span>
                  <span className={cls('dds-svc__status', tone === 'rejected' && 'dds-svc__status--rejected')}>
                    <span className="dds-svc__time">{formatTime(s.currentStatusAt).slice(0, 5)}</span> {s.currentStatus}
                  </span>
                </button>
                {own && (
                  <button
                    type="button"
                    className={cls('dds-svc__edit', isOpen && open?.view === 'status' && 'is-on')}
                    disabled={disabled}
                    title="Изменить статус своей службы"
                    aria-label={`Изменить статус: ${s.shortName}`}
                    onClick={(e) => toggle(s, 'status', e.currentTarget.parentElement ?? e.currentTarget)}
                  >
                    <Icon name="pencil" size={12} />
                  </button>
                )}
              </div>
            );
          })}
        </div>

        <div className="dds-bar__actions">{actions}</div>
      </div>
    </div>
  );
}
