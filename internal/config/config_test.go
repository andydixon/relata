package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clearEnv removes every RELATA_ variable so a test starts from defaults.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "RELATA_") {
			t.Setenv(k, "")
			os.Unsetenv(k)
		}
	}
}

func TestDefaultsAreSafe(t *testing.T) {
	clearEnv(t)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("the defaults do not pass validation: %v", err)
	}

	// The single most important default: not reachable from the network.
	if cfg.HTTP.Addr != "127.0.0.1:8080" {
		t.Errorf("default listen address = %q, want loopback", cfg.HTTP.Addr)
	}
	if cfg.HTTP.AllowExternalBind {
		t.Error("external binding is permitted by default")
	}
	if cfg.HTTP.TrustProxyHeaders {
		t.Error("proxy headers are trusted by default, which would let anyone defeat rate limiting")
	}
	if cfg.Workers.MaxAttempts <= 0 || cfg.Ollama.MaxRetries < 0 {
		t.Error("retries are unbounded by default")
	}
}

// Binding to a non-loopback interface must be a deliberate act.
func TestExternalBindRequiresExplicitOptIn(t *testing.T) {
	clearEnv(t)
	t.Setenv("RELATA_HTTP_ADDR", "0.0.0.0:8080")

	_, err := Load("")
	if err == nil {
		t.Fatal("binding to 0.0.0.0 was accepted without an explicit opt-in")
	}
	if !strings.Contains(err.Error(), "RELATA_ALLOW_EXTERNAL_BIND") {
		t.Fatalf("the error does not name the setting that permits it: %v", err)
	}

	t.Setenv("RELATA_ALLOW_EXTERNAL_BIND", "true")
	if _, err := Load(""); err != nil {
		t.Fatalf("an explicit opt-in was still refused: %v", err)
	}
}

func TestLoopbackVariantsAreAccepted(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080", "127.0.0.5:9000"} {
		clearEnv(t)
		t.Setenv("RELATA_HTTP_ADDR", addr)
		if _, err := Load(""); err != nil {
			t.Errorf("loopback address %q was refused: %v", addr, err)
		}
	}
}

// An HTTPS base URL must imply Secure cookies; forgetting that would send the
// session cookie over plain HTTP.
func TestHTTPSBaseURLEnablesSecureCookies(t *testing.T) {
	clearEnv(t)
	t.Setenv("RELATA_BASE_URL", "https://relata.example.org")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HTTP.SecureCookies {
		t.Fatal("an https base URL did not enable Secure cookies")
	}
}

func TestValidationReportsEveryProblemAtOnce(t *testing.T) {
	clearEnv(t)
	t.Setenv("RELATA_MONGO_URI", "not-a-mongo-uri")
	t.Setenv("RELATA_QDRANT_URL", "ftp://wrong")
	t.Setenv("RELATA_OLLAMA_BASE_URL", "nonsense")
	t.Setenv("RELATA_LOG_LEVEL", "verbose")

	_, err := Load("")
	if err == nil {
		t.Fatal("an invalid configuration was accepted")
	}

	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("wrong error type: %T", err)
	}
	// Reporting one problem at a time would mean several restarts to fix.
	if len(ve.Problems) < 4 {
		t.Fatalf("only %d problems reported, expected all four:\n%v", len(ve.Problems), ve.Problems)
	}
	for _, want := range []string{"RELATA_MONGO_URI", "RELATA_QDRANT_URL", "RELATA_OLLAMA_BASE_URL", "RELATA_LOG_LEVEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %s", want)
		}
	}
}

