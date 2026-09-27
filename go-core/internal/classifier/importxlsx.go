package classifier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xuri/excelize/v2"

	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
)

// Импорт реального классификатора организаторов (ТЗ, опционально: «механизм импорта
// обновлений в ручном режиме»).
//
// Раскладка листа (Классификатор_происшествий_v_046_24_корректировка_МВД_+_Департамент.xlsx,
// «Лист1», 1310 строк × 104 колонки), определяется по заголовкам, а не по буквам колонок:
//
//	строка 1 (блоки): A «Генерация номера», G «Блок, как происшествие отображается…»,
//	    M «Главная служба», N… — блоки служб (объединённые ячейки): «Классификатор МЧС»,
//	    «Классификатор МВД», «Классификатор СМП», «Классификатор МОСГАЗ», «ЦЭМП», «ФСБ», …,
//	    «Мосводоканал», «Мослифт», «ЦОДД», «Деп. ЖКХ», «Департамент РБиПК (ГКУ МОСБЕЗ)», …
//	строка 2 (заголовки): A «Г», B «п1», C «п2», D «п3», E «Номер»,
//	    F «Группа происшествий…», G «112 - Признак.1 (тип происшествия)», H «112-Признак.2»,
//	    I «112-Признак.3», J «Дополнительные признаки…», K «Итоговый тип происшествия»,
//	    L «ТИП происшествия ЕКП 35»; в блоках служб — подслужбы («Служба 101», «ОДС ПСЦ»,
//	    «МГПСС») или условия («выбран признак Правонарушение», «… Пострадавшие»).
//	строка 3 (условия): «признак не выбран», «газификация», «угроза людям», «пострадавшие/погибшие»,
//	    «мед. помощь», «треб. Эвакуация», «перекрытие движение», «(выбран признак НД - НЕТ ДОСТУПА)», …
//	далее: строки разделов (Г пуст, «Номер» = номер раздела 1..23, F — название раздела;
//	    раздел «БПЛА» — без номера, берётся из Г следующих строк) и строки типов
//	    (Г, п1, п2, п3 — числа; Номер = Г·10⁶ + п1·10⁴ + п2·10² + п3).
//	Ячейка службы: текст (как происшествие называется у службы, «карточка-112») — служба
//	    оповещается; «нет реагирования» или пусто — нет.
//
// Иерархия в БД: раздел (depth 1, code = номер раздела) -> категория п1 (depth 2,
// code = Г·10⁶ + п1·10⁴; если такой строки в листе нет — синтетический узел с названием из
// «Признак.1», extra.synthetic) -> тип (depth 3). Название типа — «Итоговый тип происшествия».
// Строки «Не отображается оператору 112» импортируются неактивными (is_active=false).
// Службы: базовые колонки -> правило без условия, колонки с условием -> правило с attr
// (опросная карта типа строится из этих условий); «Главная служба» -> primary.
// Остальные колонки — в extra как есть. Типы из фикстур (сид) импорт не трогает; строки
// прошлого импорта, которых нет в файле, выключаются (не удаляются: на них ссылаются сценарии).

// ImportStats — итог импорта (import_batches.stats).
type ImportStats struct {
	BatchID       uuid.UUID `json:"-"`
	RowsTotal     int       `json:"rows_total"`
	Nodes         int       `json:"nodes"`
	Inserted      int       `json:"inserted"`
	Updated       int       `json:"updated"`
	Skipped       int       `json:"skipped"`
	Deactivated   int       `json:"deactivated"`
	ServicesAdded int       `json:"services_added"`
	Errors        []string  `json:"errors"`
}

const maxImportErrors = 100

func (s *ImportStats) errorf(format string, args ...any) {
	if len(s.Errors) < maxImportErrors {
		s.Errors = append(s.Errors, fmt.Sprintf(format, args...))
	}
}

// ImportClassifierXLSX импортирует XLSX-классификатор: запись import_batches (running ->
// done|failed со статистикой), upsert classifier_categories по code с иерархией, недостающие
// службы. by — пользователь-инициатор (import_batches.created_by). После импорта вызовите
// Catalog.Reload (или его подхватит Catalog.Run).
func ImportClassifierXLSX(ctx context.Context, pool *pgxpool.Pool, path string, by uuid.UUID) (ImportStats, error) {
	stats := ImportStats{BatchID: ids.New(), Errors: []string{}}
	if _, err := pool.Exec(ctx,
		`INSERT INTO import_batches (id, kind, file_name, status, created_by) VALUES ($1, 'classifier', $2, 'running', $3)`,
		stats.BatchID, filepath.Base(path), by); err != nil {
		return stats, fmt.Errorf("classifier import: batch: %w", err)
	}

	err := importXLSX(ctx, pool, path, &stats)

	status, errText := "done", ""
	if err != nil {
		status, errText = "failed", err.Error()
	}
	statsJSON, _ := json.Marshal(stats)
	fctx := context.WithoutCancel(ctx) // итог пишем даже при отмене — иначе батч навсегда «running»
	if _, uerr := pool.Exec(fctx,
		`UPDATE import_batches SET status = $2, stats = $3, error = NULLIF($4, ''), finished_at = now() WHERE id = $1`,
		stats.BatchID, status, statsJSON, errText); uerr != nil && err == nil {
		err = fmt.Errorf("classifier import: finish batch: %w", uerr)
	}
	return stats, err
}

