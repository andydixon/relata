package mongo

import (
	"context"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/ids"
)

// EntityRepo stores extracted entities.
type EntityRepo struct{ base }

var _ domain.EntityRepository = (*EntityRepo)(nil)

// maxSourceRefsPerEntity bounds how many mention references one entity keeps.
// A name appearing thousands of times does not need thousands of stored refs;
// the count is tracked separately in Mentions.
const maxSourceRefsPerEntity = 50

// UpsertByKey merges an observation into the entity with the same
// (workspace, normalised_key), or creates it.
//
// Merging happens ONLY on an exact key match. Similar-looking names create
// separate entities and, elsewhere, a candidate alias for a human to review.
// The unique index on (workspace_id, normalised_key) makes this safe under
// concurrent workers: a racing insert loses and is retried as an update.
func (r *EntityRepo) UpsertByKey(ctx context.Context, e *domain.Entity) (*domain.Entity, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(e.WorkspaceID)
	if err != nil {
		return nil, err
	}
	f = append(f, bson.E{Key: "normalised_key", Value: e.NormalisedKey})

	now := time.Now().UTC()
	if e.ID == "" {
		e.ID = ids.New()
	}

	setOnInsert := bson.D{
		{Key: "_id", Value: e.ID},
		{Key: "workspace_id", Value: e.WorkspaceID},
		{Key: "normalised_key", Value: e.NormalisedKey},
		{Key: "type", Value: string(e.Type)},
		{Key: "canonical_name", Value: e.CanonicalName},
		{Key: "method", Value: string(e.Method)},
		{Key: "created_at", Value: now},
		{Key: "deleted", Value: false},
	}

	addToSet := bson.D{}
	if len(e.ItemIDs) > 0 {
		addToSet = append(addToSet, bson.E{Key: "item_ids", Value: bson.D{{Key: "$each", Value: stringsToAny(e.ItemIDs)}}})
	}
	if len(e.Aliases) > 0 {
		addToSet = append(addToSet, bson.E{Key: "aliases", Value: bson.D{{Key: "$each", Value: stringsToAny(e.Aliases)}}})
	}

	set := bson.D{
		{Key: "updated_at", Value: now},
		{Key: "evidence_score", Value: e.EvidenceScore},
	}
	if e.ModelVersion != "" {
		set = append(set, bson.E{Key: "model_version", Value: e.ModelVersion})
	}

	update := bson.D{
		{Key: "$setOnInsert", Value: setOnInsert},
		{Key: "$set", Value: set},
		{Key: "$inc", Value: bson.D{{Key: "mentions", Value: maxIntVal(e.Mentions, 1)}}},
	}
	if len(addToSet) > 0 {
		update = append(update, bson.E{Key: "$addToSet", Value: addToSet})
	}
	if len(e.SourceRefs) > 0 {
		refs := make([]any, 0, len(e.SourceRefs))
		for _, ref := range e.SourceRefs {
			refs = append(refs, ref)
		}
		update = append(update, bson.E{Key: "$push", Value: bson.D{
			{Key: "source_refs", Value: bson.D{
				{Key: "$each", Value: refs},
				// Keep the most recent references only, so a heavily-mentioned
				// entity cannot grow without bound.
				{Key: "$slice", Value: -maxSourceRefsPerEntity},
			}},
		}})
	}

	opts := options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)
	var out domain.Entity
	err = r.coll.FindOneAndUpdate(ctx, f, update, opts).Decode(&out)
	r.observe("upsert_by_key", started, err)
	if err != nil {
		return nil, translate(err)
	}
	return &out, nil
}

