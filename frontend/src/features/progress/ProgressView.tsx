import { useState } from 'react';
import { api } from '../../shared/api';
import { Badge, Card } from '../../components/ui';
import { formatDateTime, formatDuration } from '../../shared/utils/time';
import { saveBlob } from '../../shared/utils/download';
import { formatNumber, formatPct } from '../../shared/utils/format';
import type { StudentProgress } from '../../shared/types';

const XP_REASON: Record<string, string> = {
  attempt_evaluated: 'Карточка оценена',
  within_norm: 'Уложился в норматив',
  pass_bonus: 'Бонус за зачёт',
  lesson_completed: 'Занятие пройдено',
  streak: 'Серия занятий',
  manual: 'Начислено преподавателем',
};

const KIND_LABEL: Record<string, string> = { missing: 'не заполнено', wrong: 'неверно', extra: 'лишнее' };

/**
 * Прогресс обучающегося: уровень и опыт, итоги, результаты по категориям,
 * история ошибок в полях, рекомендации и журнал начислений. Один экран и
 * для самого обучающегося, и для преподавателя.
 */
export function ProgressView({ progress: p }: { progress: StudentProgress }) {
  const lvl = p.level;
  const next = lvl.nextXpRequired;
  const base = lvl.xpRequired ?? 0;
  // Доля пути от текущего уровня к следующему; на максимальном уровне шкала полная.
  const toNext = next != null && next > base ? Math.min(100, Math.max(0, ((p.xp - base) / (next - base)) * 100)) : 100;

  return (
    <div className="stack">
      <Card>
        <div className="level">
          <div className="level__badge" aria-hidden="true">{lvl.no}</div>
          <div className="level__text">
            <div className="summary__label">Уровень {lvl.no}</div>
            <div className="level__title">{lvl.title}</div>
            <div
              className="level__bar"
              role="progressbar"
              aria-label={next != null ? `Опыт до уровня «${lvl.nextTitle ?? lvl.no + 1}»` : 'Максимальный уровень'}
              aria-valuemin={base}
              aria-valuemax={next ?? p.xp}
              aria-valuenow={p.xp}
            >
              <span style={{ width: `${toNext}%` }} />
            </div>
            <div className="small muted">
              {next != null
                ? <>{p.xp} XP из {next} · до уровня «{lvl.nextTitle ?? lvl.no + 1}» осталось {Math.max(0, next - p.xp)} XP</>
                : <>{p.xp} XP · максимальный уровень</>}
            </div>
          </div>
        </div>
      </Card>

      <div className="summary">
        <Item label="Оценённых карточек" value={p.attemptsDone} />
        <Item label="Средний балл" value={p.avgScore != null ? formatNumber(p.avgScore) : '—'} note={p.avgScore30d != null ? `за 30 дней: ${formatNumber(p.avgScore30d)}` : undefined} />
        <Item label="Зачёт" value={p.passRatePct != null ? formatPct(p.passRatePct) : '—'} />
        <Item label="Среднее время" value={p.avgTimeMs != null ? formatDuration(p.avgTimeMs) : '—'} />
        <Item label="В нормативе" value={p.withinNormCount != null ? `${p.withinNormCount} из ${p.attemptsDone}` : '—'} />
        <Item label="Последняя активность" value={p.lastActivityAt ? formatDateTime(p.lastActivityAt).slice(0, 10) : '—'} />
      </div>

      {p.recommendations.length > 0 && (
        <Card title="Рекомендации">
          <div className="stack stack--tight">
            {p.recommendations.map((r) => (
              <div key={r.id}>
                <p>{r.body}</p>
                {r.items && r.items.length > 0 && (
                  <ul style={{ margin: '4px 0 0', paddingLeft: 18 }}>
                    {r.items.map((i) => <li key={i}>{i}</li>)}
                  </ul>
                )}
              </div>
            ))}
          </div>
        </Card>
      )}

      <div className="grid grid--2">
        <Card title="По категориям происшествий">
          {p.categories.length === 0 ? (
            <p className="muted small">Оценённых карточек пока нет.</p>
          ) : (
            <div className="table-scroll">
              <table className="table">
                <thead>
                  <tr><th>Категория</th><th className="num">Карточек</th><th className="num">Балл</th><th className="num">Зачёт</th><th className="num">Время</th></tr>
                </thead>
                <tbody>
                  {p.categories.map((c) => (
                    <tr key={c.categoryId}>
                      <td>{c.categoryName}</td>
                      <td className="num">{c.attemptsDone}</td>
                      <td className="num">{c.avgScore != null ? formatNumber(c.avgScore) : '—'}</td>
                      <td className="num">{c.passRatePct != null ? formatPct(c.passRatePct) : '—'}</td>
                      <td className="num nowrap">{c.avgTimeMs != null ? formatDuration(c.avgTimeMs) : '—'}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>

        <Card title="История ошибок в полях">
          {p.fieldErrors.length === 0 ? (
            <p className="muted small">Ошибок в полях карточки не было.</p>
          ) : (
            <div className="table-scroll">
              <table className="table">
                <thead><tr><th>Поле</th><th>Ошибка</th><th className="num">Раз</th></tr></thead>
                <tbody>
                  {p.fieldErrors.map((e) => (
                    <tr key={`${e.field}-${e.kind}`}>
                      <td>{e.label}</td>
                      <td className="muted small nowrap">{KIND_LABEL[e.kind] ?? e.kind}</td>
                      <td className="num">{e.count}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>
      </div>

      <Card title="Начисления опыта">
        {p.xpLog.length === 0 ? (
          <p className="muted small">Начислений пока нет.</p>
        ) : (
          <div className="table-scroll table-scroll--limited">
            <table className="table">
              <thead><tr><th>Когда</th><th>За что</th><th className="num">XP</th></tr></thead>
              <tbody>
                {p.xpLog.map((x, i) => (
                  <tr key={`${x.at}-${x.reason}-${i}`}>
                    <td className="nowrap small">{formatDateTime(x.at)}</td>
                    <td>{XP_REASON[x.reason] ?? 'Начисление'}</td>
                    <td className="num"><Badge tone={x.delta >= 0 ? 'ok' : 'danger'} value>{x.delta > 0 ? `+${x.delta}` : x.delta}</Badge></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </div>
  );
}

function Item({ label, value, note }: { label: string; value: React.ReactNode; note?: string }) {
  return (
    <div className="summary__item">
      <span className="summary__label">{label}</span>
      <span className="summary__value">{value}</span>
      {note && <span className="summary__note">{note}</span>}
    </div>
  );
}

/**
 * «Скачать сертификат (PDF)». Файл формирует сервер; 409 — оценённых карточек
 * ещё нет, это объясняется текстом у кнопки, а не скачанным JSON.
 */
export function CertificateButton({ userId, disabled }: { userId: string; disabled?: boolean }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  return (
    <span className="row row--tight">
      <button
        type="button"
        className="btn btn--primary"
        disabled={busy || disabled}
        title={disabled ? 'Сертификат выдаётся после первой оценённой карточки' : undefined}
        onClick={() => {
          setBusy(true);
          setError(null);
          api.users
            .certificate(userId)
            .then((f) => saveBlob(f.blob, f.fileName))
            .catch((e: unknown) => setError(e instanceof Error ? e.message : 'Не удалось получить сертификат'))
            .finally(() => setBusy(false));
        }}
      >
        {busy ? 'Формирование…' : 'Скачать сертификат (PDF)'}
      </button>
      {error && <span className="field__error" role="alert">{error}</span>}
    </span>
  );
}
