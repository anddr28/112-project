import { useEffect, useState } from 'react';
import { Badge } from '../../components/ui';
import { api } from '../../shared/api';
import { asCodes, formatAddress } from '../../shared/utils/card';
import type { AttributeGroup, IncidentCardDraft, IncidentType } from '../../shared/types';

/** Подписи классификатора вспомогательны: их отказ не ломает просмотр карточки. */
function ignoreLookupError(): void {}

/**
 * Просмотровое представление карточки.
 *
 * Переиспользуется для эталона, для карточки студента в отчёте преподавателя
 * и для сравнения «карточка ↔ эталон». Признаки сериализуются в читаемую
 * строку — так же, как в режиме просмотра карточки ДДС.
 *
 * Названия типов и подписи признаков берутся из классификатора через
 * сервисный слой: компонент ничего не знает о его внутреннем устройстве.
 */
export function EtalonCardView({
  card,
  requiredFields,
  highlight,
}: {
  card: IncidentCardDraft;
  requiredFields?: string[];
  /** пути полей, которые нужно подсветить как расхождение */
  highlight?: string[];
}) {
  const marked = new Set(highlight ?? []);
  const { nameOf, groupsFor } = useClassifierLabels(card.incidentTypeIds);

  return (
    <div className="stack" style={{ gap: 14 }}>
      <Row label="Заявитель" path="applicant.name" marked={marked}>
        {card.applicant.name || <span className="dim">не указан</span>}
        {card.applicant.status && <> · <span className="muted">{card.applicant.status}</span></>}
      </Row>

      <Row label="Телефон (АОН)" path="phones.aon" marked={marked}>
        <span className="mono">{card.phones.aon || <span className="dim">—</span>}</span>
      </Row>

      <Row label="Адрес" path="address.raw" marked={marked}>
        {card.address.raw ? formatAddress(card.address) : <span className="dim">не указан</span>}
      </Row>

      <Row label="Тип происшествия" path="incidentTypeIds" marked={marked}>
        {card.incidentTypeIds.length === 0 ? (
          <span className="dim">не выбран</span>
        ) : (
          <div className="row row--tight">
            {card.incidentTypeIds.map((id) => (
              <Badge key={id} tone="accent">{nameOf(id)}</Badge>
            ))}
          </div>
        )}
      </Row>

      <Row label="Признаки" path="attributes" marked={marked}>
        <AttributeSummary card={card} groupsFor={groupsFor} />
      </Row>

      <Row label="Описание со слов заявителя" path="description" marked={marked}>
        {card.description || <span className="dim">не заполнено</span>}
      </Row>

      <Row label="Пострадавшие" path="flags.victimsPresent" marked={marked}>
        {card.flags.victimsPresent ? `есть${card.flags.victimsCount ? `, ${card.flags.victimsCount}` : ''}` : 'нет'}
        {card.flags.blocked && <> · <span className="muted">заблокированные</span></>}
        {card.flags.ambulanceRefusal && <> · <span className="muted">отказ от скорой</span></>}
      </Row>

      <Row label="Службы" path="services" marked={marked}>
        {card.services.length === 0 ? (
          <span className="dim">не определены</span>
        ) : (
          <div className="row row--tight">
            {card.services.map((s) => (
              <Badge key={s.serviceId} tone={s.isPrimary ? 'accent' : 'neutral'}>
                {s.shortName}
                {s.source === 'manual' ? ' (вручную)' : ''}
              </Badge>
            ))}
          </div>
        )}
      </Row>

      {requiredFields && requiredFields.length > 0 && (
        <p className="field__hint">
          Обязательных полей в эталоне: {requiredFields.length}.
        </p>
      )}
    </div>
  );
}

function AttributeSummary({
  card,
  groupsFor,
}: {
  card: IncidentCardDraft;
  groupsFor: (typeId: string) => AttributeGroup[];
}) {
  const parts: string[] = [];

  for (const typeId of card.incidentTypeIds) {
    for (const group of groupsFor(typeId)) {
      const value = card.attributes[group.code];
      if (value == null || value === '' || (Array.isArray(value) && value.length === 0)) continue;

      if (group.widget === 'text' || group.widget === 'number') {
        parts.push(`${group.label}: ${String(value)}`);
        continue;
      }
      // Код без подписи пропускаем: технический идентификатор не место в сводке.
      const labels = asCodes(value)
        .map((code) => group.options?.find((o) => o.code === code)?.label)
        .filter((l): l is string => Boolean(l));
      if (labels.length > 0) parts.push(labels.join(', '));
    }
  }

  if (parts.length === 0) return <span className="dim">не выбраны</span>;
  return <span>{parts.join(' . ')} .</span>;
}

function Row({
  label,
  path,
  marked,
  children,
}: {
  label: string;
  path: string;
  marked: Set<string>;
  children: React.ReactNode;
}) {
  const isMarked = [...marked].some((m) => m === path || m.startsWith(`${path}.`));
  return (
    <div
      style={
        isMarked
          ? { padding: '6px 8px', margin: '-6px -8px', background: 'var(--u-danger-soft)', borderRadius: 4 }
          : undefined
      }
    >
      <div className="field__label">
        {label}
        {isMarked && <span style={{ color: 'var(--u-danger)' }}> · расхождение</span>}
      </div>
      <div>{children}</div>
    </div>
  );
}

/**
 * Подписи классификатора для просмотрового режима.
 *
 * Загружает названия типов и опросные карты выбранных типов через сервисный
 * слой. Пока данные не пришли, показывается нейтральная подпись: технический
 * идентификатор на экран не выводим, а держать весь экран в состоянии
 * загрузки ради подписей не нужно.
 */
function useClassifierLabels(typeIds: string[]) {
  /* Отказ справочника оставит нейтральные подписи — см. комментарий выше. */
  const [types, setTypes] = useState<IncidentType[]>([]);
  const [groups, setGroups] = useState<Record<string, AttributeGroup[]>>({});
  const key = typeIds.join(',');

  useEffect(() => {
    let cancelled = false;
    void api.classifier.incidentTypes().then((t) => {
      if (!cancelled) setTypes(t);
    }, ignoreLookupError);
    return () => { cancelled = true; };
  }, []);

  useEffect(() => {
    if (!key) return;
    let cancelled = false;
    const ids = key.split(',');
    void Promise.all(ids.map((id) => api.classifier.attributes(id))).then((lists) => {
      if (cancelled) return;
      setGroups(Object.fromEntries(ids.map((id, i) => [id, lists[i]])));
    }, ignoreLookupError);
    return () => { cancelled = true; };
  }, [key]);

  return {
    nameOf: (id: string) => types.find((t) => t.id === id)?.name ?? 'Тип происшествия',
    groupsFor: (id: string) => groups[id] ?? [],
  };
}
