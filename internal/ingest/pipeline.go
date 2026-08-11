package ingest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/andydixon/relata/internal/analysis"
	"github.com/andydixon/relata/internal/audit"
	"github.com/andydixon/relata/internal/chunking"
	"github.com/andydixon/relata/internal/config"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/entities"
	"github.com/andydixon/relata/internal/ids"
	"github.com/andydixon/relata/internal/jobs"
	"github.com/andydixon/relata/internal/observability"
	"github.com/andydixon/relata/internal/parsing"
	"github.com/andydixon/relata/internal/relationships"
)

// Repos bundles the repositories the pipeline writes to.
type Repos struct {
	Items    domain.ItemRepository
	Chunks   domain.ChunkRepository
	Entities domain.EntityRepository
	Aliases  domain.AliasRepository
	Claims   domain.ClaimRepository
	Rels     domain.RelationshipRepository
	Jobs     domain.JobRepository
	Runs     domain.RunRepository
	Models   domain.ModelVersionRepository
}

// Pipeline executes the processing stages as background jobs.
//
// Each stage is a separate job type so that progress is visible in the queue
// and a failure is attributable to a specific stage. Stages hand off by
// enqueuing the next one, and every stage is idempotent: re-running it replaces
// its own output rather than adding to it.
type Pipeline struct {
	repos    Repos
	vectors  domain.VectorStore
	embedder domain.Embedder
	analysis *analysis.Service
	entities *entities.Service
	discover *relationships.Discoverer
	ingest   *Service
	cfg      *config.Config
	pool     *jobs.Pool
	audit    *audit.Recorder
}

// NewPipeline builds the pipeline.
func NewPipeline(repos Repos, vectors domain.VectorStore, embedder domain.Embedder,
	an *analysis.Service, ent *entities.Service, disc *relationships.Discoverer,
	ing *Service, cfg *config.Config, pool *jobs.Pool, rec *audit.Recorder) *Pipeline {
	return &Pipeline{
		repos: repos, vectors: vectors, embedder: embedder, analysis: an,
		entities: ent, discover: disc, ingest: ing, cfg: cfg, pool: pool, audit: rec,
	}
}

// Register wires the handlers into a worker pool.
func (p *Pipeline) Register(pool *jobs.Pool) {
	pool.Register(domain.JobParseItem, p.HandleParse)
	pool.Register(domain.JobChunkItem, p.HandleChunk)
	pool.Register(domain.JobEmbedItem, p.HandleEmbed)
	pool.Register(domain.JobExtractEntity, p.HandleExtractEntities)
	pool.Register(domain.JobExtractClaims, p.HandleExtractClaims)
	pool.Register(domain.JobDiscoverRels, p.HandleDiscoverRelationships)
	pool.Register(domain.JobReprocessItem, p.HandleReprocess)
	pool.Register(domain.JobDeleteItemData, p.HandleDelete)
	pool.Register(domain.JobReembedWS, p.HandleReembedWorkspace)
}

// enqueueNext queues the following stage.
//
// The idempotency key includes the run ID, so a reprocess enqueues a fresh
// chain while a duplicate enqueue within the same run is a no-op.
func (p *Pipeline) enqueueNext(ctx context.Context, j *domain.Job, next domain.JobType, priority int) error {
	nextJob := &domain.Job{
		ID:             ids.New(),
		WorkspaceID:    j.WorkspaceID,
		Type:           next,
		Status:         domain.JobPending,
		ItemID:         j.ItemID,
		Payload:        j.Payload,
		RunID:          j.RunID,
		Priority:       priority,
		MaxAttempts:    p.cfg.Workers.MaxAttempts,
		IdempotencyKey: ids.Deterministic(j.WorkspaceID, j.ItemID, string(next), j.RunID),
	}
	return p.repos.Jobs.Enqueue(ctx, nextJob)
}

