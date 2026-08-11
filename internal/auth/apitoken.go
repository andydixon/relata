package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/ids"
)

// Errors returned by the token service. They are deliberately few: a caller
// presenting a bad token learns that it was refused, not why.
var (
	ErrInvalidToken   = errors.New("api token is not valid")
	ErrTokenScope     = errors.New("api token lacks the required scope")
	ErrTokenRateLimit = errors.New("api token is making too many requests")
)

const (
	// tokenPrefix marks the credential and carries a version, so a future
	// format change is distinguishable rather than ambiguous.
	tokenPrefix = "rlt_v1_"
	// tokenBytes is the entropy behind a token. 32 bytes is far beyond
	// guessing, which is the point: there is no rate limit that makes a
	// low-entropy secret safe.
	tokenBytes = 32
	// displayPrefixLen is how much of the token is kept in clear so a person
	// can tell their tokens apart. Short enough to be useless as a credential.
	displayPrefixLen = 12
	// touchInterval throttles last-used writes. A busy token would otherwise
	// turn every read request into a write.
	touchInterval = time.Minute
	// maxTokenNameLen bounds the person's own label.
	maxTokenNameLen = 80
	// maxTokensPerUser bounds how many live tokens one person may hold.
	maxTokensPerUser = 25
)

// TokenService issues and verifies API tokens.
type TokenService struct {
	tokens domain.APITokenRepository
	users  domain.UserRepository
	cfg    TokenConfig

	// touched remembers when each token's last-used time was last written, so
	// the throttle survives across requests without a database read.
	mu      sync.Mutex
	touched map[string]time.Time

	limiter *tokenLimiter
}

// TokenConfig bounds token lifetimes and request rates.
type TokenConfig struct {
	DefaultTTL time.Duration
	MaxTTL     time.Duration
	// RateBurst requests are allowed per RateWindow per token.
	RateBurst  int
	RateWindow time.Duration
}

// NewTokenService builds the service.
func NewTokenService(tokens domain.APITokenRepository, users domain.UserRepository, cfg TokenConfig) *TokenService {
	if cfg.DefaultTTL <= 0 {
		cfg.DefaultTTL = 90 * 24 * time.Hour
	}
	if cfg.MaxTTL <= 0 {
		cfg.MaxTTL = 365 * 24 * time.Hour
	}
	if cfg.RateBurst <= 0 {
		cfg.RateBurst = 600
	}
	if cfg.RateWindow <= 0 {
		cfg.RateWindow = time.Minute
	}
	return &TokenService{
		tokens:  tokens,
		users:   users,
		cfg:     cfg,
		touched: map[string]time.Time{},
		limiter: newTokenLimiter(cfg.RateBurst, cfg.RateWindow),
	}
}

// IssuedToken is returned once, at creation. The Secret is never recoverable
// afterwards because only its hash is stored.
type IssuedToken struct {
	Token  *domain.APIToken
	Secret string
}

// CreateRequest describes a token to issue.
type CreateRequest struct {
	Name   string
	Scopes []domain.TokenScope
	TTL    time.Duration
}

// Create issues a token for the principal.
//
// The granted scopes are the intersection of what was asked for and what the
// person's role permits. Asking for more is not an error — it is narrowed and
// the caller is shown what was actually granted — because the alternative is a
// form that fails for reasons the person cannot see.
func (s *TokenService) Create(ctx context.Context, p *domain.Principal, req CreateRequest) (*IssuedToken, error) {
	if p == nil || p.UserID == "" || p.WorkspaceID == "" {
		return nil, domain.ErrForbidden
	}
	// A token cannot mint another token: that would let a leaked credential
	// extend its own life indefinitely.
	if p.ViaToken() {
		return nil, fmt.Errorf("%w: API tokens cannot create API tokens", domain.ErrForbidden)
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: a token needs a name so you can tell it from the others", domain.ErrInvalidInput)
	}
	if len([]rune(name)) > maxTokenNameLen {
		return nil, fmt.Errorf("%w: a token name may be at most %d characters", domain.ErrInvalidInput, maxTokenNameLen)
	}

	granted := make([]domain.TokenScope, 0, len(req.Scopes))
	seen := map[domain.TokenScope]bool{}
	for _, sc := range req.Scopes {
		if !sc.Valid() {
			return nil, fmt.Errorf("%w: %q is not a recognised scope", domain.ErrInvalidInput, sc)
		}
		if seen[sc] || !domain.RoleGrants(p.Role, sc) {
			continue
		}
		seen[sc] = true
		granted = append(granted, sc)
	}
	if len(granted) == 0 {
		return nil, fmt.Errorf("%w: none of the requested scopes are available to a %s",
			domain.ErrInvalidInput, p.Role)
	}

	ttl := req.TTL
	if ttl <= 0 {
		ttl = s.cfg.DefaultTTL
	}
	if ttl > s.cfg.MaxTTL {
		ttl = s.cfg.MaxTTL
	}

	existing, err := s.tokens.ListForUser(ctx, p.WorkspaceID, p.UserID)
	if err != nil {
		return nil, err
	}
	live := 0
	now := time.Now().UTC()
	for _, t := range existing {
		if t.Active(now) {
			live++
		}
	}
	if live >= maxTokensPerUser {
		return nil, fmt.Errorf("%w: you already hold %d active tokens; revoke one first",
			domain.ErrInvalidInput, live)
	}

	secret, err := newTokenSecret()
	if err != nil {
		return nil, err
	}

	tok := &domain.APIToken{
		ID:          ids.New(),
		WorkspaceID: p.WorkspaceID,
		UserID:      p.UserID,
		Name:        name,
		Prefix:      secret[:displayPrefixLen],
		TokenHash:   ids.HashToken(secret),
		Scopes:      granted,
		CreatedAt:   now,
		ExpiresAt:   now.Add(ttl),
	}
	if err := s.tokens.Create(ctx, tok); err != nil {
		return nil, err
	}
	return &IssuedToken{Token: tok, Secret: secret}, nil
}