func importXLSX(ctx context.Context, pool *pgxpool.Pool, path string, stats *ImportStats) error {
	rows, err := readSheet(path)
	if err != nil {
		return err
	}
	parsed, err := parseClassifierSheet(rows, stats)
	if err != nil {
		return err
	}
	stats.Nodes = len(parsed.nodes)
	return pg.WithTx(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		return importTx(ctx, tx, parsed, stats)
	})
}

// importTx — запись разобранного листа в открытой транзакции.
func importTx(ctx context.Context, tx pg.Querier, parsed *xlsxParsed, stats *ImportStats) error {
	// службы: недостающие добавляются, существующие не переименовываются (их ведёт сид/админ)
	b := &pgx.Batch{}
	for _, s := range parsed.services {
		b.Queue(`INSERT INTO services (code, name, short_name, kind) VALUES ($1, $2, $3, 'city') ON CONFLICT (code) DO NOTHING`,
			s.code, s.name, s.short)
	}
	br := tx.SendBatch(ctx, b)
	for range parsed.services {
		tag, err := br.Exec()
		if err != nil {
			_ = br.Close()
			return fmt.Errorf("classifier import: services: %w", err)
		}
		stats.ServicesAdded += int(tag.RowsAffected())
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("classifier import: services: %w", err)
	}

	b = &pgx.Batch{}
	for i := range parsed.nodes {
		n := &parsed.nodes[i]
		b.Queue(qImportType, n.code, n.parent, n.name, n.depth, n.servicesJSON, n.attrsJSON, n.active,
			n.extraJSON, n.synonyms, n.sortOrder, n.rulesJSON)
	}
	br = tx.SendBatch(ctx, b)
	for i := range parsed.nodes {
		var inserted bool
		err := br.QueryRow().Scan(&inserted)
		switch {
		case pg.IsNoRows(err):
			stats.Skipped++ // без изменений или защищённый тип из фикстур
		case err != nil:
			_ = br.Close()
			return fmt.Errorf("classifier import: type %s: %w", parsed.nodes[i].code, err)
		case inserted:
			stats.Inserted++
		default:
			stats.Updated++
		}
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("classifier import: types: %w", err)
	}

	codes := make([]string, len(parsed.nodes))
	for i := range parsed.nodes {
		codes[i] = parsed.nodes[i].code
	}
	tag, err := tx.Exec(ctx, `
UPDATE classifier_categories SET is_active = false
 WHERE extra->>'source' = 'xlsx' AND is_active AND NOT (code = ANY($1))`, codes)
	if err != nil {
		return fmt.Errorf("classifier import: deactivate: %w", err)
	}
	stats.Deactivated = int(tag.RowsAffected())
	return nil
}

// qImportType — upsert типа из XLSX. Строки сида (extra.source = fixture) не трогаются;
// неизменённые не переписываются. RETURNING (xmax = 0) — вставка или обновление.
const qImportType = `
INSERT INTO classifier_categories
       (code, parent_id, name, depth, services, attributes, is_active, extra, synonyms, sort_order, service_rules)
VALUES ($1, (SELECT id FROM classifier_categories WHERE code = NULLIF($2, '')), $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (code) DO UPDATE
   SET parent_id = EXCLUDED.parent_id, name = EXCLUDED.name, depth = EXCLUDED.depth,
       services = EXCLUDED.services, attributes = EXCLUDED.attributes, is_active = EXCLUDED.is_active,
       extra = EXCLUDED.extra, synonyms = EXCLUDED.synonyms, sort_order = EXCLUDED.sort_order,
       service_rules = EXCLUDED.service_rules
 WHERE COALESCE(classifier_categories.extra->>'source', '') <> 'fixture'
   AND NOT (classifier_categories.extra ? 'fixture_id')
   AND (classifier_categories.parent_id, classifier_categories.name, classifier_categories.depth,
        classifier_categories.services, classifier_categories.attributes, classifier_categories.is_active,
        classifier_categories.extra, classifier_categories.synonyms, classifier_categories.sort_order,
        classifier_categories.service_rules)
       IS DISTINCT FROM
       (EXCLUDED.parent_id, EXCLUDED.name, EXCLUDED.depth, EXCLUDED.services, EXCLUDED.attributes,
        EXCLUDED.is_active, EXCLUDED.extra, EXCLUDED.synonyms, EXCLUDED.sort_order, EXCLUDED.service_rules)
RETURNING (xmax = 0)`

