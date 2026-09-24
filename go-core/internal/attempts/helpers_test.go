package attempts

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/core"
	"lct/gocore/internal/eventlog"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/pgtest"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// ---------------------------------------------------------------- аутентификация

// stubAuth — субъект из заголовка X-Test-Principal ("role:uuid"); нет заголовка — 401.
type stubAuth struct{}

const principalHeader = "X-Test-Principal"

func (stubAuth) Authenticate(r *http.Request) (*core.Principal, error) {
	h := r.Header.Get(principalHeader)
	role, id, ok := strings.Cut(h, ":")
	if !ok {
		return nil, core.ErrUnauthenticated
	}
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, core.ErrUnauthenticated
	}
	return &core.Principal{UserID: uid, Role: core.Role(role), LastName: "Иванова", FirstName: "Анна",
		OperatorNo: "оп. 227"}, nil
}

func who(role core.Role, id uuid.UUID) *core.Principal {
	return &core.Principal{UserID: id, Role: role}
}

// ---------------------------------------------------------------- порты-фейки

type monitorMsg struct {
	lesson uuid.UUID
	msg    public.MonitorMessage
}

type recPublisher struct {
	mu      sync.Mutex
	monitor []monitorMsg
}

func (p *recPublisher) Monitor(lessonID uuid.UUID, m public.MonitorMessage) {
	p.mu.Lock()
	p.monitor = append(p.monitor, monitorMsg{lessonID, m})
	p.mu.Unlock()
}
func (p *recPublisher) Student(uuid.UUID, public.StudentMessage) {}

// count — сколько сообщений типа typ ушло в мониторинг занятия (для attemptEvent — с типом события evType).
func (p *recPublisher) count(typ public.MonitorMessageType, evType string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, m := range p.monitor {
		if m.msg.Type != typ {
			continue
		}
		if evType != "" && (m.msg.Event == nil || string(m.msg.Event.Type) != evType) {
			continue
		}
		n++
	}
	return n
}

type recAuditor struct {
	mu      sync.Mutex
	entries []core.AuditEntry
}

func (a *recAuditor) Log(_ context.Context, e core.AuditEntry) {
	a.mu.Lock()
	a.entries = append(a.entries, e)
	a.mu.Unlock()
}

func (a *recAuditor) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.entries))
	for i, e := range a.entries {
		out[i] = e.Action
	}
	return out
}

type fakeEvaluator struct {
	mu    sync.Mutex
	calls []uuid.UUID
}

func (f *fakeEvaluator) StartEvaluation(_ context.Context, _ pgx.Tx, id uuid.UUID) error {
	f.mu.Lock()
	f.calls = append(f.calls, id)
	f.mu.Unlock()
	return nil
}

func (f *fakeEvaluator) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type fakeIssuer struct {
	mu     sync.Mutex
	issue  bool
	next   uuid.UUID
	called int
}

func (f *fakeIssuer) IssueNext(context.Context, pgx.Tx, uuid.UUID, uuid.UUID) (uuid.UUID, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called++
	return f.next, f.issue, nil
}

type fakeDialogue struct {
	mu      sync.Mutex
	opening *public.DialogueTurnView
	closed  []uuid.UUID
}

func (f *fakeDialogue) OnCallAccepted(context.Context, pgx.Tx, uuid.UUID) (*public.DialogueTurnView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.opening == nil {
		return nil, nil
	}
	o := *f.opening
	return &o, nil
}

func (f *fakeDialogue) CloseOnSubmit(_ context.Context, _ pgx.Tx, id uuid.UUID, _ time.Time) error {
	f.mu.Lock()
	f.closed = append(f.closed, id)
	f.mu.Unlock()
	return nil
}

// fakeCatalog — справочник служб 101/102/103 (как у ДДС Москвы).
type fakeCatalog struct{}

var catalogServices = map[string]core.ServiceInfo{
	"101": {ID: "svc-101", Code: "101", Name: "Пожарная охрана", ShortName: "01 Пожарные", Kind: "emergency"},
	"102": {ID: "svc-102", Code: "102", Name: "Полиция", ShortName: "02 Полиция", Kind: "emergency"},
	"103": {ID: "svc-103", Code: "103", Name: "Скорая медицинская помощь", ShortName: "03 Скорая", Kind: "emergency"},
}

