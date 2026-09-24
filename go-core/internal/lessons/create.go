package lessons

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/scoring"
	"lct/gocore/internal/settings"
	"lct/gocore/internal/store"
)

// Границы входа занятия. Пулы и списки участников ограничены, чтобы один запрос не мог
// раздуть память/транзакцию (ТЗ ×10: 200 одновременных обучающихся).
const (
	minTitleRunes   = 3
	maxTitleRunes   = 200
	minTimeLimitSec = 10
	maxTimeLimitSec = 3600
	maxScenarios    = 100
	maxParticipants = 500
	minMaxTurns     = 2
	maxMaxTurns     = 40
	maxCardsPerStud = 50
)

// ---------------------------------------------------------------- GET /lessons/default-settings

func (s *Service) defaultSettings(w http.ResponseWriter, r *http.Request) error {
	ls := newLessonSettings(s.snapshot(r.Context()))
	httpx.WriteJSON(w, http.StatusOK, convert.LessonSettingsToPublic(ls))
	return nil
}

// newLessonSettings — настройки нового занятия по умолчанию. Веса отдаются фактическими
// (нормированы к 1; если голос включён по умолчанию — с долей разговора): форма создания
// показывает ровно те проценты, по которым потом считается итог.
func newLessonSettings(snap *settings.Snapshot) model.LessonSettings {
	ls := model.DefaultLessonSettings(snap)
	ls.Weights = wholePercents(scoring.EffectiveWeights(ls.Weights, ls.Voice.Enabled, snap.DialogueWeightDefault))
	return ls
}

// wholePercents — веса целыми процентами с суммой ровно 100 (метод наибольших остатков).
// Форма создания занятия показывает Math.round(w·100) по каждому слою и не пускает дальше,
// пока сумма не 100%: пропорциональное ужатие под голос (0.375/0.1875/…) дало бы 101%.
// Вход — нормированные веса (сумма 1).
func wholePercents(w settings.Weights) settings.Weights {
	vals := [5]float64{w.Fields, w.Semantic, w.Grammar, w.Timing, w.Dialogue}
	var (
		pct   [5]int
		rem   [5]float64
		total int
	)
	for i, v := range vals {
		p := v * 100
		f := math.Floor(p + 1e-9) // 0.29·100 = 28.999999999999996 — это 29
		pct[i], rem[i] = int(f), p-f
		total += pct[i]
	}
	for ; total < 100; total++ {
		best := 0
		for i := range rem {
			if rem[i] > rem[best] {
				best = i
			}
		}
		if rem[best] <= 0 {
			break // вход не нормирован — оставляем как есть
		}
		pct[best]++
		rem[best] = -1
	}
	return settings.Weights{
		Fields: float64(pct[0]) / 100, Semantic: float64(pct[1]) / 100, Grammar: float64(pct[2]) / 100,
		Timing: float64(pct[3]) / 100, Dialogue: float64(pct[4]) / 100,
	}
}

// effectiveWeights — веса, которые пишутся в lessons.settings: перенормировка под голос
// (scoring.EffectiveWeights, та же логика, что в оценке) и округление до 4 знаков, чтобы в
// JSON не жили 0.35000000000000003.
func effectiveWeights(w settings.Weights, voice bool, dialogueDefault float64) settings.Weights {
	return scoring.RoundWeights(scoring.EffectiveWeights(w, voice, dialogueDefault))
}

// ---------------------------------------------------------------- POST /lessons

// createBody — CreateLessonInput. Разбирается в свою структуру (а не public.CreateLessonInput):
// кривой UUID в списке должен дать ошибку по полю, а не «Некорректный JSON».
// cardsPerStudent — необязательное расширение (в контракте нет; по умолчанию — из settings).
type createBody struct {
	Title           *string             `json:"title"`
	Mode            *string             `json:"mode"`
	Perspective     *string             `json:"perspective"`
	Difficulty      *int                `json:"difficulty"`
	TimeLimitSec    *int                `json:"timeLimitSec"`
	ScenarioIDs     []string            `json:"scenarioIds"`
	ParticipantIDs  []string            `json:"participantIds"`
	PassThreshold   *float64            `json:"passThreshold"`
	AllowReplay     *bool               `json:"allowReplay"`
	Weights         map[string]*float64 `json:"weights"`
	Voice           *voiceBody          `json:"voice"`
	CardsPerStudent *int                `json:"cardsPerStudent"`
}

