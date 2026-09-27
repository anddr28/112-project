package reports

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/core"
	"lct/gocore/internal/dds"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/pg"
)

// maxRows — потолок строк отчёта (защита памяти). Реальный максимум ТЗ ×10: 200 обучающихся ×
// десятки карточек — на порядок меньше.
const maxRows = 20000

// errLessonNotFound — занятия нет (404).
var errLessonNotFound = errors.New("reports: lesson not found")

// lessonMeta — шапка отчёта.
type lessonMeta struct {
	ID           uuid.UUID
	Kind         string
	Title        string
	TeacherID    *uuid.UUID
	CreatedBy    uuid.UUID
	Mode         string
	TimeLimitSec int
	Settings     model.LessonSettings
	Status       string
	StartedAt    *time.Time
	FinishedAt   *time.Time
	CreatedAt    time.Time
	Teacher      string   // «Фамилия И. О.» владельца (у практики — автор)
	Scenarios    []string // пул сценариев в порядке выдачи, «Название (версия N)»
	Participants int
}

// reportRow — строка отчёта: попытка обучающегося, либо участник без выданной карточки
// (Attempt == false) — преподавателю важно видеть и тех, кто до карточки не дошёл.
type reportRow struct {
	UserID   uuid.UUID
	FullName string // «Фамилия Имя Отчество»
	Short    string // «Фамилия И. О.»

	Attempt        bool
	AttemptID      uuid.UUID
	SeqNo          int
	IncidentNo     int64
	Status         string
	TimeLimitSec   int
	CallAcceptedAt *time.Time
	FirstInputAt   *time.Time
	SubmittedAt    *time.Time
	SpentMs        *int
	ReplayCount    int
	DialogueTurns  int
	Scenario       string // с версией, если она > 1
	Category       string

	EvalStatus    string // "" — оценки нет
	Fields        *float64
	Grammar       *float64
	Semantic      *float64
	Timing        *float64
	Dialogue      *float64
	Total         *float64
	Final         *float64 // COALESCE(override, total) — колонка final_score
	Verdict       string   // эффективный: COALESCE(override_verdict, verdict)
	Overridden    bool
	OverrideRsn   string
	NeedsReview   bool
	AIUnavailable bool
	WithinNorm    *bool
	Errors        []string // ошибки карточки, самые весомые первыми: «Подпись (не заполнено)»

	DDS *ddsProtocol // ракурс «Диспетчер ДДС»; у попыток оператора 112 — nil
}

// ddsProtocol — протокол реагирования попытки ракурса «Диспетчер ДДС» (attempts.service_id
// задан): те же данные, что преподаватель видит в разборе попытки — статусы своей службы,
// ожидание эталона (etalons.scoring.reaction), текст действия и замечания оценки.
type ddsProtocol struct {
	ServiceName  string // services.name — служба обучающегося
	ServiceShort string // краткое имя своей службы (карточка, затем справочник); нет — ServiceName
	Status       string // текущий статус своей службы
	History      []public.ReactionStatusEntry
	ActionText   string
	Expected     model.ReactionExpectation // с умолчаниями («Принята» за 30 с)
	Evaluated    bool                      // оценка есть — замечания посчитаны
	Remarks      []ddsRemark
}

// ddsRemark — замечание по протоколу (evaluations.field_errors, слой dds_reaction).
type ddsRemark struct {
	Label, Kind, Expected, Actual string
}

// Evaluated — итог окончательный: все слои доехали или балл выставил преподаватель.
func (r *reportRow) Evaluated() bool {
	return r.Attempt && (r.EvalStatus == core.EvalDone || r.Overridden) && r.Final != nil
}

// ReactionMs — время реакции (первое содержательное действие после принятия вызова).
func (r *reportRow) ReactionMs() *int {
	if r.CallAcceptedAt == nil || r.FirstInputAt == nil {
		return nil
	}
	ms := int(r.FirstInputAt.Sub(*r.CallAcceptedAt) / time.Millisecond)
	if ms < 0 {
		ms = 0
	}
	return &ms
}

