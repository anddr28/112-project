/**
 * Справочник служб.
 *
 * TODO(backend) GAP-08: сид в `00001_init.sql` использует коды «01».. «04»,
 * а реальный АРМ-112 (СКРИНШОТ КАРТОЧКИ 112ГСИ.docx) — «101».. «104» плюс
 * городские ДДС. Здесь взяты реальные коды; при появлении `GET /services`
 * этот файл удаляется целиком.
 */

import type { ServiceRef } from '../../types';

export const SERVICES: ServiceRef[] = [
  { id: 'svc-101', code: '101', shortName: 'Служба 101', name: 'ГУ МЧС России по г. Москве, ГКУ «Пожарно-спасательный центр» ОДС', kind: 'emergency' },
  { id: 'svc-102', code: '102', shortName: 'Служба 102', name: 'Полиция (СОДЧ МВД)', kind: 'emergency' },
  { id: 'svc-103', code: '103', shortName: 'Служба 103', name: 'ГБУ города Москвы «Станция скорой и неотложной медицинской помощи им. А.С. Пучкова»', kind: 'emergency' },
  { id: 'svc-104', code: '104', shortName: 'Служба 104', name: 'АО «МОСГАЗ», Диспетчерское управление', kind: 'emergency' },
  { id: 'svc-cemp', code: 'cemp', shortName: 'ЦЭМП', name: 'Центр экстренной медицинской помощи', kind: 'city' },
  { id: 'svc-zhkh', code: 'zhkh', shortName: 'Деп. ЖКХ', name: 'Департамент ЖКХ / управляющие организации', kind: 'city' },
  { id: 'svc-codd', code: 'codd', shortName: 'ЦОДД', name: 'Центр организации дорожного движения', kind: 'city' },
  { id: 'svc-mosbez', code: 'mosbez', shortName: 'Мос.Без.', name: 'ГКУ «Московский городской центр безопасности»', kind: 'city' },
  { id: 'svc-moslift', code: 'moslift', shortName: 'Мослифт', name: 'АО «Мослифт»', kind: 'city' },
  { id: 'svc-vodokanal', code: 'vodokanal', shortName: 'Мосводоканал', name: 'АО «Мосводоканал»', kind: 'city' },
  { id: 'svc-mosvodostok', code: 'mosvodostok', shortName: 'Мосводосток', name: 'ГУП «Мосводосток»', kind: 'city' },
  { id: 'svc-gormost', code: 'gormost', shortName: 'Гормост', name: 'ГБУ «Гормост»', kind: 'city' },
  { id: 'svc-oek', code: 'oek', shortName: 'ОЭК', name: 'АО «ОЭК» (электросети)', kind: 'city' },
  { id: 'svc-oati', code: 'oati', shortName: 'ОАТИ', name: 'Объединение административно-технических инспекций', kind: 'city' },
  { id: 'svc-fsb', code: 'fsb', shortName: 'ФСБ', name: 'Федеральная служба безопасности', kind: 'city' },
];

export function serviceByCode(code: string): ServiceRef | undefined {
  return SERVICES.find((s) => s.code === code);
}
