import type { AddressSource } from '../types';

/** Человекочитаемые названия источников адреса (Инструкция п.4.3). */
export const SOURCE_LABELS: Record<AddressSource, string> = {
  yandex_map: 'Яндекс.Карты',
  yandex_org: 'Яндекс.Организации',
  fias: 'ФИАС',
  map_pick: 'Указано на карте',
  manual: 'Введено вручную',
};
