# Architecture

Relata is a **modular monolith with in-process background workers**: one binary, one process,
three external data stores. That choice is deliberate and is documented in
[decisions/0001-modular-monolith.md](decisions/0001-modular-monolith.md).

---

## Package layout

```
cmd/relata/                 serve · createadmin · seed · version

internal/
  domain/                   types + port interfaces. Standard library only.
  config/                   environment configuration, validated at startup
  ids/                      UUIDs, session tokens, deterministic dedupe keys
  observability/            structured logging, correlation IDs, metrics

  parsing/                  text · markdown · json · pdf · docx → Document
  chunking/                 structure-aware chunking
  entities/                 normalisation, deterministic extraction, extraction service
  analysis/                 Analyser port, Ollama adapter, strict model-output validation
  embeddings/               Embedder port, Ollama adapter, bounded retry
  relationships/            candidate discovery, versioned scoring, human review
  search/                   hybrid retrieval, answer generation
  ranking/                  rank fusion and result explanation
  ingest/                   ingestion service and pipeline stages
  jobs/                     leased job queue worker pool

  storage/mongo/            repositories implementing the domain ports
  storage/qdrant/           vector store implementing the domain port

  auth/                     Argon2id, sessions, CSRF, login rate limiting
  audit/                    append-only audit recording
  api/                      /api/v1 REST, middleware, router
  web/                      server-rendered HTML, templates, static assets

tests/                      integration and security tests (build tag: integration)
migrations/                 note on why there are none; indexes are applied at startup
docs/                       this documentation, plus openapi.yaml
```

---

## The dependency rule

**`internal/domain` imports only the standard library.** No MongoDB driver, no HTTP types, no
Qdrant client. It declares:

- the domain types (`Item`, `Chunk`, `Entity`, `Claim`, `Relationship`, `Job`, …);
- the **port interfaces** the service layer depends on (`ItemRepository`, `VectorStore`,
  `Embedder`, `Analyser`, …).

Adapters implement those ports:

```
      ┌───────────────┐
      │    domain     │  types + ports, stdlib only
      └───────▲───────┘
              │ implements                     │ depends on
   ┌──────────┴──────────┐         ┌───────────▼──────────┐
   │ storage/mongo       │         │ ingest · search      │
   │ storage/qdrant      │         │ relationships        │
   │ embeddings (Ollama) │         │ entities · analysis  │
   │ analysis  (Ollama)  │         └───────────┬──────────┘
   └─────────────────────┘                     │
                                    ┌──────────▼──────────┐
                                    │      api · web      │
                                    └─────────────────────┘
```

Practical consequences:

- Swapping Ollama for an OpenAI-compatible API means adding one file in `embeddings/` and one
  in `analysis/`. Nothing else changes. See [AI_PROVIDER_INTERFACE.md](AI_PROVIDER_INTERFACE.md).
- The scoring model can be unit-tested without a database — `scoring_test.go` runs in
  milliseconds and needs nothing running.
- `api` and `web` depend on services, never on storage directly.

`api` and `web` have a one-way relationship: `web` imports `api` for the principal/session
context helpers, and the router accepts the web handlers through the `api.WebRoutes`
interface, so `api` never imports `web`.

---

## Data model

MongoDB is the **authoritative store**. Qdrant holds vectors and the minimum needed to
resolve a hit back to MongoDB.

### Collections

| Collection | Contents | Key indexes |
|---|---|---|
| `workspaces` | The data boundary | `slug` (unique) |
| `users` | Local accounts, Argon2id hashes | `username` (unique) |
| `sessions` | Server-side sessions | `token_hash` (unique), TTL on `absolute_expiry` |
| `items` | Source items and their originals | `(workspace, ingested_at)`, `(workspace, content_hash)`, text index |
| `chunks` | Retrievable sections | `(workspace, item, sequence)`, `(workspace, embedded, model)`, text index |
| `entities` | Extracted subjects | **`(workspace, normalised_key)` unique**, `(workspace, item_ids)` |
| `entity_aliases` | Candidate name links | `(workspace, entity, normalised)` unique |
| `claims` | Statements items make | `(workspace, subject)`, `(workspace, dedupe_key)` unique |
| `relationships` | Scored, evidenced links | **`(workspace, dedupe_key)` unique**, `(workspace, source)`, `(workspace, review_status, score)` |
| `jobs` | The persistent queue | `(status, run_after, priority)`, `(status, lease_expires)`, partial unique on `idempotency_key` |
| `processing_runs` | Per-item processing history | `(workspace, item, started_at)` |
| `model_versions` | Models that produced stored data | `(workspace, kind, name)` unique |
| `audit_events` | Append-only action log | `(workspace, at)`, `(workspace, action, at)` |
| `api_tokens` | Bearer credentials; hashes only, never secrets | `token_hash` unique, `(workspace, user, created_at)`, TTL on `expires_at` |

