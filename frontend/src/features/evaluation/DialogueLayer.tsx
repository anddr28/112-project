import { Badge, Card, Meter, Metric } from '../../components/ui';
import type { ChecklistItemKind, ChecklistStatus, DialogueResult } from '../../shared/types';

/**
 * Разбор разговора с заявителем.
 *
 * Показывает только то, что пришло в `evaluation.dialogue`. Чек-лист эталона,
 * бриф заявителя и раскрытые факты сюда не попадают: обучающийся видит
 * результат своей работы, а не правильные ответы. Компонент общий для экрана
 * результата и разбора попытки — расхождений между ними быть не должно.
 */

const STATUS_LABEL: Record<ChecklistStatus, { text: string; tone: 'ok' | 'warn' | 'danger' | 'neutral' }> = {
  done: { text: 'выполнено', tone: 'ok' },
  partial: { text: 'частично', tone: 'warn' },
  missed: { text: 'пропущено', tone: 'danger' },
  not_applicable: { text: 'не требовалось', tone: 'neutral' },
};

const KIND_LABEL: Record<ChecklistItemKind, string> = {
  question: 'вопрос',
  instruction: 'указание',
  phrase: 'формулировка',
  behavior: 'поведение',
};

/** Паузы в разговоре читаются в секундах с десятыми, а не в миллисекундах. */
function seconds(ms: number): string {
  return `${(ms / 1000).toFixed(1).replace('.', ',')} с`;
}

export function DialogueLayer({ dialogue }: { dialogue: DialogueResult }) {
  const { speech, tone, checklist, missingQuestions, forbiddenHits, summaryForStudent } = dialogue;

  const fillers = Object.entries(dialogue.speech.fillers ?? {})
    .filter(([, count]) => count > 0)
    .map(([word, count]) => `${word} — ${count}`);

  return (
    <Card title="Разбор разговора">
      {summaryForStudent && <p style={{ marginTop: 0 }}>{summaryForStudent}</p>}

      <div className="grid grid--4" style={{ marginBottom: 14 }}>
        <Metric label="Реплик оператора" value={speech.operatorTurns} />
        <Metric label="Слов сказано" value={speech.operatorWords} />
        {speech.wordsPerMin != null && (
          <Metric label="Темп речи" value={`${speech.wordsPerMin}`} note="слов в минуту" />
        )}
        {speech.avgResponseMs != null && (
          <Metric
            label="Пауза перед ответом"
            value={seconds(speech.avgResponseMs)}
            note={speech.maxResponseMs != null ? `наибольшая ${seconds(speech.maxResponseMs)}` : undefined}
          />
        )}
        {speech.fillerCount != null && (
          <Metric
            label="Слова-паразиты"
            value={speech.fillerCount}
            note={fillers.length > 0 ? fillers.join(', ') : undefined}
            tone={speech.fillerCount > 3 ? 'warn' : undefined}
          />
        )}
        {speech.lowConfidenceTurns != null && speech.lowConfidenceTurns > 0 && (
          <Metric
            label="Распознано неуверенно"
            value={speech.lowConfidenceTurns}
            note="реплик — проговаривайте чётче"
            tone="warn"
          />
        )}
      </div>

      {checklist.length > 0 && (
        <>
          <div className="field__label" style={{ marginBottom: 6 }}>Протокол опроса</div>
          <table className="table">
            <thead>
              <tr><th>Пункт</th><th>Вид</th><th>Результат</th><th>Подтверждение</th></tr>
            </thead>
            <tbody>
              {checklist.map((item) => {
                const status = STATUS_LABEL[item.status];
                return (
                  <tr key={item.id}>
                    <td>
                      {item.text}
                      {item.required && <span className="dim small"> · обязательный</span>}
                      {item.comment && <div className="muted small">{item.comment}</div>}
                    </td>
                    <td className="muted small">{item.kind ? KIND_LABEL[item.kind] : '—'}</td>
                    <td><Badge tone={status.tone}>{status.text}</Badge></td>
                    <td className="muted small nowrap">
                      {item.evidenceTurnNo != null ? `ход ${item.evidenceTurnNo}` : '—'}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </>
      )}

      <div className="grid grid--2" style={{ marginTop: 14 }}>
        <div>
          <div className="field__label">Обязательные вопросы</div>
          {missingQuestions && missingQuestions.length > 0 ? (
            <ul style={{ margin: '4px 0 0', paddingLeft: 18 }}>
              {missingQuestions.map((q) => <li key={q}>{q}</li>)}
            </ul>
          ) : (
            <p className="muted small" style={{ margin: '4px 0 0' }}>Все обязательные вопросы заданы.</p>
          )}
        </div>

        <div>
          <div className="field__label">Запрещённые формулировки</div>
          {forbiddenHits && forbiddenHits.length > 0 ? (
            <ul style={{ margin: '4px 0 0', paddingLeft: 18 }}>
              {forbiddenHits.map((hit) => (
                <li key={`${hit.phrase}-${hit.turnNo}`}>
                  «{hit.phrase}» — ход {hit.turnNo}
                </li>
              ))}
            </ul>
          ) : (
            <p className="muted small" style={{ margin: '4px 0 0' }}>Не обнаружены.</p>
          )}
        </div>
      </div>

      {tone && (
        <div style={{ marginTop: 14 }}>
          <div className="field__label" style={{ marginBottom: 6 }}>Манера разговора</div>
          <div className="stack stack--tight">
            <ToneBar label="Вежливость" value={tone.politeness} />
            <ToneBar label="Спокойствие" value={tone.calmness} />
            <ToneBar label="Ясность речи" value={tone.clarity} />
          </div>
          {tone.comment && <p className="muted small" style={{ marginTop: 8 }}>{tone.comment}</p>}
        </div>
      )}
    </Card>
  );
}

function ToneBar({ label, value }: { label: string; value?: number }) {
  if (value == null) return null;
  return (
    <div className="row row--between" style={{ gap: 10 }}>
      <span className="muted small" style={{ minWidth: 110 }}>{label}</span>
      <div style={{ flex: 1 }}>
        <Meter value={value} tone={value >= 80 ? 'ok' : value >= 60 ? 'warn' : 'danger'} />
      </div>
      <span className="mono small" style={{ minWidth: 28, textAlign: 'right' }}>{value}</span>
    </div>
  );
}
