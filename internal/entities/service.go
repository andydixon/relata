package entities

import (
	"context"
	"strings"
	"time"

	"github.com/andydixon/relata/internal/analysis"
	"github.com/andydixon/relata/internal/config"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/ids"
	"github.com/andydixon/relata/internal/jobs"
	"github.com/andydixon/relata/internal/observability"
)

// Service extracts entities and claims from an item's chunks and stores them.
//
// Extraction runs in two stages, as separate and separately-labelled sources of
// evidence: deterministic rules first, then model-assisted extraction for the
// broader concepts rules cannot reach. Every stored record carries the method
// that produced it, so a user can tell a matched pattern from a model's guess.
type Service struct {
	entities domain.EntityRepository
	aliases  domain.AliasRepository
	claims   domain.ClaimRepository
	analysis *analysis.Service
	cfg      *config.Config
}

// NewService builds the extraction service.
func NewService(ents domain.EntityRepository, aliases domain.AliasRepository,
	claims domain.ClaimRepository, an *analysis.Service, cfg *config.Config) *Service {
	return &Service{entities: ents, aliases: aliases, claims: claims, analysis: an, cfg: cfg}
}

// maxModelChunksPerItem bounds how many chunks are sent to the model for
// extraction, so a large document cannot consume unlimited model time.
const maxModelChunksPerItem = 12

// observation accumulates everything seen about one entity within an item.
type observation struct {
	names   []string
	typ     domain.EntityType
	refs    []domain.SourceRef
	aliases map[string]bool
	methods map[domain.ExtractionMethod]bool
	count   int
}

// ExtractForItem extracts entities from an item's chunks and stores them.
//
// Returns the number of distinct entities recorded. Model failures are returned
// to the caller, which treats them as degraded quality rather than a pipeline
// failure: the deterministic results are already persisted by then.
func (s *Service) ExtractForItem(ctx context.Context, item *domain.Item, chunks []*domain.Chunk, pool *jobs.Pool) (int, error) {
	log := observability.LoggerFrom(ctx)
	obs := map[string]*observation{}

	record := func(name string, typ domain.EntityType, ref domain.SourceRef, method domain.ExtractionMethod, aliases []string) {
		name = strings.TrimSpace(name)
		if name == "" || !typ.Valid() {
			return
		}
		key := Key(typ, name)
		if Normalise(name) == "" {
			return
		}
		o := obs[key]
		if o == nil {
			o = &observation{
				typ:     typ,
				aliases: map[string]bool{},
				methods: map[domain.ExtractionMethod]bool{},
			}
			obs[key] = o
		}
		o.names = append(o.names, name)
		o.methods[method] = true
		o.count++
		// Source references are bounded per entity per item: a name repeated a
		// hundred times does not need a hundred stored references.
		if len(o.refs) < 8 {
			o.refs = append(o.refs, ref)
		}
		for _, a := range aliases {
			if a != "" && !strings.EqualFold(a, name) {
				o.aliases[a] = true
			}
		}
	}

	// Stage 1: deterministic extraction over every chunk.
	for _, c := range chunks {
		for _, m := range ExtractDeterministic(c.Text) {
			record(m.Name, m.Type, domain.SourceRef{
				ItemID:  item.ID,
				ChunkID: c.ID,
				// Offsets are translated from chunk-relative to item-relative,
				// so a stored reference addresses the item's extracted text.
				Start: c.StartOffset + m.Start,
				End:   c.StartOffset + m.End,
				Quote: m.Quote,
				Page:  c.Page,
			}, domain.MethodDeterministic, nil)
		}
	}

	// Stage 2: model-assisted extraction over a bounded sample of chunks.
	var modelErr error
	if s.analysis.Enabled() {
		limit := len(chunks)
		if limit > maxModelChunksPerItem {
			limit = maxModelChunksPerItem
		}
		for _, c := range chunks[:limit] {
			if err := ctx.Err(); err != nil {
				modelErr = err
				break
			}
			release, err := pool.AcquireAI(ctx)
			if err != nil {
				modelErr = err
				break
			}
			cands, err := s.analysis.ExtractEntities(ctx, c.Text)
			release()
			if err != nil {
				modelErr = err
				log.Debug("model entity extraction failed for a chunk", "error", err.Error())
				continue
			}
			for _, cand := range cands {
				ref := domain.SourceRef{ItemID: item.ID, ChunkID: c.ID, Page: c.Page}
				// Locate the model's quotation in the chunk so the reference
				// points at real text. Unlocatable evidence yields a reference
				// to the chunk rather than a fabricated offset.
				if cand.Evidence != "" {
					if idx := strings.Index(c.Text, cand.Evidence); idx >= 0 {
						ref.Start = c.StartOffset + idx
						ref.End = ref.Start + len(cand.Evidence)
						ref.Quote = cand.Evidence
					}
				}
				if ref.Quote == "" {
					if idx := strings.Index(c.Text, cand.Name); idx >= 0 {
						ref.Start = c.StartOffset + idx
						ref.End = ref.Start + len(cand.Name)
						ref.Quote = cand.Name
					}
				}
				record(cand.Name, cand.Type, ref, domain.MethodModel, cand.Aliases)
			}
		}
	} else {
		modelErr = analysis.ErrDisabled
	}

	stored := s.persist(ctx, item, obs)
	return stored, modelErr
}

