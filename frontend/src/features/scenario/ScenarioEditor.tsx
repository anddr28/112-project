import { useState } from 'react';
import { api, hasErrorCode } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { Card, Field, NumberInput } from '../../components/ui';
import { scenarioSaveError } from './versioning';
import { TtsPreviewButton } from './TtsPreviewButton';
import { APPLICANT_STATUSES } from '../../shared/types';
import type {
  ApplicantStatus, ChecklistItemKind, DialogueBrief, DialogueChecklistItem, DialogueFact,
  Difficulty, ExpectedDialogue, FactReveal, IncidentCardDraft, Scenario,
} from '../../shared/types';

/**
 * Редактор сценария.
 *
 * Правит те же поля модели, по которым потом считается оценка: легенду
 * звонка, эталон карточки, состав обязательных полей и параметры разговора.
 * Ничего сверх модели здесь не появляется — интерфейс не может задать то,
 * чего не существует в контракте.
 *
 * Сохранение одним действием: `api.scenarios.update` принимает частичный
 * сценарий, правка эталона поднимает его версию на стороне сервиса.
 */

const REVEAL_LABEL: Record<FactReveal, string> = {
  volunteer: 'расскажет сам',
  on_request: 'только по вопросу',
  never: 'не расскажет',
};

const KIND_LABEL: Record<ChecklistItemKind, string> = {
  question: 'вопрос',
  instruction: 'указание',
  phrase: 'формулировка',
  behavior: 'поведение',
};

/** Поля карточки, которые преподаватель может сделать обязательными. */
const REQUIRABLE_FIELDS: Array<{ path: string; label: string }> = [
  { path: 'applicant.name', label: 'ФИО заявителя' },
  { path: 'applicant.status', label: 'Статус заявителя' },
  { path: 'phones.aon', label: 'Телефон АОН' },
  { path: 'address.raw', label: 'Адрес' },
  { path: 'incidentTypeIds', label: 'Тип происшествия' },
  { path: 'description', label: 'Описание со слов заявителя' },
  { path: 'flags.victimsPresent', label: 'Пострадавшие' },
  { path: 'services', label: 'Службы' },
];

type Draft = {
  title: string;
  difficulty: Difficulty;
  callerName: string;
  callerPhone: string;
  callerRole: string;
  callerEmotion: string;
  keyFacts: string;
  turns: Array<{ speaker: 'caller' | 'operator_hint'; text: string }>;
  card: IncidentCardDraft;
  requiredFields: string[];
  brief: DialogueBrief | null;
  checklist: DialogueChecklistItem[];
};

function toDraft(s: Scenario): Draft {
  return {
    title: s.title,
    difficulty: s.difficulty,
    callerName: s.callScript.caller.name ?? '',
    callerPhone: s.callScript.caller.phone ?? '',
    callerRole: s.callScript.caller.role ?? '',
    callerEmotion: s.callScript.caller.emotionalState ?? '',
    keyFacts: s.callScript.keyFacts.join('\n'),
    turns: s.callScript.turns.map((t) => ({ speaker: t.speaker, text: t.text })),
    card: JSON.parse(JSON.stringify(s.etalonDraft)) as IncidentCardDraft,
    requiredFields: [...s.requiredFields],
    brief: s.callScript.dialogue ? (JSON.parse(JSON.stringify(s.callScript.dialogue)) as DialogueBrief) : null,
    checklist: s.expectedDialogue ? [...s.expectedDialogue.checklist] : [],
  };
}

function toPatch(d: Draft, s: Scenario): Partial<Scenario> {
  const expectedDialogue: ExpectedDialogue | undefined =
    d.checklist.length > 0
      ? { ...(s.expectedDialogue ?? {}), checklist: d.checklist }
      : undefined;

  return {
    title: d.title.trim(),
    difficulty: d.difficulty,
    callScript: {
      ...s.callScript,
      caller: {
        name: d.callerName.trim() || undefined,
        phone: d.callerPhone.trim() || undefined,
        role: d.callerRole.trim() || undefined,
        emotionalState: d.callerEmotion.trim() || undefined,
      },
      keyFacts: d.keyFacts.split('\n').map((x) => x.trim()).filter(Boolean),
      turns: d.turns.filter((t) => t.text.trim()).map((t) => ({ speaker: t.speaker, text: t.text.trim() })),
      dialogue: d.brief ?? undefined,
    },
    etalonDraft: d.card,
    requiredFields: d.requiredFields,
    expectedDialogue,
  };
}