// readSheet — строки первого листа, где есть заголовок «Номер» (сырые значения ячеек:
// числа без форматирования, объединённые ячейки — значение только в левой верхней).
func readSheet(path string) ([][]string, error) {
	f, err := excelize.OpenFile(path, excelize.Options{RawCellValue: true})
	if err != nil {
		return nil, fmt.Errorf("classifier import: открыть %s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return nil, errors.New("classifier import: в файле нет листов")
	}
	for _, sh := range sheets {
		rows, err := f.GetRows(sh, excelize.Options{RawCellValue: true})
		if err != nil {
			return nil, fmt.Errorf("classifier import: лист %s: %w", sh, err)
		}
		if findHeaderRow(rows) >= 0 {
			return rows, nil
		}
	}
	return nil, errors.New("classifier import: не найден лист с заголовками «Г», «Номер»")
}

// ---------------------------------------------------------------- разбор листа (чистая часть)

type xlsxService struct{ code, name, short string }

type xlsxNode struct {
	code, parent, name string
	depth              int16
	active             bool
	synonyms           []string
	sortOrder          int32
	servicesJSON       []byte
	attrsJSON          []byte
	rulesJSON          []byte
	extraJSON          []byte
}

type xlsxParsed struct {
	nodes    []xlsxNode
	services []xlsxService
}

type sheetCols struct {
	g, p1, p2, p3, num, group, f1, f2, f3, extraSigns, final, ekp, main int
	svc                                                                 []svcCol
}

type svcCol struct {
	idx   int
	code  string
	title string // «блок / подслужба / условие» — ключ в extra.notify
	cond  *condition
}

// condition — условие колонки службы, переведённое в признак опросной карты.
type condition struct {
	attr, value, attrLabel, valueLabel, widget string
}

func findHeaderRow(rows [][]string) int {
	for i := 0; i < len(rows) && i < 12; i++ {
		hasNum, hasG := false, false
		for _, c := range rows[i] {
			switch normalize(c) {
			case "номер":
				hasNum = true
			case "г":
				hasG = true
			}
		}
		if hasNum && hasG {
			return i
		}
	}
	return -1
}

func cell(row []string, i int) string {
	if i < 0 || i >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[i])
}

// cleanText — пробелы и переводы строк схлопнуты (заголовки в файле с \n и хвостами).
func cleanText(s string) string { return strings.Join(strings.Fields(s), " ") }

func detectColumns(rows [][]string, h int) (sheetCols, error) {
	c := sheetCols{g: -1, p1: -1, p2: -1, p3: -1, num: -1, group: -1, f1: -1, f2: -1, f3: -1, extraSigns: -1, final: -1, ekp: -1, main: -1}
	head := rows[h]
	for i, v := range head {
		n := normalize(v)
		switch {
		case n == "г":
			c.g = i
		case n == "п1":
			c.p1 = i
		case n == "п2":
			c.p2 = i
		case n == "п3":
			c.p3 = i
		case n == "номер":
			c.num = i
		case strings.HasPrefix(n, "группа происшествий"):
			c.group = i
		case strings.Contains(n, "признак 1"):
			c.f1 = i
		case strings.Contains(n, "признак 2"):
			c.f2 = i
		case strings.Contains(n, "признак 3"):
			c.f3 = i
		case strings.HasPrefix(n, "дополнительные признаки"):
			c.extraSigns = i
		case strings.HasPrefix(n, "итоговый тип"):
			c.final = i
		case strings.Contains(n, "екп"):
			c.ekp = i
		}
	}
	if c.g < 0 || c.p1 < 0 || c.num < 0 {
		return c, errors.New("classifier import: нет колонок «Г», «п1», «Номер»")
	}
	var block, cond []string
	if h > 0 {
		block = rows[h-1]
	}
	if h+1 < len(rows) {
		cond = rows[h+1]
	}
	for i, v := range block {
		if normalize(v) == "главная служба" {
			c.main = i
		}
	}
	if c.main < 0 {
		// без «Главной службы» службы начинаются сразу после последней известной колонки
		c.main = max(c.g, c.p1, c.p2, c.p3, c.num, c.group, c.f1, c.f2, c.f3, c.extraSigns, c.final, c.ekp)
	}
	width := max(len(block), len(head), len(cond))
	curBlock, curSub := "", ""
	for i := c.main + 1; i < width; i++ {
		b, sub, cn := cleanText(cell(block, i)), cleanText(cell(head, i)), cleanText(cell(cond, i))
		if b != "" {
			curBlock, curSub = b, ""
		}
		if sub != "" {
			curSub = sub
		}
		if curBlock == "" && curSub == "" && cn == "" {
			continue
		}
		code := serviceCodeFor(curSub)
		if code == "" {
			code = serviceCodeFor(curBlock)
		}
		if code == "" {
			code = "xls_" + strings.ToLower(columnName(i))
		}
		condText := cn
		if condText == "" {
			condText = sub
		}
		title := strings.Trim(strings.Join([]string{curBlock, curSub, cn}, " / "), " /")
		c.svc = append(c.svc, svcCol{idx: i, code: code, title: title, cond: conditionFor(condText)})
	}
	return c, nil
}

func columnName(i int) string {
	name, err := excelize.ColumnNumberToName(i + 1)
	if err != nil {
		return strconv.Itoa(i + 1)
	}
	return name
}

