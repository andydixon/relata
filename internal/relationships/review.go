package relationships

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/andydixon/relata/internal/audit"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/ids"
)

// Reviewer records human decisions about relationships.
//
// Every decision is additive. Rejecting a relationship does not delete it, and
// editing its type does not erase what was originally proposed: the original
// type and provenance are preserved on the record and every action appends to
// an immutable history. A user must be able to ask "what did the system think,
// and who overruled it?" months later.
type Reviewer struct {
	rels  domain.RelationshipRepository
	items domain.ItemRepository
	audit *audit.Recorder
}

// NewReviewer builds the review service.
func NewReviewer(rels domain.RelationshipRepository, items domain.ItemRepository, rec *audit.Recorder) *Reviewer {
	return &Reviewer{rels: rels, items: items, audit: rec}
}

const maxNoteLength = 2000

// Confirm records that a person agrees with a relationship.
//
// The confirmation is added as a signal, so the score rises through the same
// arithmetic as everything else rather than being overwritten with a magic
// number, and the provenance becomes user_confirmed.
func (r *Reviewer) Confirm(ctx context.Context, p *domain.Principal, id, note string) (*domain.Relationship, error) {
	rel, err := r.load(ctx, p, id)
	if err != nil {
		return nil, err
	}

	signals := stripUserSignals(rel.Evidence.Signals)
	signals = append(signals, UserConfirmedSignal(actorName(p)))
	rel.Evidence.Signals = Normalise(signals)
	rel.Score = Score(rel.Evidence.Signals)
	rel.ScoreVersion = ScoreVersion
	rel.Provenance = domain.ProvUserConfirmed
	rel.UpdatedAt = time.Now().UTC()

	if err := r.rels.Update(ctx, rel); err != nil {
		return nil, err
	}

	ev := domain.ReviewEvent{
		At: time.Now().UTC(), Action: "confirm",
		ActorID: p.UserID, ActorName: p.Username,
		Note:      cleanNote(note),
		FromValue: string(rel.OriginalProvenance),
		ToValue:   string(domain.ProvUserConfirmed),
	}
	out, err := r.rels.AppendReview(ctx, p.WorkspaceID, id, ev, domain.ReviewConfirmed, rel.Type, domain.ProvUserConfirmed)
	if err != nil {
		return nil, err
	}

	r.audit.Record(ctx, p, domain.AuditRelConfirmed, "relationship", id, "success", map[string]string{
		"type": string(out.Type), "score": fmt.Sprintf("%.3f", out.Score),
	})
	return out, nil
}

// Reject records that a person disagrees with a relationship.
//
// The relationship and its evidence are kept. Its score falls to zero through
// the rejection signal, and it stops influencing search expansion — but the
// record of what was proposed, and who rejected it, survives.
func (r *Reviewer) Reject(ctx context.Context, p *domain.Principal, id, note string) (*domain.Relationship, error) {
	rel, err := r.load(ctx, p, id)
	if err != nil {
		return nil, err
	}

	signals := stripUserSignals(rel.Evidence.Signals)
	signals = append(signals, UserRejectedSignal(actorName(p)))
	rel.Evidence.Signals = Normalise(signals)
	rel.Score = Score(rel.Evidence.Signals)
	rel.ScoreVersion = ScoreVersion
	rel.Provenance = domain.ProvUserRejected
	rel.UpdatedAt = time.Now().UTC()

	if err := r.rels.Update(ctx, rel); err != nil {
		return nil, err
	}

	ev := domain.ReviewEvent{
		At: time.Now().UTC(), Action: "reject",
		ActorID: p.UserID, ActorName: p.Username,
		Note:      cleanNote(note),
		FromValue: string(rel.OriginalProvenance),
		ToValue:   string(domain.ProvUserRejected),
	}
	out, err := r.rels.AppendReview(ctx, p.WorkspaceID, id, ev, domain.ReviewRejected, rel.Type, domain.ProvUserRejected)
	if err != nil {
		return nil, err
	}

	r.audit.Record(ctx, p, domain.AuditRelRejected, "relationship", id, "success", map[string]string{
		"type": string(out.Type), "original_type": string(out.OriginalType),
	})
	return out, nil
}

