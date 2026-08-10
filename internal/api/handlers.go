package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/andydixon/relata/internal/analysis"
	"github.com/andydixon/relata/internal/audit"
	"github.com/andydixon/relata/internal/auth"
	"github.com/andydixon/relata/internal/config"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/ids"
	"github.com/andydixon/relata/internal/ingest"
	"github.com/andydixon/relata/internal/relationships"
	"github.com/andydixon/relata/internal/search"
)

// Deps bundles what the handlers need.
type Deps struct {
	Cfg      *config.Config
	Items    domain.ItemRepository
	Chunks   domain.ChunkRepository
	Entities domain.EntityRepository
	Aliases  domain.AliasRepository
	Claims   domain.ClaimRepository
	Rels     domain.RelationshipRepository
	Jobs     domain.JobRepository
	Runs     domain.RunRepository
	Models   domain.ModelVersionRepository
	Vectors  domain.VectorStore

	Ingest   *ingest.Service
	Pipeline *ingest.Pipeline
	Search   *search.Service
	Answer   *search.Answerer
	Reviewer *relationships.Reviewer
	Analysis *analysis.Service
	Audit    *audit.Recorder
	Tokens   *auth.TokenService

	Embedder domain.Embedder
}

// Handlers implements the REST API.
type Handlers struct {
	d Deps
}

// NewHandlers builds the API handlers.
func NewHandlers(d Deps) *Handlers { return &Handlers{d: d} }

// principal returns the authenticated caller, or an error.
func principal(r *http.Request) (*domain.Principal, error) {
	p := PrincipalFrom(r.Context())
	if p == nil || p.WorkspaceID == "" {
		return nil, &APIError{Status: http.StatusUnauthorized, Code: CodeUnauthorised,
			Message: "Authentication is required."}
	}
	return p, nil
}