// persist writes the accumulated observations and proposes aliases.
func (s *Service) persist(ctx context.Context, item *domain.Item, obs map[string]*observation) int {
	log := observability.LoggerFrom(ctx)
	stored := 0
	saved := make([]*domain.Entity, 0, len(obs))

	for key, o := range obs {
		canonical := Canonicalise(o.names)
		if canonical == "" {
			continue
		}
		method := domain.MethodModel
		if o.methods[domain.MethodDeterministic] {
			// A rule matched, which is the stronger provenance.
			method = domain.MethodDeterministic
		}

		aliasList := make([]string, 0, len(o.aliases))
		for a := range o.aliases {
			aliasList = append(aliasList, a)
		}

		e := &domain.Entity{
			ID:            ids.New(),
			WorkspaceID:   item.WorkspaceID,
			CanonicalName: canonical,
			NormalisedKey: key,
			Type:          o.typ,
			Aliases:       aliasList,
			SourceRefs:    o.refs,
			ItemIDs:       []string{item.ID},
			Method:        method,
			Mentions:      o.count,
			EvidenceScore: evidenceScore(o),
		}
		if method == domain.MethodModel {
			e.ModelVersion = s.analysis.ModelID()
		}

		got, err := s.entities.UpsertByKey(ctx, e)
		if err != nil {
			log.Warn("could not store an entity", "entity", canonical, "error", err.Error())
			continue
		}
		stored++
		saved = append(saved, got)
	}

	s.proposeAliases(ctx, item.WorkspaceID, saved)
	return stored
}

// evidenceScore reflects how well-attested an entity is. It is not a
// probability: it says "several independent signals point at this", nothing
// about whether the entity is real.
func evidenceScore(o *observation) float64 {
	score := 0.3
	if o.methods[domain.MethodDeterministic] {
		score += 0.4
	}
	if o.methods[domain.MethodModel] && o.methods[domain.MethodDeterministic] {
		// Both stages agreeing is the strongest evidence available here.
		score += 0.2
	}
	switch {
	case o.count >= 5:
		score += 0.2
	case o.count >= 2:
		score += 0.1
	}
	if score > 1 {
		score = 1
	}
	return score
}

// aliasProposalThreshold is the similarity at which a name pair is worth a
// human's attention.
const aliasProposalThreshold = 0.82

// maxAliasComparisons bounds the pairwise comparison so a workspace with many
// entities does not turn alias proposal into an O(n²) sweep.
const maxAliasComparisons = 400

