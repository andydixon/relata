package domain

import "time"

// RelationshipType is the semantic kind of connection between two nodes.
type RelationshipType string

const (
	RelSimilarTo          RelationshipType = "similar_to"
	RelMentions           RelationshipType = "mentions"
	RelInvolves           RelationshipType = "involves"
	RelSupports           RelationshipType = "supports"
	RelContradicts        RelationshipType = "contradicts"
	RelUpdates            RelationshipType = "updates"
	RelSupersedes         RelationshipType = "supersedes"
	RelProvidesContextFor RelationshipType = "provides_context_for"
	RelPossiblyExplains   RelationshipType = "possibly_explains"
	RelOccurredBefore     RelationshipType = "occurred_before"
	RelOccurredAfter      RelationshipType = "occurred_after"
	RelDerivedFrom        RelationshipType = "derived_from"
	RelSameEntityAs       RelationshipType = "same_entity_as"
)

// ValidRelationshipTypes is the closed set of accepted relationship types.
// Model output proposing anything outside this set is rejected.
var ValidRelationshipTypes = []RelationshipType{
	RelSimilarTo, RelMentions, RelInvolves, RelSupports, RelContradicts,
	RelUpdates, RelSupersedes, RelProvidesContextFor, RelPossiblyExplains,
	RelOccurredBefore, RelOccurredAfter, RelDerivedFrom, RelSameEntityAs,
}

// Valid reports whether t is a recognised relationship type.
func (t RelationshipType) Valid() bool {
	for _, v := range ValidRelationshipTypes {
		if v == t {
			return true
		}
	}
	return false
}

// Directed reports whether the type implies an ordered source -> target reading.
// Non-directed types are symmetric and are stored with a canonical ordering.
func (t RelationshipType) Directed() bool {
	switch t {
	case RelSimilarTo, RelSameEntityAs:
		return false
	default:
		return true
	}
}

// Inverse returns the type that describes the same connection read from the
// target's point of view, and whether such an inverse exists.
func (t RelationshipType) Inverse() (RelationshipType, bool) {
	switch t {
	case RelOccurredBefore:
		return RelOccurredAfter, true
	case RelOccurredAfter:
		return RelOccurredBefore, true
	case RelSimilarTo:
		return RelSimilarTo, true
	case RelSameEntityAs:
		return RelSameEntityAs, true
	}
	return "", false
}

// Provenance records how a relationship came to exist. Model-inferred
// relationships are proposals, not facts, and must be labelled as such.
type Provenance string

const (
	ProvExplicit      Provenance = "explicit"
	ProvDeterministic Provenance = "deterministic"
	ProvModelInferred Provenance = "model_inferred"
	ProvUserConfirmed Provenance = "user_confirmed"
	ProvUserRejected  Provenance = "user_rejected"
)

// ValidProvenances is the closed set of provenance classifications.
var ValidProvenances = []Provenance{
	ProvExplicit, ProvDeterministic, ProvModelInferred, ProvUserConfirmed, ProvUserRejected,
}

// Valid reports whether p is a recognised provenance.
func (p Provenance) Valid() bool {
	for _, v := range ValidProvenances {
		if v == p {
			return true
		}
	}
	return false
}

// Asserted reports whether the provenance represents something a human or a
// deterministic rule stated, as opposed to a model's guess.
func (p Provenance) Asserted() bool {
	return p == ProvExplicit || p == ProvDeterministic || p == ProvUserConfirmed
}

// NodeKind is what a relationship endpoint refers to.
type NodeKind string

const (
	NodeItem   NodeKind = "item"
	NodeChunk  NodeKind = "chunk"
	NodeEntity NodeKind = "entity"
	NodeClaim  NodeKind = "claim"
)

// Valid reports whether k is a recognised node kind.
func (k NodeKind) Valid() bool {
	switch k {
	case NodeItem, NodeChunk, NodeEntity, NodeClaim:
		return true
	}
	return false
}

// ReviewStatus tracks human adjudication of a proposed relationship.
type ReviewStatus string

const (
	ReviewPending   ReviewStatus = "pending"
	ReviewConfirmed ReviewStatus = "confirmed"
	ReviewRejected  ReviewStatus = "rejected"
	ReviewEdited    ReviewStatus = "edited"
)

// SignalType names a concrete, explainable contribution to a relationship score.
type SignalType string

const (
	SigSemanticSimilarity SignalType = "semantic_similarity"
	SigSharedEntity       SignalType = "shared_entity"
	SigAliasEntity        SignalType = "alias_entity"
	SigSharedConcept      SignalType = "shared_concept"
	SigTemporalProximity  SignalType = "temporal_proximity"
	SigSharedMetadata     SignalType = "shared_metadata"
	SigClaimSupport       SignalType = "claim_support"
	SigClaimContradiction SignalType = "claim_contradiction"
	SigModelClassify      SignalType = "model_classification"
	SigUserConfirmed      SignalType = "user_confirmation"
	SigUserRejected       SignalType = "user_rejection"
	SigContentHashMatch   SignalType = "content_hash_match"
)

