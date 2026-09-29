import { useEffect, useRef, useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { api } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { Badge, Card, DifficultyBadge, ErrorState, Field, Loading, Modal, ScenarioStatusBadge } from '../../components/ui';
import { groupByVersions, scenarioInUse, scenarioLessonsCount, versionNo } from '../../features/scenario/versioning';
import { formatDate } from '../../shared/utils/time';
import { saveJson } from '../../shared/utils/download';
import type { AiJob, Difficulty, ScenarioBundle, ScenarioImportResult } from '../../shared/types';

/** Интервал опроса фоновой задачи генерации. */
const JOB_POLL_MS = 1500;

function jobText(job: AiJob | null): string {
  if (!job) return 'Задача отправлена в очередь…';
  switch (job.status) {
    case 'queued':
      return job.queuePosition ? `В очереди, позиция ${job.queuePosition}` : 'В очереди';
    case 'running':
      return job.estWaitSec ? `Сервис ИИ формирует сценарий, осталось около ${job.estWaitSec} с` : 'Сервис ИИ формирует сценарий…';
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
  generated: 'Генерация ИИ',
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
  const [selected, setSelected] = useState<string[]>([]);
  const [transferBusy, setTransferBusy] = useState(false);
  const [transferError, setTransferError] = useState<string | null>(null);
  const [imported, setImported] = useState<ScenarioImportResult | null>(null);
  const fileInput = useRef<HTMLInputElement>(null);

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
  const visibleIds = rows.map((r) => r.scenario.id);
  const allVisibleSelected = visibleIds.length > 0 && visibleIds.every((id) => selected.includes(id));

  /** Выгрузка пакетом (v1.4): выбранные или, если ничего не выбрано, все подтверждённые. */
  async function exportBundle() {
    setTransferBusy(true);
    setTransferError(null);
    try {
      const bundle = await api.scenarios.exportBundle(selected.length ? selected : undefined);
      saveJson(bundle, `scenarios-${new Date().toISOString().slice(0, 10)}.json`);
    } catch (e) {
      setTransferError(e instanceof Error ? e.message : 'Не удалось выгрузить сценарии');
    } finally {
      setTransferBusy(false);
    }
  }

  /** Импорт пакета: файл читается в браузере, разбор и проверка элементов — на сервере. */
  async function importFile(file: File) {
    setTransferBusy(true);
    setTransferError(null);
    try {
      let bundle: ScenarioBundle;
      try {
        bundle = JSON.parse(await file.text()) as ScenarioBundle;
      } catch {
        throw new Error('Файл не является JSON — выберите пакет, выгруженный кнопкой «Выгрузить»');
      }
      const result = await api.scenarios.importBundle(bundle);
      setImported(result);
      scenarios.reload();
    } catch (e) {
      setTransferError(e instanceof Error ? e.message : 'Не удалось импортировать сценарии');
    } finally {
      setTransferBusy(false);
      if (fileInput.current) fileInput.current.value = '';
    }
  }

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
          <button type="button" className="btn" onClick={() => void exportBundle()} disabled={transferBusy}>
            {selected.length ? `Выгрузить выбранные (${selected.length})` : 'Выгрузить подтверждённые'}
          </button>
          <button type="button" className="btn" onClick={() => fileInput.current?.click()} disabled={transferBusy}>
            Импорт из файла
          </button>
          <input
            ref={fileInput}
            type="file"
            accept="application/json,.json"
            hidden
            onChange={(e) => {
              const file = e.target.files?.[0];
              if (file) void importFile(file);
            }}
          />
          <button type="button" className="btn btn--primary" onClick={() => setCreating(true)}>
            Создать сценарий
          </button>
        </div>
      </div>

      {transferError && <p className="field__error" role="alert" style={{ marginBottom: 12 }}>{transferError}</p>}

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
          <div className="table-scroll">
          <table className="table">
            <thead>
              <tr>
                <th style={{ width: 32 }}>
                  <input
                    type="checkbox"
                    aria-label="Выбрать все показанные сценарии"
                    checked={allVisibleSelected}
                    onChange={() =>
                      setSelected(allVisibleSelected
                        ? selected.filter((id) => !visibleIds.includes(id))
                        : [...new Set([...selected, ...visibleIds])])}
                  />
                </th>
                <th>Название</th><th>Категория</th><th>Сложность</th>
                <th>Источник</th><th>Статус</th><th>В занятиях</th><th>Создан</th>
              </tr>
            </thead>
            <tbody>
              {rows.map(({ scenario: s, grouped }) => (
                <tr key={s.id}>
                  <td>
                    <input
                      type="checkbox"
                      aria-label={`Выбрать «${s.title}» для выгрузки`}
                      checked={selected.includes(s.id)}
                      onChange={() => setSelected(selected.includes(s.id) ? selected.filter((x) => x !== s.id) : [...selected, s.id])}
                    />
                  </td>
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
          </div>
        )}
      </Card>

      {creating && <CreateScenarioModal onClose={() => setCreating(false)} onDone={scenarios.reload} />}
      {imported && <ImportResultModal result={imported} onClose={() => setImported(null)} />}
    </>
  );
}

/** Итог импорта: созданные — ссылками на черновики, отклонённые — с причиной по номеру в пакете. */
function ImportResultModal({ result, onClose }: { result: ScenarioImportResult; onClose: () => void }) {
  return (
    <Modal
      title="Импорт сценариев"
      wide
      onClose={onClose}
      footer={<button type="button" className="btn btn--primary" onClick={onClose}>Готово</button>}
    >
      <div className="stack">
        <p>
          Создано: <b>{result.created.length}</b> · отклонено: <b>{result.rejected.length}</b>.
          {result.created.length > 0 && ' Новые сценарии — черновики: проверьте и подтвердите их перед выдачей.'}
        </p>
        {result.created.length > 0 && (
          <div>
            <div className="field__label" style={{ marginBottom: 4 }}>Созданы</div>
            <ul style={{ margin: 0, paddingLeft: 18 }}>
              {result.created.map((c) => (
                <li key={c.scenarioId}><Link to={`/teacher/scenarios/${c.scenarioId}`} onClick={onClose}>{c.title}</Link></li>
              ))}
            </ul>
          </div>
        )}
        {result.rejected.length > 0 && (
          <div className="table-scroll">
            <table className="table">
              <thead><tr><th className="num">№ в пакете</th><th>Сценарий</th><th>Причина</th></tr></thead>
              <tbody>
                {result.rejected.map((r) => (
                  <tr key={r.index}>
                    <td className="num">{r.index + 1}</td>
                    <td>{r.title ?? '—'}</td>
                    <td style={{ color: 'var(--u-danger)' }}>{r.message}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </Modal>
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
