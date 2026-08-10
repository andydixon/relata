package jobs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andydixon/relata/internal/analysis"
	"github.com/andydixon/relata/internal/config"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/embeddings"
)

// fakeQueue is an in-memory job repository for exercising the worker loop.
type fakeQueue struct {
	mu         sync.Mutex
	jobs       map[string]*domain.Job
	order      []string
	completed  []string
	failed     []string
	retried    []string
	heartbeats int
}

func newFakeQueue() *fakeQueue {
	return &fakeQueue{jobs: map[string]*domain.Job{}}
}

func (q *fakeQueue) Enqueue(_ context.Context, j *domain.Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.jobs[j.ID] = j
	q.order = append(q.order, j.ID)
	return nil
}

func (q *fakeQueue) Claim(_ context.Context, owner string, _ []domain.JobType, lease time.Duration) (*domain.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, id := range q.order {
		j := q.jobs[id]
		if j.Status != domain.JobPending {
			continue
		}
		j.Status = domain.JobRunning
		j.LeaseOwner = owner
		j.Attempts++
		exp := time.Now().Add(lease)
		j.LeaseExpires = &exp
		cp := *j
		return &cp, nil
	}
	return nil, nil
}

func (q *fakeQueue) Heartbeat(_ context.Context, id, owner string, _ time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.heartbeats++
	j := q.jobs[id]
	if j == nil || j.LeaseOwner != owner {
		return domain.ErrConflict
	}
	return nil
}

func (q *fakeQueue) Complete(_ context.Context, id, owner string, _ domain.JobAttempt) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	j := q.jobs[id]
	if j == nil || j.LeaseOwner != owner {
		return domain.ErrConflict
	}
	j.Status = domain.JobSucceeded
	q.completed = append(q.completed, id)
	return nil
}

func (q *fakeQueue) Fail(_ context.Context, id, owner string, att domain.JobAttempt, retryAfter *time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	j := q.jobs[id]
	if j == nil || j.LeaseOwner != owner {
		return domain.ErrConflict
	}
	if retryAfter != nil {
		j.Status = domain.JobPending
		q.retried = append(q.retried, id)
	} else {
		j.Status = domain.JobFailed
		q.failed = append(q.failed, id)
	}
	return nil
}

func (q *fakeQueue) Get(_ context.Context, id string) (*domain.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if j := q.jobs[id]; j != nil {
		cp := *j
		return &cp, nil
	}
	return nil, domain.ErrNotFound
}

func (q *fakeQueue) List(context.Context, string, []domain.JobStatus, []domain.JobType, domain.Page) ([]*domain.Job, int64, error) {
	return nil, 0, nil
}
func (q *fakeQueue) Retry(context.Context, string, string) (*domain.Job, error) { return nil, nil }
func (q *fakeQueue) CountByStatus(context.Context, string) (map[domain.JobStatus]int64, error) {
	return nil, nil
}
func (q *fakeQueue) ReleaseExpired(context.Context, time.Time) (int64, error) { return 0, nil }
func (q *fakeQueue) DeleteForItem(context.Context, string, string) (int64, error) {
	return 0, nil
}

func (q *fakeQueue) counts() (done, failed, retried int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.completed), len(q.failed), len(q.retried)
}

func testConfig() config.WorkerConfig {
	return config.WorkerConfig{
		Count:           2,
		AIConcurrency:   1,
		PollInterval:    10 * time.Millisecond,
		LeaseDuration:   2 * time.Second,
		HeartbeatPeriod: 200 * time.Millisecond,
		MaxAttempts:     3,
		JobTimeout:      5 * time.Second,
		Enabled:         true,
	}
}

