package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHashProducesDistinctHashesForTheSamePassword(t *testing.T) {
	const pw = "correct horse battery staple"
	a, err := HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("identical hashes for the same password: the salt is not random")
	}
	for _, h := range []string{a, b} {
		if strings.Contains(h, pw) {
			t.Fatal("the hash contains the plain-text password")
		}
		if !strings.HasPrefix(h, "$argon2id$") {
			t.Fatalf("hash is not Argon2id: %q", h)
		}
	}
}

func TestVerifyAcceptsCorrectAndRejectsWrongPasswords(t *testing.T) {
	const pw = "correct horse battery staple"
	h, err := HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}

	ok, err := VerifyPassword(pw, h)
	if err != nil || !ok {
		t.Fatalf("correct password rejected: ok=%v err=%v", ok, err)
	}
	for _, wrong := range []string{
		"correct horse battery stapl",
		"correct horse battery staple ",
		"Correct horse battery staple",
		"",
		strings.Repeat("x", 200),
	} {
		ok, err := VerifyPassword(wrong, h)
		if err != nil && !errors.Is(err, ErrPasswordTooLong) {
			t.Fatalf("unexpected error for %q: %v", wrong, err)
		}
		if ok {
			t.Fatalf("wrong password %q was accepted", wrong)
		}
	}
}

func TestHashEncodesItsOwnParameters(t *testing.T) {
	h, err := HashPassword("a sufficiently long password")
	if err != nil {
		t.Fatal(err)
	}
	// Parameters travel with the hash, so they can be raised later without
	// invalidating existing passwords.
	for _, want := range []string{"v=19", "m=", "t=", "p="} {
		if !strings.Contains(h, want) {
			t.Fatalf("hash %q does not record %q", h, want)
		}
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"not phc":          "plaintext",
		"wrong algorithm":  "$argon2i$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"bad version":      "$argon2id$v=99$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"missing sections": "$argon2id$v=19$m=65536,t=3,p=2",
		"bad base64":       "$argon2id$v=19$m=65536,t=3,p=2$!!!$aGFzaA",
		"weak params":      "$argon2id$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA",
		"zero parallelism": "$argon2id$v=19$m=65536,t=3,p=0$c2FsdA$aGFzaA",
		"unknown param":    "$argon2id$v=19$m=65536,t=3,x=2$c2FsdA$aGFzaA",
		"excessive memory": "$argon2id$v=19$m=4294967295,t=3,p=2$YWJjZGVmZ2hpamtsbW5vcA$YWJjZGVmZ2hpamtsbW5vcA",
		"excessive time":   "$argon2id$v=19$m=65536,t=4294967295,p=2$YWJjZGVmZ2hpamtsbW5vcA$YWJjZGVmZ2hpamtsbW5vcA",
		"oversized hash":   strings.Repeat("x", maxEncodedHashLen+1),
	}
	for name, h := range cases {
		ok, err := VerifyPassword("anything", h)
		if ok {
			t.Errorf("%s: a malformed hash verified successfully", name)
		}
		if err == nil {
			t.Errorf("%s: malformed hash produced no error", name)
		}
	}
}

func TestPasswordPolicy(t *testing.T) {
	if err := ValidatePassword(strings.Repeat("a", MinPasswordLength-1)); !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("short password accepted: %v", err)
	}
	if err := ValidatePassword(strings.Repeat("a", MaxPasswordLength+1)); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("overlong password accepted: %v", err)
	}
	if err := ValidatePassword(strings.Repeat("a", MinPasswordLength)); err != nil {
		t.Fatalf("valid password rejected: %v", err)
	}
	// A very long password must be rejected rather than hashed: Argon2id would
	// happily consume it, but the cost is attacker-controlled.
	if _, err := HashPassword(strings.Repeat("a", MaxPasswordLength+1)); err == nil {
		t.Fatal("an unbounded password was hashed")
	}
}

