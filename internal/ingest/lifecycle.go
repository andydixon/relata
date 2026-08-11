package ingest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/ids"
	"github.com/andydixon/relata/internal/observability"
	"github.com/andydixon/relata/internal/parsing"
	"github.com/andydixon/relata/internal/relationships"
)

// DeletionPlan describes what deleting an item would remove.
//
// It is produced by the dry-run API so a user can see the blast radius before
// committing — particularly the entities that exist only because of this item
// and would therefore disappear with it.
type DeletionPlan struct {
	ItemID           string   `json:"item_id"`
	Title            string   `json:"title"`
	Chunks           int      `json:"chunks"`
	Vectors          int64    `json:"vectors"`
	Claims           int      `json:"claims"`
	Relationships    int      `json:"relationships"`
	OrphanedEntities []string `json:"orphaned_entities"`
	SharedEntities   []string `json:"shared_entities_retained"`
	OriginalFile     string   `json:"original_file,omitempty"`
	Jobs             int      `json:"queued_jobs"`
}

// PlanDeletion reports what would be removed, without changing anything.
func (p *Pipeline) PlanDeletion(ctx context.Context, workspaceID, itemID string) (*DeletionPlan, error) {
	item, err := p.repos.Items.Get(ctx, workspaceID, itemID)
	if err != nil {
		return nil, err
	}

	plan := &DeletionPlan{ItemID: item.ID, Title: item.Title}

	chunks, err := p.repos.Chunks.ListByItem(ctx, workspaceID, itemID)
	if err != nil {
		return nil, err
	}
	plan.Chunks = len(chunks)
	plan.Vectors = int64(len(chunks))

	claims, err := p.repos.Claims.ListByItem(ctx, workspaceID, itemID)
	if err != nil {
		return nil, err
	}
	plan.Claims = len(claims)

	out, in, err := p.repos.Rels.ListForNode(ctx, workspaceID, domain.NodeItem, itemID, domain.RelationshipFilter{})
	if err != nil {
		return nil, err
	}
	plan.Relationships = len(out) + len(in)

	ents, err := p.repos.Entities.ListByItem(ctx, workspaceID, itemID)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		// An entity referenced only by this item has no evidence left once the
		// item is gone; one referenced elsewhere survives.
		if len(e.ItemIDs) <= 1 {
			plan.OrphanedEntities = append(plan.OrphanedEntities, e.CanonicalName)
		} else {
			plan.SharedEntities = append(plan.SharedEntities, e.CanonicalName)
		}
	}

	if item.OriginalStoredPath != "" {
		plan.OriginalFile = item.OriginalFilename
	}
	return plan, nil
}

// RequestDeletion queues an item for deletion and tombstones it immediately.
//
// Tombstoning first means the item stops appearing in search and on the
// dashboard straight away, even though the derived records are cleaned up
// asynchronously. The user sees the deletion take effect at once.
func (p *Pipeline) RequestDeletion(ctx context.Context, pr *domain.Principal, itemID string) (string, error) {
	if pr == nil || !pr.Role.CanWrite() {
		return "", domain.ErrForbidden
	}
	item, err := p.repos.Items.Get(ctx, pr.WorkspaceID, itemID)
	if err != nil {
		return "", err
	}

	if err := p.repos.Items.Tombstone(ctx, pr.WorkspaceID, item.ID); err != nil {
		return "", err
	}

	job := &domain.Job{
		ID:             ids.New(),
		WorkspaceID:    pr.WorkspaceID,
		Type:           domain.JobDeleteItemData,
		Status:         domain.JobPending,
		ItemID:         item.ID,
		Priority:       20, // deletion outranks analysis
		MaxAttempts:    p.cfg.Workers.MaxAttempts,
		Payload:        map[string]string{"actor": pr.UserID},
		IdempotencyKey: ids.Deterministic(pr.WorkspaceID, item.ID, string(domain.JobDeleteItemData)),
	}
	if err := p.repos.Jobs.Enqueue(ctx, job); err != nil {
		return "", err
	}

	p.audit.Record(ctx, pr, domain.AuditItemDeleted, "item", item.ID, "requested", map[string]string{
		"title": item.Title,
	})
	return job.ID, nil
}