// serviceNeedles — заголовок блока/подслужбы -> код службы. Сравнение по целым словам
// нормализованного текста, порядок важен («цукб бпла» раньше «цукб»).
var serviceNeedles = [...][2]string{
	{"мгпсс", "mgpss"}, {"служба 101", "101"}, {"одс псц", "101"}, {"мчс", "101"},
	{"мвд", "102"}, {"смп", "103"}, {"мособлгаз", "mosoblgaz"}, {"мосгаз", "104"}, {"цэмп", "cemp"}, {"фсб", "fsb"},
	{"автомобильные дороги ао", "avtodor_ao"}, {"автомобильные дороги", "avtodor"}, {"мосгортранс", "mosgortrans"},
	{"гор хозяйство", "gorkhoz"}, {"гормост", "gormost"}, {"канал имени москвы", "kim"}, {"мгтс", "mgts"},
	{"метро", "metro"}, {"мосводоканал", "vodokanal"}, {"моэск", "moesk"}, {"моэк", "moek"}, {"оэк", "oek"},
	{"мослифт", "moslift"}, {"цодд", "codd"}, {"жкх", "zhkh"}, {"мосбез", "mosbez"}, {"рбипк", "mosbez"},
	{"аппарат мэра", "mayor"}, {"москоллектор", "moskollektor"}, {"ржд", "rzd"},
	{"департамент образования", "dogm"}, {"центррегионводхоз", "crvh"}, {"военная комендатура", "voenkom"},
	{"оати", "oati"}, {"мосводосток", "mosvodostok"}, {"ппиоос", "dpioos"}, {"тсзн", "dtszn"}, {"рсво", "rsvo"},
	{"эважд", "evazhd"}, {"мсппн", "msppn"}, {"ритуал", "ritual"}, {"дту", "dtu"}, {"росгвардия", "rosgvard"},
	{"территориальные оив тинао", "oiv_tinao"}, {"территориальные оив", "oiv"},
	{"гражданского строительства", "dgs"}, {"департамент строительства", "dstroy"},
	{"ветеринарии", "vet"}, {"мосжилинспекция", "mzhi"}, {"департамент культуры", "dkult"}, {"глинки", "csa"},
	{"нту", "ntu"}, {"фсо", "fso"}, {"мср", "msr"}, {"туризму", "tourism"}, {"дгп", "dgp"},
	{"цукб бпла", "mo_bpla"}, {"цукб", "mo_cukb"}, {"организатор перевозок", "orgperevoz"},
	{"мосэкомониторинг", "mosecomon"}, {"рхбз", "mo_rhbz"}, {"ситиэнерго", "citienergo"},
}

func serviceCodeFor(text string) string {
	n := normalize(text)
	if n == "" {
		return ""
	}
	padded := " " + n + " "
	for _, nd := range serviceNeedles {
		if strings.Contains(padded, " "+nd[0]+" ") {
			return nd[1]
		}
	}
	return ""
}

