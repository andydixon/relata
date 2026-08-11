//go:build integration

package tests

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/ids"
	"github.com/andydixon/relata/internal/ingest"
	"github.com/andydixon/relata/internal/relationships"
)

// Duplicate detection is a check followed by an insert. Without a uniqueness
// constraint underneath, concurrent requests carrying identical content both
// pass the check and both store an item — so "an identical item already exists"
// silently becomes false.
func TestConcurrentIdenticalIngestsStoreOnlyOneItem(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	const concurrency = 8
	body := "The revised delivery date is 14 March 2025 for Example Ltd."

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		stored    []string
		duplicate int
		other     []error
	)

	start := make(chan struct{})
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // fire together

			res, err := h.ingest.Ingest(ctx, h.principal, ingest.Request{
				Title:      "Concurrent",
				Content:    []byte(body),
				SourceType: domain.SourceTypeNote,
			})

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				stored = append(stored, res.Item.ID)
			case errors.Is(err, domain.ErrDuplicate):
				duplicate++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("unexpected errors: %v", other)
	}
	if len(stored) != 1 {
		t.Fatalf("%d of %d concurrent identical ingests stored an item; exactly one should have, "+
			"the rest reported a duplicate (duplicates reported: %d)", len(stored), concurrency, duplicate)
	}
	if duplicate != concurrency-1 {
		t.Fatalf("%d requests reported a duplicate, want %d", duplicate, concurrency-1)
	}

	// And the store must agree.
	items, total, err := h.store.Items.List(ctx, h.principal.WorkspaceID,
		domain.ItemFilter{ContentHash: itemHash(t, h, stored[0])}, domain.Page{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("%d items share the content hash; duplicate detection did not hold", total)
	}
	_ = items
}

func itemHash(t *testing.T, h *harness, id string) string {
	t.Helper()
	it, err := h.store.Items.Get(context.Background(), h.principal.WorkspaceID, id)
	if err != nil {
		t.Fatal(err)
	}
	return it.ContentHash
}

// A deliberate duplicate must still be storable, so the constraint cannot be a
// blanket uniqueness rule on the content hash.
func TestDeliberateDuplicatesAreStillAllowed(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	body := "Example Ltd shipped 240 units on 3 March 2025."
	first := h.ingestText(ctx, "Original", body)

	res, err := h.ingest.Ingest(ctx, h.principal, ingest.Request{
		Title: "Deliberate copy", Content: []byte(body),
		SourceType: domain.SourceTypeNote, AllowDuplicate: true,
	})
	if err != nil {
		t.Fatalf("an explicitly allowed duplicate was refused: %v", err)
	}
	if res.Item.DuplicateOf != first.ID {
		t.Fatalf("duplicate_of = %q, want %q", res.Item.DuplicateOf, first.ID)
	}

	// A third, also explicit, must work too.
	if _, err := h.ingest.Ingest(ctx, h.principal, ingest.Request{
		Title: "Another copy", Content: []byte(body),
		SourceType: domain.SourceTypeNote, AllowDuplicate: true,
	}); err != nil {
		t.Fatalf("a second deliberate duplicate was refused: %v", err)
	}
}

