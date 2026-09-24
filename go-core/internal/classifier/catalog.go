package classifier

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/pg"
)

// Catalog — классификатор и службы в памяти (реализация core.Catalog).
// Снимок неизменяем и подменяется атомарно (Reload): читатели не берут блокировок,
// ответы справочных ручек предкодированы при загрузке ([]byte + ETag).
type Catalog struct {
	pool *pgxpool.Pool
	log  *slog.Logger
	snap atomic.Pointer[snapshot]
	mu   sync.Mutex // сериализует Reload: более старый снимок не должен затереть новый
	fp   string     // отпечаток данных БД последнего Reload (для Run)
}

var _ core.Catalog = (*Catalog)(nil)

// NewCatalog — пустой каталог; данные появятся после Reload (до этого справочные ручки
// отдают пустые списки, а не 500).
func NewCatalog(pool *pgxpool.Pool, log *slog.Logger) *Catalog {
	if log == nil {
		log = slog.Default()
	}
	c := &Catalog{pool: pool, log: log}
	c.snap.Store(buildSnapshot(nil, nil))
	return c
}

func (c *Catalog) s() *snapshot { return c.snap.Load() }

// ---------------------------------------------------------------- снимок

// cached — предкодированный ответ.
type cached struct {
	body []byte
	etag string
}

func newCached(body []byte) cached {
	sum := sha256.Sum256(body)
	return cached{body: body, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
}

type typeEntry struct {
	info         core.IncidentTypeInfo
	active       bool
	synonyms     []string
	reference    string
	featured     string
	featuredRank int
	sortOrder    int
	rulesRank    int // порядок правил фикстуры (-1 — нет): порядок обхода в ResolveServices
	fixtureID    string
	rules        []ruleJSON
	groups       []public.AttributeGroup // отсортированы по order

	// поиск (Инструкция п.4.2)
	nameNorm  string
	nameTok   []string
	otherTok  []string // код + синонимы
	exact     []string // нормализованные название, код, синонимы — ранг «точное совпадение»
	js        []byte   // public.IncidentType
	attrsBody cached   // []public.AttributeGroup
	refBody   []byte   // {exists, text}
}

type svcEntry struct {
	info   core.ServiceInfo
	active bool
	order  int
}

type snapshot struct {
	types   []*typeEntry // все, по sort_order, code
	active  []*typeEntry // выдаются фронту (is_active)
	byID    map[string]*typeEntry
	byCode  map[string]*typeEntry
	byAlias map[string]*typeEntry // id типа во фронтовых фикстурах ("it-101")

	services  []*svcEntry // по порядку фикстуры, затем экстренные, затем по коду
	svcByID   map[string]*svcEntry
	svcByCode map[string]*svcEntry

	attrLabels  map[string]string
	valueLabels map[string]map[string]string

	typesBody    cached
	featuredBody cached
	labelsBody   cached
	servicesBody cached
	emptyAttrs   cached
}

// rawType — строка classifier_categories.
type rawType struct {
	id           uuid.UUID
	code         string
	parentID     *uuid.UUID
	name         string
	depth        int
	services     []byte
	attributes   []byte
	active       bool
	synonyms     []string
	reference    *string
	featured     *string
	sortOrder    int
	serviceRules []byte
	featuredRank int
	rulesRank    int
	fixtureID    string
}

type rawService struct {
	id        uuid.UUID
	code      string
	name      string
	shortName string
	kind      string
	active    bool
}

const qTypes = `
SELECT id, code, parent_id, name, depth, services, attributes, is_active, synonyms, reference, featured,
       sort_order, service_rules,
       COALESCE(CASE WHEN jsonb_typeof(extra->'featured_rank') = 'number' THEN (extra->>'featured_rank')::numeric::int END, 0),
       COALESCE(CASE WHEN jsonb_typeof(extra->'rules_rank') = 'number' THEN (extra->>'rules_rank')::numeric::int END, -1),
       COALESCE(extra->>'fixture_id', '')
  FROM classifier_categories
 ORDER BY sort_order, code`

const qServices = `SELECT id, code, name, short_name, kind, is_active FROM services`

// qFingerprint — дешёвый отпечаток справочников: меняется при любой вставке/правке/удалении
// (у classifier_categories есть updated_at с триггером; services маленькая — хэш содержимого).
const qFingerprint = `
SELECT (SELECT count(*)::text || ':' || COALESCE(max(updated_at)::text, '') FROM classifier_categories)
    || '|' ||
       (SELECT COALESCE(md5(string_agg(code || '/' || name || '/' || short_name || '/' || kind || '/' || is_active::text, ',' ORDER BY code)), '')
          FROM services)`

// Reload перечитывает справочники из БД и атомарно подменяет снимок.
func (c *Catalog) Reload(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var fp string
	if err := c.pool.QueryRow(ctx, qFingerprint).Scan(&fp); err != nil {
		return fmt.Errorf("classifier: fingerprint: %w", err)
	}
	types, err := loadTypes(ctx, c.pool)
	if err != nil {
		return err
	}
	services, err := loadServices(ctx, c.pool)
	if err != nil {
		return err
	}
	snap := buildSnapshot(types, services)
	c.snap.Store(snap)
	c.fp = fp
	c.log.Info("classifier: справочники загружены", "types", len(snap.types), "active", len(snap.active), "services", len(snap.services))
	return nil
}

// Run — фоновая сверка с БД: импорт XLSX или сид, выполненные другим процессом
// (cmd import-classifier), подхватываются без перезапуска. Один лёгкий запрос за период.
func (c *Catalog) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 30 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var fp string
		qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := c.pool.QueryRow(qctx, qFingerprint).Scan(&fp)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				c.log.Warn("classifier: проверка изменений справочников", "err", err)
			}
			continue
		}
		c.mu.Lock()
		changed := fp != c.fp
		c.mu.Unlock()
		if changed {
			rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			if err := c.Reload(rctx); err != nil && ctx.Err() == nil {
				c.log.Error("classifier: перечитывание справочников", "err", err)
			}
			cancel()
		}
	}
}

