package api

import (
	"net/http"
	"time"

	"github.com/andydixon/relata/internal/auth"
	"github.com/andydixon/relata/internal/domain"
)

// tokenView is a token as returned to a caller.
//
// There is no field for the secret because there is no secret to return: only
// its hash was stored. The prefix is enough to tell one token from another.
type tokenView struct {
	ID         string              `json:"id"`
	Name       string              `json:"name"`
	Prefix     string              `json:"prefix"`
	Scopes     []domain.TokenScope `json:"scopes"`
	CreatedAt  time.Time           `json:"created_at"`
	ExpiresAt  time.Time           `json:"expires_at"`
	LastUsedAt *time.Time          `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time          `json:"revoked_at,omitempty"`
	Status     string              `json:"status"`
}

func viewToken(t *domain.APIToken, now time.Time) tokenView {
	v := tokenView{
		ID: t.ID, Name: t.Name, Prefix: t.Prefix, Scopes: t.Scopes,
		CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt,
		LastUsedAt: t.LastUsedAt, RevokedAt: t.RevokedAt,
		Status: "active",
	}
	switch {
	case t.RevokedAt != nil:
		v.Status = "revoked"
	case !t.ExpiresAt.IsZero() && !now.Before(t.ExpiresAt):
		v.Status = "expired"
	}
	if v.Scopes == nil {
		v.Scopes = []domain.TokenScope{}
	}
	return v
}

// ListTokens returns the caller's own API tokens.
func (h *Handlers) ListTokens(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	toks, err := h.d.Tokens.List(r.Context(), p)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	now := time.Now().UTC()
	out := make([]tokenView, 0, len(toks))
	for _, t := range toks {
		out = append(out, viewToken(t, now))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"tokens": out, "total": len(out)})
}

// createTokenRequest is the JSON body for issuing a token.
type createTokenRequest struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
	// TTLHours is optional; the configured default applies when it is absent.
	TTLHours int `json:"ttl_hours"`
}

// createTokenResponse is the only place a token secret ever appears.
type createTokenResponse struct {
	Token tokenView `json:"token"`
	// Secret is shown once. It cannot be retrieved again.
	Secret  string `json:"secret"`
	Warning string `json:"warning"`
}

// CreateToken issues an API token for the caller.
func (h *Handlers) CreateToken(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	var req createTokenRequest
	if err := DecodeJSON(w, r, 8<<10, &req); err != nil {
		WriteError(w, r, err)
		return
	}

	scopes, err := parseScopes(req.Scopes)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	ttl := time.Duration(req.TTLHours) * time.Hour
	issued, err := h.d.Tokens.Create(r.Context(), p, auth.CreateRequest{
		Name: req.Name, Scopes: scopes, TTL: ttl,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}

	h.d.Audit.Record(r.Context(), p, domain.AuditTokenCreated, "api_token", issued.Token.ID, "success",
		map[string]string{"name": issued.Token.Name, "prefix": issued.Token.Prefix})

	WriteJSON(w, http.StatusCreated, createTokenResponse{
		Token:  viewToken(issued.Token, time.Now().UTC()),
		Secret: issued.Secret,
		Warning: "This is the only time the token is shown. Store it now; " +
			"Relata keeps only a hash and cannot show it again.",
	})
}

// RevokeToken makes one of the caller's tokens unusable.
func (h *Handlers) RevokeToken(w http.ResponseWriter, r *http.Request) {
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

	if err := h.d.Tokens.Revoke(r.Context(), p, id); err != nil {
		WriteError(w, r, err)
		return
	}

	h.d.Audit.Record(r.Context(), p, domain.AuditTokenRevoked, "api_token", id, "success", nil)
	WriteJSON(w, http.StatusOK, map[string]any{"revoked": true, "id": id})
}

// parseScopes validates requested scopes against the closed set.
//
// An unrecognised scope is refused rather than ignored: silently dropping one
// would issue a token weaker than the caller believes they hold.
func parseScopes(raw []string) ([]domain.TokenScope, error) {
	if len(raw) == 0 {
		return nil, &APIError{
			Status: http.StatusBadRequest, Code: CodeInvalidRequest,
			Message: "Choose at least one scope for the token.",
			Detail:  map[string]string{"available": "ingest, query, review"},
		}
	}
	out := make([]domain.TokenScope, 0, len(raw))
	for _, s := range raw {
		sc := domain.TokenScope(s)
		if !sc.Valid() {
			return nil, &APIError{
				Status: http.StatusBadRequest, Code: CodeInvalidRequest,
				Message: "That is not a recognised scope.",
				Detail:  map[string]string{"scope": s, "available": "ingest, query, review"},
			}
		}
		out = append(out, sc)
	}
	return out, nil
}
