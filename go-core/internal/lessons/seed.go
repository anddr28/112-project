package lessons

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/core"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/scoring"
	"lct/gocore/internal/settings"
)

// Сценарии демо-стенда (frontend/src/shared/mocks/fixtures/scenarios.ts) — по заголовку.
const (
	demoFireTitle  = "Пожар в квартире многоквартирного дома"
	demoGasTitle   = "Запах газа в подъезде жилого дома"
	demoWaterTitle = "Прорыв трубы с горячей водой во дворе"
)

// demoLesson — занятие демо-стенда (frontend/src/shared/mocks/db.ts) + голосовое занятие,
// чтобы разговор с ИИ-заявителем показывался «из коробки».
type demoLesson struct {
	title        string
	scenarios    []string // заголовки сценариев, порядок = пул
	participants []string // логины
	timeLimitSec int
	difficulty   int
	running      bool
	voice        bool
	dds          bool // ракурс «Диспетчер ДДС» (режим card_actions, без голоса)
}

var demoLessons = []demoLesson{
	{title: "Практическое занятие: пожары и запах газа", scenarios: []string{demoFireTitle, demoGasTitle},
		participants: []string{"student", "student2"}, timeLimitSec: 30, difficulty: 2, running: true},
	{title: "Аварии в городском хозяйстве", scenarios: []string{demoWaterTitle},
		participants: []string{"student"}, timeLimitSec: 45, difficulty: 1},
	// Разговор + карточка за 30 с не укладываются: норматив голосового занятия — 3 минуты.
	{title: "Голосовой приём вызова: пожар", scenarios: []string{demoFireTitle},
		participants: []string{"student2"}, timeLimitSec: 180, difficulty: 2, running: true, voice: true},
	// Ракурс ДДС: демо-обучающиеся — диспетчеры ДДС ЖКХ, служба есть в списке оповещения
	// всех трёх карточек. Норматив карточки — 2 минуты: решение за 30 с, затем статусы и
	// текст действия.
	{title: "Диспетчер ДДС ЖКХ: поступившие карточки", scenarios: []string{demoWaterTitle, demoGasTitle, demoFireTitle},
		participants: []string{"student", "student2"}, timeLimitSec: 120, difficulty: 1, running: true, dds: true},
}

const demoTeacherLogin = "teacher"

// Сценарий демо по заголовку: сид сценариев — исходный (без parent), подтверждённый;
// версии-копии с тем же заголовком идут после.
var sqlDemoScenarios = sqlScenarioChecksCols + `
 WHERE s.title = ANY($1::text[])
 ORDER BY s.title, (s.status = 'validated') DESC, (s.parent_id IS NULL) DESC, s.created_at, s.id`

const sqlDemoUsers = `
SELECT lower(login::text), id FROM users
 WHERE lower(login::text) = ANY($1::text[]) AND deleted_at IS NULL`

const sqlDemoLessonExists = `SELECT EXISTS (SELECT 1 FROM lessons WHERE title = $1 AND teacher_id = $2)`

// Сид не должен задвоиться при одновременном старте двух экземпляров.
const sqlSeedLock = `SELECT pg_advisory_xact_lock(hashtext('lct:seed:demo-lessons'))`

// SeedDemoLessons — демо-занятия (идемпотентно: по заголовку + преподавателю). Демо-учётки
// и демо-сценарии сидятся раньше; чего-то нет — занятие пропускается с предупреждением.
// Запущенные занятия сразу получают попытки через issuer (как при старте).
func SeedDemoLessons(ctx context.Context, d Deps, issuer core.AttemptIssuer) error {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	snap := settings.Defaults()
	if d.Settings != nil {
		snap = *d.Settings.Get(ctx)
	}
	return pg.WithTx(ctx, d.Pool, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, sqlSeedLock); err != nil {
			return fmt.Errorf("lessons: seed lock: %w", err)
		}
		users, err := demoUserIDs(ctx, tx)
		if err != nil {
			return err
		}
		teacherID, ok := users[demoTeacherLogin]
		if !ok {
			log.Warn("lessons: seed: демо-преподаватель не найден, демо-занятия пропущены", "login", demoTeacherLogin)
			return nil
		}
		scen, err := demoScenarios(ctx, tx)
		if err != nil {
			return err
		}
		for i := range demoLessons {
			if err := seedLesson(ctx, tx, log, &snap, issuer, &demoLessons[i], teacherID, users, scen); err != nil {
				return err
			}
		}
		return nil
	})
}

