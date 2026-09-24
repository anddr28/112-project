package auth

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/httpx"
	"lct/gocore/internal/platform/ids"
	"lct/gocore/internal/platform/pg"
)

// Register — маршруты /auth/* (относительно /api/v1).
func (s *Service) Register(r *httpx.Router) {
	r.Handle("POST /auth/login", httpx.Public, s.login)
	r.Handle("POST /auth/logout", httpx.Authenticated, s.logout)
	r.Handle("GET /auth/me", httpx.Authenticated, s.me)
	r.Handle("GET /auth/demo-accounts", httpx.Public, s.demoAccounts)
}

// ---------------------------------------------------------------- login

const loginLookupSQL = `
SELECT id, password_hash, role, status, locked_until, last_name, first_name,
       COALESCE(middle_name, ''), COALESCE(operator_no, ''), login::text
  FROM users
 WHERE login = $1 AND deleted_at IS NULL`

// Неудачная попытка: счётчик +1; на пороге — блокировка на LockMinutes и сброс счётчика.
// Правые части UPDATE видят старую строку, поэтому оба CASE считают от одного значения.
const loginFailSQL = `
UPDATE users SET
  failed_login_count = CASE WHEN failed_login_count + 1 >= $2 THEN 0 ELSE failed_login_count + 1 END,
  locked_until       = CASE WHEN failed_login_count + 1 >= $2 THEN now() + make_interval(mins => $3) ELSE locked_until END
WHERE id = $1
RETURNING locked_until`

// Успех: сброс счётчиков + новая сессия — одним оператором (атомарно, один round-trip).
const loginSuccessSQL = `
WITH u AS (
  UPDATE users SET failed_login_count = 0, locked_until = NULL, last_login_at = now() WHERE id = $1
)
INSERT INTO auth_sessions (id, user_id, refresh_token_hash, ip, user_agent, expires_at, last_seen_at)
VALUES ($2, $1, $3, $4::text::inet, $5, $6, now())`

