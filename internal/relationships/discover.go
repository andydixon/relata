package relationships

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/andydixon/relata/internal/analysis"
	"github.com/andydixon/relata/internal/config"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/ids"
	"github.com/andydixon/relata/internal/jobs"
	"github.com/andydixon/relata/internal/observability"
)

// Discoverer finds candidate relationships for a newly-processed item.
//
// It never compares an item with every other item. Candidates come from three
// bounded, indexed sources — vector neighbours, items sharing an entity, and
// items in a temporal window — and the union is capped before any scoring or
// model call happens.
type Discoverer struct {
	items    domain.ItemRepository
	chunks   domain.ChunkRepository
	entities domain.EntityRepository
	aliases  domain.AliasRepository
	claims   domain.ClaimRepository
	rels     domain.RelationshipRepository
	vectors  domain.VectorStore
	embedder domain.Embedder
	analysis *analysis.Service
	cfg      *config.Config
}

// NewDiscoverer builds the relationship discoverer.
func NewDiscoverer(items domain.ItemRepository, chunks domain.ChunkRepository,
	ents domain.EntityRepository, aliases domain.AliasRepository, claims domain.ClaimRepository,
	rels domain.RelationshipRepository, vectors domain.VectorStore,
	embedder domain.Embedder, an *analysis.Service, cfg *config.Config) *Discoverer {
	return &Discoverer{
		items: items, chunks: chunks, entities: ents, aliases: aliases, claims: claims,
		rels: rels, vectors: vectors, embedder: embedder, analysis: an, cfg: cfg,
	}
}

// Stats summarise one discovery run.
type Stats struct {
	Candidates int
	Scored     int
	Stored     int
	ModelCalls int
	Discarded  int
}

// candidate accumulates evidence about one potential relationship.
type candidate struct {
	itemID   string
	item     *domain.Item
	signals  []domain.Signal
	srcChunk []string
	tgtChunk []string
	shared   []string
	bestSim  float64
	srcText  string
	tgtText  string
}

