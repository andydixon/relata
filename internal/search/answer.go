package search

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/andydixon/relata/internal/analysis"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/observability"
)

// AnswerRequest asks a question of the stored material.
type AnswerRequest struct {
	Question string
	Query
}

// Answer is a generated answer, restricted to stored material and cited.
type Answer struct {
	Question string `json:"question"`
	// Answer is the generated prose. It is rendered as escaped text, never HTML.
	Answer string `json:"answer"`
	// Sufficient reports whether the stored material actually supports an
	// answer. When false, the answer says so rather than guessing.
	Sufficient bool `json:"sufficient"`

	// Sources used — the stored items the answer draws on.
	Sources []AnswerSource `json:"sources_used"`
	// Uncertainty and conflicts — disagreements between sources, and any
	// caveats about what the answer does and does not establish.
	Uncertainty []string `json:"uncertainty_and_conflicts,omitempty"`

	// InferredRelationshipsUsed lists links that informed retrieval but are
	// model proposals, not verified facts.
	InferredRelationshipsUsed []RelatedItem `json:"inferred_relationships_used,omitempty"`

	Model     string `json:"model,omitempty"`
	Retrieved int    `json:"retrieved_items"`
	TookMS    int64  `json:"took_ms"`
	// InjectionNoticed reports that stored content tried to instruct the model.
	InjectionNoticed bool `json:"prompt_injection_noticed,omitempty"`
}

// AnswerSource is one cited stored item.
type AnswerSource struct {
	Ref     string `json:"ref"`
	ItemID  string `json:"item_id"`
	Title   string `json:"title"`
	Excerpt string `json:"excerpt"`
	Date    string `json:"date,omitempty"`
	// Cited reports whether the generated answer actually referenced it.
	Cited bool `json:"cited"`
}

// ErrAnswerDisabled is returned when answer generation is switched off.
var ErrAnswerDisabled = fmt.Errorf("answer generation is not enabled")

// Answerer generates answers from stored material.
type Answerer struct {
	search   *Service
	analysis *analysis.Service
	claims   domain.ClaimRepository
	enabled  bool
}

// NewAnswerer builds the answer service.
func NewAnswerer(s *Service, an *analysis.Service, claims domain.ClaimRepository, enabled bool) *Answerer {
	return &Answerer{search: s, analysis: an, claims: claims, enabled: enabled}
}

// Enabled reports whether answers can be generated.
func (a *Answerer) Enabled() bool {
	return a != nil && a.enabled && a.analysis.Enabled()
}

