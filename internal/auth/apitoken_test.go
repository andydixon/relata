package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/ids"
)

// --- Fakes ------------------------------------------------------------------

type fakeTokens struct {
	mu     sync.Mutex
	byID   map[string]*domain.APIToken
	byHash map[string]*domain.APIToken
}

func newFakeTokens() *fakeTokens {
	return &fakeTokens{byID: map[string]*domain.APIToken{}, byHash: map[string]*domain.APIToken{}}
}

func (f *fakeTokens) Create(_ context.Context, t *domain.APIToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, clash := f.byHash[t.TokenHash]; clash {
		return domain.ErrDuplicate
	}
	cp := *t
	f.byID[t.ID] = &cp
	f.byHash[t.TokenHash] = &cp
	return nil
}

func (f *fakeTokens) GetByHash(_ context.Context, hash string) (*domain.APIToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.byHash[hash]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *t
	return &cp, nil
}

func (f *fakeTokens) ListForUser(_ context.Context, ws, user string) ([]*domain.APIToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*domain.APIToken
	for _, t := range f.byID {
		if t.WorkspaceID == ws && t.UserID == user {
			cp := *t
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakeTokens) Get(_ context.Context, ws, id string) (*domain.APIToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.byID[id]
	if !ok || t.WorkspaceID != ws {
		return nil, domain.ErrNotFound
	}
	cp := *t
	return &cp, nil
}

func (f *fakeTokens) Revoke(_ context.Context, ws, id, by string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.byID[id]
	if !ok || t.WorkspaceID != ws {
		return domain.ErrNotFound
	}
	t.RevokedAt, t.RevokedBy = &at, by
	return nil
}

func (f *fakeTokens) TouchLastUsed(_ context.Context, id string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.byID[id]; ok {
		t.LastUsedAt = &at
	}
	return nil
}

func (f *fakeTokens) DeleteForUser(_ context.Context, ws, user string) (int64, error) {
	return 0, nil
}

type fakeUsers struct {
	mu    sync.Mutex
	byID  map[string]*domain.User
	bySub map[string]*domain.User
}

func newFakeUsers(users ...*domain.User) *fakeUsers {
	f := &fakeUsers{byID: map[string]*domain.User{}, bySub: map[string]*domain.User{}}
	for _, u := range users {
		f.put(u)
	}
	return f
}

// put stores a copy under both indexes.
//
// Storing a copy rather than the caller's pointer is what makes this fake
// behave like a database: a caller that keeps mutating the struct it passed in
// cannot reach into the store, and two callers never share memory. Handing back
// the live pointer produced a data race that no real repository could have.
func (f *fakeUsers) put(u *domain.User) {
	cp := *u
	f.byID[cp.ID] = &cp
	if cp.OAuthSubject != "" {
		f.bySub[cp.OAuthProvider+"|"+cp.OAuthSubject] = &cp
	}
}

func (f *fakeUsers) Create(_ context.Context, u *domain.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u.OAuthSubject != "" {
		if _, clash := f.bySub[u.OAuthProvider+"|"+u.OAuthSubject]; clash {
			return domain.ErrDuplicate
		}
	}
	f.put(u)
	return nil
}
func (f *fakeUsers) GetByUsername(_ context.Context, n string) (*domain.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.byID {
		if u.Username == n {
			cp := *u
			return &cp, nil
		}
	}
	return nil, domain.ErrNotFound
}
func (f *fakeUsers) GetByID(_ context.Context, id string) (*domain.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *u
	return &cp, nil
}
func (f *fakeUsers) Update(_ context.Context, u *domain.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Both indexes must see the update. A real collection has one document; a
	// fake whose indexes diverge would let a test pass on a change the database
	// would never have made.
	f.put(u)
	return nil
}
func (f *fakeUsers) Count(context.Context) (int64, error) { return int64(len(f.byID)), nil }
func (f *fakeUsers) GetByOAuthSubject(_ context.Context, p, sub string) (*domain.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.bySub[p+"|"+sub]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *u
	return &cp, nil
}

// --- Helpers ----------------------------------------------------------------

func editorUser() *domain.User {
	return &domain.User{
		ID: ids.New(), Username: "editor", Role: domain.RoleEditor,
		WorkspaceID: "ws-1", PasswordHash: "x",
	}
}

func principalFor(u *domain.User) *domain.Principal {
	return &domain.Principal{
		UserID: u.ID, Username: u.Username, Role: u.Role, WorkspaceID: u.WorkspaceID,
	}
}

func newTestTokenService(u *domain.User) (*TokenService, *fakeTokens) {
	tk := newFakeTokens()
	return NewTokenService(tk, newFakeUsers(u), TokenConfig{
		DefaultTTL: 24 * time.Hour, MaxTTL: 72 * time.Hour,
		RateBurst: 1000, RateWindow: time.Minute,
	}), tk
}

// --- Tests ------------------------------------------------------------------

// The secret must never be recoverable from what is stored.
func TestOnlyTheHashIsStored(t *testing.T) {
	u := editorUser()
	svc, store := newTestTokenService(u)

	issued, err := svc.Create(context.Background(), principalFor(u), CreateRequest{
		Name: "script", Scopes: []domain.TokenScope{domain.ScopeQuery},
	})
	if err != nil {
		t.Fatal(err)
	}

	stored, err := store.GetByHash(context.Background(), ids.HashToken(issued.Secret))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored.TokenHash, issued.Secret) {
		t.Fatal("the stored hash contains the secret")
	}
	if stored.Prefix == issued.Secret {
		t.Fatal("the whole secret was stored as the display prefix")
	}
	if !strings.HasPrefix(issued.Secret, stored.Prefix) {
		t.Fatal("the display prefix does not match the token it identifies")
	}
	if len(stored.Prefix) >= len(issued.Secret)/2 {
		t.Fatalf("the display prefix is %d of %d characters; it must not be a usable fraction "+
			"of the secret", len(stored.Prefix), len(issued.Secret))
	}
}

// Two tokens must never collide, and each must be unguessable.
func TestTokensAreUniqueAndHighEntropy(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		s, err := newTokenSecret()
		if err != nil {
			t.Fatal(err)
		}
		if seen[s] {
			t.Fatal("two generated tokens were identical")
		}
		seen[s] = true
		if !strings.HasPrefix(s, tokenPrefix) {
			t.Fatalf("token %q lacks the version prefix", s)
		}
		if len(s)-len(tokenPrefix) < 40 {
			t.Fatalf("token body is only %d characters", len(s)-len(tokenPrefix))
		}
	}
}