// DiscoverForItem finds, scores and stores relationships for one item.
func (d *Discoverer) DiscoverForItem(ctx context.Context, item *domain.Item, pool *jobs.Pool) (Stats, error) {
	log := observability.LoggerFrom(ctx)
	var stats Stats

	chunks, err := d.chunks.ListByItem(ctx, item.WorkspaceID, item.ID)
	if err != nil {
		return stats, err
	}
	if len(chunks) == 0 {
		return stats, nil
	}

	cands := map[string]*candidate{}
	get := func(itemID string) *candidate {
		c := cands[itemID]
		if c == nil {
			c = &candidate{itemID: itemID}
			cands[itemID] = c
		}
		return c
	}

	// --- Source 1: semantically similar chunks -----------------------------
	//
	// Vector search is filtered to the workspace and excludes the item itself,
	// so the result set is already bounded and relevant.
	if err := d.gatherBySimilarity(ctx, item, chunks, get); err != nil {
		log.Warn("similarity candidate search failed", "error", err.Error())
	}

	// --- Source 2: items sharing an entity ---------------------------------
	itemEntities, err := d.entities.ListByItem(ctx, item.WorkspaceID, item.ID)
	if err != nil {
		return stats, err
	}
	if err := d.gatherByEntities(ctx, item, itemEntities, get); err != nil {
		log.Warn("shared-entity candidate search failed", "error", err.Error())
	}

	// --- Source 3: items making claims about the same subject --------------
	itemClaims, err := d.claims.ListByItem(ctx, item.WorkspaceID, item.ID)
	if err != nil {
		return stats, err
	}
	if err := d.gatherByClaims(ctx, item, itemClaims, get); err != nil {
		log.Warn("claim candidate search failed", "error", err.Error())
	}

	if len(cands) == 0 {
		return stats, nil
	}

	// Cap the candidate set before any expensive work.
	ordered := rankCandidates(cands, d.cfg.Rel.MaxCandidates)
	stats.Candidates = len(ordered)

	// Load candidate items in one query.
	ids2 := make([]string, 0, len(ordered))
	for _, c := range ordered {
		ids2 = append(ids2, c.itemID)
	}
	others, err := d.items.GetMany(ctx, item.WorkspaceID, ids2)
	if err != nil {
		return stats, err
	}
	byID := make(map[string]*domain.Item, len(others))
	for _, it := range others {
		byID[it.ID] = it
	}

	// Confirmed aliases, fetched once for the whole run and indexed both ways.
	// Querying per candidate would be O(candidates × entities) round trips, and
	// the stored direction is arbitrary, so a one-way lookup would miss half
	// the pairs.
	aliasPartners := d.confirmedAliasIndex(ctx, item.WorkspaceID)

	// Entity corpus frequency, so a name appearing everywhere counts for less.
	entIDs := make([]string, 0, len(itemEntities))
	for _, e := range itemEntities {
		entIDs = append(entIDs, e.ID)
	}
	freq := map[string]int{}
	if f, ok := d.entities.(interface {
		CorpusFrequency(context.Context, string, []string) (map[string]int, error)
	}); ok {
		if got, err := f.CorpusFrequency(ctx, item.WorkspaceID, entIDs); err == nil {
			freq = got
		}
	}

	for _, c := range ordered {
		other := byID[c.itemID]
		if other == nil || other.Deleted {
			continue
		}
		c.item = other

		d.addEntitySignals(ctx, item, other, itemEntities, freq, aliasPartners, c)
		d.addTemporalSignal(item, other, c)
		d.addMetadataSignals(item, other, c)
		d.addClaimSignals(ctx, item, other, itemClaims, c)
		if item.ContentHash == other.ContentHash {
			c.signals = append(c.signals, ContentHashSignal(item.ContentHash))
		}
	}

	// Score without the model first, so model calls go only to candidates that
	// already have real evidence behind them.
	sort.SliceStable(ordered, func(i, j int) bool {
		return Score(ordered[i].signals) > Score(ordered[j].signals)
	})

	for _, c := range ordered {
		if c.item == nil {
			continue
		}
		base := Score(c.signals)
		if base < d.cfg.Rel.MinScore {
			stats.Discarded++
			continue
		}

		var opinion *analysis.RelationshipOpinion
		if d.analysis.Enabled() && stats.ModelCalls < d.cfg.Rel.MaxModelCalls {
			op, err := d.classify(ctx, item, c, pool)
			if err == nil && op != nil {
				stats.ModelCalls++
				opinion = op
				// A model that says "not related" must not simultaneously have
				// its type suggestion recorded as supporting evidence. Only one
				// of these two signals is ever added.
				if op.Related {
					c.signals = append(c.signals,
						ModelClassificationSignal(op.Type, op.Confidence, op.Rationale, op.Model))
				} else {
					c.signals = append(c.signals, domain.Signal{
						Type:     domain.SigModelClassify,
						Value:    "judged unrelated",
						Strength: 0,
						Weight:   Weights[domain.SigModelClassify],
						Detail: "A language model judged these items unrelated despite the other signals. " +
							"This is a model opinion, not a verified fact.",
					})
				}
			}
		}

		stats.Scored++
		if err := d.store(ctx, item, c, opinion); err != nil {
			log.Warn("could not store a relationship", "target", c.itemID, "error", err.Error())
			continue
		}
		stats.Stored++
	}

	return stats, nil
}

// gatherBySimilarity finds items with semantically similar chunks.
func (d *Discoverer) gatherBySimilarity(ctx context.Context, item *domain.Item,
	chunks []*domain.Chunk, get func(string) *candidate) error {

	// Embedding every chunk again would be wasteful; a bounded sample of the
	// item's chunks is enough to find its neighbourhood.
	sample := chunks
	const maxProbe = 8
	if len(sample) > maxProbe {
		step := len(sample) / maxProbe
		picked := make([]*domain.Chunk, 0, maxProbe)
		for i := 0; i < len(sample) && len(picked) < maxProbe; i += step {
			picked = append(picked, sample[i])
		}
		sample = picked
	}

	texts := make([]string, len(sample))
	for i, c := range sample {
		texts[i] = c.Text
	}
	vecs, err := d.embedder.Embed(ctx, texts)
	if err != nil {
		return err
	}

	perChunk := d.cfg.Rel.MaxNeighboursPerChunk
	if perChunk <= 0 {
		perChunk = 10
	}

	for i, v := range vecs {
		matches, err := d.vectors.Search(ctx, v, perChunk, domain.VectorFilter{
			WorkspaceID:    item.WorkspaceID,
			ExcludeItemIDs: []string{item.ID},
			EmbeddingModel: d.embedder.ModelID(),
		})
		if err != nil {
			return err
		}
		for _, m := range matches {
			if m.ItemID == "" || m.ItemID == item.ID {
				continue
			}
			c := get(m.ItemID)
			if m.Score > c.bestSim {
				c.bestSim = m.Score
				c.srcText = sample[i].Text
			}
			c.srcChunk = appendUnique(c.srcChunk, sample[i].ID, 5)
			c.tgtChunk = appendUnique(c.tgtChunk, m.ChunkID, 5)
		}
	}

	// The similarity signal itself is built in store, from the best match
	// recorded above, so that it reflects the strongest evidence found.
	return nil
}

