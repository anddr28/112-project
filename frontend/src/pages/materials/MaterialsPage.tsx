import { useEffect, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { api } from '../../shared/api';
import { useAsync } from '../../shared/api/useAsync';
import { useAuth } from '../../app/auth';
import { Badge, Card, EmptyState, ErrorState, Field, Loading, Modal } from '../../components/ui';
import { Markdown } from '../../components/Markdown';
import { formatDate } from '../../shared/utils/time';
import type { Material } from '../../shared/types';

/** Контракт: файл до 20 МБ. Проверяем заранее, чтобы не гнать 20 МБ по сети ради отказа 413. */
const MAX_FILE_BYTES = 20 * 1024 * 1024;
const ACCEPT = '.pdf,.docx,.xlsx,.mp3,.wav,.png,.jpg,.jpeg,.txt,.xml,.json';
const CATEGORIES = ['Памятка АРМ-112', 'Регламенты', 'Аудио вызовов', 'Методические материалы', 'Прочее'];

function formatSize(bytes?: number): string {
  if (bytes == null) return '';
  if (bytes < 1024) return `${bytes} Б`;
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} КБ`;
  return `${(bytes / 1024 / 1024).toFixed(1).replace('.', ',')} МБ`;
}

/**
 * Справочная база (v1.4): памятки, регламенты, методматериалы, записи вызовов.
 * Читают все роли; добавляют и удаляют преподаватель и администратор.
 */
export function MaterialsPage() {
  const role = useAuth((s) => s.user?.role);
  const [q, setQ] = useState('');
  const [query, setQuery] = useState('');
  const [category, setCategory] = useState('');
  const [creating, setCreating] = useState(false);
  const navigate = useNavigate();
  const materials = useAsync(() => api.materials.list({ category: category || undefined, q: query || undefined }), [category, query]);
  // Список разделов — из всех материалов, а не из отфильтрованных: иначе фильтр прятал бы сам себя.
  const all = useAsync(() => api.materials.list({}), [materials.data?.length]);

  // Поиск — после паузы ввода: не запрос на каждую букву.
  useEffect(() => {
    const id = setTimeout(() => setQuery(q.trim()), 300);
    return () => clearTimeout(id);
  }, [q]);

  const canEdit = role === 'teacher' || role === 'admin';
  const categories = [...new Set((all.data ?? []).map((m) => m.category))].sort((a, b) => a.localeCompare(b, 'ru'));
  const list = materials.data ?? [];

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <h1>Справочная база</h1>
          <div className="page-head__sub">Памятки АРМ-112, регламенты и методические материалы.</div>
        </div>
        {canEdit && (
          <div className="page-head__actions">
            <button type="button" className="btn btn--primary" onClick={() => setCreating(true)}>Добавить материал</button>
          </div>
        )}
      </div>

      <div className="filters" role="search">
        <label className="filters__item filters__item--wide">
          <span className="field__label">Поиск по названию и описанию</span>
          <input className="input" type="search" value={q} maxLength={100} onChange={(e) => setQ(e.target.value)} placeholder="Например: статусы реагирования" />
        </label>
        <label className="filters__item">
          <span className="field__label">Раздел</span>
          <select className="select" value={category} onChange={(e) => setCategory(e.target.value)}>
            <option value="">Все разделы</option>
            {categories.map((c) => <option key={c} value={c}>{c}</option>)}
          </select>
        </label>
      </div>

      {materials.loading && !materials.data ? (
        <Loading />
      ) : materials.error ? (
        <ErrorState text={materials.error} onRetry={materials.reload} />
      ) : list.length === 0 ? (
        <Card><EmptyState title="Ничего не найдено" text="Измените запрос или раздел." /></Card>
      ) : (
        <div className={`materials${materials.refreshing ? ' is-refreshing' : ''}`}>
          {list.map((m) => (
            <Link key={m.id} to={`/materials/${m.id}`} className="material">
              <span className="row row--tight">
                <Badge tone="neutral">{m.category}</Badge>
                {m.hasFile && <Badge tone="accent">{fileKind(m)}</Badge>}
                {m.system && <Badge tone="neutral">из поставки</Badge>}
              </span>
              <span className="material__title">{m.title}</span>
              {m.description && <span className="material__desc">{m.description}</span>}
              <span className="dim small">
                {m.authorName ? `${m.authorName} · ` : ''}{formatDate(m.createdAt)}{m.sizeBytes ? ` · ${formatSize(m.sizeBytes)}` : ''}
              </span>
            </Link>
          ))}
        </div>
      )}

      {creating && (
        <CreateMaterialModal
          categories={[...new Set([...CATEGORIES, ...categories])]}
          onClose={() => setCreating(false)}
          onCreated={(m) => navigate(`/materials/${m.id}`)}
        />
      )}
    </>
  );
}

function fileKind(m: Material): string {
  const mime = m.mimeType ?? '';
  if (mime === 'application/pdf') return 'PDF';
  if (mime.startsWith('audio/')) return 'Аудио';
  if (mime.startsWith('image/')) return 'Изображение';
  const ext = m.fileName?.split('.').pop();
  return ext ? ext.toUpperCase() : 'Файл';
}

export function MaterialPage() {
  const { materialId = '' } = useParams();
  const role = useAuth((s) => s.user?.role);
  const userId = useAuth((s) => s.user?.id);
  const material = useAsync(() => api.materials.get(materialId), [materialId]);
  const navigate = useNavigate();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  if (material.loading) return <Loading />;
  if (material.error) return <ErrorState text={material.error} onRetry={material.reload} />;
  if (!material.data) return <ErrorState text="Материал не найден" />;
  const m = material.data;
  // Удалять может автор или администратор; сервер проверяет то же (системные — 409).
  const canDelete = !m.system && (role === 'admin' || (role === 'teacher' && m.authorId === userId));
  const url = m.hasFile ? api.materials.fileUrl(m) : '';

  async function remove() {
    if (!window.confirm(`Удалить материал «${m.title}»? Действие попадёт в журнал аудита.`)) return;
    setBusy(true);
    setError(null);
    try {
      await api.materials.remove(m.id);
      navigate('/materials', { replace: true });
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Не удалось удалить материал');
      setBusy(false);
    }
  }

  return (
    <>
      <div className="page-head">
        <div className="page-head__text">
          <div className="row row--tight" style={{ marginBottom: 6 }}>
            <Link to="/materials" className="small">← Справочная база</Link>
          </div>
          <h1>{m.title}</h1>
          <div className="row row--tight" style={{ marginTop: 8 }}>
            <Badge tone="neutral">{m.category}</Badge>
            {m.system && <Badge tone="neutral">из поставки</Badge>}
            <span className="dim small">{m.authorName ? `${m.authorName} · ` : ''}{formatDate(m.createdAt)}</span>
          </div>
        </div>
        <div className="page-head__actions">
          {url && <a className="btn" href={url} download={m.fileName ?? true}>Скачать файл</a>}
          {canDelete && <button type="button" className="btn btn--danger" disabled={busy} onClick={() => void remove()}>Удалить</button>}
        </div>
      </div>

      {error && <p className="field__error" role="alert" style={{ marginBottom: 12 }}>{error}</p>}
      {m.description && <p className="muted" style={{ marginBottom: 12 }}>{m.description}</p>}

      <div className="stack">
        {m.hasFile && (
          <Card title={m.fileName ?? 'Файл'} actions={<span className="dim small">{formatSize(m.sizeBytes)}</span>}>
            <FilePreview material={m} url={url} />
          </Card>
        )}
        {m.content && (
          <Card>
            <Markdown text={m.content} />
          </Card>
        )}
      </div>
    </>
  );
}

/** PDF — встроенным просмотрщиком, аудио — плеером, изображение — картинкой; прочее — только скачать. */
function FilePreview({ material: m, url }: { material: Material; url: string }) {
  if (!url) return <p className="muted small">Файл недоступен в этом сеансе демонстрационного режима.</p>;
  const mime = m.mimeType ?? '';
  if (mime === 'application/pdf') {
    return (
      <>
        <iframe className="material__pdf" src={url} title={m.fileName ?? m.title} />
        <p className="dim small">Если документ не отображается, <a href={url} target="_blank" rel="noreferrer">откройте его в новой вкладке</a>.</p>
      </>
    );
  }
  if (mime.startsWith('audio/')) {
    return <audio className="material__audio" controls preload="metadata" src={url}>Браузер не воспроизводит этот формат — скачайте файл.</audio>;
  }
  if (mime.startsWith('image/')) return <img className="material__img" src={url} alt={m.title} />;
  return <p className="muted small">Просмотр этого формата в браузере не предусмотрен — скачайте файл.</p>;
}

function CreateMaterialModal({
  categories,
  onClose,
  onCreated,
}: {
  categories: string[];
  onClose: () => void;
  onCreated: (m: Material) => void;
}) {
  const [title, setTitle] = useState('');
  const [description, setDescription] = useState('');
  const [category, setCategory] = useState(categories[0] ?? '');
  const [content, setContent] = useState('');
  const [file, setFile] = useState<File | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const fileError = file && file.size > MAX_FILE_BYTES ? `Файл ${formatSize(file.size)} — больше 20 МБ` : undefined;
  const titleError = title.trim().length > 0 && title.trim().length < 3 ? 'Не короче 3 символов' : undefined;
  const ready = title.trim().length >= 3 && (content.trim() !== '' || file != null) && !fileError;

  async function submit() {
    setBusy(true);
    setError(null);
    try {
      const created = await api.materials.create({
        title: title.trim(),
        description: description.trim() || undefined,
        category: category.trim() || undefined,
        content: content.trim() || undefined,
        file: file ?? undefined,
      });
      onCreated(created);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Не удалось добавить материал');
      setBusy(false);
    }
  }

  return (
    <Modal
      title="Новый материал"
      wide
      onClose={onClose}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose} disabled={busy}>Отмена</button>
          <button type="button" className="btn btn--primary" onClick={() => void submit()} disabled={!ready || busy}>
            {busy ? 'Загрузка…' : 'Добавить'}
          </button>
        </>
      }
    >
      <div className="stack">
        {error && <p className="field__error" role="alert">{error}</p>}
        <div className="grid grid--2">
          <Field label="Название" error={titleError}>
            <input className="input" value={title} maxLength={200} onChange={(e) => setTitle(e.target.value)} />
          </Field>
          <Field label="Раздел">
            <input className="input" list="material-categories" value={category} maxLength={100} onChange={(e) => setCategory(e.target.value)} />
            <datalist id="material-categories">
              {categories.map((c) => <option key={c} value={c} />)}
            </datalist>
          </Field>
        </div>
        <Field label="Краткое описание">
          <input className="input" value={description} maxLength={2000} onChange={(e) => setDescription(e.target.value)} />
        </Field>
        <Field label="Текст материала" hint="Markdown: # заголовок, **жирный**, списки через «-», таблицы через «|». Нужен текст или файл.">
          <textarea className="textarea" rows={8} value={content} onChange={(e) => setContent(e.target.value)} />
        </Field>
        <Field label="Файл" hint="PDF, DOCX, XLSX, MP3, WAV, PNG, JPG, TXT, XML, JSON — до 20 МБ" error={fileError}>
          <input className="input input--file" type="file" accept={ACCEPT} onChange={(e) => setFile(e.target.files?.[0] ?? null)} />
        </Field>
      </div>
    </Modal>
  );
}
