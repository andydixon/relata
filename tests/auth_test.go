//go:build integration

package tests

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andydixon/relata/internal/auth"
	"github.com/andydixon/relata/internal/domain"
)

func (h *harness) provisioner() *auth.Provisioner {
	return auth.NewProvisioner(h.auth, h.store.Users, h.store.Workspaces)
}

func (h *harness) tokenService() *auth.TokenService {
	return auth.NewTokenService(h.store.APITokens, h.store.Users, auth.TokenConfig{
		DefaultTTL: 24 * time.Hour, MaxTTL: 72 * time.Hour,
		RateBurst: 10_000, RateWindow: time.Minute,
	})
}

// The unique index on (oauth_provider, oauth_subject) is what makes first
// sign-in idempotent. A fake can be made to behave; only the real index proves
// it. Two workspaces for one person would split their corpus permanently.
func TestFederatedProvisioningIsIdempotentUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	p := h.provisioner()

	id := &auth.Identity{Subject: "blackwall-sub-1", Email: "racer@example.com", DisplayName: "Racer"}

	const n = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		userIDs = map[string]bool{}
		wsIDs   = map[string]bool{}
		errs    []error
	)

	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := p.SignIn(ctx, "Blackwall", id, "test")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			userIDs[res.User.ID] = true
			wsIDs[res.User.WorkspaceID] = true
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d of %d concurrent first sign-ins failed: %v", len(errs), n, errs)
	}
	if len(userIDs) != 1 {
		t.Fatalf("%d accounts were created for one identity; the unique subject index did not hold", len(userIDs))
	}
	if len(wsIDs) != 1 {
		t.Fatalf("one person ended up in %d workspaces; their corpus would be split", len(wsIDs))
	}
}

// Identity is the subject. Two people sharing an email must stay apart, and one
// person whose email changes must keep their workspace.
func TestFederatedIdentityIsKeyedOnSubject(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	p := h.provisioner()

	alice, err := p.SignIn(ctx, "Blackwall",
		&auth.Identity{Subject: "sub-a", Email: "shared@example.com", DisplayName: "Alice"}, "t")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := p.SignIn(ctx, "Blackwall",
		&auth.Identity{Subject: "sub-b", Email: "shared@example.com", DisplayName: "Bob"}, "t")
	if err != nil {
		t.Fatal(err)
	}

	if alice.User.WorkspaceID == bob.User.WorkspaceID {
		t.Fatal("two subjects sharing an email address were put in one workspace")
	}
	if alice.User.Username == bob.User.Username {
		t.Fatalf("both accounts got the username %q; the unique index should have separated them",
			alice.User.Username)
	}

	moved, err := p.SignIn(ctx, "Blackwall",
		&auth.Identity{Subject: "sub-a", Email: "alice.new@example.com", DisplayName: "Alice"}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if moved.User.ID != alice.User.ID || moved.User.WorkspaceID != alice.User.WorkspaceID {
		t.Fatal("changing email address moved the person to a different account or workspace")
	}
}

// A token must work against real storage, and stop working the moment it is
// revoked — with the secret never recoverable from what was stored.
func TestAPITokenLifecycleAgainstRealStorage(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	svc := h.tokenService()

	// The harness principal must correspond to a real user for authentication
	// to resolve it, so provision one properly.
	p := h.provisioner()
	res, err := p.SignIn(ctx, "Blackwall",
		&auth.Identity{Subject: "sub-tokens", Email: "tokens@example.com"}, "t")
	if err != nil {
		t.Fatal(err)
	}
	owner := &domain.Principal{
		UserID: res.User.ID, Username: res.User.Username,
		Role: res.User.Role, WorkspaceID: res.User.WorkspaceID,
	}

	issued, err := svc.Create(ctx, owner, auth.CreateRequest{
		Name: "integration", Scopes: []domain.TokenScope{domain.ScopeQuery, domain.ScopeIngest},
	})
	if err != nil {
		t.Fatal(err)
	}

	// It authenticates.
	got, err := svc.Authenticate(ctx, issued.Secret)
	if err != nil {
		t.Fatalf("a freshly issued token did not authenticate: %v", err)
	}
	if got.WorkspaceID != owner.WorkspaceID || got.UserID != owner.UserID {
		t.Fatal("the token resolved to the wrong principal")
	}
	if !got.ViaToken() || !got.Permits(domain.ScopeIngest) {
		t.Fatal("the token principal does not carry its scopes")
	}

	// The stored record must not contain the secret anywhere.
	stored, err := h.store.APITokens.Get(ctx, owner.WorkspaceID, issued.Token.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored.TokenHash, issued.Secret) || stored.Prefix == issued.Secret {
		t.Fatal("the secret is recoverable from the stored record")
	}

	// Revocation takes effect on the next request, with no cached grant.
	if err := svc.Revoke(ctx, owner, issued.Token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, issued.Secret); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("a revoked token still authenticated: %v", err)
	}

	// The record survives revocation, so the history stays visible.
	after, err := h.store.APITokens.Get(ctx, owner.WorkspaceID, issued.Token.ID)
	if err != nil {
		t.Fatalf("the token record was deleted rather than marked revoked: %v", err)
	}
	if after.RevokedAt == nil {
		t.Fatal("the token was not marked revoked")
	}
}

// A token belongs to one workspace. It must not reach another, and one person
// must not be able to revoke another's token by guessing its ID.
func TestAPITokensDoNotCrossWorkspaces(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	svc := h.tokenService()
	prov := h.provisioner()

	one, err := prov.SignIn(ctx, "Blackwall", &auth.Identity{Subject: "sub-1", Email: "one@example.com"}, "t")
	if err != nil {
		t.Fatal(err)
	}
	two, err := prov.SignIn(ctx, "Blackwall", &auth.Identity{Subject: "sub-2", Email: "two@example.com"}, "t")
	if err != nil {
		t.Fatal(err)
	}

	pOne := &domain.Principal{UserID: one.User.ID, Role: one.User.Role, WorkspaceID: one.User.WorkspaceID}
	pTwo := &domain.Principal{UserID: two.User.ID, Role: two.User.Role, WorkspaceID: two.User.WorkspaceID}

	issued, err := svc.Create(ctx, pOne, auth.CreateRequest{
		Name: "one's token", Scopes: []domain.TokenScope{domain.ScopeQuery},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Person two cannot see it.
	list, err := svc.List(ctx, pTwo)
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range list {
		if tok.ID == issued.Token.ID {
			t.Fatal("one person's token appeared in another's list")
		}
	}

	// Nor revoke it, even knowing the ID exactly.
	if err := svc.Revoke(ctx, pTwo, issued.Token.ID); err == nil {
		t.Fatal("one person revoked another's token")
	}

	// And it still works for its owner.
	if _, err := svc.Authenticate(ctx, issued.Secret); err != nil {
		t.Fatalf("the owner's token stopped working: %v", err)
	}
}

// A federated account has no password, so no password may ever authenticate it.
func TestFederatedAccountCannotSignInWithAPassword(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	res, err := h.provisioner().SignIn(ctx, "Blackwall",
		&auth.Identity{Subject: "sub-nopass", Email: "nopass@example.com"}, "t")
	if err != nil {
		t.Fatal(err)
	}

	for _, attempt := range []string{"", " ", "password", "nopass@example.com", "$argon2id$v=19$m=65536"} {
		if _, err := h.auth.Login(ctx, res.User.Username, attempt, "test-key", "agent"); err == nil {
			t.Fatalf("a federated account authenticated with the password %q", attempt)
		}
	}
}