// Шапка: владелец, пул сценариев (по sort_order, как их выдаёт IssueNext), число участников.
const sqlMeta = `
SELECT l.id, l.kind, l.title, l.teacher_id, l.created_by, l.mode, l.time_limit_sec, l.settings, l.status,
       l.started_at, l.finished_at, l.created_at,
       o.last_name, o.first_name, o.middle_name,
       ARRAY(SELECT s.title FROM lesson_scenarios ls JOIN scenarios s ON s.id = ls.scenario_id
              WHERE ls.lesson_id = l.id ORDER BY ls.sort_order, ls.scenario_id),
       ARRAY(SELECT s.version FROM lesson_scenarios ls JOIN scenarios s ON s.id = ls.scenario_id
              WHERE ls.lesson_id = l.id ORDER BY ls.sort_order, ls.scenario_id),
       (SELECT count(*) FROM lesson_participants p WHERE p.lesson_id = l.id)
  FROM lessons l
  LEFT JOIN users o ON o.id = COALESCE(l.teacher_id, l.created_by)
 WHERE l.id = $1`

// Протокол ракурса ДДС — только попытки со службой обучающегося (attempts.service_id; у 112 —
// NULL, поэтому отчёт оператора 112 этого набора не получает). Карточка — сданная, иначе черновик.
const sqlDDS = `
SELECT a.id, sv.code, sv.name, COALESCE(sv.short_name, ''), COALESCE(a.card, d.data), a.action_text, et.scoring -> 'reaction', e.field_errors
  FROM attempts a
  JOIN services sv ON sv.id = a.service_id
  LEFT JOIN attempt_drafts d ON d.attempt_id = a.id
  LEFT JOIN etalons et ON et.id = a.etalon_id
  LEFT JOIN evaluations e ON e.attempt_id = a.id
 WHERE a.lesson_id = $1 AND a.service_id IS NOT NULL
 LIMIT $2`

// Все строки отчёта одним запросом. Состав людей — участники занятия ∪ владельцы попыток
// (у самостоятельной практики участников может не быть). Попытки — по уникальному индексу
// (lesson_id, user_id, seq_no); оценка — по evaluations.attempt_id (UNIQUE). Ошибки карточки
// разворачиваются из jsonb в SQL: в Go едут только подписи, без разбора всего field_errors.
// Порядок ошибок — по весу поля, затем как их выдал слой 1.
const sqlRows = `
WITH m AS (
  SELECT p.user_id FROM lesson_participants p WHERE p.lesson_id = $1
  UNION
  SELECT a.user_id FROM attempts a WHERE a.lesson_id = $1
)
SELECT u.id, u.last_name, u.first_name, u.middle_name,
       a.id, a.seq_no, a.incident_no, a.status, a.time_limit_sec,
       a.call_accepted_at, a.first_input_at, a.submitted_at, a.time_spent_ms, a.replay_count, a.dialogue_turns,
       s.title, s.version, c.name,
       e.status, e.fields_score::float8, e.grammar_score::float8, e.semantic_score::float8,
       e.timing_score::float8, e.dialogue_score::float8, e.total_score::float8, e.final_score::float8,
       COALESCE(e.override_verdict, e.verdict), e.override_score IS NOT NULL, e.override_reason,
       COALESCE(e.needs_review, false), COALESCE(e.ai_unavailable, false),
       CASE WHEN jsonb_typeof(e.timing -> 'within_norm') = 'boolean' THEN (e.timing ->> 'within_norm')::boolean END,
       fe.labels, fe.kinds
  FROM m
  JOIN users u ON u.id = m.user_id
  LEFT JOIN attempts a ON a.lesson_id = $1 AND a.user_id = m.user_id
  LEFT JOIN scenarios s ON s.id = a.scenario_id
  LEFT JOIN classifier_categories c ON c.id = s.category_id
  LEFT JOIN evaluations e ON e.attempt_id = a.id
  LEFT JOIN LATERAL (
    SELECT array_agg(x.label ORDER BY x.w DESC, x.ord) AS labels,
           array_agg(x.kind ORDER BY x.w DESC, x.ord) AS kinds
      FROM (SELECT COALESCE(NULLIF(el ->> 'label', ''), el ->> 'field', '') AS label,
                   COALESCE(el ->> 'kind', '') AS kind,
                   CASE WHEN jsonb_typeof(el -> 'weight') = 'number' THEN (el ->> 'weight')::float8 ELSE 0 END AS w,
                   t.ord
              FROM jsonb_array_elements(CASE WHEN jsonb_typeof(e.field_errors) = 'array'
                                             THEN e.field_errors ELSE '[]'::jsonb END)
                   WITH ORDINALITY AS t(el, ord)) x
  ) fe ON true
 ORDER BY u.last_name, u.first_name, u.middle_name, u.id, a.seq_no NULLS FIRST
 LIMIT $2`

