package domain

import (
	"context"
	"time"
)

// ---------------------------------------------------------------------------
// AI provider ports
//
// These exist so that Ollama can be replaced by an OpenAI-compatible API or any
// other provider without touching the service layer. Adapters live in
// internal/embeddings and internal/analysis.
// ---------------------------------------------------------------------------

// Embedder turns text into vectors.
type Embedder interface {
	// Embed returns one vector per input text, in the same order. Implementations
	// must honour ctx cancellation and must not retry indefinitely.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// ModelID identifies the exact model, for stamping onto derived records.
	ModelID() string
	// Dimensions is the vector width this model produces.
	Dimensions() int
}

// ChatMessage is one turn of a model conversation.
type ChatMessage struct {
	Role    string // "system" | "user" | "assistant"
	Content string
}

// CompletionRequest asks the model for a bounded response.
type CompletionRequest struct {
	Messages []ChatMessage
	// JSONSchema, when non-empty, requests structured output conforming to it.
	JSONSchema []byte
	// MaxTokens bounds the response so a runaway generation cannot exhaust
	// memory or time.
	MaxTokens   int
	Temperature float64
}

// CompletionResponse is a model reply plus what it cost.
type CompletionResponse struct {
	Content      string
	Model        string
	PromptTokens int
	OutputTokens int
	Duration     time.Duration
}

// Analyser performs model-assisted structured tasks. Its output is always
// treated as untrusted and validated against a schema before use.
type Analyser interface {
	Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error)
	ModelID() string
}

// ProviderHealth describes whether an AI provider is reachable and configured.
type ProviderHealth struct {
	Name      string   `json:"name"`
	Endpoint  string   `json:"endpoint"`
	Model     string   `json:"model"`
	Available bool     `json:"available"`
	Detail    string   `json:"detail,omitempty"`
	Models    []string `json:"models,omitempty"`
}

// HealthChecker is implemented by providers that can report reachability.
type HealthChecker interface {
	Health(ctx context.Context) ProviderHealth
}

// ---------------------------------------------------------------------------
// Vector store port
// ---------------------------------------------------------------------------

// VectorPoint is a stored embedding plus the minimal retrieval metadata needed
// to resolve it back to MongoDB. Payloads deliberately exclude document text.
type VectorPoint struct {
	ChunkID        string
	WorkspaceID    string
	ItemID         string
	SourceType     SourceType
	CreatedAt      time.Time
	EmbeddingModel string
	Vector         []float32
}

// VectorMatch is a similarity search hit.
type VectorMatch struct {
	ChunkID     string
	ItemID      string
	WorkspaceID string
	// Score is cosine similarity in roughly -1..1; higher is more similar.
	Score          float64
	SourceType     SourceType
	CreatedAt      time.Time
	EmbeddingModel string
}

// VectorFilter narrows a similarity search. All fields are optional and are
// combined with AND. WorkspaceID is always applied by the caller.
type VectorFilter struct {
	WorkspaceID    string
	ItemIDs        []string
	ExcludeItemIDs []string
	SourceTypes    []SourceType
	EmbeddingModel string
	CreatedFrom    *time.Time
	CreatedTo      *time.Time
}

// VectorStore persists and searches embeddings.
type VectorStore interface {
	// EnsureCollection creates the collection if absent, sized for dim.
	EnsureCollection(ctx context.Context, dim int) error
	Upsert(ctx context.Context, points []VectorPoint) error
	Search(ctx context.Context, vector []float32, limit int, f VectorFilter) ([]VectorMatch, error)
	DeleteByItem(ctx context.Context, workspaceID, itemID string) error
	DeleteByChunks(ctx context.Context, workspaceID string, chunkIDs []string) error
	// CountByWorkspace reports how many vectors exist, for reconciliation.
	CountByWorkspace(ctx context.Context, workspaceID string) (int64, error)
	Health(ctx context.Context) error
}

// ---------------------------------------------------------------------------
// Repository ports
//
// Every method that touches user content takes a workspace ID and must include
// it in the underlying query. There is intentionally no "get by ID" that skips
// the workspace filter.
// ---------------------------------------------------------------------------

// Page describes a bounded slice of results.
type Page struct {
	Limit  int
	Offset int
}

