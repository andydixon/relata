package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/andydixon/relata/internal/analysis"
	"github.com/andydixon/relata/internal/api"
	"github.com/andydixon/relata/internal/audit"
	"github.com/andydixon/relata/internal/auth"
	"github.com/andydixon/relata/internal/config"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/ids"
	"github.com/andydixon/relata/internal/ingest"
	"github.com/andydixon/relata/internal/observability"
	"github.com/andydixon/relata/internal/parsing"
	"github.com/andydixon/relata/internal/ranking"
	"github.com/andydixon/relata/internal/relationships"
	"github.com/andydixon/relata/internal/search"
)

// Deps bundles what the web handlers need.
type Deps struct {
	Cfg      *config.Config
	Renderer *Renderer
	Auth     *auth.Service
	// OAuth and Provisioner are nil when federated sign-in is not configured.
	OAuth       *auth.OAuthService
	Provisioner *auth.Provisioner
	Tokens      *auth.TokenService
	Version     string

	Items    domain.ItemRepository
	Chunks   domain.ChunkRepository
	Entities domain.EntityRepository
	Aliases  domain.AliasRepository
	Claims   domain.ClaimRepository
	Rels     domain.RelationshipRepository
	Jobs     domain.JobRepository
	Runs     domain.RunRepository
	Models   domain.ModelVersionRepository
	Users    domain.UserRepository
	Vectors  domain.VectorStore

	Ingest   *ingest.Service
	Pipeline *ingest.Pipeline
	Search   *search.Service
	Answer   *search.Answerer
	Reviewer *relationships.Reviewer
	Analysis *analysis.Service
	Audit    *audit.Recorder

	Embedder      domain.Embedder
	EmbedHealth   domain.HealthChecker
	AnalyseHealth domain.HealthChecker
}

// Handlers serves the HTML interface.
type Handlers struct{ d Deps }

// NewHandlers builds the web handlers.
func NewHandlers(d Deps) *Handlers { return &Handlers{d: d} }

// base carries the fields every page template expects.
type base struct {
	Title         string
	Nav           string
	Principal     *domain.Principal
	CSRFToken     string
	Flash         *flash
	Error         string
	PendingReview int64
	RequestID     string
}

type flash struct {
	Kind    string // success | warn | error | info
	Message string
}

// newBase assembles the shared page context.
func (h *Handlers) newBase(r *http.Request, title, nav string) base {
	b := base{Title: title, Nav: nav, RequestID: observability.RequestID(r.Context())}
	b.Principal = api.PrincipalFrom(r.Context())
	if s := api.SessionFrom(r.Context()); s != nil {
		b.CSRFToken = s.CSRFToken
	}
	if b.Principal != nil {
		if n, err := h.d.Rels.CountPendingReview(r.Context(), b.Principal.WorkspaceID); err == nil {
			b.PendingReview = n
		}
	}
	// A one-shot flash carried in the query string, set by a redirect after a
	// successful action.
	if msg := r.URL.Query().Get("ok"); msg != "" {
		b.Flash = &flash{Kind: "success", Message: knownMessage(msg)}
	}
	if msg := r.URL.Query().Get("warn"); msg != "" {
		b.Flash = &flash{Kind: "warn", Message: knownMessage(msg)}
	}
	return b
}

// knownMessage maps a short code to fixed text.
//
// Flash text is never taken from the query string directly: doing so would let
// a crafted link put arbitrary words on a trusted page.
func knownMessage(code string) string {
	switch code {
	case "item_added":
		return "The item was stored and queued for processing."
	case "item_deleted":
		return "The item is being deleted along with its derived records and vectors."
	case "item_reprocessing":
		return "The item has been queued for reprocessing."
	case "rel_confirmed":
		return "Relationship confirmed. The original proposal and its evidence are retained."
	case "rel_rejected":
		return "Relationship rejected. It is kept on record and will no longer influence search."
	case "rel_updated":
		return "The relationship type was changed. The original proposal is retained."
	case "rel_created":
		return "The relationship was recorded."
	case "job_retried":
		return "The job has been returned to the queue."
	case "alias_confirmed":
		return "Alias confirmed. The entities remain separate records; the confirmation now counts as evidence when relationships are discovered."
	case "alias_rejected":
		return "Alias rejected. The proposal is kept on record so it is not offered again as new."
	case "reembed_queued":
		return "Re-embedding has been queued for this workspace."
	case "duplicate":
		return "An item with identical content already exists. Tick “store even if identical” to keep both."
	case "signed_out":
		return "You have been signed out."
	case "token_revoked":
		return "The token was revoked. It will be refused on its next use."
	case "token_invalid":
		return "The token could not be created. Check the name, the scopes and the expiry."
	}
	return "Done."
}

