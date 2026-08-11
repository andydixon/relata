package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/andydixon/relata/internal/api"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/ids"
	"github.com/andydixon/relata/internal/ingest"
	"github.com/andydixon/relata/internal/relationships"
	"github.com/andydixon/relata/internal/search"
)

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

// SearchPage runs a search, and optionally generates an answer.
func (h *Handlers) SearchPage(w http.ResponseWriter, r *http.Request) {
	p := api.PrincipalFrom(r.Context())
	q := r.URL.Query()
	text := strings.TrimSpace(q.Get("q"))

	filters := map[string]string{
		"CreatedFrom": q.Get("created_from"),
		"CreatedTo":   q.Get("created_to"),
		"SourceType":  q.Get("source_type"),
		"MIMEType":    q.Get("mime_type"),
		"EntityID":    q.Get("entity_id"),
		"RelType":     q.Get("relationship_type"),
		"MinStrength": q.Get("min_relationship_strength"),
	}
	confirmedOnly := q.Get("confirmed_only") == "true"

	// Entity options for the filter dropdown.
	allEntities, _, _ := h.d.Entities.List(r.Context(), p.WorkspaceID,
		domain.EntityFilter{MinMentions: 1}, domain.Page{Limit: 200})

	data := struct {
		base
		Query            search.Query
		Filters          map[string]string
		FiltersConfirmed bool
		HasFilters       bool
		Searched         bool
		Results          *search.Response
		Pagination       pagination
		AllEntities      []*domain.Entity
		AnswerEnabled    bool
		Answer           *search.Answer
		AnswerParagraphs []string
	}{
		base:          h.newBase(r, "Search", "search"),
		Filters:       filters,
		AllEntities:   allEntities,
		AnswerEnabled: h.d.Answer.Enabled(),
	}
	data.Filters["ConfirmedOnly"] = strconv.FormatBool(confirmedOnly)

	for _, v := range filters {
		if v != "" {
			data.HasFilters = true
		}
	}
	if confirmedOnly {
		data.HasFilters = true
	}

	if text == "" && !data.HasFilters {
		h.d.Renderer.Render(w, r, "search.html", http.StatusOK, data)
		return
	}

	limit, offset := pageParams(r, h.d.Cfg.Search.DefaultLimit)
	sq := search.Query{
		Text:          text,
		ConfirmedOnly: confirmedOnly,
		Expand:        true,
		Limit:         limit,
		Offset:        offset,
	}
	if v := filters["SourceType"]; v != "" {
		if s := domain.SourceType(v); s.Valid() {
			sq.SourceTypes = []domain.SourceType{s}
		}
	}
	if v := filters["MIMEType"]; v != "" {
		sq.MIMETypes = []string{v}
	}
	if v := filters["EntityID"]; ids.Valid(v) {
		sq.EntityIDs = []string{v}
	}
	if v := filters["RelType"]; v != "" {
		if t := domain.RelationshipType(v); t.Valid() {
			sq.RelTypes = []domain.RelationshipType{t}
		}
	}
	if v := filters["CreatedFrom"]; v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			sq.CreatedFrom = &t
		}
	}
	if v := filters["CreatedTo"]; v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			end := t.Add(24*time.Hour - time.Second)
			sq.CreatedTo = &end
		}
	}
	if v := filters["MinStrength"]; v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && f <= 1 {
			sq.MinRelStrength = f
		}
	}

	data.Query = sq
	data.Searched = true

	// The answer mode is an explicit choice, not the default: generating an
	// answer costs a model call and produces prose that must be read as a
	// summary of sources rather than as an authority.
	if q.Get("answer") == "true" && h.d.Answer.Enabled() && text != "" {
		ans, err := h.d.Answer.Answer(r.Context(), p, search.AnswerRequest{Question: text, Query: sq})
		if err != nil {
			data.Error = "The answer could not be generated: " + friendlyError(err)
		} else {
			data.Answer = ans
			data.AnswerParagraphs = splitParagraphs(ans.Answer)
			h.d.Audit.Record(r.Context(), p, domain.AuditAnswerGenerated, "answer", "", "success",
				map[string]string{"sufficient": strconv.FormatBool(ans.Sufficient)})
		}
	}

	res, err := h.d.Search.Search(r.Context(), p, sq)
	if err != nil {
		data.Error = "The search could not be completed: " + friendlyError(err)
		h.d.Renderer.Render(w, r, "search.html", http.StatusOK, data)
		return
	}
	data.Results = res
	data.Pagination = buildPagination(r, res.Total, limit, offset, len(res.Results))

	h.d.Renderer.Render(w, r, "search.html", http.StatusOK, data)
}

