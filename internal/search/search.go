// Package search implements hybrid retrieval over stored items.
//
// A query runs through several independent paths — exact and lexical matching,
// vector similarity, entity matching, and expansion along confirmed
// relationships — whose results are fused by rank. Every result carries the
// reasons it was returned, and the three distinct numbers involved (query
// relevance, semantic similarity, relationship strength) are reported
// separately rather than merged into one figure.
package search

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/andydixon/relata/internal/config"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/entities"
	"github.com/andydixon/relata/internal/observability"
	"github.com/andydixon/relata/internal/ranking"
	"github.com/andydixon/relata/internal/relationships"
)

// Query is a search request. Every field is a typed value; no part of it ever
// becomes a database operator.
type Query struct {
	Text string

	SourceTypes []domain.SourceType
	MIMETypes   []string
	EntityIDs   []string
	RelTypes    []domain.RelationshipType
	CreatedFrom *time.Time
	CreatedTo   *time.Time
	// ConfirmedOnly restricts relationship expansion to human-confirmed links.
	ConfirmedOnly bool
	// MinRelStrength filters expanded results by relationship strength.
	MinRelStrength float64
	// Expand enables relationship expansion. On by default for the UI.
	Expand bool

	Limit  int
	Offset int
}

// Result is one item returned for a query.
type Result struct {
	Item *domain.Item `json:"item"`

	// QueryRelevance is how well this item answers THIS query, 0..1 relative to
	// the best result. It is not a similarity and not a probability.
	QueryRelevance float64 `json:"query_relevance"`
	// SemanticSimilarity is the best cosine similarity of any matching chunk,
	// reported separately because similarity is not relevance.
	SemanticSimilarity float64 `json:"semantic_similarity,omitempty"`

	Excerpt         string                 `json:"excerpt"`
	MatchedChunks   []MatchedChunk         `json:"matched_chunks,omitempty"`
	MatchedEntities []MatchedEntity        `json:"matched_entities,omitempty"`
	Related         []RelatedItem          `json:"related_items,omitempty"`
	WhyMatched      []string               `json:"why_matched"`
	Contributions   []ranking.Contribution `json:"contributions,omitempty"`

	// ViaRelationship is set when this item was reached by expansion rather
	// than by matching the query itself.
	ViaRelationship *RelatedItem `json:"via_relationship,omitempty"`

	RankVersion string `json:"rank_version"`
}

// MatchedChunk is a chunk that matched, with its own similarity.
type MatchedChunk struct {
	ChunkID    string  `json:"chunk_id"`
	Sequence   int     `json:"sequence"`
	Text       string  `json:"text"`
	Similarity float64 `json:"semantic_similarity,omitempty"`
	Page       int     `json:"page,omitempty"`
	Section    string  `json:"section,omitempty"`
}

// MatchedEntity is an entity the query matched.
type MatchedEntity struct {
	ID   string            `json:"id"`
	Name string            `json:"name"`
	Type domain.EntityType `json:"type"`
}

// RelatedItem describes a relationship from a result to another item.
type RelatedItem struct {
	RelationshipID string                  `json:"relationship_id"`
	ItemID         string                  `json:"item_id"`
	Title          string                  `json:"title"`
	Type           domain.RelationshipType `json:"type"`
	Provenance     domain.Provenance       `json:"provenance"`
	// Strength is the relationship's heuristic strength, NOT query relevance.
	Strength     float64             `json:"relationship_strength"`
	ScoreVersion string              `json:"score_version"`
	ReviewStatus domain.ReviewStatus `json:"review_status"`
	Explanation  []string            `json:"explanation,omitempty"`
	Direction    string              `json:"direction"`
}

// Response is a complete search result set.
type Response struct {
	Query   string   `json:"query"`
	Results []Result `json:"results"`
	Total   int      `json:"total"`
	Limit   int      `json:"limit"`
	Offset  int      `json:"offset"`
	TookMS  int64    `json:"took_ms"`
	// Notes carries advisory messages, such as vector search being unavailable.
	Notes []string `json:"notes,omitempty"`
}

// Service performs searches.
type Service struct {
	items    domain.ItemRepository
	chunks   domain.ChunkRepository
	entities domain.EntityRepository
	rels     domain.RelationshipRepository
	vectors  domain.VectorStore
	embedder domain.Embedder
	cfg      *config.Config
	metrics  *observability.Metrics
}

