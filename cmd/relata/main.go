// Command relata runs the Relata application and its administrative commands.
//
// Subcommands:
//
//	serve         run the HTTP server and background workers (default)
//	createadmin   create the initial administrator account
//	seed          load the demonstration dataset
//	version       print version information
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/andydixon/relata/internal/analysis"
	"github.com/andydixon/relata/internal/api"
	"github.com/andydixon/relata/internal/audit"
	"github.com/andydixon/relata/internal/auth"
	"github.com/andydixon/relata/internal/config"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/embeddings"
	"github.com/andydixon/relata/internal/entities"
	"github.com/andydixon/relata/internal/ingest"
	"github.com/andydixon/relata/internal/jobs"
	"github.com/andydixon/relata/internal/observability"
	"github.com/andydixon/relata/internal/relationships"
	"github.com/andydixon/relata/internal/search"
	"github.com/andydixon/relata/internal/storage/mongo"
	"github.com/andydixon/relata/internal/storage/qdrant"
	"github.com/andydixon/relata/internal/web"
)

// version is overridden at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "relata: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !isFlag(args[0]) {
		cmd = args[0]
		args = args[1:]
	}

	switch cmd {
	case "serve":
		return cmdServe(args)
	case "createadmin":
		return cmdCreateAdmin(args)
	case "seed":
		return cmdSeed(args)
	case "version":
		fmt.Printf("relata %s\n", version)
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func isFlag(s string) bool { return len(s) > 0 && s[0] == '-' }

func usage() {
	fmt.Fprint(os.Stderr, `Relata — a self-organising semantic document database

Usage:
  relata [command] [flags]

Commands:
  serve         Run the HTTP server and background workers (default)
  createadmin   Create the initial administrator account
  seed          Load the demonstration dataset
  version       Print the version

Run "relata <command> -h" for the flags of a specific command.
Configuration is read from the environment; see .env.example.
`)
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configFile := fs.String("config", ".env", "path to a key=value configuration file (optional)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configFile)
	if err != nil {
		return err
	}

	logger := observability.NewLogger(cfg.LogLevel, cfg.LogJSON)
	metrics := observability.NewMetrics()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx = observability.WithLogger(ctx, logger)

	logger.Info("starting relata", "version", version, "config", cfg.Redacted())

	// --- Storage ------------------------------------------------------------
	store, err := mongo.Connect(ctx, cfg.Mongo, metrics)
	if err != nil {
		return fmt.Errorf("mongodb: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = store.Close(shutdownCtx)
	}()

	if err := store.EnsureIndexes(ctx); err != nil {
		return fmt.Errorf("create mongodb indexes: %w", err)
	}
	logger.Info("mongodb ready", "database", cfg.Mongo.Database)

	vectors := qdrant.New(cfg.Qdrant, metrics)
	if err := vectors.Health(ctx); err != nil {
		return fmt.Errorf("qdrant at %s is unreachable: %w", cfg.Qdrant.URL, err)
	}

	// --- AI providers -------------------------------------------------------
	retry := embeddings.RetryPolicy{
		MaxAttempts: cfg.Ollama.MaxRetries,
		BaseDelay:   cfg.Ollama.RetryBaseDelay,
		MaxDelay:    cfg.Ollama.RetryMaxDelay,
	}
	embedder := embeddings.NewOllama(embeddings.OllamaOptions{
		BaseURL:      cfg.Ollama.BaseURL,
		Model:        cfg.Ollama.EmbeddingModel,
		Timeout:      cfg.Ollama.EmbedTimeout,
		BatchSize:    cfg.Ollama.EmbedBatchSize,
		Retry:        retry,
		ExpectedDims: cfg.Ollama.EmbeddingDims,
		Metrics:      metrics,
	})
	analyser := analysis.NewOllamaAnalyser(analysis.OllamaOptions{
		BaseURL:   cfg.Ollama.BaseURL,
		Model:     cfg.Ollama.AnalysisModel,
		Timeout:   cfg.Ollama.AnalysisTimeout,
		Retry:     retry,
		MaxTokens: cfg.Ollama.MaxOutputTokens,
		Metrics:   metrics,
	})

	if err := vectors.EnsureCollection(ctx, embedder.Dimensions()); err != nil {
		return fmt.Errorf("qdrant collection: %w", err)
	}
	logger.Info("qdrant ready", "collection", cfg.Qdrant.Collection, "dimensions", embedder.Dimensions())

	// The AI providers are checked but not required: Relata's deterministic
	// pipeline, search and browsing all work without them, and refusing to
	// start would make a model restart an outage.
	if h := embedder.Health(ctx); !h.Available {
		logger.Warn("embedding model unavailable at startup; items will not be embedded until it returns",
			"model", h.Model, "detail", h.Detail)
	}
	if cfg.Ollama.AnalysisEnabled {
		if h := analyser.Health(ctx); !h.Available {
			logger.Warn("analysis model unavailable at startup; extraction falls back to deterministic rules",
				"model", h.Model, "detail", h.Detail)
		}
	}

	// --- Services -----------------------------------------------------------
	recorder := audit.NewRecorder(store.Audit)
	analysisSvc := analysis.NewService(analyser, metrics, cfg.Ollama.AnalysisEnabled)
	entitySvc := entities.NewService(store.Entities, store.Aliases, store.Claims, analysisSvc, cfg)
	discoverer := relationships.NewDiscoverer(store.Items, store.Chunks, store.Entities,
		store.Aliases, store.Claims, store.Relationships, vectors, embedder, analysisSvc, cfg)
	ingestSvc := ingest.NewService(store.Items, store.Jobs, cfg, recorder, metrics)
	searchSvc := search.NewService(store.Items, store.Chunks, store.Entities,
		store.Relationships, vectors, embedder, cfg, metrics)
	answerSvc := search.NewAnswerer(searchSvc, analysisSvc, store.Claims, cfg.Search.AnswerEnabled)
	reviewer := relationships.NewReviewer(store.Relationships, store.Items, recorder)
	authSvc := auth.NewService(store.Users, store.Sessions, cfg.Session, cfg.HTTP.SecureCookies, metrics)

	tokenSvc := auth.NewTokenService(store.APITokens, store.Users, auth.TokenConfig{
		DefaultTTL: cfg.Auth.OAuth.TokenDefaultTTL,
		MaxTTL:     cfg.Auth.OAuth.TokenMaxTTL,
	})

	// OAuth is optional. When it is not configured these stay nil and the
	// handlers report the feature as unavailable, rather than the process
	// refusing to start — the break-glass local login lives here too.
	var oauthSvc *auth.OAuthService
	var provisioner *auth.Provisioner
	if cfg.Auth.OAuth.Enabled {
		oauthSvc = auth.NewOAuthService(auth.OAuthConfig{
			Enabled:             true,
			ProviderName:        cfg.Auth.OAuth.ProviderName,
			Issuer:              cfg.Auth.OAuth.Issuer,
			AuthorizeURL:        cfg.Auth.OAuth.AuthorizeURL,
			TokenURL:            cfg.Auth.OAuth.TokenURL,
			UserInfoURL:         cfg.Auth.OAuth.UserInfoURL,
			JWKSURL:             cfg.Auth.OAuth.JWKSURL,
			ClientID:            cfg.Auth.OAuth.ClientID,
			ClientSecret:        cfg.Auth.OAuth.ClientSecret,
			RedirectURI:         cfg.RedirectURI(),
			Scopes:              cfg.Auth.OAuth.Scopes,
			AllowedEmailDomains: cfg.Auth.OAuth.AllowedEmailDomains,
			Timeout:             cfg.Auth.OAuth.Timeout,
		})
		provisioner = auth.NewProvisioner(authSvc, store.Users, store.Workspaces)
		logger.Info("federated sign-in enabled",
			"provider", cfg.Auth.OAuth.ProviderName,
			"redirect_uri", cfg.RedirectURI(),
			"local_login_enabled", cfg.Auth.LocalLoginEnabled)
	} else {
		logger.Warn("federated sign-in is not configured; local accounts are the only way in",
			"local_login_enabled", cfg.Auth.LocalLoginEnabled)
	}

	pool := jobs.NewPool(store.Jobs, cfg.Workers, metrics)
	pipeline := ingest.NewPipeline(ingest.Repos{
		Items: store.Items, Chunks: store.Chunks, Entities: store.Entities,
		Aliases: store.Aliases, Claims: store.Claims, Rels: store.Relationships,
		Jobs: store.Jobs, Runs: store.Runs, Models: store.ModelVersions,
	}, vectors, embedder, analysisSvc, entitySvc, discoverer, ingestSvc, cfg, pool, recorder)
	pipeline.Register(pool)

	// --- HTTP ---------------------------------------------------------------
	renderer, err := web.NewRenderer()
	if err != nil {
		return fmt.Errorf("templates: %w", err)
	}

	webHandlers := web.NewHandlers(web.Deps{
		Cfg: cfg, Renderer: renderer, Auth: authSvc, Version: version,
		OAuth: oauthSvc, Provisioner: provisioner, Tokens: tokenSvc,
		Items: store.Items, Chunks: store.Chunks, Entities: store.Entities,
		Aliases: store.Aliases, Claims: store.Claims, Rels: store.Relationships,
		Jobs: store.Jobs, Runs: store.Runs, Models: store.ModelVersions,
		Users: store.Users, Vectors: vectors,
		Ingest: ingestSvc, Pipeline: pipeline, Search: searchSvc, Answer: answerSvc,
		Reviewer: reviewer, Analysis: analysisSvc, Audit: recorder,
		Embedder: embedder, EmbedHealth: embedder, AnalyseHealth: analyser,
	})

	apiHandlers := api.NewHandlers(api.Deps{
		Cfg:   cfg,
		Items: store.Items, Chunks: store.Chunks, Entities: store.Entities,
		Aliases: store.Aliases, Claims: store.Claims, Rels: store.Relationships,
		Jobs: store.Jobs, Runs: store.Runs, Models: store.ModelVersions, Vectors: vectors,
		Ingest: ingestSvc, Pipeline: pipeline, Search: searchSvc, Answer: answerSvc,
		Reviewer: reviewer, Analysis: analysisSvc, Audit: recorder, Embedder: embedder,
		Tokens: tokenSvc,
	})

	health := &api.HealthChecker{
		Mongo: store.Ping, Vectors: vectors,
		Embedder: embedder, Analyser: analyser,
		Version: version, Started: time.Now(),
	}

	router := api.NewRouter(api.RouterConfig{
		Cfg: cfg, Auth: authSvc, Tokens: tokenSvc, API: apiHandlers, Web: webHandlers,
		Health: health, Metrics: metrics,
		Static: http.FileServerFS(web.StaticFS()),
	})

	srv := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           withLogger(router, logger),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
	}

	// --- Start --------------------------------------------------------------
	workerCtx, stopWorkers := context.WithCancel(ctx)
	defer stopWorkers()
	pool.Start(workerCtx)

	// Sweep expired sessions periodically, so revoked and stale sessions do not
	// accumulate even if the TTL index is unavailable.
	go sweepSessions(workerCtx, authSvc, logger)

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening",
			"addr", cfg.HTTP.Addr, "base_url", cfg.HTTP.BaseURL,
			"external_bind_allowed", cfg.HTTP.AllowExternalBind)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		logger.Info("shutdown signal received; draining")
	}

	// --- Shut down ----------------------------------------------------------
	//
	// The HTTP server drains first so no new work is accepted, then the workers
	// are given time to finish what they hold. A job that does not finish keeps
	// its lease, which expires and returns it to the queue.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http server did not shut down cleanly", "error", err.Error())
	}
	stopWorkers()
	pool.Stop(cfg.HTTP.ShutdownTimeout)

	logger.Info("relata stopped")
	return nil
}