func (fakeCatalog) TypeByID(string) (core.IncidentTypeInfo, bool) {
	return core.IncidentTypeInfo{}, false
}
func (fakeCatalog) TypeByCode(string) (core.IncidentTypeInfo, bool) {
	return core.IncidentTypeInfo{}, false
}
func (fakeCatalog) ServiceByID(id string) (core.ServiceInfo, bool) {
	for _, s := range catalogServices {
		if s.ID == id {
			return s, true
		}
	}
	return core.ServiceInfo{}, false
}
func (fakeCatalog) ServiceByCode(code string) (core.ServiceInfo, bool) {
	s, ok := catalogServices[code]
	return s, ok
}
func (fakeCatalog) FieldLabel(string) string                          { return "Поле карточки" }
func (fakeCatalog) AttributeLabel(string) (string, bool)              { return "", false }
func (fakeCatalog) AttributeValueLabel(string, string) (string, bool) { return "", false }

// ---------------------------------------------------------------- окружение

type env struct {
	t      *testing.T
	pool   *pgxpool.Pool
	svc    *Service
	mux    *http.ServeMux
	pub    *recPublisher
	aud    *recAuditor
	eval   *fakeEvaluator
	issuer *fakeIssuer
	dlg    *fakeDialogue
}

// newRouter — маршруты пакета поверх stubAuth.
func newRouter(s *Service) *http.ServeMux {
	mux := http.NewServeMux()
	r := httpx.NewRouter(mux, "/api/v1", stubAuth{}, quietLog(), nil)
	s.Register(r)
	return mux
}

// newEnv — сервис на свежей БД (pgtest; нет PostgreSQL — t.Skip) с запущенным group commit событий.
func newEnv(t *testing.T) *env {
	t.Helper()
	pool := pgtest.New(t)
	w := eventlog.NewWriter(pool, quietLog())
	w.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.Stop(ctx)
	})
	e := &env{t: t, pool: pool, pub: &recPublisher{}, aud: &recAuditor{}, eval: &fakeEvaluator{},
		issuer: &fakeIssuer{}, dlg: &fakeDialogue{}}
	e.svc = New(Deps{Pool: pool, Catalog: fakeCatalog{}, Events: w, Publisher: e.pub, Auditor: e.aud,
		Evaluator: e.eval, Issuer: e.issuer, Dialogue: e.dlg, Log: quietLog()})
	e.mux = newRouter(e.svc)
	return e
}

type resp struct {
	code int
	body []byte
}

func (r resp) json(t *testing.T, dst any) {
	t.Helper()
	if err := json.Unmarshal(r.body, dst); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

// obj — тело ответа как карта (проверка формы контракта: ключи, [] вместо null).
func (r resp) obj(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	r.json(t, &m)
	return m
}

// apiError — code/message/details ApiError.
func (r resp) apiError(t *testing.T) (code, msg string, fields map[string]any) {
	t.Helper()
	var e struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	}
	r.json(t, &e)
	if f, ok := e.Details["fields"].(map[string]any); ok {
		fields = f
	}
	return e.Code, e.Message, fields
}

