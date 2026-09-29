/**
 * FIXTURE: витрины прогресса и аналитики группы для mock-режима.
 *
 * В проде их считает go-core по оценённым попыткам (student_progress,
 * student_field_errors, xp_ledger; /analytics/overview). Здесь — те же
 * агрегаты по «результатам»: детерминированная история прошлых занятий
 * плюс попытки, оценённые в этой вкладке. Одна выборка питает и прогресс,
 * и аналитику, поэтому цифры на экранах сходятся между собой.
 */

import type { AnalyticsFilter } from '../api/types';
import type { AnalyticsOverview, EvaluationLayer, Insight, StudentProgress } from '../types';
import { fieldLabel } from '../utils/card';
import { formatNumber, formatPct } from '../utils/format';
import { db, lessonById, scenarioById } from './db';

interface FieldMiss {
  field: string;
  label: string;
  kind: 'missing' | 'wrong' | 'extra';
}

interface ResultRecord {
  attemptId: string;
  userId: string;
  lessonId: string;
  categoryId: string;
  categoryName: string;
  at: string;
  score: number;
  pass: boolean;
  timeMs: number;
  reactionMs: number;
  withinNorm: boolean;
  needsReview: boolean;
  layers: Partial<Record<EvaluationLayer, number>>;
  fieldErrors: FieldMiss[];
  missingFacts: string[];
  grammar: Array<{ rule: string; message: string; example?: string }>;
}

/** Генератор с зерном: история одинакова при каждой загрузке. */
function seeded(seed: number): () => number {
  let s = seed;
  return () => {
    s = (s * 1664525 + 1013904223) % 4294967296;
    return s / 4294967296;
  };
}

const CATEGORIES = [
  { id: 'it-101', name: 'Происшествие 101' },
  { id: 'it-104', name: 'Происшествие 104' },
  { id: 'it-water', name: 'Повреждение водопровода' },
  { id: 'it-dtp', name: 'ДТП' },
];

/** Поля, в которых ошибаются чаще всего; вес — склонность к ошибке по категории. */
const FIELD_POOL: Array<{ field: string; kind: FieldMiss['kind']; weight: Record<string, number> }> = [
  { field: 'address.floor', kind: 'missing', weight: { 'it-101': 0.55, 'it-104': 0.35, 'it-water': 0.2, 'it-dtp': 0.05 } },
  { field: 'address.entrance', kind: 'missing', weight: { 'it-101': 0.4, 'it-104': 0.3, 'it-water': 0.25, 'it-dtp': 0.05 } },
  { field: 'phones.provided', kind: 'missing', weight: { 'it-101': 0.2, 'it-104': 0.25, 'it-water': 0.3, 'it-dtp': 0.35 } },
  { field: 'incidentTypeIds', kind: 'wrong', weight: { 'it-101': 0.08, 'it-104': 0.3, 'it-water': 0.15, 'it-dtp': 0.2 } },
  { field: 'flags.victimsPresent', kind: 'wrong', weight: { 'it-101': 0.25, 'it-104': 0.1, 'it-water': 0.05, 'it-dtp': 0.45 } },
  { field: 'applicant.status', kind: 'wrong', weight: { 'it-101': 0.12, 'it-104': 0.15, 'it-water': 0.2, 'it-dtp': 0.1 } },
  { field: 'services', kind: 'extra', weight: { 'it-101': 0.1, 'it-104': 0.2, 'it-water': 0.3, 'it-dtp': 0.15 } },
];

const FACT_POOL: Record<string, string[]> = {
  'it-101': ['в квартире остался пожилой человек', 'дым идёт из окна пятого этажа', 'дверь в квартиру заперта'],
  'it-104': ['запах газа во всём подъезде', 'жильцы не выходили из квартир', 'плита выключена'],
  'it-water': ['вода заливает подвал', 'прорыв у входа в подъезд'],
  'it-dtp': ['водитель зажат в машине', 'автомобиль перегородил полосу', 'один из водителей скрылся'],
};

const GRAMMAR_POOL = [
  { rule: 'COMMA_PARENTHESIS', message: 'Вводное слово не выделено запятыми', example: 'по словам заявителя дым идёт из окна' },
  { rule: 'MORFOLOGIK_RULE_RU_RU', message: 'Возможна орфографическая ошибка', example: 'зодымление в подъезде' },
  { rule: 'UPPERCASE_SENTENCE_START', message: 'Предложение начинается со строчной буквы', example: 'заявитель сообщает о запахе газа' },
  { rule: 'RU_COMPOUND', message: 'Слитное или раздельное написание', example: 'не известно, есть ли пострадавшие' },
];