type voiceBody struct {
	Enabled    *bool   `json:"enabled"`
	Input      *string `json:"input"`
	PushToTalk *bool   `json:"pushToTalk"`
	MaxTurns   *int    `json:"maxTurns"`
	TtsEnabled *bool   `json:"ttsEnabled"`
}

// lessonDraft — проверенный вход занятия.
type lessonDraft struct {
	Title          string
	Mode           string
	Difficulty     *int
	TimeLimitSec   int
	ScenarioIDs    []uuid.UUID
	ParticipantIDs []uuid.UUID
	Settings       model.LessonSettings
}

func (s *Service) create(w http.ResponseWriter, r *http.Request) error {
	p, err := principal(r)
	if err != nil {
		return err
	}
	var body createBody
	if err := httpx.ReadJSON(r, &body); err != nil {
		return err
	}
	ctx := r.Context()
	d, fields := parseCreate(&body, s.snapshot(ctx))
	if len(fields) > 0 {
		return httpx.Validation("Проверьте параметры занятия", fields)
	}

	// Видимость участников — как в GET /users: преподаватель — только «своих», админ — всех.
	var scope *uuid.UUID
	if p.Role != core.RoleAdmin {
		scope = &p.UserID
	}
	scen, users, err := loadRefs(ctx, s.pool, d.ScenarioIDs, d.ParticipantIDs, scope)
	if err != nil {
		return httpx.Internal(err)
	}
	if herr := checkRefs(&d, scen, users); herr != nil {
		return herr
	}

	in := lessonInsert{
		ID: ids.New(), TeacherID: p.UserID, Title: d.Title, Mode: d.Mode, Status: core.LessonDraft,
		Difficulty: d.Difficulty, TimeLimitSec: d.TimeLimitSec, Settings: d.Settings,
		ScenarioIDs: d.ScenarioIDs, ParticipantIDs: d.ParticipantIDs,
	}
	// Один оператор (CTE): занятие, пул, участники и категории появляются атомарно без
	// явной транзакции — на один round-trip меньше.
	if err := insertLesson(ctx, s.pool, &in); err != nil {
		return httpx.Internal(err)
	}
	row, err := store.GetLesson(ctx, s.pool, in.ID)
	if err != nil {
		return httpx.Internal(err)
	}
	out := store.LessonToPublic(row)
	s.logAudit(ctx, core.AuditEntry{
		Action: "lesson.create", EntityType: "lesson", EntityID: in.ID, LessonID: in.ID,
		After: lessonAudit{
			Title: d.Title, Mode: d.Mode, Status: core.LessonDraft, TimeLimitSec: d.TimeLimitSec,
			Scenarios: len(d.ScenarioIDs), Participants: len(d.ParticipantIDs),
			Voice: d.Settings.Voice.Enabled, PassThreshold: d.Settings.PassThreshold,
		},
	})
	httpx.WriteJSON(w, http.StatusCreated, out)
	return nil
}

// lessonAudit — компактный снимок занятия для audit_log (без ФИО участников).
type lessonAudit struct {
	Title           string  `json:"title,omitempty"`
	Mode            string  `json:"mode,omitempty"`
	Status          string  `json:"status,omitempty"`
	TimeLimitSec    int     `json:"timeLimitSec,omitempty"`
	Scenarios       int     `json:"scenarios,omitempty"`
	Participants    int     `json:"participants,omitempty"`
	Voice           bool    `json:"voice,omitempty"`
	PassThreshold   float64 `json:"passThreshold,omitempty"`
	AttemptsIssued  int     `json:"attemptsIssued,omitempty"`
	AttemptsExpired int     `json:"attemptsExpired,omitempty"`
}