func seedLesson(ctx context.Context, tx pgx.Tx, log *slog.Logger, snap *settings.Snapshot, issuer core.AttemptIssuer,
	dl *demoLesson, teacherID uuid.UUID, users map[string]uuid.UUID, scen map[string]scenarioCheck) error {
	var exists bool
	if err := tx.QueryRow(ctx, sqlDemoLessonExists, dl.title, teacherID).Scan(&exists); err != nil {
		return fmt.Errorf("lessons: seed: exists: %w", err)
	}
	if exists {
		return nil
	}

	scenarioIDs := make([]uuid.UUID, 0, len(dl.scenarios))
	for _, title := range dl.scenarios {
		c, ok := scen[title]
		switch {
		case !ok:
			log.Warn("lessons: seed: демо-сценарий не найден, занятие пропущено", "lesson", dl.title, "scenario", title)
			return nil
		case c.Status != core.ScenarioValidated || c.Archived || !c.HasEtalon:
			log.Warn("lessons: seed: демо-сценарий не подтверждён или без эталона, занятие пропущено",
				"lesson", dl.title, "scenario", title, "status", c.Status)
			return nil
		case dl.voice && !voiceReady(&c):
			log.Warn("lessons: seed: у сценария нет брифа/чек-листа разговора, голосовое занятие пропущено",
				"lesson", dl.title, "scenario", title)
			return nil
		case dl.dds && len(c.Services) == 0:
			log.Warn("lessons: seed: у сценария нет списка оповещения служб, занятие ДДС пропущено",
				"lesson", dl.title, "scenario", title)
			return nil
		}
		scenarioIDs = append(scenarioIDs, c.ID)
	}
	participantIDs := make([]uuid.UUID, 0, len(dl.participants))
	for _, login := range dl.participants {
		id, ok := users[login]
		if !ok {
			log.Warn("lessons: seed: демо-обучающийся не найден, занятие пропущено", "lesson", dl.title, "login", login)
			return nil
		}
		participantIDs = append(participantIDs, id)
	}

	ls := model.DefaultLessonSettings(snap)
	mode := core.ModeCards
	if dl.dds {
		ls.Perspective, mode = model.PerspectiveDDS, core.ModeCardActions
	}
	if dl.voice {
		// input both — демо работает и без гарнитуры (текстом), и с микрофоном. Веса — пресет
		// формы создания при включении голоса (LessonListPage: 35/20/10/10/25).
		ls.Voice = settings.Voice{Enabled: true, Input: model.VoiceInputBoth, PushToTalk: true, MaxTurns: 12, TTSEnabled: true}
		ls.Weights = effectiveWeights(settings.Weights{Fields: 0.35, Semantic: 0.2, Grammar: 0.1, Timing: 0.1, Dialogue: 0.25},
			true, snap.DialogueWeightDefault)
	} else {
		ls.Voice.Enabled = false
		ls.Weights = wholePercents(scoring.EffectiveWeights(snap.ScoreWeights, false, snap.DialogueWeightDefault))
	}

	in := lessonInsert{
		ID: ids.New(), TeacherID: teacherID, Title: dl.title, Mode: mode, Status: core.LessonDraft,
		TimeLimitSec: dl.timeLimitSec, Settings: ls, ScenarioIDs: scenarioIDs, ParticipantIDs: participantIDs,
	}
	if dl.difficulty > 0 {
		v := dl.difficulty
		in.Difficulty = &v
	}
	if dl.running {
		now := time.Now().UTC()
		in.Status, in.StartedAt = core.LessonRunning, &now
	}
	if err := insertLesson(ctx, tx, &in); err != nil {
		return fmt.Errorf("lessons: seed %q: %w", dl.title, err)
	}
	if dl.running && issuer != nil {
		for _, uid := range participantIDs {
			if _, _, err := issuer.IssueNext(ctx, tx, in.ID, uid); err != nil {
				return fmt.Errorf("lessons: seed %q: issue: %w", dl.title, err)
			}
		}
	}
	log.Info("lessons: seed: демо-занятие создано", "lesson", dl.title, "status", in.Status)
	return nil
}

func demoUserIDs(ctx context.Context, q pg.Querier) (map[string]uuid.UUID, error) {
	logins := []string{demoTeacherLogin, "student", "student2"}
	rows, err := q.Query(ctx, sqlDemoUsers, logins)
	if err != nil {
		return nil, fmt.Errorf("lessons: seed users: %w", err)
	}
	defer rows.Close()
	out := make(map[string]uuid.UUID, len(logins))
	for rows.Next() {
		var (
			login string
			id    uuid.UUID
		)
		if err := rows.Scan(&login, &id); err != nil {
			return nil, fmt.Errorf("lessons: seed users: %w", err)
		}
		out[login] = id
	}
	return out, rows.Err()
}

// demoScenarios — первый (лучший по порядку запроса) сценарий на каждый заголовок.
func demoScenarios(ctx context.Context, q pg.Querier) (map[string]scenarioCheck, error) {
	rows, err := q.Query(ctx, sqlDemoScenarios, []string{demoFireTitle, demoGasTitle, demoWaterTitle})
	if err != nil {
		return nil, fmt.Errorf("lessons: seed scenarios: %w", err)
	}
	defer rows.Close()
	out := make(map[string]scenarioCheck, 3)
	for rows.Next() {
		var c scenarioCheck
		if err := scanScenarioCheck(rows, &c); err != nil {
			return nil, fmt.Errorf("lessons: seed scenarios: %w", err)
		}
		if _, seen := out[c.Title]; !seen {
			out[c.Title] = c
		}
	}
	return out, rows.Err()
}