### Identifiers

Every record uses a **string UUID** `_id`. Chunk IDs are used verbatim as Qdrant point IDs,
which Qdrant accepts in UUID form. That removes an entire class of ID-mapping bugs and makes
"delete this item's vectors" a filter rather than a join.

Chunk IDs are **deterministic** — derived from `(workspace, item, sequence, content_hash)` —
so re-chunking an unchanged item reuses the same point IDs and overwrites the vectors rather
than orphaning them.

### The three unique indexes that carry the weight

These are not merely constraints; they are what make the pipeline idempotent under
concurrency.

- **`entities (workspace, normalised_key)`** — one entity per normalised name and type.
  Concurrent workers extracting the same company from different documents converge on one
  record instead of racing to create two. Similar-but-different names never collide here;
  they produce a candidate alias for a person to adjudicate, and even a confirmed alias
  leaves both records in place.
- **`relationships (workspace, dedupe_key)`** — the key is derived from the endpoint pair, so
  reprocessing updates an existing relationship rather than adding a parallel one. Crucially,
  the upsert **refuses to overwrite a human review decision**: if someone has confirmed,
  rejected or edited it, only the evidence is refreshed.
- **`jobs (idempotency_key)` where `status = "pending"`** — a partial index, so at most one
  *pending* job may exist per key while completed jobs remain as history. Re-enqueuing after
  a crash is a no-op rather than duplicate work.

### Workspace enforcement

Every user-content record carries `workspace_id`, and every repository method takes one. The
helper `wsFilter` is the only sanctioned way to begin a filter and **returns an error on an
empty workspace**, so a missing principal cannot silently widen a query.

There is deliberately no `GetByID(id)` that skips the workspace. Cross-workspace access
returns `not_found`, never `forbidden`, so probing cannot confirm that an ID exists elsewhere.

### Qdrant payloads

Only what retrieval needs:

```
workspace_id · item_id · chunk_id · source_type · created_at · embedding_model
```

No text, no titles, no relationship data. Keeping the payload thin is what stops Qdrant
drifting into a second, unversioned copy of the corpus. Every payload field is indexed, so a
workspace-filtered search stays a filtered search rather than degrading into a scan.

---

## Request flow

### A search

```
GET /api/v1/search?q=…
   │
   ├─ RequestID · Recover · Logging · SecurityHeaders · RequireAuth
   │
   ├─ searchQueryFromRequest — typed values only; unknown enums rejected
   │
   └─ search.Service.Search
        ├─ items.TextSearch      (MongoDB $text, workspace-filtered)
        ├─ chunks.TextSearch     (MongoDB $text, for excerpts)
        ├─ embedder.Embed(query) → vectors.Search  (Qdrant, workspace-filtered)
        ├─ entities.FindByNormalised  (query n-grams → entity keys)
        ├─ items.GetMany         (re-checks workspace; drops anything not returned)
        ├─ rels.ListForItems     (expansion; rejected links excluded)
        └─ ranking.Rank          (reciprocal-rank fusion + explanations)
```

Note the second workspace check after `GetMany`: candidates whose items are not returned are
dropped. Qdrant's filter is already workspace-scoped, but the authoritative check is the
MongoDB read.

### An ingestion

```
POST /api/v1/items
   │
   └─ ingest.Service.Ingest
        ├─ admission semaphore (bounded concurrent ingestion)
        ├─ parsing.Sniff        — type from CONTENT, not the filename or header
        ├─ validate title, metadata, size, creation date
        ├─ SHA-256 → FindByContentHash → exact-duplicate check
        ├─ store original bytes (binary) or inline text
        ├─ items.Create
        └─ jobs.Enqueue(parse_item)
              │
              └─ parse → chunk → embed → entities → claims → relationships
```

Each stage is a separate job that enqueues the next. See
[INGESTION_PIPELINE.md](INGESTION_PIPELINE.md).

---

## Background workers

A bounded pool draining a Mongo-backed queue. The design goals are recoverability and
boundedness.

