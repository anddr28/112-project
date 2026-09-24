package auth

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
)

// demoUser — учебная учётная запись демо-стенда (frontend/src/shared/mocks/fixtures/users.ts,
// DEMO_ACCOUNTS). Пароль совпадает с логином.
type demoUser struct {
	login, role             string
	last, first, middle     string
	serviceCode             string // services.code; "" — без профиля
	workstation, operatorNo string
	label                   string // подпись в /auth/demo-accounts
}

// Порядок — как в фикстурах фронта (он же порядок кнопок на экране входа).
var demoUsers = []demoUser{
	{login: "teacher", role: "teacher", last: "Ковалёва", first: "Ирина", middle: "Сергеевна",
		label: "Преподаватель · Ковалёва И. С."},
	{login: "student", role: "student", last: "Рожкова", first: "Ольга", middle: "Ивановна",
		serviceCode: "zhkh", workstation: "АРМ 4", operatorNo: "оп. 227",
		label: "Обучающийся · Рожкова О. И."},
	{login: "student2", role: "student", last: "Никитин", first: "Павел", middle: "Андреевич",
		serviceCode: "zhkh", workstation: "АРМ 7", operatorNo: "оп. 315",
		label: "Обучающийся · Никитин П. А."},
	{login: "admin", role: "admin", last: "Глущенко", first: "Олег", middle: "Игоревич",
		label: "Администратор · Глущенко О. И."},
}

const (
	demoGroupName = "Группа ДДС-1"
	demoGroupDesc = "Учебная группа демо-стенда"
)

// Профиль обновляется при каждом сиде (фикстуры — источник истины демо-стенда), пароль и
// статус — нет: пароль ставится только при вставке (админ мог его сменить), блокировку,
// выставленную на демонстрации, рестарт снимать не должен.
const seedUserSQL = `
INSERT INTO users (id, login, password_hash, role, last_name, first_name, middle_name, service_id,
                   workstation, operator_no, must_change_password, consent_given_at, password_changed_at)
VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), (SELECT id FROM services WHERE code = NULLIF($8, '')),
        NULLIF($9, ''), NULLIF($10, ''), false, now(), now())
ON CONFLICT (login) WHERE deleted_at IS NULL DO UPDATE SET
  role        = EXCLUDED.role,
  last_name   = EXCLUDED.last_name,
  first_name  = EXCLUDED.first_name,
  middle_name = EXCLUDED.middle_name,
  service_id  = EXCLUDED.service_id,
  workstation = EXCLUDED.workstation,
  operator_no = EXCLUDED.operator_no
RETURNING id`

const seedGroupSQL = `
WITH existing AS (
  SELECT id FROM groups WHERE name = $1 AND teacher_id = $2 AND archived_at IS NULL ORDER BY created_at LIMIT 1
), ins AS (
  INSERT INTO groups (id, name, description, teacher_id)
  SELECT $3, $1, $4, $2 WHERE NOT EXISTS (SELECT 1 FROM existing)
  RETURNING id
)
SELECT id FROM existing UNION ALL SELECT id FROM ins`

// SeedDemoUsers — идемпотентный сид демо-учёток и группы «Группа ДДС-1» (оба обучающихся
// в группе преподавателя). argon2 считается только для логинов, которых ещё нет в БД, —
// повторный старт не тратит ~0.1 с CPU на хэши.
func SeedDemoUsers(ctx context.Context, pool *pgxpool.Pool) error {
	logins := make([]string, len(demoUsers))
	for i, u := range demoUsers {
		logins[i] = u.login
	}
	existing := make(map[string]bool, len(demoUsers))
	rows, err := pool.Query(ctx, `SELECT lower(login::text) FROM users WHERE lower(login::text) = ANY($1::text[]) AND deleted_at IS NULL`, logins)
	if err != nil {
		return fmt.Errorf("auth: seed: existing users: %w", err)
	}
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			rows.Close()
			return fmt.Errorf("auth: seed: scan: %w", err)
		}
		existing[l] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("auth: seed: existing users: %w", err)
	}

	hashes := make([]string, len(demoUsers))
	for i, u := range demoUsers {
		if existing[u.login] {
			continue // пароль при конфликте не обновляется — хэш не нужен
		}
		if hashes[i], err = HashPasswordCtx(ctx, u.login); err != nil {
			return fmt.Errorf("auth: seed: hash: %w", err)
		}
	}

	return pg.WithTx(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		userIDs := make(map[string]uuid.UUID, len(demoUsers))
		for i, u := range demoUsers {
			hash := hashes[i]
			if hash == "" {
				hash = "!" // не используется: строка уже есть, ON CONFLICT не трогает password_hash
			}
			var id uuid.UUID
			if err := tx.QueryRow(ctx, seedUserSQL, ids.New(), u.login, hash, u.role, u.last, u.first, u.middle,
				u.serviceCode, u.workstation, u.operatorNo).Scan(&id); err != nil {
				return fmt.Errorf("auth: seed user %s: %w", u.login, err)
			}
			userIDs[u.login] = id
		}

		var groupID uuid.UUID
		if err := tx.QueryRow(ctx, seedGroupSQL, demoGroupName, userIDs["teacher"], ids.New(), demoGroupDesc).Scan(&groupID); err != nil {
			return fmt.Errorf("auth: seed group: %w", err)
		}
		members := []uuid.UUID{userIDs["student"], userIDs["student2"]}
		if _, err := tx.Exec(ctx, `INSERT INTO group_members (group_id, user_id) SELECT $1, unnest($2::uuid[]) ON CONFLICT DO NOTHING`,
			groupID, members); err != nil {
			return fmt.Errorf("auth: seed group members: %w", err)
		}
		return nil
	})
}
