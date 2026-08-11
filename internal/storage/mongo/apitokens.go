package mongo

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/andydixon/relata/internal/domain"
)

// APITokenRepo stores API credentials.
//
// Every method here works in hashes. There is no query by prefix, no partial
// match and no method that returns a secret, because the secret is not stored:
// a database read, a backup or a dump yields nothing that can authenticate.
type APITokenRepo struct{ base }

var _ domain.APITokenRepository = (*APITokenRepo)(nil)

// Create inserts a token record.
func (r *APITokenRepo) Create(ctx context.Context, t *domain.APIToken) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	if t.WorkspaceID == "" {
		return domain.ErrWorkspaceMiss
	}
	if t.TokenHash == "" {
		return fmt.Errorf("%w: a token record must carry a hash", domain.ErrInvalidInput)
	}
	_, err := r.coll.InsertOne(ctx, t)
	r.observe("create", started, err)
	return translate(err)
}

// GetByHash finds a token by the SHA-256 of its secret.
//
// The lookup is on the whole hash against a unique index. There is no
// comparison in application code for an attacker to time, and no way to probe
// for a prefix.
func (r *APITokenRepo) GetByHash(ctx context.Context, hash string) (*domain.APIToken, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	if hash == "" {
		return nil, domain.ErrNotFound
	}
	var t domain.APIToken
	err := r.coll.FindOne(ctx, bson.D{{Key: "token_hash", Value: hash}}).Decode(&t)
	r.observe("get_by_hash", started, err)
	if err != nil {
		return nil, translate(err)
	}
	return &t, nil
}

// ListForUser returns a person's tokens, newest first.
//
// Revoked and expired tokens are included: the point of the list is to show
// what exists and what happened to it, and hiding a revocation would make the
// page a worse audit surface than the log.
func (r *APITokenRepo) ListForUser(ctx context.Context, workspaceID, userID string) ([]*domain.APIToken, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	if userID == "" {
		return nil, fmt.Errorf("%w: user id must not be empty", domain.ErrInvalidInput)
	}
	f = append(f, bson.E{Key: "user_id", Value: userID})

	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(500)
	cur, err := r.coll.Find(ctx, f, opts)
	if err != nil {
		r.observe("list_for_user", started, err)
		return nil, translate(err)
	}
	defer cur.Close(ctx)

	var out []*domain.APIToken
	for cur.Next(ctx) {
		var t domain.APIToken
		if err := cur.Decode(&t); err != nil {
			r.observe("list_for_user", started, err)
			return nil, err
		}
		out = append(out, &t)
	}
	r.observe("list_for_user", started, cur.Err())
	return out, cur.Err()
}

// Get fetches one token within a workspace.
func (r *APITokenRepo) Get(ctx context.Context, workspaceID, id string) (*domain.APIToken, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	f = append(f, bson.E{Key: "_id", Value: id})

	var t domain.APIToken
	err = r.coll.FindOne(ctx, f).Decode(&t)
	r.observe("get", started, err)
	if err != nil {
		return nil, translate(err)
	}
	return &t, nil
}

// Revoke marks a token unusable.
//
// The record is kept rather than deleted, so the token's existence and its end
// stay visible. A revoked token is refused on the next request; there is no
// cached grant to wait out.
func (r *APITokenRepo) Revoke(ctx context.Context, workspaceID, id, revokedBy string, at time.Time) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return err
	}
	f = append(f, bson.E{Key: "_id", Value: id})

	update := bson.D{{Key: "$set", Value: bson.D{
		{Key: "revoked_at", Value: at},
		{Key: "revoked_by", Value: revokedBy},
	}}}
	res, err := r.coll.UpdateOne(ctx, f, update)
	r.observe("revoke", started, err)
	if err != nil {
		return translate(err)
	}
	if res.MatchedCount == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// TouchLastUsed records that a token authenticated a request.
//
// Callers throttle this; the repository does not, because it has no view of how
// often it is being asked.
func (r *APITokenRepo) TouchLastUsed(ctx context.Context, id string, at time.Time) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	_, err := r.coll.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "last_used_at", Value: at}}}})
	r.observe("touch_last_used", started, err)
	return translate(err)
}

// DeleteForUser removes every token belonging to a user.
func (r *APITokenRepo) DeleteForUser(ctx context.Context, workspaceID, userID string) (int64, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return 0, err
	}
	if userID == "" {
		return 0, fmt.Errorf("%w: user id must not be empty", domain.ErrInvalidInput)
	}
	f = append(f, bson.E{Key: "user_id", Value: userID})

	res, err := r.coll.DeleteMany(ctx, f)
	r.observe("delete_for_user", started, err)
	if err != nil {
		return 0, translate(err)
	}
	return res.DeletedCount, nil
}
