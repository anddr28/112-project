/**
 * Классификатор происшествий: типы, опросные карты, правила определения служб.
 *
 * TODO(backend) GAP-20 / B-04 — метаданные признаков (widget, options, visibleWhen).
 * TODO(backend) GAP-21 / B-06 — определение служб от (типы, признаки, адрес).
 *
 * Состав опросных карт для «Происшествие 101», «Происшествие 104» и «Взрыв»
 * снят со скриншотов СКРИНШОТ КАРТОЧКИ 112ГСИ.docx — это реальные группы
 * признаков и реальные формулировки, а не выдумка. Правила определения служб
 * упрощены: настоящие живут в XLSX-классификаторе (1308 строк) и считаются
 * на backend.
 */

import type { AttributeGroup, AttributeValue, IncidentType } from '../../types';

export const INCIDENT_TYPES: IncidentType[] = [
  { id: 'it-101', code: '101', name: 'Происшествие 101', depth: 1, synonyms: ['пожар', 'дым', 'задымление', 'горит'], hasReference: true },
  { id: 'it-102', code: '102', name: 'Происшествие 102', depth: 1, synonyms: ['полиция', 'нападение', 'кража', 'драка'], hasReference: true },
  { id: 'it-103', code: '103', name: 'Происшествие 103', depth: 1, synonyms: ['скорая', 'медицина', 'плохо', 'вызов 03'], hasReference: true },
  { id: 'it-104', code: '104', name: 'Происшествие 104', depth: 1, synonyms: ['газ', 'запах газа', 'утечка'], hasReference: true },
  { id: 'it-gorhoz', code: 'gorhoz', name: 'Аварии и происшествия в городском хозяйстве', depth: 1, synonyms: ['авария', 'жкх', 'коммунальные'], hasReference: true },
  { id: 'it-explosion', code: 'explosion', name: 'Взрыв', depth: 2, synonyms: ['взрыв', 'хлопок'], hasReference: true },
  { id: 'it-dtp', code: 'dtp', name: 'ДТП', depth: 2, synonyms: ['дтп', 'авария', 'столкновение', 'наезд'], hasReference: true },
  { id: 'it-collapse', code: 'collapse', name: 'Обрушение', depth: 2, synonyms: ['обрушение', 'обвал'], hasReference: false },
  { id: 'it-water', code: 'water', name: 'Повреждение водопровода', depth: 2, synonyms: ['вода', 'прорыв', 'течь', 'затопление'], hasReference: true },
  { id: 'it-elevator', code: 'elevator', name: 'Застревание в лифте', depth: 2, synonyms: ['лифт', 'застрял'], hasReference: false },
  { id: 'it-tree', code: 'tree', name: 'Падение дерева', depth: 2, synonyms: ['дерево', 'упало'], hasReference: false },
  { id: 'it-wrong', code: 'wrong-number', name: 'Ошибочно набран номер', depth: 1, synonyms: ['ошибка', 'не туда'], hasReference: false },
  { id: 'it-danger', code: 'person-danger', name: 'Человек в опасности', depth: 2, synonyms: ['опасность', 'угроза'], hasReference: true },
  { id: 'it-cancel', code: 'cancel', name: 'Отмена вызова', depth: 1, synonyms: ['отмена'], hasReference: false },
  { id: 'it-test', code: 'test', name: 'Тестовый вызов', depth: 1, synonyms: ['тест', 'проверка'], hasReference: false },
  { id: 'it-consult', code: 'consult', name: 'Консультация', depth: 1, synonyms: ['консультация', 'справка'], hasReference: false },
  { id: 'it-foreign', code: 'foreign', name: 'Вызов на иностранном языке', depth: 1, synonyms: ['иностранный', 'язык'], hasReference: false },
  { id: 'it-handover', code: 'handover', name: 'Передача дежурства', depth: 1, synonyms: ['дежурство'], hasReference: false },
  { id: 'it-101ref', code: 'spravka-101', name: 'Справка-101', depth: 1, synonyms: ['справка'], hasReference: false },
  { id: 'it-hydro', code: 'hydro', name: 'Аварии на гидротехнических сооружениях', depth: 2, synonyms: ['гидро', 'плотина'], hasReference: false },
  { id: 'it-hazard', code: 'hazard', name: 'Аварии на опасных и производственных объектах', depth: 2, synonyms: ['опасный объект', 'производство'], hasReference: false },
  { id: 'it-uav', code: 'uav', name: 'БПЛА', depth: 2, synonyms: ['бпла', 'дрон', 'беспилотник'], hasReference: false },
  { id: 'it-thanks', code: 'thanks', name: 'Благодарность службам', depth: 1, synonyms: ['благодарность', 'спасибо'], hasReference: false },
];

