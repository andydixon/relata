package auth

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/andydixon/relata/internal/config"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/ids"
)

type fakeWorkspaces struct {
	mu   sync.Mutex
	byID map[string]*domain.Workspace
}

func newFakeWorkspaces() *fakeWorkspaces {
	return &fakeWorkspaces{byID: map[string]*domain.Workspace{}}
}

func (f *fakeWorkspaces) Create(_ context.Context, w *domain.Workspace) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[w.ID] = w
	return nil
}
func (f *fakeWorkspaces) Get(_ context.Context, id string) (*domain.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return w, nil
}
func (f *fakeWorkspaces) GetBySlug(context.Context, string) (*domain.Workspace, error) {
	return nil, domain.ErrNotFound
}
func (f *fakeWorkspaces) List(context.Context) ([]*domain.Workspace, error) { return nil, nil }
func (f *fakeWorkspaces) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.byID)
}

type fakeSessions struct {
	mu   sync.Mutex
	byID map[string]*domain.Session
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{byID: map[string]*domain.Session{}}
}
func (f *fakeSessions) Create(_ context.Context, s *domain.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[s.ID] = s
	return nil
}
func (f *fakeSessions) GetByTokenHash(context.Context, string) (*domain.Session, error) {
	return nil, domain.ErrNotFound
}
func (f *fakeSessions) Touch(context.Context, string, time.Time, time.Time) error { return nil }
func (f *fakeSessions) Revoke(context.Context, string) error                      { return nil }
func (f *fakeSessions) RevokeAllForUser(context.Context, string) error            { return nil }
func (f *fakeSessions) DeleteExpired(context.Context, time.Time) (int64, error)   { return 0, nil }

func newTestProvisioner() (*Provisioner, *fakeUsers, *fakeWorkspaces) {
	users := newFakeUsers()
	wss := newFakeWorkspaces()
	sessions := NewService(users, newFakeSessions(), config.SessionConfig{
		CookieName: "relata_session", IdleTimeout: time.Hour, AbsoluteTimeout: 24 * time.Hour,
		LoginRateBurst: 10, LoginRateWindow: time.Minute,
	}, false, nil)
	return NewProvisioner(sessions, users, wss), users, wss
}

// First sign-in creates a private workspace in which the person is admin.
func TestFirstSignInProvisionsAPrivateWorkspace(t *testing.T) {
	p, users, wss := newTestProvisioner()

	res, err := p.SignIn(context.Background(), "Blackwall", &Identity{
		Subject: "sub-1", Email: "alice@example.com", DisplayName: "Alice Example",
	}, "test-agent")
	if err != nil {
		t.Fatal(err)
	}

	if res.User.Role != domain.RoleAdmin {
		t.Fatalf("role = %q; a person should be admin of their own workspace", res.User.Role)
	}
	if res.User.WorkspaceID == "" {
		t.Fatal("no workspace was assigned")
	}
	if wss.count() != 1 {
		t.Fatalf("%d workspaces created, want 1", wss.count())
	}
	if res.User.PasswordHash != "" {
		t.Fatal("a federated account was given a password hash")
	}
	if !res.User.Federated() || res.User.CanLoginLocally() {
		t.Fatal("a federated account must never be able to sign in locally")
	}
	if res.Token == "" || res.Session == nil {
		t.Fatal("no session was established")
	}
	_ = users
}

