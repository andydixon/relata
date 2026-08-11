package relationships

import (
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/andydixon/relata/internal/domain"
)

func TestScoreIsSumOfWeightedContributions(t *testing.T) {
	sigs := []domain.Signal{
		{Type: domain.SigSemanticSimilarity, Strength: 1, Weight: 0.30},
		{Type: domain.SigSharedEntity, Strength: 1, Weight: 0.25},
	}
	got := Score(sigs)
	if diff := got - 0.55; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("score = %v, want the sum of contributions 0.55", got)
	}
}

func TestScoreIsClampedToUnitRange(t *testing.T) {
	// Enough independent strong evidence to exceed 1 before clamping.
	many := []domain.Signal{
		{Type: domain.SigContentHashMatch, Strength: 1, Weight: 0.45},
		{Type: domain.SigSemanticSimilarity, Strength: 1, Weight: 0.30},
		{Type: domain.SigSharedEntity, Strength: 1, Weight: 0.25},
		{Type: domain.SigClaimSupport, Strength: 1, Weight: 0.22},
		{Type: domain.SigSharedConcept, Strength: 1, Weight: 0.12},
	}
	if got := Score(many); got != 1 {
		t.Fatalf("score = %v, want it clamped to 1", got)
	}
	if got := Score([]domain.Signal{{Type: domain.SigUserRejected, Strength: 1, Weight: -1}}); got != 0 {
		t.Fatalf("rejected relationship scored %v, want 0", got)
	}
}

// Evidence breadth should count for more than repetition of one kind of
// evidence, or a document mentioning one company twelve times would outrank a
// document that genuinely relates on several independent grounds.
func TestRepeatedSignalsHaveDiminishingReturns(t *testing.T) {
	repeated := make([]domain.Signal, 0, 6)
	for i := 0; i < 6; i++ {
		repeated = append(repeated, domain.Signal{Type: domain.SigSharedEntity, Strength: 1, Weight: 0.25})
	}
	repeatedScore := Score(repeated)

	varied := []domain.Signal{
		{Type: domain.SigSharedEntity, Strength: 1, Weight: 0.25},
		{Type: domain.SigSemanticSimilarity, Strength: 1, Weight: 0.30},
		{Type: domain.SigTemporalProximity, Strength: 1, Weight: 0.10},
	}
	if Score(varied) <= repeatedScore {
		t.Fatalf("six repeats (%v) outranked three independent signals (%v)", repeatedScore, Score(varied))
	}
	// And repetition must be capped rather than accumulating without limit.
	if repeatedScore >= 0.25*6 {
		t.Fatalf("repeated signals accumulated without diminishing: %v", repeatedScore)
	}
}

// The central design claim: similarity alone must not look like a strong,
// evidenced relationship.
func TestSemanticSimilarityAloneCannotProduceAStrongScore(t *testing.T) {
	perfect := Score([]domain.Signal{SimilaritySignal(0.99)})
	if perfect >= 0.35 {
		t.Fatalf("near-identical wording alone scored %v; similarity is not evidence of a factual relationship", perfect)
	}
}

func TestSimilaritySignalIgnoresBaselineSimilarity(t *testing.T) {
	// Unrelated documents in one corpus routinely sit around 0.5–0.6 cosine.
	if s := SimilaritySignal(0.55); s.Strength != 0 {
		t.Fatalf("baseline similarity produced strength %v, want 0", s.Strength)
	}
	if s := SimilaritySignal(0.40); s.Strength != 0 {
		t.Fatalf("below-baseline similarity produced strength %v, want 0", s.Strength)
	}
	high := SimilaritySignal(0.95)
	low := SimilaritySignal(0.70)
	if high.Strength <= low.Strength {
		t.Fatal("similarity strength is not monotonic")
	}
}

func TestSharedEntityStrengthFallsWithCorpusFrequency(t *testing.T) {
	rare := SharedEntitySignal("Example Ltd", domain.EntityOrganisation, 1)
	common := SharedEntitySignal("Example Ltd", domain.EntityOrganisation, 200)
	if common.Strength >= rare.Strength {
		t.Fatalf("a ubiquitous entity (%v) counts as much as a rare one (%v)", common.Strength, rare.Strength)
	}
	concept := SharedEntitySignal("logistics", domain.EntityConcept, 1)
	if concept.Strength >= rare.Strength {
		t.Fatal("a shared concept should be weaker evidence than a shared organisation")
	}
}