// splitParagraphs breaks generated prose into paragraphs for rendering.
// The text is inserted as escaped content; splitting only affects layout.
func splitParagraphs(s string) []string {
	var out []string
	for _, p := range strings.Split(s, "\n\n") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Add and upload
// ---------------------------------------------------------------------------

// AddTextPage shows the paste-a-note form.
func (h *Handlers) AddTextPage(w http.ResponseWriter, r *http.Request) {
	data := struct {
		base
		Form map[string]string
	}{
		base: h.newBase(r, "Add text", "add"),
		Form: map[string]string{"SourceType": string(domain.SourceTypeNote)},
	}
	if r.URL.Query().Get("error") != "" {
		data.Error = knownFormError(r.URL.Query().Get("error"))
	}
	h.d.Renderer.Render(w, r, "add_text.html", http.StatusOK, data)
}

// AddText ingests pasted content.
func (h *Handlers) AddText(w http.ResponseWriter, r *http.Request) {
	p := api.PrincipalFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		h.renderFormError(w, r, "add_text.html", "Add text", "add", "The form could not be read.", nil)
		return
	}

	req := ingest.Request{
		Title:          r.PostFormValue("title"),
		Content:        []byte(r.PostFormValue("content")),
		SourceType:     domain.SourceType(r.PostFormValue("source_type")),
		Metadata:       metadataFromForm(r),
		AllowDuplicate: r.PostFormValue("allow_duplicate") == "true",
	}
	if v := r.PostFormValue("created_at"); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			req.CreatedAt = &t
		}
	}

	res, err := h.d.Ingest.Ingest(r.Context(), p, req)
	if err != nil {
		var dup *ingest.DuplicateError
		if errors.As(err, &dup) {
			h.renderFormError(w, r, "add_text.html", "Add text", "add",
				fmt.Sprintf("An item with identical content already exists (%q). "+
					"Tick “store even if identical content already exists” to keep both.", dup.ExistingTitle),
				map[string]string{
					"Title": r.PostFormValue("title"), "Content": r.PostFormValue("content"),
					"SourceType": r.PostFormValue("source_type"), "CreatedAt": r.PostFormValue("created_at"),
				})
			return
		}
		h.renderFormError(w, r, "add_text.html", "Add text", "add", friendlyError(err),
			map[string]string{
				"Title": r.PostFormValue("title"), "Content": r.PostFormValue("content"),
				"SourceType": r.PostFormValue("source_type"), "CreatedAt": r.PostFormValue("created_at"),
			})
		return
	}

	http.Redirect(w, r, "/items/"+res.Item.ID+"?ok=item_added", http.StatusSeeOther)
}

// UploadPage shows the file upload form.
func (h *Handlers) UploadPage(w http.ResponseWriter, r *http.Request) {
	data := struct {
		base
		MaxUploadMB int64
		Warning     bool
	}{
		base:        h.newBase(r, "Upload a document", "upload"),
		MaxUploadMB: h.d.Cfg.Uploads.MaxUploadBytes / (1 << 20),
		Warning:     true,
	}
	h.d.Renderer.Render(w, r, "upload.html", http.StatusOK, data)
}

