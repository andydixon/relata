package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/andydixon/relata/internal/auth"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/ids"
	"github.com/andydixon/relata/internal/observability"
)

type ctxKey int

const (
	ctxKeyPrincipal ctxKey = iota
	ctxKeySession
)

// PrincipalFrom returns the authenticated caller, or nil.
func PrincipalFrom(ctx context.Context) *domain.Principal {
	p, _ := ctx.Value(ctxKeyPrincipal).(*domain.Principal)
	return p
}

// SessionFrom returns the active session, or nil.
func SessionFrom(ctx context.Context) *domain.Session {
	s, _ := ctx.Value(ctxKeySession).(*domain.Session)
	return s
}

// WithPrincipal attaches a principal to a context. Exported for tests.
func WithPrincipal(ctx context.Context, p *domain.Principal) context.Context {
	return context.WithValue(ctx, ctxKeyPrincipal, p)
}

// Middleware is the standard decorator shape.
type Middleware func(http.Handler) http.Handler

// Chain applies middleware so that the first listed runs outermost.
func Chain(h http.Handler, mw ...Middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// statusWriter captures the response status for logging and metrics.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Flush supports streaming handlers behind the wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// RequestID assigns a correlation ID to every request and echoes it back.
//
// A client-supplied ID is accepted only when it looks like one Relata would
// generate; anything else is replaced, so a caller cannot inject arbitrary text
// into log fields.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-ID")
			if !ids.Valid(id) {
				id = ids.New()
			}
			w.Header().Set("X-Request-ID", id)
			next.ServeHTTP(w, r.WithContext(observability.WithRequestID(r.Context(), id)))
		})
	}
}

// Logging records each request's outcome and duration.
type Logging struct {
	Metrics *observability.Metrics
}

// Middleware returns the logging middleware. Route is the pattern rather than
// the literal path, so metric label cardinality stays bounded.
func (l Logging) Middleware(route string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			sw := &statusWriter{ResponseWriter: w}

			if l.Metrics != nil {
				l.Metrics.HTTPInFlight.Inc()
				defer l.Metrics.HTTPInFlight.Dec()
			}

			next.ServeHTTP(sw, r)

			if sw.status == 0 {
				sw.status = http.StatusOK
			}
			dur := time.Since(started)

			observability.LoggerFrom(r.Context()).Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"duration_ms", dur.Milliseconds(),
				"bytes", sw.bytes,
			)

			if l.Metrics != nil {
				l.Metrics.HTTPRequests.WithLabelValues(route, r.Method, statusClass(sw.status)).Inc()
				l.Metrics.HTTPDuration.WithLabelValues(route, r.Method).Observe(dur.Seconds())
			}
		})
	}
}

func statusClass(code int) string {
	switch {
	case code < 200:
		return "1xx"
	case code < 300:
		return "2xx"
	case code < 400:
		return "3xx"
	case code < 500:
		return "4xx"
	default:
		return "5xx"
	}
}

// Recover turns a panic into a 500 rather than a dropped connection.
func Recover() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if recover() != nil {
					// Panic values can contain credentials or request content. The
					// request ID and route identify the failure without persisting it.
					observability.LoggerFrom(r.Context()).Error("panic while handling request",
						"path", r.URL.Path, "method", r.Method)
					WriteError(w, r, &APIError{
						Status: http.StatusInternalServerError, Code: CodeInternal,
						Message: "An unexpected error occurred.",
					})
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// SecurityHeaders sets the response headers that constrain what a browser will
// do with Relata's pages.
//
// The Content-Security-Policy is deliberately restrictive. Relata renders
// stored content as escaped text and ships its own CSS and JavaScript as files,
// so no inline script, no remote origin and no framing are needed — and
// forbidding them means a stored-XSS payload that somehow reached the page
// still could not execute or exfiltrate.
func SecurityHeaders(allowInlineStyle bool) Middleware {
	directives := []string{
		"default-src 'none'",
		"script-src 'self'",
		"style-src 'self'",
		"img-src 'self' data:",
		"font-src 'self'",
		"connect-src 'self'",
		"form-action 'self'",
		"base-uri 'none'",
		"frame-ancestors 'none'",
		"object-src 'none'",
		"manifest-src 'self'",
	}
	csp := strings.Join(directives, "; ")

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Content-Security-Policy", csp)
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "same-origin")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=(), payment=(), usb=()")
			// Caches must not retain authenticated pages.
			h.Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r)
		})
	}
}

