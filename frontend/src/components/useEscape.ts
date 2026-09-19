import { useEffect } from 'react';

/**
 * Esc закрывает окно — привычное поведение рабочего места оператора.
 *
 * Отдельный файл, а не экспорт из `ui.tsx`: модуль с компонентами не должен
 * экспортировать ничего, кроме компонентов, иначе ломается горячая замена.
 */
export function useEscape(onClose: () => void) {
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape') onClose();
    }
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [onClose]);
}
