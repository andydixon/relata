package embeddings

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/observability"
)

// OllamaOptions configure the Ollama embedding adapter.
type OllamaOptions struct {
	BaseURL   string
	Model     string
	Timeout   time.Duration
	BatchSize int
	Retry     RetryPolicy
	// ExpectedDims, when non-zero, is asserted against the provider's first
	// response. A silent dimension change would corrupt the vector collection.
	ExpectedDims int
	HTTPClient   *http.Client
	Metrics      *observability.Metrics
}

// Ollama implements domain.Embedder against an Ollama server.
type Ollama struct {
	base     string
	model    string
	batch    int
	retry    RetryPolicy
	client   *http.Client
	metrics  *observability.Metrics
	wantDims int

	mu   sync.RWMutex
	dims int
}

var _ domain.Embedder = (*Ollama)(nil)

// NewOllama builds an Ollama-backed embedder.
func NewOllama(o OllamaOptions) *Ollama {
	if o.Timeout <= 0 {
		o.Timeout = 60 * time.Second
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 16
	}
	client := o.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: o.Timeout,
			// The provider URL comes from operator configuration, never from a
			// document, so no redirect-following restriction is needed beyond
			// refusing to follow redirects at all.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &Ollama{
		base:     strings.TrimRight(o.BaseURL, "/"),
		model:    o.Model,
		batch:    o.BatchSize,
		retry:    o.Retry,
		client:   client,
		metrics:  o.Metrics,
		wantDims: o.ExpectedDims,
		dims:     o.ExpectedDims,
	}
}

// ModelID returns the configured model name, stamped onto every stored vector.
func (o *Ollama) ModelID() string { return o.model }

// Dimensions returns the vector width, learned from the first response if the
// configured value was zero.
func (o *Ollama) Dimensions() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.dims
}

type ollamaEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type ollamaEmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
	Model      string      `json:"model"`
}

// Embed returns one vector per input text, in order.
//
// Inputs are sent in bounded batches; each batch is a separate request with its
// own retry budget, so one slow batch cannot stall the whole set indefinitely.
// Document text is never logged.
func (o *Ollama) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	out := make([][]float32, 0, len(texts))

	for start := 0; start < len(texts); start += o.batch {
		end := start + o.batch
		if end > len(texts) {
			end = len(texts)
		}
		batch := texts[start:end]

		vecs, err := o.embedBatch(ctx, batch)
		if err != nil {
			return nil, fmt.Errorf("embed batch %d-%d of %d: %w", start, end, len(texts), err)
		}
		if len(vecs) != len(batch) {
			return nil, Permanent(fmt.Errorf(
				"provider returned %d vectors for %d inputs", len(vecs), len(batch)))
		}
		out = append(out, vecs...)
	}
	return out, nil
}

func (o *Ollama) embedBatch(ctx context.Context, batch []string) ([][]float32, error) {
	// Ollama rejects empty strings; substitute a single space and let the
	// caller's chunker guarantee meaningful content.
	inputs := make([]string, len(batch))
	for i, t := range batch {
		if strings.TrimSpace(t) == "" {
			inputs[i] = " "
		} else {
			inputs[i] = t
		}
	}

	body, err := json.Marshal(ollamaEmbedRequest{Model: o.model, Input: inputs})
	if err != nil {
		return nil, Permanent(fmt.Errorf("encode request: %w", err))
	}

	var result [][]float32
	started := time.Now()

	err = Do(ctx, o.retry, func(ctx context.Context, attempt int) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.base+"/api/embed", bytes.NewReader(body))
		if err != nil {
			return Permanent(err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := o.client.Do(req)
		if err != nil {
			return err
		}
		defer func() {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			_ = resp.Body.Close()
		}()

		if resp.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			se := &StatusError{Status: resp.StatusCode, Body: strings.TrimSpace(string(msg)), Op: "ollama embed"}
			if resp.StatusCode == http.StatusNotFound {
				return Permanent(fmt.Errorf(
					"%w — is the model pulled? try: ollama pull %s", se, o.model))
			}
			return se
		}

		// Cap the response so a misbehaving provider cannot exhaust memory.
		var out ollamaEmbedResponse
		dec := json.NewDecoder(io.LimitReader(resp.Body, 64<<20))
		if err := dec.Decode(&out); err != nil {
			return fmt.Errorf("decode embedding response: %w", err)
		}
		if len(out.Embeddings) == 0 {
			return Permanent(fmt.Errorf("provider returned no embeddings"))
		}
		result = out.Embeddings
		return nil
	})

	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	if o.metrics != nil {
		o.metrics.AIRequests.WithLabelValues("ollama", "embed", outcome).Inc()
		o.metrics.AIDuration.WithLabelValues("ollama", "embed").Observe(time.Since(started).Seconds())
	}
	if err != nil {
		return nil, err
	}

	if err := o.checkDimensions(result); err != nil {
		return nil, err
	}
	return result, nil
}

// checkDimensions validates vector width and learns it on first use. A mismatch
// is permanent: writing differently-sized vectors into one Qdrant collection
// would corrupt it, so the job must fail loudly instead.
func (o *Ollama) checkDimensions(vecs [][]float32) error {
	if len(vecs) == 0 {
		return nil
	}
	got := len(vecs[0])
	if got == 0 {
		return Permanent(fmt.Errorf("provider returned a zero-length vector"))
	}
	for i, v := range vecs {
		if len(v) != got {
			return Permanent(fmt.Errorf(
				"provider returned inconsistent vector widths: %d at index 0, %d at index %d", got, len(v), i))
		}
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	if o.dims == 0 {
		o.dims = got
		return nil
	}
	if o.dims != got {
		return Permanent(fmt.Errorf(
			"embedding model %q returned %d dimensions but %d were expected; "+
				"set RELATA_EMBEDDING_DIMENSIONS to %d and re-embed the workspace",
			o.model, got, o.dims, got))
	}
	return nil
}

type ollamaTagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// Health reports whether Ollama is reachable and whether the configured model
// is present, so the settings page can tell the user what to pull.
func (o *Ollama) Health(ctx context.Context) domain.ProviderHealth {
	h := domain.ProviderHealth{Name: "ollama", Endpoint: o.base, Model: o.model}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.base+"/api/tags", nil)
	if err != nil {
		h.Detail = err.Error()
		return h
	}
	resp, err := o.client.Do(req)
	if err != nil {
		h.Detail = "cannot reach Ollama: " + err.Error()
		return h
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		h.Detail = fmt.Sprintf("Ollama returned HTTP %d", resp.StatusCode)
		return h
	}
	var tags ollamaTagsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tags); err != nil {
		h.Detail = "unreadable model list: " + err.Error()
		return h
	}
	for _, m := range tags.Models {
		h.Models = append(h.Models, m.Name)
	}
	h.Available = true
	if !modelPresent(h.Models, o.model) {
		h.Available = false
		h.Detail = fmt.Sprintf("model %q is not installed — run: ollama pull %s", o.model, o.model)
	}
	return h
}

// modelPresent matches names allowing for an implicit ":latest" tag.
func modelPresent(models []string, want string) bool {
	for _, m := range models {
		if m == want || m == want+":latest" || strings.TrimSuffix(m, ":latest") == want {
			return true
		}
	}
	return false
}