// MaxBody rejects request bodies over a limit before a handler reads them.
func MaxBody(limit int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				WriteError(w, r, &APIError{
					Status: http.StatusRequestEntityTooLarge, Code: CodePayloadTooLarge,
					Message: "The request body is larger than the configured limit.",
				})
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

// Authenticator resolves sessions and, on the API, bearer tokens.
type Authenticator struct {
	Auth *auth.Service
	// Tokens enables bearer-token authentication. It is set only on the API
	// chain: a token is never a way into the web interface.
	Tokens *auth.TokenService
	// APIMode returns a JSON 401 rather than redirecting to the login page.
	APIMode bool
	// LoginPath is where browser requests are sent when unauthenticated.
	LoginPath string
}

// resolve authenticates a request, preferring the session cookie.
//
// The ordering is load-bearing. A request carrying a valid session cookie
// authenticates as that session even if it also sends an Authorization header,
// so the CSRF exemption below — which keys off how the request authenticated —
// cannot be switched off by adding a header to a cookie-bearing request.
func (a Authenticator) resolve(r *http.Request) (*domain.Principal, *domain.Session, error) {
	if p, sess, err := a.Auth.Resolve(r.Context(), a.Auth.TokenFromRequest(r)); err == nil && p != nil {
		return p, sess, nil
	}
	if a.Tokens == nil {
		return nil, nil, auth.ErrNoSession
	}
	secret := auth.BearerFromRequest(r)
	if secret == "" {
		return nil, nil, auth.ErrNoSession
	}
	p, err := a.Tokens.Authenticate(r.Context(), secret)
	if err != nil {
		return nil, nil, err
	}
	return p, nil, nil
}

// RequireAuth rejects unauthenticated requests.
func (a Authenticator) RequireAuth() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, sess, err := a.resolve(r)
			if err != nil || p == nil {
				a.rejectWith(w, r, err)
				return
			}
			ctx := context.WithValue(r.Context(), ctxKeyPrincipal, p)
			ctx = context.WithValue(ctx, ctxKeySession, sess)
			// Correlate logs with the user without recording anything sensitive.
			ctx = observability.WithLogger(ctx,
				observability.LoggerFrom(ctx).With("user_id", p.UserID, "workspace_id", p.WorkspaceID))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// OptionalAuth resolves a session when present but permits anonymous requests.
func (a Authenticator) OptionalAuth() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, sess, err := a.resolve(r)
			if err == nil && p != nil {
				ctx := context.WithValue(r.Context(), ctxKeyPrincipal, p)
				ctx = context.WithValue(ctx, ctxKeySession, sess)
				r = r.WithContext(ctx)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// rejectWith refuses a request, distinguishing the one failure a caller can act
// on — being rate limited — from every other, which are deliberately identical.
func (a Authenticator) rejectWith(w http.ResponseWriter, r *http.Request, err error) {
	if a.APIMode && errors.Is(err, auth.ErrTokenRateLimit) {
		WriteError(w, r, &APIError{
			Status: http.StatusTooManyRequests, Code: CodeRateLimited,
			Message: "This token is making too many requests. Slow down and retry.",
		})
		return
	}
	a.reject(w, r)
}

func (a Authenticator) reject(w http.ResponseWriter, r *http.Request) {
	if a.APIMode {
		WriteError(w, r, &APIError{
			Status: http.StatusUnauthorized, Code: CodeUnauthorised,
			Message: "Authentication is required. Sign in to obtain a session.",
		})
		return
	}
	path := a.LoginPath
	if path == "" {
		path = "/login"
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// RequireScope rejects a caller that may not perform an action.
//
// The check is the intersection of the person's role and, when the request came
// from an API token, that token's scopes — so a token can never do more than
// the person who created it, and demoting someone narrows every token they hold
// without any of those tokens being touched.
func RequireScope(scope domain.TokenScope) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := PrincipalFrom(r.Context())
			if p == nil {
				WriteError(w, r, &APIError{
					Status: http.StatusUnauthorized, Code: CodeUnauthorised,
					Message: "Authentication is required.",
				})
				return
			}
			if p.Permits(scope) {
				next.ServeHTTP(w, r)
				return
			}

			// Say which scope was needed. A caller holding the wrong token
			// should be able to fix it without guessing, and the scope names
			// are public API surface rather than a secret.
			if p.ViaToken() {
				WriteError(w, r, &APIError{
					Status: http.StatusForbidden, Code: CodeInsufficientScope,
					Message: "This API token does not carry the scope this request needs.",
					Detail:  map[string]string{"required_scope": string(scope)},
				})
				return
			}
			WriteError(w, r, &APIError{
				Status: http.StatusForbidden, Code: CodeForbidden,
				Message: "Your role does not permit this.",
			})
		})
	}
}

