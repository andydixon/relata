package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/andydixon/relata/internal/api"
	"github.com/andydixon/relata/internal/auth"
	"github.com/andydixon/relata/internal/config"
	"github.com/andydixon/relata/internal/domain"
)

type tokenPageRepo struct {
	domain.APITokenRepository
	tokens []*domain.APIToken
}

func (r *tokenPageRepo) Create(_ context.Context, tok *domain.APIToken) error {
	r.tokens = append(r.tokens, tok)
	return nil
}

func (r *tokenPageRepo) ListForUser(_ context.Context, workspaceID, userID string) ([]*domain.APIToken, error) {
	var out []*domain.APIToken
	for _, tok := range r.tokens {
		if tok.WorkspaceID == workspaceID && tok.UserID == userID {
			out = append(out, tok)
		}
	}
	return out, nil
}

type tokenPageRels struct{ domain.RelationshipRepository }

func (tokenPageRels) CountPendingReview(context.Context, string) (int64, error) { return 0, nil }

func newTokenPageHandlers(t *testing.T) (*Handlers, *domain.Principal) {
	t.Helper()
	renderer, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	repo := &tokenPageRepo{}
	tokens := auth.NewTokenService(repo, nil, auth.TokenConfig{
		DefaultTTL: 24 * time.Hour,
		MaxTTL:     30 * 24 * time.Hour,
	})
	cfg := &config.Config{}
	cfg.Auth.OAuth.TokenDefaultTTL = 24 * time.Hour
	cfg.Auth.OAuth.TokenMaxTTL = 30 * 24 * time.Hour
	h := NewHandlers(Deps{Cfg: cfg, Renderer: renderer, Tokens: tokens, Rels: tokenPageRels{}})
	p := &domain.Principal{
		UserID: "user-1", Username: "admin", WorkspaceID: "workspace-1", Role: domain.RoleAdmin,
	}
	return h, p
}

func TestCreateTokenNeverPlacesSecretInRedirectURL(t *testing.T) {
	h, p := newTokenPageHandlers(t)
	form := url.Values{
		"name":     {"automation"},
		"scopes":   {string(domain.ScopeQuery)},
		"ttl_days": {"7"},
	}
	r := httptest.NewRequest(http.MethodPost, "/settings/tokens", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(api.WithPrincipal(r.Context(), p))
	w := httptest.NewRecorder()

	h.CreateToken(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w.Code)
	}
	if location := w.Header().Get("Location"); location != "" {
		t.Fatalf("token creation redirected to %q; the secret could enter URL logs", location)
	}
	if !strings.Contains(w.Body.String(), "rlt_v1_") {
		t.Fatal("the one-time token was not rendered in the creation response")
	}
	if strings.Contains(w.Body.String(), "?secret=") {
		t.Fatal("the rendered page still describes a secret-bearing URL")
	}
}

func TestTokensPageIgnoresSecretQueryParameter(t *testing.T) {
	h, p := newTokenPageHandlers(t)
	r := httptest.NewRequest(http.MethodGet, "/settings/tokens?secret=rlt_v1_attacker_controlled", nil)
	r = r.WithContext(api.WithPrincipal(r.Context(), p))
	w := httptest.NewRecorder()

	h.TokensPage(w, r)

	if strings.Contains(w.Body.String(), "rlt_v1_attacker_controlled") {
		t.Fatal("a secret query parameter was reflected into the token page")
	}
}
