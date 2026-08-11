package domain

import "time"

// JobType names a unit of background work.
type JobType string

const (
	JobParseItem      JobType = "parse_item"
	JobChunkItem      JobType = "chunk_item"
	JobEmbedItem      JobType = "generate_embeddings"
	JobExtractEntity  JobType = "extract_entities"
	JobExtractClaims  JobType = "extract_claims"
	JobDiscoverRels   JobType = "discover_relationships"
	JobReprocessItem  JobType = "reprocess_item"
	JobDeleteItemData JobType = "delete_item_data"
	JobReembedWS      JobType = "reembed_workspace"
)

// ValidJobTypes is the closed set of job types the workers understand.
var ValidJobTypes = []JobType{
	JobParseItem, JobChunkItem, JobEmbedItem, JobExtractEntity, JobExtractClaims,
	JobDiscoverRels, JobReprocessItem, JobDeleteItemData, JobReembedWS,
}

// Valid reports whether t is a recognised job type.
func (t JobType) Valid() bool {
	for _, v := range ValidJobTypes {
		if v == t {
			return true
		}
	}
	return false
}

// JobStatus is a job's lifecycle state.
type JobStatus string

const (
	JobPending   JobStatus = "pending"
	JobRunning   JobStatus = "running"
	JobSucceeded JobStatus = "succeeded"
	JobFailed    JobStatus = "failed"
	JobCancelled JobStatus = "cancelled"
)

// JobAttempt records one execution attempt and its outcome.
type JobAttempt struct {
	Number    int       `bson:"number" json:"number"`
	StartedAt time.Time `bson:"started_at" json:"started_at"`
	EndedAt   time.Time `bson:"ended_at,omitempty" json:"ended_at,omitempty"`
	Worker    string    `bson:"worker,omitempty" json:"worker,omitempty"`
	Error     string    `bson:"error,omitempty" json:"error,omitempty"`
	// ErrorClass groups failures for metrics: transient, permanent, timeout, cancelled.
	ErrorClass string `bson:"error_class,omitempty" json:"error_class,omitempty"`
	DurationMS int64  `bson:"duration_ms" json:"duration_ms"`
}

// Job is a persistent, leased unit of background work.
//
// Leases (not locks) are used so that a crashed worker's job becomes runnable
// again once its lease expires, without any external coordination.
type Job struct {
	ID          string `bson:"_id" json:"id"`
	WorkspaceID string `bson:"workspace_id" json:"workspace_id"`

	Type   JobType   `bson:"type" json:"type"`
	Status JobStatus `bson:"status" json:"status"`

	// ItemID is the primary subject for item-scoped jobs.
	ItemID string `bson:"item_id,omitempty" json:"item_id,omitempty"`
	// Payload carries small typed job parameters. It never holds document bodies.
	Payload map[string]string `bson:"payload,omitempty" json:"payload,omitempty"`

	Priority    int       `bson:"priority" json:"priority"`
	RunAfter    time.Time `bson:"run_after" json:"run_after"`
	Attempts    int       `bson:"attempts" json:"attempts"`
	MaxAttempts int       `bson:"max_attempts" json:"max_attempts"`

	LeaseOwner   string     `bson:"lease_owner,omitempty" json:"lease_owner,omitempty"`
	LeaseExpires *time.Time `bson:"lease_expires,omitempty" json:"lease_expires,omitempty"`

	History   []JobAttempt `bson:"history,omitempty" json:"history,omitempty"`
	LastError string       `bson:"last_error,omitempty" json:"last_error,omitempty"`

	// IdempotencyKey prevents duplicate enqueues of the same pending work.
	IdempotencyKey string `bson:"idempotency_key,omitempty" json:"-"`

	CreatedAt   time.Time  `bson:"created_at" json:"created_at"`
	UpdatedAt   time.Time  `bson:"updated_at" json:"updated_at"`
	CompletedAt *time.Time `bson:"completed_at,omitempty" json:"completed_at,omitempty"`
	RunID       string     `bson:"run_id,omitempty" json:"run_id,omitempty"`
}

// Terminal reports whether the job will not run again without manual action.
func (j *Job) Terminal() bool {
	return j.Status == JobSucceeded || j.Status == JobFailed || j.Status == JobCancelled
}

// ErrorClass categorises a failure for retry decisions and metrics.
type ErrorClass string

const (
	ErrClassTransient ErrorClass = "transient"
	ErrClassPermanent ErrorClass = "permanent"
	ErrClassTimeout   ErrorClass = "timeout"
	ErrClassCancelled ErrorClass = "cancelled"
	ErrClassInvalid   ErrorClass = "invalid_model_output"
)