// Normalise clamps a page request to safe bounds.
func (p Page) Normalise(defLimit, maxLimit int) Page {
	if p.Limit <= 0 {
		p.Limit = defLimit
	}
	if p.Limit > maxLimit {
		p.Limit = maxLimit
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	return p
}

// ItemFilter selects items using typed values only. Client-supplied MongoDB
// operators are never accepted anywhere in this struct.
type ItemFilter struct {
	SourceTypes    []SourceType
	MIMETypes      []string
	Statuses       []ProcessingStatus
	CreatedFrom    *time.Time
	CreatedTo      *time.Time
	ContentHash    string
	TitleContains  string
	IncludeDeleted bool
}

// ItemRepository stores source items.
type ItemRepository interface {
	Create(ctx context.Context, it *Item) error
	Get(ctx context.Context, workspaceID, id string) (*Item, error)
	GetMany(ctx context.Context, workspaceID string, ids []string) ([]*Item, error)
	Update(ctx context.Context, it *Item) error
	SetStatus(ctx context.Context, workspaceID, id string, st ProcessingStatus, errMsg string) error
	AddWarning(ctx context.Context, workspaceID, id string, w ParseWarning) error
	List(ctx context.Context, workspaceID string, f ItemFilter, p Page) ([]*Item, int64, error)
	FindByContentHash(ctx context.Context, workspaceID, hash string) (*Item, error)
	// TextSearch is the lexical half of hybrid retrieval.
	TextSearch(ctx context.Context, workspaceID, query string, f ItemFilter, limit int) ([]LexicalHit, error)
	Tombstone(ctx context.Context, workspaceID, id string) error
	HardDelete(ctx context.Context, workspaceID, id string) error
	Count(ctx context.Context, workspaceID string) (int64, error)
	CountByStatus(ctx context.Context, workspaceID string) (map[ProcessingStatus]int64, error)
}

// LexicalHit is a keyword-search result with its raw text score.
type LexicalHit struct {
	ItemID string
	Score  float64
}

// ChunkRepository stores item chunks.
type ChunkRepository interface {
	ReplaceForItem(ctx context.Context, workspaceID, itemID string, chunks []*Chunk) error
	ListByItem(ctx context.Context, workspaceID, itemID string) ([]*Chunk, error)
	GetMany(ctx context.Context, workspaceID string, ids []string) ([]*Chunk, error)
	MarkEmbedded(ctx context.Context, workspaceID string, ids []string, model string) error
	ListUnembedded(ctx context.Context, workspaceID, model string, limit int) ([]*Chunk, error)
	DeleteByItem(ctx context.Context, workspaceID, itemID string) error
	Count(ctx context.Context, workspaceID string) (int64, error)
	// TextSearch finds chunks lexically, used to produce excerpts.
	TextSearch(ctx context.Context, workspaceID, query string, limit int) ([]ChunkHit, error)
}

// ChunkHit is a lexical chunk match.
type ChunkHit struct {
	ChunkID string
	ItemID  string
	Score   float64
	Text    string
}

// EntityFilter selects entities by typed values.
type EntityFilter struct {
	Types         []EntityType
	NameContains  string
	NormalisedKey string
	ItemID        string
	MinMentions   int
}

// EntityRepository stores extracted entities.
type EntityRepository interface {
	// UpsertByKey merges a newly observed entity into an existing one with the
	// same (workspace, type, normalised key), accumulating references. It never
	// merges entities whose keys merely look similar.
	UpsertByKey(ctx context.Context, e *Entity) (*Entity, error)
	Get(ctx context.Context, workspaceID, id string) (*Entity, error)
	GetMany(ctx context.Context, workspaceID string, ids []string) ([]*Entity, error)
	FindByNormalised(ctx context.Context, workspaceID string, keys []string) ([]*Entity, error)
	List(ctx context.Context, workspaceID string, f EntityFilter, p Page) ([]*Entity, int64, error)
	ListByItem(ctx context.Context, workspaceID, itemID string) ([]*Entity, error)
	// ItemsSharingEntities returns item IDs that reference any of the given
	// entities, excluding excludeItem. This is the indexed path that keeps
	// candidate discovery away from all-pairs comparison.
	ItemsSharingEntities(ctx context.Context, workspaceID string, entityIDs []string, excludeItem string, limit int) ([]string, error)
	RemoveItemRefs(ctx context.Context, workspaceID, itemID string) (orphaned []string, err error)
	DeleteMany(ctx context.Context, workspaceID string, ids []string) error
	Count(ctx context.Context, workspaceID string) (int64, error)
}

// AliasRepository stores candidate and confirmed entity aliases.
type AliasRepository interface {
	Upsert(ctx context.Context, a *EntityAlias) error
	ListByEntity(ctx context.Context, workspaceID, entityID string) ([]*EntityAlias, error)
	List(ctx context.Context, workspaceID string, status AliasStatus, p Page) ([]*EntityAlias, int64, error)
	SetStatus(ctx context.Context, workspaceID, id string, st AliasStatus) error
	DeleteByEntity(ctx context.Context, workspaceID, entityID string) error
}

// ClaimRepository stores extracted claims.
type ClaimRepository interface {
	ReplaceForItem(ctx context.Context, workspaceID, itemID string, claims []*Claim) error
	ListByItem(ctx context.Context, workspaceID, itemID string) ([]*Claim, error)
	// FindBySubjects locates claims about the same coarse subject, which is how
	// support/contradiction candidates are found cheaply.
	FindBySubjects(ctx context.Context, workspaceID string, subjects []string, excludeItem string, limit int) ([]*Claim, error)
	GetMany(ctx context.Context, workspaceID string, ids []string) ([]*Claim, error)
	DeleteByItem(ctx context.Context, workspaceID, itemID string) error
	Count(ctx context.Context, workspaceID string) (int64, error)
}

// RelationshipFilter selects relationships by typed values.
type RelationshipFilter struct {
	Types         []RelationshipType
	Provenances   []Provenance
	ReviewStatus  []ReviewStatus
	MinScore      float64
	NodeID        string
	NodeKind      NodeKind
	Direction     string // "out" | "in" | "both"
	ConfirmedOnly bool
}

// RelationshipRepository stores relationships and their review history.
type RelationshipRepository interface {
	// UpsertByDedupeKey inserts or refreshes a relationship without creating a
	// duplicate on reprocessing. It never overwrites a human review decision.
	UpsertByDedupeKey(ctx context.Context, r *Relationship) (*Relationship, error)

	// FindByDedupeKey returns the relationship with this dedupe key, or
	// ErrNotFound. It exists so callers can locate an existing link without
	// paging through a node's relationships — a well-connected item can have
	// more than one page of them, and a paged scan would miss the match and
	// then collide with the unique index.
	FindByDedupeKey(ctx context.Context, workspaceID, dedupeKey string) (*Relationship, error)
	Create(ctx context.Context, r *Relationship) error
	Get(ctx context.Context, workspaceID, id string) (*Relationship, error)
	List(ctx context.Context, workspaceID string, f RelationshipFilter, p Page) ([]*Relationship, int64, error)
	ListForNode(ctx context.Context, workspaceID string, kind NodeKind, id string, f RelationshipFilter) (out, in []*Relationship, err error)
	// ListForItems fetches relationships touching any of the given items, used
	// for relationship expansion during search.
	ListForItems(ctx context.Context, workspaceID string, itemIDs []string, minScore float64) ([]*Relationship, error)
	Update(ctx context.Context, r *Relationship) error
	AppendReview(ctx context.Context, workspaceID, id string, ev ReviewEvent, newStatus ReviewStatus, newType RelationshipType, newProv Provenance) (*Relationship, error)
	TombstoneForItem(ctx context.Context, workspaceID, itemID string) (int64, error)
	DeleteForItem(ctx context.Context, workspaceID, itemID string) (int64, error)
	Count(ctx context.Context, workspaceID string) (int64, error)
	CountPendingReview(ctx context.Context, workspaceID string) (int64, error)
}

// JobRepository is the persistent queue.
type JobRepository interface {
	Enqueue(ctx context.Context, j *Job) error
	// Claim atomically leases the next runnable job for owner.
	Claim(ctx context.Context, owner string, types []JobType, lease time.Duration) (*Job, error)
	// Heartbeat extends a lease held by owner; it fails if the lease was lost.
	Heartbeat(ctx context.Context, id, owner string, lease time.Duration) error
	Complete(ctx context.Context, id, owner string, att JobAttempt) error
	Fail(ctx context.Context, id, owner string, att JobAttempt, retryAfter *time.Time) error
	Get(ctx context.Context, id string) (*Job, error)
	List(ctx context.Context, workspaceID string, statuses []JobStatus, types []JobType, p Page) ([]*Job, int64, error)
	Retry(ctx context.Context, workspaceID, id string) (*Job, error)
	CountByStatus(ctx context.Context, workspaceID string) (map[JobStatus]int64, error)
	// ReleaseExpired returns jobs whose lease elapsed to the pending pool.
	ReleaseExpired(ctx context.Context, now time.Time) (int64, error)
	DeleteForItem(ctx context.Context, workspaceID, itemID string) (int64, error)
}

// AuditRepository is append-only.
type AuditRepository interface {
	Record(ctx context.Context, ev *AuditEvent) error
	List(ctx context.Context, workspaceID string, p Page) ([]*AuditEvent, int64, error)
}

// RunRepository stores processing run history.
type RunRepository interface {
	Create(ctx context.Context, r *ProcessingRun) error
	Finish(ctx context.Context, id, outcome, errMsg string, stats map[string]int) error
	ListByItem(ctx context.Context, workspaceID, itemID string, limit int) ([]*ProcessingRun, error)
}

// ModelVersionRepository tracks which models have produced stored data.
type ModelVersionRepository interface {
	Observe(ctx context.Context, workspaceID, kind, name string, dims int) error
	List(ctx context.Context, workspaceID string) ([]*ModelVersion, error)
}

// UserRepository stores local accounts.
type UserRepository interface {
	Create(ctx context.Context, u *User) error
	GetByUsername(ctx context.Context, username string) (*User, error)
	GetByID(ctx context.Context, id string) (*User, error)
	Update(ctx context.Context, u *User) error
	Count(ctx context.Context) (int64, error)

	// GetByOAuthSubject finds a federated account by provider and subject, the
	// only pair a provider promises not to reassign. Returns ErrNotFound when
	// the person has not signed in before.
	GetByOAuthSubject(ctx context.Context, provider, subject string) (*User, error)
}

// APITokenRepository stores non-interactive API credentials. Only hashes are
// held; there is deliberately no method that returns a token's secret.
type APITokenRepository interface {
	Create(ctx context.Context, t *APIToken) error
	// GetByHash finds a token by the SHA-256 of its secret. The lookup is by
	// the full hash, so a partial match cannot narrow the search space.
	GetByHash(ctx context.Context, hash string) (*APIToken, error)
	// ListForUser returns a person's tokens, newest first, including revoked
	// and expired ones so the history stays visible.
	ListForUser(ctx context.Context, workspaceID, userID string) ([]*APIToken, error)
	Get(ctx context.Context, workspaceID, id string) (*APIToken, error)
	Revoke(ctx context.Context, workspaceID, id, revokedBy string, at time.Time) error
	// TouchLastUsed records use. Callers throttle this so a busy token does not
	// turn every read into a write.
	TouchLastUsed(ctx context.Context, id string, at time.Time) error
	// DeleteForUser removes a user's tokens, used when an account is removed.
	DeleteForUser(ctx context.Context, workspaceID, userID string) (int64, error)
}

// SessionRepository stores browser sessions.
type SessionRepository interface {
	Create(ctx context.Context, s *Session) error
	GetByTokenHash(ctx context.Context, hash string) (*Session, error)
	Touch(ctx context.Context, id string, lastSeen, expires time.Time) error
	Revoke(ctx context.Context, id string) error
	RevokeAllForUser(ctx context.Context, userID string) error
	DeleteExpired(ctx context.Context, before time.Time) (int64, error)
}

// WorkspaceRepository stores workspaces.
type WorkspaceRepository interface {
	Create(ctx context.Context, w *Workspace) error
	Get(ctx context.Context, id string) (*Workspace, error)
	GetBySlug(ctx context.Context, slug string) (*Workspace, error)
	List(ctx context.Context) ([]*Workspace, error)
}
