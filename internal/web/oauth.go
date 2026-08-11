package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/andydixon/relata/internal/auth"
	"github.com/andydixon/relata/internal/config"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/observability"
)

// oauthFlowCookie holds the per-attempt state, PKCE verifier and nonce.
//
// It is scoped to the callback path, HttpOnly and SameSite=Lax — Lax rather
// than Strict because the browser arrives here as a top-level navigation from
// the provider, and Strict would withhold the cookie exactly when it is needed.
const oauthFlowCookie = "relata_oauth_flow"

// oauthFlowTTL bounds how long a sign-in attempt may stay open.
const oauthFlowTTL = 15 * time.Minute

// OAuthStart begins federated sign-in.
func (h *Handlers) OAuthStart(w http.ResponseWriter, r *http.Request) {
	if h.d.OAuth == nil || !h.d.OAuth.Enabled() {
		h.renderError(w, r, http.StatusNotFound, "Not available",
			"Federated sign-in is not configured on this instance.")
		return
	}
	// Already signed in: nothing to do.
	if p, _, err := h.d.Auth.Resolve(r.Context(), h.d.Auth.TokenFromRequest(r)); err == nil && p != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	url, fs, err := h.d.OAuth.AuthorizeURL(r.Context(), r.URL.Query().Get("next"))
	if err != nil {
		observability.LoggerFrom(r.Context()).Error("could not begin federated sign-in",
			"error", err.Error())
		h.renderError(w, r, http.StatusBadGateway, "Sign-in unavailable",
			"The identity provider could not be reached. Please try again shortly.")
		return
	}

	if err := h.setFlowCookie(w, fs); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "Sign-in unavailable",
			"A security token could not be generated. Please try again.")
		return
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}

// OAuthCallback completes federated sign-in.
//
// Every failure renders the same generic page. The specifics — a mismatched
// state, a rejected token exchange, an unverifiable ID token — go to the log,
// because they describe the provider's configuration and an unauthenticated
// visitor has no business reading it.
func (h *Handlers) OAuthCallback(w http.ResponseWriter, r *http.Request) {
	if h.d.OAuth == nil || !h.d.OAuth.Enabled() {
		h.renderError(w, r, http.StatusNotFound, "Not available",
			"Federated sign-in is not configured on this instance.")
		return
	}

	log := observability.LoggerFrom(r.Context())

	// The flow cookie is single-use whatever happens next.
	fs, cookieErr := h.readFlowCookie(r)
	h.clearFlowCookie(w)

	// A provider that refused the request says so in the query string.
	if e := r.URL.Query().Get("error"); e != "" {
		log.Warn("identity provider refused the sign-in",
			"provider_error", truncateForLog(e, 200),
			"description", truncateForLog(r.URL.Query().Get("error_description"), 200))
		h.oauthFailed(w, r, "The identity provider did not complete the sign-in.")
		return
	}
	if cookieErr != nil {
		log.Warn("federated sign-in had no usable flow state", "error", cookieErr.Error())
		h.oauthFailed(w, r, "That sign-in attempt has expired. Please start again.")
		return
	}

	identity, err := h.d.OAuth.Exchange(r.Context(), fs,
		r.URL.Query().Get("state"), r.URL.Query().Get("code"))
	if err != nil {
		log.Warn("federated sign-in failed", "error", err.Error())
		h.d.Audit.RecordAnonymous(r.Context(), "", domain.AuditOAuthFailed, "failure",
			map[string]string{"reason": oauthFailureReason(err)})

		switch {
		case errors.Is(err, auth.ErrOAuthNotWanted):
			h.oauthFailed(w, r, "That account is not permitted to use this instance.")
		case errors.Is(err, auth.ErrOAuthState):
			h.oauthFailed(w, r, "That sign-in attempt could not be verified. Please start again.")
		default:
			h.oauthFailed(w, r, "Sign-in could not be completed. Please try again shortly.")
		}
		return
	}

	res, err := h.d.Provisioner.SignIn(r.Context(), h.d.OAuth.ProviderName(), identity, r.UserAgent())
	if err != nil {
		log.Error("could not establish a session after federated sign-in", "error", err.Error())
		h.d.Audit.RecordAnonymous(r.Context(), "", domain.AuditOAuthFailed, "failure",
			map[string]string{"reason": "provisioning"})
		h.oauthFailed(w, r, "Your account could not be prepared. Please try again shortly.")
		return
	}

	h.d.Auth.SetSessionCookie(w, res.Token, res.Session.ExpiresAt)
	h.d.Audit.Record(r.Context(), &domain.Principal{
		UserID: res.User.ID, Username: res.User.Username, WorkspaceID: res.User.WorkspaceID,
	}, domain.AuditOAuthLogin, "user", res.User.ID, "success", nil)

	// The destination was reduced to a local path when the flow began, so it
	// cannot send the browser off-site here.
	dest := auth.SafeRedirectPath(fs.RedirectPath)
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func (h *Handlers) oauthFailed(w http.ResponseWriter, r *http.Request, msg string) {
	h.renderError(w, r, http.StatusUnauthorized, "Sign-in failed", msg)
}

// oauthFailureReason maps an error to a bounded audit label, so the audit trail
// stays queryable and never accumulates provider text as unbounded cardinality.
func oauthFailureReason(err error) string {
	switch {
	case errors.Is(err, auth.ErrOAuthState):
		return "state_mismatch"
	case errors.Is(err, auth.ErrOAuthNotWanted):
		return "not_permitted"
	case errors.Is(err, auth.ErrOAuthIdentity):
		return "invalid_identity"
	case errors.Is(err, auth.ErrOAuthProvider):
		return "provider_error"
	case errors.Is(err, auth.ErrOAuthDisabled):
		return "disabled"
	}
	return "unknown"
}

// ---------------------------------------------------------------------------
// Flow cookie
// ---------------------------------------------------------------------------

func (h *Handlers) setFlowCookie(w http.ResponseWriter, fs *auth.FlowState) error {
	raw, err := json.Marshal(fs)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:  oauthFlowCookie,
		Value: base64.RawURLEncoding.EncodeToString(raw),
		// Scoped to the OAuth endpoints: it has no business being attached to
		// any other request.
		Path:     "/auth/oauth",
		HttpOnly: true,
		Secure:   h.d.Cfg.HTTP.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(oauthFlowTTL.Seconds()),
	})
	return nil
}

