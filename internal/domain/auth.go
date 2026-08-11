package domain

import "time"

// Role determines what a user may do.
type Role string

const (
	RoleAdmin  Role = "admin"
	RoleEditor Role = "editor"
	RoleViewer Role = "viewer"
)

// Valid reports whether r is a recognised role.
func (r Role) Valid() bool {
	switch r {
	case RoleAdmin, RoleEditor, RoleViewer:
		return true
	}
	return false
}

// CanWrite reports whether the role may create or modify content.
func (r Role) CanWrite() bool { return r == RoleAdmin || r == RoleEditor }

// Workspace is the data boundary. Every user-content record carries a
// workspace ID and every repository query filters on it.
type Workspace struct {
	ID        string    `bson:"_id" json:"id"`
	Name      string    `bson:"name" json:"name"`
	Slug      string    `bson:"slug" json:"slug"`
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
}

// User is an account, either local or federated. Only the Argon2id encoded
// hash is ever stored for a local account; a federated one has none.
type User struct {
	ID       string `bson:"_id" json:"id"`
	Username string `bson:"username" json:"username"`
	// PasswordHash is an encoded Argon2id string including salt and parameters.
	// It is empty for a federated account, which can never sign in locally.
	PasswordHash string `bson:"password_hash" json:"-"`

	// OAuthProvider and OAuthSubject identify a federated account.
	//
	// The subject is the key, never the email address: `sub` is the only
	// identifier a provider promises not to reassign, whereas email addresses
	// are recycled — and keying on one would hand a new holder the previous
	// holder's documents.
	OAuthProvider string `bson:"oauth_provider,omitempty" json:"oauth_provider,omitempty"`
	OAuthSubject  string `bson:"oauth_subject,omitempty" json:"oauth_subject,omitempty"`
	// Email is stored for display and for the optional domain allowlist. It is
	// not an identifier and nothing is looked up by it.
	Email       string `bson:"email,omitempty" json:"email,omitempty"`
	DisplayName string `bson:"display_name,omitempty" json:"display_name,omitempty"`
	Role        Role   `bson:"role" json:"role"`
	WorkspaceID string `bson:"workspace_id" json:"workspace_id"`

	Disabled    bool       `bson:"disabled" json:"disabled"`
	CreatedAt   time.Time  `bson:"created_at" json:"created_at"`
	UpdatedAt   time.Time  `bson:"updated_at" json:"updated_at"`
	LastLoginAt *time.Time `bson:"last_login_at,omitempty" json:"last_login_at,omitempty"`
}

// Session is a server-side browser session. The cookie carries only an opaque
// token; the stored record holds a hash of that token so a database read cannot
// be replayed as a login.
type Session struct {
	ID        string `bson:"_id" json:"id"`
	TokenHash string `bson:"token_hash" json:"-"`
	// CSRFToken is a per-session secret compared in constant time on
	// state-changing browser requests.
	CSRFToken string `bson:"csrf_token" json:"-"`

	UserID      string `bson:"user_id" json:"user_id"`
	WorkspaceID string `bson:"workspace_id" json:"workspace_id"`

	CreatedAt      time.Time `bson:"created_at" json:"created_at"`
	LastSeenAt     time.Time `bson:"last_seen_at" json:"last_seen_at"`
	ExpiresAt      time.Time `bson:"expires_at" json:"expires_at"`
	AbsoluteExpiry time.Time `bson:"absolute_expiry" json:"absolute_expiry"`

	UserAgentHash string `bson:"user_agent_hash,omitempty" json:"-"`
	Revoked       bool   `bson:"revoked" json:"revoked"`
}

// Active reports whether the session may still be used at time now.
func (s *Session) Active(now time.Time) bool {
	return !s.Revoked && now.Before(s.ExpiresAt) && now.Before(s.AbsoluteExpiry)
}

// Principal is the authenticated caller carried through request contexts.
// Every service call that touches user content takes one.
type Principal struct {
	UserID      string
	Username    string
	DisplayName string
	Role        Role
	WorkspaceID string
	SessionID   string

	// TokenID and TokenScopes are set when the request authenticated with an
	// API token rather than a session. A principal carries one or the other,
	// never both, so downstream code can tell an interactive actor from a
	// program without inspecting the request.
	TokenID     string
	TokenScopes []TokenScope
}