// List returns the principal's own tokens.
func (s *TokenService) List(ctx context.Context, p *domain.Principal) ([]*domain.APIToken, error) {
	if p == nil || p.UserID == "" {
		return nil, domain.ErrForbidden
	}
	return s.tokens.ListForUser(ctx, p.WorkspaceID, p.UserID)
}

// Revoke marks one of the principal's tokens unusable.
//
// A person may only revoke their own tokens: the lookup is scoped to the
// workspace and the owner is checked, so an ID guessed from elsewhere resolves
// to nothing rather than to someone else's credential.
func (s *TokenService) Revoke(ctx context.Context, p *domain.Principal, id string) error {
	if p == nil || p.UserID == "" {
		return domain.ErrForbidden
	}
	if p.ViaToken() {
		return fmt.Errorf("%w: API tokens cannot revoke API tokens", domain.ErrForbidden)
	}
	tok, err := s.tokens.Get(ctx, p.WorkspaceID, id)
	if err != nil {
		return err
	}
	if tok.UserID != p.UserID && p.Role != domain.RoleAdmin {
		// Indistinguishable from a token that does not exist.
		return domain.ErrNotFound
	}
	return s.tokens.Revoke(ctx, p.WorkspaceID, id, p.UserID, time.Now().UTC())
}

// Authenticate resolves a bearer credential into a principal.
//
// Every refusal returns ErrInvalidToken. A caller learns that the credential
// was rejected and nothing more — not whether the token existed, expired, was
// revoked, or belonged to a disabled account.
func (s *TokenService) Authenticate(ctx context.Context, secret string) (*domain.Principal, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" || !strings.HasPrefix(secret, tokenPrefix) {
		return nil, ErrInvalidToken
	}

	tok, err := s.tokens.GetByHash(ctx, ids.HashToken(secret))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, ErrInvalidToken
		}
		return nil, err
	}

	now := time.Now().UTC()
	if !tok.Active(now) {
		return nil, ErrInvalidToken
	}
	if !s.limiter.allow(tok.ID) {
		return nil, ErrTokenRateLimit
	}

	// The user is re-read on every request rather than cached on the token, so
	// disabling an account or changing a role takes effect immediately instead
	// of at the token's expiry.
	u, err := s.users.GetByID(ctx, tok.UserID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, ErrInvalidToken
		}
		return nil, err
	}
	if u.Disabled || u.WorkspaceID != tok.WorkspaceID {
		return nil, ErrInvalidToken
	}

	s.touch(ctx, tok.ID, now)

	return &domain.Principal{
		UserID:      u.ID,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Role:        u.Role,
		WorkspaceID: u.WorkspaceID,
		TokenID:     tok.ID,
		TokenScopes: tok.Scopes,
	}, nil
}

// touch records use, at most once per touchInterval per token.
func (s *TokenService) touch(ctx context.Context, id string, now time.Time) {
	s.mu.Lock()
	last, ok := s.touched[id]
	if ok && now.Sub(last) < touchInterval {
		s.mu.Unlock()
		return
	}
	s.touched[id] = now
	// Bound the map: it is a throttle, not a record.
	if len(s.touched) > 10_000 {
		for k, t := range s.touched {
			if now.Sub(t) > touchInterval {
				delete(s.touched, k)
			}
		}
	}
	s.mu.Unlock()

	// Best-effort. Failing to record use must not fail the request that used it.
	_ = s.tokens.TouchLastUsed(ctx, id, now)
}

// BearerFromRequest extracts the credential from an Authorization header.
//
// Only the Bearer scheme is accepted, and only from the header — never from a
// query parameter, which would end up in access logs, browser history and
// referrer headers.
func BearerFromRequest(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const scheme = "bearer "
	if len(h) <= len(scheme) || !strings.EqualFold(h[:len(scheme)], scheme) {
		return ""
	}
	return strings.TrimSpace(h[len(scheme):])
}

// newTokenSecret mints a token. The encoding is base64url without padding so
// the value is safe in a header, a shell variable and a configuration file.
func newTokenSecret() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate api token: %w", err)
	}
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// ---------------------------------------------------------------------------
// Rate limiting
// ---------------------------------------------------------------------------

// tokenLimiter is a fixed-window counter per token. It exists to bound the
// damage a leaked or runaway token can do, not to shape traffic.
type tokenLimiter struct {
	mu     sync.Mutex
	burst  int
	window time.Duration
	seen   map[string]*tokenWindow
}

type tokenWindow struct {
	count int
	start time.Time
}

func newTokenLimiter(burst int, window time.Duration) *tokenLimiter {
	return &tokenLimiter{burst: burst, window: window, seen: map[string]*tokenWindow{}}
}

func (l *tokenLimiter) allow(id string) bool {
	if id == "" {
		return false
	}
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	w, ok := l.seen[id]
	if !ok || now.Sub(w.start) >= l.window {
		if len(l.seen) > 10_000 {
			for k, v := range l.seen {
				if now.Sub(v.start) >= l.window {
					delete(l.seen, k)
				}
			}
		}
		l.seen[id] = &tokenWindow{count: 1, start: now}
		return true
	}
	if w.count >= l.burst {
		return false
	}
	w.count++
	return true
}
