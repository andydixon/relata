package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/ids"
	"github.com/andydixon/relata/internal/observability"
)

// Provisioner turns a validated federated identity into a signed-in session.
//
// Each person gets a private workspace of their own, created on first sign-in,
// in which they are admin. The workspace boundary is the one Relata has
// enforced in storage from the beginning; this reuses it rather than inventing
// a second notion of separation.
type Provisioner struct {
	sessions   *Service
	users      domain.UserRepository
	workspaces domain.WorkspaceRepository
}

// NewProvisioner builds the provisioner.
func NewProvisioner(sessions *Service, users domain.UserRepository,
	workspaces domain.WorkspaceRepository) *Provisioner {
	return &Provisioner{sessions: sessions, users: users, workspaces: workspaces}
}

// SignIn finds or creates the account behind an identity and starts a session.
//
// The session ID is new every time, so an identifier held before signing in
// cannot be carried across the authentication boundary.
func (p *Provisioner) SignIn(ctx context.Context, provider string, id *Identity, userAgent string) (*LoginResult, error) {
	if id == nil || strings.TrimSpace(id.Subject) == "" {
		return nil, ErrOAuthIdentity
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "oauth"
	}

	u, err := p.findOrCreate(ctx, provider, id)
	if err != nil {
		return nil, err
	}
	if u.Disabled {
		// Deliberately the same error a wrong password produces.
		return nil, ErrInvalidCredentials
	}

	sess, token, err := p.sessions.createSession(ctx, u, userAgent)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	u.LastLoginAt = &now
	// Refresh the display fields: a person who changes their name at the
	// provider should see the change here too. The subject, which is the key,
	// is never rewritten.
	changed := false
	if id.DisplayName != "" && u.DisplayName != id.DisplayName {
		u.DisplayName = id.DisplayName
		changed = true
	}
	if id.Email != "" && u.Email != id.Email {
		u.Email = id.Email
		changed = true
	}
	if err := p.users.Update(ctx, u); err != nil {
		observability.LoggerFrom(ctx).Warn("could not record federated sign-in",
			"user_id", u.ID, "error", err.Error())
	}
	_ = changed

	return &LoginResult{User: u, Session: sess, Token: token}, nil
}

// findOrCreate resolves the account for an identity, creating it on first use.
//
// Provisioning is idempotent under concurrency. Two simultaneous first sign-ins
// both find nothing and both try to create; the unique index on
// (oauth_provider, oauth_subject) lets exactly one through, and the loser
// re-reads the winner's record rather than provisioning a second workspace for
// the same person.
func (p *Provisioner) findOrCreate(ctx context.Context, provider string, id *Identity) (*domain.User, error) {
	u, err := p.users.GetByOAuthSubject(ctx, provider, id.Subject)
	if err == nil {
		return u, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}

	now := time.Now().UTC()
	ws := &domain.Workspace{
		ID:        ids.New(),
		Name:      workspaceNameFor(id),
		Slug:      "ws-" + shortID(),
		CreatedAt: now,
	}
	if err := p.workspaces.Create(ctx, ws); err != nil {
		return nil, fmt.Errorf("create workspace for new account: %w", err)
	}

	user := &domain.User{
		ID:       ids.New(),
		Username: p.uniqueUsername(ctx, id),
		// No password hash: this account can never sign in locally.
		DisplayName:   id.DisplayName,
		Email:         id.Email,
		Role:          domain.RoleAdmin, // admin of their own workspace, nobody else's
		WorkspaceID:   ws.ID,
		OAuthProvider: provider,
		OAuthSubject:  id.Subject,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	if err := p.users.Create(ctx, user); err != nil {
		if errors.Is(err, domain.ErrDuplicate) {
			// Lost the race. The winner's account is the right one; this
			// workspace was never referenced by anything, so it is abandoned
			// rather than deleted — removing it would race with the winner.
			if existing, e := p.users.GetByOAuthSubject(ctx, provider, id.Subject); e == nil {
				observability.LoggerFrom(ctx).Info(
					"concurrent first sign-in resolved to the existing account",
					"user_id", existing.ID)
				return existing, nil
			}
			// A duplicate that is not the subject can only be the username.
			user.Username = user.Username + "-" + shortID()
			if err2 := p.users.Create(ctx, user); err2 != nil {
				return nil, fmt.Errorf("create account for federated identity: %w", err2)
			}
			return user, nil
		}
		return nil, fmt.Errorf("create account for federated identity: %w", err)
	}

	observability.LoggerFrom(ctx).Info("provisioned a workspace for a new federated account",
		"user_id", user.ID, "workspace_id", ws.ID, "provider", provider)
	return user, nil
}

// uniqueUsername derives a readable, unique username from the identity.
//
// It is a label, not an identifier: the account is keyed on the provider
// subject, so a collision here is cosmetic and resolved by appending.
func (p *Provisioner) uniqueUsername(ctx context.Context, id *Identity) string {
	base := usernameSeed(id)
	candidate := base
	for attempt := 0; attempt < 5; attempt++ {
		if _, err := p.users.GetByUsername(ctx, candidate); errors.Is(err, domain.ErrNotFound) {
			return candidate
		}
		candidate = base + "-" + shortID()
	}
	return base + "-" + shortID()
}

// usernameSeed builds the readable part of a username from an identity.
func usernameSeed(id *Identity) string {
	seed := id.Email
	if at := strings.Index(seed, "@"); at > 0 {
		seed = seed[:at]
	}
	if strings.TrimSpace(seed) == "" {
		seed = id.DisplayName
	}
	if strings.TrimSpace(seed) == "" {
		seed = "user"
	}

	var b strings.Builder
	for _, r := range strings.ToLower(seed) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_':
			b.WriteRune('-')
		case unicode.IsSpace(r):
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len([]rune(out)) > 40 {
		out = string([]rune(out)[:40])
	}
	if out == "" {
		out = "user"
	}
	return out
}

// workspaceNameFor labels a person's private workspace.
func workspaceNameFor(id *Identity) string {
	who := strings.TrimSpace(id.DisplayName)
	if who == "" {
		who = strings.TrimSpace(id.Email)
	}
	if who == "" {
		return "Private workspace"
	}
	if len([]rune(who)) > 60 {
		who = string([]rune(who)[:60])
	}
	return who + "'s workspace"
}

func shortID() string {
	return strings.ReplaceAll(ids.New(), "-", "")[:8]
}
