package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- Redirect safety --------------------------------------------------------

// The post-login destination comes from a query parameter, so it is
// attacker-controlled. Anything that could leave the site must become "/".
func TestSafeRedirectPathRefusesAnythingThatLeavesTheSite(t *testing.T) {
	hostile := []string{
		"//evil.test",
		"//evil.test/path",
		"/\\evil.test",
		"https://evil.test",
		"http://evil.test",
		"javascript:alert(1)",
		"/redirect?to=https://evil.test\r\nSet-Cookie: a=b",
		"evil.test",
		"",
		"   ",
		"\\\\evil.test",
	}
	for _, in := range hostile {
		if got := SafeRedirectPath(in); got != "/" {
			t.Errorf("SafeRedirectPath(%q) = %q; anything that could leave the site must become \"/\"", in, got)
		}
	}

	// Ordinary local paths survive, or the feature would be pointless.
	for _, in := range []string{"/", "/items", "/search?q=cost", "/items/abc-123"} {
		if got := SafeRedirectPath(in); got != in {
			t.Errorf("SafeRedirectPath(%q) = %q; a local path should survive", in, got)
		}
	}
}

// --- PKCE -------------------------------------------------------------------

func TestAuthorizeURLAlwaysUsesPKCEAndAFreshState(t *testing.T) {
	s := NewOAuthService(OAuthConfig{
		Enabled: true, ProviderName: "Blackwall",
		AuthorizeURL: "https://id.example/authorize",
		TokenURL:     "https://id.example/token",
		UserInfoURL:  "https://id.example/userinfo",
		ClientID:     "client-1", ClientSecret: "secret",
		RedirectURI: "https://relata.example/auth/oauth/callback",
		Scopes:      []string{"openid", "email"},
	})

	u1, fs1, err := s.AuthorizeURL(context.Background(), "/items")
	if err != nil {
		t.Fatal(err)
	}
	u2, fs2, err := s.AuthorizeURL(context.Background(), "/items")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(u1, "code_challenge_method=S256") {
		t.Fatalf("authorize URL does not request S256 PKCE:\n%s", u1)
	}
	if strings.Contains(u1, fs1.Verifier) {
		t.Fatal("the PKCE verifier appears in the authorize URL; only its hash may be sent")
	}
	if fs1.State == fs2.State || fs1.Verifier == fs2.Verifier {
		t.Fatal("two sign-in attempts reused state or verifier; each must be fresh")
	}
	if len(fs1.State) < 32 {
		t.Fatalf("state is only %d characters; it must be unguessable", len(fs1.State))
	}
	if fs1.RedirectPath != "/items" {
		t.Fatalf("redirect path = %q, want /items", fs1.RedirectPath)
	}
	_ = u2
}

func TestAuthorizeURLSanitisesTheReturnPath(t *testing.T) {
	s := NewOAuthService(OAuthConfig{
		Enabled: true, AuthorizeURL: "https://id.example/authorize",
		TokenURL: "https://id.example/token", UserInfoURL: "https://id.example/userinfo",
		ClientID: "c", ClientSecret: "s",
	})
	_, fs, err := s.AuthorizeURL(context.Background(), "//evil.test")
	if err != nil {
		t.Fatal(err)
	}
	if fs.RedirectPath != "/" {
		t.Fatalf("an off-site return path survived into the flow state: %q", fs.RedirectPath)
	}
}

// --- State ------------------------------------------------------------------

func TestExchangeRefusesAMismatchedOrStaleState(t *testing.T) {
	s := NewOAuthService(OAuthConfig{
		Enabled: true, AuthorizeURL: "https://id.example/authorize",
		TokenURL: "https://id.example/token", UserInfoURL: "https://id.example/userinfo",
		ClientID: "c", ClientSecret: "s",
	})
	ctx := context.Background()

	fresh := &FlowState{State: "expected", Verifier: "v", IssuedAt: time.Now().Unix()}

	cases := []struct {
		name  string
		fs    *FlowState
		state string
	}{
		{"no flow state at all", nil, "expected"},
		{"empty submitted state", fresh, ""},
		{"different state", fresh, "attacker"},
		{"prefix of the real state", fresh, "expec"},
		{"stale flow", &FlowState{State: "expected", IssuedAt: time.Now().Add(-30 * time.Minute).Unix()}, "expected"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.Exchange(ctx, c.fs, c.state, "some-code")
			if !errors.Is(err, ErrOAuthState) {
				t.Fatalf("err = %v, want ErrOAuthState — a callback that fails the state check "+
					"must be refused before anything is sent to the provider", err)
			}
		})
	}
}

// --- Identity ---------------------------------------------------------------

// The subject is the account key. Without one there is nothing stable to key on,
// and falling back to the email is precisely the bug this avoids.
func TestIdentityRequiresASubject(t *testing.T) {
	if _, err := identityFromClaims(map[string]any{"email": "someone@example.com"}); err == nil {
		t.Fatal("an identity with an email but no subject was accepted")
	}
	id, err := identityFromClaims(map[string]any{"sub": "abc", "email": "A@Example.COM", "name": "A Person"})
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "abc" {
		t.Fatalf("subject = %q", id.Subject)
	}
	if id.Email != "a@example.com" {
		t.Fatalf("email = %q; it should be lowercased for comparison", id.Email)
	}
}

