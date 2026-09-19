/**
 * Фоновая иллюстрация экрана входа.
 *
 * Воспроизводит композицию реального экрана ПОВ-112 (mockups/pov112-login.png):
 * плоский векторный силуэт делового центра на градиентном небе, вертолёт,
 * низкая застройка по нижнему краю. Оригинального ассета в проекте нет,
 * поэтому геометрия построена заново.
 *
 * Правила построения, чтобы это читалось как часть интерфейса, а не случайная
 * картинка:
 *   — три плана по глубине, каждый заметно темнее предыдущего;
 *   — источник света слева: левая грань башни светлее, правая грань темнее;
 *   — все башни стоят на одной линии земли;
 *   — плотность застройки падает вправо: за формой остаётся открытое небо,
 *     как на референсе, и текст не ложится на пёстрый силуэт;
 *   — окна низкой застройки выстроены по общему модулю.
 *
 * Анимации нет. aria-hidden: смысловой нагрузки не несёт.
 */

interface Tower {
  x: number;
  w: number;
  top: number;
  /** ступени: расширенные блоки сверху вниз, как у башен делового центра */
  steps?: Array<{ dy: number; inset: number }>;
}

const GROUND = 652;

/** Дальний план: светлый, почти сливается с небом. */
const FAR: Tower[] = [
  { x: 10, w: 74, top: 96 },
  { x: 148, w: 48, top: 190 },
  { x: 296, w: 60, top: 150 },
  { x: 462, w: 54, top: 208 },
  { x: 628, w: 66, top: 132 },
  { x: 796, w: 50, top: 214 },
  { x: 946, w: 46, top: 236 },
];

/** Средний план. */
const MID: Tower[] = [
  { x: 84, w: 58, top: 230, steps: [{ dy: 98, inset: 7 }] },
  { x: 232, w: 70, top: 254, steps: [{ dy: 86, inset: 9 }, { dy: 172, inset: 18 }] },
  { x: 388, w: 56, top: 224 },
  { x: 542, w: 64, top: 278, steps: [{ dy: 94, inset: 8 }] },
  { x: 706, w: 58, top: 246 },
  { x: 858, w: 62, top: 292, steps: [{ dy: 92, inset: 9 }] },
  { x: 1008, w: 48, top: 320 },
];

/** Передний план: самые тёмные и выразительные объёмы, все левее формы. */
const NEAR: Tower[] = [
  { x: 156, w: 82, top: 300, steps: [{ dy: 106, inset: 11 }, { dy: 212, inset: 22 }] },
  { x: 318, w: 68, top: 342 },
  { x: 452, w: 90, top: 276, steps: [{ dy: 114, inset: 12 }, { dy: 228, inset: 24 }] },
  { x: 622, w: 72, top: 350 },
  { x: 772, w: 84, top: 318, steps: [{ dy: 110, inset: 11 }, { dy: 220, inset: 22 }] },
];

/** Ширина правой (теневой) грани: свет падает слева. */
const SIDE = 12;

interface Palette {
  face: string;
  side: string;
  top: string;
}

function TowerShape({ tower, p }: { tower: Tower; p: Palette }) {
  const { x, w, top: y, steps = [] } = tower;
  const bodyW = w - SIDE;

  return (
    <g>
      <rect x={x} y={y} width={bodyW} height={GROUND - y} fill={p.face} />
      <rect x={x + bodyW} y={y} width={SIDE} height={GROUND - y} fill={p.side} />
      <rect x={x} y={y} width={w} height={5} fill={p.top} />

      {steps.map((step, i) => {
        const sy = y + step.dy;
        const sx = x - step.inset;
        const sw = w + step.inset * 2;
        return (
          <g key={i}>
            <rect x={sx} y={sy} width={sw - SIDE} height={GROUND - sy} fill={p.face} />
            <rect x={sx + sw - SIDE} y={sy} width={SIDE} height={GROUND - sy} fill={p.side} />
            <rect x={sx} y={sy} width={sw} height={5} fill={p.top} />
          </g>
        );
      })}
    </g>
  );
}

