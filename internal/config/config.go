// Package config loads and validates Relata's runtime configuration from
// environment variables, optionally seeded from a key=value file.
//
// Configuration is validated once at startup and the process refuses to start
// with a clear message when a required value is missing or nonsensical, rather
// than failing later inside a request.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the fully-resolved application configuration.
type Config struct {
	Env      string
	LogLevel string
	LogJSON  bool

	HTTP     HTTPConfig
	Mongo    MongoConfig
	Qdrant   QdrantConfig
	Ollama   OllamaConfig
	Uploads  UploadConfig
	Chunking ChunkConfig
	Workers  WorkerConfig
	Search   SearchConfig
	Rel      RelationshipConfig
	Session  SessionConfig
	Auth     AuthConfig
}

// AuthConfig controls how people prove who they are.
type AuthConfig struct {
	// LocalLoginEnabled allows username-and-password sign-in. Off by default:
	// it exists to recover from a broken identity-provider configuration, not
	// as a parallel front door.
	LocalLoginEnabled bool

	OAuth OAuthConfig
}

// OAuthConfig describes the identity provider. Endpoints are operator-supplied
// and therefore trusted; they are still required to be HTTPS outside
// development, because a plaintext token exchange would publish the code.
type OAuthConfig struct {
	Enabled      bool
	ProviderName string

	// Issuer enables OIDC discovery and ID-token validation. When set, the
	// endpoint URLs below are fetched from it unless explicitly overridden.
	Issuer string

	AuthorizeURL string
	TokenURL     string
	UserInfoURL  string
	JWKSURL      string

	ClientID     string
	ClientSecret string
	Scopes       []string

	// AllowedEmailDomains optionally restricts who may sign in. Empty means any
	// identity the provider vouches for is accepted.
	AllowedEmailDomains []string

	// Timeout bounds every call to the provider.
	Timeout time.Duration

	// TokenDefaultTTL and TokenMaxTTL bound how long an API token may live.
	TokenDefaultTTL time.Duration
	TokenMaxTTL     time.Duration
}

// CallbackPath is the fixed redirect URI path registered with the provider.
// It is a constant rather than configuration so that what is registered and
// what is served cannot drift apart.
const CallbackPath = "/auth/oauth/callback"

// RedirectURI is the full redirect URI to register with the provider.
func (c *Config) RedirectURI() string {
	return strings.TrimRight(c.HTTP.BaseURL, "/") + CallbackPath
}

// HTTPConfig controls the web server.
type HTTPConfig struct {
	// Addr defaults to 127.0.0.1:8080. Listening on a non-loopback interface
	// requires AllowExternalBind to be set explicitly.
	Addr              string
	AllowExternalBind bool
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	// MaxRequestBytes bounds non-upload request bodies.
	MaxRequestBytes int64
	// TrustProxyHeaders enables X-Forwarded-For parsing. Off by default because
	// a spoofed header would defeat login rate limiting.
	TrustProxyHeaders bool
	// SecureCookies forces the Secure cookie attribute. Auto-enabled when
	// BaseURL is https.
	SecureCookies  bool
	BaseURL        string
	MetricsEnabled bool
}

// MongoConfig points at MongoDB.
type MongoConfig struct {
	URI      string
	Database string
	Timeout  time.Duration
}

// QdrantConfig points at Qdrant's REST API.
type QdrantConfig struct {
	URL        string
	APIKey     string
	Collection string
	Timeout    time.Duration
}

// OllamaConfig points at the default AI provider.
type OllamaConfig struct {
	BaseURL string
	// EmbeddingModel and AnalysisModel are stamped onto every derived record.
	EmbeddingModel  string
	AnalysisModel   string
	EmbeddingDims   int
	EmbedTimeout    time.Duration
	AnalysisTimeout time.Duration
	EmbedBatchSize  int
	MaxRetries      int
	RetryBaseDelay  time.Duration
	RetryMaxDelay   time.Duration
	MaxOutputTokens int
	// AnalysisEnabled turns off all model-assisted stages when false. The
	// deterministic pipeline still runs, so Relata is usable without an LLM.
	AnalysisEnabled bool
}

// UploadConfig bounds ingestion inputs.
type UploadConfig struct {
	Dir string
	// MaxUploadBytes is the hard ceiling on an uploaded file.
	MaxUploadBytes int64
	// MaxExtractedTextBytes truncates parser output; the original is kept intact.
	MaxExtractedTextBytes int
	MaxJSONDepth          int
	MaxMetadataKeys       int
	MaxMetadataValueLen   int
	MaxTitleLen           int
	ProcessingTimeout     time.Duration
	MaxConcurrentIngest   int
}