// parseCreate — проверка формы запроса (без БД). Отсутствующие необязательные по смыслу
// поля (perspective, timeLimitSec, passThreshold, allowReplay, voice, weights) берутся из
// настроек платформы — старый клиент без них всё равно получит рабочее занятие.
func parseCreate(b *createBody, snap *settings.Snapshot) (lessonDraft, map[string]string) {
	fields := map[string]string{}
	var d lessonDraft

	if b.Title == nil {
		fields["title"] = "Укажите название занятия"
	} else {
		d.Title = strings.TrimSpace(*b.Title)
		switch n := utf8.RuneCountInString(d.Title); {
		case n < minTitleRunes:
			fields["title"] = "Название — не короче 3 символов"
		case n > maxTitleRunes:
			fields["title"] = "Название — не длиннее 200 символов"
		}
	}

	switch m := deref(b.Mode); m {
	case core.ModeCards, core.ModeCardActions:
		d.Mode = m
	case "":
		fields["mode"] = "Укажите режим занятия"
	default:
		fields["mode"] = "Допустимо: cards, card_actions"
	}

	ls := model.DefaultLessonSettings(snap)
	switch v := deref(b.Perspective); v {
	case "", model.PerspectiveOperator112:
		ls.Perspective = model.PerspectiveOperator112
	case model.PerspectiveDDS:
		ls.Perspective = model.PerspectiveDDS
	default:
		fields["perspective"] = "Допустимо: operator112, dds"
	}

	if b.Difficulty != nil {
		if *b.Difficulty < 1 || *b.Difficulty > 3 {
			fields["difficulty"] = "Сложность — от 1 до 3"
		} else {
			v := *b.Difficulty
			d.Difficulty = &v
		}
	}

	switch {
	case b.TimeLimitSec == nil:
		// Норматив не передан — из настроек платформы (settings.time_limit_sec, ТЗ: 30 с;
		// администратор меняет его в /admin/settings). Значение там уже проверено на
		// 10..3600, но зажимаем ещё раз: битая строка settings не должна ронять создание.
		d.TimeLimitSec = min(max(snap.TimeLimitSec, minTimeLimitSec), maxTimeLimitSec)
	case *b.TimeLimitSec < minTimeLimitSec || *b.TimeLimitSec > maxTimeLimitSec:
		fields["timeLimitSec"] = "Норматив — от 10 до 3600 секунд"
	default:
		d.TimeLimitSec = *b.TimeLimitSec
	}

	d.ScenarioIDs = parseIDs("scenarioIds", b.ScenarioIDs, maxScenarios, "Выберите хотя бы один сценарий", fields)
	d.ParticipantIDs = parseIDs("participantIds", b.ParticipantIDs, maxParticipants, "Выберите хотя бы одного участника", fields)

	if b.PassThreshold != nil {
		if v := *b.PassThreshold; math.IsNaN(v) || v < 0 || v > 100 {
			fields["passThreshold"] = "Порог — от 0 до 100"
		} else {
			ls.PassThreshold = v
		}
	}
	if b.AllowReplay != nil {
		ls.AllowReplay = *b.AllowReplay
	}
	if b.CardsPerStudent != nil {
		if v := *b.CardsPerStudent; v < 1 || v > maxCardsPerStud {
			fields["cardsPerStudent"] = "Карточек на обучающегося — от 1 до 50"
		} else {
			ls.CardsPerStudent = v
		}
	}

	if v := b.Voice; v != nil {
		if v.Enabled != nil {
			ls.Voice.Enabled = *v.Enabled
		}
		if v.Input != nil {
			switch *v.Input {
			case model.VoiceInputVoice, model.VoiceInputText, model.VoiceInputBoth:
				ls.Voice.Input = *v.Input
			default:
				fields["voice.input"] = "Допустимо: voice, text, both"
			}
		}
		if v.PushToTalk != nil {
			ls.Voice.PushToTalk = *v.PushToTalk
		}
		if v.MaxTurns != nil {
			if *v.MaxTurns < minMaxTurns || *v.MaxTurns > maxMaxTurns {
				fields["voice.maxTurns"] = "Реплик оператора — от 2 до 40"
			} else {
				ls.Voice.MaxTurns = *v.MaxTurns
			}
		}
		if v.TtsEnabled != nil {
			ls.Voice.TTSEnabled = *v.TtsEnabled
		}
	}

	// Веса: переданные слои поверх весов платформы, затем фактические (голос выкл —
	// разговор 0 и перенормировка). Долю разговора по умолчанию (dialogue_weight_default)
	// подставляем, только если преподаватель вес разговора не передавал (как
	// withDialogueWeight во фронте: `input.weights ?? withDialogueWeight(base)`): явный
	// dialogue = 0 при включённом голосе — осознанный выбор «разговор не оценивать».
	wts := snap.ScoreWeights
	dialogueDefault := snap.DialogueWeightDefault
	for k, pv := range b.Weights {
		if pv == nil {
			continue
		}
		v := *pv
		if k == core.LayerDialogue {
			dialogueDefault = 0 // вес разговора задан явно — только нормировка
		}
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			fields["weights."+k] = "Вес — неотрицательное число"
			continue
		}
		switch k {
		case core.LayerFields:
			wts.Fields = v
		case core.LayerSemantic:
			wts.Semantic = v
		case core.LayerGrammar:
			wts.Grammar = v
		case core.LayerTiming:
			wts.Timing = v
		case core.LayerDialogue:
			wts.Dialogue = v
		default:
			fields["weights."+k] = "Неизвестный слой оценки"
		}
	}
	if len(b.Weights) > 0 {
		sum := wts.Fields + wts.Semantic + wts.Grammar + wts.Timing
		if ls.Voice.Enabled {
			sum += wts.Dialogue
		}
		if !(sum > 0) {
			fields["weights"] = "Сумма весов слоёв должна быть больше нуля"
		}
	}
	ls.Weights = effectiveWeights(wts, ls.Voice.Enabled, dialogueDefault)

	d.Settings = ls
	return d, fields
}

