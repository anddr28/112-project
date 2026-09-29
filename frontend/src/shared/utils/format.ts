/** Число по-русски: десятичная запятая, до одного знака после неё («81,4»). */
export function formatNumber(value: number, digits = 1): string {
  return value.toLocaleString('ru-RU', { maximumFractionDigits: digits });
}

/** Доля в процентах: «93,1%». */
export function formatPct(value: number): string {
  return `${formatNumber(value)}%`;
}