// proposeAliases suggests that two entities may be the same thing.
//
// This is the safety valve for conservative normalisation: "BBC" and "British
// Broadcasting Corporation" get separate keys, and rather than merging them —
// which would be a guess with real consequences — Relata records a candidate
// alias and waits for a person to decide.
func (s *Service) proposeAliases(ctx context.Context, workspaceID string, saved []*domain.Entity) {
	if len(saved) == 0 {
		return
	}
	log := observability.LoggerFrom(ctx)

	// Compare each newly-seen entity against existing entities that share its
	// loose key — the cheap indexed way to find plausible pairs.
	comparisons := 0
	for _, e := range saved {
		if comparisons >= maxAliasComparisons {
			return
		}
		if e.Type == domain.EntityDate || e.Type == domain.EntityConcept {
			// Dates are already normalised, and concepts overlap by nature.
			continue
		}

		candidates, _, err := s.entities.List(ctx, workspaceID, domain.EntityFilter{
			Types: []domain.EntityType{e.Type},
		}, domain.Page{Limit: 200})
		if err != nil {
			return
		}

		for _, other := range candidates {
			if other.ID == e.ID {
				continue
			}
			comparisons++
			if comparisons >= maxAliasComparisons {
				break
			}
			sim := AliasSimilarity(e.CanonicalName, other.CanonicalName)
			if sim < aliasProposalThreshold {
				continue
			}
			// Record the proposal in a stable direction so the same pair does
			// not generate two mirror-image proposals.
			from, to := e, other
			if from.ID > to.ID {
				from, to = to, from
			}
			err := s.aliases.Upsert(ctx, &domain.EntityAlias{
				ID:                ids.New(),
				WorkspaceID:       workspaceID,
				EntityID:          from.ID,
				Alias:             to.CanonicalName,
				Normalised:        Normalise(to.CanonicalName),
				CandidateEntityID: to.ID,
				Status:            domain.AliasCandidate,
				Method:            domain.MethodDeterministic,
				Similarity:        sim,
				Reason:            "These names are similar. Relata has NOT merged them: confirm only if they refer to the same thing.",
			})
			if err != nil {
				log.Debug("could not record an alias proposal", "error", err.Error())
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Claims
// ---------------------------------------------------------------------------

// ExtractClaimsForItem extracts the statements an item makes.
//
// Deterministic extraction picks out sentences carrying a figure or a date —
// the statements most likely to be compared, agreed with or contradicted.
// Model-assisted extraction adds the rest. Either way, a claim is stored only
// when its quotation can be located in the source text.
func (s *Service) ExtractClaimsForItem(ctx context.Context, item *domain.Item, chunks []*domain.Chunk, pool *jobs.Pool) (int, error) {
	log := observability.LoggerFrom(ctx)
	now := time.Now().UTC()

	var out []*domain.Claim
	seen := map[string]bool{}

	add := func(c *domain.Claim) {
		if c.DedupeKey == "" || seen[c.DedupeKey] {
			return
		}
		seen[c.DedupeKey] = true
		out = append(out, c)
	}

	// Stage 1: deterministic claims.
	for _, ch := range chunks {
		for _, dc := range ExtractDeterministicClaims(ch.Text) {
			add(&domain.Claim{
				ID:          ids.New(),
				WorkspaceID: item.WorkspaceID,
				ItemID:      item.ID,
				ChunkID:     ch.ID,
				Statement:   dc.Statement,
				SourceRef: domain.SourceRef{
					ItemID:  item.ID,
					ChunkID: ch.ID,
					Start:   ch.StartOffset + dc.Start,
					End:     ch.StartOffset + dc.End,
					Quote:   dc.Quote,
					Page:    ch.Page,
				},
				Nature:     domain.ClaimQuoted,
				Subject:    dc.Subject,
				Predicate:  dc.Predicate,
				Numbers:    dc.Numbers,
				Dates:      dc.Dates,
				Polarity:   dc.Polarity,
				Method:     domain.MethodDeterministic,
				Confidence: 0.7,
				DedupeKey:  ids.Deterministic(item.WorkspaceID, item.ID, "det", dc.Quote),
				CreatedAt:  now,
			})
		}
	}

	// Stage 2: model-assisted claims.
	var modelErr error
	if s.analysis.Enabled() {
		limit := len(chunks)
		if limit > maxModelChunksPerItem {
			limit = maxModelChunksPerItem
		}
		for _, ch := range chunks[:limit] {
			if err := ctx.Err(); err != nil {
				modelErr = err
				break
			}
			release, err := pool.AcquireAI(ctx)
			if err != nil {
				modelErr = err
				break
			}
			cands, err := s.analysis.ExtractClaims(ctx, ch.Text)
			release()
			if err != nil {
				modelErr = err
				log.Debug("model claim extraction failed for a chunk", "error", err.Error())
				continue
			}
			for _, cand := range cands {
				// The analysis layer has already verified the quotation exists
				// and returned where it is.
				if cand.QuoteOffset < 0 {
					continue
				}
				subj, pred, nums, dates, pol := AnalyseStatement(cand.Statement)
				if cand.Subject != "" {
					subj = Normalise(cand.Subject)
				}
				add(&domain.Claim{
					ID:          ids.New(),
					WorkspaceID: item.WorkspaceID,
					ItemID:      item.ID,
					ChunkID:     ch.ID,
					Statement:   cand.Statement,
					SourceRef: domain.SourceRef{
						ItemID:  item.ID,
						ChunkID: ch.ID,
						Start:   ch.StartOffset + cand.QuoteOffset,
						End:     ch.StartOffset + cand.QuoteOffset + len(cand.Quote),
						Quote:   cand.Quote,
						Page:    ch.Page,
					},
					Attribution:  cand.Attribution,
					Nature:       cand.Nature,
					Subject:      subj,
					Predicate:    pred,
					Numbers:      nums,
					Dates:        dates,
					Polarity:     pol,
					Method:       domain.MethodModel,
					ModelVersion: s.analysis.ModelID(),
					Confidence:   0.6,
					DedupeKey:    ids.Deterministic(item.WorkspaceID, item.ID, "model", cand.Quote),
					CreatedAt:    now,
				})
			}
		}
	} else {
		modelErr = analysis.ErrDisabled
	}

	if err := s.claims.ReplaceForItem(ctx, item.WorkspaceID, item.ID, out); err != nil {
		return 0, err
	}
	return len(out), modelErr
}