func serve(mux http.Handler, p *core.Principal, method, path string, body any) resp {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			panic(err)
		}
		rd = bytes.NewReader(buf)
	}
	req := httptest.NewRequest(method, "/api/v1"+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Requested-With", "fetch")
	if p != nil {
		req.Header.Set(principalHeader, string(p.Role)+":"+p.UserID.String())
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return resp{code: rec.Code, body: rec.Body.Bytes()}
}

func (e *env) do(p *core.Principal, method, path string, body any) resp {
	return serve(e.mux, p, method, path, body)
}

// expect — статус ответа (и code ApiError для ошибок).
func expect(t *testing.T, r resp, status int, code string) {
	t.Helper()
	if r.code != status {
		t.Fatalf("status = %d, want %d; body %s", r.code, status, r.body)
	}
	if code != "" {
		if got, _, _ := r.apiError(t); got != code {
			t.Fatalf("code = %q, want %q; body %s", got, code, r.body)
		}
	}
}

// ---------------------------------------------------------------- данные

// Легенда сценария: вступление озвучено (tts_cache h-open), подсказка оператору, пустая
// реплика (в текстовом режиме пропускается) и вторая реплика заявителя.
const testCallScript = `{
  "caller": {"name": "Мария Петровна", "phone": "+79991234567", "role": "очевидец", "emotional_state": "паника", "voice": "baya"},
  "address": {"raw": "Москва, Тверская, 5"},
  "key_facts": ["СЕКРЕТНЫЙ-ФАКТ: газовый баллон на балконе"],
  "dialogue": {"persona": "СЕКРЕТНЫЙ-БРИФ", "facts": [{"id": "f1", "text": "баллон", "reveal": "on_request"}]},
  "turns": [
    {"speaker": "caller", "text": "Алло! Горит квартира на Тверской, дом пять!", "tts_hash": "h-open"},
    {"speaker": "operator_hint", "text": "Уточните этаж и подъезд"},
    {"speaker": "caller", "text": "   "},
    {"speaker": "caller", "text": "Пятый этаж, второй подъезд.", "tts_hash": "h-missing"}
  ]
}`

type fixture struct {
	teacher, otherTeacher, student, student2, admin uuid.UUID
	lesson, scenario, etalon                        uuid.UUID
}

func (f fixture) asStudent() *core.Principal  { return who(core.RoleStudent, f.student) }
func (f fixture) asStudent2() *core.Principal { return who(core.RoleStudent, f.student2) }
func (f fixture) asTeacher() *core.Principal  { return who(core.RoleTeacher, f.teacher) }
func (f fixture) asOther() *core.Principal    { return who(core.RoleTeacher, f.otherTeacher) }
func (f fixture) asAdmin() *core.Principal    { return who(core.RoleAdmin, f.admin) }

type lessonOpts struct {
	status      string // default running
	voice       bool
	allowReplay *bool
}

func mustExec(t *testing.T, pool *pgxpool.Pool, q string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("exec %q: %v", firstLine(q), err)
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}

func insertUser(t *testing.T, pool *pgxpool.Pool, role, last string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO users (login, password_hash, role, last_name, first_name, middle_name)
VALUES ($1::text || '_' || gen_random_uuid(), 'x', $1::text, $2, 'Анна', 'Сергеевна') RETURNING id`, role, last).Scan(&id)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

// seed — преподаватель (владелец занятия), чужой преподаватель, два студента, админ, сценарий
// с эталоном и tts_cache вступления, занятие с участниками.
func seed(t *testing.T, pool *pgxpool.Pool, o lessonOpts) fixture {
	t.Helper()
	ctx := context.Background()
	f := fixture{
		teacher:      insertUser(t, pool, "teacher", "Петров"),
		otherTeacher: insertUser(t, pool, "teacher", "Сидоров"),
		student:      insertUser(t, pool, "student", "Иванова"),
		student2:     insertUser(t, pool, "student", "Смирнов"),
		admin:        insertUser(t, pool, "admin", "Админов"),
	}
	if o.status == "" {
		o.status = "running"
	}
	settings := map[string]any{"voice": map[string]any{"enabled": o.voice, "input": "both", "max_turns": 12}}
	if o.allowReplay != nil {
		settings["allow_replay"] = *o.allowReplay
	}
	st, _ := json.Marshal(settings)
	err := pool.QueryRow(ctx, `
WITH c AS (
  INSERT INTO classifier_categories (code, name) VALUES ('c_' || gen_random_uuid(), 'Пожар') RETURNING id
), sc AS (
  INSERT INTO scenarios (title, category_id, source, status, call_script)
  SELECT 'Пожар в квартире', c.id, 'manual', 'validated', $1::jsonb FROM c RETURNING id
), e AS (
  INSERT INTO etalons (scenario_id, card) SELECT sc.id, '{}' FROM sc RETURNING id, scenario_id
), l AS (
  INSERT INTO lessons (title, teacher_id, created_by, mode, status, settings, started_at)
  VALUES ('Практическое занятие: пожары', $2, $2, 'cards', $3, $4::jsonb, now()) RETURNING id
)
SELECT l.id, e.scenario_id, e.id FROM l, e`, testCallScript, f.teacher, o.status, string(st)).Scan(&f.lesson, &f.scenario, &f.etalon)
	if err != nil {
		t.Fatalf("seed lesson: %v", err)
	}
	mustExec(t, pool, `INSERT INTO lesson_participants (lesson_id, user_id) VALUES ($1, $2), ($1, $3)`, f.lesson, f.student, f.student2)
	mustExec(t, pool, `INSERT INTO tts_cache (text_hash, voice, file_path, duration_ms)
VALUES ('h-open', 'baya', 'ab/h-open.ogg', 3100) ON CONFLICT DO NOTHING`)
	return f
}

// newAttempt — попытка студента в статусе issued с пустой карточкой (как IssueNext).
func (f fixture) newAttempt(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO attempts (lesson_id, user_id, scenario_id, etalon_id, mode, seq_no, time_limit_sec)
VALUES ($1, $2, $3, $4, 'cards', (SELECT COALESCE(max(seq_no), 0) + 1 FROM attempts WHERE lesson_id = $1 AND user_id = $2), 30)
RETURNING id`, f.lesson, userID, f.scenario, f.etalon).Scan(&id)
	if err != nil {
		t.Fatalf("insert attempt: %v", err)
	}
	mustExec(t, pool, `INSERT INTO attempt_drafts (attempt_id, data) VALUES ($1, $2::jsonb)`, id, string(emptyDraftJSON))
	return id
}

