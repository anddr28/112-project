package access

import (
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/store"
)

var (
	teacherA = uuid.MustParse("018f0000-0000-7000-8000-00000000000a")
	teacherB = uuid.MustParse("018f0000-0000-7000-8000-00000000000b")
	student1 = uuid.MustParse("018f0000-0000-7000-8000-000000000001")
	student2 = uuid.MustParse("018f0000-0000-7000-8000-000000000002")
	adminID  = uuid.MustParse("018f0000-0000-7000-8000-0000000000ad")
)

func who(id uuid.UUID, r core.Role) *core.Principal { return &core.Principal{UserID: id, Role: r} }

// attempt — попытка student1 в занятии teacherA (класс) или практике student1 (teacher_id NULL).
func attempt(practice bool) *store.AttemptRow {
	a := &store.AttemptRow{ID: uuid.New(), UserID: student1, LessonCreatedBy: teacherA}
	if practice {
		a.LessonKind = core.LessonKindPractice
		a.LessonCreatedBy = student1
	} else {
		a.LessonKind = core.LessonKindClass
		t := teacherA
		a.LessonTeacherID = &t
	}
	return a
}

func lesson(practice bool) *store.LessonRow {
	l := &store.LessonRow{ID: uuid.New(), CreatedBy: teacherA,
		Participants: []store.ParticipantRow{{UserID: student1, LastName: "Иванов"}}}
	if practice {
		l.Kind = core.LessonKindPractice
		l.CreatedBy = student1
	} else {
		l.Kind = core.LessonKindClass
		t := teacherA
		l.TeacherID = &t
	}
	return l
}

func check(t *testing.T, name string, err error, allowed bool) {
	t.Helper()
	if allowed {
		if err != nil {
			t.Errorf("%s: запрещено, ожидался доступ: %v", name, err)
		}
		return
	}
	var he *httpx.Error
	if !errors.As(err, &he) || he.Status != http.StatusForbidden || he.Code != httpx.CodeForbidden || he.Message == "" {
		t.Errorf("%s: ожидался 403 forbidden, получено %v", name, err)
	}
}

func TestAttemptRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name              string
		p                 *core.Principal
		practice          bool
		view, own, manage bool
	}{
		{"студент-владелец", who(student1, core.RoleStudent), false, true, true, false},
		{"чужой студент", who(student2, core.RoleStudent), false, false, false, false},
		{"преподаватель занятия", who(teacherA, core.RoleTeacher), false, true, false, true},
		{"чужой преподаватель", who(teacherB, core.RoleTeacher), false, false, false, false},
		{"админ", who(adminID, core.RoleAdmin), false, true, false, true},
		// id владельца занятия совпадает, но роль не преподавательская — управлять нельзя
		{"студент с id преподавателя", who(teacherA, core.RoleStudent), false, false, false, false},
		{"неизвестная роль", who(student1, core.Role("root")), false, false, false, false},
		// практика: teacher_id NULL, владелец — автор (сам студент)
		{"практика: автор-студент", who(student1, core.RoleStudent), true, true, true, false},
		{"практика: преподаватель", who(teacherA, core.RoleTeacher), true, false, false, false},
		{"практика: админ", who(adminID, core.RoleAdmin), true, true, false, true},
	}
	for _, c := range cases {
		a := attempt(c.practice)
		check(t, c.name+"/view", ViewAttempt(c.p, a), c.view)
		check(t, c.name+"/own", OwnAttempt(c.p, a), c.own)
		check(t, c.name+"/manage", ManageAttempt(c.p, a), c.manage)
	}
}

func TestLessonRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		p            *core.Principal
		practice     bool
		view, manage bool
	}{
		{"участник", who(student1, core.RoleStudent), false, true, false},
		{"не участник", who(student2, core.RoleStudent), false, false, false},
		{"владелец", who(teacherA, core.RoleTeacher), false, true, true},
		{"чужой преподаватель", who(teacherB, core.RoleTeacher), false, false, false},
		{"админ", who(adminID, core.RoleAdmin), false, true, true},
		{"студент с id владельца", who(teacherA, core.RoleStudent), false, false, false},
		{"практика: автор", who(student1, core.RoleStudent), true, true, false},
		{"практика: преподаватель-не автор", who(teacherA, core.RoleTeacher), true, false, false},
	}
	for _, c := range cases {
		l := lesson(c.practice)
		check(t, c.name+"/view", ViewLesson(c.p, l), c.view)
		check(t, c.name+"/manage", ManageLesson(c.p, l), c.manage)
	}
	// занятие без участников: студенту — 403, а не паника
	empty := lesson(false)
	empty.Participants = nil
	check(t, "без участников", ViewLesson(who(student1, core.RoleStudent), empty), false)
}
