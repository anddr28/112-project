/**
 * FIXTURE: справочная база в mock-режиме.
 *
 * Системные материалы — выдержки из памяток АРМ-112 в Markdown (в проде их
 * поставляет go-core миграцией). Загруженные файлы живут в памяти вкладки и,
 * если помещаются, в sessionStorage как data-URL: без сервера хранить их негде.
 */

import type { CreateMaterialInput } from '../api/types';
import { ApiRequestError } from '../api/error';
import type { Material, User } from '../types';
import { uid } from '../utils/id';

const KEY = 'arm112.mock.materials';
const MAX_BYTES = 20 * 1024 * 1024;

interface Stored extends Material {
  /** data-URL файла; нет — файл не поместился в хранилище и живёт в памяти */
  dataUrl?: string;
}

const SYSTEM: Stored[] = [
  {
    id: '11111111-0000-4000-8000-000000000001',
    title: 'Памятка оператора АРМ-112: приём вызова',
    description: 'Порядок работы с входящим вызовом и заполнения карточки происшествия.',
    category: 'Памятка АРМ-112',
    hasFile: false,
    system: true,
    createdAt: '2026-09-01T09:00:00Z',
    content: `# Приём вызова

При открытии новой карточки начинается **отсчёт таймера**. Норматив — 30 секунд
до оповещения служб, если занятие не задаёт иное.

## Порядок опроса

1. Адрес происшествия: улица, дом, подъезд, этаж, ориентир.
2. Что случилось — тип происшествия по классификатору.
3. Есть ли угроза жизни, пострадавшие, заблокированные.
4. ФИО и телефон заявителя.

> Сначала адрес и тип происшествия — только потом детали. Службы оповещаются
> до того, как заявитель закончит рассказ.

## Типичные ошибки

- не уточнён **этаж** и **подъезд** — бригада теряет время на месте;
- описание не содержит фактов со слов заявителя;
- не отмечены пострадавшие при их наличии.
`,
  },
  {
    id: '11111111-0000-4000-8000-000000000002',
    title: 'Памятка диспетчера ДДС: статусы реагирования',
    description: 'Что означает каждый статус и когда его ставить.',
    category: 'Памятка АРМ-112',
    hasFile: false,
    system: true,
    createdAt: '2026-09-01T09:05:00Z',
    content: `# Статусы реагирования ДДС

| Статус | Кто ставит | Когда |
|---|---|---|
| Добавлена | система | карточка направлена службе |
| Получена службой | система | карточка поступила на сервер службы |
| Принята | диспетчер | служба берёт происшествие в работу |
| Не принята | диспетчер | не зона ответственности — **с комментарием** |
| Работы завершены | диспетчер | с комментарием о принятых мерах |

Решение «Принята» / «Не принята» — не позднее **30 секунд** после поступления.
`,
  },
  {
    id: '11111111-0000-4000-8000-000000000003',
    title: 'Регламент: вызов на иностранном языке',
    description: 'Действия оператора, если заявитель не говорит по-русски.',
    category: 'Регламенты',
    hasFile: false,
    system: true,
    createdAt: '2026-09-02T10:00:00Z',
    content: `# Вызов на иностранном языке

1. Отметьте признак *«Вызов на иностранном языке»*.
2. Подключите переводчика через кнопку конференц-связи.
3. Адрес уточняйте по ориентирам: \`метро\`, \`торговый центр\`, \`остановка\`.
`,
  },
];

const memoryFiles = new Map<string, Blob>();
const blobUrls = new Map<string, string>();

function readUploaded(): Stored[] {
  try {
    const raw = sessionStorage.getItem(KEY);
    return raw ? (JSON.parse(raw) as Stored[]) : [];
  } catch {
    return [];
  }
}

function writeUploaded(list: Stored[]): void {
  try {
    sessionStorage.setItem(KEY, JSON.stringify(list));
  } catch {
    // Не поместилось (крупный файл) — сохраняем без data-URL: файл остаётся в памяти вкладки.
    try {
      sessionStorage.setItem(KEY, JSON.stringify(list.map(({ dataUrl: _d, ...m }) => m)));
    } catch {
      // хранилище недоступно совсем — материалы живут до перезагрузки
    }
  }
}