func TestEmailDomainAllowlist(t *testing.T) {
	s := NewOAuthService(OAuthConfig{
		Enabled: true, AllowedEmailDomains: []string{"example.com", "@other.test"},
	})
	for _, c := range []struct {
		email string
		want  bool
	}{
		{"someone@example.com", true},
		{"someone@EXAMPLE.com", true},
		{"someone@other.test", true},
		{"someone@evil.test", false},
		{"someone@sub.example.com", false}, // a subdomain is a different domain
		{"", false},
		{"notanemail", false},
		{"someone@example.com.evil.test", false},
	} {
		err := s.permitted(&Identity{Email: c.email})
		if (err == nil) != c.want {
			t.Errorf("permitted(%q): err=%v, want allowed=%v", c.email, err, c.want)
		}
	}
}

// --- ID token validation ----------------------------------------------------

type testSigner struct {
	key *rsa.PrivateKey
	kid string
}

func newTestSigner(t *testing.T) *testSigner {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &testSigner{key: k, kid: "test-key-1"}
}

// jwksHandler serves the signer's public key in JWKS form.
func (s *testSigner) jwksHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(s.key.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(s.key.E)).Bytes())
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{
				{"kty": "RSA", "use": "sig", "kid": s.kid, "n": n, "e": e},
			},
		})
	})
}

func (s *testSigner) sign(t *testing.T, alg string, claims map[string]any) string {
	t.Helper()
	header := map[string]string{"alg": alg, "typ": "JWT", "kid": s.kid}
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)

	h := crypto.SHA256.New()
	h.Write([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, h.Sum(nil))
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func oidcService(t *testing.T, jwksURL, issuer, clientID string) *OAuthService {
	t.Helper()
	return NewOAuthService(OAuthConfig{
		Enabled: true, Issuer: issuer, JWKSURL: jwksURL,
		AuthorizeURL: issuer + "/authorize", TokenURL: issuer + "/token",
		ClientID: clientID, ClientSecret: "secret",
	})
}

func TestIDTokenValidationAcceptsAGenuineToken(t *testing.T) {
	signer := newTestSigner(t)
	srv := httptest.NewServer(signer.jwksHandler())
	defer srv.Close()

	s := oidcService(t, srv.URL, "https://id.example", "client-1")
	raw := signer.sign(t, "RS256", map[string]any{
		"iss": "https://id.example", "aud": "client-1", "sub": "user-42",
		"email": "person@example.com", "name": "A Person",
		"nonce": "n-1", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	})

	id, err := s.identityFromIDToken(context.Background(), raw, "n-1")
	if err != nil {
		t.Fatalf("a correctly signed token was rejected: %v", err)
	}
	if id.Subject != "user-42" {
		t.Fatalf("subject = %q", id.Subject)
	}
}

// Each of these is a real-world JWT forgery or a replay. None may pass.
func TestIDTokenValidationRejectsForgeriesAndReplays(t *testing.T) {
	signer := newTestSigner(t)
	other := newTestSigner(t)
	other.kid = signer.kid // same key ID, different key: a substitution attack

	srv := httptest.NewServer(signer.jwksHandler())
	defer srv.Close()

	base := func() map[string]any {
		return map[string]any{
			"iss": "https://id.example", "aud": "client-1", "sub": "user-42",
			"nonce": "n-1", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		}
	}

	cases := []struct {
		name  string
		build func(t *testing.T) string
		nonce string
	}{
		{"signed by the wrong key", func(t *testing.T) string {
			return other.sign(t, "RS256", base())
		}, "n-1"},

		{"alg none", func(t *testing.T) string {
			hb, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT", "kid": signer.kid})
			cb, _ := json.Marshal(base())
			return base64.RawURLEncoding.EncodeToString(hb) + "." +
				base64.RawURLEncoding.EncodeToString(cb) + "."
		}, "n-1"},

		{"HMAC using the public key as the secret", func(t *testing.T) string {
			hb, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT", "kid": signer.kid})
			cb, _ := json.Marshal(base())
			return base64.RawURLEncoding.EncodeToString(hb) + "." +
				base64.RawURLEncoding.EncodeToString(cb) + ".c2ln"
		}, "n-1"},

		{"wrong audience", func(t *testing.T) string {
			c := base()
			c["aud"] = "someone-elses-client"
			return signer.sign(t, "RS256", c)
		}, "n-1"},

		{"wrong issuer", func(t *testing.T) string {
			c := base()
			c["iss"] = "https://evil.test"
			return signer.sign(t, "RS256", c)
		}, "n-1"},

		{"expired", func(t *testing.T) string {
			c := base()
			c["exp"] = time.Now().Add(-time.Hour).Unix()
			return signer.sign(t, "RS256", c)
		}, "n-1"},

		{"no expiry at all", func(t *testing.T) string {
			c := base()
			delete(c, "exp")
			return signer.sign(t, "RS256", c)
		}, "n-1"},

		{"replayed with a different nonce", func(t *testing.T) string {
			return signer.sign(t, "RS256", base())
		}, "a-different-nonce"},

		{"no subject", func(t *testing.T) string {
			c := base()
			delete(c, "sub")
			return signer.sign(t, "RS256", c)
		}, "n-1"},

		{"payload tampered after signing", func(t *testing.T) string {
			raw := signer.sign(t, "RS256", base())
			parts := strings.Split(raw, ".")
			c := base()
			c["sub"] = "someone-else"
			cb, _ := json.Marshal(c)
			return parts[0] + "." + base64.RawURLEncoding.EncodeToString(cb) + "." + parts[2]
		}, "n-1"},

		{"not a JWT", func(t *testing.T) string { return "garbage" }, "n-1"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := oidcService(t, srv.URL, "https://id.example", "client-1")
			if _, err := s.identityFromIDToken(context.Background(), c.build(t), c.nonce); err == nil {
				t.Fatal("the token was accepted; it must be rejected")
			}
		})
	}
}

