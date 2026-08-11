// Package mongo implements Relata's repository ports against MongoDB.
//
// Two rules apply to every function in this package:
//
//  1. Workspace enforcement. Every query touching user content includes
//     workspace_id in its filter. The helper wsFilter is the only sanctioned
//     way to start a filter, and no exported method accepts a filter document
//     from a caller.
//  2. No client-controlled operators. Filters are assembled here from typed Go
//     values. A string from a request body becomes a value in a bson.D, never a
//     key, so a payload like {"$ne": null} is matched as the literal string it
//     is rather than interpreted as an operator.
package mongo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"github.com/andydixon/relata/internal/config"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/observability"
)

// Collection names.
const (
	CollUsers         = "users"
	CollSessions      = "sessions"
	CollWorkspaces    = "workspaces"
	CollItems         = "items"
	CollChunks        = "chunks"
	CollEntities      = "entities"
	CollEntityAliases = "entity_aliases"
	CollClaims        = "claims"
	CollRelationships = "relationships"
	CollJobs          = "jobs"
	CollRuns          = "processing_runs"
	CollModelVersions = "model_versions"
	CollAudit         = "audit_events"
	CollAPITokens     = "api_tokens"
)

// apiTokenRetention is how long a token record is kept past its expiry, so the
// history remains readable before it is swept.
const apiTokenRetention = 90 * 24 * time.Hour

// Store owns the MongoDB connection and exposes the repositories.
type Store struct {
	client  *mongo.Client
	db      *mongo.Database
	timeout time.Duration
	metrics *observability.Metrics

	Items         *ItemRepo
	Chunks        *ChunkRepo
	Entities      *EntityRepo
	Aliases       *AliasRepo
	Claims        *ClaimRepo
	Relationships *RelationshipRepo
	Jobs          *JobRepo
	Users         *UserRepo
	APITokens     *APITokenRepo
	Sessions      *SessionRepo
	Workspaces    *WorkspaceRepo
	Audit         *AuditRepo
	Runs          *RunRepo
	ModelVersions *ModelVersionRepo
}

// Connect dials MongoDB and verifies the connection.
func Connect(ctx context.Context, cfg config.MongoConfig, m *observability.Metrics) (*Store, error) {
	opts := options.Client().
		ApplyURI(cfg.URI).
		SetConnectTimeout(cfg.Timeout).
		SetServerSelectionTimeout(cfg.Timeout).
		SetTimeout(cfg.Timeout).
		SetAppName("relata")

	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, fmt.Errorf("connect to mongodb: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	if err := client.Ping(pingCtx, readpref.Primary()); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, fmt.Errorf("ping mongodb at %s: %w", redactURI(cfg.URI), err)
	}

	db := client.Database(cfg.Database)
	s := &Store{client: client, db: db, timeout: cfg.Timeout, metrics: m}

	s.Items = &ItemRepo{base: s.base(CollItems)}
	s.Chunks = &ChunkRepo{base: s.base(CollChunks)}
	s.Entities = &EntityRepo{base: s.base(CollEntities)}
	s.Aliases = &AliasRepo{base: s.base(CollEntityAliases)}
	s.Claims = &ClaimRepo{base: s.base(CollClaims)}
	s.Relationships = &RelationshipRepo{base: s.base(CollRelationships)}
	s.Jobs = &JobRepo{base: s.base(CollJobs)}
	s.Users = &UserRepo{base: s.base(CollUsers)}
	s.APITokens = &APITokenRepo{base: s.base(CollAPITokens)}
	s.Sessions = &SessionRepo{base: s.base(CollSessions)}
	s.Workspaces = &WorkspaceRepo{base: s.base(CollWorkspaces)}
	s.Audit = &AuditRepo{base: s.base(CollAudit)}
	s.Runs = &RunRepo{base: s.base(CollRuns)}
	s.ModelVersions = &ModelVersionRepo{base: s.base(CollModelVersions)}

	return s, nil
}

// Close disconnects from MongoDB.
func (s *Store) Close(ctx context.Context) error { return s.client.Disconnect(ctx) }

// DB exposes the database handle, used by migrations and tests.
func (s *Store) DB() *mongo.Database { return s.db }

// Ping checks connectivity for the readiness probe.
func (s *Store) Ping(ctx context.Context) error {
	return s.client.Ping(ctx, readpref.Primary())
}

// Drop removes the database. Used only by integration tests.
func (s *Store) Drop(ctx context.Context) error { return s.db.Drop(ctx) }

func (s *Store) base(name string) base {
	return base{coll: s.db.Collection(name), name: name, metrics: s.metrics, timeout: s.timeout}
}

