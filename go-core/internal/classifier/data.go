// Package classifier — справочники карточки АРМ-112: классификатор происшествий
// (типы, опросные карты, справка, «частые/значимые»), службы, подписи технических
// кодов, определение служб (resolve-services), подсказки адреса по локальному
// справочнику улиц Москвы, сид справочников и импорт XLSX-классификатора.
//
// Всё, что читает фронт, живёт в памяти (Catalog, atomic-снимок) и отдаётся
// предкодированными байтами с ETag: справочники меняются только сидом/импортом,
// а читаются на каждой карточке.
package classifier

import (
	"embed"
	"encoding/json"
	"fmt"
	"sync"

	"lct/gocore/internal/gen/public"
)

//go:embed data/*.json
var dataFS embed.FS

// ---------------------------------------------------------------- формы данных сида

// seedService — служба из data/services.json (frontend fixtures/services.ts).
type seedService struct {
	Code      string `json:"code"`
	ShortName string `json:"shortName"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
}

// seedType — тип происшествия из data/classifier.json (frontend fixtures/classifier.ts).
type seedType struct {
	Code       string                  `json:"code"`
	FixtureID  string                  `json:"fixtureId"`
	Name       string                  `json:"name"`
	Depth      int                     `json:"depth"`
	Parent     string                  `json:"parent,omitempty"` // код родителя (в фикстурах нет)
	Synonyms   []string                `json:"synonyms"`
	Reference  string                  `json:"reference,omitempty"`
	Attributes []public.AttributeGroup `json:"attributes"`
}

// seedRule — правило определения служб (порядок = порядок RULES во фронтовой фикстуре).
type seedRule struct {
	Type     string            `json:"type"`
	Attr     string            `json:"attr,omitempty"`
	AnyOf    []string          `json:"anyOf,omitempty"`
	Services []ruleServiceJSON `json:"services"`
}

type seedClassifier struct {
	Types       []seedType `json:"types"`
	Frequent    []string   `json:"frequent"`
	Significant []string   `json:"significant"`
	Rules       []seedRule `json:"rules"`
}

// ruleJSON — элемент classifier_categories.service_rules (snake_case, DESIGN §4):
// правило без attr срабатывает на сам тип; с attr — если значение признака входит в any_of.
type ruleJSON struct {
	Attr     string            `json:"attr,omitempty"`
	AnyOf    []string          `json:"any_of,omitempty"`
	Services []ruleServiceJSON `json:"services"`
}

type ruleServiceJSON struct {
	Code    string `json:"code"`
	Primary bool   `json:"primary"`
	Reason  string `json:"reason"`
}

// ---------------------------------------------------------------- загрузка (однократно)

type embedded struct {
	services    []seedService
	classifier  seedClassifier
	fieldLabels map[string]string
	streets     *streetIndex
	// fixtureCodes — коды типов из фикстур: импорт XLSX их не трогает.
	fixtureCodes map[string]struct{}
	// serviceOrder — порядок служб фикстуры (сортировка /services как во фронте).
	serviceOrder map[string]int
}

var (
	embOnce sync.Once
	emb     *embedded
)

// data — встроенные справочники; ошибка разбора — ошибка сборки, поэтому паника
// (проявится на первом же запуске/тесте, а не у пользователя).
func data() *embedded {
	embOnce.Do(func() {
		e := &embedded{}
		mustJSON("data/services.json", &e.services)
		mustJSON("data/classifier.json", &e.classifier)
		mustJSON("data/field_labels.json", &e.fieldLabels)
		var sf streetsFile
		mustJSON("data/streets.json", &sf)
		e.streets = buildStreetIndex(&sf)
		e.fixtureCodes = make(map[string]struct{}, len(e.classifier.Types))
		for _, t := range e.classifier.Types {
			e.fixtureCodes[t.Code] = struct{}{}
		}
		e.serviceOrder = make(map[string]int, len(e.services))
		for i, s := range e.services {
			e.serviceOrder[s.Code] = i
		}
		emb = e
	})
	return emb
}

func mustJSON(name string, dst any) {
	b, err := dataFS.ReadFile(name)
	if err != nil {
		panic(fmt.Sprintf("classifier: embedded %s: %v", name, err))
	}
	if err := json.Unmarshal(b, dst); err != nil {
		panic(fmt.Sprintf("classifier: parse %s: %v", name, err))
	}
}

// FieldLabels — подписи полей карточки (FIELD_LABELS фронта), копия.
func FieldLabels() map[string]string {
	src := data().fieldLabels
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
