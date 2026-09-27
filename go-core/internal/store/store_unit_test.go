package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"lct/gocore/internal/convert"
	"lct/gocore/internal/gen/components"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/model"
	"lct/gocore/internal/settings"
)

// Обязательные поля схем frontend.v1.yaml (components.schemas.*.required).
var (
	reqScenario    = []string{"id", "title", "categoryId", "categoryName", "difficulty", "mode", "source", "status", "callScript", "etalonCard", "etalonDraft", "requiredFields", "createdAt", "etalonVersion"}
	reqLesson      = []string{"id", "kind", "title", "teacherId", "mode", "perspective", "timeLimitSec", "status", "scenarioIds", "participants", "settings", "createdAt"}
	reqParticipant = []string{"userId", "name", "status"}
	reqSettings    = []string{"passThreshold", "weights", "cardsPerStudent", "allowReplay", "voice"}
	reqAttempt     = []string{"id", "lessonId", "lessonTitle", "userId", "scenarioId", "mode", "perspective", "seqNo", "status", "timeLimitSec", "issuedAt", "replayCount", "incidentNo", "serverNow", "voice"}
	reqTurnView    = []string{"turnNo", "speaker", "text", "atMs", "source"}
	reqCallScript  = []string{"caller", "address", "keyFacts", "turns"}
	reqDraft       = []string{"phones", "applicant", "address", "incidentTypeIds", "attributes", "description", "actionsTaken", "flags", "services"}
)

// asJSONMap — v как JSON-объект (так его увидит фронт).
func asJSONMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return m
}

// requireKeys — обязательные ключи есть и не null.
func requireKeys(t *testing.T, what string, m map[string]any, keys []string) {
	t.Helper()
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			t.Errorf("%s: обязательное поле %q отсутствует или null", what, k)
		}
	}
}

// requireArray — ключ — массив (не null).
func requireArray(t *testing.T, what string, m map[string]any, key string) []any {
	t.Helper()
	a, ok := m[key].([]any)
	if !ok {
		t.Errorf("%s.%s = %#v, want массив", what, key, m[key])
	}
	return a
}

func TestIsEmptyJSON(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"", " ", "null", "{}", "[]", " {} \n"} {
		if !isEmptyJSON([]byte(s)) {
			t.Errorf("isEmptyJSON(%q) = false", s)
		}
	}
	if !isEmptyJSON(nil) {
		t.Error("nil")
	}
	for _, s := range []string{`{"a":1}`, `[1]`, `""`, `0`, `{ }`} {
		if isEmptyJSON([]byte(s)) {
			t.Errorf("isEmptyJSON(%q) = true", s)
		}
	}
}

func TestNullStr(t *testing.T) {
	t.Parallel()

	if nullStr("") != nil {
		t.Fatal("\"\" -> NULL")
	}
	if p := nullStr("running"); p == nil || *p != "running" {
		t.Fatal("значение")
	}
}