// renderError shows a friendly error page.
func (h *Handlers) renderError(w http.ResponseWriter, r *http.Request, status int, title, msg string) {
	data := struct {
		base
		ErrorTitle   string
		ErrorMessage string
	}{
		base:         h.newBase(r, title, ""),
		ErrorTitle:   title,
		ErrorMessage: msg,
	}
	h.d.Renderer.Render(w, r, "error.html", status, data)
}

// ---------------------------------------------------------------------------
// Authentication
// ---------------------------------------------------------------------------

// LoginPage shows the sign-in form.
func (h *Handlers) LoginPage(w http.ResponseWriter, r *http.Request) {
	// An already-authenticated visitor goes straight to the dashboard.
	if p, _, err := h.d.Auth.Resolve(r.Context(), h.d.Auth.TokenFromRequest(r)); err == nil && p != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	// The login form needs its own CSRF token before a session exists. One is
	// issued in a short-lived cookie and checked on submission.
	token, err := h.issuePreAuthToken(w)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "Sign-in unavailable",
			"A security token could not be generated. Please try again.")
		return
	}

	firstRun := false
	if n, err := h.d.Users.Count(r.Context()); err == nil && n == 0 {
		firstRun = true
	}

	data := struct {
		base
		FirstRun     bool
		OAuthEnabled bool
		LocalEnabled bool
		ProviderName string
		Next         string
	}{
		base:         base{Title: "Sign in", CSRFToken: token},
		FirstRun:     firstRun,
		OAuthEnabled: h.oauthEnabled(),
		LocalEnabled: h.localLoginEnabled(),
		ProviderName: h.providerName(),
		// Sanitised here rather than in the template: it becomes a URL.
		Next: auth.SafeRedirectPath(r.URL.Query().Get("next")),
	}
	if data.Next == "/" {
		data.Next = "" // nothing worth carrying
	}
	if r.URL.Query().Get("ok") == "signed_out" {
		data.Flash = &flash{Kind: "success", Message: knownMessage("signed_out")}
	}
	if r.URL.Query().Get("error") == "1" {
		data.Error = "The username or password is incorrect."
	}
	if r.URL.Query().Get("error") == "rate" {
		data.Error = "Too many sign-in attempts. Please wait a few minutes and try again."
	}
	h.d.Renderer.Render(w, r, "login.html", http.StatusOK, data)
}

// preAuthCookie holds the CSRF token for the login form itself.
const preAuthCookie = "relata_login_csrf"

func (h *Handlers) issuePreAuthToken(w http.ResponseWriter) (string, error) {
	token, err := ids.Token(32)
	if err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     preAuthCookie,
		Value:    token,
		Path:     "/login",
		MaxAge:   900,
		HttpOnly: true,
		Secure:   h.d.Cfg.HTTP.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	return token, nil
}

