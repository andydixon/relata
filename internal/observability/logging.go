// Package observability provides structured logging, request/job correlation
// IDs, and Prometheus-compatible application metrics.
//
// Logging policy: never log passwords, session tokens, API credentials, whole
// uploaded documents or embedding vectors. Content fields are recorded as
// lengths and hashes, not values.
package observability

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyJobID
	ctxKeyLogger
)

// NewLogger builds the application logger.
func NewLogger(level string, asJSON bool) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lv}
	var h slog.Handler
	if asJSON {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

// WithRequestID attaches a request correlation ID to ctx.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

// RequestID returns the correlation ID attached to ctx, if any.
func RequestID(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyRequestID).(string)
	return v
}

// WithJobID attaches a job correlation ID to ctx.
func WithJobID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyJobID, id)
}

// JobID returns the job ID attached to ctx, if any.
func JobID(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyJobID).(string)
	return v
}

// WithLogger attaches a logger to ctx.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKeyLogger, l)
}

// LoggerFrom returns the context logger, enriched with any correlation IDs.
// It never returns nil.
func LoggerFrom(ctx context.Context) *slog.Logger {
	l, _ := ctx.Value(ctxKeyLogger).(*slog.Logger)
	if l == nil {
		l = slog.Default()
	}
	if id := RequestID(ctx); id != "" {
		l = l.With("request_id", id)
	}
	if id := JobID(ctx); id != "" {
		l = l.With("job_id", id)
	}
	return l
}