const HISTORY_LESSONS = ['ls-archive-fire', 'ls-archive-city'];

/** История прошлых занятий: по ~14 карточек на обучающегося за последние три недели. */
function historyRecords(): ResultRecord[] {
  const out: ResultRecord[] = [];
  const students = db.users.filter((u) => u.role === 'student');
  const now = Date.now();
  students.forEach((student, si) => {
    const rnd = seeded(20260917 + si * 7919);
    // Второй обучающийся заметно слабее — аналитике есть о чём сказать.
    const skill = si === 0 ? 0.78 : 0.42;
    for (let i = 0; i < 14; i += 1) {
      const cat = CATEGORIES[Math.floor(rnd() * CATEGORIES.length)];
      // Растёт с опытом: поздние карточки лучше ранних.
      const progress = i / 14;
      const errors = FIELD_POOL.filter((f) => rnd() < (f.weight[cat.id] ?? 0.1) * (1.45 - skill) * (1.3 - progress * 0.6))
        .map((f) => ({ field: f.field, label: fieldLabel(f.field), kind: f.kind }));
      const facts = (FACT_POOL[cat.id] ?? []).filter(() => rnd() < 0.45 - skill * 0.3);
      const grammar = GRAMMAR_POOL.filter(() => rnd() < 0.3 - progress * 0.15);
      const fields = Math.max(20, 100 - errors.length * 14 - rnd() * 8);
      const semantic = Math.max(25, 95 - facts.length * 18 - rnd() * 10);
      const grammarScore = Math.max(40, 100 - grammar.length * 12 - rnd() * 6);
      const limitMs = 45_000;
      const timeMs = Math.round(limitMs * (0.6 + rnd() * (1.2 - skill * 0.5)));
      const withinNorm = timeMs <= limitMs * 1.1;
      const timing = withinNorm ? 100 : Math.max(0, 100 - ((timeMs - limitMs * 1.1) / limitMs) * 200);
      const score = Math.round(fields * 0.5 + semantic * 0.25 + grammarScore * 0.1 + timing * 0.15);
      const day = 21 - Math.floor((i / 14) * 20) - Math.floor(rnd() * 2);
      out.push({
        attemptId: `hist-${student.id}-${i}`,
        userId: student.id,
        lessonId: HISTORY_LESSONS[i % 2],
        categoryId: cat.id,
        categoryName: cat.name,
        at: new Date(now - day * 86_400_000 - Math.floor(rnd() * 5) * 3_600_000).toISOString(),
        score,
        pass: score >= 70,
        timeMs,
        reactionMs: Math.round(3000 + rnd() * 6000),
        withinNorm,
        needsReview: rnd() < 0.08,
        layers: { fields: Math.round(fields), semantic: Math.round(semantic), grammar: Math.round(grammarScore), timing: Math.round(timing) },
        fieldErrors: errors,
        missingFacts: facts,
        grammar,
      });
    }
  });
  return out;
}

/** Попытки, оценённые в этой вкладке: те же агрегаты, что и у истории. */
function liveRecords(): ResultRecord[] {
  const out: ResultRecord[] = [];
  for (const attempt of db.attempts) {
    const ev = db.evaluations[attempt.id];
    if (!ev || ev.status !== 'done' || attempt.status !== 'evaluated') continue;
    const scenario = scenarioById(attempt.scenarioId);
    const layers: ResultRecord['layers'] = {};
    for (const [layer, value] of [
      ['fields', ev.fieldsScore], ['semantic', ev.semanticScore], ['grammar', ev.grammarScore],
      ['timing', ev.timingScore], ['dialogue', ev.dialogueScore],
    ] as const) {
      if (value != null) layers[layer] = value;
    }
    out.push({
      attemptId: attempt.id,
      userId: attempt.userId,
      lessonId: attempt.lessonId,
      categoryId: scenario?.categoryId ?? 'unknown',
      categoryName: scenario?.categoryName ?? 'Без категории',
      at: attempt.submittedAt ?? new Date().toISOString(),
      score: ev.finalScore,
      pass: ev.verdict === 'pass',
      timeMs: ev.timing.spentMs,
      reactionMs: ev.timing.reactionMs,
      withinNorm: ev.timing.withinNorm,
      needsReview: ev.needsReview,
      layers,
      fieldErrors: ev.fieldErrors.map((e) => ({ field: e.field, label: e.label, kind: e.kind })),
      missingFacts: ev.semantic?.missingFacts ?? [],
      grammar: (ev.grammar?.remarks ?? []).map((r) => ({ rule: r.rule ?? 'OTHER', message: r.message })),
    });
  }
  return out;
}

