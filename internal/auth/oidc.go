package auth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	// crypto.SHA256/384/512 resolve through the registry, and Hash.New panics
	// on an algorithm whose package was never linked in. These imports are what
	// make the RS384 and RS512 branches below work rather than crash.
	_ "crypto/sha256"
	_ "crypto/sha512"
)

// discover fills in any endpoint the operator did not configure, from the
// provider's OpenID discovery document.
//
// It runs once. An explicitly configured endpoint always wins over a discovered
// one, so an operator can override a provider that advertises something wrong.
func (s *OAuthService) discover(ctx context.Context) error {
	s.discoOnce.Do(func() {
		if s.cfg.Issuer == "" {
			return
		}
		if s.cfg.AuthorizeURL != "" && s.cfg.TokenURL != "" && s.cfg.JWKSURL != "" {
			return // nothing left to learn
		}

		u := strings.TrimRight(s.cfg.Issuer, "/") + "/.well-known/openid-configuration"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			s.discoErr = fmt.Errorf("%w: %v", ErrOAuthProvider, err)
			return
		}
		req.Header.Set("Accept", "application/json")

		resp, err := s.client.Do(req)
		if err != nil {
			s.discoErr = fmt.Errorf("%w: fetching %s: %v", ErrOAuthProvider, u, err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			s.discoErr = fmt.Errorf("%w: %s returned %d", ErrOAuthProvider, u, resp.StatusCode)
			return
		}

		var doc struct {
			Issuer      string `json:"issuer"`
			AuthzURL    string `json:"authorization_endpoint"`
			TokenURL    string `json:"token_endpoint"`
			UserInfoURL string `json:"userinfo_endpoint"`
			JWKSURL     string `json:"jwks_uri"`
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderBody))
		if err != nil {
			s.discoErr = fmt.Errorf("%w: %v", ErrOAuthProvider, err)
			return
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			s.discoErr = fmt.Errorf("%w: discovery document was not JSON: %v", ErrOAuthProvider, err)
			return
		}

		// The document's own issuer must match the one configured, or a
		// compromised discovery endpoint could redirect the whole flow.
		if doc.Issuer != "" && strings.TrimRight(doc.Issuer, "/") != strings.TrimRight(s.cfg.Issuer, "/") {
			s.discoErr = fmt.Errorf("%w: discovery document declares issuer %q, expected %q",
				ErrOAuthProvider, doc.Issuer, s.cfg.Issuer)
			return
		}

		if s.cfg.AuthorizeURL == "" {
			s.cfg.AuthorizeURL = doc.AuthzURL
		}
		if s.cfg.TokenURL == "" {
			s.cfg.TokenURL = doc.TokenURL
		}
		if s.cfg.UserInfoURL == "" {
			s.cfg.UserInfoURL = doc.UserInfoURL
		}
		if s.cfg.JWKSURL == "" {
			s.cfg.JWKSURL = doc.JWKSURL
		}
	})

	if s.discoErr != nil {
		return s.discoErr
	}
	if s.cfg.AuthorizeURL == "" || s.cfg.TokenURL == "" {
		return fmt.Errorf("%w: no authorization or token endpoint is configured or discoverable",
			ErrOAuthProvider)
	}
	return nil
}

