package classifier

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/pgtest"
)

// DB-тесты: сид справочников, загрузка каталога, фоновая сверка, импорт XLSX.
// pgtest.New пропускает тест, если PostgreSQL недоступен.

func seededPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := pgtest.New(t)
	if err := SeedReference(context.Background(), pool); err != nil {
		t.Fatalf("SeedReference: %v", err)
	}
	return pool
}

func loadedCatalog(t *testing.T, pool *pgxpool.Pool) *Catalog {
	t.Helper()
	c := NewCatalog(pool, discardLog())
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	return c
}

type typeRow struct {
	id        uuid.UUID
	updatedAt time.Time
	extra     string
	active    bool
	name      string
}

func typeRows(t *testing.T, pool *pgxpool.Pool) map[string]typeRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT code, id, updated_at, extra::text, is_active, name FROM classifier_categories`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]typeRow{}
	for rows.Next() {
		var code string
		var r typeRow
		if err := rows.Scan(&code, &r.id, &r.updatedAt, &r.extra, &r.active, &r.name); err != nil {
			t.Fatal(err)
		}
		out[code] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Каталог из PostgreSQL ведёт себя так же, как каталог из фикстур в памяти (на нём — остальные тесты).
func TestSeedAndReloadMatchFixtures(t *testing.T) {
	t.Parallel()
	pool := seededPool(t)
	db := loadedCatalog(t, pool)
	mem := fixtureCatalog(t)

	type view struct {
		code, name   string
		depth        int
		synonyms     []string
		hasReference bool
	}
	viewOf := func(c *Catalog) []view {
		var out []view
		for _, it := range decode[[]public.IncidentType](t, c.s().typesBody.body) {
			out = append(out, view{it.Code, it.Name, it.Depth, it.Synonyms, it.HasReference})
		}
		return out
	}
	if a, b := viewOf(db), viewOf(mem); !slices.EqualFunc(a, b, func(x, y view) bool {
		return x.code == y.code && x.name == y.name && x.depth == y.depth && x.hasReference == y.hasReference && slices.Equal(x.synonyms, y.synonyms)
	}) {
		t.Errorf("types differ:\n db %v\nmem %v", a, b)
	}
	type featured struct {
		Frequent    []public.IncidentType `json:"frequent"`
		Significant []public.IncidentType `json:"significant"`
	}
	fa, fb := decode[featured](t, db.s().featuredBody.body), decode[featured](t, mem.s().featuredBody.body)
	if !slices.Equal(codesOf(fa.Frequent), codesOf(fb.Frequent)) || !slices.Equal(codesOf(fa.Significant), codesOf(fb.Significant)) {
		t.Errorf("featured differ: %v / %v", codesOf(fa.Frequent), codesOf(fb.Frequent))
	}
	la, lb := decode[public.ClassifierLabels](t, db.s().labelsBody.body), decode[public.ClassifierLabels](t, mem.s().labelsBody.body)
	if !maps.Equal(la.Attributes, lb.Attributes) || !maps.EqualFunc(la.Values, lb.Values, maps.Equal) || len(la.Types) != len(lb.Types) {
		t.Error("labels differ")
	}
	for _, code := range []string{"101", "104", "dtp", "water", "cancel"} {
		a, _ := db.TypeByCode(code)
		b, _ := mem.TypeByCode(code)
		if !slices.Equal(a.Services, b.Services) || !slices.Equal(a.Attributes, b.Attributes) || !slices.Equal(a.Path, b.Path) {
			t.Errorf("%s: db %+v mem %+v", code, a, b)
		}
		if _, ok := db.TypeByID(a.ID); !ok {
			t.Errorf("%s by uuid", code)
		}
		if got, ok := db.TypeByID(fixtureAlias(code)); !ok || got.ID != a.ID {
			t.Errorf("%s by fixture id", code)
		}
	}
	req := public.ResolveServicesRequest{TypeIds: []string{"it-101", "it-water", "it-dtp"},
		Attributes:    map[string]any{"people_threat": "yes", "water_where": "street", "victims": "yes", "gasified": "yes"},
		AddressFilled: true}
	if a, b := refsOf(db.resolveAt(req, t0).Services), refsOf(mem.resolveAt(req, t0).Services); !slices.Equal(a, b) {
		t.Errorf("resolve differs:\n db %+v\nmem %+v", a, b)
	}

	// службы: фикстура + служба миграции, которой нет во фронтовых фикстурах
	var codes []string
	for _, s := range db.Services() {
		codes = append(codes, s.Code)
	}
	var want []string
	for _, s := range data().services {
		want = append(want, s.Code)
	}
	want = append(want, "moskollektor")
	if !slices.Equal(codes, want) {
		t.Errorf("services %v, want %v", codes, want)
	}
	s101, _ := db.ServiceByCode("101")
	if s101.Name != data().services[0].Name || s101.ShortName != "Служба 101" || s101.Kind != "emergency" {
		t.Errorf("101 after seed: %+v", s101)
	}
}

func TestSeedReferenceIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := seededPool(t)
	before := typeRows(t, pool)
	if len(before) != len(data().classifier.Types) {
		t.Fatalf("seeded types: %d", len(before))
	}
	for code, r := range before {
		var extra map[string]any
		_ = json.Unmarshal([]byte(r.extra), &extra)
		if extra["source"] != "fixture" || extra["fixture_id"] != fixtureAlias(code) || !r.active {
			t.Errorf("%s: %+v", code, r)
		}
	}
	// админ выключил службу и кто-то испортил название — сид вернёт название, но не включит службу
	if _, err := pool.Exec(ctx, `UPDATE services SET is_active = false WHERE code = 'oati';
		UPDATE services SET name = 'Старое название' WHERE code = '101'`); err != nil {
		t.Fatal(err)
	}
	// ключи импорта в extra сохраняются при повторном сиде
	if _, err := pool.Exec(ctx, `UPDATE classifier_categories SET extra = extra || '{"note":"x"}' WHERE code = 'dtp'`); err != nil {
		t.Fatal(err)
	}
	mid := typeRows(t, pool)
	fingerprint := func() string {
		t.Helper()
		var fp string
		if err := pool.QueryRow(ctx, qFingerprint).Scan(&fp); err != nil {
			t.Fatal(err)
		}
		return fp
	}
	typesPart := func(fp string) string { return strings.SplitN(fp, "|", 2)[0] }
	fpMid := fingerprint()

	if err := SeedReference(ctx, pool); err != nil {
		t.Fatal(err)
	}
	after := typeRows(t, pool)
	for code, r := range mid {
		a := after[code]
		// неизменённая строка не переписывается: updated_at (и отпечаток каталога) стоят на месте
		if a.id != r.id || !a.updatedAt.Equal(r.updatedAt) || a.extra != r.extra {
			t.Errorf("%s rewritten by an idempotent seed: %+v -> %+v", code, r, a)
		}
	}
	// повторный сид не двигает отпечаток типов (Catalog.Run не перечитывает зря), а правка служб — двигает
	fpAfter := fingerprint()
	if typesPart(fpAfter) != typesPart(fpMid) || fpAfter == fpMid {
		t.Errorf("fingerprint: %q -> %q", fpMid, fpAfter)
	}
	if !strings.Contains(after["dtp"].extra, `"note": "x"`) {
		t.Errorf("extra keys lost: %s", after["dtp"].extra)
	}
	var name string
	var active bool
	if err := pool.QueryRow(ctx, `SELECT name FROM services WHERE code = '101'`).Scan(&name); err != nil || name != data().services[0].Name {
		t.Errorf("service name restored: %q %v", name, err)
	}
	if err := pool.QueryRow(ctx, `SELECT is_active FROM services WHERE code = 'oati'`).Scan(&active); err != nil || active {
		t.Errorf("seed must not re-enable a service switched off by the admin: %v %v", active, err)
	}

	// изменённый в БД тип сид возвращает к фикстуре, id сохраняется (ссылки сценариев целы)
	if _, err := pool.Exec(ctx, `UPDATE classifier_categories SET name = 'Испорчено', synonyms = '{}' WHERE code = '104'`); err != nil {
		t.Fatal(err)
	}
	if err := SeedReference(ctx, pool); err != nil {
		t.Fatal(err)
	}
	c := loadedCatalog(t, pool)
	info, _ := c.TypeByCode("104")
	if info.Name != "Происшествие 104" || info.ID != before["104"].id.String() {
		t.Errorf("104 after reseed: %+v", info)
	}
	if got := codesOf(c.Search("газ", 1)); !slices.Equal(got, []string{"104"}) {
		t.Errorf("synonyms restored: %v", got)
	}
	if _, ok := c.ServiceByCode("oati"); !ok {
		t.Error("inactive service still resolvable")
	}
	for _, s := range c.Services() {
		if s.Code == "oati" {
			t.Error("inactive service listed")
		}
	}
}

func TestCatalogRunPicksUpChanges(t *testing.T) {
	t.Parallel()
	pool := seededPool(t)
	c := loadedCatalog(t, pool)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx, 20*time.Millisecond)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not stop on cancel")
		}
	}()
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("catalog did not pick up: %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	exec := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), sql); err != nil {
			t.Fatal(err)
		}
	}

	exec(`UPDATE classifier_categories SET name = 'Пожар (изменено)' WHERE code = '101'`)
	waitFor("type rename", func() bool { i, _ := c.TypeByCode("101"); return i.Name == "Пожар (изменено)" })

	exec(`UPDATE services SET short_name = 'С-101' WHERE code = '101'`)
	waitFor("service rename", func() bool { s, _ := c.ServiceByCode("101"); return s.ShortName == "С-101" })

	exec(`INSERT INTO classifier_categories (code, name, depth, synonyms) VALUES ('new-type', 'Новый тип', 1, '{новинка}')`)
	waitFor("new type", func() bool { return len(c.Search("новинка", 5)) == 1 })

	exec(`UPDATE classifier_categories SET is_active = false WHERE code = 'new-type'`)
	waitFor("deactivation", func() bool { return len(c.Search("новинка", 5)) == 0 })

	exec(`DELETE FROM classifier_categories WHERE code = 'new-type'`)
	waitFor("delete", func() bool { _, ok := c.TypeByCode("new-type"); return !ok })
}

func TestReloadErrorKeepsSnapshot(t *testing.T) {
	t.Parallel()
	pool := seededPool(t)
	c := loadedCatalog(t, pool)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Reload(ctx); err == nil {
		t.Fatal("Reload with a cancelled context must fail")
	}
	if _, ok := c.TypeByID("it-101"); !ok {
		t.Error("failed Reload dropped the snapshot")
	}
}

func insertAdmin(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `INSERT INTO users (login, password_hash, role, last_name, first_name)
		VALUES ('admin-'||gen_random_uuid(), 'x', 'admin', 'Админов', 'Админ') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

