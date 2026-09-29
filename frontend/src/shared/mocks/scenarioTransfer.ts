/**
 * FIXTURE: перенос сценариев и карточка обучающегося как сценарий (v1.4) —
 * правила те же, что описаны у эндпоинтов контракта, чтобы демо на моках не
 * расходилось с ядром. Плюс HTML-сертификат вместо серверного PDF.
 */

import { ApiRequestError } from '../api/error';
import type { Scenario, ScenarioBundle, ScenarioImportResult, StudentProgress, User } from '../types';
import { uid } from '../utils/id';
import { typeById } from './fixtures/classifier';
import { attemptById, db, persistScenario, scenarioById } from './db';

type BundleItem = ScenarioBundle['scenarios'][number];

function clone<T>(value: T): T {
  return JSON.parse(JSON.stringify(value)) as T;
}

export function exportBundle(ids?: string[]): ScenarioBundle {
  const list = ids?.length
    ? ids.map((id) => scenarioById(id)).filter((s): s is Scenario => Boolean(s))
    : db.scenarios.filter((s) => s.status === 'validated');
  return {
    format: 'lct112.scenarios.v1',
    exportedAt: new Date().toISOString(),
    // Переносимое содержание: без id, статусов, авторов и версий.
    scenarios: list.map((s) => clone({
      title: s.title,
      categoryId: s.categoryId,
      difficulty: s.difficulty,
      mode: s.mode,
      source: s.source,
      callScript: s.callScript,
      etalonCard: s.etalonCard,
      etalonDraft: s.etalonDraft,
      scoring: s.scoring,
      requiredFields: s.requiredFields,
      expectedActions: s.expectedActions,
      expectedDialogue: s.expectedDialogue,
      notesForTeacher: s.notesForTeacher,
    }) as BundleItem),
  };
}

/** Почему элемент пакета нельзя импортировать; null — можно. */
function rejectReason(item: BundleItem): string | null {
  if (!item || typeof item !== 'object') return 'Элемент пакета не является сценарием';
  if (!item.title?.trim()) return 'Нет названия';
  if (!typeById(item.categoryId)) return `Неизвестная категория «${item.categoryId}»`;
  if (![1, 2, 3].includes(item.difficulty)) return 'Сложность должна быть 1, 2 или 3';
  if (!['cards', 'card_actions', 'both'].includes(item.mode)) return 'Неизвестный режим';
  if (!item.callScript || !Array.isArray(item.callScript.turns)) return 'Нет легенды звонка';
  if (!item.etalonDraft) return 'Нет эталона карточки';
  return null;
}

export function importBundle(bundle: ScenarioBundle, author: User): ScenarioImportResult {
  if (bundle?.format !== 'lct112.scenarios.v1' || !Array.isArray(bundle.scenarios)) {
    throw new ApiRequestError('validation', 'Файл не является пакетом сценариев lct112.scenarios.v1', { status: 400 });
  }
  if (bundle.scenarios.length > 500) {
    throw new ApiRequestError('validation', 'В пакете больше 500 сценариев', { status: 400 });
  }
  const result: ScenarioImportResult = { created: [], rejected: [] };
  bundle.scenarios.forEach((item, index) => {
    const reason = rejectReason(item);
    if (reason) {
      result.rejected.push({ index, title: item?.title, message: reason });
      return;
    }
    const s: Scenario = {
      ...(clone(item) as unknown as Omit<Scenario, 'id'>),
      id: uid('sc'),
      categoryName: typeById(item.categoryId)?.name ?? item.categoryId,
      source: item.source ?? 'manual',
      status: 'draft',
      etalonVersion: 1,
      authorId: author.id,
      createdAt: new Date().toISOString(),
      requiredFields: item.requiredFields ?? [],
      etalonCard: item.etalonCard ?? {},
    } as Scenario;
    db.scenarios.unshift(s);
    persistScenario(s);
    result.created.push({ index, scenarioId: s.id, title: s.title });
  });
  return result;
}

const FROM_ATTEMPT_KEY = 'arm112.mock.scenarioFromAttempt';

