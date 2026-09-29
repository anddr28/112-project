package lessons

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"lct/gocore/internal/platform/httpx"
)

// LessonSettings.cardSource (v1.4): по умолчанию mixed, хранится в lessons.settings и в
// колонке scenario_source, пул проверяется по источнику сценариев (422 details.scenarioIds).
func TestCreateLesson_CardSource_DB(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	e := newEnv(t, f.pool, nil)
	// карточка обучающегося (to-scenario): source = student
	if _, err := f.pool.Exec(context.Background(), `UPDATE scenarios SET source = 'student' WHERE id = $1`, f.scGas); err != nil {
		t.Fatal(err)
	}
	pool := []uuid.UUID{f.scFire, f.scGas}

	// без cardSource — смешанный пул (как до v1.4)
	l := e.createLesson(t, f.teacherW(), lessonBody("Смешанный", pool, []uuid.UUID{f.s1}))
	if l.Settings.CardSource == nil || *l.Settings.CardSource != "mixed" {
		t.Fatalf("cardSource по умолчанию = %v", l.Settings.CardSource)
	}
	if got := f.scalar(t, `SELECT scenario_source || '/' || (settings->>'card_source') FROM lessons WHERE id = $1`, l.Id); got != "mixed/mixed" {
		t.Errorf("хранение = %v", got)
	}

	// generated: карточка обучающегося не подходит
	body := lessonBody("Только сгенерированные", pool, []uuid.UUID{f.s1})
	body["cardSource"] = "generated"
	er := expectError(t, e.do(t, "POST", "/lessons", f.teacherW(), body), 422, httpx.CodeValidation)
	if ids, _ := er.Details["scenarioIds"].([]any); len(ids) != 1 || ids[0] != f.scGas.String() {
		t.Errorf("generated details: %+v", er.Details)
	}
	// student: не подходят сценарии не от обучающихся
	body["cardSource"] = "student"
	er = expectError(t, e.do(t, "POST", "/lessons", f.teacherW(), body), 422, httpx.CodeValidation)
	if ids, _ := er.Details["scenarioIds"].([]any); len(ids) != 1 || ids[0] != f.scFire.String() {
		t.Errorf("student details: %+v", er.Details)
	}
	// student + только карточки обучающихся — создаётся и читается обратно
	body = lessonBody("Карточки обучающихся", []uuid.UUID{f.scGas}, []uuid.UUID{f.s1})
	body["cardSource"] = "student"
	l = e.createLesson(t, f.teacherW(), body)
	if l.Settings.CardSource == nil || *l.Settings.CardSource != "student" {
		t.Errorf("cardSource = %v", l.Settings.CardSource)
	}
	got := decode[struct {
		Settings struct {
			CardSource string `json:"cardSource"`
		} `json:"settings"`
	}](t, e.do(t, "GET", "/lessons/"+l.Id.String(), f.teacherW(), nil), 200)
	if got.Settings.CardSource != "student" {
		t.Errorf("GET: cardSource = %q", got.Settings.CardSource)
	}
	if got := f.scalar(t, `SELECT scenario_source FROM lessons WHERE id = $1`, l.Id); got != "student" {
		t.Errorf("scenario_source = %v", got)
	}

	// неизвестное значение — 400 по полю
	body["cardSource"] = "ai"
	if er := expectError(t, e.do(t, "POST", "/lessons", f.teacherW(), body), 400, httpx.CodeValidation); er.field("cardSource") == "" {
		t.Errorf("нет ошибки по cardSource: %+v", er)
	}
}
