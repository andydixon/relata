// Package jobs runs Relata's background work: a bounded pool of workers
// draining a persistent, leased queue stored in MongoDB.
//
// The design goals are recoverability and boundedness. A crashed worker's job
// returns to the queue when its lease expires; no work is ever started without
// a worker slot; AI calls are additionally limited by their own semaphore; and
// shutdown drains in-flight jobs rather than abandoning them.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"time"

	"github.com/andydixon/relata/internal/analysis"
	"github.com/andydixon/relata/internal/config"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/embeddings"
	"github.com/andydixon/relata/internal/ids"
	"github.com/andydixon/relata/internal/observability"
)

// Handler executes one job. It must honour ctx cancellation and should be
// idempotent: a job may run more than once after a crash.
type Handler func(ctx context.Context, j *domain.Job) error

// Pool is a set of workers draining the queue.
type Pool struct {
	repo     domain.JobRepository
	cfg      config.WorkerConfig
	metrics  *observability.Metrics
	handlers map[domain.JobType]Handler

	// aiSem bounds concurrent AI provider calls independently of worker count,
	// so a small local model is not swamped by every worker at once.
	aiSem chan struct{}

	workerID string
	wg       sync.WaitGroup
	stopOnce sync.Once
	stopped  chan struct{}
}

// NewPool builds a worker pool. Handlers are registered before Start.
func NewPool(repo domain.JobRepository, cfg config.WorkerConfig, m *observability.Metrics) *Pool {
	host, _ := os.Hostname()
	if host == "" {
		host = "relata"
	}
	aiConc := cfg.AIConcurrency
	if aiConc <= 0 {
		aiConc = 1
	}
	return &Pool{
		repo:     repo,
		cfg:      cfg,
		metrics:  m,
		handlers: make(map[domain.JobType]Handler),
		aiSem:    make(chan struct{}, aiConc),
		workerID: fmt.Sprintf("%s-%s", host, ids.New()[:8]),
		stopped:  make(chan struct{}),
	}
}

// Register attaches a handler to a job type. It panics on an unknown type,
// which can only be a programming error and is better caught at startup.
func (p *Pool) Register(t domain.JobType, h Handler) {
	if !t.Valid() {
		panic("jobs: cannot register handler for unknown job type " + string(t))
	}
	p.handlers[t] = h
}

// AcquireAI blocks until an AI slot is free, returning a release function.
//
// Handlers call this around provider requests so that the number of concurrent
// model calls stays bounded regardless of how many workers are running.
func (p *Pool) AcquireAI(ctx context.Context) (release func(), err error) {
	select {
	case p.aiSem <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-p.aiSem }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Start launches the workers and the lease reaper.
//
// The returned context governs the pool's lifetime: cancelling it begins a
// graceful shutdown in which no new jobs are claimed and in-flight jobs are
// given until Stop's deadline to finish.
func (p *Pool) Start(ctx context.Context) {
	if !p.cfg.Enabled {
		observability.LoggerFrom(ctx).Info("background workers are disabled by configuration")
		return
	}
	n := p.cfg.Count
	if n <= 0 {
		n = 1
	}

	types := make([]domain.JobType, 0, len(p.handlers))
	for t := range p.handlers {
		types = append(types, t)
	}

	for i := 0; i < n; i++ {
		p.wg.Add(1)
		go func(idx int) {
			defer p.wg.Done()
			p.run(ctx, fmt.Sprintf("%s-w%d", p.workerID, idx), types)
		}(i)
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.reapLeases(ctx)
	}()

	observability.LoggerFrom(ctx).Info("background workers started",
		"workers", n, "ai_concurrency", cap(p.aiSem), "job_types", len(types))
}

// Stop waits for in-flight jobs to finish, up to the given timeout.
func (p *Pool) Stop(timeout time.Duration) {
	p.stopOnce.Do(func() { close(p.stopped) })

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(timeout):
		// In-flight jobs keep their leases, which expire and return them to
		// the queue for another worker to pick up.
	}
}