/** Часто используемые — верхний ряд плашек в блоке «Что случилось?». */
export const FREQUENT_TYPE_IDS = [
  'it-dtp', 'it-wrong', 'it-104', 'it-danger', 'it-cancel',
  'it-test', 'it-handover', 'it-consult', 'it-foreign', 'it-101ref',
];

/** «Значимые типы происшествий» — список согласуется с руководством Службы 112. */
export const SIGNIFICANT_TYPE_IDS = ['it-101', 'it-explosion', 'it-collapse', 'it-hazard', 'it-uav'];

/** Опросные карты по типам происшествия. Ключ — id типа. */
export const ATTRIBUTE_GROUPS: Record<string, AttributeGroup[]> = {
  'it-101': [
    {
      code: 'where', label: 'Где', widget: 'chips-single', order: 1, required: true,
      options: [
        { code: 'street', label: 'Улица' },
        { code: 'transport', label: 'Транспорт' },
        { code: 'house', label: 'Дом' },
        { code: 'building', label: 'Здание / объект' },
        { code: 'hazard', label: 'Опасный объект' },
      ],
    },
    {
      code: 'fire_sign', label: 'Признак пожара (дом)', widget: 'chips-single', order: 2, required: true,
      visibleWhen: { attr: 'where', anyOf: ['house'] },
      options: [
        { code: 'flame', label: 'Открытое пламя / Дым' },
        { code: 'burning_smell', label: 'Запах гари' },
        { code: 'alarm', label: 'Сработала пожарная сигнализация' },
      ],
    },
    {
      code: 'access', label: 'Доступ', widget: 'chips-multi', order: 3,
      options: [{ code: 'no_access', label: 'Нет доступа' }],
    },
    {
      code: 'house_kind', label: 'Дом (пламя, дым)', widget: 'chips-single', order: 4,
      visibleWhen: { attr: 'fire_sign', anyOf: ['flame', 'burning_smell'] },
      options: [
        { code: 'multi', label: 'Дом многоквартирный' },
        { code: 'private', label: 'Дом частный' },
        { code: 'dacha', label: 'Дача' },
        { code: 'shed', label: 'Сарай / бытовка / хоз. постройка' },
        { code: 'evicted', label: 'Выселенное здание' },
      ],
    },
    { code: 'floors', label: 'Этажность здания', widget: 'number', order: 5 },
    {
      code: 'people_threat', label: 'Угроза людям', widget: 'bool', order: 6, required: true,
      options: [{ code: 'yes', label: 'Да' }, { code: 'no', label: 'Нет' }],
    },
    {
      code: 'indoor_objects', label: 'Внутридомовые объекты (пламя, дым)', widget: 'chips-multi', order: 7,
      visibleWhen: { attr: 'fire_sign', anyOf: ['flame', 'burning_smell'] },
      options: [
        { code: 'apartment', label: 'Квартира' },
        { code: 'balcony', label: 'Балкон' },
        { code: 'gas_column', label: 'Газовая колонка' },
        { code: 'gas_stove', label: 'Газовая плита' },
        { code: 'elevator', label: 'Лифт' },
        { code: 'chute', label: 'Мусоропровод' },
        { code: 'entrance', label: 'Подъезд' },
        { code: 'el_meter', label: 'Счетчик электричества' },
        { code: 'wiring', label: 'Электрическая проводка' },
        { code: 'el_panel', label: 'Электрощит' },
        { code: 'stairs', label: 'Лестничная клетка' },
        { code: 'basement', label: 'Подвал' },
        { code: 'other_indoor', label: 'Прочие внутридомовые объекты' },
        { code: 'roof', label: 'Крыша' },
      ],
    },
    {
      code: 'traffic_block', label: 'Есть ли перекрытие движения', widget: 'bool', order: 8,
      options: [{ code: 'yes', label: 'Да' }, { code: 'no', label: 'Нет' }],
    },
    {
      code: 'gasified', label: 'Проведена ли газификация', widget: 'chips-single', order: 9,
      options: [
        { code: 'yes', label: 'Да' },
        { code: 'no', label: 'Нет' },
        { code: 'unknown', label: 'Нет данных' },
      ],
    },
  ],

  'it-104': [
    {
      code: 'gas_sign', label: 'Признаки происшествия', widget: 'chips-multi', order: 1, required: true,
      options: [
        { code: 'outdoor', label: 'Запах газа вне помещения (на улице)' },
        { code: 'indoor', label: 'Запах газа в помещении (в квартире, в доме)' },
        { code: 'equipment', label: 'Нарушение в работе газового оборудования' },
        { code: 'pipeline', label: 'Повреждение газопровода' },
        { code: 'pressure', label: 'Повышенное давление газа' },
      ],
    },
    {
      code: 'people_threat', label: 'Угроза людям', widget: 'bool', order: 2, required: true,
      options: [{ code: 'yes', label: 'Да' }, { code: 'no', label: 'Нет' }],
    },
  ],

  'it-explosion': [
    {
      code: 'where_explosion', label: 'Где взрыв', widget: 'chips-single', order: 1, required: true,
      options: [
        { code: 'building', label: 'Здание / Объект' },
        { code: 'transport', label: 'Транспорт' },
        { code: 'unknown', label: 'Звуки похожие на взрыв, что взорвалось сообщить не может' },
      ],
    },
    {
      code: 'has_fire', label: 'Есть возгорание', widget: 'bool', order: 2,
      options: [{ code: 'yes', label: 'Да' }, { code: 'no', label: 'Нет' }],
    },
    {
      code: 'collapse_threat', label: 'Есть угроза обрушения', widget: 'bool', order: 3,
      options: [{ code: 'yes', label: 'Да' }, { code: 'no', label: 'Нет' }],
    },
    { code: 'damage_seen', label: 'Какие видят разрушения', widget: 'text', order: 4 },
  ],

  'it-water': [
    {
      code: 'water_where', label: 'Где повреждение', widget: 'chips-single', order: 1, required: true,
      options: [
        { code: 'yard', label: 'Двор' },
        { code: 'street', label: 'Улица' },
        { code: 'entrance', label: 'Подъезд' },
        { code: 'apartment', label: 'Квартира' },
        { code: 'basement', label: 'Подвал' },
      ],
    },
    {
      code: 'water_type', label: 'Тип воды', widget: 'chips-single', order: 2,
      options: [
        { code: 'hot', label: 'Горячая' },
        { code: 'cold', label: 'Холодная' },
        { code: 'unknown', label: 'Нет данных' },
      ],
    },
    {
      code: 'people_threat', label: 'Угроза людям', widget: 'bool', order: 3,
      options: [{ code: 'yes', label: 'Да' }, { code: 'no', label: 'Нет' }],
    },
  ],

  'it-dtp': [
    {
      code: 'dtp_kind', label: 'Вид ДТП', widget: 'chips-single', order: 1, required: true,
      options: [
        { code: 'collision', label: 'Столкновение' },
        { code: 'pedestrian', label: 'Наезд на пешехода' },
        { code: 'rollover', label: 'Опрокидывание' },
        { code: 'obstacle', label: 'Наезд на препятствие' },
      ],
    },
    {
      code: 'victims', label: 'Есть пострадавшие', widget: 'bool', order: 2, required: true,
      options: [{ code: 'yes', label: 'Да' }, { code: 'no', label: 'Нет' }],
    },
    {
      code: 'traffic_block', label: 'Есть ли перекрытие движения', widget: 'bool', order: 3,
      options: [{ code: 'yes', label: 'Да' }, { code: 'no', label: 'Нет' }],
    },
  ],

  'it-elevator': [
    {
      code: 'people_inside', label: 'Есть люди в кабине', widget: 'bool', order: 1, required: true,
      options: [{ code: 'yes', label: 'Да' }, { code: 'no', label: 'Нет' }],
    },
    {
      code: 'medical_needed', label: 'Требуется медицинская помощь', widget: 'bool', order: 2,
      options: [{ code: 'yes', label: 'Да' }, { code: 'no', label: 'Нет' }],
    },
  ],
};

