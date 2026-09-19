import type { AddressDraft, AttributeValue, IncidentCardDraft } from '../types';

export function emptyAddress(): AddressDraft {
  return {
    raw: '', country: '', region: '', settlement: '', object: '',
    okrug: '', district: '', street: '', house: '', building: '',
    structure: '', apartment: '', entrance: '', floor: '', code: '',
    descriptive: '',
  };
}

export function emptyCard(): IncidentCardDraft {
  return {
    phones: {},
    applicant: {},
    address: emptyAddress(),
    incidentTypeIds: [],
    attributes: {},
    description: '',
    actionsTaken: '',
    flags: {
      victimsPresent: false,
      ambulanceRefusal: false,
      blocked: false,
      noContact: false,
      callDropped: false,
    },
    services: [],
  };
}

/** Адрес считается заполненным, когда выбран вариант из единой строки. */
export function isAddressFilled(a: AddressDraft): boolean {
  return a.raw.trim().length > 0 && a.street.trim().length > 0;
}

/** Адрес одной строкой — как в режиме просмотра карточки ДДС. */
export function formatAddress(a: AddressDraft): string {
  const head = [a.country, a.region].filter(Boolean).join(', ');
  const geo = [a.okrug, a.district].filter(Boolean).join(', ');
  const street = [a.street && `${a.street}`, a.house && `д. ${a.house}`, a.building && `корп. ${a.building}`]
    .filter(Boolean)
    .join(', ');
  const detail = [a.entrance && `под. ${a.entrance}`, a.floor && `эт. ${a.floor}`, a.apartment && `кв. ${a.apartment}`]
    .filter(Boolean)
    .join(', ');
  return [head, geo && `(${geo})`, street, detail].filter(Boolean).join(', ') || a.raw;
}

/** Значение признака как массив кодов — единый вид для сравнения и рендера. */
export function asCodes(value: AttributeValue): string[] {
  if (value == null) return [];
  if (Array.isArray(value)) return value;
  return [String(value)];
}

/** Читаемый путь поля карточки для ошибок и feedback преподавателя. */
export const FIELD_LABELS: Record<string, string> = {
  'applicant.name': 'ФИО заявителя',
  'applicant.status': 'Статус заявителя',
  'phones.aon': 'Телефон АОН',
  'phones.provided': 'Предоставленный номер',
  'address.raw': 'Адрес',
  'address.street': 'Улица',
  'address.house': 'Дом',
  'address.entrance': 'Подъезд',
  'address.floor': 'Этаж',
  incidentTypeIds: 'Тип происшествия',
  description: 'Описание со слов заявителя',
  actionsTaken: 'Действия оператора',
  'flags.victimsPresent': 'Пострадавшие',
  'flags.victimsCount': 'Количество пострадавших',
  'flags.ambulanceRefusal': 'Нет на месте / отказ от скорой',
  'flags.blocked': 'Нет доступа / заблокированные',
  'flags.noContact': 'Нет контакта с заявителем',
  'flags.callDropped': 'Срыв звонка',
  'applicant.foreignLanguage': 'Вызов на иностранном языке',
  'phones.onSite': 'Телефон на место',
  services: 'Службы',
};

/*
 * Запасной вариант, когда подписи нет: технический путь поля на экран не
 * выводим — пользователь прочитает «attributes.where» как мусор.
 */
export function fieldLabel(path: string): string {
  if (FIELD_LABELS[path]) return FIELD_LABELS[path];
  if (path.startsWith('attributes.')) return 'Признак опросной карты';
  return 'Поле карточки';
}

/** Достаёт значение по пути «a.b.c» — для сверки карточки с эталоном. */
export function getPath(obj: unknown, path: string): unknown {
  return path.split('.').reduce<unknown>((acc, key) => {
    if (acc && typeof acc === 'object') return (acc as Record<string, unknown>)[key];
    return undefined;
  }, obj);
}
