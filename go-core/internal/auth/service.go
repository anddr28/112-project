// Package auth — сессии по cookie lct_session, пароли (argon2id), rate limit и lockout входа,
// ручки /auth/login|logout|me|demo-accounts, демо-учётки.
//
// Горячий путь — Authenticate (каждый запрос API и WS): попадание в кэш — ноль обращений
// к БД и одна маленькая аллокация (копия Principal). Промах — один SELECT по уникальному
// индексу refresh_token_hash с JOIN users. Запись в БД (last_seen_at / скольжение срока) —
// не чаще раза в 5 минут на сессию.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"

	"lct/gocore/internal/config"
	"lct/gocore/internal/core"
	"lct/gocore/internal/gen/public"
	"lct/gocore/internal/platform/pg"
	"lct/gocore/internal/settings"
)

// CookieName — cookie сессии (frontend.v1.yaml, securitySchemes.SessionCookie).
const CookieName = "lct_session"

const (
	tokenBytes = 32
	tokenLen   = 43 // base64url без паддинга от 32 байт

	// touchEvery — не чаще этого пишем last_seen_at / продлеваем expires_at.
	touchEvery = 5 * time.Minute
	// loadTimeout — общий (singleflight) запрос сессии не зависит от отмены конкретного клиента.
	loadTimeout  = 5 * time.Second
	touchTimeout = 2 * time.Second

	loginPerMinute   = 5
	loginMaxInflight = 64 // одновременных проверок пароля с одного IP (класс за NAT входит разом)
	limiterMaxIPs    = 50000
	maxLoginLen      = 256
	unknownLoginLen  = 64 // сколько символов неизвестного логина попадает в аудит
	lockMessage      = "Слишком много неудачных попыток входа. Повторите через %d мин."
	badCredentials   = "Неверный логин или пароль"
	defaultTTLHours  = 12
	sessionUpdateSQL = `UPDATE auth_sessions SET last_seen_at = now(), expires_at = GREATEST(expires_at, $2)
		WHERE id = $1 AND revoked_at IS NULL`
)

// Deps — зависимости сервиса (связывает internal/app).
type Deps struct {
	Pool     *pgxpool.Pool
	Config   *config.Config
	Settings *settings.Store
	Auditor  core.Auditor
	Log      *slog.Logger
	// LoadUser — единственная сборка public.User (users.Get). auth не импортирует users.
	LoadUser func(ctx context.Context, q pg.Querier, id uuid.UUID) (*public.User, error)
}

// Service — core.Authenticator + ручки /auth/*.
type Service struct {
	pool     *pgxpool.Pool
	cfg      *config.Config
	settings *settings.Store
	audit    core.Auditor
	log      *slog.Logger
	loadUser func(ctx context.Context, q pg.Querier, id uuid.UUID) (*public.User, error)

	cache   *sessionCache
	limiter *ipLimiter
	ghosts  *ghostLocks // lockout несуществующих логинов (не палим, какие логины есть)
	sf      singleflight.Group

	demoJSON []byte // предкодированный ответ /auth/demo-accounts
}

var _ core.Authenticator = (*Service)(nil)