// ChunkConfig controls the chunker.
type ChunkConfig struct {
	// MaxChars is the ceiling for one chunk; TargetChars is what the chunker
	// aims for when merging small blocks.
	MaxChars     int
	TargetChars  int
	MinChars     int
	OverlapChars int
	MaxPerItem   int
	// MinSplitChars is the document size below which no splitting happens at all.
	MinSplitChars int
}

// WorkerConfig controls the background job pool.
type WorkerConfig struct {
	Count           int
	AIConcurrency   int
	PollInterval    time.Duration
	LeaseDuration   time.Duration
	HeartbeatPeriod time.Duration
	MaxAttempts     int
	JobTimeout      time.Duration
	Enabled         bool
}

// SearchConfig bounds retrieval.
type SearchConfig struct {
	VectorCandidates       int
	LexicalCandidates      int
	MaxResults             int
	DefaultLimit           int
	ExpansionMinScore      float64
	MaxExpandedItems       int
	AnswerMaxChunks        int
	AnswerMaxCharsPerChunk int
	AnswerEnabled          bool
}

// RelationshipConfig bounds candidate discovery and sets acceptance thresholds.
type RelationshipConfig struct {
	MaxCandidates int
	// MinScore is the strength below which a candidate is discarded entirely.
	MinScore float64
	// AutoAcceptScore is the strength at or above which a candidate is stored
	// as pending review rather than discarded. Nothing is ever auto-confirmed.
	StoreScore float64
	// MaxModelCalls bounds LLM classification calls per discovery run.
	MaxModelCalls int
	// NearDuplicateSimilarity is the cosine similarity at which two items are
	// flagged as likely near-duplicates. They are never merged automatically.
	NearDuplicateSimilarity float64
	TemporalWindow          time.Duration
	MaxNeighboursPerChunk   int
}

// SessionConfig controls browser sessions and login throttling.
type SessionConfig struct {
	CookieName      string
	IdleTimeout     time.Duration
	AbsoluteTimeout time.Duration
	// LoginRateBurst attempts are allowed per LoginRateWindow per client key
	// before logins are refused.
	LoginRateBurst  int
	LoginRateWindow time.Duration
}

// ValidationError lists everything wrong with the configuration at once, so a
// misconfigured deployment is fixed in one pass rather than several restarts.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return "invalid configuration:\n  - " + strings.Join(e.Problems, "\n  - ")
}