// pathID extracts and validates a UUID path parameter.
//
// Validating the shape before any query means a crafted identifier never
// reaches the database layer at all.
func pathID(r *http.Request, name string) (string, error) {
	v := r.PathValue(name)
	if !ids.Valid(v) {
		return "", &APIError{Status: http.StatusBadRequest, Code: CodeInvalidRequest,
			Message: fmt.Sprintf("The %s must be a UUID.", name)}
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// Items
// ---------------------------------------------------------------------------

// createItemRequest is the JSON body for POST /api/v1/items.
type createItemRequest struct {
	Title          string            `json:"title"`
	Content        string            `json:"content"`
	SourceType     string            `json:"source_type"`
	MIMEType       string            `json:"mime_type"`
	Filename       string            `json:"filename"`
	Metadata       map[string]string `json:"metadata"`
	CreatedAt      string            `json:"created_at"`
	AllowDuplicate bool              `json:"allow_duplicate"`
}

// CreateItem ingests pasted text or a JSON document.
func (h *Handlers) CreateItem(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	var req createItemRequest
	if err := DecodeJSON(w, r, h.d.Cfg.Uploads.MaxUploadBytes, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		WriteError(w, r, BadRequest("The item has no content.",
			FieldError{Field: "content", Message: "must not be empty"}))
		return
	}

	ing := ingest.Request{
		Title:          req.Title,
		Content:        []byte(req.Content),
		Filename:       req.Filename,
		DeclaredMIME:   req.MIMEType,
		Metadata:       req.Metadata,
		AllowDuplicate: req.AllowDuplicate,
	}
	if req.SourceType != "" {
		st := domain.SourceType(req.SourceType)
		if !st.Valid() {
			WriteError(w, r, BadRequest("Unrecognised source type.",
				FieldError{Field: "source_type", Message: "must be one of " + sourceTypeList()}))
			return
		}
		ing.SourceType = st
	}
	if req.CreatedAt != "" {
		t, perr := parseTime(req.CreatedAt)
		if perr != nil {
			WriteError(w, r, BadRequest("Unrecognised creation date.",
				FieldError{Field: "created_at", Message: "must be RFC 3339, for example 2025-03-14T09:00:00Z"}))
			return
		}
		ing.CreatedAt = &t
	}

	res, err := h.d.Ingest.Ingest(r.Context(), p, ing)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{
		"item":   res.Item,
		"job_id": res.JobID,
		"status": "queued for processing",
	})
}

// UploadItem ingests a multipart file upload.
func (h *Handlers) UploadItem(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	limit := h.d.Cfg.Uploads.MaxUploadBytes
	// Allow a little headroom for multipart framing above the file limit.
	r.Body = http.MaxBytesReader(w, r.Body, limit+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			WriteError(w, r, &APIError{
				Status: http.StatusRequestEntityTooLarge, Code: CodePayloadTooLarge,
				Message: fmt.Sprintf("The upload exceeds the %d byte limit.", limit),
			})
			return
		}
		WriteError(w, r, BadRequest("The upload could not be read as a multipart form."))
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, header, err := r.FormFile("file")
	if err != nil {
		WriteError(w, r, BadRequest("No file was supplied.",
			FieldError{Field: "file", Message: "a file is required"}))
		return
	}
	defer file.Close()

	// Read at most limit+1 bytes so an oversized file is detected rather than
	// silently truncated.
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		WriteError(w, r, BadRequest("The uploaded file could not be read."))
		return
	}
	if int64(len(content)) > limit {
		WriteError(w, r, &APIError{
			Status: http.StatusRequestEntityTooLarge, Code: CodePayloadTooLarge,
			Message: fmt.Sprintf("The file exceeds the %d byte limit.", limit),
		})
		return
	}

	ing := ingest.Request{
		Title:          r.FormValue("title"),
		Content:        content,
		Filename:       header.Filename,
		DeclaredMIME:   header.Header.Get("Content-Type"),
		Metadata:       metadataFromForm(r),
		AllowDuplicate: r.FormValue("allow_duplicate") == "true",
	}
	if st := r.FormValue("source_type"); st != "" {
		if s := domain.SourceType(st); s.Valid() {
			ing.SourceType = s
		}
	}
	if ca := r.FormValue("created_at"); ca != "" {
		if t, perr := parseTime(ca); perr == nil {
			ing.CreatedAt = &t
		}
	}

	res, err := h.d.Ingest.Ingest(r.Context(), p, ing)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{
		"item":   res.Item,
		"job_id": res.JobID,
		"status": "queued for processing",
	})
}

// ListItems returns a page of items.
func (h *Handlers) ListItems(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	f, ferr := itemFilterFromQuery(r)
	if ferr != nil {
		WriteError(w, r, ferr)
		return
	}
	page := pageFromQuery(r)

	items, total, err := h.d.Items.List(r.Context(), p.WorkspaceID, f, page)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"pagination": map[string]any{
			"total": total, "limit": page.Limit, "offset": page.Offset,
			"has_more": int64(page.Offset+len(items)) < total,
		},
	})
}

// GetItem returns one item with its derived records.
func (h *Handlers) GetItem(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}

	item, err := h.d.Items.Get(r.Context(), p.WorkspaceID, id)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	ctx := r.Context()
	chunks, _ := h.d.Chunks.ListByItem(ctx, p.WorkspaceID, id)
	ents, _ := h.d.Entities.ListByItem(ctx, p.WorkspaceID, id)
	claims, _ := h.d.Claims.ListByItem(ctx, p.WorkspaceID, id)
	outRels, inRels, _ := h.d.Rels.ListForNode(ctx, p.WorkspaceID, domain.NodeItem, id, domain.RelationshipFilter{})
	runs, _ := h.d.Runs.ListByItem(ctx, p.WorkspaceID, id, 10)

	WriteJSON(w, http.StatusOK, map[string]any{
		"item":                   item,
		"chunks":                 chunks,
		"entities":               ents,
		"claims":                 claims,
		"outgoing_relationships": withExplanations(outRels),
		"incoming_relationships": withExplanations(inRels),
		"processing_runs":        runs,
	})
}

