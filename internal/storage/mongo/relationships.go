package mongo

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/ids"
)

// RelationshipRepo stores relationships and their review history.
type RelationshipRepo struct{ base }

var _ domain.RelationshipRepository = (*RelationshipRepo)(nil)

// FindByDedupeKey returns the relationship with the given dedupe key.
func (r *RelationshipRepo) FindByDedupeKey(ctx context.Context, workspaceID, dedupeKey string) (*domain.Relationship, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	if dedupeKey == "" {
		return nil, fmt.Errorf("%w: dedupe key must not be empty", domain.ErrInvalidInput)
	}
	f = append(f, bson.E{Key: "dedupe_key", Value: dedupeKey})

	out, err := r.findOne(ctx, notDeleted(f))
	r.observe("find_by_dedupe_key", started, err)
	return out, err
}

// UpsertByDedupeKey inserts or refreshes a discovered relationship.
//
// Two properties matter here and are the reason this is not a plain upsert:
//
//   - Reprocessing must not create duplicates. The dedupe key is derived from
//     the endpoints and type, and a unique index enforces one document per key.
//   - A human decision is never overwritten by a later analysis run. When the
//     relationship has been confirmed, rejected or edited, only the evidence is
//     refreshed; the type, provenance and review status stay as the person left
//     them, and the score keeps reflecting their decision.
func (r *RelationshipRepo) UpsertByDedupeKey(ctx context.Context, rel *domain.Relationship) (*domain.Relationship, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(rel.WorkspaceID)
	if err != nil {
		return nil, err
	}
	f = append(f, bson.E{Key: "dedupe_key", Value: rel.DedupeKey})

	// If a person has already ruled on this relationship, refresh only the
	// evidence and leave their decision intact.
	existing, err := r.findOne(ctx, f)
	if err != nil && err != domain.ErrNotFound {
		return nil, err
	}
	if existing != nil && existing.ReviewStatus != domain.ReviewPending {
		update := bson.D{{Key: "$set", Value: bson.D{
			{Key: "evidence", Value: rel.Evidence},
			{Key: "updated_at", Value: time.Now().UTC()},
		}}}
		if _, err := r.coll.UpdateOne(ctx, f, update); err != nil {
			r.observe("upsert", started, err)
			return nil, translate(err)
		}
		refreshed, err := r.findOne(ctx, f)
		r.observe("upsert", started, err)
		return refreshed, err
	}

	now := time.Now().UTC()
	if rel.ID == "" {
		rel.ID = ids.New()
	}

	update := bson.D{
		{Key: "$setOnInsert", Value: bson.D{
			{Key: "_id", Value: rel.ID},
			{Key: "workspace_id", Value: rel.WorkspaceID},
			{Key: "dedupe_key", Value: rel.DedupeKey},
			{Key: "source_kind", Value: string(rel.SourceKind)},
			{Key: "source_id", Value: rel.SourceID},
			{Key: "target_kind", Value: string(rel.TargetKind)},
			{Key: "target_id", Value: rel.TargetID},
			{Key: "original_type", Value: string(rel.Type)},
			{Key: "original_provenance", Value: string(rel.Provenance)},
			{Key: "review_status", Value: string(domain.ReviewPending)},
			{Key: "created_at", Value: now},
			{Key: "deleted", Value: false},
		}},
		{Key: "$set", Value: bson.D{
			{Key: "type", Value: string(rel.Type)},
			{Key: "provenance", Value: string(rel.Provenance)},
			{Key: "score", Value: rel.Score},
			{Key: "score_version", Value: rel.ScoreVersion},
			{Key: "evidence", Value: rel.Evidence},
			{Key: "updated_at", Value: now},
		}},
	}

	opts := options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)
	var out domain.Relationship
	err = r.coll.FindOneAndUpdate(ctx, f, update, opts).Decode(&out)
	r.observe("upsert", started, err)
	if err != nil {
		return nil, translate(err)
	}
	return &out, nil
}

func (r *RelationshipRepo) findOne(ctx context.Context, f bson.D) (*domain.Relationship, error) {
	var rel domain.Relationship
	if err := r.coll.FindOne(ctx, f).Decode(&rel); err != nil {
		return nil, translate(err)
	}
	return &rel, nil
}

// Create inserts a relationship, typically one a user added by hand.
func (r *RelationshipRepo) Create(ctx context.Context, rel *domain.Relationship) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	if rel.WorkspaceID == "" {
		return domain.ErrWorkspaceMiss
	}
	_, err := r.coll.InsertOne(ctx, rel)
	r.observe("create", started, err)
	return translate(err)
}

// Get fetches one relationship within a workspace.
func (r *RelationshipRepo) Get(ctx context.Context, workspaceID, id string) (*domain.Relationship, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	f = append(f, bson.E{Key: "_id", Value: id})

	rel, err := r.findOne(ctx, f)
	r.observe("get", started, err)
	return rel, err
}

