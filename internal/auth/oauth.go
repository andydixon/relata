// Package auth also implements federated sign-in.
//
// The flow is OAuth 2.0 Authorization Code with PKCE, upgraded to OpenID
// Connect when the provider offers an issuer. Everything the provider returns
// is treated as untrusted input and validated before it becomes an identity.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// OAuth errors. As with tokens, the caller learns that sign-in failed and the
// specifics go to the log.
var (
	ErrOAuthDisabled  = errors.New("federated sign-in is not configured")
	ErrOAuthState     = errors.New("sign-in state did not match")
	ErrOAuthProvider  = errors.New("the identity provider rejected the request")
	ErrOAuthIdentity  = errors.New("the identity provider returned an unusable identity")
	ErrOAuthNotWanted = errors.New("this account is not permitted to sign in")
)

// maxProviderBody bounds what will be read from the provider. A provider is
// trusted for identity, not for restraint.
const maxProviderBody = 1 << 20

// OAuthConfig is what the service needs to talk to the provider.
type OAuthConfig struct {
	Enabled      bool
	ProviderName string

	Issuer       string
	AuthorizeURL string
	TokenURL     string
	UserInfoURL  string
	JWKSURL      string

	ClientID     string
	ClientSecret string
	RedirectURI  string
	Scopes       []string

	AllowedEmailDomains []string

	Timeout time.Duration
}

// OAuthService performs the authorization-code exchange.
type OAuthService struct {
	cfg    OAuthConfig
	client *http.Client

	// discovery is resolved once, lazily, and cached. A provider's endpoints do
	// not change between requests.
	discoOnce sync.Once
	discoErr  error

	jwks *jwksCache
}

// NewOAuthService builds the service. It performs no network I/O: a provider
// that is unreachable at startup must not stop Relata from starting, because
// the break-glass local login lives in the same process.
func NewOAuthService(cfg OAuthConfig) *OAuthService {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.ProviderName == "" {
		cfg.ProviderName = "your identity provider"
	}
	c := &http.Client{
		Timeout: cfg.Timeout,
		// The provider is configured, not supplied by a user, but a redirect
		// chain is still somewhere a token exchange should not wander. Three is
		// enough for a provider behind a load balancer.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects from the identity provider")
			}
			return nil
		},
	}
	return &OAuthService{cfg: cfg, client: c, jwks: newJWKSCache(c)}
}

// Enabled reports whether federated sign-in is available.
func (s *OAuthService) Enabled() bool { return s != nil && s.cfg.Enabled }

// ProviderName is the label shown on the sign-in button.
func (s *OAuthService) ProviderName() string {
	if s == nil {
		return ""
	}
	return s.cfg.ProviderName
}

// ---------------------------------------------------------------------------
// Starting the flow
// ---------------------------------------------------------------------------

// FlowState is the per-attempt secret material. It is held in a short-lived
// cookie, never server-side: it is single-use, expires in minutes, and storing
// it would mean a write on every visit to the sign-in page.
type FlowState struct {
	State        string `json:"s"`
	Verifier     string `json:"v"`
	Nonce        string `json:"n"`
	RedirectPath string `json:"r,omitempty"`
	IssuedAt     int64  `json:"t"`
}

// AuthorizeURL builds the URL to send the browser to, and the state to keep.
//
// returnTo is where to land after a successful sign-in. It is validated as a
// local path here rather than at the callback, so an open redirect cannot be
// smuggled in through the state cookie.
func (s *OAuthService) AuthorizeURL(ctx context.Context, returnTo string) (string, *FlowState, error) {
	if !s.Enabled() {
		return "", nil, ErrOAuthDisabled
	}
	if err := s.discover(ctx); err != nil {
		return "", nil, err
	}

	state, err := randomURLSafe(32)
	if err != nil {
		return "", nil, err
	}
	verifier, err := randomURLSafe(32)
	if err != nil {
		return "", nil, err
	}
	nonce, err := randomURLSafe(16)
	if err != nil {
		return "", nil, err
	}

	fs := &FlowState{
		State:        state,
		Verifier:     verifier,
		Nonce:        nonce,
		RedirectPath: SafeRedirectPath(returnTo),
		IssuedAt:     time.Now().Unix(),
	}

	challenge := pkceChallenge(verifier)

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", s.cfg.ClientID)
	q.Set("redirect_uri", s.cfg.RedirectURI)
	q.Set("scope", strings.Join(s.cfg.Scopes, " "))
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	if s.isOIDC() {
		q.Set("nonce", nonce)
	}

	sep := "?"
	if strings.Contains(s.cfg.AuthorizeURL, "?") {
		sep = "&"
	}
	return s.cfg.AuthorizeURL + sep + q.Encode(), fs, nil
}