// Upload ingests a file.
func (h *Handlers) Upload(w http.ResponseWriter, r *http.Request) {
	p := api.PrincipalFrom(r.Context())
	limit := h.d.Cfg.Uploads.MaxUploadBytes

	renderErr := func(msg string) {
		data := struct {
			base
			MaxUploadMB int64
			Warning     bool
		}{
			base:        h.newBase(r, "Upload a document", "upload"),
			MaxUploadMB: limit / (1 << 20),
			Warning:     true,
		}
		data.Error = msg
		h.d.Renderer.Render(w, r, "upload.html", http.StatusBadRequest, data)
	}

	// The CSRF middleware has already parsed the multipart form, so the file is
	// available without re-reading the body.
	if r.MultipartForm == nil {
		r.Body = http.MaxBytesReader(w, r.Body, limit+(1<<20))
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			renderErr(fmt.Sprintf("The upload could not be read. Files must be no larger than %d MB.", limit/(1<<20)))
			return
		}
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, header, err := r.FormFile("file")
	if err != nil {
		renderErr("No file was chosen.")
		return
	}
	defer file.Close()

	content := make([]byte, 0, 64<<10)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := file.Read(buf)
		if n > 0 {
			content = append(content, buf[:n]...)
			if int64(len(content)) > limit {
				renderErr(fmt.Sprintf("That file is larger than the %d MB limit.", limit/(1<<20)))
				return
			}
		}
		if rerr != nil {
			break
		}
	}

	req := ingest.Request{
		Title:          r.FormValue("title"),
		Content:        content,
		Filename:       header.Filename,
		DeclaredMIME:   header.Header.Get("Content-Type"),
		SourceType:     domain.SourceType(r.FormValue("source_type")),
		Metadata:       metadataFromForm(r),
		AllowDuplicate: r.FormValue("allow_duplicate") == "true",
	}
	if v := r.FormValue("created_at"); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			req.CreatedAt = &t
		}
	}

	res, err := h.d.Ingest.Ingest(r.Context(), p, req)
	if err != nil {
		var dup *ingest.DuplicateError
		if errors.As(err, &dup) {
			renderErr(fmt.Sprintf("An item with identical content already exists (%q). "+
				"Tick “store even if identical content already exists” to keep both.", dup.ExistingTitle))
			return
		}
		renderErr(friendlyError(err))
		return
	}

	http.Redirect(w, r, "/items/"+res.Item.ID+"?ok=item_added", http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Item actions
// ---------------------------------------------------------------------------

// DeleteItem removes an item.
func (h *Handlers) DeleteItem(w http.ResponseWriter, r *http.Request) {
	p := api.PrincipalFrom(r.Context())
	id := r.PathValue("id")
	if !ids.Valid(id) {
		http.NotFound(w, r)
		return
	}
	if _, err := h.d.Pipeline.RequestDeletion(r.Context(), p, id); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "Could not delete the item", friendlyError(err))
		return
	}
	http.Redirect(w, r, "/items?ok=item_deleted", http.StatusSeeOther)
}

// ReprocessItem queues an item for reprocessing.
func (h *Handlers) ReprocessItem(w http.ResponseWriter, r *http.Request) {
	p := api.PrincipalFrom(r.Context())
	id := r.PathValue("id")
	if !ids.Valid(id) {
		http.NotFound(w, r)
		return
	}
	if _, err := h.d.Pipeline.RequestReprocess(r.Context(), p, id, "requested from the interface"); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "Could not reprocess the item", friendlyError(err))
		return
	}
	http.Redirect(w, r, "/items/"+id+"?ok=item_reprocessing", http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Relationship review
// ---------------------------------------------------------------------------

// ReviewPage lists relationships for adjudication.
func (h *Handlers) ReviewPage(w http.ResponseWriter, r *http.Request) {
	p := api.PrincipalFrom(r.Context())
	q := r.URL.Query()

	status := q.Get("review_status")
	if status == "" && !q.Has("review_status") {
		status = string(domain.ReviewPending)
	}

	f := domain.RelationshipFilter{}
	if status != "" {
		f.ReviewStatus = []domain.ReviewStatus{domain.ReviewStatus(status)}
	}
	if t := q.Get("type"); t != "" {
		if rt := domain.RelationshipType(t); rt.Valid() {
			f.Types = []domain.RelationshipType{rt}
		}
	}
	if v := q.Get("min_score"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n >= 0 && n <= 1 {
			f.MinScore = n
		}
	}

	limit, offset := pageParams(r, 20)
	rels, total, err := h.d.Rels.List(r.Context(), p.WorkspaceID, f, domain.Page{Limit: limit, Offset: offset})
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "Could not load relationships", "Please try again.")
		return
	}

	titles := h.titlesFor(r.Context(), p.WorkspaceID, rels)

	data := struct {
		base
		Relationships []relView
		Filters       map[string]string
		Pagination    pagination
	}{
		base:          h.newBase(r, "Relationship review", "review"),
		Relationships: toRelViews(rels, titles),
		Filters: map[string]string{
			"Status": status, "Type": q.Get("type"), "MinScore": q.Get("min_score"),
		},
		Pagination: buildPagination(r, int(total), limit, offset, len(rels)),
	}
	h.d.Renderer.Render(w, r, "review.html", http.StatusOK, data)
}