// run is one worker's claim-execute-report loop.
func (p *Pool) run(ctx context.Context, workerID string, types []domain.JobType) {
	log := observability.LoggerFrom(ctx).With("worker", workerID)

	// A small random offset stops every worker polling on the same tick.
	jitter := time.Duration(rand.Int63n(int64(p.cfg.PollInterval) + 1))
	timer := time.NewTimer(jitter)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopped:
			return
		case <-timer.C:
		}

		job, err := p.repo.Claim(ctx, workerID, types, p.cfg.LeaseDuration)
		if err != nil {
			if ctx.Err() == nil {
				log.Error("could not claim a job", "error", err.Error())
			}
			timer.Reset(p.cfg.PollInterval)
			continue
		}
		if job == nil {
			timer.Reset(p.cfg.PollInterval)
			continue
		}

		p.execute(ctx, workerID, job)
		// Work was found, so look again immediately rather than idling.
		timer.Reset(time.Millisecond)
	}
}

// execute runs one job with its own timeout, heartbeat and outcome reporting.
func (p *Pool) execute(ctx context.Context, workerID string, job *domain.Job) {
	log := observability.LoggerFrom(ctx).With(
		"worker", workerID, "job_id", job.ID, "job_type", string(job.Type),
		"attempt", job.Attempts, "item_id", job.ItemID)

	handler, ok := p.handlers[job.Type]
	if !ok {
		// Nothing can run this job, and retrying will not change that.
		p.finish(ctx, workerID, job, fmt.Errorf("no handler registered for job type %q", job.Type),
			domain.ErrClassPermanent, time.Now())
		return
	}

	jobCtx, cancel := context.WithTimeout(observability.WithJobID(ctx, job.ID), p.cfg.JobTimeout)
	defer cancel()

	// Renew the lease while the job runs, so a long job is not reclaimed from
	// under this worker. Losing the lease cancels the work: another worker has
	// taken over and this attempt's result is no longer wanted.
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		t := time.NewTicker(p.cfg.HeartbeatPeriod)
		defer t.Stop()
		for {
			select {
			case <-jobCtx.Done():
				return
			case <-t.C:
				if err := p.repo.Heartbeat(jobCtx, job.ID, workerID, p.cfg.LeaseDuration); err != nil {
					if errors.Is(err, domain.ErrConflict) {
						log.Warn("lease lost; abandoning this attempt")
						cancel()
						return
					}
					log.Debug("heartbeat failed", "error", err.Error())
				}
			}
		}
	}()

	started := time.Now()
	err := runGuarded(jobCtx, handler, job)

	// Classify before cancelling: the check distinguishes a job that ran out of
	// time from one cancelled here, and cancelling first would conflate them.
	class := Classify(err, jobCtx)

	// Stop the heartbeat and wait for it to exit. The cancel must come before
	// the wait — the goroutine's only exit is jobCtx being done, so waiting
	// first would block until the deferred cancel that never runs.
	cancel()
	<-hbDone
	p.finish(ctx, workerID, job, err, class, started)

	if p.metrics != nil {
		outcome := "success"
		if err != nil {
			outcome = string(class)
		}
		p.metrics.JobsProcessed.WithLabelValues(string(job.Type), outcome).Inc()
		p.metrics.JobDuration.WithLabelValues(string(job.Type)).Observe(time.Since(started).Seconds())
	}

	if err != nil {
		log.Error("job failed", "error", err.Error(), "class", string(class),
			"duration_ms", time.Since(started).Milliseconds())
	} else {
		log.Info("job completed", "duration_ms", time.Since(started).Milliseconds())
	}
}

// runGuarded executes a handler, converting a panic into an error so that one
// malformed document cannot take down a worker.
func runGuarded(ctx context.Context, h Handler, job *domain.Job) (err error) {
	defer func() {
		if recover() != nil {
			// Never persist the panic value: handler panics can include document
			// content or credentials. The job and request IDs provide correlation.
			err = errors.New("job handler panicked")
		}
	}()
	return h(ctx, job)
}

