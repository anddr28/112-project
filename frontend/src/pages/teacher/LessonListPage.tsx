import { useState } from 'react';
import { Link, useNavigate, useSearchParams } from 'react-router-dom';
import { api } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { isApiError } from '../../shared/api/error';
import { Badge, Card, ErrorState, Field, LessonStatusBadge, Loading, Modal, NumberInput } from '../../components/ui';
import { formatDate } from '../../shared/utils/time';
import { shortName } from '../../shared/utils/user';
import { groupByVersions, versionNo } from '../../features/scenario/versioning';
import type { ArmPerspective, CardSource, Difficulty, LessonSettings, Scenario, VoiceInput, VoiceSettings } from '../../shared/types';

/**
 * ТЗ, «действия с карточками»: пул занятия — из карточек, сгенерированных
 * системой, сформированных обучающимися или смешанный. Карточка обучающегося —
 * сценарий с source = student (кнопка «Сделать сценарием» в разборе попытки).
 */
const CARD_SOURCES: Array<{ value: CardSource; label: string }> = [
  { value: 'mixed', label: 'Смешанный' },
  { value: 'generated', label: 'Сгенерированные системой' },
  { value: 'student', label: 'Сформированные обучающимися' },
];

function matchesSource(s: Scenario, source: CardSource): boolean {
  if (source === 'mixed') return true;
  return source === 'student' ? s.source === 'student' : s.source !== 'student';
}

/** Доли слоёв в процентах — так их складывает и правит преподаватель. */
type WeightPct = Record<keyof LessonSettings['weights'], number>;

/** Слои оценки в порядке, в котором они показываются преподавателю. */
const WEIGHT_LAYERS: Array<{ key: keyof LessonSettings['weights']; label: string }> = [
  { key: 'fields', label: 'Поля карточки' },
  { key: 'semantic', label: 'Семантика' },
  { key: 'grammar', label: 'Грамматика' },
  { key: 'timing', label: 'Время' },
  { key: 'dialogue', label: 'Разговор' },
];

export function LessonListPage() {
  const lessons = useAsync(() => api.lessons.list(), []);
  // Из карточки сценария: ?create=1&scenario=<id> — форма с выбранным сценарием.
  const [searchParams, setSearchParams] = useSearchParams();
  const [creating, setCreating] = useState(searchParams.get('create') === '1');
  const preselected = searchParams.get('scenario');

  function closeCreate() {
    setCreating(false);
    if (searchParams.has('create')) setSearchParams({}, { replace: true });
  }

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
                <th>Сценарии</th><th>Участники</th><th>Норматив</th><th>Разговор</th><th>Статус</th><th>Создано</th>
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
                  <td className="muted small">{l.settings.voice.enabled ? 'голосовой' : '—'}</td>
                  <td><LessonStatusBadge status={l.status} /></td>
                  <td className="muted small nowrap">{formatDate(l.createdAt)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>

      {creating && <CreateLessonModal onClose={closeCreate} initialScenarioId={preselected} />}
    </>
  );
}