// ConfirmRelationship records a confirmation.
func (h *Handlers) ConfirmRelationship(w http.ResponseWriter, r *http.Request) {
	h.reviewAction(w, r, "confirm")
}

// RejectRelationship records a rejection.
func (h *Handlers) RejectRelationship(w http.ResponseWriter, r *http.Request) {
	h.reviewAction(w, r, "reject")
}

// ChangeRelationshipType records a type correction.
func (h *Handlers) ChangeRelationshipType(w http.ResponseWriter, r *http.Request) {
	h.reviewAction(w, r, "type")
}

func (h *Handlers) reviewAction(w http.ResponseWriter, r *http.Request, action string) {
	p := api.PrincipalFrom(r.Context())
	id := r.PathValue("id")
	if !ids.Valid(id) {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	note := r.PostFormValue("note")

	var err error
	var flashCode string
	switch action {
	case "confirm":
		_, err = h.d.Reviewer.Confirm(r.Context(), p, id, note)
		flashCode = "rel_confirmed"
	case "reject":
		_, err = h.d.Reviewer.Reject(r.Context(), p, id, note)
		flashCode = "rel_rejected"
	case "type":
		newType := domain.RelationshipType(r.PostFormValue("type"))
		_, err = h.d.Reviewer.ChangeType(r.Context(), p, id, newType, note)
		flashCode = "rel_updated"
	}
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, "Could not record the decision", friendlyError(err))
		return
	}

	redirectBack(w, r, "/review", "?ok="+flashCode)
}

// CreateRelationship records a manually-asserted relationship.
func (h *Handlers) CreateRelationship(w http.ResponseWriter, r *http.Request) {
	p := api.PrincipalFrom(r.Context())
	_ = r.ParseForm()

	sourceID := r.PostFormValue("source_id")
	targetID := r.PostFormValue("target_id")
	relType := domain.RelationshipType(r.PostFormValue("type"))

	if _, err := h.d.Reviewer.CreateManual(r.Context(), p, sourceID, targetID, relType, r.PostFormValue("note")); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "Could not add the relationship", friendlyError(err))
		return
	}
	http.Redirect(w, r, "/items/"+sourceID+"?ok=rel_created", http.StatusSeeOther)
}

// redirectBack returns to the referring page when it is one of ours.
//
// The Referer is only honoured when it is a same-origin path, so it cannot be
// used to bounce a user to another site.
func redirectBack(w http.ResponseWriter, r *http.Request, fallback, suffix string) {
	target := fallback
	if ref := r.Header.Get("Referer"); ref != "" {
		if i := strings.Index(ref, "://"); i >= 0 {
			rest := ref[i+3:]
			if j := strings.IndexByte(rest, '/'); j >= 0 {
				path := rest[j:]
				if strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "//") {
					if q := strings.IndexByte(path, '?'); q >= 0 {
						path = path[:q]
					}
					target = path
				}
			}
		}
	}
	http.Redirect(w, r, target+suffix, http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Entities
// ---------------------------------------------------------------------------

// EntitiesPage lists extracted entities.
func (h *Handlers) EntitiesPage(w http.ResponseWriter, r *http.Request) {
	p := api.PrincipalFrom(r.Context())
	q := r.URL.Query()

	f := domain.EntityFilter{NameContains: strings.TrimSpace(q.Get("q"))}
	if t := q.Get("type"); t != "" {
		if et := domain.EntityType(t); et.Valid() {
			f.Types = []domain.EntityType{et}
		}
	}

	limit, offset := pageParams(r, 50)
	ents, total, err := h.d.Entities.List(r.Context(), p.WorkspaceID, f, domain.Page{Limit: limit, Offset: offset})
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "Could not list entities", "Please try again.")
		return
	}

	data := struct {
		base
		Entities   []*domain.Entity
		Filters    map[string]string
		Pagination pagination
	}{
		base:       h.newBase(r, "Entities", "entities"),
		Entities:   ents,
		Filters:    map[string]string{"Query": q.Get("q"), "Type": q.Get("type")},
		Pagination: buildPagination(r, int(total), limit, offset, len(ents)),
	}
	h.d.Renderer.Render(w, r, "entities.html", http.StatusOK, data)
}