// ViaToken reports whether this principal came from an API token.
func (p *Principal) ViaToken() bool { return p != nil && p.TokenID != "" }

// Permits reports whether the principal may perform an action needing scope s.
//
// A session-authenticated person is limited only by their role. A token is
// limited by the intersection of its scopes and that role, so a token can never
// do more than the person who created it.
func (p *Principal) Permits(s TokenScope) bool {
	if p == nil || !RoleGrants(p.Role, s) {
		return false
	}
	if !p.ViaToken() {
		return true
	}
	for _, got := range p.TokenScopes {
		if got == s {
			return true
		}
	}
	return false
}

// AuditEvent is an append-only record of a consequential action.
type AuditEvent struct {
	ID          string    `bson:"_id" json:"id"`
	WorkspaceID string    `bson:"workspace_id" json:"workspace_id"`
	At          time.Time `bson:"at" json:"at"`

	Action     string `bson:"action" json:"action"`
	ActorID    string `bson:"actor_id,omitempty" json:"actor_id,omitempty"`
	ActorName  string `bson:"actor_name,omitempty" json:"actor_name,omitempty"`
	TargetKind string `bson:"target_kind,omitempty" json:"target_kind,omitempty"`
	TargetID   string `bson:"target_id,omitempty" json:"target_id,omitempty"`
	RequestID  string `bson:"request_id,omitempty" json:"request_id,omitempty"`
	Outcome    string `bson:"outcome" json:"outcome"`
	// Detail holds short, non-sensitive descriptive fields. Never credentials,
	// never full document bodies.
	Detail map[string]string `bson:"detail,omitempty" json:"detail,omitempty"`
}

// Well-known audit actions.
const (
	AuditLogin            = "auth.login"
	AuditLoginFailed      = "auth.login_failed"
	AuditLogout           = "auth.logout"
	AuditUserCreated      = "user.created"
	AuditItemCreated      = "item.created"
	AuditItemDeleted      = "item.deleted"
	AuditItemReprocessed  = "item.reprocessed"
	AuditRelConfirmed     = "relationship.confirmed"
	AuditRelRejected      = "relationship.rejected"
	AuditRelEdited        = "relationship.edited"
	AuditRelCreated       = "relationship.created"
	AuditTokenCreated     = "api_token.created"
	AuditTokenRevoked     = "api_token.revoked"
	AuditOAuthLogin       = "auth.oauth_login"
	AuditOAuthProvisioned = "auth.oauth_provisioned"
	AuditOAuthFailed      = "auth.oauth_failed"
	AuditJobRetried       = "job.retried"
	AuditModelBadOutput   = "model.invalid_output"
	AuditAnswerGenerated  = "answer.generated"
	AuditWorkspaceReembed = "workspace.reembed"
)

// ProcessingRun records one pass of the pipeline over an item, retained for
// audit so that a reprocess does not erase what previous versions produced.
type ProcessingRun struct {
	ID          string `bson:"_id" json:"id"`
	WorkspaceID string `bson:"workspace_id" json:"workspace_id"`
	ItemID      string `bson:"item_id" json:"item_id"`

	Reason    string     `bson:"reason" json:"reason"`
	StartedAt time.Time  `bson:"started_at" json:"started_at"`
	EndedAt   *time.Time `bson:"ended_at,omitempty" json:"ended_at,omitempty"`
	Outcome   string     `bson:"outcome" json:"outcome"`
	Error     string     `bson:"error,omitempty" json:"error,omitempty"`

	ParserVersion  string `bson:"parser_version,omitempty" json:"parser_version,omitempty"`
	EmbeddingModel string `bson:"embedding_model,omitempty" json:"embedding_model,omitempty"`
	AnalysisModel  string `bson:"analysis_model,omitempty" json:"analysis_model,omitempty"`
	ScoreVersion   string `bson:"score_version,omitempty" json:"score_version,omitempty"`

	Stats map[string]int `bson:"stats,omitempty" json:"stats,omitempty"`
}

