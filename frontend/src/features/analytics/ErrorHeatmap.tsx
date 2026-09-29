import { useState } from 'react';
import type { AnalyticsOverview } from '../../shared/types';

type Heatmap = AnalyticsOverview['heatmap'];

/**
 * Тепловая карта ошибок: строки — категории происшествий, столбцы — поля
 * карточки, в ячейке — число карточек с ошибкой в поле.
 *
 * Цвет — последовательная шкала одного оттенка (светлее — меньше), считается
 * от доли карточек категории, а не от сырого числа: иначе частая категория
 * всегда выглядела бы «хуже» редкой. Сделана таблицей, а не рисунком: у
 * программы экранного доступа есть заголовки строк и столбцов, каждое число
 * написано в ячейке, подсказка дублирует его долей — цвет ничего не несёт один.
 */
const STEPS = 7;

interface Tip {
  row: string;
  col: string;
  count: number;
  share: number;
  attempts: number;
  x: number;
  y: number;
}

export function ErrorHeatmap({ heatmap }: { heatmap: Heatmap }) {
  const [tip, setTip] = useState<Tip | null>(null);
  const { rows, cols, cells } = heatmap;

  if (rows.length === 0 || cols.length === 0) {
    return <p className="muted small">Ошибок в полях карточки за период нет — карта пуста.</p>;
  }

  const shares = rows.map((r, i) => cols.map((_, j) => (r.attempts ? cells[i]?.[j] ?? 0 : 0) / Math.max(1, r.attempts)));
  const maxShare = Math.max(0.01, ...shares.flat());
  const step = (share: number) => (share <= 0 ? 0 : Math.max(1, Math.ceil((share / maxShare) * STEPS)));

  function show(e: React.PointerEvent | React.FocusEvent, i: number, j: number) {
    const cell = e.currentTarget as HTMLElement;
    const wrap = cell.closest('.heatmap') as HTMLElement | null;
    if (!wrap) return;
    const a = cell.getBoundingClientRect();
    const b = wrap.getBoundingClientRect();
    setTip({
      row: rows[i].label,
      col: cols[j].label,
      count: cells[i]?.[j] ?? 0,
      share: Math.round(shares[i][j] * 100),
      attempts: rows[i].attempts,
      x: a.left - b.left + wrap.scrollLeft + a.width / 2,
      y: a.top - b.top,
    });
  }

  return (
    <div className="heatmap" onPointerLeave={() => setTip(null)}>
      <table className="heatmap__table">
        <caption className="sr-only">
          Число карточек с ошибкой по категориям происшествий (строки) и полям карточки (столбцы)
        </caption>
        <thead>
          <tr>
            <th scope="col" className="heatmap__corner">Категория · карточек</th>
            {cols.map((c) => (
              <th key={c.id} scope="col" className="heatmap__col"><span>{c.label}</span></th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((r, i) => (
            <tr key={r.id}>
              <th scope="row" className="heatmap__row">
                {r.label} <span className="dim num">· {r.attempts}</span>
              </th>
              {cols.map((c, j) => {
                const count = cells[i]?.[j] ?? 0;
                const level = step(shares[i][j]);
                return (
                  <td
                    key={c.id}
                    className={`heatmap__cell heatmap__cell--${level}`}
                    tabIndex={0}
                    aria-label={`${r.label}, ${c.label}: ${count} из ${r.attempts} карточек`}
                    onPointerEnter={(e) => show(e, i, j)}
                    onFocus={(e) => show(e, i, j)}
                    onBlur={() => setTip(null)}
                  >
                    {count > 0 ? count : ''}
                  </td>
                );
              })}
            </tr>
          ))}
        </tbody>
      </table>

      {tip && (
        <div className="viz-tooltip" style={{ left: tip.x, top: Math.max(0, tip.y - 58) }} role="status">
          <div className="viz-tooltip__title">{tip.row} · {tip.col}</div>
          <div><b>{tip.count}</b> <span className="dim">из {tip.attempts} карточек · {tip.share}%</span></div>
        </div>
      )}

      <div className="heatmap__legend" aria-hidden="true">
        <span className="dim small">меньше ошибок</span>
        {Array.from({ length: STEPS + 1 }, (_, k) => <span key={k} className={`heatmap__swatch heatmap__cell--${k}`} />)}
        <span className="dim small">больше (до {Math.round(maxShare * 100)}% карточек категории)</span>
      </div>
    </div>
  );
}