// EntityDetail shows one entity, its aliases and the items citing it.
func (h *Handlers) EntityDetail(w http.ResponseWriter, r *http.Request) {
	p := api.PrincipalFrom(r.Context())
	id := r.PathValue("id")
	if !ids.Valid(id) {
		h.renderError(w, r, http.StatusNotFound, "Entity not found", "That entity identifier is not valid.")
		return
	}

	ent, err := h.d.Entities.Get(r.Context(), p.WorkspaceID, id)
	if err != nil {
		h.renderError(w, r, http.StatusNotFound, "Entity not found", "That entity does not exist in this workspace.")
		return
	}
	aliases, _ := h.d.Aliases.ListByEntity(r.Context(), p.WorkspaceID, id)
	items, _ := h.d.Items.GetMany(r.Context(), p.WorkspaceID, ent.ItemIDs)

	data := struct {
		base
		Entity  *domain.Entity
		Aliases []*domain.EntityAlias
		Items   []*domain.Item
	}{
		base:    h.newBase(r, ent.CanonicalName, "entities"),
		Entity:  ent,
		Aliases: aliases,
		Items:   items,
	}
	h.d.Renderer.Render(w, r, "entity_detail.html", http.StatusOK, data)
}

// ---------------------------------------------------------------------------
// Jobs
// ---------------------------------------------------------------------------

type jobCounts struct{ Pending, Running, Succeeded, Failed int }

// JobsPage shows the processing queue.
func (h *Handlers) JobsPage(w http.ResponseWriter, r *http.Request) {
	data, err := h.jobsData(r)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "Could not load the queue", "Please try again.")
		return
	}
	h.d.Renderer.Render(w, r, "jobs.html", http.StatusOK, data)
}

// JobsTable renders just the queue table, for HTMX polling.
func (h *Handlers) JobsTable(w http.ResponseWriter, r *http.Request) {
	data, err := h.jobsData(r)
	if err != nil {
		http.Error(w, "Could not load the queue", http.StatusInternalServerError)
		return
	}
	h.d.Renderer.RenderPartial(w, r, "partial_jobs_table.html", data)
}

type jobsPage struct {
	base
	Jobs   []*domain.Job
	Counts jobCounts
}

func (h *Handlers) jobsData(r *http.Request) (jobsPage, error) {
	p := api.PrincipalFrom(r.Context())
	limit, offset := pageParams(r, 40)

	jobs, _, err := h.d.Jobs.List(r.Context(), p.WorkspaceID, nil, nil,
		domain.Page{Limit: limit, Offset: offset})
	if err != nil {
		return jobsPage{}, err
	}

	counts := jobCounts{}
	if c, err := h.d.Jobs.CountByStatus(r.Context(), p.WorkspaceID); err == nil {
		counts.Pending = int(c[domain.JobPending])
		counts.Running = int(c[domain.JobRunning])
		counts.Succeeded = int(c[domain.JobSucceeded])
		counts.Failed = int(c[domain.JobFailed])
	}

	return jobsPage{
		base:   h.newBase(r, "Processing queue", "jobs"),
		Jobs:   jobs,
		Counts: counts,
	}, nil
}