func (h *Handlers) readFlowCookie(r *http.Request) (*auth.FlowState, error) {
	c, err := r.Cookie(oauthFlowCookie)
	if err != nil {
		return nil, errors.New("no sign-in flow cookie")
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return nil, errors.New("sign-in flow cookie is malformed")
	}
	// The cookie is our own, but it arrives from the browser, so it is decoded
	// strictly like any other untrusted input.
	dec := json.NewDecoder(newLimitedReader(raw, 4<<10))
	dec.DisallowUnknownFields()
	var fs auth.FlowState
	if err := dec.Decode(&fs); err != nil {
		return nil, errors.New("sign-in flow cookie is not valid")
	}
	if fs.State == "" || fs.Verifier == "" {
		return nil, errors.New("sign-in flow cookie is incomplete")
	}
	return &fs, nil
}

func (h *Handlers) clearFlowCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: oauthFlowCookie, Value: "", Path: "/auth/oauth", MaxAge: -1,
		HttpOnly: true, Secure: h.d.Cfg.HTTP.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
}

// oauthEnabled reports whether the sign-in button should be shown.
func (h *Handlers) oauthEnabled() bool {
	return h.d.OAuth != nil && h.d.OAuth.Enabled()
}

// localLoginEnabled reports whether the password form should be shown.
func (h *Handlers) localLoginEnabled() bool {
	return h.d.Cfg != nil && h.d.Cfg.Auth.LocalLoginEnabled
}

// providerName is the label for the sign-in button.
func (h *Handlers) providerName() string {
	if h.d.OAuth == nil {
		return ""
	}
	return h.d.OAuth.ProviderName()
}

// callbackPath is exported to the settings page so an operator can see exactly
// what to register with the provider.
func (h *Handlers) callbackURL() string {
	if h.d.Cfg == nil {
		return config.CallbackPath
	}
	return h.d.Cfg.RedirectURI()
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// newLimitedReader wraps a byte slice so the JSON decoder cannot be handed more
// than the caller intended, even if the cookie is enormous.
func newLimitedReader(b []byte, max int64) *io.LimitedReader {
	return &io.LimitedReader{R: bytes.NewReader(b), N: max}
}