**Leases, not locks.** A claimed job records its owner and an expiry. A worker renews the
lease while it runs. A worker that dies stops renewing; the lease expires; `ReleaseExpired`
returns the job to the pending pool. Nothing external is needed to detect the crash.

**Losing a lease cancels the work.** If a heartbeat finds the lease gone — another worker has
taken over — the job's context is cancelled so the original worker abandons its attempt
instead of writing a stale result.

**Two independent limits.** Worker count bounds concurrent jobs; a separate semaphore bounds
concurrent AI calls. A small local model is not swamped just because the worker count was
raised.

**Only transient failures retry.** Invalid model output, an unparseable document, a missing
handler and a deleted item are permanent — repeating them wastes the queue. Transient
failures retry with bounded exponential backoff and full jitter, within an attempt budget.

**A panicking handler does not take the worker down.** Handlers run guarded; a panic becomes
an ordinary job failure.

**Shutdown drains.** The HTTP server stops accepting first, then workers are given until the
shutdown deadline. A job that does not finish keeps its lease, which expires and returns it
to the queue.

---

## Idempotency

Reprocessing must not multiply data. Each derived record type has its own mechanism:

| Records | Mechanism |
|---|---|
| Chunks | `ReplaceForItem` — delete then insert, so one generation exists |
| Vectors | Deterministic chunk IDs; `DeleteByItem` before re-chunking |
| Claims | `ReplaceForItem`, plus a unique `dedupe_key` |
| Entities | `UpsertByKey` on `(workspace, normalised_key)`; observations accumulate |
| Relationships | `UpsertByDedupeKey`; **human decisions are never overwritten** |
| Jobs | Partial unique index on `idempotency_key` for pending jobs |
| Items | Partial unique index on `(workspace_id, content_hash)` for live, non-deliberate items — duplicate detection is a read then a write, so the index is what actually enforces it ([ADR 6](decisions/0006-duplicate-detection-enforced-by-index.md)) |
| Users | Partial unique index on `(oauth_provider, oauth_subject)` — first sign-in is a read then a write, and two concurrent ones must not provision two workspaces for one person ([ADR 8](decisions/0008-federated-identity-private-workspaces.md)) |
| API tokens | Unique index on `token_hash`; authentication is a lookup on it and nothing else |

Processing runs are the exception: each reprocess **adds** a run, because the point is to
retain what previous parser and model versions produced.

---

## Security architecture

Detailed in [THREAT_MODEL.md](THREAT_MODEL.md). The structural points:

- **Content-based type detection.** A browser's `Content-Type` and the filename are hints; the
  magic bytes decide. A zip renamed `.docx` is rejected unless it genuinely contains
  `word/document.xml`.
- **Stored paths never derive from user input.** An uploaded file is stored under the item's
  own UUID with an extension from a fixed allow-list. The user's filename is retained for
  display only.
- **Escaping by construction.** All HTML goes through `html/template`'s contextual escaping,
  and the `web` package deliberately contains **no** helper that turns a stored string into
  `template.HTML`.
- **A CSP that makes XSS inert.** `default-src 'none'`, `script-src 'self'`, no
  `unsafe-inline`, no remote origins. Relata ships its own CSS and JavaScript as files, so
  nothing inline is needed.
- **Typed queries only.** Filters are assembled in the storage layer from Go values. A client
  string becomes an *operand*, never a key or an operator.
- **Model output is untrusted input.** Strict schema decoding with unknown fields rejected,
  enum validation, reference checking against what was actually supplied, and no regex-based
  repair of malformed JSON.
- **Uploaded content is data, never instruction.** Retrieved text reaches a model only inside
  delimited blocks whose terminator the content cannot forge.

---

## Testing strategy

| Level | Location | Needs |
|---|---|---|
| Unit | `internal/*/[!i]*_test.go` | nothing |
| Fuzz | parsers, model-output decoding, fences, hashes | nothing |
| HTTP | `internal/api/http_test.go` | nothing |
| Integration | `tests/` (tag `integration`) | Docker: MongoDB + Qdrant |
| Security regression | across packages | nothing |

Unit tests assert **behaviour and design invariants**, not coverage. Several encode the
central claims of the design and will fail if a change breaks them:

- similarity alone cannot produce a strong relationship score;
- a maximally confident model opinion cannot reach the storage threshold;
- observed contradiction outranks a model's opinion when inferring a type;
- chronology alone never yields a causal or updating relationship;
- a cross-workspace access is indistinguishable from a missing record;
- a fabricated citation cannot resolve to a real item.