func TestScanners(t *testing.T) {
	t.Parallel()

	t.Run("jsonScan", func(t *testing.T) {
		t.Parallel()
		dst := model.CallScript{Caller: model.Caller{Name: "было"}}
		s := &jsonScan{dst: &dst, name: "scenarios.call_script"}
		if err := s.ScanBytes(nil); err != nil || dst.Caller.Name != "было" {
			t.Fatal("NULL оставляет dst нетронутым")
		}
		if err := s.ScanBytes([]byte(`{"caller":{"name":"Иван"},"turns":[]}`)); err != nil || dst.Caller.Name != "Иван" {
			t.Fatalf("разбор: %v %+v", err, dst)
		}
		err := s.ScanBytes([]byte(`{"turns": 5}`))
		if err == nil || !strings.Contains(err.Error(), "scenarios.call_script") {
			t.Fatalf("ошибка должна называть колонку: %v", err)
		}
	})

	t.Run("rawScan копирует буфер драйвера", func(t *testing.T) {
		t.Parallel()
		var dst json.RawMessage = json.RawMessage(`{"old":1}`)
		s := &rawScan{dst: &dst}
		if err := s.ScanBytes(nil); err != nil || dst != nil {
			t.Fatal("NULL -> nil")
		}
		buf := []byte(`{"a":1}`)
		if err := s.ScanBytes(buf); err != nil {
			t.Fatal(err)
		}
		buf[2] = 'X' // драйвер переиспользует буфер
		if string(dst) != `{"a":1}` {
			t.Fatalf("rawScan не скопировал: %s", dst)
		}
	})

	t.Run("settingsScan", func(t *testing.T) {
		t.Parallel()
		var got model.LessonSettings
		s := &settingsScan{dst: &got}
		if err := s.ScanBytes(nil); err != nil || got != defaultLessonSettings {
			t.Fatalf("NULL -> дефолты: %+v", got)
		}
		if err := s.ScanBytes([]byte(`{"perspective":"dds","pass_threshold":"сломано"}`)); err != nil {
			t.Fatalf("кривые settings не роняют чтение: %v", err)
		}
		if got.Perspective != model.PerspectiveDDS || got.PassThreshold != defaultLessonSettings.PassThreshold {
			t.Fatalf("best effort: %+v", got)
		}
		if err := s.ScanBytes([]byte(`{не json`)); err != nil || got != defaultLessonSettings {
			t.Fatalf("синтаксический мусор -> дефолты: %+v %v", got, err)
		}
	})

	t.Run("dialogueScan", func(t *testing.T) {
		t.Parallel()
		d := &model.ExpectedDialogue{}
		s := &dialogueScan{dst: &d}
		for _, empty := range []string{"", "{}", "null"} {
			d = &model.ExpectedDialogue{}
			if err := s.ScanBytes([]byte(empty)); err != nil || d != nil {
				t.Fatalf("%q -> nil, got %+v %v", empty, d, err)
			}
		}
		if err := s.ScanBytes([]byte(`{"max_operator_turns":5}`)); err != nil || d == nil || d.Checklist == nil || d.MaxOperatorTurns != 5 {
			t.Fatalf("checklist нормализуется в []: %+v %v", d, err)
		}
		if err := s.ScanBytes([]byte(`{"checklist":"x"}`)); err == nil {
			t.Fatal("кривой чек-лист -> ошибка")
		}
	})

	t.Run("participantsScan", func(t *testing.T) {
		t.Parallel()
		var ps []ParticipantRow
		s := &participantsScan{dst: &ps}
		if err := s.ScanBytes(nil); err != nil || ps != nil {
			t.Fatal("NULL (нет участников) -> nil")
		}
		uid, aid := uuid.New(), uuid.New()
		raw := `[{"user_id":"` + uid.String() + `","last_name":"Иванова","first_name":"Мария","middle_name":null,
		          "status":"active","joined_at":"2026-09-24T15:04:05.123456+03:00","finished_at":null,
		          "last_seen_at":"2026-09-24T12:05:00+00:00","mic_ready":true,"attempt_id":"` + aid.String() + `"}]`
		if err := s.ScanBytes([]byte(raw)); err != nil {
			t.Fatal(err)
		}
		p := ps[0]
		if p.UserID != uid || p.Name != "Иванова М." || p.Status != "active" || !p.MicReady || p.AttemptID == nil || *p.AttemptID != aid {
			t.Fatalf("participant = %+v", p)
		}
		if p.JoinedAt.Location() != time.UTC || p.JoinedAt.Hour() != 12 || p.FinishedAt != nil || p.LastSeenAt.Location() != time.UTC {
			t.Fatalf("времена в UTC: %v %v", p.JoinedAt, p.LastSeenAt)
		}
		if err := s.ScanBytes([]byte(`{"not":"array"}`)); err == nil {
			t.Fatal("не массив -> ошибка")
		}
	})
}