// NewService builds the search service.
func NewService(items domain.ItemRepository, chunks domain.ChunkRepository,
	ents domain.EntityRepository, rels domain.RelationshipRepository,
	vectors domain.VectorStore, embedder domain.Embedder,
	cfg *config.Config, m *observability.Metrics) *Service {
	return &Service{
		items: items, chunks: chunks, entities: ents, rels: rels,
		vectors: vectors, embedder: embedder, cfg: cfg, metrics: m,
	}
}

// recencyHalfLife controls how gently recency nudges the ordering.
const recencyHalfLife = 180 * 24 * time.Hour

// Search runs the hybrid retrieval pipeline.
func (s *Service) Search(ctx context.Context, p *domain.Principal, q Query) (*Response, error) {
	if p == nil || p.WorkspaceID == "" {
		return nil, domain.ErrWorkspaceMiss
	}
	started := time.Now()

	limit := q.Limit
	if limit <= 0 {
		limit = s.cfg.Search.DefaultLimit
	}
	if limit > s.cfg.Search.MaxResults {
		limit = s.cfg.Search.MaxResults
	}
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}

	resp := &Response{Query: q.Text, Limit: limit, Offset: offset}
	cands := map[string]*ranking.Candidate{}
	get := func(itemID string) *ranking.Candidate {
		c := cands[itemID]
		if c == nil {
			c = &ranking.Candidate{ItemID: itemID}
			cands[itemID] = c
		}
		return c
	}

	itemFilter := domain.ItemFilter{
		SourceTypes: q.SourceTypes,
		MIMETypes:   q.MIMETypes,
		CreatedFrom: q.CreatedFrom,
		CreatedTo:   q.CreatedTo,
		Statuses: []domain.ProcessingStatus{
			domain.StatusReady, domain.StatusPartial,
			domain.StatusAnalysing, domain.StatusRelating, domain.StatusEmbedding,
		},
	}

	trimmed := strings.TrimSpace(q.Text)

	// --- Path 1: lexical item search ---------------------------------------
	if trimmed != "" {
		hits, err := s.items.TextSearch(ctx, p.WorkspaceID, trimmed, itemFilter, s.cfg.Search.LexicalCandidates)
		if err != nil {
			resp.Notes = append(resp.Notes, "Keyword search was unavailable for this query.")
			observability.LoggerFrom(ctx).Warn("lexical item search failed", "error", err.Error())
		}
		for i, h := range hits {
			get(h.ItemID).Add(ranking.SourceLexical, i+1,
				"The item's text contains words from your query.")
		}
	}

	// --- Path 2: lexical chunk search, for excerpts -------------------------
	chunkHits := map[string][]domain.ChunkHit{}
	if trimmed != "" {
		hits, err := s.chunks.TextSearch(ctx, p.WorkspaceID, trimmed, s.cfg.Search.LexicalCandidates)
		if err == nil {
			for i, h := range hits {
				chunkHits[h.ItemID] = append(chunkHits[h.ItemID], h)
				c := get(h.ItemID)
				c.MatchedChunks = appendUnique(c.MatchedChunks, h.ChunkID, 5)
				if i < 20 {
					c.Add(ranking.SourceLexical, i+1,
						"A section of this item contains words from your query.")
				}
			}
		}
	}

	// --- Path 3: vector similarity -----------------------------------------
	vectorSims := map[string]float64{}
	chunkSims := map[string]float64{}
	if trimmed != "" {
		vecs, err := s.embedder.Embed(ctx, []string{trimmed})
		if err != nil || len(vecs) == 0 {
			resp.Notes = append(resp.Notes,
				"Meaning-based search was unavailable, so these results are from keyword matching only.")
			observability.LoggerFrom(ctx).Warn("could not embed the query", "error", errString(err))
		} else {
			matches, err := s.vectors.Search(ctx, vecs[0], s.cfg.Search.VectorCandidates, domain.VectorFilter{
				WorkspaceID:    p.WorkspaceID,
				SourceTypes:    q.SourceTypes,
				EmbeddingModel: s.embedder.ModelID(),
				CreatedFrom:    q.CreatedFrom,
				CreatedTo:      q.CreatedTo,
			})
			if err != nil {
				resp.Notes = append(resp.Notes, "Meaning-based search was unavailable for this query.")
				observability.LoggerFrom(ctx).Warn("vector search failed", "error", err.Error())
			}
			for i, m := range matches {
				c := get(m.ItemID)
				c.Add(ranking.SourceVector, i+1,
					fmt.Sprintf("A section of this item is close in meaning to your query (similarity %.2f).", m.Score))
				if m.Score > c.BestSimilarity {
					c.BestSimilarity = m.Score
				}
				if m.Score > vectorSims[m.ItemID] {
					vectorSims[m.ItemID] = m.Score
				}
				chunkSims[m.ChunkID] = m.Score
				c.MatchedChunks = appendUnique(c.MatchedChunks, m.ChunkID, 5)
			}
		}
	}

	// --- Path 4: entity matching -------------------------------------------
	matchedEntities := map[string][]*domain.Entity{}
	if err := s.matchEntities(ctx, p.WorkspaceID, q, trimmed, get, matchedEntities); err != nil {
		observability.LoggerFrom(ctx).Warn("entity matching failed", "error", err.Error())
	}

	// An empty query with filters is a legitimate browse request.
	if trimmed == "" && len(cands) == 0 {
		items, total, err := s.items.List(ctx, p.WorkspaceID, itemFilter,
			domain.Page{Limit: limit, Offset: offset})
		if err != nil {
			return nil, err
		}
		for i, it := range items {
			c := get(it.ID)
			c.CreatedAt = it.CreatedAt
			c.Add(ranking.SourceLexical, i+1, "Matched your filters.")
		}
		resp.Total = int(total)
	}

	if len(cands) == 0 {
		resp.TookMS = time.Since(started).Milliseconds()
		s.count("hybrid", "empty")
		return resp, nil
	}

	// Load the candidate items, applying the workspace and filter constraints
	// that the vector store cannot express.
	ids := make([]string, 0, len(cands))
	for id := range cands {
		ids = append(ids, id)
	}
	items, err := s.items.GetMany(ctx, p.WorkspaceID, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*domain.Item, len(items))
	for _, it := range items {
		if it.Deleted || !matchesFilter(it, itemFilter, q) {
			delete(cands, it.ID)
			continue
		}
		byID[it.ID] = it
		cands[it.ID].CreatedAt = it.CreatedAt
	}
	// Drop candidates whose items were not returned at all: they are deleted,
	// in another workspace, or otherwise not visible to this principal.
	for id := range cands {
		if byID[id] == nil {
			delete(cands, id)
		}
	}

	// Title matching is cheap and a strong signal a user expects to work.
	if trimmed != "" {
		lower := strings.ToLower(trimmed)
		for id, it := range byID {
			if strings.Contains(strings.ToLower(it.Title), lower) {
				cands[id].AddScaled(ranking.SourceTitle, 1,
					fmt.Sprintf("The title %q contains your query.", it.Title))
			}
		}
	}

	// --- Path 5: relationship expansion ------------------------------------
	expansions := map[string]*RelatedItem{}
	relsByItem := map[string][]*domain.Relationship{}
	if q.Expand {
		s.expand(ctx, p.WorkspaceID, q, cands, byID, get, expansions, relsByItem)

		// Newly-expanded items still need their records.
		var missing []string
		for id := range cands {
			if byID[id] == nil {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			more, err := s.items.GetMany(ctx, p.WorkspaceID, missing)
			if err == nil {
				for _, it := range more {
					if it.Deleted || !matchesFilter(it, itemFilter, q) {
						delete(cands, it.ID)
						continue
					}
					byID[it.ID] = it
					cands[it.ID].CreatedAt = it.CreatedAt
				}
			}
			for _, id := range missing {
				if byID[id] == nil {
					delete(cands, id)
				}
			}
		}
	}

	// --- Fuse and order -----------------------------------------------------
	list := make([]*ranking.Candidate, 0, len(cands))
	for _, c := range cands {
		list = append(list, c)
	}
	ranked := ranking.Rank(list, time.Now(), recencyHalfLife)

	if resp.Total == 0 {
		resp.Total = len(ranked)
	}

	// Apply paging after ranking, so a page boundary does not change ordering.
	start := offset
	if start > len(ranked) {
		start = len(ranked)
	}
	end := start + limit
	if end > len(ranked) {
		end = len(ranked)
	}
	page := ranked[start:end]

	// Fetch the chunks needed for excerpts, in one query.
	var wanted []string
	for _, c := range page {
		wanted = append(wanted, c.MatchedChunks...)
	}
	chunkByID := map[string]*domain.Chunk{}
	if len(wanted) > 0 {
		got, err := s.chunks.GetMany(ctx, p.WorkspaceID, wanted)
		if err == nil {
			for _, ch := range got {
				chunkByID[ch.ID] = ch
			}
		}
	}

	for _, c := range page {
		it := byID[c.ItemID]
		if it == nil {
			continue
		}
		r := Result{
			Item:               it,
			QueryRelevance:     c.Relevance,
			SemanticSimilarity: c.BestSimilarity,
			WhyMatched:         ranking.Explain(c),
			Contributions:      c.Contributions,
			RankVersion:        ranking.RankVersion,
		}

		for _, cid := range c.MatchedChunks {
			ch := chunkByID[cid]
			if ch == nil {
				continue
			}
			r.MatchedChunks = append(r.MatchedChunks, MatchedChunk{
				ChunkID:    ch.ID,
				Sequence:   ch.Sequence,
				Text:       ch.Text,
				Similarity: chunkSims[ch.ID],
				Page:       ch.Page,
				Section:    ch.Section,
			})
		}
		sort.SliceStable(r.MatchedChunks, func(i, j int) bool {
			return r.MatchedChunks[i].Similarity > r.MatchedChunks[j].Similarity
		})

		r.Excerpt = buildExcerpt(it, r.MatchedChunks, chunkHits[it.ID], trimmed)

		for _, e := range matchedEntities[it.ID] {
			r.MatchedEntities = append(r.MatchedEntities, MatchedEntity{
				ID: e.ID, Name: e.CanonicalName, Type: e.Type,
			})
		}

		if via, ok := expansions[it.ID]; ok {
			r.ViaRelationship = via
		}
		r.Related = s.relatedFor(it.ID, relsByItem[it.ID], byID)

		resp.Results = append(resp.Results, r)
	}

	resp.TookMS = time.Since(started).Milliseconds()
	s.count("hybrid", "success")
	if s.metrics != nil {
		s.metrics.SearchLatency.Observe(time.Since(started).Seconds())
	}
	return resp, nil
}

