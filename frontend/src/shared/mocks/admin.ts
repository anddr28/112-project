/**
 * FIXTURE: технический раздел администратора в mock-режиме.
 *
 * Журналы аудита и системы, резервные копии и настройки ведёт go-core; здесь —
 * правдоподобные записи тех же форм, чтобы экраны можно было проверить без
 * сервера. Состояние контура честно говорит, что сервера нет: подменять его
 * «зелёными» цифрами нельзя — администратор принял бы их за правду.
 */

import type { AuditFilter } from '../api/types';
import { ApiRequestError } from '../api/error';
import type { AuditEntry, Backup, LogEntry, LogLevel, Setting, SystemHealth } from '../types';

const MIN = 60_000;

// ───────────────────────────────────────────────────────────────── аудит

const AUDIT_TEMPLATES: Array<Omit<AuditEntry, 'id' | 'at'>> = [
  { actorId: 'u-teacher', actorName: 'Ковалёва И. С.', actorRole: 'teacher', action: 'user.login', entityType: 'user', ip: '10.10.4.12' },
  { actorId: 'u-student', actorName: 'Рожкова О. И.', actorRole: 'student', action: 'user.login', entityType: 'user', ip: '10.10.4.31' },
  { actorId: 'u-teacher', actorName: 'Ковалёва И. С.', actorRole: 'teacher', action: 'scenario.approve', entityType: 'scenario', before: { status: 'generated' }, after: { status: 'validated' } },
  { actorId: 'u-teacher', actorName: 'Ковалёва И. С.', actorRole: 'teacher', action: 'scenario.update', entityType: 'scenario', before: { title: 'Пожар в квартире', difficulty: 1 }, after: { title: 'Пожар в квартире многоквартирного дома', difficulty: 2 } },
  { actorId: 'u-teacher', actorName: 'Ковалёва И. С.', actorRole: 'teacher', action: 'lesson.create', entityType: 'lesson', after: { title: 'Практическое занятие: пожары и запах газа', timeLimitSec: 30 } },
  { actorId: 'u-teacher', actorName: 'Ковалёва И. С.', actorRole: 'teacher', action: 'lesson.start', entityType: 'lesson', before: { status: 'draft' }, after: { status: 'running' } },
  { actorId: 'u-teacher', actorName: 'Ковалёва И. С.', actorRole: 'teacher', action: 'evaluation.override', entityType: 'evaluation', before: { finalScore: 64, verdict: 'fail' }, after: { finalScore: 72, verdict: 'pass', reason: 'Этаж указан в описании, поле пропущено технически' } },
  { actorId: 'u-student-2', actorName: 'Никитин П. А.', actorRole: 'student', action: 'user.login', entityType: 'user', ip: '10.10.4.33' },
  { actorId: 'u-admin', actorName: 'Глущенко О. И.', actorRole: 'admin', action: 'user.block', entityType: 'user', before: { status: 'active' }, after: { status: 'blocked' } },
  { actorId: 'u-admin', actorName: 'Глущенко О. И.', actorRole: 'admin', action: 'user.unblock', entityType: 'user', before: { status: 'blocked' }, after: { status: 'active' } },
  { actorId: 'u-admin', actorName: 'Глущенко О. И.', actorRole: 'admin', action: 'settings.update', entityType: 'setting', before: { key: 'confidence_threshold', value: 0.6 }, after: { key: 'confidence_threshold', value: 0.7 } },
  { actorId: 'u-admin', actorName: 'Глущенко О. И.', actorRole: 'admin', action: 'backup.run', entityType: 'backup' },
  { actorId: 'u-teacher', actorName: 'Ковалёва И. С.', actorRole: 'teacher', action: 'material.create', entityType: 'material', after: { title: 'Разбор типичных ошибок группы' } },
  { actorId: 'u-teacher', actorName: 'Ковалёва И. С.', actorRole: 'teacher', action: 'lesson.finish', entityType: 'lesson', before: { status: 'running' }, after: { status: 'finished' } },
  { actorId: 'u-student', actorName: 'Рожкова О. И.', actorRole: 'student', action: 'user.logout', entityType: 'user', ip: '10.10.4.31' },
];

const ENTITY_IDS: Partial<Record<string, string>> = {
  user: '6f1d2c1e-8a3b-4c55-9d11-2b0f7e6a1c01',
  scenario: '0b7a9c4e-1f2d-4e8a-b3c5-7d9e1f2a3b4c',
  lesson: '9c8b7a6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d',
  evaluation: '3e2d1c0b-9a8f-4e7d-a6c5-b4a3928170ff',
  backup: '5a4b3c2d-1e0f-4a9b-8c7d-6e5f4a3b2c1d',
  material: '7d6c5b4a-3e2f-4d1c-9b8a-7f6e5d4c3b2a',
};

