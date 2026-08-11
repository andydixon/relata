package mongo

import (
	"context"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/andydixon/relata/internal/domain"
)

// ChunkRepo stores item chunks.
type ChunkRepo struct{ base }

var _ domain.ChunkRepository = (*ChunkRepo)(nil)

// ReplaceForItem swaps an item's chunks for a new set.
//
// Delete-then-insert is what makes re-chunking idempotent: reprocessing an item
// leaves exactly one generation of chunks, never two overlapping ones. The
// caller is responsible for removing the corresponding vectors, which it does
// by deleting the item's vectors before re-embedding.
func (r *ChunkRepo) ReplaceForItem(ctx context.Context, workspaceID, itemID string, chunks []*domain.Chunk) error {
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
	if len(chunks) == 0 {
		r.observe("replace_for_item", started, nil)
		return nil
	}

	docs := make([]any, 0, len(chunks))
	for _, c := range chunks {
		if c.WorkspaceID != workspaceID || c.ItemID != itemID {
			return domain.ErrWorkspaceMiss
		}
		docs = append(docs, c)
	}
	_, err = r.coll.InsertMany(ctx, docs)
	r.observe("replace_for_item", started, err)
	return translate(err)
}

// ListByItem returns an item's chunks in sequence order.
func (r *ChunkRepo) ListByItem(ctx context.Context, workspaceID, itemID string) ([]*domain.Chunk, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	f = append(f, bson.E{Key: "item_id", Value: itemID})

	cur, err := r.coll.Find(ctx, f, options.Find().SetSort(bson.D{{Key: "sequence", Value: 1}}))
	r.observe("list_by_item", started, err)
	if err != nil {
		return nil, translate(err)
	}
	var out []*domain.Chunk
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetMany fetches chunks by ID within a workspace.
func (r *ChunkRepo) GetMany(ctx context.Context, workspaceID string, ids []string) ([]*domain.Chunk, error) {
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
	var out []*domain.Chunk
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// MarkEmbedded records that chunks now have vectors under a given model.
func (r *ChunkRepo) MarkEmbedded(ctx context.Context, workspaceID string, ids []string, model string) error {
	if len(ids) == 0 {
		return nil
	}
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return err
	}
	f = append(f, bson.E{Key: "_id", Value: bson.D{{Key: "$in", Value: stringsToAny(ids)}}})

	update := bson.D{{Key: "$set", Value: bson.D{
		{Key: "embedded", Value: true},
		{Key: "embedding_model", Value: model},
	}}}
	_, err = r.coll.UpdateMany(ctx, f, update)
	r.observe("mark_embedded", started, err)
	return translate(err)
}

// ListUnembedded finds chunks lacking a vector for the given model.
//
// A chunk embedded under a different model counts as unembedded, which is what
// makes re-embedding after a model change straightforward.
func (r *ChunkRepo) ListUnembedded(ctx context.Context, workspaceID, model string, limit int) ([]*domain.Chunk, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	f = append(f, bson.E{Key: "$or", Value: bson.A{
		bson.D{{Key: "embedded", Value: bson.D{{Key: "$ne", Value: true}}}},
		bson.D{{Key: "embedding_model", Value: bson.D{{Key: "$ne", Value: model}}}},
	}})

	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	cur, err := r.coll.Find(ctx, f, options.Find().SetLimit(int64(limit)).
		SetSort(bson.D{{Key: "item_id", Value: 1}, {Key: "sequence", Value: 1}}))
	r.observe("list_unembedded", started, err)
	if err != nil {
		return nil, translate(err)
	}
	var out []*domain.Chunk
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteByItem removes an item's chunks.
func (r *ChunkRepo) DeleteByItem(ctx context.Context, workspaceID, itemID string) error {
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

// Count returns the number of chunks in a workspace.
func (r *ChunkRepo) Count(ctx context.Context, workspaceID string) (int64, error) {
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

// TextSearch finds chunks lexically, used to build result excerpts.
func (r *ChunkRepo) TextSearch(ctx context.Context, workspaceID, query string, limit int) ([]domain.ChunkHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	f = append(f, bson.E{Key: "$text", Value: bson.D{{Key: "$search", Value: query}}})

	if limit <= 0 || limit > 500 {
		limit = 60
	}
	opts := options.Find().
		SetProjection(bson.D{
			{Key: "score", Value: bson.D{{Key: "$meta", Value: "textScore"}}},
			{Key: "item_id", Value: 1},
			{Key: "text", Value: 1},
		}).
		SetSort(bson.D{{Key: "score", Value: bson.D{{Key: "$meta", Value: "textScore"}}}}).
		SetLimit(int64(limit))

	cur, err := r.coll.Find(ctx, f, opts)
	r.observe("text_search", started, err)
	if err != nil {
		return nil, translate(err)
	}
	defer cur.Close(ctx)

	var out []domain.ChunkHit
	for cur.Next(ctx) {
		var row struct {
			ID     string  `bson:"_id"`
			ItemID string  `bson:"item_id"`
			Text   string  `bson:"text"`
			Score  float64 `bson:"score"`
		}
		if err := cur.Decode(&row); err != nil {
			continue
		}
		out = append(out, domain.ChunkHit{
			ChunkID: row.ID, ItemID: row.ItemID, Text: row.Text, Score: row.Score,
		})
	}
	return out, cur.Err()
}
