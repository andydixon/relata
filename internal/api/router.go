package api

import (
	"net/http"
	"strings"

	"github.com/andydixon/relata/internal/auth"
	"github.com/andydixon/relata/internal/config"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/observability"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// WebRoutes is implemented by the HTML handler set, so the router can mount the
// browser interface without importing the web package (which imports this one).
type WebRoutes interface {
	LoginPage(http.ResponseWriter, *http.Request)
	Login(http.ResponseWriter, *http.Request)
	Logout(http.ResponseWriter, *http.Request)
	OAuthStart(http.ResponseWriter, *http.Request)
	OAuthCallback(http.ResponseWriter, *http.Request)
	LandingPage(http.ResponseWriter, *http.Request)
	HowItWorksPage(http.ResponseWriter, *http.Request)
	PublicAPIPage(http.ResponseWriter, *http.Request)
	TokensPage(http.ResponseWriter, *http.Request)
	CreateToken(http.ResponseWriter, *http.Request)
	RevokeToken(http.ResponseWriter, *http.Request)
	Dashboard(http.ResponseWriter, *http.Request)
	ItemsPage(http.ResponseWriter, *http.Request)
	ItemDetail(http.ResponseWriter, *http.Request)
	Neighbourhood(http.ResponseWriter, *http.Request)
	DownloadOriginal(http.ResponseWriter, *http.Request)
	AddTextPage(http.ResponseWriter, *http.Request)
	AddText(http.ResponseWriter, *http.Request)
	UploadPage(http.ResponseWriter, *http.Request)
	Upload(http.ResponseWriter, *http.Request)
	DeleteItem(http.ResponseWriter, *http.Request)
	ReprocessItem(http.ResponseWriter, *http.Request)
	SearchPage(http.ResponseWriter, *http.Request)
	ReviewPage(http.ResponseWriter, *http.Request)
	ConfirmRelationship(http.ResponseWriter, *http.Request)
	RejectRelationship(http.ResponseWriter, *http.Request)
	ChangeRelationshipType(http.ResponseWriter, *http.Request)
	CreateRelationship(http.ResponseWriter, *http.Request)
	EntitiesPage(http.ResponseWriter, *http.Request)
	EntityDetail(http.ResponseWriter, *http.Request)
	ConfirmAlias(http.ResponseWriter, *http.Request)
	RejectAlias(http.ResponseWriter, *http.Request)
	JobsPage(http.ResponseWriter, *http.Request)
	JobsTable(http.ResponseWriter, *http.Request)
	RetryJob(http.ResponseWriter, *http.Request)
	SettingsPage(http.ResponseWriter, *http.Request)
	Reembed(http.ResponseWriter, *http.Request)
}

// RouterConfig holds what the router needs to wire the application together.
type RouterConfig struct {
	Cfg     *config.Config
	Auth    *auth.Service
	Tokens  *auth.TokenService
	API     *Handlers
	Web     WebRoutes
	Health  *HealthChecker
	Metrics *observability.Metrics
	Static  http.Handler
	Logger  interface{ Handler() any }
}

// NewRouter builds the complete HTTP handler.
//
// Route registration is explicit rather than pattern-driven, so that the exact
// authentication, CSRF and body-size treatment of every endpoint is readable in
// one place.
func NewRouter(rc RouterConfig) http.Handler {
	mux := http.NewServeMux()

	logging := Logging{Metrics: rc.Metrics}
	authn := Authenticator{Auth: rc.Auth, LoginPath: "/login"}
	apiAuthn := Authenticator{Auth: rc.Auth, Tokens: rc.Tokens, APIMode: true}

	csrf := CSRF{
		TrustedOrigins: trustedOrigins(rc.Cfg),
		HeaderName:     "X-CSRF-Token",
		FieldName:      "csrf_token",
	}

	// --- Health and metrics -------------------------------------------------
	//
	// Health endpoints are unauthenticated so an orchestrator can reach them,
	// but they expose only status, never workspace content.
	mux.Handle("GET /health/live", wrap(rc, "/health/live", http.HandlerFunc(rc.Health.Live)))
	mux.Handle("GET /health/ready", wrap(rc, "/health/ready", http.HandlerFunc(rc.Health.Ready)))

	if rc.Cfg.HTTP.MetricsEnabled && rc.Metrics != nil {
		// Metrics describe the service, not stored knowledge. They are bound to
		// the same loopback listener as everything else.
		mux.Handle("GET /metrics", promhttp.HandlerFor(rc.Metrics.Registry, promhttp.HandlerOpts{
			ErrorHandling: promhttp.ContinueOnError,
		}))
	}

	// --- Static assets ------------------------------------------------------
	mux.Handle("GET /static/", staticHandler(rc.Static))

	// --- REST API -----------------------------------------------------------
	//
	// API routes authenticate from the session cookie and enforce CSRF on
	// state-changing methods, exactly as the browser interface does.
	// Every API route names the scope it needs. CSRF is applied unconditionally
	// because the middleware skips safe methods itself and exempts bearer
	// requests — so no route can be registered having forgotten it.
	//
	// RequireScope subsumes the old RequireWrite: the scope check is the
	// intersection of the caller's role and, for a token, its granted scopes.
	apiChain := func(route string, h http.HandlerFunc, scope domain.TokenScope) http.Handler {
		return Chain(h,
			RequestID(),
			Recover(),
			logging.Middleware(route),
			SecurityHeaders(false),
			apiAuthn.RequireAuth(),
			csrf.Middleware(),
			RequireScope(scope),
		)
	}

	// Token management is deliberately not scoped: it requires an interactive
	// session, so a leaked token cannot mint its own replacements.
	tokenChain := func(route string, h http.HandlerFunc) http.Handler {
		return Chain(h,
			RequestID(),
			Recover(),
			logging.Middleware(route),
			SecurityHeaders(false),
			apiAuthn.RequireAuth(),
			csrf.Middleware(),
			RequireSession(),
		)
	}

	a := rc.API
	mux.Handle("POST /api/v1/items", apiChain("/api/v1/items", a.CreateItem, domain.ScopeIngest))
	mux.Handle("POST /api/v1/items/upload", apiChain("/api/v1/items/upload", a.UploadItem, domain.ScopeIngest))
	mux.Handle("GET /api/v1/items", apiChain("/api/v1/items", a.ListItems, domain.ScopeQuery))
	mux.Handle("GET /api/v1/items/{id}", apiChain("/api/v1/items/{id}", a.GetItem, domain.ScopeQuery))
	mux.Handle("DELETE /api/v1/items/{id}", apiChain("/api/v1/items/{id}", a.DeleteItem, domain.ScopeIngest))
	mux.Handle("POST /api/v1/items/{id}/reprocess", apiChain("/api/v1/items/{id}/reprocess", a.ReprocessItem, domain.ScopeIngest))
	mux.Handle("GET /api/v1/items/{id}/relationships", apiChain("/api/v1/items/{id}/relationships", a.GetItemRelationships, domain.ScopeQuery))

	mux.Handle("GET /api/v1/search", apiChain("/api/v1/search", a.Search, domain.ScopeQuery))
	// Answering reads; it creates nothing. It needs the query scope, not ingest.
	mux.Handle("POST /api/v1/search/answer", apiChain("/api/v1/search/answer", a.Answer, domain.ScopeQuery))

	mux.Handle("GET /api/v1/relationships", apiChain("/api/v1/relationships", a.ListRelationships, domain.ScopeQuery))
	mux.Handle("POST /api/v1/relationships", apiChain("/api/v1/relationships", a.CreateRelationship, domain.ScopeReview))
	mux.Handle("PATCH /api/v1/relationships/{id}", apiChain("/api/v1/relationships/{id}", a.PatchRelationship, domain.ScopeReview))
	mux.Handle("POST /api/v1/relationships/{id}/confirm", apiChain("/api/v1/relationships/{id}/confirm", a.ConfirmRelationship, domain.ScopeReview))
	mux.Handle("POST /api/v1/relationships/{id}/reject", apiChain("/api/v1/relationships/{id}/reject", a.RejectRelationship, domain.ScopeReview))

	mux.Handle("GET /api/v1/entities", apiChain("/api/v1/entities", a.ListEntities, domain.ScopeQuery))
	mux.Handle("GET /api/v1/entities/{id}", apiChain("/api/v1/entities/{id}", a.GetEntity, domain.ScopeQuery))
	mux.Handle("POST /api/v1/aliases/{id}/confirm", apiChain("/api/v1/aliases/{id}/confirm", a.ConfirmAlias, domain.ScopeReview))
	mux.Handle("POST /api/v1/aliases/{id}/reject", apiChain("/api/v1/aliases/{id}/reject", a.RejectAlias, domain.ScopeReview))

	mux.Handle("GET /api/v1/jobs", apiChain("/api/v1/jobs", a.ListJobs, domain.ScopeQuery))
	mux.Handle("POST /api/v1/jobs/{id}/retry", apiChain("/api/v1/jobs/{id}/retry", a.RetryJob, domain.ScopeIngest))

	mux.Handle("GET /api/v1/tokens", tokenChain("/api/v1/tokens", a.ListTokens))
	mux.Handle("POST /api/v1/tokens", tokenChain("/api/v1/tokens", a.CreateToken))
	mux.Handle("DELETE /api/v1/tokens/{id}", tokenChain("/api/v1/tokens/{id}", a.RevokeToken))

	// --- Browser interface --------------------------------------------------
	webChain := func(route string, h http.HandlerFunc, opts webOpts) http.Handler {
		mw := []Middleware{
			RequestID(),
			Recover(),
			logging.Middleware(route),
			SecurityHeaders(false),
		}
		if opts.authenticated {
			mw = append(mw, authn.RequireAuth())
		} else if opts.optionalAuth {
			mw = append(mw, authn.OptionalAuth())
		}
		if opts.csrf {
			mw = append(mw, csrf.Middleware())
		}
		if opts.write {
			mw = append(mw, RequireWrite())
		}
		return Chain(h, mw...)
	}

	pub := webOpts{}
	read := webOpts{authenticated: true}
	write := webOpts{authenticated: true, csrf: true, write: true}

	wb := rc.Web
	mux.Handle("GET /login", webChain("/login", wb.LoginPage, pub))
	// The password endpoint is registered only when local login is enabled, so
	// a disabled break-glass path is genuinely absent rather than present and
	// refusing. There is no handler to reach and nothing to probe.
	if rc.Cfg.Auth.LocalLoginEnabled {
		// It has its own CSRF handling: no session exists yet, so the
		// session-token check cannot apply.
		mux.Handle("POST /login", webChain("/login", wb.Login, pub))
	}
	mux.Handle("POST /logout", webChain("/logout", wb.Logout, webOpts{authenticated: true, csrf: true}))

	// Federated sign-in. Both are public: the first has no session yet, and the
	// second is where the provider sends the browser back to.
	mux.Handle("GET /auth/oauth/start", webChain("/auth/oauth/start", wb.OAuthStart, pub))
	mux.Handle("GET "+config.CallbackPath, webChain(config.CallbackPath, wb.OAuthCallback, pub))

	// The public site. These pages read nothing from the database.
	mux.Handle("GET /how-it-works", webChain("/how-it-works", wb.HowItWorksPage, pub))
	mux.Handle("GET /api", webChain("/api", wb.PublicAPIPage, pub))

	// The only dual-purpose route: the dashboard when signed in, the landing
	// page when not. OptionalAuth rather than RequireAuth, so an anonymous
	// visitor is served rather than redirected.
	mux.Handle("GET /{$}", webChain("/", wb.LandingPage, webOpts{optionalAuth: true}))
	mux.Handle("GET /items", webChain("/items", wb.ItemsPage, read))
	mux.Handle("GET /items/{id}", webChain("/items/{id}", wb.ItemDetail, read))
	mux.Handle("GET /items/{id}/neighbourhood", webChain("/items/{id}/neighbourhood", wb.Neighbourhood, read))
	mux.Handle("GET /items/{id}/download", webChain("/items/{id}/download", wb.DownloadOriginal, read))
	mux.Handle("POST /items/{id}/delete", webChain("/items/{id}/delete", wb.DeleteItem, write))
	mux.Handle("POST /items/{id}/reprocess", webChain("/items/{id}/reprocess", wb.ReprocessItem, write))

	mux.Handle("GET /add", webChain("/add", wb.AddTextPage, read))
	mux.Handle("POST /add", webChain("/add", wb.AddText, write))
	mux.Handle("GET /upload", webChain("/upload", wb.UploadPage, read))
	mux.Handle("POST /upload", webChain("/upload", wb.Upload, write))

	mux.Handle("GET /search", webChain("/search", wb.SearchPage, read))

	mux.Handle("GET /review", webChain("/review", wb.ReviewPage, read))
	mux.Handle("POST /relationships/create", webChain("/relationships/create", wb.CreateRelationship, write))
	mux.Handle("POST /relationships/{id}/confirm", webChain("/relationships/{id}/confirm", wb.ConfirmRelationship, write))
	mux.Handle("POST /relationships/{id}/reject", webChain("/relationships/{id}/reject", wb.RejectRelationship, write))
	mux.Handle("POST /relationships/{id}/type", webChain("/relationships/{id}/type", wb.ChangeRelationshipType, write))

	mux.Handle("GET /entities", webChain("/entities", wb.EntitiesPage, read))
	mux.Handle("GET /entities/{id}", webChain("/entities/{id}", wb.EntityDetail, read))
	mux.Handle("POST /aliases/{id}/confirm", webChain("/aliases/{id}/confirm", wb.ConfirmAlias, write))
	mux.Handle("POST /aliases/{id}/reject", webChain("/aliases/{id}/reject", wb.RejectAlias, write))

	mux.Handle("GET /jobs", webChain("/jobs", wb.JobsPage, read))
	mux.Handle("GET /jobs/table", webChain("/jobs/table", wb.JobsTable, read))
	mux.Handle("POST /jobs/{id}/retry", webChain("/jobs/{id}/retry", wb.RetryJob, write))

	mux.Handle("GET /settings", webChain("/settings", wb.SettingsPage, read))
	mux.Handle("POST /settings/reembed", webChain("/settings/reembed", wb.Reembed, write))

	mux.Handle("GET /settings/tokens", webChain("/settings/tokens", wb.TokensPage, read))
	mux.Handle("POST /settings/tokens", webChain("/settings/tokens", wb.CreateToken, write))
	mux.Handle("POST /settings/tokens/{id}/revoke", webChain("/settings/tokens/{id}/revoke", wb.RevokeToken, write))

	// Anything unmatched. The handler distinguishes API paths from browser
	// paths so a mistyped API URL gets JSON, not HTML.
	mux.Handle("/", Chain(http.HandlerFunc(notFound),
		RequestID(), Recover(), logging.Middleware("not_found"), SecurityHeaders(false)))

	// The outermost body limit applies to every route. Upload routes raise it
	// themselves, since a document is legitimately larger than a JSON request.
	return MaxBody(maxBodyFor(rc.Cfg))(mux)
}

type webOpts struct {
	authenticated bool
	// optionalAuth resolves a session when one is present but serves anonymous
	// callers rather than redirecting them.
	optionalAuth bool
	csrf         bool
	write        bool
}

// maxBodyFor returns the global request-body ceiling.
//
// It is the larger of the JSON limit and the upload limit plus multipart
// overhead, because one middleware guards both kinds of route.
func maxBodyFor(cfg *config.Config) int64 {
	limit := cfg.HTTP.MaxRequestBytes
	if upload := cfg.Uploads.MaxUploadBytes + (2 << 20); upload > limit {
		limit = upload
	}
	return limit
}

// wrap applies the standard middleware to an unauthenticated endpoint.
func wrap(rc RouterConfig, route string, h http.Handler) http.Handler {
	logging := Logging{Metrics: rc.Metrics}
	return Chain(h, RequestID(), Recover(), logging.Middleware(route), SecurityHeaders(false))
}

// staticHandler serves embedded assets with long-lived caching.
//
// Static files carry no user content, so they are the one place where caching
// is safe; the no-store default set by SecurityHeaders is overridden here.
func staticHandler(fs http.Handler) http.Handler {
	return http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reject any path attempting to escape the asset root. http.FS already
		// refuses these, but failing early keeps the intent explicit.
		if strings.Contains(r.URL.Path, "..") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		fs.ServeHTTP(w, r)
	}))
}