func TestLessonRowHelpers(t *testing.T) {
	t.Parallel()

	teacher, author, student := uuid.New(), uuid.New(), uuid.New()
	r := &LessonRow{CreatedBy: author}
	if r.OwnerID() != author {
		t.Fatal("практика: владелец — автор")
	}
	r.TeacherID = &teacher
	if r.OwnerID() != teacher {
		t.Fatal("занятие: владелец — преподаватель")
	}
	r.Participants = []ParticipantRow{{UserID: uuid.New()}, {UserID: student, Status: "joined"}}
	p, ok := r.Participant(student)
	if !ok || p.Status != "joined" {
		t.Fatal("Participant")
	}
	p.Status = "active"
	if r.Participants[1].Status != "active" {
		t.Fatal("Participant возвращает указатель на элемент")
	}
	if _, ok := r.Participant(uuid.New()); ok {
		t.Fatal("чужой")
	}
}

func sampleLessonRow() LessonRow {
	teacher := uuid.New()
	started := time.Date(2026, 9, 24, 10, 0, 0, 0, time.FixedZone("MSK", 3*3600))
	d := 2
	s := model.DefaultLessonSettings(nil)
	s.Perspective = model.PerspectiveDDS
	s.Voice.Enabled = true
	return LessonRow{
		ID: uuid.New(), Kind: "class", Title: "Пожары", TeacherID: &teacher, CreatedBy: teacher,
		Mode: "cards", Difficulty: &d, TimeLimitSec: 45, Settings: s, Status: "running",
		StartedAt: &started, CreatedAt: started.Add(-time.Hour),
	}
}

func TestLessonToPublic(t *testing.T) {
	t.Parallel()

	t.Run("пустые массивы и обязательные поля", func(t *testing.T) {
		t.Parallel()
		r := sampleLessonRow()
		out := LessonToPublic(&r)
		m := asJSONMap(t, out)
		requireKeys(t, "Lesson", m, reqLesson)
		if len(requireArray(t, "Lesson", m, "scenarioIds")) != 0 || len(requireArray(t, "Lesson", m, "participants")) != 0 {
			t.Fatal("массивы пусты")
		}
		requireKeys(t, "Lesson.settings", m["settings"].(map[string]any), reqSettings)
		if m["perspective"] != "dds" || m["difficulty"] != float64(2) || m["teacherId"] != r.TeacherID.String() {
			t.Fatalf("lesson = %v", m)
		}
		if out.StartedAt.Location() != time.UTC || out.CreatedAt.Location() != time.UTC {
			t.Fatal("время в UTC")
		}
		if _, ok := m["finishedAt"]; ok {
			t.Fatal("finishedAt не задан — не отдаётся")
		}
		if v := m["settings"].(map[string]any)["voice"].(map[string]any); v["enabled"] != true {
			t.Fatalf("voice = %v", v)
		}
	})

	t.Run("практика и участники", func(t *testing.T) {
		t.Parallel()
		r := sampleLessonRow()
		author := uuid.New()
		r.Kind, r.TeacherID, r.CreatedBy, r.Difficulty = "practice", nil, author, nil
		aid := uuid.New()
		r.ScenarioIDs = []uuid.UUID{uuid.New()}
		r.Participants = []ParticipantRow{
			{UserID: author, LastName: "Петров", FirstName: "Пётр", MiddleName: "Петрович", Status: "finished", AttemptID: &aid},
			{UserID: uuid.New(), Name: "Готовое И.", Status: "assigned", MicReady: true},
		}
		out := LessonToPublic(&r)
		if out.TeacherId != author {
			t.Fatal("teacherId практики — автор")
		}
		if out.Difficulty != nil {
			t.Fatal("difficulty NULL -> не отдаётся")
		}
		m := asJSONMap(t, out)
		ps := requireArray(t, "Lesson", m, "participants")
		if len(ps) != 2 {
			t.Fatalf("participants = %v", ps)
		}
		p0 := ps[0].(map[string]any)
		requireKeys(t, "LessonParticipant", p0, reqParticipant)
		if p0["name"] != "Петров П. П." || p0["attemptId"] != aid.String() || p0["micReady"] != false {
			t.Fatalf("p0 = %v (имя собирается, если не посчитано)", p0)
		}
		if ps[1].(map[string]any)["name"] != "Готовое И." || ps[1].(map[string]any)["micReady"] != true {
			t.Fatalf("p1 = %v", ps[1])
		}
	})

	if got := LessonsToPublic(nil); got == nil || len(got) != 0 {
		t.Fatal("LessonsToPublic(nil) -> []")
	}
	if b, _ := json.Marshal(LessonsToPublic(nil)); string(b) != "[]" {
		t.Fatalf("json = %s", b)
	}
}

