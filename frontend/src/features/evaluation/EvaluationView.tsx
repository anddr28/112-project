import { Badge, Card, Meter, Metric } from '../../components/ui';
import { scoreTone } from '../../shared/utils/score';
import { formatDelta, formatDuration } from '../../shared/utils/time';
import type { Evaluation, LessonSettings } from '../../shared/types';

/**
 * Отображение результата проверки.
 *
 * docs/contracts.md: go-core считает поля и тайминг мгновенно (`partial`),
 * а грамматика и семантика доезжают callback'ом от ai-service. Поэтому
 * слои рендерятся независимо: готовые показывают балл, недосчитанные —
 * состояние «Анализируется…».
 *
 * Отдельно поддержаны два документированных состояния:
 *   — `confidence < 0.7` → «требует ревью преподавателя»;
 *   — исчерпаны ретраи AI-задачи → «AI-слой недоступен».
 */
export function EvaluationView({
  evaluation,
  threshold,
  weights,
}: {
  evaluation: Evaluation;
  threshold: number;
  /**
   * Веса слоёв из настроек занятия. Подписи «· 50%» обязаны совпадать
   * с коэффициентами, по которым посчитан итоговый балл, поэтому берутся
   * из той же конфигурации, а не из константы в разметке.
   */
  weights?: LessonSettings['weights'];
}) {
  const ev = evaluation;
  const pending = ev.status === 'partial' || ev.status === 'pending';

  return (
    <div className="stack" style={{ gap: 16 }}>
      <div className="grid grid--4">
        <ScoreTile label="Поля карточки" weight={weights?.fields} score={ev.fieldsScore} threshold={threshold} pending={pending} />
        <ScoreTile label="Семантика" weight={weights?.semantic} score={ev.semanticScore} threshold={threshold} pending={pending} />
        <ScoreTile label="Грамматика" weight={weights?.grammar} score={ev.grammarScore} threshold={threshold} pending={pending} />
        <ScoreTile label="Время" weight={weights?.timing} score={ev.timingScore} threshold={threshold} pending={pending} />
      </div>

      {pending && (
        <div className="card" style={{ borderColor: 'var(--u-accent)' }}>
          <div className="card__body row" style={{ gap: 10 }}>
            <div className="spinner" />
            <div>
              <b>Проверка продолжается</b>
              <div className="muted small">
                Поля и время посчитаны сразу. Грамматика и семантика обрабатываются
                моделью — результат появится здесь автоматически.
              </div>
            </div>
          </div>
        </div>
      )}

      {ev.needsReview && (
        <div className="card" style={{ borderColor: 'var(--u-warn)' }}>
          <div className="card__body">
            <b>Требуется ревью преподавателя</b>
            <div className="muted small">
              Уверенность модели в семантической оценке ниже порога
              {ev.semantic ? ` (${Math.round(ev.semantic.confidence * 100)}%)` : ''}.
              Итоговый балл может быть скорректирован после проверки.
            </div>
          </div>
        </div>
      )}

      {ev.aiUnavailable && (
        <div className="card" style={{ borderColor: 'var(--u-danger)' }}>
          <div className="card__body">
            <b>AI-слой недоступен</b>
            <div className="muted small">
              Автоматическая проверка текста не выполнена. Попытку проверит преподаватель.
            </div>
          </div>
        </div>
      )}

      {ev.override && (
        <div className="card" style={{ borderColor: 'var(--u-warn)' }}>
          <div className="card__body">
            <b>Оценка скорректирована преподавателем вручную</b>
            <div className="grid grid--3" style={{ marginTop: 10 }}>
              <div>
                <div className="field__label">Автоматическая оценка</div>
                <div className="mono" style={{ fontSize: 'var(--u-fs-lg)' }}>{ev.totalScore}</div>
              </div>
              <div>
                <div className="field__label">Ручная корректировка</div>
                <div className="mono" style={{ fontSize: 'var(--u-fs-lg)', color: 'var(--u-warn)' }}>{ev.override.score}</div>
              </div>
              <div>
                <div className="field__label">Преподаватель</div>
                <div className="small">{ev.override.by}</div>
              </div>
            </div>
            <div style={{ marginTop: 10 }}>
              <div className="field__label">Причина</div>
              <div className="small">{ev.override.reason}</div>
            </div>
          </div>
        </div>
      )}

      <div className="grid grid--2">
        <Card title="Ошибки заполнения полей">
          {ev.fieldErrors.length === 0 ? (
            <p className="muted small">Обязательные поля заполнены без замечаний.</p>
          ) : (
            <table className="table">
              <thead><tr><th>Поле</th><th>Тип</th><th>Ожидалось</th></tr></thead>
              <tbody>
                {ev.fieldErrors.map((e) => (
                  <tr key={e.field}>
                    <td>{e.label}</td>
                    <td>
                      <Badge tone={e.kind === 'missing' ? 'danger' : 'warn'}>
                        {e.kind === 'missing' ? 'не заполнено' : e.kind === 'wrong' ? 'не совпадает' : 'лишнее'}
                      </Badge>
                    </td>
                    <td className="muted small">{renderValue(e.expected)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Card>

        <Card title="Время обработки">
          <div className="grid grid--2" style={{ marginBottom: 12 }}>
            <Metric label="Затрачено" value={formatDuration(ev.timing.spentMs)} />
            <Metric label="Норматив" value={`${ev.timing.limitSec} с`} />
          </div>
          <div className="stack" style={{ gap: 8 }}>
            <div className="row row--between">
              <span className="muted small">Отклонение от норматива</span>
              <Badge tone={ev.timing.withinNorm ? 'ok' : 'danger'} value>{formatDelta(ev.timing.deltaMs)}</Badge>
            </div>
            <div className="row row--between">
              <span className="muted small">Время реакции (до первого ввода)</span>
              <span className="mono small">{ev.timing.reactionMs > 0 ? formatDuration(ev.timing.reactionMs) : '—'}</span>
            </div>
            <div className="row row--between">
              <span className="muted small">В пределах норматива</span>
              <span>{ev.timing.withinNorm ? 'да' : 'нет'}</span>
            </div>
          </div>
        </Card>
      </div>

      <div className="grid grid--2">
        <Card title="Смысловая полнота">
          {!ev.semantic ? (
            pending ? <PendingLayer /> : <p className="muted small">Оценка не выполнена.</p>
          ) : (
            <div className="stack" style={{ gap: 12 }}>
              {ev.semantic.summaryForStudent && (
                <p>{ev.semantic.summaryForStudent}</p>
              )}
              {(ev.semantic.missingFacts?.length ?? 0) > 0 && (
                <div>
                  <div className="field__label">Не зафиксировано</div>
                  <ul style={{ margin: '4px 0 0', paddingLeft: 18 }}>
                    {ev.semantic.missingFacts?.map((f) => <li key={f}>{f}</li>)}
                  </ul>
                </div>
              )}
              {(ev.semantic.extraFacts?.length ?? 0) > 0 && (
                <div>
                  <div className="field__label">Отсутствует в обращении (домыслы)</div>
                  <ul style={{ margin: '4px 0 0', paddingLeft: 18 }}>
                    {ev.semantic.extraFacts?.map((f) => <li key={f}>{f}</li>)}
                  </ul>
                </div>
              )}
            </div>
          )}
        </Card>

        <Card title="Замечания к тексту">
          {!ev.grammar ? (
            pending ? <PendingLayer /> : (
              <p className="muted small">
                Свободный текст в карточке не заполнен — проверка не проводилась.
              </p>
            )
          ) : ev.grammar.remarks.length === 0 ? (
            <p className="muted small">Замечаний нет. Проверено слов: {ev.grammar.stats.wordsChecked}.</p>
          ) : (
            <div className="stack" style={{ gap: 8 }}>
              {ev.grammar.remarks.map((r, i) => (
                <div key={`${r.rule}-${i}`} className="row" style={{ alignItems: 'flex-start', gap: 8 }}>
                  <Badge tone={r.severity === 'error' ? 'danger' : r.severity === 'warning' ? 'warn' : 'neutral'}>
                    {r.severity === 'error' ? 'ошибка' : r.severity === 'warning' ? 'замечание' : 'стиль'}
                  </Badge>
                  <div style={{ fontSize: 13 }}>
                    {r.message}
                    {(r.suggestions?.length ?? 0) > 0 && (
                      <span className="dim"> → {r.suggestions?.join(', ')}</span>
                    )}
                  </div>
                </div>
              ))}
            </div>
          )}
        </Card>
      </div>

      {ev.recommendations.length > 0 && (
        <Card title="Рекомендации">
          <div className="stack stack--tight">
            {ev.recommendations.map((r) => (
              <div key={r.id}>
                <div>{r.body}</div>
                {r.items && r.items.length > 0 && (
                  <ul style={{ margin: '4px 0 0', paddingLeft: 18 }}>
                    {r.items.map((item) => <li key={item}>{item}</li>)}
                  </ul>
                )}
              </div>
            ))}
          </div>
        </Card>
      )}
    </div>
  );
}

function ScoreTile({
  label,
  weight,
  score,
  threshold,
  pending,
}: {
  label: string;
  /** доля слоя в итоговом балле, 0..1; undefined — настройки ещё не получены */
  weight?: number;
  score?: number;
  threshold: number;
  /** проверка ещё идёт: отличает «считается» от «оценивать было нечего» */
  pending: boolean;
}) {
  const caption = weight == null ? label : `${label} · ${Math.round(weight * 100)}%`;

  if (score == null) {
    return (
      <div className="metric">
        <span className="metric__label">{caption}</span>
        <span className="metric__value" style={{ fontSize: 'var(--u-fs-md)', color: 'var(--u-text-3)' }}>
          {pending ? 'Анализируется…' : 'Не оценивалось'}
        </span>
        {pending ? (
          <div className="meter"><div className="meter__fill" style={{ width: '30%', opacity: 0.4 }} /></div>
        ) : (
          <span className="metric__note">в итоговом балле не учтено</span>
        )}
      </div>
    );
  }
  const tone = scoreTone(score, threshold);
  return (
    <div className="metric">
      <span className="metric__label">{caption}</span>
      <span className="metric__value" style={{ color: `var(--u-${tone === 'ok' ? 'ok' : tone === 'warn' ? 'warn' : 'danger'})` }}>
        {score}
      </span>
      <Meter value={score} tone={tone} />
    </div>
  );
}

function PendingLayer() {
  return (
    <div className="row" style={{ gap: 10 }}>
      <div className="spinner" />
      <span className="muted small">Анализируется…</span>
    </div>
  );
}

function renderValue(v: unknown): string {
  if (v == null || v === '') return '—';
  if (Array.isArray(v)) return v.join(', ');
  if (typeof v === 'object') return '—';
  if (typeof v === 'boolean') return v ? 'да' : 'нет';
  return String(v);
}
