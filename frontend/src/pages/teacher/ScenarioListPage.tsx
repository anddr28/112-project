import { useEffect, useRef, useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { api } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { Badge, Card, DifficultyBadge, ErrorState, Field, Loading, Modal, ScenarioStatusBadge } from '../../components/ui';
import { groupByVersions, scenarioInUse, scenarioLessonsCount, versionNo } from '../../features/scenario/versioning';
import { formatDate } from '../../shared/utils/time';
import type { AiJob, Difficulty } from '../../shared/types';

/** Интервал опроса фоновой задачи генерации. */
const JOB_POLL_MS = 1500;

function jobText(job: AiJob | null): string {
  if (!job) return 'Задача отправлена в очередь…';
  switch (job.status) {
    case 'queued':
      return job.queuePosition ? `В очереди, позиция ${job.queuePosition}` : 'В очереди';
    case 'running':
      return job.estWaitSec ? `Нейросеть формирует сценарий, осталось около ${job.estWaitSec} с` : 'Нейросеть формирует сценарий…';
    case 'done':
      return 'Сценарий сформирован';
    case 'cancelled':
      return 'Генерация отменена';
    default:
      return job.error ?? 'Генерация не удалась';
  }
}

/** Источник сценария. Значения перечисления не показываем напрямую. */
const SOURCE_LABEL: Record<string, string> = {
  generated: 'Нейросеть',
  manual: 'Вручную',
  ticket: 'Билет',
  student: 'Карточка обучающегося',
};

export function ScenarioListPage() {
  const scenarios = useAsync(() => api.scenarios.list(), []);
  const types = useAsync(() => api.classifier.incidentTypes(), []);
  const [creating, setCreating] = useState(false);
  const [statusFilter, setStatusFilter] = useState('all');
  const [categoryFilter, setCategoryFilter] = useState('all');

  if (scenarios.loading) return <Loading />;
  if (scenarios.error) return <ErrorState text={scenarios.error} onRetry={scenarios.reload} />;

  const all = scenarios.data ?? [];
  const list = all.filter(
    (s) =>
      (statusFilter === 'all' || s.status === statusFilter) &&
      (categoryFilter === 'all' || s.categoryId === categoryFilter),
  );
  // Версии одного сценария идут подряд, чтобы v1 и v2 не выглядели разными сценариями.
  const rows = groupByVersions(list, all);

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <h1>Сценарии</h1>
          <div className="page-head__sub">
            Легенда звонка и эталон карточки. Сгенерированный ИИ сценарий подтверждает преподаватель.
          </div>
        </div>
        <div className="page-head__actions">
          <button type="button" className="btn btn--primary" onClick={() => setCreating(true)}>
            Создать сценарий
          </button>
        </div>
      </div>

      <Card
        title={`Найдено: ${list.length}`}
        actions={
          <div className="row row--tight">
            <select className="select" style={{ width: 200 }} value={categoryFilter} onChange={(e) => setCategoryFilter(e.target.value)}>
              <option value="all">Все категории</option>
              {(types.data ?? []).filter((t) => t.depth <= 2).map((t) => (
                <option key={t.id} value={t.id}>{t.name}</option>
              ))}
            </select>
            <select className="select" style={{ width: 170 }} value={statusFilter} onChange={(e) => setStatusFilter(e.target.value)}>
              <option value="all">Любой статус</option>
              <option value="draft">Черновик</option>
              <option value="generated">Сгенерирован ИИ</option>
              <option value="validated">Подтверждён</option>
              <option value="rejected">Отклонён</option>
            </select>
          </div>
        }
      >
        {list.length === 0 ? (
          <p className="muted small">Ничего не найдено. Измените фильтры или создайте сценарий.</p>
        ) : (
          <table className="table">
            <thead>
              <tr>
                <th>Название</th><th>Категория</th><th>Сложность</th>
                <th>Источник</th><th>Статус</th><th>В занятиях</th><th>Создан</th>
              </tr>
            </thead>
            <tbody>
              {rows.map(({ scenario: s, grouped }) => (
                <tr key={s.id}>
                  <td>
                    {grouped && versionNo(s) > 1 && <span className="dim" aria-hidden="true">↳ </span>}
                    <Link to={`/teacher/scenarios/${s.id}`}>{s.title}</Link>
                    {grouped && <> <Badge tone="accent">версия {versionNo(s)}</Badge></>}
                  </td>
                  <td className="muted">{s.categoryName}</td>
                  <td><DifficultyBadge level={s.difficulty} /></td>
                  <td className="muted small">
                    {SOURCE_LABEL[s.source] ?? 'Не указан'}
                  </td>
                  <td><ScenarioStatusBadge status={s.status} /></td>
                  <td className="mono">{scenarioInUse(s) ? scenarioLessonsCount(s) : '—'}</td>
                  <td className="muted small nowrap">{formatDate(s.createdAt)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>

      {creating && <CreateScenarioModal onClose={() => setCreating(false)} onDone={scenarios.reload} />}
    </>
  );
}

function CreateScenarioModal({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const types = useAsync(() => api.classifier.incidentTypes(), []);
  const [mode, setMode] = useState<'generate' | 'manual'>('generate');
  // Идентификатор категории не задаём константой: список приходит из
  // классификатора, и хардкод разъехался бы с ним при первом же изменении.
  const [categoryId, setCategoryId] = useState('');
  const [difficulty, setDifficulty] = useState<Difficulty>(2);
  const [title, setTitle] = useState('');
  const [comment, setComment] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  /** Запущенная генерация: задача ai_jobs и уже созданный черновик сценария. */
  const [started, setStarted] = useState<{ jobId: string; scenarioId: string } | null>(null);
  const [job, setJob] = useState<AiJob | null>(null);
  const navigate = useNavigate();

  const categories = (types.data ?? []).filter((t) => t.depth <= 2);
  const selectedCategory = categoryId || categories[0]?.id || '';

  // Колбэки родителя меняют идентичность при каждой его перерисовке —
  // опрос от этого перезапускаться не должен.
  const handlers = useRef({ onClose, onDone, navigate });
  useEffect(() => {
    handlers.current = { onClose, onDone, navigate };
  });

  // Опрос фоновой задачи генерации до done / failed / cancelled.
  useEffect(() => {
    if (!started) return;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout>;
    const poll = () => {
      api.aiJobs
        .get(started.jobId)
        .then((next) => {
          if (cancelled) return;
          setJob(next);
          if (next.status === 'done') {
            const h = handlers.current;
            h.onDone();
            h.onClose();
            h.navigate(`/teacher/scenarios/${started.scenarioId}`);
            return;
          }
          if (next.status === 'failed' || next.status === 'cancelled') {
            setError(jobText(next));
            setBusy(false);
            return;
          }
          timer = setTimeout(poll, JOB_POLL_MS);
        })
        .catch((e: unknown) => {
          if (cancelled) return;
          setError(e instanceof Error ? e.message : 'Не удалось получить состояние генерации');
          setBusy(false);
        });
    };
    poll();
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [started]);

  async function submit() {
    if (!selectedCategory) {
      setError('Классификатор ещё не загружен');
      return;
    }
    setBusy(true);
    setError(null);
    setJob(null);
    try {
      if (mode === 'generate') {
        const accepted = await api.scenarios.generate({
          categoryId: selectedCategory,
          difficulty,
          mode: 'cards',
          teacherComment: comment || undefined,
          withDialogue: true,
        });
        // Черновик уже есть в списке — обновляем его сразу, не дожидаясь результата.
        onDone();
        setStarted({ jobId: accepted.jobId, scenarioId: accepted.scenarioId });
        return;
      }
      const scenario = await api.scenarios.create({ title: title || 'Новый сценарий', categoryId: selectedCategory, difficulty, mode: 'cards' });
      onDone();
      onClose();
      navigate(`/teacher/scenarios/${scenario.id}`);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Не удалось создать сценарий');
      setBusy(false);
    }
  }

  const generating = Boolean(started) && busy;

  return (
    <Modal
      title="Новый сценарий"
      onClose={onClose}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose} disabled={busy && !generating}>
            {generating ? 'Закрыть' : 'Отмена'}
          </button>
          <button type="button" className="btn btn--primary" onClick={() => void submit()} disabled={busy}>
            {busy
              ? (mode === 'generate' ? 'Генерация…' : 'Создание…')
              : mode === 'generate'
                ? (started ? 'Повторить генерацию' : 'Сгенерировать')
                : 'Создать'}
          </button>
        </>
      }
    >
      <div className="stack">
        <div className="row row--tight">
          <button type="button" className={`btn ${mode === 'generate' ? 'btn--primary' : ''}`} onClick={() => setMode('generate')}>
            Генерация нейросетью
          </button>
          <button type="button" className={`btn ${mode === 'manual' ? 'btn--primary' : ''}`} onClick={() => setMode('manual')}>
            Вручную
          </button>
        </div>

        {mode === 'manual' && (
          <Field label="Название">
            <input className="input" value={title} onChange={(e) => setTitle(e.target.value)} placeholder="Например: Пожар в квартире" />
          </Field>
        )}

        <Field label="Категория происшествия">
          <select className="select" value={selectedCategory} onChange={(e) => setCategoryId(e.target.value)}>
            {categories.map((t) => (
              <option key={t.id} value={t.id}>{t.name}</option>
            ))}
          </select>
        </Field>

        <Field label="Сложность">
          <select className="select" value={difficulty} onChange={(e) => setDifficulty(Number(e.target.value) as Difficulty)}>
            <option value={1}>1 — базовая</option>
            <option value={2}>2 — средняя</option>
            <option value={3}>3 — высокая</option>
          </select>
        </Field>

        {mode === 'generate' && (
          <Field
            label="Контекстное поле для корректировки генерации"
            hint="Необязательно. Комментарий передаётся модели и влияет на легенду звонка."
          >
            <textarea
              className="textarea"
              value={comment}
              onChange={(e) => setComment(e.target.value)}
              placeholder="Например: заявитель говорит сбивчиво, адрес называет не сразу"
            />
          </Field>
        )}

        {error && <div className="field__error" role="alert">{error}</div>}

        {generating && (
          <div className="card" style={{ padding: '10px 12px' }} role="status" aria-live="polite">
            <b>{jobText(job)}</b>
            <p className="field__hint" style={{ margin: '4px 0 0' }}>
              Черновик сценария уже создан. Окно можно закрыть — генерация продолжится,
              сценарий появится в списке со статусом «Сгенерирован ИИ».
            </p>
          </div>
        )}

        {mode === 'generate' && !generating && (
          <p className="field__hint">
            Генерация выполняется в фоновой очереди и занимает время. Результат приходит
            со статусом «Сгенерирован ИИ» и требует подтверждения преподавателем.
          </p>
        )}
      </div>
    </Modal>
  );
}
