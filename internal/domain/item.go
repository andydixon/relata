// Package domain holds Relata's core types and the port interfaces the service
// layer depends on. It imports only the standard library: no MongoDB, Qdrant,
// Ollama or HTTP types may appear here.
package domain

import (
	"errors"
	"time"
)

// SourceType classifies where an item came from.
type SourceType string

const (
	SourceTypeNote       SourceType = "note"
	SourceTypeDocument   SourceType = "document"
	SourceTypeJSON       SourceType = "json"
	SourceTypeEmail      SourceType = "email"
	SourceTypeTranscript SourceType = "transcript"
	SourceTypeMarkdown   SourceType = "markdown"
)

// ValidSourceTypes enumerates every accepted source type.
var ValidSourceTypes = []SourceType{
	SourceTypeNote, SourceTypeDocument, SourceTypeJSON,
	SourceTypeEmail, SourceTypeTranscript, SourceTypeMarkdown,
}

// Valid reports whether s is a recognised source type.
func (s SourceType) Valid() bool {
	for _, v := range ValidSourceTypes {
		if v == s {
			return true
		}
	}
	return false
}

// ProcessingStatus tracks an item's progress through the pipeline.
type ProcessingStatus string

const (
	StatusPending    ProcessingStatus = "pending"
	StatusParsing    ProcessingStatus = "parsing"
	StatusChunking   ProcessingStatus = "chunking"
	StatusEmbedding  ProcessingStatus = "embedding"
	StatusAnalysing  ProcessingStatus = "analysing"
	StatusRelating   ProcessingStatus = "relating"
	StatusReady      ProcessingStatus = "ready"
	StatusFailed     ProcessingStatus = "failed"
	StatusPartial    ProcessingStatus = "partial"
	StatusDeleting   ProcessingStatus = "deleting"
	StatusTombstoned ProcessingStatus = "tombstoned"
)

// ParseWarning records a non-fatal problem encountered while extracting text.
// Warnings are retained on the item so that a degraded extraction is visible
// rather than silently accepted.
type ParseWarning struct {
	Code    string `bson:"code" json:"code"`
	Message string `bson:"message" json:"message"`
	Detail  string `bson:"detail,omitempty" json:"detail,omitempty"`
}

// Well-known parse warning codes.
const (
	WarnLowExtractableText = "low_extractable_text"
	WarnParseFailed        = "parse_failed"
	WarnTruncatedText      = "truncated_text"
	WarnTruncatedChunks    = "truncated_chunks"
	WarnJSONDepthExceeded  = "json_depth_exceeded"
	WarnEncodingRepaired   = "encoding_repaired"
	WarnNoPageInfo         = "no_page_info"
)