type batchRow struct {
	status, fileName string
	stats            map[string]any
	errText          *string
	finished         *time.Time
	createdBy        uuid.UUID
}

func batch(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) batchRow {
	t.Helper()
	var b batchRow
	var stats []byte
	if err := pool.QueryRow(context.Background(), `SELECT status, file_name, stats, error, finished_at, created_by
		FROM import_batches WHERE id = $1 AND kind = 'classifier'`, id).Scan(&b.status, &b.fileName, &stats, &b.errText, &b.finished, &b.createdBy); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(stats, &b.stats)
	return b
}

func TestImportClassifierXLSX(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := seededPool(t)
	admin := insertAdmin(t, pool)
	fixtureBefore := typeRows(t, pool)
	path := writeXLSX(t, sampleSheet())

	st, err := ImportClassifierXLSX(ctx, pool, path, admin)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if st.Nodes != 9 || st.Inserted != 9 || st.Updated != 0 || st.Skipped != 3 || st.Deactivated != 0 ||
		st.ServicesAdded != 1 || st.RowsTotal != 11 || len(st.Errors) != 3 || st.BatchID == uuid.Nil {
		t.Errorf("stats %+v", st)
	}
	b := batch(t, pool, st.BatchID)
	if b.status != "done" || b.fileName != "Классификатор_тест.xlsx" || b.errText != nil || b.finished == nil || b.createdBy != admin {
		t.Errorf("batch %+v", b)
	}
	if b.stats["inserted"] != 9.0 || b.stats["nodes"] != 9.0 {
		t.Errorf("batch stats %v", b.stats)
	}

	// иерархия по parent_id
	var path3 []string
	if err := pool.QueryRow(ctx, `
WITH RECURSIVE up AS (
  SELECT id, parent_id, code, 0 AS lvl FROM classifier_categories WHERE code = '24010101'
  UNION ALL SELECT c.id, c.parent_id, c.code, up.lvl + 1 FROM classifier_categories c JOIN up ON c.id = up.parent_id)
SELECT array_agg(code ORDER BY lvl DESC) FROM up`).Scan(&path3); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(path3, []string{"24", "24010000", "24010101"}) {
		t.Errorf("hierarchy %v", path3)
	}
	var svcName, svcKind string
	if err := pool.QueryRow(ctx, `SELECT name, kind FROM services WHERE code = 'xls_v'`).Scan(&svcName, &svcKind); err != nil ||
		svcName != "Служба Новая" || svcKind != "city" {
		t.Errorf("new service: %q %q %v", svcName, svcKind, err)
	}
	var mosliftName string
	_ = pool.QueryRow(ctx, `SELECT name FROM services WHERE code = 'moslift'`).Scan(&mosliftName)
	if mosliftName != "АО «Мослифт»" {
		t.Errorf("existing service renamed by import: %q", mosliftName)
	}
	// типы сида импорт не трогает
	for code, r := range fixtureBefore {
		if a := typeRows(t, pool)[code]; !a.updatedAt.Equal(r.updatedAt) || a.name != r.name {
			t.Errorf("fixture type %s changed by import", code)
		}
	}

	// каталог видит импорт: поиск по признаку, скрытый тип не ищется, службы по правилам и условиям
	c := loadedCatalog(t, pool)
	if got := c.Search("мусор", 10); len(got) != 1 || got[0].Code != "1010101" || got[0].ParentId == nil {
		t.Errorf("search «мусор»: %+v", got)
	}
	info, _ := c.TypeByCode("1010101")
	if !slices.Equal(info.Path, []string{"Пожары и задымления", "На улице", "Пожар: мусор"}) || info.Depth != 3 {
		t.Errorf("imported type info: %+v", info)
	}
	res := c.resolveAt(public.ResolveServicesRequest{TypeIds: []string{info.ID}, Attributes: map[string]any{"offense": "yes"},
		AddressFilled: true}, t0)
	var got []string
	for _, s := range res.Services {
		got = append(got, s.Code)
	}
	if !slices.Equal(got, []string{"101", "103", "102"}) || !res.Services[0].IsPrimary || res.Services[1].IsPrimary {
		t.Errorf("resolve imported type: %v", got)
	}
	if groups, ok := c.Attributes("1010101"); !ok || len(groups) != 3 {
		t.Errorf("imported questionnaire: %+v", groups)
	}
	if l, ok := c.AttributeLabel("offense"); !ok || l != "Правонарушение" {
		t.Errorf("attribute label from import: %q", l)
	}

	// повторный импорт того же файла — ничего не меняется
	st2, err := ImportClassifierXLSX(ctx, pool, path, admin)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Inserted != 0 || st2.Updated != 0 || st2.Deactivated != 0 || st2.ServicesAdded != 0 || st2.Skipped != 3+9 {
		t.Errorf("re-import stats %+v", st2)
	}

	// тип исчез из файла — выключается (не удаляется: на него могут ссылаться сценарии)
	rows := sampleSheet()
	rows = slices.Delete(rows, 7, 8) // «Пожар: автобус»
	st3, err := ImportClassifierXLSX(ctx, pool, writeXLSX(t, rows), admin)
	if err != nil {
		t.Fatal(err)
	}
	if st3.Deactivated != 1 || st3.Inserted != 0 {
		t.Errorf("import without a row: %+v", st3)
	}
	if r := typeRows(t, pool)["1020101"]; r.active {
		t.Error("type missing from the file must be deactivated")
	}
	// вернули строку — тип снова активен
	st4, err := ImportClassifierXLSX(ctx, pool, path, admin)
	if err != nil {
		t.Fatal(err)
	}
	if r := typeRows(t, pool)["1020101"]; !r.active || st4.Updated == 0 {
		t.Errorf("type back in the file must be re-activated: %+v", st4)
	}
}

func TestImportClassifierXLSXFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := seededPool(t)
	admin := insertAdmin(t, pool)

	st, err := ImportClassifierXLSX(ctx, pool, filepath.Join(t.TempDir(), "нет.xlsx"), admin)
	if err == nil {
		t.Fatal("missing file must fail")
	}
	b := batch(t, pool, st.BatchID)
	if b.status != "failed" || b.errText == nil || *b.errText == "" || b.finished == nil {
		t.Errorf("failed batch %+v", b)
	}
	if errs, ok := b.stats["errors"].([]any); !ok || errs == nil {
		t.Errorf("stats.errors must be an array: %v", b.stats)
	}

	notXLSX := filepath.Join(t.TempDir(), "file.xlsx")
	if err := os.WriteFile(notXLSX, []byte("это не xlsx"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err = ImportClassifierXLSX(ctx, pool, notXLSX, admin)
	if err == nil || batch(t, pool, st.BatchID).status != "failed" {
		t.Errorf("corrupt file: %v", err)
	}
	// импорт от несуществующего пользователя — ошибка, без висящего батча
	if _, err := ImportClassifierXLSX(ctx, pool, notXLSX, uuid.New()); err == nil {
		t.Error("unknown user must fail")
	}
	var running int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM import_batches WHERE status = 'running'`).Scan(&running)
	if running != 0 {
		t.Errorf("batches left running: %d", running)
	}
}

// TestImportRealClassifier — реальный файл организаторов (не в репозитории):
// LCT_CLASSIFIER_XLSX=/path/Классификатор_происшествий_v_046_24_….xlsx go test ./internal/classifier -run Real
func TestImportRealClassifier(t *testing.T) {
	t.Parallel()
	path := os.Getenv("LCT_CLASSIFIER_XLSX")
	if path == "" {
		t.Skip("LCT_CLASSIFIER_XLSX not set")
	}
	ctx := context.Background()
	pool := seededPool(t)
	admin := insertAdmin(t, pool)
	st, err := ImportClassifierXLSX(ctx, pool, path, admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("real import: %+v", st)
	if st.Nodes < 1000 || st.Inserted != st.Nodes || len(st.Errors) != 0 {
		t.Errorf("stats %+v", st)
	}
	st2, err := ImportClassifierXLSX(ctx, pool, path, admin)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Inserted != 0 || st2.Updated != 0 || st2.Deactivated != 0 {
		t.Errorf("re-import must be a no-op: %+v", st2)
	}
	c := loadedCatalog(t, pool)
	for q, first := range map[string]string{"газ": "104", "пожар": "101", "дтп": "dtp"} {
		if got := codesOf(c.Search(q, 20)); len(got) == 0 || got[0] != first {
			t.Errorf("search %q after the real import: %v", q, got)
		}
	}
	if got := c.Search("мусор", 20); len(got) == 0 {
		t.Error("imported types are not searchable")
	}
}