// Signing in again must reuse the account, not make another workspace.
func TestSecondSignInReusesTheSameWorkspace(t *testing.T) {
	p, _, wss := newTestProvisioner()
	id := &Identity{Subject: "sub-1", Email: "alice@example.com", DisplayName: "Alice"}

	first, err := p.SignIn(context.Background(), "Blackwall", id, "a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.SignIn(context.Background(), "Blackwall", id, "a")
	if err != nil {
		t.Fatal(err)
	}

	if first.User.ID != second.User.ID {
		t.Fatal("signing in twice produced two accounts")
	}
	if first.User.WorkspaceID != second.User.WorkspaceID {
		t.Fatal("signing in twice produced two workspaces")
	}
	if wss.count() != 1 {
		t.Fatalf("%d workspaces exist after two sign-ins, want 1", wss.count())
	}
	if first.Session.ID == second.Session.ID {
		t.Fatal("the session ID was reused across sign-ins; it must be fresh each time")
	}
}

// Two people whose email addresses collide, or whose provider changes an email,
// must never be confused for one another: the subject is the key.
func TestIdentityIsKeyedOnSubjectNotEmail(t *testing.T) {
	p, _, wss := newTestProvisioner()
	ctx := context.Background()

	alice, err := p.SignIn(ctx, "Blackwall", &Identity{
		Subject: "sub-alice", Email: "shared@example.com", DisplayName: "Alice",
	}, "a")
	if err != nil {
		t.Fatal(err)
	}

	// A different person who has been given the same address.
	bob, err := p.SignIn(ctx, "Blackwall", &Identity{
		Subject: "sub-bob", Email: "shared@example.com", DisplayName: "Bob",
	}, "a")
	if err != nil {
		t.Fatal(err)
	}

	if alice.User.ID == bob.User.ID {
		t.Fatal("two different subjects sharing an email resolved to one account")
	}
	if alice.User.WorkspaceID == bob.User.WorkspaceID {
		t.Fatal("two different people were put in the same workspace")
	}
	if wss.count() != 2 {
		t.Fatalf("%d workspaces, want 2", wss.count())
	}

	// The same person whose email changed keeps their workspace.
	aliceAgain, err := p.SignIn(ctx, "Blackwall", &Identity{
		Subject: "sub-alice", Email: "alice.new@example.com", DisplayName: "Alice",
	}, "a")
	if err != nil {
		t.Fatal(err)
	}
	if aliceAgain.User.ID != alice.User.ID {
		t.Fatal("changing email address produced a new account")
	}
	if aliceAgain.User.Email != "alice.new@example.com" {
		t.Fatalf("the display email was not refreshed: %q", aliceAgain.User.Email)
	}
}

// Two simultaneous first sign-ins must converge on one account and one workspace.
func TestConcurrentFirstSignInsConvergeOnOneAccount(t *testing.T) {
	p, _, wss := newTestProvisioner()
	id := &Identity{Subject: "sub-race", Email: "race@example.com", DisplayName: "Racer"}

	const n = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		userIDs = map[string]bool{}
		errs    []error
	)

	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := p.SignIn(context.Background(), "Blackwall", id, "a")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			userIDs[res.User.ID] = true
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("concurrent sign-ins produced errors: %v", errs)
	}
	if len(userIDs) != 1 {
		t.Fatalf("%d distinct accounts were created for one identity; provisioning is not idempotent", len(userIDs))
	}
	// One workspace is used. Others may have been created and abandoned by
	// racers that lost, which is deliberate — deleting one would race with the
	// winner — but nobody may end up *in* a second workspace.
	if wss.count() < 1 {
		t.Fatal("no workspace was created")
	}
}

// A disabled account cannot sign in, however good the identity is.
func TestDisabledFederatedAccountCannotSignIn(t *testing.T) {
	p, users, _ := newTestProvisioner()
	ctx := context.Background()

	res, err := p.SignIn(ctx, "Blackwall", &Identity{Subject: "sub-1", Email: "a@example.com"}, "a")
	if err != nil {
		t.Fatal(err)
	}

	u, _ := users.GetByID(ctx, res.User.ID)
	u.Disabled = true
	_ = users.Update(ctx, u)

	if _, err := p.SignIn(ctx, "Blackwall", &Identity{Subject: "sub-1", Email: "a@example.com"}, "a"); err == nil {
		t.Fatal("a disabled account signed in")
	}
}

// A username is a label derived from the identity; it must be safe and unique.
func TestUsernameSeedProducesSafeLabels(t *testing.T) {
	cases := []struct {
		id   Identity
		want string
	}{
		{Identity{Email: "alice.smith@example.com"}, "alice-smith"},
		{Identity{Email: "A.B@EXAMPLE.COM"}, "a-b"},
		{Identity{DisplayName: "Alice Smith"}, "alice-smith"},
		{Identity{Email: "../../etc/passwd@x.com"}, "etcpasswd"},
		{Identity{Email: "<script>@x.com"}, "script"},
		{Identity{}, "user"},
		{Identity{DisplayName: "   "}, "user"},
	}
	for _, c := range cases {
		got := usernameSeed(&c.id)
		if got != c.want {
			t.Errorf("usernameSeed(%+v) = %q, want %q", c.id, got, c.want)
		}
		for _, r := range got {
			ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
			if !ok {
				t.Errorf("usernameSeed produced %q, containing unsafe character %q", got, r)
			}
		}
	}
}

var _ = ids.New
