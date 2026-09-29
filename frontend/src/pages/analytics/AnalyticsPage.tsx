import { Link, useSearchParams } from 'react-router-dom';
import { api } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { useAuth } from '../../app/auth';
import { Card, ErrorState, Loading } from '../../components/ui';
import { TrendChart } from '../../features/analytics/TrendChart';
import { ErrorHeatmap } from '../../features/analytics/ErrorHeatmap';
import { FieldErrorsTable, InsightCard, LayerBars } from '../../features/analytics/parts';
import { LAYER_LABEL } from '../../features/analytics/labels';
import { formatDateTime, formatDuration } from '../../shared/utils/time';
import { shortName } from '../../shared/utils/user';
import { formatNumber, formatPct } from '../../shared/utils/format';
import type { AnalyticsFilter } from '../../shared/api';

const PERIODS = [7, 30, 90, 365];

/**
 * Аналитика группы (v1.4): тепловая карта ошибок, динамика, слои оценки,
 * типичные ошибки и инсайты с рекомендациями преподавателю.
 *
 * Фильтры — в адресе страницы: ссылку «аналитика этого занятия» можно
 * открыть из занятия, а обновление страницы не сбрасывает выбор. Пока
 * данные перезагружаются, старые остаются на экране приглушёнными.
 */