func TestUsernameValidation(t *testing.T) {
	valid := map[string]string{
		"Admin":       "admin",
		"  ops.team ": "ops.team",
		"a_b-c.123":   "a_b-c.123",
	}
	for in, want := range valid {
		got, err := ValidateUsername(in)
		if err != nil {
			t.Errorf("ValidateUsername(%q) errored: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ValidateUsername(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "ab", strings.Repeat("a", 65), "has space", "quote'", "semi;colon", "$ne", "../../etc"} {
		if _, err := ValidateUsername(bad); err == nil {
			t.Errorf("ValidateUsername(%q) was accepted", bad)
		}
	}
}

// A username that would be meaningful to MongoDB as an operator must be
// rejected before it ever reaches a query.
func TestUsernameRejectsOperatorShapedInput(t *testing.T) {
	for _, bad := range []string{"$ne", "$gt", "{\"$ne\":null}", "a$b"} {
		if _, err := ValidateUsername(bad); err == nil {
			t.Errorf("operator-shaped username %q accepted", bad)
		}
	}
}

func TestDummyVerificationCostsRealTime(t *testing.T) {
	// The unknown-user path must not be obviously faster than a real
	// verification, or response timing would enumerate accounts.
	h, err := HashPassword("a sufficiently long password")
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, _ = VerifyPassword("wrong password entirely", h)
	real := time.Since(start)

	start = time.Now()
	SpendDummyVerification("wrong password entirely")
	dummy := time.Since(start)

	if dummy < real/4 {
		t.Fatalf("dummy verification took %v against a real %v: the timing gap would leak account existence", dummy, real)
	}
}

func TestCSRFComparison(t *testing.T) {
	if !CheckCSRF("token-value", "token-value") {
		t.Fatal("matching CSRF tokens rejected")
	}
	for _, bad := range []string{"", "token-valu", "token-value ", "TOKEN-VALUE", "x"} {
		if CheckCSRF("token-value", bad) {
			t.Fatalf("CSRF token %q accepted", bad)
		}
	}
	if CheckCSRF("", "") {
		t.Fatal("two empty tokens compared as a match")
	}
}

// --- Rate limiting ---------------------------------------------------------

func TestLoginLimiterEnforcesBurst(t *testing.T) {
	l := newLoginLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !l.allow("client-a") {
			t.Fatalf("attempt %d refused within the burst", i+1)
		}
	}
	if l.allow("client-a") {
		t.Fatal("attempt beyond the burst was permitted")
	}
	// A different client is unaffected.
	if !l.allow("client-b") {
		t.Fatal("one client's limit blocked another")
	}
}

func TestLoginLimiterResetsOnSuccess(t *testing.T) {
	l := newLoginLimiter(2, time.Minute)
	l.allow("c")
	l.allow("c")
	if l.allow("c") {
		t.Fatal("limit not reached")
	}
	l.reset("c")
	if !l.allow("c") {
		t.Fatal("a successful login did not clear the attempt budget")
	}
}

func TestLoginLimiterWindowExpires(t *testing.T) {
	l := newLoginLimiter(1, 20*time.Millisecond)
	if !l.allow("c") {
		t.Fatal("first attempt refused")
	}
	if l.allow("c") {
		t.Fatal("second attempt allowed within the window")
	}
	time.Sleep(30 * time.Millisecond)
	if !l.allow("c") {
		t.Fatal("the window did not expire")
	}
}

func TestLoginLimiterDoesNotGrowWithoutBound(t *testing.T) {
	l := newLoginLimiter(1, 10*time.Millisecond)
	for i := 0; i < 500; i++ {
		l.allow(string(rune('a'+i%26)) + string(rune('0'+i%10)) + "-key")
	}
	time.Sleep(20 * time.Millisecond)
	// A sweep runs on the next call, discarding expired buckets.
	l.allow("trigger-sweep")

	l.mu.Lock()
	n := len(l.buckets)
	l.mu.Unlock()
	if n > 50 {
		t.Fatalf("limiter retained %d buckets after expiry; memory would grow without bound", n)
	}
}

// --- Client key ------------------------------------------------------------

func TestClientKeyIgnoresProxyHeadersByDefault(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/login", nil)
	r.RemoteAddr = "192.0.2.10:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.1")

	// Trusting the header by default would let anyone bypass rate limiting by
	// varying it.
	if got := ClientKey(r, false); got != "192.0.2.10" {
		t.Fatalf("ClientKey = %q; a spoofable header was trusted", got)
	}
	if got := ClientKey(r, true); got != "203.0.113.1" {
		t.Fatalf("ClientKey with trusted proxy = %q, want the forwarded address", got)
	}
}

func TestClientKeyHandlesIPv6AndMalformedAddresses(t *testing.T) {
	cases := map[string]string{
		"[2001:db8::1]:443": "2001:db8::1",
		"192.0.2.1:80":      "192.0.2.1",
		"garbage":           "garbage",
	}
	for addr, want := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = addr
		if got := ClientKey(r, false); got != want {
			t.Errorf("ClientKey(%q) = %q, want %q", addr, got, want)
		}
	}
}

func TestUserAgentIsHashedNotStored(t *testing.T) {
	ua := "Mozilla/5.0 (very identifying string)"
	h := hashUserAgent(ua)
	if h == "" {
		t.Fatal("no hash produced")
	}
	if strings.Contains(h, "Mozilla") {
		t.Fatal("the user agent was stored verbatim")
	}
	if h != hashUserAgent(ua) {
		t.Fatal("hashing is not deterministic")
	}
}

func FuzzDecodeHash(f *testing.F) {
	h, _ := HashPassword("a sufficiently long password")
	f.Add(h)
	f.Add("$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA")
	f.Add("")

	f.Fuzz(func(t *testing.T, encoded string) {
		// Must never panic, and must never verify an arbitrary string.
		ok, err := VerifyPassword("some candidate password", encoded)
		if ok && err != nil {
			t.Fatal("verification both succeeded and errored")
		}
	})
}
