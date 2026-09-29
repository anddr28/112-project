import { useState } from 'react';
import { api } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { Card, EmptyState, ErrorState, Loading } from '../../components/ui';
import { formatDateTime } from '../../shared/utils/time';
import type { Setting } from '../../shared/types';

/**
 * Настройки платформы (FE-10): ключ — значение JSON. Значение правится как
 * JSON и проверяется до отправки: сервер хранит jsonb, и строка «0.7» вместо
 * числа 0.7 тихо сломала бы расчёт. Каждое изменение попадает в аудит.
 */
export function AdminSettingsPage() {
  const settings = useAsync(() => api.admin.settings(), []);

  if (settings.loading) return <Loading />;
  if (settings.error) return <ErrorState text={settings.error} onRetry={settings.reload} />;
  const list = settings.data ?? [];

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <h1>Настройки платформы</h1>
          <div className="page-head__sub">Параметры оценки, опыта и обслуживания. Изменения действуют для новых занятий и фиксируются в журнале аудита.</div>
        </div>
      </div>
      {list.length === 0 ? (
        <Card><EmptyState title="Настроек нет" /></Card>
      ) : (
        <div className="stack">
          {list.map((s) => <SettingRow key={s.key} setting={s} />)}
        </div>
      )}
    </>
  );
}

function pretty(value: unknown): string {
  return JSON.stringify(value, null, 2) ?? 'null';
}

function SettingRow({ setting }: { setting: Setting }) {
  const [current, setCurrent] = useState(setting);
  const [text, setText] = useState(() => pretty(setting.value));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  let parsed: unknown;
  let parseError: string | null = null;
  try {
    parsed = JSON.parse(text);
  } catch {
    parseError = 'Некорректный JSON: проверьте кавычки, запятые и скобки';
  }
  const dirty = !parseError && JSON.stringify(parsed) !== JSON.stringify(current.value);
  const multiline = typeof current.value === 'object' && current.value !== null;

  async function save() {
    setBusy(true);
    setError(null);
    setSaved(false);
    try {
      const next = await api.admin.updateSetting(current.key, parsed);
      setCurrent(next);
      setText(pretty(next.value));
      setSaved(true);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Не удалось сохранить настройку');
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card
      title={<span className="mono" style={{ textTransform: 'none' }}>{current.key}</span>}
      actions={current.updatedAt ? <span className="dim small">изменено {formatDateTime(current.updatedAt)}</span> : undefined}
    >
      {current.description && <p className="muted small" style={{ marginBottom: 8 }}>{current.description}</p>}
      <label className="field">
        <span className="sr-only">Значение настройки {current.key} в формате JSON</span>
        {multiline ? (
          <textarea
            className="textarea mono"
            rows={Math.min(12, text.split('\n').length + 1)}
            value={text}
            spellCheck={false}
            onChange={(e) => { setText(e.target.value); setSaved(false); }}
          />
        ) : (
          <input className="input mono" value={text} spellCheck={false} onChange={(e) => { setText(e.target.value); setSaved(false); }} />
        )}
        {parseError && <span className="field__error">{parseError}</span>}
      </label>
      <div className="row" style={{ marginTop: 8 }}>
        <button type="button" className="btn btn--primary btn--sm" disabled={!dirty || busy} onClick={() => void save()}>
          {busy ? 'Сохранение…' : 'Сохранить'}
        </button>
        {dirty && (
          <button type="button" className="btn btn--sm" disabled={busy} onClick={() => setText(pretty(current.value))}>Отменить</button>
        )}
        {saved && <span className="small" role="status" style={{ color: 'var(--u-ok)' }}>Сохранено</span>}
        {error && <span className="field__error" role="alert">{error}</span>}
      </div>
    </Card>
  );
}
