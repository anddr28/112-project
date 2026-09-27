import { Badge, Card, Meter, Metric } from '../../components/ui';
import { DialogueLayer } from './DialogueLayer';
import { cls } from '../../shared/utils/cls';
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
   * Запасной источник весов — настройки занятия. Используется, только если
   * сама оценка их не принесла: занятие могло переопределить веса уже после
   * проверки, и тогда подписи разошлись бы с формулой.
   */
  weights?: LessonSettings['weights'];
}) {
  const ev = evaluation;
  const pending = ev.status === 'partial' || ev.status === 'pending';

  // Веса той оценки, которую показываем, важнее текущих настроек занятия.
  const used = ev.weights ?? weights;
  const dialogueLayer = ev.layers?.dialogue;
  /*
   * Ракурс ДДС (v1.3): слой «поля» считается не по карточке, а по протоколу
   * реагирования своей службы — решение, время решения, отказ, статусы.
   */
  const reaction = ev.engine?.fields?.source === 'dds_reaction';

  /*
   * Слой разговора показываем по фактическим данным оценки, а не по настройкам
   * занятия: у попытки без разговора пятая плитка появляться не должна.
   */
  const hasDialogue =
    ev.dialogue != null ||
    ev.dialogueScore != null ||
    (dialogueLayer != null && dialogueLayer !== 'skipped') ||
    (used?.dialogue ?? 0) > 0;

  return (
    <div className="stack" style={{ gap: 16 }}>
      <div className={cls('grid', hasDialogue ? 'grid--5' : 'grid--4')}>
        <ScoreTile label={reaction ? 'Протокол реагирования' : 'Поля карточки'} weight={used?.fields} score={ev.fieldsScore} threshold={threshold} pending={pending} />
        <ScoreTile label="Семантика" weight={used?.semantic} score={ev.semanticScore} threshold={threshold} pending={pending} />
        <ScoreTile label="Грамматика" weight={used?.grammar} score={ev.grammarScore} threshold={threshold} pending={pending} />
        <ScoreTile label="Время" weight={used?.timing} score={ev.timingScore} threshold={threshold} pending={pending} />
        {hasDialogue && (
          <ScoreTile
            label="Разговор"
            weight={used?.dialogue}
            score={ev.dialogueScore}
            threshold={threshold}
            pending={dialogueLayer === 'queued' || dialogueLayer === 'running'}
            failed={dialogueLayer === 'failed'}
          />
        )}
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
              {lowConfidenceText(ev)} Итоговый балл может быть скорректирован после проверки.
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
        <Card title={reaction ? 'Замечания по протоколу реагирования' : 'Ошибки заполнения полей'}>
          {ev.fieldErrors.length === 0 ? (
            <p className="muted small">
              {reaction ? 'Решение и статусы реагирования проставлены без замечаний.' : 'Обязательные поля заполнены без замечаний.'}
            </p>
          ) : (
            <table className="table">
              <thead>
                <tr>
                  <th>{reaction ? 'Требование' : 'Поле'}</th>
                  <th>Тип</th>
                  <th>Ожидалось</th>
                  {reaction && <th>Факт</th>}
                </tr>
              </thead>
              <tbody>
                {/* ключ с индексом: у протокола ДДС одно поле (reaction.statuses) повторяется по статусам */}
                {ev.fieldErrors.map((e, i) => (
                  <tr key={`${e.field}-${i}`}>
                    <td>{e.label}</td>
                    <td>
                      <Badge tone={e.kind === 'missing' ? 'danger' : 'warn'}>
                        {e.kind === 'missing'
                          ? reaction ? 'не проставлено' : 'не заполнено'
                          : e.kind === 'wrong' ? 'не совпадает' : reaction ? 'лишнее действие' : 'лишнее'}
                      </Badge>
                    </td>
                    <td className="muted small">{renderValue(e.expected)}</td>
                    {reaction && <td className="muted small">{renderValue(e.actual)}</td>}
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

      {ev.dialogue && <DialogueLayer dialogue={ev.dialogue} />}

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

/** Текст причины ревью: называем слой, чья уверенность ниже порога. */
function lowConfidenceText(ev: Evaluation): string {
  const low: string[] = [];
  if (ev.semantic && ev.semantic.confidence < 0.7) {
    low.push(`смысловой полноте (${Math.round(ev.semantic.confidence * 100)}%)`);
  }
  if (ev.dialogue && ev.dialogue.confidence < 0.7) {
    low.push(`оценке разговора (${Math.round(ev.dialogue.confidence * 100)}%)`);
  }
  return low.length > 0
    ? `Уверенность модели ниже порога в ${low.join(' и ')}.`
    : 'Оценка помечена как требующая проверки преподавателем.';
}

function ScoreTile({
  label,
  weight,
  score,
  threshold,
  pending,
  failed,
}: {
  label: string;
  /** доля слоя в итоговом балле, 0..1; undefined — настройки ещё не получены */
  weight?: number;
  score?: number;
  threshold: number;
  /** проверка ещё идёт: отличает «считается» от «оценивать было нечего» */
  pending: boolean;
  /** слой не удалось посчитать — отличается от «оценивать было нечего» */
  failed?: boolean;
}) {
  const caption = weight == null ? label : `${label} · ${Math.round(weight * 100)}%`;

  if (score == null) {
    return (
      <div className="metric">
        <span className="metric__label">{caption}</span>
        <span
          className="metric__value"
          style={{ fontSize: 'var(--u-fs-md)', color: failed ? 'var(--u-danger)' : 'var(--u-text-3)' }}
        >
          {failed ? 'Не удалось посчитать' : pending ? 'Анализируется…' : 'Не оценивалось'}
        </span>
        {pending ? (
          <div className="meter"><div className="meter__fill" style={{ width: '30%', opacity: 0.4 }} /></div>
        ) : (
          <span className="metric__note">
            {failed ? 'требуется проверка преподавателем' : 'в итоговом балле не учтено'}
          </span>
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