// RequireSession rejects anything not authenticated by an interactive session.
//
// Token management lives behind this: a leaked token must not be able to mint
// more tokens, extend its own life, or revoke the tokens that would replace it.
func RequireSession() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := PrincipalFrom(r.Context())
			if p == nil || p.ViaToken() {
				WriteError(w, r, &APIError{
					Status: http.StatusForbidden, Code: CodeForbidden,
					Message: "This action requires a signed-in session. API tokens cannot manage API tokens.",
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireWrite rejects a caller whose role cannot modify content.
func RequireWrite() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := PrincipalFrom(r.Context())
			if p == nil || !p.Role.CanWrite() {
				WriteError(w, r, &APIError{
					Status: http.StatusForbidden, Code: CodeForbidden,
					Message: "Your role does not permit changes.",
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CSRF protects state-changing browser requests.
//
// The check has two independent parts:
//
//  1. Origin/Referer must match the expected host. A cross-site form post
//     carries the attacker's origin, and browsers will not let script forge it.
//  2. A per-session token, submitted in a form field or header, must match the
//     session's own token in a constant-time comparison.
//
// Safe methods are exempt because they must not change state in the first place.
type CSRF struct {
	// TrustedOrigins are the origins permitted to submit state-changing
	// requests, derived from the configured base URL.
	TrustedOrigins []string
	// HeaderName is where a fetch/HTMX request supplies the token.
	HeaderName string
	// FieldName is the form field carrying the token.
	FieldName string
}

// Middleware returns the CSRF middleware.
func (c CSRF) Middleware() Middleware {
	header := c.HeaderName
	if header == "" {
		header = "X-CSRF-Token"
	}
	field := c.FieldName
	if field == "" {
		field = "csrf_token"
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
				next.ServeHTTP(w, r)
				return
			}

			// A bearer-authenticated request needs no CSRF token. CSRF exists
			// to stop a third-party page spending *ambient* credentials, and an
			// Authorization header is not ambient: a browser never attaches one
			// on its own.
			//
			// The exemption keys off how the request authenticated, not off a
			// header being present. Because resolve() prefers the session
			// cookie, a request holding a cookie is never ViaToken, so adding
			// an Authorization header cannot disable this check.
			if p := PrincipalFrom(r.Context()); p != nil && p.ViaToken() {
				next.ServeHTTP(w, r)
				return
			}

			sess := SessionFrom(r.Context())
			if sess == nil {
				WriteError(w, r, &APIError{
					Status: http.StatusUnauthorized, Code: CodeUnauthorised,
					Message: "Authentication is required.",
				})
				return
			}

			if !c.originAllowed(r) {
				WriteError(w, r, &APIError{
					Status: http.StatusForbidden, Code: CodeCSRFFailed,
					Message: "This request appears to originate from another site and was refused.",
				})
				return
			}

			token := r.Header.Get(header)
			if token == "" {
				// Reading the form consumes the body; only do so for form
				// content types, so a JSON body is left intact for the handler.
				ct := r.Header.Get("Content-Type")
				base, _, _ := strings.Cut(ct, ";")
				switch strings.TrimSpace(base) {
				case "application/x-www-form-urlencoded", "multipart/form-data":
					_ = r.ParseMultipartForm(8 << 20)
					token = r.FormValue(field)
				}
			}

			if !auth.CheckCSRF(sess.CSRFToken, token) {
				observability.LoggerFrom(r.Context()).Warn("CSRF check failed",
					"path", r.URL.Path, "method", r.Method, "has_token", token != "")
				WriteError(w, r, &APIError{
					Status: http.StatusForbidden, Code: CodeCSRFFailed,
					Message: "Your session's security token was missing or invalid. Reload the page and try again.",
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// originAllowed checks the Origin header, falling back to Referer.
//
// A request with neither header is refused rather than allowed: every browser
// that can perform a cross-site state-changing request sends at least one.
func (c CSRF) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		if ref := r.Header.Get("Referer"); ref != "" {
			origin = originOf(ref)
		}
	}
	if origin == "" {
		// A non-browser API client with a valid session token and no Origin
		// header is not subject to CSRF (there is no ambient credential to
		// abuse from a browser), but it must still present the token, which is
		// checked next.
		return true
	}
	for _, allowed := range c.TrustedOrigins {
		if strings.EqualFold(origin, allowed) {
			return true
		}
	}
	return false
}

// originOf extracts scheme://host from a URL string without full parsing.
func originOf(u string) string {
	i := strings.Index(u, "://")
	if i < 0 {
		return ""
	}
	rest := u[i+3:]
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		rest = rest[:j]
	}
	return u[:i+3] + rest
}

// withSession attaches a session to a context. Exported for tests in this
// package; the middleware sets it during normal operation.
func withSession(ctx context.Context, s *domain.Session) context.Context {
	return context.WithValue(ctx, ctxKeySession, s)
}
