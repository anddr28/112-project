import { useLayoutEffect, useRef, useState } from 'react';
import type { AnalyticsOverview } from '../../shared/types';
import { formatNumber } from '../../shared/utils/format';

type Point = AnalyticsOverview['trend'][number];

/**
 * Динамика группы по дням: средний балл и доля зачётов.
 *
 * Обе величины — на шкале 0–100 (баллы и проценты), поэтому ось одна:
 * вторая ось исказила бы сравнение. Линии 2 px, маркеры 8 px с кольцом цвета
 * фона, перекрестие по ближайшему дню и подсказка со всеми рядами; значения
 * дублирует таблица под графиком — подсказка ничего не прячет.
 */
const SERIES = [
  { key: 'avgScore' as const, label: 'Средний балл', color: 'var(--viz-1)' },
  { key: 'passRatePct' as const, label: 'Доля зачётов, %', color: 'var(--viz-2)' },
];

const HEIGHT = 220;
const PAD = { top: 12, right: 44, bottom: 28, left: 34 };
const TICKS = [0, 25, 50, 75, 100];

function shortDate(iso: string): string {
  const [, m, d] = iso.split('-');
  return `${d}.${m}`;
}

export function TrendChart({ data }: { data: Point[] }) {
  const box = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(600);
  const [hover, setHover] = useState<number | null>(null);
  const [showTable, setShowTable] = useState(false);

  useLayoutEffect(() => {
    const el = box.current;
    if (!el) return;
    const ro = new ResizeObserver(([entry]) => setWidth(Math.max(280, Math.floor(entry.contentRect.width))));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  if (data.length === 0) return <p className="muted small">Нет оценённых карточек за выбранный период.</p>;

  const plotW = width - PAD.left - PAD.right;
  const plotH = HEIGHT - PAD.top - PAD.bottom;
  const x = (i: number) => PAD.left + (data.length === 1 ? plotW / 2 : (i / (data.length - 1)) * plotW);
  const y = (v: number) => PAD.top + plotH - (Math.max(0, Math.min(100, v)) / 100) * plotH;
  // Подписи дат не должны налезать друг на друга: не чаще одной на ~56 px.
  const labelEvery = Math.max(1, Math.ceil(data.length / Math.max(1, Math.floor(plotW / 56))));

  function nearest(clientX: number, rect: DOMRect): number {
    const px = ((clientX - rect.left) / rect.width) * width;
    if (data.length === 1) return 0;
    return Math.round(Math.max(0, Math.min(1, (px - PAD.left) / plotW)) * (data.length - 1));
  }

  const h = hover != null ? data[hover] : null;
  const hx = hover != null ? x(hover) : 0;
  const last = data.length - 1;

  return (
    <div className="viz">
      <div className="viz-legend" aria-hidden="true">
        {SERIES.map((s) => (
          <span key={s.key} className="viz-legend__item">
            <span className="viz-legend__line" style={{ background: s.color }} />
            {s.label}
          </span>
        ))}
      </div>

      <div ref={box} className="viz-plot" style={{ height: HEIGHT }}>
        <svg
          width={width}
          height={HEIGHT}
          viewBox={`0 0 ${width} ${HEIGHT}`}
          role="img"
          aria-label={`Динамика за ${data.length} дн.: средний балл от ${data[0].avgScore} до ${data[last].avgScore}, доля зачётов от ${data[0].passRatePct}% до ${data[last].passRatePct}%`}
          tabIndex={0}
          onPointerMove={(e) => setHover(nearest(e.clientX, e.currentTarget.getBoundingClientRect()))}
          onPointerLeave={() => setHover(null)}
          onFocus={() => setHover(last)}
          onBlur={() => setHover(null)}
          onKeyDown={(e) => {
            if (e.key === 'ArrowLeft') setHover((v) => Math.max(0, (v ?? last) - 1));
            if (e.key === 'ArrowRight') setHover((v) => Math.min(last, (v ?? 0) + 1));
          }}
        >
          {TICKS.map((t) => (
            <g key={t}>
              <line x1={PAD.left} x2={width - PAD.right} y1={y(t)} y2={y(t)} className={t === 0 ? 'viz-axis' : 'viz-grid'} />
              <text x={PAD.left - 6} y={y(t)} className="viz-tick" textAnchor="end" dominantBaseline="middle">{t}</text>
            </g>
          ))}
          {/* Регулярная подпись у самого края уступает место подписи последнего дня — иначе они слипаются. */}
          {data.map((d, i) =>
            i === last || (i % labelEvery === 0 && last - i >= labelEvery) ? (
              <text key={d.date} x={x(i)} y={HEIGHT - 8} className="viz-tick" textAnchor="middle">{shortDate(d.date)}</text>
            ) : null,
          )}

          {h && <line x1={hx} x2={hx} y1={PAD.top} y2={PAD.top + plotH} className="viz-crosshair" />}

          {SERIES.map((s) => (
            <g key={s.key}>
              <polyline
                points={data.map((d, i) => `${x(i)},${y(d[s.key])}`).join(' ')}
                fill="none"
                stroke={s.color}
                strokeWidth={2}
                strokeLinejoin="round"
                strokeLinecap="round"
              />
              {/* Маркер — только у последней точки и у точки под курсором: число на каждой точке нечитаемо. */}
              {[last, ...(hover != null && hover !== last ? [hover] : [])].map((i) => (
                <circle key={i} cx={x(i)} cy={y(data[i][s.key])} r={4} fill={s.color} className="viz-dot" />
              ))}
              <text x={x(last) + 8} y={y(data[last][s.key])} className="viz-end-label" dominantBaseline="middle">
                {Math.round(data[last][s.key])}
              </text>
            </g>
          ))}
        </svg>

        {h && (
          <div
            className="viz-tooltip viz-tooltip--side"
            // Сбоку от перекрестия, а не поверх него: подсказка не закрывает точку, которую описывает.
            style={hx < width / 2 ? { left: hx + 12, top: 4 } : { right: width - hx + 12, top: 4 }}
            role="status"
          >
            <div className="viz-tooltip__title">{new Date(h.date).toLocaleDateString('ru-RU')} · карточек: {h.attempts}</div>
            {SERIES.map((s) => (
              <div key={s.key} className="viz-tooltip__row">
                <span className="viz-legend__line" style={{ background: s.color }} />
                <b>{formatNumber(h[s.key])}</b>
                <span className="dim">{s.label}</span>
              </div>
            ))}
          </div>
        )}
      </div>

      <button type="button" className="btn btn--ghost btn--sm" onClick={() => setShowTable((v) => !v)} aria-expanded={showTable}>
        {showTable ? 'Скрыть таблицу' : 'Показать таблицей'}
      </button>
      {showTable && (
        <div className="table-scroll">
          <table className="table">
            <thead><tr><th>Дата</th><th>Карточек</th><th>Средний балл</th><th>Зачёт, %</th></tr></thead>
            <tbody>
              {data.map((d) => (
                <tr key={d.date}>
                  <td className="nowrap">{new Date(d.date).toLocaleDateString('ru-RU')}</td>
                  <td className="num">{d.attempts}</td>
                  <td className="num">{formatNumber(d.avgScore)}</td>
                  <td className="num">{formatNumber(d.passRatePct)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