// Login processes the sign-in form.
//
// Failures are deliberately uniform: the same message and the same redirect,
// whether the username is unknown, the password is wrong or the account is
// disabled. Nothing about which one it was reaches the client.
func (h *Handlers) Login(w http.ResponseWriter, r *http.Request) {
	// The route is not registered when local login is disabled, so this is
	// belt and braces — but the guarantee should not rest on the router alone.
	if !h.localLoginEnabled() {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
		return
	}

	// Check the pre-authentication CSRF token.
	cookie, err := r.Cookie(preAuthCookie)
	if err != nil || !auth.CheckCSRF(cookie.Value, r.PostFormValue("csrf_token")) {
		observability.LoggerFrom(r.Context()).Warn("login CSRF check failed")
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
		return
	}

	username := r.PostFormValue("username")
	password := r.PostFormValue("password")
	rateKey := auth.ClientKey(r, h.d.Cfg.HTTP.TrustProxyHeaders)

	res, err := h.d.Auth.Login(r.Context(), username, password, rateKey, r.UserAgent())
	if err != nil {
		h.d.Audit.RecordAnonymous(r.Context(), "", domain.AuditLoginFailed, "failure",
			map[string]string{"reason": "invalid_credentials"})

		if errors.Is(err, auth.ErrTooManyAttempts) {
			http.Redirect(w, r, "/login?error=rate", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
		return
	}

	// Clear the pre-auth cookie and issue the session cookie. The session ID is
	// new, so no identifier survives across the authentication boundary.
	http.SetCookie(w, &http.Cookie{
		Name: preAuthCookie, Value: "", Path: "/login", MaxAge: -1,
		HttpOnly: true, Secure: h.d.Cfg.HTTP.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
	h.d.Auth.SetSessionCookie(w, res.Token, res.Session.ExpiresAt)

	h.d.Audit.Record(r.Context(), &domain.Principal{
		UserID: res.User.ID, Username: res.User.Username, WorkspaceID: res.User.WorkspaceID,
	}, domain.AuditLogin, "user", res.User.ID, "success", nil)

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Logout ends the session.
func (h *Handlers) Logout(w http.ResponseWriter, r *http.Request) {
	if s := api.SessionFrom(r.Context()); s != nil {
		_ = h.d.Auth.Logout(r.Context(), s.ID)
		h.d.Audit.Record(r.Context(), api.PrincipalFrom(r.Context()),
			domain.AuditLogout, "session", s.ID, "success", nil)
	}
	h.d.Auth.ClearSessionCookie(w)
	http.Redirect(w, r, "/login?ok=signed_out", http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Dashboard
// ---------------------------------------------------------------------------

type dashStats struct {
	Items, Chunks, Entities, Relationships int64
	PendingJobs, FailedJobs                int
	AwaitingReview                         int
}

type pendingRel struct {
	Rel         *domain.Relationship
	SourceTitle string
	TargetTitle string
}

// Dashboard shows workspace totals and what needs attention.
func (h *Handlers) Dashboard(w http.ResponseWriter, r *http.Request) {
	p := api.PrincipalFrom(r.Context())
	ctx := r.Context()

	stats := dashStats{}
	stats.Items, _ = h.d.Items.Count(ctx, p.WorkspaceID)
	stats.Chunks, _ = h.d.Chunks.Count(ctx, p.WorkspaceID)
	stats.Entities, _ = h.d.Entities.Count(ctx, p.WorkspaceID)
	stats.Relationships, _ = h.d.Rels.Count(ctx, p.WorkspaceID)

	if counts, err := h.d.Jobs.CountByStatus(ctx, p.WorkspaceID); err == nil {
		stats.PendingJobs = int(counts[domain.JobPending] + counts[domain.JobRunning])
		stats.FailedJobs = int(counts[domain.JobFailed])
	}
	if n, err := h.d.Rels.CountPendingReview(ctx, p.WorkspaceID); err == nil {
		stats.AwaitingReview = int(n)
	}

	items, _, _ := h.d.Items.List(ctx, p.WorkspaceID, domain.ItemFilter{}, domain.Page{Limit: 8})

	rels, _, _ := h.d.Rels.List(ctx, p.WorkspaceID, domain.RelationshipFilter{
		ReviewStatus: []domain.ReviewStatus{domain.ReviewPending},
	}, domain.Page{Limit: 8})

	pending := make([]pendingRel, 0, len(rels))
	titles := h.titlesFor(ctx, p.WorkspaceID, rels)
	for _, rel := range rels {
		pending = append(pending, pendingRel{
			Rel: rel, SourceTitle: titles[rel.SourceID], TargetTitle: titles[rel.TargetID],
		})
	}

	data := struct {
		base
		Stats                dashStats
		RecentItems          []*domain.Item
		PendingRelationships []pendingRel
		ProviderWarnings     []string
	}{
		base:                 h.newBase(r, "Dashboard", "dashboard"),
		Stats:                stats,
		RecentItems:          items,
		PendingRelationships: pending,
		ProviderWarnings:     h.providerWarnings(ctx),
	}
	h.d.Renderer.Render(w, r, "dashboard.html", http.StatusOK, data)
}

// providerWarnings reports any AI provider that is unreachable, so the
// dashboard can say plainly which features are degraded rather than leaving a
// user to wonder why entities stopped appearing.
func (h *Handlers) providerWarnings(ctx context.Context) []string {
	var out []string
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	if h.d.EmbedHealth != nil {
		if st := h.d.EmbedHealth.Health(checkCtx); !st.Available {
			msg := "The embedding model is unavailable, so new items cannot be indexed for meaning-based search."
			if st.Detail != "" {
				msg += " " + st.Detail
			}
			out = append(out, msg)
		}
	}
	if h.d.AnalyseHealth != nil && h.d.Cfg.Ollama.AnalysisEnabled {
		if st := h.d.AnalyseHealth.Health(checkCtx); !st.Available {
			msg := "The analysis model is unavailable. Entity and claim extraction fall back to deterministic rules, and generated answers are disabled."
			if st.Detail != "" {
				msg += " " + st.Detail
			}
			out = append(out, msg)
		}
	}
	return out
}

// titlesFor resolves item titles at both ends of a set of relationships.
func (h *Handlers) titlesFor(ctx context.Context, workspaceID string, rels []*domain.Relationship) map[string]string {
	var idList []string
	for _, rel := range rels {
		idList = append(idList, rel.SourceID, rel.TargetID)
	}
	out := map[string]string{}
	if len(idList) == 0 {
		return out
	}
	items, err := h.d.Items.GetMany(ctx, workspaceID, idList)
	if err != nil {
		return out
	}
	for _, it := range items {
		out[it.ID] = it.Title
	}
	return out
}

// ---------------------------------------------------------------------------
// Items
// ---------------------------------------------------------------------------

type pagination struct {
	Total            int
	From, To         int
	HasMore          bool
	PrevURL, NextURL string
}

func buildPagination(r *http.Request, total, limit, offset, shown int) pagination {
	p := pagination{Total: total, From: offset + 1, To: offset + shown}
	if shown == 0 {
		p.From = 0
	}
	p.HasMore = offset+shown < total

	mk := func(newOffset int) string {
		q := r.URL.Query()
		q.Set("offset", strconv.Itoa(newOffset))
		return r.URL.Path + "?" + q.Encode()
	}
	if offset > 0 {
		prev := offset - limit
		if prev < 0 {
			prev = 0
		}
		p.PrevURL = mk(prev)
	}
	if p.HasMore {
		p.NextURL = mk(offset + limit)
	}
	return p
}

// ItemsPage lists stored items.
func (h *Handlers) ItemsPage(w http.ResponseWriter, r *http.Request) {
	p := api.PrincipalFrom(r.Context())
	q := r.URL.Query()

	f := domain.ItemFilter{TitleContains: strings.TrimSpace(q.Get("title"))}
	if st := q.Get("source_type"); st != "" {
		if s := domain.SourceType(st); s.Valid() {
			f.SourceTypes = []domain.SourceType{s}
		}
	}
	if s := q.Get("status"); s != "" {
		f.Statuses = []domain.ProcessingStatus{domain.ProcessingStatus(s)}
	}

	limit, offset := pageParams(r, 25)
	items, total, err := h.d.Items.List(r.Context(), p.WorkspaceID, f, domain.Page{Limit: limit, Offset: offset})
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "Could not list items", "Please try again.")
		return
	}

	data := struct {
		base
		Items      []*domain.Item
		Filters    map[string]string
		Pagination pagination
	}{
		base:  h.newBase(r, "Items", "items"),
		Items: items,
		Filters: map[string]string{
			"Title": q.Get("title"), "SourceType": q.Get("source_type"), "Status": q.Get("status"),
		},
		Pagination: buildPagination(r, int(total), limit, offset, len(items)),
	}
	h.d.Renderer.Render(w, r, "items.html", http.StatusOK, data)
}

// ItemDetail shows one item and everything derived from it.
func (h *Handlers) ItemDetail(w http.ResponseWriter, r *http.Request) {
	p := api.PrincipalFrom(r.Context())
	id := r.PathValue("id")
	if !ids.Valid(id) {
		h.renderError(w, r, http.StatusNotFound, "Item not found", "That item identifier is not valid.")
		return
	}

	ctx := r.Context()
	item, err := h.d.Items.Get(ctx, p.WorkspaceID, id)
	if err != nil {
		h.renderError(w, r, http.StatusNotFound, "Item not found",
			"That item does not exist in this workspace, or it has been deleted.")
		return
	}

	chunks, _ := h.d.Chunks.ListByItem(ctx, p.WorkspaceID, id)
	ents, _ := h.d.Entities.ListByItem(ctx, p.WorkspaceID, id)
	claims, _ := h.d.Claims.ListByItem(ctx, p.WorkspaceID, id)
	outRels, inRels, _ := h.d.Rels.ListForNode(ctx, p.WorkspaceID, domain.NodeItem, id, domain.RelationshipFilter{})
	runs, _ := h.d.Runs.ListByItem(ctx, p.WorkspaceID, id, 10)
	others, _, _ := h.d.Items.List(ctx, p.WorkspaceID, domain.ItemFilter{}, domain.Page{Limit: 100})

	// Exclude this item from the "add a relationship" picker.
	picker := make([]*domain.Item, 0, len(others))
	for _, it := range others {
		if it.ID != id {
			picker = append(picker, it)
		}
	}

	titles := h.titlesFor(ctx, p.WorkspaceID, append(append([]*domain.Relationship{}, outRels...), inRels...))
	titles[item.ID] = item.Title

	data := struct {
		base
		Item              *domain.Item
		Chunks            []*domain.Chunk
		Entities          []*domain.Entity
		Claims            []*domain.Claim
		Outgoing          []relView
		Incoming          []relView
		Runs              []*domain.ProcessingRun
		OtherItems        []*domain.Item
		RelationshipCount int
	}{
		base:              h.newBase(r, item.Title, "items"),
		Item:              item,
		Chunks:            chunks,
		Entities:          ents,
		Claims:            claims,
		Outgoing:          toRelViews(outRels, titles),
		Incoming:          toRelViews(inRels, titles),
		Runs:              runs,
		OtherItems:        picker,
		RelationshipCount: len(outRels) + len(inRels),
	}
	h.d.Renderer.Render(w, r, "item_detail.html", http.StatusOK, data)
}

// relView is a relationship with its endpoint titles resolved for display.
type relView struct {
	Rel         *domain.Relationship
	SourceID    string
	TargetID    string
	SourceTitle string
	TargetTitle string
}

func toRelViews(rels []*domain.Relationship, titles map[string]string) []relView {
	out := make([]relView, 0, len(rels))
	for _, rel := range rels {
		out = append(out, relView{
			Rel:      rel,
			SourceID: rel.SourceID, TargetID: rel.TargetID,
			SourceTitle: titles[rel.SourceID], TargetTitle: titles[rel.TargetID],
		})
	}
	return out
}

// Neighbourhood shows an item and its directly connected items.
func (h *Handlers) Neighbourhood(w http.ResponseWriter, r *http.Request) {
	p := api.PrincipalFrom(r.Context())
	id := r.PathValue("id")
	if !ids.Valid(id) {
		h.renderError(w, r, http.StatusNotFound, "Item not found", "That item identifier is not valid.")
		return
	}

	ctx := r.Context()
	item, err := h.d.Items.Get(ctx, p.WorkspaceID, id)
	if err != nil {
		h.renderError(w, r, http.StatusNotFound, "Item not found", "That item does not exist in this workspace.")
		return
	}

	outRels, inRels, _ := h.d.Rels.ListForNode(ctx, p.WorkspaceID, domain.NodeItem, id, domain.RelationshipFilter{})
	titles := h.titlesFor(ctx, p.WorkspaceID, append(append([]*domain.Relationship{}, outRels...), inRels...))

	type neighbour struct {
		Rel        *domain.Relationship
		OtherID    string
		OtherTitle string
	}
	mk := func(rels []*domain.Relationship, takeTarget bool) []neighbour {
		out := make([]neighbour, 0, len(rels))
		for _, rel := range rels {
			other := rel.SourceID
			if takeTarget {
				other = rel.TargetID
			}
			out = append(out, neighbour{Rel: rel, OtherID: other, OtherTitle: titles[other]})
		}
		return out
	}

	data := struct {
		base
		Item     *domain.Item
		Outgoing []neighbour
		Incoming []neighbour
	}{
		base:     h.newBase(r, "Neighbourhood", "items"),
		Item:     item,
		Outgoing: mk(outRels, true),
		Incoming: mk(inRels, false),
	}
	h.d.Renderer.Render(w, r, "neighbourhood.html", http.StatusOK, data)
}

// DownloadOriginal serves an item's original uploaded file.
//
// The file is always sent as an attachment with a generic content type, so a
// browser downloads it rather than rendering it. That is what stops an uploaded
// HTML or SVG file executing script in Relata's origin.
func (h *Handlers) DownloadOriginal(w http.ResponseWriter, r *http.Request) {
	p := api.PrincipalFrom(r.Context())
	id := r.PathValue("id")
	if !ids.Valid(id) {
		http.NotFound(w, r)
		return
	}

	item, err := h.d.Items.Get(r.Context(), p.WorkspaceID, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	content, err := h.d.Ingest.ReadOriginal(item)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	name := ingest.SanitiseFilename(item.OriginalFilename)
	if name == "" {
		name = "item-" + item.ID
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename*=UTF-8''%s", url.PathEscape(name)))
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	_, _ = w.Write(content)
}

func pageParams(r *http.Request, def int) (limit, offset int) {
	limit = def
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	return limit, offset
}

// versionInfo is used by the settings page.
type versionInfo struct {
	Version       string
	ParserVersion string
	ScoreVersion  string
	RankVersion   string
	EmbeddingDims int
}

func currentVersions(appVersion string, dims int) versionInfo {
	return versionInfo{
		Version:       appVersion,
		ParserVersion: parsing.Version,
		ScoreVersion:  relationships.ScoreVersion,
		RankVersion:   ranking.RankVersion,
		EmbeddingDims: dims,
	}
}