// Load reads configuration from the environment, first applying any key=value
// pairs from the file at path (if it exists and the key is not already set).
func Load(path string) (*Config, error) {
	if path != "" {
		if err := loadFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read config file %s: %w", path, err)
		}
	}

	c := &Config{
		Env:      env("RELATA_ENV", "development"),
		LogLevel: env("RELATA_LOG_LEVEL", "info"),
		LogJSON:  envBool("RELATA_LOG_JSON", false),

		HTTP: HTTPConfig{
			Addr:              env("RELATA_HTTP_ADDR", "127.0.0.1:8080"),
			AllowExternalBind: envBool("RELATA_ALLOW_EXTERNAL_BIND", false),
			ReadTimeout:       envDur("RELATA_HTTP_READ_TIMEOUT", 30*time.Second),
			WriteTimeout:      envDur("RELATA_HTTP_WRITE_TIMEOUT", 360*time.Second),
			IdleTimeout:       envDur("RELATA_HTTP_IDLE_TIMEOUT", 90*time.Second),
			ShutdownTimeout:   envDur("RELATA_HTTP_SHUTDOWN_TIMEOUT", 20*time.Second),
			MaxRequestBytes:   envInt64("RELATA_MAX_REQUEST_BYTES", 1<<20),
			TrustProxyHeaders: envBool("RELATA_TRUST_PROXY_HEADERS", false),
			SecureCookies:     envBool("RELATA_SECURE_COOKIES", false),
			BaseURL:           env("RELATA_BASE_URL", "http://127.0.0.1:8080"),
			MetricsEnabled:    envBool("RELATA_METRICS_ENABLED", true),
		},
		Mongo: MongoConfig{
			URI:      env("RELATA_MONGO_URI", "mongodb://127.0.0.1:27017"),
			Database: env("RELATA_MONGO_DATABASE", "relata"),
			Timeout:  envDur("RELATA_MONGO_TIMEOUT", 10*time.Second),
		},
		Qdrant: QdrantConfig{
			URL:        env("RELATA_QDRANT_URL", "http://127.0.0.1:6333"),
			APIKey:     env("RELATA_QDRANT_API_KEY", ""),
			Collection: env("RELATA_QDRANT_COLLECTION", "relata_chunks"),
			Timeout:    envDur("RELATA_QDRANT_TIMEOUT", 15*time.Second),
		},
		Ollama: OllamaConfig{
			BaseURL:         env("RELATA_OLLAMA_BASE_URL", "http://127.0.0.1:11434"),
			EmbeddingModel:  env("RELATA_EMBEDDING_MODEL", "nomic-embed-text"),
			AnalysisModel:   env("RELATA_ANALYSIS_MODEL", "llama3.1:8b"),
			EmbeddingDims:   envInt("RELATA_EMBEDDING_DIMENSIONS", 768),
			EmbedTimeout:    envDur("RELATA_EMBED_TIMEOUT", 60*time.Second),
			AnalysisTimeout: envDur("RELATA_ANALYSIS_TIMEOUT", 300*time.Second),
			EmbedBatchSize:  envInt("RELATA_EMBED_BATCH_SIZE", 16),
			MaxRetries:      envInt("RELATA_AI_MAX_RETRIES", 3),
			RetryBaseDelay:  envDur("RELATA_AI_RETRY_BASE_DELAY", 500*time.Millisecond),
			RetryMaxDelay:   envDur("RELATA_AI_RETRY_MAX_DELAY", 8*time.Second),
			MaxOutputTokens: envInt("RELATA_AI_MAX_OUTPUT_TOKENS", 2048),
			AnalysisEnabled: envBool("RELATA_ANALYSIS_ENABLED", true),
		},
		Uploads: UploadConfig{
			Dir:                   env("RELATA_UPLOAD_DIR", "./data/uploads"),
			MaxUploadBytes:        envInt64("RELATA_MAX_UPLOAD_BYTES", 25<<20),
			MaxExtractedTextBytes: envInt("RELATA_MAX_EXTRACTED_TEXT_BYTES", 2<<20),
			MaxJSONDepth:          envInt("RELATA_MAX_JSON_DEPTH", 20),
			MaxMetadataKeys:       envInt("RELATA_MAX_METADATA_KEYS", 40),
			MaxMetadataValueLen:   envInt("RELATA_MAX_METADATA_VALUE_LEN", 2000),
			MaxTitleLen:           envInt("RELATA_MAX_TITLE_LEN", 300),
			ProcessingTimeout:     envDur("RELATA_PROCESSING_TIMEOUT", 10*time.Minute),
			MaxConcurrentIngest:   envInt("RELATA_MAX_CONCURRENT_INGEST", 4),
		},
		Chunking: ChunkConfig{
			MaxChars:      envInt("RELATA_CHUNK_MAX_CHARS", 1800),
			TargetChars:   envInt("RELATA_CHUNK_TARGET_CHARS", 1100),
			MinChars:      envInt("RELATA_CHUNK_MIN_CHARS", 200),
			OverlapChars:  envInt("RELATA_CHUNK_OVERLAP_CHARS", 120),
			MaxPerItem:    envInt("RELATA_MAX_CHUNKS_PER_ITEM", 400),
			MinSplitChars: envInt("RELATA_CHUNK_MIN_SPLIT_CHARS", 900),
		},
		Workers: WorkerConfig{
			Count:           envInt("RELATA_WORKER_COUNT", 3),
			AIConcurrency:   envInt("RELATA_AI_CONCURRENCY", 2),
			PollInterval:    envDur("RELATA_WORKER_POLL_INTERVAL", 1*time.Second),
			LeaseDuration:   envDur("RELATA_JOB_LEASE_DURATION", 2*time.Minute),
			HeartbeatPeriod: envDur("RELATA_JOB_HEARTBEAT", 30*time.Second),
			MaxAttempts:     envInt("RELATA_JOB_MAX_ATTEMPTS", 4),
			JobTimeout:      envDur("RELATA_JOB_TIMEOUT", 8*time.Minute),
			Enabled:         envBool("RELATA_WORKERS_ENABLED", true),
		},
		Search: SearchConfig{
			VectorCandidates:       envInt("RELATA_SEARCH_VECTOR_CANDIDATES", 60),
			LexicalCandidates:      envInt("RELATA_SEARCH_LEXICAL_CANDIDATES", 60),
			MaxResults:             envInt("RELATA_SEARCH_MAX_RESULTS", 100),
			DefaultLimit:           envInt("RELATA_SEARCH_DEFAULT_LIMIT", 20),
			ExpansionMinScore:      envFloat("RELATA_SEARCH_EXPANSION_MIN_SCORE", 0.45),
			MaxExpandedItems:       envInt("RELATA_SEARCH_MAX_EXPANDED_ITEMS", 20),
			AnswerMaxChunks:        envInt("RELATA_ANSWER_MAX_CHUNKS", 6),
			AnswerMaxCharsPerChunk: envInt("RELATA_ANSWER_MAX_CHARS_PER_CHUNK", 1200),
			AnswerEnabled:          envBool("RELATA_ANSWER_ENABLED", true),
		},
		Rel: RelationshipConfig{
			MaxCandidates:           envInt("RELATA_REL_MAX_CANDIDATES", 40),
			MinScore:                envFloat("RELATA_REL_MIN_SCORE", 0.18),
			StoreScore:              envFloat("RELATA_REL_STORE_SCORE", 0.25),
			MaxModelCalls:           envInt("RELATA_REL_MAX_MODEL_CALLS", 8),
			NearDuplicateSimilarity: envFloat("RELATA_NEAR_DUPLICATE_SIMILARITY", 0.94),
			TemporalWindow:          envDur("RELATA_REL_TEMPORAL_WINDOW", 168*time.Hour),
			MaxNeighboursPerChunk:   envInt("RELATA_REL_MAX_NEIGHBOURS_PER_CHUNK", 10),
		},
		Session: SessionConfig{
			CookieName:      env("RELATA_SESSION_COOKIE", "relata_session"),
			IdleTimeout:     envDur("RELATA_SESSION_IDLE_TIMEOUT", 12*time.Hour),
			AbsoluteTimeout: envDur("RELATA_SESSION_ABSOLUTE_TIMEOUT", 168*time.Hour),
			LoginRateBurst:  envInt("RELATA_LOGIN_RATE_BURST", 8),
			LoginRateWindow: envDur("RELATA_LOGIN_RATE_WINDOW", 5*time.Minute),
		},
		Auth: AuthConfig{
			// Resolved below: the default depends on whether OAuth is configured.
			LocalLoginEnabled: envBool("RELATA_LOCAL_LOGIN_ENABLED", false),
			OAuth: OAuthConfig{
				Enabled:             envBool("RELATA_OAUTH_ENABLED", false),
				ProviderName:        env("RELATA_OAUTH_PROVIDER_NAME", "Blackwall"),
				Issuer:              strings.TrimRight(env("RELATA_OAUTH_ISSUER", ""), "/"),
				AuthorizeURL:        env("RELATA_OAUTH_AUTHORIZE_URL", ""),
				TokenURL:            env("RELATA_OAUTH_TOKEN_URL", ""),
				UserInfoURL:         env("RELATA_OAUTH_USERINFO_URL", ""),
				JWKSURL:             env("RELATA_OAUTH_JWKS_URL", ""),
				ClientID:            env("RELATA_OAUTH_CLIENT_ID", ""),
				ClientSecret:        env("RELATA_OAUTH_CLIENT_SECRET", ""),
				Scopes:              envList("RELATA_OAUTH_SCOPES", []string{"openid", "profile", "email"}),
				AllowedEmailDomains: envList("RELATA_OAUTH_ALLOWED_EMAIL_DOMAINS", nil),
				Timeout:             envDur("RELATA_OAUTH_TIMEOUT", 15*time.Second),
				TokenDefaultTTL:     envDur("RELATA_API_TOKEN_DEFAULT_TTL", 90*24*time.Hour),
				TokenMaxTTL:         envDur("RELATA_API_TOKEN_MAX_TTL", 365*24*time.Hour),
			},
		},
	}

	// Local login defaults to on while there is no identity provider, and to off
	// once there is one. An installation with nothing configured must still have
	// a way in, and an installation behind an identity provider should not keep
	// a second front door open by accident. Setting the variable explicitly
	// always wins over both.
	if _, explicit := os.LookupEnv("RELATA_LOCAL_LOGIN_ENABLED"); !explicit {
		c.Auth.LocalLoginEnabled = !c.Auth.OAuth.Enabled
	}

	if strings.HasPrefix(strings.ToLower(c.HTTP.BaseURL), "https://") {
		c.HTTP.SecureCookies = true
	}
	if abs, err := filepath.Abs(c.Uploads.Dir); err == nil {
		c.Uploads.Dir = abs
	}

	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Validate reports every configuration problem found.