// A key too small to be safe must not be usable, however well-formed the JWKS is.
func TestJWKSRejectsUndersizedKeys(t *testing.T) {
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	n := base64.RawURLEncoding.EncodeToString(small.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(small.E)).Bytes())
	if _, err := rsaKeyFromJWK(n, e); err == nil {
		t.Fatal("a 1024-bit RSA key was accepted as a signing key")
	}
}

// The discovery document must not be able to point the flow at another issuer.
func TestDiscoveryRefusesAnIssuerMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 "https://evil.test",
			"authorization_endpoint": "https://evil.test/authorize",
			"token_endpoint":         "https://evil.test/token",
		})
	}))
	defer srv.Close()

	s := NewOAuthService(OAuthConfig{
		Enabled: true, Issuer: srv.URL, ClientID: "c", ClientSecret: "s",
	})
	err := s.discover(context.Background())
	if err == nil {
		t.Fatal("a discovery document declaring a different issuer was accepted")
	}
	if !strings.Contains(err.Error(), "issuer") {
		t.Fatalf("error does not name the problem: %v", err)
	}
}

// A provider that replaces a signing key while keeping its key ID looks, from
// the cache's point of view, entirely fresh. Without a refetch on a failed
// signature, every sign-in would fail until the cache expired — up to an hour
// of nobody being able to get in.
func TestKeyRotationUnderTheSameKeyIDRecovers(t *testing.T) {
	first := newTestSigner(t)
	second := newTestSigner(t)
	second.kid = first.kid // same ID, different key

	serving := first
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serving.jwksHandler().ServeHTTP(w, r)
	}))
	defer srv.Close()

	s := oidcService(t, srv.URL, "https://id.example", "client-1")
	claims := func() map[string]any {
		return map[string]any{
			"iss": "https://id.example", "aud": "client-1", "sub": "user-1",
			"nonce": "n", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		}
	}

	// Warm the cache with the first key.
	if _, err := s.identityFromIDToken(context.Background(), first.sign(t, "RS256", claims()), "n"); err != nil {
		t.Fatalf("the first key did not work: %v", err)
	}

	// The provider rotates, reusing the key ID.
	serving = second

	id, err := s.identityFromIDToken(context.Background(), second.sign(t, "RS256", claims()), "n")
	if err != nil {
		t.Fatalf("a token signed with the rotated key was rejected: %v", err)
	}
	if id.Subject != "user-1" {
		t.Fatalf("subject = %q", id.Subject)
	}
}

// The refetch must not turn a forged token into a way to hammer the provider,
// and must not make a genuinely bad signature acceptable.
func TestRotationRefetchDoesNotAcceptForgeries(t *testing.T) {
	signer := newTestSigner(t)
	forger := newTestSigner(t)
	forger.kid = signer.kid

	var fetches int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		signer.jwksHandler().ServeHTTP(w, r)
	}))
	defer srv.Close()

	s := oidcService(t, srv.URL, "https://id.example", "client-1")
	claims := map[string]any{
		"iss": "https://id.example", "aud": "client-1", "sub": "user-1",
		"nonce": "n", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	}

	// Warm the cache.
	if _, err := s.identityFromIDToken(context.Background(), signer.sign(t, "RS256", claims), "n"); err != nil {
		t.Fatal(err)
	}
	afterWarm := fetches

	// Twenty forged tokens must all be refused, and must not cause twenty fetches.
	for i := 0; i < 20; i++ {
		if _, err := s.identityFromIDToken(context.Background(), forger.sign(t, "RS256", claims), "n"); err == nil {
			t.Fatal("a token signed by the wrong key was accepted")
		}
	}
	if extra := fetches - afterWarm; extra > 2 {
		t.Fatalf("20 forged tokens caused %d key-set fetches; the refetch must be throttled", extra)
	}
}