// gatherByEntities finds items referencing the same entities.
func (d *Discoverer) gatherByEntities(ctx context.Context, item *domain.Item,
	ents []*domain.Entity, get func(string) *candidate) error {
	if len(ents) == 0 {
		return nil
	}
	entIDs := make([]string, 0, len(ents))
	for _, e := range ents {
		// Concepts are shared too widely to be useful as a candidate source;
		// they still contribute a signal once a candidate exists.
		if e.Type == domain.EntityConcept {
			continue
		}
		entIDs = append(entIDs, e.ID)
	}
	if len(entIDs) == 0 {
		return nil
	}

	itemIDs, err := d.entities.ItemsSharingEntities(ctx, item.WorkspaceID, entIDs, item.ID,
		d.cfg.Rel.MaxCandidates*2)
	if err != nil {
		return err
	}
	for _, id := range itemIDs {
		get(id)
	}
	return nil
}

// gatherByClaims finds items making claims about the same subjects.
func (d *Discoverer) gatherByClaims(ctx context.Context, item *domain.Item,
	claims []*domain.Claim, get func(string) *candidate) error {
	if len(claims) == 0 {
		return nil
	}
	subjects := make([]string, 0, len(claims))
	seen := map[string]bool{}
	for _, c := range claims {
		if c.Subject != "" && !seen[c.Subject] {
			seen[c.Subject] = true
			subjects = append(subjects, c.Subject)
		}
	}
	if len(subjects) == 0 {
		return nil
	}

	others, err := d.claims.FindBySubjects(ctx, item.WorkspaceID, subjects, item.ID,
		d.cfg.Rel.MaxCandidates*3)
	if err != nil {
		return err
	}
	for _, oc := range others {
		get(oc.ItemID)
	}
	return nil
}

// addEntitySignals records exactly which entities two items share.
func (d *Discoverer) addEntitySignals(ctx context.Context, item, other *domain.Item,
	itemEntities []*domain.Entity, freq map[string]int,
	aliasPartners map[string][]aliasLink, c *candidate) {

	otherEnts, err := d.entities.ListByItem(ctx, other.WorkspaceID, other.ID)
	if err != nil {
		return
	}
	otherByKey := make(map[string]*domain.Entity, len(otherEnts))
	for _, e := range otherEnts {
		otherByKey[e.NormalisedKey] = e
	}

	concepts := []string{}
	added := 0
	for _, e := range itemEntities {
		if _, ok := otherByKey[e.NormalisedKey]; !ok {
			continue
		}
		if e.Type == domain.EntityConcept || e.Type == domain.EntitySubject {
			concepts = append(concepts, e.CanonicalName)
			continue
		}
		if added >= 6 {
			continue
		}
		added++
		c.shared = append(c.shared, e.CanonicalName)
		c.signals = append(c.signals, SharedEntitySignal(e.CanonicalName, e.Type, freq[e.ID]))
	}

	// Entities linked by a CONFIRMED alias count too, at a lower weight than an
	// exact match: the identification came from a person, but the two records
	// are still separate entities. Unconfirmed candidate aliases are ignored
	// entirely — treating a guess as evidence is exactly what the alias
	// mechanism exists to avoid.
	addAliasSignals(itemEntities, otherEnts, otherByKey, aliasPartners, c)

	if len(concepts) > 0 {
		// Overlap fraction, so two documents sharing three of four concepts
		// score higher than two sharing three of forty.
		denom := float64(maxInt(len(itemEntities), len(otherEnts)))
		overlap := float64(len(concepts))
		if denom > 0 {
			overlap /= denom
		}
		c.signals = append(c.signals, SharedConceptSignal(concepts, overlap))
	}
}