func TestScenarioToPublic(t *testing.T) {
	t.Parallel()

	base := func() ScenarioRow {
		return ScenarioRow{
			ID: uuid.New(), Title: "Пожар в квартире", CategoryID: uuid.New(), CategoryName: "Пожар", CategoryCode: "101",
			Difficulty: 2, Mode: "both", Source: "manual", Status: "draft",
			CallScript: model.CallScript{Turns: []model.Turn{}}, CreatedAt: time.Now(),
		}
	}

	t.Run("без эталона", func(t *testing.T) {
		t.Parallel()
		r := base()
		out := ScenarioToPublic(&r)
		m := asJSONMap(t, out)
		requireKeys(t, "Scenario", m, reqScenario)
		requireKeys(t, "Scenario.callScript", m["callScript"].(map[string]any), reqCallScript)
		requireKeys(t, "Scenario.etalonDraft", m["etalonDraft"].(map[string]any), reqDraft)
		if len(requireArray(t, "Scenario", m, "requiredFields")) != 0 {
			t.Fatal("requiredFields []")
		}
		if m["etalonVersion"] != float64(0) || m["version"] != float64(1) || m["inUse"] != false ||
			m["lessonsCount"] != float64(0) || m["ttsReady"] != false {
			t.Fatalf("scenario = %v", m)
		}
		if e, ok := m["etalonCard"].(map[string]any); !ok || len(e) != 0 {
			t.Fatalf("etalonCard = %v, want {}", m["etalonCard"])
		}
		for _, k := range []string{"scoring", "expectedActions", "expectedDialogue", "generationMeta", "notesForTeacher", "teacherComment"} {
			if _, ok := m[k]; ok {
				t.Errorf("пустое %s не отдаётся", k)
			}
		}
	})

	t.Run("с эталоном", func(t *testing.T) {
		t.Parallel()
		r := base()
		r.Version, r.LessonsCount, r.TTSReady = 3, 2, true
		r.TeacherComment = convert.Ptr("  ")
		r.GenerationMeta = model.GenerationMeta{NotesForTeacher: "Проверьте этаж", JobID: "j1"}
		draft := convert.EmptyDraft()
		draft.Address.Raw = "Москва"
		r.Etalon = &EtalonRow{
			ID: uuid.New(), Version: 4, IsCurrent: true,
			Card:             components.IncidentCard{CategoryCode: convert.Ptr("101")},
			CardDraft:        draft,
			Scoring:          model.Scoring{RequiredFields: []string{"address.raw"}},
			ExpectedActions:  []model.ExpectedAction{},
			ExpectedDialogue: &model.ExpectedDialogue{Checklist: []model.ChecklistItem{{ID: "q", Text: "Адрес?", Kind: "question"}}},
		}
		out := ScenarioToPublic(&r)
		m := asJSONMap(t, out)
		requireKeys(t, "Scenario", m, reqScenario)
		if m["etalonVersion"] != float64(4) || m["version"] != float64(3) || m["inUse"] != true || m["lessonsCount"] != float64(2) || m["ttsReady"] != true {
			t.Fatalf("scenario = %v", m)
		}
		if m["notesForTeacher"] != "Проверьте этаж" || m["generationMeta"].(map[string]any)["job_id"] != "j1" {
			t.Fatalf("meta: %v %v", m["notesForTeacher"], m["generationMeta"])
		}
		if _, ok := m["teacherComment"]; ok {
			t.Error("комментарий из пробелов не отдаётся")
		}
		if rf := requireArray(t, "Scenario", m, "requiredFields"); len(rf) != 1 || rf[0] != "address.raw" {
			t.Fatalf("requiredFields = %v", rf)
		}
		if m["etalonCard"].(map[string]any)["categoryCode"] != "101" || m["etalonDraft"].(map[string]any)["address"].(map[string]any)["raw"] != "Москва" {
			t.Fatal("эталон")
		}
		if _, ok := m["scoring"]; !ok {
			t.Error("непустой scoring отдаётся")
		}
		if _, ok := m["expectedActions"]; ok {
			t.Error("пустые expectedActions не отдаются")
		}
		if ed, ok := m["expectedDialogue"].(map[string]any); !ok || len(ed["checklist"].([]any)) != 1 {
			t.Errorf("expectedDialogue = %v", m["expectedDialogue"])
		}
	})

	if b, _ := json.Marshal(ScenariosToPublic(nil)); string(b) != "[]" {
		t.Fatalf("ScenariosToPublic(nil) = %s", b)
	}
}

