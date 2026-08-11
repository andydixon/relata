package mongo

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/ids"
)

// AuditRepo is an append-only record of consequential actions.
type AuditRepo struct{ base }

var _ domain.AuditRepository = (*AuditRepo)(nil)

// Record appends an audit event. There is deliberately no update or delete.
func (r *AuditRepo) Record(ctx context.Context, ev *domain.AuditEvent) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	if ev.ID == "" {
		ev.ID = ids.New()
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	_, err := r.coll.InsertOne(ctx, ev)
	r.observe("record", started, err)
	return translate(err)
}

// List returns a page of audit events, newest first.
func (r *AuditRepo) List(ctx context.Context, workspaceID string, p domain.Page) ([]*domain.AuditEvent, int64, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, 0, err
	}
	p = p.Normalise(50, 200)

	total, err := r.coll.CountDocuments(ctx, f)
	if err != nil {
		return nil, 0, translate(err)
	}
	cur, err := r.coll.Find(ctx, f, options.Find().
		SetSort(bson.D{{Key: "at", Value: -1}}).
		SetSkip(int64(p.Offset)).SetLimit(int64(p.Limit)))
	r.observe("list", started, err)
	if err != nil {
		return nil, 0, translate(err)
	}
	var out []*domain.AuditEvent
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// ---------------------------------------------------------------------------

// RunRepo stores processing run history.
type RunRepo struct{ base }

var _ domain.RunRepository = (*RunRepo)(nil)

// Create opens a processing run.
func (r *RunRepo) Create(ctx context.Context, run *domain.ProcessingRun) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	if run.ID == "" {
		run.ID = ids.New()
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now().UTC()
	}
	if run.Outcome == "" {
		run.Outcome = "running"
	}
	_, err := r.coll.InsertOne(ctx, run)
	r.observe("create", started, err)
	return translate(err)
}

// Finish closes a processing run with its outcome and statistics.
//
// Runs are never deleted: a reprocess adds a new run, so the record of what a
// previous parser or model produced survives for audit.
func (r *RunRepo) Finish(ctx context.Context, id, outcome, errMsg string, stats map[string]int) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	now := time.Now().UTC()
	_, err := r.coll.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "ended_at", Value: now},
			{Key: "outcome", Value: outcome},
			{Key: "error", Value: errMsg},
			{Key: "stats", Value: stats},
		}}})
	r.observe("finish", started, err)
	return translate(err)
}

// ListByItem returns an item's processing history, newest first.
func (r *RunRepo) ListByItem(ctx context.Context, workspaceID, itemID string, limit int) ([]*domain.ProcessingRun, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	f = append(f, bson.E{Key: "item_id", Value: itemID})

	if limit <= 0 || limit > 100 {
		limit = 20
	}
	cur, err := r.coll.Find(ctx, f, options.Find().
		SetSort(bson.D{{Key: "started_at", Value: -1}}).SetLimit(int64(limit)))
	r.observe("list_by_item", started, err)
	if err != nil {
		return nil, translate(err)
	}
	var out []*domain.ProcessingRun
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteByItem removes an item's run history, used by hard deletion.
func (r *RunRepo) DeleteByItem(ctx context.Context, workspaceID, itemID string) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return err
	}
	f = append(f, bson.E{Key: "item_id", Value: itemID})
	_, err = r.coll.DeleteMany(ctx, f)
	return translate(err)
}

// ---------------------------------------------------------------------------

// ModelVersionRepo tracks which models have produced stored data.
type ModelVersionRepo struct{ base }

var _ domain.ModelVersionRepository = (*ModelVersionRepo)(nil)

// Observe records that a model was used, so a configuration change can be
// detected and the affected records identified for reprocessing.
func (r *ModelVersionRepo) Observe(ctx context.Context, workspaceID, kind, name string, dims int) error {
	if name == "" {
		return nil
	}
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return err
	}
	f = append(f, bson.E{Key: "kind", Value: kind}, bson.E{Key: "name", Value: name})

	now := time.Now().UTC()
	update := bson.D{
		{Key: "$setOnInsert", Value: bson.D{
			{Key: "_id", Value: ids.New()},
			{Key: "workspace_id", Value: workspaceID},
			{Key: "kind", Value: kind},
			{Key: "name", Value: name},
			{Key: "first_seen_at", Value: now},
		}},
		{Key: "$set", Value: bson.D{
			{Key: "last_seen_at", Value: now},
			{Key: "dimensions", Value: dims},
		}},
	}
	_, err = r.coll.UpdateOne(ctx, f, update, options.UpdateOne().SetUpsert(true))
	if isDup(err) {
		err = nil
	}
	r.observe("observe", started, err)
	return translate(err)
}

// List returns the models seen in a workspace.
func (r *ModelVersionRepo) List(ctx context.Context, workspaceID string) ([]*domain.ModelVersion, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	cur, err := r.coll.Find(ctx, f, options.Find().SetSort(bson.D{
		{Key: "kind", Value: 1}, {Key: "name", Value: 1},
	}))
	r.observe("list", started, err)
	if err != nil {
		return nil, translate(err)
	}
	var out []*domain.ModelVersion
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}