// A token may never carry more authority than the person who created it.
func TestScopesAreNarrowedToTheCreatorsRole(t *testing.T) {
	viewer := &domain.User{ID: ids.New(), Username: "viewer", Role: domain.RoleViewer, WorkspaceID: "ws-1"}
	svc, _ := newTestTokenService(viewer)

	// Asking for everything as a viewer yields query only.
	issued, err := svc.Create(context.Background(), principalFor(viewer), CreateRequest{
		Name: "over-reaching",
		Scopes: []domain.TokenScope{
			domain.ScopeQuery, domain.ScopeIngest, domain.ScopeReview,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(issued.Token.Scopes) != 1 || issued.Token.Scopes[0] != domain.ScopeQuery {
		t.Fatalf("a viewer was granted %v; only query is available to that role", issued.Token.Scopes)
	}

	// Asking for nothing available at all is refused rather than issuing an
	// empty token that would fail confusingly on first use.
	if _, err := svc.Create(context.Background(), principalFor(viewer), CreateRequest{
		Name: "nothing", Scopes: []domain.TokenScope{domain.ScopeIngest},
	}); err == nil {
		t.Fatal("a token with no usable scopes was issued")
	}
}

// The role is re-read on every request, so demoting someone takes effect at once.
func TestDemotingTheOwnerImmediatelyNarrowsTheToken(t *testing.T) {
	u := editorUser()
	users := newFakeUsers(u)
	store := newFakeTokens()
	svc := NewTokenService(store, users, TokenConfig{DefaultTTL: time.Hour, MaxTTL: time.Hour, RateBurst: 100, RateWindow: time.Minute})

	issued, err := svc.Create(context.Background(), principalFor(u), CreateRequest{
		Name: "ingester", Scopes: []domain.TokenScope{domain.ScopeIngest, domain.ScopeQuery},
	})
	if err != nil {
		t.Fatal(err)
	}

	p, err := svc.Authenticate(context.Background(), issued.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Permits(domain.ScopeIngest) {
		t.Fatal("an editor's ingest token was refused ingest")
	}

	// Demote. The token is untouched.
	u.Role = domain.RoleViewer
	_ = users.Update(context.Background(), u)

	p2, err := svc.Authenticate(context.Background(), issued.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if p2.Permits(domain.ScopeIngest) {
		t.Fatal("the token still permits ingest after its owner was demoted to viewer")
	}
	if !p2.Permits(domain.ScopeQuery) {
		t.Fatal("the token lost query, which a viewer still has")
	}
}

// Every rejection must look the same to the caller.
func TestAuthenticateRefusesEverythingItShould(t *testing.T) {
	u := editorUser()
	users := newFakeUsers(u)
	store := newFakeTokens()
	svc := NewTokenService(store, users, TokenConfig{DefaultTTL: time.Hour, MaxTTL: time.Hour, RateBurst: 1000, RateWindow: time.Minute})

	issue := func(t *testing.T) string {
		t.Helper()
		iss, err := svc.Create(context.Background(), principalFor(u), CreateRequest{
			Name: "t", Scopes: []domain.TokenScope{domain.ScopeQuery},
		})
		if err != nil {
			t.Fatal(err)
		}
		return iss.Secret
	}

	t.Run("garbage", func(t *testing.T) {
		for _, bad := range []string{"", "   ", "not-a-token", "Bearer x", tokenPrefix, "rlt_v1_"} {
			if _, err := svc.Authenticate(context.Background(), bad); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("Authenticate(%q) = %v, want ErrInvalidToken", bad, err)
			}
		}
	})

	t.Run("revoked", func(t *testing.T) {
		secret := issue(t)
		p, _ := svc.Authenticate(context.Background(), secret)
		if p == nil {
			t.Fatal("the token did not work before revocation")
		}
		toks, _ := store.ListForUser(context.Background(), "ws-1", u.ID)
		var id string
		for _, tok := range toks {
			if tok.TokenHash == ids.HashToken(secret) {
				id = tok.ID
			}
		}
		if err := svc.Revoke(context.Background(), principalFor(u), id); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Authenticate(context.Background(), secret); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("a revoked token still authenticated: %v", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		secret := issue(t)
		toks, _ := store.ListForUser(context.Background(), "ws-1", u.ID)
		for _, tok := range toks {
			if tok.TokenHash == ids.HashToken(secret) {
				store.mu.Lock()
				store.byID[tok.ID].ExpiresAt = time.Now().Add(-time.Hour)
				store.byHash[tok.TokenHash].ExpiresAt = time.Now().Add(-time.Hour)
				store.mu.Unlock()
			}
		}
		if _, err := svc.Authenticate(context.Background(), secret); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("an expired token still authenticated: %v", err)
		}
	})

	t.Run("disabled owner", func(t *testing.T) {
		secret := issue(t)
		u.Disabled = true
		_ = users.Update(context.Background(), u)
		defer func() { u.Disabled = false; _ = users.Update(context.Background(), u) }()

		if _, err := svc.Authenticate(context.Background(), secret); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("a token belonging to a disabled account still authenticated: %v", err)
		}
	})
}

// A leaked token must not be able to mint replacements or revoke rivals.
func TestATokenCannotManageTokens(t *testing.T) {
	u := editorUser()
	svc, _ := newTestTokenService(u)

	tokenPrincipal := &domain.Principal{
		UserID: u.ID, Role: u.Role, WorkspaceID: u.WorkspaceID,
		TokenID: "tok-1", TokenScopes: []domain.TokenScope{domain.ScopeIngest, domain.ScopeQuery},
	}

	if _, err := svc.Create(context.Background(), tokenPrincipal, CreateRequest{
		Name: "child", Scopes: []domain.TokenScope{domain.ScopeQuery},
	}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a token created another token: %v", err)
	}
	if err := svc.Revoke(context.Background(), tokenPrincipal, "any"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a token revoked a token: %v", err)
	}
}

// Bearer credentials come from the header and nowhere else.
func TestBearerIsReadOnlyFromTheAuthorizationHeader(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{"Bearer abc123", "abc123"},
		{"bearer abc123", "abc123"},
		{"BEARER abc123", "abc123"},
		{"Bearer   abc123  ", "abc123"},
		{"Basic abc123", ""},
		{"abc123", ""},
		{"Bearer", ""},
		{"", ""},
	}
	for _, c := range cases {
		r := httptestRequest(c.header)
		if got := BearerFromRequest(r); got != c.want {
			t.Errorf("BearerFromRequest(%q) = %q, want %q", c.header, got, c.want)
		}
	}
}

// The rate limiter must actually stop a runaway token.
func TestTokenRateLimitBites(t *testing.T) {
	u := editorUser()
	users := newFakeUsers(u)
	store := newFakeTokens()
	svc := NewTokenService(store, users, TokenConfig{
		DefaultTTL: time.Hour, MaxTTL: time.Hour,
		RateBurst: 5, RateWindow: time.Minute,
	})

	issued, err := svc.Create(context.Background(), principalFor(u), CreateRequest{
		Name: "busy", Scopes: []domain.TokenScope{domain.ScopeQuery},
	})
	if err != nil {
		t.Fatal(err)
	}

	var limited bool
	for i := 0; i < 20; i++ {
		if _, err := svc.Authenticate(context.Background(), issued.Secret); errors.Is(err, ErrTokenRateLimit) {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("a token made 20 requests against a burst of 5 without being limited")
	}
}

func httptestRequest(authHeader string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/search", nil)
	if authHeader != "" {
		r.Header.Set("Authorization", authHeader)
	}
	return r
}

// --- Client identification behind a proxy -----------------------------------

// The client key is what login rate limiting buckets on. A client that can
// choose its own key can have as many buckets as it likes, which is the same as
// having no rate limit at all.
func TestClientKeyCannotBeChosenByTheClient(t *testing.T) {
	const realClient = "203.0.113.9"

	newReq := func(headers map[string]string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/login", nil)
		r.RemoteAddr = "10.0.0.5:34567" // the proxy
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		return r
	}

	t.Run("X-Real-IP wins and cannot be overridden by a forged XFF", func(t *testing.T) {
		// What nginx actually sends: X-Real-IP is $remote_addr, and XFF has the
		// client's forged value appended with the real one.
		r := newReq(map[string]string{
			"X-Real-IP":       realClient,
			"X-Forwarded-For": "1.2.3.4, " + realClient,
		})
		if got := ClientKey(r, true); got != realClient {
			t.Fatalf("ClientKey = %q, want the real client %q — a forged X-Forwarded-For "+
				"must not choose the rate-limit bucket", got, realClient)
		}
	})

	t.Run("without X-Real-IP the rightmost XFF entry is used", func(t *testing.T) {
		r := newReq(map[string]string{"X-Forwarded-For": "1.2.3.4, 5.6.7.8, " + realClient})
		if got := ClientKey(r, true); got != realClient {
			t.Fatalf("ClientKey = %q, want %q — the trustworthy entry is the one the "+
				"nearest proxy appended, not the one the client sent first", got, realClient)
		}
	})

	t.Run("a hundred different forged values all land in one bucket", func(t *testing.T) {
		seen := map[string]bool{}
		for i := 0; i < 100; i++ {
			r := newReq(map[string]string{
				"X-Real-IP":       realClient,
				"X-Forwarded-For": fmt.Sprintf("10.%d.%d.%d, %s", i, i, i, realClient),
			})
			seen[ClientKey(r, true)] = true
		}
		if len(seen) != 1 {
			t.Fatalf("100 forged headers produced %d distinct rate-limit buckets; "+
				"login throttling would be trivially bypassed", len(seen))
		}
	})

	t.Run("proxy headers are ignored entirely when not trusted", func(t *testing.T) {
		r := newReq(map[string]string{"X-Real-IP": "1.2.3.4", "X-Forwarded-For": "1.2.3.4"})
		if got := ClientKey(r, false); got != "10.0.0.5" {
			t.Fatalf("ClientKey = %q, want the socket address; proxy headers must be "+
				"ignored unless explicitly trusted", got)
		}
	})
}
