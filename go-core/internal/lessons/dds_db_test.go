package lessons

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"lct/gocore/internal/core"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/pg"
)

// ddsBody — занятие ракурса «Диспетчер ДДС» (v1.3).
func ddsBody(title string, scen, parts []uuid.UUID) map[string]any {
	b := lessonBody(title, scen, parts)
	b["perspective"], b["mode"] = "dds", core.ModeCardActions
	return b
}

// setProfile — профиль ДДС обучающегося (код services.code; "" — без профиля).
func (f *fixture) setProfile(t *testing.T, user uuid.UUID, code string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE users SET service_id = (SELECT id FROM services WHERE code = NULLIF($2, '')) WHERE id = $1`, user, code); err != nil {
		t.Fatal(err)
	}
}

// actingCode — служба попытки ("" — ракурс 112).
func (f *fixture) actingCode(t *testing.T, attemptID uuid.UUID) string {
	t.Helper()
	return f.scalar(t, `SELECT COALESCE((SELECT sv.code FROM services sv WHERE sv.id = a.service_id), '')
		FROM attempts a WHERE a.id = $1`, attemptID).(string)
}

func TestCreateLesson_DDS_DB(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e := newEnv(t, f.pool, nil)
	f.setProfile(t, f.s1, "zhkh")    // есть в списке оповещения фикстуры (101, zhkh)
	f.setProfile(t, f.s2, "gormost") // нет ни в одном сценарии

	// Ракурс ДДС — только «действия с карточками».
	body := ddsBody("ДДС", []uuid.UUID{f.scFire}, []uuid.UUID{f.s1})
	body["mode"] = core.ModeCards
	if er := expectError(t, e.do(t, "POST", "/lessons", f.teacherW(), body), 400, httpx.CodeValidation); er.field("mode") == "" {
		t.Errorf("нет ошибки по mode: %+v", er)
	}

	// У участника с профилем нет профильных карточек — 422 с участником в details.
	er := expectError(t, e.do(t, "POST", "/lessons", f.teacherW(),
		ddsBody("ДДС", []uuid.UUID{f.scFire, f.scGas}, []uuid.UUID{f.s1, f.s2})), 422, httpx.CodeValidation)
	ids, _ := er.Details["participantIds"].([]any)
	if len(ids) != 1 || ids[0] != f.s2.String() {
		t.Errorf("details: %+v", er.Details)
	}

	// Сценарий без списка оповещения карточке ДДС не годится.
	if _, err := f.pool.Exec(context.Background(), `UPDATE etalons SET card = '{}' WHERE scenario_id = $1`, f.scGas); err != nil {
		t.Fatal(err)
	}
	er = expectError(t, e.do(t, "POST", "/lessons", f.teacherW(),
		ddsBody("ДДС", []uuid.UUID{f.scFire, f.scGas}, []uuid.UUID{f.s1})), 422, httpx.CodeValidation)
	if ids, _ := er.Details["scenarioIds"].([]any); len(ids) != 1 || ids[0] != f.scGas.String() {
		t.Errorf("details: %+v", er.Details)
	}

	// Режим сценария ракурсу ДДС не важен (нужна только карточка 112), голос выключается.
	body = ddsBody("Диспетчер ДДС", []uuid.UUID{f.scPlain, f.scActions}, []uuid.UUID{f.s1})
	body["voice"] = map[string]any{"enabled": true}
	l := e.createLesson(t, f.teacherW(), body)
	if l.Perspective != "dds" || l.Mode != core.ModeCardActions || l.Settings.Voice.Enabled || l.Settings.Weights.Dialogue != 0 {
		t.Errorf("lesson %+v", l)
	}
	if a := e.audit.byAction("lesson.create"); len(a) == 0 {
		t.Error("нет аудита")
	}
}

func TestIssue_DDS_DB(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e := newEnv(t, f.pool, nil)
	f.setProfile(t, f.s1, "zhkh")
	f.setProfile(t, f.s2, "") // без профиля — основная служба карточки (101)

	body := ddsBody("Диспетчер ДДС", []uuid.UUID{f.scFire, f.scGas}, []uuid.UUID{f.s1, f.s2})
	body["cardsPerStudent"] = 3
	l := e.createLesson(t, f.teacherW(), body)
	e.startLesson(t, f.teacherW(), l.Id)

	a1, a2 := f.attemptOf(t, l.Id, f.s1).ID, f.attemptOf(t, l.Id, f.s2).ID
	if got := f.actingCode(t, a1); got != "zhkh" {
		t.Errorf("служба s1 = %q, want zhkh (профиль)", got)
	}
	if got := f.actingCode(t, a2); got != "101" {
		t.Errorf("служба s2 = %q, want 101 (основная)", got)
	}

	issue := func(userID uuid.UUID) (uuid.UUID, bool) {
		var (
			id uuid.UUID
			ok bool
		)
		if err := pg.WithTx(context.Background(), f.pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			id, ok, err = e.svc.IssueNext(ctx, tx, l.Id, userID)
			return err
		}); err != nil {
			t.Fatalf("IssueNext: %v", err)
		}
		return id, ok
	}
	// Следующая карточка — тоже профильная и с той же службой.
	if id, ok := issue(f.s1); !ok || f.actingCode(t, id) != "zhkh" {
		t.Errorf("следующая карточка s1: %v %v", id, ok)
	}
	// Профиль сменили на службу, которой нет в пуле, — карточек больше нет, занятие не падает.
	f.setProfile(t, f.s1, "gormost")
	if _, ok := issue(f.s1); ok {
		t.Error("непрофильная карточка выдана")
	}
}

// Занятие ракурса 112 службу попытке не назначает.
func TestIssue_Operator112_NoService_DB(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e := newEnv(t, f.pool, nil)
	f.setProfile(t, f.s1, "zhkh")
	l := f.runningLesson(t, e, 1)
	if got := f.actingCode(t, f.attemptOf(t, l.Id, f.s1).ID); got != "" {
		t.Errorf("служба у попытки 112: %q", got)
	}
}
