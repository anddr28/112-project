import { useEffect, useState } from 'react';
import { api } from '../../shared/api';
import { asCodes } from '../../shared/utils/card';
import { cls } from '../../shared/utils/cls';
import type { AttributeGroup, AttributeValue, IncidentType } from '../../shared/types';

/**
 * Блок «Что случилось?» и опросные карты.
 *
 * Поведение по Инструкции п.4.2 и п.4.4:
 *   — три способа выбора: часто используемые, значимые типы, поиск по строке;
 *   — множественный выбор; вторая и последующие — через «добавить тип происшествия»;
 *   — опросная карта открывается на каждый выбранный тип и доступна ТОЛЬКО
 *     после заполнения адреса;
 *   — при удалении всех типов происходит возврат к блоку «ЧТО СЛУЧИЛОСЬ?»;
 *   — заголовок опросной карты — ссылка на справку: сплошное подчёркивание,
 *     если страница есть, пунктир — если нет.
 *
 * Классификатор целиком приходит из сервисного слоя: ни списка типов, ни
 * правил поиска (часть слова, синонимы, любой порядок) в этом компоненте нет —
 * он только показывает то, что вернул классификатор.
 */
/** Отказ справочного запроса не должен прерывать работу с карточкой. */
function ignore(): void {}

export function WhatHappened({
  selectedIds,
  attributes,
  addressFilled,
  disabled,
  onChangeTypes,
  onChangeAttribute,
  onOpenReference,
}: {
  selectedIds: string[];
  attributes: Record<string, AttributeValue>;
  addressFilled: boolean;
  disabled: boolean;
  onChangeTypes: (ids: string[]) => void;
  onChangeAttribute: (code: string, value: AttributeValue) => void;
  onOpenReference: (typeId: string) => void;
}) {
  const [query, setQuery] = useState('');
  const [featured, setFeatured] = useState<{ frequent: IncidentType[]; significant: IncidentType[] }>({
    frequent: [],
    significant: [],
  });
  const [allTypes, setAllTypes] = useState<IncidentType[]>([]);
  const [found, setFound] = useState<IncidentType[]>([]);

  useEffect(() => {
    let cancelled = false;
    // Отказ классификатора оставляет пустые списки: поиск честно покажет,
    // что ничего не найдено, вместо необработанного отказа промиса.
    void api.classifier.featured().then((f) => {
      if (!cancelled) setFeatured(f);
    }, ignore);
    void api.classifier.incidentTypes().then((t) => {
      if (!cancelled) setAllTypes(t);
    }, ignore);
    return () => { cancelled = true; };
  }, []);

  const searching = query.trim().length > 0;

  useEffect(() => {
    if (!searching) return;
    let cancelled = false;
    const timer = setTimeout(() => {
      void api.classifier.searchTypes(query).then((r) => {
        if (!cancelled) setFound(r);
      }, ignore);
    }, 150);
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [query, searching]);

  const results = searching ? found.filter((t) => !selectedIds.includes(t.id)) : [];
  const hasSelection = selectedIds.length > 0;
  // Пока классификатор не ответил, показываем нейтральную подпись, а не идентификатор.
  const nameOf = (id: string) => allTypes.find((t) => t.id === id)?.name ?? 'Тип происшествия';

  function add(id: string) {
    if (selectedIds.includes(id)) return;
    onChangeTypes([...selectedIds, id]);
    setQuery('');
  }

  function remove(id: string) {
    onChangeTypes(selectedIds.filter((x) => x !== id));
  }

  return (
    <div className="arm-what">
      <div className="arm-what__hint">
        {hasSelection ? 'Добавить тип происшествия' : 'Введите тип происшествия'}
      </div>

      <input
        className="arm-what__input"
        value={query}
        disabled={disabled}
        placeholder={hasSelection ? 'добавить тип происшествия' : 'что случилось?'}
        onChange={(e) => setQuery(e.target.value)}
        aria-label="Поиск типа происшествия"
      />

      {searching && (
        <div className="arm-what__results">
          {results.length === 0 ? (
            <div style={{ padding: '8px 10px', color: 'var(--arm-label)' }}>
              Ничего не найдено. Попробуйте часть слова или синоним.
            </div>
          ) : (
            results.map((t) => (
              <button key={t.id} type="button" className="arm-what__result" onClick={() => add(t.id)}>
                {t.name}
              </button>
            ))
          )}
        </div>
      )}

      {/* Выбранные типы — плашками под строкой поиска */}
      {hasSelection && (
        <div className="arm-chips" style={{ marginTop: 8 }}>
          {selectedIds.map((id) => (
            <button
              key={id}
              type="button"
              className="arm-chip arm-chip--picked"
              disabled={disabled}
              onClick={() => remove(id)}
              title="Нажмите, чтобы отменить выбор"
            >
              {nameOf(id)} ✕
            </button>
          ))}
        </div>
      )}

      {!hasSelection && (
        <>
          <div className="arm-what__section">
            <div className="arm-chips">
              {featured.frequent.map((t) => (
                <button key={t.id} type="button" className="arm-chip" disabled={disabled} onClick={() => add(t.id)}>
                  {t.name}
                </button>
              ))}
            </div>
          </div>

          <div className="arm-what__section">
            <div className="arm-what__section-title">Значимые типы происшествий:</div>
            <div className="arm-chips">
              {featured.significant.map((t) => (
                <button key={t.id} type="button" className="arm-chip" disabled={disabled} onClick={() => add(t.id)}>
                  {t.name}
                </button>
              ))}
            </div>
          </div>
        </>
      )}

      {/* Опросные карты — по одной на выбранный тип */}
      {hasSelection && !addressFilled && (
        <div className="arm-note arm-note--blocked">
          Дополнительные вопросы станут доступны после заполнения блока «Адрес».
        </div>
      )}

      {hasSelection &&
        addressFilled &&
        selectedIds.map((id) => (
          <SurveyCard
            key={id}
            typeId={id}
            typeName={nameOf(id)}
            attributes={attributes}
            disabled={disabled}
            onChange={onChangeAttribute}
            onRemove={() => remove(id)}
            onOpenReference={() => onOpenReference(id)}
          />
        ))}
    </div>
  );
}

function SurveyCard({
  typeId,
  typeName,
  attributes,
  disabled,
  onChange,
  onRemove,
  onOpenReference,
}: {
  typeId: string;
  typeName: string;
  attributes: Record<string, AttributeValue>;
  disabled: boolean;
  onChange: (code: string, value: AttributeValue) => void;
  onRemove: () => void;
  onOpenReference: () => void;
}) {
  const [groups, setGroups] = useState<AttributeGroup[] | null>(null);
  const [failed, setFailed] = useState(false);
  const [hasReference, setHasReference] = useState(false);

  // Компонент смонтирован под ключом typeId, поэтому сбрасывать состояние
  // при его смене не нужно: React создаёт новый экземпляр.
  useEffect(() => {
    let cancelled = false;
    void api.classifier
      .attributes(typeId)
      .then((g) => {
        if (!cancelled) setGroups(g);
      })
      .catch(() => {
        // Без опросной карты оператор продолжает заполнять остальную карточку,
        // поэтому показываем отказ на месте, а не роняем рабочее место.
        if (!cancelled) setFailed(true);
      });
    void api.classifier
      .reference(typeId)
      .then((r) => {
        if (!cancelled) setHasReference(r.exists);
      })
      .catch(() => undefined);
    return () => { cancelled = true; };
  }, [typeId]);

  /*
   * Снятие родительского признака скрывает зависимую группу — вместе со
   * значением. Иначе в карточке остался бы ответ на вопрос, которого оператор
   * уже не видит, и он ушёл бы и на оценку, и в службы.
   */
  useEffect(() => {
    if (!groups) return;
    for (const g of groups) {
      if (!g.visibleWhen) continue;
      const parent = asCodes(attributes[g.visibleWhen.attr]);
      const shown = g.visibleWhen.anyOf.some((code) => parent.includes(code));
      if (!shown && attributes[g.code] != null) onChange(g.code, null);
    }
  }, [groups, attributes, onChange]);

  if (!groups) {
    return (
      <div className="arm-survey">
        <div className="arm-survey__head"><span className="arm-survey__title">{typeName}</span></div>
        <div className="arm-survey__body" style={{ color: 'var(--arm-label)' }}>
          {failed ? 'Не удалось загрузить дополнительные вопросы' : 'Загрузка опросной карты…'}
        </div>
      </div>
    );
  }

  // Инструкция п.4.4: форма с доп. вопросами есть не у всех типов происшествия.
  if (groups.length === 0) {
    return (
      <div className="arm-survey">
        <div className="arm-survey__head">
          <button type="button" className={cls('arm-survey__title', hasReference ? 'arm-survey__title--has-ref' : 'arm-survey__title--no-ref')} onClick={onOpenReference}>
            {typeName}
          </button>
          <button type="button" className="arm-survey__close" onClick={onRemove} aria-label="Убрать тип происшествия">✕</button>
        </div>
        <div className="arm-survey__body" style={{ color: 'var(--arm-label)' }}>
          Для этого типа происшествия дополнительные вопросы не предусмотрены.
        </div>
      </div>
    );
  }

  const visible = groups
    .filter((g) => {
      if (!g.visibleWhen) return true;
      const parent = asCodes(attributes[g.visibleWhen.attr]);
      return g.visibleWhen.anyOf.some((code) => parent.includes(code));
    })
    .sort((a, b) => a.order - b.order);

  return (
    <div className="arm-survey">
      <div className="arm-survey__head">
        <button
          type="button"
          className={cls('arm-survey__title', hasReference ? 'arm-survey__title--has-ref' : 'arm-survey__title--no-ref')}
          onClick={onOpenReference}
          title="Открыть справочную информацию"
        >
          {typeName}
        </button>
        <button type="button" className="arm-survey__close" onClick={onRemove} aria-label="Убрать тип происшествия">✕</button>
      </div>

      <div className="arm-survey__body">
        {visible.map((group) => (
          <AttributeControl
            key={group.code}
            group={group}
            value={attributes[group.code] ?? null}
            disabled={disabled}
            onChange={(v) => onChange(group.code, v)}
          />
        ))}
      </div>
    </div>
  );
}

/** Один рендерер на все виды виджетов — метаданные приходят из классификатора. */
function AttributeControl({
  group,
  value,
  disabled,
  onChange,
}: {
  group: AttributeGroup;
  value: AttributeValue;
  disabled: boolean;
  onChange: (v: AttributeValue) => void;
}) {
  const selected = asCodes(value);

  return (
    <div className="arm-attr">
      <div className={cls('arm-attr__label', group.required && 'arm-attr__label--req')}>{group.label}</div>
      <div className="arm-attr__control">
        {(group.widget === 'chips-single' || group.widget === 'bool') && (
          <div className="arm-chips">
            {group.options?.map((opt) => (
              <button
                key={opt.code}
                type="button"
                className={cls('arm-chip', selected.includes(opt.code) && 'is-selected')}
                disabled={disabled}
                onClick={() => onChange(selected.includes(opt.code) ? null : opt.code)}
              >
                {opt.label}
              </button>
            ))}
          </div>
        )}

        {group.widget === 'chips-multi' && (
          <div className="arm-chips">
            {group.options?.map((opt) => (
              <button
                key={opt.code}
                type="button"
                className={cls('arm-chip', selected.includes(opt.code) && 'is-selected')}
                disabled={disabled}
                onClick={() =>
                  onChange(
                    selected.includes(opt.code)
                      ? selected.filter((c) => c !== opt.code)
                      : [...selected, opt.code],
                  )
                }
              >
                {opt.label}
              </button>
            ))}
          </div>
        )}

        {group.widget === 'text' && (
          <input
            className="arm-attr__text"
            type="text"
            value={typeof value === 'string' ? value : ''}
            disabled={disabled}
            onChange={(e) => onChange(e.target.value)}
          />
        )}

        {group.widget === 'number' && (
          <input
            className="arm-attr__number"
            type="number"
            min={0}
            value={typeof value === 'number' ? value : ''}
            disabled={disabled}
            onChange={(e) => onChange(e.target.value === '' ? null : Number(e.target.value))}
          />
        )}
      </div>
    </div>
  );
}