func TestEtalonHelpers(t *testing.T) {
	t.Parallel()

	var nilE *EtalonRow
	if rf := nilE.RequiredFields(); rf == nil || len(rf) != 0 {
		t.Fatal("nil эталон -> []")
	}
	if rf := (&EtalonRow{}).RequiredFields(); rf == nil {
		t.Fatal("пустые -> []")
	}

	for _, raw := range []string{"", "{}", "null", `{"битый`, `[1,2]`} {
		e := EtalonRow{CardDraftRaw: json.RawMessage(raw)}
		finishEtalon(&e)
		m := asJSONMap(t, e.CardDraft)
		requireKeys(t, "etalonDraft("+raw+")", m, reqDraft)
		if e.ExpectedActions == nil {
			t.Fatal("expectedActions -> []")
		}
	}
	e := EtalonRow{CardDraftRaw: json.RawMessage(`{"incidentTypeIds":["t1"],"address":{"raw":"Москва"}}`)}
	finishEtalon(&e)
	if len(e.CardDraft.IncidentTypeIds) != 1 || e.CardDraft.Address.Raw != "Москва" || e.CardDraft.Services == nil || e.CardDraft.Attributes == nil {
		t.Fatalf("draft = %+v", e.CardDraft)
	}

	r := ScenarioRow{}
	if r.InUse() {
		t.Fatal("InUse без занятий")
	}
	r.LessonsCount = 1
	if !r.InUse() {
		t.Fatal("InUse")
	}
}