// New собирает сервис. Фоновых горутин нет: кэш сессий и лимитер чистятся попутно.
func New(d Deps) *Service {
	s := &Service{
		pool:     d.Pool,
		cfg:      d.Config,
		settings: d.Settings,
		audit:    d.Auditor,
		log:      d.Log,
		loadUser: d.LoadUser,
		cache:    newSessionCache(),
		limiter:  newIPLimiter(loginPerMinute, loginMaxInflight, limiterMaxIPs),
		ghosts:   newGhostLocks(ghostMaxKeys),
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.audit == nil {
		s.audit = nopAuditor{}
	}
	if s.cfg == nil {
		s.cfg = &config.Config{CookieSecure: true}
	}
	accounts := []public.DemoAccount{}
	if s.cfg.DemoMode {
		for _, u := range demoUsers {
			accounts = append(accounts, public.DemoAccount{Login: u.login, Password: u.login, Label: u.label})
		}
	}
	s.demoJSON, _ = json.Marshal(accounts)
	s.demoJSON = append(s.demoJSON, '\n')
	// Прогрев хэша-пустышки: первый вход с неизвестным логином не должен отвечать заметно
	// дольше последующих (выравнивание времени — смысл пустышки).
	_ = dummyHash()
	return s
}

type nopAuditor struct{}

func (nopAuditor) Log(context.Context, core.AuditEntry) {}

// ActiveSessions — живые сессии, проверенные за последние минуты (SystemHealth.goCore.activeSessions).
func (s *Service) ActiveSessions() int { return s.cache.active() }

func (s *Service) loginSettings(ctx context.Context) settings.Login {
	if s.settings == nil {
		return settings.Defaults().Login
	}
	l := s.settings.Get(ctx).Login
	if l.SessionTTLHours <= 0 {
		l.SessionTTLHours = defaultTTLHours
	}
	return l
}

func (s *Service) sessionTTL(ctx context.Context) time.Duration {
	return time.Duration(s.loginSettings(ctx).SessionTTLHours) * time.Hour
}

// ---------------------------------------------------------------- Authenticate

// Authenticate — core.Authenticator. ErrUnauthenticated: нет cookie / сессия неизвестна,
// отозвана, истекла / пользователь удалён. ErrUserBlocked: пользователь заблокирован
// (сессия при этом отзывается — дальше только вход заново после разблокировки).
func (s *Service) Authenticate(r *http.Request) (*core.Principal, error) {
	token, ok := readToken(r)
	if !ok {
		return nil, core.ErrUnauthenticated
	}
	key := tokenKey(sha256.Sum256([]byte(token)))
	now := time.Now()
	nowNs := now.UnixNano()

	e := s.cache.get(key)
	if e == nil || !e.fresh(nowNs) {
		var err error
		if e, err = s.load(r.Context(), key); err != nil {
			return nil, err
		}
	}
	switch e.state {
	case stateBlocked:
		return nil, core.ErrUserBlocked
	case stateDead:
		return nil, core.ErrUnauthenticated
	}
	if nowNs >= e.expiresAt.Load() {
		return nil, core.ErrUnauthenticated
	}
	s.maybeTouch(r.Context(), e, now)
	p := e.principal // копия: кэшированный Principal никто не должен иметь возможности менять
	return &p, nil
}

// readToken — значение cookie, если оно похоже на наш токен (мусор отсекаем без хэширования и БД).
func readToken(r *http.Request) (string, bool) {
	c, err := r.Cookie(CookieName)
	if err != nil || len(c.Value) != tokenLen {
		return "", false
	}
	return c.Value, true
}

const loadSessionSQL = `
SELECT s.id, s.user_id, s.expires_at, s.revoked_at IS NOT NULL, COALESCE(s.last_seen_at, s.created_at),
       u.role, u.status, u.deleted_at IS NOT NULL, u.login::text, u.last_name, u.first_name,
       COALESCE(u.middle_name, ''), COALESCE(u.operator_no, '')
  FROM auth_sessions s
  JOIN users u ON u.id = s.user_id
 WHERE s.refresh_token_hash = $1`

// load читает сессию из БД (одновременные промахи по одному токену — один запрос).
func (s *Service) load(ctx context.Context, key tokenKey) (*entry, error) {
	hashHex := hex.EncodeToString(key[:])
	v, err, _ := s.sf.Do(hashHex, func() (any, error) {
		seq := s.cache.purgeSeq.Load()
		lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loadTimeout)
		defer cancel()

		var (
			sessID, userID    uuid.UUID
			expiresAt, seenAt time.Time
			revoked, deleted  bool
			role, status      string
			p                 core.Principal
		)
		err := s.pool.QueryRow(lctx, loadSessionSQL, hashHex).Scan(&sessID, &userID, &expiresAt, &revoked, &seenAt,
			&role, &status, &deleted, &p.Login, &p.LastName, &p.FirstName, &p.MiddleName, &p.OperatorNo)
		if err != nil {
			if pg.IsNoRows(err) {
				// Неизвестные токены не кэшируем: перебор случайных cookie не должен вытеснять живые сессии.
				return nil, core.ErrUnauthenticated
			}
			return nil, fmt.Errorf("auth: load session: %w", err)
		}
		p.UserID, p.SessionID, p.Role = userID, sessID, core.Role(role)
		e := &entry{principal: p, loadedAt: time.Now().UnixNano()}
		e.expiresAt.Store(expiresAt.UnixNano())
		e.touchedAt.Store(seenAt.UnixNano())

		switch {
		case deleted || time.Now().After(expiresAt):
			e.state = stateDead
		case status == "blocked":
			e.state = stateBlocked
			if !revoked {
				// Заблокировали «мимо» go-core (или до отзыва): сессия больше не должна жить.
				if _, err := s.pool.Exec(lctx, `UPDATE auth_sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, sessID); err != nil {
					s.log.Warn("auth: revoke blocked session failed", "session", sessID, "err", err)
				}
			}
		case revoked:
			e.state = stateDead
		}
		s.cache.store(key, e, seq)
		return e, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*entry), nil
}

// maybeTouch — last_seen_at и скользящий срок: не чаще touchEvery на сессию. Продлеваем,
// когда прошло больше половины TTL (новый срок — now+TTL). Ошибка записи не роняет запрос.
func (s *Service) maybeTouch(ctx context.Context, e *entry, now time.Time) {
	nowNs := now.UnixNano()
	last := e.touchedAt.Load()
	if nowNs-last < int64(touchEvery) || !e.touchedAt.CompareAndSwap(last, nowNs) {
		return
	}
	ttl := s.sessionTTL(ctx)
	exp := time.Unix(0, e.expiresAt.Load())
	newExp := exp
	if exp.Sub(now) < ttl/2 {
		newExp = now.Add(ttl)
	}
	// Отмена клиентского запроса не должна обрывать запись (иначе шум в логе и лишняя попытка).
	tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), touchTimeout)
	defer cancel()
	if _, err := s.pool.Exec(tctx, sessionUpdateSQL, e.principal.SessionID, newExp); err != nil {
		s.log.Warn("auth: touch session failed", "session", e.principal.SessionID, "err", err)
		return
	}
	if newExp.After(exp) {
		e.expiresAt.Store(newExp.UnixNano())
	}
}

// ---------------------------------------------------------------- отзыв

// RevokeUserSessions отзывает все сессии пользователя (блокировка, смена пароля).
// Кэш чистится после COMMIT транзакции q (pg.OnCommit), иначе запрос, успевший прочитать
// ещё не отозванную сессию, закэшировал бы её; вне транзакции — сразу.
func (s *Service) RevokeUserSessions(ctx context.Context, q pg.Querier, userID uuid.UUID) error {
	if _, err := q.Exec(ctx, `UPDATE auth_sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID); err != nil {
		return fmt.Errorf("auth: revoke user sessions: %w", err)
	}
	pg.OnCommit(ctx, func() { s.cache.purgeUser(userID) })
	return nil
}

// PurgeUser — выбросить из кэша сессии пользователя (роль/ФИО изменились: Principal перечитается).
func (s *Service) PurgeUser(userID uuid.UUID) { s.cache.purgeUser(userID) }

// ---------------------------------------------------------------- cookie

func newToken() (string, error) {
	var raw [tokenBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("auth: token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func hashToken(token string) (tokenKey, string) {
	k := tokenKey(sha256.Sum256([]byte(token)))
	return k, hex.EncodeToString(k[:])
}

func (s *Service) setCookie(w http.ResponseWriter, token string, maxAge time.Duration) {
	sec := int(maxAge / time.Second)
	if sec <= 0 {
		sec = -1 // Max-Age=0: удалить
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   sec,
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *Service) clearCookie(w http.ResponseWriter) { s.setCookie(w, "", 0) }
