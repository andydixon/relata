package mongo

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/andydixon/relata/internal/domain"
)

// ClaimRepo stores extracted claims.
type ClaimRepo struct{ base }

var _ domain.ClaimRepository = (*ClaimRepo)(nil)

// ReplaceForItem swaps an item's claims for a new set, making claim extraction
// idempotent across reprocessing runs.
func (r *ClaimRepo) ReplaceForItem(ctx context.Context, workspaceID, itemID string, claims []*domain.Claim) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return err
	}
	f = append(f, bson.E{Key: "item_id", Value: itemID})

	if _, err := r.coll.DeleteMany(ctx, f); err != nil {
		r.observe("replace_for_item", started, err)
		return translate(err)
	}
	if len(claims) == 0 {
		r.observe("replace_for_item", started, nil)
		return nil
	}

	// Deduplicate within the batch: two chunks can yield the same claim, and
	// the unique dedupe_key index would otherwise reject the whole insert.
	seen := map[string]bool{}
	docs := make([]any, 0, len(claims))
	for _, c := range claims {
		if c.WorkspaceID != workspaceID || c.ItemID != itemID {
			return domain.ErrWorkspaceMiss
		}
		if seen[c.DedupeKey] {
			continue
		}
		seen[c.DedupeKey] = true
		docs = append(docs, c)
	}

	_, err = r.coll.InsertMany(ctx, docs, options.InsertMany().SetOrdered(false))
	if isDup(err) {
		// A concurrent run inserted an identical claim; that is the intended
		// outcome, not a failure.
		err = nil
	}
	r.observe("replace_for_item", started, err)
	return translate(err)
}

// ListByItem returns an item's claims.
func (r *ClaimRepo) ListByItem(ctx context.Context, workspaceID, itemID string) ([]*domain.Claim, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	f = append(f, bson.E{Key: "item_id", Value: itemID})

	cur, err := r.coll.Find(ctx, f, options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}))
	r.observe("list_by_item", started, err)
	if err != nil {
		return nil, translate(err)
	}
	var out []*domain.Claim
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// FindBySubjects locates claims about the same coarse subject in other items.
//
// This is how support and contradiction candidates are found without comparing
// every claim to every other claim: the subject key is indexed, so the search
// is a bounded lookup rather than a scan.
func (r *ClaimRepo) FindBySubjects(ctx context.Context, workspaceID string, subjects []string, excludeItem string, limit int) ([]*domain.Claim, error) {
	if len(subjects) == 0 {
		return nil, nil
	}
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	f = append(f, bson.E{Key: "subject", Value: bson.D{{Key: "$in", Value: stringsToAny(subjects)}}})
	if excludeItem != "" {
		f = append(f, bson.E{Key: "item_id", Value: bson.D{{Key: "$ne", Value: excludeItem}}})
	}

	if limit <= 0 || limit > 500 {
		limit = 100
	}
	cur, err := r.coll.Find(ctx, f, options.Find().SetLimit(int64(limit)))
	r.observe("find_by_subjects", started, err)
	if err != nil {
		return nil, translate(err)
	}
	var out []*domain.Claim
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetMany fetches claims by ID within a workspace.
func (r *ClaimRepo) GetMany(ctx context.Context, workspaceID string, claimIDs []string) ([]*domain.Claim, error) {
	if len(claimIDs) == 0 {
		return nil, nil
	}
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	f = append(f, bson.E{Key: "_id", Value: bson.D{{Key: "$in", Value: stringsToAny(claimIDs)}}})

	cur, err := r.coll.Find(ctx, f)
	r.observe("get_many", started, err)
	if err != nil {
		return nil, translate(err)
	}
	var out []*domain.Claim
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteByItem removes an item's claims.
func (r *ClaimRepo) DeleteByItem(ctx context.Context, workspaceID, itemID string) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return err
	}
	f = append(f, bson.E{Key: "item_id", Value: itemID})

	_, err = r.coll.DeleteMany(ctx, f)
	r.observe("delete_by_item", started, err)
	return translate(err)
}

// Count returns the number of claims in a workspace.
func (r *ClaimRepo) Count(ctx context.Context, workspaceID string) (int64, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return 0, err
	}
	n, err := r.coll.CountDocuments(ctx, f)
	r.observe("count", started, err)
	return n, translate(err)
}