func (c *Config) Validate() error {
	var p []string

	req := func(v, name string) {
		if strings.TrimSpace(v) == "" {
			p = append(p, name+" must not be empty")
		}
	}
	pos := func(v int, name string) {
		if v <= 0 {
			p = append(p, fmt.Sprintf("%s must be greater than zero (got %d)", name, v))
		}
	}

	req(c.Mongo.URI, "RELATA_MONGO_URI")
	req(c.Mongo.Database, "RELATA_MONGO_DATABASE")
	req(c.Qdrant.URL, "RELATA_QDRANT_URL")
	req(c.Qdrant.Collection, "RELATA_QDRANT_COLLECTION")
	req(c.Ollama.BaseURL, "RELATA_OLLAMA_BASE_URL")
	req(c.Ollama.EmbeddingModel, "RELATA_EMBEDDING_MODEL")
	req(c.Uploads.Dir, "RELATA_UPLOAD_DIR")
	req(c.Session.CookieName, "RELATA_SESSION_COOKIE")

	p = append(p, c.validateAuth()...)

	if !strings.HasPrefix(c.Mongo.URI, "mongodb://") && !strings.HasPrefix(c.Mongo.URI, "mongodb+srv://") {
		p = append(p, "RELATA_MONGO_URI must start with mongodb:// or mongodb+srv://")
	}
	if !strings.HasPrefix(c.Qdrant.URL, "http://") && !strings.HasPrefix(c.Qdrant.URL, "https://") {
		p = append(p, "RELATA_QDRANT_URL must start with http:// or https://")
	}
	if !strings.HasPrefix(c.Ollama.BaseURL, "http://") && !strings.HasPrefix(c.Ollama.BaseURL, "https://") {
		p = append(p, "RELATA_OLLAMA_BASE_URL must start with http:// or https://")
	}
	if c.Ollama.AnalysisEnabled && strings.TrimSpace(c.Ollama.AnalysisModel) == "" {
		p = append(p, "RELATA_ANALYSIS_MODEL must be set when RELATA_ANALYSIS_ENABLED is true")
	}

	// Refuse to listen on a non-loopback interface without an explicit opt-in.
	if host, _, err := splitHostPort(c.HTTP.Addr); err != nil {
		p = append(p, "RELATA_HTTP_ADDR must be host:port: "+err.Error())
	} else if !isLoopback(host) && !c.HTTP.AllowExternalBind {
		p = append(p, fmt.Sprintf(
			"RELATA_HTTP_ADDR binds to %q which is not loopback; set RELATA_ALLOW_EXTERNAL_BIND=true to confirm this is intended", host))
	}

	pos(c.Ollama.EmbeddingDims, "RELATA_EMBEDDING_DIMENSIONS")
	pos(c.Ollama.EmbedBatchSize, "RELATA_EMBED_BATCH_SIZE")
	pos(c.Workers.Count, "RELATA_WORKER_COUNT")
	pos(c.Workers.AIConcurrency, "RELATA_AI_CONCURRENCY")
	pos(c.Workers.MaxAttempts, "RELATA_JOB_MAX_ATTEMPTS")
	pos(c.Uploads.MaxConcurrentIngest, "RELATA_MAX_CONCURRENT_INGEST")
	pos(c.Chunking.MaxPerItem, "RELATA_MAX_CHUNKS_PER_ITEM")
	pos(c.Search.DefaultLimit, "RELATA_SEARCH_DEFAULT_LIMIT")

	if c.Ollama.MaxRetries < 0 || c.Ollama.MaxRetries > 10 {
		p = append(p, "RELATA_AI_MAX_RETRIES must be between 0 and 10 (retries are bounded by design)")
	}
	if c.Uploads.MaxUploadBytes <= 0 {
		p = append(p, "RELATA_MAX_UPLOAD_BYTES must be greater than zero")
	}
	if c.Uploads.MaxExtractedTextBytes <= 0 {
		p = append(p, "RELATA_MAX_EXTRACTED_TEXT_BYTES must be greater than zero")
	}
	if c.Uploads.MaxJSONDepth <= 0 || c.Uploads.MaxJSONDepth > 200 {
		p = append(p, "RELATA_MAX_JSON_DEPTH must be between 1 and 200")
	}
	if c.Chunking.MaxChars <= c.Chunking.MinChars {
		p = append(p, "RELATA_CHUNK_MAX_CHARS must exceed RELATA_CHUNK_MIN_CHARS")
	}
	if c.Chunking.TargetChars > c.Chunking.MaxChars {
		p = append(p, "RELATA_CHUNK_TARGET_CHARS must not exceed RELATA_CHUNK_MAX_CHARS")
	}
	if c.Chunking.OverlapChars < 0 || c.Chunking.OverlapChars >= c.Chunking.MaxChars {
		p = append(p, "RELATA_CHUNK_OVERLAP_CHARS must be >= 0 and less than RELATA_CHUNK_MAX_CHARS")
	}
	if c.Rel.MinScore < 0 || c.Rel.MinScore > 1 {
		p = append(p, "RELATA_REL_MIN_SCORE must be between 0 and 1")
	}
	if c.Rel.StoreScore < c.Rel.MinScore {
		p = append(p, "RELATA_REL_STORE_SCORE must be >= RELATA_REL_MIN_SCORE")
	}
	if c.Rel.NearDuplicateSimilarity <= 0 || c.Rel.NearDuplicateSimilarity > 1 {
		p = append(p, "RELATA_NEAR_DUPLICATE_SIMILARITY must be between 0 and 1")
	}
	if c.Session.IdleTimeout <= 0 || c.Session.AbsoluteTimeout < c.Session.IdleTimeout {
		p = append(p, "RELATA_SESSION_ABSOLUTE_TIMEOUT must be >= RELATA_SESSION_IDLE_TIMEOUT and both positive")
	}
	// An answer is generated inside a request, so the HTTP write timeout must
	// outlast the model call. Getting this the wrong way round closes the
	// connection while the model is still working and the user sees nothing at
	// all — a silent failure rather than a slow success.
	if c.Search.AnswerEnabled && c.Ollama.AnalysisEnabled &&
		c.HTTP.WriteTimeout <= c.Ollama.AnalysisTimeout {
		p = append(p, fmt.Sprintf(
			"RELATA_HTTP_WRITE_TIMEOUT (%s) must exceed RELATA_ANALYSIS_TIMEOUT (%s), "+
				"or answer generation will be cut off mid-response",
			c.HTTP.WriteTimeout, c.Ollama.AnalysisTimeout))
	}
	if c.Workers.HeartbeatPeriod >= c.Workers.LeaseDuration {
		p = append(p, "RELATA_JOB_HEARTBEAT must be shorter than RELATA_JOB_LEASE_DURATION, or leases will expire mid-job")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		p = append(p, "RELATA_LOG_LEVEL must be one of debug, info, warn, error")
	}

	if len(p) > 0 {
		return &ValidationError{Problems: p}
	}
	return nil
}

