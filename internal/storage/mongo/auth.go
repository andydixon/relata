package mongo

import (
	"context"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/andydixon/relata/internal/domain"
)

// UserRepo stores local accounts.
type UserRepo struct{ base }

var _ domain.UserRepository = (*UserRepo)(nil)

// Create inserts a user. Usernames are stored lowercased and a unique index
// enforces that two accounts cannot differ only by case.
func (r *UserRepo) Create(ctx context.Context, u *domain.User) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	u.Username = strings.ToLower(strings.TrimSpace(u.Username))
	_, err := r.coll.InsertOne(ctx, u)
	r.observe("create", started, err)
	return translate(err)
}

// GetByUsername looks up an account for authentication.
func (r *UserRepo) GetByUsername(ctx context.Context, username string) (*domain.User, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f := bson.D{{Key: "username", Value: strings.ToLower(strings.TrimSpace(username))}}
	var u domain.User
	err := r.coll.FindOne(ctx, f).Decode(&u)
	r.observe("get_by_username", started, err)
	if err != nil {
		return nil, translate(err)
	}
	return &u, nil
}

// GetByOAuthSubject looks up a federated account.
//
// The filter is the (provider, subject) pair and nothing else. Falling back to
// the email address would let a provider that recycled one hand over the
// previous holder's workspace.
func (r *UserRepo) GetByOAuthSubject(ctx context.Context, provider, subject string) (*domain.User, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	provider, subject = strings.TrimSpace(provider), strings.TrimSpace(subject)
	if provider == "" || subject == "" {
		return nil, domain.ErrNotFound
	}

	f := bson.D{
		{Key: "oauth_provider", Value: provider},
		{Key: "oauth_subject", Value: subject},
	}
	var u domain.User
	err := r.coll.FindOne(ctx, f).Decode(&u)
	r.observe("get_by_oauth_subject", started, err)
	if err != nil {
		return nil, translate(err)
	}
	return &u, nil
}

// GetByID fetches a user by ID.
func (r *UserRepo) GetByID(ctx context.Context, id string) (*domain.User, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	var u domain.User
	err := r.coll.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&u)
	r.observe("get_by_id", started, err)
	if err != nil {
		return nil, translate(err)
	}
	return &u, nil
}

// Update replaces a user document.
func (r *UserRepo) Update(ctx context.Context, u *domain.User) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	u.UpdatedAt = time.Now().UTC()
	res, err := r.coll.ReplaceOne(ctx, bson.D{{Key: "_id", Value: u.ID}}, u)
	r.observe("update", started, err)
	if err != nil {
		return translate(err)
	}
	if res.MatchedCount == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// Count returns the number of accounts, used to detect first-run setup.
func (r *UserRepo) Count(ctx context.Context) (int64, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	n, err := r.coll.CountDocuments(ctx, bson.D{})
	r.observe("count", started, err)
	return n, translate(err)
}

// ---------------------------------------------------------------------------

// SessionRepo stores browser sessions.
type SessionRepo struct{ base }

var _ domain.SessionRepository = (*SessionRepo)(nil)

// Create inserts a session record.
func (r *SessionRepo) Create(ctx context.Context, s *domain.Session) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	_, err := r.coll.InsertOne(ctx, s)
	r.observe("create", started, err)
	return translate(err)
}

// GetByTokenHash looks up a session by the hash of its cookie token.
//
// The lookup is by hash, never by the token itself, so the stored record cannot
// be replayed as a credential by anyone who can read the database.
func (r *SessionRepo) GetByTokenHash(ctx context.Context, hash string) (*domain.Session, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	var s domain.Session
	err := r.coll.FindOne(ctx, bson.D{{Key: "token_hash", Value: hash}}).Decode(&s)
	r.observe("get_by_token", started, err)
	if err != nil {
		return nil, translate(err)
	}
	return &s, nil
}

// Touch extends a session's idle expiry.
func (r *SessionRepo) Touch(ctx context.Context, id string, lastSeen, expires time.Time) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	_, err := r.coll.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "last_seen_at", Value: lastSeen},
			{Key: "expires_at", Value: expires},
		}}})
	r.observe("touch", started, err)
	return translate(err)
}

// Revoke invalidates one session.
func (r *SessionRepo) Revoke(ctx context.Context, id string) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	_, err := r.coll.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "revoked", Value: true}}}})
	r.observe("revoke", started, err)
	return translate(err)
}

// RevokeAllForUser invalidates every session belonging to a user.
func (r *SessionRepo) RevokeAllForUser(ctx context.Context, userID string) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	_, err := r.coll.UpdateMany(ctx,
		bson.D{{Key: "user_id", Value: userID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "revoked", Value: true}}}})
	r.observe("revoke_all", started, err)
	return translate(err)
}

// DeleteExpired removes sessions past their absolute expiry. A TTL index does
// this too; the explicit sweep makes the behaviour testable and prompt.
func (r *SessionRepo) DeleteExpired(ctx context.Context, before time.Time) (int64, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	res, err := r.coll.DeleteMany(ctx, bson.D{
		{Key: "absolute_expiry", Value: bson.D{{Key: "$lt", Value: before}}},
	})
	r.observe("delete_expired", started, err)
	if err != nil {
		return 0, translate(err)
	}
	return res.DeletedCount, nil
}

// ---------------------------------------------------------------------------

// WorkspaceRepo stores workspaces.
type WorkspaceRepo struct{ base }

var _ domain.WorkspaceRepository = (*WorkspaceRepo)(nil)

// Create inserts a workspace.
func (r *WorkspaceRepo) Create(ctx context.Context, w *domain.Workspace) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	_, err := r.coll.InsertOne(ctx, w)
	r.observe("create", started, err)
	return translate(err)
}

// Get fetches a workspace by ID.
func (r *WorkspaceRepo) Get(ctx context.Context, id string) (*domain.Workspace, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	var w domain.Workspace
	err := r.coll.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&w)
	r.observe("get", started, err)
	if err != nil {
		return nil, translate(err)
	}
	return &w, nil
}

// GetBySlug fetches a workspace by its URL-safe name.
func (r *WorkspaceRepo) GetBySlug(ctx context.Context, slug string) (*domain.Workspace, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	var w domain.Workspace
	err := r.coll.FindOne(ctx, bson.D{{Key: "slug", Value: slug}}).Decode(&w)
	r.observe("get_by_slug", started, err)
	if err != nil {
		return nil, translate(err)
	}
	return &w, nil
}

// List returns every workspace.
func (r *WorkspaceRepo) List(ctx context.Context) ([]*domain.Workspace, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	cur, err := r.coll.Find(ctx, bson.D{}, options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}))
	r.observe("list", started, err)
	if err != nil {
		return nil, translate(err)
	}
	var out []*domain.Workspace
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}
