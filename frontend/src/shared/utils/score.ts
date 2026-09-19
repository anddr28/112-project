/** Цветовая группа балла относительно порога зачёта. */
export type ScoreTone = 'ok' | 'warn' | 'danger';

export function scoreTone(value: number, threshold = 70): ScoreTone {
  if (value >= threshold) return 'ok';
  if (value >= threshold - 20) return 'warn';
  return 'danger';
}
