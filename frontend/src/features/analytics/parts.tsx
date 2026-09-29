import { Badge } from '../../components/ui';
import { LAYER_LABEL } from './labels';
import { formatNumber, formatPct } from '../../shared/utils/format';
import type { AnalyticsOverview, Insight } from '../../shared/types';

const KIND_LABEL: Record<string, string> = {
  missing: 'не заполнено',
  wrong: 'неверно',
  extra: 'лишнее',
};

/**
 * Средний балл по слоям оценки — горизонтальные полосы одного цвета на общей
 * шкале 0–100. Число — у конца полосы, текстом, а не цветом.
 */
export function LayerBars({ layers }: { layers: AnalyticsOverview['layers'] }) {
  if (layers.length === 0) return <p className="muted small">Нет оценённых слоёв.</p>;
  return (
    <div className="hbars" role="list">
      {layers.map((l) => (
        <div key={l.layer} className="hbars__row" role="listitem" title={`${LAYER_LABEL[l.layer] ?? l.layer}: ${formatNumber(l.avgScore)} (карточек: ${l.attempts})`}>
          <span className="hbars__label">{LAYER_LABEL[l.layer] ?? l.layer}</span>
          <span className="hbars__track">
            <span className="hbars__bar" style={{ width: `${Math.max(1, Math.min(100, l.avgScore))}%` }} />
          </span>
          <span className="hbars__value num">{formatNumber(l.avgScore)}</span>
        </div>
      ))}
    </div>
  );
}

const SEVERITY: Record<Insight['severity'], { label: string; tone: 'accent' | 'warn' | 'danger'; mark: string }> = {
  info: { label: 'К сведению', tone: 'accent', mark: 'i' },
  warning: { label: 'Внимание', tone: 'warn', mark: '!' },
  critical: { label: 'Важно', tone: 'danger', mark: '!!' },
};

/**
 * Инсайт: вывод по типичным ошибкам группы и что с этим делать. Важность —
 * значком и словом, цвет только дублирует их.
 */
export function InsightCard({ insight }: { insight: Insight }) {
  const s = SEVERITY[insight.severity] ?? SEVERITY.info;
  return (
    <article className={`insight insight--${insight.severity}`}>
      <header className="row row--tight">
        <span className={`insight__mark insight__mark--${insight.severity}`} aria-hidden="true">{s.mark}</span>
        <Badge tone={s.tone}>{s.label}</Badge>
        {insight.metric != null && <span className="insight__metric num">{formatNumber(insight.metric)}</span>}
      </header>
      <h3 className="insight__title">{insight.title}</h3>
      <p className="insight__body">{insight.body}</p>
      {insight.affectedStudents && insight.affectedStudents.length > 0 && (
        <p className="small muted">Обучающиеся: {insight.affectedStudents.join(', ')}</p>
      )}
      {insight.evidence && insight.evidence.length > 0 && (
        <ul className="insight__evidence">
          {insight.evidence.map((e) => <li key={e}>«{e}»</li>)}
        </ul>
      )}
    </article>
  );
}

export function FieldErrorsTable({ rows }: { rows: AnalyticsOverview['topFieldErrors'] }) {
  if (rows.length === 0) return <p className="muted small">Ошибок в полях нет.</p>;
  return (
    <table className="table">
      <thead><tr><th>Поле</th><th>Ошибка</th><th className="num">Карточек</th><th className="num">Доля</th></tr></thead>
      <tbody>
        {rows.map((r) => (
          <tr key={`${r.field}-${r.kind}`}>
            <td>{r.label}</td>
            <td className="muted small nowrap">{KIND_LABEL[r.kind] ?? r.kind}</td>
            <td className="num">{r.count}</td>
            <td className="num">{formatPct(r.sharePct)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
