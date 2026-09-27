import type { AssignedService, Attempt, IncidentCardDraft } from '../../shared/types';

/** Памятка ДДС: «Принята» / «Не принята» — в течение 30 с после поступления карточки. */
export const DDS_DECISION_SEC = 30;
const DECISION_STATUSES = new Set(['Принята', 'Не принята']);

/** Первое решение своей службы по карточке и когда оно принято. */
export function ddsDecision(own: AssignedService | undefined, acceptedAt: string | undefined) {
  const entry = own?.history.find((h) => DECISION_STATUSES.has(h.status));
  if (!entry) return null;
  const ms = acceptedAt ? new Date(entry.at).getTime() - new Date(acceptedAt).getTime() : null;
  return { status: entry.status, ms };
}

/** Своя служба в карточке: сначала по id, затем по коду (карточку могли пересобрать). */
export function ownService(card: IncidentCardDraft, attempt: Attempt): AssignedService | undefined {
  const acting = attempt.actingService;
  if (!acting) return undefined;
  return card.services.find((s) => s.serviceId === acting.id) ?? card.services.find((s) => s.code === acting.code);
}