// serviceNames — названия служб, которых нет в сиде (для INSERT … ON CONFLICT DO NOTHING).
var serviceNames = map[string][2]string{
	"101":          {"ГУ МЧС России по г. Москве, ГКУ «Пожарно-спасательный центр» ОДС", "Служба 101"},
	"102":          {"Полиция (СОДЧ МВД)", "Служба 102"},
	"103":          {"ГБУ города Москвы «Станция скорой и неотложной медицинской помощи им. А.С. Пучкова»", "Служба 103"},
	"104":          {"АО «МОСГАЗ», Диспетчерское управление", "Служба 104"},
	"mgpss":        {"ГКУ «Московская городская поисково-спасательная служба на водных объектах»", "МГПСС"},
	"cemp":         {"Центр экстренной медицинской помощи", "ЦЭМП"},
	"fsb":          {"Федеральная служба безопасности", "ФСБ"},
	"mosoblgaz":    {"АО «Мособлгаз»", "Мособлгаз"},
	"avtodor":      {"ГБУ «Автомобильные дороги»", "Автодороги"},
	"avtodor_ao":   {"ГБУ «Автомобильные дороги» административных округов", "Автодороги АО"},
	"mosgortrans":  {"ГУП «Мосгортранс»", "Мосгортранс"},
	"gorkhoz":      {"Городское хозяйство", "Гор. хозяйство"},
	"gormost":      {"ГБУ «Гормост»", "Гормост"},
	"kim":          {"ФГБУ «Канал имени Москвы»", "Канал им. Москвы"},
	"mgts":         {"ПАО «МГТС»", "МГТС"},
	"metro":        {"ГУП «Московский метрополитен»", "Метро"},
	"vodokanal":    {"АО «Мосводоканал»", "Мосводоканал"},
	"moesk":        {"ПАО «Россети Московский регион» (МОЭСК)", "МОЭСК"},
	"moek":         {"ПАО «МОЭК»", "МОЭК"},
	"oek":          {"АО «ОЭК» (электросети)", "ОЭК"},
	"moslift":      {"АО «Мослифт»", "Мослифт"},
	"codd":         {"Центр организации дорожного движения", "ЦОДД"},
	"zhkh":         {"Департамент ЖКХ / управляющие организации", "Деп. ЖКХ"},
	"mosbez":       {"ГКУ «Московский городской центр безопасности»", "Мос.Без."},
	"mayor":        {"Аппарат Мэра и Правительства Москвы", "Аппарат Мэра"},
	"moskollektor": {"ГУП «Москоллектор»", "Москоллектор"},
	"rzd":          {"Московская железная дорога — филиал ОАО «РЖД»", "РЖД"},
	"dogm":         {"Департамент образования и науки города Москвы", "ДОНМ"},
	"crvh":         {"ФГБВУ «Центррегионводхоз» (Московско-Окское БВУ)", "Центррегионводхоз"},
	"voenkom":      {"Военная комендатура", "Военная комендатура"},
	"oati":         {"Объединение административно-технических инспекций", "ОАТИ"},
	"mosvodostok":  {"ГУП «Мосводосток»", "Мосводосток"},
	"dpioos":       {"Департамент природопользования и охраны окружающей среды", "ДПиООС"},
	"dtszn":        {"Департамент труда и социальной защиты населения", "ДТСЗН"},
	"rsvo":         {"РСВО", "РСВО"},
	"evazhd":       {"ЭВАЖД", "ЭВАЖД"},
	"msppn":        {"ГБУ «Московская служба психологической помощи населению»", "МСППН"},
	"ritual":       {"ГБУ «Ритуал»", "Ритуал"},
	"dtu":          {"Департамент торговли и услуг", "ДТиУ"},
	"rosgvard":     {"Росгвардия", "Росгвардия"},
	"oiv":          {"Территориальные органы исполнительной власти", "Терр. ОИВ"},
	"oiv_tinao":    {"Территориальные органы исполнительной власти ТиНАО", "ОИВ ТиНАО"},
	"dstroy":       {"Департамент строительства города Москвы", "Депстрой"},
	"dgs":          {"Департамент гражданского строительства", "Деп. гражд. строительства"},
	"vet":          {"Комитет ветеринарии города Москвы", "Ветеринария"},
	"mzhi":         {"Мосжилинспекция", "МЖИ"},
	"dkult":        {"Департамент культуры города Москвы", "Деп. культуры"},
	"csa":          {"ГКУ ЦСА имени Е.П. Глинки", "ЦСА"},
	"ntu":          {"ГКУ НТУ", "НТУ"},
	"fso":          {"Федеральная служба охраны", "ФСО"},
	"msr":          {"ГУП МСР", "МСР"},
	"tourism":      {"Комитет по туризму города Москвы", "Комтуризм"},
	"dgp":          {"Департамент градостроительной политики", "ДГП"},
	"mo_cukb":      {"ЦУКБ Министерства обороны", "ЦУКБ МО"},
	"mo_bpla":      {"ЦУКБ Министерства обороны (БПЛА)", "ЦУКБ БПЛА"},
	"orgperevoz":   {"ГКУ «Организатор перевозок»", "Орг. перевозок"},
	"mosecomon":    {"ГПБУ «Мосэкомониторинг»", "Мосэкомониторинг"},
	"mo_rhbz":      {"Министерство обороны (РХБЗ)", "МО РХБЗ"},
	"citienergo":   {"ООО «Ситиэнерго»", "Ситиэнерго"},
}

// mainServiceCodes — «Главная служба» (латиница в файле) -> код службы.
var mainServiceCodes = map[string]string{
	"MCHS": "101", "POLICE": "102", "AMBULANCE": "103", "MOSGAZ": "104", "MOSLIFT": "moslift",
	"AUTOROADS": "avtodor", "MOSVODOCANAL": "vodokanal", "METRO": "metro", "OEK": "oek",
	"MOSGORTRANS": "mosgortrans", "MOESK": "moesk", "MOEK": "moek", "MZD": "rzd", "MGTS": "mgts",
	"MOSVODOSTOK": "mosvodostok", "MOSCOLLECTOR": "moskollektor", "GORMOST": "gormost", "GKH": "zhkh",
	"DEP.TSZN": "dtszn", "ZODD": "codd", "MSPPN": "msppn", "DEPECO": "dpioos", "ZEMP": "cemp", "МСР": "msr",
}