// identityFromIDToken validates an OIDC ID token and reads the identity from it.
//
// Validation is signature first, then claims. A token whose signature does not
// verify is never parsed for meaning.
func (s *OAuthService) identityFromIDToken(ctx context.Context, raw, nonce string) (*Identity, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: ID token is not a three-part JWT", ErrOAuthIdentity)
	}

	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: ID token header is not base64url", ErrOAuthIdentity)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return nil, fmt.Errorf("%w: ID token header is not JSON", ErrOAuthIdentity)
	}

	// Only RSA signing is accepted. "none" is the classic JWT forgery, and an
	// HMAC algorithm here would let the public key be used as the shared
	// secret — both are refused by allowing an explicit list rather than
	// trusting the token to name its own algorithm.
	var hash crypto.Hash
	switch header.Alg {
	case "RS256":
		hash = crypto.SHA256
	case "RS384":
		hash = crypto.SHA384
	case "RS512":
		hash = crypto.SHA512
	default:
		return nil, fmt.Errorf("%w: ID token algorithm %q is not accepted", ErrOAuthIdentity, header.Alg)
	}

	// Belt and braces: if the algorithm's package were ever dropped, this
	// returns an error instead of panicking inside a request handler.
	if !hash.Available() {
		return nil, fmt.Errorf("%w: hash for %s is not available in this build",
			ErrOAuthIdentity, header.Alg)
	}

	if s.cfg.JWKSURL == "" {
		return nil, fmt.Errorf("%w: no JWKS endpoint to verify the ID token with", ErrOAuthIdentity)
	}
	key, fromCache, err := s.jwks.key(ctx, s.cfg.JWKSURL, header.Kid)
	if err != nil {
		return nil, err
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: ID token signature is not base64url", ErrOAuthIdentity)
	}
	signed := parts[0] + "." + parts[1]

	h := hash.New()
	h.Write([]byte(signed))
	digest := h.Sum(nil)

	if err := rsa.VerifyPKCS1v15(key, hash, digest, sig); err != nil {
		// A cached key that no longer verifies is what a rotation looks like
		// when the provider keeps the key ID. Refetch once and try again;
		// anything else is a genuinely bad signature.
		if !fromCache {
			return nil, fmt.Errorf("%w: ID token signature does not verify", ErrOAuthIdentity)
		}
		rotated, rerr := s.jwks.rotatedKey(ctx, s.cfg.JWKSURL, header.Kid)
		if rerr != nil {
			return nil, fmt.Errorf("%w: ID token signature does not verify", ErrOAuthIdentity)
		}
		if err := rsa.VerifyPKCS1v15(rotated, hash, digest, sig); err != nil {
			return nil, fmt.Errorf("%w: ID token signature does not verify", ErrOAuthIdentity)
		}
	}

	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: ID token payload is not base64url", ErrOAuthIdentity)
	}
	var claims map[string]any
	if err := json.Unmarshal(payloadRaw, &claims); err != nil {
		return nil, fmt.Errorf("%w: ID token payload is not JSON", ErrOAuthIdentity)
	}

	if err := s.checkIDTokenClaims(claims, nonce); err != nil {
		return nil, err
	}
	return identityFromClaims(claims)
}

// checkIDTokenClaims enforces issuer, audience, expiry and nonce.
func (s *OAuthService) checkIDTokenClaims(claims map[string]any, nonce string) error {
	if iss := claimString(claims, "iss"); s.cfg.Issuer != "" &&
		strings.TrimRight(iss, "/") != strings.TrimRight(s.cfg.Issuer, "/") {
		return fmt.Errorf("%w: ID token issuer %q does not match %q",
			ErrOAuthIdentity, iss, s.cfg.Issuer)
	}

	if !audienceContains(claims["aud"], s.cfg.ClientID) {
		return fmt.Errorf("%w: ID token audience does not include this client", ErrOAuthIdentity)
	}

	now := time.Now()
	// A little leeway for clock skew, in both directions.
	const skew = 2 * time.Minute
	if exp, ok := claimTime(claims, "exp"); ok && now.After(exp.Add(skew)) {
		return fmt.Errorf("%w: ID token has expired", ErrOAuthIdentity)
	} else if !ok {
		return fmt.Errorf("%w: ID token has no expiry", ErrOAuthIdentity)
	}
	if iat, ok := claimTime(claims, "iat"); ok && now.Add(skew).Before(iat) {
		return fmt.Errorf("%w: ID token is issued in the future", ErrOAuthIdentity)
	}

	// The nonce binds the token to this browser's sign-in attempt, which is
	// what stops a token obtained elsewhere being replayed here.
	if nonce != "" {
		got := claimString(claims, "nonce")
		if subtle.ConstantTimeCompare([]byte(got), []byte(nonce)) != 1 {
			return fmt.Errorf("%w: ID token nonce does not match this sign-in attempt", ErrOAuthIdentity)
		}
	}
	return nil
}

func audienceContains(aud any, clientID string) bool {
	switch v := aud.(type) {
	case string:
		return subtle.ConstantTimeCompare([]byte(v), []byte(clientID)) == 1
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok &&
				subtle.ConstantTimeCompare([]byte(s), []byte(clientID)) == 1 {
				return true
			}
		}
	}
	return false
}

func claimTime(m map[string]any, key string) (time.Time, bool) {
	switch v := m[key].(type) {
	case float64:
		return time.Unix(int64(v), 0), true
	case int64:
		return time.Unix(v, 0), true
	}
	return time.Time{}, false
}

// ---------------------------------------------------------------------------
// JWKS
// ---------------------------------------------------------------------------

