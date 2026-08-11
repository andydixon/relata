package mongo

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/andydixon/relata/internal/domain"
)

// JobRepo is the persistent, leased job queue.
type JobRepo struct{ base }

var _ domain.JobRepository = (*JobRepo)(nil)

// maxJobHistory bounds how many attempt records one job retains.
const maxJobHistory = 10

// Enqueue adds a job to the queue.
//
// When the job carries an idempotency key, a unique partial index on pending
// jobs makes a duplicate enqueue a no-op rather than a second unit of work.
// That is what lets the pipeline re-enqueue freely after a crash.
func (r *JobRepo) Enqueue(ctx context.Context, j *domain.Job) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	if j.WorkspaceID == "" {
		return domain.ErrWorkspaceMiss
	}
	if !j.Type.Valid() {
		return domain.ErrInvalidInput
	}
	now := time.Now().UTC()
	if j.CreatedAt.IsZero() {
		j.CreatedAt = now
	}
	j.UpdatedAt = now
	if j.RunAfter.IsZero() {
		j.RunAfter = now
	}
	if j.Status == "" {
		j.Status = domain.JobPending
	}

	_, err := r.coll.InsertOne(ctx, j)
	if isDup(err) {
		// Equivalent work is already queued.
		r.observe("enqueue", started, nil)
		return nil
	}
	r.observe("enqueue", started, err)
	return translate(err)
}

// Claim atomically leases the next runnable job.
//
// A lease, not a lock: the document records who holds it and until when. A
// worker that dies stops renewing, the lease expires, and ReleaseExpired
// returns the job to the pool. Nothing external is needed to detect the crash.
func (r *JobRepo) Claim(ctx context.Context, owner string, types []domain.JobType, lease time.Duration) (*domain.Job, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	now := time.Now().UTC()
	f := bson.D{
		{Key: "status", Value: string(domain.JobPending)},
		{Key: "run_after", Value: bson.D{{Key: "$lte", Value: now}}},
	}
	if len(types) > 0 {
		vals := make([]any, 0, len(types))
		for _, t := range types {
			vals = append(vals, string(t))
		}
		f = append(f, bson.E{Key: "type", Value: bson.D{{Key: "$in", Value: vals}}})
	}

	expires := now.Add(lease)
	update := bson.D{
		{Key: "$set", Value: bson.D{
			{Key: "status", Value: string(domain.JobRunning)},
			{Key: "lease_owner", Value: owner},
			{Key: "lease_expires", Value: expires},
			{Key: "updated_at", Value: now},
		}},
		{Key: "$inc", Value: bson.D{{Key: "attempts", Value: 1}}},
	}

	opts := options.FindOneAndUpdate().
		SetSort(bson.D{{Key: "priority", Value: -1}, {Key: "run_after", Value: 1}}).
		SetReturnDocument(options.After)

	var j domain.Job
	err := r.coll.FindOneAndUpdate(ctx, f, update, opts).Decode(&j)
	if err == mongo.ErrNoDocuments {
		r.observe("claim", started, nil)
		return nil, nil // an empty queue is not an error
	}
	r.observe("claim", started, err)
	if err != nil {
		return nil, translate(err)
	}
	return &j, nil
}

// Heartbeat extends a lease. It fails if the lease was lost — for instance
// because it expired and another worker claimed the job — which is the signal
// for the original worker to abandon its work rather than write a stale result.
func (r *JobRepo) Heartbeat(ctx context.Context, id, owner string, lease time.Duration) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	now := time.Now().UTC()
	f := bson.D{
		{Key: "_id", Value: id},
		{Key: "lease_owner", Value: owner},
		{Key: "status", Value: string(domain.JobRunning)},
	}
	update := bson.D{{Key: "$set", Value: bson.D{
		{Key: "lease_expires", Value: now.Add(lease)},
		{Key: "updated_at", Value: now},
	}}}

	res, err := r.coll.UpdateOne(ctx, f, update)
	r.observe("heartbeat", started, err)
	if err != nil {
		return translate(err)
	}
	if res.MatchedCount == 0 {
		return domain.ErrConflict
	}
	return nil
}