/** Справочная информация по типу происшествия (Инструкция п.4.4). */
export const TYPE_REFERENCE: Record<string, string> = {
  'it-101': 'Пожар. Уточнить: что горит, есть ли открытое пламя, угроза людям, этаж, наличие газификации. При угрозе людям — немедленно 101 и 103.',
  'it-104': 'Запах газа. Запретить пользоваться электроприборами и открытым огнём, рекомендовать проветрить помещение и покинуть его. Службы 104, при угрозе — 101.',
  'it-explosion': 'Взрыв. Уточнить характер разрушений, наличие возгорания и угрозы обрушения. Оповещаются 101, 102, 103, ФСБ.',
  'it-water': 'Повреждение водопровода. Уточнить тип воды и зону затопления. Оповещаются Мосводоканал и Деп. ЖКХ.',
  'it-dtp': 'ДТП. Уточнить количество пострадавших и наличие перекрытия движения. Оповещаются 102, 103, ЦОДД.',
};

// ─────────────────────────────────── FIXTURE: определение служб (GAP-21/B-06)

interface ServiceRule {
  /** служба назначается, если выполнены все условия */
  when: { typeId?: string; attr?: string; anyOf?: string[] };
  services: Array<{ code: string; primary?: boolean; reason: string }>;
}

const RULES: ServiceRule[] = [
  { when: { typeId: 'it-101' }, services: [
    { code: '101', primary: true, reason: 'тип происшествия: пожар' },
    { code: 'zhkh', reason: 'тип происшествия: пожар в жилом секторе' },
  ] },
  { when: { typeId: 'it-101', attr: 'people_threat', anyOf: ['yes'] }, services: [
    { code: '103', primary: true, reason: 'признак: угроза людям' },
    { code: 'cemp', reason: 'признак: угроза людям' },
  ] },
  { when: { typeId: 'it-101', attr: 'indoor_objects', anyOf: ['gas_column', 'gas_stove'] }, services: [
    { code: '104', primary: true, reason: 'признак: газовое оборудование' },
  ] },
  { when: { typeId: 'it-101', attr: 'indoor_objects', anyOf: ['elevator'] }, services: [
    { code: 'moslift', reason: 'признак: лифт' },
  ] },
  { when: { typeId: 'it-101', attr: 'indoor_objects', anyOf: ['wiring', 'el_panel', 'el_meter'] }, services: [
    { code: 'oek', reason: 'признак: электрооборудование' },
  ] },
  { when: { typeId: 'it-101', attr: 'traffic_block', anyOf: ['yes'] }, services: [
    { code: 'codd', reason: 'признак: перекрытие движения' },
  ] },
  { when: { typeId: 'it-101', attr: 'gasified', anyOf: ['yes'] }, services: [
    { code: '104', reason: 'признак: дом газифицирован' },
  ] },

  { when: { typeId: 'it-104' }, services: [
    { code: '104', primary: true, reason: 'тип происшествия: запах газа' },
    { code: 'zhkh', reason: 'тип происшествия: запах газа' },
  ] },
  { when: { typeId: 'it-104', attr: 'gas_sign', anyOf: ['indoor', 'pipeline', 'pressure'] }, services: [
    { code: '101', primary: true, reason: 'признак: газ в помещении / повреждение газопровода' },
  ] },
  { when: { typeId: 'it-104', attr: 'people_threat', anyOf: ['yes'] }, services: [
    { code: '103', reason: 'признак: угроза людям' },
  ] },

  { when: { typeId: 'it-explosion' }, services: [
    { code: '101', primary: true, reason: 'тип происшествия: взрыв' },
    { code: '102', primary: true, reason: 'тип происшествия: взрыв' },
    { code: '103', primary: true, reason: 'тип происшествия: взрыв' },
    { code: 'fsb', reason: 'тип происшествия: взрыв' },
    { code: 'mosbez', reason: 'тип происшествия: взрыв' },
  ] },

  { when: { typeId: 'it-water' }, services: [
    { code: 'vodokanal', primary: true, reason: 'тип происшествия: повреждение водопровода' },
    { code: 'zhkh', primary: true, reason: 'тип происшествия: повреждение водопровода' },
  ] },
  { when: { typeId: 'it-water', attr: 'water_where', anyOf: ['street', 'yard'] }, services: [
    { code: 'mosvodostok', reason: 'признак: повреждение вне здания' },
    { code: 'oati', reason: 'признак: повреждение вне здания' },
  ] },

  { when: { typeId: 'it-dtp' }, services: [
    { code: '102', primary: true, reason: 'тип происшествия: ДТП' },
    { code: 'codd', primary: true, reason: 'тип происшествия: ДТП' },
  ] },
  { when: { typeId: 'it-dtp', attr: 'victims', anyOf: ['yes'] }, services: [
    { code: '103', primary: true, reason: 'признак: есть пострадавшие' },
  ] },

  { when: { typeId: 'it-elevator' }, services: [
    { code: 'moslift', primary: true, reason: 'тип происшествия: застревание в лифте' },
  ] },
  { when: { typeId: 'it-elevator', attr: 'people_inside', anyOf: ['yes'] }, services: [
    { code: '101', primary: true, reason: 'признак: люди в кабине' },
  ] },

  { when: { typeId: 'it-tree' }, services: [
    { code: 'zhkh', primary: true, reason: 'тип происшествия: падение дерева' },
    { code: 'oati', reason: 'тип происшествия: падение дерева' },
  ] },
  { when: { typeId: 'it-102' }, services: [{ code: '102', primary: true, reason: 'тип происшествия' }] },
  { when: { typeId: 'it-103' }, services: [{ code: '103', primary: true, reason: 'тип происшествия' }] },
];

