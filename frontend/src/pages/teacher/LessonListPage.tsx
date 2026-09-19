import { useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { api } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { Card, ErrorState, Field, LessonStatusBadge, Loading, Modal, NumberInput } from '../../components/ui';
import { formatDate } from '../../shared/utils/time';
import { shortName } from '../../shared/utils/user';
import type { ArmPerspective, Difficulty } from '../../shared/types';

/** Доля слоя в итоговом балле как проценты: 0.5 → «50%». */
function pct(value: number): string {
  return `${Math.round(value * 100)}%`;
}

export function LessonListPage() {
  const lessons = useAsync(() => api.lessons.list(), []);
  const [creating, setCreating] = useState(false);

  if (lessons.loading) return <Loading />;
  if (lessons.error) return <ErrorState text={lessons.error} onRetry={lessons.reload} />;

  const list = lessons.data ?? [];

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <h1>Занятия</h1>
          <div className="page-head__sub">
            Сценарии, участники, норматив времени и критерии оценки.
          </div>
        </div>
        <div className="page-head__actions">
          <button type="button" className="btn btn--primary" onClick={() => setCreating(true)}>
            Создать занятие
          </button>
        </div>
      </div>

      <Card title={`Всего: ${list.length}`}>
        {list.length === 0 ? (
          <p className="muted small">Занятий пока нет.</p>
        ) : (
          <table className="table">
            <thead>
              <tr>
                <th>Название</th><th>Режим</th><th>Рабочее место</th>
                <th>Сценарии</th><th>Участники</th><th>Норматив</th><th>Статус</th><th>Создано</th>
              </tr>
            </thead>
            <tbody>
              {list.map((l) => (
                <tr key={l.id}>
                  <td><Link to={`/teacher/lessons/${l.id}`}>{l.title}</Link></td>
                  <td className="muted small">{l.mode === 'cards' ? 'Карточки' : 'Действия с карточками'}</td>
                  <td className="muted small">{l.perspective === 'operator112' ? 'Оператор-112' : 'Диспетчер ДДС'}</td>
                  <td className="mono">{l.scenarioIds.length}</td>
                  <td className="mono">{l.participants.length}</td>
                  <td className="mono nowrap">{l.timeLimitSec} с</td>
                  <td><LessonStatusBadge status={l.status} /></td>
                  <td className="muted small nowrap">{formatDate(l.createdAt)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>

      {creating && <CreateLessonModal onClose={() => setCreating(false)} />}
    </>
  );
}

function CreateLessonModal({ onClose }: { onClose: () => void }) {
  const scenarios = useAsync(() => api.scenarios.list(), []);
  const students = useAsync(() => api.users.list(), []);
  const defaults = useAsync(() => api.lessons.defaultSettings(), []);
  const navigate = useNavigate();

  const [title, setTitle] = useState('');
  const [perspective, setPerspective] = useState<ArmPerspective>('operator112');
  const [difficulty, setDifficulty] = useState<Difficulty | 0>(0);
  const [timeLimitSec, setTimeLimitSec] = useState(30);
  /** null — ещё не получен из параметров по умолчанию */
  const [passThreshold, setPassThreshold] = useState<number | null>(null);
  const [allowReplay, setAllowReplay] = useState(true);
  const [scenarioIds, setScenarioIds] = useState<string[]>([]);
  const [participantIds, setParticipantIds] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const validated = (scenarios.data ?? []).filter((s) => s.status === 'validated');
  const learners = (students.data ?? []).filter((u) => u.role === 'student' && u.status === 'active');
  const weights = defaults.data?.weights;
  // Значение поля — введённое пользователем либо значение по умолчанию.
  const threshold = passThreshold ?? defaults.data?.passThreshold;

  async function submit() {
    if (scenarioIds.length === 0) {
      setError('Выберите хотя бы один подтверждённый сценарий');
      return;
    }
    if (participantIds.length === 0) {
      setError('Выберите хотя бы одного участника');
      return;
    }
    if (threshold == null) {
      setError('Параметры по умолчанию ещё не загружены');
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const lesson = await api.lessons.create({
        title: title || 'Практическое занятие',
        mode: 'cards',
        perspective,
        difficulty: difficulty === 0 ? undefined : difficulty,
        timeLimitSec,
        scenarioIds,
        participantIds,
        passThreshold: threshold ?? 0,
        allowReplay,
      });
      onClose();
      navigate(`/teacher/lessons/${lesson.id}`);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Не удалось создать занятие');
      setBusy(false);
    }
  }

  function toggle(list: string[], setList: (v: string[]) => void, id: string) {
    setList(list.includes(id) ? list.filter((x) => x !== id) : [...list, id]);
  }

  return (
    <Modal
      title="Новое занятие"
      wide
      onClose={onClose}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose} disabled={busy}>Отмена</button>
          <button type="button" className="btn btn--primary" onClick={() => void submit()} disabled={busy}>
            {busy ? 'Создание…' : 'Создать занятие'}
          </button>
        </>
      }
    >
      <div className="stack" style={{ gap: 16 }}>
        <Field label="Название занятия">
          <input className="input" value={title} onChange={(e) => setTitle(e.target.value)} placeholder="Практическое занятие: пожары" />
        </Field>

        <div className="grid grid--3">
          <Field
            label="Рабочее место"
            hint="Определяет интерфейс обучающегося. Рабочее место диспетчера ДДС в этой версии не реализовано."
          >
            <select className="select" value={perspective} onChange={(e) => setPerspective(e.target.value as ArmPerspective)}>
              <option value="operator112">Оператор-112 — заполнение карточки</option>
              <option value="dds" disabled>Диспетчер ДДС — статусы реагирования (недоступно)</option>
            </select>
          </Field>

          <Field label="Сложность">
            <select className="select" value={difficulty} onChange={(e) => setDifficulty(Number(e.target.value) as Difficulty | 0)}>
              <option value={0}>Любая</option>
              <option value={1}>1 — базовая</option>
              <option value={2}>2 — средняя</option>
              <option value={3}>3 — высокая</option>
            </select>
          </Field>

          <Field label="Норматив заполнения, с" hint="По умолчанию 30 с">
            <NumberInput min={10} max={600} value={timeLimitSec} onChange={setTimeLimitSec} />
          </Field>
        </div>

        <div className="grid grid--3">
          <Field label="Порог зачёта, баллов">
            <NumberInput min={0} max={100} value={threshold} onChange={setPassThreshold} />
          </Field>

          <Field label="Переспрашивание заявителя">
            <select className="select" value={allowReplay ? 'yes' : 'no'} onChange={(e) => setAllowReplay(e.target.value === 'yes')}>
              <option value="yes">Разрешено</option>
              <option value="no">Запрещено</option>
            </select>
          </Field>

          <Field
            label="Веса слоёв оценки"
            hint="Поля / семантика / грамматика / время. Применяются к занятию и видны в результате."
          >
            <div className="input" style={{ display: 'flex', alignItems: 'center' }}>
              {weights
                ? `${pct(weights.fields)} / ${pct(weights.semantic)} / ${pct(weights.grammar)} / ${pct(weights.timing)}`
                : '—'}
            </div>
          </Field>
        </div>

        <div>
          <div className="field__label" style={{ marginBottom: 6 }}>
            Сценарии ({scenarioIds.length} выбрано)
          </div>
          {scenarios.loading ? (
            <Loading text="Загрузка сценариев…" />
          ) : validated.length === 0 ? (
            <p className="muted small">Нет подтверждённых сценариев. Сначала подтвердите сценарий.</p>
          ) : (
            <div className="stack" style={{ gap: 6 }}>
              {validated.map((s) => (
                <label key={s.id} className="row" style={{ padding: '7px 10px', border: '1px solid var(--u-border)', borderRadius: 4, cursor: 'pointer' }}>
                  <input
                    type="checkbox"
                    checked={scenarioIds.includes(s.id)}
                    onChange={() => toggle(scenarioIds, setScenarioIds, s.id)}
                  />
                  <span>{s.title}</span>
                  <span className="dim small">· {s.categoryName} · сложность {s.difficulty}</span>
                </label>
              ))}
            </div>
          )}
        </div>

        <div>
          <div className="field__label" style={{ marginBottom: 6 }}>
            Участники ({participantIds.length} выбрано)
          </div>
          {students.loading ? (
            <Loading text="Загрузка списка обучающихся…" />
          ) : learners.length === 0 ? (
            <p className="muted small">Активных обучающихся нет.</p>
          ) : (
            <div className="stack" style={{ gap: 6 }}>
              {learners.map((u) => (
                <label key={u.id} className="row" style={{ padding: '7px 10px', border: '1px solid var(--u-border)', borderRadius: 4, cursor: 'pointer' }}>
                  <input
                    type="checkbox"
                    checked={participantIds.includes(u.id)}
                    onChange={() => toggle(participantIds, setParticipantIds, u.id)}
                  />
                  <span>{shortName(u)}</span>
                  {u.serviceName && <span className="dim small">· {u.serviceName}</span>}
                </label>
              ))}
            </div>
          )}
        </div>

        {error && <div className="field__error">{error}</div>}
      </div>
    </Modal>
  );
}