// sweepSessions periodically deletes expired sessions.
func sweepSessions(ctx context.Context, a *auth.Service, logger loggerType) {
	t := time.NewTicker(1 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := a.SweepExpired(ctx); err != nil {
				logger.Debug("session sweep failed", "error", err.Error())
			} else if n > 0 {
				logger.Info("removed expired sessions", "count", n)
			}
		}
	}
}

// withLogger attaches the application logger to every request context.
func withLogger(next http.Handler, logger loggerType) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(observability.WithLogger(r.Context(), logger)))
	})
}

// ensureWorkspace returns the default workspace, creating it if absent.
//
// Relata is single-workspace in practice today, but every record carries a
// workspace ID and every query filters on it, so adding more later is a matter
// of creating them rather than migrating the schema.
func ensureWorkspace(ctx context.Context, store *mongo.Store) (*domain.Workspace, error) {
	const defaultSlug = "default"

	if ws, err := store.Workspaces.GetBySlug(ctx, defaultSlug); err == nil {
		return ws, nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}

	ws := &domain.Workspace{
		ID:        newID(),
		Name:      "Default workspace",
		Slug:      defaultSlug,
		CreatedAt: time.Now().UTC(),
	}
	if err := store.Workspaces.Create(ctx, ws); err != nil {
		// A concurrent process may have created it first.
		if existing, gerr := store.Workspaces.GetBySlug(ctx, defaultSlug); gerr == nil {
			return existing, nil
		}
		return nil, err
	}
	return ws, nil
}