export interface ResolvedServiceRef {
  code: string;
  isPrimary: boolean;
  reason: string;
}

/**
 * FIXTURE определения служб. Настоящая логика — 1308 строк XLSX на backend.
 * Соблюдает документированное правило: адрес из ФИАС службы не подтягивает.
 */
export function resolveServices(
  typeIds: string[],
  attributes: Record<string, AttributeValue>,
  addressFilled: boolean,
  addressSource?: string,
): ResolvedServiceRef[] {
  if (typeIds.length === 0 || !addressFilled) return [];
  if (addressSource === 'fias') return [];

  const acc = new Map<string, ResolvedServiceRef>();

  for (const rule of RULES) {
    if (rule.when.typeId && !typeIds.includes(rule.when.typeId)) continue;

    if (rule.when.attr) {
      const value = attributes[rule.when.attr];
      const selected = Array.isArray(value) ? value : value == null ? [] : [String(value)];
      const matched = (rule.when.anyOf ?? []).some((code) => selected.includes(code));
      if (!matched) continue;
    }

    for (const svc of rule.services) {
      const existing = acc.get(svc.code);
      if (existing) {
        // Первичность «залипает»: если хоть одно правило назвало службу основной.
        if (svc.primary) existing.isPrimary = true;
      } else {
        acc.set(svc.code, { code: svc.code, isPrimary: Boolean(svc.primary), reason: svc.reason });
      }
    }
  }

  return [...acc.values()];
}