export function ScenarioEditor({
  scenario,
  onSaved,
  onConflict,
  onCancel,
}: {
  scenario: Scenario;
  onSaved: () => void;
  /** сценарий успел попасть в занятие — страница перечитает его и предложит новую версию */
  onConflict?: (message: string) => void;
  onCancel: () => void;
}) {
  const types = useAsync(() => api.classifier.incidentTypes(), []);
  const services = useAsync(() => api.services.list(), []);

  const [draft, setDraft] = useState<Draft>(() => toDraft(scenario));
  const [typeQuery, setTypeQuery] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const card = draft.card;
  const patch = (next: Partial<Draft>) => setDraft((prev) => ({ ...prev, ...next }));
  const patchCard = (next: Partial<IncidentCardDraft>) =>
    setDraft((prev) => ({ ...prev, card: { ...prev.card, ...next } }));

  async function save() {
    setBusy(true);
    setError(null);
    try {
      await api.scenarios.update(scenario.id, toPatch(draft, scenario));
      onSaved();
    } catch (e) {
      const message = scenarioSaveError(e, scenario, 'Не удалось сохранить сценарий');
      setError(message);
      setBusy(false);
      if (hasErrorCode(e, 'conflict')) onConflict?.(message);
    }
  }

  return (
    <div className="stack" style={{ gap: 16 }}>
      {error && <p className="field__error" role="alert">{error}</p>}

      <Card title="Общие сведения">
        <div className="grid grid--3">
          <Field label="Название сценария">
            <input className="input" value={draft.title} onChange={(e) => patch({ title: e.target.value })} />
          </Field>
          <Field label="Сложность">
            <select
              className="select"
              value={draft.difficulty}
              onChange={(e) => patch({ difficulty: Number(e.target.value) as Difficulty })}
            >
              <option value={1}>1 — базовая</option>
              <option value={2}>2 — средняя</option>
              <option value={3}>3 — высокая</option>
            </select>
          </Field>
          <Field label="Категория" hint="Задаётся при создании сценария">
            <input className="input" value={scenario.categoryName} readOnly />
          </Field>
        </div>
      </Card>

      <Card title="Легенда звонка">
        <div className="grid grid--4">
          <Field label="Имя заявителя">
            <input className="input" value={draft.callerName} onChange={(e) => patch({ callerName: e.target.value })} />
          </Field>
          <Field label="Телефон">
            <input className="input" value={draft.callerPhone} onChange={(e) => patch({ callerPhone: e.target.value })} />
          </Field>
          <Field label="Кто звонит" hint="очевидец, сосед, житель…">
            <input className="input" value={draft.callerRole} onChange={(e) => patch({ callerRole: e.target.value })} />
          </Field>
          <Field label="Состояние" hint="паника, спокоен, встревожен">
            <input className="input" value={draft.callerEmotion} onChange={(e) => patch({ callerEmotion: e.target.value })} />
          </Field>
        </div>

        <Field
          label="Ключевые факты обращения"
          hint="По одному в строке. Обучающемуся не показываются — по ним проверяется описание."
        >
          <textarea
            className="textarea"
            rows={4}
            value={draft.keyFacts}
            onChange={(e) => patch({ keyFacts: e.target.value })}
          />
        </Field>

        <div className="field__label" style={{ margin: '12px 0 6px' }}>
          Реплики ({draft.turns.length})
        </div>
        <div className="stack" style={{ gap: 6 }}>
          {draft.turns.map((turn, i) => (
            <div key={i} className="row row--tight" style={{ alignItems: 'flex-start' }}>
              <select
                className="select"
                style={{ width: 190 }}
                value={turn.speaker}
                aria-label={`Кто говорит в реплике ${i + 1}`}
                onChange={(e) => {
                  const turns = [...draft.turns];
                  turns[i] = { ...turn, speaker: e.target.value as 'caller' | 'operator_hint' };
                  patch({ turns });
                }}
              >
                <option value="caller">Заявитель</option>
                <option value="operator_hint">Подсказка оператору</option>
              </select>
              <input
                className="input"
                style={{ flex: 1 }}
                value={turn.text}
                aria-label={`Текст реплики ${i + 1}`}
                onChange={(e) => {
                  const turns = [...draft.turns];
                  turns[i] = { ...turn, text: e.target.value };
                  patch({ turns });
                }}
              />
              {turn.speaker === 'caller' && <TtsPreviewButton scenarioId={scenario.id} text={turn.text} />}
              <button
                type="button"
                className="btn btn--sm"
                onClick={() => patch({ turns: draft.turns.filter((_, j) => j !== i) })}
              >
                Убрать
              </button>
            </div>
          ))}
          <div>
            <button
              type="button"
              className="btn btn--sm"
              onClick={() => patch({ turns: [...draft.turns, { speaker: 'caller', text: '' }] })}
            >
              Добавить реплику
            </button>
          </div>
        </div>
      </Card>

      <Card title="Эталон карточки">
        <div className="grid grid--3">
          <Field label="ФИО заявителя">
            <input
              className="input"
              value={card.applicant.name ?? ''}
              onChange={(e) => patchCard({ applicant: { ...card.applicant, name: e.target.value } })}
            />
          </Field>
          <Field label="Статус заявителя">
            <select
              className="select"
              value={card.applicant.status ?? ''}
              onChange={(e) =>
                patchCard({
                  applicant: { ...card.applicant, status: (e.target.value || undefined) as ApplicantStatus | undefined },
                })
              }
            >
              <option value="">не задан</option>
              {APPLICANT_STATUSES.map((st) => <option key={st} value={st}>{st}</option>)}
            </select>
          </Field>
          <Field label="Телефон АОН">
            <input
              className="input"
              value={card.phones.aon ?? ''}
              onChange={(e) => patchCard({ phones: { ...card.phones, aon: e.target.value } })}
            />
          </Field>
        </div>

        <div className="grid grid--4">
          <Field label="Адрес одной строкой">
            <input
              className="input"
              value={card.address.raw}
              onChange={(e) => patchCard({ address: { ...card.address, raw: e.target.value } })}
            />
          </Field>
          <Field label="Улица">
            <input
              className="input"
              value={card.address.street}
              onChange={(e) => patchCard({ address: { ...card.address, street: e.target.value } })}
            />
          </Field>
          <Field label="Дом">
            <input
              className="input"
              value={card.address.house}
              onChange={(e) => patchCard({ address: { ...card.address, house: e.target.value } })}
            />
          </Field>
          <Field label="Подъезд">
            <input
              className="input"
              value={card.address.entrance}
              onChange={(e) => patchCard({ address: { ...card.address, entrance: e.target.value } })}
            />
          </Field>
        </div>

        <Field label="Описание со слов заявителя">
          <textarea
            className="textarea"
            rows={3}
            value={card.description}
            onChange={(e) => patchCard({ description: e.target.value })}
          />
        </Field>

        <div className="grid grid--3" style={{ marginTop: 12 }}>
          <Field label="Пострадавшие">
            <select
              className="select"
              value={card.flags.victimsPresent ? 'yes' : 'no'}
              onChange={(e) =>
                patchCard({
                  flags: {
                    ...card.flags,
                    victimsPresent: e.target.value === 'yes',
                    victimsCount: e.target.value === 'yes' ? (card.flags.victimsCount ?? 1) : undefined,
                  },
                })
              }
            >
              <option value="no">нет</option>
              <option value="yes">есть</option>
            </select>
          </Field>
          {card.flags.victimsPresent && (
            <Field label="Количество пострадавших">
              <NumberInput
                min={1}
                value={card.flags.victimsCount ?? 1}
                onChange={(victimsCount) => patchCard({ flags: { ...card.flags, victimsCount } })}
              />
            </Field>
          )}
        </div>

        <div className="field__label" style={{ margin: '12px 0 6px' }}>
          Тип происшествия ({card.incidentTypeIds.length} выбрано)
        </div>
        {types.loading ? (
          <p className="muted small">Загрузка классификатора…</p>
        ) : (
          <>
            <input
              className="input"
              style={{ marginBottom: 6 }}
              value={typeQuery}
              placeholder="Поиск по классификатору"
              aria-label="Поиск типа происшествия"
              onChange={(e) => setTypeQuery(e.target.value)}
            />
            <div className="row row--tight" style={{ flexWrap: 'wrap', maxHeight: 150, overflow: 'auto' }}>
            {(types.data ?? []).filter((t) => t.name.toLowerCase().includes(typeQuery.trim().toLowerCase())).map((t) => {
              const on = card.incidentTypeIds.includes(t.id);
              return (
                <button
                  key={t.id}
                  type="button"
                  className={`btn btn--sm${on ? ' btn--primary' : ''}`}
                  onClick={() =>
                    patchCard({
                      incidentTypeIds: on
                        ? card.incidentTypeIds.filter((x) => x !== t.id)
                        : [...card.incidentTypeIds, t.id],
                    })
                  }
                >
                  {t.name}
                </button>
              );
            })}
            </div>
          </>
        )}

        <div className="field__label" style={{ margin: '12px 0 6px' }}>
          Службы эталона ({card.services.length})
        </div>
        {services.loading ? (
          <p className="muted small">Загрузка справочника служб…</p>
        ) : (
          <div className="row row--tight" style={{ flexWrap: 'wrap' }}>
            {(services.data ?? []).map((svc) => {
              const on = card.services.some((x) => x.code === svc.code);
              return (
                <button
                  key={svc.id}
                  type="button"
                  className={`btn btn--sm${on ? ' btn--primary' : ''}`}
                  title={svc.name}
                  onClick={() =>
                    patchCard({
                      services: on
                        ? card.services.filter((x) => x.code !== svc.code)
                        : [
                            ...card.services,
                            {
                              serviceId: svc.id,
                              code: svc.code,
                              name: svc.name,
                              shortName: svc.shortName,
                              isPrimary: false,
                              source: 'auto',
                              currentStatus: 'Добавлена',
                              currentStatusAt: new Date().toISOString(),
                              history: [],
                              allowedNext: [],
                              editable: true,
                            },
                          ],
                    })
                  }
                >
                  {svc.shortName}
                </button>
              );
            })}
          </div>
        )}
      </Card>

      <Card title="Обязательные поля карточки">
        <p className="field__hint" style={{ marginTop: 0 }}>
          По этому списку сверяется карточка обучающегося. Пустой список не даст
          подтвердить сценарий.
        </p>
        <div className="row row--tight" style={{ flexWrap: 'wrap' }}>
          {REQUIRABLE_FIELDS.map((f) => {
            const on = draft.requiredFields.includes(f.path);
            return (
              <button
                key={f.path}
                type="button"
                className={`btn btn--sm${on ? ' btn--primary' : ''}`}
                onClick={() =>
                  patch({
                    requiredFields: on
                      ? draft.requiredFields.filter((x) => x !== f.path)
                      : [...draft.requiredFields, f.path],
                  })
                }
              >
                {f.label}
              </button>
            );
          })}
        </div>
      </Card>

      <Card title="Разговор с заявителем">
        <p className="field__hint" style={{ marginTop: 0 }}>
          Нужен голосовым занятиям: бриф определяет, что заявитель расскажет,
          чек-лист — что обязан спросить оператор. Обучающемуся не показывается.
          Ключевые слова факта — то, по чему заявитель узнаёт вопрос о нём.
        </p>

        {!draft.brief ? (
          <button
            type="button"
            className="btn btn--sm"
            onClick={() =>
              patch({ brief: { persona: '', speakingStyle: '', facts: [], maxTurns: 12 } })
            }
          >
            Добавить бриф заявителя
          </button>
        ) : (
          <>
            <div className="grid grid--2">
              <Field label="Кто заявитель" hint="Короткое описание роли">
                <input
                  className="input"
                  value={draft.brief.persona}
                  onChange={(e) => patch({ brief: { ...draft.brief!, persona: e.target.value } })}
                />
              </Field>
              <Field label="Манера речи">
                <input
                  className="input"
                  value={draft.brief.speakingStyle ?? ''}
                  onChange={(e) => patch({ brief: { ...draft.brief!, speakingStyle: e.target.value } })}
                />
              </Field>
            </div>

            <div className="field__label" style={{ margin: '12px 0 6px' }}>
              Факты заявителя ({draft.brief.facts.length})
            </div>
            <div className="stack" style={{ gap: 6 }}>
              {draft.brief.facts.map((fact, i) => (
                <div key={fact.id || i} className="row row--tight">
                  <input
                    className="input"
                    style={{ flex: 1 }}
                    value={fact.text}
                    aria-label={`Текст факта ${i + 1}`}
                    onChange={(e) => {
                      const facts = [...draft.brief!.facts];
                      facts[i] = { ...fact, text: e.target.value };
                      patch({ brief: { ...draft.brief!, facts } });
                    }}
                  />
                  <input
                    className="input"
                    style={{ width: 210 }}
                    value={(fact.hints ?? []).join(', ')}
                    placeholder="Ключевые слова через запятую"
                    aria-label={`Ключевые слова факта ${i + 1}`}
                    title="По этим словам заявитель понимает, что факт спрашивают"
                    onChange={(e) => {
                      const facts = [...draft.brief!.facts];
                      facts[i] = {
                        ...fact,
                        hints: e.target.value.split(',').map((x) => x.trim()).filter(Boolean),
                      };
                      patch({ brief: { ...draft.brief!, facts } });
                    }}
                  />
                  <select
                    className="select"
                    style={{ width: 190 }}
                    value={fact.reveal}
                    aria-label={`Когда заявитель сообщит факт ${i + 1}`}
                    onChange={(e) => {
                      const facts = [...draft.brief!.facts];
                      facts[i] = { ...fact, reveal: e.target.value as FactReveal };
                      patch({ brief: { ...draft.brief!, facts } });
                    }}
                  >
                    {(Object.keys(REVEAL_LABEL) as FactReveal[]).map((r) => (
                      <option key={r} value={r}>{REVEAL_LABEL[r]}</option>
                    ))}
                  </select>
                  <button
                    type="button"
                    className="btn btn--sm"
                    onClick={() =>
                      patch({
                        brief: { ...draft.brief!, facts: draft.brief!.facts.filter((_, j) => j !== i) },
                      })
                    }
                  >
                    Убрать
                  </button>
                </div>
              ))}
              <div>
                <button
                  type="button"
                  className="btn btn--sm"
                  onClick={() => {
                    const fact: DialogueFact = {
                      id: `fact-${draft.brief!.facts.length + 1}`,
                      text: '',
                      reveal: 'on_request',
                    };
                    patch({ brief: { ...draft.brief!, facts: [...draft.brief!.facts, fact] } });
                  }}
                >
                  Добавить факт
                </button>
              </div>
            </div>
          </>
        )}

        <div className="field__label" style={{ margin: '14px 0 6px' }}>
          Чек-лист протокола опроса ({draft.checklist.length})
        </div>
        <div className="stack" style={{ gap: 6 }}>
          {draft.checklist.map((item, i) => (
            <div key={item.id || i} className="row row--tight">
              <input
                className="input"
                style={{ flex: 1 }}
                value={item.text}
                aria-label={`Текст пункта ${i + 1}`}
                onChange={(e) => {
                  const checklist = [...draft.checklist];
                  checklist[i] = { ...item, text: e.target.value };
                  patch({ checklist });
                }}
              />
              <input
                className="input"
                style={{ width: 200 }}
                value={(item.hints ?? []).join(', ')}
                placeholder="Ключевые слова (необязательно)"
                aria-label={`Ключевые слова пункта ${i + 1}`}
                title="По ним пункт считается выполненным; без них — по словам самого пункта"
                onChange={(e) => {
                  const checklist = [...draft.checklist];
                  checklist[i] = {
                    ...item,
                    hints: e.target.value.split(',').map((x) => x.trim()).filter(Boolean),
                  };
                  patch({ checklist });
                }}
              />
              <select
                className="select"
                style={{ width: 150 }}
                value={item.kind}
                aria-label={`Вид пункта ${i + 1}`}
                onChange={(e) => {
                  const checklist = [...draft.checklist];
                  checklist[i] = { ...item, kind: e.target.value as ChecklistItemKind };
                  patch({ checklist });
                }}
              >
                {(Object.keys(KIND_LABEL) as ChecklistItemKind[]).map((k) => (
                  <option key={k} value={k}>{KIND_LABEL[k]}</option>
                ))}
              </select>
              <button
                type="button"
                className={`btn btn--sm${item.required ? ' btn--primary' : ''}`}
                title="Обязательный пункт протокола"
                onClick={() => {
                  const checklist = [...draft.checklist];
                  checklist[i] = { ...item, required: !item.required };
                  patch({ checklist });
                }}
              >
                {item.required ? 'обязательный' : 'желательный'}
              </button>
              <button
                type="button"
                className="btn btn--sm"
                onClick={() => patch({ checklist: draft.checklist.filter((_, j) => j !== i) })}
              >
                Убрать
              </button>
            </div>
          ))}
          <div>
            <button
              type="button"
              className="btn btn--sm"
              onClick={() =>
                patch({
                  checklist: [
                    ...draft.checklist,
                    {
                      id: `chk-${draft.checklist.length + 1}`,
                      text: '',
                      kind: 'question',
                      required: true,
                      weight: 1,
                    },
                  ],
                })
              }
            >
              Добавить пункт
            </button>
          </div>
        </div>
      </Card>

      <div className="row row--tight">
        <button type="button" className="btn btn--primary" disabled={busy} onClick={() => void save()}>
          {busy ? 'Сохранение…' : 'Сохранить сценарий'}
        </button>
        <button type="button" className="btn" disabled={busy} onClick={onCancel}>
          Отмена
        </button>
      </div>
    </div>
  );
}
