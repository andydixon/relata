// Package qdrant implements the vector store port against Qdrant's REST API.
//
// Payloads deliberately hold only what retrieval needs — workspace, item,
// chunk, source type, creation date and embedding model. Document text and
// relationship data stay in MongoDB, which remains the authoritative store.
// Keeping the payload thin is what stops Qdrant drifting into a second,
// unversioned copy of the corpus.
package qdrant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/andydixon/relata/internal/config"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/observability"
)

// Client talks to Qdrant over REST.
type Client struct {
	base       string
	collection string
	apiKey     string
	http       *http.Client
	metrics    *observability.Metrics
	timeout    time.Duration
}

var _ domain.VectorStore = (*Client)(nil)

// New builds a Qdrant client.
func New(cfg config.QdrantConfig, m *observability.Metrics) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &Client{
		base:       strings.TrimRight(cfg.URL, "/"),
		collection: cfg.Collection,
		apiKey:     cfg.APIKey,
		timeout:    timeout,
		metrics:    m,
		http: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Collection returns the configured collection name.
func (c *Client) Collection() string { return c.collection }

// do performs a bounded JSON request against Qdrant.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	started := time.Now()
	op := method + " " + strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)[0]

	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode qdrant request: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("api-key", c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.observe(op, started, err)
		return fmt.Errorf("qdrant %s %s: %w", method, path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		err := fmt.Errorf("qdrant %s %s returned HTTP %d: %s",
			method, path, resp.StatusCode, strings.TrimSpace(string(msg)))
		c.observe(op, started, err)
		return err
	}

	if out != nil {
		// Bound the response so a huge reply cannot exhaust memory.
		if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<20)).Decode(out); err != nil {
			c.observe(op, started, err)
			return fmt.Errorf("decode qdrant response: %w", err)
		}
	}
	c.observe(op, started, nil)
	return nil
}

func (c *Client) observe(op string, started time.Time, err error) {
	if c.metrics == nil {
		return
	}
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	c.metrics.StorageOps.WithLabelValues("qdrant", op, outcome).Inc()
	c.metrics.StorageDuration.WithLabelValues("qdrant", op).Observe(time.Since(started).Seconds())
}

// Payload field names. These are also the names indexed for filtering.
const (
	fWorkspace = "workspace_id"
	fItem      = "item_id"
	fChunk     = "chunk_id"
	fSource    = "source_type"
	fCreated   = "created_at"
	fModel     = "embedding_model"
)

type collectionInfo struct {
	Result struct {
		Config struct {
			Params struct {
				Vectors struct {
					Size     int    `json:"size"`
					Distance string `json:"distance"`
				} `json:"vectors"`
			} `json:"params"`
		} `json:"config"`
		PointsCount int64 `json:"points_count"`
	} `json:"result"`
	Status string `json:"status"`
}

// EnsureCollection creates the collection if absent and verifies its geometry.
//
// A dimension mismatch is fatal rather than silently tolerated: writing 768-wide
// vectors into a 1024-wide collection would either error per-write or, worse,
// produce a collection whose contents cannot all be compared.
func (c *Client) EnsureCollection(ctx context.Context, dim int) error {
	if dim <= 0 {
		return fmt.Errorf("vector dimension must be positive, got %d", dim)
	}

	var info collectionInfo
	err := c.do(ctx, http.MethodGet, "/collections/"+c.collection, nil, &info)
	if err == nil {
		existing := info.Result.Config.Params.Vectors.Size
		if existing != 0 && existing != dim {
			return fmt.Errorf(
				"qdrant collection %q stores %d-dimensional vectors but the embedding model produces %d; "+
					"either restore the previous embedding model or drop the collection and re-embed the workspace",
				c.collection, existing, dim)
		}
		return c.ensurePayloadIndexes(ctx)
	}

	// Create it. Cosine distance matches the normalised embeddings Relata uses.
	body := map[string]any{
		"vectors": map[string]any{
			"size":     dim,
			"distance": "Cosine",
		},
	}
	if err := c.do(ctx, http.MethodPut, "/collections/"+c.collection, body, nil); err != nil {
		return fmt.Errorf("create qdrant collection %q: %w", c.collection, err)
	}
	return c.ensurePayloadIndexes(ctx)
}

// ensurePayloadIndexes creates the payload indexes the workspace filter needs.
//
// Without an index on workspace_id every filtered search degrades to a full
// scan, so this is a correctness-adjacent performance requirement rather than
// an optimisation.
func (c *Client) ensurePayloadIndexes(ctx context.Context) error {
	fields := map[string]string{
		fWorkspace: "keyword",
		fItem:      "keyword",
		fChunk:     "keyword",
		fSource:    "keyword",
		fModel:     "keyword",
		fCreated:   "integer",
	}
	for field, schema := range fields {
		body := map[string]any{"field_name": field, "field_schema": schema}
		// Qdrant returns an error for an index that already exists; that is
		// expected on every start after the first.
		if err := c.do(ctx, http.MethodPut,
			"/collections/"+c.collection+"/index?wait=true", body, nil); err != nil {
			if !strings.Contains(err.Error(), "already exists") {
				observability.LoggerFrom(ctx).Debug("qdrant payload index not created",
					"field", field, "error", err.Error())
			}
		}
	}
	return nil
}

