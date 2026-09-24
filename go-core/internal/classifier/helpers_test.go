package classifier

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
)

// Общие помощники тестов пакета: каталог из встроенных фикстур без БД. Строки строятся
// тем же seedRows, что пишет сид, и переводятся в rawType с той же семантикой, что qTypes
// (COALESCE featured_rank -> 0, rules_rank -> -1), — поэтому поведение совпадает с
// каталогом, загруженным из PostgreSQL (это отдельно сверяет DB-тест).

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// typeUUID / serviceUUID — детерминированные id (тесты сравнивают ответы между запусками).
func typeUUID(code string) uuid.UUID { return uuid.NewSHA1(uuid.NameSpaceOID, []byte("type:"+code)) }
func serviceUUID(code string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("service:"+code))
}

func fixtureRaw(t testing.TB) ([]rawType, []rawService) {
	t.Helper()
	emb := data()
	rows, err := seedRows(emb)
	if err != nil {
		t.Fatalf("seedRows: %v", err)
	}
	types := make([]rawType, 0, len(rows))
	for _, r := range rows {
		var extra map[string]any
		if err := json.Unmarshal(r.extra, &extra); err != nil {
			t.Fatalf("extra of %s: %v", r.code, err)
		}
		rt := rawType{
			id:           typeUUID(r.code),
			code:         r.code,
			name:         r.name,
			depth:        int(r.depth),
			services:     r.services,
			attributes:   r.attributes,
			active:       true,
			synonyms:     r.synonyms,
			sortOrder:    int(r.sortOrder),
			serviceRules: r.rules,
			featuredRank: 0,
			rulesRank:    -1,
		}
		if r.parent != "" {
			p := typeUUID(r.parent)
			rt.parentID = &p
		}
		if r.reference != "" {
			ref := r.reference
			rt.reference = &ref
		}
		if r.featured != "" {
			f := r.featured
			rt.featured = &f
		}
		if v, ok := extra["featured_rank"].(float64); ok {
			rt.featuredRank = int(v)
		}
		if v, ok := extra["rules_rank"].(float64); ok {
			rt.rulesRank = int(v)
		}
		if v, ok := extra["fixture_id"].(string); ok {
			rt.fixtureID = v
		}
		types = append(types, rt)
	}
	services := make([]rawService, 0, len(emb.services))
	for _, s := range emb.services {
		services = append(services, rawService{id: serviceUUID(s.Code), code: s.Code, name: s.Name,
			shortName: s.ShortName, kind: s.Kind, active: true})
	}
	return types, services
}

// catalogFrom — каталог без пула с готовым снимком.
func catalogFrom(types []rawType, services []rawService) *Catalog {
	c := NewCatalog(nil, discardLog())
	c.snap.Store(buildSnapshot(types, services))
	return c
}

func fixtureCatalog(t testing.TB) *Catalog {
	t.Helper()
	types, services := fixtureRaw(t)
	return catalogFrom(types, services)
}

func ptr[T any](v T) *T { return &v }
