package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/andydixon/relata/internal/config"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/ids"
	"github.com/andydixon/relata/internal/observability"
)

// Errors returned by the session service.
var (
	// ErrInvalidCredentials is returned for both an unknown username and a
	// wrong password. The two cases are never distinguished to a caller.
	ErrInvalidCredentials = errors.New("the username or password is incorrect")
	ErrAccountDisabled    = errors.New("this account is disabled")
	ErrTooManyAttempts    = errors.New("too many sign-in attempts; please wait and try again")
	ErrNoSession          = errors.New("no active session")
)

// sessionTokenBytes is the entropy in a session cookie value.
const sessionTokenBytes = 32

// Service performs authentication and session management.
type Service struct {
	users    domain.UserRepository
	sessions domain.SessionRepository
	cfg      config.SessionConfig
	secure   bool
	limiter  *loginLimiter
	metrics  *observability.Metrics
}

// NewService builds the authentication service.
func NewService(users domain.UserRepository, sessions domain.SessionRepository,
	cfg config.SessionConfig, secureCookies bool, m *observability.Metrics) *Service {
	return &Service{
		users:    users,
		sessions: sessions,
		cfg:      cfg,
		secure:   secureCookies,
		limiter:  newLoginLimiter(cfg.LoginRateBurst, cfg.LoginRateWindow),
		metrics:  m,
	}
}

// CookieName returns the session cookie's name.
func (s *Service) CookieName() string { return s.cfg.CookieName }

// LoginResult carries what a successful login produced.
type LoginResult struct {
	User    *domain.User
	Session *domain.Session
	// Token is the raw cookie value. It exists only for the length of the
	// response and is never stored or logged.
	Token string
}

// Login authenticates a username and password and creates a new session.
//
// Several properties are load-bearing:
//   - rateKey (normally the client IP) is throttled before any database work;
//   - an unknown username still costs a full Argon2id verification, so timing
//     does not disclose which usernames exist;
//   - every failure returns the same error, whatever actually went wrong;
//   - a brand new session ID is minted, so a pre-existing session identifier
//     cannot be carried across the authentication boundary (session fixation).
func (s *Service) Login(ctx context.Context, username, password, rateKey, userAgent string) (*LoginResult, error) {
	if !s.limiter.allow(rateKey) {
		s.count("rate_limited")
		return nil, ErrTooManyAttempts
	}

	u, err := s.users.GetByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// Spend comparable time so the response does not reveal that the
			// username is unknown.
			SpendDummyVerification(password)
			s.count("unknown_user")
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}

	// A federated account has no password hash. Refuse it here explicitly
	// rather than relying on hash verification to fail on an empty string: the
	// guarantee that an OAuth identity cannot be impersonated with a password
	// should not depend on the behaviour of a parser.
	if !u.CanLoginLocally() {
		SpendDummyVerification(password)
		s.count("federated_account")
		return nil, ErrInvalidCredentials
	}

	ok, err := VerifyPassword(password, u.PasswordHash)
	if err != nil {
		// A corrupt stored hash must not be reported to the client as
		// something distinguishable from a wrong password.
		observability.LoggerFrom(ctx).Error("password hash could not be verified",
			"user_id", u.ID, "error", err.Error())
		s.count("hash_error")
		return nil, ErrInvalidCredentials
	}
	if !ok {
		s.count("bad_password")
		return nil, ErrInvalidCredentials
	}
	if u.Disabled {
		s.count("disabled")
		return nil, ErrInvalidCredentials
	}

	sess, token, err := s.createSession(ctx, u, userAgent)
	if err != nil {
		return nil, err
	}

	// A successful login clears the attempt budget for this client.
	s.limiter.reset(rateKey)
	s.count("success")

	now := time.Now().UTC()
	u.LastLoginAt = &now
	if err := s.users.Update(ctx, u); err != nil {
		// Not fatal: the session exists and the user is signed in.
		observability.LoggerFrom(ctx).Warn("could not record last login time", "error", err.Error())
	}

	return &LoginResult{User: u, Session: sess, Token: token}, nil
}

func (s *Service) count(outcome string) {
	if s.metrics != nil {
		s.metrics.AuthAttempts.WithLabelValues(outcome).Inc()
	}
}

// createSession mints a fresh session and its opaque token.
func (s *Service) createSession(ctx context.Context, u *domain.User, userAgent string) (*domain.Session, string, error) {
	token, err := ids.Token(sessionTokenBytes)
	if err != nil {
		return nil, "", err
	}
	csrf, err := ids.Token(32)
	if err != nil {
		return nil, "", err
	}

	now := time.Now().UTC()
	sess := &domain.Session{
		ID:             ids.New(),
		TokenHash:      ids.HashToken(token),
		CSRFToken:      csrf,
		UserID:         u.ID,
		WorkspaceID:    u.WorkspaceID,
		CreatedAt:      now,
		LastSeenAt:     now,
		ExpiresAt:      now.Add(s.cfg.IdleTimeout),
		AbsoluteExpiry: now.Add(s.cfg.AbsoluteTimeout),
		UserAgentHash:  hashUserAgent(userAgent),
	}
	if err := s.sessions.Create(ctx, sess); err != nil {
		return nil, "", err
	}
	return sess, token, nil
}