func (s *Service) count(mode, outcome string) {
	if s.metrics != nil {
		s.metrics.SearchOps.WithLabelValues(mode, outcome).Inc()
	}
}

// matchEntities finds entities the query names and adds the items citing them.
func (s *Service) matchEntities(ctx context.Context, workspaceID string, q Query, text string,
	get func(string) *ranking.Candidate, out map[string][]*domain.Entity) error {

	var found []*domain.Entity

	// Explicit entity filters are authoritative.
	if len(q.EntityIDs) > 0 {
		ents, err := s.entities.GetMany(ctx, workspaceID, q.EntityIDs)
		if err != nil {
			return err
		}
		found = append(found, ents...)
	}

	// Otherwise look for entity names appearing in the query text.
	if text != "" {
		keys := candidateEntityKeys(text)
		if len(keys) > 0 {
			ents, err := s.entities.FindByNormalised(ctx, workspaceID, keys)
			if err != nil {
				return err
			}
			found = append(found, ents...)
		}
	}

	seen := map[string]bool{}
	for _, e := range found {
		if seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		for _, itemID := range e.ItemIDs {
			c := get(itemID)
			c.AddScaled(ranking.SourceEntity, 1,
				fmt.Sprintf("This item refers to %s (%s), which your query names.", e.CanonicalName, e.Type))
			c.MatchedEntities = appendUnique(c.MatchedEntities, e.ID, 8)
			out[itemID] = append(out[itemID], e)
		}
	}
	return nil
}