// accepted — попытка, по которой студент уже принял вызов (через API).
func (e *env) accepted(f fixture, userID uuid.UUID) uuid.UUID {
	e.t.Helper()
	id := f.newAttempt(e.t, e.pool, userID)
	r := e.do(who(core.RoleStudent, userID), http.MethodPost, "/attempts/"+id.String()+"/accept-call", nil)
	expect(e.t, r, http.StatusOK, "")
	return id
}

func (e *env) setStatus(id uuid.UUID, status string) {
	e.t.Helper()
	mustExec(e.t, e.pool, `UPDATE attempts SET status = $2 WHERE id = $1`, id, status)
	// кэш метаданных пакета доверяет только терминальным статусам; сбрасываем для чистоты проверки
	e.svc.meta = newMetaCache(metaMaxEntries)
}

// draft — черновик из БД как карточка.
func (e *env) draft(id uuid.UUID) public.IncidentCardDraft {
	e.t.Helper()
	var raw []byte
	if err := e.pool.QueryRow(context.Background(), `SELECT data FROM attempt_drafts WHERE attempt_id = $1`, id).Scan(&raw); err != nil {
		e.t.Fatalf("read draft: %v", err)
	}
	var d public.IncidentCardDraft
	if err := json.Unmarshal(raw, &d); err != nil {
		e.t.Fatalf("decode draft %s: %v", raw, err)
	}
	return d
}

func (e *env) getDraft(f fixture, id uuid.UUID) public.IncidentCardDraft {
	e.t.Helper()
	r := e.do(f.asStudent(), http.MethodGet, "/attempts/"+id.String()+"/draft", nil)
	expect(e.t, r, http.StatusOK, "")
	var d public.IncidentCardDraft
	r.json(e.t, &d)
	return d
}

type attemptTimes struct {
	status         string
	callAcceptedAt *time.Time
	firstInputAt   *time.Time
	submittedAt    *time.Time
	timeSpentMs    *int
	card           []byte
	actionText     *string
	replayCount    int
}

func (e *env) attemptRow(id uuid.UUID) attemptTimes {
	e.t.Helper()
	var a attemptTimes
	err := e.pool.QueryRow(context.Background(), `
SELECT status, call_accepted_at, first_input_at, submitted_at, time_spent_ms, card, action_text, replay_count
  FROM attempts WHERE id = $1`, id).Scan(&a.status, &a.callAcceptedAt, &a.firstInputAt, &a.submittedAt,
		&a.timeSpentMs, &a.card, &a.actionText, &a.replayCount)
	if err != nil {
		e.t.Fatalf("read attempt: %v", err)
	}
	return a
}

func (e *env) eventTypes(id uuid.UUID) []string {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT type FROM attempt_events WHERE attempt_id = $1 ORDER BY id`, id)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func codes(list []public.AssignedService) []string {
	out := make([]string, len(list))
	for i := range list {
		out[i] = list[i].Code
	}
	return out
}

func path(id uuid.UUID, suffix string) string { return "/attempts/" + id.String() + suffix }
