package mongo

import (
	"context"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/andydixon/relata/internal/domain"
)

// ItemRepo stores source items.
type ItemRepo struct{ base }

var _ domain.ItemRepository = (*ItemRepo)(nil)

// Create inserts a new item.
func (r *ItemRepo) Create(ctx context.Context, it *domain.Item) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	if it.WorkspaceID == "" {
		return domain.ErrWorkspaceMiss
	}
	_, err := r.coll.InsertOne(ctx, it)
	r.observe("create", started, err)
	// translate maps a duplicate-key rejection to domain.ErrDuplicate. That is
	// how the partial unique index on content_hash reaches the caller when a
	// concurrent request stored identical content first.
	return translate(err)
}

// Get fetches one item within a workspace.
func (r *ItemRepo) Get(ctx context.Context, workspaceID, id string) (*domain.Item, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	f = append(f, bson.E{Key: "_id", Value: id})

	var it domain.Item
	err = r.coll.FindOne(ctx, f).Decode(&it)
	r.observe("get", started, err)
	if err != nil {
		return nil, translate(err)
	}
	return &it, nil
}

// GetMany fetches items by ID within a workspace, preserving nothing about the
// requested order — callers that need order re-map by ID.
func (r *ItemRepo) GetMany(ctx context.Context, workspaceID string, ids []string) ([]*domain.Item, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	f = append(f, bson.E{Key: "_id", Value: bson.D{{Key: "$in", Value: stringsToAny(ids)}}})

	cur, err := r.coll.Find(ctx, f)
	r.observe("get_many", started, err)
	if err != nil {
		return nil, translate(err)
	}
	var out []*domain.Item
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Update replaces an item's mutable fields.
func (r *ItemRepo) Update(ctx context.Context, it *domain.Item) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(it.WorkspaceID)
	if err != nil {
		return err
	}
	f = append(f, bson.E{Key: "_id", Value: it.ID})
	it.UpdatedAt = time.Now().UTC()

	res, err := r.coll.ReplaceOne(ctx, f, it)
	r.observe("update", started, err)
	if err != nil {
		return translate(err)
	}
	if res.MatchedCount == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// SetStatus updates processing status and error text.
func (r *ItemRepo) SetStatus(ctx context.Context, workspaceID, id string, st domain.ProcessingStatus, errMsg string) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return err
	}
	f = append(f, bson.E{Key: "_id", Value: id})

	update := bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: string(st)},
		{Key: "error", Value: errMsg},
		{Key: "updated_at", Value: time.Now().UTC()},
	}}}

	res, err := r.coll.UpdateOne(ctx, f, update)
	r.observe("set_status", started, err)
	if err != nil {
		return translate(err)
	}
	if res.MatchedCount == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// AddWarning appends a parse warning without disturbing existing ones.
//
// $addToSet rather than $push: reprocessing an item that still has the same
// problem must not accumulate duplicate warnings.
func (r *ItemRepo) AddWarning(ctx context.Context, workspaceID, id string, w domain.ParseWarning) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return err
	}
	f = append(f, bson.E{Key: "_id", Value: id})

	update := bson.D{{Key: "$addToSet", Value: bson.D{{Key: "warnings", Value: w}}}}
	_, err = r.coll.UpdateOne(ctx, f, update)
	r.observe("add_warning", started, err)
	return translate(err)
}

// buildItemFilter converts a typed filter into a BSON document.
//
// Every value here originates as a Go typed value. Client strings become
// operands, never keys or operators.
func buildItemFilter(workspaceID string, f domain.ItemFilter) (bson.D, error) {
	q, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	if !f.IncludeDeleted {
		q = notDeleted(q)
	}
	if len(f.SourceTypes) > 0 {
		vals := make([]any, 0, len(f.SourceTypes))
		for _, s := range f.SourceTypes {
			if s.Valid() {
				vals = append(vals, string(s))
			}
		}
		if len(vals) > 0 {
			q = append(q, bson.E{Key: "source_type", Value: bson.D{{Key: "$in", Value: vals}}})
		}
	}
	if len(f.MIMETypes) > 0 {
		q = append(q, bson.E{Key: "mime_type", Value: bson.D{{Key: "$in", Value: stringsToAny(f.MIMETypes)}}})
	}
	if len(f.Statuses) > 0 {
		vals := make([]any, 0, len(f.Statuses))
		for _, s := range f.Statuses {
			vals = append(vals, string(s))
		}
		q = append(q, bson.E{Key: "status", Value: bson.D{{Key: "$in", Value: vals}}})
	}
	if f.ContentHash != "" {
		q = append(q, bson.E{Key: "content_hash", Value: f.ContentHash})
	}
	if f.CreatedFrom != nil || f.CreatedTo != nil {
		rng := bson.D{}
		if f.CreatedFrom != nil {
			rng = append(rng, bson.E{Key: "$gte", Value: *f.CreatedFrom})
		}
		if f.CreatedTo != nil {
			rng = append(rng, bson.E{Key: "$lte", Value: *f.CreatedTo})
		}
		q = append(q, bson.E{Key: "created_at", Value: rng})
	}
	if t := strings.TrimSpace(f.TitleContains); t != "" {
		// The user's text is escaped and used as a literal substring: it can
		// never become a regular expression with its own semantics.
		q = append(q, bson.E{Key: "title", Value: bson.D{
			{Key: "$regex", Value: regexpQuote(t)},
			{Key: "$options", Value: "i"},
		}})
	}
	return q, nil
}