export function AnalyticsPage() {
  const role = useAuth((s) => s.user?.role);
  const [params, setParams] = useSearchParams();
  const filter: AnalyticsFilter = {
    lessonId: params.get('lessonId') || undefined,
    studentId: params.get('studentId') || undefined,
    categoryId: params.get('categoryId') || undefined,
    days: Number(params.get('days')) || 30,
  };
  const key = JSON.stringify(filter);
  const overview = useAsync(() => api.analytics.overview(filter), [key]);
  const lessons = useAsync(() => api.lessons.list(), []);
  const users = useAsync(() => api.users.list(), []);
  const types = useAsync(() => api.classifier.incidentTypes(), []);

  function setFilter(name: keyof AnalyticsFilter, value: string) {
    const next = new URLSearchParams(params);
    if (value) next.set(name, value);
    else next.delete(name);
    setParams(next, { replace: true });
  }

  const students = (users.data ?? []).filter((u) => u.role === 'student');
  const o = overview.data;
  const progressLink = role === 'teacher';

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <h1>Аналитика группы</h1>
          <div className="page-head__sub">
            По оценённым карточкам{role === 'teacher' ? ' ваших занятий' : ' всех занятий'}: где группа ошибается и что с этим делать.
          </div>
        </div>
      </div>

      <div className="filters" role="group" aria-label="Фильтры аналитики">
        <label className="filters__item">
          <span className="field__label">Период</span>
          <select className="select" value={filter.days} onChange={(e) => setFilter('days', e.target.value)}>
            {PERIODS.map((d) => <option key={d} value={d}>{d === 365 ? 'Год' : `${d} дней`}</option>)}
          </select>
        </label>
        <label className="filters__item filters__item--wide">
          <span className="field__label">Занятие</span>
          <select className="select" value={filter.lessonId ?? ''} onChange={(e) => setFilter('lessonId', e.target.value)}>
            <option value="">Все занятия</option>
            {(lessons.data ?? []).map((l) => <option key={l.id} value={l.id}>{l.title}</option>)}
          </select>
        </label>
        <label className="filters__item">
          <span className="field__label">Обучающийся</span>
          <select className="select" value={filter.studentId ?? ''} onChange={(e) => setFilter('studentId', e.target.value)}>
            <option value="">Все</option>
            {students.map((u) => <option key={u.id} value={u.id}>{shortName(u)}</option>)}
          </select>
        </label>
        <label className="filters__item">
          <span className="field__label">Категория</span>
          <select className="select" value={filter.categoryId ?? ''} onChange={(e) => setFilter('categoryId', e.target.value)}>
            <option value="">Все категории</option>
            {(types.data ?? []).filter((t) => t.depth <= 2).map((t) => <option key={t.id} value={t.id}>{t.name}</option>)}
          </select>
        </label>
      </div>

      {overview.loading && !o ? (
        <Loading text="Считаем аналитику…" />
      ) : overview.error && !o ? (
        <ErrorState text={overview.error} onRetry={overview.reload} />
      ) : o ? (
        <div className={overview.refreshing ? 'is-refreshing stack' : 'stack'} aria-busy={overview.refreshing}>
          {overview.error && <p className="field__error" role="alert">{overview.error}</p>}

          <div className="summary">
            <Tile label="Карточек" value={o.summary.attempts} />
            <Tile label="Обучающихся" value={o.summary.students} />
            <Tile label="Средний балл" value={formatNumber(o.summary.avgScore)} />
            <Tile label="Зачёт" value={formatPct(o.summary.passRatePct)} />
            <Tile label="Среднее время" value={formatDuration(o.summary.avgTimeMs)} />
            {o.summary.avgReactionMs != null && <Tile label="Реакция" value={formatDuration(o.summary.avgReactionMs)} note="до первого ввода" />}
            <Tile label="В нормативе" value={formatPct(o.summary.withinNormPct)} />
            <Tile label="Ждут ревью" value={o.summary.needsReview} tone={o.summary.needsReview ? 'warn' : undefined} />
          </div>

          {o.summary.attempts === 0 ? (
            <Card><p className="muted">За выбранный период оценённых карточек нет. Расширьте период или снимите фильтры.</p></Card>
          ) : (
            <>
              {o.insights.length > 0 && (
                <section aria-labelledby="insights-title">
                  <h2 id="insights-title" className="section-title">Инсайты и рекомендации</h2>
                  <div className="insights">
                    {o.insights.map((i) => <InsightCard key={i.id} insight={i} />)}
                  </div>
                </section>
              )}

              <div className="grid grid--sidebar-wide">
                <Card title="Динамика по дням">
                  <TrendChart data={o.trend} />
                </Card>
                <Card title="Средний балл по слоям оценки">
                  <LayerBars layers={o.layers} />
                </Card>
              </div>

              <Card title="Тепловая карта ошибок: категория × поле карточки">
                <ErrorHeatmap heatmap={o.heatmap} />
              </Card>

              <div className="grid grid--3">
                <Card title="Типичные ошибки в полях">
                  <div className="table-scroll"><FieldErrorsTable rows={o.topFieldErrors} /></div>
                </Card>
                <Card title="Забытые факты">
                  {o.topMissingFacts.length === 0 ? (
                    <p className="muted small">Все факты из легенды отражены.</p>
                  ) : (
                    <table className="table">
                      <thead><tr><th>Факт</th><th className="num">Доля</th></tr></thead>
                      <tbody>
                        {o.topMissingFacts.map((f) => (
                          <tr key={f.fact}><td>{f.fact}</td><td className="num">{formatPct(f.sharePct)}</td></tr>
                        ))}
                      </tbody>
                    </table>
                  )}
                </Card>
                <Card title="Повторяющиеся правила грамматики">
                  {o.topGrammarRules.length === 0 ? (
                    <p className="muted small">Замечаний нет.</p>
                  ) : (
                    <table className="table">
                      <thead><tr><th>Замечание</th><th className="num">Раз</th></tr></thead>
                      <tbody>
                        {o.topGrammarRules.map((g) => (
                          <tr key={g.rule}>
                            <td>
                              {g.message}
                              {g.example && <div className="dim small">«{g.example}»</div>}
                            </td>
                            <td className="num">{g.count}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  )}
                </Card>
              </div>

              <div className="grid grid--2">
                <Card title="Обучающиеся">
                  <div className="table-scroll">
                    <table className="table">
                      <thead>
                        <tr><th>ФИО</th><th className="num">Карточек</th><th className="num">Балл</th><th className="num">Зачёт</th><th>Слабый слой</th></tr>
                      </thead>
                      <tbody>
                        {o.students.map((s) => (
                          <tr key={s.userId}>
                            <td className="nowrap">
                              {progressLink ? <Link to={`/teacher/students/${encodeURIComponent(s.userId)}`}>{s.name}</Link> : s.name}
                            </td>
                            <td className="num">{s.attempts}</td>
                            <td className="num">{formatNumber(s.avgScore)}</td>
                            <td className="num">{formatPct(s.passRatePct)}</td>
                            <td className="muted small">{s.weakestLayer ? LAYER_LABEL[s.weakestLayer] ?? s.weakestLayer : '—'}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                </Card>
                <Card title="Категории происшествий">
                  <div className="table-scroll">
                    <table className="table">
                      <thead>
                        <tr><th>Категория</th><th className="num">Карточек</th><th className="num">Балл</th><th className="num">Зачёт</th><th className="num">Время</th></tr>
                      </thead>
                      <tbody>
                        {o.categories.map((c) => (
                          <tr key={c.categoryId}>
                            <td>{c.categoryName}</td>
                            <td className="num">{c.attempts}</td>
                            <td className="num">{formatNumber(c.avgScore)}</td>
                            <td className="num">{formatPct(c.passRatePct)}</td>
                            <td className="num nowrap">{c.avgTimeMs != null ? formatDuration(c.avgTimeMs) : '—'}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                </Card>
              </div>
            </>
          )}

          <p className="dim small">Сформировано: {formatDateTime(o.generatedAt)}</p>
        </div>
      ) : null}
    </>
  );
}

function Tile({ label, value, note, tone }: { label: string; value: React.ReactNode; note?: string; tone?: 'warn' }) {
  return (
    <div className="summary__item">
      <span className="summary__label">{label}</span>
      <span className={`summary__value${tone ? ` summary__value--${tone}` : ''}`}>{value}</span>
      {note && <span className="summary__note">{note}</span>}
    </div>
  );
}