// parseIDs — список UUID без повторов (порядок сохраняется: для пула сценариев он значим).
func parseIDs(name string, raw []string, limit int, emptyMsg string, fields map[string]string) []uuid.UUID {
	if len(raw) == 0 {
		fields[name] = emptyMsg
		return nil
	}
	if len(raw) > limit {
		fields[name] = "Слишком длинный список: не больше " + strconv.Itoa(limit)
		return nil
	}
	out := make([]uuid.UUID, 0, len(raw))
	seen := make(map[uuid.UUID]struct{}, len(raw))
	for i, s := range raw {
		id, ok := ids.Parse(strings.TrimSpace(s))
		if !ok {
			fields[name+"["+strconv.Itoa(i)+"]"] = "Некорректный идентификатор"
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// ---------------------------------------------------------------- проверка ссылок

// scenarioCheck — то, что нужно знать о сценарии, чтобы поставить его в занятие.
type scenarioCheck struct {
	ID         uuid.UUID
	Title      string
	Status     string
	Mode       string
	Archived   bool
	HasEtalon  bool
	Facts      int // call_script.dialogue.facts
	Checklist  int // etalons.expected_dialogue.checklist (текущей версии)
	HasBrief   bool
	CategoryID uuid.UUID
}

// Размеры массивов считаются в SQL: большой call_script/эталон не едет в Go целиком.
const sqlScenarioChecksCols = `
SELECT s.id, s.title, s.status, s.mode, s.category_id,
       (s.archived_at IS NOT NULL OR s.status = 'archived'),
       e.id IS NOT NULL,
       jsonb_typeof(s.call_script->'dialogue') = 'object',
       CASE WHEN jsonb_typeof(s.call_script->'dialogue'->'facts') = 'array'
            THEN jsonb_array_length(s.call_script->'dialogue'->'facts') ELSE 0 END,
       CASE WHEN jsonb_typeof(e.expected_dialogue->'checklist') = 'array'
            THEN jsonb_array_length(e.expected_dialogue->'checklist') ELSE 0 END
  FROM scenarios s
  LEFT JOIN etalons e ON e.scenario_id = s.id AND e.is_current`

const sqlScenarioChecks = sqlScenarioChecksCols + `
 WHERE s.id = ANY($1::uuid[])`

// userCheck — участник занятия.
type userCheck struct {
	ID      uuid.UUID
	Role    string
	Status  string
	Name    string // «Фамилия И. О.» — для сообщения об ошибке
	Visible bool   // преподаватель видит этого обучающегося в GET /users (см. sqlUserChecks)
}

// sqlUserChecks — участники и их видимость преподавателю $2 (NULL — админ, видно всех).
// Предикат видимости — тот же, что у списка обучающихся преподавателя (users.listSQL):
// активный обучающийся без неархивной группы, обучающийся его неархивной группы или
// бывший участник его занятий. Иначе преподаватель мог бы вписать в своё занятие чужого
// студента по UUID и через участие получить доступ к его прогрессу (users.progressAccessSQL).
const sqlUserChecks = `
SELECT u.id, u.role, u.status, u.last_name, u.first_name, COALESCE(u.middle_name, ''),
       ($2::uuid IS NULL OR (u.role = 'student' AND (
            (u.status = 'active' AND NOT EXISTS (SELECT 1 FROM group_members gm JOIN groups g ON g.id = gm.group_id
                         WHERE gm.user_id = u.id AND g.archived_at IS NULL))
         OR EXISTS (SELECT 1 FROM group_members gm JOIN groups g ON g.id = gm.group_id
                     WHERE gm.user_id = u.id AND g.teacher_id = $2 AND g.archived_at IS NULL)
         OR EXISTS (SELECT 1 FROM lesson_participants lp JOIN lessons l ON l.id = lp.lesson_id
                     WHERE lp.user_id = u.id AND l.teacher_id = $2))))
  FROM users u
 WHERE u.id = ANY($1::uuid[]) AND u.deleted_at IS NULL`

func scanScenarioCheck(row pgx.Row, c *scenarioCheck) error {
	var hasBrief *bool
	if err := row.Scan(&c.ID, &c.Title, &c.Status, &c.Mode, &c.CategoryID, &c.Archived, &c.HasEtalon,
		&hasBrief, &c.Facts, &c.Checklist); err != nil {
		return err
	}
	c.HasBrief = hasBrief != nil && *hasBrief
	return nil
}

// loadRefs — сценарии и участники одним пакетом (один round-trip). teacherScope — чью
// видимость обучающихся применять (nil — админ: видно всех).
func loadRefs(ctx context.Context, q pg.Querier, scenarioIDs, userIDs []uuid.UUID, teacherScope *uuid.UUID) (map[uuid.UUID]scenarioCheck, map[uuid.UUID]userCheck, error) {
	b := &pgx.Batch{}
	b.Queue(sqlScenarioChecks, scenarioIDs)
	b.Queue(sqlUserChecks, userIDs, teacherScope)
	br := q.SendBatch(ctx, b)
	defer br.Close()

	scen := make(map[uuid.UUID]scenarioCheck, len(scenarioIDs))
	rows, err := br.Query()
	if err != nil {
		return nil, nil, fmt.Errorf("lessons: scenario checks: %w", err)
	}
	for rows.Next() {
		var c scenarioCheck
		if err := scanScenarioCheck(rows, &c); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("lessons: scenario checks: %w", err)
		}
		scen[c.ID] = c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("lessons: scenario checks: %w", err)
	}

	users := make(map[uuid.UUID]userCheck, len(userIDs))
	rows, err = br.Query()
	if err != nil {
		return nil, nil, fmt.Errorf("lessons: participant checks: %w", err)
	}
	for rows.Next() {
		var (
			u                   userCheck
			last, first, middle string
		)
		if err := rows.Scan(&u.ID, &u.Role, &u.Status, &last, &first, &middle, &u.Visible); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("lessons: participant checks: %w", err)
		}
		u.Name = core.ShortName(last, first, middle)
		users[u.ID] = u
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("lessons: participant checks: %w", err)
	}
	return scen, users, nil
}

// checkRefs — сценарии существуют, подтверждены, подходят режиму (и голосу); участники —
// активные обучающиеся. Ошибки — по-русски, с перечнем того, что не так.
func checkRefs(d *lessonDraft, scen map[uuid.UUID]scenarioCheck, users map[uuid.UUID]userCheck) *httpx.Error {
	var missing []string
	for _, id := range d.ScenarioIDs {
		if _, ok := scen[id]; !ok {
			missing = append(missing, id.String())
		}
	}
	if len(missing) > 0 {
		return httpx.Validation("Сценарии не найдены", map[string]string{"scenarioIds": "Неизвестные сценарии: " + strings.Join(missing, ", ")}).
			WithDetails(map[string]any{"scenarioIds": missing})
	}

	var bad, badTitles []string
	for _, id := range d.ScenarioIDs {
		if c := scen[id]; c.Status != core.ScenarioValidated || c.Archived || !c.HasEtalon {
			bad, badTitles = append(bad, id.String()), append(badTitles, "«"+c.Title+"»")
		}
	}
	if len(bad) > 0 {
		return httpx.Validation("В занятие можно включить только подтверждённые сценарии с эталоном: "+strings.Join(badTitles, ", "),
			map[string]string{"scenarioIds": "Есть неподтверждённые сценарии"}).
			WithDetails(map[string]any{"scenarioIds": bad})
	}

	bad, badTitles = bad[:0], badTitles[:0]
	for _, id := range d.ScenarioIDs {
		if c := scen[id]; c.Mode != d.Mode && c.Mode != core.ModeBoth {
			bad, badTitles = append(bad, id.String()), append(badTitles, "«"+c.Title+"»")
		}
	}
	if len(bad) > 0 {
		return httpx.Validation("Сценарии не подходят для режима занятия: "+strings.Join(badTitles, ", "),
			map[string]string{"scenarioIds": "Режим сценария не совпадает с режимом занятия"}).
			WithDetails(map[string]any{"scenarioIds": bad})
	}

	if d.Settings.Voice.Enabled {
		bad, badTitles = bad[:0], badTitles[:0]
		for _, id := range d.ScenarioIDs {
			if c := scen[id]; !voiceReady(&c) {
				bad, badTitles = append(bad, id.String()), append(badTitles, c.Title)
			}
		}
		if len(bad) > 0 {
			return httpx.Unprocessable("Для голосового занятия нужны бриф заявителя и чек-лист разговора. Не заполнены: " + strings.Join(badTitles, ", ")).
				WithDetails(map[string]any{"scenarioIds": bad, "scenarios": badTitles})
		}
	}

	var unknown, notStudent, blocked []string
	for _, id := range d.ParticipantIDs {
		u, ok := users[id]
		switch {
		case !ok || !u.Visible:
			// Невидимый преподавателю — как несуществующий: ни ФИО, ни роль, ни статус
			// чужого пользователя в ответ не попадают.
			unknown = append(unknown, id.String())
		case u.Role != string(core.RoleStudent):
			notStudent = append(notStudent, id.String())
		case u.Status != "active":
			blocked = append(blocked, u.Name)
		}
	}
	switch {
	case len(unknown) > 0:
		return httpx.Validation("Неизвестные участники", map[string]string{"participantIds": "Не найдены: " + strings.Join(unknown, ", ")}).
			WithDetails(map[string]any{"participantIds": unknown})
	case len(notStudent) > 0:
		return httpx.Validation("Участником занятия может быть только обучающийся", map[string]string{"participantIds": "Есть учётные записи не обучающихся"}).
			WithDetails(map[string]any{"participantIds": notStudent})
	case len(blocked) > 0:
		return httpx.Validation("Учётные записи заблокированы: "+strings.Join(blocked, ", "), map[string]string{"participantIds": "Есть заблокированные участники"})
	}
	return nil
}

// voiceReady — сценарий пригоден для разговора с ИИ-заявителем: есть бриф с фактами
// (заявителю есть что отвечать) и чек-лист протокола (разговор есть чем оценить).
func voiceReady(c *scenarioCheck) bool { return c.HasBrief && c.Facts > 0 && c.Checklist > 0 }

// ---------------------------------------------------------------- запись занятия

// lessonInsert — новое занятие (create и демо-сид).
type lessonInsert struct {
	ID             uuid.UUID
	TeacherID      uuid.UUID
	Title          string
	Mode           string
	Status         string // draft (create) | running (сид)
	Difficulty     *int
	TimeLimitSec   int
	Settings       model.LessonSettings
	StartedAt      *time.Time
	ScenarioIDs    []uuid.UUID // порядок = sort_order
	ParticipantIDs []uuid.UUID
}

// Занятие, пул (sort_order = порядок во входе), участники и категории (различные категории
// сценариев пула) — одним оператором. Внешние ключи дочерних таблиц проверяются в конце
// оператора, когда строка lessons уже вставлена CTE.
const sqlInsertLesson = `
WITH l AS (
  INSERT INTO lessons (id, kind, title, teacher_id, created_by, mode, difficulty, time_limit_sec,
                       settings, status, started_at)
  VALUES ($1, 'class', $2, $3, $3, $4, $5, $6, $7, $8, $9)
  RETURNING id
), sc AS (
  INSERT INTO lesson_scenarios (lesson_id, scenario_id, sort_order)
  SELECT $1, t.sid, (t.ord - 1)::int FROM unnest($10::uuid[]) WITH ORDINALITY AS t(sid, ord)
), pt AS (
  INSERT INTO lesson_participants (lesson_id, user_id)
  SELECT $1, u FROM unnest($11::uuid[]) AS u
), cat AS (
  INSERT INTO lesson_categories (lesson_id, category_id)
  SELECT DISTINCT $1::uuid, s.category_id FROM scenarios s WHERE s.id = ANY($10::uuid[])
)
SELECT id FROM l`

func insertLesson(ctx context.Context, q pg.Querier, in *lessonInsert) error {
	raw, err := json.Marshal(in.Settings)
	if err != nil {
		return fmt.Errorf("lessons: settings: %w", err)
	}
	var id uuid.UUID
	if err := q.QueryRow(ctx, sqlInsertLesson, in.ID, in.Title, in.TeacherID, in.Mode, in.Difficulty,
		in.TimeLimitSec, raw, in.Status, in.StartedAt, in.ScenarioIDs, in.ParticipantIDs).Scan(&id); err != nil {
		return fmt.Errorf("lessons: insert: %w", err)
	}
	return nil
}