// aliasLink is one confirmed "these are the same thing" link between entities.
type aliasLink struct {
	otherID    string
	similarity float64
}

// maxConfirmedAliases bounds the index. Confirmed aliases require human action,
// so a workspace will not have many.
const maxConfirmedAliases = 500

// confirmedAliasIndex maps each entity to the entities a person has confirmed
// it is the same as, in both directions.
//
// Only CONFIRMED aliases appear. A candidate alias is a proposal, and treating
// a proposal as evidence is exactly what the alias mechanism exists to avoid.
func (d *Discoverer) confirmedAliasIndex(ctx context.Context, workspaceID string) map[string][]aliasLink {
	if d.aliases == nil {
		return nil
	}
	links, total, err := d.aliases.List(ctx, workspaceID, domain.AliasConfirmed,
		domain.Page{Limit: maxConfirmedAliases})
	if err != nil || len(links) == 0 {
		return nil
	}
	if total > int64(len(links)) {
		// Past the cap, confirmed aliases stop contributing their signal. That
		// weakens discovery rather than breaking it, but silently dropping
		// human-confirmed information is not something to do quietly.
		slog.Warn("confirmed alias index truncated; aliases beyond the cap will not contribute a signal",
			"workspace_id", workspaceID, "confirmed", total, "indexed", len(links),
			"cap", maxConfirmedAliases)
	}

	index := make(map[string][]aliasLink, len(links)*2)
	for _, l := range links {
		if l.EntityID == "" || l.CandidateEntityID == "" {
			continue
		}
		index[l.EntityID] = append(index[l.EntityID],
			aliasLink{otherID: l.CandidateEntityID, similarity: l.Similarity})
		index[l.CandidateEntityID] = append(index[l.CandidateEntityID],
			aliasLink{otherID: l.EntityID, similarity: l.Similarity})
	}
	return index
}

// addAliasSignals records matches made through a human-confirmed alias.
//
// These are weighted below an exact match: the identification came from a
// person, but the two records remain separate entities.
func addAliasSignals(itemEntities, otherEnts []*domain.Entity,
	otherByKey map[string]*domain.Entity, aliasPartners map[string][]aliasLink, c *candidate) {

	if len(aliasPartners) == 0 || len(itemEntities) == 0 || len(otherEnts) == 0 {
		return
	}
	otherByID := make(map[string]*domain.Entity, len(otherEnts))
	for _, e := range otherEnts {
		otherByID[e.ID] = e
	}

	added := 0
	for _, e := range itemEntities {
		if added >= 3 {
			return
		}
		// An exact match is already counted as a shared entity.
		if _, exact := otherByKey[e.NormalisedKey]; exact {
			continue
		}
		for _, link := range aliasPartners[e.ID] {
			match, ok := otherByID[link.otherID]
			if !ok {
				continue
			}
			c.signals = append(c.signals,
				AliasEntitySignal(e.CanonicalName, match.CanonicalName, link.similarity))
			c.shared = append(c.shared, e.CanonicalName)
			added++
			break
		}
	}
}

// addTemporalSignal records how close in time two items are.
func (d *Discoverer) addTemporalSignal(item, other *domain.Item, c *candidate) {
	if item.CreatedAt.IsZero() || other.CreatedAt.IsZero() {
		return
	}
	gap := item.CreatedAt.Sub(other.CreatedAt)
	sig := TemporalProximitySignal(gap, d.cfg.Rel.TemporalWindow)
	if sig.Strength > 0 {
		c.signals = append(c.signals, sig)
	}
}

// addMetadataSignals records matching user-supplied metadata.
func (d *Discoverer) addMetadataSignals(item, other *domain.Item, c *candidate) {
	added := 0
	for k, v := range item.Metadata {
		if added >= 3 {
			return
		}
		if ov, ok := other.Metadata[k]; ok && ov == v && v != "" {
			c.signals = append(c.signals, SharedMetadataSignal(k, v))
			added++
		}
	}
}

// numericTolerance is the relative difference below which two figures are
// treated as the same value rather than a disagreement.
const numericTolerance = 0.001

