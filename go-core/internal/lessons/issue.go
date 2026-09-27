package lessons

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/core"
	"lct/gocore/internal/dds"
	"lct/gocore/internal/eventlog"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/store"
)

// Выдача попыток (DESIGN §5 «Попытка»):
//   - seq_no = (сколько уже выдано) + 1; лимит — settings.cards_per_student;
//   - сценарий — по кругу из пула (lesson_scenarios.sort_order):
//     pool[(индекс участника + выдано) % len(pool)], индекс участника — позиция в
//     lesson_participants по user_id (детерминированно и без отдельной колонки порядка;
//     соседние обучающиеся получают разные сценарии, как issueAttempts во фронте);
//     подряд один и тот же сценарий не выдаётся, если в пуле их больше одного;
//   - etalon_id — текущая версия эталона сценария на момент выдачи;
//   - time_limit_sec — норматив занятия, incident_no — sequence (DEFAULT), пустой
//     черновик (convert.EmptyDraft), серверное событие issued {scenarioId};
//   - ракурс dds (v1.3): в пуле участника — только профильные сценарии (dds.Pick: служба
//     профиля есть в списке оповещения; без профиля — любой сценарий со службами), служба
//     обучающегося фиксируется в attempts.service_id. Профильных карточек нет — участнику
//     ничего не выдаётся (как исчерпанный лимит), занятие не ломается.

// emptyDraftJSON — attempt_drafts.data новой попытки; форма неизменна — кодируется один раз.
var emptyDraftJSON = func() json.RawMessage {
	b, err := json.Marshal(convert.EmptyDraft())
	if err != nil {
		panic("lessons: empty draft: " + err.Error())
	}
	return b
}()

// Пул сценариев и текущие эталоны в одном порядке (uuid.Nil — у сценария нет эталона) и —
// только в ракурсе dds — службы списка оповещения каждого сценария (строка кодов через
// запятую, store.ServiceCodesSQL); у ракурса 112 массив пуст и jsonb эталонов не разбирается.
var poolColumns = `
       ARRAY(SELECT ls.scenario_id FROM lesson_scenarios ls
              WHERE ls.lesson_id = l.id ORDER BY ls.sort_order, ls.scenario_id),
       ARRAY(SELECT COALESCE(e.id, '00000000-0000-0000-0000-000000000000'::uuid)
               FROM lesson_scenarios ls
               LEFT JOIN etalons e ON e.scenario_id = ls.scenario_id AND e.is_current
              WHERE ls.lesson_id = l.id ORDER BY ls.sort_order, ls.scenario_id),
       CASE WHEN l.settings->>'perspective' = 'dds' THEN
       ARRAY(SELECT COALESCE(` + store.ServiceCodesSQL("e") + `, '')
               FROM lesson_scenarios ls
               LEFT JOIN etalons e ON e.scenario_id = ls.scenario_id AND e.is_current
              WHERE ls.lesson_id = l.id ORDER BY ls.sort_order, ls.scenario_id)
       ELSE '{}'::text[] END`

// IssueNext: всё, что нужно для решения, — одним запросом; занятие под FOR SHARE, чтобы
// завершение занятия (UPDATE lessons) не проскочило между проверкой и вставкой попытки.
// Данные участника — для participantStatus в монитор.
var sqlIssueInfo = `
SELECT l.status, l.mode, l.time_limit_sec, l.settings,` + poolColumns + `,
       p.user_id IS NOT NULL,
       (SELECT count(*)::int FROM lesson_participants px WHERE px.lesson_id = l.id AND px.user_id < $2),
       COALESCE(la.seq_no, 0), la.scenario_id,
       COALESCE(p.status, ''), p.joined_at, p.finished_at, COALESCE(p.mic_ready, false),
       COALESCE(u.last_name, ''), COALESCE(u.first_name, ''), COALESCE(u.middle_name, ''),
       COALESCE((SELECT sv.code FROM services sv WHERE sv.id = u.service_id), '')
  FROM lessons l
  LEFT JOIN lesson_participants p ON p.lesson_id = l.id AND p.user_id = $2
  LEFT JOIN users u ON u.id = p.user_id
  LEFT JOIN LATERAL (SELECT a.seq_no, a.scenario_id FROM attempts a
                      WHERE a.lesson_id = l.id AND a.user_id = $2
                      ORDER BY a.seq_no DESC LIMIT 1) la ON true
 WHERE l.id = $1
   FOR SHARE OF l`