// conditionFor — текст условия колонки -> признак. nil — базовая колонка (без условия).
func conditionFor(text string) *condition {
	n := normalize(text)
	if n == "" || strings.Contains(n, "не выбран") || strings.Contains(n, "реагирование всегда") {
		return nil
	}
	padded := " " + n + " "
	has := func(s string) bool {
		return strings.Contains(padded, " "+s+" ") || strings.Contains(n, s) && len([]rune(s)) > 4
	}
	switch {
	case has("не на месте"):
		return &condition{"victims", "absent", "Есть пострадавшие", "Не на месте", "chips-single"}
	case has("нет доступа"):
		return &condition{"access", "no_access", "Доступ", "Нет доступа", "chips-multi"}
	case has("угроза людям"):
		return &condition{"people_threat", "yes", "Угроза людям", "Да", "bool"}
	case has("пострадавшие") || has("погибшие") || has("постр"):
		return &condition{"victims", "yes", "Есть пострадавшие", "Да", "bool"}
	case has("правонарушение"):
		return &condition{"offense", "yes", "Правонарушение", "Да", "bool"}
	case has("газификация"):
		return &condition{"gasified", "yes", "Проведена ли газификация", "Да", "bool"}
	case has("мед помощь"):
		return &condition{"medical_needed", "yes", "Требуется медицинская помощь", "Да", "bool"}
	case has("эвакуация"):
		return &condition{"evacuation", "yes", "Требуется эвакуация", "Да", "bool"}
	case has("5 чел"):
		return &condition{"mass_casualties", "yes", "Более 5 человек / ОД", "Да", "bool"}
	case has("перекрытие движение") || has("перекрытие движения"):
		return &condition{"traffic_block", "yes", "Есть ли перекрытие движения", "Да", "bool"}
	case has("тоннель"):
		return &condition{"object_kind", "tunnel", "Объект", "Тоннель", "chips-single"}
	case has("пеш"):
		return &condition{"object_kind", "pedestrian_bridge", "Объект", "Пешеходный мост", "chips-single"}
	case has("ав"):
		return &condition{"object_kind", "road_bridge", "Объект", "Автомобильный мост", "chips-single"}
	case has("на объектах связи"):
		return &condition{"object_kind", "comm_object", "Объект", "Объект связи", "chips-single"}
	case has("стройка"):
		return &condition{"object_kind", "construction", "Объект", "Стройка", "chips-single"}
	case has("объект из перечня"):
		return &condition{"object_kind", "listed_object", "Объект", "Объект из перечня", "chips-single"}
	}
	return nil
}

const hiddenMarker = "не отображается оператору 112"