/** Подпись группы признаков по её коду: «where» → «Где». */
export function attributeLabel(code: string): string | undefined {
  for (const groups of Object.values(ATTRIBUTE_GROUPS)) {
    const g = groups.find((x) => x.code === code);
    if (g) return g.label;
  }
  return undefined;
}

/** Подпись значения признака: («where», «house») → «Дом». */
export function attributeValueLabel(attrCode: string, valueCode: string): string | undefined {
  for (const groups of Object.values(ATTRIBUTE_GROUPS)) {
    const g = groups.find((x) => x.code === attrCode);
    const o = g?.options?.find((x) => x.code === valueCode);
    if (o) return o.label;
  }
  return undefined;
}

export function attributeGroupsFor(typeId: string): AttributeGroup[] {
  return ATTRIBUTE_GROUPS[typeId] ?? [];
}

export function typeById(id: string): IncidentType | undefined {
  return INCIDENT_TYPES.find((t) => t.id === id);
}

/**
 * Поиск типов по правилам Инструкции п.4.2: по части слова, слова в любом
 * порядке, без учёта регистра, знаки препинания можно опускать, плюс синонимы.
 */
export function searchTypes(query: string): IncidentType[] {
  const normalized = query.toLowerCase().replace(/ё/g, 'е').replace(/[-,.]/g, ' ').trim();
  if (!normalized) return [];
  const words = normalized.split(/\s+/);

  return INCIDENT_TYPES.filter((type) => {
    const haystack = [type.name, type.code, ...type.synonyms]
      .join(' ')
      .toLowerCase()
      .replace(/ё/g, 'е')
      .replace(/[-,.]/g, ' ');
    return words.every((word) => haystack.split(/\s+/).some((token) => token.startsWith(word)));
  });
}