// Redacted returns a copy safe to log: secrets are replaced with a marker.
func (c *Config) Redacted() map[string]any {
	secret := func(v string) string {
		if v == "" {
			return ""
		}
		return "[set]"
	}
	return map[string]any{
		"env":              c.Env,
		"http_addr":        c.HTTP.Addr,
		"mongo_database":   c.Mongo.Database,
		"qdrant_url":       c.Qdrant.URL,
		"qdrant_api_key":   secret(c.Qdrant.APIKey),
		"ollama_base_url":  c.Ollama.BaseURL,
		"embedding_model":  c.Ollama.EmbeddingModel,
		"analysis_model":   c.Ollama.AnalysisModel,
		"analysis_enabled": c.Ollama.AnalysisEnabled,
		"workers":          c.Workers.Count,
		"ai_concurrency":   c.Workers.AIConcurrency,
		"upload_dir":       c.Uploads.Dir,
	}
}

func splitHostPort(addr string) (string, string, error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", "", errors.New("missing port")
	}
	host := strings.Trim(addr[:i], "[]")
	port := addr[i+1:]
	if port == "" {
		return "", "", errors.New("missing port")
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", "", errors.New("port is not a number")
	}
	return host, port, nil
}

func isLoopback(host string) bool {
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return strings.HasPrefix(host, "127.")
}