func maxIntVal(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Get fetches one entity within a workspace.
func (r *EntityRepo) Get(ctx context.Context, workspaceID, id string) (*domain.Entity, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	f = append(f, bson.E{Key: "_id", Value: id})

	var e domain.Entity
	err = r.coll.FindOne(ctx, f).Decode(&e)
	r.observe("get", started, err)
	if err != nil {
		return nil, translate(err)
	}
	return &e, nil
}

// GetMany fetches entities by ID within a workspace.
func (r *EntityRepo) GetMany(ctx context.Context, workspaceID string, entityIDs []string) ([]*domain.Entity, error) {
	if len(entityIDs) == 0 {
		return nil, nil
	}
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	f = append(f, bson.E{Key: "_id", Value: bson.D{{Key: "$in", Value: stringsToAny(entityIDs)}}})

	cur, err := r.coll.Find(ctx, f)
	r.observe("get_many", started, err)
	if err != nil {
		return nil, translate(err)
	}
	var out []*domain.Entity
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// FindByNormalised looks up entities by their exact normalised keys.
func (r *EntityRepo) FindByNormalised(ctx context.Context, workspaceID string, keys []string) ([]*domain.Entity, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	f = append(f, bson.E{Key: "normalised_key", Value: bson.D{{Key: "$in", Value: stringsToAny(keys)}}})

	cur, err := r.coll.Find(ctx, f)
	r.observe("find_by_normalised", started, err)
	if err != nil {
		return nil, translate(err)
	}
	var out []*domain.Entity
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// List returns a page of entities matching a typed filter.
func (r *EntityRepo) List(ctx context.Context, workspaceID string, ef domain.EntityFilter, p domain.Page) ([]*domain.Entity, int64, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, 0, err
	}
	f = notDeleted(f)

	if len(ef.Types) > 0 {
		vals := make([]any, 0, len(ef.Types))
		for _, t := range ef.Types {
			if t.Valid() {
				vals = append(vals, string(t))
			}
		}
		if len(vals) > 0 {
			f = append(f, bson.E{Key: "type", Value: bson.D{{Key: "$in", Value: vals}}})
		}
	}
	if ef.NormalisedKey != "" {
		f = append(f, bson.E{Key: "normalised_key", Value: ef.NormalisedKey})
	}
	if ef.ItemID != "" {
		f = append(f, bson.E{Key: "item_ids", Value: ef.ItemID})
	}
	if ef.MinMentions > 0 {
		f = append(f, bson.E{Key: "mentions", Value: bson.D{{Key: "$gte", Value: ef.MinMentions}}})
	}
	if n := strings.TrimSpace(ef.NameContains); n != "" {
		f = append(f, bson.E{Key: "canonical_name", Value: bson.D{
			{Key: "$regex", Value: regexpQuote(n)},
			{Key: "$options", Value: "i"},
		}})
	}

	p = p.Normalise(50, 500)
	total, err := r.coll.CountDocuments(ctx, f)
	if err != nil {
		r.observe("list", started, err)
		return nil, 0, translate(err)
	}

	cur, err := r.coll.Find(ctx, f, options.Find().
		SetSort(bson.D{{Key: "mentions", Value: -1}, {Key: "canonical_name", Value: 1}}).
		SetSkip(int64(p.Offset)).SetLimit(int64(p.Limit)))
	r.observe("list", started, err)
	if err != nil {
		return nil, 0, translate(err)
	}
	var out []*domain.Entity
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// ListByItem returns the entities referencing an item.
func (r *EntityRepo) ListByItem(ctx context.Context, workspaceID, itemID string) ([]*domain.Entity, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	f = notDeleted(append(f, bson.E{Key: "item_ids", Value: itemID}))

	cur, err := r.coll.Find(ctx, f, options.Find().SetSort(bson.D{{Key: "mentions", Value: -1}}))
	r.observe("list_by_item", started, err)
	if err != nil {
		return nil, translate(err)
	}
	var out []*domain.Entity
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ItemsSharingEntities returns item IDs referencing any of the given entities.
//
// This is the indexed path that keeps candidate discovery bounded: instead of
// comparing a new item with every stored item, it asks only for items that
// already share an entity.
func (r *EntityRepo) ItemsSharingEntities(ctx context.Context, workspaceID string, entityIDs []string, excludeItem string, limit int) ([]string, error) {
	if len(entityIDs) == 0 {
		return nil, nil
	}
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	f = notDeleted(append(f, bson.E{Key: "_id", Value: bson.D{{Key: "$in", Value: stringsToAny(entityIDs)}}}))

	if limit <= 0 || limit > 500 {
		limit = 100
	}

	cur, err := r.coll.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: f}},
		{{Key: "$unwind", Value: "$item_ids"}},
		{{Key: "$match", Value: bson.D{{Key: "item_ids", Value: bson.D{{Key: "$ne", Value: excludeItem}}}}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$item_ids"},
			{Key: "shared", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
		// Items sharing the most entities are the best candidates.
		{{Key: "$sort", Value: bson.D{{Key: "shared", Value: -1}}}},
		{{Key: "$limit", Value: limit}},
	})
	r.observe("items_sharing_entities", started, err)
	if err != nil {
		return nil, translate(err)
	}
	defer cur.Close(ctx)

	var out []string
	for cur.Next(ctx) {
		var row struct {
			ID string `bson:"_id"`
		}
		if err := cur.Decode(&row); err == nil && row.ID != "" {
			out = append(out, row.ID)
		}
	}
	return out, cur.Err()
}

// RemoveItemRefs strips an item from every entity that references it and
// returns the IDs of entities left with no references at all.
//
// Those orphans are what item deletion removes: an entity that exists only
// because of a deleted item has no evidence left to justify it.
func (r *EntityRepo) RemoveItemRefs(ctx context.Context, workspaceID, itemID string) ([]string, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	f = append(f, bson.E{Key: "item_ids", Value: itemID})

	update := bson.D{
		{Key: "$pull", Value: bson.D{
			{Key: "item_ids", Value: itemID},
			{Key: "source_refs", Value: bson.D{{Key: "item_id", Value: itemID}}},
		}},
		{Key: "$set", Value: bson.D{{Key: "updated_at", Value: time.Now().UTC()}}},
	}
	if _, err := r.coll.UpdateMany(ctx, f, update); err != nil {
		r.observe("remove_item_refs", started, err)
		return nil, translate(err)
	}

	// Find entities that now reference nothing.
	orphanFilter, _ := wsFilter(workspaceID)
	orphanFilter = append(orphanFilter, bson.E{Key: "$or", Value: bson.A{
		bson.D{{Key: "item_ids", Value: bson.D{{Key: "$size", Value: 0}}}},
		bson.D{{Key: "item_ids", Value: bson.D{{Key: "$exists", Value: false}}}},
	}})

	cur, err := r.coll.Find(ctx, orphanFilter, options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}}))
	r.observe("remove_item_refs", started, err)
	if err != nil {
		return nil, translate(err)
	}
	defer cur.Close(ctx)

	var orphans []string
	for cur.Next(ctx) {
		var row struct {
			ID string `bson:"_id"`
		}
		if err := cur.Decode(&row); err == nil {
			orphans = append(orphans, row.ID)
		}
	}
	return orphans, cur.Err()
}