func TestAttemptToPublic(t *testing.T) {
	t.Parallel()

	msk := time.FixedZone("MSK", 3*3600)
	issued := time.Date(2026, 9, 24, 12, 0, 0, 0, msk)
	now := time.Date(2026, 9, 24, 12, 0, 30, 0, msk)
	base := func() AttemptRow {
		return AttemptRow{
			ID: uuid.New(), LessonID: uuid.New(), UserID: uuid.New(), ScenarioID: uuid.New(), EtalonID: uuid.New(),
			Mode: "cards", SeqNo: 1, Status: "issued", TimeLimitSec: 30, IssuedAt: issued,
			IncidentNo: 36814852, LessonTitle: "Пожары", LessonSettings: model.DefaultLessonSettings(nil),
		}
	}

	t.Run("текстовый режим", func(t *testing.T) {
		t.Parallel()
		r := base()
		out := AttemptToPublic(&r, now)
		m := asJSONMap(t, out)
		requireKeys(t, "Attempt", m, reqAttempt)
		if m["incidentNo"] != "36814852" || m["perspective"] != "operator112" || m["lessonTitle"] != "Пожары" {
			t.Fatalf("attempt = %v", m)
		}
		if out.ServerNow.Location() != time.UTC || !out.ServerNow.Equal(now) || out.IssuedAt.Location() != time.UTC {
			t.Fatal("время в UTC")
		}
		for _, k := range []string{"dialogue", "card", "callAcceptedAt", "submittedAt", "timeSpentMs"} {
			if _, ok := m[k]; ok {
				t.Errorf("%s не отдаётся", k)
			}
		}
		if m["voice"].(map[string]any)["enabled"] != false {
			t.Fatal("voice.enabled")
		}
	})

	t.Run("голосовой режим и карточка", func(t *testing.T) {
		t.Parallel()
		r := base()
		r.LessonSettings.Voice.Enabled = true
		r.LessonSettings.Perspective = model.PerspectiveDDS
		ended := issued.Add(time.Minute)
		r.CallEndedAt, r.DialogueTurns, r.Status = &ended, 7, "evaluating"
		r.Card = json.RawMessage(`{"description":"Пожар","address":{"raw":"Москва"}}`)
		out := AttemptToPublic(&r, now)
		if out.Dialogue == nil || !*out.Dialogue.CallEnded || *out.Dialogue.TurnsCount != 7 || out.Dialogue.CallEndedAt.Location() != time.UTC {
			t.Fatalf("dialogue = %+v", out.Dialogue)
		}
		if out.Card == nil || out.Card.Description != "Пожар" || out.Card.IncidentTypeIds == nil || out.Card.Services == nil {
			t.Fatalf("card = %+v", out.Card)
		}
		if out.Perspective != public.Dds {
			t.Fatal("perspective из настроек занятия")
		}
	})

	t.Run("голос включён, разговор идёт", func(t *testing.T) {
		t.Parallel()
		r := base()
		r.LessonSettings.Voice.Enabled = true
		out := AttemptToPublic(&r, now)
		if out.Dialogue == nil || *out.Dialogue.CallEnded || out.Dialogue.CallEndedAt != nil || *out.Dialogue.TurnsCount != 0 {
			t.Fatalf("dialogue = %+v", out.Dialogue)
		}
	})

	if b, _ := json.Marshal(AttemptsToPublic(nil, now)); string(b) != "[]" {
		t.Fatalf("AttemptsToPublic(nil) = %s", b)
	}
}

func TestAttemptRowHelpers(t *testing.T) {
	t.Parallel()

	teacher, author := uuid.New(), uuid.New()
	a := AttemptRow{LessonCreatedBy: author}
	if a.LessonOwnerID() != author {
		t.Fatal("практика")
	}
	a.LessonTeacherID = &teacher
	if a.LessonOwnerID() != teacher {
		t.Fatal("занятие")
	}
	if a.VoiceEnabled() || a.CallEnded() {
		t.Fatal("по умолчанию выключено")
	}
	a.LessonSettings.Voice.Enabled = true
	now := time.Now()
	a.CallEndedAt = &now
	if !a.VoiceEnabled() || !a.CallEnded() {
		t.Fatal("флаги")
	}

	for _, raw := range []string{"", "null", "{}", `{"битая`, `"строка"`} {
		a.Card = json.RawMessage(raw)
		if a.DecodeCard() != nil {
			t.Errorf("DecodeCard(%q) != nil", raw)
		}
	}
}