/** 180 записей за последние трое суток, новые сверху (id убывают). */
const AUDIT: AuditEntry[] = (() => {
  const now = Date.now();
  const out: AuditEntry[] = [];
  for (let i = 0; i < 180; i += 1) {
    const t = AUDIT_TEMPLATES[(i * 7) % AUDIT_TEMPLATES.length];
    out.push({
      ...t,
      id: 5400 - i,
      at: new Date(now - i * 23 * MIN - (i % 5) * 3 * MIN).toISOString(),
      entityId: t.entityType ? ENTITY_IDS[t.entityType] : undefined,
    });
  }
  return out;
})();

export function listAudit(f: AuditFilter): AuditEntry[] {
  const from = f.from ? Date.parse(f.from) : undefined;
  const to = f.to ? Date.parse(f.to) : undefined;
  return AUDIT.filter((e) => {
    if (f.beforeId != null && e.id >= f.beforeId) return false;
    if (f.actorId && e.actorId !== f.actorId) return false;
    // Контракт: точное имя или префикс с точкой («user.»).
    if (f.action && !(f.action.endsWith('.') ? e.action.startsWith(f.action) : e.action === f.action)) return false;
    if (f.entityType && e.entityType !== f.entityType) return false;
    if (f.entityId && e.entityId !== f.entityId) return false;
    if (from != null && Date.parse(e.at) < from) return false;
    if (to != null && Date.parse(e.at) > to) return false;
    return true;
  }).slice(0, Math.min(f.limit ?? 100, 500));
}

// ─────────────────────────────────────────────────────── резервные копии

const BACKUP_MS = 7000;
const BACKUPS_KEY = 'arm112.mock.backups';

function seedBackups(): Backup[] {
  const today3am = new Date();
  today3am.setHours(3, 0, 0, 0);
  if (today3am.getTime() > Date.now()) today3am.setDate(today3am.getDate() - 1);
  return Array.from({ length: 7 }, (_, i) => {
    const started = today3am.getTime() - i * 86_400_000;
    const date = new Date(started).toISOString().slice(0, 10);
    // Одна неудачная копия в неделю — чтобы экран было на чём проверить.
    if (i === 4) {
      return {
        id: `bk-${date}`,
        startedAt: new Date(started).toISOString(),
        finishedAt: new Date(started + 4000).toISOString(),
        status: 'failed' as const,
        error: 'pg_dump: нет свободного места на томе /backups',
      };
    }
    return {
      id: `bk-${date}`,
      startedAt: new Date(started).toISOString(),
      finishedAt: new Date(started + 38_000 + i * 900).toISOString(),
      status: 'done' as const,
      filePath: `/backups/lct112-${date}.sql.gz`,
      sizeBytes: 48_234_496 - i * 612_352,
      sha256: `9f2c${date.replaceAll('-', '')}a47e1b0c55d8e3f6a2b19c4d7e0f13a5b8c2d4e6f7a9b1c3d5e7f9a0b2c4d6`.slice(0, 64),
    };
  });
}

function readBackups(): Backup[] {
  try {
    const raw = sessionStorage.getItem(BACKUPS_KEY);
    if (raw) return JSON.parse(raw) as Backup[];
  } catch {
    // повреждённая запись — начинаем с истории по расписанию
  }
  return seedBackups();
}

function writeBackups(list: Backup[]): void {
  try {
    sessionStorage.setItem(BACKUPS_KEY, JSON.stringify(list));
  } catch {
    // хранилище недоступно: журнал живёт до перезагрузки
  }
}

/** Копирование «идёт» BACKUP_MS после запуска, затем завершается. */
export function listBackups(): Backup[] {
  const list = readBackups();
  for (const b of list) {
    if (b.status === 'running' && Date.now() - Date.parse(b.startedAt) > BACKUP_MS) {
      const date = b.startedAt.slice(0, 10);
      Object.assign(b, {
        status: 'done',
        finishedAt: new Date(Date.parse(b.startedAt) + BACKUP_MS).toISOString(),
        filePath: `/backups/lct112-${date}-manual.sql.gz`,
        sizeBytes: 48_901_120,
        sha256: 'c4e1a9f07b2d3e58a61f9c0d2b7e4a31f58c6d90e2b1a7c3d4f5e6a7b8c9d0e1',
      });
    }
  }
  writeBackups(list);
  return list;
}

export function runBackup(actorId: string): Backup {
  const list = listBackups();
  if (list.some((b) => b.status === 'running')) {
    throw new ApiRequestError('conflict', 'Резервное копирование уже выполняется', { status: 409 });
  }
  const backup: Backup = { id: `bk-manual-${Date.now()}`, startedAt: new Date().toISOString(), status: 'running', triggeredBy: actorId };
  writeBackups([backup, ...list]);
  return backup;
}

export function lastBackupAt(): string | undefined {
  return listBackups().find((b) => b.status === 'done')?.finishedAt;
}

// ────────────────────────────────────────────────────────────── настройки

const SETTINGS_KEY = 'arm112.mock.settings';