type loginAudit struct {
	Login       string     `json:"login,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	LockedUntil *time.Time `json:"lockedUntil,omitempty"`
	SessionID   *uuid.UUID `json:"sessionId,omitempty"`
}

func (s *Service) login(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	var in public.LoginRequest
	if err := httpx.ReadJSON(r, &in); err != nil {
		return err
	}
	in.Login = strings.TrimSpace(in.Login)
	fields := map[string]string{}
	switch {
	case in.Login == "":
		fields["login"] = "Введите логин"
	case utf8.RuneCountInString(in.Login) > maxLoginLen:
		fields["login"] = "Слишком длинный логин"
	case strings.ContainsRune(in.Login, 0):
		// PostgreSQL не принимает NUL в тексте (поиск упал бы 500); такого логина быть не может.
		fields["login"] = "Логин содержит недопустимые символы"
	}
	if in.Password == "" {
		fields["password"] = "Введите пароль"
	} else if len(in.Password) > MaxPasswordLen {
		fields["password"] = "Слишком длинный пароль"
	}
	if len(fields) > 0 {
		return httpx.Validation("Введите логин и пароль", fields)
	}

	ip := httpx.ClientIP(r)
	now := time.Now()
	lb, retry := s.limiter.acquire(ip, now)
	if lb == nil {
		return httpx.TooManyRequests("Слишком много попыток входа. Повторите позже.", retry)
	}
	// Жетон лимита списывается только за неудачную проверку (failed=true ниже). Верный пароль,
	// ошибка сервера или отмена запроса попыткой подбора не считаются: вердикта клиент не получил.
	failed := false
	defer func() { s.limiter.release(lb, time.Now(), failed) }()

	var (
		userID             uuid.UUID
		hash, role, status string
		lockedUntil        *time.Time
		p                  core.Principal
	)
	err := s.pool.QueryRow(ctx, loginLookupSQL, in.Login).Scan(&userID, &hash, &role, &status, &lockedUntil,
		&p.LastName, &p.FirstName, &p.MiddleName, &p.OperatorNo, &p.Login)
	if err != nil {
		if !pg.IsNoRows(err) {
			return httpx.Internal(fmt.Errorf("auth: login lookup: %w", err))
		}
		failed = true
		return s.failUnknownLogin(ctx, in.Login, in.Password, now)
	}
	actor := userID
	actorRole := core.Role(role)

	// Учётка временно заблокирована после серии неудач. Верность пароля во время блокировки
	// не раскрываем (иначе перебор продолжался бы сквозь lockout), но argon2 всё равно считаем:
	// по времени ответа блокировка не отличается от проверки пароля.
	if lockedUntil != nil && lockedUntil.After(now) {
		failed = true
		burnVerify(ctx, in.Password)
		s.audit.Log(ctx, core.AuditEntry{Action: "user.login_failed", EntityType: "user", EntityID: userID,
			ActorID: &actor, ActorRole: actorRole, After: loginAudit{Reason: "locked", LockedUntil: lockedUntil}})
		return lockedError(*lockedUntil, now)
	}

	ok, err := VerifyPasswordCtx(ctx, hash, in.Password)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Испорченный хэш в БД — войти нельзя, но это не вина пользователя: в лог, наружу — 401.
		s.log.Error("auth: bad password hash", "user", userID, "err", err)
		ok = false
	}
	if !ok {
		failed = true
		ls := s.loginSettings(ctx)
		var newLock *time.Time
		if err := s.pool.QueryRow(ctx, loginFailSQL, userID, max(1, ls.MaxFailed), max(1, ls.LockMinutes)).Scan(&newLock); err != nil {
			return httpx.Internal(fmt.Errorf("auth: login fail update: %w", err))
		}
		a := loginAudit{Reason: "wrong_password"}
		if newLock != nil && newLock.After(now) {
			a.LockedUntil = newLock
		}
		s.audit.Log(ctx, core.AuditEntry{Action: "user.login_failed", EntityType: "user", EntityID: userID,
			ActorID: &actor, ActorRole: actorRole, After: a})
		return badCredentialsError()
	}

	if status == "blocked" {
		s.audit.Log(ctx, core.AuditEntry{Action: "user.login_failed", EntityType: "user", EntityID: userID,
			ActorID: &actor, ActorRole: actorRole, After: loginAudit{Reason: "blocked"}})
		return httpx.UserBlocked()
	}

	token, err := newToken()
	if err != nil {
		return httpx.Internal(err)
	}
	key, hashHex := hashToken(token)
	sessID := ids.New()
	ttl := s.sessionTTL(ctx)
	expires := now.Add(ttl)
	var ipArg *string
	if a, err := netip.ParseAddr(ip); err == nil {
		v := a.Unmap().String()
		ipArg = &v
	}
	ua := r.UserAgent()
	if len(ua) > 512 {
		ua = truncateRunes(ua, 512)
	}
	if _, err := s.pool.Exec(ctx, loginSuccessSQL, userID, sessID, hashHex, ipArg, nullIfEmpty(ua), expires); err != nil {
		return httpx.Internal(fmt.Errorf("auth: create session: %w", err))
	}

	// Сразу кладём сессию в кэш: первый же запрос SPA (/auth/me) обойдётся без БД.
	p.UserID, p.SessionID, p.Role = userID, sessID, actorRole
	e := &entry{principal: p, state: stateValid, loadedAt: time.Now().UnixNano()}
	e.expiresAt.Store(expires.UnixNano())
	e.touchedAt.Store(now.UnixNano())
	s.cache.put(key, e)

	u, err := s.loadUser(ctx, s.pool, userID)
	if err != nil {
		return httpx.Internal(fmt.Errorf("auth: load user: %w", err))
	}
	s.audit.Log(ctx, core.AuditEntry{Action: "user.login", EntityType: "user", EntityID: userID,
		ActorID: &actor, ActorRole: actorRole, After: loginAudit{SessionID: &sessID}})
	s.setCookie(w, token, ttl)
	httpx.WriteJSON(w, http.StatusOK, u)
	return nil
}

// ---------------------------------------------------------------- logout / me / demo

func (s *Service) logout(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.PrincipalFrom(ctx)
	if p == nil {
		return httpx.Unauthorized()
	}
	if _, err := s.pool.Exec(ctx, `UPDATE auth_sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, p.SessionID); err != nil {
		return httpx.Internal(fmt.Errorf("auth: logout: %w", err))
	}
	if token, ok := readToken(r); ok {
		key, _ := hashToken(token)
		s.cache.markDead(key, p.UserID)
	}
	s.audit.Log(ctx, core.AuditEntry{Action: "user.logout", EntityType: "user", EntityID: p.UserID,
		After: loginAudit{SessionID: &p.SessionID}})
	s.clearCookie(w)
	httpx.NoContent(w)
	return nil
}