// Signal is one explicit piece of evidence behind a relationship score.
//
// Value carries the human-readable evidence ("Example Ltd", "2 days"); Strength
// is the normalised 0..1 contribution the signal makes before weighting. Keeping
// them separate is what lets the UI show the arithmetic instead of a bare number.
type Signal struct {
	Type     SignalType `bson:"type" json:"type"`
	Value    string     `bson:"value" json:"value"`
	Strength float64    `bson:"strength" json:"strength"`
	Weight   float64    `bson:"weight" json:"weight"`
	Detail   string     `bson:"detail,omitempty" json:"detail,omitempty"`
}

// Contribution is the signal's weighted addition to the total score.
func (s Signal) Contribution() float64 { return s.Strength * s.Weight }

// Evidence is the concrete supporting material for a proposed relationship:
// the signals plus pointers at the exact chunks and entities involved.
type Evidence struct {
	Signals []Signal `bson:"signals" json:"signals"`

	SourceChunkIDs []string `bson:"source_chunk_ids,omitempty" json:"source_chunk_ids,omitempty"`
	TargetChunkIDs []string `bson:"target_chunk_ids,omitempty" json:"target_chunk_ids,omitempty"`
	SharedEntities []string `bson:"shared_entities,omitempty" json:"shared_entities,omitempty"`

	// Excerpts are short verbatim quotations from each side, for display.
	SourceExcerpt string `bson:"source_excerpt,omitempty" json:"source_excerpt,omitempty"`
	TargetExcerpt string `bson:"target_excerpt,omitempty" json:"target_excerpt,omitempty"`

	// ModelRationale is the model's own words about the connection. It is
	// displayed as a model opinion, never as an established fact.
	ModelRationale string `bson:"model_rationale,omitempty" json:"model_rationale,omitempty"`
	ModelVersion   string `bson:"model_version,omitempty" json:"model_version,omitempty"`
}

// ReviewEvent is one immutable entry in a relationship's decision history.
// History is append-only: rejecting a relationship never erases the proposal.
type ReviewEvent struct {
	At        time.Time `bson:"at" json:"at"`
	Action    string    `bson:"action" json:"action"`
	ActorID   string    `bson:"actor_id" json:"actor_id"`
	ActorName string    `bson:"actor_name,omitempty" json:"actor_name,omitempty"`
	Note      string    `bson:"note,omitempty" json:"note,omitempty"`
	FromValue string    `bson:"from_value,omitempty" json:"from_value,omitempty"`
	ToValue   string    `bson:"to_value,omitempty" json:"to_value,omitempty"`
}

// Relationship connects two nodes with a scored, evidenced, provenance-tagged link.
type Relationship struct {
	ID          string `bson:"_id" json:"id"`
	WorkspaceID string `bson:"workspace_id" json:"workspace_id"`

	SourceKind NodeKind `bson:"source_kind" json:"source_kind"`
	SourceID   string   `bson:"source_id" json:"source_id"`
	TargetKind NodeKind `bson:"target_kind" json:"target_kind"`
	TargetID   string   `bson:"target_id" json:"target_id"`

	Type       RelationshipType `bson:"type" json:"type"`
	Provenance Provenance       `bson:"provenance" json:"provenance"`

	// Score is a heuristic relationship strength in 0..1. It is NOT a calibrated
	// probability and must never be presented as one.
	Score        float64  `bson:"score" json:"score"`
	ScoreVersion string   `bson:"score_version" json:"score_version"`
	Evidence     Evidence `bson:"evidence" json:"evidence"`

	// OriginalType and OriginalProvenance preserve what was first proposed, even
	// after a user edits the relationship.
	OriginalType       RelationshipType `bson:"original_type" json:"original_type"`
	OriginalProvenance Provenance       `bson:"original_provenance" json:"original_provenance"`

	ReviewStatus  ReviewStatus  `bson:"review_status" json:"review_status"`
	ReviewHistory []ReviewEvent `bson:"review_history,omitempty" json:"review_history,omitempty"`

	// DedupeKey makes relationship creation idempotent across reprocessing runs.
	DedupeKey string `bson:"dedupe_key" json:"-"`

	CreatedAt time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
	Deleted   bool      `bson:"deleted" json:"deleted"`
}

// Confirmed reports whether a human affirmed this relationship.
func (r *Relationship) Confirmed() bool { return r.ReviewStatus == ReviewConfirmed }

// Rejected reports whether a human rejected this relationship. Rejected
// relationships are retained for audit and to inform future ranking.
func (r *Relationship) Rejected() bool { return r.ReviewStatus == ReviewRejected }

// Inferred reports whether the link is a model proposal rather than an assertion.
func (r *Relationship) Inferred() bool { return r.Provenance == ProvModelInferred }