func loadTypes(ctx context.Context, q pg.Querier) ([]rawType, error) {
	rows, err := q.Query(ctx, qTypes)
	if err != nil {
		return nil, fmt.Errorf("classifier: load types: %w", err)
	}
	defer rows.Close()
	out := make([]rawType, 0, 256)
	for rows.Next() {
		var t rawType
		var depth int16
		if err := rows.Scan(&t.id, &t.code, &t.parentID, &t.name, &depth, &t.services, &t.attributes, &t.active,
			&t.synonyms, &t.reference, &t.featured, &t.sortOrder, &t.serviceRules,
			&t.featuredRank, &t.rulesRank, &t.fixtureID); err != nil {
			return nil, fmt.Errorf("classifier: scan type: %w", err)
		}
		t.depth = int(depth)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("classifier: load types: %w", err)
	}
	return out, nil
}

func loadServices(ctx context.Context, q pg.Querier) ([]rawService, error) {
	rows, err := q.Query(ctx, qServices)
	if err != nil {
		return nil, fmt.Errorf("classifier: load services: %w", err)
	}
	defer rows.Close()
	out := make([]rawService, 0, 64)
	for rows.Next() {
		var s rawService
		if err := rows.Scan(&s.id, &s.code, &s.name, &s.shortName, &s.kind, &s.active); err != nil {
			return nil, fmt.Errorf("classifier: scan service: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("classifier: load services: %w", err)
	}
	return out, nil
}

// buildSnapshot — чистая функция: строки БД -> индексы и предкодированные ответы.
func buildSnapshot(types []rawType, services []rawService) *snapshot {
	emb := data()
	s := &snapshot{
		types:       make([]*typeEntry, 0, len(types)),
		byID:        make(map[string]*typeEntry, len(types)),
		byCode:      make(map[string]*typeEntry, len(types)),
		byAlias:     make(map[string]*typeEntry),
		svcByID:     make(map[string]*svcEntry, len(services)),
		svcByCode:   make(map[string]*svcEntry, len(services)),
		attrLabels:  make(map[string]string),
		valueLabels: make(map[string]map[string]string),
	}

	// ---- службы
	for _, r := range services {
		e := &svcEntry{
			info:   core.ServiceInfo{ID: r.id.String(), Code: r.code, Name: r.name, ShortName: r.shortName, Kind: r.kind},
			active: r.active,
			order:  1 << 20,
		}
		if i, ok := emb.serviceOrder[r.code]; ok {
			e.order = i
		}
		s.services = append(s.services, e)
		s.svcByID[e.info.ID] = e
		s.svcByCode[e.info.Code] = e
	}
	sort.SliceStable(s.services, func(i, j int) bool {
		a, b := s.services[i], s.services[j]
		if a.order != b.order {
			return a.order < b.order
		}
		if (a.info.Kind == "emergency") != (b.info.Kind == "emergency") {
			return a.info.Kind == "emergency"
		}
		return a.info.Code < b.info.Code
	})

	// ---- типы
	for i := range types {
		r := &types[i]
		e := &typeEntry{
			info: core.IncidentTypeInfo{
				ID:    r.id.String(),
				Code:  r.code,
				Name:  r.name,
				Depth: r.depth,
			},
			active:       r.active,
			synonyms:     r.synonyms,
			sortOrder:    r.sortOrder,
			featuredRank: r.featuredRank,
			rulesRank:    r.rulesRank,
			fixtureID:    r.fixtureID,
		}
		if e.synonyms == nil {
			e.synonyms = []string{}
		}
		if r.parentID != nil {
			e.info.ParentID = r.parentID.String()
		}
		if r.reference != nil {
			e.reference = strings.TrimSpace(*r.reference)
		}
		if r.featured != nil {
			e.featured = *r.featured
		}
		// Битый jsonb не должен ронять весь справочник: тип остаётся без опросной карты/правил.
		if len(r.attributes) > 0 {
			_ = json.Unmarshal(r.attributes, &e.groups)
		}
		sort.SliceStable(e.groups, func(i, j int) bool { return e.groups[i].Order < e.groups[j].Order })
		if len(r.serviceRules) > 0 {
			_ = json.Unmarshal(r.serviceRules, &e.rules)
		}
		var svcCodes []string
		if len(r.services) > 0 {
			_ = json.Unmarshal(r.services, &svcCodes)
		}
		if len(svcCodes) == 0 {
			svcCodes = codesFromRules(e.rules)
		}
		e.info.Services = svcCodes
		e.info.Attributes = make([]string, 0, len(e.groups))
		for _, g := range e.groups {
			e.info.Attributes = append(e.info.Attributes, g.Code)
		}
		e.nameNorm = normalize(r.name)
		e.nameTok = tokens(r.name)
		e.otherTok = tokens(r.code)
		e.exact = make([]string, 0, 2+len(e.synonyms))
		e.exact = append(e.exact, e.nameNorm, normalize(r.code))
		for _, syn := range e.synonyms {
			e.otherTok = append(e.otherTok, tokens(syn)...)
			e.exact = append(e.exact, normalize(syn))
		}

		s.types = append(s.types, e)
		s.byID[e.info.ID] = e
		s.byCode[e.info.Code] = e
		if e.fixtureID != "" {
			s.byAlias[e.fixtureID] = e
		}
	}
	// типы уже отсортированы запросом; повторная стабильная сортировка — для вызова из тестов
	sort.SliceStable(s.types, func(i, j int) bool {
		if s.types[i].sortOrder != s.types[j].sortOrder {
			return s.types[i].sortOrder < s.types[j].sortOrder
		}
		return s.types[i].info.Code < s.types[j].info.Code
	})

	// Путь от корня (scenario_context.category.path) — по parent_id, с защитой от циклов.
	for _, e := range s.types {
		path := make([]string, 0, 4)
		seen := 0
		for p := e; p != nil && seen < 16; seen++ {
			path = append(path, p.info.Name)
			if p.info.ParentID == "" {
				break
			}
			p = s.byID[p.info.ParentID]
		}
		for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
			path[i], path[j] = path[j], path[i]
		}
		e.info.Path = path
	}

	// Подписи признаков и значений (как labels() в mockApi.ts: сбор по всем опросным картам).
	for _, e := range s.types {
		for _, g := range e.groups {
			if _, ok := s.attrLabels[g.Code]; !ok {
				s.attrLabels[g.Code] = g.Label
			}
			if g.Options != nil && len(*g.Options) > 0 {
				vm := s.valueLabels[g.Code]
				if vm == nil {
					vm = make(map[string]string, len(*g.Options))
					s.valueLabels[g.Code] = vm
				}
				for _, o := range *g.Options {
					if _, ok := vm[o.Code]; !ok {
						vm[o.Code] = o.Label
					}
				}
			}
		}
	}

	// ---- предкодирование
	s.emptyAttrs = newCached([]byte("[]"))
	for _, e := range s.types {
		it := public.IncidentType{
			Id:           e.info.ID,
			Code:         e.info.Code,
			Name:         e.info.Name,
			Depth:        e.info.Depth,
			Synonyms:     e.synonyms,
			HasReference: e.reference != "",
		}
		if e.info.ParentID != "" {
			pid := e.info.ParentID
			it.ParentId = &pid
		}
		e.js = mustMarshal(it)
		if len(e.groups) == 0 {
			e.attrsBody = s.emptyAttrs
		} else {
			e.attrsBody = newCached(mustMarshal(e.groups))
		}
		if e.reference != "" {
			text := e.reference
			e.refBody = mustMarshal(struct {
				Exists bool    `json:"exists"`
				Text   *string `json:"text,omitempty"`
			}{true, &text})
		} else {
			e.refBody = refMissing
		}
		if e.active {
			s.active = append(s.active, e)
		}
	}
	s.typesBody = newCached(joinTypes(s.active))

	var frequent, significant []*typeEntry
	for _, e := range s.active {
		switch e.featured {
		case "frequent":
			frequent = append(frequent, e)
		case "significant":
			significant = append(significant, e)
		}
	}
	byRank := func(l []*typeEntry) {
		sort.SliceStable(l, func(i, j int) bool { return l[i].featuredRank < l[j].featuredRank })
	}
	byRank(frequent)
	byRank(significant)
	var fb bytes.Buffer
	fb.WriteString(`{"frequent":`)
	fb.Write(joinTypes(frequent))
	fb.WriteString(`,"significant":`)
	fb.Write(joinTypes(significant))
	fb.WriteByte('}')
	s.featuredBody = newCached(fb.Bytes())

	typeNames := make(map[string]string, len(s.types))
	for _, e := range s.types {
		typeNames[e.info.ID] = e.info.Name
	}
	s.labelsBody = newCached(mustMarshal(public.ClassifierLabels{
		Fields:     emb.fieldLabels,
		Attributes: s.attrLabels,
		Values:     s.valueLabels,
		Types:      typeNames,
	}))

	refs := make([]public.ServiceRef, 0, len(s.services))
	for _, e := range s.services {
		if !e.active {
			continue
		}
		refs = append(refs, public.ServiceRef{
			Id: e.info.ID, Code: e.info.Code, Name: e.info.Name, ShortName: e.info.ShortName,
			Kind: public.ServiceRefKind(e.info.Kind),
		})
	}
	s.servicesBody = newCached(mustMarshal(refs))
	return s
}

var refMissing = []byte(`{"exists":false}`)

// joinTypes — JSON-массив из предкодированных типов (без повторной сериализации).
func joinTypes(l []*typeEntry) []byte {
	n := 2
	for _, e := range l {
		n += len(e.js) + 1
	}
	b := make([]byte, 0, n)
	b = append(b, '[')
	for i, e := range l {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, e.js...)
	}
	return append(b, ']')
}

