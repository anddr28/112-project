// Package access — единые правила доступа к попыткам и занятиям (RBAC уровня объекта).
// Роль проверяет Router; «чьё это» — здесь, одинаково для attempts, dialogue, evaluation,
// reports. Ответы — ошибки контракта: чужое для студента/преподавателя -> 403, нет — 404.
package access

import (
	"lct/gocore/internal/core"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/store"
)

// ViewAttempt — смотреть попытку: студент-владелец, преподаватель-владелец занятия, админ.
func ViewAttempt(p *core.Principal, a *store.AttemptRow) error {
	switch p.Role {
	case core.RoleAdmin:
		return nil
	case core.RoleStudent:
		if a.UserID == p.UserID {
			return nil
		}
	case core.RoleTeacher:
		if a.LessonOwnerID() == p.UserID {
			return nil
		}
	}
	return httpx.Forbidden("Нет доступа к этой попытке")
}

// OwnAttempt — действовать от имени обучающегося (ввод, разговор, сдача): только сам студент.
func OwnAttempt(p *core.Principal, a *store.AttemptRow) error {
	if p.Role == core.RoleStudent && a.UserID == p.UserID {
		return nil
	}
	return httpx.Forbidden("Действие доступно только обучающемуся, которому выдана карточка")
}

// ManageAttempt — действия преподавателя над попыткой (ручная оценка, комментарий):
// преподаватель-владелец занятия или админ.
func ManageAttempt(p *core.Principal, a *store.AttemptRow) error {
	if p.Role == core.RoleAdmin || (p.Role == core.RoleTeacher && a.LessonOwnerID() == p.UserID) {
		return nil
	}
	return httpx.Forbidden("Действие доступно преподавателю занятия")
}

// ViewLesson — смотреть занятие: владелец, админ, студент-участник.
func ViewLesson(p *core.Principal, l *store.LessonRow) error {
	switch p.Role {
	case core.RoleAdmin:
		return nil
	case core.RoleTeacher:
		if l.OwnerID() == p.UserID {
			return nil
		}
	case core.RoleStudent:
		for i := range l.Participants {
			if l.Participants[i].UserID == p.UserID {
				return nil
			}
		}
	}
	return httpx.Forbidden("Нет доступа к этому занятию")
}

// ManageLesson — управлять занятием: преподаватель-владелец или админ.
func ManageLesson(p *core.Principal, l *store.LessonRow) error {
	if p.Role == core.RoleAdmin || (p.Role == core.RoleTeacher && l.OwnerID() == p.UserID) {
		return nil
	}
	return httpx.Forbidden("Действие доступно преподавателю этого занятия")
}
