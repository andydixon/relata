# AI provider interface

Relata's AI providers sit behind two interfaces declared in `internal/domain`. Ollama is the
default; nothing in the pipeline knows that.

---

## The two ports

```go
// internal/domain/ports.go

type Embedder interface {
    // Embed returns one vector per input text, in the same order.
    // Implementations must honour ctx cancellation and must not retry indefinitely.
    Embed(ctx context.Context, texts []string) ([][]float32, error)
    ModelID() string      // stamped onto every derived record
    Dimensions() int      // vector width
}

type Analyser interface {
    Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error)
    ModelID() string
}
```

`CompletionRequest` carries messages, an optional JSON schema, a token ceiling and a
temperature. `Complete` returns the **raw content** — it does not parse it. Parsing belongs to
`internal/analysis`, so a validation failure is attributed to the task rather than the
transport.

Optionally, a provider may implement `HealthChecker` to appear on the settings page:

```go
type HealthChecker interface {
    Health(ctx context.Context) ProviderHealth
}
```

---

## Contract

Anything implementing these interfaces must:

| Requirement | Why |
|---|---|
| Honour `ctx` cancellation | A worker that loses its lease cancels the context; a provider that ignores it wastes the slot |
| Bound retries | A permanently failing provider must surface as a job failure, not an endless loop |
| Retry only transient errors | A 400 or an unknown model will not fix itself |
| Return `Permanent(err)` for unrecoverable failures | The worker uses this to decide not to retry |
| Return vectors in input order | The caller pairs them with chunks positionally |
| Return exactly one vector per input | Anything else is a permanent error |
| Keep vector width stable | A change corrupts the Qdrant collection |
| Bound response size | A misbehaving provider must not exhaust memory |
| Never log document content | Content is the user's data |
| Apply a timeout | Every outbound call is bounded |
| Refuse redirects | A compromised provider must not bounce requests elsewhere |

The helpers in `internal/embeddings/retry.go` — `Do`, `Permanent`, `Retryable`, `StatusError`
— are provider-agnostic and are the expected way to satisfy the retry requirements.

---

## Adding an OpenAI-compatible provider

Two files. Nothing else changes.

### 1. The embedder

```go
package embeddings

type OpenAICompatible struct {
    base, model, apiKey string
    dims                int
    client              *http.Client
    retry               RetryPolicy
}

var _ domain.Embedder = (*OpenAICompatible)(nil)

func (o *OpenAICompatible) ModelID() string  { return o.model }
func (o *OpenAICompatible) Dimensions() int  { return o.dims }

func (o *OpenAICompatible) Embed(ctx context.Context, texts []string) ([][]float32, error) {
    var out [][]float32

    for start := 0; start < len(texts); start += o.batch {
        end := min(start+o.batch, len(texts))
        body, _ := json.Marshal(map[string]any{
            "model": o.model,
            "input": texts[start:end],
        })

        var resp struct {
            Data []struct {
                Index     int       `json:"index"`
                Embedding []float32 `json:"embedding"`
            } `json:"data"`
        }

        err := Do(ctx, o.retry, func(ctx context.Context, attempt int) error {
            req, err := http.NewRequestWithContext(ctx, http.MethodPost,
                o.base+"/v1/embeddings", bytes.NewReader(body))
            if err != nil {
                return Permanent(err)
            }
            req.Header.Set("Content-Type", "application/json")
            req.Header.Set("Authorization", "Bearer "+o.apiKey)

            httpResp, err := o.client.Do(req)
            if err != nil {
                return err // transport failures are retried
            }
            defer httpResp.Body.Close()

            if httpResp.StatusCode != http.StatusOK {
                msg, _ := io.ReadAll(io.LimitReader(httpResp.Body, 2048))
                se := &StatusError{Status: httpResp.StatusCode, Body: string(msg), Op: "embeddings"}
                if httpResp.StatusCode == 400 || httpResp.StatusCode == 404 {
                    return Permanent(se) // never retried
                }
                return se // Retryable() decides from the status
            }
            return json.NewDecoder(io.LimitReader(httpResp.Body, 64<<20)).Decode(&resp)
        })
        if err != nil {
            return nil, err
        }

        // The API may return results out of order.
        sort.Slice(resp.Data, func(i, j int) bool { return resp.Data[i].Index < resp.Data[j].Index })
        for _, d := range resp.Data {
            out = append(out, d.Embedding)
        }
    }

    if len(out) != len(texts) {
        return nil, Permanent(fmt.Errorf("provider returned %d vectors for %d inputs", len(out), len(texts)))
    }
    return out, nil
}
```

### 2. The analyser

```go
package analysis

type OpenAICompatibleAnalyser struct { /* … */ }

var _ domain.Analyser = (*OpenAICompatibleAnalyser)(nil)

func (a *OpenAICompatibleAnalyser) Complete(ctx context.Context,
    req domain.CompletionRequest) (*domain.CompletionResponse, error) {

    payload := map[string]any{
        "model":       a.model,
        "messages":    toOpenAIMessages(req.Messages),
        "max_tokens":  req.MaxTokens,
        "temperature": req.Temperature,
    }
    // Structured output, where the provider supports it.
    if len(req.JSONSchema) > 0 {
        payload["response_format"] = map[string]any{
            "type": "json_schema",
            "json_schema": map[string]any{
                "name":   "relata_task",
                "strict": true,
                "schema": json.RawMessage(req.JSONSchema),
            },
        }
    }
    // … POST /v1/chat/completions, return the raw content unparsed.
}
```

