// Package relationships discovers, scores and reviews connections between
// stored items.
//
// The scoring model is deliberately not a black box. A relationship's strength
// is the sum of weighted contributions from named signals, each of which is
// stored alongside the score so the interface can show the arithmetic. A
// language model contributes exactly one of those signals; it is never asked
// for a percentage and its opinion can never be the whole score.
//
// The resulting number is a HEURISTIC RELATIONSHIP STRENGTH. It is not a
// calibrated probability and must not be presented as one.
package relationships

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/ids"
)

// ScoreVersion identifies the scoring model. Every stored relationship records
// the version that produced its score, so a weight change is detectable and the
// affected relationships can be recomputed rather than silently reinterpreted.
const ScoreVersion = "relationship-score-v1"

// Weights are the per-signal contributions of the current scoring model.
//
// The values are chosen so that:
//   - no single soft signal can reach the storage threshold alone;
//   - semantic similarity, on its own, tops out well below a confirmed link,
//     because similarity is not evidence of a factual relationship;
//   - human decisions dominate everything else.
var Weights = map[domain.SignalType]float64{
	domain.SigSemanticSimilarity: 0.30,
	domain.SigSharedEntity:       0.25,
	domain.SigAliasEntity:        0.10,
	domain.SigSharedConcept:      0.12,
	domain.SigTemporalProximity:  0.10,
	domain.SigSharedMetadata:     0.08,
	domain.SigClaimSupport:       0.22,
	domain.SigClaimContradiction: 0.22,
	domain.SigModelClassify:      0.15,
	domain.SigContentHashMatch:   0.45,
	domain.SigUserConfirmed:      1.00,
	domain.SigUserRejected:       -1.00,
}

// WeightOrder lists the signal types in a fixed, meaningful order: strongest
// evidence first, human decisions last.
//
// Map iteration order is random, so anything that displays the weights needs
// this to render the same way twice.
func WeightOrder() []domain.SignalType {
	return []domain.SignalType{
		domain.SigContentHashMatch,
		domain.SigSemanticSimilarity,
		domain.SigSharedEntity,
		domain.SigClaimSupport,
		domain.SigClaimContradiction,
		domain.SigModelClassify,
		domain.SigSharedConcept,
		domain.SigAliasEntity,
		domain.SigTemporalProximity,
		domain.SigSharedMetadata,
		domain.SigUserConfirmed,
		domain.SigUserRejected,
	}
}

// PairKey builds the deduplication key identifying one connection between two
// items.
//
// Endpoint order is normalised, so a link discovered while processing A and the
// same link rediscovered while processing B collide on one record. The type is
// deliberately excluded: a reprocess that reclassifies a relationship should
// update it, not add a second edge between the same pair.
func PairKey(workspaceID, a, b string) string {
	if a > b {
		a, b = b, a
	}
	return ids.Deterministic(workspaceID, a, b)
}

// maxSignalsOfType caps how many instances of one signal type contribute, so
// twelve shared entities do not simply out-vote every other consideration.
const maxSignalsOfType = 3

// diminishing is the factor applied to the second and subsequent signals of
// the same type.
const diminishing = 0.5

// MaxUnreviewedScore is the ceiling for a relationship no person has reviewed.
//
// Only a human decision can produce a 1.00. Without this cap, a candidate with
// enough accumulated signals would reach the same strength as one someone had
// actually checked, and the interface would present them identically — exactly
// the conflation between "much evidence" and "confirmed" the design avoids.
const MaxUnreviewedScore = 0.95

// Score computes the relationship strength from a signal set.
//
// Repeated signals of the same type contribute with diminishing returns and are
// capped, so evidence breadth matters more than evidence repetition. The result
// is clamped to 0..1: a rejection drives it to zero rather than negative.
func Score(signals []domain.Signal) float64 {
	byType := map[domain.SignalType][]domain.Signal{}
	for _, s := range signals {
		byType[s.Type] = append(byType[s.Type], s)
	}

	total := 0.0
	for _, group := range byType {
		// Strongest first, so the full-weight slot goes to the best evidence.
		sort.SliceStable(group, func(i, j int) bool {
			return math.Abs(group[i].Contribution()) > math.Abs(group[j].Contribution())
		})
		factor := 1.0
		for i, s := range group {
			if i >= maxSignalsOfType {
				break
			}
			total += s.Contribution() * factor
			factor *= diminishing
		}
	}
	return clamp01(total)
}