// ChangeType records that a person corrected the relationship's type.
func (r *Reviewer) ChangeType(ctx context.Context, p *domain.Principal, id string, newType domain.RelationshipType, note string) (*domain.Relationship, error) {
	if !newType.Valid() {
		return nil, fmt.Errorf("%w: %q is not a recognised relationship type", domain.ErrInvalidInput, newType)
	}
	rel, err := r.load(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if rel.Type == newType {
		return rel, nil
	}
	previous := rel.Type

	// A person correcting the type is asserting it, so the link becomes
	// user-confirmed with a confirmation signal behind its score.
	signals := stripUserSignals(rel.Evidence.Signals)
	signals = append(signals, UserConfirmedSignal(actorName(p)))
	rel.Evidence.Signals = Normalise(signals)
	rel.Score = Score(rel.Evidence.Signals)
	rel.ScoreVersion = ScoreVersion
	rel.Type = newType
	rel.Provenance = domain.ProvUserConfirmed
	rel.UpdatedAt = time.Now().UTC()

	if err := r.rels.Update(ctx, rel); err != nil {
		return nil, err
	}

	ev := domain.ReviewEvent{
		At: time.Now().UTC(), Action: "change_type",
		ActorID: p.UserID, ActorName: p.Username,
		Note:      cleanNote(note),
		FromValue: string(previous),
		ToValue:   string(newType),
	}
	out, err := r.rels.AppendReview(ctx, p.WorkspaceID, id, ev, domain.ReviewEdited, newType, domain.ProvUserConfirmed)
	if err != nil {
		return nil, err
	}

	r.audit.Record(ctx, p, domain.AuditRelEdited, "relationship", id, "success", map[string]string{
		"from": string(previous), "to": string(newType),
	})
	return out, nil
}

// CreateManual records a relationship a person asserted directly.
//
// It is stored with explicit provenance and confirmed review status: a human
// statement is not a proposal awaiting review.
func (r *Reviewer) CreateManual(ctx context.Context, p *domain.Principal,
	sourceID, targetID string, relType domain.RelationshipType, note string) (*domain.Relationship, error) {

	if p == nil || !p.Role.CanWrite() {
		return nil, domain.ErrForbidden
	}
	if !relType.Valid() {
		return nil, fmt.Errorf("%w: %q is not a recognised relationship type", domain.ErrInvalidInput, relType)
	}
	if sourceID == targetID {
		return nil, fmt.Errorf("%w: an item cannot be related to itself", domain.ErrInvalidInput)
	}
	if !ids.Valid(sourceID) || !ids.Valid(targetID) {
		return nil, fmt.Errorf("%w: item identifiers must be UUIDs", domain.ErrInvalidInput)
	}

	// Both endpoints must exist IN THIS WORKSPACE. This is the check that stops
	// a relationship being created across the workspace boundary.
	src, err := r.items.Get(ctx, p.WorkspaceID, sourceID)
	if err != nil {
		return nil, fmt.Errorf("source item: %w", err)
	}
	tgt, err := r.items.Get(ctx, p.WorkspaceID, targetID)
	if err != nil {
		return nil, fmt.Errorf("target item: %w", err)
	}
	if src.Deleted || tgt.Deleted {
		return nil, fmt.Errorf("%w: one of the items has been deleted", domain.ErrNotFound)
	}

	s, t := sourceID, targetID
	if !relType.Directed() && s > t {
		s, t = t, s
	}

	now := time.Now().UTC()
	signals := Normalise([]domain.Signal{UserConfirmedSignal(actorName(p))})
	rel := &domain.Relationship{
		ID:           ids.New(),
		WorkspaceID:  p.WorkspaceID,
		SourceKind:   domain.NodeItem,
		SourceID:     s,
		TargetKind:   domain.NodeItem,
		TargetID:     t,
		Type:         relType,
		Provenance:   domain.ProvExplicit,
		Score:        Score(signals),
		ScoreVersion: ScoreVersion,
		Evidence: domain.Evidence{
			Signals:       signals,
			SourceExcerpt: excerpt(src.ExtractedText, 300),
			TargetExcerpt: excerpt(tgt.ExtractedText, 300),
		},
		OriginalType:       relType,
		OriginalProvenance: domain.ProvExplicit,
		ReviewStatus:       domain.ReviewConfirmed,
		ReviewHistory: []domain.ReviewEvent{{
			At: now, Action: "create", ActorID: p.UserID, ActorName: p.Username,
			Note: cleanNote(note), ToValue: string(relType),
		}},
		DedupeKey: PairKey(p.WorkspaceID, s, t),
		CreatedAt: now,
		UpdatedAt: now,
	}

	// Upsert rather than insert: if analysis already proposed a link between
	// this pair, the person's assertion replaces the proposal instead of
	// creating a second edge.
	out, err := r.upsertManual(ctx, rel)
	if err != nil {
		return nil, err
	}

	r.audit.Record(ctx, p, domain.AuditRelCreated, "relationship", out.ID, "success", map[string]string{
		"type": string(relType), "source": s, "target": t,
	})
	return out, nil
}

// upsertManual writes a user-asserted relationship over any existing proposal.
//
// The existing link is looked up by dedupe key rather than by scanning the
// node's relationships: a well-connected item has more than one page of them, so
// a paged scan would miss the match and then collide with the unique index.
//
// A person asserting a relationship is the authority on it, so this deliberately
// overwrites type, provenance and review status — unlike UpsertByDedupeKey,
// which protects a human decision from a later analysis run.
func (r *Reviewer) upsertManual(ctx context.Context, rel *domain.Relationship) (*domain.Relationship, error) {
	existing, err := r.rels.FindByDedupeKey(ctx, rel.WorkspaceID, rel.DedupeKey)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}
	if existing != nil {
		return r.applyManual(ctx, existing, rel)
	}

	if err := r.rels.Create(ctx, rel); err != nil {
		// Another request created the same link between the lookup and this
		// insert. The unique index caught it; fold this assertion into the
		// record that won rather than failing the user's action.
		if errors.Is(err, domain.ErrDuplicate) {
			winner, findErr := r.rels.FindByDedupeKey(ctx, rel.WorkspaceID, rel.DedupeKey)
			if findErr == nil && winner != nil {
				return r.applyManual(ctx, winner, rel)
			}
		}
		return nil, err
	}
	return rel, nil
}

