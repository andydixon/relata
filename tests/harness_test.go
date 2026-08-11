//go:build integration

// Package tests holds Relata's integration and security-regression tests.
//
// They run against real MongoDB and Qdrant instances. Each run uses its own
// database and its own Qdrant collection, both named after the run and dropped
// afterwards, so runs are disposable and cannot interfere with each other or
// with a developer's working data.
//
//	make test-integration
//
// To point the tests at instances you already have:
//
//	RELATA_TEST_MONGO_URI=mongodb://127.0.0.1:27017 \
//	RELATA_TEST_QDRANT_URL=http://127.0.0.1:6333 \
//	go test -tags=integration ./tests/...
package tests

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andydixon/relata/internal/analysis"
	"github.com/andydixon/relata/internal/audit"
	"github.com/andydixon/relata/internal/auth"
	"github.com/andydixon/relata/internal/config"
	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/entities"
	"github.com/andydixon/relata/internal/ids"
	"github.com/andydixon/relata/internal/ingest"
	"github.com/andydixon/relata/internal/jobs"
	"github.com/andydixon/relata/internal/observability"
	"github.com/andydixon/relata/internal/relationships"
	"github.com/andydixon/relata/internal/search"
	"github.com/andydixon/relata/internal/storage/mongo"
	"github.com/andydixon/relata/internal/storage/qdrant"
)

// --- Fake AI providers -----------------------------------------------------

// fakeEmbedder produces deterministic vectors from text, so integration tests
// exercise real storage and retrieval without needing a model.
//
// Vectors are built from token hashes, which makes similarity behave sensibly:
// texts sharing words are closer than texts that do not.
type fakeEmbedder struct {
	dims  int
	calls int
	mu    sync.Mutex
}

func newFakeEmbedder() *fakeEmbedder { return &fakeEmbedder{dims: 64} }

func (f *fakeEmbedder) ModelID() string { return "fake-embed-v1" }
func (f *fakeEmbedder) Dimensions() int { return f.dims }

func (f *fakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	f.calls += len(texts)
	f.mu.Unlock()

	out := make([][]float32, len(texts))
	for i, t := range texts {
		vec := make([]float32, f.dims)
		for _, word := range strings.Fields(strings.ToLower(t)) {
			sum := sha256.Sum256([]byte(word))
			for j := 0; j < 8; j++ {
				idx := int(sum[j]) % f.dims
				vec[idx] += 1
			}
		}
		// Normalise, so cosine similarity is meaningful.
		var norm float64
		for _, v := range vec {
			norm += float64(v) * float64(v)
		}
		norm = math.Sqrt(norm)
		if norm == 0 {
			vec[0] = 1
			norm = 1
		}
		for j := range vec {
			vec[j] = float32(float64(vec[j]) / norm)
		}
		out[i] = vec
	}
	return out, nil
}

// fakeAnalyser returns a canned reply, or an error when disabled.
type fakeAnalyser struct {
	reply string
	err   error
	mu    sync.Mutex
	seen  []string
}

func (f *fakeAnalyser) ModelID() string { return "fake-analysis-v1" }

func (f *fakeAnalyser) Complete(_ context.Context, req domain.CompletionRequest) (*domain.CompletionResponse, error) {
	f.mu.Lock()
	for _, m := range req.Messages {
		f.seen = append(f.seen, m.Content)
	}
	f.mu.Unlock()

	if f.err != nil {
		return nil, f.err
	}
	return &domain.CompletionResponse{Content: f.reply, Model: f.ModelID()}, nil
}

func (f *fakeAnalyser) prompts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

// --- Harness ---------------------------------------------------------------

// harness is a fully-wired Relata with disposable storage.
type harness struct {
	t   *testing.T
	cfg *config.Config

	store   *mongo.Store
	vectors *qdrant.Client

	embedder *fakeEmbedder
	analyser *fakeAnalyser
	analysis *analysis.Service

	ingest   *ingest.Service
	pipeline *ingest.Pipeline
	search   *search.Service
	answer   *search.Answerer
	reviewer *relationships.Reviewer
	auth     *auth.Service
	pool     *jobs.Pool

	workspace *domain.Workspace
	principal *domain.Principal

	cancel context.CancelFunc
}

func mongoURI() string {
	if v := os.Getenv("RELATA_TEST_MONGO_URI"); v != "" {
		return v
	}
	return "mongodb://127.0.0.1:27017"
}

func qdrantURL() string {
	if v := os.Getenv("RELATA_TEST_QDRANT_URL"); v != "" {
		return v
	}
	return "http://127.0.0.1:6333"
}