// Resolve validates a session token and returns the authenticated principal.
//
// It extends the idle expiry as a side effect, but only once per minute, so
// that a busy page does not write to the database on every request.
func (s *Service) Resolve(ctx context.Context, token string) (*domain.Principal, *domain.Session, error) {
	if token == "" {
		return nil, nil, ErrNoSession
	}
	sess, err := s.sessions.GetByTokenHash(ctx, ids.HashToken(token))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil, ErrNoSession
		}
		return nil, nil, err
	}

	now := time.Now().UTC()
	if !sess.Active(now) {
		return nil, nil, ErrNoSession
	}

	u, err := s.users.GetByID(ctx, sess.UserID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil, ErrNoSession
		}
		return nil, nil, err
	}
	if u.Disabled {
		_ = s.sessions.Revoke(ctx, sess.ID)
		return nil, nil, ErrNoSession
	}

	// The session's workspace is authoritative for the request. Should a user's
	// workspace change, existing sessions are revoked rather than silently
	// re-scoped, so an in-flight session cannot straddle two workspaces.
	if u.WorkspaceID != sess.WorkspaceID {
		_ = s.sessions.Revoke(ctx, sess.ID)
		return nil, nil, ErrNoSession
	}

	if now.Sub(sess.LastSeenAt) > time.Minute {
		newExpiry := now.Add(s.cfg.IdleTimeout)
		if newExpiry.After(sess.AbsoluteExpiry) {
			newExpiry = sess.AbsoluteExpiry
		}
		if err := s.sessions.Touch(ctx, sess.ID, now, newExpiry); err != nil {
			observability.LoggerFrom(ctx).Debug("could not extend session", "error", err.Error())
		}
		sess.LastSeenAt = now
		sess.ExpiresAt = newExpiry
	}

	return &domain.Principal{
		UserID:      u.ID,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Role:        u.Role,
		WorkspaceID: sess.WorkspaceID,
		SessionID:   sess.ID,
	}, sess, nil
}

// Logout revokes a session.
func (s *Service) Logout(ctx context.Context, sessionID string) error {
	return s.sessions.Revoke(ctx, sessionID)
}

// SweepExpired removes sessions past their absolute expiry.
func (s *Service) SweepExpired(ctx context.Context) (int64, error) {
	return s.sessions.DeleteExpired(ctx, time.Now().UTC())
}

// SetSessionCookie writes the session cookie.
//
// HttpOnly keeps the token away from JavaScript, SameSite=Lax blocks it from
// cross-site form posts while still allowing ordinary top-level navigation, and
// Secure is set whenever the deployment is served over HTTPS.
func (s *Service) SetSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cfg.CookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookie expires the session cookie.
func (s *Service) ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cfg.CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// TokenFromRequest reads the session token from the request cookie.
func (s *Service) TokenFromRequest(r *http.Request) string {
	c, err := r.Cookie(s.cfg.CookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// CheckCSRF compares a submitted CSRF token with the session's, in constant
// time.
func CheckCSRF(sessionToken, submitted string) bool {
	if sessionToken == "" || submitted == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(sessionToken), []byte(submitted)) == 1
}

func hashUserAgent(ua string) string {
	if ua == "" {
		return ""
	}
	if len(ua) > 512 {
		ua = ua[:512]
	}
	sum := sha256.Sum256([]byte(ua))
	return hex.EncodeToString(sum[:8])
}

// ---------------------------------------------------------------------------
// Login rate limiting
// ---------------------------------------------------------------------------

// loginLimiter throttles sign-in attempts per client key.
//
// It is an in-process fixed-window counter: Relata is a single-process
// application, so a shared store would add a dependency without adding
// protection. The limit is on attempts, and a successful login clears the
// counter, so a legitimate user who mistypes a few times is not locked out for
// long.
type loginLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	burst   int
	window  time.Duration
	// lastSweep bounds map growth from a flood of distinct keys.
	lastSweep time.Time
}

type bucket struct {
	count int
	start time.Time
}

func newLoginLimiter(burst int, window time.Duration) *loginLimiter {
	if burst <= 0 {
		burst = 8
	}
	if window <= 0 {
		window = 5 * time.Minute
	}
	return &loginLimiter{
		buckets:   make(map[string]*bucket),
		burst:     burst,
		window:    window,
		lastSweep: time.Now(),
	}
}

func (l *loginLimiter) allow(key string) bool {
	if key == "" {
		key = "unknown"
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	l.sweepLocked(now)

	b, ok := l.buckets[key]
	if !ok || now.Sub(b.start) > l.window {
		l.buckets[key] = &bucket{count: 1, start: now}
		return true
	}
	if b.count >= l.burst {
		return false
	}
	b.count++
	return true
}

func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, key)
}

// sweepLocked drops expired buckets. Without it, an attacker cycling source
// addresses would grow the map without bound.
func (l *loginLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < l.window {
		return
	}
	l.lastSweep = now
	for k, b := range l.buckets {
		if now.Sub(b.start) > l.window {
			delete(l.buckets, k)
		}
	}
}

// ClientKey derives the rate-limiting key for a request.
//
// Proxy headers are honoured only when the operator has explicitly said the
// deployment sits behind a trusted proxy. Trusting them by default would let
// anyone defeat the rate limit by varying X-Forwarded-For.
func ClientKey(r *http.Request, trustProxy bool) string {
	if trustProxy {
		// X-Real-IP first. A reverse proxy sets it to the address it actually
		// accepted the connection from, replacing anything the client sent, so
		// it is the one header here a client cannot choose.
		if xr := r.Header.Get("X-Real-IP"); xr != "" {
			return strings.TrimSpace(xr)
		}

		// Otherwise the RIGHTMOST entry of X-Forwarded-For — the hop appended
		// by the nearest proxy, which is the last one we trust.
		//
		// The leftmost entry is the obvious choice and the wrong one: proxies
		// append rather than replace, so a client that sends
		// "X-Forwarded-For: 1.2.3.4" puts that value at the front. Reading it
		// would give every forged header its own rate-limit bucket and defeat
		// login throttling entirely — the attack the throttle exists to stop.
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.LastIndex(xff, ","); i >= 0 {
				return strings.TrimSpace(xff[i+1:])
			}
			return strings.TrimSpace(xff)
		}
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}