// loadReport — шапка и строки за один round-trip (pgx.Batch). authorize вызывается сразу после
// шапки, до чтения строк: чужое занятие не материализуется в памяти. Соединение освобождается
// до рендеринга — медленный клиент не держит соединение пула.
func loadReport(ctx context.Context, q pg.Querier, lessonID uuid.UUID,
	authorize func(*lessonMeta) error) (*lessonMeta, []reportRow, bool, error) {
	b := &pgx.Batch{}
	b.Queue(sqlMeta, lessonID)
	b.Queue(sqlRows, lessonID, maxRows+1)
	b.Queue(sqlDDS, lessonID, maxRows+1)
	br := q.SendBatch(ctx, b)
	defer br.Close()

	meta, err := scanMeta(br.QueryRow())
	if err != nil {
		if pg.IsNoRows(err) {
			return nil, nil, false, errLessonNotFound
		}
		return nil, nil, false, fmt.Errorf("reports: lesson meta: %w", err)
	}
	if err := authorize(meta); err != nil {
		return nil, nil, false, err
	}

	out, err := readRows(br, meta.Participants)
	if err != nil {
		return nil, nil, false, err
	}
	protocols, err := readDDS(br)
	if err != nil {
		return nil, nil, false, err
	}
	truncated := len(out) > maxRows
	if truncated {
		out = out[:maxRows]
	}
	for i := range out {
		if out[i].Attempt {
			out[i].DDS = protocols[out[i].AttemptID]
		}
	}
	return meta, out, truncated, nil
}

// readRows — строки отчёта (результат batch закрывается до чтения следующего).
func readRows(br pgx.BatchResults, participants int) ([]reportRow, error) {
	rows, err := br.Query()
	if err != nil {
		return nil, fmt.Errorf("reports: rows: %w", err)
	}
	defer rows.Close()
	out := make([]reportRow, 0, max(participants, 16))
	for rows.Next() {
		out = append(out, reportRow{})
		if err := scanRow(rows, &out[len(out)-1]); err != nil {
			return nil, fmt.Errorf("reports: scan row: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reports: rows: %w", err)
	}
	return out, nil
}

// readDDS — протоколы ракурса ДДС по попыткам (пусто у занятий оператора 112). Битый JSON
// карточки или эталона не роняет отчёт: протокол остаётся без этих частей.
func readDDS(br pgx.BatchResults) (map[uuid.UUID]*ddsProtocol, error) {
	rows, err := br.Query()
	if err != nil {
		return nil, fmt.Errorf("reports: dds: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]*ddsProtocol{}
	for rows.Next() {
		var (
			id                        uuid.UUID
			code, name, short         string
			cardRaw, reactionRaw, fes []byte
			action                    *string
		)
		if err := rows.Scan(&id, &code, &name, &short, &cardRaw, &action, &reactionRaw, &fes); err != nil {
			return nil, fmt.Errorf("reports: scan dds: %w", err)
		}
		out[id] = newDDSProtocol(code, name, short, cardRaw, deref(action), reactionRaw, fes)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reports: dds: %w", err)
	}
	return out, nil
}

func newDDSProtocol(code, name, short string, cardRaw []byte, action string, reactionRaw, fieldErrors []byte) *ddsProtocol {
	p := &ddsProtocol{ServiceName: name, ServiceShort: orText(short, name), ActionText: strings.TrimSpace(action)}
	var card public.IncidentCardDraft
	if len(cardRaw) > 0 && json.Unmarshal(cardRaw, &card) == nil {
		if sv := dds.FindService(&card, code); sv != nil {
			p.Status, p.History = string(sv.CurrentStatus), sv.History
			if sv.ShortName != "" {
				p.ServiceShort = sv.ShortName
			}
		}
		if p.ActionText == "" {
			p.ActionText = strings.TrimSpace(card.ActionsTaken)
		}
	}
	var reaction *model.ReactionExpectation
	if len(reactionRaw) > 0 {
		reaction = new(model.ReactionExpectation)
		if json.Unmarshal(reactionRaw, reaction) != nil {
			reaction = nil
		}
	}
	p.Expected = reaction.Resolved()
	if fieldErrors != nil {
		p.Evaluated = true
		var fe []public.FieldError
		_ = json.Unmarshal(fieldErrors, &fe)
		for _, e := range fe {
			label := e.Label
			if label == "" {
				label = e.Field
			}
			p.Remarks = append(p.Remarks, ddsRemark{Label: label, Kind: string(e.Kind),
				Expected: remarkValue(e.Expected), Actual: remarkValue(e.Actual)})
		}
	}
	return p
}

// remarkValue — значение «ожидалось/факт» замечания как текст (как renderValue в UI).
func remarkValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "да"
		}
		return "нет"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			if s := remarkValue(e); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	}
	return ""
}