function readLinks(): Record<string, string> {
  try {
    return JSON.parse(sessionStorage.getItem(FROM_ATTEMPT_KEY) ?? '{}') as Record<string, string>;
  } catch {
    return {};
  }
}

export function scenarioFromAttempt(attemptId: string, title: string | undefined, author: User): Scenario {
  const attempt = attemptById(attemptId);
  if (!attempt) throw new ApiRequestError('not_found', 'Попытка не найдена', { status: 404 });
  const links = readLinks();
  const existing = links[attemptId];
  if (existing && scenarioById(existing)) {
    throw new ApiRequestError('conflict', 'Сценарий по этой карточке уже создан', { status: 409, details: { scenarioId: existing } });
  }
  if (attempt.status !== 'evaluated' || !attempt.card) {
    throw new ApiRequestError('conflict', 'Сценарий можно создать только из оценённой карточки', { status: 409 });
  }
  if (attempt.mode !== 'cards' || attempt.perspective !== 'operator112') {
    throw new ApiRequestError('conflict', 'Сценарий создаётся только из карточки оператора 112 (режим «Карточки»)', { status: 409 });
  }
  const origin = scenarioById(attempt.scenarioId);
  if (!origin) throw new ApiRequestError('not_found', 'Исходный сценарий не найден', { status: 404 });
  const student = db.users.find((u) => u.id === attempt.userId);
  const s: Scenario = {
    ...clone(origin),
    id: uid('sc'),
    title: title?.trim() || `${origin.title} — карточка ${student ? student.lastName : 'обучающегося'}`,
    source: 'student',
    status: 'draft',
    // Карточка обучающегося — эталон v1; легенда и категория — из исходного сценария.
    etalonDraft: clone(attempt.card),
    etalonVersion: 1,
    version: 1,
    authorId: author.id,
    createdAt: new Date().toISOString(),
    notesForTeacher: `Эталон взят из карточки № ${attempt.incidentNo}. Проверьте его перед подтверждением.`,
  };
  delete s.parentScenarioId;
  delete s.validatedBy;
  delete s.validatedAt;
  db.scenarios.unshift(s);
  persistScenario(s);
  links[attemptId] = s.id;
  try {
    sessionStorage.setItem(FROM_ATTEMPT_KEY, JSON.stringify(links));
  } catch {
    // хранилище недоступно: повтор после перезагрузки создаст вторую копию
  }
  return s;
}

function esc(text: string): string {
  return text.replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c] ?? c);
}

/** Сертификат той же формы, что серверный PDF: уровень, карточки, средний балл, категории. */
export function certificateHtml(p: StudentProgress): string {
  const rows = p.categories
    .map((c) => `<tr><td>${esc(c.categoryName)}</td><td>${c.attemptsDone}</td><td>${c.avgScore ?? '—'}</td><td>${c.passRatePct ?? '—'}%</td></tr>`)
    .join('');
  return `<!doctype html><html lang="ru"><meta charset="utf-8"><title>Сертификат</title>
<style>body{font:14px 'Segoe UI',Arial,sans-serif;max-width:720px;margin:40px auto;color:#16191d}
h1{font-size:24px;margin:0 0 4px}table{border-collapse:collapse;width:100%;margin-top:16px}
td,th{border:1px solid #b3bac2;padding:6px 8px;text-align:left}.dim{color:#565c64}</style>
<p class="dim">Тренажёр АРМ-112 · демонстрационный режим (PDF формирует сервер)</p>
<h1>Сертификат о прохождении подготовки</h1>
<p>Выдан: <b>${esc(p.name)}</b> · ${new Date().toLocaleDateString('ru-RU')}</p>
<p>Уровень: <b>${esc(p.level.title)}</b> (${p.xp} XP) · оценённых карточек: <b>${p.attemptsDone}</b> ·
средний балл: <b>${p.avgScore ?? '—'}</b> · зачёт: <b>${p.passRatePct ?? 0}%</b></p>
<table><tr><th>Категория</th><th>Карточек</th><th>Средний балл</th><th>Зачёт</th></tr>${rows}</table></html>`;
}