// DeleteMany removes entities by ID within a workspace.
func (r *EntityRepo) DeleteMany(ctx context.Context, workspaceID string, entityIDs []string) error {
	if len(entityIDs) == 0 {
		return nil
	}
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return err
	}
	f = append(f, bson.E{Key: "_id", Value: bson.D{{Key: "$in", Value: stringsToAny(entityIDs)}}})

	_, err = r.coll.DeleteMany(ctx, f)
	r.observe("delete_many", started, err)
	return translate(err)
}

// Count returns the number of live entities in a workspace.
func (r *EntityRepo) Count(ctx context.Context, workspaceID string) (int64, error) {
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

// CorpusFrequency reports how many items reference each of the given entities,
// which the scorer uses to discount ubiquitous names.
func (r *EntityRepo) CorpusFrequency(ctx context.Context, workspaceID string, entityIDs []string) (map[string]int, error) {
	if len(entityIDs) == 0 {
		return nil, nil
	}
	ctx, cancel := r.ctx(ctx)
	defer cancel()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	f = append(f, bson.E{Key: "_id", Value: bson.D{{Key: "$in", Value: stringsToAny(entityIDs)}}})

	cur, err := r.coll.Find(ctx, f, options.Find().SetProjection(bson.D{{Key: "item_ids", Value: 1}}))
	if err != nil {
		return nil, translate(err)
	}
	defer cur.Close(ctx)

	out := map[string]int{}
	for cur.Next(ctx) {
		var row struct {
			ID      string   `bson:"_id"`
			ItemIDs []string `bson:"item_ids"`
		}
		if err := cur.Decode(&row); err == nil {
			out[row.ID] = len(row.ItemIDs)
		}
	}
	return out, cur.Err()
}

// ---------------------------------------------------------------------------

// AliasRepo stores candidate and confirmed entity aliases.
type AliasRepo struct{ base }

var _ domain.AliasRepository = (*AliasRepo)(nil)

// Upsert records an alias proposal, leaving an existing human decision intact.
func (r *AliasRepo) Upsert(ctx context.Context, a *domain.EntityAlias) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(a.WorkspaceID)
	if err != nil {
		return err
	}
	f = append(f,
		bson.E{Key: "entity_id", Value: a.EntityID},
		bson.E{Key: "normalised", Value: a.Normalised},
	)

	now := time.Now().UTC()
	if a.ID == "" {
		a.ID = ids.New()
	}

	// Status is set only on insert: re-running discovery must not reset an
	// alias a person has already confirmed or rejected.
	update := bson.D{
		{Key: "$setOnInsert", Value: bson.D{
			{Key: "_id", Value: a.ID},
			{Key: "workspace_id", Value: a.WorkspaceID},
			{Key: "entity_id", Value: a.EntityID},
			{Key: "normalised", Value: a.Normalised},
			{Key: "status", Value: string(domain.AliasCandidate)},
			{Key: "created_at", Value: now},
		}},
		{Key: "$set", Value: bson.D{
			{Key: "alias", Value: a.Alias},
			{Key: "candidate_entity_id", Value: a.CandidateEntityID},
			{Key: "method", Value: string(a.Method)},
			{Key: "similarity", Value: a.Similarity},
			{Key: "reason", Value: a.Reason},
			{Key: "updated_at", Value: now},
		}},
	}

	_, err = r.coll.UpdateOne(ctx, f, update, options.UpdateOne().SetUpsert(true))
	r.observe("upsert", started, err)
	if isDup(err) {
		return nil // a concurrent worker inserted the same proposal
	}
	return translate(err)
}

