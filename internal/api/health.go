package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/observability"
)

// HealthChecker reports whether Relata and its dependencies are working.
type HealthChecker struct {
	Mongo    func(context.Context) error
	Vectors  domain.VectorStore
	Embedder domain.HealthChecker
	Analyser domain.HealthChecker
	Version  string
	Started  time.Time
}

// checkResult is one dependency's status.
type checkResult struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Required marks dependencies without which Relata cannot serve requests.
	Required bool `json:"required"`
}

// Live reports that the process is running.
//
// It deliberately checks nothing else: a liveness probe that fails when a
// database is briefly unavailable causes an orchestrator to restart a process
// that was working perfectly well.
func (h *HealthChecker) Live(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"version":    h.Version,
		"uptime_sec": int(time.Since(h.Started).Seconds()),
	})
}

// Ready reports whether Relata can serve requests.
//
// MongoDB and Qdrant are required: without them nothing can be stored or
// retrieved. The AI providers are reported but not required, because Relata's
// deterministic pipeline, search and browsing all work without a model — saying
// otherwise would take the whole service down when Ollama restarts.
func (h *HealthChecker) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	checks := map[string]checkResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup

	set := func(name string, res checkResult) {
		mu.Lock()
		checks[name] = res
		mu.Unlock()
	}

	// unavailable records a failed dependency. The driver's own error text is
	// logged but never returned: readiness probes are unauthenticated, and
	// MongoDB's topology errors carry the host, port and sometimes the
	// credentials from the connection string. The caller gets a stable
	// description; the operator gets the specifics in the log.
	unavailable := func(name, detail string, required bool, err error) {
		slog.WarnContext(ctx, "readiness check failed",
			"dependency", name, "error", err, "request_id", observability.RequestID(ctx))
		set(name, checkResult{Status: "unavailable", Detail: detail, Required: required})
	}

	if h.Mongo != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.Mongo(ctx); err != nil {
				unavailable("mongodb",
					"the database did not respond to a health check; see the server log for the cause",
					true, err)
				return
			}
			set("mongodb", checkResult{Status: "ok", Required: true})
		}()
	}

	if h.Vectors != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.Vectors.Health(ctx); err != nil {
				unavailable("qdrant",
					"the vector store did not respond to a health check; see the server log for the cause",
					true, err)
				return
			}
			set("qdrant", checkResult{Status: "ok", Required: true})
		}()
	}

	if h.Embedder != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := h.Embedder.Health(ctx)
			if !st.Available {
				unavailable("embedding_provider",
					"the embedding model is not reachable; new items cannot be embedded",
					false, errors.New(st.Detail))
				return
			}
			set("embedding_provider", checkResult{Status: "ok"})
		}()
	}

	if h.Analyser != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := h.Analyser.Health(ctx)
			if !st.Available {
				unavailable("analysis_provider",
					"the analysis model is not reachable; extraction falls back to deterministic rules only",
					false, errors.New(st.Detail))
				return
			}
			set("analysis_provider", checkResult{Status: "ok"})
		}()
	}

	wg.Wait()

	status := http.StatusOK
	overall := "ready"
	for _, c := range checks {
		if c.Required && c.Status != "ok" {
			status = http.StatusServiceUnavailable
			overall = "not ready"
		}
	}

	WriteJSON(w, status, map[string]any{
		"status":  overall,
		"checks":  checks,
		"version": h.Version,
		"note":    "AI providers are reported but not required: ingestion, search and browsing work without them.",
	})
}