// base carries what every repository needs: its collection, its name for
// metrics, and the default operation timeout.
type base struct {
	coll    *mongo.Collection
	name    string
	metrics *observability.Metrics
	timeout time.Duration
}

// ctx applies the storage timeout unless the caller's context is tighter.
// Every MongoDB call is bounded, so a stalled database cannot pin a worker.
func (b base) ctx(parent context.Context) (context.Context, context.CancelFunc) {
	if b.timeout <= 0 {
		return context.WithCancel(parent)
	}
	if dl, ok := parent.Deadline(); ok && time.Until(dl) < b.timeout {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, b.timeout)
}

// observe records the outcome of a storage operation.
func (b base) observe(op string, started time.Time, err error) {
	if b.metrics == nil {
		return
	}
	outcome := "success"
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		outcome = "error"
	}
	b.metrics.StorageOps.WithLabelValues("mongo", b.name+"."+op, outcome).Inc()
	b.metrics.StorageDuration.WithLabelValues("mongo", b.name+"."+op).Observe(time.Since(started).Seconds())
}

// wsFilter starts a filter scoped to a workspace.
//
// This is the single entry point for building a query over user content. It
// rejects an empty workspace so that a missing principal can never widen a
// query to every workspace.
func wsFilter(workspaceID string) (bson.D, error) {
	if workspaceID == "" {
		return nil, fmt.Errorf("%w: workspace is required for this query", domain.ErrWorkspaceMiss)
	}
	return bson.D{{Key: "workspace_id", Value: workspaceID}}, nil
}

// notDeleted appends the tombstone exclusion used by most reads.
func notDeleted(f bson.D) bson.D {
	return append(f, bson.E{Key: "deleted", Value: bson.D{{Key: "$ne", Value: true}}})
}

// isDup reports whether err is a duplicate-key error.
func isDup(err error) bool {
	var we mongo.WriteException
	if errors.As(err, &we) {
		for _, e := range we.WriteErrors {
			if e.Code == 11000 {
				return true
			}
		}
	}
	var be mongo.BulkWriteException
	if errors.As(err, &be) {
		for _, e := range be.WriteErrors {
			if e.Code == 11000 {
				return true
			}
		}
	}
	return false
}

// translate converts driver errors into domain errors.
func translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, mongo.ErrNoDocuments) {
		return domain.ErrNotFound
	}
	if isDup(err) {
		return domain.ErrDuplicate
	}
	return err
}

// redactURI removes credentials from a connection string before logging it.
func redactURI(uri string) string {
	at := -1
	for i := len(uri) - 1; i >= 0; i-- {
		if uri[i] == '@' {
			at = i
			break
		}
	}
	if at < 0 {
		return uri
	}
	scheme := "mongodb://"
	if len(uri) > 14 && uri[:14] == "mongodb+srv://" {
		scheme = "mongodb+srv://"
	}
	return scheme + "[redacted]" + uri[at:]
}