// loadItem fetches the job's item, treating a missing item as a permanent
// failure — it was deleted while queued, and retrying will not bring it back.
func (p *Pipeline) loadItem(ctx context.Context, j *domain.Job) (*domain.Item, error) {
	if j.ItemID == "" {
		return nil, fmt.Errorf("%w: job has no item", domain.ErrInvalidInput)
	}
	item, err := p.repos.Items.Get(ctx, j.WorkspaceID, j.ItemID)
	if err != nil {
		return nil, err
	}
	if item.Deleted {
		return nil, fmt.Errorf("%w: item has been deleted", domain.ErrNotFound)
	}
	return item, nil
}

// ---------------------------------------------------------------------------
// Stage 1: parse
// ---------------------------------------------------------------------------

// HandleParse extracts text from the item's original content.
//
// A parse failure does not fail the job in the sense of losing the item: the
// original is retained, the failure is recorded as a warning, and the item is
// marked partial so a user can see what happened and reprocess later.
func (p *Pipeline) HandleParse(ctx context.Context, j *domain.Job) error {
	item, err := p.loadItem(ctx, j)
	if err != nil {
		return err
	}
	log := observability.LoggerFrom(ctx).With("item_id", item.ID)

	if err := p.repos.Items.SetStatus(ctx, item.WorkspaceID, item.ID, domain.StatusParsing, ""); err != nil {
		return err
	}

	// Open a processing run so that what each generation of parser and model
	// produced stays on the record.
	runID := j.RunID
	if runID == "" {
		runID = ids.New()
		j.RunID = runID
		run := &domain.ProcessingRun{
			ID:             runID,
			WorkspaceID:    item.WorkspaceID,
			ItemID:         item.ID,
			Reason:         reasonOr(j.Payload["reason"], "ingest"),
			StartedAt:      time.Now().UTC(),
			ParserVersion:  parsing.Version,
			EmbeddingModel: p.embedder.ModelID(),
			AnalysisModel:  p.analysis.ModelID(),
			ScoreVersion:   relationships.ScoreVersion,
		}
		if err := p.repos.Runs.Create(ctx, run); err != nil {
			log.Warn("could not open a processing run", "error", err.Error())
		}
	}

	content, err := p.ingest.ReadOriginal(item)
	if err != nil {
		return fmt.Errorf("read original content: %w", err)
	}

	doc, perr := parsing.Parse(parsing.Input{
		Data:         content,
		MIMEType:     item.MIMEType,
		Filename:     item.OriginalFilename,
		MaxTextBytes: p.cfg.Uploads.MaxExtractedTextBytes,
		MaxJSONDepth: p.cfg.Uploads.MaxJSONDepth,
	})

	if perr != nil {
		// Record the failure and stop the chain, keeping the original intact.
		for _, w := range doc.Warnings {
			_ = p.repos.Items.AddWarning(ctx, item.WorkspaceID, item.ID, w)
		}
		item.ParserVersion = parsing.Version
		item.Status = domain.StatusFailed
		item.Error = "Text could not be extracted from this item. The original content is preserved and can be reprocessed."
		if err := p.repos.Items.Update(ctx, item); err != nil {
			return err
		}
		p.finishRun(ctx, runID, "failed", perr.Error(), nil)
		log.Warn("parsing failed; the original is preserved", "error", perr.Error())
		// Returning nil: the job did its work correctly. The item's failure is
		// recorded as item state, not as an endlessly retried job.
		return nil
	}

	item.Warnings = append(item.Warnings, doc.Warnings...)
	item.ExtractedText = doc.Text
	item.ParserVersion = doc.Version
	item.Status = domain.StatusChunking
	item.Error = ""
	if err := p.repos.Items.Update(ctx, item); err != nil {
		return err
	}

	_ = p.repos.Models.Observe(ctx, item.WorkspaceID, "parser", parsing.Version, 0)
	log.Info("parsed item", "text_bytes", len(doc.Text), "blocks", len(doc.Blocks),
		"warnings", len(doc.Warnings))

	// The parsed document is not carried in the job payload — payloads never
	// hold content — so the next stage re-parses from the preserved original.
	return p.enqueueNext(ctx, j, domain.JobChunkItem, 9)
}

