/** Формат MM:SS — как таймер в правом верхнем углу реального АРМ-112. */
export function formatClock(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  const mm = Math.floor(total / 60);
  const ss = total % 60;
  return `${String(mm).padStart(2, '0')}:${String(ss).padStart(2, '0')}`;
}

/** «1 мин 23 с» — для отчётов, где важна читаемость, а не моноширинность. */
export function formatDuration(ms: number): string {
  const total = Math.max(0, Math.round(ms / 1000));
  if (total < 60) return `${total} с`;
  const mm = Math.floor(total / 60);
  const ss = total % 60;
  return ss === 0 ? `${mm} мин` : `${mm} мин ${ss} с`;
}

/** Знаковая дельта относительно норматива: «+1 мин 12 с» / «−4 с». */
export function formatDelta(ms: number): string {
  const sign = ms >= 0 ? '+' : '−';
  return `${sign}${formatDuration(Math.abs(ms))}`;
}

/** ЧЧ:ММ:СС — формат времени статуса в истории реагирования. */
export function formatTime(iso: string): string {
  return new Date(iso).toLocaleTimeString('ru-RU', { hour12: false });
}

/** ДД.ММ.ГГГГ ЧЧ:ММ:СС — как в панели истории статусов АРМ. */
export function formatDateTime(iso: string): string {
  const d = new Date(iso);
  return `${d.toLocaleDateString('ru-RU')} ${d.toLocaleTimeString('ru-RU', { hour12: false })}`;
}

export function formatDate(iso: string): string {
  return new Date(iso).toLocaleDateString('ru-RU', {
    day: '2-digit',
    month: 'long',
    year: 'numeric',
  });
}

