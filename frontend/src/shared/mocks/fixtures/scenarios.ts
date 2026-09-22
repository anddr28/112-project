/**
 * Учебные сценарии: легенда звонка + эталон карточки.
 *
 * `callScript` следует контрактной схеме `_components.yaml#/CallScript`
 * (caller / address / keyFacts / turns[speaker,text,ttsHash]).
 * `keyFacts` студенту НЕ отдаётся — см. GAP-11 и mock-метод callScript.get().
 */

import type { IncidentCardDraft, Scenario } from '../../types';
import { emptyAddress, emptyCard } from '../../utils/card';
import { dialogueFixture } from './dialogue';

function etalon(partial: Partial<IncidentCardDraft>): IncidentCardDraft {
  return { ...emptyCard(), ...partial };
}

export const SCENARIOS: Scenario[] = [
  {
    id: 'sc-fire-apartment',
    title: 'Пожар в квартире многоквартирного дома',
    categoryId: 'it-101',
    categoryName: 'Происшествие 101',
    difficulty: 2,
    mode: 'cards',
    source: 'generated',
    status: 'validated',
    etalonVersion: 1,
    expectedDialogue: dialogueFixture('sc-fire-apartment').expected,
    ttsReady: true,
    createdAt: '2026-09-15T09:12:00+03:00',
    validatedBy: 'Ковалёва И. С.',
    validatedAt: '2026-09-15T10:40:00+03:00',
    notesForTeacher:
      'Проверить, что студент зафиксировал угрозу людям и газовую колонку — от этого зависит состав служб.',
    callScript: {
      dialogue: dialogueFixture('sc-fire-apartment').brief,
      caller: { name: 'Мария', phone: '+7 (903) 511-33-67', role: 'соседка', emotionalState: 'паника' },
      address: { raw: 'Москва, Тверская улица, 12, подъезд 3', city: 'Москва', street: 'Тверская', house: '12', entrance: '3' },
      keyFacts: [
        'дым из квартиры на 5 этаже',
        'подъезд 3',
        'в квартире может находиться пожилой человек',
        'в доме газовые колонки',
      ],
      turns: [
        { speaker: 'caller', text: 'Алло! Помогите, у нас дым идёт из квартиры! Тверская, дом двенадцать, третий подъезд!' },
        { speaker: 'operator_hint', text: 'Уточните этаж и что именно горит.' },
        { speaker: 'caller', text: 'Пятый этаж, квартира напротив моей. Из-под двери валит чёрный дым, на площадке уже не продохнуть.' },
        { speaker: 'caller', text: 'Там бабушка живёт, одна. Я стучу — не открывает. Кажется, она дома, утром её видела.' },
        { speaker: 'operator_hint', text: 'Уточните, газифицирован ли дом.' },
        { speaker: 'caller', text: 'Да, у нас газовые колонки во всех квартирах. Дом старый, пятиэтажка.' },
        { speaker: 'caller', text: 'Пожалуйста, быстрее! Соседи уже выходят на улицу!' },
      ],
    },
    etalonCard: {
      categoryCode: '101',
      address: { raw: 'Москва, Тверская улица, 12', city: 'Москва', street: 'Тверская', house: '12', entrance: '3', floor: '5' },
      applicant: { name: 'Мария', phone: '+7 (903) 511-33-67' },
      casualties: { trapped: 1 },
      servicesToNotify: ['101', '103', '104', 'zhkh', 'cemp'],
      description:
        'Задымление в квартире на 5 этаже многоквартирного дома, подъезд 3. В квартире предположительно находится пожилой человек, дверь не открывает. Дом газифицирован.',
    },
    etalonDraft: etalon({
      applicant: { name: 'Мария', status: 'очевидец', foreignLanguage: false },
      phones: { aon: '+7 (903) 511-33-67' },
      address: {
        ...emptyAddress(),
        raw: 'Москва, Тверская улица, 12',
        country: 'Россия', region: 'Москва', settlement: 'Москва',
        okrug: 'ЦАО', district: 'Тверской', street: 'Тверская', house: '12',
        entrance: '3', floor: '5', source: 'yandex_map',
      },
      incidentTypeIds: ['it-101'],
      attributes: {
        where: 'house',
        fire_sign: 'flame',
        house_kind: 'multi',
        floors: 5,
        people_threat: 'yes',
        indoor_objects: ['apartment', 'gas_column'],
        gasified: 'yes',
      },
      description:
        'Задымление в квартире на 5 этаже многоквартирного дома, подъезд 3. В квартире предположительно находится пожилой человек, дверь не открывает. Дом газифицирован.',
      flags: {
        victimsPresent: true, victimsCount: 1,
        ambulanceRefusal: false, blocked: true, noContact: false, callDropped: false,
      },
    }),
    requiredFields: [
      'applicant.name', 'applicant.status', 'address.raw', 'incidentTypeIds',
      'description', 'flags.victimsPresent', 'attributes.where',
      'attributes.fire_sign', 'attributes.people_threat',
    ],
  },

  {
    id: 'sc-gas-smell',
    title: 'Запах газа в подъезде жилого дома',
    categoryId: 'it-104',
    categoryName: 'Происшествие 104',
    difficulty: 1,
    mode: 'cards',
    source: 'generated',
    status: 'validated',
    etalonVersion: 1,
    expectedDialogue: dialogueFixture('sc-gas-smell').expected,
    ttsReady: true,
    createdAt: '2026-09-16T11:00:00+03:00',
    validatedBy: 'Ковалёва И. С.',
    validatedAt: '2026-09-16T11:35:00+03:00',
    callScript: {
      dialogue: dialogueFixture('sc-gas-smell').brief,
      caller: { name: 'Виктор Степанович', phone: '+7 (915) 244-08-91', role: 'житель', emotionalState: 'встревожен' },
      address: { raw: 'Москва, Профсоюзная улица, 45, корпус 2', city: 'Москва', street: 'Профсоюзная', house: '45' },
      keyFacts: ['сильный запах газа в подъезде', 'второй этаж', 'жильцы на месте', 'источник неизвестен'],
      turns: [
        { speaker: 'caller', text: 'Здравствуйте. У нас в подъезде очень сильно пахнет газом. Профсоюзная, сорок пять, корпус два.' },
        { speaker: 'operator_hint', text: 'Уточните этаж и где сильнее всего чувствуется запах.' },
        { speaker: 'caller', text: 'Сильнее всего на втором этаже, у лифта. Я поднимался — там прямо невозможно дышать.' },
        { speaker: 'caller', text: 'Откуда идёт — не понимаю. Может, из квартиры какой-то. Люди дома, я слышу.' },
        { speaker: 'caller', text: 'Свет я не включал, как учили. Окно на площадке открыл.' },
      ],
    },
    etalonCard: {
      categoryCode: '104',
      address: { raw: 'Москва, Профсоюзная улица, 45, корпус 2', city: 'Москва', street: 'Профсоюзная', house: '45', floor: '2' },
      applicant: { name: 'Виктор Степанович', phone: '+7 (915) 244-08-91' },
      servicesToNotify: ['104', '101', '103', 'zhkh'],
      description: 'Сильный запах газа в подъезде жилого дома на 2 этаже. Источник не установлен. В доме находятся жильцы.',
    },
    etalonDraft: etalon({
      applicant: { name: 'Виктор Степанович', status: 'очевидец', foreignLanguage: false },
      phones: { aon: '+7 (915) 244-08-91' },
      address: {
        ...emptyAddress(),
        raw: 'Москва, Профсоюзная улица, 45, корпус 2',
        country: 'Россия', region: 'Москва', settlement: 'Москва',
        okrug: 'ЮЗАО', district: 'Черёмушки', street: 'Профсоюзная', house: '45',
        building: '2', floor: '2', source: 'yandex_map',
      },
      incidentTypeIds: ['it-104'],
      attributes: { gas_sign: ['indoor'], people_threat: 'yes' },
      description: 'Сильный запах газа в подъезде жилого дома на 2 этаже. Источник не установлен. В доме находятся жильцы.',
      flags: {
        victimsPresent: false, ambulanceRefusal: false, blocked: false,
        noContact: false, callDropped: false,
      },
    }),
    requiredFields: [
      'applicant.name', 'applicant.status', 'address.raw', 'incidentTypeIds',
      'description', 'attributes.gas_sign', 'attributes.people_threat',
    ],
  },

  {
    id: 'sc-water-pipe',
    title: 'Прорыв трубы с горячей водой во дворе',
    categoryId: 'it-water',
    categoryName: 'Повреждение водопровода',
    difficulty: 1,
    mode: 'cards',
    source: 'manual',
    status: 'validated',
    etalonVersion: 1,
    createdAt: '2026-09-16T14:20:00+03:00',
    validatedBy: 'Ковалёва И. С.',
    validatedAt: '2026-09-16T14:50:00+03:00',
    callScript: {
      caller: { name: 'Анна', phone: '+7 (926) 700-11-42', role: 'очевидец', emotionalState: 'спокоен' },
      address: { raw: 'Москва, Чертановская улица, 58, корпус 2', city: 'Москва', street: 'Чертановская', house: '58' },
      keyFacts: ['прорыв трубы с горячей водой', 'двор', 'заливает проезд', 'пострадавших нет'],
      turns: [
        { speaker: 'caller', text: 'Добрый день. У нас во дворе прорвало трубу, идёт пар и горячая вода. Чертановская, пятьдесят восемь, корпус два.' },
        { speaker: 'operator_hint', text: 'Уточните, заливает ли проезд и есть ли пострадавшие.' },
        { speaker: 'caller', text: 'Заливает весь проезд, машины не проедут. Вода горячая, парит сильно.' },
        { speaker: 'caller', text: 'Людей рядом нет, никто не пострадал. Дети во двор не выходят, я предупредила.' },
      ],
    },
    etalonCard: {
      categoryCode: 'water',
      address: { raw: 'Москва, Чертановская улица, 58, корпус 2', city: 'Москва', street: 'Чертановская', house: '58' },
      applicant: { name: 'Анна', phone: '+7 (926) 700-11-42' },
      servicesToNotify: ['vodokanal', 'zhkh', 'mosvodostok', 'oati'],
      description: 'Прорыв трубы с горячей водой во дворе дома, заливает внутридворовой проезд. Пострадавших нет.',
    },
    etalonDraft: etalon({
      applicant: { name: 'Анна', status: 'очевидец', foreignLanguage: false },
      phones: { aon: '+7 (926) 700-11-42' },
      address: {
        ...emptyAddress(),
        raw: 'Москва, Чертановская улица, 58, корпус 2',
        country: 'Россия', region: 'Москва', settlement: 'Москва',
        okrug: 'ЮАО', district: 'Чертаново Южное', street: 'Чертановская',
        house: '58', building: '2', source: 'yandex_map',
      },
      incidentTypeIds: ['it-water'],
      attributes: { water_where: 'yard', water_type: 'hot', people_threat: 'no' },
      description: 'Прорыв трубы с горячей водой во дворе дома, заливает внутридворовой проезд. Пострадавших нет.',
      flags: {
        victimsPresent: false, ambulanceRefusal: false, blocked: false,
        noContact: false, callDropped: false,
      },
    }),
    requiredFields: [
      'applicant.name', 'applicant.status', 'address.raw', 'incidentTypeIds',
      'description', 'attributes.water_where',
    ],
  },

  {
    id: 'sc-dtp-draft',
    title: 'ДТП с пострадавшими на перекрёстке',
    categoryId: 'it-dtp',
    categoryName: 'ДТП',
    difficulty: 3,
    mode: 'cards',
    source: 'generated',
    status: 'generated',
    etalonVersion: 1,
    createdAt: '2026-09-17T08:05:00+03:00',
    notesForTeacher: 'Сгенерировано нейросетью, требует проверки: уточнить количество пострадавших в легенде.',
    callScript: {
      caller: { name: 'Неизвестен', phone: '+7 (985) 330-77-10', role: 'очевидец', emotionalState: 'взволнован' },
      address: { raw: 'Москва, Ленинский проспект, 32', city: 'Москва', street: 'Ленинский проспект', house: '32' },
      keyFacts: ['столкновение двух автомобилей', 'двое пострадавших', 'перекрыта полоса движения'],
      turns: [
        { speaker: 'caller', text: 'Тут авария, две машины столкнулись! Ленинский проспект, дом тридцать два!' },
        { speaker: 'caller', text: 'Есть пострадавшие, двое точно. Один за рулём не двигается совсем.' },
        { speaker: 'caller', text: 'Одна полоса перекрыта, пробка собирается.' },
      ],
    },
    etalonCard: {
      categoryCode: 'dtp',
      address: { raw: 'Москва, Ленинский проспект, 32', city: 'Москва', street: 'Ленинский проспект', house: '32' },
      applicant: { phone: '+7 (985) 330-77-10' },
      casualties: { injured: 2 },
      servicesToNotify: ['102', '103', 'codd'],
      description: 'Столкновение двух автомобилей на Ленинском проспекте. Двое пострадавших, один без движения. Перекрыта одна полоса движения.',
    },
    etalonDraft: etalon({
      applicant: { status: 'очевидец', foreignLanguage: false },
      phones: { aon: '+7 (985) 330-77-10' },
      address: {
        ...emptyAddress(),
        raw: 'Москва, Ленинский проспект, 32',
        country: 'Россия', region: 'Москва', settlement: 'Москва',
        okrug: 'ЮЗАО', district: 'Гагаринский', street: 'Ленинский проспект',
        house: '32', source: 'yandex_map',
      },
      incidentTypeIds: ['it-dtp'],
      attributes: { dtp_kind: 'collision', victims: 'yes', traffic_block: 'yes' },
      description: 'Столкновение двух автомобилей на Ленинском проспекте. Двое пострадавших, один без движения. Перекрыта одна полоса движения.',
      flags: {
        victimsPresent: true, victimsCount: 2,
        ambulanceRefusal: false, blocked: false, noContact: false, callDropped: false,
      },
    }),
    requiredFields: [
      'applicant.status', 'address.raw', 'incidentTypeIds', 'description',
      'flags.victimsPresent', 'attributes.dtp_kind', 'attributes.victims',
    ],
  },
];