// ---------------------------------------------------------------------------
// Stage 2: chunk
// ---------------------------------------------------------------------------

// HandleChunk divides the extracted text into retrievable chunks.
func (p *Pipeline) HandleChunk(ctx context.Context, j *domain.Job) error {
	item, err := p.loadItem(ctx, j)
	if err != nil {
		return err
	}
	log := observability.LoggerFrom(ctx).With("item_id", item.ID)

	if err := p.repos.Items.SetStatus(ctx, item.WorkspaceID, item.ID, domain.StatusChunking, ""); err != nil {
		return err
	}

	doc, err := p.reparse(item)
	if err != nil {
		return err
	}

	res := chunking.Split(doc, chunking.Options{
		MaxChars:      p.cfg.Chunking.MaxChars,
		TargetChars:   p.cfg.Chunking.TargetChars,
		MinChars:      p.cfg.Chunking.MinChars,
		OverlapChars:  p.cfg.Chunking.OverlapChars,
		MaxChunks:     p.cfg.Chunking.MaxPerItem,
		MinSplitChars: p.cfg.Chunking.MinSplitChars,
	})

	if res.Truncated {
		_ = p.repos.Items.AddWarning(ctx, item.WorkspaceID, item.ID, domain.ParseWarning{
			Code:    domain.WarnTruncatedChunks,
			Message: "This item produced more sections than the configured limit, so its later content is not searchable.",
			Detail:  fmt.Sprintf("limit=%d", p.cfg.Chunking.MaxPerItem),
		})
	}

	now := time.Now().UTC()
	chunks := make([]*domain.Chunk, 0, len(res.Chunks))
	for _, c := range res.Chunks {
		chunks = append(chunks, &domain.Chunk{
			// A deterministic ID keyed on item and sequence means re-chunking
			// reuses the same Qdrant point IDs, so vectors are overwritten
			// rather than orphaned.
			ID:          ids.DeterministicUUID(item.WorkspaceID, item.ID, fmt.Sprint(c.Sequence), c.Hash),
			WorkspaceID: item.WorkspaceID,
			ItemID:      item.ID,
			Sequence:    c.Sequence,
			Text:        c.Text,
			ContentHash: c.Hash,
			StartOffset: c.Start,
			EndOffset:   c.End,
			Page:        c.Page,
			Section:     c.Section,
			Heading:     c.Heading,
			CreatedAt:   now,
		})
	}

	// Chunk IDs change when content changes, so vectors for the previous
	// generation must go before the new chunks are written.
	if err := p.vectors.DeleteByItem(ctx, item.WorkspaceID, item.ID); err != nil {
		log.Warn("could not clear previous vectors before re-chunking", "error", err.Error())
	}
	if err := p.repos.Chunks.ReplaceForItem(ctx, item.WorkspaceID, item.ID, chunks); err != nil {
		return err
	}

	item.ChunkCount = len(chunks)
	item.Status = domain.StatusEmbedding
	if err := p.repos.Items.Update(ctx, item); err != nil {
		return err
	}

	log.Info("chunked item", "chunks", len(chunks), "truncated", res.Truncated)
	if len(chunks) == 0 {
		// Nothing to embed or analyse. The item is complete, if empty.
		return p.markReady(ctx, item, j)
	}
	return p.enqueueNext(ctx, j, domain.JobEmbedItem, 8)
}

// reparse re-derives the parsed document from the preserved original.
func (p *Pipeline) reparse(item *domain.Item) (*parsing.Document, error) {
	content, err := p.ingest.ReadOriginal(item)
	if err != nil {
		return nil, fmt.Errorf("read original content: %w", err)
	}
	doc, err := parsing.Parse(parsing.Input{
		Data:         content,
		MIMEType:     item.MIMEType,
		Filename:     item.OriginalFilename,
		MaxTextBytes: p.cfg.Uploads.MaxExtractedTextBytes,
		MaxJSONDepth: p.cfg.Uploads.MaxJSONDepth,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrUnsupported, err)
	}
	return doc, nil
}