// loadFile applies KEY=VALUE lines from path into the environment without
// overwriting values already present, so real environment variables win.
func loadFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
	return sc.Err()
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

// envList reads a whitespace- or comma-separated list. Both separators are
// accepted because OAuth scopes are conventionally space-separated while
// domain lists are conventionally comma-separated, and an operator should not
// have to remember which is which.
func envList(k string, def []string) []string {
	v, ok := os.LookupEnv(k)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	parts := strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

func envInt(k string, def int) int {
	if v, ok := os.LookupEnv(k); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func envInt64(k string, def int64) int64 {
	if v, ok := os.LookupEnv(k); ok {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v, ok := os.LookupEnv(k); ok {
		if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return n
		}
	}
	return def
}

func envBool(k string, def bool) bool {
	if v, ok := os.LookupEnv(k); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(k); ok {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil {
			return d
		}
	}
	return def
}

// validateAuth checks the authentication configuration as a set. The checks are
// deliberately whole-configuration rather than per-field: every failure here is
// one that would otherwise present as "nobody can sign in" at the worst moment.
func (c *Config) validateAuth() []string {
	var p []string
	o := c.Auth.OAuth

	// The one that matters most: no way in at all.
	if !o.Enabled && !c.Auth.LocalLoginEnabled {
		p = append(p, "no authentication method is enabled: set RELATA_OAUTH_ENABLED=true, "+
			"or RELATA_LOCAL_LOGIN_ENABLED=true, or both — otherwise nobody can sign in")
	}
	if !o.Enabled {
		return p
	}

	if strings.TrimSpace(o.ClientID) == "" {
		p = append(p, "RELATA_OAUTH_CLIENT_ID must not be empty when RELATA_OAUTH_ENABLED=true")
	}
	if strings.TrimSpace(o.ClientSecret) == "" {
		p = append(p, "RELATA_OAUTH_CLIENT_SECRET must not be empty when RELATA_OAUTH_ENABLED=true")
	}

	// Either discovery, or the endpoints spelled out. Half of each is the
	// configuration most likely to look right and fail at the token exchange.
	explicit := strings.TrimSpace(o.AuthorizeURL) != "" && strings.TrimSpace(o.TokenURL) != ""
	if strings.TrimSpace(o.Issuer) == "" && !explicit {
		p = append(p, "RELATA_OAUTH_ENABLED=true needs either RELATA_OAUTH_ISSUER (for OIDC "+
			"discovery) or both RELATA_OAUTH_AUTHORIZE_URL and RELATA_OAUTH_TOKEN_URL")
	}
	if explicit && strings.TrimSpace(o.Issuer) == "" &&
		strings.TrimSpace(o.UserInfoURL) == "" {
		p = append(p, "RELATA_OAUTH_USERINFO_URL is required when no RELATA_OAUTH_ISSUER is set: "+
			"without discovery there is no ID token to read an identity from")
	}

	// The token exchange carries the authorization code and the client secret.
	// Outside development that must not travel in clear.
	if c.Env != "development" {
		for _, u := range []struct{ v, name string }{
			{o.Issuer, "RELATA_OAUTH_ISSUER"},
			{o.AuthorizeURL, "RELATA_OAUTH_AUTHORIZE_URL"},
			{o.TokenURL, "RELATA_OAUTH_TOKEN_URL"},
			{o.UserInfoURL, "RELATA_OAUTH_USERINFO_URL"},
			{o.JWKSURL, "RELATA_OAUTH_JWKS_URL"},
		} {
			if v := strings.TrimSpace(u.v); v != "" && !strings.HasPrefix(strings.ToLower(v), "https://") {
				p = append(p, u.name+" must be https outside development: the token exchange "+
					"carries the authorization code and the client secret")
			}
		}
	}

	// The redirect URI is derived from BaseURL, so a wrong BaseURL sends people
	// somewhere the provider will refuse.
	if strings.TrimSpace(c.HTTP.BaseURL) == "" {
		p = append(p, "RELATA_BASE_URL must be set when OAuth is enabled: the redirect URI "+
			"registered with the provider is derived from it")
	}

	if o.TokenMaxTTL > 0 && o.TokenDefaultTTL > o.TokenMaxTTL {
		p = append(p, fmt.Sprintf("RELATA_API_TOKEN_DEFAULT_TTL (%s) must not exceed "+
			"RELATA_API_TOKEN_MAX_TTL (%s)", o.TokenDefaultTTL, o.TokenMaxTTL))
	}
	return p
}