const DEFAULT_SETTINGS: Setting[] = [
  { key: 'pass_threshold', value: 70, description: 'Порог зачёта по умолчанию, баллов' },
  { key: 'lesson_weights', value: { fields: 0.5, semantic: 0.25, grammar: 0.1, timing: 0.15, dialogue: 0 }, description: 'Веса слоёв оценки по умолчанию (сумма = 1)' },
  { key: 'dialogue_weight_default', value: 0.25, description: 'Доля разговора в голосовом занятии, если вес не задан' },
  { key: 'timing_tolerance', value: { softPct: 10, hardPct: 100 }, description: 'Допуск норматива: до soft — 100 баллов, к hard — 0' },
  { key: 'confidence_threshold', value: 0.7, description: 'Ниже этой уверенности модели — ревью преподавателя' },
  { key: 'xp_rules', value: { attempt_evaluated: 10, within_norm: 5, pass_bonus_per_10_points: 2, lesson_completed: 20 }, description: 'Начисление опыта' },
  { key: 'backup_schedule', value: { cron: '0 3 * * *', keepDays: 14 }, description: 'Расписание резервного копирования (ТЗ: не реже раза в сутки)' },
  { key: 'session_ttl_hours', value: 12, description: 'Срок жизни сессии, часов' },
];

export function listSettings(): Setting[] {
  try {
    const raw = sessionStorage.getItem(SETTINGS_KEY);
    if (raw) return JSON.parse(raw) as Setting[];
  } catch {
    // повреждённая запись — значения по умолчанию
  }
  return DEFAULT_SETTINGS.map((s) => ({ ...s, updatedAt: '2026-09-15T10:00:00Z' }));
}

export function updateSetting(key: string, value: unknown): Setting {
  const list = listSettings();
  const item = list.find((s) => s.key === key);
  if (!item) throw new ApiRequestError('not_found', `Настройка ${key} не найдена`, { status: 404 });
  item.value = value;
  item.updatedAt = new Date().toISOString();
  try {
    sessionStorage.setItem(SETTINGS_KEY, JSON.stringify(list));
  } catch {
    // хранилище недоступно: значение живёт до перезагрузки
  }
  return item;
}

// ───────────────────────────────────────────────────────── системный журнал

const LEVEL_RANK: Record<LogLevel, number> = { debug: 0, info: 1, warn: 2, error: 3 };

const LOG_TEMPLATES: Array<Omit<LogEntry, 'at'>> = [
  { level: 'info', message: 'http request', attrs: { method: 'POST', path: '/api/v1/attempts/…/events', status: 200, durationMs: 4 } },
  { level: 'info', message: 'http request', attrs: { method: 'PUT', path: '/api/v1/attempts/…/draft', status: 200, durationMs: 7 } },
  { level: 'info', message: 'ai job dispatched', attrs: { type: 'evaluate_semantic', queue: 'eval_fast' } },
  { level: 'info', message: 'ai job done', attrs: { type: 'evaluate_grammar', durationMs: 812 } },
  { level: 'warn', message: 'ai-service slow response', attrs: { type: 'evaluate_semantic', durationMs: 21450 } },
  { level: 'info', message: 'ws monitor connected', attrs: { lesson: 'ls-demo', clients: 1 } },
  { level: 'warn', message: 'ws client too slow, disconnected', attrs: { channel: 'monitor' } },
  { level: 'error', message: 'ai-service unavailable, breaker open', attrs: { failures: 5, retryInSec: 30 } },
  { level: 'info', message: 'breaker half-open, probing ai-service' },
  { level: 'info', message: 'backup finished', attrs: { sizeBytes: 48234496 } },
  { level: 'error', message: 'backup failed', attrs: { err: 'pg_dump: no space left on device' } },
  { level: 'info', message: 'user login', attrs: { login: 'teacher' } },
  { level: 'warn', message: 'login failed', attrs: { login: 'studnet', reason: 'invalid credentials' } },
];

const LOGS: LogEntry[] = Array.from({ length: 240 }, (_, i) => ({
  ...LOG_TEMPLATES[(i * 5) % LOG_TEMPLATES.length],
  at: new Date(Date.now() - i * 47_000).toISOString(),
}));

export function listLogs(f: { level?: LogLevel; q?: string; limit?: number }): LogEntry[] {
  const min = LEVEL_RANK[f.level ?? 'info'];
  const q = f.q?.trim().toLowerCase();
  return LOGS.filter((e) => LEVEL_RANK[e.level] >= min)
    .filter((e) => !q || e.message.toLowerCase().includes(q) || JSON.stringify(e.attrs ?? {}).toLowerCase().includes(q))
    .slice(0, Math.min(f.limit ?? 200, 1000));
}

// ────────────────────────────────────────────────────────── состояние контура

/**
 * Сервера в mock-режиме нет — так и сообщаем: ядро отвечает (это сам mock),
 * БД и ИИ-сервис не подключены. Правдоподобные «зелёные» цифры здесь были бы ложью.
 */
export function mockHealth(): SystemHealth {
  return {
    status: 'degraded',
    goCore: { version: 'демонстрационный режим (без сервера)', activeSessions: 1, wsConnections: 0 },
    postgres: { ok: undefined, lastBackupAt: lastBackupAt() },
    aiService: { ok: false, breakerState: 'open', ollama: false, languagetool: false, tts: false, stt: false, modelsAvailable: [] },
    jobs: { queued: 0, running: 0, failed: 0 },
  };
}