// Answer retrieves relevant material and generates a cited answer from it.
//
// Several guarantees hold regardless of what the model returns:
//   - the answer is built only from retrieved passages;
//   - citations are resolved against the passages actually supplied, so a
//     fabricated reference cannot point at a real item;
//   - conflicting sources are surfaced, never reconciled away;
//   - any inferred relationship that influenced retrieval is listed as a
//     proposal rather than presented as fact.
func (a *Answerer) Answer(ctx context.Context, p *domain.Principal, req AnswerRequest) (*Answer, error) {
	if !a.Enabled() {
		return nil, ErrAnswerDisabled
	}
	started := time.Now()

	question := strings.TrimSpace(req.Question)
	if question == "" {
		return nil, domain.ErrInvalidInput
	}

	q := req.Query
	q.Text = question
	q.Expand = true
	if q.Limit <= 0 || q.Limit > a.search.cfg.Search.AnswerMaxChunks {
		q.Limit = a.search.cfg.Search.AnswerMaxChunks
	}

	results, err := a.search.Search(ctx, p, q)
	if err != nil {
		return nil, err
	}

	out := &Answer{Question: question, Retrieved: len(results.Results)}

	if len(results.Results) == 0 {
		out.Answer = "There is nothing in the stored material that addresses this question. " +
			"Nothing has been inferred or supplied from outside the stored items."
		out.Sufficient = false
		out.TookMS = time.Since(started).Milliseconds()
		a.search.count("answer", "no_material")
		return out, nil
	}

	// Build the passages. The model sees opaque refs (S1, S2…) rather than item
	// IDs, so it cannot invent a citation that resolves to a real record.
	maxChars := a.search.cfg.Search.AnswerMaxCharsPerChunk
	if maxChars <= 0 {
		maxChars = 1200
	}
	passages := make([]analysis.Passage, 0, len(results.Results))
	for i, r := range results.Results {
		ref := fmt.Sprintf("S%d", i+1)
		excerpt := r.Excerpt
		if len(r.MatchedChunks) > 0 {
			// The matched chunk carries more context than the short excerpt.
			excerpt = truncateWords(r.MatchedChunks[0].Text, maxChars)
		}
		if strings.TrimSpace(excerpt) == "" {
			excerpt = truncateWords(r.Item.ExtractedText, maxChars)
		}
		if strings.TrimSpace(excerpt) == "" {
			continue
		}

		date := ""
		if !r.Item.CreatedAt.IsZero() {
			date = r.Item.CreatedAt.Format("2 January 2006")
		}
		passages = append(passages, analysis.Passage{
			Ref: ref, ItemID: r.Item.ID, Title: r.Item.Title, Excerpt: excerpt, Date: date,
		})
		out.Sources = append(out.Sources, AnswerSource{
			Ref: ref, ItemID: r.Item.ID, Title: r.Item.Title, Excerpt: excerpt, Date: date,
		})

		// Record any inferred relationship that pulled this item in, so the
		// answer can disclose that it was reached by a proposal.
		if r.ViaRelationship != nil && r.ViaRelationship.Provenance == domain.ProvModelInferred {
			out.InferredRelationshipsUsed = append(out.InferredRelationshipsUsed, *r.ViaRelationship)
		}
	}

	if len(passages) == 0 {
		out.Answer = "Items were found, but none contained readable text to answer from."
		out.Sufficient = false
		out.TookMS = time.Since(started).Milliseconds()
		return out, nil
	}

	generated, err := a.analysis.GenerateAnswer(ctx, question, passages)
	if err != nil {
		// The truncated sample of what the model actually said is the whole
		// diagnostic here: "rejected as degenerate" tells an operator nothing
		// about whether the model is broken, the prompt is wrong, or the check
		// is too strict. It is already bounded to 300 characters.
		log := observability.LoggerFrom(ctx)
		var sampled *analysis.InvalidOutputError
		if errors.As(err, &sampled) {
			log.Warn("answer generation failed",
				"error", err.Error(), "reason", sampled.Reason, "model_output", sampled.Sample)
		} else {
			log.Warn("answer generation failed", "error", err.Error())
		}

		// A model that returned something unusable is not a failed request.
		// Retrieval already found the material and Relata has already recorded
		// any contradictions between those items — the deterministic work is
		// the part worth keeping. Report that no prose could be generated and
		// hand back the sources and conflicts, as the no-material and
		// no-readable-text paths above do.
		var invalid *analysis.InvalidOutputError
		if errors.As(err, &invalid) {
			a.search.count("answer", "invalid_output")
			out.Answer = "The answering model did not return a usable answer, so none is shown. " +
				"The sources below were retrieved for this question and can be read directly; " +
				"nothing has been inferred or supplied from outside the stored items."
			out.Sufficient = false
			out.Uncertainty = append(out.Uncertainty,
				"No answer was generated: the model's response was rejected as invalid "+
					"("+invalid.Reason+"). The retrieved sources are unaffected.")
			if conflicts := a.storedConflicts(ctx, p, results.Results); len(conflicts) > 0 {
				out.Uncertainty = append(out.Uncertainty, conflicts...)
			}
			out.TookMS = time.Since(started).Milliseconds()
			return out, nil
		}

		// Anything else — a timeout, an unreachable provider — is an
		// operational condition the caller needs to see as a failure.
		a.search.count("answer", "error")
		return nil, err
	}

	out.Answer = generated.Answer
	out.Sufficient = generated.Sufficient
	out.Model = generated.Model
	out.InjectionNoticed = generated.InjectionNoticed

	cited := map[string]bool{}
	for _, c := range generated.Citations {
		cited[c.Ref] = true
	}
	for i := range out.Sources {
		out.Sources[i].Cited = cited[out.Sources[i].Ref]
	}

	out.Uncertainty = append(out.Uncertainty, generated.Conflicts...)

	// Independently of what the model said, surface contradictions Relata has
	// recorded between the retrieved items. A model that quietly picked one
	// side must not be able to hide the disagreement.
	if conflicts := a.storedConflicts(ctx, p, results.Results); len(conflicts) > 0 {
		out.Uncertainty = append(out.Uncertainty, conflicts...)
	}

	if !out.Sufficient {
		out.Uncertainty = append(out.Uncertainty,
			"The stored material does not fully answer this question. Treat the answer above as partial.")
	}
	if len(out.InferredRelationshipsUsed) > 0 {
		out.Uncertainty = append(out.Uncertainty, fmt.Sprintf(
			"%d of the sources were reached through relationships that Relata inferred and no one has confirmed. "+
				"An inferred relationship is a proposal, not an established fact.",
			len(out.InferredRelationshipsUsed)))
	}
	if generated.InjectionNoticed {
		out.Uncertainty = append(out.Uncertainty,
			"One or more stored documents contained text that attempted to instruct the answering model. "+
				"Those instructions were ignored, but the affected sources should be reviewed.")
	}

	out.TookMS = time.Since(started).Milliseconds()
	a.search.count("answer", "success")
	return out, nil
}

// storedConflicts reports contradictions Relata recorded between the retrieved
// items, independently of the model's own account.
func (a *Answerer) storedConflicts(ctx context.Context, p *domain.Principal, results []Result) []string {
	if len(results) < 2 {
		return nil
	}
	ids := make([]string, 0, len(results))
	titles := make(map[string]string, len(results))
	for _, r := range results {
		ids = append(ids, r.Item.ID)
		titles[r.Item.ID] = r.Item.Title
	}

	rels, err := a.search.rels.ListForItems(ctx, p.WorkspaceID, ids, 0)
	if err != nil {
		return nil
	}

	var out []string
	seen := map[string]bool{}
	for _, rel := range rels {
		if rel.Type != domain.RelContradicts || rel.ReviewStatus == domain.ReviewRejected {
			continue
		}
		// Both ends must be in the retrieved set for the conflict to be
		// relevant to this answer.
		srcTitle, okA := titles[rel.SourceID]
		tgtTitle, okB := titles[rel.TargetID]
		if !okA || !okB || seen[rel.ID] {
			continue
		}
		seen[rel.ID] = true

		detail := ""
		for _, s := range rel.Evidence.Signals {
			if s.Type == domain.SigClaimContradiction {
				detail = " " + s.Detail
				break
			}
		}
		out = append(out, fmt.Sprintf(
			"%q and %q record conflicting information (relationship strength %.2f, %s).%s",
			srcTitle, tgtTitle, rel.Score, provenanceLabel(rel.Provenance), detail))
	}
	sort.Strings(out)
	if len(out) > 6 {
		out = out[:6]
	}
	return out
}
