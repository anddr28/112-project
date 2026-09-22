import { useCallback, useEffect, useRef, useState } from 'react';

export interface AsyncState<T> {
  data: T | null;
  /** первая загрузка: данных ещё нет и показывать нечего */
  loading: boolean;
  /** повторная загрузка поверх уже показанных данных */
  refreshing: boolean;
  error: string | null;
  reload: () => void;
}

/**
 * Минимальная загрузка данных без внешних библиотек: состояние + защита от
 * гонок при быстрой смене зависимостей. Хватает для прототипа; при переходе
 * на реальный API заменяется на react-query без правки компонентов.
 */
export function useAsync<T>(fn: () => Promise<T>, deps: unknown[]): AsyncState<T> {
  const [data, setData] = useState<T | null>(null);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [nonce, setNonce] = useState(0);
  const fnRef = useRef(fn);
  fnRef.current = fn;
  /** уже показанные данные: по ним отличаем первую загрузку от обновления */
  const dataRef = useRef<T | null>(null);

  useEffect(() => {
    let cancelled = false;
    /*
     * Периодическое обновление не должно прятать уже показанный экран:
     * иначе страница мониторинга подменяется заглушкой загрузки каждые
     * несколько секунд — она мигает, теряет фокус ввода и положение прокрутки.
     */
    const first = dataRef.current == null;
    setLoading(first);
    setRefreshing(!first);
    setError(null);

    // Promise.resolve().then(...) ловит и синхронный throw: часть методов
    // сервисного слоя выбрасывают ошибку до создания промиса, и без этого
    // исключение уходило мимо catch и роняло экран.
    Promise.resolve()
      .then(() => fnRef.current())
      .then((result) => {
        if (cancelled) return;
        dataRef.current = result;
        setData(result);
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(e instanceof Error ? e.message : 'Не удалось загрузить данные');
      })
      .finally(() => {
        if (cancelled) return;
        setLoading(false);
        setRefreshing(false);
      });

    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, nonce]);

  const reload = useCallback(() => setNonce((n) => n + 1), []);

  return { data, loading, refreshing, error, reload };
}