// buildRelFilter converts a typed filter into BSON.
func buildRelFilter(workspaceID string, rf domain.RelationshipFilter) (bson.D, error) {
	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	f = notDeleted(f)

	if len(rf.Types) > 0 {
		vals := make([]any, 0, len(rf.Types))
		for _, t := range rf.Types {
			if t.Valid() {
				vals = append(vals, string(t))
			}
		}
		if len(vals) > 0 {
			f = append(f, bson.E{Key: "type", Value: bson.D{{Key: "$in", Value: vals}}})
		}
	}
	if len(rf.Provenances) > 0 {
		vals := make([]any, 0, len(rf.Provenances))
		for _, p := range rf.Provenances {
			if p.Valid() {
				vals = append(vals, string(p))
			}
		}
		if len(vals) > 0 {
			f = append(f, bson.E{Key: "provenance", Value: bson.D{{Key: "$in", Value: vals}}})
		}
	}
	if rf.ConfirmedOnly {
		f = append(f, bson.E{Key: "review_status", Value: string(domain.ReviewConfirmed)})
	} else if len(rf.ReviewStatus) > 0 {
		vals := make([]any, 0, len(rf.ReviewStatus))
		for _, s := range rf.ReviewStatus {
			vals = append(vals, string(s))
		}
		f = append(f, bson.E{Key: "review_status", Value: bson.D{{Key: "$in", Value: vals}}})
	}
	if rf.MinScore > 0 {
		f = append(f, bson.E{Key: "score", Value: bson.D{{Key: "$gte", Value: rf.MinScore}}})
	}
	if rf.NodeID != "" {
		switch rf.Direction {
		case "out":
			f = append(f, bson.E{Key: "source_id", Value: rf.NodeID})
		case "in":
			f = append(f, bson.E{Key: "target_id", Value: rf.NodeID})
		default:
			f = append(f, bson.E{Key: "$or", Value: bson.A{
				bson.D{{Key: "source_id", Value: rf.NodeID}},
				bson.D{{Key: "target_id", Value: rf.NodeID}},
			}})
		}
	}
	return f, nil
}