// ---------------------------------------------------------------------------
// Stage 3: embed
// ---------------------------------------------------------------------------

// HandleEmbed generates and stores vectors for the item's chunks.
func (p *Pipeline) HandleEmbed(ctx context.Context, j *domain.Job) error {
	item, err := p.loadItem(ctx, j)
	if err != nil {
		return err
	}
	log := observability.LoggerFrom(ctx).With("item_id", item.ID)

	if err := p.repos.Items.SetStatus(ctx, item.WorkspaceID, item.ID, domain.StatusEmbedding, ""); err != nil {
		return err
	}

	chunks, err := p.repos.Chunks.ListByItem(ctx, item.WorkspaceID, item.ID)
	if err != nil {
		return err
	}
	if len(chunks) == 0 {
		return p.markReady(ctx, item, j)
	}

	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Text
	}

	// Hold an AI slot for the duration of the provider call so that concurrent
	// model usage stays within the configured limit.
	release, err := p.pool.AcquireAI(ctx)
	if err != nil {
		return err
	}
	vecs, err := p.embedder.Embed(ctx, texts)
	release()
	if err != nil {
		return fmt.Errorf("generate embeddings: %w", err)
	}
	if len(vecs) != len(chunks) {
		return fmt.Errorf("embedding provider returned %d vectors for %d chunks", len(vecs), len(chunks))
	}

	model := p.embedder.ModelID()
	points := make([]domain.VectorPoint, 0, len(chunks))
	for i, c := range chunks {
		points = append(points, domain.VectorPoint{
			ChunkID:        c.ID,
			WorkspaceID:    c.WorkspaceID,
			ItemID:         c.ItemID,
			SourceType:     item.SourceType,
			CreatedAt:      item.CreatedAt,
			EmbeddingModel: model,
			Vector:         vecs[i],
		})
	}
	if err := p.vectors.Upsert(ctx, points); err != nil {
		return fmt.Errorf("store vectors: %w", err)
	}

	chunkIDs := make([]string, len(chunks))
	for i, c := range chunks {
		chunkIDs[i] = c.ID
	}
	if err := p.repos.Chunks.MarkEmbedded(ctx, item.WorkspaceID, chunkIDs, model); err != nil {
		return err
	}

	item.EmbeddingModel = model
	item.Status = domain.StatusAnalysing
	if err := p.repos.Items.Update(ctx, item); err != nil {
		return err
	}
	_ = p.repos.Models.Observe(ctx, item.WorkspaceID, "embedding", model, p.embedder.Dimensions())

	log.Info("embedded item", "chunks", len(chunks), "model", model)
	return p.enqueueNext(ctx, j, domain.JobExtractEntity, 7)
}

// ---------------------------------------------------------------------------
// Stage 4: entities
// ---------------------------------------------------------------------------

// HandleExtractEntities runs deterministic and model-assisted entity extraction.
func (p *Pipeline) HandleExtractEntities(ctx context.Context, j *domain.Job) error {
	item, err := p.loadItem(ctx, j)
	if err != nil {
		return err
	}
	log := observability.LoggerFrom(ctx).With("item_id", item.ID)

	if err := p.repos.Items.SetStatus(ctx, item.WorkspaceID, item.ID, domain.StatusAnalysing, ""); err != nil {
		return err
	}

	chunks, err := p.repos.Chunks.ListByItem(ctx, item.WorkspaceID, item.ID)
	if err != nil {
		return err
	}

	n, err := p.entities.ExtractForItem(ctx, item, chunks, p.pool)
	if err != nil {
		// Model-assisted extraction is best-effort: the deterministic results
		// are already stored, so a model failure degrades quality rather than
		// stopping the pipeline.
		if errors.Is(err, analysis.ErrDisabled) || errors.Is(err, analysis.ErrInvalidOutput) {
			log.Warn("model-assisted entity extraction unavailable; deterministic results kept",
				"error", err.Error())
		} else {
			return err
		}
	}

	item.EntityCount = n
	item.AnalysisModel = p.analysis.ModelID()
	if err := p.repos.Items.Update(ctx, item); err != nil {
		return err
	}
	if m := p.analysis.ModelID(); m != "" {
		_ = p.repos.Models.Observe(ctx, item.WorkspaceID, "analysis", m, 0)
	}

	log.Info("extracted entities", "count", n)
	return p.enqueueNext(ctx, j, domain.JobExtractClaims, 6)
}