// Item is a complete piece of original information supplied by a user.
// The original content is never rewritten; everything else is derived.
type Item struct {
	ID          string `bson:"_id" json:"id"`
	WorkspaceID string `bson:"workspace_id" json:"workspace_id"`

	Title      string     `bson:"title" json:"title"`
	SourceType SourceType `bson:"source_type" json:"source_type"`
	MIMEType   string     `bson:"mime_type" json:"mime_type"`

	// OriginalFilename is the sanitised upload name; OriginalStoredPath points at
	// the preserved bytes on disk for binary uploads (PDF, DOCX).
	OriginalFilename   string `bson:"original_filename,omitempty" json:"original_filename,omitempty"`
	OriginalStoredPath string `bson:"original_stored_path,omitempty" json:"-"`
	// OriginalText holds the verbatim user-supplied text for text-shaped inputs.
	OriginalText string `bson:"original_text,omitempty" json:"original_text,omitempty"`
	SizeBytes    int64  `bson:"size_bytes" json:"size_bytes"`

	// ExtractedText is the parser's output. For plain text it equals OriginalText.
	ExtractedText string `bson:"extracted_text,omitempty" json:"extracted_text,omitempty"`

	ContentHash string            `bson:"content_hash" json:"content_hash"`
	Metadata    map[string]string `bson:"metadata,omitempty" json:"metadata,omitempty"`

	// CreatedAt is the user-asserted creation time of the information itself
	// (may predate ingestion); IngestedAt is when Relata received it.
	CreatedAt  time.Time `bson:"created_at" json:"created_at"`
	IngestedAt time.Time `bson:"ingested_at" json:"ingested_at"`
	UpdatedAt  time.Time `bson:"updated_at" json:"updated_at"`

	Status   ProcessingStatus `bson:"status" json:"status"`
	Warnings []ParseWarning   `bson:"warnings,omitempty" json:"warnings,omitempty"`
	Error    string           `bson:"error,omitempty" json:"error,omitempty"`

	ParserVersion    string `bson:"parser_version,omitempty" json:"parser_version,omitempty"`
	EmbeddingModel   string `bson:"embedding_model,omitempty" json:"embedding_model,omitempty"`
	AnalysisModel    string `bson:"analysis_model,omitempty" json:"analysis_model,omitempty"`
	ScoreVersion     string `bson:"score_version,omitempty" json:"score_version,omitempty"`
	ChunkCount       int    `bson:"chunk_count" json:"chunk_count"`
	EntityCount      int    `bson:"entity_count" json:"entity_count"`
	ClaimCount       int    `bson:"claim_count" json:"claim_count"`
	RelationshipSeen int    `bson:"relationship_count" json:"relationship_count"`

	// DuplicateOf is set when the item was knowingly kept alongside an identical
	// existing item. It is informational; the duplicate is still fully processed.
	// The bson tag deliberately omits `omitempty`: the uniqueness index that
	// enforces duplicate detection is a partial index keyed on this field being
	// the empty string, and an absent field would not match it.
	DuplicateOf string `bson:"duplicate_of" json:"duplicate_of,omitempty"`

	CreatedBy string `bson:"created_by,omitempty" json:"created_by,omitempty"`
	Deleted   bool   `bson:"deleted" json:"deleted"`
}

// HasWarning reports whether the item carries a warning with the given code.
func (i *Item) HasWarning(code string) bool {
	for _, w := range i.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// Chunk is a retrievable section of an item.
type Chunk struct {
	ID          string `bson:"_id" json:"id"`
	WorkspaceID string `bson:"workspace_id" json:"workspace_id"`
	ItemID      string `bson:"item_id" json:"item_id"`

	Sequence    int    `bson:"sequence" json:"sequence"`
	Text        string `bson:"text" json:"text"`
	ContentHash string `bson:"content_hash" json:"content_hash"`

	StartOffset int `bson:"start_offset" json:"start_offset"`
	EndOffset   int `bson:"end_offset" json:"end_offset"`

	// Page is 1-based and only set for PDF-derived chunks.
	Page    int    `bson:"page,omitempty" json:"page,omitempty"`
	Section string `bson:"section,omitempty" json:"section,omitempty"`
	Heading string `bson:"heading,omitempty" json:"heading,omitempty"`

	EmbeddingModel string    `bson:"embedding_model,omitempty" json:"embedding_model,omitempty"`
	Embedded       bool      `bson:"embedded" json:"embedded"`
	CreatedAt      time.Time `bson:"created_at" json:"created_at"`
}

// Errors returned by repositories and services.
var (
	ErrNotFound      = errors.New("not found")
	ErrDuplicate     = errors.New("duplicate content")
	ErrForbidden     = errors.New("forbidden")
	ErrInvalidInput  = errors.New("invalid input")
	ErrTooLarge      = errors.New("payload too large")
	ErrUnsupported   = errors.New("unsupported content type")
	ErrConflict      = errors.New("conflict")
	ErrRateLimited   = errors.New("rate limited")
	ErrModelInvalid  = errors.New("model returned invalid output")
	ErrUnavailable   = errors.New("dependency unavailable")
	ErrWorkspaceMiss = errors.New("workspace mismatch")
)