// stringsToAny converts a typed slice for use as an $in value. Values only ever
// appear on the right-hand side of a comparison, never as document keys.
func stringsToAny(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// EnsureIndexes creates every index Relata relies on.
//
// Index creation is idempotent, so this runs on every startup and after any
// schema change. Uniqueness constraints here are load-bearing: they are what
// make ingestion and reprocessing idempotent under concurrency rather than
// merely usually-idempotent.
func (s *Store) EnsureIndexes(ctx context.Context) error {
	type spec struct {
		coll   string
		models []mongo.IndexModel
	}

	specs := []spec{
		{CollWorkspaces, []mongo.IndexModel{
			{Keys: bson.D{{Key: "slug", Value: 1}}, Options: options.Index().SetUnique(true).SetName("workspace_slug_unique")},
		}},
		{CollUsers, []mongo.IndexModel{
			{Keys: bson.D{{Key: "username", Value: 1}}, Options: options.Index().SetUnique(true).SetName("username_unique")},
			{Keys: bson.D{{Key: "workspace_id", Value: 1}}, Options: options.Index().SetName("user_workspace")},
			// One account per federated identity. This is what makes first
			// sign-in idempotent: two concurrent callbacks race here, and the
			// loser re-reads the winner rather than provisioning a second
			// workspace for the same person. Partial, so local accounts —
			// which have no subject — are not all collapsed onto one another.
			{
				Keys: bson.D{{Key: "oauth_provider", Value: 1}, {Key: "oauth_subject", Value: 1}},
				Options: options.Index().SetUnique(true).SetName("user_oauth_subject_unique").
					SetPartialFilterExpression(bson.D{
						{Key: "oauth_subject", Value: bson.D{{Key: "$type", Value: "string"}}},
					}),
			},
		}},
		{CollAPITokens, []mongo.IndexModel{
			// Authentication is a lookup on this index and nothing else.
			{Keys: bson.D{{Key: "token_hash", Value: 1}}, Options: options.Index().SetUnique(true).SetName("api_token_hash_unique")},
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "user_id", Value: 1}, {Key: "created_at", Value: -1}}, Options: options.Index().SetName("api_token_ws_user")},
			// Retention, not enforcement. A token past its expiry is refused on
			// the next request regardless of this index; the index removes the
			// record ninety days *after* that expiry, so a token's existence
			// and its end stay visible for a while and then stop accumulating.
			{Keys: bson.D{{Key: "expires_at", Value: 1}}, Options: options.Index().SetExpireAfterSeconds(int32(apiTokenRetention.Seconds())).SetName("api_token_ttl")},
		}},
		{CollSessions, []mongo.IndexModel{
			{Keys: bson.D{{Key: "token_hash", Value: 1}}, Options: options.Index().SetUnique(true).SetName("session_token_unique")},
			{Keys: bson.D{{Key: "user_id", Value: 1}}, Options: options.Index().SetName("session_user")},
			// A TTL index removes expired sessions even if nothing sweeps them.
			{Keys: bson.D{{Key: "absolute_expiry", Value: 1}}, Options: options.Index().SetExpireAfterSeconds(0).SetName("session_ttl")},
		}},
		{CollItems, []mongo.IndexModel{
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "ingested_at", Value: -1}}, Options: options.Index().SetName("item_ws_ingested")},
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "created_at", Value: -1}}, Options: options.Index().SetName("item_ws_created")},
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "content_hash", Value: 1}}, Options: options.Index().SetName("item_ws_hash")},
			// Duplicate detection is a read followed by a write, so concurrent
			// requests carrying identical content can both pass the read. This index
			// makes the database the authority: at most one live, non-deliberate item
			// per content hash. Deliberate duplicates carry a non-empty duplicate_of
			// and tombstoned items carry deleted=true, so both fall outside it.
			{
				Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "content_hash", Value: 1}},
				Options: options.Index().SetUnique(true).SetName("item_ws_hash_unique").
					SetPartialFilterExpression(bson.D{
						{Key: "duplicate_of", Value: ""},
						{Key: "deleted", Value: false},
					}),
			},
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "status", Value: 1}}, Options: options.Index().SetName("item_ws_status")},
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "source_type", Value: 1}}, Options: options.Index().SetName("item_ws_source")},
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "mime_type", Value: 1}}, Options: options.Index().SetName("item_ws_mime")},
			{
				Keys:    bson.D{{Key: "title", Value: "text"}, {Key: "extracted_text", Value: "text"}},
				Options: options.Index().SetName("item_text").SetWeights(bson.D{{Key: "title", Value: 5}, {Key: "extracted_text", Value: 1}}),
			},
		}},
		{CollChunks, []mongo.IndexModel{
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "item_id", Value: 1}, {Key: "sequence", Value: 1}}, Options: options.Index().SetName("chunk_ws_item_seq")},
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "embedded", Value: 1}, {Key: "embedding_model", Value: 1}}, Options: options.Index().SetName("chunk_ws_embedded")},
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "content_hash", Value: 1}}, Options: options.Index().SetName("chunk_ws_hash")},
			{Keys: bson.D{{Key: "text", Value: "text"}}, Options: options.Index().SetName("chunk_text")},
		}},
		{CollEntities, []mongo.IndexModel{
			// Identity: one entity per (workspace, type, normalised name).
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "normalised_key", Value: 1}}, Options: options.Index().SetUnique(true).SetName("entity_ws_key_unique")},
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "item_ids", Value: 1}}, Options: options.Index().SetName("entity_ws_items")},
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "type", Value: 1}}, Options: options.Index().SetName("entity_ws_type")},
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "mentions", Value: -1}}, Options: options.Index().SetName("entity_ws_mentions")},
		}},
		{CollEntityAliases, []mongo.IndexModel{
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "entity_id", Value: 1}, {Key: "normalised", Value: 1}}, Options: options.Index().SetUnique(true).SetName("alias_unique")},
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "status", Value: 1}}, Options: options.Index().SetName("alias_ws_status")},
		}},
		{CollClaims, []mongo.IndexModel{
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "item_id", Value: 1}}, Options: options.Index().SetName("claim_ws_item")},
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "subject", Value: 1}}, Options: options.Index().SetName("claim_ws_subject")},
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "dedupe_key", Value: 1}}, Options: options.Index().SetUnique(true).SetName("claim_dedupe_unique")},
		}},
		{CollRelationships, []mongo.IndexModel{
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "dedupe_key", Value: 1}}, Options: options.Index().SetUnique(true).SetName("rel_dedupe_unique")},
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "source_id", Value: 1}, {Key: "score", Value: -1}}, Options: options.Index().SetName("rel_ws_source")},
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "target_id", Value: 1}, {Key: "score", Value: -1}}, Options: options.Index().SetName("rel_ws_target")},
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "type", Value: 1}}, Options: options.Index().SetName("rel_ws_type")},
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "review_status", Value: 1}, {Key: "score", Value: -1}}, Options: options.Index().SetName("rel_ws_review")},
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "provenance", Value: 1}}, Options: options.Index().SetName("rel_ws_prov")},
		}},
		{CollJobs, []mongo.IndexModel{
			// The claim query: pending jobs whose lease has expired, by priority.
			{Keys: bson.D{{Key: "status", Value: 1}, {Key: "run_after", Value: 1}, {Key: "priority", Value: -1}}, Options: options.Index().SetName("job_claim")},
			{Keys: bson.D{{Key: "status", Value: 1}, {Key: "lease_expires", Value: 1}}, Options: options.Index().SetName("job_lease")},
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "status", Value: 1}, {Key: "created_at", Value: -1}}, Options: options.Index().SetName("job_ws_status")},
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "item_id", Value: 1}}, Options: options.Index().SetName("job_ws_item")},
			// Partial unique index: at most one pending job per idempotency key,
			// so a retried enqueue does not duplicate work, while completed jobs
			// with the same key remain as history.
			{
				Keys: bson.D{{Key: "idempotency_key", Value: 1}},
				Options: options.Index().SetName("job_idempotency_pending").SetUnique(true).
					SetPartialFilterExpression(bson.D{{Key: "status", Value: "pending"}}),
			},
		}},
		{CollRuns, []mongo.IndexModel{
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "item_id", Value: 1}, {Key: "started_at", Value: -1}}, Options: options.Index().SetName("run_ws_item")},
		}},
		{CollModelVersions, []mongo.IndexModel{
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "kind", Value: 1}, {Key: "name", Value: 1}}, Options: options.Index().SetUnique(true).SetName("model_version_unique")},
		}},
		{CollAudit, []mongo.IndexModel{
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "at", Value: -1}}, Options: options.Index().SetName("audit_ws_at")},
			{Keys: bson.D{{Key: "workspace_id", Value: 1}, {Key: "action", Value: 1}, {Key: "at", Value: -1}}, Options: options.Index().SetName("audit_ws_action")},
			{Keys: bson.D{{Key: "target_id", Value: 1}}, Options: options.Index().SetName("audit_target")},
		}},
	}

	if err := s.backfillDuplicateOf(ctx); err != nil {
		return err
	}

	for _, sp := range specs {
		if _, err := s.db.Collection(sp.coll).Indexes().CreateMany(ctx, sp.models); err != nil {
			if sp.coll == CollItems && isDup(err) {
				// The unique content-hash index cannot be built because the data
				// already violates it — duplicates admitted before this index
				// existed. Say so plainly and name the fix; a bare index error
				// here would look like a configuration fault.
				return fmt.Errorf("cannot create the unique content-hash index on %s: the "+
					"collection already contains more than one live item with the same "+
					"content hash, admitted before this constraint existed. Remove or mark "+
					"the extras (set duplicate_of to the item being kept) and restart. "+
					"See migrations/README.md: %w", sp.coll, err)
			}
			return fmt.Errorf("create indexes on %s: %w", sp.coll, err)
		}
	}
	return nil
}

// backfillDuplicateOf gives older items the empty duplicate_of the partial
// unique index keys on. Items written before that field was stored
// unconditionally omit it, and an absent field does not match the index's
// filter — so without this they would sit outside the constraint and duplicates
// could still slip past. Idempotent, and a no-op once applied.
func (s *Store) backfillDuplicateOf(ctx context.Context) error {
	res, err := s.db.Collection(CollItems).UpdateMany(ctx,
		bson.D{{Key: "duplicate_of", Value: bson.D{{Key: "$exists", Value: false}}}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "duplicate_of", Value: ""}}}},
	)
	if err != nil {
		return fmt.Errorf("backfill duplicate_of on %s: %w", CollItems, err)
	}
	if res.ModifiedCount > 0 {
		slog.Info("migrated items for the unique content-hash index",
			"collection", CollItems, "items", res.ModifiedCount)
	}
	return nil
}