// notFound answers unmatched routes in the right format.
func notFound(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		WriteError(w, r, &APIError{
			Status: http.StatusNotFound, Code: CodeNotFound,
			Message: "No such endpoint. The API is versioned under /api/v1.",
		})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`<!DOCTYPE html><html lang="en-GB"><head><meta charset="utf-8">` +
		`<title>Not found — Relata</title><link rel="stylesheet" href="/static/app.css"></head>` +
		`<body><main class="page"><div class="error-page"><h1>Page not found</h1>` +
		`<p>There is nothing at this address.</p>` +
		`<p><a class="btn" href="/">Back to the dashboard</a></p></div></main></body></html>`))
}

// trustedOrigins derives the origins permitted to submit state-changing
// requests, from the configured base URL and listen address.
func trustedOrigins(cfg *config.Config) []string {
	origins := []string{}
	if base := strings.TrimRight(cfg.HTTP.BaseURL, "/"); base != "" {
		origins = append(origins, base)
	}
	// The listen address is also accepted, so that reaching Relata directly at
	// http://127.0.0.1:8080 works when BaseURL names something else.
	addr := cfg.HTTP.Addr
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	for _, scheme := range []string{"http://", "https://"} {
		o := scheme + addr
		if !containsStr(origins, o) {
			origins = append(origins, o)
		}
	}
	// localhost and 127.0.0.1 are the same host to a person but different
	// origins to a browser.
	if strings.HasPrefix(addr, "127.0.0.1:") {
		port := strings.TrimPrefix(addr, "127.0.0.1")
		origins = append(origins, "http://localhost"+port, "https://localhost"+port)
	}
	return origins
}

func containsStr(list []string, v string) bool {
	for _, e := range list {
		if e == v {
			return true
		}
	}
	return false
}