// ListByEntity returns aliases proposed for an entity.
func (r *AliasRepo) ListByEntity(ctx context.Context, workspaceID, entityID string) ([]*domain.EntityAlias, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	f = append(f, bson.E{Key: "entity_id", Value: entityID})

	cur, err := r.coll.Find(ctx, f, options.Find().SetSort(bson.D{{Key: "similarity", Value: -1}}))
	r.observe("list_by_entity", started, err)
	if err != nil {
		return nil, translate(err)
	}
	var out []*domain.EntityAlias
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// List returns aliases with a given status.
func (r *AliasRepo) List(ctx context.Context, workspaceID string, status domain.AliasStatus, p domain.Page) ([]*domain.EntityAlias, int64, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, 0, err
	}
	if status != "" {
		f = append(f, bson.E{Key: "status", Value: string(status)})
	}
	p = p.Normalise(50, 200)

	total, err := r.coll.CountDocuments(ctx, f)
	if err != nil {
		return nil, 0, translate(err)
	}
	cur, err := r.coll.Find(ctx, f, options.Find().
		SetSort(bson.D{{Key: "similarity", Value: -1}}).
		SetSkip(int64(p.Offset)).SetLimit(int64(p.Limit)))
	r.observe("list", started, err)
	if err != nil {
		return nil, 0, translate(err)
	}
	var out []*domain.EntityAlias
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// SetStatus records a human decision about an alias.
func (r *AliasRepo) SetStatus(ctx context.Context, workspaceID, id string, st domain.AliasStatus) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return err
	}
	f = append(f, bson.E{Key: "_id", Value: id})

	res, err := r.coll.UpdateOne(ctx, f, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: string(st)},
		{Key: "updated_at", Value: time.Now().UTC()},
	}}})
	r.observe("set_status", started, err)
	if err != nil {
		return translate(err)
	}
	if res.MatchedCount == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// DeleteByEntity removes an entity's aliases.
func (r *AliasRepo) DeleteByEntity(ctx context.Context, workspaceID, entityID string) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return err
	}
	f = append(f, bson.E{Key: "entity_id", Value: entityID})

	_, err = r.coll.DeleteMany(ctx, f)
	r.observe("delete_by_entity", started, err)
	return translate(err)
}
