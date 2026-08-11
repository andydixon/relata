// Package ranking fuses the several retrieval signals a search produces into a
// single ordered result list, and records why each result ranked where it did.
//
// The output is a QUERY RELEVANCE score. It is deliberately a different number
// from semantic similarity (how alike two texts are) and from relationship
// strength (how well evidenced a link between two items is). Conflating the
// three would make every explanation meaningless, so they are kept apart all
// the way to the interface.
package ranking

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Source names a retrieval path that contributed to a result.
type Source string

const (
	SourceLexical      Source = "lexical"
	SourceVector       Source = "vector"
	SourceEntity       Source = "entity"
	SourceExpansion    Source = "relationship"
	SourceRecency      Source = "recency"
	SourceConfirmedRel Source = "confirmed_relationship"
	SourceTitle        Source = "title"
)

// Weights control how much each retrieval path contributes to query relevance.
var Weights = map[Source]float64{
	SourceLexical:      1.00,
	SourceVector:       1.00,
	SourceEntity:       0.60,
	SourceTitle:        0.50,
	SourceExpansion:    0.35,
	SourceRecency:      0.15,
	SourceConfirmedRel: 0.25,
}

// RankVersion identifies the fusion model, so a change is visible in results.
const RankVersion = "query-rank-v1"

// rrfK is the reciprocal-rank-fusion constant.
//
// Fusion is by rank rather than by raw score because MongoDB text scores and
// cosine similarities are not on comparable scales: adding them directly would
// let whichever happened to have the larger range dominate. Rank position is
// comparable by construction.
const rrfK = 60.0

// Contribution is one retrieval path's effect on a result's rank.
type Contribution struct {
	Source Source `json:"source"`
	// Rank is this result's position in that path's own ordering, 1-based.
	// Zero means the path contributed without an ordering of its own.
	Rank   int     `json:"rank,omitempty"`
	Weight float64 `json:"weight"`
	Score  float64 `json:"score"`
	// Detail is the human-readable reason, shown as "why this matched".
	Detail string `json:"detail"`
}

// Candidate is one item under consideration, with everything known about it.
type Candidate struct {
	ItemID        string
	Contributions []Contribution
	// Relevance is the fused query relevance, filled in by Rank.
	Relevance float64
	// BestSimilarity is the highest cosine similarity of any matching chunk.
	// It is reported separately and never folded into Relevance.
	BestSimilarity  float64
	MatchedChunks   []string
	MatchedEntities []string
	// ExpandedFrom names the item that pulled this one in by relationship.
	ExpandedFrom string
	CreatedAt    time.Time
}

// Add records a contribution from one retrieval path.
func (c *Candidate) Add(src Source, rank int, detail string) {
	weight := Weights[src]
	score := weight
	if rank > 0 {
		score = weight * (1.0 / (rrfK + float64(rank)))
	}
	c.Contributions = append(c.Contributions, Contribution{
		Source: src, Rank: rank, Weight: weight, Score: score, Detail: detail,
	})
}

// weakestEffectiveRank is the rank a zero-strength scaled contribution maps to.
const weakestEffectiveRank = 50

// AddScaled records a contribution whose strength is already known in 0..1,
// used for paths without a meaningful ordering of their own.
//
// The strength is converted to an *effective rank* so that scaled and ranked
// contributions sit on the same 1/(k+rank) curve, differing only by weight.
// Scoring them on a separate curve made them incomparable: a full-strength
// expansion could outrank a direct keyword hit, which would mean "related to a
// match" beating "is a match".
func (c *Candidate) AddScaled(src Source, strength float64, detail string) {
	weight := Weights[src]
	if strength < 0 {
		strength = 0
	}
	if strength > 1 {
		strength = 1
	}
	// A path reporting zero strength contributes nothing, but is still recorded
	// so the explanation shows that it ran and found nothing.
	score := 0.0
	if strength > 0 {
		effectiveRank := 1 + (1-strength)*(weakestEffectiveRank-1)
		score = weight * (1.0 / (rrfK + effectiveRank))
	}

	c.Contributions = append(c.Contributions, Contribution{
		Source: src, Weight: weight, Score: score, Detail: detail,
	})
}

// Rank fuses contributions and returns candidates ordered by query relevance.
func Rank(cands []*Candidate, now time.Time, recencyHalfLife time.Duration) []*Candidate {
	for _, c := range cands {
		total := 0.0
		for _, contrib := range c.Contributions {
			total += contrib.Score
		}

		// Recency is a gentle tie-breaker, not a ranking principle: a highly
		// relevant old document must still outrank a barely-relevant new one.
		if recencyHalfLife > 0 && !c.CreatedAt.IsZero() {
			age := now.Sub(c.CreatedAt)
			if age > 0 {
				decay := math.Exp(-math.Ln2 * float64(age) / float64(recencyHalfLife))
				if decay > 0.01 {
					// On the same curve as every other contribution.
					effectiveRank := 1 + (1-decay)*(weakestEffectiveRank-1)
					w := Weights[SourceRecency] * (1.0 / (rrfK + effectiveRank))
					c.Contributions = append(c.Contributions, Contribution{
						Source: SourceRecency, Weight: Weights[SourceRecency], Score: w,
						Detail: fmt.Sprintf("Created %s ago.", humanise(age)),
					})
					total += w
				}
			}
		}
		c.Relevance = total
	}

	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Relevance != cands[j].Relevance {
			return cands[i].Relevance > cands[j].Relevance
		}
		// A stable, meaningful tie-break keeps result order reproducible.
		if cands[i].BestSimilarity != cands[j].BestSimilarity {
			return cands[i].BestSimilarity > cands[j].BestSimilarity
		}
		return cands[i].ItemID < cands[j].ItemID
	})

	// Normalise to 0..1 against the top result so the displayed figure is
	// interpretable. It remains a relative ranking score, not a probability.
	if len(cands) > 0 && cands[0].Relevance > 0 {
		top := cands[0].Relevance
		for _, c := range cands {
			c.Relevance = c.Relevance / top
		}
	}
	return cands
}

// Explain renders a candidate's contributions as ordered plain-English lines.
func Explain(c *Candidate) []string {
	if c == nil {
		return nil
	}
	contribs := append([]Contribution(nil), c.Contributions...)
	sort.SliceStable(contribs, func(i, j int) bool { return contribs[i].Score > contribs[j].Score })

	out := make([]string, 0, len(contribs))
	for _, ct := range contribs {
		out = append(out, ct.Detail)
	}
	return out
}

func humanise(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d < 730*24*time.Hour:
		return fmt.Sprintf("%d months", int(d.Hours()/24/30))
	default:
		return fmt.Sprintf("%d years", int(d.Hours()/24/365))
	}
}