// regexpQuote escapes every regular-expression metacharacter so that a search
// string is matched literally. Without this, a query of ".*" would match
// everything and "(((" would be a denial-of-service.
func regexpQuote(s string) string {
	const special = `\.+*?()|[]{}^$`
	var b strings.Builder
	b.Grow(len(s) * 2)
	for _, r := range s {
		if r < 128 && strings.ContainsRune(special, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// List returns a page of items and the total matching count.
func (r *ItemRepo) List(ctx context.Context, workspaceID string, f domain.ItemFilter, p domain.Page) ([]*domain.Item, int64, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	q, err := buildItemFilter(workspaceID, f)
	if err != nil {
		return nil, 0, err
	}
	p = p.Normalise(20, 200)

	total, err := r.coll.CountDocuments(ctx, q)
	if err != nil {
		r.observe("list", started, err)
		return nil, 0, translate(err)
	}

	opts := options.Find().
		SetSort(bson.D{{Key: "ingested_at", Value: -1}, {Key: "_id", Value: 1}}).
		SetSkip(int64(p.Offset)).
		SetLimit(int64(p.Limit))

	cur, err := r.coll.Find(ctx, q, opts)
	r.observe("list", started, err)
	if err != nil {
		return nil, 0, translate(err)
	}
	var out []*domain.Item
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// FindByContentHash locates an existing item with identical content.
func (r *ItemRepo) FindByContentHash(ctx context.Context, workspaceID, hash string) (*domain.Item, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	f = notDeleted(append(f, bson.E{Key: "content_hash", Value: hash}))

	var it domain.Item
	err = r.coll.FindOne(ctx, f, options.FindOne().SetSort(bson.D{{Key: "ingested_at", Value: 1}})).Decode(&it)
	r.observe("find_by_hash", started, err)
	if err != nil {
		return nil, translate(err)
	}
	return &it, nil
}

// TextSearch runs MongoDB's indexed lexical search within a workspace.
//
// The query string is passed as a value to $text, which treats it as search
// terms rather than as a document — it cannot introduce operators.
func (r *ItemRepo) TextSearch(ctx context.Context, workspaceID, query string, f domain.ItemFilter, limit int) ([]domain.LexicalHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	q, err := buildItemFilter(workspaceID, f)
	if err != nil {
		return nil, err
	}
	q = append(q, bson.E{Key: "$text", Value: bson.D{{Key: "$search", Value: query}}})

	if limit <= 0 || limit > 500 {
		limit = 60
	}
	opts := options.Find().
		SetProjection(bson.D{{Key: "score", Value: bson.D{{Key: "$meta", Value: "textScore"}}}}).
		SetSort(bson.D{{Key: "score", Value: bson.D{{Key: "$meta", Value: "textScore"}}}}).
		SetLimit(int64(limit))

	cur, err := r.coll.Find(ctx, q, opts)
	r.observe("text_search", started, err)
	if err != nil {
		return nil, translate(err)
	}
	defer cur.Close(ctx)

	var out []domain.LexicalHit
	for cur.Next(ctx) {
		var row struct {
			ID    string  `bson:"_id"`
			Score float64 `bson:"score"`
		}
		if err := cur.Decode(&row); err != nil {
			continue
		}
		out = append(out, domain.LexicalHit{ItemID: row.ID, Score: row.Score})
	}
	return out, cur.Err()
}

// Tombstone marks an item deleted while retaining the record, so that audit
// history and any references to it remain resolvable.
func (r *ItemRepo) Tombstone(ctx context.Context, workspaceID, id string) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return err
	}
	f = append(f, bson.E{Key: "_id", Value: id})

	// The original content is cleared on tombstone: a deletion request should
	// remove the user's data, while the shell of the record documents that
	// something existed and was removed.
	update := bson.D{{Key: "$set", Value: bson.D{
		{Key: "deleted", Value: true},
		{Key: "status", Value: string(domain.StatusTombstoned)},
		{Key: "original_text", Value: ""},
		{Key: "extracted_text", Value: ""},
		{Key: "updated_at", Value: time.Now().UTC()},
	}}}

	res, err := r.coll.UpdateOne(ctx, f, update)
	r.observe("tombstone", started, err)
	if err != nil {
		return translate(err)
	}
	if res.MatchedCount == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// HardDelete removes the item document entirely.
func (r *ItemRepo) HardDelete(ctx context.Context, workspaceID, id string) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return err
	}
	f = append(f, bson.E{Key: "_id", Value: id})

	res, err := r.coll.DeleteOne(ctx, f)
	r.observe("hard_delete", started, err)
	if err != nil {
		return translate(err)
	}
	if res.DeletedCount == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// Count returns the number of live items in a workspace.
func (r *ItemRepo) Count(ctx context.Context, workspaceID string) (int64, error) {
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

// CountByStatus groups live items by processing status, for the dashboard.
func (r *ItemRepo) CountByStatus(ctx context.Context, workspaceID string) (map[domain.ProcessingStatus]int64, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: notDeleted(f)}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$status"},
			{Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
	}
	cur, err := r.coll.Aggregate(ctx, pipeline)
	r.observe("count_by_status", started, err)
	if err != nil {
		return nil, translate(err)
	}
	defer cur.Close(ctx)

	out := map[domain.ProcessingStatus]int64{}
	for cur.Next(ctx) {
		var row struct {
			ID string `bson:"_id"`
			N  int64  `bson:"n"`
		}
		if err := cur.Decode(&row); err != nil {
			continue
		}
		out[domain.ProcessingStatus(row.ID)] = row.N
	}
	return out, cur.Err()
}