// addClaimSignals compares the two items' claims for agreement and conflict.
//
// This is what makes "which sources disagree about the cost?" answerable
// without a language model: two claims about the same subject carrying
// materially different figures are a contradiction, and it is recorded with
// both quotations attached.
func (d *Discoverer) addClaimSignals(ctx context.Context, item, other *domain.Item,
	itemClaims []*domain.Claim, c *candidate) {

	if len(itemClaims) == 0 {
		return
	}
	otherClaims, err := d.claims.ListByItem(ctx, other.WorkspaceID, other.ID)
	if err != nil || len(otherClaims) == 0 {
		return
	}

	bySubject := map[string][]*domain.Claim{}
	for _, oc := range otherClaims {
		if oc.Subject != "" {
			bySubject[oc.Subject] = append(bySubject[oc.Subject], oc)
		}
	}

	supports, conflicts := 0, 0
	for _, ic := range itemClaims {
		if ic.Subject == "" {
			continue
		}
		for _, oc := range bySubject[ic.Subject] {
			agree, disagree, detail := compareClaims(ic, oc)
			switch {
			case disagree && conflicts < 3:
				conflicts++
				c.signals = append(c.signals, ClaimContradictionSignal(ic.Subject, detail, 1))
				c.srcText = ic.SourceRef.Quote
				c.tgtText = oc.SourceRef.Quote
			case agree && supports < 3:
				supports++
				c.signals = append(c.signals, ClaimSupportSignal(ic.Subject, detail, 0.8))
				if c.srcText == "" {
					c.srcText = ic.SourceRef.Quote
					c.tgtText = oc.SourceRef.Quote
				}
			}
		}
	}
}

// compareClaims decides whether two claims about the same subject agree.
func compareClaims(a, b *domain.Claim) (agree, disagree bool, detail string) {
	// Figures about the same subject.
	if len(a.Numbers) > 0 && len(b.Numbers) > 0 {
		an, bn := a.Numbers[0], b.Numbers[0]
		if an == 0 && bn == 0 {
			return true, false, "Both sources state the same figure for " + a.Subject + "."
		}
		denom := math.Max(math.Abs(an), math.Abs(bn))
		if denom > 0 && math.Abs(an-bn)/denom < numericTolerance {
			return true, false, fmt.Sprintf("Both sources give %s as %s.", a.Subject, formatNumber(an))
		}
		return false, true, fmt.Sprintf(
			"The sources give different figures for %s: %s and %s. Relata does not decide which is correct.",
			a.Subject, formatNumber(an), formatNumber(bn))
	}

	// Dates about the same subject.
	if len(a.Dates) > 0 && len(b.Dates) > 0 {
		if a.Dates[0] == b.Dates[0] {
			return true, false, fmt.Sprintf("Both sources give %s as %s.", a.Subject, a.Dates[0])
		}
		return false, true, fmt.Sprintf(
			"The sources give different dates for %s: %s and %s. Relata does not decide which is correct.",
			a.Subject, a.Dates[0], b.Dates[0])
	}

	// Opposite polarity on the same subject.
	if a.Polarity != 0 && b.Polarity != 0 && a.Polarity != b.Polarity {
		return false, true, fmt.Sprintf("One source affirms and the other denies a statement about %s.", a.Subject)
	}
	return false, false, ""
}

func formatNumber(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%g", v)
}