func scanMeta(row pgx.Row) (*lessonMeta, error) {
	var (
		m                   lessonMeta
		settingsRaw         []byte
		last, first, middle *string
		titles              []string
		versions            []int32
	)
	if err := row.Scan(&m.ID, &m.Kind, &m.Title, &m.TeacherID, &m.CreatedBy, &m.Mode, &m.TimeLimitSec,
		&settingsRaw, &m.Status, &m.StartedAt, &m.FinishedAt, &m.CreatedAt,
		&last, &first, &middle, &titles, &versions, &m.Participants); err != nil {
		return nil, err
	}
	// Битые настройки не мешают отчёту: ParseLessonSettings всегда отдаёт валидный результат.
	m.Settings, _ = model.ParseLessonSettings(settingsRaw, model.DefaultLessonSettings(nil))
	if last != nil {
		m.Teacher = core.ShortName(*last, deref(first), deref(middle))
	}
	m.Scenarios = make([]string, len(titles))
	for i, t := range titles {
		v := 1
		if i < len(versions) {
			v = int(versions[i])
		}
		m.Scenarios[i] = withVersion(t, v)
	}
	m.StartedAt, m.FinishedAt = utcPtr(m.StartedAt), utcPtr(m.FinishedAt)
	m.CreatedAt = m.CreatedAt.UTC()
	return &m, nil
}

func scanRow(rows pgx.Rows, r *reportRow) error {
	var (
		last, first     string
		middle          *string
		attemptID       *uuid.UUID
		seqNo, limitSec *int
		incidentNo      *int64
		status          *string
		spent           *int
		replays, turns  *int
		title, category *string
		version         *int
		evalStatus      *string
		verdict         *string
		overrideRsn     *string
		labels, kinds   []string
	)
	if err := rows.Scan(&r.UserID, &last, &first, &middle,
		&attemptID, &seqNo, &incidentNo, &status, &limitSec,
		&r.CallAcceptedAt, &r.FirstInputAt, &r.SubmittedAt, &spent, &replays, &turns,
		&title, &version, &category,
		&evalStatus, &r.Fields, &r.Grammar, &r.Semantic, &r.Timing, &r.Dialogue, &r.Total, &r.Final,
		&verdict, &r.Overridden, &overrideRsn, &r.NeedsReview, &r.AIUnavailable, &r.WithinNorm,
		&labels, &kinds); err != nil {
		return err
	}
	r.FullName = fullName(last, first, deref(middle))
	r.Short = core.ShortName(last, first, deref(middle))
	if attemptID == nil {
		return nil
	}
	r.Attempt = true
	r.AttemptID = *attemptID
	r.SeqNo = derefInt(seqNo)
	if incidentNo != nil {
		r.IncidentNo = *incidentNo
	}
	r.Status = deref(status)
	r.TimeLimitSec = derefInt(limitSec)
	r.CallAcceptedAt, r.FirstInputAt, r.SubmittedAt = utcPtr(r.CallAcceptedAt), utcPtr(r.FirstInputAt), utcPtr(r.SubmittedAt)
	r.SpentMs = spent
	r.ReplayCount = derefInt(replays)
	r.DialogueTurns = derefInt(turns)
	if title != nil {
		r.Scenario = withVersion(*title, derefInt(version))
	}
	r.Category = deref(category)
	r.EvalStatus = deref(evalStatus)
	r.Verdict = deref(verdict)
	r.OverrideRsn = deref(overrideRsn)
	// Баллы — целые (UI и отчёт рассчитаны на целые; в БД numeric(5,2)).
	for _, p := range []*float64{r.Fields, r.Grammar, r.Semantic, r.Timing, r.Dialogue, r.Total, r.Final} {
		if p != nil {
			*p = roundScore(*p)
		}
	}
	if len(labels) > 0 {
		r.Errors = make([]string, 0, len(labels))
		for i, l := range labels {
			if l == "" {
				continue // элемент без подписи и имени поля (не объект) — пустое «; » в отчёте не нужно
			}
			k := ""
			if i < len(kinds) {
				k = kinds[i]
			}
			r.Errors = append(r.Errors, errorText(l, k))
		}
	}
	return nil
}

// ---------------------------------------------------------------- сводка