### 3. Wire it in

In `cmd/relata/main.go`, choose the implementation from configuration:

```go
var embedder domain.Embedder
switch cfg.Ollama.Provider {           // e.g. RELATA_AI_PROVIDER
case "openai":
    embedder = embeddings.NewOpenAICompatible(...)
default:
    embedder = embeddings.NewOllama(...)
}
```

Every service takes `domain.Embedder` and `domain.Analyser`, so nothing downstream changes.

---

## Structured output without native schema support

`analysis.DecodeStrict` does not require the provider to enforce the schema. It:

- rejects unknown fields and trailing content;
- bounds the payload before parsing;
- unwraps a complete markdown fence (a structural operation on a delimiter — if what remains
  is not valid JSON it is still rejected);
- never uses a regular expression to find or repair JSON.

A provider without structured-output support therefore still works: the schema is included in
the prompt as guidance, and validation catches what the model gets wrong. Expect a higher
rejection rate, visible in `relata_model_invalid_output_total`.

---

## Task-level validation

Above the transport, each task validates independently of the provider:

| Task | Checks |
|---|---|
| `extract_entities` | Entity type enum per row; a bad row is skipped, not fatal. An evidence quotation absent from the source is dropped. |
| `extract_claims` | Nature enum; **quotation must be locatable in the source** (whitespace reflow tolerated), or the claim is discarded. |
| `classify_relationship` | Relationship type enum; confidence in 0–1. |
| `generate_answer` | Citations resolved against supplied refs; fabricated references dropped and counted. |

### Substance, not only shape

A JSON schema constrains a response's *shape*, not its *substance*. A small model asked for
`{"answer": ..., "sufficient": ...}` will sometimes return the schema back:

```json
{"answer": "sufficient", "sufficient": true, "citations": []}
```

That is schema-valid and completely empty. `llama3.2:3b` produced exactly this, and an earlier
build displayed the word "sufficient" to the user as an answer.

Free-text fields are therefore also checked for degeneracy (`analysis.Degenerate`), which
rejects a value that is a bare field name or enum member from the schema itself, a placeholder
(`"string"`, `"n/a"`, `"TODO"`, `"..."`), or too short to carry meaning:

| Field | Minimum | Rejected as |
|---|---:|---|
| `generate_answer` → answer | 15 chars | `degenerate_content` |
| `extract_entities` → name | 2 chars | `degenerate_content` |
| `extract_claims` → statement | 10 chars | `degenerate_content` |
| proposed alias → name | 2 chars | `degenerate_content` |

A rejected answer is reported as unavailable rather than shown, and the request still returns
its retrieved sources and any contradictions Relata recorded between them — those are
deterministic and do not depend on the model. Only a transport failure fails the request.

The same word list applies to every field, which costs a little precision on names: a person
genuinely surnamed "Na", or an organisation called exactly "Example", would be rejected as a
placeholder. The cost is bounded — a rejected row is skipped, the rest of the extraction
proceeds, and deterministic extraction is unaffected — and a model returning a bare
placeholder word as an entity name is overwhelmingly more often noise than a real name.

This is a floor on obvious emptiness, not a quality judgement. It cannot tell a poor answer
from a good one, only a non-answer from an answer.

Rejections are recorded on `relata_model_invalid_output_total{task,reason}` with a bounded
reason set, and are **not retried** — a permanently invalid response will not become valid by
repetition.

---

## Running without a language model

Set `RELATA_ANALYSIS_ENABLED=false`. `analysis.Service` reports itself disabled and every task
returns `ErrDisabled`, which the pipeline treats as degraded quality rather than failure.

Still working: ingestion, parsing, chunking, embedding, deterministic entity extraction,
deterministic claim extraction, relationship discovery and scoring, hybrid search, and the
whole interface.

Not working: model-assisted entity and claim extraction, relationship type classification by
model, and generated answers.

An embedder is still required for meaning-based search. Without one, search degrades to
keyword, entity and relationship matching, and says so in the response notes.

---

## Model versions and stored data

Every derived record carries the model that produced it, and `model_versions` records what has
been seen per workspace. The settings page shows this, and warns when the stored vector count
does not match the chunk count.

**Changing the embedding model changes the vector geometry.** Relata detects a dimension
mismatch and refuses to write mixed-width vectors — a permanent error naming the fix. After
such a change: set `RELATA_EMBEDDING_DIMENSIONS`, then use **Settings → Re-embed workspace**,
which processes in bounded batches and re-enqueues itself rather than holding one lease for
hours.

**Changing the analysis model** does not invalidate anything; reprocess items to refresh
model-derived entities, claims and relationship classifications.