// ---------------------------------------------------------------------------
// Completing the flow
// ---------------------------------------------------------------------------

// Identity is what the provider vouched for, after validation.
type Identity struct {
	// Subject is the provider's immutable identifier. It is the key.
	Subject string
	// Email and DisplayName are for display and the domain allowlist only.
	Email       string
	DisplayName string
	Provider    string
}

// Exchange completes the flow: validates state, swaps the code, and resolves an
// identity.
//
// The state comparison is constant-time and happens before anything is sent to
// the provider, so a mismatched callback costs nothing and reveals nothing.
func (s *OAuthService) Exchange(ctx context.Context, fs *FlowState, gotState, code string) (*Identity, error) {
	if !s.Enabled() {
		return nil, ErrOAuthDisabled
	}
	if fs == nil || fs.State == "" || gotState == "" {
		return nil, ErrOAuthState
	}
	if subtle.ConstantTimeCompare([]byte(fs.State), []byte(gotState)) != 1 {
		return nil, ErrOAuthState
	}
	// A flow that has been open for a long time is more likely to be a replayed
	// callback than a slow typist.
	if age := time.Since(time.Unix(fs.IssuedAt, 0)); age > 15*time.Minute || age < -time.Minute {
		return nil, ErrOAuthState
	}
	if strings.TrimSpace(code) == "" {
		return nil, ErrOAuthProvider
	}
	if err := s.discover(ctx); err != nil {
		return nil, err
	}

	tok, err := s.exchangeCode(ctx, code, fs.Verifier)
	if err != nil {
		return nil, err
	}

	id, err := s.identify(ctx, tok, fs.Nonce)
	if err != nil {
		return nil, err
	}
	if err := s.permitted(id); err != nil {
		return nil, err
	}
	id.Provider = s.cfg.ProviderName
	return id, nil
}

// tokenResponse is the provider's reply to the code exchange.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	IDToken     string `json:"id_token"`
	ExpiresIn   int64  `json:"expires_in"`
	Scope       string `json:"scope"`
}

func (s *OAuthService) exchangeCode(ctx context.Context, code, verifier string) (*tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", s.cfg.RedirectURI)
	form.Set("client_id", s.cfg.ClientID)
	form.Set("code_verifier", verifier)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.TokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("%w: building the token request: %v", ErrOAuthProvider, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// Client credentials go in the Authorization header rather than the body:
	// it is the form every provider accepts, and it keeps the secret out of
	// anything that logs request bodies.
	req.SetBasicAuth(url.QueryEscape(s.cfg.ClientID), url.QueryEscape(s.cfg.ClientSecret))

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOAuthProvider, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderBody))
	if err != nil {
		return nil, fmt.Errorf("%w: reading the token response: %v", ErrOAuthProvider, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The body may name the misconfiguration, which is exactly what an
		// operator needs — and exactly what a user must not be shown.
		return nil, fmt.Errorf("%w: token endpoint returned %d: %s",
			ErrOAuthProvider, resp.StatusCode, truncate(string(body), 300))
	}

	var tr tokenResponse
	dec := json.NewDecoder(strings.NewReader(string(body)))
	if err := dec.Decode(&tr); err != nil {
		return nil, fmt.Errorf("%w: token response was not JSON: %v", ErrOAuthProvider, err)
	}
	if tr.AccessToken == "" && tr.IDToken == "" {
		return nil, fmt.Errorf("%w: token response carried neither an access token nor an ID token",
			ErrOAuthProvider)
	}
	return &tr, nil
}