function allRecords(): ResultRecord[] {
  return [...historyRecords(), ...liveRecords()].sort((a, b) => b.at.localeCompare(a.at));
}

const avg = (xs: number[]): number => (xs.length ? xs.reduce((s, x) => s + x, 0) / xs.length : 0);
const round1 = (x: number): number => Math.round(x * 10) / 10;
const pct = (part: number, total: number): number => (total ? round1((part / total) * 100) : 0);

function userName(userId: string): string {
  const u = db.users.find((x) => x.id === userId);
  return u ? `${u.lastName} ${u.firstName[0]}. ${u.middleName ? `${u.middleName[0]}.` : ''}`.trim() : 'Обучающийся';
}

// ───────────────────────────────────────────────────── прогресс обучающегося

const LEVELS = [
  { no: 1, title: 'Стажёр', xp: 0 },
  { no: 2, title: 'Оператор-кандидат', xp: 150 },
  { no: 3, title: 'Оператор', xp: 350 },
  { no: 4, title: 'Старший оператор', xp: 700 },
  { no: 5, title: 'Наставник', xp: 1200 },
];

type XpEntry = StudentProgress['xpLog'][number];

/** Начисления XP по правилам go-core (settings.xp_rules). */
function xpFor(r: ResultRecord): XpEntry[] {
  const entries: XpEntry[] = [{ delta: 10, reason: 'attempt_evaluated', at: r.at, attemptId: r.attemptId }];
  if (r.withinNorm) entries.push({ delta: 5, reason: 'within_norm', at: r.at, attemptId: r.attemptId });
  if (r.pass) entries.push({ delta: 2 * Math.floor(r.score / 10), reason: 'pass_bonus', at: r.at, attemptId: r.attemptId });
  return entries;
}

export function buildProgress(userId: string): StudentProgress {
  const records = allRecords().filter((r) => r.userId === userId);
  const xpLog = records.flatMap(xpFor);
  const xp = xpLog.reduce((s, e) => s + e.delta, 0);
  const level = [...LEVELS].reverse().find((l) => xp >= l.xp) ?? LEVELS[0];
  const next = LEVELS.find((l) => l.no === level.no + 1);
  const monthAgo = Date.now() - 30 * 86_400_000;

  const byCategory = new Map<string, ResultRecord[]>();
  for (const r of records) byCategory.set(r.categoryId, [...(byCategory.get(r.categoryId) ?? []), r]);
  const categories = [...byCategory.values()].map((rs) => ({
    categoryId: rs[0].categoryId,
    categoryName: rs[0].categoryName,
    attemptsDone: rs.length,
    avgScore: round1(avg(rs.map((r) => r.score))),
    passRatePct: pct(rs.filter((r) => r.pass).length, rs.length),
    avgTimeMs: Math.round(avg(rs.map((r) => r.timeMs))),
    lastAttemptAt: rs[0].at,
  }));

  const errorCount = new Map<string, StudentProgress['fieldErrors'][number]>();
  for (const e of records.flatMap((r) => r.fieldErrors)) {
    const key = `${e.field}|${e.kind}`;
    const prev = errorCount.get(key);
    errorCount.set(key, { ...e, count: (prev?.count ?? 0) + 1 });
  }
  const fieldErrors = [...errorCount.values()].sort((a, b) => b.count - a.count);

  const recommendations: StudentProgress['recommendations'] = [];
  const weakest = [...categories].sort((a, b) => (a.avgScore ?? 0) - (b.avgScore ?? 0))[0];
  if (weakest && (weakest.avgScore ?? 100) < 75) {
    recommendations.push({
      id: 'rec-category',
      kind: 'weak_category',
      body: `Больше всего потерь в категории «${weakest.categoryName}»: средний балл ${formatNumber(weakest.avgScore ?? 0)}. Повторите памятку по этой категории и пройдите ещё 2–3 карточки.`,
    });
  }
  if (fieldErrors.length > 0) {
    recommendations.push({
      id: 'rec-fields',
      kind: 'weak_field',
      body: 'Чаще всего не заполняются или заполняются неверно поля:',
      items: fieldErrors.slice(0, 3).map((e) => e.label),
    });
  }
  const within = records.filter((r) => r.withinNorm).length;
  if (records.length > 0 && within / records.length < 0.75) {
    recommendations.push({
      id: 'rec-timing',
      kind: 'slow_timing',
      body: 'Норматив времени выдержан меньше чем в трёх карточках из четырёх. Сначала фиксируйте адрес и тип происшествия, детали — после оповещения служб.',
    });
  }

  return {
    userId,
    name: userName(userId),
    xp,
    level: {
      no: level.no,
      title: level.title,
      xpRequired: level.xp,
      nextTitle: next?.title,
      nextXpRequired: next?.xp,
    },
    attemptsDone: records.length,
    avgScore: records.length ? round1(avg(records.map((r) => r.score))) : undefined,
    avgScore30d: round1(avg(records.filter((r) => Date.parse(r.at) >= monthAgo).map((r) => r.score))),
    passRatePct: pct(records.filter((r) => r.pass).length, records.length),
    avgTimeMs: Math.round(avg(records.map((r) => r.timeMs))),
    withinNormCount: within,
    lastActivityAt: records[0]?.at,
    categories,
    fieldErrors,
    recommendations,
    xpLog: xpLog.slice(0, 50),
  };
}

