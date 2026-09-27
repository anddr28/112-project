import { formatDateTime } from '../../shared/utils/time';
import { DDS_DECISION_SEC, ownService } from './ddsDecision';
import type { Attempt, IncidentCardDraft, ReactionExpectation } from '../../shared/types';

/**
 * Протокол реагирования диспетчера ДДС по сданной карточке: история статусов
 * своей службы и текст действия. Оценку по протоколу считает сервер
 * (engine.fields.source = dds_reaction) — здесь только то, что сделал обучающийся.
 *
 * `expected` — ожидание эталона (scoring.reaction), показывается преподавателю.
 */
export function DdsProtocolView({
  attempt,
  card,
  expected,
}: {
  attempt: Attempt;
  card: IncidentCardDraft;
  expected?: ReactionExpectation | null;
}) {
  const own = ownService(card, attempt);

  return (
    <div className="stack" style={{ gap: 12 }}>
      <div>
        <div className="field__label">Служба обучающегося</div>
        <div>{attempt.actingService?.name ?? own?.name ?? '—'}</div>
      </div>

      {expected !== undefined && (
        <div>
          <div className="field__label">Ожидается по эталону</div>
          <div className="small">
            Решение: <b>{expected?.decision === 'reject' ? '«Не принята»' : '«Принята»'}</b>
            {' '}в течение {expected?.decisionWithinSec ?? DDS_DECISION_SEC} с
            {expected?.requiredStatuses?.length ? <> · статусы: {expected.requiredStatuses.map((s) => `«${s}»`).join(', ')}</> : null}
          </div>
        </div>
      )}

      <div>
        <div className="field__label">Статусы своей службы</div>
        {!own || own.history.length === 0 ? (
          <p className="muted small">Статусов нет.</p>
        ) : (
          <table className="table">
            <thead><tr><th style={{ width: 150 }}>Время</th><th>Статус</th><th>Комментарий</th></tr></thead>
            <tbody>
              {own.history.map((h, i) => (
                <tr key={`${h.status}-${i}`}>
                  <td className="mono small nowrap">{formatDateTime(h.at)}</td>
                  <td>{h.status}<span className="dim small"> · {h.operator}</span></td>
                  <td className="muted small">
                    {h.comment || '—'}
                    {h.squadNumber ? ` (наряд ${h.squadNumber})` : ''}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      <div>
        <div className="field__label">Текст действия</div>
        {card.actionsTaken.trim() ? (
          <p style={{ whiteSpace: 'pre-wrap' }}>{card.actionsTaken}</p>
        ) : (
          <p className="muted small">Не заполнен.</p>
        )}
      </div>
    </div>
  );
}