// DeleteItem removes an item, or reports what would be removed.
func (h *Handlers) DeleteItem(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}

	// A dry run reports the blast radius and changes nothing.
	if r.URL.Query().Get("dry_run") == "true" {
		plan, err := h.d.Pipeline.PlanDeletion(r.Context(), p.WorkspaceID, id)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"dry_run": true,
			"plan":    plan,
			"note":    "Nothing has been deleted. Repeat this request without dry_run=true to proceed.",
		})
		return
	}

	jobID, err := h.d.Pipeline.RequestDeletion(r.Context(), p, id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{
		"deleted": id,
		"job_id":  jobID,
		"status":  "The item is hidden immediately; its chunks, vectors, claims, orphaned entities and relationships are being removed.",
	})
}

// ReprocessItem queues an item to be processed again.
func (h *Handlers) ReprocessItem(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}

	var req struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength > 0 {
		if err := DecodeJSON(w, r, 8<<10, &req); err != nil {
			WriteError(w, r, err)
			return
		}
	}

	jobID, err := h.d.Pipeline.RequestReprocess(r.Context(), p, id, req.Reason)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{
		"item_id": id, "job_id": jobID, "status": "queued for reprocessing",
	})
}

// GetItemRelationships returns an item's relationships.
func (h *Handlers) GetItemRelationships(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}

	f, ferr := relFilterFromQuery(r)
	if ferr != nil {
		WriteError(w, r, ferr)
		return
	}

	out, in, err := h.d.Rels.ListForNode(r.Context(), p.WorkspaceID, domain.NodeItem, id, f)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	// Resolve the titles at the far end of each relationship.
	var otherIDs []string
	for _, rel := range append(append([]*domain.Relationship{}, out...), in...) {
		otherIDs = append(otherIDs, rel.SourceID, rel.TargetID)
	}
	titles := map[string]string{}
	if items, err := h.d.Items.GetMany(r.Context(), p.WorkspaceID, otherIDs); err == nil {
		for _, it := range items {
			titles[it.ID] = it.Title
		}
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"item_id":  id,
		"outgoing": withTitles(withExplanations(out), titles),
		"incoming": withTitles(withExplanations(in), titles),
		"note": "Relationship strength is a heuristic score derived from the listed signals. " +
			"It is not a calibrated probability.",
	})
}

// ---------------------------------------------------------------------------
// Relationships
// ---------------------------------------------------------------------------

// CreateRelationship records a relationship a person asserted.
func (h *Handlers) CreateRelationship(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	var req struct {
		SourceID string `json:"source_id"`
		TargetID string `json:"target_id"`
		Type     string `json:"type"`
		Note     string `json:"note"`
	}
	if err := DecodeJSON(w, r, 32<<10, &req); err != nil {
		WriteError(w, r, err)
		return
	}

	rel, err := h.d.Reviewer.CreateManual(r.Context(), p, req.SourceID, req.TargetID,
		domain.RelationshipType(req.Type), req.Note)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, relResponse(rel))
}

// PatchRelationship changes a relationship's type.
func (h *Handlers) PatchRelationship(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}

	var req struct {
		Type string `json:"type"`
		Note string `json:"note"`
	}
	if err := DecodeJSON(w, r, 32<<10, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if req.Type == "" {
		WriteError(w, r, BadRequest("No change was requested.",
			FieldError{Field: "type", Message: "a relationship type is required"}))
		return
	}

	rel, err := h.d.Reviewer.ChangeType(r.Context(), p, id, domain.RelationshipType(req.Type), req.Note)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, relResponse(rel))
}

// ConfirmRelationship records a human confirmation.
func (h *Handlers) ConfirmRelationship(w http.ResponseWriter, r *http.Request) {
	h.review(w, r, "confirm")
}

// RejectRelationship records a human rejection.
func (h *Handlers) RejectRelationship(w http.ResponseWriter, r *http.Request) {
	h.review(w, r, "reject")
}

