// Package analysis performs model-assisted structured tasks: entity extraction,
// claim extraction, relationship classification and answer generation.
//
// Two rules govern everything here:
//
//  1. Stored document content is data, never instruction. Retrieved text is
//     delimited, labelled untrusted, and the system prompt states that any
//     instructions inside it must be ignored and reported.
//  2. Model output is untrusted input. Responses are decoded against a strict
//     schema with unknown fields rejected, enum values checked, and referenced
//     IDs verified against what was actually supplied. Invalid output is
//     recorded and discarded — never repaired with regular expressions.
package analysis

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/andydixon/relata/internal/domain"
	"github.com/andydixon/relata/internal/embeddings"
	"github.com/andydixon/relata/internal/observability"
)

// OllamaOptions configure the Ollama chat adapter.
type OllamaOptions struct {
	BaseURL    string
	Model      string
	Timeout    time.Duration
	Retry      embeddings.RetryPolicy
	MaxTokens  int
	HTTPClient *http.Client
	Metrics    *observability.Metrics
}

// OllamaAnalyser implements domain.Analyser against an Ollama server.
type OllamaAnalyser struct {
	base      string
	model     string
	retry     embeddings.RetryPolicy
	maxTokens int
	client    *http.Client
	metrics   *observability.Metrics
}

var _ domain.Analyser = (*OllamaAnalyser)(nil)

// NewOllamaAnalyser builds an Ollama-backed analyser.
func NewOllamaAnalyser(o OllamaOptions) *OllamaAnalyser {
	if o.Timeout <= 0 {
		o.Timeout = 120 * time.Second
	}
	if o.MaxTokens <= 0 {
		o.MaxTokens = 2048
	}
	client := o.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: o.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &OllamaAnalyser{
		base:      strings.TrimRight(o.BaseURL, "/"),
		model:     o.Model,
		retry:     o.Retry,
		maxTokens: o.MaxTokens,
		client:    client,
		metrics:   o.Metrics,
	}
}

// ModelID returns the configured model name.
func (a *OllamaAnalyser) ModelID() string { return a.model }

type ollamaChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaChatRequest struct {
	Model    string              `json:"model"`
	Messages []ollamaChatMessage `json:"messages"`
	Stream   bool                `json:"stream"`
	// Format carries a JSON schema when structured output is requested.
	Format  json.RawMessage `json:"format,omitempty"`
	Options ollamaOptions   `json:"options"`
}

type ollamaOptions struct {
	Temperature float64 `json:"temperature"`
	NumPredict  int     `json:"num_predict"`
}

type ollamaChatResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	Model           string `json:"model"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
	Done            bool   `json:"done"`
}

// maxResponseBytes caps a model response. Output size is bounded both by
// num_predict and here, because a provider is not trusted to honour either.
const maxResponseBytes = 1 << 20

// Complete sends a bounded chat request and returns the raw content.
//
// The content is not parsed here: callers decode it against a task-specific
// schema so that validation failures are attributed to the right task.
func (a *OllamaAnalyser) Complete(ctx context.Context, req domain.CompletionRequest) (*domain.CompletionResponse, error) {
	if len(req.Messages) == 0 {
		return nil, embeddings.Permanent(fmt.Errorf("completion request has no messages"))
	}
	maxTok := req.MaxTokens
	if maxTok <= 0 || maxTok > a.maxTokens {
		maxTok = a.maxTokens
	}

	msgs := make([]ollamaChatMessage, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = ollamaChatMessage{Role: m.Role, Content: m.Content}
	}

	chatReq := ollamaChatRequest{
		Model:    a.model,
		Messages: msgs,
		Stream:   false,
		Options:  ollamaOptions{Temperature: req.Temperature, NumPredict: maxTok},
	}
	if len(req.JSONSchema) > 0 {
		if !json.Valid(req.JSONSchema) {
			return nil, embeddings.Permanent(fmt.Errorf("supplied JSON schema is not valid JSON"))
		}
		chatReq.Format = json.RawMessage(req.JSONSchema)
	}

	body, err := json.Marshal(chatReq)
	if err != nil {
		return nil, embeddings.Permanent(fmt.Errorf("encode chat request: %w", err))
	}

	var out ollamaChatResponse
	started := time.Now()

	err = embeddings.Do(ctx, a.retry, func(ctx context.Context, attempt int) error {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/api/chat", bytes.NewReader(body))
		if err != nil {
			return embeddings.Permanent(err)
		}
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := a.client.Do(httpReq)
		if err != nil {
			return err
		}
		defer func() {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			_ = resp.Body.Close()
		}()

		if resp.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			se := &embeddings.StatusError{
				Status: resp.StatusCode,
				Body:   strings.TrimSpace(string(msg)),
				Op:     "ollama chat",
			}
			if resp.StatusCode == http.StatusNotFound {
				return embeddings.Permanent(fmt.Errorf(
					"%w — is the model pulled? try: ollama pull %s", se, a.model))
			}
			return se
		}

		var decoded ollamaChatResponse
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&decoded); err != nil {
			return fmt.Errorf("decode chat response: %w", err)
		}
		out = decoded
		return nil
	})

	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	if a.metrics != nil {
		a.metrics.AIRequests.WithLabelValues("ollama", "chat", outcome).Inc()
		a.metrics.AIDuration.WithLabelValues("ollama", "chat").Observe(time.Since(started).Seconds())
	}
	if err != nil {
		return nil, err
	}

	return &domain.CompletionResponse{
		Content:      out.Message.Content,
		Model:        firstNonEmpty(out.Model, a.model),
		PromptTokens: out.PromptEvalCount,
		OutputTokens: out.EvalCount,
		Duration:     time.Since(started),
	}, nil
}

// Health reports whether the analysis model is reachable and installed.
func (a *OllamaAnalyser) Health(ctx context.Context) domain.ProviderHealth {
	h := domain.ProviderHealth{Name: "ollama-analysis", Endpoint: a.base, Model: a.model}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+"/api/tags", nil)
	if err != nil {
		h.Detail = err.Error()
		return h
	}
	resp, err := a.client.Do(req)
	if err != nil {
		h.Detail = "cannot reach Ollama: " + err.Error()
		return h
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.Detail = fmt.Sprintf("Ollama returned HTTP %d", resp.StatusCode)
		return h
	}

	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tags); err != nil {
		h.Detail = "unreadable model list: " + err.Error()
		return h
	}
	for _, m := range tags.Models {
		h.Models = append(h.Models, m.Name)
	}
	h.Available = true
	for _, m := range h.Models {
		if m == a.model || m == a.model+":latest" || strings.TrimSuffix(m, ":latest") == a.model {
			return h
		}
	}
	h.Available = false
	h.Detail = fmt.Sprintf("model %q is not installed — run: ollama pull %s", a.model, a.model)
	return h
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