type upsertPoint struct {
	ID      string         `json:"id"`
	Vector  []float32      `json:"vector"`
	Payload map[string]any `json:"payload"`
}

// maxUpsertBatch bounds one write request.
const maxUpsertBatch = 128

// Upsert writes vectors, replacing any point with the same chunk ID.
//
// Chunk IDs are UUIDs and are used directly as point IDs, so re-embedding a
// chunk overwrites its vector rather than adding a second one. That is what
// keeps reprocessing free of orphaned vectors.
func (c *Client) Upsert(ctx context.Context, points []domain.VectorPoint) error {
	if len(points) == 0 {
		return nil
	}

	for start := 0; start < len(points); start += maxUpsertBatch {
		end := start + maxUpsertBatch
		if end > len(points) {
			end = len(points)
		}

		batch := make([]upsertPoint, 0, end-start)
		for _, p := range points[start:end] {
			if p.WorkspaceID == "" || p.ChunkID == "" || p.ItemID == "" {
				return fmt.Errorf("vector point is missing workspace, item or chunk identity")
			}
			if len(p.Vector) == 0 {
				return fmt.Errorf("vector point %s has an empty vector", p.ChunkID)
			}
			batch = append(batch, upsertPoint{
				ID:     p.ChunkID,
				Vector: p.Vector,
				Payload: map[string]any{
					fWorkspace: p.WorkspaceID,
					fItem:      p.ItemID,
					fChunk:     p.ChunkID,
					fSource:    string(p.SourceType),
					fCreated:   p.CreatedAt.Unix(),
					fModel:     p.EmbeddingModel,
				},
			})
		}

		body := map[string]any{"points": batch}
		if err := c.do(ctx, http.MethodPut,
			"/collections/"+c.collection+"/points?wait=true", body, nil); err != nil {
			return fmt.Errorf("upsert %d vectors: %w", len(batch), err)
		}
	}
	return nil
}

type searchRequest struct {
	Vector      []float32      `json:"vector"`
	Limit       int            `json:"limit"`
	WithPayload bool           `json:"with_payload"`
	Filter      map[string]any `json:"filter,omitempty"`
	ScoreThresh *float64       `json:"score_threshold,omitempty"`
}

type searchResponse struct {
	Result []struct {
		ID      string         `json:"id"`
		Score   float64        `json:"score"`
		Payload map[string]any `json:"payload"`
	} `json:"result"`
}

// buildFilter converts a typed filter into Qdrant's filter document.
//
// The workspace clause is mandatory and is always added first: a search without
// it would cross the workspace boundary, so the function refuses to build one.
func buildFilter(f domain.VectorFilter) (map[string]any, error) {
	if f.WorkspaceID == "" {
		return nil, fmt.Errorf("%w: vector search requires a workspace", domain.ErrWorkspaceMiss)
	}

	must := []any{
		map[string]any{"key": fWorkspace, "match": map[string]any{"value": f.WorkspaceID}},
	}
	var mustNot []any

	if len(f.ItemIDs) > 0 {
		must = append(must, map[string]any{
			"key": fItem, "match": map[string]any{"any": f.ItemIDs},
		})
	}
	if len(f.ExcludeItemIDs) > 0 {
		mustNot = append(mustNot, map[string]any{
			"key": fItem, "match": map[string]any{"any": f.ExcludeItemIDs},
		})
	}
	if len(f.SourceTypes) > 0 {
		vals := make([]string, 0, len(f.SourceTypes))
		for _, s := range f.SourceTypes {
			vals = append(vals, string(s))
		}
		must = append(must, map[string]any{
			"key": fSource, "match": map[string]any{"any": vals},
		})
	}
	if f.EmbeddingModel != "" {
		must = append(must, map[string]any{
			"key": fModel, "match": map[string]any{"value": f.EmbeddingModel},
		})
	}
	if f.CreatedFrom != nil || f.CreatedTo != nil {
		rng := map[string]any{}
		if f.CreatedFrom != nil {
			rng["gte"] = f.CreatedFrom.Unix()
		}
		if f.CreatedTo != nil {
			rng["lte"] = f.CreatedTo.Unix()
		}
		must = append(must, map[string]any{"key": fCreated, "range": rng})
	}

	out := map[string]any{"must": must}
	if len(mustNot) > 0 {
		out["must_not"] = mustNot
	}
	return out, nil
}