// Попытка и её черновик — одним оператором. ON CONFLICT по (lesson_id, user_id, seq_no):
// параллельная выдача того же номера (гонка двух submit) не роняет транзакцию — вторая
// просто ничего не вставит.
const sqlInsertAttempt = `
WITH ins AS (
  INSERT INTO attempts (id, lesson_id, user_id, scenario_id, etalon_id, mode, seq_no, status,
                        time_limit_sec, issued_at, service_id)
  VALUES ($1, $2, $3, $4, $5, $6, $7, 'issued', $8, $9,
          (SELECT sv.id FROM services sv WHERE sv.code = NULLIF($11::text, '')))
  ON CONFLICT (lesson_id, user_id, seq_no) DO NOTHING
  RETURNING id
), d AS (
  INSERT INTO attempt_drafts (attempt_id, data, updated_at)
  SELECT id, $10::jsonb, $9::timestamptz FROM ins
)
SELECT id FROM ins`

const sqlAttemptBySeq = `SELECT id FROM attempts WHERE lesson_id = $1 AND user_id = $2 AND seq_no = $3`

// IssueNext — следующая карточка обучающемуся (core.AttemptIssuer). Вызывается в
// транзакции submit (attempts) и сида. issued=false — занятие не идёт, пользователь не
// участник или лимит карточек исчерпан. После COMMIT — participantStatus (новый attemptId)
// и attemptEvent issued в монитор.
func (s *Service) IssueNext(ctx context.Context, tx pgx.Tx, lessonID, userID uuid.UUID) (uuid.UUID, bool, error) {
	var (
		status, mode        string
		timeLimit           int
		rawSettings         []byte
		pool, etalons       []uuid.UUID
		codes               []string
		profile             string
		isParticipant       bool
		participantIdx      int
		lastSeq             int
		lastScenario        *uuid.UUID
		pr                  store.ParticipantRow
		last, first, middle string
	)
	err := tx.QueryRow(ctx, sqlIssueInfo, lessonID, userID).Scan(
		&status, &mode, &timeLimit, &rawSettings, &pool, &etalons, &codes,
		&isParticipant, &participantIdx, &lastSeq, &lastScenario,
		&pr.Status, &pr.JoinedAt, &pr.FinishedAt, &pr.MicReady, &last, &first, &middle, &profile,
	)
	if err != nil {
		if pg.IsNoRows(err) {
			return uuid.Nil, false, fmt.Errorf("lessons: issue: занятие %s не найдено", lessonID)
		}
		return uuid.Nil, false, fmt.Errorf("lessons: issue info: %w", err)
	}
	if status != core.LessonRunning || !isParticipant {
		return uuid.Nil, false, nil
	}
	ls, _ := model.ParseLessonSettings(rawSettings, defaultLessonSettings)
	if lastSeq >= ls.CardsPerStudent {
		return uuid.Nil, false, nil
	}
	acting := actingServices(&ls, codes, profile)
	pick, ok := pickScenario(pool, etalons, acting, participantIdx, lastSeq, lastScenario)
	if !ok {
		if ls.IsDDS() {
			// Профильных карточек для службы обучающегося в пуле нет (профиль сменили после
			// создания занятия) — участнику выдавать нечего.
			s.log.Warn("lessons: issue: в пуле нет профильных карточек для службы обучающегося",
				"lesson", lessonID, "user", userID, "service", profile)
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, fmt.Errorf("lessons: issue: в пуле занятия %s нет сценария с эталоном", lessonID)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	seq := lastSeq + 1
	id := ids.New()
	var got uuid.UUID
	err = tx.QueryRow(ctx, sqlInsertAttempt, id, lessonID, userID, pool[pick], etalons[pick], mode, seq,
		timeLimit, now, emptyDraftJSON, actingAt(acting, pick)).Scan(&got)
	if pg.IsNoRows(err) {
		// Номер уже выдан параллельной транзакцией (она и опубликует) — карточка есть.
		if err := tx.QueryRow(ctx, sqlAttemptBySeq, lessonID, userID, seq).Scan(&got); err != nil {
			return uuid.Nil, false, fmt.Errorf("lessons: issue: concurrent attempt: %w", err)
		}
		return got, true, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("lessons: issue insert: %w", err)
	}
	ev, err := eventlog.InsertServer(ctx, tx, id, core.EventIssued,
		map[string]any{"scenarioId": pool[pick].String()}, now)
	if err != nil {
		return uuid.Nil, false, err
	}

	pr.UserID, pr.LastName, pr.FirstName, pr.MiddleName, pr.AttemptID = userID, last, first, middle, &id
	participant := store.ParticipantToPublic(&pr)
	pg.OnCommit(ctx, func() {
		s.publishParticipant(lessonID, participant)
		s.publishAttemptEvent(lessonID, id, ev)
	})
	return id, true, nil
}

// actingServices — ракурс dds: служба обучающегося для каждого сценария пула ("" — сценарий
// ему не выдаётся, dds.Pick). codes — строки кодов из poolColumns. nil — ракурс 112:
// выдаётся любой сценарий, службы у попытки нет.
func actingServices(ls *model.LessonSettings, codes []string, profile string) []string {
	if !ls.IsDDS() {
		return nil
	}
	out := make([]string, len(codes))
	for i, c := range codes {
		if code, ok := dds.Pick(dds.SplitCodes(c), profile); ok {
			out[i] = code
		}
	}
	return out
}

// actingAt — служба обучающегося для выбранного сценария ("" — ракурс 112).
func actingAt(acting []string, i int) string {
	if i < 0 || i >= len(acting) {
		return ""
	}
	return acting[i]
}

// pickScenario — индекс сценария в пуле для очередной карточки. Сначала — по кругу от
// (индекс участника + выдано); сценарии без эталона пропускаются, в ракурсе dds — и
// непрофильные (acting[i] == ""; acting == nil — ракурс 112, фильтра нет); повтор
// предыдущего сценария — только если другого нет.
func pickScenario(pool, etalons []uuid.UUID, acting []string, participantIdx, issued int, prev *uuid.UUID) (int, bool) {
	n := len(pool)
	if n == 0 || len(etalons) != n || (acting != nil && len(acting) != n) {
		return -1, false
	}
	start := (participantIdx%n + issued%n) % n
	fallback := -1
	for k := 0; k < n; k++ {
		i := (start + k) % n
		if etalons[i] == uuid.Nil || (acting != nil && acting[i] == "") {
			continue
		}
		if n > 1 && prev != nil && pool[i] == *prev {
			if fallback < 0 {
				fallback = i
			}
			continue
		}
		return i, true
	}
	return fallback, fallback >= 0
}

// ---------------------------------------------------------------- массовая выдача (старт)

// issuePlan — очередная карточка одного участника.
type issuePlan struct {
	UserID      uuid.UUID
	AttemptID   uuid.UUID
	ScenarioID  uuid.UUID
	EtalonID    uuid.UUID
	SeqNo       int
	ServiceCode string // ракурс dds — служба обучающегося; "" — ракурс 112
}

// Участники по user_id (тот же порядок, что индекс участника в IssueNext) и последняя
// выданная карточка каждого.
const sqlParticipantsPlan = `
SELECT p.user_id, COALESCE(la.seq_no, 0), la.scenario_id,
       COALESCE((SELECT sv.code FROM users u JOIN services sv ON sv.id = u.service_id WHERE u.id = p.user_id), '')
  FROM lesson_participants p
  LEFT JOIN LATERAL (SELECT a.seq_no, a.scenario_id FROM attempts a
                      WHERE a.lesson_id = p.lesson_id AND a.user_id = p.user_id
                      ORDER BY a.seq_no DESC LIMIT 1) la ON true
 WHERE p.lesson_id = $1
 ORDER BY p.user_id`

// Все попытки старта — одним оператором: попытки, черновики и события issued. Формат
// события тот же, что пишет eventlog.InsertServer ({scenarioId}, client_seq NULL, at =
// issued_at). На 200 участников это 1 round-trip вместо ~600.
const sqlInsertAttemptsBulk = `
WITH ins AS (
  INSERT INTO attempts (id, lesson_id, user_id, scenario_id, etalon_id, mode, seq_no, status,
                        time_limit_sec, issued_at, service_id)
  SELECT t.id, $1, t.user_id, t.scenario_id, t.etalon_id, $2, t.seq_no, 'issued', $3, $4,
         (SELECT sv.id FROM services sv WHERE sv.code = NULLIF(t.service_code, ''))
    FROM unnest($5::uuid[], $6::uuid[], $7::uuid[], $8::uuid[], $9::int[], $11::text[])
         AS t(id, user_id, scenario_id, etalon_id, seq_no, service_code)
  ON CONFLICT (lesson_id, user_id, seq_no) DO NOTHING
  RETURNING id, scenario_id
), d AS (
  INSERT INTO attempt_drafts (attempt_id, data, updated_at)
  SELECT id, $10::jsonb, $4::timestamptz FROM ins
), ev AS (
  INSERT INTO attempt_events (attempt_id, type, payload, at)
  SELECT id, 'issued', jsonb_build_object('scenarioId', scenario_id), $4::timestamptz FROM ins
  RETURNING id, attempt_id
)
SELECT ins.id, ins.scenario_id, ev.id FROM ins JOIN ev ON ev.attempt_id = ins.id`

// issuedEvent — выданная попытка и её событие (для attemptEvent в монитор).
type issuedEvent struct {
	AttemptID uuid.UUID
	Event     public.AttemptEvent
}

// planFromRows — очередная карточка каждому участнику, у кого лимит не исчерпан
// (rows — результат sqlParticipantsPlan; принимаются строки, а не Querier, чтобы запрос
// можно было отправить в одном пакете со сменой статуса занятия). codes — службы
// сценариев пула (ракурс dds, poolColumns); участник без профильных карточек пропускается.
func planFromRows(rows pgx.Rows, lessonID uuid.UUID, ls *model.LessonSettings, pool, etalons []uuid.UUID, codes []string) ([]issuePlan, error) {
	defer rows.Close()
	var (
		plans   []issuePlan
		userID  uuid.UUID
		lastSeq int
		prev    *uuid.UUID
		profile string
	)
	for idx := 0; rows.Next(); idx++ {
		prev = nil
		if err := rows.Scan(&userID, &lastSeq, &prev, &profile); err != nil {
			return nil, fmt.Errorf("lessons: plan: %w", err)
		}
		if lastSeq >= ls.CardsPerStudent {
			continue
		}
		acting := actingServices(ls, codes, profile)
		pick, ok := pickScenario(pool, etalons, acting, idx, lastSeq, prev)
		if !ok {
			if ls.IsDDS() {
				continue // профильных карточек для службы участника нет — ему выдавать нечего
			}
			return nil, fmt.Errorf("lessons: plan: в пуле занятия %s нет сценария с эталоном", lessonID)
		}
		plans = append(plans, issuePlan{
			UserID: userID, AttemptID: ids.New(), ScenarioID: pool[pick], EtalonID: etalons[pick], SeqNo: lastSeq + 1,
			ServiceCode: actingAt(acting, pick),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lessons: plan: %w", err)
	}
	return plans, nil
}

// insertPlans — массовая вставка попыток по плану (внутри транзакции старта).
func insertPlans(ctx context.Context, tx pgx.Tx, lessonID uuid.UUID, mode string, timeLimit int, plans []issuePlan) ([]issuedEvent, error) {
	if len(plans) == 0 {
		return nil, nil
	}
	n := len(plans)
	attemptIDs, userIDs := make([]uuid.UUID, n), make([]uuid.UUID, n)
	scenarioIDs, etalonIDs := make([]uuid.UUID, n), make([]uuid.UUID, n)
	seqs := make([]int32, n)
	codes := make([]string, n)
	for i := range plans {
		attemptIDs[i], userIDs[i] = plans[i].AttemptID, plans[i].UserID
		scenarioIDs[i], etalonIDs[i], seqs[i] = plans[i].ScenarioID, plans[i].EtalonID, int32(plans[i].SeqNo)
		codes[i] = plans[i].ServiceCode
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	rows, err := tx.Query(ctx, sqlInsertAttemptsBulk, lessonID, mode, timeLimit, now,
		attemptIDs, userIDs, scenarioIDs, etalonIDs, seqs, emptyDraftJSON, codes)
	if err != nil {
		return nil, fmt.Errorf("lessons: issue bulk: %w", err)
	}
	defer rows.Close()
	out := make([]issuedEvent, 0, n)
	for rows.Next() {
		var (
			attemptID, scenarioID uuid.UUID
			eventID               int64
		)
		if err := rows.Scan(&attemptID, &scenarioID, &eventID); err != nil {
			return nil, fmt.Errorf("lessons: issue bulk: %w", err)
		}
		payload := map[string]any{"scenarioId": scenarioID.String()}
		out = append(out, issuedEvent{AttemptID: attemptID, Event: public.AttemptEvent{
			Id: int(eventID), Type: public.AttemptEventType(core.EventIssued), Payload: &payload, At: now,
		}})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lessons: issue bulk: %w", err)
	}
	return out, nil
}
