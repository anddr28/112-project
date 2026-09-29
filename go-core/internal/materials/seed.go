package materials

import (
	"bufio"
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Системные материалы поставки — Markdown с заголовком-«front matter»:
//
//	---
//	slug: 01-arm112-memo
//	title: …
//	category: …
//	description: …
//	---
//	текст
//
// Тексты написаны командой по мотивам памятки ОКР Службы 112 для ДДС, инструкции оператора,
// правил статусов реагирования и методики оценки тренажёра.
//
//go:embed data/*.md
var dataFS embed.FS

// SystemMaterial — встроенный материал.
type SystemMaterial struct {
	Slug, Title, Category, Description, Content string
}

// System — встроенные материалы в порядке slug (разобранные; ошибка — битый файл поставки).
func System() ([]SystemMaterial, error) {
	names, err := fs.Glob(dataFS, "data/*.md")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	out := make([]SystemMaterial, 0, len(names))
	for _, n := range names {
		b, err := dataFS.ReadFile(n)
		if err != nil {
			return nil, err
		}
		m, err := parseMaterial(string(b))
		if err != nil {
			return nil, fmt.Errorf("materials: %s: %w", path.Base(n), err)
		}
		out = append(out, m)
	}
	return out, nil
}

func parseMaterial(s string) (SystemMaterial, error) {
	var m SystemMaterial
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return m, fmt.Errorf("нет заголовка ---")
	}
	head, body, ok := strings.Cut(s[4:], "\n---\n")
	if !ok {
		return m, fmt.Errorf("заголовок не закрыт ---")
	}
	sc := bufio.NewScanner(strings.NewReader(head))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "slug":
			m.Slug = v
		case "title":
			m.Title = v
		case "category":
			m.Category = v
		case "description":
			m.Description = v
		}
	}
	m.Content = strings.TrimSpace(body) + "\n"
	if m.Slug == "" || runes(m.Title) < minTitleRunes || runes(m.Title) > maxTitleRunes || m.Category == "" ||
		runes(m.Description) > maxDescRunes || runes(m.Content) > maxContentRunes {
		return m, fmt.Errorf("неполный или слишком длинный материал (slug/title/category/description/content)")
	}
	return m, nil
}

// sqlSeed — системный материал по slug: новые добавляются, изменившиеся в поставке —
// обновляются (текст правится релизом, не руками в БД); неизменённые не переписываются.
const sqlSeed = `
INSERT INTO materials (slug, title, description, category, content, is_system)
VALUES ($1, $2, $3, $4, $5, true)
ON CONFLICT (slug) WHERE slug IS NOT NULL DO UPDATE
   SET title = EXCLUDED.title, description = EXCLUDED.description, category = EXCLUDED.category,
       content = EXCLUDED.content, is_system = true
 WHERE (materials.title, materials.description, materials.category, materials.content, materials.is_system)
       IS DISTINCT FROM (EXCLUDED.title, EXCLUDED.description, EXCLUDED.category, EXCLUDED.content, true)`

// SeedSystem — системные материалы (идемпотентно, один round-trip). Вызывается при каждом
// сиде справочников — не только в демо-режиме: это содержимое продукта, а не демо-данные.
func SeedSystem(ctx context.Context, pool *pgxpool.Pool) error {
	ms, err := System()
	if err != nil {
		return err
	}
	b := &pgx.Batch{}
	for _, m := range ms {
		b.Queue(sqlSeed, m.Slug, m.Title, m.Description, m.Category, m.Content)
	}
	if err := pool.SendBatch(ctx, b).Close(); err != nil {
		return fmt.Errorf("materials: seed: %w", err)
	}
	return nil
}