// mustMarshal — JSON без HTML-экранирования (как httpx.WriteJSON); типы из gen всегда
// сериализуемы, поэтому ошибка — программная.
func mustMarshal(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic("classifier: marshal: " + err.Error())
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func codesFromRules(rules []ruleJSON) []string {
	out := make([]string, 0, 4)
	seen := make(map[string]struct{}, 8)
	for _, r := range rules {
		for _, sv := range r.Services {
			if _, ok := seen[sv.Code]; ok {
				continue
			}
			seen[sv.Code] = struct{}{}
			out = append(out, sv.Code)
		}
	}
	return out
}

// ---------------------------------------------------------------- core.Catalog

// lookupType — по uuid, коду или id фронтовой фикстуры ("it-101"): фронт и демо-данные
// могут прислать любой из них, а ответ всегда несёт uuid.
func (s *snapshot) lookupType(key string) *typeEntry {
	if e := s.byID[key]; e != nil {
		return e
	}
	if e := s.byCode[key]; e != nil {
		return e
	}
	return s.byAlias[key]
}

// TypeByID — тип по uuid (строкой); для переноса фикстур принимается и id фронтовой
// фикстуры ("it-101"). Срезы в результате общие со снимком — не изменять.
func (c *Catalog) TypeByID(id string) (core.IncidentTypeInfo, bool) {
	s := c.s()
	e := s.byID[id]
	if e == nil {
		e = s.byAlias[id]
	}
	if e == nil {
		return core.IncidentTypeInfo{}, false
	}
	return e.info, true
}

// TypeByCode — тип по коду классификатора.
func (c *Catalog) TypeByCode(code string) (core.IncidentTypeInfo, bool) {
	if e := c.s().byCode[code]; e != nil {
		return e.info, true
	}
	return core.IncidentTypeInfo{}, false
}

// ResolveType — тип по uuid, коду или id фикстуры.
func (c *Catalog) ResolveType(key string) (core.IncidentTypeInfo, bool) {
	if e := c.s().lookupType(key); e != nil {
		return e.info, true
	}
	return core.IncidentTypeInfo{}, false
}

func (c *Catalog) ServiceByID(id string) (core.ServiceInfo, bool) {
	if e := c.s().svcByID[id]; e != nil {
		return e.info, true
	}
	return core.ServiceInfo{}, false
}

func (c *Catalog) ServiceByCode(code string) (core.ServiceInfo, bool) {
	if e := c.s().svcByCode[code]; e != nil {
		return e.info, true
	}
	return core.ServiceInfo{}, false
}

// Services — активные службы в порядке справочника.
func (c *Catalog) Services() []core.ServiceInfo {
	s := c.s()
	out := make([]core.ServiceInfo, 0, len(s.services))
	for _, e := range s.services {
		if e.active {
			out = append(out, e.info)
		}
	}
	return out
}

// FieldLabel — подпись поля карточки по пути (как fieldLabel во фронте, плюс подпись
// конкретного признака для "attributes.<code>").
func (c *Catalog) FieldLabel(path string) string {
	if l, ok := data().fieldLabels[path]; ok {
		return l
	}
	if code, ok := strings.CutPrefix(path, "attributes."); ok {
		if l, ok := c.s().attrLabels[code]; ok {
			return l
		}
		return "Признак опросной карты"
	}
	return "Поле карточки"
}

func (c *Catalog) AttributeLabel(code string) (string, bool) {
	l, ok := c.s().attrLabels[code]
	return l, ok
}

func (c *Catalog) AttributeValueLabel(attr, value string) (string, bool) {
	vm := c.s().valueLabels[attr]
	if vm == nil {
		return "", false
	}
	l, ok := vm[value]
	return l, ok
}

// Attributes — опросная карта типа (uuid/код/id фикстуры), отсортирована по order.
func (c *Catalog) Attributes(typeKey string) ([]public.AttributeGroup, bool) {
	e := c.s().lookupType(typeKey)
	if e == nil {
		return nil, false
	}
	out := make([]public.AttributeGroup, len(e.groups))
	copy(out, e.groups)
	return out, true
}
