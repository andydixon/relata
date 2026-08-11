// Package embeddings provides the Embedder port's implementations.
//
// The default provider is Ollama. Everything provider-specific lives behind
// domain.Embedder so an OpenAI-compatible endpoint can be added without
// touching the pipeline.
package embeddings

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"time"
)

// RetryPolicy bounds how hard a provider call is retried. Retries are always
// finite: a permanently failing provider must surface as a job failure rather
// than an endless loop holding a worker.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

func (p RetryPolicy) withDefaults() RetryPolicy {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = 3
	}
	if p.MaxAttempts > 10 {
		p.MaxAttempts = 10
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = 500 * time.Millisecond
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = 8 * time.Second
	}
	return p
}

// PermanentError marks a failure that retrying cannot fix — a bad request, an
// unknown model, or output that will never validate.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent wraps err so callers stop retrying it.
func Permanent(err error) error { return &PermanentError{Err: err} }

// IsPermanent reports whether err should not be retried.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// Retryable classifies an error as worth another attempt. Only transport
// failures and explicitly transient HTTP statuses qualify; a 400 never does.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	if IsPermanent(err) {
		return false
	}
	// A cancelled or expired context is the caller's decision, not a blip.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		switch se.Status {
		case http.StatusTooManyRequests,
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	// Connection refused / reset while the provider restarts.
	var oe *net.OpError
	return errors.As(err, &oe)
}

// StatusError is a non-2xx provider response.
type StatusError struct {
	Status int
	Body   string
	Op     string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: provider returned HTTP %d: %s", e.Op, e.Status, e.Body)
}

// Do runs fn with bounded exponential backoff and full jitter.
//
// Jitter matters here because embedding jobs are dispatched in parallel by the
// worker pool; without it, a provider restart would be met by every worker
// retrying in lockstep.
func Do(ctx context.Context, p RetryPolicy, fn func(ctx context.Context, attempt int) error) error {
	p = p.withDefaults()
	var last error

	for attempt := 1; attempt <= p.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if last != nil {
				return fmt.Errorf("%w (last provider error: %v)", err, last)
			}
			return err
		}

		err := fn(ctx, attempt)
		if err == nil {
			return nil
		}
		last = err

		if !Retryable(err) || attempt == p.MaxAttempts {
			return err
		}

		delay := p.BaseDelay << (attempt - 1)
		if delay > p.MaxDelay || delay <= 0 {
			delay = p.MaxDelay
		}
		// Full jitter: sleep somewhere in [0, delay].
		wait := time.Duration(rand.Int63n(int64(delay) + 1))

		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("%w (last provider error: %v)", ctx.Err(), last)
		case <-t.C:
		}
	}
	return last
}