// Complete marks a job succeeded. It is scoped to the lease owner so a worker
// that lost its lease cannot mark someone else's attempt as done.
func (r *JobRepo) Complete(ctx context.Context, id, owner string, att domain.JobAttempt) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	now := time.Now().UTC()
	f := bson.D{{Key: "_id", Value: id}, {Key: "lease_owner", Value: owner}}
	update := bson.D{
		{Key: "$set", Value: bson.D{
			{Key: "status", Value: string(domain.JobSucceeded)},
			{Key: "completed_at", Value: now},
			{Key: "updated_at", Value: now},
			{Key: "last_error", Value: ""},
			{Key: "lease_owner", Value: ""},
		}},
		{Key: "$unset", Value: bson.D{{Key: "lease_expires", Value: ""}}},
		{Key: "$push", Value: bson.D{{Key: "history", Value: bson.D{
			{Key: "$each", Value: bson.A{att}},
			{Key: "$slice", Value: -maxJobHistory},
		}}}},
	}

	res, err := r.coll.UpdateOne(ctx, f, update)
	r.observe("complete", started, err)
	if err != nil {
		return translate(err)
	}
	if res.MatchedCount == 0 {
		return domain.ErrConflict
	}
	return nil
}

// Fail records a failed attempt.
//
// When retryAfter is non-nil the job returns to pending for another attempt;
// otherwise it is terminal. The decision belongs to the worker, which knows
// whether the error was transient — the queue does not guess.
func (r *JobRepo) Fail(ctx context.Context, id, owner string, att domain.JobAttempt, retryAfter *time.Time) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	now := time.Now().UTC()
	set := bson.D{
		{Key: "updated_at", Value: now},
		{Key: "last_error", Value: att.Error},
		{Key: "lease_owner", Value: ""},
	}
	if retryAfter != nil {
		set = append(set,
			bson.E{Key: "status", Value: string(domain.JobPending)},
			bson.E{Key: "run_after", Value: *retryAfter},
		)
	} else {
		set = append(set,
			bson.E{Key: "status", Value: string(domain.JobFailed)},
			bson.E{Key: "completed_at", Value: now},
		)
	}

	f := bson.D{{Key: "_id", Value: id}, {Key: "lease_owner", Value: owner}}
	update := bson.D{
		{Key: "$set", Value: set},
		{Key: "$unset", Value: bson.D{{Key: "lease_expires", Value: ""}}},
		{Key: "$push", Value: bson.D{{Key: "history", Value: bson.D{
			{Key: "$each", Value: bson.A{att}},
			{Key: "$slice", Value: -maxJobHistory},
		}}}},
	}

	res, err := r.coll.UpdateOne(ctx, f, update)
	r.observe("fail", started, err)
	if err != nil {
		return translate(err)
	}
	if res.MatchedCount == 0 {
		return domain.ErrConflict
	}
	return nil
}

// Get fetches a job by ID.
//
// Jobs are infrastructure records rather than user content, so this is not
// workspace-scoped; callers that expose a job to a user check its workspace.
func (r *JobRepo) Get(ctx context.Context, id string) (*domain.Job, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	var j domain.Job
	err := r.coll.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&j)
	r.observe("get", started, err)
	if err != nil {
		return nil, translate(err)
	}
	return &j, nil
}

// List returns a page of jobs in a workspace.
func (r *JobRepo) List(ctx context.Context, workspaceID string, statuses []domain.JobStatus, types []domain.JobType, p domain.Page) ([]*domain.Job, int64, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, 0, err
	}
	if len(statuses) > 0 {
		vals := make([]any, 0, len(statuses))
		for _, s := range statuses {
			vals = append(vals, string(s))
		}
		f = append(f, bson.E{Key: "status", Value: bson.D{{Key: "$in", Value: vals}}})
	}
	if len(types) > 0 {
		vals := make([]any, 0, len(types))
		for _, t := range types {
			if t.Valid() {
				vals = append(vals, string(t))
			}
		}
		if len(vals) > 0 {
			f = append(f, bson.E{Key: "type", Value: bson.D{{Key: "$in", Value: vals}}})
		}
	}
	p = p.Normalise(50, 200)

	total, err := r.coll.CountDocuments(ctx, f)
	if err != nil {
		r.observe("list", started, err)
		return nil, 0, translate(err)
	}
	cur, err := r.coll.Find(ctx, f, options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetSkip(int64(p.Offset)).SetLimit(int64(p.Limit)))
	r.observe("list", started, err)
	if err != nil {
		return nil, 0, translate(err)
	}
	var out []*domain.Job
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// Retry returns a failed job to the queue and resets its attempt budget.
func (r *JobRepo) Retry(ctx context.Context, workspaceID, id string) (*domain.Job, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	f = append(f,
		bson.E{Key: "_id", Value: id},
		bson.E{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{
			string(domain.JobFailed), string(domain.JobCancelled),
		}}}},
	)

	now := time.Now().UTC()
	update := bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: string(domain.JobPending)},
		{Key: "run_after", Value: now},
		{Key: "attempts", Value: 0},
		{Key: "lease_owner", Value: ""},
		{Key: "updated_at", Value: now},
	}}}

	opts := options.FindOneAndUpdate().SetReturnDocument(options.After)
	var j domain.Job
	err = r.coll.FindOneAndUpdate(ctx, f, update, opts).Decode(&j)
	r.observe("retry", started, err)
	if err != nil {
		return nil, translate(err)
	}
	return &j, nil
}