// RetryJob returns a failed job to the queue.
func (h *Handlers) RetryJob(w http.ResponseWriter, r *http.Request) {
	p := api.PrincipalFrom(r.Context())
	id := r.PathValue("id")
	if !ids.Valid(id) {
		http.NotFound(w, r)
		return
	}
	if _, err := h.d.Jobs.Retry(r.Context(), p.WorkspaceID, id); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "Could not retry the job",
			"That job does not exist, or it is not in a state that can be retried.")
		return
	}
	h.d.Audit.Record(r.Context(), p, domain.AuditJobRetried, "job", id, "success", nil)
	http.Redirect(w, r, "/jobs?ok=job_retried", http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

type weightRow struct {
	Name   string
	Weight float64
}

// SettingsPage shows configuration, model status and the scoring model.
func (h *Handlers) SettingsPage(w http.ResponseWriter, r *http.Request) {
	p := api.PrincipalFrom(r.Context())
	ctx := r.Context()
	cfg := h.d.Cfg

	var providers []domain.ProviderHealth
	checkCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	if h.d.EmbedHealth != nil {
		providers = append(providers, h.d.EmbedHealth.Health(checkCtx))
	}
	if h.d.AnalyseHealth != nil {
		st := h.d.AnalyseHealth.Health(checkCtx)
		if !cfg.Ollama.AnalysisEnabled {
			st.Available = false
			st.Detail = "Model-assisted analysis is disabled by configuration (RELATA_ANALYSIS_ENABLED=false)."
		}
		providers = append(providers, st)
	}

	modelVersions, _ := h.d.Models.List(ctx, p.WorkspaceID)

	chunkCount, _ := h.d.Chunks.Count(ctx, p.WorkspaceID)
	vectorCount, verr := h.d.Vectors.CountByWorkspace(ctx, p.WorkspaceID)
	if verr != nil {
		vectorCount = -1
	}

	weights := make([]weightRow, 0, len(relationships.Weights))
	for _, t := range []domain.SignalType{
		domain.SigSemanticSimilarity, domain.SigSharedEntity, domain.SigAliasEntity,
		domain.SigSharedConcept, domain.SigTemporalProximity, domain.SigSharedMetadata,
		domain.SigClaimSupport, domain.SigClaimContradiction, domain.SigModelClassify,
		domain.SigContentHashMatch, domain.SigUserConfirmed, domain.SigUserRejected,
	} {
		weights = append(weights, weightRow{Name: string(t), Weight: relationships.Weights[t]})
	}

	v := currentVersions(h.d.Version, h.d.Embedder.Dimensions())

	data := struct {
		base
		versionInfo
		Providers     []domain.ProviderHealth
		ModelVersions []*domain.ModelVersion
		Limits        map[string]string
		Storage       map[string]any
		Weights       []weightRow
		IsAdmin       bool
		OAuthEnabled  bool
		ProviderName  string
	}{
		base:          h.newBase(r, "Settings", "settings"),
		versionInfo:   v,
		OAuthEnabled:  h.oauthEnabled(),
		ProviderName:  h.providerName(),
		Providers:     providers,
		ModelVersions: modelVersions,
		Limits: map[string]string{
			"MaxUpload":        fmt.Sprintf("%d MB", cfg.Uploads.MaxUploadBytes/(1<<20)),
			"MaxText":          fmt.Sprintf("%d KB", cfg.Uploads.MaxExtractedTextBytes/1024),
			"MaxJSONDepth":     strconv.Itoa(cfg.Uploads.MaxJSONDepth),
			"MaxChunks":        strconv.Itoa(cfg.Chunking.MaxPerItem),
			"ChunkMax":         strconv.Itoa(cfg.Chunking.MaxChars),
			"Workers":          strconv.Itoa(cfg.Workers.Count),
			"AIConcurrency":    strconv.Itoa(cfg.Workers.AIConcurrency),
			"ConcurrentIngest": strconv.Itoa(cfg.Uploads.MaxConcurrentIngest),
			"RelCandidates":    strconv.Itoa(cfg.Rel.MaxCandidates),
			"RelStoreScore":    fmt.Sprintf("%.2f", cfg.Rel.StoreScore),
			"NearDuplicate":    fmt.Sprintf("%.2f", cfg.Rel.NearDuplicateSimilarity),
			"SessionIdle":      cfg.Session.IdleTimeout.String(),
		},
		Storage: map[string]any{
			"MongoDatabase":    cfg.Mongo.Database,
			"QdrantCollection": cfg.Qdrant.Collection,
			"VectorCount":      vectorCount,
			"ChunkCount":       chunkCount,
			"UploadDir":        cfg.Uploads.Dir,
			"Mismatch":         vectorCount >= 0 && vectorCount != chunkCount,
		},
		Weights: weights,
		IsAdmin: p.Role == domain.RoleAdmin,
	}
	h.d.Renderer.Render(w, r, "settings.html", http.StatusOK, data)
}

// Reembed queues a workspace-wide re-embedding.
func (h *Handlers) Reembed(w http.ResponseWriter, r *http.Request) {
	p := api.PrincipalFrom(r.Context())
	if _, err := h.d.Pipeline.RequestReembed(r.Context(), p); err != nil {
		h.renderError(w, r, http.StatusForbidden, "Could not queue re-embedding", friendlyError(err))
		return
	}
	http.Redirect(w, r, "/settings?ok=reembed_queued", http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// friendlyError converts an internal error into something safe to display.
//
// Only recognised, user-actionable conditions get specific text; anything else
// becomes a generic message so that internal detail does not reach the page.
func friendlyError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, domain.ErrForbidden):
		return "You do not have permission to do that."
	case errors.Is(err, domain.ErrNotFound):
		return "That record does not exist in this workspace."
	case errors.Is(err, domain.ErrDuplicate):
		return "Identical content already exists."
	case errors.Is(err, domain.ErrTooLarge):
		return "That content is larger than the configured limit."
	case errors.Is(err, domain.ErrUnsupported):
		return "That file type is not supported."
	case errors.Is(err, search.ErrAnswerDisabled):
		return "Answer generation requires a language model, which is not configured."
	}

	var ve *ingest.ValidationError
	if errors.As(err, &ve) {
		return ve.Message
	}
	var apiErr *api.APIError
	if errors.As(err, &apiErr) && apiErr.Status < 500 {
		return apiErr.Message
	}
	return "The request could not be completed. Please try again."
}

