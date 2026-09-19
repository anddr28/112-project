/**
 * Mock адресных подсказок.
 *
 * TODO(backend) C-01 / C-02 / GAP-05: endpoint'ов подсказок и резолва нет,
 * контрактный `Address` беднее реального (8 полей против 15 + координаты).
 * Инструкция п.4.3: варианты приходят из Яндекс.Карт, Яндекс.Организаций и
 * ФИАС; для ФИАС службы автоматически НЕ подставляются — это правило здесь
 * воспроизведено, чтобы студент видел реальное поведение.
 */

import type { AddressSuggestion } from '../../types';
import { emptyAddress } from '../../utils/card';

interface Seed {
  label: string;
  source: AddressSuggestion['source'];
  okrug: string;
  district: string;
  street: string;
  house: string;
  building?: string;
  lat: number;
  lon: number;
}

const SEEDS: Seed[] = [
  { label: 'Москва, Тверская улица, 12', source: 'yandex_map', okrug: 'ЦАО', district: 'Тверской', street: 'Тверская', house: '12', lat: 55.7616, lon: 37.6065 },
  { label: 'Москва, Тверская улица, 12, строение 2', source: 'yandex_org', okrug: 'ЦАО', district: 'Тверской', street: 'Тверская', house: '12', building: '2', lat: 55.7619, lon: 37.6071 },
  { label: 'Москва, Тверская улица, 21', source: 'yandex_map', okrug: 'ЦАО', district: 'Тверской', street: 'Тверская', house: '21', lat: 55.7657, lon: 37.5998 },
  { label: 'Москва, Профсоюзная улица, 45, корпус 2', source: 'yandex_map', okrug: 'ЮЗАО', district: 'Черёмушки', street: 'Профсоюзная', house: '45', building: '2', lat: 55.6771, lon: 37.5613 },
  { label: 'Москва, Профсоюзная улица, 45', source: 'fias', okrug: 'ЮЗАО', district: 'Черёмушки', street: 'Профсоюзная', house: '45', lat: 55.6768, lon: 37.5607 },
  { label: 'Москва, Чертановская улица, 58, корпус 2', source: 'yandex_map', okrug: 'ЮАО', district: 'Чертаново Южное', street: 'Чертановская', house: '58', building: '2', lat: 55.6023, lon: 37.6031 },
  { label: 'Москва, Чертановская улица, 58', source: 'fias', okrug: 'ЮАО', district: 'Чертаново Южное', street: 'Чертановская', house: '58', lat: 55.6021, lon: 37.6028 },
  { label: 'Москва, Ленинский проспект, 32', source: 'yandex_map', okrug: 'ЮЗАО', district: 'Гагаринский', street: 'Ленинский проспект', house: '32', lat: 55.7043, lon: 37.5716 },
  { label: 'Москва, Ленинский проспект, 32А', source: 'yandex_org', okrug: 'ЮЗАО', district: 'Гагаринский', street: 'Ленинский проспект', house: '32А', lat: 55.7047, lon: 37.5721 },
  { label: 'Москва, Охотный Ряд, 2', source: 'yandex_org', okrug: 'ЦАО', district: 'Тверской', street: 'Охотный Ряд', house: '2', lat: 55.7570, lon: 37.6156 },
  { label: 'Москва, Никольская улица, 10', source: 'yandex_map', okrug: 'ЦАО', district: 'Тверской', street: 'Никольская', house: '10', lat: 55.7556, lon: 37.6224 },
  { label: 'Москва, Садовая-Кудринская улица, 7', source: 'yandex_map', okrug: 'ЦАО', district: 'Пресненский', street: 'Садовая-Кудринская', house: '7', lat: 55.7612, lon: 37.5830 },
  { label: 'Москва, Ярославское шоссе, 116', source: 'yandex_map', okrug: 'СВАО', district: 'Ярославский', street: 'Ярославское шоссе', house: '116', lat: 55.8676, lon: 37.7085 },
  { label: 'Москва, Каширское шоссе, 26', source: 'yandex_map', okrug: 'ЮАО', district: 'Нагатино-Садовники', street: 'Каширское шоссе', house: '26', lat: 55.6486, lon: 37.6285 },
];

/** Нормализация для поиска: регистр, «ё» и знаки препинания не должны мешать. */
function norm(value: string): string {
  return value.toLowerCase().replace(/ё/g, 'е').replace(/[.,]/g, ' ');
}

/**
 * Поиск по словам, а не по подстроке.
 *
 * Инструкция п.4.3 требует вводить адрес «одной строкой с домом включительно»,
 * то есть «Тверская 12». Сравнение строки целиком такой запрос не находит:
 * в названии между улицей и домом стоит слово «улица».
 */
export function suggestAddresses(query: string): AddressSuggestion[] {
  const q = norm(query).trim();
  if (q.length < 3) return [];

  const words = q.split(/\s+/).filter(Boolean);

  return SEEDS.filter((seed) => {
    const label = norm(seed.label);
    return words.every((w) => label.split(/\s+/).some((part) => part.startsWith(w)));
  })
    .slice(0, 7)
    .map((seed, index) => ({
      id: `addr-${index}-${seed.street}-${seed.house}-${seed.source}`,
      label: seed.label,
      source: seed.source,
      value: {
        ...emptyAddress(),
        raw: seed.label,
        country: 'Россия',
        region: 'Москва',
        settlement: 'Москва',
        okrug: seed.okrug,
        district: seed.district,
        street: seed.street,
        house: seed.house,
        building: seed.building ?? '',
        lat: seed.lat,
        lon: seed.lon,
        source: seed.source,
      },
    }));
}