function all(): Stored[] {
  return [...readUploaded(), ...SYSTEM];
}

/** Метаданные без текста и служебного data-URL — как в списке контракта. */
function meta({ dataUrl: _d, content: _c, ...m }: Stored): Material {
  return m;
}

export function listMaterials(f: { category?: string; q?: string }): Material[] {
  const q = f.q?.trim().toLowerCase();
  return all()
    .filter((m) => !f.category || m.category === f.category)
    .filter((m) => !q || m.title.toLowerCase().includes(q) || (m.description ?? '').toLowerCase().includes(q))
    .map(meta);
}

export function getMaterial(id: string): Material {
  const m = all().find((x) => x.id === id);
  if (!m) throw new ApiRequestError('not_found', 'Материал не найден', { status: 404 });
  const { dataUrl: _d, ...rest } = m;
  return rest;
}

function readAsDataUrl(file: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result));
    reader.onerror = () => reject(reader.error ?? new Error('Не удалось прочитать файл'));
    reader.readAsDataURL(file);
  });
}

export async function createMaterial(input: CreateMaterialInput, author: User): Promise<Material> {
  if (input.title.trim().length < 3) {
    throw new ApiRequestError('validation', 'Название — не короче 3 символов', { status: 400, details: { fields: ['title'] } });
  }
  if (!input.content?.trim() && !input.file) {
    throw new ApiRequestError('validation', 'Нужен текст материала или файл', { status: 400 });
  }
  if (input.file && input.file.size > MAX_BYTES) {
    throw new ApiRequestError('validation', 'Файл больше 20 МБ', { status: 413 });
  }
  const id = uid('mat');
  const item: Stored = {
    id,
    title: input.title.trim(),
    description: input.description?.trim() || undefined,
    category: input.category?.trim() || 'Прочее',
    content: input.content?.trim() || undefined,
    hasFile: Boolean(input.file),
    fileName: input.file?.name,
    mimeType: input.file?.type || undefined,
    sizeBytes: input.file?.size,
    system: false,
    authorId: author.id,
    authorName: `${author.lastName} ${author.firstName[0]}.`,
    createdAt: new Date().toISOString(),
  };
  if (input.file) {
    memoryFiles.set(id, input.file);
    // Небольшие файлы переживают перезагрузку вкладки.
    if (input.file.size <= 2 * 1024 * 1024) item.dataUrl = await readAsDataUrl(input.file);
  }
  writeUploaded([item, ...readUploaded()]);
  return getMaterial(id);
}

export function removeMaterial(id: string, actor: User): void {
  const m = all().find((x) => x.id === id);
  if (!m) throw new ApiRequestError('not_found', 'Материал не найден', { status: 404 });
  if (m.system) throw new ApiRequestError('conflict', 'Системный материал из поставки не удаляется', { status: 409 });
  if (actor.role !== 'admin' && m.authorId !== actor.id) {
    throw new ApiRequestError('forbidden', 'Удалить материал может его автор или администратор', { status: 403 });
  }
  writeUploaded(readUploaded().filter((x) => x.id !== id));
  memoryFiles.delete(id);
}

/** Адрес файла для <iframe>/<audio>: blob-URL из памяти или из сохранённого data-URL. */
export function materialFileUrl(id: string): string {
  const cached = blobUrls.get(id);
  if (cached) return cached;
  let blob = memoryFiles.get(id);
  const stored = readUploaded().find((x) => x.id === id);
  if (!blob && stored?.dataUrl) {
    const [head, body] = stored.dataUrl.split(',', 2);
    const bytes = Uint8Array.from(atob(body), (c) => c.charCodeAt(0));
    blob = new Blob([bytes], { type: /data:([^;]+)/.exec(head)?.[1] ?? 'application/octet-stream' });
  }
  if (!blob) return '';
  const url = URL.createObjectURL(blob);
  blobUrls.set(id, url);
  return url;
}
