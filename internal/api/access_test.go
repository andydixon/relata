package api

import (
	"regexp"
	"strings"
	"testing"
)

// Route registration is explicit so that the authentication treatment of every
// endpoint is readable in one place. These tests read that place and assert it,
// so a route added without auth — or a page accidentally made public — fails
// here rather than in production.

// webRoutePattern captures a browser route and the options it was registered with.
var webRoutePattern = regexp.MustCompile(
	`mux\.Handle\("(GET|POST|PATCH|DELETE|PUT) ([^"]+)",\s*webChain\("[^"]*",\s*wb\.\w+,\s*([^)]+)\)\)`)

// publicRoutes are the only browser routes an unauthenticated visitor may reach.
//
// Adding to this list is a deliberate act: every entry is a page that must read
// nothing from the database.
var publicRoutes = map[string]bool{
	// Go's exact-match pattern for the root. Dual-purpose: the dashboard when
	// signed in, the landing page when not.
	"/{$}":                 true,
	"/login":               true,
	"/auth/oauth/start":    true,
	"/auth/oauth/callback": true,
	"/how-it-works":        true,
	"/api":                 true,
}

func TestOnlyIntendedBrowserRoutesArePublic(t *testing.T) {
	src := readFile(t, routerPath)

	matches := webRoutePattern.FindAllStringSubmatch(src, -1)
	if len(matches) < 20 {
		t.Fatalf("only %d browser routes matched; the pattern has probably drifted from the router",
			len(matches))
	}

	for _, m := range matches {
		method, path, opts := m[1], m[2], strings.TrimSpace(m[3])

		authenticated := strings.Contains(opts, "read") ||
			strings.Contains(opts, "write") ||
			strings.Contains(opts, "authenticated: true")
		optional := strings.Contains(opts, "optionalAuth")
		public := opts == "pub" || optional

		if public && !publicRoutes[path] {
			t.Errorf("%s %s is registered as public (%s) but is not in the intended public list; "+
				"if that is deliberate, add it there and make sure the page reads no stored data",
				method, path, opts)
		}
		if !public && !authenticated {
			t.Errorf("%s %s is registered with %q, which neither authenticates nor is public",
				method, path, opts)
		}
	}
}

// Every route under /api/v1 must name the scope it needs. A route that reached
// the mux without one would authenticate but authorise nothing.
func TestEveryAPIRouteNamesAScope(t *testing.T) {
	src := readFile(t, routerPath)

	registration := regexp.MustCompile(`mux\.Handle\("(GET|POST|PATCH|DELETE|PUT) (/api/v1[^"]*)",\s*(\w+)\(`)
	matches := registration.FindAllStringSubmatch(src, -1)
	if len(matches) < 15 {
		t.Fatalf("only %d API routes matched; the pattern has probably drifted", len(matches))
	}

	for _, m := range matches {
		method, path, chain := m[1], m[2], m[3]
		switch chain {
		case "apiChain":
			// apiChain takes a scope as its third argument; check this call has one.
			call := regexp.MustCompile(
				`mux\.Handle\("` + regexp.QuoteMeta(method+" "+path) + `",\s*apiChain\([^)]*domain\.Scope\w+\)\)`)
			if !call.MatchString(src) {
				t.Errorf("%s %s uses apiChain without a domain.Scope argument", method, path)
			}
		case "tokenChain":
			// Token management is session-only by design, not scoped.
		default:
			t.Errorf("%s %s is registered with %q, which is neither apiChain nor tokenChain — "+
				"its authentication and scope treatment is not readable in one place",
				method, path, chain)
		}
	}
}

// The password endpoint must be conditional on local login being enabled, so a
// disabled break-glass path is absent rather than present and refusing.
func TestPasswordLoginIsRegisteredConditionally(t *testing.T) {
	src := readFile(t, routerPath)

	idx := strings.Index(src, `mux.Handle("POST /login"`)
	if idx < 0 {
		t.Fatal("the password login route is not registered at all")
	}
	// Look back a little way for the guard.
	window := src[max(0, idx-500):idx]
	if !strings.Contains(window, "LocalLoginEnabled") {
		t.Fatal("POST /login is registered unconditionally; it must be guarded by " +
			"cfg.Auth.LocalLoginEnabled so a disabled local login has no handler to reach")
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