// ModelVersion records a model/algorithm version that has been used, so that
// stale derived data can be identified when configuration changes.
type ModelVersion struct {
	ID          string    `bson:"_id" json:"id"`
	WorkspaceID string    `bson:"workspace_id" json:"workspace_id"`
	Kind        string    `bson:"kind" json:"kind"`
	Name        string    `bson:"name" json:"name"`
	Dimensions  int       `bson:"dimensions,omitempty" json:"dimensions,omitempty"`
	FirstSeenAt time.Time `bson:"first_seen_at" json:"first_seen_at"`
	LastSeenAt  time.Time `bson:"last_seen_at" json:"last_seen_at"`
}

// Federated reports whether the account signs in through an identity provider.
func (u *User) Federated() bool { return u != nil && u.OAuthSubject != "" }

// CanLoginLocally reports whether a username and password could ever
// authenticate this account. A federated account has no password hash, so no
// submitted password can match one.
func (u *User) CanLoginLocally() bool {
	return u != nil && u.PasswordHash != "" && !u.Federated()
}

// ---------------------------------------------------------------------------
// API tokens
// ---------------------------------------------------------------------------

// TokenScope is a coarse permission carried by an API token.
type TokenScope string

const (
	// ScopeIngest permits creating, reprocessing and deleting items.
	ScopeIngest TokenScope = "ingest"
	// ScopeQuery permits search, answer generation, and reading stored records.
	ScopeQuery TokenScope = "query"
	// ScopeReview permits acting on relationship and alias review decisions.
	ScopeReview TokenScope = "review"
)

// Valid reports whether s is a recognised scope.
func (s TokenScope) Valid() bool {
	switch s {
	case ScopeIngest, ScopeQuery, ScopeReview:
		return true
	}
	return false
}

// AllTokenScopes lists every scope, for validation and for the interface.
func AllTokenScopes() []TokenScope {
	return []TokenScope{ScopeIngest, ScopeQuery, ScopeReview}
}

// RoleGrants reports whether a role is capable of a scope at all. A token can
// never exceed the role of the user who created it, so the effective permission
// is the intersection of the two — which means demoting a person immediately
// narrows every token they hold, without touching the tokens.
func RoleGrants(r Role, s TokenScope) bool {
	switch s {
	case ScopeQuery:
		return r.Valid()
	case ScopeIngest, ScopeReview:
		return r.CanWrite()
	}
	return false
}

// APIToken is a non-interactive credential for /api/v1.
//
// Only the hash of the secret is stored, so neither a database read nor a
// backup yields a usable credential.
type APIToken struct {
	ID          string `bson:"_id" json:"id"`
	WorkspaceID string `bson:"workspace_id" json:"workspace_id"`
	UserID      string `bson:"user_id" json:"user_id"`

	// Name is the person's own label for the token.
	Name string `bson:"name" json:"name"`
	// Prefix is the leading, non-secret portion of the token, kept in clear so
	// the interface can identify a token without holding it.
	Prefix string `bson:"prefix" json:"prefix"`
	// TokenHash is the SHA-256 of the whole token. It is never returned.
	TokenHash string `bson:"token_hash" json:"-"`

	Scopes []TokenScope `bson:"scopes" json:"scopes"`

	CreatedAt  time.Time  `bson:"created_at" json:"created_at"`
	ExpiresAt  time.Time  `bson:"expires_at" json:"expires_at"`
	LastUsedAt *time.Time `bson:"last_used_at,omitempty" json:"last_used_at,omitempty"`

	RevokedAt *time.Time `bson:"revoked_at,omitempty" json:"revoked_at,omitempty"`
	// RevokedBy records who revoked it, for the audit trail.
	RevokedBy string `bson:"revoked_by,omitempty" json:"revoked_by,omitempty"`
}

// Active reports whether the token may authenticate a request at time now.
func (t *APIToken) Active(now time.Time) bool {
	if t == nil || t.RevokedAt != nil {
		return false
	}
	return t.ExpiresAt.IsZero() || now.Before(t.ExpiresAt)
}

// HasScope reports whether the token was granted s.
func (t *APIToken) HasScope(s TokenScope) bool {
	if t == nil {
		return false
	}
	for _, got := range t.Scopes {
		if got == s {
			return true
		}
	}
	return false
}