// finish reports the attempt's outcome and decides whether to retry.
func (p *Pool) finish(ctx context.Context, workerID string, job *domain.Job, err error, class domain.ErrorClass, started time.Time) {
	// Report using a fresh context: the job's context may already be cancelled,
	// and the outcome must still be recorded.
	repCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()

	att := domain.JobAttempt{
		Number:     job.Attempts,
		StartedAt:  started,
		EndedAt:    time.Now().UTC(),
		Worker:     workerID,
		DurationMS: time.Since(started).Milliseconds(),
	}

	if err == nil {
		if cerr := p.repo.Complete(repCtx, job.ID, workerID, att); cerr != nil {
			observability.LoggerFrom(ctx).Error("could not mark job complete",
				"job_id", job.ID, "error", cerr.Error())
		}
		return
	}

	att.Error = truncateError(err.Error())
	att.ErrorClass = string(class)

	maxAttempts := job.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = p.cfg.MaxAttempts
	}

	// Only transient failures are retried, and only within the attempt budget.
	// A permanent failure — invalid model output, an unparseable document, a
	// missing handler — is terminal: repeating it would waste the queue.
	var retryAfter *time.Time
	if class == domain.ErrClassTransient && job.Attempts < maxAttempts {
		delay := backoff(job.Attempts)
		t := time.Now().UTC().Add(delay)
		retryAfter = &t
	}

	if ferr := p.repo.Fail(repCtx, job.ID, workerID, att, retryAfter); ferr != nil {
		observability.LoggerFrom(ctx).Error("could not record job failure",
			"job_id", job.ID, "error", ferr.Error())
	}
}

// backoff returns the delay before attempt n+1, with jitter.
func backoff(attempt int) time.Duration {
	base := 2 * time.Second
	d := base << minInt(attempt, 6) // capped so the shift cannot overflow
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d/2 + time.Duration(rand.Int63n(int64(d/2)+1))
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Classify decides whether a failure is worth retrying.
func Classify(err error, ctx context.Context) domain.ErrorClass {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.Canceled):
		return domain.ErrClassCancelled
	case errors.Is(err, context.DeadlineExceeded):
		// A job that ran out of time may succeed with a fresh budget.
		return domain.ErrClassTimeout
	case errors.Is(err, analysis.ErrInvalidOutput):
		// Retrying a model that produced structurally invalid output tends to
		// produce more of the same; the failure is recorded instead.
		return domain.ErrClassInvalid
	case errors.Is(err, analysis.ErrDisabled):
		return domain.ErrClassPermanent
	case embeddings.IsPermanent(err):
		return domain.ErrClassPermanent
	case errors.Is(err, domain.ErrNotFound),
		errors.Is(err, domain.ErrInvalidInput),
		errors.Is(err, domain.ErrUnsupported),
		errors.Is(err, domain.ErrWorkspaceMiss):
		return domain.ErrClassPermanent
	case embeddings.Retryable(err):
		return domain.ErrClassTransient
	}
	// An unrecognised error is treated as transient: the attempt budget bounds
	// the cost, and a genuinely permanent failure will exhaust it quickly.
	return domain.ErrClassTransient
}

// reapLeases periodically returns jobs from crashed workers to the queue and
// refreshes the queue-depth gauges.
func (p *Pool) reapLeases(ctx context.Context) {
	interval := p.cfg.LeaseDuration / 2
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopped:
			return
		case <-t.C:
			released, err := p.repo.ReleaseExpired(ctx, time.Now().UTC())
			if err != nil {
				if ctx.Err() == nil {
					observability.LoggerFrom(ctx).Error("could not release expired leases", "error", err.Error())
				}
				continue
			}
			if released > 0 {
				observability.LoggerFrom(ctx).Warn(
					"returned jobs from expired leases to the queue", "count", released)
			}
		}
	}
}

// truncateError bounds an error string before it is stored. Error text can
// include untrusted content, so its length is capped.
func truncateError(s string) string {
	const max = 2000
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