// List returns a page of relationships.
func (r *RelationshipRepo) List(ctx context.Context, workspaceID string, rf domain.RelationshipFilter, p domain.Page) ([]*domain.Relationship, int64, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := buildRelFilter(workspaceID, rf)
	if err != nil {
		return nil, 0, err
	}
	p = p.Normalise(25, 200)

	total, err := r.coll.CountDocuments(ctx, f)
	if err != nil {
		r.observe("list", started, err)
		return nil, 0, translate(err)
	}
	cur, err := r.coll.Find(ctx, f, options.Find().
		SetSort(bson.D{{Key: "score", Value: -1}, {Key: "_id", Value: 1}}).
		SetSkip(int64(p.Offset)).SetLimit(int64(p.Limit)))
	r.observe("list", started, err)
	if err != nil {
		return nil, 0, translate(err)
	}
	var out []*domain.Relationship
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// ListForNode returns a node's outgoing and incoming relationships separately,
// which is what the item detail page shows.
func (r *RelationshipRepo) ListForNode(ctx context.Context, workspaceID string, kind domain.NodeKind, id string, rf domain.RelationshipFilter) (out, in []*domain.Relationship, err error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	fetch := func(direction string) ([]*domain.Relationship, error) {
		sub := rf
		sub.NodeID = id
		sub.NodeKind = kind
		sub.Direction = direction
		f, err := buildRelFilter(workspaceID, sub)
		if err != nil {
			return nil, err
		}
		cur, err := r.coll.Find(ctx, f, options.Find().
			SetSort(bson.D{{Key: "score", Value: -1}}).SetLimit(200))
		if err != nil {
			return nil, translate(err)
		}
		var rels []*domain.Relationship
		if err := cur.All(ctx, &rels); err != nil {
			return nil, err
		}
		return rels, nil
	}

	out, err = fetch("out")
	if err != nil {
		r.observe("list_for_node", started, err)
		return nil, nil, err
	}
	in, err = fetch("in")
	r.observe("list_for_node", started, err)
	if err != nil {
		return nil, nil, err
	}
	return out, in, nil
}

// ListForItems fetches relationships touching any of the given items, used for
// relationship expansion during search.
func (r *RelationshipRepo) ListForItems(ctx context.Context, workspaceID string, itemIDs []string, minScore float64) ([]*domain.Relationship, error) {
	if len(itemIDs) == 0 {
		return nil, nil
	}
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	f = notDeleted(f)
	vals := stringsToAny(itemIDs)
	f = append(f, bson.E{Key: "$or", Value: bson.A{
		bson.D{{Key: "source_id", Value: bson.D{{Key: "$in", Value: vals}}}},
		bson.D{{Key: "target_id", Value: bson.D{{Key: "$in", Value: vals}}}},
	}})
	if minScore > 0 {
		f = append(f, bson.E{Key: "score", Value: bson.D{{Key: "$gte", Value: minScore}}})
	}
	// Rejected relationships are excluded from expansion: a person has said
	// this link is wrong, so it must not pull results in.
	f = append(f, bson.E{Key: "review_status", Value: bson.D{{Key: "$ne", Value: string(domain.ReviewRejected)}}})

	cur, err := r.coll.Find(ctx, f, options.Find().
		SetSort(bson.D{{Key: "score", Value: -1}}).SetLimit(500))
	r.observe("list_for_items", started, err)
	if err != nil {
		return nil, translate(err)
	}
	var rels []*domain.Relationship
	if err := cur.All(ctx, &rels); err != nil {
		return nil, err
	}
	return rels, nil
}

// Update replaces a relationship document.
func (r *RelationshipRepo) Update(ctx context.Context, rel *domain.Relationship) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(rel.WorkspaceID)
	if err != nil {
		return err
	}
	f = append(f, bson.E{Key: "_id", Value: rel.ID})
	rel.UpdatedAt = time.Now().UTC()

	res, err := r.coll.ReplaceOne(ctx, f, rel)
	r.observe("update", started, err)
	if err != nil {
		return translate(err)
	}
	if res.MatchedCount == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// AppendReview records a human decision.
//
// The review history is append-only and the original proposal fields are never
// touched, so rejecting a relationship documents the disagreement rather than
// erasing what analysis suggested.
func (r *RelationshipRepo) AppendReview(ctx context.Context, workspaceID, id string, ev domain.ReviewEvent, newStatus domain.ReviewStatus, newType domain.RelationshipType, newProv domain.Provenance) (*domain.Relationship, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	f = append(f, bson.E{Key: "_id", Value: id})

	set := bson.D{
		{Key: "review_status", Value: string(newStatus)},
		{Key: "updated_at", Value: time.Now().UTC()},
	}
	if newType != "" {
		set = append(set, bson.E{Key: "type", Value: string(newType)})
	}
	if newProv != "" {
		set = append(set, bson.E{Key: "provenance", Value: string(newProv)})
	}

	update := bson.D{
		{Key: "$set", Value: set},
		{Key: "$push", Value: bson.D{{Key: "review_history", Value: ev}}},
	}

	opts := options.FindOneAndUpdate().SetReturnDocument(options.After)
	var out domain.Relationship
	err = r.coll.FindOneAndUpdate(ctx, f, update, opts).Decode(&out)
	r.observe("append_review", started, err)
	if err != nil {
		return nil, translate(err)
	}
	return &out, nil
}

// TombstoneForItem marks an item's relationships deleted while retaining them.
func (r *RelationshipRepo) TombstoneForItem(ctx context.Context, workspaceID, itemID string) (int64, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := r.itemFilter(workspaceID, itemID)
	if err != nil {
		return 0, err
	}
	res, err := r.coll.UpdateMany(ctx, f, bson.D{{Key: "$set", Value: bson.D{
		{Key: "deleted", Value: true},
		{Key: "updated_at", Value: time.Now().UTC()},
	}}})
	r.observe("tombstone_for_item", started, err)
	if err != nil {
		return 0, translate(err)
	}
	return res.ModifiedCount, nil
}

// DeleteForItem removes an item's relationships outright.
func (r *RelationshipRepo) DeleteForItem(ctx context.Context, workspaceID, itemID string) (int64, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := r.itemFilter(workspaceID, itemID)
	if err != nil {
		return 0, err
	}
	res, err := r.coll.DeleteMany(ctx, f)
	r.observe("delete_for_item", started, err)
	if err != nil {
		return 0, translate(err)
	}
	return res.DeletedCount, nil
}

func (r *RelationshipRepo) itemFilter(workspaceID, itemID string) (bson.D, error) {
	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	return append(f, bson.E{Key: "$or", Value: bson.A{
		bson.D{{Key: "source_id", Value: itemID}},
		bson.D{{Key: "target_id", Value: itemID}},
	}}), nil
}

// Count returns the number of live relationships in a workspace.
func (r *RelationshipRepo) Count(ctx context.Context, workspaceID string) (int64, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return 0, err
	}
	n, err := r.coll.CountDocuments(ctx, notDeleted(f))
	r.observe("count", started, err)
	return n, translate(err)
}

// CountPendingReview returns how many relationships await human review.
func (r *RelationshipRepo) CountPendingReview(ctx context.Context, workspaceID string) (int64, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return 0, err
	}
	f = notDeleted(append(f, bson.E{Key: "review_status", Value: string(domain.ReviewPending)}))

	n, err := r.coll.CountDocuments(ctx, f)
	r.observe("count_pending", started, err)
	return n, translate(err)
}
