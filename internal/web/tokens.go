package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/andydixon/relata/internal/api"
	"github.com/andydixon/relata/internal/auth"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/observability"
)

// tokenRow is one token as shown on the page.
type tokenRow struct {
	Token   *domain.APIToken
	Status  string
	Expired bool
	Revoked bool
	Scopes  string
}

// scopeChoice is one selectable scope on the creation form.
type scopeChoice struct {
	Scope       domain.TokenScope
	Label       string
	Description string
	Available   bool
}

// TokensPage lists the caller's API tokens and offers the creation form.
func (h *Handlers) TokensPage(w http.ResponseWriter, r *http.Request) {
	p := api.PrincipalFrom(r.Context())
	if p == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	h.renderTokensPage(w, r, p, "", "", http.StatusOK)
}

// renderTokensPage renders the token list and, after creation, the one-time
// secret directly in the POST response. A bearer credential must never travel
// through a redirect URL, where it would enter browser history and proxy logs.
func (h *Handlers) renderTokensPage(w http.ResponseWriter, r *http.Request, p *domain.Principal,
	newSecret, newName string, status int) {
	toks, err := h.d.Tokens.List(r.Context(), p)
	if err != nil {
		if newSecret == "" {
			h.renderError(w, r, http.StatusInternalServerError, "Could not list tokens",
				"The token list could not be read. Please try again.")
			return
		}
		// Creation has already succeeded and the secret is unrecoverable. Show it
		// even if the follow-up list refresh failed rather than losing it forever.
		toks = nil
	}

	now := time.Now().UTC()
	rows := make([]tokenRow, 0, len(toks))
	for _, t := range toks {
		row := tokenRow{Token: t, Status: "Active", Scopes: joinScopes(t.Scopes)}
		switch {
		case t.RevokedAt != nil:
			row.Status, row.Revoked = "Revoked", true
		case !t.ExpiresAt.IsZero() && !now.Before(t.ExpiresAt):
			row.Status, row.Expired = "Expired", true
		}
		rows = append(rows, row)
	}

	data := struct {
		base
		Tokens []tokenRow
		Scopes []scopeChoice
		// NewSecret is set only immediately after creation.
		NewSecret  string
		NewName    string
		DefaultTTL int
		MaxTTL     int
	}{
		base:       h.newBase(r, "API tokens", "settings"),
		Tokens:     rows,
		Scopes:     scopeChoicesFor(p.Role),
		NewSecret:  newSecret,
		NewName:    newName,
		DefaultTTL: int(h.d.Cfg.Auth.OAuth.TokenDefaultTTL.Hours() / 24),
		MaxTTL:     int(h.d.Cfg.Auth.OAuth.TokenMaxTTL.Hours() / 24),
	}
	if err != nil {
		data.Error = "The token was created, but the token list could not be refreshed. Copy the new token now."
	}
	h.d.Renderer.Render(w, r, "tokens.html", status, data)
}

// CreateToken issues a token from the interface.
func (h *Handlers) CreateToken(w http.ResponseWriter, r *http.Request) {
	p := api.PrincipalFrom(r.Context())
	if p == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/settings/tokens?warn=token_invalid", http.StatusSeeOther)
		return
	}

	scopes := make([]domain.TokenScope, 0, 3)
	for _, raw := range r.PostForm["scopes"] {
		sc := domain.TokenScope(strings.TrimSpace(raw))
		if sc.Valid() {
			scopes = append(scopes, sc)
		}
	}

	days, _ := strconv.Atoi(r.PostFormValue("ttl_days"))
	ttl := time.Duration(days) * 24 * time.Hour

	issued, err := h.d.Tokens.Create(r.Context(), p, auth.CreateRequest{
		Name:   r.PostFormValue("name"),
		Scopes: scopes,
		TTL:    ttl,
	})
	if err != nil {
		observability.LoggerFrom(r.Context()).Warn("token creation refused", "error", err.Error())
		http.Redirect(w, r, "/settings/tokens?warn=token_invalid", http.StatusSeeOther)
		return
	}

	h.d.Audit.Record(r.Context(), p, domain.AuditTokenCreated, "api_token", issued.Token.ID, "success",
		map[string]string{"name": issued.Token.Name, "prefix": issued.Token.Prefix})

	// Render the one-time secret in this response. Redirect URLs must never carry
	// bearer credentials: URLs are retained by browsers, proxies and access logs.
	h.renderTokensPage(w, r, p, issued.Secret, issued.Token.Name, http.StatusCreated)
}

// RevokeToken makes a token unusable from the interface.
func (h *Handlers) RevokeToken(w http.ResponseWriter, r *http.Request) {
	p := api.PrincipalFrom(r.Context())
	if p == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	id := r.PathValue("id")
	if err := h.d.Tokens.Revoke(r.Context(), p, id); err != nil {
		http.Redirect(w, r, "/settings/tokens?warn=token_invalid", http.StatusSeeOther)
		return
	}
	h.d.Audit.Record(r.Context(), p, domain.AuditTokenRevoked, "api_token", id, "success", nil)
	http.Redirect(w, r, "/settings/tokens?ok=token_revoked", http.StatusSeeOther)
}

// scopeChoicesFor describes the scopes, marking those the role cannot grant.
//
// An unavailable scope is shown and disabled rather than hidden, so a viewer
// can see that ingestion exists and why it is not offered to them.
func scopeChoicesFor(role domain.Role) []scopeChoice {
	return []scopeChoice{
		{
			Scope: domain.ScopeQuery, Label: "Query",
			Description: "Search, generate answers, and read stored items, entities and relationships.",
			Available:   domain.RoleGrants(role, domain.ScopeQuery),
		},
		{
			Scope: domain.ScopeIngest, Label: "Ingest",
			Description: "Add items, upload files, reprocess and delete.",
			Available:   domain.RoleGrants(role, domain.ScopeIngest),
		},
		{
			Scope: domain.ScopeReview, Label: "Review",
			Description: "Confirm, reject and retype relationships; confirm entity aliases.",
			Available:   domain.RoleGrants(role, domain.ScopeReview),
		},
	}
}

func joinScopes(scopes []domain.TokenScope) string {
	if len(scopes) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(scopes))
	for _, s := range scopes {
		parts = append(parts, string(s))
	}
	return strings.Join(parts, ", ")
}