/** Оценённые попытки обучающегося: без них сертификат не выдаётся (409). */
export function evaluatedCount(userId: string): number {
  return allRecords().filter((r) => r.userId === userId).length;
}

// ─────────────────────────────────────────────────────────── аналитика группы

const LAYER_ORDER: EvaluationLayer[] = ['fields', 'semantic', 'grammar', 'timing', 'dialogue'];

export function buildOverview(filter: AnalyticsFilter, teacherId?: string): AnalyticsOverview {
  const days = filter.days ?? 30;
  const since = Date.now() - days * 86_400_000;
  const records = allRecords().filter((r) => {
    if (Date.parse(r.at) < since) return false;
    if (filter.lessonId && r.lessonId !== filter.lessonId) return false;
    if (filter.studentId && r.userId !== filter.studentId) return false;
    if (filter.categoryId && r.categoryId !== filter.categoryId) return false;
    // Преподавателю — только его занятия; архив истории принадлежит демо-преподавателю.
    if (teacherId) {
      const lesson = lessonById(r.lessonId);
      if (lesson && lesson.teacherId !== teacherId) return false;
    }
    return true;
  });
  const n = records.length;
  const studentIds = [...new Set(records.map((r) => r.userId))];

  const layers = LAYER_ORDER.map((layer) => {
    const values = records.map((r) => r.layers[layer]).filter((v): v is number => v != null);
    return { layer, avgScore: round1(avg(values)), attempts: values.length };
  }).filter((l) => l.attempts > 0);

  // Тепловая карта: строки — категории, столбцы — поля с ошибками (самые частые слева).
  const fieldTotals = new Map<string, { label: string; count: number }>();
  for (const e of records.flatMap((r) => r.fieldErrors)) {
    fieldTotals.set(e.field, { label: e.label, count: (fieldTotals.get(e.field)?.count ?? 0) + 1 });
  }
  const cols = [...fieldTotals.entries()]
    .sort((a, b) => b[1].count - a[1].count)
    .slice(0, 8)
    .map(([id, v]) => ({ id, label: v.label }));
  const catMap = new Map<string, ResultRecord[]>();
  for (const r of records) catMap.set(r.categoryId, [...(catMap.get(r.categoryId) ?? []), r]);
  const rows = [...catMap.values()].map((rs) => ({ id: rs[0].categoryId, label: rs[0].categoryName, attempts: rs.length }));
  const cells = rows.map((row) =>
    cols.map((col) => (catMap.get(row.id) ?? []).filter((r) => r.fieldErrors.some((e) => e.field === col.id)).length),
  );

  const byDay = new Map<string, ResultRecord[]>();
  for (const r of records) {
    const date = r.at.slice(0, 10);
    byDay.set(date, [...(byDay.get(date) ?? []), r]);
  }
  const trend = [...byDay.entries()]
    .sort((a, b) => a[0].localeCompare(b[0]))
    .map(([date, rs]) => ({
      date,
      attempts: rs.length,
      avgScore: round1(avg(rs.map((r) => r.score))),
      passRatePct: pct(rs.filter((r) => r.pass).length, rs.length),
      avgTimeMs: Math.round(avg(rs.map((r) => r.timeMs))),
    }));

  const errorKinds = new Map<string, { field: string; label: string; kind: FieldMiss['kind']; attempts: Set<string>; users: Set<string> }>();
  for (const r of records) {
    for (const e of r.fieldErrors) {
      const key = `${e.field}|${e.kind}`;
      const item = errorKinds.get(key) ?? { ...e, attempts: new Set<string>(), users: new Set<string>() };
      item.attempts.add(r.attemptId);
      item.users.add(r.userId);
      errorKinds.set(key, item);
    }
  }
  const topFieldErrorsFull = [...errorKinds.values()]
    .sort((a, b) => b.attempts.size - a.attempts.size)
    .slice(0, 8);
  const topFieldErrors = topFieldErrorsFull.map((e) => ({
    field: e.field, label: e.label, kind: e.kind, count: e.attempts.size, sharePct: pct(e.attempts.size, n),
  }));

  const facts = new Map<string, number>();
  for (const f of records.flatMap((r) => r.missingFacts)) facts.set(f, (facts.get(f) ?? 0) + 1);
  const topMissingFacts = [...facts.entries()]
    .sort((a, b) => b[1] - a[1])
    .slice(0, 6)
    .map(([fact, count]) => ({ fact, count, sharePct: pct(count, n) }));

  const rules = new Map<string, { rule: string; message: string; count: number; example?: string }>();
  for (const g of records.flatMap((r) => r.grammar)) {
    const prev = rules.get(g.rule);
    rules.set(g.rule, { ...g, count: (prev?.count ?? 0) + 1 });
  }
  const topGrammarRules = [...rules.values()].sort((a, b) => b.count - a.count).slice(0, 6);

  const students = studentIds.map((userId) => {
    const rs = records.filter((r) => r.userId === userId);
    const layerAvg = LAYER_ORDER.map((layer) => ({
      layer,
      value: avg(rs.map((r) => r.layers[layer]).filter((v): v is number => v != null)),
      has: rs.some((r) => r.layers[layer] != null),
    })).filter((l) => l.has);
    return {
      userId,
      name: userName(userId),
      attempts: rs.length,
      avgScore: round1(avg(rs.map((r) => r.score))),
      passRatePct: pct(rs.filter((r) => r.pass).length, rs.length),
      avgTimeMs: Math.round(avg(rs.map((r) => r.timeMs))),
      weakestLayer: [...layerAvg].sort((a, b) => a.value - b.value)[0]?.layer,
    };
  }).sort((a, b) => a.avgScore - b.avgScore);

  const categories = [...catMap.values()].map((rs) => ({
    categoryId: rs[0].categoryId,
    categoryName: rs[0].categoryName,
    attempts: rs.length,
    avgScore: round1(avg(rs.map((r) => r.score))),
    passRatePct: pct(rs.filter((r) => r.pass).length, rs.length),
    avgTimeMs: Math.round(avg(rs.map((r) => r.timeMs))),
  }));

  const summary = {
    attempts: n,
    students: studentIds.length,
    avgScore: round1(avg(records.map((r) => r.score))),
    passRatePct: pct(records.filter((r) => r.pass).length, n),
    avgTimeMs: Math.round(avg(records.map((r) => r.timeMs))),
    avgReactionMs: Math.round(avg(records.map((r) => r.reactionMs))),
    withinNormPct: pct(records.filter((r) => r.withinNorm).length, n),
    needsReview: records.filter((r) => r.needsReview).length,
  };

  return {
    generatedAt: new Date().toISOString(),
    scope: { days, lessonId: filter.lessonId, studentId: filter.studentId, categoryId: filter.categoryId },
    summary,
    layers,
    heatmap: { rows, cols, cells },
    trend,
    topFieldErrors,
    topMissingFacts,
    topGrammarRules,
    students,
    categories,
    insights: buildInsights(records, topFieldErrorsFull, topMissingFacts, topGrammarRules, students, categories, summary),
  };
}