// CountByStatus groups jobs by status, for the dashboard and metrics.
func (r *JobRepo) CountByStatus(ctx context.Context, workspaceID string) (map[domain.JobStatus]int64, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return nil, err
	}
	cur, err := r.coll.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: f}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$status"},
			{Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
	})
	r.observe("count_by_status", started, err)
	if err != nil {
		return nil, translate(err)
	}
	defer cur.Close(ctx)

	out := map[domain.JobStatus]int64{}
	for cur.Next(ctx) {
		var row struct {
			ID string `bson:"_id"`
			N  int64  `bson:"n"`
		}
		if err := cur.Decode(&row); err == nil {
			out[domain.JobStatus(row.ID)] = row.N
		}
	}
	return out, cur.Err()
}

// ReleaseExpired returns jobs whose lease elapsed to the pending pool.
//
// This is the crash-recovery path. A job that has exhausted its attempts is
// marked failed instead, so a job that reliably kills its worker cannot loop
// forever.
func (r *JobRepo) ReleaseExpired(ctx context.Context, now time.Time) (int64, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f := bson.D{
		{Key: "status", Value: string(domain.JobRunning)},
		{Key: "lease_expires", Value: bson.D{{Key: "$lt", Value: now}}},
	}

	// Jobs still within their attempt budget go back to pending.
	retryable := append(bson.D{}, f...)
	retryable = append(retryable, bson.E{Key: "$expr", Value: bson.D{
		{Key: "$lt", Value: bson.A{"$attempts", "$max_attempts"}},
	}})

	res, err := r.coll.UpdateMany(ctx, retryable, bson.D{
		{Key: "$set", Value: bson.D{
			{Key: "status", Value: string(domain.JobPending)},
			{Key: "run_after", Value: now},
			{Key: "lease_owner", Value: ""},
			{Key: "last_error", Value: "worker lease expired; the job was returned to the queue"},
			{Key: "updated_at", Value: now},
		}},
		{Key: "$unset", Value: bson.D{{Key: "lease_expires", Value: ""}}},
	})
	if err != nil {
		r.observe("release_expired", started, err)
		return 0, translate(err)
	}
	released := res.ModifiedCount

	// Jobs out of attempts are terminal.
	exhausted := append(bson.D{}, f...)
	exhausted = append(exhausted, bson.E{Key: "$expr", Value: bson.D{
		{Key: "$gte", Value: bson.A{"$attempts", "$max_attempts"}},
	}})
	_, err = r.coll.UpdateMany(ctx, exhausted, bson.D{
		{Key: "$set", Value: bson.D{
			{Key: "status", Value: string(domain.JobFailed)},
			{Key: "lease_owner", Value: ""},
			{Key: "completed_at", Value: now},
			{Key: "last_error", Value: "worker lease expired and the job has no attempts remaining"},
			{Key: "updated_at", Value: now},
		}},
		{Key: "$unset", Value: bson.D{{Key: "lease_expires", Value: ""}}},
	})
	r.observe("release_expired", started, err)
	if err != nil {
		return released, translate(err)
	}
	return released, nil
}

// DeleteForItem removes an item's queued work, used during deletion.
func (r *JobRepo) DeleteForItem(ctx context.Context, workspaceID, itemID string) (int64, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	started := time.Now()

	f, err := wsFilter(workspaceID)
	if err != nil {
		return 0, err
	}
	f = append(f,
		bson.E{Key: "item_id", Value: itemID},
		// A running job is left alone; it will notice the item is gone.
		bson.E{Key: "status", Value: bson.D{{Key: "$ne", Value: string(domain.JobRunning)}}},
		// The deletion job itself must survive.
		bson.E{Key: "type", Value: bson.D{{Key: "$ne", Value: string(domain.JobDeleteItemData)}}},
	)

	res, err := r.coll.DeleteMany(ctx, f)
	r.observe("delete_for_item", started, err)
	if err != nil {
		return 0, translate(err)
	}
	return res.DeletedCount, nil
}
