import { useState } from 'react';
import type { ReactNode } from 'react';
import { cls } from '../shared/utils/cls';
import { useEscape } from './useEscape';

export function Badge({
  tone = 'neutral',
  value,
  children,
}: {
  tone?: 'neutral' | 'accent' | 'ok' | 'warn' | 'danger';
  /** Числовое значение с единицами: без перевода в заглавные. */
  value?: boolean;
  children: ReactNode;
}) {
  return <span className={cls('badge', `badge--${tone}`, value && 'badge--value')}>{children}</span>;
}

export function Card({ title, actions, children, footer }: {
  title?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
  footer?: ReactNode;
}) {
  return (
    <section className="card">
      {(title || actions) && (
        <header className="card__head">
          <div className="card__title">{title}</div>
          <div className="shell__spacer" />
          {actions}
        </header>
      )}
      <div className="card__body">{children}</div>
      {footer && <footer className="card__foot">{footer}</footer>}
    </section>
  );
}

/**
 * Числовое поле.
 *
 * Обычный `value={number}` с `Number(e.target.value)` навязывает ноль: стоит
 * очистить поле, как `Number('')` даёт 0, поле показывает «0», и следующая
 * цифра дописывается к нему. Здесь поле может быть пустым во время ввода:
 * модель обновляется только разобранным числом, границы применяются при
 * потере фокуса, чтобы «1» на пути к «120» не обрезалось до минимума.
 */
export function NumberInput({
  value,
  onChange,
  min,
  max,
  className = 'input',
  disabled,
  'aria-label': ariaLabel,
  style,
}: {
  value: number | undefined;
  onChange: (value: number) => void;
  min?: number;
  max?: number;
  className?: string;
  disabled?: boolean;
  'aria-label'?: string;
  style?: React.CSSProperties;
}) {
  // Черновик ввода: null — показываем значение модели, строка — пользователь печатает.
  const [draft, setDraft] = useState<string | null>(null);
  const shown = draft ?? (value == null ? '' : String(value));

  function commit(raw: string) {
    setDraft(null);
    if (raw.trim() === '') return;
    const parsed = Number(raw);
    if (!Number.isFinite(parsed)) return;
    const lo = min ?? Number.NEGATIVE_INFINITY;
    const hi = max ?? Number.POSITIVE_INFINITY;
    const clamped = Math.min(hi, Math.max(lo, parsed));
    if (clamped !== value) onChange(clamped);
  }

  return (
    <input
      className={className}
      type="number"
      inputMode="numeric"
      min={min}
      max={max}
      value={shown}
      disabled={disabled}
      aria-label={ariaLabel}
      style={style}
      onChange={(e) => {
        const raw = e.target.value;
        setDraft(raw);
        if (raw.trim() === '') return;
        const parsed = Number(raw);
        if (Number.isFinite(parsed)) onChange(parsed);
      }}
      onBlur={(e) => commit(e.target.value)}
      onKeyDown={(e) => {
        if (e.key === 'Enter') commit((e.target as HTMLInputElement).value);
      }}
    />
  );
}

export function Field({ label, hint, error, children }: {
  label: string;
  hint?: string;
  error?: string;
  children: ReactNode;
}) {
  return (
    <label className="field">
      <span className="field__label">{label}</span>
      {children}
      {error ? <span className="field__error">{error}</span> : hint ? <span className="field__hint">{hint}</span> : null}
    </label>
  );
}

export function Metric({ label, value, note, tone }: {
  label: string;
  value: ReactNode;
  note?: ReactNode;
  tone?: 'ok' | 'warn' | 'danger';
}) {
  const color = tone === 'ok' ? 'var(--u-ok)' : tone === 'warn' ? 'var(--u-warn)' : tone === 'danger' ? 'var(--u-danger)' : undefined;
  return (
    <div className="metric">
      <span className="metric__label">{label}</span>
      <span className="metric__value" style={color ? { color } : undefined}>{value}</span>
      {note && <span className="metric__note">{note}</span>}
    </div>
  );
}

export function Meter({ value, tone }: { value: number; tone?: 'ok' | 'warn' | 'danger' }) {
  return (
    <div className="meter" role="presentation">
      <div className={cls('meter__fill', tone && `meter__fill--${tone}`)} style={{ width: `${Math.min(100, Math.max(0, value))}%` }} />
    </div>
  );
}

export function Loading({ text = 'Загрузка…' }: { text?: string }) {
  return (
    <div className="state">
      <div className="spinner" />
      <div className="state__text">{text}</div>
    </div>
  );
}

export function ErrorState({ text, onRetry }: { text: string; onRetry?: () => void }) {
  return (
    <div className="state">
      <div className="state__title">Не удалось загрузить</div>
      <div className="state__text">{text}</div>
      {onRetry && <button type="button" className="btn" onClick={onRetry}>Повторить</button>}
    </div>
  );
}

export function EmptyState({ title, text, action }: { title: string; text?: string; action?: ReactNode }) {
  return (
    <div className="state">
      <div className="state__title">{title}</div>
      {text && <div className="state__text">{text}</div>}
      {action}
    </div>
  );
}

export function Modal({ title, onClose, children, footer, wide }: {
  title: string;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
  wide?: boolean;
}) {
  useEscape(onClose);

  return (
    <div
      className="modal-backdrop"
      role="presentation"
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div className={cls('modal', wide && 'modal--wide')} role="dialog" aria-modal="true" aria-label={title}>
        <header className="card__head">
          <div className="card__title">{title}</div>
          <div className="shell__spacer" />
          <button type="button" className="btn btn--ghost btn--sm" onClick={onClose} aria-label="Закрыть">✕</button>
        </header>
        <div className="card__body">{children}</div>
        {footer && <footer className="card__foot">{footer}</footer>}
      </div>
    </div>
  );
}

const DIFFICULTY_LABEL: Record<number, string> = { 1: 'Базовая', 2: 'Средняя', 3: 'Высокая' };

export function DifficultyBadge({ level }: { level: number }) {
  const tone = level === 1 ? 'ok' : level === 2 ? 'warn' : 'danger';
  return <Badge tone={tone}>{DIFFICULTY_LABEL[level] ?? `Уровень ${level}`}</Badge>;
}

const SCENARIO_STATUS: Record<string, { label: string; tone: 'neutral' | 'accent' | 'ok' | 'warn' | 'danger' }> = {
  draft: { label: 'Черновик', tone: 'neutral' },
  generated: { label: 'Сгенерирован ИИ', tone: 'warn' },
  validated: { label: 'Подтверждён', tone: 'ok' },
  rejected: { label: 'Отклонён', tone: 'danger' },
  archived: { label: 'В архиве', tone: 'neutral' },
};

export function ScenarioStatusBadge({ status }: { status: string }) {
  const s = SCENARIO_STATUS[status] ?? { label: 'Состояние не определено', tone: 'neutral' as const };
  return <Badge tone={s.tone}>{s.label}</Badge>;
}

const LESSON_STATUS: Record<string, { label: string; tone: 'neutral' | 'accent' | 'ok' | 'warn' | 'danger' }> = {
  draft: { label: 'Черновик', tone: 'neutral' },
  scheduled: { label: 'Запланировано', tone: 'accent' },
  running: { label: 'Идёт', tone: 'ok' },
  finished: { label: 'Завершено', tone: 'neutral' },
  cancelled: { label: 'Отменено', tone: 'danger' },
};

export function LessonStatusBadge({ status }: { status: string }) {
  const s = LESSON_STATUS[status] ?? { label: 'Состояние не определено', tone: 'neutral' as const };
  return <Badge tone={s.tone}>{s.label}</Badge>;
}