// classify asks the model for its opinion on one candidate.
func (d *Discoverer) classify(ctx context.Context, item *domain.Item, c *candidate, pool *jobs.Pool) (*analysis.RelationshipOpinion, error) {
	srcText := c.srcText
	if srcText == "" {
		srcText = excerpt(item.ExtractedText, 1200)
	}
	tgtText := c.tgtText
	if tgtText == "" && c.item != nil {
		tgtText = excerpt(c.item.ExtractedText, 1200)
	}
	if strings.TrimSpace(srcText) == "" || strings.TrimSpace(tgtText) == "" {
		return nil, nil
	}

	release, err := pool.AcquireAI(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	return d.analysis.ClassifyRelationship(ctx, srcText, tgtText, c.shared)
}

// store writes a scored relationship, or skips it when too weak.
func (d *Discoverer) store(ctx context.Context, item *domain.Item, c *candidate, opinion *analysis.RelationshipOpinion) error {
	// Add the similarity signal now that the best match is known.
	signals := c.signals
	if c.bestSim > 0 {
		signals = append(signals, SimilaritySignal(c.bestSim))
	}
	signals = Normalise(signals)
	score := Score(signals)

	if score < d.cfg.Rel.StoreScore {
		return nil
	}
	// A discovered relationship has not been reviewed, so it cannot reach the
	// score reserved for a human decision.
	if score > MaxUnreviewedScore {
		score = MaxUnreviewedScore
	}

	var opType *domain.RelationshipType
	if opinion != nil && opinion.Related {
		opType = &opinion.Type
	}
	relType := InferType(signals, item.CreatedAt, c.item.CreatedAt, opType)

	// A near-duplicate is flagged as such but never merged: two items may be
	// near-identical and still be genuinely separate records.
	if c.bestSim >= d.cfg.Rel.NearDuplicateSimilarity && item.ContentHash != c.item.ContentHash {
		signals = append(signals, domain.Signal{
			Type:     domain.SigSemanticSimilarity,
			Value:    fmt.Sprintf("%.2f — likely near-duplicate", c.bestSim),
			Strength: 1,
			Weight:   Weights[domain.SigSemanticSimilarity],
			Detail:   "These items are nearly identical in meaning. Relata has NOT merged them; review them if one supersedes the other.",
		})
		score = Score(Normalise(signals))
		if score > MaxUnreviewedScore {
			score = MaxUnreviewedScore
		}
	}

	// Non-directional types are stored in a canonical order so that A~B and
	// B~A are one relationship, not two.
	src, tgt := item.ID, c.item.ID
	srcTime, tgtTime := item.CreatedAt, c.item.CreatedAt
	if !relType.Directed() && src > tgt {
		src, tgt = tgt, src
	}
	// A chronological type must be stated in the direction the dates support.
	if relType == domain.RelOccurredBefore && srcTime.After(tgtTime) {
		relType = domain.RelOccurredAfter
	}

	evidence := domain.Evidence{
		Signals:        signals,
		SourceChunkIDs: c.srcChunk,
		TargetChunkIDs: c.tgtChunk,
		SharedEntities: c.shared,
		SourceExcerpt:  excerpt(c.srcText, 400),
		TargetExcerpt:  excerpt(c.tgtText, 400),
	}
	if opinion != nil {
		evidence.ModelRationale = opinion.Rationale
		evidence.ModelVersion = opinion.Model
	}

	provenance := domain.ProvDeterministic
	if opinion != nil {
		// The model influenced the type, so the relationship is a model
		// inference and must be labelled as one.
		provenance = domain.ProvModelInferred
	}

	now := time.Now().UTC()
	rel := &domain.Relationship{
		ID:                 ids.New(),
		WorkspaceID:        item.WorkspaceID,
		SourceKind:         domain.NodeItem,
		SourceID:           src,
		TargetKind:         domain.NodeItem,
		TargetID:           tgt,
		Type:               relType,
		Provenance:         provenance,
		Score:              score,
		ScoreVersion:       ScoreVersion,
		Evidence:           evidence,
		OriginalType:       relType,
		OriginalProvenance: provenance,
		ReviewStatus:       domain.ReviewPending,
		// The dedupe key excludes the type and ignores endpoint order, so that
		// one connection between two items is one relationship however it was
		// found. Without the ordering, discovery run from each end would store
		// the same link twice — once as A→B and once as B→A.
		DedupeKey: PairKey(item.WorkspaceID, src, tgt),
		CreatedAt: now,
		UpdatedAt: now,
	}

	_, err := d.rels.UpsertByDedupeKey(ctx, rel)
	return err
}

// rankCandidates caps the candidate set, keeping those with the most evidence.
func rankCandidates(cands map[string]*candidate, limit int) []*candidate {
	out := make([]*candidate, 0, len(cands))
	for _, c := range cands {
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if len(out[i].shared) != len(out[j].shared) {
			return len(out[i].shared) > len(out[j].shared)
		}
		if out[i].bestSim != out[j].bestSim {
			return out[i].bestSim > out[j].bestSim
		}
		return out[i].itemID < out[j].itemID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func appendUnique(list []string, v string, max int) []string {
	if v == "" || len(list) >= max {
		return list
	}
	for _, e := range list {
		if e == v {
			return list
		}
	}
	return append(list, v)
}

func excerpt(s string, n int) string {
	s = strings.TrimSpace(collapseSpace(s))
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}

func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