// A model must never be able to manufacture a relationship by itself.
func TestModelOpinionAloneCannotReachTheStorageThreshold(t *testing.T) {
	const typicalStoreThreshold = 0.25
	confident := ModelClassificationSignal(domain.RelContradicts, 1.0, "very sure", "some-model")
	if got := Score([]domain.Signal{confident}); got >= typicalStoreThreshold {
		t.Fatalf("a maximally confident model opinion alone scored %v, at or above the storage threshold %v",
			got, typicalStoreThreshold)
	}
}

func TestModelSignalIsLabelledAsAnOpinion(t *testing.T) {
	s := ModelClassificationSignal(domain.RelSupports, 0.8, "they agree on the date", "llama")
	if !strings.Contains(s.Detail, "model opinion") || !strings.Contains(s.Detail, "not a verified fact") {
		t.Fatalf("model signal is not labelled as an opinion: %q", s.Detail)
	}
}

func TestTemporalSignalDisclaimsCausation(t *testing.T) {
	s := TemporalProximitySignal(48*time.Hour, 7*24*time.Hour)
	if !strings.Contains(strings.ToLower(s.Detail), "not evidence") {
		t.Fatalf("temporal signal does not disclaim causation: %q", s.Detail)
	}
	near := TemporalProximitySignal(time.Hour, 7*24*time.Hour)
	far := TemporalProximitySignal(6*24*time.Hour, 7*24*time.Hour)
	if near.Strength <= far.Strength {
		t.Fatal("temporal strength does not decay with distance")
	}
	outside := TemporalProximitySignal(30*24*time.Hour, 7*24*time.Hour)
	if outside.Strength != 0 {
		t.Fatalf("a gap beyond the window scored %v, want 0", outside.Strength)
	}
}

func TestUserDecisionsDominate(t *testing.T) {
	base := []domain.Signal{
		{Type: domain.SigSemanticSimilarity, Strength: 1, Weight: 0.30},
		{Type: domain.SigSharedEntity, Strength: 1, Weight: 0.25},
	}
	confirmed := Score(append(append([]domain.Signal{}, base...), UserConfirmedSignal("alice")))
	if confirmed != 1 {
		t.Fatalf("confirmed relationship scored %v, want 1", confirmed)
	}
	rejected := Score(append(append([]domain.Signal{}, base...), UserRejectedSignal("alice")))
	if rejected != 0 {
		t.Fatalf("rejected relationship scored %v, want 0", rejected)
	}
}

func TestNormaliseAppliesConfiguredWeights(t *testing.T) {
	// A caller cannot smuggle in an inflated weight.
	got := Normalise([]domain.Signal{{Type: domain.SigSemanticSimilarity, Strength: 5, Weight: 99}})
	if got[0].Weight != Weights[domain.SigSemanticSimilarity] {
		t.Fatalf("weight %v was not replaced with the configured value", got[0].Weight)
	}
	if got[0].Strength != 1 {
		t.Fatalf("strength %v was not clamped to 1", got[0].Strength)
	}
}

// --- Type inference --------------------------------------------------------

func TestObservedContradictionOutranksModelOpinion(t *testing.T) {
	opinion := domain.RelSupports
	sigs := []domain.Signal{
		ClaimContradictionSignal("project cost", "£42,000 versus £51,500", 1),
		ModelClassificationSignal(domain.RelSupports, 0.9, "", "m"),
	}
	if got := InferType(sigs, time.Time{}, time.Time{}, &opinion); got != domain.RelContradicts {
		t.Fatalf("inferred %q; an observed contradiction must outrank a model's opinion", got)
	}
}

func TestModelCannotAssertIdentityOrSupersession(t *testing.T) {
	sigs := []domain.Signal{SimilaritySignal(0.9)}
	for _, forbidden := range []domain.RelationshipType{domain.RelSameEntityAs, domain.RelSupersedes, domain.RelDerivedFrom} {
		op := forbidden
		if got := InferType(sigs, time.Time{}, time.Time{}, &op); got == forbidden {
			t.Fatalf("a model's word alone established %q", forbidden)
		}
	}
}