// Normalise fills in the configured weight for each signal and clamps its
// strength, so a caller cannot accidentally construct an over-weighted signal.
func Normalise(signals []domain.Signal) []domain.Signal {
	out := make([]domain.Signal, 0, len(signals))
	for _, s := range signals {
		s.Weight = Weights[s.Type]
		if s.Type == domain.SigUserRejected {
			s.Strength = clampAbs(s.Strength)
		} else {
			s.Strength = clamp01(s.Strength)
		}
		out = append(out, s)
	}
	return out
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func clampAbs(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// ---------------------------------------------------------------------------
// Signal builders
//
// Each builder turns a raw observation into a named signal with a normalised
// strength and a human-readable value. The value is what the UI shows as the
// evidence; the strength is what the arithmetic uses.
// ---------------------------------------------------------------------------

// SimilaritySignal converts a cosine similarity into a signal.
//
// Cosine similarity between unrelated documents from the same corpus is
// routinely 0.5–0.7, so the strength curve starts above that floor: without
// rebasing, every pair of documents in a workspace would look related.
func SimilaritySignal(cosine float64) domain.Signal {
	const floor = 0.55 // below this, similarity carries no information
	strength := 0.0
	if cosine > floor {
		strength = (cosine - floor) / (1 - floor)
	}
	return domain.Signal{
		Type:     domain.SigSemanticSimilarity,
		Value:    fmt.Sprintf("%.2f cosine", cosine),
		Strength: clamp01(strength),
		Weight:   Weights[domain.SigSemanticSimilarity],
		Detail:   "How alike the wording and subject matter are. Similar wording is not evidence that two items describe the same thing.",
	}
}

// SharedEntitySignal reports an exact entity match between two items.
//
// Strength reflects how discriminating the entity is: a subject or concept
// shared by many documents says less than a specific organisation or person.
func SharedEntitySignal(name string, t domain.EntityType, corpusFrequency int) domain.Signal {
	strength := 1.0
	switch t {
	case domain.EntityConcept, domain.EntitySubject:
		strength = 0.5
	case domain.EntityDate:
		strength = 0.6
	}
	// A name appearing in most documents is not evidence of a connection.
	if corpusFrequency > 1 {
		strength *= 1 / (1 + math.Log(float64(corpusFrequency)))
	}
	return domain.Signal{
		Type:     domain.SigSharedEntity,
		Value:    name,
		Strength: clamp01(strength),
		Weight:   Weights[domain.SigSharedEntity],
		Detail:   fmt.Sprintf("Both items refer to this %s.", t),
	}
}

// AliasEntitySignal reports a match made through an alias a person confirmed.
//
// Only confirmed aliases produce this signal; an unconfirmed candidate is a
// proposal and contributes nothing. It is still weighted below an exact name
// match, because the two entities remain separate records linked by a human
// judgement rather than by identical names.
func AliasEntitySignal(nameA, nameB string, similarity float64) domain.Signal {
	return domain.Signal{
		Type:     domain.SigAliasEntity,
		Value:    fmt.Sprintf("%s ≈ %s", nameA, nameB),
		Strength: clamp01(similarity),
		Weight:   Weights[domain.SigAliasEntity],
		Detail: "A person confirmed that these two names refer to the same thing. " +
			"They remain separate entity records.",
	}
}

// SharedConceptSignal reports overlapping concept vocabulary.
func SharedConceptSignal(concepts []string, overlap float64) domain.Signal {
	label := "shared concepts"
	if len(concepts) > 0 {
		label = joinLimit(concepts, 3)
	}
	return domain.Signal{
		Type:     domain.SigSharedConcept,
		Value:    label,
		Strength: clamp01(overlap),
		Weight:   Weights[domain.SigSharedConcept],
		Detail:   "Both items discuss these concepts.",
	}
}

// TemporalProximitySignal reports how close two items are in time.
//
// Proximity in time is context, never causation. The detail text says so,
// because a user reading a score needs to know what it does not mean.
func TemporalProximitySignal(gap time.Duration, window time.Duration) domain.Signal {
	if gap < 0 {
		gap = -gap
	}
	strength := 0.0
	if window > 0 && gap < window {
		// Linear decay across the window.
		strength = 1 - float64(gap)/float64(window)
	}
	return domain.Signal{
		Type:     domain.SigTemporalProximity,
		Value:    humaniseDuration(gap),
		Strength: clamp01(strength),
		Weight:   Weights[domain.SigTemporalProximity],
		Detail:   "The items were created close together in time. This is context, not evidence that one caused or followed from the other.",
	}
}

// SharedMetadataSignal reports matching user-supplied metadata.
func SharedMetadataSignal(key, value string) domain.Signal {
	return domain.Signal{
		Type:     domain.SigSharedMetadata,
		Value:    key + "=" + value,
		Strength: 1,
		Weight:   Weights[domain.SigSharedMetadata],
		Detail:   "Both items carry this metadata value.",
	}
}

// ClaimSupportSignal reports two claims agreeing about the same subject.
func ClaimSupportSignal(subject, detail string, strength float64) domain.Signal {
	return domain.Signal{
		Type:     domain.SigClaimSupport,
		Value:    subject,
		Strength: clamp01(strength),
		Weight:   Weights[domain.SigClaimSupport],
		Detail:   detail,
	}
}

// ClaimContradictionSignal reports two claims disagreeing about the same
// subject. Contradictions are surfaced, never reconciled away.
func ClaimContradictionSignal(subject, detail string, strength float64) domain.Signal {
	return domain.Signal{
		Type:     domain.SigClaimContradiction,
		Value:    subject,
		Strength: clamp01(strength),
		Weight:   Weights[domain.SigClaimContradiction],
		Detail:   detail,
	}
}

// ModelClassificationSignal records a language model's opinion as ONE signal.
//
// The model's self-reported confidence scales the signal's strength, but the
// weight caps how much any model opinion can move the score. A model cannot,
// on its own, push a relationship past the storage threshold.
func ModelClassificationSignal(relType domain.RelationshipType, confidence float64, rationale, model string) domain.Signal {
	return domain.Signal{
		Type:     domain.SigModelClassify,
		Value:    string(relType),
		Strength: clamp01(confidence),
		Weight:   Weights[domain.SigModelClassify],
		Detail: fmt.Sprintf("A language model (%s) suggested this relationship type. This is a model opinion, not a verified fact. %s",
			model, rationale),
	}
}

// ContentHashSignal reports byte-identical content.
func ContentHashSignal(hash string) domain.Signal {
	return domain.Signal{
		Type:     domain.SigContentHashMatch,
		Value:    shortHash(hash),
		Strength: 1,
		Weight:   Weights[domain.SigContentHashMatch],
		Detail:   "The two items have identical content.",
	}
}

// UserConfirmedSignal records a human confirmation.
func UserConfirmedSignal(actor string) domain.Signal {
	return domain.Signal{
		Type:     domain.SigUserConfirmed,
		Value:    "confirmed by " + actor,
		Strength: 1,
		Weight:   Weights[domain.SigUserConfirmed],
		Detail:   "A person reviewed and confirmed this relationship.",
	}
}

// UserRejectedSignal records a human rejection. Its negative weight drives the
// score to zero without deleting the evidence that produced the proposal.
func UserRejectedSignal(actor string) domain.Signal {
	return domain.Signal{
		Type:     domain.SigUserRejected,
		Value:    "rejected by " + actor,
		Strength: 1,
		Weight:   Weights[domain.SigUserRejected],
		Detail:   "A person reviewed this relationship and rejected it. The original proposal and its evidence are retained.",
	}
}

// ---------------------------------------------------------------------------
// Type inference
// ---------------------------------------------------------------------------

// InferType chooses a relationship type from the evidence.
//
// Deterministic evidence outranks a model opinion: an observed contradiction
// between claims is a stronger basis for calling something `contradicts` than
// a model saying so. When nothing more specific applies, the honest answer is
// `similar_to` — not a guess at a causal or updating relationship.
func InferType(signals []domain.Signal, sourceTime, targetTime time.Time, modelOpinion *domain.RelationshipType) domain.RelationshipType {
	has := func(t domain.SignalType) bool {
		for _, s := range signals {
			if s.Type == t && s.Strength > 0 {
				return true
			}
		}
		return false
	}

	switch {
	case has(domain.SigContentHashMatch):
		return domain.RelDerivedFrom
	case has(domain.SigClaimContradiction):
		return domain.RelContradicts
	case has(domain.SigClaimSupport):
		return domain.RelSupports
	}

	// A model opinion is accepted only for types it is well placed to judge,
	// and never for a claim about identity or supersession, which need
	// stronger evidence than a language model's impression.
	if modelOpinion != nil && modelOpinion.Valid() {
		switch *modelOpinion {
		case domain.RelSameEntityAs, domain.RelSupersedes, domain.RelDerivedFrom:
			// Not accepted on a model's word alone.
		default:
			return *modelOpinion
		}
	}

	// Chronology alone justifies only a chronological statement — never
	// causation, and never "updates".
	if has(domain.SigSharedEntity) && !sourceTime.IsZero() && !targetTime.IsZero() {
		if sourceTime.Before(targetTime) {
			return domain.RelOccurredBefore
		}
		if targetTime.Before(sourceTime) {
			return domain.RelOccurredAfter
		}
	}
	if has(domain.SigSharedEntity) {
		return domain.RelMentions
	}
	return domain.RelSimilarTo
}

// ---------------------------------------------------------------------------
// Explanation
// ---------------------------------------------------------------------------

// Explain renders the score's arithmetic as ordered lines of plain English,
// used by both the HTML interface and the API.
func Explain(r *domain.Relationship) []string {
	if r == nil {
		return nil
	}
	sigs := append([]domain.Signal(nil), r.Evidence.Signals...)
	sort.SliceStable(sigs, func(i, j int) bool {
		return math.Abs(sigs[i].Contribution()) > math.Abs(sigs[j].Contribution())
	})

	out := make([]string, 0, len(sigs)+2)
	for _, s := range sigs {
		out = append(out, fmt.Sprintf("%s: %s (strength %.2f × weight %.2f = %+.2f)",
			labelFor(s.Type), s.Value, s.Strength, s.Weight, s.Contribution()))
	}
	out = append(out, fmt.Sprintf("Relationship strength %.2f under %s. "+
		"This is a heuristic strength from the signals above, not a calibrated probability.",
		r.Score, r.ScoreVersion))
	if r.Provenance == domain.ProvModelInferred {
		out = append(out, "Provenance: proposed by analysis and not yet reviewed by a person.")
	}
	return out
}

func labelFor(t domain.SignalType) string {
	switch t {
	case domain.SigSemanticSimilarity:
		return "Semantic similarity"
	case domain.SigSharedEntity:
		return "Shared entity"
	case domain.SigAliasEntity:
		return "Possible alias match"
	case domain.SigSharedConcept:
		return "Shared concepts"
	case domain.SigTemporalProximity:
		return "Temporal proximity"
	case domain.SigSharedMetadata:
		return "Shared metadata"
	case domain.SigClaimSupport:
		return "Supporting claims"
	case domain.SigClaimContradiction:
		return "Conflicting claims"
	case domain.SigModelClassify:
		return "Model classification"
	case domain.SigContentHashMatch:
		return "Identical content"
	case domain.SigUserConfirmed:
		return "Confirmed by a person"
	case domain.SigUserRejected:
		return "Rejected by a person"
	}
	return string(t)
}

func humaniseDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%d months", int(d.Hours()/24/30))
	}
}

func joinLimit(items []string, n int) string {
	if len(items) <= n {
		return joinComma(items)
	}
	return fmt.Sprintf("%s and %d more", joinComma(items[:n]), len(items)-n)
}

func joinComma(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