func TestNonsensicalLimitsAreRejected(t *testing.T) {
	cases := map[string]map[string]string{
		"zero workers":           {"RELATA_WORKER_COUNT": "0"},
		"zero AI concurrency":    {"RELATA_AI_CONCURRENCY": "0"},
		"negative upload limit":  {"RELATA_MAX_UPLOAD_BYTES": "-1"},
		"absurd JSON depth":      {"RELATA_MAX_JSON_DEPTH": "5000"},
		"chunk min above max":    {"RELATA_CHUNK_MAX_CHARS": "100", "RELATA_CHUNK_MIN_CHARS": "500"},
		"overlap exceeds chunk":  {"RELATA_CHUNK_OVERLAP_CHARS": "99999"},
		"store below min score":  {"RELATA_REL_MIN_SCORE": "0.8", "RELATA_REL_STORE_SCORE": "0.2"},
		"score outside 0..1":     {"RELATA_REL_MIN_SCORE": "5"},
		"absolute below idle":    {"RELATA_SESSION_IDLE_TIMEOUT": "48h", "RELATA_SESSION_ABSOLUTE_TIMEOUT": "1h"},
		"too many retries":       {"RELATA_AI_MAX_RETRIES": "50"},
		"near-duplicate above 1": {"RELATA_NEAR_DUPLICATE_SIMILARITY": "1.5"},
	}

	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			clearEnv(t)
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := Load(""); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

// A heartbeat at or beyond the lease duration means a running job loses its
// lease and is reclaimed while still working.
func TestHeartbeatMustBeShorterThanTheLease(t *testing.T) {
	clearEnv(t)
	t.Setenv("RELATA_JOB_LEASE_DURATION", "30s")
	t.Setenv("RELATA_JOB_HEARTBEAT", "60s")

	_, err := Load("")
	if err == nil {
		t.Fatal("a heartbeat longer than the lease was accepted")
	}
	if !strings.Contains(err.Error(), "leases will expire mid-job") {
		t.Fatalf("the error does not explain the consequence: %v", err)
	}
}

func TestAnalysisModelRequiredOnlyWhenAnalysisEnabled(t *testing.T) {
	clearEnv(t)

	// A whitespace-only environment value falls back to the default model, so
	// the rule is exercised against a Config built directly.
	base, err := Load("")
	if err != nil {
		t.Fatal(err)
	}

	withModel := *base
	withModel.Ollama.AnalysisModel = ""
	withModel.Ollama.AnalysisEnabled = true
	if err := withModel.Validate(); err == nil {
		t.Fatal("analysis was enabled with no model configured")
	}

	disabled := *base
	disabled.Ollama.AnalysisModel = ""
	disabled.Ollama.AnalysisEnabled = false
	if err := disabled.Validate(); err != nil {
		t.Fatalf("a missing analysis model was refused even with analysis disabled: %v", err)
	}
}

// Whitespace-only values fall back to the default rather than becoming empty,
// so a stray space in .env cannot silently unset a required setting.
func TestWhitespaceValuesFallBackToDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("RELATA_EMBEDDING_MODEL", "   ")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ollama.EmbeddingModel != "nomic-embed-text" {
		t.Fatalf("embedding model = %q, want the default", cfg.Ollama.EmbeddingModel)
	}
}

// --- Configuration file ----------------------------------------------------