// identify resolves an identity, preferring a validated ID token.
func (s *OAuthService) identify(ctx context.Context, tok *tokenResponse, nonce string) (*Identity, error) {
	if s.isOIDC() && tok.IDToken != "" {
		id, err := s.identityFromIDToken(ctx, tok.IDToken, nonce)
		if err == nil {
			return id, nil
		}
		// An ID token that fails validation is a hard failure, not a reason to
		// fall back: falling back would let a forged token downgrade the check.
		return nil, err
	}
	if s.cfg.UserInfoURL == "" {
		return nil, fmt.Errorf("%w: no ID token and no userinfo endpoint", ErrOAuthIdentity)
	}
	return s.identityFromUserInfo(ctx, tok.AccessToken)
}

func (s *OAuthService) identityFromUserInfo(ctx context.Context, accessToken string) (*Identity, error) {
	if accessToken == "" {
		return nil, fmt.Errorf("%w: no access token to call userinfo with", ErrOAuthIdentity)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.UserInfoURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOAuthProvider, err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOAuthProvider, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderBody))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOAuthProvider, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: userinfo returned %d: %s",
			ErrOAuthProvider, resp.StatusCode, truncate(string(body), 300))
	}

	var claims map[string]any
	if err := json.Unmarshal(body, &claims); err != nil {
		return nil, fmt.Errorf("%w: userinfo was not JSON: %v", ErrOAuthIdentity, err)
	}
	return identityFromClaims(claims)
}

// permitted applies the optional email-domain allowlist.
func (s *OAuthService) permitted(id *Identity) error {
	if len(s.cfg.AllowedEmailDomains) == 0 {
		return nil
	}
	at := strings.LastIndex(id.Email, "@")
	if at < 0 {
		return fmt.Errorf("%w: an email address is required when a domain allowlist is set",
			ErrOAuthNotWanted)
	}
	got := strings.ToLower(id.Email[at+1:])
	for _, d := range s.cfg.AllowedEmailDomains {
		if strings.EqualFold(strings.TrimPrefix(strings.TrimSpace(d), "@"), got) {
			return nil
		}
	}
	return ErrOAuthNotWanted
}

// identityFromClaims pulls the standard claims out of a claim set.
func identityFromClaims(claims map[string]any) (*Identity, error) {
	sub := claimString(claims, "sub")
	if sub == "" {
		// Without a subject there is nothing stable to key an account on, and
		// guessing from the email is what this design exists to avoid.
		return nil, fmt.Errorf("%w: no 'sub' claim", ErrOAuthIdentity)
	}
	if len(sub) > 255 {
		return nil, fmt.Errorf("%w: 'sub' claim is implausibly long", ErrOAuthIdentity)
	}

	email := strings.ToLower(claimString(claims, "email"))
	name := claimString(claims, "name")
	if name == "" {
		name = strings.TrimSpace(claimString(claims, "given_name") + " " + claimString(claims, "family_name"))
	}
	if name == "" {
		name = claimString(claims, "preferred_username")
	}

	return &Identity{
		Subject:     sub,
		Email:       truncate(email, 320),
		DisplayName: truncate(strings.TrimSpace(name), 120),
	}, nil
}

func claimString(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		// Some providers number their subjects.
		return strings.TrimSpace(fmt.Sprintf("%.0f", v))
	}
	return ""
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *OAuthService) isOIDC() bool {
	return s.cfg.Issuer != "" || s.cfg.JWKSURL != ""
}

// SafeRedirectPath reduces a requested post-login destination to a local path.
//
// Anything that could leave the site becomes "/". Rejecting a leading "//" or a
// backslash matters as much as rejecting a scheme: browsers read "//evil.test"
// as protocol-relative and "/\evil.test" as the same thing.
func SafeRedirectPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || !strings.HasPrefix(p, "/") {
		return "/"
	}
	if strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") {
		return "/"
	}
	if strings.ContainsAny(p, "\r\n") {
		return "/"
	}
	// A path with a scheme in it is not a path.
	if strings.Contains(strings.ToLower(p), "://") {
		return "/"
	}
	return p
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomURLSafe(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