// requireServices skips the test with a clear message when the backing
// services are not reachable, rather than failing with a connection error.
func requireServices(t *testing.T) {
	t.Helper()

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(qdrantURL() + "/healthz")
	if err != nil {
		t.Skipf("Qdrant is not reachable at %s (start it with `make compose-up`): %v", qdrantURL(), err)
	}
	_ = resp.Body.Close()
}

// newHarness wires a complete Relata against disposable storage.
func newHarness(t *testing.T) *harness {
	t.Helper()
	requireServices(t)

	ctx, cancel := context.WithCancel(context.Background())
	logger := observability.NewLogger("error", false)
	ctx = observability.WithLogger(ctx, logger)

	// A database and collection unique to this run, dropped on cleanup.
	suffix := strings.ReplaceAll(ids.New(), "-", "")[:12]

	cfg := testConfig(t, suffix)

	store, err := mongo.Connect(ctx, cfg.Mongo, nil)
	if err != nil {
		cancel()
		t.Skipf("MongoDB is not reachable at %s (start it with `make compose-up`): %v", mongoURI(), err)
	}
	// Index creation is retried: on a loaded machine MongoDB occasionally
	// resets a connection mid-command, and a flaky harness is worse than a
	// slow one.
	if err := retry(3, 2*time.Second, func() error { return store.EnsureIndexes(ctx) }); err != nil {
		cancel()
		t.Fatalf("create indexes: %v", err)
	}

	vectors := qdrant.New(cfg.Qdrant, nil)
	embedder := newFakeEmbedder()
	if err := retry(3, 2*time.Second, func() error {
		return vectors.EnsureCollection(ctx, embedder.Dimensions())
	}); err != nil {
		cancel()
		t.Fatalf("create qdrant collection: %v", err)
	}

	h := &harness{
		t: t, cfg: cfg, store: store, vectors: vectors,
		embedder: embedder, cancel: cancel,
	}

	// Analysis is off by default: most integration tests exercise the
	// deterministic pipeline, which must work on its own.
	h.analyser = &fakeAnalyser{err: analysis.ErrDisabled}
	h.analysis = analysis.NewService(nil, nil, false)

	h.build(ctx)

	t.Cleanup(func() {
		h.pool.Stop(5 * time.Second)
		cancel()

		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = vectors.DropCollection(cleanupCtx)
		_ = store.Drop(cleanupCtx)
		_ = store.Close(cleanupCtx)
		_ = os.RemoveAll(cfg.Uploads.Dir)
	})

	h.workspace = h.ensureWorkspace(ctx, "default")
	h.principal = h.createUser(ctx, "tester", domain.RoleAdmin, h.workspace.ID)

	h.pool.Start(ctx)
	return h
}

// build wires the services. Called again by enableAnalysis.
func (h *harness) build(ctx context.Context) {
	recorder := audit.NewRecorder(h.store.Audit)
	entitySvc := entities.NewService(h.store.Entities, h.store.Aliases, h.store.Claims, h.analysis, h.cfg)
	discoverer := relationships.NewDiscoverer(h.store.Items, h.store.Chunks, h.store.Entities,
		h.store.Aliases, h.store.Claims, h.store.Relationships, h.vectors, h.embedder, h.analysis, h.cfg)

	h.ingest = ingest.NewService(h.store.Items, h.store.Jobs, h.cfg, recorder, nil)
	h.search = search.NewService(h.store.Items, h.store.Chunks, h.store.Entities,
		h.store.Relationships, h.vectors, h.embedder, h.cfg, nil)
	h.answer = search.NewAnswerer(h.search, h.analysis, h.store.Claims, true)
	h.reviewer = relationships.NewReviewer(h.store.Relationships, h.store.Items, recorder)
	h.auth = auth.NewService(h.store.Users, h.store.Sessions, h.cfg.Session, false, nil)

	h.pool = jobs.NewPool(h.store.Jobs, h.cfg.Workers, nil)
	h.pipeline = ingest.NewPipeline(ingest.Repos{
		Items: h.store.Items, Chunks: h.store.Chunks, Entities: h.store.Entities,
		Aliases: h.store.Aliases, Claims: h.store.Claims, Rels: h.store.Relationships,
		Jobs: h.store.Jobs, Runs: h.store.Runs, Models: h.store.ModelVersions,
	}, h.vectors, h.embedder, h.analysis, entitySvc, discoverer, h.ingest, h.cfg, h.pool, recorder)
	h.pipeline.Register(h.pool)
}