// parseClassifierSheet — лист -> узлы классификатора (родители раньше детей) и службы.
func parseClassifierSheet(rows [][]string, stats *ImportStats) (*xlsxParsed, error) {
	h := findHeaderRow(rows)
	if h < 0 {
		return nil, errors.New("classifier import: не найдена строка заголовков («Г», «Номер»)")
	}
	cols, err := detectColumns(rows, h)
	if err != nil {
		return nil, err
	}
	fixture := data().fixtureCodes

	out := &xlsxParsed{}
	svcSeen := map[string]bool{}
	addService := func(code, title string) {
		if svcSeen[code] {
			return
		}
		svcSeen[code] = true
		name, short := title, title
		if nm, ok := serviceNames[code]; ok {
			name, short = nm[0], nm[1]
		}
		if len([]rune(short)) > 40 {
			short = string([]rune(short)[:40])
		}
		out.services = append(out.services, xlsxService{code: code, name: name, short: short})
	}
	for _, sc := range cols.svc {
		addService(sc.code, strings.SplitN(sc.title, " / ", 2)[0])
	}

	type section struct{ code, name string }
	sections := map[int]*section{}
	var pendingName string // раздел без номера («БПЛА») — номер из Г следующей строки
	nodeIdx := map[string]int{}
	var order int32 = 1000

	type catInfo struct {
		idx       int  // индекс узла категории в out.nodes (-1 — узел не создан)
		synthetic bool // узел создан по «Признак.1», строки категории в листе (пока) нет
		anyActive bool // есть активный тип в категории
	}
	cats := map[string]*catInfo{}

	emit := func(n xlsxNode) int {
		if _, dup := nodeIdx[n.code]; dup {
			stats.errorf("код %s повторяется — строка пропущена", n.code)
			stats.Skipped++
			return -1
		}
		if _, isFixture := fixture[n.code]; isFixture {
			stats.errorf("код %s совпадает с типом из встроенного справочника — строка пропущена", n.code)
			stats.Skipped++
			return -1
		}
		n.sortOrder = order
		order++
		nodeIdx[n.code] = len(out.nodes)
		out.nodes = append(out.nodes, n)
		return len(out.nodes) - 1
	}
	ensureSection := func(g int, rowNo int) *section {
		if s := sections[g]; s != nil {
			return s
		}
		name := pendingName
		if name == "" {
			name = "Раздел " + strconv.Itoa(g)
		}
		pendingName = ""
		s := &section{code: strconv.Itoa(g), name: name}
		sections[g] = s
		emit(xlsxNode{code: s.code, name: s.name, depth: 1, active: true, synonyms: []string{},
			servicesJSON: []byte("[]"), attrsJSON: []byte("[]"), rulesJSON: []byte("[]"),
			extraJSON: mustMarshal(map[string]any{"source": "xlsx", "row": rowNo, "kind": "section"})})
		return s
	}

	// h+1 — строка условий (пустые Г/Номер/F — пропускается как пустая строка данных)
	for ri := h + 1; ri < len(rows); ri++ {
		row := rows[ri]
		rowNo := ri + 1
		gs, num, grp := cell(row, cols.g), cell(row, cols.num), cleanText(cell(row, cols.group))
		if gs == "" {
			if num == "" && grp == "" {
				continue // пустая строка
			}
			stats.RowsTotal++
			// строка раздела: «Номер» = номер раздела, F — название
			if gnum, err := strconv.Atoi(num); err == nil && grp != "" {
				if sections[gnum] == nil {
					sections[gnum] = &section{code: strconv.Itoa(gnum), name: grp}
					emit(xlsxNode{code: strconv.Itoa(gnum), name: grp, depth: 1, active: true, synonyms: []string{},
						servicesJSON: []byte("[]"), attrsJSON: []byte("[]"), rulesJSON: []byte("[]"),
						extraJSON: mustMarshal(map[string]any{"source": "xlsx", "row": rowNo, "kind": "section"})})
				}
				continue
			}
			if grp != "" {
				pendingName = grp
				continue
			}
			stats.errorf("строка %d: не распознана (нет «Г» и названия раздела)", rowNo)
			stats.Skipped++
			continue
		}
		stats.RowsTotal++
		g, e1 := strconv.Atoi(gs)
		p1, e2 := strconv.Atoi(cell(row, cols.p1))
		p2, e3 := atoiOrZero(cell(row, cols.p2))
		p3, e4 := atoiOrZero(cell(row, cols.p3))
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
			stats.errorf("строка %d: «Г/п1/п2/п3» не числа", rowNo)
			stats.Skipped++
			continue
		}
		code := num
		if _, err := strconv.ParseInt(code, 10, 64); err != nil || code == "" {
			code = strconv.Itoa(g*1_000_000 + p1*10_000 + p2*100 + p3)
		}
		sec := ensureSection(g, rowNo)

		f1, f2, f3 := cleanText(cell(row, cols.f1)), cleanText(cell(row, cols.f2)), cleanText(cell(row, cols.f3))
		final := cleanText(cell(row, cols.final))
		hidden := normalize(f1) == hiddenMarker
		name := final
		if name == "" {
			name = strings.Join(nonEmpty(f1, f2, f3), " — ")
		}
		if name == "" {
			name = "Тип " + code
		}
		name = upperFirst(name)

		isCategory := p2 == 0 && p3 == 0
		catCode := strconv.Itoa(g*1_000_000 + p1*10_000)
		parent := sec.code
		depth := int16(2)
		if !isCategory {
			ci := cats[catCode]
			if ci == nil {
				// категории п1 в листе нет — синтетический узел с названием «Признак.1»
				catName := f1
				if catName == "" {
					catName = "Категория " + catCode
				}
				idx := emit(xlsxNode{code: catCode, parent: sec.code, name: upperFirst(catName), depth: 2, active: false,
					synonyms: []string{}, servicesJSON: []byte("[]"), attrsJSON: []byte("[]"), rulesJSON: []byte("[]"),
					extraJSON: mustMarshal(map[string]any{"source": "xlsx", "synthetic": true, "row": rowNo, "kind": "category"})})
				ci = &catInfo{idx: idx, synthetic: idx >= 0}
				cats[catCode] = ci
			}
			if !hidden {
				ci.anyActive = true
			}
			parent, depth = catCode, 3
		}

		rules, attrs, svcCodes, notify := rowServices(row, &cols, name)
		for _, c := range svcCodes {
			if !svcSeen[c] {
				addService(c, c)
			}
		}
		kind := "type"
		if isCategory {
			kind = "category"
		}
		extra := map[string]any{"source": "xlsx", "row": rowNo, "kind": kind, "g": g, "p1": p1, "p2": p2, "p3": p3}
		putNonEmpty(extra, "group", grp)
		putNonEmpty(extra, "priznak1", f1)
		putNonEmpty(extra, "priznak2", f2)
		putNonEmpty(extra, "priznak3", f3)
		putNonEmpty(extra, "extra_signs", cleanText(cell(row, cols.extraSigns)))
		putNonEmpty(extra, "final_type", final)
		putNonEmpty(extra, "ekp35", cleanText(cell(row, cols.ekp)))
		putNonEmpty(extra, "main_service", cleanText(cell(row, cols.main)))
		if len(notify) > 0 {
			extra["notify"] = notify
		}

		synonyms := synonymsFor(name, f1, f2, f3, cleanText(cell(row, cols.ekp)), grp)
		servicesJSON, _ := json.Marshal(svcCodes)
		attrsJSON := []byte("[]")
		if len(attrs) > 0 {
			attrsJSON = mustMarshal(attrs)
		}
		node := xlsxNode{code: code, parent: parent, name: name, depth: depth, active: !hidden, synonyms: synonyms,
			servicesJSON: servicesJSON, attrsJSON: attrsJSON, rulesJSON: mustMarshal(rules), extraJSON: mustMarshal(extra)}

		if isCategory && code == catCode {
			if ci := cats[catCode]; ci != nil && ci.synthetic {
				// синтетический узел уже создан раньше строки категории — заменяем данными строки
				node.sortOrder = out.nodes[ci.idx].sortOrder
				out.nodes[ci.idx] = node
				ci.synthetic = false
				ci.anyActive = ci.anyActive || !hidden
				continue
			}
			idx := emit(node)
			if idx >= 0 {
				cats[catCode] = &catInfo{idx: idx, anyActive: !hidden}
			}
			continue
		}
		emit(node)
	}
	// синтетическая категория активна, если активен хоть один её тип
	for _, ci := range cats {
		if ci.synthetic && ci.idx >= 0 {
			out.nodes[ci.idx].active = ci.anyActive
		}
	}
	return out, nil
}