function CreateLessonModal({ onClose, initialScenarioId }: { onClose: () => void; initialScenarioId: string | null }) {
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
  const [voice, setVoice] = useState<VoiceSettings | null>(null);
  /** проценты слоёв; null — ещё не получены параметры по умолчанию */
  const [weightPct, setWeightPct] = useState<WeightPct | null>(null);
  const [scenarioIds, setScenarioIds] = useState<string[]>(initialScenarioId ? [initialScenarioId] : []);
  const [participantIds, setParticipantIds] = useState<string[]>([]);
  const [cardSource, setCardSource] = useState<CardSource>('mixed');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  /** Сценарии и участники, на которые указал сервер в отказе (details) — подсвечиваются. */
  const [rejected, setRejected] = useState<string[]>([]);

  const allScenarios = scenarios.data ?? [];
  // Только подтверждённые; версии одного сценария — рядом и с номером.
  const validated = groupByVersions(allScenarios.filter((s) => s.status === 'validated'), allScenarios);
  const learners = (students.data ?? []).filter((u) => u.role === 'student' && u.status === 'active');
  // Значение поля — введённое пользователем либо значение по умолчанию.
  const threshold = passThreshold ?? defaults.data?.passThreshold;

  /*
   * Голосовой режим и веса берут значения по умолчанию из сервисного слоя,
   * пока преподаватель их не тронул. Проценты хранятся целыми: так их проще
   * складывать до ста и невозможно получить 0.30000000000000004.
   */
  const voiceSettings: VoiceSettings | null = voice ?? defaults.data?.voice ?? null;
  const defaultWeights = defaults.data?.weights;
  const defaultPct: WeightPct | null = defaultWeights
    ? {
        fields: Math.round(defaultWeights.fields * 100),
        semantic: Math.round(defaultWeights.semantic * 100),
        grammar: Math.round(defaultWeights.grammar * 100),
        timing: Math.round(defaultWeights.timing * 100),
        dialogue: Math.round(defaultWeights.dialogue * 100),
      }
    : null;
  const pctValues = weightPct ?? defaultPct;
  /*
   * Ракурс «Диспетчер ДДС» (v1.3): карточка поступает от оператора 112, звонка
   * нет — занятие только в режиме «Действия с карточками», голос и
   * переспрашивание не применяются, разговор не оценивается.
   */
  const isDds = perspective === 'dds';
  // Источник карточек — только у «действий с карточками» (ракурс ДДС); в пул попадают подходящие сценарии.
  const source: CardSource = isDds ? cardSource : 'mixed';
  const pool = validated.filter(({ scenario }) => matchesSource(scenario, source));
  const voiceOn = !isDds && (voiceSettings?.enabled ?? false);

  /** Вес разговора учитывается только при включённом голосе. */
  const effectivePct: WeightPct | null = pctValues
    ? { ...pctValues, dialogue: voiceOn ? pctValues.dialogue : 0 }
    : null;
  const pctSum = effectivePct
    ? WEIGHT_LAYERS.reduce((acc, l) => acc + (effectivePct[l.key] ?? 0), 0)
    : 0;

  function setLayerPct(key: keyof WeightPct, value: number) {
    if (!pctValues) return;
    setWeightPct({ ...pctValues, [key]: value });
  }

  function patchVoice(patch: Partial<VoiceSettings>) {
    if (!voiceSettings) return;
    const next = { ...voiceSettings, ...patch };
    setVoice(next);
    // Включили голос впервые — даём разговору заметную долю, чтобы
    // преподавателю не пришлось собирать сто процентов вручную.
    if (patch.enabled === true && pctValues && (pctValues.dialogue ?? 0) === 0) {
      setWeightPct({ fields: 35, semantic: 20, grammar: 10, timing: 10, dialogue: 25 });
    }
    if (patch.enabled === false && pctValues && pctValues.dialogue > 0 && defaultPct) {
      setWeightPct({ ...defaultPct, dialogue: 0 });
    }
  }

  async function submit() {
    if (scenarioIds.length === 0) {
      setError('Выберите хотя бы один подтверждённый сценарий');
      return;
    }
    if (participantIds.length === 0) {
      setError('Выберите хотя бы одного участника');
      return;
    }
    if (threshold == null || !effectivePct || !voiceSettings) {
      setError('Параметры по умолчанию ещё не загружены');
      return;
    }
    if (pctSum !== 100) {
      setError(`Сумма весов слоёв должна быть равна 100%, сейчас ${pctSum}%`);
      return;
    }
    setBusy(true);
    setError(null);
    setRejected([]);
    try {
      const lesson = await api.lessons.create({
        title: title || 'Практическое занятие',
        mode: isDds ? 'card_actions' : 'cards',
        perspective,
        difficulty: difficulty === 0 ? undefined : difficulty,
        timeLimitSec,
        scenarioIds,
        participantIds,
        cardSource: isDds ? cardSource : undefined,
        passThreshold: threshold ?? 0,
        allowReplay: isDds ? false : allowReplay,
        voice: isDds ? { ...voiceSettings, enabled: false } : voiceSettings,
        weights: {
          fields: effectivePct.fields / 100,
          semantic: effectivePct.semantic / 100,
          grammar: effectivePct.grammar / 100,
          timing: effectivePct.timing / 100,
          dialogue: effectivePct.dialogue / 100,
        },
      });
      onClose();
      navigate(`/teacher/lessons/${lesson.id}`);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Не удалось создать занятие');
      if (isApiError(e)) setRejected([...idList(e.details?.scenarioIds), ...idList(e.details?.participantIds)]);
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
            hint={
              isDds
                ? 'Обучающийся получает карточку от оператора 112 и работает за службу своего профиля. Режим — «Действия с карточками», без голоса.'
                : 'Определяет интерфейс обучающегося.'
            }
          >
            <select className="select" value={perspective} onChange={(e) => setPerspective(e.target.value as ArmPerspective)}>
              <option value="operator112">Оператор-112 — заполнение карточки</option>
              <option value="dds">Диспетчер ДДС — приём карточки и статусы реагирования</option>
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

          <Field label={isDds ? 'Норматив работы с карточкой, с' : 'Норматив заполнения, с'} hint="По умолчанию 30 с">
            <NumberInput min={10} max={600} value={timeLimitSec} onChange={setTimeLimitSec} />
          </Field>
        </div>

        {isDds && (
          <Field
            label="Источник карточек"
            hint="Из каких карточек собирается пул занятия. Карточка обучающегося становится сценарием в разборе попытки («Сделать сценарием»)."
          >
            <select
              className="select"
              value={cardSource}
              onChange={(e) => {
                const next = e.target.value as CardSource;
                setCardSource(next);
                // Уже выбранные сценарии другого источника в пул не пройдут — снимаем выбор сразу.
                setScenarioIds((ids) => ids.filter((id) => {
                  const sc = allScenarios.find((x) => x.id === id);
                  return sc ? matchesSource(sc, next) : false;
                }));
              }}
            >
              {CARD_SOURCES.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
            </select>
          </Field>
        )}

        <div className="grid grid--3">
          <Field label="Порог зачёта, баллов">
            <NumberInput min={0} max={100} value={threshold} onChange={setPassThreshold} />
          </Field>

          {!isDds && (
            <Field label="Переспрашивание заявителя">
              <select className="select" value={allowReplay ? 'yes' : 'no'} onChange={(e) => setAllowReplay(e.target.value === 'yes')}>
                <option value="yes">Разрешено</option>
                <option value="no">Запрещено</option>
              </select>
            </Field>
          )}

          {!isDds && (
            <Field label="Голосовой режим" hint="Разговор с ИИ-заявителем вместо сценарных реплик">
              <select
                className="select"
                value={voiceOn ? 'on' : 'off'}
                disabled={!voiceSettings}
                onChange={(e) => patchVoice({ enabled: e.target.value === 'on' })}
              >
                <option value="off">Выключен</option>
                <option value="on">Включён</option>
              </select>
            </Field>
          )}
        </div>

        {voiceOn && voiceSettings && (
          <div className="grid grid--3">
            <Field label="Ввод реплик обучающегося">
              <select
                className="select"
                value={voiceSettings.input}
                onChange={(e) => patchVoice({ input: e.target.value as VoiceInput })}
              >
                <option value="voice">Голосом</option>
                <option value="text">Текстом</option>
                <option value="both">На выбор обучающегося</option>
              </select>
            </Field>

            <Field label="Кнопка ответа" hint="Как обучающийся отвечает заявителю">
              <select
                className="select"
                value={voiceSettings.pushToTalk ? 'ptt' : 'toggle'}
                onChange={(e) => patchVoice({ pushToTalk: e.target.value === 'ptt' })}
              >
                <option value="ptt">Удерживать кнопку</option>
                <option value="toggle">Нажатие — старт, нажатие — стоп</option>
              </select>
            </Field>

            <Field label="Максимум реплик оператора" hint="Разговор закрывается по достижении">
              <NumberInput
                min={2}
                max={40}
                value={voiceSettings.maxTurns}
                onChange={(maxTurns) => patchVoice({ maxTurns })}
              />
            </Field>
          </div>
        )}

        <div>
          <div className="field__label" style={{ marginBottom: 6 }}>
            Веса слоёв оценки — сумма {pctSum}%{pctSum !== 100 ? ' (должно быть 100%)' : ''}
          </div>
          <div className="grid grid--5">
            {WEIGHT_LAYERS.map((layer) => {
              const disabled = layer.key === 'dialogue' && !voiceOn;
              return (
                <Field key={layer.key} label={layer.label}>
                  <NumberInput
                    min={0}
                    max={100}
                    value={effectivePct?.[layer.key]}
                    disabled={disabled || !pctValues}
                    aria-label={`Вес слоя «${layer.label}», проценты`}
                    onChange={(v) => setLayerPct(layer.key, v)}
                  />
                </Field>
              );
            })}
          </div>
          <p className="field__hint" style={{ marginTop: 6 }}>
            {isDds
              ? '«Поля карточки» в ракурсе ДДС — протокол реагирования: решение, время решения, статусы. Смысл и грамотность — по тексту действия.'
              : voiceOn
                ? 'Балл считается по этим долям и показывается обучающемуся в результате.'
                : 'Без голосового режима разговор не оценивается: его доля равна нулю.'}
          </p>
        </div>

        <div>
          <div className="field__label" style={{ marginBottom: 6 }}>
            Сценарии ({scenarioIds.length} выбрано)
          </div>
          {scenarios.loading ? (
            <Loading text="Загрузка сценариев…" />
          ) : validated.length === 0 ? (
            <p className="muted small">Нет подтверждённых сценариев. Сначала подтвердите сценарий.</p>
          ) : pool.length === 0 ? (
            <p className="muted small">
              {source === 'student'
                ? 'Подтверждённых сценариев из карточек обучающихся нет. Создайте их из разбора оценённой попытки — «Сделать сценарием».'
                : 'Нет подтверждённых сценариев, сгенерированных системой.'}
            </p>
          ) : (
            <div className="stack" style={{ gap: 6 }}>
              {pool.map(({ scenario: s, grouped }) => (
                <label key={s.id} className="row" style={{ padding: '7px 10px', border: `1px solid ${rejected.includes(s.id) ? 'var(--u-danger)' : 'var(--u-border)'}`, borderRadius: 4, cursor: 'pointer' }}>
                  <input
                    type="checkbox"
                    checked={scenarioIds.includes(s.id)}
                    onChange={() => toggle(scenarioIds, setScenarioIds, s.id)}
                  />
                  <span>{s.title}</span>
                  {grouped && <Badge tone="accent">версия {versionNo(s)}</Badge>}
                  {s.source === 'student' && <Badge tone="neutral">карточка обучающегося</Badge>}
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
                <label key={u.id} className="row" style={{ padding: '7px 10px', border: `1px solid ${rejected.includes(u.id) ? 'var(--u-danger)' : 'var(--u-border)'}`, borderRadius: 4, cursor: 'pointer' }}>
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

/** Идентификаторы из details отказа сервера (scenarioIds / participantIds). */
function idList(value: unknown): string[] {
  return Array.isArray(value) ? value.filter((v): v is string => typeof v === 'string') : [];
}