func TestConfigFileIsRead(t *testing.T) {
	clearEnv(t)

	path := filepath.Join(t.TempDir(), "test.env")
	content := `# A comment
RELATA_LOG_LEVEL=debug
RELATA_WORKER_COUNT=7
RELATA_QDRANT_COLLECTION="quoted_value"
RELATA_MONGO_DATABASE='single_quoted'

MALFORMED_LINE_WITHOUT_EQUALS
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("log level = %q, want debug", cfg.LogLevel)
	}
	if cfg.Workers.Count != 7 {
		t.Errorf("worker count = %d, want 7", cfg.Workers.Count)
	}
	if cfg.Qdrant.Collection != "quoted_value" {
		t.Errorf("quotes were not stripped: %q", cfg.Qdrant.Collection)
	}
	if cfg.Mongo.Database != "single_quoted" {
		t.Errorf("single quotes were not stripped: %q", cfg.Mongo.Database)
	}
}

// A real environment variable must beat the file, so a deployment can override
// without editing a shipped file.
func TestEnvironmentOverridesTheConfigFile(t *testing.T) {
	clearEnv(t)

	path := filepath.Join(t.TempDir(), "test.env")
	if err := os.WriteFile(path, []byte("RELATA_LOG_LEVEL=debug\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RELATA_LOG_LEVEL", "warn")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "warn" {
		t.Fatalf("log level = %q; the file overrode the environment", cfg.LogLevel)
	}
}

func TestMissingConfigFileIsNotAnError(t *testing.T) {
	clearEnv(t)
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.env")); err != nil {
		t.Fatalf("a missing config file was treated as fatal: %v", err)
	}
}

// --- Redaction -------------------------------------------------------------

func TestRedactedOutputContainsNoSecrets(t *testing.T) {
	clearEnv(t)
	t.Setenv("RELATA_QDRANT_API_KEY", "super-secret-key-value")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}

	red := cfg.Redacted()
	for k, v := range red {
		if s, ok := v.(string); ok && strings.Contains(s, "super-secret") {
			t.Fatalf("field %q leaked the API key: %q", k, s)
		}
	}
	if red["qdrant_api_key"] != "[set]" {
		t.Fatalf("qdrant_api_key = %v, want the marker [set]", red["qdrant_api_key"])
	}
}

func TestRedactedShowsAbsentSecretAsEmpty(t *testing.T) {
	clearEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Redacted()["qdrant_api_key"] != "" {
		t.Fatal("an unset secret is reported as set")
	}
}

// --- Parsing ---------------------------------------------------------------

func TestDurationsAndNumbersParse(t *testing.T) {
	clearEnv(t)
	t.Setenv("RELATA_SESSION_IDLE_TIMEOUT", "90m")
	t.Setenv("RELATA_MAX_UPLOAD_BYTES", "1048576")
	t.Setenv("RELATA_REL_MIN_SCORE", "0.42")
	t.Setenv("RELATA_REL_STORE_SCORE", "0.55") // must not be below the minimum
	t.Setenv("RELATA_METRICS_ENABLED", "false")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Session.IdleTimeout != 90*time.Minute {
		t.Errorf("idle timeout = %v, want 90m", cfg.Session.IdleTimeout)
	}
	if cfg.Uploads.MaxUploadBytes != 1048576 {
		t.Errorf("upload limit = %d", cfg.Uploads.MaxUploadBytes)
	}
	if cfg.Rel.MinScore != 0.42 {
		t.Errorf("min score = %v", cfg.Rel.MinScore)
	}
	if cfg.HTTP.MetricsEnabled {
		t.Error("metrics were not disabled")
	}
}

// An unparseable value falls back to the default rather than to zero, which
// would silently disable a limit.
func TestUnparseableValuesFallBackToDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("RELATA_WORKER_COUNT", "not-a-number")
	t.Setenv("RELATA_SESSION_IDLE_TIMEOUT", "not-a-duration")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workers.Count != 3 {
		t.Errorf("worker count = %d, want the default 3", cfg.Workers.Count)
	}
	if cfg.Session.IdleTimeout != 12*time.Hour {
		t.Errorf("idle timeout = %v, want the default 12h", cfg.Session.IdleTimeout)
	}
}

func TestUploadDirectoryIsMadeAbsolute(t *testing.T) {
	clearEnv(t)
	t.Setenv("RELATA_UPLOAD_DIR", "./relative/path")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(cfg.Uploads.Dir) {
		t.Fatalf("upload directory %q is relative; it must be resolved so the containment check is meaningful",
			cfg.Uploads.Dir)
	}
}

// An answer is generated inside a request, so a write timeout shorter than the
// analysis timeout closes the connection while the model is still working —
// a silent failure rather than a slow success.
func TestWriteTimeoutMustOutlastTheAnalysisTimeout(t *testing.T) {
	clearEnv(t)
	t.Setenv("RELATA_ANALYSIS_TIMEOUT", "300s")
	t.Setenv("RELATA_HTTP_WRITE_TIMEOUT", "60s")

	_, err := Load("")
	if err == nil {
		t.Fatal("a write timeout shorter than the analysis timeout was accepted")
	}
	if !strings.Contains(err.Error(), "cut off mid-response") {
		t.Fatalf("the error does not explain the consequence: %v", err)
	}

	// The rule does not apply when answer generation is off.
	clearEnv(t)
	t.Setenv("RELATA_ANALYSIS_TIMEOUT", "300s")
	t.Setenv("RELATA_HTTP_WRITE_TIMEOUT", "60s")
	t.Setenv("RELATA_ANSWER_ENABLED", "false")
	if _, err := Load(""); err != nil {
		t.Fatalf("the rule was applied with answers disabled: %v", err)
	}
}

// The shipped defaults must be a working configuration, not one that times out
// on the documented default model.
func TestShippedDefaultsSupportAnswerGeneration(t *testing.T) {
	clearEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.WriteTimeout <= cfg.Ollama.AnalysisTimeout {
		t.Fatalf("default write timeout %s does not outlast the analysis timeout %s",
			cfg.HTTP.WriteTimeout, cfg.Ollama.AnalysisTimeout)
	}
	// Measured: llama3.1:8b on CPU handles three passages in ~33s and exceeds
	// 120s at six. The default must leave headroom for the default model.
	if cfg.Search.AnswerMaxChunks > 8 {
		t.Fatalf("default answer passages = %d; too many to complete with the default model on CPU",
			cfg.Search.AnswerMaxChunks)
	}
}
