import { useEffect, useRef, useState } from 'react';
import { useConnectivity } from '../shared/api/connectivity';

/** Сколько висит сообщение «связь восстановлена». */
const RECOVERED_MS = 4000;

/**
 * Индикатор связи на рабочем месте обучающегося (ТЗ: обрыв до 30 с без потери
 * данных). Обучающийся должен видеть, что ввод не пропадает: пока связи нет,
 * данные лежат на устройстве и уйдут сами. Живой регион — чтобы сообщение
 * прочитала и программа экранного доступа.
 */
export function ConnectionBanner({ notice, onDismiss }: { notice?: string | null; onDismiss?: () => void }) {
  const offline = useConnectivity((s) => s.offline);
  const pending = useConnectivity((s) => s.pending);
  const [recovered, setRecovered] = useState(false);
  const wasOffline = useRef(offline);

  useEffect(() => {
    if (wasOffline.current && !offline) {
      setRecovered(true);
      const id = setTimeout(() => setRecovered(false), RECOVERED_MS);
      wasOffline.current = offline;
      return () => clearTimeout(id);
    }
    wasOffline.current = offline;
  }, [offline]);

  let text: string | null = null;
  let tone: 'warn' | 'ok' = 'warn';
  if (notice) text = notice;
  else if (offline) {
    text = `Нет связи с сервером — данные сохраняются на этом устройстве и будут отправлены автоматически${pending ? ` (в очереди: ${pending})` : ''}.`;
  } else if (recovered) {
    text = 'Связь восстановлена, данные отправлены.';
    tone = 'ok';
  }

  return (
    <div className="conn-banner-slot" role="status" aria-live="polite">
      {text && (
        <div className={`conn-banner conn-banner--${tone}`}>
          <span className="conn-banner__dot" aria-hidden="true" />
          <span>{text}</span>
          {notice && onDismiss && (
            <button type="button" className="conn-banner__close" onClick={onDismiss} aria-label="Скрыть сообщение">✕</button>
          )}
        </div>
      )}
    </div>
  );
}
