// Package audit records consequential actions to an append-only log.
//
// The recorder never writes credentials, session tokens or document bodies. It
// stores who did what to which record, when, and a small map of short
// descriptive values.
package audit

import (
	"context"

	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/ids"
	"github.com/andydixon/relata/internal/observability"
)

// Recorder writes audit events.
type Recorder struct {
	repo domain.AuditRepository
}

// NewRecorder builds an audit recorder.
func NewRecorder(repo domain.AuditRepository) *Recorder { return &Recorder{repo: repo} }

// maxDetailValue bounds each detail value, since some come from user input.
const maxDetailValue = 300

// Record appends an audit event.
//
// Failing to write an audit record must not fail the action that prompted it —
// refusing to serve a request because the audit log is unavailable would turn a
// logging outage into an outage of the whole application. The failure is logged
// at error level instead.
func (r *Recorder) Record(ctx context.Context, p *domain.Principal, action, targetKind, targetID, outcome string, detail map[string]string) {
	if r == nil || r.repo == nil {
		return
	}

	ev := &domain.AuditEvent{
		ID:         ids.New(),
		Action:     action,
		TargetKind: targetKind,
		TargetID:   targetID,
		Outcome:    outcome,
		RequestID:  observability.RequestID(ctx),
	}
	if p != nil {
		ev.WorkspaceID = p.WorkspaceID
		ev.ActorID = p.UserID
		ev.ActorName = p.Username
	}
	if len(detail) > 0 {
		ev.Detail = make(map[string]string, len(detail))
		for k, v := range detail {
			if len(v) > maxDetailValue {
				v = v[:maxDetailValue] + "…"
			}
			ev.Detail[k] = v
		}
	}

	// Use a context detached from the request so an audit write still lands
	// when the client has already disconnected.
	if err := r.repo.Record(context.WithoutCancel(ctx), ev); err != nil {
		observability.LoggerFrom(ctx).Error("could not write audit event",
			"action", action, "target_id", targetID, "error", err.Error())
	}
}

// RecordAnonymous writes an event with no authenticated actor, used for failed
// sign-in attempts where there is no principal yet.
func (r *Recorder) RecordAnonymous(ctx context.Context, workspaceID, action, outcome string, detail map[string]string) {
	if r == nil || r.repo == nil {
		return
	}
	ev := &domain.AuditEvent{
		ID:          ids.New(),
		WorkspaceID: workspaceID,
		Action:      action,
		Outcome:     outcome,
		RequestID:   observability.RequestID(ctx),
		Detail:      detail,
	}
	if err := r.repo.Record(context.WithoutCancel(ctx), ev); err != nil {
		observability.LoggerFrom(ctx).Error("could not write audit event",
			"action", action, "error", err.Error())
	}
}

// List returns a page of audit events for a workspace.
func (r *Recorder) List(ctx context.Context, workspaceID string, p domain.Page) ([]*domain.AuditEvent, int64, error) {
	return r.repo.List(ctx, workspaceID, p)
}