func queueJob(q *fakeQueue, id string, t domain.JobType) {
	_ = q.Enqueue(context.Background(), &domain.Job{
		ID: id, WorkspaceID: "ws", Type: t, Status: domain.JobPending,
		MaxAttempts: 3, RunAfter: time.Now(),
	})
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// This is the regression test for a deadlock in which the worker waited for its
// heartbeat goroutine to exit, while that goroutine waited for a context that
// was only cancelled once the worker returned. Every worker wedged after one
// job and the queue stopped draining.
func TestWorkerCompletesJobsAndDoesNotDeadlock(t *testing.T) {
	q := newFakeQueue()
	pool := NewPool(q, testConfig(), nil)

	var ran int
	var mu sync.Mutex
	pool.Register(domain.JobParseItem, func(ctx context.Context, j *domain.Job) error {
		mu.Lock()
		ran++
		mu.Unlock()
		return nil
	})

	// More jobs than workers: if a worker wedges after its first job, the
	// remainder never complete.
	for i := 0; i < 8; i++ {
		queueJob(q, string(rune('a'+i)), domain.JobParseItem)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Stop(2 * time.Second)

	waitFor(t, 5*time.Second, "all 8 jobs to complete", func() bool {
		done, _, _ := q.counts()
		return done == 8
	})

	mu.Lock()
	defer mu.Unlock()
	if ran != 8 {
		t.Fatalf("handler ran %d times, want 8", ran)
	}
}

func TestWorkerRetriesTransientFailures(t *testing.T) {
	q := newFakeQueue()
	pool := NewPool(q, testConfig(), nil)

	pool.Register(domain.JobParseItem, func(context.Context, *domain.Job) error {
		// A network-shaped error is transient and should be retried.
		return &embeddings.StatusError{Status: 503, Op: "test", Body: "unavailable"}
	})
	queueJob(q, "j1", domain.JobParseItem)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Stop(2 * time.Second)

	waitFor(t, 5*time.Second, "the job to be retried", func() bool {
		_, _, retried := q.counts()
		return retried >= 1
	})
}

func TestWorkerDoesNotRetryPermanentFailures(t *testing.T) {
	q := newFakeQueue()
	pool := NewPool(q, testConfig(), nil)

	pool.Register(domain.JobParseItem, func(context.Context, *domain.Job) error {
		// Invalid model output will never become valid by repeating the call.
		return analysis.Reject("task", analysis.ReasonBadEnum, "nope", "raw")
	})
	queueJob(q, "j1", domain.JobParseItem)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Stop(2 * time.Second)

	waitFor(t, 5*time.Second, "the job to fail terminally", func() bool {
		_, failed, _ := q.counts()
		return failed == 1
	})
	if _, _, retried := q.counts(); retried != 0 {
		t.Fatalf("a permanently invalid job was retried %d times", retried)
	}
}

func TestWorkerSurvivesAPanickingHandler(t *testing.T) {
	q := newFakeQueue()
	pool := NewPool(q, testConfig(), nil)

	pool.Register(domain.JobParseItem, func(context.Context, *domain.Job) error {
		panic("a malformed document exploded")
	})
	pool.Register(domain.JobChunkItem, func(context.Context, *domain.Job) error { return nil })

	queueJob(q, "boom", domain.JobParseItem)
	queueJob(q, "fine", domain.JobChunkItem)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Stop(2 * time.Second)

	// The panicking job must not take the worker down: the other job still runs.
	waitFor(t, 5*time.Second, "the healthy job to complete despite a panic", func() bool {
		done, _, _ := q.counts()
		return done == 1
	})
}

func TestRunGuardedDoesNotPersistPanicValue(t *testing.T) {
	err := runGuarded(context.Background(), func(context.Context, *domain.Job) error {
		panic("bearer credential rlt_v1_secret")
	}, &domain.Job{ID: "job-1"})
	if err == nil {
		t.Fatal("panic was not converted to an error")
	}
	if strings.Contains(err.Error(), "rlt_v1_secret") {
		t.Fatalf("panic value reached the persistent job error: %q", err)
	}
}

func TestUnregisteredJobTypeFailsPermanently(t *testing.T) {
	q := newFakeQueue()
	pool := NewPool(q, testConfig(), nil)
	pool.Register(domain.JobParseItem, func(context.Context, *domain.Job) error { return nil })

	// A job whose type nothing handles must not spin in the queue.
	queueJob(q, "orphan", domain.JobExtractClaims)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Stop(2 * time.Second)

	waitFor(t, 5*time.Second, "the unhandled job to fail", func() bool {
		_, failed, _ := q.counts()
		return failed == 1
	})
}

func TestAIConcurrencyIsBounded(t *testing.T) {
	cfg := testConfig()
	cfg.Count = 4
	cfg.AIConcurrency = 2

	q := newFakeQueue()
	pool := NewPool(q, cfg, nil)

	var mu sync.Mutex
	var inFlight, peak int

	pool.Register(domain.JobEmbedItem, func(ctx context.Context, j *domain.Job) error {
		release, err := pool.AcquireAI(ctx)
		if err != nil {
			return err
		}
		defer release()

		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()

		time.Sleep(80 * time.Millisecond)

		mu.Lock()
		inFlight--
		mu.Unlock()
		return nil
	})

	for i := 0; i < 8; i++ {
		queueJob(q, string(rune('a'+i)), domain.JobEmbedItem)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)
	defer pool.Stop(3 * time.Second)

	waitFor(t, 8*time.Second, "all embedding jobs to finish", func() bool {
		done, _, _ := q.counts()
		return done == 8
	})

	mu.Lock()
	defer mu.Unlock()
	if peak > cfg.AIConcurrency {
		t.Fatalf("%d concurrent AI calls observed, limit is %d", peak, cfg.AIConcurrency)
	}
	if peak < 2 {
		t.Fatalf("AI calls never overlapped (peak %d); the semaphore may be serialising everything", peak)
	}
}

func TestStopDrainsInFlightWork(t *testing.T) {
	q := newFakeQueue()
	pool := NewPool(q, testConfig(), nil)

	started := make(chan struct{})
	var once sync.Once
	pool.Register(domain.JobParseItem, func(ctx context.Context, j *domain.Job) error {
		once.Do(func() { close(started) })
		time.Sleep(150 * time.Millisecond)
		return nil
	})
	queueJob(q, "j1", domain.JobParseItem)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)

	<-started
	pool.Stop(3 * time.Second)

	// A job that was running when shutdown began should have been finished,
	// not abandoned.
	done, _, _ := q.counts()
	if done != 1 {
		t.Fatalf("in-flight job was not completed during shutdown (completed=%d)", done)
	}
}

func TestClassifyDistinguishesFailureKinds(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name string
		err  error
		want domain.ErrorClass
	}{
		{"nil", nil, ""},
		{"cancelled", context.Canceled, domain.ErrClassCancelled},
		{"timeout", context.DeadlineExceeded, domain.ErrClassTimeout},
		{"invalid model output", analysis.Reject("t", "r", "d", ""), domain.ErrClassInvalid},
		{"analysis disabled", analysis.ErrDisabled, domain.ErrClassPermanent},
		{"not found", domain.ErrNotFound, domain.ErrClassPermanent},
		{"invalid input", domain.ErrInvalidInput, domain.ErrClassPermanent},
		{"permanent provider error", embeddings.Permanent(errors.New("bad model")), domain.ErrClassPermanent},
		{"503 from provider", &embeddings.StatusError{Status: 503}, domain.ErrClassTransient},
		{"unknown", errors.New("something odd"), domain.ErrClassTransient},
	}
	for _, c := range cases {
		if got := Classify(c.err, ctx); got != c.want {
			t.Errorf("Classify(%s) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBackoffIsBoundedAndJittered(t *testing.T) {
	seen := map[time.Duration]bool{}
	for attempt := 1; attempt <= 12; attempt++ {
		for i := 0; i < 20; i++ {
			d := backoff(attempt)
			if d < 0 || d > 5*time.Minute {
				t.Fatalf("backoff(%d) = %v, outside the expected bounds", attempt, d)
			}
			seen[d] = true
		}
	}
	// Jitter means repeated calls must not all return the same value, or a
	// provider restart would be met by every worker retrying in lockstep.
	if len(seen) < 10 {
		t.Fatalf("backoff produced only %d distinct delays; jitter is not being applied", len(seen))
	}
}