func (h *Handlers) review(w http.ResponseWriter, r *http.Request, action string) {
	p, err := principal(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}

	var req struct {
		Note string `json:"note"`
	}
	if r.ContentLength > 0 {
		if err := DecodeJSON(w, r, 32<<10, &req); err != nil {
			WriteError(w, r, err)
			return
		}
	}

	var rel *domain.Relationship
	if action == "confirm" {
		rel, err = h.d.Reviewer.Confirm(r.Context(), p, id, req.Note)
	} else {
		rel, err = h.d.Reviewer.Reject(r.Context(), p, id, req.Note)
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, relResponse(rel))
}

// ListRelationships returns a page of relationships.
func (h *Handlers) ListRelationships(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	f, ferr := relFilterFromQuery(r)
	if ferr != nil {
		WriteError(w, r, ferr)
		return
	}
	page := pageFromQuery(r)

	rels, total, err := h.d.Rels.List(r.Context(), p.WorkspaceID, f, page)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"relationships": withExplanations(rels),
		"pagination": map[string]any{
			"total": total, "limit": page.Limit, "offset": page.Offset,
			"has_more": int64(page.Offset+len(rels)) < total,
		},
	})
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

// Search runs a hybrid query.
func (h *Handlers) Search(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	q, qerr := searchQueryFromRequest(r)
	if qerr != nil {
		WriteError(w, r, qerr)
		return
	}

	resp, err := h.d.Search.Search(r.Context(), p, q)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"search": resp,
		"score_guide": map[string]string{
			"query_relevance":       "How well this item answers this particular query, relative to the best result. Not a probability.",
			"semantic_similarity":   "How close in meaning a matched section is to the query text. Similarity is not evidence of a factual relationship.",
			"relationship_strength": "A heuristic strength derived from the listed signals, under a versioned scoring model. Not a calibrated probability.",
		},
	})
}

// Answer generates an answer from stored material only.
func (h *Handlers) Answer(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if !h.d.Answer.Enabled() {
		WriteError(w, r, &APIError{
			Status: http.StatusServiceUnavailable, Code: CodeFeatureDisabled,
			Message: "Answer generation requires a language model, which is not configured.",
		})
		return
	}

	var req struct {
		Question      string   `json:"question"`
		SourceTypes   []string `json:"source_types"`
		EntityIDs     []string `json:"entity_ids"`
		ConfirmedOnly bool     `json:"confirmed_only"`
		Limit         int      `json:"limit"`
	}
	if err := DecodeJSON(w, r, 64<<10, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if strings.TrimSpace(req.Question) == "" {
		WriteError(w, r, BadRequest("No question was supplied.",
			FieldError{Field: "question", Message: "must not be empty"}))
		return
	}
	if len(req.Question) > 2000 {
		WriteError(w, r, BadRequest("The question is too long.",
			FieldError{Field: "question", Message: "must be at most 2000 characters"}))
		return
	}

	ar := search.AnswerRequest{Question: req.Question}
	ar.ConfirmedOnly = req.ConfirmedOnly
	ar.Limit = req.Limit
	ar.EntityIDs = filterUUIDs(req.EntityIDs)
	for _, st := range req.SourceTypes {
		if s := domain.SourceType(st); s.Valid() {
			ar.SourceTypes = append(ar.SourceTypes, s)
		}
	}

	ans, err := h.d.Answer.Answer(r.Context(), p, ar)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	h.d.Audit.Record(r.Context(), p, domain.AuditAnswerGenerated, "answer", "", "success", map[string]string{
		"retrieved": strconv.Itoa(ans.Retrieved), "sufficient": strconv.FormatBool(ans.Sufficient),
	})
	WriteJSON(w, http.StatusOK, ans)
}

// ---------------------------------------------------------------------------
// Entities
// ---------------------------------------------------------------------------

// ListEntities returns a page of entities.
func (h *Handlers) ListEntities(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	f := domain.EntityFilter{
		NameContains: strings.TrimSpace(r.URL.Query().Get("q")),
	}
	for _, t := range splitCSV(r.URL.Query().Get("type")) {
		if et := domain.EntityType(t); et.Valid() {
			f.Types = append(f.Types, et)
		}
	}
	if v := r.URL.Query().Get("item_id"); v != "" {
		if !ids.Valid(v) {
			WriteError(w, r, BadRequest("item_id must be a UUID."))
			return
		}
		f.ItemID = v
	}
	if v := r.URL.Query().Get("min_mentions"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.MinMentions = n
		}
	}

	page := pageFromQuery(r)
	ents, total, err := h.d.Entities.List(r.Context(), p.WorkspaceID, f, page)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"entities": ents,
		"pagination": map[string]any{
			"total": total, "limit": page.Limit, "offset": page.Offset,
			"has_more": int64(page.Offset+len(ents)) < total,
		},
	})
}

