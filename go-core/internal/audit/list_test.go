package audit

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/pgtest"
)

type listFixture struct {
	pool                    *pgxpool.Pool
	teacher, ghost, backup  uuid.UUID
	lesson                  uuid.UUID
	t0                      time.Time
	idLogin, idSystem, idLF int64
}

// seedAuditRows — записи с контролируемым at (прямой INSERT, как пишет COPY).
func seedAuditRows(t *testing.T) *listFixture {
	t.Helper()
	pool := pgtest.New(t)
	f := &listFixture{pool: pool, ghost: uuid.New(), backup: uuid.New(), lesson: uuid.New()}
	f.teacher = seedUser(t, pool, "sidorova", "teacher", "Сидорова", "Анна", "")
	f.t0 = time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	ins := func(at time.Time, actor *uuid.UUID, role, action, etype string, entity, lesson *uuid.UUID, before, after, ip string) int64 {
		t.Helper()
		var id int64
		nz := func(s string) *string {
			if s == "" {
				return nil
			}
			return &s
		}
		err := pool.QueryRow(ctxT(t), `
			INSERT INTO audit_log (at, actor_id, actor_role, action, entity_type, entity_id, lesson_id, before, after, ip)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9::jsonb, $10::inet) RETURNING id`,
			at, actor, nz(role), action, nz(etype), entity, lesson, nz(before), nz(after), nz(ip)).Scan(&id)
		if err != nil {
			t.Fatalf("seed audit: %v", err)
		}
		return id
	}
	f.idLogin = ins(f.t0, &f.teacher, "teacher", "user.login", "session", nil, nil, "", `{"sessionId":"s1"}`, "10.0.0.5")
	f.idSystem = ins(f.t0.Add(time.Minute), nil, RoleSystem, "backup.run", "backup", &f.backup, nil, "", `{"status":"running"}`, "")
	f.idLF = ins(f.t0.Add(2*time.Minute), &f.teacher, "teacher", "user.login_failed", "", nil, &f.lesson, `[1,2]`, "", "2001:db8::1")
	ins(f.t0.Add(3*time.Minute), &f.ghost, "admin", "a_b.x", "", nil, nil, "", "", "")
	ins(f.t0.Add(4*time.Minute), nil, "", "axb.x", "", nil, nil, "", `"строка"`, "")
	return f
}

func actions(list []public.AuditEntry) string {
	s := make([]string, len(list))
	for i, e := range list {
		s[i] = e.Action
	}
	return strings.Join(s, ",")
}

func TestListFilters(t *testing.T) {
	t.Parallel()
	f := seedAuditRows(t)
	ptr := func(u uuid.UUID) *uuid.UUID { return &u }
	tp := func(d time.Duration) *time.Time { x := f.t0.Add(d); return &x }
	tests := []struct {
		name string
		f    Filter
		want string
	}{
		{"all newest first", Filter{}, "axb.x,a_b.x,user.login_failed,backup.run,user.login"},
		{"actor", Filter{ActorID: &f.teacher}, "user.login_failed,user.login"},
		{"actor without rows", Filter{ActorID: ptr(uuid.New())}, ""},
		{"exact action", Filter{Action: "user.login"}, "user.login"},
		{"exact action trimmed", Filter{Action: "  backup.run "}, "backup.run"},
		{"prefix action", Filter{Action: "user."}, "user.login_failed,user.login"},
		{"prefix underscore is literal", Filter{Action: "a_b."}, "a_b.x"},
		{"entity type", Filter{EntityType: "backup"}, "backup.run"},
		{"entity id", Filter{EntityID: &f.backup}, "backup.run"},
		{"from inclusive", Filter{From: tp(2 * time.Minute)}, "axb.x,a_b.x,user.login_failed"},
		{"to inclusive", Filter{To: tp(time.Minute)}, "backup.run,user.login"},
		{"from-to", Filter{From: tp(time.Minute), To: tp(2 * time.Minute)}, "user.login_failed,backup.run"},
		{"before id", Filter{BeforeID: &f.idLF}, "backup.run,user.login"},
		{"limit", Filter{Limit: 2}, "axb.x,a_b.x"},
		{"limit negative = default", Filter{Limit: -1}, "axb.x,a_b.x,user.login_failed,backup.run,user.login"},
		{"limit over max clamps", Filter{Limit: MaxLimit * 10}, "axb.x,a_b.x,user.login_failed,backup.run,user.login"},
		{"combined", Filter{ActorID: &f.teacher, Action: "user.", To: tp(time.Minute)}, "user.login"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := List(ctxT(t), f.pool, tt.f)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil {
				t.Fatal("List вернул nil вместо пустого среза")
			}
			if a := actions(got); a != tt.want {
				t.Fatalf("got %q, want %q", a, tt.want)
			}
		})
	}
}

func TestListShapes(t *testing.T) {
	t.Parallel()
	f := seedAuditRows(t)
	list, err := List(ctxT(t), f.pool, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]public.AuditEntry{}
	for _, e := range list {
		by[e.Action] = e
	}

	login := by["user.login"]
	if login.Id != int(f.idLogin) || !login.At.Equal(f.t0) || login.At.Location() != time.UTC {
		t.Errorf("login id/at = %d/%v", login.Id, login.At)
	}
	if login.ActorName == nil || *login.ActorName != "Сидорова А." {
		t.Errorf("actorName = %v", login.ActorName)
	}
	if login.Ip == nil || *login.Ip != "10.0.0.5" || login.Before != nil || login.After == nil || (*login.After)["sessionId"] != "s1" {
		t.Errorf("login = %+v", login)
	}

	sys := by["backup.run"]
	if sys.ActorId != nil || sys.ActorName == nil || *sys.ActorName != SystemActorName || sys.EntityId == nil || *sys.EntityId != f.backup {
		t.Errorf("system = %+v", sys)
	}
	if sys.Ip != nil {
		t.Errorf("system ip = %v", *sys.Ip)
	}

	lf := by["user.login_failed"]
	if lf.Before == nil || (*lf.Before)["value"] == nil || lf.LessonId == nil || *lf.LessonId != f.lesson || lf.Ip == nil || *lf.Ip != "2001:db8::1" {
		t.Errorf("login_failed = %+v", lf)
	}
	if lf.EntityType != nil {
		t.Errorf("entityType = %v, want nil", *lf.EntityType)
	}

	ghost := by["a_b.x"]
	if ghost.ActorId == nil || *ghost.ActorId != f.ghost || ghost.ActorName != nil || ghost.ActorRole == nil || *ghost.ActorRole != "admin" {
		t.Errorf("ghost actor = %+v", ghost)
	}
	anon := by["axb.x"]
	if anon.ActorName != nil || anon.ActorRole != nil || anon.After == nil || (*anon.After)["value"] != "строка" {
		t.Errorf("anon = %+v", anon)
	}

	// Контракт AuditEntry: обязательные id, at, action; before/after — объекты.
	b, _ := json.Marshal(list)
	var raw []map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, e := range raw {
		for _, k := range []string{"id", "at", "action"} {
			if _, ok := e[k]; !ok {
				t.Errorf("нет обязательного %q в %v", k, e)
			}
		}
		for _, k := range []string{"before", "after"} {
			if v, ok := e[k]; ok {
				if _, isObj := v.(map[string]any); !isObj {
					t.Errorf("%s не объект: %v", k, v)
				}
			}
		}
	}
}

func TestListEmptyIsArray(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t)
	list, err := List(ctxT(t), pool, Filter{Action: "nothing."})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(list)
	if string(b) != "[]" {
		t.Fatalf("got %s, want []", b)
	}
}