// candidateEntityKeys generates normalised keys for word sequences in the
// query, so "Example Ltd" matches the stored organisation.
//
// Only short n-grams are tried, and each becomes a value in an indexed $in
// lookup — never a pattern or an operator.
func candidateEntityKeys(text string) []string {
	words := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\'' && r != '&'
	})
	if len(words) == 0 || len(words) > 40 {
		return nil
	}

	var keys []string
	seen := map[string]bool{}
	addKey := func(phrase string) {
		norm := entities.Normalise(phrase)
		if norm == "" || len(norm) < 3 {
			return
		}
		for _, t := range domain.ValidEntityTypes {
			k := string(t) + ":" + norm
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}

	// Unigrams through trigrams.
	for n := 1; n <= 3; n++ {
		for i := 0; i+n <= len(words); i++ {
			if len(keys) > 600 {
				return keys
			}
			addKey(strings.Join(words[i:i+n], " "))
		}
	}
	return keys
}

// expand pulls in items connected to the current matches by a relationship.
//
// Expanded items are labelled with the relationship that brought them in, and
// their contribution is weighted well below a direct match: being related to a
// hit is weaker evidence than being one. Rejected relationships never expand.
func (s *Service) expand(ctx context.Context, workspaceID string, q Query,
	cands map[string]*ranking.Candidate, byID map[string]*domain.Item,
	get func(string) *ranking.Candidate, expansions map[string]*RelatedItem,
	relsByItem map[string][]*domain.Relationship) {

	// Expand only from the strongest current matches.
	seeds := make([]*ranking.Candidate, 0, len(cands))
	for _, c := range cands {
		seeds = append(seeds, c)
	}
	sort.SliceStable(seeds, func(i, j int) bool {
		return len(seeds[i].Contributions) > len(seeds[j].Contributions)
	})
	max := s.cfg.Search.MaxExpandedItems
	if max <= 0 {
		max = 20
	}
	if len(seeds) > max {
		seeds = seeds[:max]
	}

	seedIDs := make([]string, 0, len(seeds))
	for _, c := range seeds {
		seedIDs = append(seedIDs, c.ItemID)
	}

	minScore := q.MinRelStrength
	if minScore <= 0 {
		minScore = s.cfg.Search.ExpansionMinScore
	}

	rels, err := s.rels.ListForItems(ctx, workspaceID, seedIDs, minScore)
	if err != nil {
		observability.LoggerFrom(ctx).Warn("relationship expansion failed", "error", err.Error())
		return
	}

	isSeed := map[string]bool{}
	for _, id := range seedIDs {
		isSeed[id] = true
	}

	added := 0
	for _, rel := range rels {
		if q.ConfirmedOnly && rel.ReviewStatus != domain.ReviewConfirmed {
			continue
		}
		if len(q.RelTypes) > 0 && !containsRelType(q.RelTypes, rel.Type) {
			continue
		}

		relsByItem[rel.SourceID] = append(relsByItem[rel.SourceID], rel)
		relsByItem[rel.TargetID] = append(relsByItem[rel.TargetID], rel)

		// Identify the far end of the relationship.
		var otherID, direction string
		switch {
		case isSeed[rel.SourceID] && !isSeed[rel.TargetID]:
			otherID, direction = rel.TargetID, "outgoing"
		case isSeed[rel.TargetID] && !isSeed[rel.SourceID]:
			otherID, direction = rel.SourceID, "incoming"
		default:
			continue // both ends already matched, or neither did
		}
		if added >= max {
			break
		}
		added++

		c := get(otherID)
		c.ExpandedFrom = rel.SourceID
		label := "related to"
		if rel.ReviewStatus == domain.ReviewConfirmed {
			label = "confirmed as related to"
			c.AddScaled(ranking.SourceConfirmedRel, 1,
				"A person has confirmed this item's relationship to a matching item.")
		}
		c.AddScaled(ranking.SourceExpansion, rel.Score, fmt.Sprintf(
			"Not a direct match: this item is %s a matching item by a %s relationship (strength %.2f, %s).",
			label, rel.Type, rel.Score, provenanceLabel(rel.Provenance)))

		expansions[otherID] = &RelatedItem{
			RelationshipID: rel.ID,
			ItemID:         otherID,
			Type:           rel.Type,
			Provenance:     rel.Provenance,
			Strength:       rel.Score,
			ScoreVersion:   rel.ScoreVersion,
			ReviewStatus:   rel.ReviewStatus,
			Direction:      direction,
			Explanation:    relationships.Explain(rel),
		}
	}
}