func testConfig(t *testing.T, suffix string) *config.Config {
	t.Helper()

	uploadDir := t.TempDir()
	cfg := &config.Config{LogLevel: "error"}

	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.BaseURL = "http://127.0.0.1:8080"
	cfg.HTTP.MaxRequestBytes = 1 << 20

	cfg.Mongo.URI = mongoURI()
	cfg.Mongo.Database = "relata_test_" + suffix
	// Generous, because these tests often run alongside a developer's own
	// Relata instance competing for the same MongoDB.
	cfg.Mongo.Timeout = 30 * time.Second

	cfg.Qdrant.URL = qdrantURL()
	cfg.Qdrant.Collection = "relata_test_" + suffix
	cfg.Qdrant.Timeout = 30 * time.Second

	cfg.Ollama.EmbeddingModel = "fake-embed-v1"
	cfg.Ollama.AnalysisModel = "fake-analysis-v1"
	cfg.Ollama.EmbeddingDims = 64
	cfg.Ollama.AnalysisEnabled = false

	cfg.Uploads.Dir = uploadDir
	cfg.Uploads.MaxUploadBytes = 5 << 20
	cfg.Uploads.MaxExtractedTextBytes = 1 << 20
	cfg.Uploads.MaxJSONDepth = 20
	cfg.Uploads.MaxMetadataKeys = 40
	cfg.Uploads.MaxMetadataValueLen = 2000
	cfg.Uploads.MaxTitleLen = 300
	cfg.Uploads.MaxConcurrentIngest = 4

	cfg.Chunking.MaxChars = 800
	cfg.Chunking.TargetChars = 500
	cfg.Chunking.MinChars = 100
	cfg.Chunking.OverlapChars = 60
	cfg.Chunking.MaxPerItem = 50
	cfg.Chunking.MinSplitChars = 400

	// Short intervals so tests do not wait on polling.
	cfg.Workers.Count = 3
	cfg.Workers.AIConcurrency = 2
	cfg.Workers.PollInterval = 20 * time.Millisecond
	cfg.Workers.LeaseDuration = 10 * time.Second
	cfg.Workers.HeartbeatPeriod = 2 * time.Second
	cfg.Workers.MaxAttempts = 3
	cfg.Workers.JobTimeout = 60 * time.Second
	cfg.Workers.Enabled = true

	cfg.Search.VectorCandidates = 50
	cfg.Search.LexicalCandidates = 50
	cfg.Search.DefaultLimit = 20
	cfg.Search.MaxResults = 100
	cfg.Search.ExpansionMinScore = 0.3
	cfg.Search.MaxExpandedItems = 20
	cfg.Search.AnswerEnabled = true
	cfg.Search.AnswerMaxChunks = 10
	cfg.Search.AnswerMaxCharsPerChunk = 1000

	cfg.Rel.MaxCandidates = 40
	cfg.Rel.MinScore = 0.15
	cfg.Rel.StoreScore = 0.2
	cfg.Rel.MaxModelCalls = 5
	cfg.Rel.NearDuplicateSimilarity = 0.94
	cfg.Rel.TemporalWindow = 168 * time.Hour
	cfg.Rel.MaxNeighboursPerChunk = 10

	cfg.Session.CookieName = "relata_session"
	cfg.Session.IdleTimeout = time.Hour
	cfg.Session.AbsoluteTimeout = 24 * time.Hour
	cfg.Session.LoginRateBurst = 5
	cfg.Session.LoginRateWindow = time.Minute

	return cfg
}

// enableAnalysis rewires the harness with a fake analyser returning reply.
func (h *harness) enableAnalysis(ctx context.Context, reply string) *fakeAnalyser {
	h.pool.Stop(2 * time.Second)

	h.analyser = &fakeAnalyser{reply: reply}
	h.analysis = analysis.NewService(h.analyser, nil, true)
	h.cfg.Ollama.AnalysisEnabled = true

	h.build(ctx)
	h.pool.Start(ctx)
	return h.analyser
}

func (h *harness) ensureWorkspace(ctx context.Context, slug string) *domain.Workspace {
	h.t.Helper()
	ws := &domain.Workspace{
		ID: ids.New(), Name: slug, Slug: slug, CreatedAt: time.Now().UTC(),
	}
	if err := h.store.Workspaces.Create(ctx, ws); err != nil {
		h.t.Fatalf("create workspace: %v", err)
	}
	return ws
}

