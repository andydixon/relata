//go:build integration

package tests

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/ingest"
	"github.com/andydixon/relata/internal/relationships"
)

// --- Complete ingestion ----------------------------------------------------

func TestCompleteIngestionPipeline(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	body := `Project Atlas delivery schedule

Prepared by Dr Alice Chen for Example Ltd on 2025-02-10.

The initial delivery date is 3 March 2025. Example Ltd will ship 240 units
to the Northfield depot. The total project cost is £42,000 excluding VAT.

Operational questions should go to ops@example.com.`

	item := h.ingestAndWait(ctx, "Atlas schedule", body,
		withMetadata(map[string]string{"project": "atlas"}))

	if item.Status != domain.StatusReady {
		t.Fatalf("status = %s, want ready (error: %s)", item.Status, item.Error)
	}

	// The original must survive verbatim.
	if item.OriginalText != body {
		t.Fatal("the original text was altered during ingestion")
	}
	if item.ContentHash == "" || len(item.ContentHash) != 64 {
		t.Fatalf("content hash is not a SHA-256 digest: %q", item.ContentHash)
	}
	if item.ParserVersion == "" {
		t.Fatal("the parser version was not recorded")
	}
	if item.Metadata["project"] != "atlas" {
		t.Fatal("metadata was not retained")
	}

	// Chunks, with offsets that address their own text.
	chunks, err := h.store.Chunks.ListByItem(ctx, h.principal.WorkspaceID, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) == 0 {
		t.Fatal("no chunks were produced")
	}
	for _, c := range chunks {
		if c.EndOffset > len(item.ExtractedText) || item.ExtractedText[c.StartOffset:c.EndOffset] != c.Text {
			t.Fatalf("chunk %d offsets do not address its own text", c.Sequence)
		}
		if !c.Embedded || c.EmbeddingModel != h.embedder.ModelID() {
			t.Fatalf("chunk %d was not embedded", c.Sequence)
		}
	}

	// Vectors persisted, one per chunk.
	if got := h.vectorCount(ctx, h.principal.WorkspaceID); got != int64(len(chunks)) {
		t.Fatalf("%d vectors stored for %d chunks", got, len(chunks))
	}

	// Deterministic entities.
	ents, err := h.store.Entities.ListByItem(ctx, h.principal.WorkspaceID, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, e := range ents {
		found[e.CanonicalName] = true
		if e.Method != domain.MethodDeterministic {
			t.Errorf("entity %q has method %q; analysis is disabled so all should be deterministic",
				e.CanonicalName, e.Method)
		}
	}
	for _, want := range []string{"Example Ltd", "Alice Chen", "ops@example.com"} {
		if !found[want] {
			t.Errorf("entity %q was not extracted (got %v)", want, keys(found))
		}
	}

	// Deterministic claims, each keeping its exact source text.
	claims, err := h.store.Claims.ListByItem(ctx, h.principal.WorkspaceID, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) == 0 {
		t.Fatal("no claims were extracted from text containing figures and dates")
	}
	for _, c := range claims {
		if c.SourceRef.Quote == "" {
			t.Fatalf("claim %q lost its source quotation", c.Statement)
		}
		if !strings.Contains(item.ExtractedText, c.SourceRef.Quote) {
			t.Fatalf("claim quotation %q does not appear in the source text", c.SourceRef.Quote)
		}
	}

	// A processing run was recorded.
	runs, err := h.store.Runs.ListByItem(ctx, h.principal.WorkspaceID, item.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) == 0 {
		t.Fatal("no processing run was recorded")
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- Duplicate detection ---------------------------------------------------

func TestExactDuplicateIsRefusedUnlessExplicitlyAllowed(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	body := "The revised delivery date is 14 March 2025 for Example Ltd."
	first := h.ingestAndWait(ctx, "Original", body)

	// The same bytes again must be refused, naming the existing item.
	_, err := h.ingest.Ingest(ctx, h.principal, ingest.Request{
		Title: "A copy", Content: []byte(body), SourceType: domain.SourceTypeNote,
	})
	var dup *ingest.DuplicateError
	if !errors.As(err, &dup) {
		t.Fatalf("identical content was accepted: %v", err)
	}
	if dup.ExistingID != first.ID {
		t.Fatalf("duplicate pointed at %s, want %s", dup.ExistingID, first.ID)
	}

	// With the explicit option it is stored, and the link is recorded.
	res, err := h.ingest.Ingest(ctx, h.principal, ingest.Request{
		Title: "A copy", Content: []byte(body), SourceType: domain.SourceTypeNote,
		AllowDuplicate: true,
	})
	if err != nil {
		t.Fatalf("allow_duplicate was not honoured: %v", err)
	}
	if res.Item.DuplicateOf != first.ID {
		t.Fatalf("duplicate_of = %q, want %q", res.Item.DuplicateOf, first.ID)
	}
}

// --- Search ----------------------------------------------------------------

func TestSearchFindsAndExplains(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	h.ingestAndWait(ctx, "Delivery schedule",
		"The revised delivery date is 14 March 2025. Example Ltd will ship 240 units to the Northfield depot.")
	h.ingestAndWait(ctx, "Plant care rota",
		"Watering is on Mondays and Thursdays. The ficus needs less water in winter than the ferns.")
	h.waitForQueue(ctx, 60*time.Second)

	resp, err := h.search.Search(ctx, h.principal, search_Query("revised delivery date"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) == 0 {
		t.Fatal("search returned nothing for content that is definitely stored")
	}

	top := resp.Results[0]
	if !strings.Contains(top.Item.Title, "Delivery") {
		t.Fatalf("the delivery document did not rank first; got %q", top.Item.Title)
	}
	if top.QueryRelevance <= 0 {
		t.Fatal("the top result has no query relevance")
	}
	if len(top.WhyMatched) == 0 {
		t.Fatal("the result carries no explanation of why it matched")
	}
	if top.Excerpt == "" {
		t.Fatal("the result carries no excerpt")
	}
	if top.RankVersion == "" {
		t.Fatal("the ranking version was not recorded")
	}

	// The unrelated document must not outrank the relevant one.
	for _, r := range resp.Results {
		if strings.Contains(r.Item.Title, "Plant") && r.QueryRelevance >= top.QueryRelevance {
			t.Fatal("an unrelated document scored as highly as the relevant one")
		}
	}
}

func TestSearchFiltersApply(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	old := time.Now().AddDate(0, 0, -400)
	h.ingestAndWait(ctx, "Old delivery note",
		"Delivery of units to the depot was completed.", withCreatedAt(old))
	h.ingestAndWait(ctx, "Recent delivery note",
		"Delivery of units to the depot is scheduled.")
	h.waitForQueue(ctx, 60*time.Second)

	from := time.Now().AddDate(0, 0, -30)
	q := search_Query("delivery units depot")
	q.CreatedFrom = &from

	resp, err := h.search.Search(ctx, h.principal, q)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range resp.Results {
		if r.Item.CreatedAt.Before(from) {
			t.Fatalf("the date filter was not applied: %q is dated %s", r.Item.Title, r.Item.CreatedAt)
		}
	}
}

// --- Relationships ---------------------------------------------------------

func TestRelationshipsAreDiscoveredWithInspectableEvidence(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	h.ingestAndWait(ctx, "Atlas original",
		"Prepared for Example Ltd. The initial delivery date is 3 March 2025. "+
			"Example Ltd will ship 240 units to the Northfield depot.",
		withMetadata(map[string]string{"project": "atlas"}))

	h.ingestAndWait(ctx, "Atlas revision",
		"Prepared for Example Ltd. The revised delivery date is 14 March 2025. "+
			"Example Ltd will ship 240 units to the Northfield depot as before.",
		withMetadata(map[string]string{"project": "atlas"}))

	h.waitForQueue(ctx, 90*time.Second)

	rels, _, err := h.store.Relationships.List(ctx, h.principal.WorkspaceID,
		domain.RelationshipFilter{}, domain.Page{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) == 0 {
		t.Fatal("no relationship was discovered between two clearly related documents")
	}

	rel := rels[0]
	if len(rel.Evidence.Signals) == 0 {
		t.Fatal("the relationship has no evidence signals")
	}
	if rel.ScoreVersion != relationships.ScoreVersion {
		t.Fatalf("score version = %q, want %q", rel.ScoreVersion, relationships.ScoreVersion)
	}
	if rel.ReviewStatus != domain.ReviewPending {
		t.Fatalf("a discovered relationship was stored as %q; nothing may be auto-confirmed", rel.ReviewStatus)
	}
	if rel.Score > relationships.MaxUnreviewedScore {
		t.Fatalf("an unreviewed relationship scored %.2f, above the ceiling of %.2f",
			rel.Score, relationships.MaxUnreviewedScore)
	}

	// The score must equal the arithmetic of its own signals.
	recomputed := relationships.Score(rel.Evidence.Signals)
	if recomputed > relationships.MaxUnreviewedScore {
		recomputed = relationships.MaxUnreviewedScore
	}
	if diff := rel.Score - recomputed; diff > 0.001 || diff < -0.001 {
		t.Fatalf("stored score %.4f does not match its signals (%.4f)", rel.Score, recomputed)
	}

	// A shared entity must be named, not merely counted.
	var hasEntity bool
	for _, s := range rel.Evidence.Signals {
		if s.Type == domain.SigSharedEntity && s.Value != "" {
			hasEntity = true
		}
	}
	if !hasEntity {
		t.Fatal("two documents sharing Example Ltd produced no shared-entity signal")
	}

	if lines := relationships.Explain(rel); len(lines) == 0 ||
		!strings.Contains(strings.Join(lines, " "), "not a calibrated probability") {
		t.Fatal("the explanation does not state that the score is heuristic")
	}
}

func TestContradictionIsDetectedBetweenConflictingFigures(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	h.ingestAndWait(ctx, "Schedule",
		"Prepared for Example Ltd. The total project cost is £42,000 excluding VAT.",
		withMetadata(map[string]string{"project": "atlas"}))
	h.ingestAndWait(ctx, "Invoice reconciliation",
		"Reconciling against Example Ltd invoices. The total project cost is £51,500 excluding VAT.",
		withMetadata(map[string]string{"project": "atlas"}))

	h.waitForQueue(ctx, 90*time.Second)

	rels, _, err := h.store.Relationships.List(ctx, h.principal.WorkspaceID,
		domain.RelationshipFilter{}, domain.Page{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}

	var contradiction *domain.Signal
	for _, rel := range rels {
		for i, s := range rel.Evidence.Signals {
			if s.Type == domain.SigClaimContradiction {
				contradiction = &rel.Evidence.Signals[i]
			}
		}
	}
	if contradiction == nil {
		t.Fatal("two documents stating different costs produced no contradiction signal")
	}
	if !strings.Contains(contradiction.Detail, "42000") || !strings.Contains(contradiction.Detail, "51500") {
		t.Fatalf("the contradiction does not name both figures: %q", contradiction.Detail)
	}
	// Relata must not adjudicate.
	if !strings.Contains(contradiction.Detail, "does not decide which is correct") {
		t.Fatalf("the contradiction does not disclaim adjudication: %q", contradiction.Detail)
	}
}

func TestSimilarButDifferentDocumentsDoNotBecomeStronglyRelated(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	// Same place, same vocabulary, different events.
	h.ingestAndWait(ctx, "Fire drill report",
		"Fire drill report for the Northfield depot. The depot was evacuated in 4 minutes. "+
			"Two fire doors on the east side were obstructed. This was a scheduled drill.")
	h.ingestAndWait(ctx, "Safety inspection",
		"Safety inspection for the Northfield depot. Racking, lighting and fire doors were examined. "+
			"Two fire doors on the east side were noted as clear. This was a routine annual inspection.")

	h.waitForQueue(ctx, 90*time.Second)

	rels, _, err := h.store.Relationships.List(ctx, h.principal.WorkspaceID,
		domain.RelationshipFilter{}, domain.Page{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}

	for _, rel := range rels {
		// Similarity-driven links must be described as similarity, not as an
		// assertion that the documents concern the same event.
		for _, forbidden := range []domain.RelationshipType{
			domain.RelSameEntityAs, domain.RelSupersedes, domain.RelUpdates, domain.RelDerivedFrom,
		} {
			if rel.Type == forbidden {
				t.Fatalf("two superficially similar documents were linked as %q without evidence", forbidden)
			}
		}
	}
}

func TestRelationshipReviewIsAppendOnly(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	a := h.ingestAndWait(ctx, "Doc A", "Example Ltd will ship 240 units to the Northfield depot.")
	b := h.ingestAndWait(ctx, "Doc B", "Example Ltd confirmed the Northfield depot shipment of 240 units.")
	h.waitForQueue(ctx, 90*time.Second)

	rels, _, err := h.store.Relationships.List(ctx, h.principal.WorkspaceID,
		domain.RelationshipFilter{}, domain.Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) == 0 {
		// Create one explicitly so the review path is still exercised.
		rel, err := h.reviewer.CreateManual(ctx, h.principal, a.ID, b.ID, domain.RelSupports, "manual")
		if err != nil {
			t.Fatal(err)
		}
		rels = []*domain.Relationship{rel}
	}

	original := rels[0]
	originalType := original.Type
	originalProv := original.OriginalProvenance

	// Confirm.
	confirmed, err := h.reviewer.Confirm(ctx, h.principal, original.ID, "checked against the contract")
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.ReviewStatus != domain.ReviewConfirmed {
		t.Fatalf("review status = %q, want confirmed", confirmed.ReviewStatus)
	}
	if confirmed.Score != 1.0 {
		t.Fatalf("a confirmed relationship scored %.2f, want 1.00", confirmed.Score)
	}
	if confirmed.OriginalType != originalType || confirmed.OriginalProvenance != originalProv {
		t.Fatal("confirming altered the original proposal")
	}

	// Reject, which must keep the evidence and the earlier decision.
	rejected, err := h.reviewer.Reject(ctx, h.principal, original.ID, "actually unrelated")
	if err != nil {
		t.Fatal(err)
	}
	if rejected.ReviewStatus != domain.ReviewRejected {
		t.Fatalf("review status = %q, want rejected", rejected.ReviewStatus)
	}
	if rejected.Score != 0 {
		t.Fatalf("a rejected relationship scored %.2f, want 0", rejected.Score)
	}
	if len(rejected.Evidence.Signals) == 0 {
		t.Fatal("rejection destroyed the evidence")
	}
	if rejected.OriginalType != originalType {
		t.Fatal("rejection erased what was originally proposed")
	}
	if len(rejected.ReviewHistory) < 2 {
		t.Fatalf("review history has %d entries; both decisions must be retained", len(rejected.ReviewHistory))
	}

	var sawConfirm, sawReject bool
	for _, ev := range rejected.ReviewHistory {
		if ev.Action == "confirm" {
			sawConfirm = true
			if ev.Note != "checked against the contract" {
				t.Fatal("the confirmation note was lost")
			}
		}
		if ev.Action == "reject" {
			sawReject = true
		}
		if ev.ActorID != h.principal.UserID {
			t.Fatal("the acting user was not recorded")
		}
		if ev.At.IsZero() {
			t.Fatal("the decision timestamp was not recorded")
		}
	}
	if !sawConfirm || !sawReject {
		t.Fatal("the history does not contain both decisions")
	}
}

func TestRejectedRelationshipsDoNotExpandSearch(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	a := h.ingestAndWait(ctx, "Alpha topic", "Example Ltd shipped 240 units to the Northfield depot.")
	b := h.ingestAndWait(ctx, "Beta unrelated wording", "The quarterly gardening rota was agreed by the committee.")
	h.waitForQueue(ctx, 60*time.Second)

	rel, err := h.reviewer.CreateManual(ctx, h.principal, a.ID, b.ID, domain.RelProvidesContextFor, "")
	if err != nil {
		t.Fatal(err)
	}

	// While confirmed, expansion may reach the second item.
	resp, err := h.search.Search(ctx, h.principal, search_Query("Northfield depot units"))
	if err != nil {
		t.Fatal(err)
	}
	reachedBefore := containsResult(resp, b.ID)

	if _, err := h.reviewer.Reject(ctx, h.principal, rel.ID, "not actually related"); err != nil {
		t.Fatal(err)
	}

	resp, err = h.search.Search(ctx, h.principal, search_Query("Northfield depot units"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range resp.Results {
		if r.Item.ID == b.ID && r.ViaRelationship != nil {
			t.Fatal("a rejected relationship is still pulling items into search results")
		}
	}
	_ = reachedBefore
}

// --- Reprocessing ----------------------------------------------------------

func TestReprocessingDoesNotDuplicateDerivedRecords(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	item := h.ingestAndWait(ctx, "Atlas schedule",
		"Prepared by Dr Alice Chen for Example Ltd on 2025-02-10. "+
			"The delivery date is 3 March 2025 and the total project cost is £42,000.")
	h.ingestAndWait(ctx, "Atlas revision",
		"Prepared by Dr Alice Chen for Example Ltd. The revised delivery date is 14 March 2025.")
	h.waitForQueue(ctx, 90*time.Second)

	before := h.counts(ctx)

	for i := 0; i < 2; i++ {
		if _, err := h.pipeline.RequestReprocess(ctx, h.principal, item.ID, "test"); err != nil {
			t.Fatalf("reprocess %d: %v", i+1, err)
		}
		h.waitForQueue(ctx, 90*time.Second)
	}

	after := h.counts(ctx)

	if after.chunks != before.chunks {
		t.Errorf("chunks %d → %d after reprocessing", before.chunks, after.chunks)
	}
	if after.claims != before.claims {
		t.Errorf("claims %d → %d after reprocessing", before.claims, after.claims)
	}
	if after.entities != before.entities {
		t.Errorf("entities %d → %d after reprocessing", before.entities, after.entities)
	}
	if after.relationships != before.relationships {
		t.Errorf("relationships %d → %d after reprocessing", before.relationships, after.relationships)
	}
	if after.vectors != before.vectors {
		t.Errorf("vectors %d → %d after reprocessing", before.vectors, after.vectors)
	}

	// Processing runs are the exception: history accumulates on purpose.
	runs, err := h.store.Runs.ListByItem(ctx, h.principal.WorkspaceID, item.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) < 3 {
		t.Fatalf("only %d processing runs recorded; each reprocess must add one for audit", len(runs))
	}
}

func TestReprocessingPreservesHumanReviewDecisions(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	a := h.ingestAndWait(ctx, "Doc A", "Example Ltd shipped 240 units to the Northfield depot on 3 March 2025.")
	b := h.ingestAndWait(ctx, "Doc B", "Example Ltd confirmed 240 units at the Northfield depot on 4 March 2025.")
	h.waitForQueue(ctx, 90*time.Second)

	rels, _, err := h.store.Relationships.List(ctx, h.principal.WorkspaceID,
		domain.RelationshipFilter{}, domain.Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) == 0 {
		rel, err := h.reviewer.CreateManual(ctx, h.principal, a.ID, b.ID, domain.RelSupports, "")
		if err != nil {
			t.Fatal(err)
		}
		rels = []*domain.Relationship{rel}
	}

	confirmed, err := h.reviewer.Confirm(ctx, h.principal, rels[0].ID, "verified")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := h.pipeline.RequestReprocess(ctx, h.principal, a.ID, "test"); err != nil {
		t.Fatal(err)
	}
	h.waitForQueue(ctx, 90*time.Second)

	after, err := h.store.Relationships.Get(ctx, h.principal.WorkspaceID, confirmed.ID)
	if err != nil {
		t.Fatalf("the confirmed relationship disappeared during reprocessing: %v", err)
	}
	if after.ReviewStatus != domain.ReviewConfirmed {
		t.Fatalf("reprocessing reset the review status to %q", after.ReviewStatus)
	}
	if len(after.ReviewHistory) == 0 {
		t.Fatal("reprocessing erased the review history")
	}
}

type counts struct {
	items, chunks, entities, claims, relationships, vectors int64
}

func (h *harness) counts(ctx context.Context) counts {
	h.t.Helper()
	ws := h.principal.WorkspaceID

	var c counts
	c.items, _ = h.store.Items.Count(ctx, ws)
	c.chunks, _ = h.store.Chunks.Count(ctx, ws)
	c.entities, _ = h.store.Entities.Count(ctx, ws)
	c.claims, _ = h.store.Claims.Count(ctx, ws)
	c.relationships, _ = h.store.Relationships.Count(ctx, ws)
	c.vectors = h.vectorCount(ctx, ws)
	return c
}

// --- Deletion --------------------------------------------------------------

func TestDeletionRemovesDerivedRecordsAndVectors(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	item := h.ingestAndWait(ctx, "To be deleted",
		"Prepared by Dr Alice Chen for Uniquely Named Ltd. The cost is £42,000 and the date is 3 March 2025.")
	h.waitForQueue(ctx, 60*time.Second)

	before := h.counts(ctx)
	if before.vectors == 0 || before.chunks == 0 {
		t.Fatal("nothing was created to delete")
	}

	// The dry run must report without changing anything.
	plan, err := h.pipeline.PlanDeletion(ctx, h.principal.WorkspaceID, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Chunks == 0 {
		t.Fatal("the deletion plan reports no chunks")
	}
	if len(plan.OrphanedEntities) == 0 {
		t.Fatal("the plan does not identify entities that only this item references")
	}
	if mid := h.counts(ctx); mid != before {
		t.Fatal("the dry run changed stored data")
	}

	if _, err := h.pipeline.RequestDeletion(ctx, h.principal, item.ID); err != nil {
		t.Fatal(err)
	}
	h.waitForQueue(ctx, 60*time.Second)

	after := h.counts(ctx)
	if after.items != before.items-1 {
		t.Errorf("items %d → %d, want one fewer", before.items, after.items)
	}
	if after.chunks != 0 {
		t.Errorf("%d chunks survived deletion", after.chunks)
	}
	if after.claims != 0 {
		t.Errorf("%d claims survived deletion", after.claims)
	}
	if after.vectors != 0 {
		t.Errorf("%d orphaned vectors survived deletion", after.vectors)
	}
	if after.entities != 0 {
		t.Errorf("%d entities survived, though only the deleted item referenced them", after.entities)
	}

	// The audit trail must record the deletion.
	events, _, err := h.store.Audit.List(ctx, h.principal.WorkspaceID, domain.Page{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var sawDeletion bool
	for _, ev := range events {
		if ev.Action == domain.AuditItemDeleted && ev.TargetID == item.ID {
			sawDeletion = true
		}
	}
	if !sawDeletion {
		t.Fatal("the deletion was not recorded in the audit log")
	}
}

func TestDeletionRetainsEntitiesStillReferencedElsewhere(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	a := h.ingestAndWait(ctx, "First", "Example Ltd shipped 240 units on 3 March 2025.")
	h.ingestAndWait(ctx, "Second", "Example Ltd also sponsors the athletics championship in 2025.")
	h.waitForQueue(ctx, 60*time.Second)

	if _, err := h.pipeline.RequestDeletion(ctx, h.principal, a.ID); err != nil {
		t.Fatal(err)
	}
	h.waitForQueue(ctx, 60*time.Second)

	ents, _, err := h.store.Entities.List(ctx, h.principal.WorkspaceID,
		domain.EntityFilter{NameContains: "Example Ltd"}, domain.Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) == 0 {
		t.Fatal("an entity still referenced by another item was deleted")
	}
	for _, e := range ents {
		for _, id := range e.ItemIDs {
			if id == a.ID {
				t.Fatal("the deleted item is still referenced by an entity")
			}
		}
	}
}

// --- Job recovery ----------------------------------------------------------

func TestJobsRecoverAfterWorkerInterruption(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	// Simulate a worker that claimed a job and died: a running job whose lease
	// has already expired.
	job := &domain.Job{
		ID: "11111111-1111-4111-8111-111111111111", WorkspaceID: h.principal.WorkspaceID,
		Type: domain.JobParseItem, Status: domain.JobPending,
		ItemID:      "22222222-2222-4222-8222-222222222222",
		MaxAttempts: 3, RunAfter: time.Now().Add(-time.Minute),
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := h.store.Jobs.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}

	claimed, err := h.store.Jobs.Claim(ctx, "dead-worker", nil, -time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil {
		t.Skip("another worker claimed the job first")
	}

	released, err := h.store.Jobs.ReleaseExpired(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if released == 0 {
		t.Fatal("a job with an expired lease was not returned to the queue")
	}

	got, err := h.store.Jobs.Get(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.JobPending {
		t.Fatalf("status = %q after lease expiry, want pending", got.Status)
	}
	if got.LeaseOwner != "" {
		t.Fatalf("the dead worker still owns the lease: %q", got.LeaseOwner)
	}
	if !strings.Contains(got.LastError, "lease expired") {
		t.Fatalf("the reason was not recorded: %q", got.LastError)
	}
}

func TestExhaustedJobsFailRatherThanLoopForever(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	job := &domain.Job{
		ID: "33333333-3333-4333-8333-333333333333", WorkspaceID: h.principal.WorkspaceID,
		Type: domain.JobParseItem, Status: domain.JobPending,
		ItemID: "44444444-4444-4444-8444-444444444444",
		// Already at its attempt limit.
		Attempts: 3, MaxAttempts: 3,
		RunAfter: time.Now().Add(-time.Minute), CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := h.store.Jobs.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Jobs.Claim(ctx, "dead-worker", nil, -time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Jobs.ReleaseExpired(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}

	got, err := h.store.Jobs.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.JobFailed {
		t.Fatalf("status = %q; a job out of attempts must be terminal, not requeued forever", got.Status)
	}
}

func TestIdempotentEnqueueDoesNotDuplicatePendingWork(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	// The workers must not claim the job mid-test: once it leaves the pending
	// pool the partial index no longer constrains the key, which is correct
	// behaviour but makes the count non-deterministic.
	h.pauseWorkers()

	key := "shared-idempotency-key"
	for i := 0; i < 5; i++ {
		err := h.store.Jobs.Enqueue(ctx, &domain.Job{
			ID: ids_New(), WorkspaceID: h.principal.WorkspaceID,
			Type: domain.JobParseItem, Status: domain.JobPending,
			ItemID:      "55555555-5555-4555-8555-555555555555",
			MaxAttempts: 3, IdempotencyKey: key,
			RunAfter: time.Now(), CreatedAt: time.Now(), UpdatedAt: time.Now(),
		})
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	jobsList, _, err := h.store.Jobs.List(ctx, h.principal.WorkspaceID,
		[]domain.JobStatus{domain.JobPending}, []domain.JobType{domain.JobParseItem},
		domain.Page{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var matching int
	for _, j := range jobsList {
		if j.IdempotencyKey == key {
			matching++
		}
	}
	if matching != 1 {
		t.Fatalf("%d pending jobs share one idempotency key; duplicate work would be performed", matching)
	}
}

// --- Answer generation -----------------------------------------------------

func TestAnswerCitesOnlyRetrievedSources(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	h.ingestAndWait(ctx, "Cost schedule", "The total project cost is £42,000 excluding VAT for Example Ltd.")
	h.waitForQueue(ctx, 60*time.Second)

	// The model cites one real reference and one it invented.
	h.enableAnalysis(ctx, `{"answer":"The cost is £42,000 [S1].","sufficient":true,`+
		`"citations":["S1","S99"],"conflicts":[],"injection_attempt":false}`)

	ans, err := h.answer.Answer(ctx, h.principal, search_AnswerRequest("What is the project cost?"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.Sources) == 0 {
		t.Fatal("the answer used no sources")
	}
	for _, s := range ans.Sources {
		if s.ItemID == "" {
			t.Fatal("a source does not resolve to a stored item")
		}
	}
	var cited int
	for _, s := range ans.Sources {
		if s.Cited {
			cited++
		}
	}
	if cited != 1 {
		t.Fatalf("%d sources marked as cited; the fabricated reference should have been dropped", cited)
	}
}

func TestAnswerNeverSeesItemIdentifiers(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	item := h.ingestAndWait(ctx, "Cost schedule", "The total project cost is £42,000 excluding VAT.")
	h.waitForQueue(ctx, 60*time.Second)

	fake := h.enableAnalysis(ctx, `{"answer":"x","sufficient":false,"citations":[]}`)
	if _, err := h.answer.Answer(ctx, h.principal, search_AnswerRequest("cost?")); err != nil {
		t.Fatal(err)
	}

	for _, p := range fake.prompts() {
		if strings.Contains(p, item.ID) {
			t.Fatal("an internal item ID was exposed to the model, so a fabricated citation could resolve to a real record")
		}
	}
}

func TestAnswerReportsInsufficientMaterial(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.enableAnalysis(ctx, `{"answer":"x","sufficient":true,"citations":["S1"]}`)

	ans, err := h.answer.Answer(ctx, h.principal, search_AnswerRequest("something never stored"))
	if err != nil {
		t.Fatal(err)
	}
	if ans.Sufficient {
		t.Fatal("an answer with no stored material claimed sufficiency")
	}
	if !strings.Contains(strings.ToLower(ans.Answer), "nothing in the stored material") {
		t.Fatalf("insufficiency was not stated plainly: %q", ans.Answer)
	}
}
