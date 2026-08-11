package ranking

import (
	"strings"
	"testing"
	"time"
)

func newCandidate(id string) *Candidate {
	return &Candidate{ItemID: id, CreatedAt: time.Now()}
}

func TestHigherRankedHitsContributeMore(t *testing.T) {
	first := newCandidate("a")
	first.Add(SourceLexical, 1, "ranked first")

	tenth := newCandidate("b")
	tenth.Add(SourceLexical, 10, "ranked tenth")

	ranked := Rank([]*Candidate{tenth, first}, time.Now(), 0)
	if ranked[0].ItemID != "a" {
		t.Fatalf("the better-ranked hit did not come first: %v", ids(ranked))
	}
}

// Fusion must be by rank, not raw score: MongoDB text scores and cosine
// similarities are not on comparable scales, so adding them directly would let
// whichever has the larger range dominate.
func TestAgreementBetweenPathsOutranksASinglePath(t *testing.T) {
	both := newCandidate("both")
	both.Add(SourceLexical, 3, "keyword match")
	both.Add(SourceVector, 3, "meaning match")

	one := newCandidate("one")
	one.Add(SourceVector, 1, "meaning match only")

	ranked := Rank([]*Candidate{one, both}, time.Now(), 0)
	if ranked[0].ItemID != "both" {
		t.Fatalf("a result found by two paths did not outrank one found by a single path: %v", ids(ranked))
	}
}

func TestRelevanceIsNormalisedAgainstTheBestResult(t *testing.T) {
	a := newCandidate("a")
	a.Add(SourceLexical, 1, "x")
	b := newCandidate("b")
	b.Add(SourceLexical, 50, "y")

	ranked := Rank([]*Candidate{a, b}, time.Now(), 0)
	if ranked[0].Relevance != 1.0 {
		t.Fatalf("top result relevance = %v, want 1.00", ranked[0].Relevance)
	}
	for _, c := range ranked {
		if c.Relevance < 0 || c.Relevance > 1 {
			t.Fatalf("relevance %v is outside 0..1", c.Relevance)
		}
	}
	if ranked[1].Relevance >= ranked[0].Relevance {
		t.Fatal("normalisation flattened the ordering")
	}
}

// Recency is a tie-breaker, not a ranking principle: a highly relevant old
// document must still beat a barely-relevant new one.
func TestRecencyDoesNotOverwhelmRelevance(t *testing.T) {
	old := newCandidate("old-but-relevant")
	old.Add(SourceLexical, 1, "strong match")
	old.Add(SourceVector, 1, "strong match")
	old.CreatedAt = time.Now().AddDate(-3, 0, 0)

	fresh := newCandidate("new-but-weak")
	fresh.Add(SourceLexical, 45, "weak match")
	fresh.CreatedAt = time.Now()

	ranked := Rank([]*Candidate{fresh, old}, time.Now(), 180*24*time.Hour)
	if ranked[0].ItemID != "old-but-relevant" {
		t.Fatalf("recency outranked relevance: %v", ids(ranked))
	}
}

func TestRecencyBreaksTiesBetweenEqualMatches(t *testing.T) {
	older := newCandidate("older")
	older.Add(SourceLexical, 5, "match")
	older.CreatedAt = time.Now().AddDate(0, -6, 0)

	newer := newCandidate("newer")
	newer.Add(SourceLexical, 5, "match")
	newer.CreatedAt = time.Now()

	ranked := Rank([]*Candidate{older, newer}, time.Now(), 180*24*time.Hour)
	if ranked[0].ItemID != "newer" {
		t.Fatalf("recency did not break the tie: %v", ids(ranked))
	}
}

// Relationship expansion is weaker evidence than a direct match: being related
// to a hit is not the same as being one.
func TestExpansionRanksBelowADirectMatch(t *testing.T) {
	direct := newCandidate("direct")
	direct.Add(SourceLexical, 20, "direct keyword match")

	expanded := newCandidate("expanded")
	expanded.AddScaled(SourceExpansion, 1.0, "reached through a relationship")

	ranked := Rank([]*Candidate{expanded, direct}, time.Now(), 0)
	if ranked[0].ItemID != "direct" {
		t.Fatalf("an expanded result outranked a direct match: %v", ids(ranked))
	}
}