func TestDialogueTurnViews(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 9, 24, 15, 0, 0, 0, time.FixedZone("MSK", 3*3600))
	conf := float32(0.93)
	dur := 2300
	op := DialogueTurnRow{TurnNo: 1, Speaker: "operator", Text: "112, что случилось?", Source: "stt", Confidence: &conf,
		At: at, AtMs: 4100, EmotionalState: convert.Ptr("спокоен"), DurationMs: &dur}
	caller := DialogueTurnRow{TurnNo: 1, Speaker: "caller", Text: "Пожар!", Source: "llm", Confidence: &conf,
		AudioPath: convert.Ptr("ab/голос 1.ogg"), DurationMs: &dur, At: at, AtMs: 6000,
		EmotionalState: convert.Ptr("паника"), RevealedFactIDs: []string{"f1"}}
	silent := DialogueTurnRow{TurnNo: 0, Speaker: "caller", Text: "Алло", Source: "script", AudioPath: convert.Ptr(""), EmotionalState: convert.Ptr(" ")}

	v := DialogueTurnToView(&op)
	m := asJSONMap(t, v)
	requireKeys(t, "DialogueTurnView", m, reqTurnView)
	if v.Confidence == nil || *v.Confidence != conf || v.EmotionalState != nil || v.Audio != nil || v.At.Location() != time.UTC {
		t.Fatalf("operator view = %+v (confidence только у оператора, эмоция — только у заявителя)", v)
	}

	v = DialogueTurnToView(&caller)
	if v.Confidence != nil || convert.Deref(v.EmotionalState) != "паника" || v.Audio == nil ||
		v.Audio.AudioUrl != "/api/v1/media/tts/ab/%D0%B3%D0%BE%D0%BB%D0%BE%D1%81%201.ogg" || v.Audio.DurationMs != 2300 ||
		convert.Deref(v.Audio.Mime) != "audio/ogg" {
		t.Fatalf("caller view = %+v audio=%+v", v, v.Audio)
	}

	v = DialogueTurnToView(&silent)
	if v.Audio != nil || v.EmotionalState != nil {
		t.Fatalf("без аудио: %+v", v)
	}
	caller2 := caller
	caller2.AudioPath = convert.Ptr("x/y.flac")
	caller2.DurationMs = nil
	v = DialogueTurnToView(&caller2)
	if v.Audio == nil || v.Audio.Mime != nil || v.Audio.DurationMs != 0 {
		t.Fatalf("неизвестный формат: %+v", v.Audio)
	}

	c := DialogueTurnToContract(&op)
	if c.TurnNo != 2 || c.Confidence == nil || c.RevealedFactIds != nil || convert.Deref(c.AtMs) != 4100 || string(convert.Deref(c.Source)) != "stt" {
		t.Fatalf("operator contract = %+v", c)
	}
	c = DialogueTurnToContract(&caller)
	if c.TurnNo != 3 || c.Confidence != nil || c.RevealedFactIds == nil || (*c.RevealedFactIds)[0] != "f1" || convert.Deref(c.AudioDurationMs) != 2300 {
		t.Fatalf("caller contract = %+v", c)
	}
	if c = DialogueTurnToContract(&silent); c.TurnNo != 1 || c.RevealedFactIds != nil {
		t.Fatalf("вступление = %+v", c)
	}

	if b, _ := json.Marshal(DialogueTurnsToView(nil)); string(b) != "[]" {
		t.Fatalf("views(nil) = %s", b)
	}
	if b, _ := json.Marshal(DialogueTurnsToContract(nil)); string(b) != "[]" {
		t.Fatalf("contract(nil) = %s", b)
	}
	if got := DialogueTurnsToContract([]DialogueTurnRow{silent, op, caller}); got[0].TurnNo != 1 || got[1].TurnNo != 2 || got[2].TurnNo != 3 {
		t.Fatalf("сквозная нумерация: %+v", got)
	}
}

// defaultLessonSettings = миграционные дефолты (settings.Defaults), не живой снимок.
func TestDefaultLessonSettingsVar(t *testing.T) {
	t.Parallel()

	d := settings.Defaults()
	if defaultLessonSettings.PassThreshold != d.PassThreshold || defaultLessonSettings.Voice != d.Voice {
		t.Fatalf("defaultLessonSettings = %+v", defaultLessonSettings)
	}
}