// GetEntity returns one entity with its aliases and citing items.
func (h *Handlers) GetEntity(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}

	ent, err := h.d.Entities.Get(r.Context(), p.WorkspaceID, id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	aliases, _ := h.d.Aliases.ListByEntity(r.Context(), p.WorkspaceID, id)
	items, _ := h.d.Items.GetMany(r.Context(), p.WorkspaceID, ent.ItemIDs)

	WriteJSON(w, http.StatusOK, map[string]any{
		"entity":  ent,
		"aliases": aliases,
		"items":   items,
		"note":    "Candidate aliases are suggestions only. Relata never merges entities on name similarity alone.",
	})
}

// ---------------------------------------------------------------------------
// Jobs
// ---------------------------------------------------------------------------

// ListJobs returns a page of background jobs.
func (h *Handlers) ListJobs(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	var statuses []domain.JobStatus
	for _, s := range splitCSV(r.URL.Query().Get("status")) {
		switch domain.JobStatus(s) {
		case domain.JobPending, domain.JobRunning, domain.JobSucceeded, domain.JobFailed, domain.JobCancelled:
			statuses = append(statuses, domain.JobStatus(s))
		}
	}
	var types []domain.JobType
	for _, t := range splitCSV(r.URL.Query().Get("type")) {
		if jt := domain.JobType(t); jt.Valid() {
			types = append(types, jt)
		}
	}

	page := pageFromQuery(r)
	jobs, total, err := h.d.Jobs.List(r.Context(), p.WorkspaceID, statuses, types, page)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"jobs": jobs,
		"pagination": map[string]any{
			"total": total, "limit": page.Limit, "offset": page.Offset,
			"has_more": int64(page.Offset+len(jobs)) < total,
		},
	})
}

// RetryJob returns a failed job to the queue.
func (h *Handlers) RetryJob(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}

	job, err := h.d.Jobs.Retry(r.Context(), p.WorkspaceID, id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			WriteError(w, r, &APIError{
				Status: http.StatusConflict, Code: CodeConflict,
				Message: "That job does not exist, or is not in a state that can be retried.",
			})
			return
		}
		WriteError(w, r, err)
		return
	}
	h.d.Audit.Record(r.Context(), p, domain.AuditJobRetried, "job", id, "success",
		map[string]string{"type": string(job.Type)})
	WriteJSON(w, http.StatusOK, map[string]any{"job": job})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// relView adds computed fields to a relationship for API responses.
type relView struct {
	*domain.Relationship
	Explanation []string `json:"explanation"`
	SourceTitle string   `json:"source_title,omitempty"`
	TargetTitle string   `json:"target_title,omitempty"`
	// ScoreMeaning states plainly what the number is, in every response that
	// carries one.
	ScoreMeaning string `json:"score_meaning"`
}

func relResponse(rel *domain.Relationship) map[string]any {
	return map[string]any{"relationship": relViewOf(rel)}
}

func relViewOf(rel *domain.Relationship) *relView {
	return &relView{
		Relationship: rel,
		Explanation:  relationships.Explain(rel),
		ScoreMeaning: "A heuristic relationship strength derived from the listed signals under " +
			rel.ScoreVersion + ". It is not a calibrated probability.",
	}
}

func withExplanations(rels []*domain.Relationship) []*relView {
	out := make([]*relView, 0, len(rels))
	for _, rel := range rels {
		out = append(out, relViewOf(rel))
	}
	return out
}

func withTitles(views []*relView, titles map[string]string) []*relView {
	for _, v := range views {
		v.SourceTitle = titles[v.SourceID]
		v.TargetTitle = titles[v.TargetID]
	}
	return views
}

