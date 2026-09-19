import type { ClassifierLabels } from '../api';

/**
 * Перевод технических кодов в подписи для интерфейса.
 *
 * Пути полей карточки и коды признаков не должны попадать на экран как есть:
 * «attributes.where» читается пользователем как мусор. Словарь подписей
 * приходит из классификатора через сервисный слой.
 */

const EMPTY: ClassifierLabels = { fields: {}, attributes: {}, values: {}, types: {} };

/**
 * Значение без подписи в классификаторе.
 *
 * Числа и русский текст показываем как есть, а латинский код — нет:
 * это внутренний идентификатор, пользователю он ничего не говорит.
 */
function plain(value: unknown): string {
  const text = String(value);
  return /^[a-z0-9_.-]+$/i.test(text) && /[a-z]/i.test(text) ? 'значение' : text;
}

/** Подпись поля карточки по его пути. */
export function labelForPath(path: string, labels: ClassifierLabels = EMPTY): string {
  if (labels.fields[path]) return labels.fields[path];

  if (path.startsWith('attributes.')) {
    const code = path.slice('attributes.'.length);
    return labels.attributes[code] ?? 'Признак опросной карты';
  }

  // Неизвестный путь: показываем нейтральную подпись, а не техническую строку.
  return 'Поле карточки';
}

/** Значение поля в читаемом виде: коды признаков разворачиваются в подписи. */
export function labelForValue(path: string, value: unknown, labels: ClassifierLabels = EMPTY): string {
  if (value == null || value === '') return '—';
  if (typeof value === 'boolean') return value ? 'да' : 'нет';

  const asList = Array.isArray(value) ? value : [value];

  // Типы происшествия хранятся идентификаторами.
  if (path === 'incidentTypeIds') {
    return asList.map((v) => labels.types[String(v)] ?? 'тип происшествия').join(', ');
  }

  if (path.startsWith('attributes.')) {
    const code = path.slice('attributes.'.length);
    const map = labels.values[code] ?? {};
    return asList.map((v) => map[String(v)] ?? plain(v)).join(', ');
  }

  return asList.map((v) => String(v)).join(', ');
}