// Search finds the nearest vectors within a workspace.
func (c *Client) Search(ctx context.Context, vector []float32, limit int, f domain.VectorFilter) ([]domain.VectorMatch, error) {
	if len(vector) == 0 {
		return nil, fmt.Errorf("search vector is empty")
	}
	if limit <= 0 || limit > 1000 {
		limit = 50
	}

	filter, err := buildFilter(f)
	if err != nil {
		return nil, err
	}

	var resp searchResponse
	req := searchRequest{Vector: vector, Limit: limit, WithPayload: true, Filter: filter}
	if err := c.do(ctx, http.MethodPost,
		"/collections/"+c.collection+"/points/search", req, &resp); err != nil {
		return nil, err
	}

	out := make([]domain.VectorMatch, 0, len(resp.Result))
	for _, r := range resp.Result {
		ws := payloadString(r.Payload, fWorkspace)
		// Defence in depth: even though the filter scoped the query, a result
		// from another workspace is dropped rather than returned.
		if ws != f.WorkspaceID {
			continue
		}
		m := domain.VectorMatch{
			ChunkID:        payloadString(r.Payload, fChunk),
			ItemID:         payloadString(r.Payload, fItem),
			WorkspaceID:    ws,
			Score:          r.Score,
			SourceType:     domain.SourceType(payloadString(r.Payload, fSource)),
			EmbeddingModel: payloadString(r.Payload, fModel),
		}
		if m.ChunkID == "" {
			m.ChunkID = r.ID
		}
		if ts, ok := payloadInt(r.Payload, fCreated); ok {
			m.CreatedAt = time.Unix(ts, 0).UTC()
		}
		out = append(out, m)
	}
	return out, nil
}

// DeleteByItem removes every vector belonging to an item.
//
// Deletion is by filter rather than by enumerated IDs, so it removes vectors
// even for chunks MongoDB no longer knows about — which is exactly the orphan
// case that would otherwise accumulate.
func (c *Client) DeleteByItem(ctx context.Context, workspaceID, itemID string) error {
	if workspaceID == "" || itemID == "" {
		return domain.ErrWorkspaceMiss
	}
	body := map[string]any{
		"filter": map[string]any{
			"must": []any{
				map[string]any{"key": fWorkspace, "match": map[string]any{"value": workspaceID}},
				map[string]any{"key": fItem, "match": map[string]any{"value": itemID}},
			},
		},
	}
	return c.do(ctx, http.MethodPost,
		"/collections/"+c.collection+"/points/delete?wait=true", body, nil)
}

// DeleteByChunks removes specific vectors, scoped to a workspace.
func (c *Client) DeleteByChunks(ctx context.Context, workspaceID string, chunkIDs []string) error {
	if workspaceID == "" {
		return domain.ErrWorkspaceMiss
	}
	if len(chunkIDs) == 0 {
		return nil
	}
	body := map[string]any{
		"filter": map[string]any{
			"must": []any{
				map[string]any{"key": fWorkspace, "match": map[string]any{"value": workspaceID}},
				map[string]any{"key": fChunk, "match": map[string]any{"any": chunkIDs}},
			},
		},
	}
	return c.do(ctx, http.MethodPost,
		"/collections/"+c.collection+"/points/delete?wait=true", body, nil)
}

type countResponse struct {
	Result struct {
		Count int64 `json:"count"`
	} `json:"result"`
}

// CountByWorkspace reports how many vectors a workspace holds, used to
// reconcile the vector store against MongoDB.
func (c *Client) CountByWorkspace(ctx context.Context, workspaceID string) (int64, error) {
	if workspaceID == "" {
		return 0, domain.ErrWorkspaceMiss
	}
	body := map[string]any{
		"exact": true,
		"filter": map[string]any{
			"must": []any{
				map[string]any{"key": fWorkspace, "match": map[string]any{"value": workspaceID}},
			},
		},
	}
	var resp countResponse
	if err := c.do(ctx, http.MethodPost,
		"/collections/"+c.collection+"/points/count", body, &resp); err != nil {
		return 0, err
	}
	return resp.Result.Count, nil
}

// Health checks that Qdrant is reachable.
func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/healthz", nil, nil)
}

// DropCollection removes the collection. Used by tests and by the documented
// re-embedding procedure after a dimension change.
func (c *Client) DropCollection(ctx context.Context) error {
	return c.do(ctx, http.MethodDelete, "/collections/"+c.collection, nil, nil)
}

func payloadString(p map[string]any, key string) string {
	if v, ok := p[key].(string); ok {
		return v
	}
	return ""
}

func payloadInt(p map[string]any, key string) (int64, bool) {
	switch v := p[key].(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	}
	return 0, false
}