// relatedFor renders an item's relationships for display.
func (s *Service) relatedFor(itemID string, rels []*domain.Relationship, byID map[string]*domain.Item) []RelatedItem {
	if len(rels) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]RelatedItem, 0, len(rels))

	for _, rel := range rels {
		if seen[rel.ID] {
			continue
		}
		seen[rel.ID] = true

		otherID, direction := rel.TargetID, "outgoing"
		if rel.TargetID == itemID {
			otherID, direction = rel.SourceID, "incoming"
		}
		title := ""
		if it := byID[otherID]; it != nil {
			title = it.Title
		}
		out = append(out, RelatedItem{
			RelationshipID: rel.ID,
			ItemID:         otherID,
			Title:          title,
			Type:           rel.Type,
			Provenance:     rel.Provenance,
			Strength:       rel.Score,
			ScoreVersion:   rel.ScoreVersion,
			ReviewStatus:   rel.ReviewStatus,
			Direction:      direction,
			Explanation:    relationships.Explain(rel),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Strength > out[j].Strength })
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

// buildExcerpt picks the most useful snippet to show for a result.
func buildExcerpt(item *domain.Item, matched []MatchedChunk, lexical []domain.ChunkHit, query string) string {
	const width = 320

	// Prefer a chunk that lexically contains a query term, so the excerpt shows
	// the user their own words where possible.
	if query != "" {
		terms := strings.Fields(strings.ToLower(query))
		for _, mc := range matched {
			if snippet := findTermWindow(mc.Text, terms, width); snippet != "" {
				return snippet
			}
		}
		for _, lh := range lexical {
			if snippet := findTermWindow(lh.Text, terms, width); snippet != "" {
				return snippet
			}
		}
	}
	if len(matched) > 0 {
		return truncateWords(matched[0].Text, width)
	}
	if len(lexical) > 0 {
		return truncateWords(lexical[0].Text, width)
	}
	return truncateWords(item.ExtractedText, width)
}

// findTermWindow returns a window of text around the first matching term.
func findTermWindow(text string, terms []string, width int) string {
	lower := strings.ToLower(text)
	best := -1
	for _, t := range terms {
		if len(t) < 3 {
			continue
		}
		if i := strings.Index(lower, t); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	if best < 0 {
		return ""
	}
	start := best - width/3
	if start < 0 {
		start = 0
	}
	for start > 0 && text[start]&0xC0 == 0x80 {
		start--
	}
	end := start + width
	if end > len(text) {
		end = len(text)
	}
	for end < len(text) && text[end]&0xC0 == 0x80 {
		end++
	}
	out := strings.TrimSpace(collapse(text[start:end]))
	if start > 0 {
		out = "…" + out
	}
	if end < len(text) {
		out += "…"
	}
	return out
}

func truncateWords(s string, n int) string {
	s = collapse(strings.TrimSpace(s))
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	if i := strings.LastIndexByte(s[:cut], ' '); i > n/2 {
		cut = i
	}
	return s[:cut] + "…"
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// matchesFilter applies the filters the storage query could not express.
func matchesFilter(it *domain.Item, f domain.ItemFilter, q Query) bool {
	if len(f.SourceTypes) > 0 && !containsSourceType(f.SourceTypes, it.SourceType) {
		return false
	}
	if len(f.MIMETypes) > 0 && !containsString(f.MIMETypes, it.MIMEType) {
		return false
	}
	if f.CreatedFrom != nil && it.CreatedAt.Before(*f.CreatedFrom) {
		return false
	}
	if f.CreatedTo != nil && it.CreatedAt.After(*f.CreatedTo) {
		return false
	}
	return true
}

func containsSourceType(list []domain.SourceType, v domain.SourceType) bool {
	for _, e := range list {
		if e == v {
			return true
		}
	}
	return false
}

func containsString(list []string, v string) bool {
	for _, e := range list {
		if e == v {
			return true
		}
	}
	return false
}

func containsRelType(list []domain.RelationshipType, v domain.RelationshipType) bool {
	for _, e := range list {
		if e == v {
			return true
		}
	}
	return false
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

func provenanceLabel(p domain.Provenance) string {
	switch p {
	case domain.ProvExplicit:
		return "stated explicitly"
	case domain.ProvDeterministic:
		return "derived from matching evidence"
	case domain.ProvModelInferred:
		return "suggested by analysis, not yet reviewed"
	case domain.ProvUserConfirmed:
		return "confirmed by a person"
	case domain.ProvUserRejected:
		return "rejected by a person"
	}
	return string(p)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
