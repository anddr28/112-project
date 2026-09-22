import { useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { api } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { Badge, Card, DifficultyBadge, ErrorState, Field, Loading, ScenarioStatusBadge } from '../../components/ui';
import { EtalonCardView } from '../../features/incident-card/EtalonCardView';
import { ScenarioEditor } from '../../features/scenario/ScenarioEditor';
import { formatDateTime } from '../../shared/utils/time';
import { labelForPath } from '../../shared/utils/labels';

export function ScenarioDetailPage() {
  const { scenarioId = '' } = useParams();
  const scenario = useAsync(() => api.scenarios.get(scenarioId), [scenarioId]);
  const labels = useAsync(() => api.classifier.labels(), []);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [title, setTitle] = useState<string | null>(null);
  const [editing, setEditing] = useState(false);

  if (scenario.loading) return <Loading />;
  if (scenario.error) return <ErrorState text={scenario.error} onRetry={scenario.reload} />;
  if (!scenario.data) return <ErrorState text="Сценарий не найден" />;

  const s = scenario.data;
  const editable = s.status === 'draft' || s.status === 'generated';

  /** Операции сервера: отказ показываем, иначе кнопка осталась бы заблокированной. */
  async function run(action: () => Promise<unknown>, failure: string, after?: () => void) {
    setBusy(true);
    setActionError(null);
    try {
      await action();
      after?.();
      scenario.reload();
    } catch (e) {
      setActionError(e instanceof Error ? e.message : failure);
    } finally {
      setBusy(false);
    }
  }

  async function saveTitle() {
    if (title === null) return;
    const next = title;
    await run(
      () => api.scenarios.update(scenarioId, { title: next }),
      'Не удалось сохранить название',
      () => setTitle(null),
    );
  }

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <div className="row row--tight" style={{ marginBottom: 6 }}>
            <Link to="/teacher/scenarios" className="small">← Сценарии</Link>
          </div>
          <h1>{s.title}</h1>
          <div className="row row--tight" style={{ marginTop: 8 }}>
            <ScenarioStatusBadge status={s.status} />
            <DifficultyBadge level={s.difficulty} />
            <Badge tone="neutral">{s.categoryName}</Badge>
            <Badge tone="neutral">Эталон, версия {s.etalonVersion}</Badge>
            {s.source === 'generated' && <Badge tone="warn">Нейросеть</Badge>}
          </div>
        </div>

        <div className="page-head__actions">
          {editable && !editing && (
            <button type="button" className="btn" onClick={() => setEditing(true)}>
              Редактировать сценарий
            </button>
          )}
          {/*
            * Подтверждать нужно и созданный вручную черновик: без этого
            * сценарий не попадёт в занятие — туда берутся только
            * подтверждённые.
            */}
          {editable && !editing && (
            <button
              type="button"
              className="btn btn--primary"
              onClick={() => void run(() => api.scenarios.approve(scenarioId), 'Не удалось подтвердить сценарий')}
              disabled={busy}
            >
              Подтвердить сценарий
            </button>
          )}
          {s.status === 'validated' && (
            <Link className="btn" to="/teacher/lessons">Создать занятие</Link>
          )}
        </div>
      </div>

      {actionError && <p className="field__error" role="alert" style={{ marginBottom: 12 }}>{actionError}</p>}

      {s.status === 'generated' && (
        <div className="card" style={{ marginBottom: 16, borderColor: 'var(--u-warn)' }}>
          <div className="card__body">
            <b>Требуется проверка преподавателя.</b>
            <p className="muted small" style={{ marginTop: 4 }}>
              {s.notesForTeacher ?? 'Сценарий сгенерирован нейросетью. Проверьте легенду звонка и эталон перед выдачей обучающимся.'}
            </p>
          </div>
        </div>
      )}

      {editing ? (
        <ScenarioEditor
          scenario={s}
          onSaved={() => {
            setEditing(false);
            scenario.reload();
          }}
          onCancel={() => setEditing(false)}
        />
      ) : (
      <div className="grid grid--sidebar">
        <div className="stack">
          <Card title="Легенда звонка">
            <div className="grid grid--3" style={{ marginBottom: 14 }}>
              <div>
                <div className="field__label">Заявитель</div>
                <div>{s.callScript.caller.name ?? '—'}</div>
              </div>
              <div>
                <div className="field__label">Роль</div>
                <div>{s.callScript.caller.role ?? '—'}</div>
              </div>
              <div>
                <div className="field__label">Состояние</div>
                <div>{s.callScript.caller.emotionalState ?? '—'}</div>
              </div>
            </div>

            <div className="field__label" style={{ marginBottom: 6 }}>Реплики</div>
            <div className="stack" style={{ gap: 8 }}>
              {s.callScript.turns.length === 0 && <p className="muted small">Реплики не заданы.</p>}
              {s.callScript.turns.map((turn, i) => (
                <div
                  key={i}
                  style={{
                    padding: '8px 10px',
                    borderLeft: `3px solid ${turn.speaker === 'caller' ? 'var(--u-border-strong)' : 'var(--u-accent)'}`,
                    background: 'var(--u-surface-2)',
                  }}
                >
                  <div className="dim small">
                    {turn.speaker === 'caller' ? 'Заявитель' : 'Подсказка оператору'}
                  </div>
                  <div>{turn.text}</div>
                </div>
              ))}
            </div>
          </Card>

          <Card title="Эталон карточки">
            <EtalonCardView card={s.etalonDraft} requiredFields={s.requiredFields} />
          </Card>
        </div>

        <div className="stack">
          <Card title="Свойства">
            <div className="stack" style={{ gap: 12 }}>
              <Field label="Название">
                <input
                  className="input"
                  value={title ?? s.title}
                  disabled={!editable || busy}
                  onChange={(e) => setTitle(e.target.value)}
                  onBlur={() => void saveTitle()}
                />
              </Field>

              <div>
                <div className="field__label">Создан</div>
                <div className="small">{formatDateTime(s.createdAt)}</div>
              </div>

              {s.validatedBy && (
                <div>
                  <div className="field__label">Подтверждён</div>
                  <div className="small">{s.validatedBy}</div>
                  <div className="dim small">{s.validatedAt ? formatDateTime(s.validatedAt) : ''}</div>
                </div>
              )}

              {s.teacherComment && (
                <div>
                  <div className="field__label">Комментарий преподавателя</div>
                  <div className="small">{s.teacherComment}</div>
                </div>
              )}
            </div>
          </Card>

          <Card title="Ключевые факты">
            <p className="field__hint" style={{ marginBottom: 8 }}>
              Источник обязательных фактов эталона. Обучающемуся не показывается.
            </p>
            <ul style={{ margin: 0, paddingLeft: 18 }}>
              {s.callScript.keyFacts.length === 0 && <li className="muted">Не заданы</li>}
              {s.callScript.keyFacts.map((f) => <li key={f}>{f}</li>)}
            </ul>
          </Card>

          <Card title="Обязательные поля">
            <div className="row row--tight">
              {s.requiredFields.map((f) => (
                <Badge key={f} tone="neutral">{labelForPath(f, labels.data ?? undefined)}</Badge>
              ))}
            </div>
            <p className="field__hint" style={{ marginTop: 10 }}>
              Обязательность приходит из эталона, а не зашита в интерфейс.
            </p>
          </Card>
        </div>
      </div>
      )}
    </>
  );
}
