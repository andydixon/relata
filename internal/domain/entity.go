package domain

import "time"

// EntityType classifies an extracted subject.
type EntityType string

const (
	EntityPerson       EntityType = "person"
	EntityOrganisation EntityType = "organisation"
	EntityLocation     EntityType = "location"
	EntityProduct      EntityType = "product"
	EntityEvent        EntityType = "event"
	EntityDate         EntityType = "date"
	EntitySubject      EntityType = "subject"
	EntityConcept      EntityType = "concept"
)

// ValidEntityTypes is the closed set of accepted entity types.
var ValidEntityTypes = []EntityType{
	EntityPerson, EntityOrganisation, EntityLocation, EntityProduct,
	EntityEvent, EntityDate, EntitySubject, EntityConcept,
}

// Valid reports whether t is a recognised entity type.
func (t EntityType) Valid() bool {
	for _, v := range ValidEntityTypes {
		if v == t {
			return true
		}
	}
	return false
}

// ExtractionMethod records how a derived record was produced.
type ExtractionMethod string

const (
	MethodDeterministic ExtractionMethod = "deterministic"
	MethodModel         ExtractionMethod = "model"
	MethodUser          ExtractionMethod = "user"
)

// SourceRef points at the exact place a derived record came from.
// A claim or entity must never lose this link back to original text.
type SourceRef struct {
	ItemID  string `bson:"item_id" json:"item_id"`
	ChunkID string `bson:"chunk_id,omitempty" json:"chunk_id,omitempty"`
	// Start and End are byte offsets into the item's extracted text.
	Start int `bson:"start" json:"start"`
	End   int `bson:"end" json:"end"`
	// Quote is the verbatim text at those offsets.
	Quote string `bson:"quote,omitempty" json:"quote,omitempty"`
	Page  int    `bson:"page,omitempty" json:"page,omitempty"`
}

// Entity is an identifiable subject extracted from source material.
//
// Entities are keyed on (workspace, type, normalised name). Two entities whose
// names merely look alike are never merged automatically; a candidate alias is
// proposed instead and awaits confirmation.
type Entity struct {
	ID          string `bson:"_id" json:"id"`
	WorkspaceID string `bson:"workspace_id" json:"workspace_id"`

	CanonicalName string     `bson:"canonical_name" json:"canonical_name"`
	NormalisedKey string     `bson:"normalised_key" json:"normalised_key"`
	Type          EntityType `bson:"type" json:"type"`

	Aliases    []string    `bson:"aliases,omitempty" json:"aliases,omitempty"`
	SourceRefs []SourceRef `bson:"source_refs,omitempty" json:"source_refs,omitempty"`
	ItemIDs    []string    `bson:"item_ids" json:"item_ids"`

	Method ExtractionMethod `bson:"method" json:"method"`
	// EvidenceScore reflects how well-attested the entity is (mention count,
	// extraction agreement). It is not a probability.
	EvidenceScore float64 `bson:"evidence_score" json:"evidence_score"`
	Mentions      int     `bson:"mentions" json:"mentions"`
	ModelVersion  string  `bson:"model_version,omitempty" json:"model_version,omitempty"`

	CreatedAt time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
	Deleted   bool      `bson:"deleted" json:"deleted"`
}

// AliasStatus tracks whether a proposed alias link has been adjudicated.
type AliasStatus string

const (
	AliasCandidate AliasStatus = "candidate"
	AliasConfirmed AliasStatus = "confirmed"
	AliasRejected  AliasStatus = "rejected"
)

// EntityAlias is a proposed "these two names may be the same thing" link.
// It exists precisely so that name similarity does not silently merge entities.
type EntityAlias struct {
	ID          string `bson:"_id" json:"id"`
	WorkspaceID string `bson:"workspace_id" json:"workspace_id"`

	EntityID   string `bson:"entity_id" json:"entity_id"`
	Alias      string `bson:"alias" json:"alias"`
	Normalised string `bson:"normalised" json:"normalised"`
	// CandidateEntityID is set when the alias points at another existing entity.
	CandidateEntityID string `bson:"candidate_entity_id,omitempty" json:"candidate_entity_id,omitempty"`

	Status     AliasStatus      `bson:"status" json:"status"`
	Method     ExtractionMethod `bson:"method" json:"method"`
	Similarity float64          `bson:"similarity" json:"similarity"`
	Reason     string           `bson:"reason,omitempty" json:"reason,omitempty"`

	CreatedAt time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
}

// ClaimNature distinguishes how faithfully a claim reproduces its source.
type ClaimNature string

const (
	ClaimQuoted      ClaimNature = "quoted"
	ClaimParaphrased ClaimNature = "paraphrased"
	ClaimInferred    ClaimNature = "inferred"
)

// Valid reports whether n is a recognised claim nature.
func (n ClaimNature) Valid() bool {
	switch n {
	case ClaimQuoted, ClaimParaphrased, ClaimInferred:
		return true
	}
	return false
}

// Claim is a statement made by a source item. A claim is not a fact: it is a
// record that some source said something, and it always retains the exact text.
type Claim struct {
	ID          string `bson:"_id" json:"id"`
	WorkspaceID string `bson:"workspace_id" json:"workspace_id"`
	ItemID      string `bson:"item_id" json:"item_id"`
	ChunkID     string `bson:"chunk_id,omitempty" json:"chunk_id,omitempty"`

	// Statement is a short normalised rendering; SourceRef.Quote is the original.
	Statement string    `bson:"statement" json:"statement"`
	SourceRef SourceRef `bson:"source_ref" json:"source_ref"`

	// Attribution is who the source says is making the claim, where known.
	Attribution string      `bson:"attribution,omitempty" json:"attribution,omitempty"`
	Nature      ClaimNature `bson:"nature" json:"nature"`

	// Subject and Predicate are coarse normalised keys used to find claims that
	// talk about the same thing, which is how support/contradiction candidates
	// are located without comparing every claim to every other claim.
	Subject   string `bson:"subject,omitempty" json:"subject,omitempty"`
	Predicate string `bson:"predicate,omitempty" json:"predicate,omitempty"`
	// Numbers and Dates hold normalised values mentioned in the claim, used to
	// detect disagreement about the same quantity or date.
	Numbers []float64 `bson:"numbers,omitempty" json:"numbers,omitempty"`
	Dates   []string  `bson:"dates,omitempty" json:"dates,omitempty"`
	// Polarity is +1 for affirmative, -1 for negated statements.
	Polarity int `bson:"polarity" json:"polarity"`

	Method       ExtractionMethod `bson:"method" json:"method"`
	ModelVersion string           `bson:"model_version,omitempty" json:"model_version,omitempty"`
	Confidence   float64          `bson:"confidence" json:"confidence"`
	DedupeKey    string           `bson:"dedupe_key" json:"-"`

	CreatedAt time.Time `bson:"created_at" json:"created_at"`
	Deleted   bool      `bson:"deleted" json:"deleted"`
}
