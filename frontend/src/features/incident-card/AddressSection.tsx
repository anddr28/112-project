import { useEffect, useRef, useState } from 'react';
import { api } from '../../shared/api';
import { useEscape } from '../../components/useEscape';
import { SOURCE_LABELS } from '../../shared/utils/address';
import { emptyAddress } from '../../shared/utils/card';
import type { AddressDraft, AddressSuggestion } from '../../shared/types';

/**
 * Блок «Адрес» (Alt+A).
 *
 * Инструкция п.4.3: адрес вводится в единую строку с домом включительно,
 * система показывает варианты из Яндекс.Карт / Яндекс.Организаций / ФИАС,
 * после выбора автоматически заполняет поля и определяет округ и район.
 * Карта открывается отдельным окном — здесь это диалог с координатами
 * (полноценная ГИС вынесена в P2, архитектура её допускает).
 */
export function AddressSection({
  value,
  disabled,
  onChange,
}: {
  value: AddressDraft;
  disabled: boolean;
  onChange: (next: AddressDraft) => void;
}) {
  // Черновик строки поиска: null — показываем выбранный адрес, строка — оператор печатает.
  const [draft, setDraft] = useState<string | null>(null);
  const [suggestions, setSuggestions] = useState<AddressSuggestion[]>([]);
  const [open, setOpen] = useState(false);
  const [mapOpen, setMapOpen] = useState(false);
  const boxRef = useRef<HTMLDivElement>(null);
  const query = draft ?? value.raw;

  const searching = draft !== null && draft.trim().length >= 3;

  useEffect(() => {
    if (!searching || draft === null) return;
    let cancelled = false;
    const timer = setTimeout(() => {
      void api.address.suggest(draft).then(
        (items) => {
          // Ответ на устаревший запрос не должен подменять более свежие подсказки.
          if (cancelled) return;
          setSuggestions(items);
          setOpen(items.length > 0);
        },
        () => {
          // Подсказки недоступны — адрес остаётся доступным для ручного ввода.
          if (!cancelled) setOpen(false);
        },
      );
    }, 280);
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [draft, searching]);

  useEffect(() => {
    function onDocClick(e: MouseEvent) {
      if (boxRef.current && !boxRef.current.contains(e.target as Node)) setOpen(false);
    }
    document.addEventListener('mousedown', onDocClick);
    return () => document.removeEventListener('mousedown', onDocClick);
  }, []);

  function pick(s: AddressSuggestion) {
    onChange(s.value);
    setDraft(null);
    setOpen(false);
  }

  function set<K extends keyof AddressDraft>(key: K, v: AddressDraft[K]) {
    onChange({ ...value, [key]: v });
  }

  return (
    <div className="arm-panel">
      <div className="arm-address__head">
        <span>Адрес:</span>
        <button
          type="button"
          className="arm-mini"
          onClick={() => setMapOpen(true)}
          disabled={disabled}
          title="Открыть карту"
        >
          Карта
        </button>
        {value.source && <span style={{ marginLeft: 'auto' }}>{SOURCE_LABELS[value.source]}</span>}
      </div>

      <div className="arm-address__search" ref={boxRef}>
        <input
          value={query}
          disabled={disabled}
          placeholder="Введите адрес с номером дома"
          onChange={(e) => setDraft(e.target.value)}
          onFocus={() => suggestions.length > 0 && setOpen(true)}
          aria-label="Единая адресная строка"
        />
        <button
          type="button"
          className="arm-mini"
          disabled={disabled}
          onClick={() => {
            setDraft('');
            setSuggestions([]);
          }}
          title="Очистить поисковый запрос"
        >
          ✕
        </button>

        {open && searching && (
          <div className="arm-suggest">
            {suggestions.map((s) => (
              <button key={s.id} type="button" className="arm-suggest__item" onClick={() => pick(s)}>
                <span>{s.label}</span>
                <span className="arm-suggest__src">{SOURCE_LABELS[s.source]}</span>
              </button>
            ))}
          </div>
        )}
      </div>

      <div className="arm-address__grid">
        <Fld className="c4" label="Страна:" value={value.country} disabled={disabled} onChange={(v) => set('country', v)} />
        <Fld className="c4" label="Субъект:" value={value.region} disabled={disabled} onChange={(v) => set('region', v)} />
        <Fld className="c4" label="Населенный пункт:" value={value.settlement} disabled={disabled} onChange={(v) => set('settlement', v)} />

        <Fld className="c4" label="Объект:" value={value.object} disabled={disabled} onChange={(v) => set('object', v)} />
        <Fld className="c4" label="Округ:" value={value.okrug} disabled={disabled} onChange={(v) => set('okrug', v)} />
        <Fld className="c4" label="Район:" value={value.district} disabled={disabled} onChange={(v) => set('district', v)} />

        <Fld className="c6" label="Улица:" value={value.street} disabled={disabled} onChange={(v) => set('street', v)} />
        <Fld className="c3" label="Дом/Вл:" value={value.house} disabled={disabled} onChange={(v) => set('house', v)} />
        <Fld className="c3" label="Корпус:" value={value.building} disabled={disabled} onChange={(v) => set('building', v)} />

        <Fld className="c3" label="Стр/соор:" value={value.structure} disabled={disabled} onChange={(v) => set('structure', v)} />
        <Fld className="c3" label="Квартира/офис:" value={value.apartment} disabled={disabled} onChange={(v) => set('apartment', v)} />
        <Fld className="c2" label="Подъезд:" value={value.entrance} disabled={disabled} onChange={(v) => set('entrance', v)} />
        <Fld className="c2" label="Этаж:" value={value.floor} disabled={disabled} onChange={(v) => set('floor', v)} />
        <Fld className="c2" label="Код:" value={value.code} disabled={disabled} onChange={(v) => set('code', v)} />

        <Fld className="c12" label="Описательный адрес:" value={value.descriptive} disabled={disabled} onChange={(v) => set('descriptive', v)} />
      </div>

      <div className="arm-address__foot">
        <button
          type="button"
          className="arm-mini"
          disabled={disabled}
          onClick={() => {
            onChange(emptyAddress());
            setDraft(null);
          }}
        >
          очистить адрес
        </button>
      </div>

      {mapOpen && (
        <MapDialog
          value={value}
          onClose={() => setMapOpen(false)}
          onApply={(lat, lon) => {
            onChange({ ...value, lat, lon, source: value.source ?? 'map_pick' });
            setMapOpen(false);
          }}
        />
      )}
    </div>
  );
}

function Fld({
  className,
  label,
  value,
  disabled,
  onChange,
}: {
  className: string;
  label: string;
  value: string;
  disabled: boolean;
  onChange: (v: string) => void;
}) {
  return (
    <label className={`arm-fld ${className}`}>
      <span className="arm-fld__label">{label}</span>
      <input
        className="arm-line-input"
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
      />
    </label>
  );
}

/**
 * Окно карты.
 *
 * Реальный АРМ открывает Яндекс.Карты в отдельном окне со слоями, радиусом
 * и кнопкой «Указать на карте». В P0 сделан контур: координаты + применение.
 * Полноценная ГИС — P2; внешняя карта не должна быть обязательной зависимостью
 * изолированного контура.
 */
function MapDialog({
  value,
  onClose,
  onApply,
}: {
  value: AddressDraft;
  onClose: () => void;
  onApply: (lat: number, lon: number) => void;
}) {
  const [lat, setLat] = useState(value.lat != null ? String(value.lat) : '');
  const [lon, setLon] = useState(value.lon != null ? String(value.lon) : '');

  useEscape(onClose);

  return (
    <div className="modal-backdrop" role="presentation" onClick={(e) => e.target === e.currentTarget && onClose()}>
      <div className="arm-modal" role="dialog" aria-modal="true" aria-label="Карта">
        <div className="arm-modal__head">
          <span className="arm-modal__title">С112 — карта</span>
          <button type="button" className="arm-survey__close" style={{ color: 'var(--arm-text)' }} onClick={onClose}>✕</button>
        </div>

        <div style={{ padding: '0 16px 12px' }}>
          <div className="row row--tight" style={{ marginBottom: 10 }}>
            <input className="input" style={{ width: 130 }} placeholder="Широта" value={lat} onChange={(e) => setLat(e.target.value)} />
            <input className="input" style={{ width: 130 }} placeholder="Долгота" value={lon} onChange={(e) => setLon(e.target.value)} />
            <button
              type="button"
              className="btn btn--sm"
              onClick={() => {
                const la = Number(lat);
                const lo = Number(lon);
                if (Number.isFinite(la) && Number.isFinite(lo)) onApply(la, lo);
              }}
            >
              ОК
            </button>
          </div>

          <div
            style={{
              height: 240,
              display: 'grid',
              placeItems: 'center',
              background:
                'repeating-linear-gradient(0deg,#eef0f2,#eef0f2 23px,#e3e6e9 23px,#e3e6e9 24px),' +
                'repeating-linear-gradient(90deg,#eef0f2,#eef0f2 23px,#e3e6e9 23px,#e3e6e9 24px)',
              border: '1px solid var(--arm-border)',
              color: 'var(--arm-text-2)',
              fontSize: 12,
              textAlign: 'center',
              padding: 16,
            }}
          >
            <div>
              <div style={{ fontSize: 22, marginBottom: 6 }}>📍</div>
              <div>{value.raw || 'Адрес не выбран'}</div>
              {value.lat != null && (
                <div className="mono" style={{ marginTop: 6 }}>
                  {value.lat.toFixed(4)}, {value.lon?.toFixed(4)}
                </div>
              )}
              <div style={{ marginTop: 10, color: 'var(--arm-label)' }}>
                Картографическая подложка подключается отдельно.<br />
                Изолированный контур не зависит от внешних сервисов.
              </div>
            </div>
          </div>
        </div>

        <div className="arm-modal__foot">
          <button type="button" className="arm-modal__save" onClick={onClose}>Закрыть</button>
        </div>
      </div>
    </div>
  );
}
