import type { ReactNode } from 'react';

/**
 * Небольшой рендерер Markdown для справочной базы.
 *
 * Строит React-элементы, а не HTML-строку: текст материала загружают
 * преподаватели, и `dangerouslySetInnerHTML` открыл бы XSS. Сырой HTML в
 * тексте показывается как текст. Ссылки — только http(s), mailto и
 * относительные: `javascript:` и `data:` в href не попадут.
 *
 * Поддержано то, что встречается в памятках: заголовки, абзацы, списки,
 * цитаты, таблицы, блоки кода, горизонтальная линия, **жирный**, *курсив*,
 * `код` и [ссылки](url).
 */
export function Markdown({ text }: { text: string }) {
  return <div className="md">{blocks(text.replace(/\r\n?/g, '\n'))}</div>;
}

function safeHref(url: string): string | undefined {
  const u = url.trim();
  if (/^(https?:|mailto:)/i.test(u) || u.startsWith('/') || u.startsWith('#')) return u;
  return undefined;
}

/** Строчная разметка: код, ссылки, жирный, курсив. */
function inline(text: string, keyBase = 'i'): ReactNode[] {
  const out: ReactNode[] = [];
  const re = /(`[^`]+`)|(\[[^\]]+\]\([^)\s]+\))|(\*\*[^*]+\*\*)|(\*[^*\s][^*]*\*|_[^_\s][^_]*_)/g;
  let last = 0;
  let m: RegExpExecArray | null;
  let n = 0;
  while ((m = re.exec(text))) {
    if (m.index > last) out.push(text.slice(last, m.index));
    const token = m[0];
    const key = `${keyBase}-${n++}`;
    if (m[1]) out.push(<code key={key}>{token.slice(1, -1)}</code>);
    else if (m[2]) {
      const [, label, url] = /^\[([^\]]+)\]\(([^)\s]+)\)$/.exec(token) ?? [];
      const href = safeHref(url ?? '');
      out.push(href
        ? <a key={key} href={href} target={href.startsWith('http') ? '_blank' : undefined} rel="noreferrer noopener">{label}</a>
        : <span key={key}>{label}</span>);
    } else if (m[3]) out.push(<strong key={key}>{inline(token.slice(2, -2), key)}</strong>);
    else out.push(<em key={key}>{inline(token.slice(1, -1), key)}</em>);
    last = m.index + token.length;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

const cellsOf = (row: string): string[] => row.trim().replace(/^\||\|$/g, '').split('|').map((c) => c.trim());

function blocks(src: string): ReactNode[] {
  const lines = src.split('\n');
  const out: ReactNode[] = [];
  let i = 0;
  let k = 0;
  const isBlockStart = (l: string) => /^(#{1,6}\s|>|```|\s*([-*+]|\d+\.)\s|\s*(---|\*\*\*)\s*$)/.test(l) || isTableStart(i);
  function isTableStart(at: number): boolean {
    return /^\s*\|.*\|\s*$/.test(lines[at] ?? '') && /^\s*\|?\s*:?-{2,}/.test(lines[at + 1] ?? '');
  }

  while (i < lines.length) {
    const line = lines[i];
    const key = `b${k++}`;
    if (!line.trim()) { i++; continue; }

    if (line.startsWith('```')) {
      const body: string[] = [];
      i++;
      while (i < lines.length && !lines[i].startsWith('```')) body.push(lines[i++]);
      i++;
      out.push(<pre key={key}><code>{body.join('\n')}</code></pre>);
      continue;
    }
    const h = /^(#{1,6})\s+(.*)$/.exec(line);
    if (h) {
      // Заголовки материала вложены в страницу: h1 материала — это h2 страницы.
      const level = Math.min(6, h[1].length + 1);
      const Tag = `h${level}` as 'h2';
      out.push(<Tag key={key}>{inline(h[2], key)}</Tag>);
      i++;
      continue;
    }
    if (/^\s*(---|\*\*\*)\s*$/.test(line)) { out.push(<hr key={key} />); i++; continue; }
    if (line.startsWith('>')) {
      const body: string[] = [];
      while (i < lines.length && lines[i].startsWith('>')) body.push(lines[i++].replace(/^>\s?/, ''));
      out.push(<blockquote key={key}>{blocks(body.join('\n'))}</blockquote>);
      continue;
    }
    if (isTableStart(i)) {
      const head = cellsOf(lines[i]);
      i += 2;
      const rows: string[][] = [];
      while (i < lines.length && /^\s*\|.*\|\s*$/.test(lines[i])) rows.push(cellsOf(lines[i++]));
      out.push(
        <div key={key} className="table-scroll">
          <table className="table">
            <thead><tr>{head.map((c, j) => <th key={j}>{inline(c, `${key}h${j}`)}</th>)}</tr></thead>
            <tbody>{rows.map((r, ri) => <tr key={ri}>{r.map((c, j) => <td key={j}>{inline(c, `${key}r${ri}c${j}`)}</td>)}</tr>)}</tbody>
          </table>
        </div>,
      );
      continue;
    }
    const list = /^\s*([-*+]|\d+\.)\s+/.exec(line);
    if (list) {
      const ordered = /\d/.test(list[1]);
      const items: string[] = [];
      while (i < lines.length && /^\s*([-*+]|\d+\.)\s+/.test(lines[i])) {
        let item = lines[i++].replace(/^\s*([-*+]|\d+\.)\s+/, '');
        // Продолжение пункта — строка с отступом.
        while (i < lines.length && /^\s{2,}\S/.test(lines[i]) && !/^\s*([-*+]|\d+\.)\s+/.test(lines[i])) item += ` ${lines[i++].trim()}`;
        items.push(item);
      }
      const children = items.map((it, j) => <li key={j}>{inline(it, `${key}l${j}`)}</li>);
      out.push(ordered ? <ol key={key}>{children}</ol> : <ul key={key}>{children}</ul>);
      continue;
    }
    const para: string[] = [];
    while (i < lines.length && lines[i].trim() && !(para.length > 0 && isBlockStart(lines[i]))) para.push(lines[i++].trim());
    out.push(<p key={key}>{inline(para.join(' '), key)}</p>);
  }
  return out;
}