// jwksCache holds the provider's signing keys.
//
// Keys are cached because fetching them per sign-in would make the identity
// provider a hard dependency of every login, and refreshed on an unknown key ID
// because that is exactly what a key rotation looks like.
type jwksCache struct {
	client *http.Client

	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
	// lastMiss throttles refetching, so an unknown key ID cannot be used to
	// make Relata hammer the provider.
	lastMiss time.Time
	// lastRotationFetch throttles the refetch that follows a failed signature.
	lastRotationFetch time.Time
}

const (
	jwksTTL       = time.Hour
	jwksMissDelay = time.Minute
)

func newJWKSCache(c *http.Client) *jwksCache {
	return &jwksCache{client: c, keys: map[string]*rsa.PublicKey{}}
}

func (c *jwksCache) key(ctx context.Context, url, kid string) (*rsa.PublicKey, bool, error) {
	c.mu.RLock()
	k, ok := c.keys[kid]
	fresh := time.Since(c.fetchedAt) < jwksTTL
	missRecently := time.Since(c.lastMiss) < jwksMissDelay
	c.mu.RUnlock()

	if ok && fresh {
		return k, true, nil
	}
	if !ok && fresh && missRecently {
		return nil, false, fmt.Errorf("%w: no signing key %q in the provider's key set", ErrOAuthIdentity, kid)
	}

	if err := c.refresh(ctx, url); err != nil {
		// A stale key still verifies a token signed before the fetch failed.
		if ok {
			return k, true, nil
		}
		return nil, false, err
	}

	c.mu.RLock()
	k, ok = c.keys[kid]
	c.mu.RUnlock()
	if !ok {
		c.mu.Lock()
		c.lastMiss = time.Now()
		c.mu.Unlock()
		return nil, false, fmt.Errorf("%w: no signing key %q in the provider's key set", ErrOAuthIdentity, kid)
	}
	return k, false, nil
}

// rotatedKey refetches the key set and returns the key for kid.
//
// It exists for one case: a provider that replaces a key while keeping its key
// ID. Nothing about the cached entry looks stale, so without this every sign-in
// would fail until the cache expired — up to an hour of nobody being able to
// get in. It is called only after a signature has already failed against a
// cached key, and it is throttled, so it cannot be used to make Relata hammer
// the provider.
func (c *jwksCache) rotatedKey(ctx context.Context, url, kid string) (*rsa.PublicKey, error) {
	c.mu.Lock()
	if time.Since(c.lastRotationFetch) < jwksMissDelay {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: signing key %q did not verify and the key set was "+
			"refetched too recently to try again", ErrOAuthIdentity, kid)
	}
	c.lastRotationFetch = time.Now()
	c.mu.Unlock()

	if err := c.refresh(ctx, url); err != nil {
		return nil, err
	}
	c.mu.RLock()
	k, ok := c.keys[kid]
	c.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: no signing key %q in the provider's key set", ErrOAuthIdentity, kid)
	}
	return k, nil
}

func (c *jwksCache) refresh(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrOAuthProvider, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: fetching signing keys: %v", ErrOAuthProvider, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: key set endpoint returned %d", ErrOAuthProvider, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderBody))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrOAuthProvider, err)
	}

	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return fmt.Errorf("%w: key set was not JSON: %v", ErrOAuthProvider, err)
	}

	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		pub, err := rsaKeyFromJWK(k.N, k.E)
		if err != nil {
			continue // one unusable key must not discard the rest
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return fmt.Errorf("%w: key set contained no usable RSA signing keys", ErrOAuthProvider)
	}

	c.mu.Lock()
	c.keys = keys
	c.fetchedAt = time.Now()
	c.mu.Unlock()
	return nil
}

func rsaKeyFromJWK(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, err
	}
	if len(eBytes) == 0 || len(eBytes) > 8 {
		return nil, fmt.Errorf("implausible RSA exponent")
	}
	// Left-pad the exponent to eight bytes so it can be read as a uint64.
	padded := make([]byte, 8)
	copy(padded[8-len(eBytes):], eBytes)
	e := binary.BigEndian.Uint64(padded)
	if e == 0 || e > 1<<31 {
		return nil, fmt.Errorf("implausible RSA exponent")
	}

	key := &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: int(e)}
	if key.N.BitLen() < 2048 {
		return nil, fmt.Errorf("RSA key smaller than 2048 bits")
	}
	return key, nil
}