// layerKeys — слои в порядке, в котором их показывает UI преподавателя.
var layerKeys = [...]string{core.LayerFields, core.LayerSemantic, core.LayerGrammar, core.LayerTiming, core.LayerDialogue}

// summary — агрегаты по занятию (считаются в Go по уже загруженным строкам: второй проход
// по БД ради пары средних не нужен).
type summary struct {
	Participants int // людей в отчёте
	Attempts     int // выданных карточек
	NotIssued    int // участников без карточки
	InWork       int // issued | in_progress
	Submitted    int // сдано (submitted | evaluating | evaluated)
	Evaluated    int // итог окончательный
	Expired      int // expired | aborted
	Passed       int
	NeedsReview  int
	Overridden   int

	AvgFinal    *float64
	PassRate    *float64 // % от оценённых
	WithinNorm  int
	TimingKnown int
	WithinRate  *float64 // % от попыток с известным таймингом
	AvgSpentMs  *float64
	AvgLayers   [len(layerKeys)]*float64
}

// studentSummary — итоги обучающегося (лист «Сводка»).
type studentSummary struct {
	Name        string
	Attempts    int
	Evaluated   int
	Passed      int
	AvgFinal    *float64
	WithinNorm  int
	TimingKnown int
}

func summarize(rows []reportRow) (summary, []studentSummary) {
	var (
		s                  summary
		sumFinal, sumSpent float64
		nSpent             int
		layerSum           [len(layerKeys)]float64
		layerN             [len(layerKeys)]int
		students           []studentSummary
		stSum              float64
		prev               uuid.UUID
		cur                *studentSummary
	)
	flush := func() {
		if cur != nil && cur.Evaluated > 0 {
			avg := roundScore(stSum / float64(cur.Evaluated))
			cur.AvgFinal = &avg
		}
	}
	for i := range rows {
		r := &rows[i]
		if cur == nil || r.UserID != prev {
			flush()
			students = append(students, studentSummary{Name: r.FullName})
			cur, prev, stSum = &students[len(students)-1], r.UserID, 0
		}
		if !r.Attempt {
			s.NotIssued++
			continue
		}
		s.Attempts++
		cur.Attempts++
		switch r.Status {
		case core.AttemptIssued, core.AttemptInProgress:
			s.InWork++
		case core.AttemptSubmitted, core.AttemptEvaluating, core.AttemptEvaluated:
			s.Submitted++
		case core.AttemptExpired, core.AttemptAborted:
			s.Expired++
		}
		if r.NeedsReview {
			s.NeedsReview++
		}
		if r.Overridden {
			s.Overridden++
		}
		if r.Evaluated() {
			s.Evaluated++
			cur.Evaluated++
			sumFinal += *r.Final
			stSum += *r.Final
			if r.Verdict == core.VerdictPass {
				s.Passed++
				cur.Passed++
			}
		}
		if r.WithinNorm != nil {
			s.TimingKnown++
			cur.TimingKnown++
			if *r.WithinNorm {
				s.WithinNorm++
				cur.WithinNorm++
			}
		}
		if r.SpentMs != nil {
			sumSpent += float64(*r.SpentMs)
			nSpent++
		}
		for j, p := range [...]*float64{r.Fields, r.Semantic, r.Grammar, r.Timing, r.Dialogue} {
			if p != nil {
				layerSum[j] += *p
				layerN[j]++
			}
		}
	}
	flush()
	s.Participants = len(students)

	if s.Evaluated > 0 {
		avg := roundScore(sumFinal / float64(s.Evaluated))
		rate := roundScore(100 * float64(s.Passed) / float64(s.Evaluated))
		s.AvgFinal, s.PassRate = &avg, &rate
	}
	if s.TimingKnown > 0 {
		rate := roundScore(100 * float64(s.WithinNorm) / float64(s.TimingKnown))
		s.WithinRate = &rate
	}
	if nSpent > 0 {
		avg := sumSpent / float64(nSpent)
		s.AvgSpentMs = &avg
	}
	for j := range layerKeys {
		if layerN[j] > 0 {
			avg := roundScore(layerSum[j] / float64(layerN[j]))
			s.AvgLayers[j] = &avg
		}
	}
	if students == nil {
		students = []studentSummary{}
	}
	return s, students
}

// roundScore — целые баллы/проценты (правило проекта: UI рассчитан на целые).
func roundScore(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	v := math.Round(x)
	if v == 0 {
		return 0 // без «-0»
	}
	return v
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// utcPtr — pgx отдаёт timestamptz в time.Local; в отчёте время переводится в зону отчёта явно.
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