// me — текущий пользователь. Заодно переставляет cookie со сроком, равным текущему сроку
// сессии в БД: срок скользит (maybeTouch), а Authenticate не видит ResponseWriter —
// SPA зовёт /auth/me при каждой загрузке, этого достаточно, чтобы cookie не истекла раньше сессии.
func (s *Service) me(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.PrincipalFrom(ctx)
	if p == nil {
		return httpx.Unauthorized()
	}
	u, err := s.loadUser(ctx, s.pool, p.UserID)
	if err != nil {
		if pg.IsNoRows(err) {
			return httpx.Unauthorized()
		}
		return httpx.Internal(fmt.Errorf("auth: me: %w", err))
	}
	if token, ok := readToken(r); ok {
		key, _ := hashToken(token)
		if e := s.cache.get(key); e != nil && e.state == stateValid {
			if left := time.Until(time.Unix(0, e.expiresAt.Load())); left > time.Minute {
				s.setCookie(w, token, left)
			}
		}
	}
	httpx.WriteJSON(w, http.StatusOK, u)
	return nil
}

func (s *Service) demoAccounts(w http.ResponseWriter, _ *http.Request) error {
	httpx.WriteRawJSON(w, http.StatusOK, s.demoJSON)
	return nil
}

// ---------------------------------------------------------------- helpers

// failUnknownLogin — ответ на несуществующий логин, неотличимый от ответа на существующий:
// то же время (argon2 по хэшу-пустышке), тот же 401 и та же блокировка (429) после MaxFailed
// неудач подряд — иначе перебором логинов видно, какие учётки есть.
func (s *Service) failUnknownLogin(ctx context.Context, login, password string, now time.Time) error {
	burnVerify(ctx, password)
	a := loginAudit{Login: truncateRunes(login, unknownLoginLen), Reason: "unknown_login"}
	if until, locked := s.ghosts.lockedUntil(login, now); locked {
		a.Reason, a.LockedUntil = "unknown_login_locked", &until
		s.audit.Log(ctx, core.AuditEntry{Action: "user.login_failed", EntityType: "user", After: a})
		return lockedError(until, now)
	}
	ls := s.loginSettings(ctx)
	if until, locked := s.ghosts.fail(login, now, max(1, ls.MaxFailed), time.Duration(max(1, ls.LockMinutes))*time.Minute); locked {
		a.LockedUntil = &until
	}
	s.audit.Log(ctx, core.AuditEntry{Action: "user.login_failed", EntityType: "user", After: a})
	return badCredentialsError()
}

// burnVerify — проверка пароля по хэшу-пустышке: выравнивает время ответа там, где настоящей
// проверки нет (неизвестный логин, учётка под блокировкой).
func burnVerify(ctx context.Context, password string) {
	_, _ = VerifyPasswordCtx(ctx, dummyHash(), password)
}

func badCredentialsError() *httpx.Error {
	return &httpx.Error{Status: http.StatusUnauthorized, Code: httpx.CodeUnauthorized, Message: badCredentials}
}

// lockedError — 429 «слишком много неудачных попыток» до момента until.
func lockedError(until, now time.Time) *httpx.Error {
	left := until.Sub(now)
	return httpx.TooManyRequests(fmt.Sprintf(lockMessage, max(1, int(math.Ceil(left.Minutes())))),
		max(1, int(math.Ceil(left.Seconds()))))
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