func (h *harness) createUser(ctx context.Context, username string, role domain.Role, workspaceID string) *domain.Principal {
	h.t.Helper()

	hash, err := auth.HashPassword("a sufficiently long test password")
	if err != nil {
		h.t.Fatalf("hash password: %v", err)
	}
	u := &domain.User{
		ID: ids.New(), Username: username, PasswordHash: hash,
		Role: role, WorkspaceID: workspaceID,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := h.store.Users.Create(ctx, u); err != nil {
		h.t.Fatalf("create user: %v", err)
	}
	return &domain.Principal{
		UserID: u.ID, Username: u.Username, Role: u.Role, WorkspaceID: u.WorkspaceID,
	}
}

// ingestText stores a note and returns it.
func (h *harness) ingestText(ctx context.Context, title, body string, opts ...func(*ingest.Request)) *domain.Item {
	h.t.Helper()

	req := ingest.Request{
		Title:      title,
		Content:    []byte(body),
		SourceType: domain.SourceTypeNote,
	}
	for _, o := range opts {
		o(&req)
	}

	res, err := h.ingest.Ingest(ctx, h.principal, req)
	if err != nil {
		h.t.Fatalf("ingest %q: %v", title, err)
	}
	return res.Item
}

func withMetadata(m map[string]string) func(*ingest.Request) {
	return func(r *ingest.Request) { r.Metadata = m }
}

func withCreatedAt(t time.Time) func(*ingest.Request) {
	return func(r *ingest.Request) { r.CreatedAt = &t }
}

// waitForItem blocks until an item reaches a terminal status.
func (h *harness) waitForItem(ctx context.Context, itemID string, timeout time.Duration) *domain.Item {
	h.t.Helper()

	deadline := time.Now().Add(timeout)
	var last domain.ProcessingStatus
	for time.Now().Before(deadline) {
		item, err := h.store.Items.Get(ctx, h.principal.WorkspaceID, itemID)
		if err == nil {
			last = item.Status
			switch item.Status {
			case domain.StatusReady, domain.StatusPartial, domain.StatusFailed:
				return item
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.dumpJobs(ctx)
	h.t.Fatalf("item %s did not finish within %s (last status: %s)", itemID, timeout, last)
	return nil
}

// waitForQueue blocks until nothing is pending or running.
func (h *harness) waitForQueue(ctx context.Context, timeout time.Duration) {
	h.t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		counts, err := h.store.Jobs.CountByStatus(ctx, h.principal.WorkspaceID)
		if err == nil && counts[domain.JobPending]+counts[domain.JobRunning] == 0 {
			// Let any final write settle.
			time.Sleep(100 * time.Millisecond)
			counts, _ = h.store.Jobs.CountByStatus(ctx, h.principal.WorkspaceID)
			if counts[domain.JobPending]+counts[domain.JobRunning] == 0 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.dumpJobs(ctx)
	h.t.Fatalf("the queue did not drain within %s", timeout)
}

// dumpJobs prints the queue, to make a timeout diagnosable.
func (h *harness) dumpJobs(ctx context.Context) {
	jobsList, _, err := h.store.Jobs.List(ctx, h.principal.WorkspaceID, nil, nil, domain.Page{Limit: 100})
	if err != nil {
		return
	}
	var b strings.Builder
	b.WriteString("\nqueue state:\n")
	for _, j := range jobsList {
		fmt.Fprintf(&b, "  %-24s %-10s attempts=%d %s\n", j.Type, j.Status, j.Attempts, j.LastError)
	}
	h.t.Log(b.String())
}

// ingestAndWait stores a note and waits for it to finish processing.
func (h *harness) ingestAndWait(ctx context.Context, title, body string, opts ...func(*ingest.Request)) *domain.Item {
	h.t.Helper()
	item := h.ingestText(ctx, title, body, opts...)
	return h.waitForItem(ctx, item.ID, 120*time.Second)
}

// pauseWorkers stops the worker pool for the remainder of the test, so a test
// can observe queue state without workers changing it underneath.
func (h *harness) pauseWorkers() {
	h.t.Helper()
	h.pool.Stop(5 * time.Second)
}

// retry runs fn up to attempts times, pausing between failures.
func retry(attempts int, pause time.Duration, fn func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil {
			return nil
		}
		if i < attempts-1 {
			time.Sleep(pause)
		}
	}
	return err
}

// vectorCount reports how many vectors the workspace holds.
func (h *harness) vectorCount(ctx context.Context, workspaceID string) int64 {
	h.t.Helper()
	n, err := h.vectors.CountByWorkspace(ctx, workspaceID)
	if err != nil {
		h.t.Fatalf("count vectors: %v", err)
	}
	return n
}