func knownFormError(code string) string {
	switch code {
	case "duplicate":
		return "An item with identical content already exists."
	case "too_large":
		return "That content is larger than the configured limit."
	}
	return "The request could not be completed."
}

// renderFormError re-renders a form with an error and the user's input intact.
func (h *Handlers) renderFormError(w http.ResponseWriter, r *http.Request, page, title, nav, msg string, form map[string]string) {
	if form == nil {
		form = map[string]string{}
	}
	data := struct {
		base
		Form map[string]string
	}{
		base: h.newBase(r, title, nav),
		Form: form,
	}
	data.Error = msg
	h.d.Renderer.Render(w, r, page, http.StatusBadRequest, data)
}

// metadataFromForm reads parallel metadata_key/metadata_value form fields.
func metadataFromForm(r *http.Request) map[string]string {
	keys := r.Form["metadata_key"]
	vals := r.Form["metadata_value"]
	out := map[string]string{}
	for i, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" || i >= len(vals) {
			continue
		}
		out[k] = vals[i]
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
func (h *Handlers) ConfirmAlias(w http.ResponseWriter, r *http.Request) {
	h.reviewAlias(w, r, domain.AliasConfirmed, "alias_confirmed")
}

// RejectAlias records that a person judged two similar names to be different.
func (h *Handlers) RejectAlias(w http.ResponseWriter, r *http.Request) {
	h.reviewAlias(w, r, domain.AliasRejected, "alias_rejected")
}

func (h *Handlers) reviewAlias(w http.ResponseWriter, r *http.Request, status domain.AliasStatus, flashCode string) {
	p := api.PrincipalFrom(r.Context())
	id := r.PathValue("id")
	if !ids.Valid(id) {
		http.NotFound(w, r)
		return
	}
	if err := h.d.Aliases.SetStatus(r.Context(), p.WorkspaceID, id, status); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "Could not record the decision", friendlyError(err))
		return
	}
	h.d.Audit.Record(r.Context(), p, "entity_alias."+string(status), "entity_alias", id, "success", nil)
	redirectBack(w, r, "/entities", "?ok="+flashCode)
}