func TestChronologyAloneYieldsOnlyAChronologicalType(t *testing.T) {
	earlier := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	later := time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC)
	sigs := []domain.Signal{SharedEntitySignal("Example Ltd", domain.EntityOrganisation, 1)}

	got := InferType(sigs, earlier, later, nil)
	if got != domain.RelOccurredBefore {
		t.Fatalf("inferred %q, want occurred_before", got)
	}
	// Never an unproven causal or updating claim.
	for _, forbidden := range []domain.RelationshipType{domain.RelPossiblyExplains, domain.RelUpdates, domain.RelSupersedes} {
		if got == forbidden {
			t.Fatalf("chronology alone produced %q", forbidden)
		}
	}
}

func TestSimilarityOnlyYieldsSimilarTo(t *testing.T) {
	if got := InferType([]domain.Signal{SimilaritySignal(0.97)}, time.Time{}, time.Time{}, nil); got != domain.RelSimilarTo {
		t.Fatalf("inferred %q from similarity alone, want similar_to", got)
	}
}

func TestIdenticalContentYieldsDerivedFrom(t *testing.T) {
	sigs := []domain.Signal{ContentHashSignal("abc123def456789")}
	if got := InferType(sigs, time.Time{}, time.Time{}, nil); got != domain.RelDerivedFrom {
		t.Fatalf("inferred %q for identical content, want derived_from", got)
	}
}

// --- Explanation -----------------------------------------------------------