/** Низкая застройка переднего края: светлые объёмы с оконными строками. */
function LowRise({ x, w, h }: { x: number; w: number; h: number }) {
  const y = 760 - h;
  const bodyW = w - 9;
  const cols = Math.max(2, Math.floor((bodyW - 20) / 34));
  const rows = Math.max(1, Math.floor((h - 34) / 28));

  return (
    <g>
      <rect x={x} y={y} width={bodyW} height={h} fill="#f5f7f9" />
      <rect x={x + bodyW} y={y} width={9} height={h} fill="#e2e8ed" />
      {Array.from({ length: rows }).map((_, r) =>
        Array.from({ length: cols }).map((_, c) => (
          <rect
            key={`${r}-${c}`}
            x={x + 14 + c * 34}
            y={y + 20 + r * 28}
            width={20}
            height={5}
            fill="#d3dce2"
          />
        )),
      )}
    </g>
  );
}

export function CitySkyline() {
  return (
    <svg
      className="login__art"
      viewBox="0 0 1200 760"
      preserveAspectRatio="xMidYMax slice"
      aria-hidden="true"
      focusable="false"
    >
      <defs>
        <linearGradient id="sky" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor="var(--login-sky-top)" />
          <stop offset="55%" stopColor="var(--login-sky-mid)" />
          <stop offset="100%" stopColor="var(--login-sky-low)" />
        </linearGradient>
      </defs>

      <rect width="1200" height="760" fill="url(#sky)" />

      {FAR.map((t) => (
        <TowerShape key={`f-${t.x}`} tower={t} p={{ face: '#aed0e7', side: '#97c0dd', top: '#c0dcee' }} />
      ))}

      {MID.map((t) => (
        <TowerShape key={`m-${t.x}`} tower={t} p={{ face: '#8cb9d9', side: '#6d9fc4', top: '#a3cae4' }} />
      ))}

      {NEAR.map((t) => (
        <TowerShape key={`n-${t.x}`} tower={t} p={{ face: '#6a9fc7', side: '#4c82ae', top: '#84b3d5' }} />
      ))}

      <g>
        <LowRise x={-12} w={196} h={128} />
        <LowRise x={178} w={154} h={100} />
        <LowRise x={326} w={214} h={142} />
        <LowRise x={534} w={166} h={106} />
        <LowRise x={694} w={184} h={134} />
        <LowRise x={872} w={152} h={98} />
        <LowRise x={1018} w={200} h={138} />
      </g>

      {/* вертолёт: тот же элемент композиции, что и на референсе */}
      <g transform="translate(408 138) scale(1.5)">
        {/* несущий винт */}
        <rect x="-16" y="0" width="92" height="3" rx="1.5" fill="#5f6b74" />
        <rect x="28" y="3" width="4" height="9" fill="#93a0a9" />
        {/* корпус и хвостовая балка */}
        <ellipse cx="30" cy="22" rx="30" ry="13" fill="#ffffff" />
        <path d="M56 17 L92 13 L92 22 L58 26 Z" fill="#ffffff" />
        {/* киль */}
        <path d="M86 13 L96 2 L98 4 L92 14 Z" fill="#ffffff" />
        <circle cx="94" cy="9" r="4" fill="none" stroke="#cdd6dc" strokeWidth="2" />
        {/* остекление кабины */}
        <path d="M4 22 Q6 13 18 12 L18 24 Z" fill="#8fc3e4" />
        {/* полоса и шасси */}
        <rect x="2" y="29" width="56" height="4" rx="2" fill="#e8642a" />
        <rect x="16" y="33" width="3" height="9" fill="#93a0a9" />
        <rect x="40" y="33" width="3" height="9" fill="#93a0a9" />
        <rect x="10" y="41" width="40" height="3.5" rx="1.75" fill="#cdd6dc" />
      </g>
    </svg>
  );
}