// HandleDelete removes an item's derived records and vectors.
//
// The order matters: vectors go first, because an orphaned vector is the one
// artefact that nothing else would ever clean up. Everything after it is
// reachable from MongoDB and could be swept later if a step failed.
func (p *Pipeline) HandleDelete(ctx context.Context, j *domain.Job) error {
	log := observability.LoggerFrom(ctx).With("item_id", j.ItemID)

	// Deletion is by filter, so it removes vectors even for chunks that are
	// already gone from MongoDB.
	if err := p.vectors.DeleteByItem(ctx, j.WorkspaceID, j.ItemID); err != nil {
		return fmt.Errorf("delete vectors: %w", err)
	}

	if err := p.repos.Chunks.DeleteByItem(ctx, j.WorkspaceID, j.ItemID); err != nil {
		return fmt.Errorf("delete chunks: %w", err)
	}
	if err := p.repos.Claims.DeleteByItem(ctx, j.WorkspaceID, j.ItemID); err != nil {
		return fmt.Errorf("delete claims: %w", err)
	}

	// Strip this item from every entity, then remove entities left with no
	// evidence at all. An entity still referenced elsewhere is retained.
	orphans, err := p.repos.Entities.RemoveItemRefs(ctx, j.WorkspaceID, j.ItemID)
	if err != nil {
		return fmt.Errorf("update entity references: %w", err)
	}
	for _, id := range orphans {
		if err := p.repos.Aliases.DeleteByEntity(ctx, j.WorkspaceID, id); err != nil {
			log.Warn("could not delete aliases for an orphaned entity", "entity_id", id, "error", err.Error())
		}
	}
	if len(orphans) > 0 {
		if err := p.repos.Entities.DeleteMany(ctx, j.WorkspaceID, orphans); err != nil {
			return fmt.Errorf("delete orphaned entities: %w", err)
		}
	}

	relCount, err := p.repos.Rels.DeleteForItem(ctx, j.WorkspaceID, j.ItemID)
	if err != nil {
		return fmt.Errorf("delete relationships: %w", err)
	}

	if _, err := p.repos.Jobs.DeleteForItem(ctx, j.WorkspaceID, j.ItemID); err != nil {
		log.Warn("could not remove queued jobs for the deleted item", "error", err.Error())
	}

	// Remove the stored original, then the item record itself.
	item, err := p.repos.Items.Get(ctx, j.WorkspaceID, j.ItemID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if item != nil {
		if err := p.ingest.DeleteOriginal(item); err != nil {
			log.Warn("could not delete the stored original file", "error", err.Error())
		}
		if err := p.repos.Items.HardDelete(ctx, j.WorkspaceID, j.ItemID); err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
	}

	log.Info("deleted item and its derived records",
		"orphaned_entities", len(orphans), "relationships", relCount)
	return nil
}

// RequestReprocess queues an item to be processed again.
//
// Reprocessing replaces derived records rather than adding to them: chunks and
// claims are swapped wholesale, entity observations merge by exact key, and
// relationships upsert on a dedupe key that leaves human review decisions
// untouched. That is what keeps a reprocess from multiplying data.
func (p *Pipeline) RequestReprocess(ctx context.Context, pr *domain.Principal, itemID, reason string) (string, error) {
	if pr == nil || !pr.Role.CanWrite() {
		return "", domain.ErrForbidden
	}
	item, err := p.repos.Items.Get(ctx, pr.WorkspaceID, itemID)
	if err != nil {
		return "", err
	}
	if item.Deleted {
		return "", domain.ErrNotFound
	}

	runID := ids.New()
	run := &domain.ProcessingRun{
		ID:             runID,
		WorkspaceID:    item.WorkspaceID,
		ItemID:         item.ID,
		Reason:         reasonOr(reason, "manual reprocess"),
		StartedAt:      time.Now().UTC(),
		ParserVersion:  parsing.Version,
		EmbeddingModel: p.embedder.ModelID(),
		AnalysisModel:  p.analysis.ModelID(),
		ScoreVersion:   relationships.ScoreVersion,
	}
	if err := p.repos.Runs.Create(ctx, run); err != nil {
		observability.LoggerFrom(ctx).Warn("could not open a processing run", "error", err.Error())
	}

	job := &domain.Job{
		ID:          ids.New(),
		WorkspaceID: pr.WorkspaceID,
		Type:        domain.JobParseItem,
		Status:      domain.JobPending,
		ItemID:      item.ID,
		RunID:       runID,
		Priority:    12,
		MaxAttempts: p.cfg.Workers.MaxAttempts,
		Payload:     map[string]string{"reason": run.Reason},
		// The run ID is part of the key, so a reprocess is never suppressed as
		// a duplicate of the original ingest.
		IdempotencyKey: ids.Deterministic(pr.WorkspaceID, item.ID, string(domain.JobParseItem), runID),
	}
	if err := p.repos.Jobs.Enqueue(ctx, job); err != nil {
		return "", err
	}

	p.audit.Record(ctx, pr, domain.AuditItemReprocessed, "item", item.ID, "requested", map[string]string{
		"reason":          run.Reason,
		"parser_version":  parsing.Version,
		"embedding_model": p.embedder.ModelID(),
		"score_version":   relationships.ScoreVersion,
	})
	return job.ID, nil
}

// HandleReprocess is the entry point for a queued reprocess request; it simply
// restarts the chain at parsing.
func (p *Pipeline) HandleReprocess(ctx context.Context, j *domain.Job) error {
	return p.HandleParse(ctx, j)
}

// HandleReembedWorkspace re-embeds chunks whose vectors were produced by a
// different model.
//
// The job re-enqueues itself while work remains, so a large workspace is
// processed in bounded batches instead of one enormous job that would hold a
// lease for hours.
func (p *Pipeline) HandleReembedWorkspace(ctx context.Context, j *domain.Job) error {
	log := observability.LoggerFrom(ctx)
	model := p.embedder.ModelID()

	if err := p.vectors.EnsureCollection(ctx, p.embedder.Dimensions()); err != nil {
		return err
	}

	const batchSize = 64
	chunks, err := p.repos.Chunks.ListUnembedded(ctx, j.WorkspaceID, model, batchSize)
	if err != nil {
		return err
	}
	if len(chunks) == 0 {
		log.Info("workspace re-embedding complete", "model", model)
		return nil
	}

	// Item metadata is needed for the vector payload.
	itemIDs := make([]string, 0, len(chunks))
	seen := map[string]bool{}
	for _, c := range chunks {
		if !seen[c.ItemID] {
			seen[c.ItemID] = true
			itemIDs = append(itemIDs, c.ItemID)
		}
	}
	items, err := p.repos.Items.GetMany(ctx, j.WorkspaceID, itemIDs)
	if err != nil {
		return err
	}
	byID := make(map[string]*domain.Item, len(items))
	for _, it := range items {
		byID[it.ID] = it
	}

	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Text
	}

	release, err := p.pool.AcquireAI(ctx)
	if err != nil {
		return err
	}
	vecs, err := p.embedder.Embed(ctx, texts)
	release()
	if err != nil {
		return err
	}
	if len(vecs) != len(chunks) {
		return fmt.Errorf("embedding provider returned %d vectors for %d chunks", len(vecs), len(chunks))
	}

	points := make([]domain.VectorPoint, 0, len(chunks))
	ok := make([]string, 0, len(chunks))
	for i, c := range chunks {
		it := byID[c.ItemID]
		if it == nil {
			// The chunk's item is gone; drop the stale chunk rather than
			// creating a vector that points at nothing.
			continue
		}
		points = append(points, domain.VectorPoint{
			ChunkID:        c.ID,
			WorkspaceID:    c.WorkspaceID,
			ItemID:         c.ItemID,
			SourceType:     it.SourceType,
			CreatedAt:      it.CreatedAt,
			EmbeddingModel: model,
			Vector:         vecs[i],
		})
		ok = append(ok, c.ID)
	}

	if err := p.vectors.Upsert(ctx, points); err != nil {
		return err
	}
	if err := p.repos.Chunks.MarkEmbedded(ctx, j.WorkspaceID, ok, model); err != nil {
		return err
	}
	_ = p.repos.Models.Observe(ctx, j.WorkspaceID, "embedding", model, p.embedder.Dimensions())

	log.Info("re-embedded a batch", "count", len(ok), "model", model)

	// Queue the next batch. A fresh idempotency key is required or the unique
	// pending-job index would suppress it.
	next := &domain.Job{
		ID:             ids.New(),
		WorkspaceID:    j.WorkspaceID,
		Type:           domain.JobReembedWS,
		Status:         domain.JobPending,
		Priority:       3,
		MaxAttempts:    p.cfg.Workers.MaxAttempts,
		IdempotencyKey: ids.Deterministic(j.WorkspaceID, string(domain.JobReembedWS), ids.New()),
	}
	return p.repos.Jobs.Enqueue(ctx, next)
}

// RequestReembed queues a workspace-wide re-embedding.
func (p *Pipeline) RequestReembed(ctx context.Context, pr *domain.Principal) (string, error) {
	if pr == nil || pr.Role != domain.RoleAdmin {
		return "", domain.ErrForbidden
	}
	job := &domain.Job{
		ID:             ids.New(),
		WorkspaceID:    pr.WorkspaceID,
		Type:           domain.JobReembedWS,
		Status:         domain.JobPending,
		Priority:       3,
		MaxAttempts:    p.cfg.Workers.MaxAttempts,
		IdempotencyKey: ids.Deterministic(pr.WorkspaceID, string(domain.JobReembedWS), "manual"),
	}
	if err := p.repos.Jobs.Enqueue(ctx, job); err != nil {
		return "", err
	}
	p.audit.Record(ctx, pr, domain.AuditWorkspaceReembed, "workspace", pr.WorkspaceID, "requested",
		map[string]string{"embedding_model": p.embedder.ModelID()})
	return job.ID, nil
}