// upsertManual used to locate an existing link by paging through the node's
// relationships. A well-connected item has more than one page of them, so the
// match was missed and the insert then collided with the unique dedupe index —
// asserting a relationship on a busy item failed outright.
func TestAssertingARelationshipOnAWellConnectedItemSucceeds(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	hub := h.ingestText(ctx, "Hub", "Example Ltd operates the central depot.")
	other := h.ingestText(ctx, "Other", "A separate note about the northern site.")

	// Give the hub more relationships than the old scan's page size.
	const beyondOnePage = 250
	for i := 0; i < beyondOnePage; i++ {
		spoke := &domain.Relationship{
			ID: ids.New(), WorkspaceID: h.principal.WorkspaceID,
			SourceKind: domain.NodeItem, SourceID: hub.ID,
			TargetKind: domain.NodeItem, TargetID: ids.New(),
			Type: domain.RelSimilarTo, Provenance: domain.ProvDeterministic,
			Score: 0.4, ScoreVersion: relationships.ScoreVersion,
			ReviewStatus: domain.ReviewPending,
			DedupeKey:    ids.New(),
			CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := h.store.Relationships.Create(ctx, spoke); err != nil {
			t.Fatalf("seeding relationship %d: %v", i, err)
		}
	}

	// Assert the same relationship twice. The second must update the first,
	// not fail on the unique index.
	first, err := h.reviewer.CreateManual(ctx, h.principal, hub.ID, other.ID, domain.RelSupports, "first")
	if err != nil {
		t.Fatalf("asserting a relationship on a well-connected item failed: %v", err)
	}

	second, err := h.reviewer.CreateManual(ctx, h.principal, hub.ID, other.ID, domain.RelContradicts, "corrected")
	if err != nil {
		t.Fatalf("re-asserting the relationship failed: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("re-assertion created a second record (%s then %s); it should have updated the first",
			first.ID, second.ID)
	}
	if second.Type != domain.RelContradicts {
		t.Fatalf("type = %q, want the corrected %q", second.Type, domain.RelContradicts)
	}
	if len(second.ReviewHistory) < 2 {
		t.Fatalf("review history has %d entries; both assertions should be recorded",
			len(second.ReviewHistory))
	}
}

// A model that satisfies the schema without answering must not cost the user
// the retrieval. The sources were found deterministically and any contradiction
// between them was recorded deterministically; neither depends on the model.
func TestADegenerateModelAnswerStillReturnsSourcesAndConflicts(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	h.ingestAndWait(ctx, "Cost schedule", "The total project cost is £42,000 excluding VAT for Example Ltd.")
	h.ingestAndWait(ctx, "Revised budget", "The total project cost is £51,500 including contingency for Example Ltd.")
	h.waitForQueue(ctx, 90*time.Second)

	// Exactly what llama3.2:3b returned: the schema echoed back as content.
	h.enableAnalysis(ctx, `{"answer":"sufficient","sufficient":true,"citations":[],"conflicts":[],"injection_attempt":false}`)

	ans, err := h.answer.Answer(ctx, h.principal, search_AnswerRequest("What is the project cost?"))
	if err != nil {
		t.Fatalf("a useless model response failed the whole request: %v", err)
	}

	// The non-answer must not be presented as an answer.
	if strings.Contains(strings.ToLower(ans.Answer), "\"sufficient\"") || ans.Answer == "sufficient" {
		t.Fatalf("the schema echo was shown to the user as an answer: %q", ans.Answer)
	}
	if ans.Sufficient {
		t.Fatal("a rejected answer was still marked sufficient")
	}

	// The deterministic work must survive.
	if len(ans.Sources) == 0 {
		t.Fatal("the retrieved sources were discarded because the model misbehaved")
	}
	joined := strings.Join(ans.Uncertainty, " ")
	if !strings.Contains(joined, "degenerate_content") {
		t.Fatalf("the rejection was not disclosed to the user; uncertainty said: %q", joined)
	}
	// Both figures were recorded as contradicting; that is independent of the model.
	if !strings.Contains(joined, "42,000") && !strings.Contains(joined, "42000") {
		t.Fatalf("the recorded contradiction was not surfaced; uncertainty said: %q", joined)
	}
}

// allow_duplicate means "keep a second copy". Losing the insert race reveals
// which item this one duplicates; it does not change the caller's answer.
func TestConcurrentDeliberateDuplicatesAreAllStored(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	const concurrency = 6
	body := "Example Ltd confirmed 240 units for delivery on 3 March 2025."

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		stored []*domain.Item
		errs   []error
	)

	start := make(chan struct{})
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			res, err := h.ingest.Ingest(ctx, h.principal, ingest.Request{
				Title: "Deliberate copy", Content: []byte(body),
				SourceType: domain.SourceTypeNote, AllowDuplicate: true,
			})

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			stored = append(stored, res.Item)
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d of %d explicitly-allowed duplicates were refused: %v",
			len(errs), concurrency, errs)
	}
	if len(stored) != concurrency {
		t.Fatalf("stored %d items, want all %d", len(stored), concurrency)
	}

	// Exactly one is the original; the rest must record what they duplicate.
	originals := 0
	for _, it := range stored {
		if it.DuplicateOf == "" {
			originals++
			continue
		}
		if _, err := h.store.Items.Get(ctx, h.principal.WorkspaceID, it.DuplicateOf); err != nil {
			t.Fatalf("item %s points at %s, which does not resolve: %v", it.ID, it.DuplicateOf, err)
		}
	}
	if originals != 1 {
		t.Fatalf("%d items claim to be the original; exactly one should", originals)
	}
}