func pageFromQuery(r *http.Request) domain.Page {
	p := domain.Page{}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			p.Limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			p.Offset = n
		}
	}
	return p.Normalise(20, 200)
}

// itemFilterFromQuery builds a typed filter from query parameters.
//
// Unrecognised values are rejected rather than ignored, so a client that
// mistypes a source type learns about it instead of silently getting
// unfiltered results.
func itemFilterFromQuery(r *http.Request) (domain.ItemFilter, error) {
	q := r.URL.Query()
	f := domain.ItemFilter{TitleContains: strings.TrimSpace(q.Get("title"))}

	for _, s := range splitCSV(q.Get("source_type")) {
		st := domain.SourceType(s)
		if !st.Valid() {
			return f, BadRequest("Unrecognised source type.",
				FieldError{Field: "source_type", Message: s + " is not one of " + sourceTypeList()})
		}
		f.SourceTypes = append(f.SourceTypes, st)
	}
	f.MIMETypes = append(f.MIMETypes, splitCSV(q.Get("mime_type"))...)
	for _, s := range splitCSV(q.Get("status")) {
		f.Statuses = append(f.Statuses, domain.ProcessingStatus(s))
	}
	if v := strings.TrimSpace(q.Get("content_hash")); v != "" {
		if !isHex(v) {
			return f, BadRequest("content_hash must be a hexadecimal digest.")
		}
		f.ContentHash = v
	}
	if v := q.Get("created_from"); v != "" {
		t, err := parseTime(v)
		if err != nil {
			return f, BadRequest("created_from must be an RFC 3339 date.")
		}
		f.CreatedFrom = &t
	}
	if v := q.Get("created_to"); v != "" {
		t, err := parseTime(v)
		if err != nil {
			return f, BadRequest("created_to must be an RFC 3339 date.")
		}
		f.CreatedTo = &t
	}
	return f, nil
}

func relFilterFromQuery(r *http.Request) (domain.RelationshipFilter, error) {
	q := r.URL.Query()
	f := domain.RelationshipFilter{Direction: q.Get("direction")}

	for _, t := range splitCSV(q.Get("type")) {
		rt := domain.RelationshipType(t)
		if !rt.Valid() {
			return f, BadRequest("Unrecognised relationship type.",
				FieldError{Field: "type", Message: t + " is not a recognised relationship type"})
		}
		f.Types = append(f.Types, rt)
	}
	for _, pv := range splitCSV(q.Get("provenance")) {
		p := domain.Provenance(pv)
		if !p.Valid() {
			return f, BadRequest("Unrecognised provenance.",
				FieldError{Field: "provenance", Message: pv + " is not a recognised provenance"})
		}
		f.Provenances = append(f.Provenances, p)
	}
	for _, rs := range splitCSV(q.Get("review_status")) {
		f.ReviewStatus = append(f.ReviewStatus, domain.ReviewStatus(rs))
	}
	if q.Get("confirmed_only") == "true" {
		f.ConfirmedOnly = true
	}
	if v := q.Get("min_score"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n < 0 || n > 1 {
			return f, BadRequest("min_score must be a number between 0 and 1.")
		}
		f.MinScore = n
	}
	return f, nil
}