// applyManual overlays a user assertion onto an existing relationship record,
// keeping the original identity and appending to the review history rather than
// replacing it.
func (r *Reviewer) applyManual(ctx context.Context, existing, rel *domain.Relationship) (*domain.Relationship, error) {
	existing.Type = rel.Type
	existing.Provenance = domain.ProvExplicit
	existing.Score = rel.Score
	existing.ScoreVersion = rel.ScoreVersion
	existing.Evidence.Signals = rel.Evidence.Signals
	existing.ReviewStatus = domain.ReviewConfirmed
	existing.ReviewHistory = append(existing.ReviewHistory, rel.ReviewHistory...)
	existing.UpdatedAt = rel.UpdatedAt
	if err := r.rels.Update(ctx, existing); err != nil {
		return nil, err
	}
	return existing, nil
}

// load fetches a relationship, enforcing the workspace boundary.
func (r *Reviewer) load(ctx context.Context, p *domain.Principal, id string) (*domain.Relationship, error) {
	if p == nil || p.WorkspaceID == "" {
		return nil, domain.ErrWorkspaceMiss
	}
	if !p.Role.CanWrite() {
		return nil, domain.ErrForbidden
	}
	if !ids.Valid(id) {
		return nil, fmt.Errorf("%w: relationship id must be a UUID", domain.ErrInvalidInput)
	}
	return r.rels.Get(ctx, p.WorkspaceID, id)
}

// stripUserSignals removes previous human-decision signals so that changing a
// decision replaces it rather than stacking a confirmation on a rejection.
func stripUserSignals(signals []domain.Signal) []domain.Signal {
	out := signals[:0:0]
	for _, s := range signals {
		if s.Type == domain.SigUserConfirmed || s.Type == domain.SigUserRejected {
			continue
		}
		out = append(out, s)
	}
	return out
}

func actorName(p *domain.Principal) string {
	if p == nil {
		return "unknown"
	}
	if p.DisplayName != "" {
		return p.DisplayName
	}
	return p.Username
}

// cleanNote bounds and sanitises a user-supplied note. It is stored as plain
// text and escaped on output; this only keeps it a sane size and free of
// control characters.
func cleanNote(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if len(s) > maxNoteLength {
		s = s[:maxNoteLength]
	}
	return s
}