// ---------------------------------------------------------------------------
// Stage 5: claims
// ---------------------------------------------------------------------------

// HandleExtractClaims extracts the statements the item makes.
func (p *Pipeline) HandleExtractClaims(ctx context.Context, j *domain.Job) error {
	item, err := p.loadItem(ctx, j)
	if err != nil {
		return err
	}
	log := observability.LoggerFrom(ctx).With("item_id", item.ID)

	chunks, err := p.repos.Chunks.ListByItem(ctx, item.WorkspaceID, item.ID)
	if err != nil {
		return err
	}

	n, err := p.entities.ExtractClaimsForItem(ctx, item, chunks, p.pool)
	if err != nil {
		if errors.Is(err, analysis.ErrDisabled) || errors.Is(err, analysis.ErrInvalidOutput) {
			log.Warn("claim extraction unavailable", "error", err.Error())
		} else {
			return err
		}
	}

	item.ClaimCount = n
	item.Status = domain.StatusRelating
	if err := p.repos.Items.Update(ctx, item); err != nil {
		return err
	}

	log.Info("extracted claims", "count", n)
	return p.enqueueNext(ctx, j, domain.JobDiscoverRels, 5)
}

// ---------------------------------------------------------------------------
// Stage 6: relationships
// ---------------------------------------------------------------------------

// HandleDiscoverRelationships finds and scores candidate relationships.
func (p *Pipeline) HandleDiscoverRelationships(ctx context.Context, j *domain.Job) error {
	item, err := p.loadItem(ctx, j)
	if err != nil {
		return err
	}
	log := observability.LoggerFrom(ctx).With("item_id", item.ID)

	if err := p.repos.Items.SetStatus(ctx, item.WorkspaceID, item.ID, domain.StatusRelating, ""); err != nil {
		return err
	}

	stats, err := p.discover.DiscoverForItem(ctx, item, p.pool)
	if err != nil {
		return err
	}

	item.RelationshipSeen = stats.Stored
	item.ScoreVersion = relationships.ScoreVersion
	if err := p.repos.Items.Update(ctx, item); err != nil {
		return err
	}

	log.Info("discovered relationships",
		"candidates", stats.Candidates, "stored", stats.Stored, "model_calls", stats.ModelCalls)

	p.finishRun(ctx, j.RunID, "succeeded", "", map[string]int{
		"chunks":        item.ChunkCount,
		"entities":      item.EntityCount,
		"claims":        item.ClaimCount,
		"relationships": stats.Stored,
		"candidates":    stats.Candidates,
	})
	return p.markReady(ctx, item, j)
}

func (p *Pipeline) markReady(ctx context.Context, item *domain.Item, j *domain.Job) error {
	status := domain.StatusReady
	// An item that produced no searchable text is "partial": it exists and is
	// preserved, but it will not be found by content.
	if item.ChunkCount == 0 || item.HasWarning(domain.WarnLowExtractableText) {
		status = domain.StatusPartial
	}
	if err := p.repos.Items.SetStatus(ctx, item.WorkspaceID, item.ID, status, ""); err != nil {
		return err
	}
	if j != nil && j.RunID != "" {
		p.finishRun(ctx, j.RunID, "succeeded", "", nil)
	}
	return nil
}

func (p *Pipeline) finishRun(ctx context.Context, runID, outcome, errMsg string, stats map[string]int) {
	if runID == "" {
		return
	}
	if err := p.repos.Runs.Finish(ctx, runID, outcome, errMsg, stats); err != nil {
		observability.LoggerFrom(ctx).Debug("could not close processing run", "error", err.Error())
	}
}

func reasonOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