func atoiOrZero(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.Atoi(s)
}

func nonEmpty(vals ...string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func putNonEmpty(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}

// synonymsFor — признаки и тип ЕКП как синонимы для поиска (Инструкция п.4.2: оператор
// ищет «мусор», «трава», а не «пожар: мусор»).
func synonymsFor(name string, vals ...string) []string {
	out := make([]string, 0, len(vals))
	seen := map[string]bool{normalize(name): true, hiddenMarker: true, "нет в екп": true}
	for _, v := range vals {
		n := normalize(v)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, strings.ToLower(cleanText(v)))
	}
	return out
}

// rowServices — правила служб, опросная карта (из условий колонок), коды служб и
// сырые значения колонок служб (extra.notify) для строки типа.
func rowServices(row []string, cols *sheetCols, typeName string) ([]ruleJSON, []public.AttributeGroup, []string, map[string]string) {
	primary := map[string]bool{}
	var primaryOrder []string // порядок из файла: JSON правил детерминирован (повторный импорт = «без изменений»)
	for part := range strings.SplitSeq(strings.ToUpper(cell(row, cols.main)), ",") {
		if code, ok := mainServiceCodes[strings.TrimSpace(part)]; ok && !primary[code] {
			primary[code] = true
			primaryOrder = append(primaryOrder, code)
		}
	}
	reasonBase := "тип происшествия: " + strings.ToLower(typeName)

	base := ruleJSON{Services: []ruleServiceJSON{}}
	inBase := map[string]int{}
	type condKey struct{ attr, value string }
	var condOrder []condKey
	condRules := map[condKey]*ruleJSON{}
	condMeta := map[condKey]*condition{}
	notify := map[string]string{}

	for i := range cols.svc {
		sc := &cols.svc[i]
		v := cleanText(cell(row, sc.idx))
		if v == "" {
			continue
		}
		notify[sc.title] = v
		if normalize(v) == "нет реагирования" {
			continue
		}
		if sc.cond == nil {
			if j, ok := inBase[sc.code]; ok {
				base.Services[j].Primary = base.Services[j].Primary || primary[sc.code]
				continue
			}
			inBase[sc.code] = len(base.Services)
			base.Services = append(base.Services, ruleServiceJSON{Code: sc.code, Primary: primary[sc.code], Reason: reasonBase})
			continue
		}
		k := condKey{sc.cond.attr, sc.cond.value}
		r := condRules[k]
		if r == nil {
			r = &ruleJSON{Attr: k.attr, AnyOf: []string{k.value}, Services: []ruleServiceJSON{}}
			condRules[k] = r
			condMeta[k] = sc.cond
			condOrder = append(condOrder, k)
		}
		dup := false
		for _, s := range r.Services {
			if s.Code == sc.code {
				dup = true
				break
			}
		}
		if !dup {
			r.Services = append(r.Services, ruleServiceJSON{Code: sc.code, Primary: false,
				Reason: "признак: " + strings.ToLower(sc.cond.attrLabel) + " — " + strings.ToLower(sc.cond.valueLabel)})
		}
	}
	// главная служба получает карточку всегда
	for _, code := range primaryOrder {
		if _, ok := inBase[code]; !ok {
			inBase[code] = len(base.Services)
			base.Services = append(base.Services, ruleServiceJSON{Code: code, Primary: true, Reason: reasonBase})
		}
	}

	rules := make([]ruleJSON, 0, 1+len(condOrder))
	if len(base.Services) > 0 {
		rules = append(rules, base)
	}
	for _, k := range condOrder {
		rules = append(rules, *condRules[k])
	}

	// опросная карта из условий: признак на attr, значения — из условий (+ «Нет» у да/нет)
	var attrs []public.AttributeGroup
	attrPos := map[string]int{}
	for _, k := range condOrder {
		m := condMeta[k]
		pos, ok := attrPos[m.attr]
		if !ok {
			g := public.AttributeGroup{Code: m.attr, Label: m.attrLabel, Widget: public.AttributeGroupWidget(m.widget),
				Order: len(attrs) + 1}
			opts := []public.AttributeOption{}
			if m.widget == "bool" || m.attr == "victims" {
				opts = append(opts, public.AttributeOption{Code: "yes", Label: "Да"}, public.AttributeOption{Code: "no", Label: "Нет"})
			}
			g.Options = &opts
			attrPos[m.attr] = len(attrs)
			attrs = append(attrs, g)
			pos = len(attrs) - 1
		}
		g := &attrs[pos]
		found := false
		for _, o := range *g.Options {
			if o.Code == m.value {
				found = true
				break
			}
		}
		if !found {
			*g.Options = append(*g.Options, public.AttributeOption{Code: m.value, Label: m.valueLabel})
		}
		// «пострадавшие» бывают да/нет и «не на месте» — это уже выбор из вариантов
		if m.attr == "victims" && len(*g.Options) > 2 {
			g.Widget = public.AttributeGroupWidgetChipsSingle
		}
	}
	codes := codesFromRules(rules)
	return rules, attrs, codes, notify
}