func TestConfirmedRelationshipsAddWeight(t *testing.T) {
	plain := newCandidate("plain")
	plain.AddScaled(SourceExpansion, 0.6, "inferred link")

	confirmed := newCandidate("confirmed")
	confirmed.AddScaled(SourceExpansion, 0.6, "inferred link")
	confirmed.AddScaled(SourceConfirmedRel, 1.0, "a person confirmed this link")

	ranked := Rank([]*Candidate{plain, confirmed}, time.Now(), 0)
	if ranked[0].ItemID != "confirmed" {
		t.Fatalf("a human-confirmed link did not outrank an unreviewed one: %v", ids(ranked))
	}
}

func TestOrderingIsStableAndReproducible(t *testing.T) {
	build := func() []*Candidate {
		var out []*Candidate
		for _, id := range []string{"c", "a", "b"} {
			c := newCandidate(id)
			c.Add(SourceLexical, 5, "identical match") // deliberately tied
			c.CreatedAt = time.Time{}                  // no recency contribution
			out = append(out, c)
		}
		return out
	}

	first := ids(Rank(build(), time.Now(), 0))
	for i := 0; i < 5; i++ {
		if got := ids(Rank(build(), time.Now(), 0)); got != first {
			t.Fatalf("ordering is not reproducible: %q then %q", first, got)
		}
	}
	// Ties break on item ID, so the order is meaningful rather than arbitrary.
	if first != "a,b,c" {
		t.Fatalf("tied results are not ordered deterministically: %q", first)
	}
}

func TestSimilarityIsNeverFoldedIntoRelevance(t *testing.T) {
	// The two numbers answer different questions and must stay separate.
	c := newCandidate("x")
	c.Add(SourceLexical, 1, "keyword match")
	c.BestSimilarity = 0.97

	ranked := Rank([]*Candidate{c}, time.Now(), 0)
	if ranked[0].BestSimilarity != 0.97 {
		t.Fatal("similarity was modified by ranking")
	}
	if ranked[0].Relevance == ranked[0].BestSimilarity {
		t.Fatal("relevance and similarity are the same number; they must be reported separately")
	}
}

func TestExplainListsEveryContributionStrongestFirst(t *testing.T) {
	c := newCandidate("x")
	c.AddScaled(SourceEntity, 0.2, "weak entity match")
	c.Add(SourceVector, 1, "strong meaning match")
	c.Add(SourceLexical, 2, "keyword match")

	lines := Explain(c)
	if len(lines) != 3 {
		t.Fatalf("expected 3 explanations, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "strong meaning match") {
		t.Fatalf("explanations are not ordered by contribution:\n%s", strings.Join(lines, "\n"))
	}
}

func TestExplainOnNilCandidate(t *testing.T) {
	if got := Explain(nil); got != nil {
		t.Fatalf("Explain(nil) = %v, want nil", got)
	}
}

func TestScaledContributionsAreClamped(t *testing.T) {
	c := newCandidate("x")
	c.AddScaled(SourceEntity, 99, "out of range")
	c.AddScaled(SourceEntity, -5, "negative")

	if c.Contributions[0].Score <= 0 {
		t.Fatal("an over-range strength produced no contribution")
	}
	if c.Contributions[1].Score != 0 {
		t.Fatalf("a negative strength contributed %v, want 0", c.Contributions[1].Score)
	}
}

func TestEmptyInputIsHandled(t *testing.T) {
	if got := Rank(nil, time.Now(), time.Hour); len(got) != 0 {
		t.Fatalf("Rank(nil) returned %d candidates", len(got))
	}
	if got := Rank([]*Candidate{}, time.Now(), time.Hour); len(got) != 0 {
		t.Fatal("Rank on an empty slice returned candidates")
	}
}

func TestEveryWeightedSourceHasAWeight(t *testing.T) {
	for _, s := range []Source{
		SourceLexical, SourceVector, SourceEntity, SourceExpansion,
		SourceRecency, SourceConfirmedRel, SourceTitle,
	} {
		if _, ok := Weights[s]; !ok {
			t.Errorf("source %q has no configured weight", s)
		}
	}
}

func ids(cands []*Candidate) string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.ItemID)
	}
	return strings.Join(out, ",")
}