func TestExplainShowsTheArithmeticAndTheCaveat(t *testing.T) {
	r := &domain.Relationship{
		Score:        0.55,
		ScoreVersion: ScoreVersion,
		Provenance:   domain.ProvModelInferred,
		Evidence: domain.Evidence{Signals: []domain.Signal{
			SimilaritySignal(0.9),
			SharedEntitySignal("Example Ltd", domain.EntityOrganisation, 1),
		}},
	}
	lines := Explain(r)
	joined := strings.Join(lines, "\n")

	for _, want := range []string{"Semantic similarity", "Shared entity", "Example Ltd", "strength", "weight"} {
		if !strings.Contains(joined, want) {
			t.Errorf("explanation is missing %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, "not a calibrated probability") {
		t.Error("explanation does not state that the score is heuristic")
	}
	if !strings.Contains(joined, "not yet reviewed by a person") {
		t.Error("an unreviewed model-inferred relationship is not labelled as such")
	}
	if !strings.Contains(joined, ScoreVersion) {
		t.Error("the scoring version is not recorded in the explanation")
	}
}

func TestExplainOrdersByContribution(t *testing.T) {
	r := &domain.Relationship{
		ScoreVersion: ScoreVersion,
		Evidence: domain.Evidence{Signals: []domain.Signal{
			{Type: domain.SigSharedMetadata, Value: "weak", Strength: 0.1, Weight: 0.08},
			{Type: domain.SigContentHashMatch, Value: "strong", Strength: 1, Weight: 0.45},
		}},
	}
	lines := Explain(r)
	if !strings.Contains(lines[0], "Identical content") {
		t.Fatalf("strongest signal is not listed first:\n%s", strings.Join(lines, "\n"))
	}
}

func TestEveryWeightedSignalTypeHasAWeightAndLabel(t *testing.T) {
	all := []domain.SignalType{
		domain.SigSemanticSimilarity, domain.SigSharedEntity, domain.SigAliasEntity,
		domain.SigSharedConcept, domain.SigTemporalProximity, domain.SigSharedMetadata,
		domain.SigClaimSupport, domain.SigClaimContradiction, domain.SigModelClassify,
		domain.SigUserConfirmed, domain.SigUserRejected, domain.SigContentHashMatch,
	}
	for _, tp := range all {
		if _, ok := Weights[tp]; !ok {
			t.Errorf("signal %q has no configured weight", tp)
		}
		if labelFor(tp) == string(tp) {
			t.Errorf("signal %q has no human-readable label", tp)
		}
	}
}

// --- Deduplication ---------------------------------------------------------

// One connection between two items is one relationship, however it was found.
// Discovery runs once per item, so without an order-independent key the same
// link is stored twice — once as A→B and once as B→A.
func TestPairKeyIsIndependentOfEndpointOrder(t *testing.T) {
	const ws = "workspace-1"
	a, b := "aaaaaaaa-0000-4000-8000-000000000001", "bbbbbbbb-0000-4000-8000-000000000002"

	if PairKey(ws, a, b) != PairKey(ws, b, a) {
		t.Fatal("the dedupe key depends on endpoint order, so each link would be stored twice")
	}
}

func TestPairKeyDistinguishesDifferentPairsAndWorkspaces(t *testing.T) {
	a := "aaaaaaaa-0000-4000-8000-000000000001"
	b := "bbbbbbbb-0000-4000-8000-000000000002"
	c := "cccccccc-0000-4000-8000-000000000003"

	if PairKey("ws1", a, b) == PairKey("ws1", a, c) {
		t.Fatal("different pairs collide on one key")
	}
	// A workspace boundary must be part of the identity.
	if PairKey("ws1", a, b) == PairKey("ws2", a, b) {
		t.Fatal("the same pair in two workspaces shares a key")
	}
}

func TestUnreviewedScoreIsCappedBelowAHumanDecision(t *testing.T) {
	// Enough independent evidence to saturate the raw score.
	strong := []domain.Signal{
		{Type: domain.SigContentHashMatch, Strength: 1, Weight: 0.45},
		{Type: domain.SigSemanticSimilarity, Strength: 1, Weight: 0.30},
		{Type: domain.SigSharedEntity, Strength: 1, Weight: 0.25},
		{Type: domain.SigClaimSupport, Strength: 1, Weight: 0.22},
	}
	if Score(strong) != 1 {
		t.Fatalf("expected the raw score to saturate, got %v", Score(strong))
	}
	// The ceiling exists so "much evidence" never presents as "confirmed".
	if MaxUnreviewedScore >= 1.0 {
		t.Fatalf("MaxUnreviewedScore = %v; it must be below 1.00, which is reserved for a human decision",
			MaxUnreviewedScore)
	}
	confirmed := Score(append(append([]domain.Signal{}, strong...), UserConfirmedSignal("alice")))
	if confirmed <= MaxUnreviewedScore {
		t.Fatal("a confirmed relationship does not outrank the strongest unreviewed one")
	}
}

// --- Alias signals ---------------------------------------------------------

func TestAliasSignalIsWeakerThanAnExactEntityMatch(t *testing.T) {
	exact := SharedEntitySignal("Example Ltd", domain.EntityOrganisation, 1)
	alias := AliasEntitySignal("BBC", "British Broadcasting Corporation", 0.9)

	if alias.Contribution() >= exact.Contribution() {
		t.Fatalf("an alias match (%v) counts as much as an exact match (%v); "+
			"the identification is a human judgement about two separate records",
			alias.Contribution(), exact.Contribution())
	}
	// The signal only fires for a confirmed alias, so its wording must say so
	// and must make clear the records are still separate.
	if !strings.Contains(alias.Detail, "person confirmed") ||
		!strings.Contains(alias.Detail, "separate entity records") {
		t.Fatalf("the alias signal does not describe what it means: %q", alias.Detail)
	}
}

// excerpt slices UTF-8 evidence text at a computed offset for display.
func FuzzExcerpt(f *testing.F) {
	f.Add("The revised delivery date is 14 March 2025.", 20)
	f.Add("ééééééé", 3)
	f.Add("🌍🌍🌍", 2)
	f.Add("", 0)

	f.Fuzz(func(t *testing.T, text string, n int) {
		if n < 0 || n > 1<<16 || !utf8.ValidString(text) {
			return
		}
		if got := excerpt(text, n); !utf8.ValidString(got) {
			t.Fatalf("excerpt(%q, %d) produced invalid UTF-8: %q", text, n, got)
		}
	})
}

// Score must stay in range whatever signals it is handed, including values a
// caller should never construct.
func FuzzScore(f *testing.F) {
	f.Add(1.0, 0.3, 2)
	f.Add(-5.0, 99.0, 7)

	f.Fuzz(func(t *testing.T, strength, weight float64, count int) {
		if count < 0 || count > 64 {
			return
		}
		sigs := make([]domain.Signal, 0, count)
		for i := 0; i < count; i++ {
			sigs = append(sigs, domain.Signal{
				Type: domain.SigSharedEntity, Strength: strength, Weight: weight,
			})
		}
		got := Score(Normalise(sigs))
		if math.IsNaN(got) || got < 0 || got > 1 {
			t.Fatalf("Score = %v for strength=%v weight=%v count=%d; must stay within 0..1",
				got, strength, weight, count)
		}
	})
}