/** Детерминированный разбор типичных ошибок — те же правила, что у go-core. */
function buildInsights(
  records: ResultRecord[],
  fieldErrors: Array<{ label: string; attempts: Set<string>; users: Set<string> }>,
  facts: AnalyticsOverview['topMissingFacts'],
  rules: AnalyticsOverview['topGrammarRules'],
  students: AnalyticsOverview['students'],
  categories: AnalyticsOverview['categories'],
  summary: AnalyticsOverview['summary'],
): Insight[] {
  const n = records.length;
  if (n === 0) return [];
  const out: Insight[] = [];

  for (const e of fieldErrors.slice(0, 2)) {
    const share = pct(e.attempts.size, n);
    if (share < 25) continue;
    out.push({
      id: `weak_field-${e.label}`,
      severity: share >= 45 ? 'critical' : 'warning',
      kind: 'weak_field',
      title: `Поле «${e.label}» заполняют с ошибкой в ${formatPct(share)} карточек`,
      body: 'Ошибка системная, а не случайная: она повторяется у разных обучающихся. Разберите на занятии, из какой реплики заявителя берётся это значение, и дайте 2–3 карточки, где поле обязательно.',
      metric: share,
      affectedStudents: [...e.users].map(userName),
    });
  }

  const fact = facts[0];
  if (fact && fact.sharePct >= 20) {
    out.push({
      id: 'missing_fact',
      severity: 'warning',
      kind: 'missing_fact',
      title: `Факт «${fact.fact}» теряется в описании`,
      body: 'Обучающиеся слышат факт, но не переносят его в описание со слов заявителя. Потренируйте пересказ: «кто — что — где — есть ли угроза людям».',
      metric: fact.sharePct,
      evidence: facts.slice(1, 3).map((f) => f.fact),
    });
  }

  const rule = rules[0];
  if (rule && rule.count >= 3) {
    out.push({
      id: 'grammar_pattern',
      severity: 'info',
      kind: 'grammar_pattern',
      title: `Повторяющаяся ошибка: ${rule.message.toLowerCase()}`,
      body: 'Ошибка встречается в разных карточках — это навык, а не опечатка. Короткий разбор правила на примере из карточек группы.',
      metric: rule.count,
      evidence: rule.example ? [rule.example] : undefined,
    });
  }

  if (summary.withinNormPct < 75) {
    out.push({
      id: 'slow_timing',
      severity: summary.withinNormPct < 50 ? 'critical' : 'warning',
      kind: 'slow_timing',
      title: `Норматив времени выдержан лишь в ${formatPct(summary.withinNormPct)} карточек`,
      body: 'Время уходит на второстепенные поля до оповещения служб. Напомните порядок: адрес → тип происшествия → службы → детали.',
      metric: summary.withinNormPct,
    });
  }

  const weakCategory = [...categories].sort((a, b) => a.avgScore - b.avgScore)[0];
  if (weakCategory && weakCategory.avgScore < 70 && categories.length > 1) {
    out.push({
      id: 'weak_category',
      severity: 'warning',
      kind: 'weak_category',
      title: `Слабая категория: «${weakCategory.categoryName}»`,
      body: `Средний балл ${formatNumber(weakCategory.avgScore)} при зачёте в ${formatPct(weakCategory.passRatePct)} карточек. Включите категорию в следующее занятие и повторите справку по ней.`,
      metric: weakCategory.avgScore,
    });
  }

  const atRisk = students.filter((s) => s.avgScore < 65 || s.passRatePct < 50);
  if (atRisk.length > 0) {
    out.push({
      id: 'student_at_risk',
      severity: 'critical',
      kind: 'student_at_risk',
      title: atRisk.length === 1 ? 'Обучающийся в зоне риска' : `Обучающихся в зоне риска: ${atRisk.length}`,
      body: 'Средний балл ниже 65 или зачёт меньше чем в половине карточек. Нужна индивидуальная работа: разбор последних попыток и повтор слабой категории.',
      metric: atRisk[0].avgScore,
      affectedStudents: atRisk.map((s) => s.name),
    });
  }

  if (out.length === 0) {
    out.push({
      id: 'general',
      severity: 'info',
      kind: 'general',
      title: 'Системных ошибок не выявлено',
      body: 'Группа стабильно выполняет норматив и заполняет карточку. Можно повышать сложность сценариев.',
    });
  }
  return out;
}