func searchQueryFromRequest(r *http.Request) (search.Query, error) {
	q := r.URL.Query()
	out := search.Query{
		Text:          q.Get("q"),
		ConfirmedOnly: q.Get("confirmed_only") == "true",
		Expand:        q.Get("expand") != "false",
	}
	if len(out.Text) > 2000 {
		return out, BadRequest("The query is too long.",
			FieldError{Field: "q", Message: "must be at most 2000 characters"})
	}

	for _, s := range splitCSV(q.Get("source_type")) {
		st := domain.SourceType(s)
		if !st.Valid() {
			return out, BadRequest("Unrecognised source type.",
				FieldError{Field: "source_type", Message: s + " is not one of " + sourceTypeList()})
		}
		out.SourceTypes = append(out.SourceTypes, st)
	}
	out.MIMETypes = append(out.MIMETypes, splitCSV(q.Get("mime_type"))...)
	for _, t := range splitCSV(q.Get("relationship_type")) {
		rt := domain.RelationshipType(t)
		if !rt.Valid() {
			return out, BadRequest("Unrecognised relationship type.",
				FieldError{Field: "relationship_type", Message: t + " is not recognised"})
		}
		out.RelTypes = append(out.RelTypes, rt)
	}
	out.EntityIDs = filterUUIDs(splitCSV(q.Get("entity_id")))

	if v := q.Get("created_from"); v != "" {
		t, err := parseTime(v)
		if err != nil {
			return out, BadRequest("created_from must be an RFC 3339 date.")
		}
		out.CreatedFrom = &t
	}
	if v := q.Get("created_to"); v != "" {
		t, err := parseTime(v)
		if err != nil {
			return out, BadRequest("created_to must be an RFC 3339 date.")
		}
		out.CreatedTo = &t
	}
	if v := q.Get("min_relationship_strength"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n < 0 || n > 1 {
			return out, BadRequest("min_relationship_strength must be between 0 and 1.")
		}
		out.MinRelStrength = n
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			out.Limit = n
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n > 0 {
				out.Offset = n
			}
		}
	}
	return out, nil
}

// parseTime accepts RFC 3339 or a plain date.
func parseTime(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("unrecognised time %q", v)
}

func splitCSV(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func filterUUIDs(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if ids.Valid(v) {
			out = append(out, v)
		}
	}
	return out
}

func isHex(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func sourceTypeList() string {
	names := make([]string, 0, len(domain.ValidSourceTypes))
	for _, s := range domain.ValidSourceTypes {
		names = append(names, string(s))
	}
	return strings.Join(names, ", ")
}

// metadataFromForm reads metadata[key]=value pairs from a multipart form.
func metadataFromForm(r *http.Request) map[string]string {
	out := map[string]string{}
	for k, vs := range r.Form {
		if !strings.HasPrefix(k, "metadata[") || !strings.HasSuffix(k, "]") || len(vs) == 0 {
			continue
		}
		key := strings.TrimSuffix(strings.TrimPrefix(k, "metadata["), "]")
		if key != "" {
			out[key] = vs[0]
		}
	}
	// Also accept parallel metadata_key / metadata_value arrays from the form.
	keys := r.Form["metadata_key"]
	vals := r.Form["metadata_value"]
	for i := range keys {
		if i < len(vals) && strings.TrimSpace(keys[i]) != "" {
			out[strings.TrimSpace(keys[i])] = vals[i]
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---------------------------------------------------------------------------
// Entity aliases
// ---------------------------------------------------------------------------

// ConfirmAlias records that a person agrees two names refer to the same thing.
//
// Only a confirmed alias contributes evidence to relationship discovery. Until
// someone decides, a candidate is a suggestion and is weighted at nothing.
func (h *Handlers) ConfirmAlias(w http.ResponseWriter, r *http.Request) {
	h.reviewAlias(w, r, domain.AliasConfirmed)
}

// RejectAlias records that a person judged two similar names to be different
// things. The proposal is retained, so it is not offered again as new.
func (h *Handlers) RejectAlias(w http.ResponseWriter, r *http.Request) {
	h.reviewAlias(w, r, domain.AliasRejected)
}

func (h *Handlers) reviewAlias(w http.ResponseWriter, r *http.Request, status domain.AliasStatus) {
	p, err := principal(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		WriteError(w, r, err)
		return
	}

	if err := h.d.Aliases.SetStatus(r.Context(), p.WorkspaceID, id, status); err != nil {
		WriteError(w, r, err)
		return
	}
	h.d.Audit.Record(r.Context(), p, "entity_alias."+string(status), "entity_alias", id, "success", nil)

	WriteJSON(w, http.StatusOK, map[string]any{
		"alias_id": id,
		"status":   status,
		"note": "Entities are still stored separately. A confirmed alias contributes evidence to " +
			"relationship discovery; it does not merge the two records.",
	})
}
